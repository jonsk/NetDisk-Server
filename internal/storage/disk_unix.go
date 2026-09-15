//go:build !windows

package storage

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Usage 返回存储根所在文件系统的总容量与可用字节(POSIX:statfs)。
//
// Bavail 而不是 Bfree:前者是"非特权用户可用的块数"(已扣掉 root 保留份额),
// 而服务进程正是以普通用户运行的 —— 用 Bfree 会在磁盘将满时高估可用量,
// 于是水位保护在最需要它的时候不触发。
func (f *FS) Usage() (total, free int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(f.Root, &st); err != nil {
		return 0, 0, fmt.Errorf("storage: 读取磁盘容量失败: %w", err)
	}
	bs := int64(st.Bsize)
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs, nil
}
