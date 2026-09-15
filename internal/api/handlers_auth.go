package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/reqctx"
)

// maxJSONBody 限制认证类请求体大小,防止超大 JSON 打爆内存。
const maxJSONBody = 64 << 10 // 64KB

// decodeJSON 严格解码:拒绝未知字段、限制体积、拒绝多段 JSON。
//
// 严格模式是刻意的 —— 客户端把 token 放错字段这类问题应当在开发期暴露,
// 而不是被静默忽略后表现为"登录成功但没有 token"。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			return apierr.TooLarge("请求体过大")
		case errors.Is(err, io.EOF):
			return apierr.BadRequest(apierr.CodeInvalidArgument, "请求体为空")
		default:
			return apierr.BadRequest(apierr.CodeInvalidArgument, "请求体解析失败: %s", err.Error())
		}
	}
	if dec.More() {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "请求体只能包含一个 JSON 对象")
	}
	return nil
}

// decodeStrictBody 与 decodeJSON 同规则,但**不需要 ResponseWriter**
// (用于非 HTTP 错误响应路径,例如 TUS 建任务先解析 JSON body 再决定响应形态)。
//
// 体积上限复用 maxJSONBody;同样拒绝未知字段与多段 JSON。
func decodeStrictBody(r *http.Request, dst any) error {
	return decodeStrictReader(r.Body, dst)
}

// decodeStrictReader 是 decodeStrictBody 的"任意 reader"版本。
//
// 存在的理由:multipart 的某个**段**也是要严格解码的 JSON,
// 但它不是 http.Request.Body —— 把段内容读进来再走同一个解码规则,
// 才能保证"JSON body"与"JSON 段"的严格度完全一致(拒绝未知字段/多段)。
// 体积上限由调用方用 io.LimitReader 施加(段本身没有 Content-Length 语义)。
func decodeStrictReader(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		switch {
		case errors.Is(err, io.EOF):
			return apierr.BadRequest(apierr.CodeInvalidArgument, "请求体为空")
		default:
			return apierr.BadRequest(apierr.CodeInvalidArgument, "请求体解析失败: %s", err.Error())
		}
	}
	if dec.More() {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "请求体只能包含一个 JSON 对象")
	}
	return nil
}

// audienceFromRequest 决定本次请求的令牌受众(R-14:web / desktop 分端;H5 渠道已随服务端拆除)。
func audienceFromRequest(r *http.Request, explicit string) string {
	if explicit != "" {
		return normalizeAudience(explicit)
	}
	if h := r.Header.Get("X-Client-Kind"); h != "" {
		return normalizeAudience(h)
	}
	return auth.AudienceWeb
}

func normalizeAudience(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case auth.AudienceDesktop, "client", "wpf":
		return auth.AudienceDesktop
	default:
		return auth.AudienceWeb
	}
}

// ---- POST /api/v1/auth/login ----

type loginRequest struct {
	Login    string `json:"login"`    // 用户名或邮箱
	Username string `json:"username"` // 兼容字段名
	Password string `json:"password"`
	Audience string `json:"audience"`
}

func (d Deps) handleLogin(w http.ResponseWriter, r *http.Request) {
	if d.Auth == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("认证服务未装配")))
		return
	}
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	login := strings.TrimSpace(req.Login)
	if login == "" {
		login = strings.TrimSpace(req.Username)
	}
	if login == "" || req.Password == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "用户名与密码不能为空"))
		return
	}

	res, err := d.Auth.Login(r.Context(), authsvc.LoginInput{
		Login:     login,
		Password:  req.Password,
		Audience:  audienceFromRequest(r, req.Audience),
		IP:        reqctx.ClientIP(r.Context()),
		UserAgent: truncate(r.UserAgent(), 256),
	})
	// 成功与失败都要留痕(4.4 审计范围含登录)
	d.auditAuth(r, "auth.login", login, err)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}

	// 响应刻意不含任何权限列表(R-13)
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"token":          res.Token,
		"user":           userView(res.User),
		"personal_space": spaceView(res.Space),
	})
}

// ---- POST /api/v1/auth/refresh ----

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
	Audience     string `json:"audience"`
}

func (d Deps) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if d.Auth == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("认证服务未装配")))
		return
	}
	var req refreshRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "refresh_token 不能为空"))
		return
	}
	// audience 只在客户端显式给出时才校验(否则沿用 token 自带的)
	aud := ""
	if req.Audience != "" || r.Header.Get("X-Client-Kind") != "" {
		aud = audienceFromRequest(r, req.Audience)
	}
	token, err := d.Auth.Refresh(r.Context(), req.RefreshToken, aud)
	d.auditAuth(r, "auth.refresh", "", err)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 轮换后必须返回**新的** refresh,客户端要立即替换旧值
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"token": token})
}

// ---- POST /api/v1/auth/logout ----

type logoutRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (d Deps) handleLogout(w http.ResponseWriter, r *http.Request) {
	if d.Auth == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("认证服务未装配")))
		return
	}
	var req logoutRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		// 登出幂等:没有 token 也返回 204,避免客户端"登不出去"
		apierr.WriteOK(w, r, http.StatusNoContent, nil)
		return
	}
	err := d.Auth.Logout(r.Context(), req.RefreshToken)
	d.auditAuth(r, "auth.logout", "", err)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// ---- 视图 ----

// userView 是用户对外可见字段(刻意不含 role 之外的任何权限信息)。
func userView(u *model.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"id":           u.ID,
		"username":     u.Username,
		"display_name": u.DisplayName,
		"avatar_url":   u.AvatarURL,
		"role":         u.Role,
		"status":       u.Status,
	}
}

// spaceView 是空间对外可见字段(4.3:配额挂在空间行,前端首屏要用)。
//
// quota_bytes = 0 表示不限制 —— 原样返回,由前端展示为"不限"。
func spaceView(sp *model.Space) map[string]any {
	if sp == nil {
		return nil
	}
	return map[string]any{
		"id":          sp.ID,
		"kind":        sp.Kind,
		"name":        sp.Name,
		"quota_bytes": sp.QuotaBytes,
		"used_bytes":  sp.UsedBytes,
		"frozen":      sp.Frozen,
	}
}

// truncate 截断过长字符串(User-Agent 可能被塞很长,防日志/审计膨胀)。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
