package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/reqctx"
)

// 文件元数据接口(BE-S4-05 / BE-S6-xx 的第一批)。
//
// 全部需要登录;权限由 filesvc 在服务端判定(4.3),handler 不做任何判权。

// handleFileList 列出目录子项。
//
// GET /api/v1/files?space=&parent=&after=&limit=
//
// 查询参数名以**契约(docs/api/openapi.yaml)与架构文档的写法为准:`space` / `parent`**;
// 历史实现只认 `space_id` / `parent_id`,两种拼法都收(见 queryAlias 的说明)。
//
// 为什么这不是"随手宽容":两套拼法同时存在而只认一套时,**认错的那一套不会报错** ——
// 参数被忽略 → 静默返回个人空间根目录列表。前端会显示一个"看起来正常但位置不对"
// 的列表,而服务端日志里只有一个 200。这类缺陷在 FE-W-02 的契约门禁里被测出来过
// (契约写 `space`,实现读 `space_id`)。
func (d Deps) handleFileList(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	q := r.URL.Query()
	limit := 0
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "limit 必须是正整数"))
			return
		}
		limit = n
	}

	start := time.Now()
	res, err := d.Files.List(r.Context(), filesvc.ListInput{
		UserID:   a.UserID,
		SpaceID:  strings.TrimSpace(queryAlias(q, "space", "space_id")),
		ParentID: strings.TrimSpace(queryAlias(q, "parent", "parent_id")),
		After:    strings.TrimSpace(q.Get("after")),
		Limit:    limit,
	})
	d.auditAction(r, "file.list", spaceOfList(res, queryAlias(q, "space", "space_id")), "dir",
		parentOfList(res, queryAlias(q, "parent", "parent_id")), err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, res)
}

// handleFileGet 取单个文件的元数据。
//
// 同时把 ETag 写进响应头(出口 2)—— 与响应体里的 `etag` 字段**字节一致**,
// 客户端拿哪个都行(见 filesvc.QuoteETag 的契约说明)。
func (d Deps) handleFileGet(w http.ResponseWriter, r *http.Request) {
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
	view, err := d.Files.Get(r.Context(), a.UserID, id)
	d.auditAction(r, "file.get", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	filesvc.WriteETagHeader(w.Header().Set, view.ETag)
	apierr.WriteOK(w, r, http.StatusOK, view)
}

// handleFileHead 只返回元数据头(不返回体),用于同步前的轻量探测。
//
// 与 GET 共用同一个 ETag 出口,保证 HEAD/GET 的 ETag 必然一致 ——
// 客户端常用 HEAD 拿 ETag 再带 If-Match 发写入,两处不一致会导致恒 412。
func (d Deps) handleFileHead(w http.ResponseWriter, r *http.Request) {
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
	h.Set("Content-Type", view.MimeType)
	h.Set("Content-Length", strconv.FormatInt(view.Size, 10))
	h.Set("Cache-Control", "no-store")
	h.Set("X-File-Version", strconv.FormatInt(view.Version, 10))
	h.Set("X-File-Name", encodeHeaderValue(view.Name))
	w.WriteHeader(http.StatusOK)
}

func spaceOfList(res *filesvc.ListResult, fallback string) string {
	if res != nil {
		return res.SpaceID
	}
	return fallback
}

func parentOfList(res *filesvc.ListResult, fallback string) string {
	if res != nil {
		return res.ParentID
	}
	return fallback
}

// encodeHeaderValue 把可能含非 ASCII 的值编码进响应头。
//
// HTTP 头只允许 ASCII;文件名含中文时必须编码,否则部分客户端会拒收整个响应。
// 用百分号编码(与 RFC 5987 的 filename* 一致的思路),客户端解码后即得原名。
func encodeHeaderValue(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x80 && c >= 0x20 && c != '%' && c != '"' && c != '\\' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
