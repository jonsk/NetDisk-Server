package middleware

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/netdisk/netdisk/internal/apierr"
)

// StructuredErrors 把 ServeMux 自己产生的**非结构化**错误响应转成统一 JSON。
//
// 为什么需要它:Go 1.22+ 的 ServeMux 在两个场景会自己写响应,绕过我们的 handler ——
//  1. **405 Method Not Allowed**:路径匹配但没有该方法的 handler,ServeMux 会写
//     `405` + `Allow` 头 + 纯文本体 `Method Not Allowed\n`;
//  2. **redirect / 301**:注册了 `/admin/{rest...}` 而请求 `/admin` 时会 301 补斜杠。
//
// 纯文本体与项目其它错误(JSON + code + request_id)不一致:客户端解析错误码时会
// 在 405 上拿到空 code,前端只能靠 HTTP 码硬编码分支。这里统一改写。
//
// 实现要点(写错会静默失效):
//   - 必须在 WriteHeader 阶段拦截,**不能**等 body 写完再改(那时头已发出);
//   - 必须**抑制**内层的 WriteHeader 与 Write,否则会出现"两个响应体拼接";
//   - 只在 Content-Type 为空或纯文本时改写:如果内层已经是 JSON(我们自己的
//     apierr),说明结构正确,不能动它;
//   - `Allow` 头必须保留 —— 它是 405 语义的核心(客户端据此知道该用哪个方法)。
func StructuredErrors(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &errWriter{ResponseWriter: w, req: r, logger: logger}
			next.ServeHTTP(sw, r)
			// 内层什么都没写(例如 panic 被更外层 Recoverer 接管)→ 补一个 500,
			// 否则客户端会拿到一个空响应
			if !sw.wrote {
				apierr.Write(w, r, apierr.Internal(nil))
			}
		})
	}
}

type errWriter struct {
	http.ResponseWriter
	req      *http.Request
	logger   *slog.Logger
	wrote    bool
	replaced bool
}

func (w *errWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true

	// 只接管 ServeMux 自产的非结构化错误
	if code != http.StatusMethodNotAllowed && code != http.StatusNotFound {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	ct := w.Header().Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		// 已经是我们自己的结构化响应,原样透传
		w.ResponseWriter.WriteHeader(code)
		return
	}

	w.replaced = true
	allow := w.Header().Get("Allow")
	if allow != "" {
		w.Header().Set("Allow", allow)
	}
	if code == http.StatusMethodNotAllowed {
		msg := "该接口不支持此方法"
		if allow != "" {
			msg = "该接口不支持此方法(允许: " + allow + ")"
		}
		apierr.Write(w.ResponseWriter, w.req, apierr.MethodNotAllowed("%s", msg))
		return
	}
	apierr.Write(w.ResponseWriter, w.req, apierr.NotFound("接口不存在: %s %s", w.req.Method, w.req.URL.Path))
}

func (w *errWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.replaced {
		// 内层(ServeMux)写的纯文本体必须丢弃:结构化体已经写出去了
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap 让 http.ResponseController 能拿到原始 ResponseWriter。
//
// 缺了它,`SetWriteDeadline` / `Flush`(SSE 必需)会失效 ——
// 这是包装 ResponseWriter 最容易踩的坑,所以显式提供并在单测里验证。
func (w *errWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush 转发到底层 writer。
//
// **这条曾经真的缺失,后果很隐蔽**:中间件链从外到内是
// StructuredErrors(errWriter) → Logging(recorder) → Recoverer → mux,
// 而 `recorder.Flush()` 原先的实现是"对**直接包装的** writer 做 http.Flusher
// 类型断言" —— 直接包装的是 errWriter,它当时只有 Unwrap 没有 Flush,
// 于是断言失败、**静默什么都不做**:SSE 的响应头与事件帧被攒在缓冲里,
// 直到 handler 返回(连接关闭)才一次性发出。客户端表现为"连上了但永远收不到
// 事件",而服务端没有任何报错、日志一切正常。
//
// 发现方式:SSE 的**真实 HTTP 长连接**用例。`httptest.NewRecorder` 测不出来 ——
// 它的 Flush 是空操作,永远不会暴露这个问题(所以 SSE 必须用真实 server 测)。
//
// 用 ResponseController 而不是再写一次类型断言:它会顺着 Unwrap 链一路找下去,
// 于是"中间再插一层包装"也不会重新引入同一个洞。
func (w *errWriter) Flush() {
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
