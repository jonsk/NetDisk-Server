package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/usersvc"
)

// 后台用户管理(FE-W-04):列表 / 搜索 / 建号 / 启停用 / 角色调整。
//
// 三条纪律:
//
//  1. **全部走 adminChain**(web audience + 管理员角色):用户与角色是权限的输入,
//     不能让 H5/桌面的令牌改(2.7 分端)。
//  2. **停用是一次"生效"操作**:`usersvc.Update` 在同一事务里吊销刷新令牌 +
//     自增 token_version —— 只改 status 的话,已登录会话能一直用到 access 过期。
//  3. **唯一冲突要指出字段**:用户名与邮箱撞车的处置不同,只说"唯一约束冲突"
//     等于让管理员猜。响应带 `details.field`,前端据此高亮对应输入框。

type adminUserCreateRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type adminUserUpdateRequest struct {
	// 指针字段:区分"没传"与"传了空值"。传空值一律 400(而不是静默忽略),
	// 否则管理员点了"停用"而前端漏填 status 时会**什么都不发生**。
	Role   *string `json:"role"`
	Status *string `json:"status"`
}

// GET /api/v1/admin/users?search=&role=&status=&limit=&offset=
func (d Deps) handleAdminUserList(w http.ResponseWriter, r *http.Request) {
	if d.UserAdmin == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("用户管理服务未装配")))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	f := repo.UserFilter{
		Search: strings.TrimSpace(q.Get("search")),
		Role:   strings.TrimSpace(q.Get("role")),
		Status: strings.TrimSpace(q.Get("status")),
		Limit:  limit,
		Offset: offset,
	}
	rows, total, err := d.UserAdmin.List(r.Context(), f)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, adminUserView(&rows[i]))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"users": out, "total": total,
		"limit": effectiveUserLimit(limit), "offset": offset,
	})
}

// POST /api/v1/admin/users
func (d Deps) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	if d.UserAdmin == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("用户管理服务未装配")))
		return
	}
	var req adminUserCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	start := time.Now()
	created, err := d.UserAdmin.Create(r.Context(), usersvc.CreateInput{
		Username: req.Username, Email: req.Email, DisplayName: req.DisplayName, Role: req.Role,
	})
	d.auditAction(r, "user.create", "", "user", userIDOf(created), err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusCreated, adminUserView(&repo.AdminUserRow{User: *created}))
}

// PATCH /api/v1/admin/users/{id}
func (d Deps) handleAdminUserUpdate(w http.ResponseWriter, r *http.Request) {
	if d.UserAdmin == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("用户管理服务未装配")))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少用户 id"))
		return
	}
	var req adminUserUpdateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	start := time.Now()
	updated, err := d.UserAdmin.Update(r.Context(), usersvc.UpdateInput{
		UserID: id, Role: req.Role, Status: req.Status,
	})
	action := "user.update"
	if req.Status != nil {
		if strings.TrimSpace(*req.Status) == model.StatusActive {
			action = "user.enable"
		} else {
			action = "user.disable"
		}
	}
	d.auditAction(r, action, "", "user", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, adminUserView(&repo.AdminUserRow{User: *updated}))
}

// adminUserView 是后台用户视图:**比登录返回多**邮箱/状态/最后登录/创建时间。
//
// 登录响应刻意不含这些(那是给用户自己看的);后台需要它们来做运维判断
// ("这账号是他本人吗""最近登录过吗")。
func adminUserView(row *repo.AdminUserRow) map[string]any {
	if row == nil {
		return nil
	}
	v := userView(&row.User)
	v["email"] = row.Email
	v["token_version"] = row.TokenVersion
	v["created_at"] = row.CreatedAt
	v["updated_at"] = row.UpdatedAt
	if row.LastLoginAt != nil {
		v["last_login_at"] = row.LastLoginAt
	} else {
		v["last_login_at"] = nil
	}
	return v
}

// effectiveUserLimit 回显实际生效的 limit(便于前端对齐分页)。
func effectiveUserLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > repo.MaxUserListLimit {
		return repo.MaxUserListLimit
	}
	return limit
}

func userIDOf(u *model.User) string {
	if u == nil {
		return ""
	}
	return u.ID
}
