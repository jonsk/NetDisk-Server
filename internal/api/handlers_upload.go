package api

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/middleware"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// maxHashLen 是客户端声明哈希的长度上限(sha256 hex = 64)。
const maxHashLen = 128

// base64Std 是 TUS Upload-Metadata 使用的标准 base64(带 padding)。
var base64Std = base64.StdEncoding

// uploadCreateRequest 是 POST /api/v1/upload/create 的请求体。
//
// **刻意只声明元数据**:文件字节走后续 TUS PATCH(6.10 单一上传通道),
// 这样"建任务"这一步是廉价且可重试的,不会因为网络抖动把 GB 级流量重来一遍。
type uploadCreateRequest struct {
	SpaceID  string `json:"space_id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	// Hash 是可选的 SHA-256(客户端的秒传 hint);服务器在 finalize 时**重新计算**
	// 实际哈希,绝不相信客户端声明值(4.2/R-08)。
	Hash string `json:"hash"`
	// AllowOverwrite = "这次上传要原地覆盖同目录同名文件"(契约 UploadCreateRequest.allow_overwrite)。
	//
	// ⚠ 这个字段曾经只在 TUS 的 Upload-Metadata 里被解析,JSON 入口漏了 ——
	// 而客户端的 TUS 上传器把 allow_overwrite 放在**JSON body** 里发,于是
	//   `请求体解析失败: json: unknown field "allow_overwrite"`
	// 直接 400:客户端**任何一次"改本地文件后传回去"都失败**。decodeJSON 开着
	// DisallowUnknownFields,所以契约与实现不一致时是硬失败而不是静默忽略
	// (这正是它该有的表现 —— 静默忽略会让"覆盖"悄悄退化成"新建同名"或 409)。
	AllowOverwrite bool `json:"allow_overwrite"`
}

// handleUploadCreate 创建上传任务并预留额度。
//
// 语义要点(4.3 + 6.10):
//   - 额度不足 → **这里就返回 507**,而不是让客户端白传 10GB;
//   - 返回的 upload_ticket 只出现这一次,客户端需自行保存(服务端只存哈希);
//   - 返回 upload_id 供 TUS 续传与幂等重试。
func (d Deps) handleUploadCreate(w http.ResponseWriter, r *http.Request) {
	if d.Uploads == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("上传服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req uploadCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "name 不能为空"))
		return
	}
	if len(req.Hash) > maxHashLen {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "hash 字段过长"))
		return
	}

	start := time.Now()
	res, err := d.Uploads.Create(r.Context(), uploadsvc.CreateInput{
		UserID:       a.UserID,
		SpaceID:      strings.TrimSpace(req.SpaceID),
		ParentID:     strings.TrimSpace(req.ParentID),
		Name:         name,
		DeclaredSize: req.Size,
		DeclaredHash: strings.ToLower(strings.TrimSpace(req.Hash)),
		// 覆盖意图必须一路透传到定稿(见 uploadsvc.CreateInput.AllowOverwrite):
		// 任务可能隔很久才传完,定稿那一刻只能靠落库的这份意图判断该不该覆盖。
		AllowOverwrite: req.AllowOverwrite,
	})
	d.auditAction(r, "upload.create", spaceOf(res, req.SpaceID), "upload", targetOf(res, name), err, req.Size, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusCreated, res)
}

// handleUploadCancel 取消上传任务并即时释放预留额度。
//
// 幂等:任务不存在或已结束都返回 204 —— 客户端重试取消不该报错,
// 否则"取消失败"会让用户以为额度还在被占着。
func (d Deps) handleUploadCancel(w http.ResponseWriter, r *http.Request) {
	if d.Uploads == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("上传服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	if id == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少上传任务 id"))
		return
	}
	start := time.Now()
	err := d.Uploads.Cancel(r.Context(), id, a.UserID)
	if err != nil {
		var ae *apierr.Error
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			// 已不存在 == 已经释放过,对取消语义而言就是成功
			err = nil
		}
	}
	d.auditAction(r, "upload.cancel", "", "upload", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// handleUploadFinish 是无内容流的**秒传定稿**入口(BE-S4-04 / 6.10 五细则)。
//
// 语义:客户端在 `/upload/create` 里带了 hash+size → 预检命中 → 服务端下发
// 持物证明挑战(随机偏移 + 一次性 nonce)→ 客户端算样本摘要 → POST 到这里,
// **不重传整份内容**即可完成定稿。
//
// 三条纪律的落点:
//   - 证明校验在**服务层**(finalize.stage)而不是这里:否则换个入口就能绕过;
//   - 失败(证明不匹配/过期/无挑战)按接口族的分钟窗**额外扣配额**并写审计 ——
//     否则"猜样本"是无成本的,挑战就退化成一个哈希预言机;
//   - 样本摘要**不进审计、不进日志**(细则⑤),只记"失败在第几个样本"。
func (d Deps) handleUploadFinish(w http.ResponseWriter, r *http.Request) {
	if d.Uploads == nil || d.TUS == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("上传服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	up, finished, err := d.authorizeUploadMaybeFinished(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// **幂等重放**:服务端已定稿但响应在网络上丢了 → 直接回既有结果,
	// 而不是让客户端重新建任务、重传整份内容(6.10 幂等)。
	// 这一条对秒传尤其重要:nonce 是一次性的,重试时它已经失效,
	// 若不做这层兜底,客户端会因为"证明过期"而被迫重传一个它早就传完的文件。
	if finished {
		v, verr := d.Files.Get(r.Context(), up.UserID, up.TargetFileID)
		if verr != nil {
			apierr.Write(w, r, verr)
			return
		}
		apierr.WriteOK(w, r, http.StatusOK, map[string]any{
			"file": v, "upload_id": up.ID, "replayed": true, "uploaded_bytes": 0,
		})
		return
	}
	var req uploadFinishRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	nonce := strings.TrimSpace(req.Nonce)
	if nonce == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeFastUploadDenied, "缺少 nonce"))
		return
	}
	if len(req.SampleSHA256) > maxSamples {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"样本摘要数量超出上限(%d)", maxSamples))
		return
	}
	for i, s := range req.SampleSHA256 {
		if len(s) != 64 {
			apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
				"第 %d 个样本摘要必须是 64 位十六进制", i+1))
			return
		}
	}

	start := time.Now()
	fin, err := d.TUS.FinishFast(r.Context(), up.model(), nonce, req.SampleSHA256)
	if err != nil {
		// 证明没过的三种原因(无挑战/过期/不匹配)都要让攻击者付出代价。
		// 注意惩罚失败不改响应:Redis 抖动不该把"证明没过"变成 500。
		if isFastUploadDenied(err) {
			_ = middleware.Penalize(r.Context(), d.Cache, d.Limiter,
				d.rateLimitScope("upload"), fastUploadPenaltyCost)
		}
		d.auditAction(r, "upload.finish_fast", up.SpaceID, "upload", up.ID, err, up.DeclaredSize, time.Since(start))
		apierr.Write(w, r, err)
		return
	}
	d.auditAction(r, "upload.finish_fast", up.SpaceID, "upload", up.ID, nil, up.DeclaredSize, time.Since(start))
	kind := model.FeedUpdated
	if fin.NewFile {
		kind = model.FeedCreated
	}
	d.publishSeq(r.Context(), fin.File.SpaceID, fin.File.ID, kind, fin.ChangeSeq)
	apierr.WriteOK(w, r, http.StatusCreated, map[string]any{
		"file":      filesvc.View(fin.File),
		"upload_id": up.ID,
		"deduped":   fin.Deduped,
		"new_file":  fin.NewFile,
		// no_upload_bytes 明确告知客户端"这次一个字节都没传"
		"uploaded_bytes": 0,
	})
}

// uploadFinishRequest 是秒传定稿的请求体。
//
// **只回摘要,不回内容**:服务端已经有这份内容(内容寻址),
// 它需要的只是"证明你也有"。
type uploadFinishRequest struct {
	Nonce string `json:"nonce"`
	// SampleSHA256 与预检响应里的 sample_offsets 一一对应(顺序不可打乱)
	SampleSHA256 []string `json:"sample_sha256"`
}

// maxSamples 是单次请求允许的样本摘要数量上限。
//
// 存在的意义是**封顶服务端读取量**:每个样本要读 4KB 已存对象,
// 不设上限时一个请求可以要求服务端读任意多次(放大攻击)。
const maxSamples = 64

// fastUploadPenaltyCost 是一次挑战失败额外扣除的配额(分钟窗)。
//
// 取值理由:正常上传的分钟窗是几百次量级,而一次合法预检只会失败 0~1 次;
// 扣 10 意味着"连续猜 30 次左右就把自己的窗口耗光" —— 对正常用户无感,
// 对拿别人 hash 反复探样本的行为立刻生效。
const fastUploadPenaltyCost = 10

// isFastUploadDenied 判断错误是否是"持物证明没过"。
func isFastUploadDenied(err error) bool {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		return ae.Code == apierr.CodeFastUploadDenied
	}
	return false
}


// handleUploadHead 返回当前已接收偏移,支撑断点续传(6.10)。
//
// 采用 TUS 的 Upload-Offset 头约定,客户端据此决定从哪一字节继续。
// 偏移来自 uploads.uploaded_bytes(而非临时文件 stat),见 00003 迁移的说明。
func (d Deps) handleUploadHead(w http.ResponseWriter, r *http.Request) {
	if d.Uploads == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("上传服务未装配")))
		return
	}
	up, err := d.authorizeUpload(r)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(up.UploadedBytes, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(up.DeclaredSize, 10))
	w.Header().Set("Tus-Resumable", "1.0.0")
	// 上传进度是私密状态,不许任何中间层缓存
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "X-Upload-Token")
	w.WriteHeader(http.StatusOK)
}

// ---- 辅助 ----

// authorizeUpload 从请求中提取 (upload_id, ticket) 并校验(6.10)。
//
// ticket 通过 X-Upload-Token 头传递,而不是 URL 查询串 ——
// URL 会进 Nginx access log 与浏览器历史,凭据不该出现在那里。
func (d Deps) authorizeUpload(r *http.Request) (*uploadTask, error) {
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证")
	}
	id := r.PathValue("id")
	ticket := r.Header.Get("X-Upload-Token")
	if ticket == "" {
		// TUS 客户端习惯把 token 放 Upload-Metadata,这里放宽兼容
		ticket = ticketFromMetadata(r.Header.Get("Upload-Metadata"))
	}
	up, err := d.Uploads.Authorize(r.Context(), id, a.UserID, ticket)
	if err != nil {
		return nil, err
	}
	return &uploadTask{
		ID: up.ID, SpaceID: up.SpaceID, DeclaredSize: up.DeclaredSize,
		UploadedBytes: up.UploadedBytes, UserID: up.UserID,
		TargetFileID: up.TargetFileID, State: up.State, Model: up,
	}, nil
}

// uploadTask 是 handler 层需要的最小任务视图(避免 handler 直接依赖 model)。
type uploadTask struct {
	ID            string
	SpaceID       string
	DeclaredSize  int64
	UploadedBytes int64
	// UserID / TargetFileID 供幂等重放用(已定稿时直接回既有文件)
	UserID       string
	TargetFileID string
	State        string
	// Model 是完整任务行(秒传定稿需要把 hash/space/parent/name 原样交给 finalize)。
	// 其余字段仍走上面的窄视图 —— handler 不需要的部分就不暴露。
	Model *model.Upload
}

func (u *uploadTask) model() *model.Upload { return u.Model }

// authorizeUploadMaybeFinished 与 authorizeUpload 相同,但允许"已定稿"的任务通过。
//
// 只给秒传定稿入口用:那条路径的 nonce 是一次性的,重试时必须靠任务状态来幂等,
// 而不是靠 nonce(S4-04)。
func (d Deps) authorizeUploadMaybeFinished(r *http.Request) (*uploadTask, bool, error) {
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		return nil, false, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证")
	}
	id := r.PathValue("id")
	ticket := r.Header.Get("X-Upload-Token")
	if ticket == "" {
		ticket = ticketFromMetadata(r.Header.Get("Upload-Metadata"))
	}
	up, finished, err := d.Uploads.AuthorizeOrFinished(r.Context(), id, a.UserID, ticket)
	if err != nil {
		return nil, false, err
	}
	return &uploadTask{
		ID: up.ID, SpaceID: up.SpaceID, DeclaredSize: up.DeclaredSize,
		UploadedBytes: up.UploadedBytes, UserID: up.UserID,
		TargetFileID: up.TargetFileID, State: up.State, Model: up,
	}, finished, nil
}

// ticketFromMetadata 从 TUS Upload-Metadata 头中取 upload_ticket。
//
// 格式:逗号分隔的 `key base64value` 列表。
func ticketFromMetadata(h string) string {
	for _, part := range strings.Split(h, ",") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) != 2 || fields[0] != "upload_ticket" {
			continue
		}
		if v, err := base64Std.DecodeString(fields[1]); err == nil {
			return string(v)
		}
	}
	return ""
}

func spaceOf(res *uploadsvc.CreateResult, fallback string) string {
	if res != nil {
		return res.SpaceID
	}
	return fallback
}

func targetOf(res *uploadsvc.CreateResult, name string) string {
	if res != nil {
		return res.UploadID
	}
	return name
}
