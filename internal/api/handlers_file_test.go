package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/syncfeed"
	"github.com/netdisk/netdisk/internal/syncsse"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 文件元数据接口的 HTTP 层测试(BE-S4-05 / R-21)。
//
// 重点:**ETag 在响应体、GET 头、HEAD 头三处必须字节一致** ——
// 客户端常用 HEAD 拿 ETag 再带 If-Match 写入,不一致会导致恒 412。

type fileEnv struct {
	// deps 保留装配出的依赖,供个别用例替换其中一项后重建 handler
	// (例如 SSE 用例需要可控的心跳与连接上限)。
	deps    api.Deps
	handler http.Handler
	token   string
	userID  string
	spaceID string
	rootID  string
	dbase   *db.DB
}

func setupFileEnv(t *testing.T) *fileEnv {
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

	suffix := time.Now().Format("150405.000000000")
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := users.Create(ctx, tx, repo.CreateInput{
			Username: "fh_" + suffix, Email: "fh_" + suffix + "@example.com",
			DisplayName: "文件接口测试用户", Role: model.RoleUser,
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
		_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	spaces := repo.SpaceRepo{}
	sp, err := spaces.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	files := repo.FileRepo{}
	root, err := files.GetRoot(ctx, database.Pool, sp.ID)
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

	// 上传入口需要 TUS/暂存/定稿这一套(与生产同一形状):
	// 少了它,`/upload/simple` 与 `/upload/{id}/finish` 会返回 500「服务未装配」——
	// 那会让"接口没装配"与"接口实现有 bug"混成同一个现象。
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	namePol := namepolicy.Default()
	uploadService := &uploadsvc.Service{
		Spaces: spaces, Files: files, Uploads: repo.UploadRepo{},
		DB: db.AsQuerier(database), Name: namePol,
	}
	finSvc := &finalize.Service{
		Pool: database.Pool, Storage: store,
		Spaces: spaces, Files: files, Name: namePol,
	}

	deps := api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens,
		Uploads: uploadService,
		Files: &filesvc.Service{
			Spaces: spaces, Files: files, DB: db.AsQuerier(database),
			// 装上真实变更流:所有写路径(改名/移动/删除/复制/共享)都会记一笔,
			// 于是"写路径必须记变更"这条纪律在 HTTP 层也被真的走了一遍。
			Feed: syncfeed.Writer{},
		},
		TUS: &uploadsvc.TUSService{
			Service: uploadService, Stager: store, Finalizer: finSvc, DB: db.AsQuerier(database),
		},
		Objects: store,
	}
	return &fileEnv{
		deps: deps, handler: api.New(deps), token: out.AccessToken, userID: u.ID,
		spaceID: sp.ID, rootID: root.ID, dbase: database,
	}
}

// setEvents 换掉 SSE 通道并重建 handler(测试需要可控心跳/连接上限)。
func (e *fileEnv) setEvents(hub *syncsse.Hub) {
	e.deps.Events = hub
	e.deps.Broadcaster = hub
	e.handler = api.New(e.deps)
}

func (e *fileEnv) mkFile(t *testing.T, name string, size, version int64) string {
	t.Helper()
	var id string
	err := e.dbase.Pool.QueryRow(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, version, depth, hash_sha256)
VALUES ($1, $2, $3, $4, false, $5, $6, 1, $7) RETURNING id`,
		e.spaceID, e.rootID, e.userID, name, size, version,
		fmt.Sprintf("%064x", len(name)+int(size)+int(version))).Scan(&id)
	if err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	return id
}

func TestFileListRequiresAuth(t *testing.T) {
	e := setupFileEnv(t)
	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401,got %d", rec.Code)
	}
}

func TestFileListHTTPContract(t *testing.T) {
	e := setupFileEnv(t)
	e.mkFile(t, "报表.xlsx", 2048, 4)

	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files", e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	for _, k := range []string{"space_id", "parent_id", "entries"} {
		if _, ok := body[k]; !ok {
			t.Errorf("缺少字段 %q: %s", k, rec.Body.String())
		}
	}
	entries := body["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("应有 1 个条目,got %d", len(entries))
	}
	entry := entries[0].(map[string]any)
	for _, k := range []string{"id", "name", "is_dir", "size", "mime_type", "version", "etag", "updated_at"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("条目缺少字段 %q: %v", k, entry)
		}
	}
	if entry["name"] != "报表.xlsx" {
		t.Errorf("名称应原样返回(UTF-8),got %v", entry["name"])
	}
	etag, _ := entry["etag"].(string)
	if !strings.HasPrefix(etag, `"00000004-`) || !strings.HasSuffix(etag, `"`) {
		t.Errorf("响应体 etag 应为带引号的裸值形式,got %q", etag)
	}
}

// R-21 契约:**HEAD 与 GET 的 ETag 响应头、以及响应体里的 etag 必须完全一致**
func TestFileETagConsistentAcrossExits(t *testing.T) {
	e := setupFileEnv(t)
	id := e.mkFile(t, "consistency.txt", 123, 9)

	get := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files/"+id, e.token, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET 应 200,got %d body=%s", get.Code, get.Body.String())
	}
	var view map[string]any
	_ = json.Unmarshal(get.Body.Bytes(), &view)
	bodyETag, _ := view["etag"].(string)
	headerETag := get.Header().Get("ETag")

	if bodyETag == "" || headerETag == "" {
		t.Fatalf("三处 ETag 都必须非空: body=%q header=%q", bodyETag, headerETag)
	}
	if bodyETag != headerETag {
		t.Fatalf("GET 响应体 etag(%q) 与响应头 ETag(%q) 不一致 —— 客户端条件请求会恒 412",
			bodyETag, headerETag)
	}

	head := doJSONReq(t, e.handler, http.MethodHead, "/api/v1/files/"+id, e.token, nil)
	if head.Code != http.StatusOK {
		t.Fatalf("HEAD 应 200,got %d", head.Code)
	}
	if headETag := head.Header().Get("ETag"); headETag != bodyETag {
		t.Fatalf("HEAD 的 ETag(%q) 与 GET 不一致(%q)", headETag, bodyETag)
	}
	if head.Body.Len() != 0 {
		t.Errorf("HEAD 不应返回响应体,got %d 字节", head.Body.Len())
	}
	// HEAD 还应给出同步需要的元数据头
	if head.Header().Get("Content-Length") != "123" {
		t.Errorf("HEAD 应带 Content-Length,got %q", head.Header().Get("Content-Length"))
	}
	if head.Header().Get("X-File-Version") != "9" {
		t.Errorf("HEAD 应带 X-File-Version,got %q", head.Header().Get("X-File-Version"))
	}
}

// 中文文件名必须能进响应头(编码后),否则部分客户端拒收整个响应
func TestFileHeadEncodesNonASCIIName(t *testing.T) {
	e := setupFileEnv(t)
	id := e.mkFile(t, "中文名称.txt", 1, 1)

	rec := doJSONReq(t, e.handler, http.MethodHead, "/api/v1/files/"+id, e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD 应 200,got %d", rec.Code)
	}
	name := rec.Header().Get("X-File-Name")
	if name == "" {
		t.Fatal("应带 X-File-Name")
	}
	// 头里必须是纯 ASCII(百分号编码)
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			t.Fatalf("响应头含非 ASCII 字节: %q", name)
		}
	}
	if !strings.Contains(name, "%") {
		t.Fatalf("中文名应被百分号编码,got %q", name)
	}
}

func TestFileGetMissingIs404Structured(t *testing.T) {
	e := setupFileEnv(t)
	rec := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/files/00000000-0000-7000-8000-000000000000", e.token, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404,got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("404 应为结构化 JSON: %v", err)
	}
	if body["request_id"] == nil {
		t.Error("错误响应必须带 request_id(R-23)")
	}
}

// 列表分页游标:HTTP 层把 next_after 原样回传即可翻页
func TestFileListPaginationOverHTTP(t *testing.T) {
	e := setupFileEnv(t)
	for i := 0; i < 5; i++ {
		e.mkFile(t, fmt.Sprintf("p%02d.txt", i), 1, 1)
	}
	first := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?limit=2", e.token, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("首页应 200,got %d", first.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &body)
	next, _ := body["next_after"].(string)
	if next == "" {
		t.Fatal("limit=2 时应返回 next_after")
	}
	if len(body["entries"].([]any)) != 2 {
		t.Fatalf("首页应有 2 个条目,got %d", len(body["entries"].([]any)))
	}

	second := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?limit=2&after="+next, e.token, nil)
	var body2 map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &body2)
	entries2 := body2["entries"].([]any)
	if len(entries2) != 2 {
		t.Fatalf("第二页应有 2 个条目,got %d", len(entries2))
	}
	// 两页不得重复
	seen := map[string]bool{}
	for _, raw := range append(body["entries"].([]any), entries2...) {
		n := raw.(map[string]any)["name"].(string)
		if seen[n] {
			t.Fatalf("分页出现重复条目 %s", n)
		}
		seen[n] = true
	}
}

// limit 非法 → 400(而不是静默用默认值)
func TestFileListRejectsBadLimit(t *testing.T) {
	e := setupFileEnv(t)
	for _, bad := range []string{"0", "-1", "abc"} {
		rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?limit="+bad, e.token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s 应 400,got %d", bad, rec.Code)
		}
	}
}
