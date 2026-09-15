package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// ---- 带数据库的测试环境(通过 NETDISK_TEST_DSN 判定,缺失即 skip)----

type authFixture struct {
	router http.Handler
	userID string
	login  string
	pass   string
	dbase  *db.DB
	tokens *auth.Manager
	svc    *authsvc.Service
	mr     *miniredis.Miniredis
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func setupAuth(t *testing.T) *authFixture {
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
	login := "api_" + suffix
	pass := "P@ssw0rd" + time.Now().Format("05.000")

	pol := credentials.DefaultPolicy()
	pol.Cost = 4
	users := repo.UserRepo{}
	var created *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		u, err := users.Create(ctx, tx, repo.CreateInput{
			Username: login, Email: login + "@example.com", DisplayName: "API 测试用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		h, err := pol.Hash(pass)
		if err != nil {
			return err
		}
		if err := users.SetPassword(ctx, tx, u.ID, h); err != nil {
			return err
		}
		created = u
		return nil
	}); err != nil {
		t.Fatalf("建测试用户: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, created.ID)
	})

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := config.Default()
	cfg.JWT.Secret = "0123456789abcdef0123456789abcdef"
	cfg.Log.Level = "error"
	tokens := auth.NewManager(cfg.JWT, rdb, cfg.Redis.KeyPrefix)

	svc := &authsvc.Service{
		Users: users, Spaces: repo.SpaceRepo{}, Tokens: tokens, Policy: pol, DB: db.AsQuerier(database),
	}

	router := api.New(api.Deps{
		Cfg: cfg, Log: slog.New(slog.NewTextHandler(discardWriter{}, nil)),
		DB: database, Redis: rdb, Tokens: tokens, Auth: svc,
	})
	return &authFixture{
		router: router,
		userID: created.ID, login: login, pass: pass, dbase: database, tokens: tokens, svc: svc, mr: mr,
	}
}

func postJSON(t *testing.T, h http.Handler, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON(%d): %s", rec.Code, rec.Body.String())
	}
	return m
}

// 登录成功:返回双 token + 用户 + 个人空间;且审计留痕
func TestLoginEndpointSuccess(t *testing.T) {
	f := setupAuth(t)
	rec := postJSON(t, f.router, "/api/v1/auth/login", map[string]any{
		"login": f.login, "password": f.pass,
	}, map[string]string{"X-Client-Kind": "desktop"})
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)

	tok, ok := body["token"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 token: %s", rec.Body.String())
	}
	for _, k := range []string{"access_token", "refresh_token", "expires_in", "refresh_expires_in"} {
		if tok[k] == nil {
			t.Errorf("token 缺少字段 %q", k)
		}
	}
	if tok["audience"] != auth.AudienceDesktop {
		t.Errorf("audience 应为 desktop(X-Client-Kind), got %v", tok["audience"])
	}
	// 桌面端 access 应为 15 分钟
	if int(tok["expires_in"].(float64)) != 900 {
		t.Errorf("desktop access 应为 900s, got %v", tok["expires_in"])
	}
	// 个人空间必须返回,且配额为 0(不限制)
	sp, ok := body["personal_space"].(map[string]any)
	if !ok {
		t.Fatal("响应应含 personal_space")
	}
	if sp["kind"] != "personal" || sp["quota_bytes"].(float64) != 0 {
		t.Errorf("个人空间字段异常: %v", sp)
	}
	// R-13:响应不得出现任何权限列表
	for _, forbidden := range []string{"permissions", "spaces_list", "scopes"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("响应不应包含 %q", forbidden)
		}
	}
}

// 登录失败:401 且**不泄露账号是否存在**(防枚举)
func TestLoginEndpointFailureIsAudited(t *testing.T) {
	f := setupAuth(t)

	recBadPwd := postJSON(t, f.router, "/api/v1/auth/login",
		map[string]any{"login": f.login, "password": "wrongpass1"}, nil)
	recNoUser := postJSON(t, f.router, "/api/v1/auth/login",
		map[string]any{"login": "definitely_not_exists_xyz", "password": "wrongpass1"}, nil)

	if recBadPwd.Code != http.StatusUnauthorized || recNoUser.Code != http.StatusUnauthorized {
		t.Fatalf("两种失败都应 401, got %d / %d", recBadPwd.Code, recNoUser.Code)
	}
	// 对外错误**语义**必须一致(防枚举)。
	// 注意:request_id 必然不同,所以比对的是 code + message,而不是整个响应体。
	badBody := decodeBody(t, recBadPwd)
	noUserBody := decodeBody(t, recNoUser)
	if badBody["code"] != noUserBody["code"] || badBody["message"] != noUserBody["message"] {
		t.Errorf("错误响应语义必须一致(防枚举):\n  bad_pwd=%v\n  no_user=%v", badBody, noUserBody)
	}
	if badBody["code"] != "unauthorized" {
		t.Errorf("业务码应为 unauthorized, got %v", badBody["code"])
	}
	// 两侧都必须带 request_id,便于用户报障时定位(R-23)
	for _, b := range []map[string]any{badBody, noUserBody} {
		if b["request_id"] == nil || b["request_id"] == "" {
			t.Errorf("错误响应必须带 request_id: %v", b)
		}
	}
}

// 停用账号 → 403(4.1)
func TestLoginEndpointDisabledForbidden(t *testing.T) {
	f := setupAuth(t)
	ur := repo.UserRepo{}
	if err := ur.SetStatus(context.Background(), f.dbase.Pool, f.userID, model.StatusDisabled); err != nil {
		t.Fatalf("停用: %v", err)
	}
	rec := postJSON(t, f.router, "/api/v1/auth/login",
		map[string]any{"login": f.login, "password": f.pass}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("停用账号应 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// 严格 JSON:未知字段/空体/超大体 必须 400/413 而不是静默成功
func TestLoginEndpointStrictJSON(t *testing.T) {
	f := setupAuth(t)

	// 未知字段(把 password 拼错成 passwrd 这类问题必须在开发期暴露)
	rec := postJSON(t, f.router, "/api/v1/auth/login",
		map[string]any{"login": f.login, "passwrd": f.pass}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知字段应 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 空体
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(""))
	recEmpty := httptest.NewRecorder()
	f.router.ServeHTTP(recEmpty, req)
	if recEmpty.Code != http.StatusBadRequest {
		t.Errorf("空体应 400, got %d", recEmpty.Code)
	}

	// 超大体(> 64KB)
	huge := strings.Repeat("x", 70<<10)
	reqHuge := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"login":"`+huge+`","password":"x"}`))
	recHuge := httptest.NewRecorder()
	f.router.ServeHTTP(recHuge, reqHuge)
	if recHuge.Code != http.StatusRequestEntityTooLarge && recHuge.Code != http.StatusBadRequest {
		t.Errorf("超大体应 413/400, got %d", recHuge.Code)
	}

	// 缺少必填字段
	recMissing := postJSON(t, f.router, "/api/v1/auth/login", map[string]any{"login": f.login}, nil)
	if recMissing.Code != http.StatusBadRequest {
		t.Errorf("缺 password 应 400, got %d", recMissing.Code)
	}
}

// refresh 轮换:旧 refresh 立刻失效,新 refresh 可用;登出后失效
func TestRefreshAndLogoutEndpoints(t *testing.T) {
	f := setupAuth(t)

	login := postJSON(t, f.router, "/api/v1/auth/login",
		map[string]any{"login": f.login, "password": f.pass}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("登录失败: %d", login.Code)
	}
	tok := decodeBody(t, login)["token"].(map[string]any)
	first := tok["refresh_token"].(string)

	// 第一次刷新成功
	r1 := postJSON(t, f.router, "/api/v1/auth/refresh", map[string]any{"refresh_token": first}, nil)
	if r1.Code != http.StatusOK {
		t.Fatalf("refresh 应 200, got %d body=%s", r1.Code, r1.Body.String())
	}
	sp2 := decodeBody(t, r1)["token"].(map[string]any)
	second, _ := sp2["refresh_token"].(string)
	if second == "" {
		t.Fatalf("刷新响应缺少 refresh_token: %s", r1.Body.String())
	}
	if second == first {
		t.Error("refresh 轮换后应返回**新的** refresh token")
	}

	// 旧 refresh 立即失效
	rOld := postJSON(t, f.router, "/api/v1/auth/refresh", map[string]any{"refresh_token": first}, nil)
	if rOld.Code != http.StatusUnauthorized {
		t.Errorf("已轮换的旧 refresh 应 401, got %d", rOld.Code)
	}
	var oldBody map[string]any
	_ = json.Unmarshal(rOld.Body.Bytes(), &oldBody)
	if oldBody["code"] != "token_revoked" {
		t.Errorf("应返回 token_revoked, got %v", oldBody["code"])
	}

	// 登出 → 204,之后 refresh 失效
	lo := postJSON(t, f.router, "/api/v1/auth/logout", map[string]any{"refresh_token": second}, nil)
	if lo.Code != http.StatusNoContent {
		t.Fatalf("登出应 204, got %d", lo.Code)
	}
	rAfter := postJSON(t, f.router, "/api/v1/auth/refresh", map[string]any{"refresh_token": second}, nil)
	if rAfter.Code != http.StatusUnauthorized {
		t.Errorf("登出后 refresh 应 401, got %d", rAfter.Code)
	}

	// 空 refresh 的登出必须幂等成功
	loEmpty := postJSON(t, f.router, "/api/v1/auth/logout", map[string]any{}, nil)
	if loEmpty.Code != http.StatusNoContent {
		t.Errorf("空 token 登出应 204(幂等), got %d", loEmpty.Code)
	}
}

var _ = api.BuildVersion
