package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/repo"
)

// 空间治理(FE-W-06 / 4.3)。
//
// # 与"协作管理"的边界(文档 4.3 决策)
//
// 团队空间的创建/删除/邀请/退出属于**协作管理**,入口在桌面端与 H5;
// Web 管理后台只做**全局治理**:配额与预警阈值、冻结/收回、查审计。
// 这不是"还没做",而是刻意的职责划分 —— 管理员不该在后台替业务建空间。
//
// # 三条纪律
//
//  1. **只改策略,不改用量**:`SetQuota` 只动 `quota_bytes`/`quota_warn_percent`。
//     用量只能由写事务增减(R-16)—— 后台手动改用量会让对账把真实漂移当正常。
//  2. **收回 = 收回访问权,不删数据**:冻结 + 清空成员。文档把"回收/保留期限"
//     列为待拍板的裁量项(B 类),因此这里只实现机制,不替业务决定数据去留。
//  3. **每个动作都入审计**:治理动作对用户有外部影响(传不了文件、被移出空间),
//     必须留下"谁在什么时候对哪个空间做了什么"。

type spaceQuotaRequest struct {
	QuotaBytes  *int64 `json:"quota_bytes"`
	WarnPercent *int   `json:"warn_percent"`
}

type spaceFreezeRequest struct {
	Frozen *bool `json:"frozen"`
}

// GET /api/v1/admin/spaces?search=&kind=&frozen=&limit=&offset=
func (d Deps) handleAdminSpaceList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(strings.TrimSpace(q.Get("limit")))
	offset, _ := strconv.Atoi(strings.TrimSpace(q.Get("offset")))
	f := repo.SpaceFilter{
		Search: strings.TrimSpace(q.Get("search")),
		Kind:   strings.TrimSpace(q.Get("kind")),
		Limit:  limit,
		Offset: offset,
	}
	if v := strings.TrimSpace(q.Get("frozen")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "frozen 只能是 true/false"))
			return
		}
		f.Frozen = &b
	}
	rows, total, err := repo.SpaceRepo{}.ListAdmin(r.Context(), d.dbq(), f)
	if err != nil {
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, adminSpaceView(&rows[i]))
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"spaces": out, "total": total})
}

// PATCH /api/v1/admin/spaces/{id}/quota —— 设置配额与预警阈值。
func (d Deps) handleAdminSpaceQuota(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少空间 id"))
		return
	}
	var req spaceQuotaRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if req.QuotaBytes == nil && req.WarnPercent == nil {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "没有要修改的字段"))
		return
	}
	if req.QuotaBytes != nil && *req.QuotaBytes < 0 {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "配额不能为负(0 表示不限制)"))
		return
	}
	if req.WarnPercent != nil && (*req.WarnPercent <= 0 || *req.WarnPercent > 100) {
		// 0 会让"预警"永远不触发,>100 则永远触发(通知疲劳)
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "预警阈值必须在 1..100 之间"))
		return
	}

	sp, err := repo.SpaceRepo{}.GetByID(r.Context(), d.dbq(), id)
	if err != nil {
		apierr.Write(w, r, mapNotFound(err, "空间不存在"))
		return
	}
	quota := sp.QuotaBytes
	if req.QuotaBytes != nil {
		quota = *req.QuotaBytes
	}
	// 未传阈值时保留原值(需要单独读一次 warn_percent:model.Space 不含它)
	warn := 80
	warnInDB, warnErr := repo.SpaceRepo{}.WarnPercentOf(r.Context(), d.dbq(), id)
	if warnErr == nil {
		warn = warnInDB
	}
	if req.WarnPercent != nil {
		warn = *req.WarnPercent
	}
	start := time.Now()
	uerr := repo.SpaceRepo{}.SetQuota(r.Context(), d.dbq(), id, quota, warn)
	d.auditAction(r, "space.set_quota", id, "space", id, uerr, 0, time.Since(start))
	if uerr != nil {
		apierr.Write(w, r, mapNotFound(uerr, "空间不存在"))
		return
	}
	after, aerr := repo.SpaceRepo{}.GetByID(r.Context(), d.dbq(), id)
	if aerr != nil {
		apierr.Write(w, r, apierr.Internal(aerr))
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"id": after.ID, "quota_bytes": after.QuotaBytes, "used_bytes": after.UsedBytes,
		"warn_percent": warn,
	})
}

// POST /api/v1/admin/spaces/{id}/freeze —— 冻结/解冻(管理员治理)。
//
// **刻意不走 spacesvc**:那个服务是"成员视角"的(要求调用者对该空间是 manager),
// 而管理员通常**不是**空间的成员 —— 用它会出现"管理员冻结不了别人的空间"。
// 治理动作直连仓储,并且在服务端留审计。
func (d Deps) handleAdminSpaceFreeze(w http.ResponseWriter, r *http.Request) {
	spaceID := strings.TrimSpace(r.PathValue("id"))
	if spaceID == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少空间 id"))
		return
	}
	var req spaceFreezeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	if req.Frozen == nil {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 frozen 字段"))
		return
	}
	start := time.Now()
	sp, err := repo.SpaceRepo{}.SetFrozenByAdmin(r.Context(), d.dbq(), spaceID, *req.Frozen)
	action := "space.freeze"
	if !*req.Frozen {
		action = "space.unfreeze"
	}
	d.auditAction(r, action, spaceID, "space", spaceID, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, mapNotFound(err, "空间不存在"))
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"id": sp.ID, "frozen": sp.Frozen})
}

// POST /api/v1/admin/spaces/{id}/revoke —— 收回空间(冻结 + 清空成员,不删数据)。
func (d Deps) handleAdminSpaceRevoke(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少空间 id"))
		return
	}
	start := time.Now()
	repoSp := repo.SpaceRepo{}
	removed, err := repoSp.ClearMembers(r.Context(), d.dbq(), id)
	if err != nil {
		d.auditAction(r, "space.revoke", id, "space", id, err, 0, time.Since(start))
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	frozen, ferr := repoSp.SetFrozenByAdmin(r.Context(), d.dbq(), id, true)
	if ferr != nil {
		d.auditAction(r, "space.revoke", id, "space", id, ferr, 0, time.Since(start))
		apierr.Write(w, r, mapNotFound(ferr, "空间不存在"))
		return
	}
	d.auditAction(r, "space.revoke", id, "space", id, nil, 0, time.Since(start))
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"id": frozen.ID, "frozen": frozen.Frozen, "members_removed": removed,
		// 明确告诉调用方"数据没删":这是机制与策略的边界(保留期限待 B 类拍板)
		"note": "已收回访问权(冻结并清空成员);空间内文件**未删除**",
	})
}

// dbq 返回只读查询入口。
//
// `*db.DB` 只提供 InTx/Acquire,不直接实现 repo.Querier(缺 Exec)——
// 这是**刻意的**:写操作必须显式走事务,查询才允许直接用池。
func (d Deps) dbq() repo.Querier { return db.AsQuerier(d.DB) }

// adminSpaceView 是治理视图(带所有者与用量占比,前端直接展示进度条)。
func adminSpaceView(r *repo.AdminSpaceRow) map[string]any {
	percent := 0.0
	if r.QuotaBytes > 0 {
		percent = float64(r.UsedBytes) / float64(r.QuotaBytes) * 100
	}
	return map[string]any{
		"id": r.ID, "kind": r.Kind, "name": r.Name,
		"owner_id": r.OwnerID, "owner_username": r.OwnerUsername, "owner_display_name": r.OwnerDisplay,
		"quota_bytes": r.QuotaBytes, "used_bytes": r.UsedBytes, "warn_percent": r.WarnPercent,
		"used_percent": percent, "frozen": r.Frozen,
		"member_count": r.MemberCount, "files_count": r.FilesCount,
		"created_at": r.CreatedAt,
	}
}

// mapNotFound 把领域错误映射成 404(其余原样)。
func mapNotFound(err error, msg string) error {
	if errors.Is(err, repo.ErrNotFound) {
		// 用 "%s" 而不是把 msg 当格式串:msg 里出现 % 时后者会读越界参数(vet 会报)
		return apierr.NotFound("%s", msg)
	}
	return err
}
