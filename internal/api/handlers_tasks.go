package api

import (
	"errors"
	"net/http"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/reqctx"
)

// GET /api/v1/tasks/{id} —— 目录级异步任务状态(6.11 API 表 / BE-S7-02)。
//
// 三条纪律:
//
//  1. **只能看自己的任务**。任务行里有空间 id、文件 id 与文件原名 ——
//     用别人的 task_id 探到"某个空间里有个叫 薪资表.xlsx 的目录正在被移动"
//     就是信息泄露。非本人一律 404(不是 403):403 等于确认"这个 id 存在"。
//  2. **数字口径与同步路径一致**:`rows_affected / freed_bytes / released_objects`
//     与同步删除响应的 `deleted_files+deleted_dirs / freed_bytes / released_objects`
//     同义,客户端两条路径用同一套展示逻辑。
//  3. 轮询接口必须能区分"还在跑"与"失败了":`state` 是 pending/running/done/failed,
//     失败的 `error_code` 是稳定业务码(客户端按码分流,不解析文案)。
func (d Deps) handleTaskGet(w http.ResponseWriter, r *http.Request) {
	if d.DirOps == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("目录任务队列未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	t, err := d.DirOps.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, dirops.ErrNotFound) {
			apierr.Write(w, r, apierr.NotFound("任务不存在"))
			return
		}
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	if t.UserID != a.UserID && a.Role != "admin" {
		// 刻意用 404 而不是 403:403 会确认"这个任务 id 存在"
		apierr.Write(w, r, apierr.NotFound("任务不存在"))
		return
	}

	out := map[string]any{
		"id":       t.ID,
		"kind":     string(t.Kind),
		"state":    string(t.State),
		"file_id":  t.FileID,
		"space_id": t.SpaceID,
		"async":    true,
		"created_at": t.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
	if t.RootName != "" {
		out["name"] = t.RootName
	}
	if t.IsDir {
		out["is_dir"] = true
	}
	// 未完成时不回结果字段:回 0 会被客户端当成"这次操作什么都没删"
	if !t.State.IsInflight() {
		out["rows_affected"] = t.RowsAffected
		out["freed_bytes"] = t.FreedBytes
		out["released_objects"] = t.ReleasedObjects
	}
	if t.ErrorCode != "" {
		out["error_code"] = t.ErrorCode
		out["error_message"] = t.ErrorMessage
	}
	if t.FinishedAt != nil {
		out["finished_at"] = t.FinishedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	apierr.WriteOK(w, r, http.StatusOK, out)
}
