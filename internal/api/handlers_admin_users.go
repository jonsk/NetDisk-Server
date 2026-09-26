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
	Phone       string `json:"phone"`
	// 建号即归属(可选):department_id 为初始所属机构,primary_department_id 可选主部门。
	DepartmentID         string `json:"department_id"`
	PrimaryDepartmentID  string `json:"primary_department_id"`
}

type adminUserUpdateRequest struct {
	// 指针字段:区分"没传"与"传了空值"。传空值一律 400(而不是静默忽略),
	// 否则管理员点了"停用"而前端漏填 status 时会**什么都不发生**。
	Role   *string `json:"role"`
	Status *string `json:"status"`
	Phone  *string `json:"phone"`
	// 后台"编辑用户":显示名 / 邮箱。
	DisplayName *string `json:"display_name"`
	Email       *string `json:"email"`
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
	// include_subtree 缺省/非法值一律按 true 处理:点根机构时应返回全量(4 万人)而非仅直属。
	// 仅当显式为 "false"/"0" 时才为 false。
	includeSubtree := true
	if v := strings.TrimSpace(q.Get("include_subtree")); v == "false" || v == "0" {
		includeSubtree = false
	}
	f := repo.UserFilter{
		Search:         strings.TrimSpace(q.Get("search")),
		Role:           strings.TrimSpace(q.Get("role")),
		Status:         strings.TrimSpace(q.Get("status")),
		DepartmentID:   strings.TrimSpace(q.Get("department_id")),
		IncludeSubtree: includeSubtree,
		DeptScope:      strings.TrimSpace(q.Get("scope")),
		Limit:          limit,
		Offset:         offset,
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
		Phone: req.Phone,
	})
	d.auditAction(r, "user.create", "", "user", userIDOf(created), err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 建号即归属:create 成功后,若带部门则把用户挂到指定机构(与"设所属部门"同语义)。
	if deptID := strings.TrimSpace(req.DepartmentID); deptID != "" {
		if derr := d.setUserDepartmentOnCreate(r, created.ID, deptID, strings.TrimSpace(req.PrimaryDepartmentID)); derr != nil {
			apierr.Write(w, r, derr)
			return
		}
	}
	apierr.WriteOK(w, r, http.StatusCreated, adminUserView(&repo.AdminUserRow{User: *created}))
}

// setUserDepartmentOnCreate 在建号后把用户挂到机构(创建 + 设为单一部门)。Org 未装配时静默跳过部门归属。
func (d Deps) setUserDepartmentOnCreate(r *http.Request, userID, deptID, primaryID string) error {
	if d.Org == nil {
		return nil
	}
	return d.Org.SetUserDepartments(r.Context(), userID, []string{deptID}, primaryID)
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
		UserID: id, Role: req.Role, Status: req.Status, Phone: req.Phone,
		DisplayName: req.DisplayName, Email: req.Email,
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
	v["phone"] = row.Phone
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

// ---- 用户-部门关联(FE-W-04:用户管理页设置所属部门/主部门)----

type adminUserDepartmentsRequest struct {
	DepartmentIDs     []string `json:"department_ids"`
	PrimaryDepartmentID string `json:"primary_department_id"`
}

// GET /api/v1/admin/users/{id}/departments — 回显用户所属部门 + 主部门。
func (d Deps) handleGetUserDepartments(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织架构服务未装配")))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少用户 id"))
		return
	}
	depts, err := d.Org.Depts.DepartmentsOfUser(r.Context(), d.Org.DB, id)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.NotFound("用户不存在"))
			return
		}
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	primary, err := d.Org.Depts.PrimaryDepartmentOfUser(r.Context(), d.Org.DB, id)
	if err != nil {
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	nodes := make([]map[string]any, 0, len(depts))
	for _, dept := range depts {
		nodes = append(nodes, deptView(dept, false))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"departments":          nodes,
		"primary_department_id": primary,
	})
}

// PUT /api/v1/admin/users/{id}/departments — 覆盖式设置用户所属部门 + 主部门。
func (d Deps) handleSetUserDepartments(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织架构服务未装配")))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少用户 id"))
		return
	}
	var req adminUserDepartmentsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if err := d.Org.SetUserDepartments(r.Context(), id, req.DepartmentIDs, req.PrimaryDepartmentID); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 回显设置后的结果(与 GET 同构,前端可直接更新本地状态)。
	depts, err := d.Org.Depts.DepartmentsOfUser(r.Context(), d.Org.DB, id)
	if err != nil {
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	primary, err := d.Org.Depts.PrimaryDepartmentOfUser(r.Context(), d.Org.DB, id)
	if err != nil {
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	nodes := make([]map[string]any, 0, len(depts))
	for _, dept := range depts {
		nodes = append(nodes, deptView(dept, false))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"departments":          nodes,
		"primary_department_id": primary,
	})
}
