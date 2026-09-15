package storage_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/storage"
)

// BE-S5-10:对象区里的原子写临时文件必须能被回收,而且**只回收该回收的**。
//
// 这条断言的价值在于三条"不能碰":正在写的临时文件、正式对象、以及不认识的文件名。
func TestReapTempObjects(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	dir := filepath.Join(f.Root, "objects", "ab", "cd")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	write := func(name string, size int, mtime time.Time) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o640); err != nil {
			t.Fatalf("造文件 %s 失败: %v", name, err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("改 mtime 失败: %v", err)
		}
		return p
	}
	oldTmp := write(".tmp-old", 100, old)           // 够老的临时文件 → 应删
	oldTmp2 := write("leftover.tmp", 50, old)       // 后缀 .tmp 同样算临时文件 → 应删
	freshTmp := write(".tmp-fresh", 30, time.Now()) // 正在写 → 不能删
	real := write("abcdef0123456789", 200, old)     // 正式对象(名字像 hash)→ 不能删
	other := write("notes.txt", 10, old)            // 不认识的名字 → 不能删

	files, bytes, err := f.ReapTempObjects(ctx, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatalf("ReapTempObjects 失败: %v", err)
	}
	if files != 2 || bytes != 150 {
		t.Fatalf("应回收 2 个文件 / 150 字节,实际 %d 个 / %d 字节", files, bytes)
	}
	for _, p := range []string{oldTmp, oldTmp2} {
		if _, serr := os.Stat(p); serr == nil {
			t.Errorf("够老的临时文件应被删除: %s", p)
		}
	}
	for _, p := range []string{freshTmp, real, other} {
		if _, serr := os.Stat(p); serr != nil {
			t.Errorf("这个文件不该被删: %s(%v)", p, serr)
		}
	}
	// 幂等:再跑一轮没有可删的
	files2, _, err := f.ReapTempObjects(ctx, 24*time.Hour, time.Now())
	if err != nil || files2 != 0 {
		t.Fatalf("第二轮应无文件可删: files=%d err=%v", files2, err)
	}
}

// object_key 一致性校验(BE-S5-11):空值不算不一致,布局不符才算。
func TestMismatchedObjectKeys(t *testing.T) {
	keyFor := func(h string) string {
		if len(h) != 64 {
			return ""
		}
		return "objects/" + h[0:2] + "/" + h[2:4] + "/" + h
	}
	h := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	ok := keyFor(h)
	rows := [][2]string{
		{h, ok},             // 一致
		{h, ""},             // 没记录 → 不算不一致(历史行允许为空)
		{h, "objects/" + h}, // 布局不符(少了两级分桶)→ 不一致
		{"short", ok},       // 非法 hash:keyFor 返回空 → 不算不一致(由别的校验负责)
	}
	bad := storage.MismatchedObjectKeys(keyFor, rows)
	if len(bad) != 1 || bad[0] != h {
		t.Fatalf("应恰好报出 1 行不一致,实际 %v", bad)
	}
}
