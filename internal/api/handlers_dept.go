package api

import (
	"errors"
	"net/http"
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
		out = append(out, deptView(dpt))
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
	apierr.WriteOK(w, r, http.StatusCreated, deptView(created))
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
		v := deptView(n)
		v["relative_depth"] = st.Depth[n.ID]
		nodes = append(nodes, v)
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"root": deptView(st.Root), "nodes": nodes, "total": len(nodes),
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
func deptView(d *model.Department) map[string]any {
	if d == nil {
		return nil
	}
	return map[string]any{
		"id":         d.ID,
		"parent_id":  d.ParentID,
		"name":       d.Name,
		"sort_order": d.SortOrder,
		"source":     d.Source,
		"ext_id":     d.ExtID,
		"is_root":    d.IsRoot(),
	}
}

func deptIDOf(d *model.Department) string {
	if d == nil {
		return ""
	}
	return d.ID
}
