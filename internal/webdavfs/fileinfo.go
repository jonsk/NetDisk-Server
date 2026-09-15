package webdavfs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// FileInfo 是 DAV 的条目元信息。
//
// 它刻意**不是** *model.File 的直接包装:DAV 要的是 os.FileInfo 的三个方法,
// 而 ETag/ContentType 走**独立接口**(ETager/ContentTyper)——上游用类型断言发现它们,
// 因此必须是**导出方法**(`ETag(ctx)` / `ContentType(ctx)`)。
type FileInfo struct {
	ID string
	// EntryName 是条目名。**不能**叫 `Name`:那会与 os.FileInfo 的 `Name()` 方法
	// 同名冲突(Go 不允许同名字段与方法共存)。这个冲突编译器会直接报错,
	// 属于"写错立刻知道"的一类;真正危险的是**漏实现 ETager**(见下)。
	EntryName string
	Dir       bool
	ByteSize  int64
	Modified  time.Time
	// RawETag 是数据库里的**裸值**(不含引号)
	RawETag  string
	MimeType string
}

var _ os.FileInfo = (*FileInfo)(nil)

func (i *FileInfo) Name() string { return i.EntryName }
func (i *FileInfo) Size() int64  { return i.ByteSize }
func (i *FileInfo) IsDir() bool  { return i.Dir }

// Mode 给目录加 fs.ModeDir(DAV 客户端据此判断能不能展开)。
func (i *FileInfo) Mode() fs.FileMode {
	if i.Dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

// ModTime 是 files.updated_at(仅展示;**绝不**作为冲突裁决依据,6.6 用 version)。
func (i *FileInfo) ModTime() time.Time { return i.Modified }

func (i *FileInfo) Sys() any { return nil }

// ETag 实现上游的 ETager 接口(6.12 缺口 2 / R-21)。
//
// **不实现它的后果是静默的**:上游退化为 `ModTime()+Size()` 现算 ETag,
// 于是 PROPFIND 的 getetag 与 If-Match/304 校验全部绕开 `files.etag` ——
// 客户端拿到的 ETag 与 REST 侧的**不是同一个值**,条件请求永远不匹配。
// 这里经 `filesvc.QuoteETag` 出口,与 REST 的响应体/响应头字节一致。
func (i *FileInfo) ETag(context.Context) (string, error) {
	if i.Dir && i.RawETag == "" {
		return "", nil
	}
	return filesvc.QuoteETag(i.RawETag), nil
}

// ContentType 实现上游的 ContentTyper 接口。
//
// 免开文件嗅探:内容寻址下"为了知道 mime 去读文件头"是纯浪费,
// 而 files.mime_type 在上传定稿时已经落库。
func (i *FileInfo) ContentType(context.Context) (string, error) {
	if i.Dir {
		return "", nil
	}
	ct := strings.TrimSpace(i.MimeType)
	if ct == "" {
		// 兜底:按扩展名猜(仍不开文件);再不行给通用二进制类型
		if guess := mime.TypeByExtension(path.Ext(i.EntryName)); guess != "" {
			return guess, nil
		}
		return "application/octet-stream", nil
	}
	if !strings.Contains(ct, "/") {
		// 库里存了非法 mime(历史数据):按扩展名兜底,别把非法值当响应头写出去
		if guess := mime.TypeByExtension(path.Ext(i.EntryName)); guess != "" {
			return guess, nil
		}
		return "application/octet-stream", nil
	}
	return ct, nil
}

// infoFrom 把 files 行转成 DAV FileInfo。
func (f *FileSystem) infoFrom(row *model.File) *FileInfo {
	name := row.Name
	if row.ParentID == "" {
		// 空间根:DAV 的根路径是 "/",条目名用空串(上游会按路径拼 href)
		name = ""
	}
	return &FileInfo{
		ID: row.ID, EntryName: name, Dir: row.IsDir, ByteSize: row.Size,
		Modified: row.UpdatedAt, RawETag: row.Etag, MimeType: row.MimeType,
	}
}

// ---- File 实现 ----

// dirFile 是目录句柄(Readdir 返回子项)。
type dirFile struct {
	info     *FileInfo
	children []os.FileInfo
	pos      int
}

var _ File = (*dirFile)(nil)

func (d *dirFile) Close() error                   { return nil }
func (d *dirFile) Read([]byte) (int, error)       { return 0, os.ErrInvalid }
func (d *dirFile) Seek(int64, int) (int64, error) { return 0, os.ErrInvalid }
func (d *dirFile) Write([]byte) (int, error)      { return 0, os.ErrInvalid }
func (d *dirFile) Stat() (os.FileInfo, error)     { return d.info, nil }

// Readdir 按 count 返回子项;count <= 0 返回全部(上游 PROPFIND Depth:1 用 -1)。
func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	if count <= 0 {
		out := d.children[d.pos:]
		d.pos = len(d.children)
		return out, nil
	}
	if d.pos >= len(d.children) {
		return nil, io.EOF
	}
	end := d.pos + count
	if end > len(d.children) {
		end = len(d.children)
	}
	out := d.children[d.pos:end]
	d.pos = end
	return out, nil
}

// fileReader 是文件句柄(读路径;GET 与 Range 都走它)。
type fileReader struct {
	io.ReadSeekCloser
	info *FileInfo
}

var _ File = (*fileReader)(nil)

func (r *fileReader) Stat() (os.FileInfo, error)         { return r.info, nil }
func (r *fileReader) Readdir(int) ([]os.FileInfo, error) { return nil, os.ErrInvalid }
func (r *fileReader) Write([]byte) (int, error)          { return 0, os.ErrPermission }

// ---- 权限与错误映射 ----

func (f *FileSystem) checkRead(ctx context.Context) error {
	if f.Svc == nil {
		return nil
	}
	return f.Svc.CheckReadable(ctx, f.UserID, f.SpaceID)
}

func (f *FileSystem) checkWrite(ctx context.Context) error {
	if f.Svc == nil {
		return nil
	}
	return f.Svc.CheckWritable(ctx, f.UserID, f.SpaceID)
}

// mapSvcErr 把用例层的 *apierr.Error 映射成 DAV 能理解的标准错误。
//
// 映射规则(每一条都对应一个具体的客户端行为):
//   - 404 → os.ErrNotExist:客户端显示"文件不存在",**不删本地**(远端真的没有)
//   - 403/410 → os.ErrPermission:客户端显示"无权限/空间被移出"
//   - 409 → os.ErrExist:目标被占 / 版本冲突 → 上游按 Overwrite 语义裁决(412)
//   - 其余 → 原样返回(上游给 500)
//
// 为什么不把 410 单独表达:上游 webdav.Handler 只认这几个 sentinel,
// 410 由上层 auth 适配器在**进入 handler 之前**判空间状态时给出(那才能带出
// `space_revoked` 的业务码,让客户端保留本地文件而不是误判"远端删了")。
func mapSvcErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		if errors.Is(err, repo.ErrNotFound) {
			return os.ErrNotExist
		}
		return err
	}
	switch {
	case ae.Status == 404:
		return os.ErrNotExist
	case ae.Status == 403 || ae.Status == 410:
		return os.ErrPermission
	case ae.Status == 409:
		return os.ErrExist
	default:
		return err
	}
}
