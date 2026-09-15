package api_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 上传端点的 HTTP 层测试:装配真实数据库 + 真实限速旁路(不注入 Cache/Limiter),
// 只验证"HTTP 契约"部分 —— 状态码、错误体形状、头部、审计。
// 业务语义(额度、冲突、ticket)在 uploadsvc 的集成测试里覆盖。

type uploadEnv struct {
	handler http.Handler
	token   string
	userID  string
	spaceID string
	rootID  string
	dbase   *db.DB
}

func setupUploadEnv(t *testing.T) *uploadEnv {
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

	suffix := time.Now().Format("150405.000000")
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := users.Create(ctx, tx, repo.CreateInput{
			Username:    "hup_" + suffix,
			Email:       "hup_" + suffix + "@example.com",
			DisplayName: "上传接口测试用户",
			Role:        model.RoleUser,
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
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM uploads WHERE user_id = $1`, u.ID)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
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
	out, err := tokens.Issue(ctx, auth.IssueInput{
		UserID: u.ID, Username: u.Username, Role: u.Role, TokenVersion: u.TokenVersion,
		Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发测试令牌失败: %v", err)
	}

	svc := &uploadsvc.Service{
		Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Uploads: repo.UploadRepo{},
		DB:   db.AsQuerier(database),
		Name: namepolicy.Default(),
	}
	h := api.New(api.Deps{Cfg: cfg, Log: testLogger(), Tokens: tokens, Uploads: svc})

	return &uploadEnv{handler: h, token: out.AccessToken, userID: u.ID, spaceID: sp.ID, rootID: root.ID, dbase: database}
}

// do 发一个带 Bearer 令牌的请求。
func (e *uploadEnv) do(t *testing.T, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Authorization", "Bearer "+e.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// 建任务成功:201 + 契约字段齐全 + ticket 只在此处出现。
func TestUploadCreateHTTPContract(t *testing.T) {
	e := setupUploadEnv(t)

	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "报表 2026.xlsx", "size": 2048,
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("应 201,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	for _, k := range []string{"upload_id", "upload_ticket", "expires_at", "space_id", "parent_id", "name", "quota_after_bytes"} {
		if body[k] == nil {
			t.Errorf("缺少字段 %q: %s", k, rec.Body.String())
		}
	}
	if body["name"] != "报表 2026.xlsx" {
		t.Errorf("应回显归一后的文件名,got %v", body["name"])
	}
	// ticket 必须是一次性下发的随机值,不能被猜到
	tk, _ := body["upload_ticket"].(string)
	if len(tk) < 32 {
		t.Errorf("ticket 太短,可能可预测: %q", tk)
	}
}

// 未认证 → 401(数据面必须先过 access token)
func TestUploadCreateRequiresAuth(t *testing.T) {
	e := setupUploadEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload/create",
		bytes.NewReader([]byte(`{"name":"x.txt","size":1}`)))
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401,got %d", rec.Code)
	}
}

// 非法命名 → 400 invalid_name,且带结构化明细(6.7)
func TestUploadCreateRejectsInvalidName(t *testing.T) {
	e := setupUploadEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "bad:name.txt", "size": 10,
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法名应 400,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "invalid_name" {
		t.Fatalf("code 应为 invalid_name,got %v", body["code"])
	}
	details, ok := body["details"].(map[string]any)
	if !ok {
		t.Fatalf("应带 details(规则号/建议名),got %s", rec.Body.String())
	}
	if details["rule"] == nil || details["suggestion"] == nil {
		t.Errorf("details 应含 rule 与 suggestion,got %+v", details)
	}
	if body["request_id"] == nil {
		t.Error("错误响应必须带 request_id(R-23)")
	}
}

// 未知字段必须被拒(严格解码:客户端放错字段要立刻暴露)
func TestUploadCreateRejectsUnknownField(t *testing.T) {
	e := setupUploadEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "ok.txt", "size": 10,
		"ticket": "client-should-not-send-this",
	}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400,got %d body=%s", rec.Code, rec.Body.String())
	}
}

// 取消:204;重复取消仍 204(幂等)
func TestUploadCancelIsIdempotent(t *testing.T) {
	e := setupUploadEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "cancel-me.bin", "size": 100,
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("建任务应 201,got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["upload_id"].(string)
	if id == "" {
		t.Fatalf("未拿到 upload_id: %s", rec.Body.String())
	}

	first := e.do(t, http.MethodDelete, "/api/v1/upload/"+id, nil, nil)
	if first.Code != http.StatusNoContent {
		t.Fatalf("取消应 204,got %d body=%s", first.Code, first.Body.String())
	}
	second := e.do(t, http.MethodDelete, "/api/v1/upload/"+id, nil, nil)
	if second.Code != http.StatusNoContent {
		t.Fatalf("重复取消应仍 204(幂等),got %d body=%s", second.Code, second.Body.String())
	}
}

// HEAD 进度:无 ticket → 401;有 ticket → 200 + Tus/Upload-Offset 头
func TestUploadHeadRequiresTicket(t *testing.T) {
	e := setupUploadEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "resume.bin", "size": 4096,
	}, nil)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["upload_id"].(string)
	ticket, _ := created["upload_ticket"].(string)

	noTicket := e.do(t, http.MethodHead, "/api/v1/upload/"+id, nil, nil)
	if noTicket.Code != http.StatusUnauthorized {
		t.Fatalf("无 ticket 的 HEAD 应 401,got %d", noTicket.Code)
	}

	ok := e.do(t, http.MethodHead, "/api/v1/upload/"+id, nil, map[string]string{"X-Upload-Token": ticket})
	if ok.Code != http.StatusOK {
		t.Fatalf("带 ticket 的 HEAD 应 200,got %d", ok.Code)
	}
	if got := ok.Header().Get("Upload-Offset"); got != "0" {
		t.Errorf("初始 Upload-Offset 应为 0,got %q", got)
	}
	if got := ok.Header().Get("Upload-Length"); got != "4096" {
		t.Errorf("Upload-Length 应回显声明大小,got %q", got)
	}
	if got := ok.Header().Get("Tus-Resumable"); got != "1.0.0" {
		t.Errorf("应声明 Tus-Resumable: 1.0.0,got %q", got)
	}
	if cc := ok.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("上传进度必须 no-store,got %q", cc)
	}
}

// TUS Upload-Metadata 兼容:ticket 也可用标准 TUS 头传递
func TestUploadHeadAcceptsTusMetadata(t *testing.T) {
	e := setupUploadEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "tus.bin", "size": 128,
	}, nil)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["upload_id"].(string)
	ticket, _ := created["upload_ticket"].(string)

	md := "filename " + base64.StdEncoding.EncodeToString([]byte("tus.bin")) +
		",upload_ticket " + base64.StdEncoding.EncodeToString([]byte(ticket))
	ok := e.do(t, http.MethodHead, "/api/v1/upload/"+id, nil, map[string]string{"Upload-Metadata": md})
	if ok.Code != http.StatusOK {
		t.Fatalf("通过 Upload-Metadata 传 ticket 应 200,got %d body=%s", ok.Code, ok.Body.String())
	}
}

// 额度不足必须映射为 507 而不是 500(4.3:创建即拒)
func TestUploadCreateQuotaExceededIs507(t *testing.T) {
	e := setupUploadEnv(t)
	if _, err := e.dbase.Pool.Exec(context.Background(),
		`UPDATE spaces SET quota_bytes = 100, used_bytes = 0 WHERE id = $1`, e.spaceID); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "too-big.bin", "size": 4096,
	}, nil)
	if rec.Code != http.StatusInsufficientStorage {
		t.Fatalf("额度不足应 507,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "quota_exceeded" {
		t.Errorf("code 应为 quota_exceeded,got %v", body["code"])
	}
}

// 取消是幂等的:即使 id 属于别人(或根本不存在),也返回 204 且**不改变任何状态**。
//
// 为什么 HTTP 层故意不区分?—— 若对"别人的任务"返回 404、对"自己的任务"返回 204,
// 攻击者就能靠状态码枚举出哪些 upload_id 真实存在。越权的真实拦截在 service 层,
// 且已由 uploadsvc 的集成测试断言"状态未被改动"。
func TestUploadCancelDoesNotLeakForeignUpload(t *testing.T) {
	e := setupUploadEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "not-yours.bin", "size": 64,
	}, nil)
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id, _ := created["upload_id"].(string)

	other := setupUploadEnv(t)
	got := other.do(t, http.MethodDelete, "/api/v1/upload/"+id, nil, nil)
	if got.Code != http.StatusNoContent {
		t.Fatalf("取消(幂等语义)应 204,got %d body=%s", got.Code, got.Body.String())
	}
	// 关键断言:越权请求只是"静默成功",任务必须原封不动
	var state string
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT state FROM uploads WHERE id = $1`, id).Scan(&state); err != nil {
		t.Fatalf("读任务状态失败: %v", err)
	}
	if state != "reserved" {
		t.Fatalf("越权取消不得改变状态,期望 reserved,实际 %s", state)
	}
}

// 未认证的取消必须 401:数据面不允许匿名访问
func TestUploadCancelRequiresAuth(t *testing.T) {
	e := setupUploadEnv(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/upload/00000000-0000-7000-8000-000000000000", nil)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌取消应 401,got %d body=%s", rec.Code, rec.Body.String())
	}
}
