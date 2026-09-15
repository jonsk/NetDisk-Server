package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// volumeName 是**包内**函数,只能在这里测。它是"磁盘水位保护"能否生效的关键一环:
// Windows 上 `GetDiskFreeSpaceExW` 需要一个卷路径,拿到空串就会退化成"用量未知",
// 而水位保护在"用量未知"时**不拦截** —— 于是磁盘写满才会暴露,那时已经晚了。
//
// 这里刻意断言"盘符必须被识别",而不是断言"和 filepath.VolumeName 一致":
// 后者在 Unix 上恒为空串,正是本函数当初绕开它的原因(见 disk_path.go 注释)。

func TestVolumeNameRecognizesDriveLetter(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"D:\\WorkSpace\\GO", "D:" + string(filepath.Separator)},
		{"d:\\data", "d:" + string(filepath.Separator)},
		{"C:/data", "C:" + string(filepath.Separator)},
		{"/opt/netdisk/data", ""},
		{"", ""},
		{"x", ""},
	}
	for _, c := range cases {
		if got := volumeName(c.in); got != c.want {
			t.Errorf("volumeName(%q) = %q,期望 %q", c.in, got, c.want)
		}
	}
}

// UNC 路径(`\\server\share\...`):没有盘符,GetDiskFreeSpaceExW 能直接吃共享路径,
// 因此原样返回;若这里返回空串,网络存储上的部署水位保护就静默失效。
func TestVolumeNameKeepsUNCPath(t *testing.T) {
	p := `\\server\share\netdisk-data`
	got := volumeName(p)
	if got != p {
		t.Fatalf("UNC 路径应原样返回,实际 %q", got)
	}
	if got == "" {
		t.Fatal("UNC 路径不能返回空串(否则水位保护静默失效)")
	}
}

// 与 Usage 联动:把 Root 放在一个**真实存在的卷**上,必须拿到可信的容量数值。
// (Windows 走 volumeName + GetDiskFreeSpaceExW,Unix 走 Statfs —— 两条实现都要过。)
func TestUsageWorksForRealRoot(t *testing.T) {
	root := t.TempDir()
	f, err := NewFS(root)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	total, free, err := f.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if total <= 0 || free < 0 || free > total {
		t.Fatalf("容量数值不合理:total=%d free=%d", total, free)
	}
	if v := volumeName(filepath.Clean(root)); v == "" {
		// 这是**平台相关**的:Unix 上本来就没有卷名,不算失败,但要显式说明
		if filepath.Separator != '\\' {
			t.Logf("非 Windows 平台:volumeName(%q) 为空属预期(Unix 用 Statfs 直接统计 Root)", root)
		} else {
			t.Fatalf("Windows 上 volumeName(%q) 不应为空", root)
		}
	}
}

// objects/ 整个目录不存在(全新部署且从未上传):ObjectUsage 必须回 0/0 而不是报错,
// 否则首次巡检就会红("未开始使用"不是故障)。
// 目录存在但读不动才是故障 —— 那条由错误返回而不是 0 来表达。
func TestObjectUsageAbsentRootIsZeroNotError(t *testing.T) {
	root := t.TempDir()
	f, err := NewFS(root)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "objects")); err != nil {
		t.Fatalf("移除 objects 目录失败: %v", err)
	}
	files, bytesUsed, err := f.ObjectUsage(t.Context())
	if err != nil {
		t.Fatalf("objects/ 不存在应回 0/0 而非错误: %v", err)
	}
	if files != 0 || bytesUsed != 0 {
		t.Fatalf("应为 0/0,实际 %d/%d", files, bytesUsed)
	}
}

// 暂存区文件名白名单:ListStages 只认 `<upload_id>.chunk`。
// 清理任务按这个列表删文件 —— 白名单一旦放宽,人工放进 tus-tmp 的运维文件、
// 原子写的临时文件都会被"清理"掉。
func TestListStagesIgnoresNonChunkFiles(t *testing.T) {
	f, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	dir, err := f.StageDir()
	if err != nil {
		t.Fatalf("StageDir: %v", err)
	}
	keep := "0f7c1b2e-1111-2222-3333-444455556666"
	files := map[string]string{
		keep + ".chunk":            "real",
		"tmp-1234.tmp":             "temp",
		"readme.txt":               "human",
		"tus-tmp-backup.chunk.bak": "backup",
		"not a uuid.chunk":         "space",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
			t.Fatalf("造文件 %s 失败: %v", name, err)
		}
	}
	got, err := f.ListStages()
	if err != nil {
		t.Fatalf("ListStages: %v", err)
	}
	if len(got) != 1 || got[0] != keep {
		t.Fatalf("只应列出 %q,实际 %v", keep, got)
	}
}

// 暂存路径必须堵死路径穿越:upload_id 来自 URL 路径,拼进文件名前必须校验。
// 少了这一步,`../../etc/passwd` 这类 id 就能让服务端在任意位置建/删文件。
func TestStagePathRejectsTraversal(t *testing.T) {
	f, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	for _, bad := range []string{
		"../../etc/passwd",
		"..",
		"a/b",
		`a\b`,
		"a b",
		"0f7c1b2e-1111-2222-3333-444455556666.chunk",
		strings.Repeat("a", 65),
		"----",
		"",
	} {
		p, err := f.StagePath(bad)
		if err == nil {
			t.Errorf("非法 upload_id %q 必须被拒绝,实际返回 %q", bad, p)
		}
		if p != "" {
			t.Errorf("非法 upload_id %q 不得产出路径,实际 %q", bad, p)
		}
	}
	// 正例:合法 UUID 形态必须通过,且落在 tus-tmp 下
	p, err := f.StagePath("0F7C1B2E-1111-2222-3333-444455556666")
	if err != nil {
		t.Fatalf("合法 upload_id 应通过: %v", err)
	}
	if filepath.Base(p) != "0F7C1B2E-1111-2222-3333-444455556666.chunk" {
		t.Fatalf("暂存路径命名不符: %s", p)
	}
	if !strings.Contains(p, "tus-tmp") {
		t.Fatalf("暂存文件应落在 tus-tmp 下: %s", p)
	}
}
