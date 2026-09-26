package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/orgsvc"
)

// 组织架构(部门树)后台接口(BE-S3-01)。
//
// 全部限定 web audience + 管理员角色:组织树决定"谁能看到哪些团队空间",
// 是权限的输入,不能让 H5/桌面端令牌改动(2.7)。

type deptCreateRequest struct {
	ParentID  string `json:"parent_id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

func (d Deps) handleDeptTree(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	depts, err := d.Org.Tree(r.Context())
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(depts))
	for _, dpt := range depts {
		// 已 deprecated 的全量端点:前端不再用,不逐节点跑 EXISTS(N+1),传 false。
		out = append(out, deptView(dpt, false))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"departments": out, "total": len(out)})
}

func (d Deps) handleDeptCreate(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	var req deptCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	start := time.Now()
	created, err := d.Org.Create(r.Context(), orgsvc.CreateInput{
		ParentID:  strings.TrimSpace(req.ParentID),
		Name:      strings.TrimSpace(req.Name),
		SortOrder: req.SortOrder,
	})
	d.auditAction(r, "dept.create", "", "department", deptIDOf(created), err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusCreated, deptView(created, false))
}

// handleDeptSubtree 返回子树(含自身),用于"按部门授权"前预览影响面。
func (d Deps) handleDeptSubtree(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	id := r.PathValue("id")
	st, err := d.Org.Subtree(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	nodes := make([]map[string]any, 0, len(st.Nodes))
	for _, n := range st.Nodes {
		v := deptView(n, false)
		v["relative_depth"] = st.Depth[n.ID]
		nodes = append(nodes, v)
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"root": deptView(st.Root, false), "nodes": nodes, "total": len(nodes),
	})
}

// handleDeptRebuild 全量重建闭包表(组织同步后必须调用一次)。
//
// 幂等,可安全重试 —— 这正是"同步任务可全量重跑"的实现基础(6.4)。
// 失败时闭包表整体回滚,不会留下"一半新一半旧"的状态。
func (d Deps) handleDeptRebuild(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	start := time.Now()
	res, err := d.Org.Rebuild(r.Context())
	d.auditAction(r, "dept.rebuild_closure", "", "department", "", err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, res)
}

// handleDeptDelete 删除部门(仅叶子)。
func (d Deps) handleDeptDelete(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	err := d.Org.Delete(r.Context(), id)
	d.auditAction(r, "dept.delete", "", "department", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// deptView 是部门对外字段(source/ext_id 是同步对账用的,后台需要看到)。
//
// hasChildren 告诉前端"要不要去拉子节点"(懒加载)。懒加载模式下每个节点都是扁平的,
// 不再递归 children。
func deptView(d *model.Department, hasChildren bool) map[string]any {
	if d == nil {
		return nil
	}
	return map[string]any{
		"id":           d.ID,
		"parent_id":    d.ParentID,
		"name":         d.Name,
		"sort_order":   d.SortOrder,
		"source":       d.Source,
		"ext_id":       d.ExtID,
		"is_root":      d.IsRoot(),
		"has_children": hasChildren,
	}
}

// handleDeptChildren 返回某节点的直接子部门(一层,懒加载)。parent_id 为空返回根节点。
//
// total = 当前层节点数(本页返回数)。单层 >500 子节点时 total 反映本页返回数而非真实总数
// (契约已注明;单层 >500 在 8000 节点树里极少,懒加载无需精确总量)。
func (d Deps) handleDeptChildren(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	parentID := strings.TrimSpace(r.URL.Query().Get("parent_id"))
	depts, err := d.Org.Children(r.Context(), parentID)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(depts))
	for i := range depts {
		out = append(out, deptView(&depts[i].Department, depts[i].HasChildren))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"departments": out, "total": len(out)})
}

// handleDeptSearch 按名称子串搜索机构(后台树内搜索,命中返回节点 + total)。
// 前端据此在树内展开到命中路径;不做全量 8000 节点拉回。
func (d Deps) handleDeptSearch(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少搜索词 q"))
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit")))
	depts, err := d.Org.Depts.SearchByName(r.Context(), d.Org.DB, q, limit)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(depts))
	for _, dept := range depts {
		out = append(out, deptView(dept, false))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"departments": out, "total": len(out)})
}

// handleDeptRename 改名机构/部门(PUT /departments/{id},整字段替换)。
type deptRenameRequest struct {
	Name string `json:"name"`
}

func (d Deps) handleDeptRename(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少机构 id"))
		return
	}
	var req deptRenameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	start := time.Now()
	renamed, err := d.Org.Rename(r.Context(), id, strings.TrimSpace(req.Name))
	d.auditAction(r, "dept.rename", "", "department", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, deptView(renamed, false))
}

// handleDeptDeleteCheck 删除前预检:返回该机构能否删除 + 阻断原因计数。
func (d Deps) handleDeptDeleteCheck(w http.ResponseWriter, r *http.Request) {
	if d.Org == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("组织服务未装配")))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少机构 id"))
		return
	}
	res, err := d.Org.DeleteCheck(r.Context(), id)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, res)
}

func deptIDOf(d *model.Department) string {
	if d == nil {
		return ""
	}
	return d.ID
}
