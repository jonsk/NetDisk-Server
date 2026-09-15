//go:build windows

package storage

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

// Usage 返回存储根所在卷的总容量与可用字节。
//
// 为什么不用 Go 标准库:标准库没有跨平台的"磁盘剩余空间"接口
// (os.Stat 只给目录元信息),而"水位过高拒绝新上传"这条保护(9.1/6.1)
// 必须在**收下数据之前**拿到真实剩余量。
//
// Windows 走 GetDiskFreeSpaceExW:它返回的"可用字节"已经扣掉了配额限制与
// 保留给管理员的份额 —— 这正是"还能写多少"的答案;而另一个 API
// (GetDiskFreeSpace) 返回的是**簇**数,在大簇卷上会明显高估可用空间。
func (f *FS) Usage() (total, free int64, err error) {
	root, err := f.volumeRoot()
	if err != nil {
		return 0, 0, err
	}
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	p, perr := syscall.UTF16PtrFromString(root)
	if perr != nil {
		return 0, 0, fmt.Errorf("storage: 路径转换失败: %w", perr)
	}
	var freeToCaller, totalBytes, totalFree uint64
	ret, _, callErr := proc.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&freeToCaller)),
		uintptr(unsafe.Pointer(&totalBytes)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 {
		return 0, 0, fmt.Errorf("storage: 读取磁盘容量失败: %w", callErr)
	}
	return int64(totalBytes), int64(freeToCaller), nil
}

// volumeRoot 取卷根(GetDiskFreeSpaceExW 也接受普通路径,
// 但显式取根可以避免"路径不存在"时报一个看不懂的错误)。
func (f *FS) volumeRoot() (string, error) {
	if f.Root == "" {
		return "", errors.New("storage: 存储根为空")
	}
	v := volumeName(f.Root)
	if v == "" {
		return f.Root, nil
	}
	return v, nil
}
