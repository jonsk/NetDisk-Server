// Package reqctx 提供请求级上下文载体(request_id / 用户 / 客户端 IP)。
// 独立成包是为了避免中间件与 apierr 之间产生循环依赖。
package reqctx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyActor
	keyClientIP
	keyAudience
)

// Actor 是已认证的主体(权限绝不放在 JWT 里,由服务端按 space 判定,R-13)。
type Actor struct {
	UserID   string
	Username string
	Role     string // super_admin / dept_admin / user
	// Audience 取自 token 的 aud(R-14:web / h5 / desktop)
	Audience string
	// TokenVersion 用于"静默退出"比对(6.8 纪律 3)
	TokenVersion int64
}

// NewRequestID 生成 16 字节随机 id(hex 32 字符)。
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-" + hex.EncodeToString([]byte{0, 0, 0, 0})
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID / RequestIDFrom 贯穿 Nginx→Go→worker→审计(R-23)。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}

func RequestIDFrom(r *http.Request) string {
	if r == nil {
		return ""
	}
	if v, ok := r.Context().Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(keyRequestID).(string); ok {
		return v
	}
	return ""
}

func WithActor(ctx context.Context, a *Actor) context.Context {
	return context.WithValue(ctx, keyActor, a)
}

// ActorFrom 返回已认证主体;未认证时为 nil。
func ActorFrom(ctx context.Context) *Actor {
	if a, ok := ctx.Value(keyActor).(*Actor); ok {
		return a
	}
	return nil
}

// WithClientIP 记录真实客户端 IP(由中间件依据 TrustedProxies 解析,3.2)。
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, keyClientIP, ip)
}

func ClientIP(ctx context.Context) string {
	if v, ok := ctx.Value(keyClientIP).(string); ok {
		return v
	}
	return ""
}

func WithAudience(ctx context.Context, aud string) context.Context {
	return context.WithValue(ctx, keyAudience, aud)
}

func Audience(ctx context.Context) string {
	if v, ok := ctx.Value(keyAudience).(string); ok {
		return v
	}
	return ""
}

// ParseClientIP 按可信代理白名单解析客户端 IP。
//
// 3.2 职责 3:Go 侧只信 Nginx 传来的头,且仅在直连对端属于 TrustedProxies 时采信,
// 否则一律用 RemoteAddr —— 防止客户端伪造 X-Forwarded-For 绕过审计与限速。
func ParseClientIP(r *http.Request, trusted map[string]struct{}) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if _, ok := trusted[host]; !ok {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for _, p := range parts {
			if ip := strings.TrimSpace(p); ip != "" {
				return ip
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		return xri
	}
	return host
}

// TrustedSet 把配置里的白名单转成集合。
func TrustedSet(list []string) map[string]struct{} {
	m := make(map[string]struct{}, len(list))
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			m[v] = struct{}{}
		}
	}
	return m
}
