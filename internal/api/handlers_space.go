package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/spacesvc"
)

// 空间与团队成员接口(BE-S3-04 / S3-07)。
//
// 判权全部在 spacesvc 内(4.3):handler 只做取参与解参,不做任何权限判断。

type memberUpsertRequest struct {
	UserID     string `json:"user_id"`
	// Username 是 user_id 的替代写法(按用户名邀请)
	Username string `json:"username"`
	Permission string `json:"permission"`
}

// GET /api/v1/spaces/{id}/members
func (d Deps) handleMemberList(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	ms, err := d.Spaces.ListMembers(r.Context(), a.UserID, id)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"members": ms, "total": len(ms)})
}

// PUT /api/v1/spaces/{id}/members  新增或改权限(幂等)
func (d Deps) handleMemberUpsert(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req memberUpsertRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	spaceID := r.PathValue("id")
	// 邀请支持"用户 id 或用户名":协作界面里人能输入的只有用户名
	// (解析规则与转让一致,见 resolveUser)
	target, rerr := d.resolveUser(r, req.UserID, req.Username)
	if rerr != nil {
		apierr.Write(w, r, rerr)
		return
	}
	perm := spacesvc.NormalizePermission(req.Permission)
	if !model.ValidPermission(perm) {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"permission 只能是 manager / editor / reader"))
		return
	}

	start := time.Now()
	m, err := d.Spaces.AddOrUpdateMember(r.Context(), a.UserID, spaceID, target, perm)
	d.auditAction(r, "space.member_upsert", spaceID, "user", target, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, m)
}

// DELETE /api/v1/spaces/{id}/members/{userId}  移除成员
func (d Deps) handleMemberRemove(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	spaceID := r.PathValue("id")
	target := r.PathValue("userId")
	start := time.Now()
	err := d.Spaces.RemoveMember(r.Context(), a.UserID, spaceID, target)
	d.auditAction(r, "space.member_remove", spaceID, "user", target, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// POST /api/v1/spaces/{id}/leave  主动退出
func (d Deps) handleSpaceLeave(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	spaceID := r.PathValue("id")
	start := time.Now()
	err := d.Spaces.Leave(r.Context(), a.UserID, spaceID)
	d.auditAction(r, "space.leave", spaceID, "space", spaceID, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// POST /api/v1/admin/spaces/{id}/freeze  (S3-07 管理员治理)
//
// 冻结后成员的一切读写都被拒且原因为"被管理员冻结"(space_revoked),
// 客户端据此保留本地文件等待恢复(7.2 P1-2)。
func (d Deps) handleSpaceFreeze(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	var req struct {
		Frozen *bool `json:"frozen"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if req.Frozen == nil {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 frozen 字段"))
		return
	}
	spaceID := r.PathValue("id")
	start := time.Now()
	sp, err := d.Spaces.SetFrozen(r.Context(), spaceID, *req.Frozen)
	d.auditAction(r, "space.freeze", spaceID, "space", spaceID, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, spaceView(sp))
}
