package storage

import (
	"path/filepath"
	"strings"
)

// volumeName 返回路径所在卷的根(Windows: `D:\`;其它平台返回空串)。
//
// 刻意**不用** filepath.VolumeName 直接判平台:本函数编译到所有平台,
// 而 `filepath.VolumeName` 在 Unix 上永远返回空串 —— 那样 Windows 的
// 盘符识别就悄悄失效了(表现为"水位保护从不触发"),而不是报错。
func volumeName(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return p[:2] + string(filepath.Separator)
	}
	// UNC 路径(\\server\share)暂时回落到路径本身:GetDiskFreeSpaceExW 能处理它
	if strings.HasPrefix(p, `\\`) {
		return p
	}
	return ""
}
