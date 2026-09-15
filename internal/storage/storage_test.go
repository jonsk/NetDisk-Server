package storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/netdisk/netdisk/internal/storage"
)

func newFS(t *testing.T) *storage.FS {
	t.Helper()
	fs, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// BE-S5-01 依赖的存储层不变量(内容寻址 + 原子提交)。

func TestKeyForContentAddressed(t *testing.T) {
	fs := newFS(t)
	h := hashOf([]byte("x"))
	key := fs.KeyFor(h)
	// objects/{xx}/{xx}/{sha256}:两级散列目录
	want := "objects/" + h[0:2] + "/" + h[2:4] + "/" + h
	if key != want {
		t.Fatalf("对象键应为 %q,实际 %q", want, key)
	}
	// 两级散列:防止单目录文件数过大(NTFS/ext4 都退化)
	if strings.Count(key, "/") != 3 {
		t.Fatalf("应是三级路径(objects/xx/xx/hash),实际 %q", key)
	}
}

// 非法哈希不得产出可用键 —— 哈希会拼进路径,不校验等于把路径交给外部输入。
func TestKeyForRejectsBadHash(t *testing.T) {
	fs := newFS(t)
	for _, bad := range []string{
		"", "short", strings.Repeat("z", 64), strings.Repeat("a", 63),
		strings.Repeat("a", 65), "../../etc/passwd",
	} {
		if got := fs.KeyFor(bad); got != "" {
			t.Errorf("非法哈希 %q 应返回空键,实际 %q", bad, got)
		}
	}
}

func TestHexSHA256NormalizesCase(t *testing.T) {
	up := strings.Repeat("AB", 32)
	got, err := storage.HexSHA256(up)
	if err != nil {
		t.Fatalf("大写十六进制应被接受: %v", err)
	}
	if got != strings.Repeat("ab", 32) {
		t.Fatalf("应归一为小写,实际 %q", got)
	}
}

func TestWriteAndStatRoundTrip(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("content-"), 1000)
	h := hashOf(data)

	n, err := fs.Write(ctx, h, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("应写入 %d 字节,实际 %d", len(data), n)
	}
	sz, err := fs.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if sz != int64(len(data)) {
		t.Fatalf("对象大小应为 %d,实际 %d", len(data), sz)
	}
}

// 短写必须致命:静默接受会让 files.size 与实际内容不符
func TestWriteRejectsShortInput(t *testing.T) {
	fs := newFS(t)
	data := []byte("only-ten!!") // 10 字节
	h := hashOf(data)
	_, err := fs.Write(context.Background(), h, bytes.NewReader(data), 9999)
	if !errors.Is(err, storage.ErrShortWrite) {
		t.Fatalf("声明大于实际应 ErrShortWrite,实际 %v", err)
	}
	// 失败后不得留下对象
	if _, serr := fs.Stat(context.Background(), h); !errors.Is(serr, storage.ErrNotFound) {
		t.Fatalf("短写不应留下对象,实际 Stat=%v", serr)
	}
}

// 写入失败不得留下半成品对象(临时文件 + rename 的意义)
func TestWriteLeavesNoPartialObjectOnReadError(t *testing.T) {
	fs := newFS(t)
	data := []byte("will-be-cut-off-midway")
	h := hashOf(data)
	// 只给一半就报错
	_, err := fs.Write(context.Background(), h, &failingReader{data: data[:8]}, int64(len(data)))
	if err == nil {
		t.Fatal("读取出错时 Write 应失败")
	}
	if _, serr := fs.Stat(context.Background(), h); !errors.Is(serr, storage.ErrNotFound) {
		t.Fatalf("失败后不应留下对象,实际 Stat=%v", serr)
	}
	// 也不应留下临时文件(objects 目录下除正式对象外不应有东西)
	root := fs.Root
	entries := []string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			entries = append(entries, p)
		}
		return nil
	})
	for _, e := range entries {
		if strings.Contains(filepath.Base(e), ".tmp-") {
			t.Fatalf("不应留下临时文件: %s", e)
		}
	}
}

// 内容寻址下重复写同一 hash 必须幂等(秒传/多空间共享内容时是常见路径)
func TestWriteIsIdempotent(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := []byte("same-content-twice")
	h := hashOf(data)

	if _, err := fs.Write(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("首次 Write 失败: %v", err)
	}
	n, err := fs.Write(ctx, h, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("重复 Write 应成功: %v", err)
	}
	if n != int64(len(data)) {
		t.Fatalf("重复写入应回报声明大小,实际 %d", n)
	}
	if sz, _ := fs.Stat(ctx, h); sz != int64(len(data)) {
		t.Fatalf("对象大小不应变化,实际 %d", sz)
	}
}

// 同 hash 但大小不符:内容寻址下**不可能发生**,必须显式报错而不是覆盖
func TestWriteRejectsSameHashDifferentSize(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := []byte("abcdefghij")
	h := hashOf(data)
	if _, err := fs.Write(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("首次 Write 失败: %v", err)
	}
	_, err := fs.Write(ctx, h, bytes.NewReader(data), 5)
	if err == nil {
		t.Fatal("同哈希不同大小应报错(说明存储被破坏或哈希被误用)")
	}
	if !strings.Contains(err.Error(), "大小不符") {
		t.Fatalf("错误信息应说明大小不符,实际 %v", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := []byte("to-delete")
	h := hashOf(data)
	if _, err := fs.Write(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if err := fs.Delete(ctx, h); err != nil {
		t.Fatalf("首次 Delete 失败: %v", err)
	}
	// 生命周期 worker 会重试删除:把"已经不在了"当失败会让对象永远停在 deleting
	if err := fs.Delete(ctx, h); err != nil {
		t.Fatalf("重复 Delete 必须幂等: %v", err)
	}
	if _, err := fs.Stat(ctx, h); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("删除后 Stat 应 ErrNotFound,实际 %v", err)
	}
}

// ---- 暂存→提交(Sponsorable) ----

func TestStageFromComputesHashInOnePass(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("stage"), 5000)
	want := hashOf(data)

	stagePath, gotHash, size, err := fs.StageFrom(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("StageFrom 失败: %v", err)
	}
	defer func() { _ = os.Remove(stagePath) }()

	if gotHash != want {
		t.Fatalf("暂存同时算出的哈希应为 %s,实际 %s", want, gotHash)
	}
	if size != int64(len(data)) {
		t.Fatalf("暂存大小应为 %d,实际 %d", len(data), size)
	}
	if _, serr := os.Stat(stagePath); serr != nil {
		t.Fatalf("暂存文件应存在: %v", serr)
	}
	// 暂存目录与 TUS 分片同一处(6.1),便于一条清理规则覆盖
	if filepath.Base(filepath.Dir(stagePath)) != "tus-tmp" {
		t.Fatalf("暂存应落在 tus-tmp 目录,实际 %s", stagePath)
	}
}

func TestCommitStagedMovesObject(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := []byte("commit-me")
	h := hashOf(data)

	stagePath, _, size, err := fs.StageFrom(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("StageFrom 失败: %v", err)
	}
	if err := fs.CommitStaged(ctx, h, stagePath, size); err != nil {
		t.Fatalf("CommitStaged 失败: %v", err)
	}
	if sz, err := fs.Stat(ctx, h); err != nil || sz != size {
		t.Fatalf("提交后对象应存在且大小正确: size=%d err=%v", sz, err)
	}
	// 暂存文件应已被 rename 消耗
	if _, serr := os.Stat(stagePath); serr == nil {
		t.Fatal("提交后暂存文件不应仍存在(同卷 rename 会移动它)")
	}
}

// 对象已存在 → 直接丢弃暂存文件(秒传路径不该重复落盘)
func TestCommitStagedReusesExistingObject(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := []byte("already-there")
	h := hashOf(data)
	if _, err := fs.Write(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("预置对象失败: %v", err)
	}
	before, _ := fs.Stat(ctx, h)

	stagePath, _, size, err := fs.StageFrom(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("StageFrom 失败: %v", err)
	}
	if err := fs.CommitStaged(ctx, h, stagePath, size); err != nil {
		t.Fatalf("CommitStaged 应成功(复用已有对象): %v", err)
	}
	if _, serr := os.Stat(stagePath); serr == nil {
		t.Fatal("复用路径应丢弃暂存文件")
	}
	if after, _ := fs.Stat(ctx, h); after != before {
		t.Fatalf("复用不应改变对象: before=%d after=%d", before, after)
	}
}

func TestCommitStagedRejectsSizeMismatch(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := []byte("mismatch")
	h := hashOf(data)
	stagePath, _, _, err := fs.StageFrom(ctx, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("StageFrom 失败: %v", err)
	}
	if err := fs.CommitStaged(ctx, h, stagePath, 9999); !errors.Is(err, storage.ErrShortWrite) {
		t.Fatalf("大小不符应 ErrShortWrite,实际 %v", err)
	}
	if _, serr := fs.Stat(ctx, h); !errors.Is(serr, storage.ErrNotFound) {
		t.Fatal("失败的提交不应留下对象")
	}
}

// 并发写同一 hash:内容相同,必须都成功且只留一份对象(内容寻址的天然幂等)
func TestConcurrentWriteSameHash(t *testing.T) {
	fs := newFS(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("cc"), 4096)
	h := hashOf(data)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = fs.Write(ctx, h, bytes.NewReader(data), int64(len(data)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发写入 #%d 失败: %v", i, err)
		}
	}
	if sz, _ := fs.Stat(ctx, h); sz != int64(len(data)) {
		t.Fatalf("应只留一份正确大小的对象,实际 %d", sz)
	}
}

// NewFS 在根目录不可写时必须**启动即失败**(而不是等第一次上传才发现)
func TestNewFSFailsFastOnUnwritableRoot(t *testing.T) {
	dir := t.TempDir()
	// 用一个"路径中间是文件"的根触发失败
	bogus := filepath.Join(dir, "afile", "sub")
	if err := os.WriteFile(filepath.Join(dir, "afile"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备失败: %v", err)
	}
	if _, err := storage.NewFS(bogus); err == nil {
		t.Fatal("数据目录不可用时 NewFS 必须报错")
	}
	if _, err := storage.NewFS(""); err == nil {
		t.Fatal("空 root 必须报错")
	}
}

func TestParseSize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{"", 0, false}, {"0", 0, false}, {"123", 123, false},
		{"-1", 0, true}, {"abc", 0, true},
	} {
		got, err := storage.ParseSize(c.in)
		if c.bad {
			if err == nil {
				t.Errorf("ParseSize(%q) 应报错", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("ParseSize(%q) = (%d,%v),期望 %d", c.in, got, err, c.want)
		}
	}
}

// failingReader 读到一半报错,用于验证"不留半成品对象"。
type failingReader struct {
	data []byte
	pos  int
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errors.New("模拟读取失败")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

var _ io.Reader = (*failingReader)(nil)
