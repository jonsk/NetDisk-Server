package middleware_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/middleware"
	"github.com/netdisk/netdisk/internal/ratelimit"
	"github.com/netdisk/netdisk/internal/reqctx"
)

func newLogins(t *testing.T) (*middleware.Middleware, *cache.Client, *ratelimit.Limiter) {
	t.Helper()
	return nil, nil, nil // 占位,便于下方 helper 复用签名
}

func setupLimiter(t *testing.T) (*cache.Client, *ratelimit.Limiter, func(time.Duration)) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	c := cache.New(rdb, "netdisk:")
	lim := ratelimit.New(rdb)
	now := time.Now()
	lim.SetClock(func() time.Time { return now })
	return c, lim, func(d time.Duration) { now = now.Add(d) }
}

// 超限必须返回 429 + Retry-After,且响应体是结构化错误
func TestRateLimitReturns429(t *testing.T) {
	c, lim, _ := setupLimiter(t)
	scope := middleware.Scope{
		Name:      "login",
		PerSecond: ratelimit.Limit{Window: time.Second, Max: 2},
		PerMinute: ratelimit.Limit{Window: time.Minute, Max: 100},
		Cost:      1,
		ByIP:      true,
	}
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.RequestID(),
		middleware.RealIP([]string{"127.0.0.1"}),
		middleware.RateLimit(c, lim, scope),
	)

	do := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := 1; i <= 2; i++ {
		if rec := do(); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次应放行, got %d", i, rec.Code)
		}
	}
	rec := do()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 3 次应 429, got %d body=%s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("429 必须带 Retry-After")
	} else if n, err := strconv.Atoi(ra); err != nil || n < 1 || n > 2 {
		t.Errorf("Retry-After 应为 1~2 秒, got %q", ra)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("429 应为结构化 JSON: %v", err)
	}
	if body["code"] != "rate_limited" {
		t.Errorf("业务码应为 rate_limited, got %v", body["code"])
	}
	if body["request_id"] == nil || body["request_id"] == "" {
		t.Error("429 也应带 request_id")
	}
}

// 已认证请求按 userID 维度限速(而非 IP),避免同一 NAT 出口互相影响
func TestRateLimitKeysByUser(t *testing.T) {
	c, lim, _ := setupLimiter(t)
	scope := middleware.Scope{
		Name:      "upload",
		PerSecond: ratelimit.Limit{Window: time.Second, Max: 1},
		PerMinute: ratelimit.Limit{Window: time.Minute, Max: 100},
		ByIP:      true,
	}
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.RateLimit(c, lim, scope),
	)

	call := func(userID string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upload/create", nil)
		req.RemoteAddr = "203.0.113.5:1234" // 同一 IP
		if userID != "" {
			req = req.WithContext(reqctx.WithActor(req.Context(), &reqctx.Actor{UserID: userID, Audience: "desktop"}))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := call("user-1"); code != http.StatusOK {
		t.Fatalf("user-1 首次应放行: %d", code)
	}
	if code := call("user-1"); code != http.StatusTooManyRequests {
		t.Fatalf("user-1 第二次应 429: %d", code)
	}
	// 同一 IP 另一个用户不应受影响 → 证明 key 用的是 userID
	if code := call("user-2"); code != http.StatusOK {
		t.Fatalf("user-2 不应受 user-1 影响: %d", code)
	}
}

// 窗口跨越后应恢复。
//
// 注意:必须让**两个窗口同时**卡住,否则分钟窗(100/min)会放行而看不到 429 ——
// 这正是"双窗口取更严者"的设计意图,本用例同时验证了这一点。
func TestRateLimitRecoversNextWindow(t *testing.T) {
	c, lim, advance := setupLimiter(t)
	scope := middleware.Scope{
		Name:      "download",
		PerSecond: ratelimit.Limit{Window: time.Second, Max: 1},
		PerMinute: ratelimit.Limit{Window: time.Minute, Max: 1},
		ByIP:      true,
	}
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		// RealIP 必须挂:否则拿不到 client IP,限速主体为空
		middleware.RealIP([]string{"127.0.0.1"}),
		middleware.RateLimit(c, lim, scope),
	)
	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/files/x/download", nil)
		req.RemoteAddr = "198.51.100.9:9999"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != http.StatusOK {
		t.Fatalf("首次应放行: %d", code)
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Fatalf("第二次应 429: %d", code)
	}
	// 分钟窗设定为 1/min,所以必须推进超过 1 分钟才会进入新窗口
	advance(61 * time.Second)
	if code := do(); code != http.StatusOK {
		t.Fatalf("新窗口应恢复: %d", code)
	}
}

// 装配了限速但拿不到主体(未挂 RealIP)时,必须显式报错而不是静默放行 ——
// 否则"限速看起来生效、实际没生效"这类问题会长期潜伏。
func TestRateLimitFailsLoudlyWithoutSubject(t *testing.T) {
	c, lim, _ := setupLimiter(t)
	scope := middleware.Scope{
		Name:      "login",
		PerSecond: ratelimit.Limit{Window: time.Second, Max: 1},
		PerMinute: ratelimit.Limit{Window: time.Minute, Max: 1},
		ByIP:      true,
	}
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		// 刻意不挂 RealIP
		middleware.RateLimit(c, lim, scope),
	)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("缺少限速主体时应 500 暴露装配错误, got %d", rec.Code)
	}
}

// 未注入 Cache/Limiter 时中间件必须透传(测试与降级场景)
func TestRateLimitPassthroughWhenUnconfigured(t *testing.T) {
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }),
		middleware.RateLimit(nil, nil, middleware.DefaultScope("x")),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("未配置限速时应放行: %d", rec.Code)
	}
}

// DefaultScope 的 key 片段必须合法(只允许 id/枚举),防止中间件因 key 非法而 500
func TestDefaultScopeKeyIsValid(t *testing.T) {
	c, _, _ := setupLimiter(t)
	scope := middleware.DefaultScope("login")
	if _, err := c.Key("rl", scope.Name, "u", "11111111-1111-7111-8111-111111111111"); err != nil {
		t.Fatalf("DefaultScope 生成的 key 片段应合法: %v", err)
	}
}

var _ = context.Background
var _ = newLogins
