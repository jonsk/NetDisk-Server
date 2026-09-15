package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 本文件是 **TS-06 多客户端同步冲突验证** 的验收测试
// ("两端同改同文件 → 一 409 并按策略保留冲突副本;改名/移动经 FileId 识别不重传")。
//
// "两个客户端"= **同一个用户的两个令牌**(两台设备)。这是同步冲突的真实形态:
// 冲突并不来自权限,而来自"两端各自读到同一个 base_version 之后都去写"。
// 用同一用户可以把权限判定从冲突语义里排除掉,冲突结论才干净。
//
// 全链路走真实 HTTP 路由(api.New)+ 真实 PostgreSQL + 真实对象存储:
// 409 的 code/reason/details、file_objects.ref_count、FileId 都是**服务端行为**,
// 只有真跑一遍才算验过。

type mcEnv struct {
	handler http.Handler
	tokenA  string
	tokenB  string
	userID  string
	spaceID string
	rootID  string
	dbase   *db.DB
	store   *storage.FS
}

func setupMultiClient(t *testing.T) *mcEnv {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	suffix := time.Now().Format("150405.000000000")
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, cerr := (repo.UserRepo{}).Create(ctx, tx, repo.CreateInput{
			Username: "mc_" + suffix, Email: "mc_" + suffix + "@example.com",
			DisplayName: "多客户端冲突用例用户", Role: model.RoleUser,
		})
		if cerr != nil {
			return cerr
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = database.Pool.Exec(bg, `DELETE FROM uploads WHERE user_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, u.ID)
		_, _ = database.Pool.Exec(bg,
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	sp, err := (repo.SpaceRepo{}).PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	root, err := (repo.FileRepo{}).GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	issue := func() string {
		out, ierr := tokens.Issue(ctx, auth.IssueInput{
			UserID: u.ID, Username: u.Username, Role: u.Role, TokenVersion: u.TokenVersion,
			Audience: auth.AudienceDesktop,
		})
		if ierr != nil {
			t.Fatalf("签发测试令牌失败: %v", ierr)
		}
		return out.AccessToken
	}
	tokenA, tokenB := issue(), issue()

	// 与生产同一套装配(复用 testsupport_test.go 的既有构造函数)
	uploadSvc := newUploadSvc(database)
	fin := newFinalizer(t, database, store)
	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens,
		Uploads: uploadSvc,
		Files:   &filesvc.Service{Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, DB: db.AsQuerier(database)},
		TUS: &uploadsvc.TUSService{
			Service: uploadSvc, Stager: store, Finalizer: fin, DB: db.AsQuerier(database),
		},
		Objects: store, Finalizer: fin,
	})
	return &mcEnv{
		handler: h, tokenA: tokenA, tokenB: tokenB, userID: u.ID,
		spaceID: sp.ID, rootID: root.ID, dbase: database, store: store,
	}
}

// ---- 响应与库表投影 ----

// mcEntry 是 GET /api/v1/files/{id} 的响应投影(只取断言要用的字段)。
type mcEntry struct {
	ID         string `json:"id"`
	ParentID   string `json:"parent_id"`
	Name       string `json:"name"`
	Version    int64  `json:"version"`
	ETag       string `json:"etag"`
	HashSHA256 string `json:"hash_sha256"`
	Size       int64  `json:"size"`
}

// mcErrBody 是结构化错误体:409 的 code 与 details.reason 是客户端分支的依据。
type mcErrBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func mcErrOf(t *testing.T, rec *httptest.ResponseRecorder) mcErrBody {
	t.Helper()
	var b mcErrBody
	if err := jsonUnmarshal(rec.Body.Bytes(), &b); err != nil {
		t.Fatalf("解析错误响应失败: %v body=%s", err, rec.Body.String())
	}
	return b
}

// mcDetailInt 把 details 里的数值(JSON 解出来是 float64)转成 int64。
func mcDetailInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	default:
		return -1
	}
}

func mcNames(entries map[string]mcEntry) []string {
	out := make([]string, 0, len(entries))
	for n := range entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// mcSHA256 算内容哈希(断言"败方的字节没有落库"时要用)。
func mcSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// mcObject 是 file_objects 里与"这份内容"对应的那一行(内容侧的对象身份)。
type mcObject struct {
	Key      string
	RefCount int
	State    string
	Backend  string
}

// ---- HTTP 助手 ----

func mcMetadata(spaceID, parentID, name string) string {
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	return "filename " + enc(name) + ",space_id " + enc(spaceID) + ",parent_id " + enc(parentID)
}

// postTUS 只发建任务请求,把响应原样交回(不做任何状态断言)。
func (e *mcEnv) postTUS(t *testing.T, token, name string, size int) *httptest.ResponseRecorder {
	t.Helper()
	creq := httptest.NewRequest(http.MethodPost, "/tus", nil)
	creq.Header.Set("Authorization", "Bearer "+token)
	creq.Header.Set("Tus-Resumable", "1.0.0")
	creq.Header.Set("Upload-Length", strconv.Itoa(size))
	creq.Header.Set("Upload-Metadata", mcMetadata(e.spaceID, e.rootID, name))
	crec := httptest.NewRecorder()
	e.handler.ServeHTTP(crec, creq)
	return crec
}

// createUpload 走"建任务"并断言成功,返回 (upload_id, ticket) ——
// 定稿时机由调用方控制(并发定稿用例需要"任务已建、内容还没落库"的中间态)。
func (e *mcEnv) createUpload(t *testing.T, token, name string, size int) (string, string) {
	t.Helper()
	crec := e.postTUS(t, token, name, size)
	if crec.Code != http.StatusCreated {
		t.Fatalf("建任务(%s)应 201,实际 %d body=%s", name, crec.Code, crec.Body.String())
	}
	id := strings.TrimPrefix(crec.Header().Get("Location"), "/tus/")
	ticket := crec.Header().Get("X-Upload-Token")
	if id == "" || ticket == "" {
		t.Fatalf("建任务响应应带 Location 与 X-Upload-Token,实际 Location=%q ticket=%q body=%s",
			crec.Header().Get("Location"), ticket, crec.Body.String())
	}
	return id, ticket
}

// patchUpload 追写分片(写满声明长度即触发定稿)。
func (e *mcEnv) patchUpload(t *testing.T, token, id, ticket string, offset int64, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	preq := httptest.NewRequest(http.MethodPatch, "/tus/"+id, bytes.NewReader(data))
	preq.Header.Set("Authorization", "Bearer "+token)
	preq.Header.Set("Tus-Resumable", "1.0.0")
	preq.Header.Set("Content-Type", "application/offset+octet-stream")
	preq.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	preq.Header.Set("X-Upload-Token", ticket)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, preq)
	return rec
}

// uploadRaw 走真实 TUS 路由发"建任务 + 单片定稿"两步,把两个响应原样交回。
//
// 为什么不在这里 Fatalf:同名冲突用例**要**断言服务端 409(这正是"客户端必须
// 自己造冲突副本名"的前提),失败在这里是预期结果。
func (e *mcEnv) uploadRaw(t *testing.T, token, name string, data []byte) (*httptest.ResponseRecorder, *httptest.ResponseRecorder) {
	t.Helper()
	crec := e.postTUS(t, token, name, len(data))
	if crec.Code != http.StatusCreated {
		return crec, httptest.NewRecorder()
	}
	prec := e.patchUpload(t, token,
		strings.TrimPrefix(crec.Header().Get("Location"), "/tus/"),
		crec.Header().Get("X-Upload-Token"), 0, data)
	return crec, prec
}

// upload 上传一个文件并返回 (FileId, 内容哈希, 版本)。
func (e *mcEnv) upload(t *testing.T, token, name string, data []byte) (string, string, int64) {
	t.Helper()
	crec, prec := e.uploadRaw(t, token, name, data)
	if crec.Code != http.StatusCreated {
		t.Fatalf("上传建任务(%s)应 201,实际 %d body=%s", name, crec.Code, crec.Body.String())
	}
	if prec.Code != http.StatusOK {
		t.Fatalf("上传定稿(%s)应 200,实际 %d body=%s", name, prec.Code, prec.Body.String())
	}
	fileID := prec.Header().Get("X-File-Id")
	if fileID == "" {
		t.Fatalf("定稿响应应带 X-File-Id,实际响应头=%v", prec.Header())
	}
	v := e.fileView(t, token, fileID)
	return fileID, v.HashSHA256, v.Version
}

func (e *mcEnv) fileView(t *testing.T, token, fileID string) mcEntry {
	t.Helper()
	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files/"+fileID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读文件元数据(%s)应 200,实际 %d body=%s", fileID, rec.Code, rec.Body.String())
	}
	var v mcEntry
	if err := jsonUnmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析文件元数据失败: %v body=%s", err, rec.Body.String())
	}
	return v
}

// content 下载文件内容(真实下载端点),返回 (状态码, 字节)。
func (e *mcEnv) content(t *testing.T, token, fileID string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/files/"+fileID+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

// dirEntries 列出某目录下的 "名字 → 条目"。
func (e *mcEnv) dirEntries(t *testing.T, token, parentID string) map[string]mcEntry {
	t.Helper()
	rec := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/files?space="+e.spaceID+"&parent="+parentID, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列目录应 200,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []mcEntry `json:"entries"`
	}
	if err := jsonUnmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析目录列表失败: %v body=%s", err, rec.Body.String())
	}
	out := map[string]mcEntry{}
	for _, en := range body.Entries {
		out[en.Name] = en
	}
	return out
}

// mkdir 直接造一条目录行。
//
// REST 侧**没有**建目录接口(建目录是 WebDAV MKCOL 的职责,webdavfs 直接调
// repo.CreateDir),而移动用例需要一个真实的目标目录行。这是既有测试夹具的做法
// (filesvc 与 handlers_sync 的用例同样这么造目录),不是另写一套生产逻辑。
func (e *mcEnv) mkdir(t *testing.T, name string) string {
	t.Helper()
	d, err := (repo.FileRepo{}).CreateDir(context.Background(), e.dbase.Pool, repo.CreateDirInput{
		SpaceID: e.spaceID, ParentID: e.rootID, OwnerID: e.userID, Name: name, Depth: 1,
	})
	if err != nil {
		t.Fatalf("造目标目录失败: %v", err)
	}
	return d.ID
}

// ---- 对象身份的库/盘读数 ----

func (e *mcEnv) objectOf(t *testing.T, hash string) mcObject {
	t.Helper()
	var o mcObject
	if err := e.dbase.Pool.QueryRow(context.Background(), `
SELECT object_key, ref_count, state, storage_backend
  FROM file_objects WHERE hash_sha256 = $1`, hash).
		Scan(&o.Key, &o.RefCount, &o.State, &o.Backend); err != nil {
		t.Fatalf("读 file_objects(hash=%s)失败: %v", hash, err)
	}
	return o
}

// objectRowCount 返回该内容在 file_objects 里的行数(正常恒为 1;>1 意味着重复登记)。
func (e *mcEnv) objectRowCount(t *testing.T, hash string) int {
	t.Helper()
	var n int
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("统计 file_objects 行数失败(hash=%s): %v", hash, err)
	}
	return n
}

// objectFileCount 数存储根目录 objects/ 下的物理对象文件数 ——
// "纯改名/移动不重传"的最强证据:一个字节都没有重新落盘。
func (e *mcEnv) objectFileCount(t *testing.T) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(e.store.Root, "objects"), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历对象目录失败: %v", err)
	}
	return n
}

// ---- 验收用例 ----

// TestMultiClientSyncConflict 覆盖 TS-06 的三条验收子句。每个子测试名就是验收子句本身。
func TestMultiClientSyncConflict(t *testing.T) {
	t.Run("同改同文件_一端胜出另一端409", func(t *testing.T) {
		e := setupMultiClient(t)
		id, hash, v0 := e.upload(t, e.tokenA, "共享预算表.txt", []byte("第一版内容 —— 客户端 A 上传"))

		// 两端各自读到同一版本(同步冲突的前提:双方都以为自己基于 v0 在改)
		a := e.fileView(t, e.tokenA, id)
		b := e.fileView(t, e.tokenB, id)
		if a.Version != b.Version || a.Version != v0 {
			t.Fatalf("两端应读到同一版本 v%d,实际 A=v%d B=v%d", v0, a.Version, b.Version)
		}
		if a.ETag != b.ETag || a.HashSHA256 != b.HashSHA256 {
			t.Fatalf("两端应读到同一 ETag/内容哈希,实际 A=%s/%s B=%s/%s",
				a.ETag, a.HashSHA256, b.ETag, b.HashSHA256)
		}
		if hash == "" || a.HashSHA256 != hash {
			t.Fatalf("前置不成立:内容哈希应为 %s,实际 %s", hash, a.HashSHA256)
		}

		// 客户端 A 先写(改名,带双方读到的 base_version)
		const nameA = "共享预算表-已改.txt"
		recA := doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/files/"+id, e.tokenA,
			map[string]any{"name": nameA, "base_version": v0})
		if recA.Code != http.StatusOK {
			t.Fatalf("先写的一方应 200,实际 %d body=%s", recA.Code, recA.Body.String())
		}
		var winner mcEntry
		if err := jsonUnmarshal(recA.Body.Bytes(), &winner); err != nil {
			t.Fatalf("解析胜者响应失败: %v body=%s", err, recA.Body.String())
		}
		if winner.Version != v0+1 || winner.Name != nameA {
			t.Fatalf("胜者应是 v%d 且名字为 %q,实际 v%d/%q", v0+1, nameA, winner.Version, winner.Name)
		}

		// 客户端 B 用**同一个** base_version 写 → 必须 409
		const nameB = "共享预算表-B改的.txt"
		recB := doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/files/"+id, e.tokenB,
			map[string]any{"name": nameB, "base_version": v0})
		if recB.Code != http.StatusConflict {
			t.Fatalf("后写的一方必须 409(同一 base_version 只能有一个人赢),实际 %d body=%s",
				recB.Code, recB.Body.String())
		}
		cf := mcErrOf(t, recB)
		// reason 必须是**代码里实际使用的那个串**(filesvc.ReasonVersionConflict):
		// 客户端按它分支 —— version_conflict → 刷新后重试;name_conflict → 换个名字。
		if cf.Code != string(apierr.CodeVersionConflict) {
			t.Fatalf("409 的业务码应为 %q,实际 %q body=%s",
				apierr.CodeVersionConflict, cf.Code, recB.Body.String())
		}
		if reason, _ := cf.Details["reason"].(string); reason != filesvc.ReasonVersionConflict {
			t.Fatalf("409 的 reason 应为代码实际使用的 %q,实际 %q(details=%v)",
				filesvc.ReasonVersionConflict, reason, cf.Details)
		}
		if got := mcDetailInt(cf.Details["server_version"]); got != v0+1 {
			t.Fatalf("409 必须带胜出后的服务端版本 %d,实际 %v(details=%v)",
				v0+1, cf.Details["server_version"], cf.Details)
		}
		if etag, _ := cf.Details["server_etag"].(string); etag == "" || etag == a.ETag {
			t.Fatalf("409 必须带胜出后的 server_etag(且不同于本端持有的 %s),实际 %q", a.ETag, etag)
		}
		if at, _ := cf.Details["server_updated_at"].(string); at == "" {
			t.Fatalf("409 必须带 server_updated_at(客户端据此无需再查一次就能显示远端状态),实际 details=%v", cf.Details)
		}

		// 恰好一次写入:版本只前进一格,败方的名字没有落到服务端
		final := e.fileView(t, e.tokenB, id)
		if final.Version != v0+1 {
			t.Fatalf("同改同文件只应产生一次写入(v%d → v%d),实际 v%d", v0, v0+1, final.Version)
		}
		if final.Name != nameA {
			t.Fatalf("服务端应暴露胜者的新名字 %q,实际 %q(败方的名字 %q 不得落库)",
				nameA, final.Name, nameB)
		}
	})

	// 第二条更新形态:**并发定稿同名**。上传/覆盖写这条路径上的冲突来自
	// finalize 的 upsertFileRow(没有 base_version 可用),它的纪律是
	// "**绝不静默覆盖**":目标名字已被占 → 409,先到者的内容原封不动。
	t.Run("Conflict_并发定稿同名被拒且不静默覆盖", func(t *testing.T) {
		e := setupMultiClient(t)
		winnerData := []byte("A 的内容(先占住名字)")
		winnerID, winnerHash, v0 := e.upload(t, e.tokenA, "原文件.txt", winnerData)

		// 客户端 B 先为「并发定稿.txt」建任务(此时没有同名 files 行,建任务成功)
		loserData := []byte("B 要落到「并发定稿.txt」的内容")
		upID, ticket := e.createUpload(t, e.tokenB, "并发定稿.txt", len(loserData))

		// 客户端 A 在 B 定稿之前把名字占掉(改名只查 files 行,不查进行中的任务)
		rec := doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/files/"+winnerID, e.tokenA,
			map[string]any{"name": "并发定稿.txt", "base_version": v0})
		if rec.Code != http.StatusOK {
			t.Fatalf("A 改名为目标名应 200,实际 %d body=%s", rec.Code, rec.Body.String())
		}

		// B 传满 → 定稿时才发现同名:必须 409 name_conflict
		prec := e.patchUpload(t, e.tokenB, upID, ticket, 0, loserData)
		if prec.Code != http.StatusConflict {
			t.Fatalf("并发定稿同名应 409(绝不静默覆盖),实际 %d body=%s", prec.Code, prec.Body.String())
		}
		cf := mcErrOf(t, prec)
		if cf.Code != string(apierr.CodeNameConflict) {
			t.Fatalf("并发定稿同名的业务码应为 %q,实际 %q body=%s",
				apierr.CodeNameConflict, cf.Code, prec.Body.String())
		}

		// 胜者(A)的名字与内容都没被改动
		winner := e.fileView(t, e.tokenA, winnerID)
		if winner.Name != "并发定稿.txt" || winner.HashSHA256 != winnerHash {
			t.Fatalf("先到者应是胜者,其名字/内容不得被后到的定稿改掉:期望 (%s, %s),实际 (%s, %s)",
				"并发定稿.txt", winnerHash, winner.Name, winner.HashSHA256)
		}
		// 库里该名字只有一行,且内容是 A 的(B 的字节没有被登记成文件)
		var rows int
		var storedHash string
		if err := e.dbase.Pool.QueryRow(context.Background(), `
SELECT count(*), coalesce(max(hash_sha256),'') FROM files
 WHERE space_id = $1 AND parent_id = $2 AND lower(name) = lower($3)`,
			e.spaceID, e.rootID, "并发定稿.txt").Scan(&rows, &storedHash); err != nil {
			t.Fatalf("统计同名文件行失败: %v", err)
		}
		if rows != 1 || storedHash != winnerHash {
			t.Fatalf("该名字应恰有 1 行且内容为先到者的 %s,实际 %d 行 / %s", winnerHash, rows, storedHash)
		}
		var loserRows int
		if err := e.dbase.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM files WHERE hash_sha256 = $1`, mcSHA256(loserData)).Scan(&loserRows); err != nil {
			t.Fatalf("统计败方内容行失败: %v", err)
		}
		if loserRows != 0 {
			t.Fatalf("败方(未被采纳)的字节不得被登记成文件,实际有 %d 行", loserRows)
		}
		// B 的任务仍为 reserved、定稿用的暂存文件保留 —— 这是 tus.go 文档化的失败语义:
		// 冲突解决后(例如对方把名字让开)再发一次 PATCH 就能定稿,不必重传这几个字节。
		// 注意"取消任务"会删掉暂存文件,所以这份残留只对"重试"有意义。
		var state string
		if err := e.dbase.Pool.QueryRow(context.Background(),
			`SELECT state FROM uploads WHERE id = $1`, upID).Scan(&state); err != nil {
			t.Fatalf("读任务状态失败: %v", err)
		}
		if state != model.UploadReserved {
			t.Fatalf("定稿失败后任务应仍为 reserved(可重试),实际 %s", state)
		}
		if n, _ := e.store.StageSize(upID); n != int64(len(loserData)) {
			t.Fatalf("定稿失败后暂存文件应保留 %d 字节(否则客户端要重传),实际 %d", len(loserData), n)
		}
	})

	t.Run("Conflict冲突副本_同目录不同名可共存且都可读", func(t *testing.T) {
		e := setupMultiClient(t)
		serverData := []byte("服务端这一版的内容")
		originID, originHash, _ := e.upload(t, e.tokenA, "季度报告.txt", serverData)

		// ① 服务端**拒绝**同目录同名:这正是"客户端必须自己造冲突副本名"的前提
		//    (6.7:服务器是唯一裁判,绝不静默改名)
		crec, _ := e.uploadRaw(t, e.tokenB, "季度报告.txt", []byte("本地这一版的内容"))
		if crec.Code != http.StatusConflict {
			t.Fatalf("同目录同名上传应 409,实际 %d body=%s", crec.Code, crec.Body.String())
		}
		nameErr := mcErrOf(t, crec)
		if nameErr.Code != string(apierr.CodeNameConflict) {
			t.Fatalf("同名声明的业务码应为 %q,实际 %q body=%s",
				apierr.CodeNameConflict, nameErr.Code, crec.Body.String())
		}

		// ② 冲突副本名由**客户端策略**决定(服务端版本占规范名,本地那份改名保留)。
		//    命名算法服务端另有一份权威实现(namepolicy.ConflictSuffix,由双端共享夹具
		//    desktop/testdata/conflict-cases.json 锁住),客户端只是它的逐字节镜像。
		pol := namepolicy.Default()
		const ts = "20260214T101500"
		conflictName := pol.ConflictSuffix("季度报告.txt", ts)
		if conflictName == "季度报告.txt" {
			t.Fatalf("冲突副本名必须与原名字不同,实际 %q", conflictName)
		}
		if again := pol.ConflictSuffix("季度报告.txt", ts); again != conflictName {
			t.Fatalf("冲突命名必须确定(否则每次重试都会在用户目录里造一个新副本):%q vs %q", conflictName, again)
		}
		if _, ok := pol.Validate(conflictName); !ok {
			t.Fatalf("冲突副本名必须能通过服务端命名校验,实际 %q", conflictName)
		}
		if !strings.Contains(conflictName, "_conflict_") {
			t.Fatalf("冲突副本名应含客户端策略使用的 _conflict_ 后缀,实际 %q", conflictName)
		}

		// ③ 服务端侧必须允许它落地:同目录 + 不同名 → 两份共存
		localData := []byte("本地这一版的内容")
		conflictID, conflictHash, _ := e.upload(t, e.tokenB, conflictName, localData)
		if conflictID == originID {
			t.Fatalf("冲突副本必须是**另一行**(FileId 不能复用):两份都是 %s", originID)
		}
		if conflictHash == originHash {
			t.Fatalf("两份内容不同,哈希不该相同(都是 %s) —— 前置数据有问题", conflictHash)
		}

		entries := e.dirEntries(t, e.tokenA, e.rootID)
		for _, name := range []string{"季度报告.txt", conflictName} {
			if _, ok := entries[name]; !ok {
				t.Fatalf("同目录下两份都必须存在,缺 %q(实际条目=%v)", name, mcNames(entries))
			}
		}
		if entries["季度报告.txt"].ID == entries[conflictName].ID {
			t.Fatalf("两份应是不同文件,实际同 id=%s", entries["季度报告.txt"].ID)
		}

		// ④ 两份都必须可下载且各自内容正确(另一端也能读)
		if code, got := e.content(t, e.tokenB, originID); code != http.StatusOK || !bytes.Equal(got, serverData) {
			t.Fatalf("服务端版本应可下载且内容正确:状态=%d 内容=%q", code, got)
		}
		if code, got := e.content(t, e.tokenA, conflictID); code != http.StatusOK || !bytes.Equal(got, localData) {
			t.Fatalf("冲突副本应可下载且内容正确:状态=%d 内容=%q", code, got)
		}
	})

	t.Run("Rename经FileId识别不重传", func(t *testing.T) {
		e := setupMultiClient(t)
		data := bytes.Repeat([]byte("内容寻址-改名不重传:"), 512)
		id, hash, v0 := e.upload(t, e.tokenA, "要改名的文件.bin", data)

		// 上传完成后先记下"对象身份"(键 + 引用计数 + 物理对象个数)
		keyBefore := e.store.KeyFor(hash)
		objBefore := e.objectOf(t, hash)
		rowsBefore := e.objectRowCount(t, hash)
		filesBefore := e.objectFileCount(t)
		if objBefore.RefCount != 1 {
			t.Fatalf("前置不成立:新上传的对象 ref_count 应为 1,实际 %d", objBefore.RefCount)
		}

		// ① 纯改名:FileId 与内容身份必须保持,且**不产生任何对象写**
		rec := doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/files/"+id, e.tokenA,
			map[string]any{"name": "改名后的文件.bin", "base_version": v0})
		if rec.Code != http.StatusOK {
			t.Fatalf("改名应 200,实际 %d body=%s", rec.Code, rec.Body.String())
		}
		var renamed mcEntry
		if err := jsonUnmarshal(rec.Body.Bytes(), &renamed); err != nil {
			t.Fatalf("解析改名响应失败: %v body=%s", err, rec.Body.String())
		}
		if renamed.ID != id {
			t.Fatalf("改名后 FileId 必须不变(客户端靠它识别「还是同一个文件」):期望 %s,实际 %s", id, renamed.ID)
		}
		if renamed.HashSHA256 != hash {
			t.Fatalf("改名不改变内容身份:期望 hash %s,实际 %s", hash, renamed.HashSHA256)
		}
		if renamed.ParentID != e.rootID || renamed.Name != "改名后的文件.bin" || renamed.Version != v0+1 {
			t.Fatalf("改名后应为 (root, 改名后的文件.bin, v%d),实际 (%s, %s, v%d)",
				v0+1, renamed.ParentID, renamed.Name, renamed.Version)
		}
		// 库层:对象行原封不动 —— ref_count 不涨就意味着"没有重新上传/登记同一份内容"
		if got := e.objectOf(t, hash); got != objBefore {
			t.Fatalf("纯改名不得改动 file_objects 行:改名前 %+v,改名后 %+v(键变了就意味着对象被重新 put)", objBefore, got)
		}
		if n := e.objectRowCount(t, hash); n != rowsBefore {
			t.Fatalf("纯改名不得新增对象行:改名前 %d 行,改名后 %d 行", rowsBefore, n)
		}
		if got := e.store.KeyFor(hash); got != keyBefore {
			t.Fatalf("对象键必须不变:期望 %s,实际 %s", keyBefore, got)
		}
		if n := e.objectFileCount(t); n != filesBefore {
			t.Fatalf("纯改名不得新增物理对象文件:改名前 %d 个,改名后 %d 个", filesBefore, n)
		}

		// ② 移动(换目录):同样不重传
		dirID := e.mkdir(t, "归档目录")
		recMove := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/move", e.tokenA,
			map[string]any{"parent_id": dirID, "base_version": renamed.Version})
		if recMove.Code != http.StatusOK {
			t.Fatalf("移动应 200,实际 %d body=%s", recMove.Code, recMove.Body.String())
		}
		// 移动返回的是 {entry, async} 包装(小文件走同步路径,entry 非空)
		var moveRes struct {
			Entry *mcEntry `json:"entry"`
			Async bool     `json:"async"`
		}
		if err := jsonUnmarshal(recMove.Body.Bytes(), &moveRes); err != nil {
			t.Fatalf("解析移动响应失败: %v body=%s", err, recMove.Body.String())
		}
		if moveRes.Async || moveRes.Entry == nil {
			t.Fatalf("小文件移动应走同步路径并返回 entry,实际 async=%v entry=%+v body=%s",
				moveRes.Async, moveRes.Entry, recMove.Body.String())
		}
		moved := *moveRes.Entry
		if moved.ID != id || moved.HashSHA256 != hash {
			t.Fatalf("移动后 FileId/内容身份必须不变:期望 id=%s hash=%s,实际 id=%s hash=%s",
				id, hash, moved.ID, moved.HashSHA256)
		}
		if moved.ParentID != dirID {
			t.Fatalf("移动后父目录应为 %s,实际 %s", dirID, moved.ParentID)
		}
		if got := e.objectOf(t, hash); got != objBefore {
			t.Fatalf("纯移动不得改动 file_objects 行:移动前 %+v,移动后 %+v", objBefore, got)
		}
		if n := e.objectRowCount(t, hash); n != rowsBefore {
			t.Fatalf("纯移动不得新增对象行:移动前 %d 行,移动后 %d 行", rowsBefore, n)
		}
		if got := e.store.KeyFor(hash); got != keyBefore {
			t.Fatalf("移动后对象键必须不变:期望 %s,实际 %s", keyBefore, got)
		}
		if n := e.objectFileCount(t); n != filesBefore {
			t.Fatalf("纯移动不得新增物理对象文件:移动前 %d 个,移动后 %d 个", filesBefore, n)
		}

		// ③ 内容仍可读且逐字节相同:这就是"改名/移动不必重传"对用户的意义
		if code, got := e.content(t, e.tokenB, id); code != http.StatusOK || !bytes.Equal(got, data) {
			t.Fatalf("改名+移动后内容应可原样下载:状态=%d 字节数=%d(期望 %d)", code, len(got), len(data))
		}
		// 内容身份不变、ETag 随版本变 —— 客户端据此区分"元数据变了"与"内容要重传"
		if moved.ETag == renamed.ETag {
			t.Fatalf("移动后 version 变了,ETag 也应随之变化(实际两者都是 %s)", moved.ETag)
		}
	})
}
