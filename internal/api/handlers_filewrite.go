package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/reqctx"
)

// 文件写操作接口(BE-S6-01 改名/移动、BE-S6-02 删除)。
//
// 全部要求写权限(服务端判定,4.3),并支持 `base_version` 乐观锁(6.6)。

type renameRequest struct {
	Name string `json:"name"`
	// BaseVersion 客户端持有的版本(0 = 不带乐观锁)
	BaseVersion int64 `json:"base_version"`
}

type moveRequest struct {
	ParentID    string `json:"parent_id"`
	Name        string `json:"name"`
	BaseVersion int64  `json:"base_version"`
}

// createDirRequest 建目录(契约 `POST /api/v1/files/dirs`)。
//
// 字段名与 moveRequest 的 `parent_id` 对齐;`space_id`/`parent_id` 留空时
// 分别落到调用者个人空间与空间根目录(与列表接口同义)。
type createDirRequest struct {
	SpaceID  string `json:"space_id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
}

// POST /api/v1/files/dirs 新建目录(Bearer 令牌可用的入口,补 WebDAV MKCOL 的缺口)
//
// 201 而不是 200:确实新建了一行。同名占用走 409 `name_conflict`
// (**不**返回已有条目),客户端要复用就自己按名字查 —— 见契约说明。
func (d Deps) handleFileCreateDir(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req createDirRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	start := time.Now()
	v, err := d.Files.CreateDir(r.Context(), filesvc.CreateDirInput{
		UserID:   a.UserID,
		SpaceID:  strings.TrimSpace(req.SpaceID),
		ParentID: strings.TrimSpace(req.ParentID),
		Name:     req.Name,
	})
	// 审计里的 target_id 在成功前拿不到(条目刚建),所以先记空 id:
	// 空间与动作足以回答"谁在什么时候建过目录",而**不能**为了一次审计
	// 把 id 提前生成(那会让"建目录失败也留下一个 id"成为可能)。
	d.auditAction(r, "file.mkdir", "", "file", "", err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	d.publishWrite(r.Context(), v.SpaceID, v.ID, model.FeedCreated)
	apierr.WriteOK(w, r, http.StatusCreated, v)
}

// PATCH /api/v1/files/{id} 改名
func (d Deps) handleFileRename(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req renameRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	v, err := d.Files.Rename(r.Context(), filesvc.RenameInput{
		UserID: a.UserID, FileID: id,
		NewName:     strings.TrimSpace(req.Name),
		BaseVersion: req.BaseVersion,
	})
	d.auditAction(r, "file.rename", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 事件推送在**提交之后**(审计行已写):失败不影响响应(事件允许丢失)
	d.publishWrite(r.Context(), v.SpaceID, v.ID, model.FeedUpdated)
	apierr.WriteOK(w, r, http.StatusOK, v)
}

// POST /api/v1/files/{id}/move 移动(可同时改名)
func (d Deps) handleFileMove(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req moveRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	res, err := d.Files.Move(r.Context(), filesvc.MoveInput{
		UserID: a.UserID, FileID: id,
		NewParentID: strings.TrimSpace(req.ParentID),
		NewName:     strings.TrimSpace(req.Name),
		BaseVersion: req.BaseVersion,
	})
	d.auditAction(r, "file.move", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 异步分支:没有可推的条目状态(子树还在原来的位置),只把 task_id 交回去。
	// **不推事件**:任务还没跑,推一条 moved 会让客户端提前把条目挂到新目录,
	// 而任务可能失败/稍后才执行;真正的 moved 事件由 worker 完成后写入变更流。
	if res.Async {
		apierr.WriteOK(w, r, http.StatusAccepted, res)
		return
	}
	d.publishWrite(r.Context(), res.Entry.SpaceID, res.Entry.ID, model.FeedMoved)
	apierr.WriteOK(w, r, http.StatusOK, res)
}

// DELETE /api/v1/files/{id} 硬删(无回收站,4.5)
//
// 返回删除影响面(文件数/目录数/释放字节/回收对象数),便于客户端在删除后
// 更新本地容量显示,也便于审计回答"这次删了多少"。
func (d Deps) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	res, err := d.Files.Delete(r.Context(), a.UserID, id)
	d.auditAction(r, "file.delete", "", "file", id, err, freedBytes(res), time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 异步分支:此时行还在(任务只是入队了),**不能**推 deleted 事件 ——
	// 推了会让客户端立刻删掉本地副本,而任务可能失败/稍后才跑。
	// 真正的 deleted 事件由 worker 完成后写进变更流。
	if res.Async {
		apierr.WriteOK(w, r, http.StatusAccepted, res)
		return
	}
	// 删除事件用**请求里的空间**提示:被删的行已经不在了,拿不到它的 space_id。
	// 客户端收到后按游标去 /changes 拉 — 它那里有权威的 deleted 事件(带 space_id)。
	d.publishWrite(r.Context(), spaceOfDelete(r), id, model.FeedDeleted)
	apierr.WriteOK(w, r, http.StatusOK, res)
}

// GET /api/v1/files/{id}/subtree-stats 删除前的影响面预检(BE-S7-01)
//
// 与删除共用同一份统计实现(`FileRepo.SubtreeStatsOf` 的递归 CTE),
// 因此"预检说会删 3 个文件"与"删除时确实删了 3 个"不会漂移。
func (d Deps) handleSubtreeStats(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	stats, err := d.Files.SubtreeStats(r.Context(), a.UserID, id)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, stats)
}

func freedBytes(res *filesvc.DeleteResult) int64 {
	if res == nil {
		return 0
	}
	return res.FreedBytes
}

// ---- 共享到空间(BE-S6-03)与复制(BE-S7-03)----
//
// 两者都是"引用 +1"路径(4.5 逻辑复制 + 物理去重),差别只在目标:
// 共享跨空间、只接文件;复制同空间、可递归目录。

type shareRequest struct {
	// TargetSpaceID 目标空间(必填)
	TargetSpaceID string `json:"target_space_id"`
	// TargetParentID 目标目录;空 = 目标空间根目录
	TargetParentID string `json:"target_parent_id"`
	// Name 目标名;空 = 沿用原名
	Name string `json:"name"`
}

type copyRequest struct {
	// TargetParentID 目标目录;空 = 与源同目录
	TargetParentID string `json:"target_parent_id"`
	// Name 目标名;空 = 自动生成"原名 (副本)"
	Name string `json:"name"`
}

// POST /api/v1/files/{id}/share 共享到另一个空间(4.5)
//
// 201 而不是 200:语义上**新建了一个条目**(新 files 行),与 POST /copy 一致。
func (d Deps) handleFileShare(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req shareRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	v, err := d.Files.Share(r.Context(), filesvc.ShareInput{
		UserID: a.UserID, FileID: id,
		TargetSpaceID:  strings.TrimSpace(req.TargetSpaceID),
		TargetParentID: strings.TrimSpace(req.TargetParentID),
		NewName:        strings.TrimSpace(req.Name),
	})
	d.auditAction(r, "file.share", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	d.publishWrite(r.Context(), v.SpaceID, v.ID, model.FeedSharedIn)
	apierr.WriteOK(w, r, http.StatusCreated, v)
}

// POST /api/v1/files/{id}/copy 复制(文件或目录)
//
// 返回新建行数与被去重复用的对象数 —— 客户端据此提示"复制完成,
// 未额外占用磁盘空间"这类信息(内容寻址下去重是常态而非例外)。
func (d Deps) handleFileCopy(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req copyRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	res, err := d.Files.Copy(r.Context(), filesvc.CopyInput{
		UserID: a.UserID, FileID: id,
		TargetParentID: strings.TrimSpace(req.TargetParentID),
		NewName:        strings.TrimSpace(req.Name),
	})
	d.auditAction(r, "file.copy", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	if res.Root != nil {
		d.publishWrite(r.Context(), res.Root.SpaceID, res.Root.ID, model.FeedCreated)
	}
	apierr.WriteOK(w, r, http.StatusCreated, res)
}

// spaceOfDelete 返回删除请求对应的空间提示。
//
// 删除的响应体不含 space_id(行已消失),而事件里的 space_id 是**连接内过滤**的
// 依据 —— 拿不到时宁可不推(客户端仍有 /changes 权威路径与兜底扫描),
// 也不要瞎猜一个空间去推(推错就是给无权限的连接泄露"某个 id 被删了")。
func spaceOfDelete(r *http.Request) string {
	return strings.TrimSpace(r.URL.Query().Get("space"))
}
