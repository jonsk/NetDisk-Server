package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/spacesvc"
)

// 团队空间协作(FE-W-11 / 4.3)。
//
// 与 Web 管理后台的边界:后台只做**全局治理**(配额/冻结/收回),而
// **创建/解散/邀请/退出/转让**属协作管理,入口在桌面客户端与工作台 H5 ——
// 两者调的是**同一组** `/api/v1/spaces*` 接口(4.3 决策)。
//
// 三条纪律:
//   - 全部走 `fileRead`(已登录 + 非冻结):协作动作也要受"空间被冻结"约束;
//   - **解散是硬删**(4.5 无回收站),因此文案里必须写明并二次确认(前端);
//   - 判权在服务层(owner/manager),handler 只搬运 —— 否则"谁是 owner"
//     会在多个入口各写一遍。

type spaceCreateRequest struct {
	Name string `json:"name"`
}

type spaceTransferRequest struct {
	NewOwnerID string `json:"new_owner_id"`
	// NewOwnerUsername 是 new_owner_id 的替代写法(按用户名找人)。
	//
	// 为什么支持它:**真实用户不知道对方的 uuid** —— 协作界面里能输入的只有
	// 用户名。让前端先"查人再转让"等于要求一个额外的用户检索接口,而那个接口
	// 又要把"谁能看全公司用户"这件事重新敞开(权限面变大)。这里直接在服务端解析。
	NewOwnerUsername string `json:"new_owner_username"`
}

type memberInviteRequest struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	// Permission 见 model.Perm*:manager/editor/reader
	Permission string `json:"permission"`
}

// POST /api/v1/spaces —— 创建团队空间。
func (d Deps) handleSpaceCreate(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req spaceCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	start := time.Now()
	sp, err := d.Spaces.Create(r.Context(), spacesvc.CreateInput{ActorID: a.UserID, Name: req.Name})
	d.auditAction(r, "space.create", spaceIDOf(sp), "space", spaceIDOf(sp), err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusCreated, collabSpaceView(sp, true))
}

// GET /api/v1/spaces —— 我能看到的空间(自己的 + 参与的)。
func (d Deps) handleSpaceListMine(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	mine, err := d.Spaces.ListMine(r.Context(), a.UserID)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(mine))
	for _, v := range mine {
		out = append(out, collabSpaceView(v.Space, v.IsOwner))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"spaces": out, "total": len(out)})
}

// DELETE /api/v1/spaces/{id} —— 解散团队空间(仅 owner,硬删)。
func (d Deps) handleSpaceDissolve(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	spaceID := strings.TrimSpace(r.PathValue("id"))
	start := time.Now()
	err := d.Spaces.DissolveSpace(r.Context(), a.UserID, spaceID)
	d.auditAction(r, "space.dissolve", spaceID, "space", spaceID, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"dissolved": true, "id": spaceID})
}

// POST /api/v1/spaces/{id}/transfer —— 转让 owner。
func (d Deps) handleSpaceTransfer(w http.ResponseWriter, r *http.Request) {
	if d.Spaces == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("空间服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	spaceID := strings.TrimSpace(r.PathValue("id"))
	var req spaceTransferRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	newOwner, rerr := d.resolveUser(r, req.NewOwnerID, req.NewOwnerUsername)
	if rerr != nil {
		apierr.Write(w, r, rerr)
		return
	}
	start := time.Now()
	err := d.Spaces.TransferOwnership(r.Context(), a.UserID, spaceID, newOwner)
	d.auditAction(r, "space.transfer", spaceID, "space", spaceID, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"id": spaceID, "new_owner_id": newOwner})
}

// POST /api/v1/spaces/{id}/members —— 邀请成员(与 PUT 同一语义,文档写的是 POST)。
//
// 文档 6.3 的接口表这一行是 `POST/DELETE/PUT`;实现里 PUT 是"新增或改权限"、
// DELETE 走 `/members/{userId}`。为了不出现两套用法,H5 与桌面端**统一用 PUT**
// (幂等),POST 保留为同义入口(文档口径)。
func (d Deps) handleSpaceInvite(w http.ResponseWriter, r *http.Request) {
	d.handleMemberUpsert(w, r)
}

// resolveUser 把"id 或用户名"解析成用户 id(两者都空 → 400;找不到 → 404)。
//
// 先 id 后用户名:两种写法同时给时以 id 为准(它更精确),但**不做冲突报错** ——
// 前端不该因为"两个字段都填了"就失败(那更像客户端的渲染 bug,不是用户意图冲突)。
func (d Deps) resolveUser(r *http.Request, id, username string) (string, error) {
	id = strings.TrimSpace(id)
	username = strings.TrimSpace(username)
	if id != "" {
		return id, nil
	}
	if username == "" {
		return "", apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 user_id 或 username")
	}
	if d.DB == nil {
		return "", apierr.Internal(errors.New("DB 未装配"))
	}
	u, err := repo.UserRepo{}.GetByUsername(r.Context(), db.AsQuerier(d.DB), username)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", apierr.NotFound("用户不存在: %s", username)
		}
		return "", apierr.Internal(err)
	}
	return u.ID, nil
}

// collabSpaceView 是协作视图(空间基本字段 + 是否我是 owner)。
//
// 刻意不带 `feed_floor_seq`/`last_seq` 之类的同步内部字段:协作界面不需要它们,
// 而多回一个字段就等于多一份"前端可能依赖它"的耦合。
func collabSpaceView(sp *model.Space, isOwner bool) map[string]any {
	if sp == nil {
		return nil
	}
	return map[string]any{
		"id": sp.ID, "kind": sp.Kind, "name": sp.Name,
		"owner_id": sp.OwnerID, "quota_bytes": sp.QuotaBytes, "used_bytes": sp.UsedBytes,
		"frozen": sp.Frozen, "is_owner": isOwner,
	}
}

func spaceIDOf(sp *model.Space) string {
	if sp == nil {
		return ""
	}
	return sp.ID
}
