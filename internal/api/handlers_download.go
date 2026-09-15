package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/condreq"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/storage"
)

// 文件下载(BE-S6-06:9.3 / 6.5 规则 6)。
//
// 三条硬要求:
//  1. **流式**:内存占用不随文件大小增长 —— 用存储层给出的可寻址流交给
//     `http.ServeContent`,绝不在 handler 里把内容读进内存再切片
//     (后者下载 1GB 文件就要 1GB 内存,是最容易被忽略的 OOM 来源)。
//  2. **Range 精确到字节**:206 + Content-Range 由 ServeContent 实现,
//     我们只需保证给出的是 `io.ReadSeeker` 且长度真实。
//  3. **条件请求**:If-Match / If-None-Match / If-Range 统一走 condreq,
//     且比较口径与 ETag 的加引号出口(6.6)完全同源。

// handleFileDownload 下载文件内容(GET /api/v1/files/{id}/content)。
func (d Deps) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil || d.Objects == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务或存储未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	view, err := d.Files.Get(r.Context(), a.UserID, id)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	if view.IsDir {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "目录不能直接下载"))
		return
	}
	if view.HashSHA256 == "" {
		// 元数据有哈希但内容不在(历史数据/异常路径)—— 明确 404 而不是回空内容,
		// 回空内容会让客户端把文件"下载成功"地截断。
		apierr.Write(w, r, apierr.NotFound("文件内容不存在"))
		return
	}

	// 条件请求:写场景的 412 与读场景的 304 都在这里判定
	cur := condreq.Current{ETag: view.ETag, Exists: true}
	if res := condreq.Evaluate(r.Header, cur); res.Fail {
		// 条件失败时把当前 ETag 回给客户端,便于其更新本地状态后重试
		filesvc.WriteETagHeader(w.Header().Set, view.ETag)
		if res.Reason == "not_modified" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		apierr.Write(w, r, apierr.PreconditionFailed("%s", res.Message))
		return
	}

	obj, err := d.Objects.Open(r.Context(), view.HashSHA256)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			// 元数据在、对象不在 —— 必须显式暴露(运维信号:对象泄漏/被误删)
			apierr.Write(w, r, apierr.NotFound("文件内容不存在(对象缺失)"))
			return
		}
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	defer func() { _ = obj.Close() }()

	h := w.Header()
	// ETag 必须在 ServeContent 之前设置:ServeContent 会用它处理 If-Range
	filesvc.WriteETagHeader(h.Set, view.ETag)
	h.Set("Content-Type", view.MimeType)
	// 下载内容与元数据同源、且带权限判定 → 不允许中间层缓存共享
	h.Set("Cache-Control", "private, no-store")
	h.Set("Accept-Ranges", "bytes")

	// If-Range 不匹配 → **退化为完整 200**(不是错误):客户端据此丢弃半截文件重下
	if !condreq.IfRangeAllows(r.Header.Get("If-Range"), view.ETag) {
		r.Header.Del("Range")
	}

	// ServeContent 负责 Range/206/Content-Range/HEAD 语义。
	// modtime 传零值:我们以 ETag 为唯一校验器,不引入 mtime 弱比较
	// (传 mtime 会让 ServeContent 在 If-Range 为日期时用它比较,与我们的
	//  ETag 口径分叉)。
	start := time.Now()
	http.ServeContent(w, r, view.Name, time.Time{}, obj)

	// 审计:记录下载动作(字节数在 ServeContent 之后已不可得,
	// 因此这里记声明大小 —— 与 4.4 的"这次下载真的完成了吗"仍可比对)
	d.auditAction(r, "file.download", "", "file", id, nil, view.Size, time.Since(start))
}

// handleFileDownloadHead 只返回下载所需的元数据头(内容长度/ETag/可范围请求)。
//
// 与 GET 的差别:不打开对象流。客户端同步前用它判断"是否需要下载"
// (比对 ETag 与本地记录),因此必须与 GET **同一个 ETag 出口**。
func (d Deps) handleFileDownloadHead(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	view, err := d.Files.Get(r.Context(), a.UserID, r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	h := w.Header()
	filesvc.WriteETagHeader(h.Set, view.ETag)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Cache-Control", "private, no-store")
	h.Set("X-File-Version", strconv.FormatInt(view.Version, 10))
	if view.IsDir {
		h.Set("Content-Type", "application/x-directory")
		h.Set("Content-Length", "0")
	} else {
		h.Set("Content-Type", view.MimeType)
		h.Set("Content-Length", strconv.FormatInt(view.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
}
