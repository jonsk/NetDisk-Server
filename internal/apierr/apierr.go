// Package apierr 实现架构文档要求的统一错误模型:
// HTTP 码 + 业务码 + 可本地化文案,覆盖 6.7(结构化 400)、6.11(409 reason)、7.2(token_expired / space_revoked)。
package apierr

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/netdisk/netdisk/internal/reqctx"
)

// Code 是稳定业务码,前端/客户端据此做分支,不解析文案。
type Code string

const (
	CodeOK Code = ""

	// 400 参数/命名(6.7)
	CodeInvalidName     Code = "invalid_name"
	CodeInvalidArgument Code = "invalid_argument"
	CodePathTooDeep     Code = "path_too_deep"
	CodePathTooLong     Code = "path_too_long"
	CodeNameConflict    Code = "name_conflict"

	// 401/403 认证与权限(R-13/7.2)
	CodeUnauthorized Code = "unauthorized"
	CodeTokenExpired Code = "token_expired"
	CodeTokenRevoked Code = "token_revoked"
	CodeForbidden    Code = "forbidden"
	CodeSpaceRevoked Code = "space_revoked"

	// 404/409/410/412/413 资源与并发(R-20/R-22)
	CodeNotFound Code = "not_found"
	// CodeMethodNotAllowed 用于"路径存在但方法不对"(405 + Allow)。
	// 必须与 not_found 区分:客户端据此判断是"改方法"还是"换接口"。
	CodeMethodNotAllowed Code = "method_not_allowed"
	// CodeNotImplemented 是"明确不提供该能力"(501),与 500 区分开便于诊断
	CodeNotImplemented Code = "not_implemented"
	// CodeResourceGone 是"曾经存在、现在不可用"(410):分享过期/超次/吊销走它。
	// 与 404 区分开:410 是**终态**(客户端与用户不该反复重试),404 可能只是拼错了。
	CodeResourceGone     Code = "resource_gone"
	CodeVersionConflict  Code = "version_conflict"
	CodeCursorExpired    Code = "cursor_expired"
	CodeSpaceGone        Code = "space_gone"
	CodePreconditionFail Code = "precondition_failed"
	CodeTooLarge         Code = "payload_too_large"
	// CodeAccountConflict 是"这个外部身份要用的账号名已被别的账号占了"(409)。
	// 与 name_conflict(文件/目录重名)分开:这条是**组织同步/回调**的确定性冲突,
	// 重试无用、必须人工处理(改名或先绑定既有账号),对端据此停止重投而不是当故障。
	CodeAccountConflict Code = "account_conflict"

	// 配额(4.3)
	CodeQuotaExceeded Code = "quota_exceeded"
	CodeQuotaReserved Code = "quota_reserved"
	// CodeStorageFull 是**服务端磁盘**水位过高(9.1:>90% 拒绝新上传)。
	// 与 quota_exceeded 分开:前者是"这台机器要满了"(与用户无关,重试无用),
	// 后者是"你的额度不够"(换个空间/清点文件就能继续)。混用会让用户做错事。
	CodeStorageFull Code = "storage_full"

	// 上传(6.10)
	CodeUploadGone       Code = "upload_gone"
	CodeFastUploadDenied Code = "fast_upload_denied"
	CodeHashMismatch     Code = "hash_mismatch"

	// 429/5xx
	CodeRateLimited Code = "rate_limited"
	CodeInternal    Code = "internal_error"
	CodeUnavailable Code = "unavailable"
)

// Error 是可安全返回给客户端的错误。Details 只放无敏感信息的结构化字段。
type Error struct {
	Status  int            `json:"-"`
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	// cause 仅用于服务端日志,不序列化给客户端
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s(%d) %s: %v", e.Code, e.Status, e.Message, e.cause)
	}
	return fmt.Sprintf("%s(%d) %s", e.Code, e.Status, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

// WithCause 附加内部原因(仅进日志)。
func (e *Error) WithCause(err error) *Error { e.cause = err; return e }

// WithDetail 附加结构化字段(返回给客户端,勿放敏感值)。
func (e *Error) WithDetail(k string, v any) *Error {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[k] = v
	return e
}

func newf(status int, code Code, format string, args ...any) *Error {
	return &Error{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// ---- 构造器(按语义命名,避免调用处写裸状态码)----

func BadRequest(code Code, format string, args ...any) *Error {
	return newf(http.StatusBadRequest, code, format, args...)
}

func InvalidName(msg string, details map[string]any) *Error {
	e := newf(http.StatusBadRequest, CodeInvalidName, "%s", msg)
	for k, v := range details {
		e.WithDetail(k, v)
	}
	return e
}

func Unauthorized(code Code, msg string) *Error {
	return newf(http.StatusUnauthorized, code, "%s", msg)
}

func Forbidden(format string, args ...any) *Error {
	return newf(http.StatusForbidden, CodeForbidden, format, args...)
}

// ForbiddenCode 是**带业务码的 403**。
//
// 存在的理由:有些拒绝不是因为"你没权限",而是"你这次没通过某个校验",
// 客户端要按业务码分流(例如 `fast_upload_denied` → 老实全量上传,
// 而 `forbidden` → 提示没有权限、别重试)。混用同一个码会让客户端永远走错分支。
func ForbiddenCode(code Code, format string, args ...any) *Error {
	return newf(http.StatusForbidden, code, format, args...)
}

// SpaceRevoked 用于"我被移出空间/空间被冻结":客户端必须据此**不删本地**(7.2 P1-2)。
func SpaceRevoked(format string, args ...any) *Error {
	return newf(http.StatusForbidden, CodeSpaceRevoked, format, args...)
}

func NotFound(format string, args ...any) *Error {
	return newf(http.StatusNotFound, CodeNotFound, format, args...)
}

// MethodNotAllowed 用于"路径存在但方法不对"(405 + Allow 头)。
//
// 必须与 NotFound 分开:客户端据此决定"换个方法重试"还是"接口写错了"。
// 混成 404 会让调用方反复检查 URL —— 这也是 ServeMux 内建 405 的意义所在。
func MethodNotAllowed(format string, args ...any) *Error {
	return newf(http.StatusMethodNotAllowed, CodeMethodNotAllowed, format, args...)
}

// NotImplemented 是 501:功能**明确不做**时的应答(而不是 500)。
//
// 用 501 而不是 500 的意义在于"可诊断":客户端与排障者能立刻区分
// 「服务端坏了」与「这个能力我们有意不提供」(例如 WebDAV LOCK ——
// 上游锁是内存实现,重启即失、多实例不共享,所以不宣称支持)。
func NotImplemented(format string, args ...any) *Error {
	return newf(http.StatusNotImplemented, CodeNotImplemented, format, args...)
}

// SpaceGone 用于空间被解散/被移出:客户端据此走 PendingRemoteGone 双态确认(9.11)。
func SpaceGone(format string, args ...any) *Error {
	return newf(http.StatusGone, CodeSpaceGone, format, args...)
}

// Conflict 用于乐观锁失败(6.6 并发写矩阵)。
func Conflict(code Code, format string, args ...any) *Error {
	return newf(http.StatusConflict, code, format, args...)
}

// PreconditionFailed 用于 WebDAV(If-Match 不匹配,R-20)。
func PreconditionFailed(format string, args ...any) *Error {
	return newf(http.StatusPreconditionFailed, CodePreconditionFail, format, args...)
}

func TooLarge(format string, args ...any) *Error {
	return newf(http.StatusRequestEntityTooLarge, CodeTooLarge, format, args...)
}

// QuotaExceeded 用于预留阶段额度不足(4.3:创建即拒,不是传完才拒)。
func QuotaExceeded(format string, args ...any) *Error {
	return newf(http.StatusInsufficientStorage, CodeQuotaExceeded, format, args...)
}

func RateLimited(format string, args ...any) *Error {
	return newf(http.StatusTooManyRequests, CodeRateLimited, format, args...)
}

func Internal(err error) *Error {
	e := newf(http.StatusInternalServerError, CodeInternal, "服务器内部错误")
	if err != nil {
		e.cause = err
	}
	return e
}

// Unavailable 是"服务端缺某个前置条件,暂时做不了这件事"(503),**带可读原因**。
//
// 与 Internal 的区别在**谁能修**:500 的处置是"看日志/报障",而 503 这里
// 表达的是"部署少配了一个东西,运维补上就好" —— 例如后台保存 IdP 配置时
// 缺 `NETDISK_SECRET_KEY`。把这类情况混进 500 会让管理员只看到"服务器内部错误",
// 而真正的原因(少一个环境变量)埋在日志里、来回排查。
//
// 因此:文案里**只放配置项名字**,不放任何密钥/凭据本身。
func Unavailable(format string, args ...any) *Error {
	return newf(http.StatusServiceUnavailable, CodeUnavailable, format, args...)
}

// ---- 响应 ----

// Body 是稳定的错误响应结构。
type Body struct {
	Code    Code           `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	// RequestID 便于把用户截图与日志对齐(R-23)
	RequestID string `json:"request_id,omitempty"`
}

// Write 把 err 写成 JSON 响应。非 *Error 一律降级为 500 且不泄露内部信息。
func Write(w http.ResponseWriter, r *http.Request, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = Internal(err)
	}
	writeBody(w, r, e.Status, Body{Code: e.Code, Message: e.Message, Details: e.Details})
}

// WriteOK 写成功响应(带 request_id)。
func WriteOK(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}

func writeBody(w http.ResponseWriter, r *http.Request, status int, b Body) {
	b.RequestID = reqctx.RequestIDFrom(r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}
