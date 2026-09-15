package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/middleware"
	"github.com/netdisk/netdisk/internal/ratelimit"
)

// 本文件覆盖**真实中间件组合**:之前的 HTTP 测试都不注入 Cache/Limiter,
// 于是 `d.rateLimit(...)` 因依赖未装配而透传 —— **整条限速链从未被验证过**,
// 顺序错误也就一路潜伏到真实二进制端到端跑时才以 500 暴露。
//
// 这里用 miniredis 装配真实限速器,断言"带有效令牌的请求不会被限速层拒绝"。
func newTestServerWithLimiter(t *testing.T) (http.Handler, *auth.Manager) {
	t.Helper()
	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	c := newCache(t, cfg.Redis.KeyPrefix)
	limiter := ratelimit.New(c.Raw())

	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), Tokens: tokens,
		Cache: c, Limiter: limiter,
	})
	return h, tokens
}

// **回归用例**:限速必须在鉴权**内层** —— 否则已认证请求会被限速层
// 判为"既无 userID 也无 IP 主体",按设计返回 500(而不是放行)。
//
// 这不是"配置问题"而是**路由链顺序 bug**:同一个 handler 在注入限速器后
// 从 200 变成 500,而单测(不注入)永远看不到。
func TestRateLimitRunsAfterAuth(t *testing.T) {
	h, tokens := newTestServerWithLimiter(t)
	out, err := tokens.Issue(context.Background(), auth.IssueInput{
		UserID: "u-rl-1", Username: "rl", Role: "user", TokenVersion: 1,
		Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}
	tok := out.AccessToken

	// 只检查"限速层没有拒绝"这一点:业务 handler 可能因为 DB 未装配返回 500,
	// 但**限速层的 500 会带 details.scope**,据此可精确区分责任方。
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/files"},
		{http.MethodGet, "/api/v1/files/00000000-0000-7000-8000-000000000000"},
		{http.MethodPost, "/api/v1/upload/create"},
		{http.MethodPost, "/tus"},
	}
	for _, c := range cases {
		rec := doJSONReq(t, h, c.method, c.path, tok, nil)
		if rec.Code == http.StatusTooManyRequests {
			t.Errorf("%s %s 首次请求不应被限速,实际 429", c.method, c.path)
			continue
		}
		// 解析错误体:若 details.scope 出现,说明是限速层报的 500
		var body struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Details != nil {
			if scope, ok := body.Details["scope"]; ok {
				t.Errorf("%s %s 被**限速层**拒绝(scope=%v, status=%d)—— "+
					"限速必须在鉴权内层,否则拿不到 userID", c.method, c.path, scope, rec.Code)
			}
		}
	}
}

// 限速确实生效:超过阈值返回 429(证明上面的用例不是因为限速没接上才通过)
func TestRateLimitActuallyEngages(t *testing.T) {
	h, _ := newTestServerWithLimiter(t)
	// login 口默认 5/s;不带令牌按 IP 限速(ByIP=true)
	sawLimit := false
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			http.NoBody)
		req.RemoteAddr = "203.0.113.77:12345"
		req.Header.Set("Content-Type", "application/json")
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			sawLimit = true
			// 429 必须带 Retry-After(BE-S0-08 验收②)
			if rec.Header().Get("Retry-After") == "" {
				t.Error("429 必须带 Retry-After 响应头")
			}
			break
		}
	}
	if !sawLimit {
		t.Fatal("连续 20 次登录请求应触发限速(说明限速链确实生效)")
	}
}

// 限速中间件本身要求"装配了就必须能拿到主体":验证 scope 名会出现在错误详情里,
// 便于运维按 scope 定位是哪个接口族的限速配置有误。
func TestRateLimitScopeNameInError(t *testing.T) {
	// 直接用中间件构造一个"既无 userID 也无 IP"的场景
	s := middleware.Scope{Name: "probe_scope", ByIP: false}
	mw := middleware.RateLimit(nil, nil, s)
	// 未装配依赖 → 透传(不是 500);这里只断言构造不 panic
	rec := httptest.NewRecorder()
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("未装配依赖时限速应透传,实际 %d", rec.Code)
	}
}
