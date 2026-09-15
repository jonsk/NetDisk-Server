package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/middleware"
)

// BE-S0-04 验收③:**方法不符返回 405 + Allow,未匹配路由返回 404**。
//
// 这条曾经真实失效过:根路径注册了 `mux.HandleFunc("/", ...)` 兜底,
// 它匹配一切未命中路径,于是"路径对、方法不对"的请求被兜底接走返回 404,
// 把 ServeMux 内建的 405 + Allow 语义彻底吞掉。
// 后果是客户端无法区分"接口不存在"与"用错了方法" ——
// 典型症状是"明明有这个接口却一直 404",排查方向被带偏。
func TestMethodNotAllowedReturns405WithAllow(t *testing.T) {
	h, _ := newTestServer(t)

	cases := []struct {
		method, path string
	}{
		{http.MethodPatch, "/healthz"},
		{http.MethodDelete, "/api/v1/version"},
		{http.MethodPost, "/api/v1/files"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s 应 405,实际 %d body=%s", c.method, c.path, rec.Code, rec.Body.String())
			continue
		}
		// Allow 头是 405 语义的核心:客户端据此知道该用哪个方法
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("%s %s 的 405 必须带 Allow 头", c.method, c.path)
		}
		// 响应体必须与其它错误同构(JSON + code + request_id),不能是纯文本
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s %s 的 405 应为结构化 JSON,实际 %q", c.method, c.path, rec.Body.String())
			continue
		}
		if body["code"] != "method_not_allowed" {
			t.Errorf("code 应为 method_not_allowed,实际 %v", body["code"])
		}
		if body["request_id"] == nil {
			t.Errorf("405 响应必须带 request_id(R-23)")
		}
		// 纯文本体不得残留(出现过"结构化体 + ServeMux 文本体"拼接则这里会命中)
		if strings.Contains(rec.Body.String(), "Method Not Allowed\n") {
			t.Errorf("不应残留 ServeMux 的纯文本体: %q", rec.Body.String())
		}
	}
}

// 未匹配路径仍是 404,且不与 405 混淆
func TestUnmatchedPathIs404Not405(t *testing.T) {
	h, _ := newTestServer(t)
	for _, p := range []string{"/api/v1/does-not-exist", "/api/v1/nope/deep/path"} {
		rec := get(h, p, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 应 404,实际 %d", p, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != "" {
			t.Errorf("%s 是 404,不应带 Allow 头(实际 %q)", p, allow)
		}
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["code"] != "not_found" {
			t.Errorf("code 应为 not_found,实际 %v", body["code"])
		}
	}
}

// 根路径仍可访问(收窄兜底后不能连根都挂掉)
func TestRootPathStillServed(t *testing.T) {
	h, _ := newTestServer(t)
	rec := get(h, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / 应 200,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("根路径响应应为 JSON: %v", err)
	}
	if body["service"] != "netdisk" {
		t.Errorf("应回服务名,实际 %v", body)
	}
}

// 结构化改写中间件**不能吞掉 Flush**(SSE 必需)。
//
// 这条纪律曾经真的失效过:中间件链是 StructuredErrors(errWriter) → Logging(recorder),
// 而 recorder.Flush 当时对**直接包装的** writer 做 `http.Flusher` 类型断言 ——
// errWriter 只有 Unwrap 没有 Flush,断言落空、静默什么都不做:
// SSE 的响应头与事件帧全攒在缓冲里,直到 handler 返回才发出去,
// 客户端表现为"连上了但永远收不到事件",而服务端毫无报错。
//
// 因此这里用一个**会记账**的 ResponseWriter 直接验证"Flush 真的到了最底层",
// 而不是用 httptest.ResponseRecorder(它的 Flush 是空操作,永远测不出来)。
func TestStructuredErrorsPreservesFlusher(t *testing.T) {
	flushes := 0
	base := &flushCounter{n: &flushes}
	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			f, ok := w.(http.Flusher)
			if !ok {
				t.Error("中间件链吞掉了 Flusher:SSE 会退化为攒够缓冲才发")
				return
			}
			f.Flush()
		}),
		middleware.StructuredErrors(testLogger()),
		middleware.Logging(testLogger()),
	)
	h.ServeHTTP(base, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if flushes == 0 {
		t.Fatal("Flush 没有到达底层 ResponseWriter(被中间件包装吞掉了)")
	}
}

// flushCounter 记录 Flush 调用次数的最小 ResponseWriter。
type flushCounter struct {
	hdr     http.Header
	n       *int
	status  int
	written int
}

func (f *flushCounter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *flushCounter) WriteHeader(code int) { f.status = code }
func (f *flushCounter) Write(b []byte) (int, error) {
	f.written += len(b)
	return len(b), nil
}
func (f *flushCounter) Flush() { *f.n++ }
