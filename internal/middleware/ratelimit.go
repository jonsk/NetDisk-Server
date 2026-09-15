package middleware

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/ratelimit"
	"github.com/netdisk/netdisk/internal/reqctx"
)

func errNoRateLimitSubject(scope string) error {
	return fmt.Errorf("ratelimit: 无法确定限速主体(scope=%s):既无已认证 userID,也无 client IP;"+
		"请确认已在该路由链中挂载 middleware.RealIP", scope)
}

// Scope 描述一个接口族的独立限速阈值(3.2 职责 7:与 Nginx 粗限流双层互补)。
type Scope struct {
	// Name 作为 key 片段,只允许 id/枚举字符
	Name string
	// PerSecond 与 PerMinute 是两个独立时间窗;任一超限即拒绝
	PerSecond ratelimit.Limit
	PerMinute ratelimit.Limit
	// Cost 每次请求消耗的配额(默认 1)
	Cost int64
	// ByIP 为 true 时对未认证请求按客户端 IP 限速
	ByIP bool
}

// DefaultScope 给出保守默认值(正式阈值由 config/路由按接口族覆盖)。
func DefaultScope(name string) Scope {
	return Scope{
		Name:      name,
		PerSecond: ratelimit.Limit{Window: time.Second, Max: 20},
		PerMinute: ratelimit.Limit{Window: time.Minute, Max: 600},
		Cost:      1,
		ByIP:      true,
	}
}

// RateLimit 按"用户优先、IP 兜底"的双维度限速。
//
// key 生成规则(6.7:只用 id,不用文件名):
//
//	netdisk:rl:<scope>:u:<userID>:s | :m
//	netdisk:rl:<scope>:ip:<clientIP>:s | :m
func RateLimit(c *cache.Client, limiter *ratelimit.Limiter, scope Scope) Middleware {
	// 未装配 Cache/Limiter 时直接透传:
	// 这是**有意支持**的场景 —— 单元测试与"无 Redis 的极简部署"下,限速整体关闭,
	// 由 Nginx 粗限流兜底(3.2 职责 7)。与"装配了但拿不到主体"必须区分开。
	if c == nil || limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			actor := reqctx.ActorFrom(ctx)

			var subjectKind, subject string
			if actor != nil && actor.UserID != "" {
				subjectKind, subject = "u", actor.UserID
			} else if scope.ByIP {
				subjectKind, subject = "ip", reqctx.ClientIP(ctx)
			}
			// 既未认证又拿不到客户端 IP:属**中间件装配错误**(忘了挂 RealIP),
			// 必须显式报错而不是静默放行 —— 否则限速形同不存在且无人察觉。
			if subject == "" {
				apierr.Write(w, r, apierr.Internal(
					errNoRateLimitSubject(scope.Name)).WithDetail("scope", scope.Name))
				return
			}

			base, err := c.Key("rl", scope.Name, subjectKind, subject)
			if err != nil {
				// key 非法说明编码有 bug:不放行,直接 500 暴露问题
				apierr.Write(w, r, apierr.Internal(err))
				return
			}

			for _, win := range []struct {
				suffix string
				limit  ratelimit.Limit
			}{
				{"s", scope.PerSecond},
				{"m", scope.PerMinute},
			} {
				res, err := limiter.Allow(ctx, base+":"+win.suffix, win.limit, scope.Cost)
				if err != nil {
					// FailOpen 时 Allow 已返回 Allowed=true;这里只处理 FailOpen=false
					apierr.Write(w, r, apierr.RateLimited("请求过于频繁,请稍后重试").WithCause(err))
					return
				}
				if !res.Allowed {
					ra := res.RetryAfter
					if ra <= 0 {
						ra = time.Second
					}
					w.Header().Set("Retry-After", strconv.Itoa(int(ra.Seconds()+0.999)))
					e := apierr.RateLimited("请求过于频繁,请稍后重试")
					e.WithDetail("scope", scope.Name).
						WithDetail("retry_after_ms", ra.Milliseconds())
					apierr.Write(w, r, e)
					return
				}
				w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(res.Remaining, 10))
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Penalize 在指定接口族的**分钟窗**上额外扣除配额(用于"这次请求虽然失败了,
// 但必须让攻击者付出代价"的场景,例如持物证明挑战连续失败)。
//
// 为什么不用"拒绝请求"来表达惩罚:被拒绝的请求不会消耗任何额度,
// 于是攻击者可以无限次尝试(只是每次拿到 429);而**失败也扣配额**才能让
// "猜样本"变得不划算 —— 试几次就把自己的窗口耗光。
//
// 返回错误表示"连惩罚都做不了"(Redis 不可用):调用方应继续返回原本的业务错误,
// 不要因为惩罚失败而把响应改成 500(那会把攻击者的错误变成服务端故障)。
func Penalize(ctx context.Context, c *cache.Client, limiter *ratelimit.Limiter, scope Scope, cost int64) error {
	if c == nil || limiter == nil || cost <= 0 {
		return nil
	}
	actor := reqctx.ActorFrom(ctx)
	var subjectKind, subject string
	if actor != nil && actor.UserID != "" {
		subjectKind, subject = "u", actor.UserID
	} else if scope.ByIP {
		subjectKind, subject = "ip", reqctx.ClientIP(ctx)
	}
	if subject == "" {
		return errNoRateLimitSubject(scope.Name)
	}
	base, err := c.Key("rl", scope.Name, subjectKind, subject)
	if err != nil {
		return err
	}
	_, err = limiter.Allow(ctx, base+":m", scope.PerMinute, cost)
	return err
}
