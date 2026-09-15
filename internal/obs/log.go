// Package obs 提供结构化日志(slog)与日志字段规范。
//
// 约定(4.4 / 8.4):
//   - 每条请求日志必带 request_id
//   - 敏感值(secret / token / 密码 / 对象内容样本)一律不进日志
//   - 文件名只在必要的业务日志出现;Redis key、监控 label 只用 id(6.7)
package obs

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/netdisk/netdisk/internal/reqctx"
)

// New 按配置创建 logger。level: debug/info/warn/error;format: text/json。
func New(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// WithRequest 从上下文取出 request_id / 客户端 IP / 主体,便于 handler 与 worker 复用。
func WithRequest(ctx context.Context) []any {
	attrs := make([]any, 0, 6)
	if id := reqctx.RequestID(ctx); id != "" {
		attrs = append(attrs, "request_id", id)
	}
	if ip := reqctx.ClientIP(ctx); ip != "" {
		attrs = append(attrs, "client_ip", ip)
	}
	if a := reqctx.ActorFrom(ctx); a != nil {
		attrs = append(attrs, "user_id", a.UserID)
		if a.Audience != "" {
			attrs = append(attrs, "aud", a.Audience)
		}
	}
	return attrs
}
