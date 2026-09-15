// Package middleware 提供 net/http 签名的中间件链(6.8:统一 func(http.Handler) http.Handler,
// 便于日后平移到 chi)。
package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/obs"
	"github.com/netdisk/netdisk/internal/reqctx"
)

// Middleware 与标准库同签名。
type Middleware func(http.Handler) http.Handler

// Chain 按声明顺序包裹:chain(h, a, b, c) 的执行顺序是 a→b→c→h。
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// RequestIDHeader 是贯穿 Nginx→Go 的关联头(4.4 / R-23)。
const RequestIDHeader = "X-Request-Id"

// RequestID 复用上游(Nginx)传来的 id,没有则生成,并回写在响应头。
func RequestID() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(RequestIDHeader)
			if id == "" || len(id) > 64 {
				id = reqctx.NewRequestID()
			}
			w.Header().Set(RequestIDHeader, id)
			next.ServeHTTP(w, r.WithContext(reqctx.WithRequestID(r.Context(), id)))
		})
	}
}

// RealIP 依可信代理白名单解析真实客户端 IP(3.2 职责 3)。
func RealIP(trusted []string) Middleware {
	set := reqctx.TrustedSet(trusted)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := reqctx.ParseClientIP(r, set)
			next.ServeHTTP(w, r.WithContext(reqctx.WithClientIP(r.Context(), ip)))
		})
	}
}

// recorder 记录状态码与响应字节数。
//
// 关键:实现 Unwrap(),使 http.ResponseController 能穿透到原始 ResponseWriter,
// 从而不影响 SSE 的 Flush 与下载路径的 sendfile(R-02/6.9)。
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *recorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *recorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

// Unwrap 让 ResponseController 找到底层 writer。
func (rec *recorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }

// Flush 保底实现(SSE 依赖)。
func (rec *recorder) Flush() {
	// 用 ResponseController:它顺着 Unwrap 链找真正的 Flusher。
	// 原先的"对直接包装的 writer 做类型断言"会在中间夹一层没有 Flush 的包装时
	// **静默失效**(SSE 的事件因此全部攒到连接关闭才发出去)。
	_ = http.NewResponseController(rec.ResponseWriter).Flush()
}

// Logging 输出访问日志,必带 request_id(R-23)。
func Logging(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			level := slog.LevelInfo
			switch {
			case rec.status >= 500:
				level = slog.LevelError
			case rec.status >= 400:
				level = slog.LevelWarn
			}
			attrs := append(obs.WithRequest(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"bytes", rec.bytes,
				"dur_ms", time.Since(start).Milliseconds(),
			)
			logger.Log(r.Context(), level, "http", attrs...)
		})
	}
}

// Recoverer 捕获 panic:记日志 + 返回 500,不泄露堆栈给客户端。
func Recoverer(logger *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						append(obs.WithRequest(r.Context()),
							"panic", rec,
							"stack", string(debug.Stack()),
						)...)
					apierr.Write(w, r, apierr.Internal(nil))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// TokenVerifier 由 auth 包实现(此处只依赖接口,避免包间循环)。
type TokenVerifier interface {
	// Verify 校验 access token 并返回主体;失败返回 *apierr.Error。
	Verify(ctx context.Context, token string) (*reqctx.Actor, error)
}

// bearerToken 从 Authorization 头取 Bearer。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && (h[:len(p)] == p || h[:len(p)] == "bearer ") {
		return h[len(p):]
	}
	return ""
}

// Auth 要求有效 access token,并把 Actor 注入上下文。
// 允许的 audience 为空表示不限(R-14:web/h5/desktop 分端)。
func Auth(v TokenVerifier, audiences ...string) Middleware {
	allowed := make(map[string]struct{}, len(audiences))
	for _, a := range audiences {
		allowed[a] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := bearerToken(r)
			if tok == "" {
				apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "缺少 Bearer token"))
				return
			}
			actor, err := v.Verify(r.Context(), tok)
			if err != nil {
				apierr.Write(w, r, err)
				return
			}
			if len(allowed) > 0 {
				if _, ok := allowed[actor.Audience]; !ok {
					// 典型场景:H5 token 调 /api/v1/admin/*(2.7 明确禁止)
					apierr.Write(w, r, apierr.Forbidden("当前客户端类型无权访问该接口"))
					return
				}
			}
			ctx := reqctx.WithActor(r.Context(), actor)
			ctx = reqctx.WithAudience(ctx, actor.Audience)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole 限制后台角色的最小权限(4.1)。
func RequireRole(roles ...string) Middleware {
	allowed := make(map[string]struct{}, len(roles))
	for _, x := range roles {
		allowed[x] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a := reqctx.ActorFrom(r.Context())
			if a == nil {
				apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
				return
			}
			if _, ok := allowed[a.Role]; !ok {
				apierr.Write(w, r, apierr.Forbidden("角色 %s 无权访问", a.Role))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// NoStore 给敏感接口加禁缓存头(登录、审计查询等)。
func NoStore() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
}
