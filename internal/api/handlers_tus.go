package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// TUS 协议头(6.1 / 3.4)。
//
// 这些常量单独命名而不是散落字符串:客户端实现会逐字匹配头名,
// 拼错一个字母的表现是"客户端永远认为服务端不支持该能力"(静默降级)。
const (
	tusResumable   = "Tus-Resumable"
	tusVersion     = "1.0.0"
	tusOffset      = "Upload-Offset"
	tusLength      = "Upload-Length"
	tusMetadata    = "Upload-Metadata"
	uploadTokenHdr = "X-Upload-Token"
)

// handleTUSOptions 是 TUS 能力探测(OPTIONS /tus)。
//
// 免认证即可访问:客户端在建立上传前用它判断服务端支持哪些扩展与上限,
// 此时还没有任何凭据。响应里**不得**包含任何用户数据。
func (d Deps) handleTUSOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(tusResumable, tusVersion)
	w.Header().Set("Allow", "OPTIONS, POST, HEAD, PATCH, DELETE")
	// 支持的协议扩展(客户端据此启用断点续传与延时）
	w.Header().Set("Tus-Extension", "creation,expiration")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleTUSCreate 是 TUS 建任务(TUS `creation` 扩展的 POST 语义)。
//
// 与 REST 的 `POST /api/v1/upload/create` 共用同一个用例服务 ——
// **不出现第二套建任务逻辑**(否则配额预留或命名校验迟早只落在一边)。
//
// 入参优先取 TUS 头(`Upload-Length` / `Upload-Metadata`),缺省回落到 JSON body,
// 以兼容两种客户端。
func (d Deps) handleTUSCreate(w http.ResponseWriter, r *http.Request) {
	if d.Uploads == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("上传服务未装配")))
		return
	}
	// TUS 要求所有请求带 Tus-Resumable;缺了说明客户端不是 TUS 客户端
	if !hasTUSVersion(r) {
		writeTUSError(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"缺少 Tus-Resumable: 1.0.0 请求头"))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}

	in, err := parseTUSCreate(r)
	if err != nil {
		writeTUSError(w, r, err)
		return
	}
	in.UserID = a.UserID

	start := time.Now()
	res, err := d.Uploads.Create(r.Context(), in)
	d.auditAction(r, "upload.tus_create", spaceOf(res, in.SpaceID), "upload", targetOf(res, in.Name),
		err, in.DeclaredSize, time.Since(start))
	if err != nil {
		writeTUSError(w, r, err)
		return
	}
	// TUS 规范:POST 返回 201 + Location 指向上传资源;凭据放**响应头**而不是 URL
	w.Header().Set(tusResumable, tusVersion)
	w.Header().Set("Location", "/tus/"+res.UploadID)
	w.Header().Set(uploadTokenHdr, res.Ticket)
	w.Header().Set("Cache-Control", "no-store")
	apierr.WriteOK(w, r, http.StatusCreated, map[string]any{
		"upload_id":   res.UploadID,
		"expires_at":  res.ExpiresAt,
		"quota_after": res.QuotaAfter,
	})
}

// handleTUSHead 返回当前偏移(TUS HEAD)。
func (d Deps) handleTUSHead(w http.ResponseWriter, r *http.Request) {
	if !hasTUSVersion(r) {
		writeTUSError(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 Tus-Resumable 请求头"))
		return
	}
	if d.TUS == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("TUS 服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	st, err := d.TUS.Head(r.Context(), r.PathValue("id"), a.UserID, uploadTicket(r))
	if err != nil {
		writeTUSError(w, r, err)
		return
	}
	h := w.Header()
	h.Set(tusResumable, tusVersion)
	h.Set(tusOffset, strconv.FormatInt(st.Offset, 10))
	h.Set(tusLength, strconv.FormatInt(st.Length, 10))
	h.Set("Cache-Control", "no-store")
	// 未完成时告知客户端可以继续；完成态由定稿流程收尾
	if st.Offset >= st.Length {
		h.Set("Upload-Complete", "true")
	}
	w.WriteHeader(http.StatusOK)
}

// handleTUSPatch 追写分片(TUS PATCH)。
//
// 这是**流式**请求体:必须 `Content-Type: application/offset+octet-stream`,
// 且请求体不得被缓冲到内存(Nginx 侧配 `proxy_request_buffering off`)。
func (d Deps) handleTUSPatch(w http.ResponseWriter, r *http.Request) {
	if !hasTUSVersion(r) {
		writeTUSError(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 Tus-Resumable 请求头"))
		return
	}
	if d.TUS == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("TUS 服务未装配")))
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/offset+octet-stream") {
		writeTUSError(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"TUS PATCH 要求 Content-Type: application/offset+octet-stream,实际 %q", ct))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	// Upload-Offset 必填:TUS 用它保证分片按序落盘
	offHdr := r.Header.Get(tusOffset)
	if offHdr == "" {
		writeTUSError(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 Upload-Offset 请求头"))
		return
	}
	expect, perr := strconv.ParseInt(offHdr, 10, 64)
	if perr != nil || expect < 0 {
		writeTUSError(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"Upload-Offset 必须是非负整数,实际 %q", offHdr))
		return
	}
	// 限制单次 PATCH 体积,防止一个请求塞进整个文件把内存/连接占满。
	// 客户端按 chunk(如 256KB~5MB)上传,该上限远高于正常值。
	if r.ContentLength > maxTUSChunkBytes {
		writeTUSError(w, r, apierr.TooLarge("单个分片不能超过 %d 字节", maxTUSChunkBytes))
		return
	}

	start := time.Now()
	res, err := d.TUS.Patch(r.Context(), uploadsvc.PatchInput{
		UploadID:      r.PathValue("id"),
		UserID:        a.UserID,
		Ticket:        uploadTicket(r),
		ExpectOffset:  expect,
		Content:       r.Body,
		ContentLength: r.ContentLength,
	})
	d.auditAction(r, "upload.tus_patch", "", "upload", r.PathValue("id"), err, resBytes(res), time.Since(start))
	if err != nil {
		// 偏移不符要在响应头里回真实偏移,客户端才能自我纠正(不重传整个文件)
		var ae *apierr.Error
		if errors.As(err, &ae) {
			if v, ok := ae.Details["upload_offset"]; ok {
				w.Header().Set(tusOffset, strconv.FormatInt(toInt64(v), 10))
			}
		}
		writeTUSError(w, r, err)
		return
	}
	h := w.Header()
	h.Set(tusResumable, tusVersion)
	h.Set(tusOffset, strconv.FormatInt(res.Offset, 10))
	h.Set("Cache-Control", "no-store")
	if res.Finalized {
		// 定稿完成:回文件 id,客户端可直接进入同步对账
		h.Set("Upload-Complete", "true")
		h.Set("X-File-Id", res.File.ID)
		h.Set("X-File-Version", strconv.FormatInt(res.File.Version, 10))
		// ⚠ **必须在这里推 SSE 事件**:multipart 路径(handlers_upload.go 的
		// finish_fast)在定稿后调 publishSeq,而 TUS 分支原先直接 return ——
		// 结果是"走 TUS 上传的文件不会推事件",依赖 SSE 的同步端看不到变更,
		// 只能等下一次游标读取(实测:tus_sse_frames_in_4s=0,而 multipart 30/30 收到)。
		// 客户端的大文件走的正是 TUS,所以这条漏推是**用户可见**的延迟。
		// 这里用 publishWrite(而不是 publishSeq):它同样在**写操作提交之后**推送,
		// 并自行取序号;PatchResult 不暴露 ChangeSeq,硬凑一个反而会造出错误序号。
		kind := model.FeedUpdated
		if res.NewFile {
			kind = model.FeedCreated
		}
		d.publishWrite(r.Context(), res.File.SpaceID, res.File.ID, kind)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTUSDelete 取消上传(TUS DELETE):释放预留 + 删除暂存文件。
func (d Deps) handleTUSDelete(w http.ResponseWriter, r *http.Request) {
	if d.TUS == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("TUS 服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	err := d.TUS.Cancel(r.Context(), id, a.UserID)
	d.auditAction(r, "upload.tus_cancel", "", "upload", id, err, 0, time.Since(start))
	if err != nil {
		// 取消幂等:已不存在视为成功(与 REST DELETE 语义一致)
		var ae *apierr.Error
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			err = nil
		}
	}
	if err != nil {
		writeTUSError(w, r, err)
		return
	}
	w.Header().Set(tusResumable, tusVersion)
	w.WriteHeader(http.StatusNoContent)
}

// maxTUSChunkBytes 是单个 PATCH 分片的体量上限。
//
// 取 32MB:远高于任何合理 chunk(客户端一般 256KB~5MB),
// 又能拦住"一个请求传完整个文件"这种把暂存 IO 与连接都占满的用法。
const maxTUSChunkBytes = 32 << 20

// parseTUSCreate 解析建任务入参:TUS 头优先,JSON body 兜底。
func parseTUSCreate(r *http.Request) (uploadsvc.CreateInput, error) {
	var in uploadsvc.CreateInput
	if v := r.Header.Get(tusLength); v != "" {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n < 0 {
			return in, apierr.BadRequest(apierr.CodeInvalidArgument, "Upload-Length 必须是非负整数")
		}
		in.DeclaredSize = n
	}
	// Upload-Metadata 是 TUS 的 `key base64value` 逗号分隔列表
	if md := r.Header.Get(tusMetadata); md != "" {
		meta, err := parseTUSMetadata(md)
		if err != nil {
			return in, err
		}
		if meta["filename"] != "" {
			in.Name = meta["filename"]
		}
		if meta["space_id"] != "" {
			in.SpaceID = meta["space_id"]
		}
		// allow_overwrite:显式覆盖意图(契约 UploadCreateRequest.allow_overwrite)。
		// TUS 用 metadata 传布尔:接受 "1"/"true"(大小写不敏感),其余一律当 false ——
		// 宁可多一次 409 让客户端显式处理,也不猜用户的意图。
		if v := meta["allow_overwrite"]; v == "1" || strings.EqualFold(v, "true") {
			in.AllowOverwrite = true
		}
		if meta["parent_id"] != "" {
			in.ParentID = meta["parent_id"]
		}
		if meta["hash"] != "" {
			in.DeclaredHash = strings.ToLower(meta["hash"])
		}
	}
	// 头里没有的字段允许用 JSON body 补(兼容 REST 风格客户端)
	if in.Name == "" || in.DeclaredSize == 0 {
		var body struct {
			SpaceID  string `json:"space_id"`
			ParentID string `json:"parent_id"`
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			Hash     string `json:"hash"`
		}
		if r.ContentLength != 0 {
			// 复用与 REST 入口同一个严格解码器:拒绝未知字段、限制体积。
			// 这里直接作用于 r.Body(而非经过 ResponseWriter 包一层 MaxBytesReader),
			// 因为 TUS 建任务请求是小 JSON,不需要写错误响应时回退文件。
			if err := decodeStrictBody(r, &body); err != nil {
				return in, err
			}
		}
		if in.Name == "" {
			in.Name = strings.TrimSpace(body.Name)
		}
		if in.DeclaredSize == 0 {
			in.DeclaredSize = body.Size
		}
		if in.SpaceID == "" {
			in.SpaceID = strings.TrimSpace(body.SpaceID)
		}
		if in.ParentID == "" {
			in.ParentID = strings.TrimSpace(body.ParentID)
		}
		if in.DeclaredHash == "" {
			in.DeclaredHash = strings.ToLower(strings.TrimSpace(body.Hash))
		}
	}
	if strings.TrimSpace(in.Name) == "" {
		return in, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少文件名(Upload-Metadata 的 filename 或 JSON body 的 name)")
	}
	return in, nil
}

// parseTUSMetadata 解析 TUS `Upload-Metadata` 头。
//
// 格式:`key1 base64v1,key2 base64v2`(值必须 base64,可省略值只留 key 表示布尔)。
// 严格解析而不是"容错跳过":元数据里带着文件名与目标目录,
// 静默忽略解析失败的项会让文件落到错误的位置(用户看到"传上去了但不在我选的目录")。
func parseTUSMetadata(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.SplitN(part, " ", 2)
		key := strings.TrimSpace(fields[0])
		if key == "" {
			return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "Upload-Metadata 项缺少键名")
		}
		if len(fields) == 1 {
			out[key] = "" // 布尔型元数据(如 is_confidential)
			continue
		}
		val, err := base64Std.DecodeString(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, apierr.BadRequest(apierr.CodeInvalidArgument,
				"Upload-Metadata 的 %s 值不是合法 base64", key)
		}
		out[key] = string(val)
	}
	return out, nil
}

// uploadTicket 取上传凭据:优先专用头,其次 TUS 元数据(部分客户端习惯)。
func uploadTicket(r *http.Request) string {
	if v := r.Header.Get(uploadTokenHdr); v != "" {
		return v
	}
	return ticketFromMetadata(r.Header.Get(tusMetadata))
}

func hasTUSVersion(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get(tusResumable)) == tusVersion
}

// writeTUSError 写 TUS 错误响应:仍带 Tus-Resumable,便于客户端识别协议层错误。
func writeTUSError(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set(tusResumable, tusVersion)
	apierr.Write(w, r, err)
}

func resBytes(res *uploadsvc.PatchResult) int64 {
	if res == nil {
		return 0
	}
	return res.Offset
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}
