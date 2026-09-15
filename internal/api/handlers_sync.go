package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 增量变更接口(BE-S9-02 / 6.3 / 6.4)。
//
// 契约(`GET /api/v1/changes?space={id}&since={seq}&limit=1000`):
//
//	{"items":[{"change_seq","file_id","kind","version","parent_id","name","old_parent_id"}],
//	 "next_seq":N,"has_more":false,"space_id":"..."}
//
// 三条必须成立的语义:
//   - `since` 之后的变更按 change_seq 升序返回,客户端把 next_seq 原样回传即可翻页;
//   - `limit` 有上限(超过夹到上限,**不报错** —— 报错只会逼客户端写重试逻辑);
//   - `since` 早于本空间已清理水位 → **409 cursor_expired**,客户端走全量清单。
//     绝不能静默返回空:那会让客户端以为自己已经是最新的,永久漏掉一段变更。
func (d Deps) handleChanges(w http.ResponseWriter, r *http.Request) {
	if d.DB == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("数据库未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	q := db.AsQuerier(d.DB)
	params := r.URL.Query()
	spaceID := strings.TrimSpace(queryAlias(params, "space", "space_id"))
	if spaceID == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 space 参数"))
		return
	}

	// since 缺省 = 0(从头);负数与非法值一律 400,不静默当 0 ——
	// "把非法游标当 0"会让客户端每次都从头拉一遍(还可能直接撞超窗 409),
	// 而它自己不知道为什么。
	var since int64
	if raw := strings.TrimSpace(params.Get("since")); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < 0 {
			apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
				"since 必须是非负整数"))
			return
		}
		since = v
	}
	limit := 0
	if raw := strings.TrimSpace(params.Get("limit")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
				"limit 必须是非负整数"))
			return
		}
		limit = v
	}

	// 判权在服务端(4.3):无权限/被移出返回 410 space_revoked 而不是空列表 ——
	// 空列表会被客户端当成"这个空间没有变化",于是它保留本地文件、继续等;
	// 而事实是它已经看不到这个空间了(6.4 权限受限态)。
	svc := d.Files
	if svc == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	if err := svc.CheckReadable(r.Context(), a.UserID, spaceID); err != nil {
		apierr.Write(w, r, err)
		return
	}

	page, err := syncfeed.Changes(r.Context(), q, spaceID, since, limit)
	if err != nil {
		switch {
		case errors.Is(err, syncfeed.ErrCursorExpired):
			// 409 + cursor_expired + 建议动作:客户端据此丢弃本地游标并走全量清单
			e := apierr.Conflict(apierr.CodeCursorExpired,
				"游标已超窗(所需变更已被清理),请重新拉取全量清单")
			e.WithDetail("space_id", spaceID)
			e.WithDetail("action", "full_resync")
			apierr.Write(w, r, e)
		case errors.Is(err, repo.ErrNotFound):
			apierr.Write(w, r, apierr.SpaceGone("空间不存在或已被解散"))
		default:
			apierr.Write(w, r, apierr.Internal(err))
		}
		return
	}
	// 增量接口是同步主路径,不许任何中间层缓存(否则客户端会拿到旧游标)
	w.Header().Set("Cache-Control", "no-store")
	apierr.WriteOK(w, r, http.StatusOK, page)
}

// handleChangesHead 返回当前最大 change_seq(客户端首次同步的起点)。
//
// 为什么单独给"头指针"而不是让客户端从 0 拉:从 0 拉等于把历史变更全看一遍,
// 而且很可能直接 409(超窗)。首次同步的正确姿势是"全量清单 + 从当前头上开始增量"。
func (d Deps) handleChangesHead(w http.ResponseWriter, r *http.Request) {
	if d.DB == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("数据库未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	// 查询参数名与契约一致:`space`(历史别名 `space_id` 同收,理由见 query_alias.go)
	spaceID := strings.TrimSpace(queryAlias(r.URL.Query(), "space", "space_id"))
	if spaceID == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 space 参数"))
		return
	}
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	if err := d.Files.CheckReadable(r.Context(), a.UserID, spaceID); err != nil {
		apierr.Write(w, r, err)
		return
	}
	seq, err := syncfeed.HeadSeq(r.Context(), db.AsQuerier(d.DB), spaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.SpaceGone("空间不存在或已被解散"))
			return
		}
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"space_id": spaceID, "head_seq": seq,
	})
}

// handleCursorReport 接收客户端游标上报(BE-S9-03 / 6.4 T2-4)。
//
// 语义:`(client_id, space_id)` 单维**取 max**,乱序与重放都不会让进度倒退。
// 权威值落 PG(sync_cursors),Redis 只作加速 —— Redis 重启不丢账(10.7)。
func (d Deps) handleCursorReport(w http.ResponseWriter, r *http.Request) {
	if d.DB == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("数据库未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req struct {
		ClientID string `json:"client_id"`
		SpaceID  string `json:"space_id"`
		LastSeq  int64  `json:"last_seq"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	req.ClientID = strings.TrimSpace(req.ClientID)
	req.SpaceID = strings.TrimSpace(req.SpaceID)
	if req.ClientID == "" || req.SpaceID == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"client_id 与 space_id 均不能为空"))
		return
	}
	if req.LastSeq < 0 {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "last_seq 不能为负"))
		return
	}
	if d.Files != nil {
		if err := d.Files.CheckReadable(r.Context(), a.UserID, req.SpaceID); err != nil {
			apierr.Write(w, r, err)
			return
		}
	}
	// client_id 只允许 id/枚举字符(**与 cache.Key 的白名单同源**)。
	//
	// 刻意不写"只拒绝空格与斜杠"那种黑名单:client_id 会被用作缓存键片段,
	// 而 cache.Key 的白名单是 `[A-Za-z0-9_.:-]` —— 黑名单会放行中文、全角字符、
	// 控制符等一切"没想到"的字符,于是问题在**用它的那一刻**才炸(写 Redis 前
	// 的键校验失败 → 500),而不是在这里得到 400。
	if !validClientID(req.ClientID) {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "client_id 非法"))
		return
	}
	if err := syncfeed.ReportCursor(r.Context(), db.AsQuerier(d.DB), syncfeed.CursorReport{
		ClientID: req.ClientID, SpaceID: req.SpaceID, LastSeq: req.LastSeq,
	}); err != nil {
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"ok": true})
}

// validClientID 校验客户端标识:只允许 `[A-Za-z0-9_.:-]` 且长度 <=128。
//
// 与 `cache.Key` 的 idPattern 逐字一致。两处若不一致,就会出现
// "接口放行、写缓存时失败"的 500 —— 那是把一次客户端的输入错误
// 变成了服务端故障,排障方向也会被带偏。
func validClientID(v string) bool {
	if len(v) == 0 || len(v) > 128 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == ':' || r == '-':
		default:
			return false
		}
	}
	return true
}
