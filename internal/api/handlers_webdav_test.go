package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/webdavauth"
)

// WebDAV 认证通道的 HTTP 层测试(BE-S1-06)。
type webdavEnv struct {
	handler http.Handler
	login   string
	pass    string
	dbase   *db.DB
}

func setupWebDAVEnv(t *testing.T) *webdavEnv {
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
	login := "wd_" + suffix
	pass := "P@ssw0rd" + time.Now().Format("05.000")

	pol := credentials.DefaultPolicy()
	pol.Cost = 4 // 测试加速
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := users.Create(ctx, tx, repo.CreateInput{
			Username: login, Email: login + "@example.com",
			DisplayName: "WebDAV 测试用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		hash, err := pol.Hash(pass)
		if err != nil {
			return err
		}
		if err := users.SetPassword(ctx, tx, created.ID, hash); err != nil {
			return err
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	authSvc := &authsvc.Service{
		Users: repo.UserRepo{}, Spaces: repo.SpaceRepo{}, Tokens: tokens, Policy: pol,
		DB: db.AsQuerier(database),
	}

	// 把账密校验接上:复用 authsvc.VerifyCredentials(防枚举/锁定只有一份实现)
	verify := func(ctx context.Context, username, password string) (string, int64, error) {
		v, err := authSvc.VerifyCredentials(ctx, username, password)
		if err != nil || v == nil {
			return "", 0, webdavauth.ErrUnauthorized
		}
		return v.ID, v.TokenVersion, nil
	}

	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens, Auth: authSvc,
		WebDAV: &webdavauth.Authenticator{
			Store:  &webdavauth.RedisStore{Cache: newCache(t, cfg.Redis.KeyPrefix)},
			Verify: verify,
		},
	})
	return &webdavEnv{handler: h, login: login, pass: pass, dbase: database}
}

func (e *webdavEnv) do(t *testing.T, method, path string, basicUser, basicPass string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if basicUser != "" || basicPass != "" {
		req.Header.Set("Authorization", webdavauth.EncodeBasic(basicUser, basicPass))
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// OPTIONS /webdav 免认证,用于能力探测(客户端靠 DAV 头判断是否支持 WebDAV)
func TestWebDAVOptionsAdvertisesDAV(t *testing.T) {
	e := setupWebDAVEnv(t)
	rec := e.do(t, http.MethodOptions, "/webdav", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("匿名 OPTIONS 应 200(能力探测),got %d", rec.Code)
	}
	if dav := rec.Header().Get("DAV"); !strings.Contains(dav, "1") {
		t.Fatalf("必须声明 DAV 能力,got %q", dav)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "PROPFIND") {
		t.Fatalf("Allow 应列出 DAV 方法,got %q", allow)
	}
}

// 未带凭据 → 401 + WWW-Authenticate(客户端据此弹密码框)
func TestWebDAVRequiresAuth(t *testing.T) {
	e := setupWebDAVEnv(t)
	rec := e.do(t, "PROPFIND", "/webdav/", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 PROPFIND 应 401,got %d body=%s", rec.Code, rec.Body.String())
	}
	if wa := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Basic ") {
		t.Fatalf("必须给出 Basic 挑战,got %q", wa)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("401 应为结构化 JSON: %v", err)
	}
	if body["code"] != "unauthorized" {
		t.Errorf("code 应为 unauthorized,got %v", body["code"])
	}
}

// 错误口令 → 401(且不回令牌)
func TestWebDAVWrongPassword(t *testing.T) {
	e := setupWebDAVEnv(t)
	rec := e.do(t, "PROPFIND", "/webdav/", e.login, "definitely-wrong")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错误口令应 401,got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-WebDAV-Token") != "" {
		t.Fatal("认证失败时不得回令牌")
	}
}

// 未知用户与错误口令返回完全相同的响应体形状(防用户枚举)
func TestWebDAVDoesNotEnumerateUsers(t *testing.T) {
	e := setupWebDAVEnv(t)
	unknown := e.do(t, "PROPFIND", "/webdav/", "no-such-user-"+e.login, "whatever")
	known := e.do(t, "PROPFIND", "/webdav/", e.login, "definitely-wrong")
	if unknown.Code != known.Code {
		t.Fatalf("未知用户与错口令状态码必须一致: %d vs %d", unknown.Code, known.Code)
	}
	var bu, bk map[string]any
	_ = json.Unmarshal(unknown.Body.Bytes(), &bu)
	_ = json.Unmarshal(known.Body.Bytes(), &bk)
	if bu["code"] != bk["code"] || bu["message"] != bk["message"] {
		t.Fatalf("未知用户与错口令响应必须一致: %v vs %v", bu, bk)
	}
}

// 正确凭据:认证通过 → 换票 → 501(方法集未实现,而不是 401)
//
// 断言"**不是 401**"很关键:若这里返回 401,说明认证链路有 bug,
// 客户端会陷入"密码明明是对的却一直弹框"的死循环。
//
// 注意断言方式:本 fixture **没有装配 DAV 后端**(无 Files/Objects),所以方法分发
// 会以 500「服务未装配」结束 —— 那仍然是"认证通过"的证据。刻意**不**断言 501:
// 方法集已经实现(BE-S8),用"501=未实现"当认证通过的标志会随实现推进而失真;
// 真正的方法集行为由 handlers_webdav_methods_test.go 的真实 HTTP 用例覆盖。
func TestWebDAVValidCredentialsExchangesToken(t *testing.T) {
	e := setupWebDAVEnv(t)
	rec := e.do(t, "PROPFIND", "/webdav/", e.login, e.pass)
	if rec.Code == http.StatusUnauthorized {
		t.Fatalf("凭据正确时不该 401,got %d body=%s", rec.Code, rec.Body.String())
	}
	token := rec.Header().Get("X-WebDAV-Token")
	if !webdavauth.IsSessionToken(token) {
		t.Fatalf("应回短期会话令牌,got %q", token)
	}
	if rec.Header().Get("X-WebDAV-Token-Expires-In") == "" {
		t.Error("应回令牌有效期,便于客户端决定何时重新输密码")
	}
	// 回归:未显式配置 TTL 时必须回落到 12h,不能是 "0"
	// (回 0 会让客户端认定凭据立即过期 → 输对密码也进不去)
	if got := rec.Header().Get("X-WebDAV-Token-Expires-In"); got != "43200" {
		t.Errorf("未配置时有效期应为默认 43200 秒,got %q", got)
	}
	if dav := rec.Header().Get("DAV"); dav == "" {
		t.Error("应声明 DAV 能力")
	}
}

// 用换来的令牌再发请求:必须同样认证通过(证明"后续跳不再校验账密")
func TestWebDAVSessionTokenReused(t *testing.T) {
	e := setupWebDAVEnv(t)
	first := e.do(t, "PROPFIND", "/webdav/", e.login, e.pass)
	token := first.Header().Get("X-WebDAV-Token")
	if token == "" {
		t.Fatalf("首次换票失败: %d %s", first.Code, first.Body.String())
	}

	second := e.do(t, "PROPFIND", "/webdav/", e.login, token)
	if second.Code == http.StatusUnauthorized {
		t.Fatalf("会话令牌应认证通过(非 401),got %d body=%s", second.Code, second.Body.String())
	}
	if second.Header().Get("X-WebDAV-Token") != "" {
		t.Error("命中会话时不应重复签发令牌")
	}
}

// 伪造会话令牌 → 401
func TestWebDAVFakeSessionTokenRejected(t *testing.T) {
	e := setupWebDAVEnv(t)
	fake := webdavauth.TokenPrefix + strings.Repeat("A", 43)
	rec := e.do(t, "PROPFIND", "/webdav/", e.login, fake)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("伪造令牌应 401,got %d body=%s", rec.Code, rec.Body.String())
	}
}

// token_version 变化(改密/强制下线)后,旧会话必须立即失效(R-13)
func TestWebDAVSessionInvalidatedByPasswordChange(t *testing.T) {
	e := setupWebDAVEnv(t)
	first := e.do(t, "PROPFIND", "/webdav/", e.login, e.pass)
	token := first.Header().Get("X-WebDAV-Token")
	if token == "" {
		t.Fatalf("首次换票失败: %d", first.Code)
	}

	// 模拟管理员改密:自增 token_version
	if _, err := e.dbase.Pool.Exec(context.Background(),
		`UPDATE users SET token_version = token_version + 1 WHERE username = $1`, e.login); err != nil {
		t.Fatalf("自增 token_version 失败: %v", err)
	}

	rec := e.do(t, "PROPFIND", "/webdav/", e.login, token)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("改密后旧会话必须失效,got %d body=%s", rec.Code, rec.Body.String())
	}
}
