package api

import (
	"net/http"
	"time"
)

// 本文件的 auditAuth / auditAction 在开源版(Server-com)中是 **no-op 占位**。
//
// 业务审计(持久化到 audit_logs、可查询/导出)已随开源精简移除(见 #4a 拆除)。
// 这里保留两个同名辅助函数,唯一目的是让全部 handler 里的调用点(约 20 处)
// 无需改动即可通过编译 —— 移除整个审计子系统时不改动业务代码本身。
//
// 若有需要,后续可在不触碰调用点的前提下,把这两个函数重新接到某个
// 日志/观测后端(函数签名与调用点均已冻结)。不要在此直接 import audit 包。

// auditAuth 记录认证类审计。开源版为 no-op。
func (d Deps) auditAuth(r *http.Request, action, login string, err error) {}

// auditAction 记录通用业务动作。开源版为 no-op。
func (d Deps) auditAction(r *http.Request, action, spaceID, targetType, targetID string, err error, bytesIn int64, dur time.Duration) {}
