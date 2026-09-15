// Package webdavfs 是 x/net/webdav 的**PG 后端适配层**(BE-S8-01/02 / 6.12 缺口 2)。
//
// # 为什么必须有这一层
//
// `x/net/webdav` 自带的 `webdav.Dir` 直接把请求路径当文件系统路径用 ——
// 对内容寻址的网盘完全不适用(目录树与文件名只在 PG 里,磁盘上是 `objects/xx/xx/hash`)。
// 因此必须实现 `webdav.FileSystem` / `File` / `FileInfo` 三个接口,把
// "路径操作"映射到 `files` 表。
//
// # 两条会**静默出错**的纪律
//
//  1. **`FileInfo` 必须实现 `ETager`**(缺口 2)。不实现时,上游用
//     `ModTime()+Size()` 现算 ETag:PROPFIND 的 `getetag` 与 304/If-Match 校验
//     会**静默绕开** `files.etag` —— 表现是"客户端明明拿着 ETag,条件请求却永远
//     不匹配(或永远匹配)",而所有代码单独看都没错。ETag 经 `filesvc.QuoteETag`
//     出口,与 REST 的响应体/响应头**字节一致**(R-21 三出口同源)。
//  2. **`Rename` 不做静默覆盖**。上游 MOVE/COPY 的 Overwrite 语义已在 handleMove/
//     handleCopy 里判过目标是否存在;但 `FileSystem.Rename` 自身也必须**显式**
//     拒绝覆盖(返回 `os.ErrExist`)—— 因为覆盖一个**内容不同**的文件是数据丢失,
//     而"上游判过了"这句话在将来有人直接调用本层时会不成立。
//
// # 权限
//
// 每个方法都先判权(4.3 权限只在服务端):读路径要求可读,写路径要求可写。
// 判权失败时**不能**返回 os.ErrNotExist:那会让客户端以为"远端删了"而删本地文件
// (6.4 P1-2 的教训)。这里用 `apierr` 的 403/410 语义,由上层 auth 适配器转成
// DAV 状态码。
package webdavfs

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"

	"golang.org/x/net/webdav"

	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// File / FileSystem 用**类型别名**指向上游接口,而不是自己声明同形接口。
//
// 原因很实在:Go 的接口满足是**名义上的** —— 自己声明一个"长得一样"的 File
// 接口并不会让 `OpenFile` 的返回值满足 `webdav.FileSystem`(返回类型是接口类型,
// 必须**就是**那个接口)。别名让两侧永远一致,也避免了"签名看着对、赋值报错"的折腾。
type File = webdav.File

// FileSystemIface 是上游接口别名(便于在测试里断言"确实满足上游契约")。
type FileSystemIface = webdav.FileSystem

// MaxPropfindItems 是单次 PROPFIND 的条目上限(BE-S8-04 验收①)。
//
// 为什么要有上限:资源管理器对深层目录会发 Depth: infinity(部分客户端会),
// 而"一次列 10 万个孩子"会在单请求里做巨量序列化 —— 上游不会替我们封顶。
const MaxPropfindItems = 1000

// FileSystem 把 WebDAV 路径映射到某个空间的 PG 目录树。
//
// 每个实例绑定 (userID, spaceID):WebDAV 客户端映射一个网络位置 = 一个空间,
// 而权限判定的最小单位正是空间(4.3)。
type FileSystem struct {
	Spaces  repo.SpaceRepo
	Files   repo.FileRepo
	Svc     *filesvc.Service
	Objects storage.Storager

	// DB 是数据库入口(读走池、写走事务)
	DB Querier

	UserID  string
	SpaceID string
	// RootID 是空间根目录行 id(缓存,避免每次操作都查一次)
	RootID string
}

// Querier 是仓储读写所需的数据库能力(与 filesvc.DB 同构:读走池、写走事务)。
type Querier = filesvc.DB

// ---- 路径解析 ----

// clean 归一 DAV 路径:上游传的是 URL 路径("/a/b"),必须以 "/" 开头且无尾斜杠。
func clean(name string) string {
	if name == "" {
		return "/"
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	c := path.Clean(name)
	return c
}

// split 把 DAV 路径拆成 (父目录路径, 末段名)。根返回 ("", "")。
func split(name string) (string, string) {
	c := clean(name)
	if c == "/" {
		return "", ""
	}
	return path.Dir(c), path.Base(c)
}

// resolve 把 DAV 路径解析为 files 行。
//
// 逐段 `GetChildByName` 走:父目录的 id + lower(name) 唯一索引已经能定位,
// 不需要递归 CTE。段数受 namepolicy 的深度上限约束(≤31),因此最多 31 次往返 ——
// 换来的是"每一段都做一次权限与存在性检查"的清晰语义。
func (f *FileSystem) resolve(ctx context.Context, q Querier, name string) (*model.File, error) {
	c := clean(name)
	if c == "/" {
		if f.RootID == "" {
			root, err := f.Files.GetRoot(ctx, q, f.SpaceID)
			if err != nil {
				return nil, err
			}
			f.RootID = root.ID
		}
		return f.Files.GetByID(ctx, q, f.RootID)
	}
	parent := f.RootID
	if parent == "" {
		root, err := f.Files.GetRoot(ctx, q, f.SpaceID)
		if err != nil {
			return nil, err
		}
		f.RootID = root.ID
		parent = root.ID
	}
	segs := strings.Split(strings.TrimPrefix(c, "/"), "/")
	for i, seg := range segs {
		row, err := f.Files.GetChildByName(ctx, q, f.SpaceID, parent, seg)
		if err != nil {
			return nil, err // repo.ErrNotFound
		}
		if i == len(segs)-1 {
			return row, nil
		}
		if !row.IsDir {
			// 中间段不是目录:对 DAV 而言等价于"路径不存在"
			// (不使用 ErrNotADirectory 之类的细分:客户端只认 404)
			return nil, repo.ErrNotFound
		}
		parent = row.ID
	}
	return nil, repo.ErrNotFound
}

// Resolve 是 route 层需要的最小能力:把 DAV 路径解析为 files 行。
//
// 导出理由:PUT 拦截与条件请求校验都要**先看目标当前是什么**才能裁决,
// 而那两步发生在进入上游 handler 之前(见 api/handlers_webdav.go 的分工说明)。
func (f *FileSystem) Resolve(ctx context.Context, name string) (*model.File, error) {
	return f.resolve(ctx, f.db(), name)
}

// SplitPath 把 DAV 路径拆成 (父目录路径, 末段名),供 PUT 定位父目录。
func SplitPath(name string) (string, string) { return split(name) }

// Mkdir 建目录(BE-S8-01:映射到 files 行)。
func (f *FileSystem) Mkdir(ctx context.Context, name string, _ os.FileMode) error {
	if err := f.checkWrite(ctx); err != nil {
		return err
	}
	parentPath, base := split(name)
	if base == "" {
		// MKCOL / == 根:已存在(客户端会再发一次 MKCOL,幂等返回 405 更准确,
		// 但返回 os.ErrExist 让上游给 405 也合理)
		return os.ErrExist
	}
	parent, err := f.resolve(ctx, f.db(), parentPath)
	if err != nil {
		return err
	}
	if !parent.IsDir {
		return os.ErrNotExist
	}
	_, cerr := f.Files.CreateDir(ctx, f.db(), repo.CreateDirInput{
		SpaceID: f.SpaceID, ParentID: parent.ID, OwnerID: f.UserID,
		Name: base, Depth: parent.Depth + 1,
	})
	if cerr != nil {
		if errors.Is(cerr, repo.ErrConflict) {
			return os.ErrExist
		}
		return cerr
	}
	return nil
}

// RemoveAll 删除文件或整棵子树(DELETE 方法)。
func (f *FileSystem) RemoveAll(ctx context.Context, name string) error {
	if err := f.checkWrite(ctx); err != nil {
		return err
	}
	if clean(name) == "/" {
		// 删根 = 解散空间,绝不由 DELETE 触发
		return os.ErrPermission
	}
	row, err := f.resolve(ctx, f.db(), name)
	if err != nil {
		return err
	}
	if _, derr := f.Svc.Delete(ctx, f.UserID, row.ID); derr != nil {
		return mapSvcErr(derr)
	}
	return nil
}

// Rename 移动/改名(MOVE 方法)。
//
// **显式拒绝覆盖**:目标已存在 → os.ErrExist。上游 handleMove 在 Overwrite: F 时
// 已判过一遍,但本层不能依赖那个判断(见包注释纪律 2)。
func (f *FileSystem) Rename(ctx context.Context, oldName, newName string) error {
	if err := f.checkWrite(ctx); err != nil {
		return err
	}
	src, err := f.resolve(ctx, f.db(), oldName)
	if err != nil {
		return err
	}
	dstParentPath, dstBase := split(newName)
	dstParent, err := f.resolve(ctx, f.db(), dstParentPath)
	if err != nil {
		return err
	}
	if !dstParent.IsDir {
		return os.ErrNotExist
	}
	// 目标已存在 → 不做静默覆盖(见包注释)
	if existing, gerr := f.Files.GetChildByName(ctx, f.db(), f.SpaceID, dstParent.ID, dstBase); gerr == nil {
		if existing.ID != src.ID {
			return os.ErrExist
		}
	}
	if _, merr := f.Svc.Move(ctx, filesvc.MoveInput{
		UserID: f.UserID, FileID: src.ID, NewParentID: dstParent.ID, NewName: dstBase,
	}); merr != nil {
		return mapSvcErr(merr)
	}
	return nil
}

// Stat 返回一条目的元信息(实现 ETager/ContentTyper,见 fileinfo.go)。
func (f *FileSystem) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	if err := f.checkRead(ctx); err != nil {
		return nil, err
	}
	row, err := f.resolve(ctx, f.db(), name)
	if err != nil {
		return nil, err
	}
	return f.infoFrom(row), nil
}

// OpenFile 打开文件或目录(flag 决定读/写/建)。
//
// 上游对 PUT 的调用序列是 OpenFile(O_RDWR|O_CREATE|O_TRUNC) → Write* → Close。
// 本层的写入路径**不在** Write 里落盘,而是把内容收到内存/临时缓冲、在 Close 时
// 交给统一定稿(6.10);但生产路径上 PUT 在进入 DAV handler **之前**就被拦截并
// 直接走 finalizeUpload(BE-S5-06),因此这里的写分支只作为兜底(且明确报错而非
// 静默成功 —— "PUT 成功但内容没进去"是最坏的结果)。
func (f *FileSystem) OpenFile(ctx context.Context, name string, flag int, _ os.FileMode) (File, error) {
	c := clean(name)

	// 目录:读目录列表
	if flag == os.O_RDONLY || flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) == 0 {
		row, err := f.resolve(ctx, f.db(), c)
		if err != nil {
			return nil, err
		}
		if row.IsDir {
			if err := f.checkRead(ctx); err != nil {
				return nil, err
			}
			children, lerr := f.listChildren(ctx, row.ID)
			if lerr != nil {
				return nil, lerr
			}
			return &dirFile{info: f.infoFrom(row), children: children}, nil
		}
		if err := f.checkRead(ctx); err != nil {
			return nil, err
		}
		if flag&(os.O_WRONLY|os.O_RDWR) != 0 {
			// 以写方式打开一个已存在的文件:必须走 PUT 拦截路径(见上)
			return nil, os.ErrPermission
		}
		if row.HashSHA256 == "" || f.Objects == nil {
			return nil, os.ErrNotExist
		}
		rc, oerr := f.Objects.Open(ctx, row.HashSHA256)
		if oerr != nil {
			if errors.Is(oerr, storage.ErrNotFound) {
				return nil, os.ErrNotExist
			}
			return nil, oerr
		}
		return &fileReader{ReadSeekCloser: rc, info: f.infoFrom(row)}, nil
	}

	// 写分支:明确拒绝(生产由 PUT 拦截器处理)
	return nil, os.ErrPermission
}

// listChildren 列出直接子项(最多 MaxPropfindItems 条)。
func (f *FileSystem) listChildren(ctx context.Context, parentID string) ([]os.FileInfo, error) {
	rows, err := f.Files.ListChildren(ctx, f.db(), f.SpaceID, parentID, MaxPropfindItems, "")
	if err != nil {
		return nil, err
	}
	out := make([]os.FileInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, f.infoFrom(row))
	}
	return out, nil
}

// db 是内部统一取用数据库入口(便于将来换成事务感知的实现)。
func (f *FileSystem) db() Querier { return f.DB }
