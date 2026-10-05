package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
