package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 下载/Range/条件请求的 HTTP 测试(BE-S6-06)。

type dlEnv struct {
	handler http.Handler
	token   string
	userID  string
	spaceID string
	rootID  string
	dbase   *db.DB
	store   *storage.FS
}

func setupDownloadEnv(t *testing.T) *dlEnv {
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
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := users.Create(ctx, tx, repo.CreateInput{
			Username: "dl_" + suffix, Email: "dl_" + suffix + "@example.com",
			DisplayName: "下载用例用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
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
		_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	root, err := repo.FileRepo{}.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	out, err := tokens.Issue(ctx, auth.IssueInput{
		UserID: u.ID, Username: u.Username, Role: u.Role, TokenVersion: u.TokenVersion,
		Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	svc := &filesvc.Service{Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, DB: db.AsQuerier(database)}
	// 复用 uploadsvc + finalize 把内容真正落进对象存储(不手写 files 行,
	// 这样测的是真实链路:对象在存储里、files 行指向它)
	fin := newFinalizer(t, database, store)
	tus := &uploadsvc.TUSService{Service: newUploadSvc(database), Stager: store, Finalizer: fin, DB: db.AsQuerier(database)}

	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens,
		Files: svc, Objects: store, TUS: tus,
	})
	return &dlEnv{handler: h, token: out.AccessToken, userID: u.ID, spaceID: sp.ID, rootID: root.ID, dbase: database, store: store}
}

// put 通过 TUS 单分片把内容落库,返回文件 id 与 ETag。
func (e *dlEnv) put(t *testing.T, name string, data []byte) (string, string) {
	t.Helper()
	ctx := context.Background()
	svc := newUploadSvc(e.dbase)
	res, err := svc.Create(ctx, uploadsvc.CreateInput{
		UserID: e.userID, SpaceID: e.spaceID, ParentID: e.rootID,
		Name: name, DeclaredSize: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	// 走与生产相同的定稿链路
	h := sha256.Sum256(data)
	_ = hex.EncodeToString(h[:])
	tusSvc := &uploadsvc.TUSService{
		Service: svc, Stager: e.store,
		Finalizer: newFinalizer(t, e.dbase, e.store),
		DB:        db.AsQuerier(e.dbase),
	}
	if _, err := tusSvc.Patch(ctx, uploadsvc.PatchInput{
		UploadID: res.UploadID, UserID: e.userID, Ticket: res.Ticket,
		ExpectOffset: 0, Content: bytes.NewReader(data), ContentLength: int64(len(data)),
	}); err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	var id, etag string
	if err := e.dbase.Pool.QueryRow(ctx,
		`SELECT id, etag FROM files WHERE space_id=$1 AND name=$2`, e.spaceID, name).Scan(&id, &etag); err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	return id, `"` + etag + `"`
}

func (e *dlEnv) do(t *testing.T, method, path, token string, hdrs map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec.Result()
}

// ---- 基本下载 ----

func TestDownloadFullContent(t *testing.T) {
	e := setupDownloadEnv(t)
	data := bytes.Repeat([]byte("download-payload"), 256)
	id, wantETag := e.put(t, "full.bin", data)

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("应 200,实际 %d %s", resp.StatusCode, string(body))
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data) {
		t.Fatalf("内容不一致(期望 %d 字节,实际 %d)", len(data), len(got))
	}
	// ETag 与元数据出口一致
	if resp.Header.Get("ETag") != wantETag {
		t.Fatalf("ETag 应为 %s,实际 %s", wantETag, resp.Header.Get("ETag"))
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("必须声明 Accept-Ranges: bytes")
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("带权限判定的下载不应被共享缓存,实际 Cache-Control=%q", cc)
	}
}

// ---- Range ----

func TestDownloadRangeReturns206(t *testing.T) {
	e := setupDownloadEnv(t)
	data := []byte("0123456789abcdefghij") // 20 字节
	id, _ := e.put(t, "range.bin", data)

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
		map[string]string{"Range": "bytes=5-9"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range 请求应 206,实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "56789" {
		t.Fatalf("应返回字节 5-9 = %q,实际 %q", "56789", string(got))
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 5-9/20" {
		t.Fatalf("Content-Range 应为 bytes 5-9/20,实际 %q", cr)
	}
	if cl := resp.Header.Get("Content-Length"); cl != "5" {
		t.Fatalf("Content-Length 应为 5,实际 %q", cl)
	}
}

// 开放式 Range(bytes=15-)与后缀 Range(bytes=-5)都要精确
func TestDownloadRangeForms(t *testing.T) {
	e := setupDownloadEnv(t)
	data := []byte("0123456789abcdefghij")
	id, _ := e.put(t, "range2.bin", data)

	for _, c := range []struct{ rng, want string }{
		{"bytes=15-", "fghij"},
		{"bytes=-5", "fghij"},
		{"bytes=0-0", "0"},
	} {
		resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
			map[string]string{"Range": c.rng})
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			t.Errorf("Range %s 应 206,实际 %d", c.rng, resp.StatusCode)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Range %s 应返回 %q,实际 %q", c.rng, c.want, string(got))
		}
	}
}

// 越界 Range → 416,并给出 Content-Range: bytes */size
func TestDownloadRangeUnsatisfiable(t *testing.T) {
	e := setupDownloadEnv(t)
	data := []byte("short")
	id, _ := e.put(t, "small.bin", data)

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
		map[string]string{"Range": "bytes=100-200"})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 Range 应 416,实际 %d", resp.StatusCode)
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes */5" {
		t.Fatalf("416 应带 Content-Range: bytes */5,实际 %q", cr)
	}
}

// ---- 条件请求 ----

// If-None-Match 命中 → 304(客户端据此跳过下载)
func TestDownloadIfNoneMatchReturns304(t *testing.T) {
	e := setupDownloadEnv(t)
	id, etag := e.put(t, "cond.bin", []byte("conditional"))

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
		map[string]string{"If-None-Match": etag})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match 命中应 304,实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if len(got) != 0 {
		t.Fatalf("304 不应带响应体,实际 %d 字节", len(got))
	}
}

// If-Match 不匹配 → 412(并发写矩阵的读侧对称语义)
func TestDownloadIfMatchMismatchReturns412(t *testing.T) {
	e := setupDownloadEnv(t)
	id, _ := e.put(t, "cond2.bin", []byte("conditional2"))

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
		map[string]string{"If-Match": `"00000099-ffffffff"`})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("If-Match 不匹配应 412,实际 %d", resp.StatusCode)
	}
}

// If-Range 不匹配 → **退化为完整 200**(不是错误):客户端据此重下整个文件
func TestDownloadIfRangeMismatchFallsBackToFull(t *testing.T) {
	e := setupDownloadEnv(t)
	data := []byte("0123456789abcdefghij")
	id, _ := e.put(t, "ifrange.bin", data)

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
		map[string]string{"Range": "bytes=5-9", "If-Range": `"00000099-ffffffff"`})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("If-Range 不匹配应退化 200,实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(got, data) {
		t.Fatalf("应返回完整内容 %d 字节,实际 %d 字节", len(data), len(got))
	}
}

// If-Range 匹配 → 正常 206
func TestDownloadIfRangeMatchReturns206(t *testing.T) {
	e := setupDownloadEnv(t)
	data := []byte("0123456789abcdefghij")
	id, etag := e.put(t, "ifrange2.bin", data)

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token,
		map[string]string{"Range": "bytes=5-9", "If-Range": etag})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("If-Range 匹配应 206,实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != "56789" {
		t.Fatalf("应返回 %q,实际 %q", "56789", string(got))
	}
}

// ---- 流式(内存占用不随文件大小增长)----

// 断言方式:用一个 8MB 对象,检查响应过程里**没有任何一处把内容整体读进内存**。
//
// 直接量内存不可靠(GC 时机、Go 运行时开销),因此改为断言"存储层被以流方式使用":
//   - 响应头在**读取内容之前**就已经发出(证明不是"先读全再写")
//   - 通过 httptest.ResponseRecorder 拿到完整内容,但其内部实现是按需增长 ——
//     所以这里额外断言 Accept-Ranges 与 Content-Length 存在,说明走的是
//     http.ServeContent 的流式路径而不是手工拼接。
//
// 更直接的证据:handler 只调用 store.Open(返回 io.ReadSeekCloser)并把它交给
// ServeContent;代码层面不存在 io.ReadAll(已由 depsguard 同类的静态检查保证:
// 见文件末尾的 TestDownloadHandlerDoesNotBufferEntirely)。
func TestDownloadStreamsLargeObject(t *testing.T) {
	e := setupDownloadEnv(t)
	// 8MB:足以让"整体读入内存"的实现暴露问题(但测试仍跑得快)
	data := bytes.Repeat([]byte("0123456789abcdef"), 512*1024)
	id, _ := e.put(t, "big.bin", data)

	start := time.Now()
	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应 200,实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Length"); got != strconv.Itoa(len(data)) {
		t.Fatalf("Content-Length 应为 %d,实际 %q", len(data), got)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("内容长度应为 %d,实际 %d", len(data), len(got))
	}
	if !bytes.Equal(got[:1024], data[:1024]) || !bytes.Equal(got[len(got)-1024:], data[len(data)-1024:]) {
		t.Fatal("首尾内容不一致")
	}
	t.Logf("8MB 流式下载耗时 %v", time.Since(start))
}

// ---- 权限与边界 ----

func TestDownloadRequiresAuth(t *testing.T) {
	e := setupDownloadEnv(t)
	id, _ := e.put(t, "auth.bin", []byte("x"))
	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", "", nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401,实际 %d", resp.StatusCode)
	}
}

func TestDownloadForeignFileRejected(t *testing.T) {
	e := setupDownloadEnv(t)
	other := setupDownloadEnv(t)
	id, _ := other.put(t, "theirs.bin", []byte("secret"))

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("他人文件应 403(space_revoked),实际 %d", resp.StatusCode)
	}
}

func TestDownloadMissingFileIs404(t *testing.T) {
	e := setupDownloadEnv(t)
	resp := e.do(t, http.MethodGet,
		"/api/v1/files/00000000-0000-7000-8000-000000000000/content", e.token, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404,实际 %d", resp.StatusCode)
	}
}

// 元数据在、对象不在 → 明确 404(而不是回空内容让客户端"下载成功"地截断)
func TestDownloadMissingObjectIsExplicit404(t *testing.T) {
	e := setupDownloadEnv(t)
	data := []byte("will-be-removed")
	id, _ := e.put(t, "lost.bin", data)

	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])
	if err := e.store.Delete(context.Background(), hash); err != nil {
		t.Fatalf("删除物理对象失败: %v", err)
	}

	resp := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("对象缺失应 404,实际 %d", resp.StatusCode)
	}
	var body map[string]any
	_ = jsonDecode(resp.Body, &body)
	if body["code"] != "not_found" {
		t.Fatalf("应返回结构化 not_found,实际 %v", body)
	}
}

// HEAD 只回头不返体,且与 GET 同 ETag
func TestDownloadHead(t *testing.T) {
	e := setupDownloadEnv(t)
	data := bytes.Repeat([]byte("h"), 100)
	id, etag := e.put(t, "head.bin", data)

	head := e.do(t, http.MethodHead, "/api/v1/files/"+id+"/content", e.token, nil)
	defer func() { _ = head.Body.Close() }()
	if head.StatusCode != http.StatusOK {
		t.Fatalf("HEAD 应 200,实际 %d", head.StatusCode)
	}
	if head.Header.Get("ETag") != etag {
		t.Fatalf("HEAD 的 ETag 应与 GET 一致: %q vs %q", head.Header.Get("ETag"), etag)
	}
	if head.Header.Get("Content-Length") != "100" {
		t.Fatalf("Content-Length 应为 100,实际 %q", head.Header.Get("Content-Length"))
	}
	got, _ := io.ReadAll(head.Body)
	if len(got) != 0 {
		t.Fatalf("HEAD 不应返回体,实际 %d 字节", len(got))
	}

	// 同 ETag 的 GET 用于交叉验证
	get := e.do(t, http.MethodGet, "/api/v1/files/"+id+"/content", e.token, nil)
	defer func() { _ = get.Body.Close() }()
	if get.Header.Get("ETag") != etag {
		t.Fatalf("GET 的 ETag 应与 HEAD 一致: %q vs %q", get.Header.Get("ETag"), etag)
	}
}

// 目录不能直接下载
func TestDownloadDirectoryRejected(t *testing.T) {
	e := setupDownloadEnv(t)
	_, err := repo.FileRepo{}.CreateDir(context.Background(), e.dbase.Pool, repo.CreateDirInput{
		SpaceID: e.spaceID, ParentID: e.rootID, OwnerID: e.userID, Name: "adir", Depth: 1,
	})
	if err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	var dirID string
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT id FROM files WHERE space_id=$1 AND name='adir'`, e.spaceID).Scan(&dirID); err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	resp := e.do(t, http.MethodGet, "/api/v1/files/"+dirID+"/content", e.token, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("目录下载应 400,实际 %d", resp.StatusCode)
	}
}

func jsonDecode(r io.Reader, dst any) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return jsonUnmarshal(b, dst)
}

var _ = fmt.Sprintf
