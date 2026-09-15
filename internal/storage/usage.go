package storage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ObjectUsage 统计 `objects/` 树的**实际磁盘占用**(文件数与字节数)。
//
// 用途只有一个:与 PG 里 `SELECT sum(size) FROM file_objects WHERE state <> 'deleted'`
// 对照,发现**正向泄漏**(BE-S10-04 / 8.4:磁盘"只涨不跌"从此可见)。
//
// 为什么不让调用方自己 walk:对象目录的布局(两级散列、根名 `objects`)是
// storage 包的私有知识;调用方自己拼路径,某天布局一改就会静默统计到一个空目录
// ——差值恒为负,而"巡检说没问题"比不巡检更危险。
//
// 三条纪律:
//
//  1. **只读**:不创建、不删除、不改名。巡检绝不能"顺手修复"——
//     误判(例如正好在读一个正在写入的临时文件)会被自动修复放大成数据损坏。
//  2. **跳过临时文件**:原子写会先落 `.tmp-*` 再改名;把它们算进去会让
//     正常上传期间的差值抖动(而那正是巡检最可能被判成泄漏的时刻)。
//  3. 目录读不动(权限/IO)时**返回错误而不是 0**:0 会让差值恒为正、
//     看起来像"泄漏了几个 T",把一个巡检故障伪装成数据事故。
func (f *FS) ObjectUsage(ctx context.Context) (files int64, bytes int64, err error) {
	root := filepath.Join(f.Root, "objects")
	if _, serr := os.Stat(root); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			// 根还没建(全新部署且从未上传):0/0 是事实,不是故障
			return 0, 0, nil
		}
		return 0, 0, serr
	}
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-") || strings.HasSuffix(name, ".tmp") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		files++
		bytes += info.Size()
		return nil
	})
	if walkErr != nil {
		return 0, 0, walkErr
	}
	return files, bytes, nil
}

// ReapTempObjects 回收对象区里**够老的原子写临时文件**(`.tmp-*` / `*.tmp`)。
//
// 为什么需要它:`Write` 先在对象目录落 `.tmp-*` 再 rename;进程在 rename 前崩溃
// (kill -9 / OOM / 断电)会把它留下。而 `ObjectUsage` **刻意跳过**这些文件(否则正常上传
// 期间的巡检差值会抖动,把"正在上传"误报成泄漏),于是**没有任何人回收** ——
// 真空洞是"崩几次就慢慢吃盘,而巡检差值恒为 0"。
//
// 三条纪律:
//  1. **只扫对象区**:暂存区(tus-tmp)有自己的回收任务与文件名白名单,这里不越界;
//  2. **只删够老的**:正在写的临时文件不能被删(那正是原子写要保护的东西),
//     所以要么比阈值老、要么不动;
//  3. **只读之外只做删除,且失败不致命**:单个文件删不掉要如实计数返回,
//     而不是让整轮失败(否则一个被占用的文件会让清理永久停摆)。
func (f *FS) ReapTempObjects(ctx context.Context, olderThan time.Duration, now time.Time) (files int, bytes int64, err error) {
	root := filepath.Join(f.Root, "objects")
	if _, serr := os.Stat(root); serr != nil {
		if errors.Is(serr, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, serr
	}
	cutoff := now.Add(-olderThan)
	var firstErr error
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			if firstErr == nil {
				firstErr = werr
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, ".tmp-") && !strings.HasSuffix(name, ".tmp") {
			return nil // 只认原子写的临时文件,绝不碰正式对象
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil // 还年轻:可能正在写
		}
		if rerr := os.Remove(p); rerr != nil {
			if firstErr == nil {
				firstErr = rerr
			}
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	if walkErr != nil {
		return files, bytes, walkErr
	}
	return files, bytes, firstErr
}
