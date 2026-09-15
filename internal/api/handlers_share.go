package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/sharesvc"
)

// 编辑锁与分享链接的 HTTP 入口(BE-S6-05 / BE-S6-04)。

// POST /api/v1/files/{id}/lock 获取或续期编辑锁(30min,可抢占过期锁)
func (d Deps) handleFileLock(w http.ResponseWriter, r *http.Request) {
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
	lk, err := d.Files.AcquireLock(r.Context(), a.UserID, id)
	d.auditAction(r, "file.lock", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"lock":        lk,
		"ttl_seconds": int64(d.Files.LockTTL().Seconds()),
	})
}

// GET /api/v1/files/{id}/lock 读当前锁(无锁返回 200 + lock: null)
func (d Deps) handleFileLockGet(w http.ResponseWriter, r *http.Request) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	lk, err := d.Files.GetLock(r.Context(), a.UserID, r.PathValue("id"))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 无锁用 200 + null 而不是 404:客户端问的是"锁状态",
	// "没有锁"是一个**正常答案**,不是"资源不存在"
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"lock": lk})
}

// DELETE /api/v1/files/{id}/lock 释放锁(**幂等**)
func (d Deps) handleFileUnlock(w http.ResponseWriter, r *http.Request) {
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
	err := d.Files.ReleaseLock(r.Context(), a.UserID, id)
	d.auditAction(r, "file.unlock", "", "file", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// 分享链接 ------------------------------------------------------------------

type shareCreateRequest struct {
	FileID string `json:"file_id"`
	// Password 为空 = 公开链接
	Password string `json:"password"`
	// ExpiresInHours 0 = 用默认(7 天);负数 = 400
	ExpiresInHours int `json:"expires_in_hours"`
	// MaxDownloads 0 = 不限
	MaxDownloads int `json:"max_downloads"`
}

// POST /api/v1/shares 创建分享链接
func (d Deps) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	if d.Shares == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("分享服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	var req shareCreateRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.Write(w, r, err)
		return
	}
	req.FileID = strings.TrimSpace(req.FileID)
	if req.FileID == "" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 file_id"))
		return
	}
	if req.ExpiresInHours < 0 {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "expires_in_hours 不能为负"))
		return
	}
	// 分享的前提是**你本来就能看它**(4.3:分享不是提权手段)
	if d.Files != nil {
		if _, err := d.Files.Get(r.Context(), a.UserID, req.FileID); err != nil {
			apierr.Write(w, r, err)
			return
		}
	}
	in := sharesvc.CreateInput{
		UserID: a.UserID, FileID: req.FileID,
		Password: req.Password, MaxDownloads: req.MaxDownloads,
	}
	if req.ExpiresInHours > 0 {
		in.ExpiresAt = time.Now().Add(time.Duration(req.ExpiresInHours) * time.Hour)
	}
	start := time.Now()
	sh, err := d.Shares.Create(r.Context(), in)
	d.auditAction(r, "share.create", "", "file", req.FileID, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusCreated, map[string]any{
		"id":             sh.ID,
		"token":          sh.Token,
		"file_id":        sh.FileID,
		"need_password":  sh.HasPassword,
		"expires_at":     sh.ExpiresAt,
		"max_downloads":  sh.MaxDownloads,
		"download_count": sh.DownloadCount,
		// path 便于前端直接拼落地页地址
		"path": "/api/v1/shares/" + sh.Token + "/meta",
	})
}

// GET /api/v1/shares 列出我创建的分享
func (d Deps) handleShareList(w http.ResponseWriter, r *http.Request) {
	if d.Shares == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("分享服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	shares, metas, err := d.Shares.ListMine(r.Context(), a.UserID, 100)
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	items := make([]map[string]any, 0, len(shares))
	for i := range shares {
		items = append(items, map[string]any{
			"id":             shares[i].ID,
			"token":          shares[i].Token,
			"file_id":        shares[i].FileID,
			"need_password":  shares[i].HasPassword,
			"expires_at":     shares[i].ExpiresAt,
			"revoked":        shares[i].Revoked,
			"download_count": shares[i].DownloadCount,
			"max_downloads":  shares[i].MaxDownloads,
			"name":           metas[i].Name,
			"size":           metas[i].Size,
			"is_dir":         metas[i].IsDir,
		})
	}
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{"shares": items})
}

// DELETE /api/v1/shares/{id} 吊销分享
func (d Deps) handleShareRevoke(w http.ResponseWriter, r *http.Request) {
	if d.Shares == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("分享服务未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	id := r.PathValue("id")
	start := time.Now()
	err := d.Shares.Revoke(r.Context(), a.UserID, id)
	d.auditAction(r, "share.revoke", "", "share", id, err, 0, time.Since(start))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	apierr.WriteOK(w, r, http.StatusNoContent, nil)
}

// GET /api/v1/shares/{token}/meta 落地页**免登录**查分享信息
//
// 只回名称/大小/是否需要密码 —— 不需要密码的链接也不回下载地址,
// 避免"meta 变成一个免登录的内容发现接口"。
func (d Deps) handleShareMeta(w http.ResponseWriter, r *http.Request) {
	if d.Shares == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("分享服务未装配")))
		return
	}
	sh, meta, err := d.Shares.Load(r.Context(), r.PathValue("token"))
	if err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 失效链接也要如实告知(否则落地页会显示"文件不存在",而用户手里的链接其实
	// 只是过期了 —— 这两种情况该给不同的提示)
	if !sh.Revoked && (sh.ExpiresAt == nil || sh.ExpiresAt.After(time.Now())) &&
		(sh.MaxDownloads == 0 || sh.DownloadCount < sh.MaxDownloads) {
		w.Header().Set("Cache-Control", "no-store")
		apierr.WriteOK(w, r, http.StatusOK, meta)
		return
	}
	err = goneForShare(sh)
	apierr.Write(w, r, err)
}

// POST/GET /api/v1/shares/{token}/download **免 JWT** 下载(校验 token + 密码)
func (d Deps) handleShareDownload(w http.ResponseWriter, r *http.Request) {
	if d.Shares == nil || d.Files == nil || d.Objects == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("分享服务未装配")))
		return
	}
	token := r.PathValue("token")
	password := r.URL.Query().Get("password")
	if password == "" {
		password = r.Header.Get("X-Share-Password")
	}
	sh, meta, err := d.Shares.Authorize(r.Context(), token, password)
	if err != nil {
		d.auditAction(r, "share.download", "", "share", token, err, 0, 0)
		apierr.Write(w, r, err)
		return
	}
	if meta.IsDir {
		// 目录分享需要打包(异步任务),一期不做 —— 明确告知而不是回一个空文件
		apierr.Write(w, r, apierr.NotImplemented("目录分享暂不支持,请分享单个文件"))
		return
	}
	v, gerr := d.Files.Get(r.Context(), sh.UserID, sh.FileID)
	if gerr != nil {
		apierr.Write(w, r, gerr)
		return
	}
	obj, oerr := d.Objects.Open(r.Context(), v.HashSHA256)
	if oerr != nil {
		apierr.Write(w, r, apierr.Internal(oerr))
		return
	}
	defer func() { _ = obj.Close() }()

	// 下载文件名走 RFC 5987 编码(中文名不能直接进头)
	w.Header().Set("Content-Disposition", contentDisposition(v.Name))
	w.Header().Set("Cache-Control", "private, no-store")
	filesvc.WriteETagHeader(w.Header().Set, v.ETag)
	start := time.Now()
	// Range/206/HEAD 语义交给 ServeContent(与登录态下载同一条实现)
	http.ServeContent(w, r, v.Name, time.Time{}, obj)
	d.auditAction(r, "share.download", "", "share", token, nil, v.Size, time.Since(start))
}

// goneForShare 给出与 sharesvc 一致的 410(带 reason)。
func goneForShare(sh *sharesvc.Share) error {
	reason := "revoked"
	switch {
	case sh.Revoked:
		reason = "revoked"
	case sh.ExpiresAt != nil && !sh.ExpiresAt.After(time.Now()):
		reason = "expired"
	case sh.MaxDownloads > 0 && sh.DownloadCount >= sh.MaxDownloads:
		reason = "download_limit_reached"
	}
	e := apierr.SpaceGone("分享链接已不可用")
	e.Code = apierr.CodeResourceGone
	e.WithDetail("reason", reason)
	return e
}

// contentDisposition 生成带 RFC 5987 形式的 Content-Disposition。
//
// 两步都必要:ASCII 回退名给老客户端,`filename*` 给现代客户端 ——
// 只给中文名会让部分客户端把文件存成 URL 编码后的乱码名。
func contentDisposition(name string) string {
	ascii := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 0x80 && r != '"' && r != '\\' {
			ascii = append(ascii, r)
		} else {
			ascii = append(ascii, '_')
		}
	}
	return "attachment; filename=\"" + string(ascii) + "\"; filename*=UTF-8''" + urlEncode(name)
}

func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0F])
	}
	return b.String()
}
