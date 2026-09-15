package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/middleware"
	"github.com/netdisk/netdisk/internal/reqctx"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func newTestServer(t *testing.T) (http.Handler, *auth.Manager) {
	t.Helper()
	cfg := testConfig(t)
	tokens := newTokens(t, cfg)

	h := api.New(api.Deps{
		Cfg:    cfg,
		Log:    testLogger(),
		Tokens: tokens,
		// DB/Redis 置 nil:健康检查会跳过对应项(仅测路由与鉴权链)
	})
	return h, tokens
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func get(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 未带 token 的受保护接口必须 401 且返回结构化错误体
func TestAuthRequired(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/api/v1/me", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("应 401,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应应为 JSON: %v", err)
	}
	if body["code"] != "unauthorized" {
		t.Errorf("业务码应为 unauthorized,got %v", body["code"])
	}
	if body["request_id"] == "" || body["request_id"] == nil {
		t.Error("错误响应必须带 request_id(R-23)")
	}
}

// 带有效 token 应 200,且回显 audience
func TestAuthorizedMe(t *testing.T) {
	h, tokens := newTestServer(t)
	out, err := tokens.Issue(context.Background(), auth.IssueInput{
		UserID: "u-1", Username: "alice", Role: "user", TokenVersion: 1, Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	rec := get(h, "/api/v1/me", map[string]string{"Authorization": "Bearer " + out.AccessToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["audience"] != auth.AudienceDesktop {
		t.Errorf("audience 应为 desktop: %v", body["audience"])
	}
	if strings.Contains(rec.Body.String(), "permissions") {
		t.Error("响应不应含权限列表(R-13:权限由服务端判定)")
	}
}

// 2.7:H5 token 不得访问管理后台接口
func TestH5TokenCannotAccessAdmin(t *testing.T) {
	h, tokens := newTestServer(t)
	out, _ := tokens.Issue(context.Background(), auth.IssueInput{
		UserID: "u-1", Username: "alice", Role: "super_admin", TokenVersion: 1, Audience: auth.AudienceDesktop,
	})
	rec := get(h, "/api/v1/admin/ping", map[string]string{"Authorization": "Bearer " + out.AccessToken})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("H5 token 访问后台应 403,got %d body=%s", rec.Code, rec.Body.String())
	}

	// web audience + 足够的角色 → 放行
	out2, _ := tokens.Issue(context.Background(), auth.IssueInput{
		UserID: "u-2", Username: "admin", Role: "super_admin", TokenVersion: 1, Audience: auth.AudienceWeb,
	})
	rec2 := get(h, "/api/v1/admin/ping", map[string]string{"Authorization": "Bearer " + out2.AccessToken})
	if rec2.Code != http.StatusOK {
		t.Fatalf("web+super_admin 应 200,got %d body=%s", rec2.Code, rec2.Body.String())
	}

	// web audience + 普通用户 → 403(RequireRole)
	out3, _ := tokens.Issue(context.Background(), auth.IssueInput{
		UserID: "u-3", Username: "bob", Role: "user", TokenVersion: 1, Audience: auth.AudienceWeb,
	})
	rec3 := get(h, "/api/v1/admin/ping", map[string]string{"Authorization": "Bearer " + out3.AccessToken})
	if rec3.Code != http.StatusForbidden {
		t.Fatalf("普通用户访问后台应 403,got %d", rec3.Code)
	}
}

// request_id:优先复用上游(Nginx)传入的 id,并回写响应头
func TestRequestIDPropagation(t *testing.T) {
	h, _ := newTestServer(t)
	const upstream = "trace-abc-123"
	rec := get(h, "/healthz", map[string]string{middleware.RequestIDHeader: upstream})
	if got := rec.Header().Get(middleware.RequestIDHeader); got != upstream {
		t.Errorf("应复用上游 request_id: got %q", got)
	}
	// 未提供时应自动生成
	rec2 := get(h, "/healthz", nil)
	if got := rec2.Header().Get(middleware.RequestIDHeader); len(got) != 32 {
		t.Errorf("应自动生成 32 位 request_id,got %q", got)
	}
}

// 遍历防护:超长 request_id 必须被替换(防注入日志)
func TestRequestIDTooLong(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/healthz", map[string]string{middleware.RequestIDHeader: strings.Repeat("x", 200)})
	if got := rec.Header().Get(middleware.RequestIDHeader); len(got) != 32 {
		t.Errorf("超长 request_id 应被替换,got len=%d", len(got))
	}
}

// 健康检查:DB/Redis 未注入时应仍返回 200(仅报 ok)
func TestHealthzWithoutDeps(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,got %d", rec.Code)
	}
}

// /api/v1/version 必须给出桌面端协商所需字段(11 章)
func TestVersionEndpoint(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/api/v1/version", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	for _, k := range []string{"server_version", "min_client_version", "api_contract_version", "cursor_expire_days"} {
		if _, ok := body[k]; !ok {
			t.Errorf("缺少字段 %q: %s", k, rec.Body.String())
		}
	}
}

// R-01/R-02:/admin 由 Go 提供,index.html 必须 no-cache,assets 长缓存,SPA 回退
func TestWebUIEmbedAndCacheHeaders(t *testing.T) {
	h, _ := newTestServer(t)

	rec := get(h, "/admin/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("/admin/ 应 200,got %d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("index.html 必须 no-cache(R-02),got %q", cc)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type 应为 text/html,got %q", ct)
	}

	// SPA 回退:未知深层路由也返回 index.html
	spa := get(h, "/admin/files/123/detail", nil)
	if spa.Code != http.StatusOK || !strings.Contains(spa.Body.String(), "网盘管理后台") {
		t.Errorf("SPA 回退失败: %d %s", spa.Code, spa.Body.String()[:min(80, len(spa.Body.String()))])
	}

	// assets/ 未命中必须 404,不能把 HTML 当 JS 返回
	miss := get(h, "/admin/assets/missing.js", nil)
	if miss.Code != http.StatusNotFound {
		t.Errorf("assets 未命中应 404,got %d", miss.Code)
	}

	// 目录穿越防护
	trav := get(h, "/admin/..%2f..%2fgo.mod", nil)
	if trav.Code == http.StatusOK && strings.Contains(trav.Body.String(), "module ") {
		t.Error("目录穿越未被拦截")
	}
}

// SSE 端点必须声明不缓冲(3.2 职责 4b),且未实现时给出结构化提示
// SSE 是**接口**而不是匿名广播:未认证必须 401。
//
// 真实的 SSE 报文头(X-Accel-Buffering、text/event-stream)与帧格式由
// handlers_events_test.go 的**真实长连接**用例断言 —— 这里刻意不再用
// ResponseRecorder 断言流式行为:Recorder 的 Flush 是空操作,
// 会让"中间件吞掉 Flush"这类缺陷完全测不出来(实测踩过)。
func TestSSERequiresAuth(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/api/v1/events", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("SSE 未认证应 401,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 404 必须是结构化错误而不是空响应
func TestNotFoundStructured(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/api/v1/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("应 404,got %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("404 应为结构化 JSON: %v", err)
	}
	if body["code"] != "not_found" {
		t.Errorf("code 应为 not_found,got %v", body["code"])
	}
}

// 可信代理:仅当直连来自 127.0.0.1 时才采信 X-Forwarded-For(3.2 职责 3)
func TestRealIPTrustedProxy(t *testing.T) {
	cfg := config.Default()
	cfg.Server.TrustedProxies = []string{"127.0.0.1"}

	var seen string
	h := middleware.Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = reqctx.ClientIP(r.Context())
	}), middleware.RealIP(cfg.Server.TrustedProxies))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen != "203.0.113.9" {
		t.Errorf("可信代理下应取 XFF 首个地址,got %q", seen)
	}

	// 非可信来源:忽略 XFF,用 RemoteAddr
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.RemoteAddr = "198.51.100.7:4444"
	req2.Header.Set("X-Forwarded-For", "1.2.3.4")
	h.ServeHTTP(httptest.NewRecorder(), req2)
	if seen != "198.51.100.7" {
		t.Errorf("非可信来源必须忽略 XFF,got %q", seen)
	}
}

// Recoverer:panic 转 500,不泄露堆栈
func TestRecovererHidesPanic(t *testing.T) {
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom: secret detail") }),
		middleware.Recoverer(slog.New(slog.NewTextHandler(discard{}, nil))),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应转 500,got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") || strings.Contains(rec.Body.String(), "secret detail") {
		t.Error("不得把 panic 细节返回给客户端")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = time.Second
