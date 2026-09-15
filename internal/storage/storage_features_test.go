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

// 存储层**可用能力**的逐项断言(2026-09-13 补:写"存储配置指南"时先做一次能力清点,
// 把原先只被间接覆盖或完全没覆盖的能力补上)。
//
// 为什么单独一个文件:这些不是"某个接口的正常路径",而是**运维与后端契约**上必须成立的性质 ——
// 磁盘水位(容量/剩余)、对象占用统计(跳过临时文件)、0 字节对象、Range(可寻址流)、
// 后端标识落库、并发提交同一对象。少了任何一条,配置指南里写的东西就成了"文档说行,实际没验证"。

// ① 后端标识:这个字符串会写进 `file_objects.storage_backend`,巡检也按它分支
// (`patrol` 只对 fs/memory 做目录统计)。改了就静默改变巡检行为。
func TestBackendIdentifier(t *testing.T) {
	f := newFS(t)
	// 常量值本身也是契约:`file_objects.storage_backend` 里存的就是这个字面量,
	// 改常量等于改变已落库数据的含义(老数据会被判成"未知后端")。
	if storage.BackendFS != "fs" {
		t.Fatalf("storage.BackendFS 字面量应为 \"fs\",实际 %q", storage.BackendFS)
	}
	if got := f.Backend(); got != storage.BackendFS {
		t.Fatalf("本地磁盘后端标识应为 %q,实际 %q", storage.BackendFS, got)
	}
}

// ② 磁盘容量/剩余:**水位保护**(disk_watermark_percent)唯一的输入。
// 拿不到真实数值时水位保护会静默失效 —— 那正是磁盘写满时才暴露的故障。
func TestUsageReportsRealDiskCapacity(t *testing.T) {
	f := newFS(t)
	total, free, err := f.Usage()
	if err != nil {
		t.Fatalf("Usage 应能拿到卷容量: %v", err)
	}
	if total <= 0 {
		t.Fatalf("总容量应 > 0,实际 %d", total)
	}
	if free < 0 || free > total {
		t.Fatalf("剩余容量应在 [0,total] 内,实际 free=%d total=%d", free, total)
	}
}

// ③ 对象占用统计:跳过原子写的临时文件(.tmp-*),否则"正在上传"期间的巡检会误报泄漏
// (差值抖动 → 看起来像泄漏了几个 T)。
func TestObjectUsageSkipsTempFiles(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("a"), 1024)
	h := hashOf(payload)
	if _, err := f.Write(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Write: %v", err)
	}

	files, bytesUsed, err := f.ObjectUsage(ctx)
	if err != nil {
		t.Fatalf("ObjectUsage: %v", err)
	}
	if files != 1 || bytesUsed != int64(len(payload)) {
		t.Fatalf("应统计 1 个对象/%d 字节,实际 %d 个/%d 字节", len(payload), files, bytesUsed)
	}

	// 在同一目录塞一个原子写的临时文件:统计必须**看不见它**
	objDir := filepath.Join(f.Root, "objects", h[0:2], h[2:4])
	tmp := filepath.Join(objDir, ".tmp-123456")
	if err := os.WriteFile(tmp, bytes.Repeat([]byte("b"), 4096), 0o640); err != nil {
		t.Fatalf("造临时文件失败: %v", err)
	}
	files2, bytes2, err := f.ObjectUsage(ctx)
	if err != nil {
		t.Fatalf("ObjectUsage(含临时文件): %v", err)
	}
	if files2 != 1 || bytes2 != int64(len(payload)) {
		t.Fatalf("临时文件必须被跳过:期望 1 个/%d 字节,实际 %d 个/%d 字节", len(payload), files2, bytes2)
	}
}

// ④ **0 字节对象**:整条链路(Range 下载、etag、空文件)都要支持。
// 客户端侧刚修过"0 字节文件传不上去"的缺陷,存储层这一端必须本来就没问题 —— 断言钉住。
func TestZeroByteObjectRoundTrip(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	empty := []byte{}
	h := hashOf(empty)

	written, err := f.Write(ctx, h, bytes.NewReader(empty), 0)
	if err != nil {
		t.Fatalf("写 0 字节对象失败: %v", err)
	}
	if written != 0 {
		t.Fatalf("应写入 0 字节,实际 %d", written)
	}
	size, err := f.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != 0 {
		t.Fatalf("0 字节对象大小应为 0,实际 %d", size)
	}
	rc, err := f.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读 0 字节对象失败: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("内容应为空,实际 %d 字节", len(data))
	}
}

// ⑤ 暂存 → 提交 的 **0 字节**路径:与上传的 0 字节文件同一条路(StageFrom 的实测哈希 + CommitStaged)。
func TestStageAndCommitZeroBytes(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()

	stagePath, gotHash, size, err := f.StageFrom(ctx, bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatalf("StageFrom: %v", err)
	}
	if size != 0 {
		t.Fatalf("暂存大小应为 0,实际 %d", size)
	}
	wantHash := hashOf([]byte{})
	if gotHash != wantHash {
		t.Fatalf("空内容的 sha256 应为 %s,实际 %s", wantHash, gotHash)
	}
	if !strings.HasPrefix(filepath.Base(stagePath), "stage-") {
		t.Fatalf("暂存文件应落在 tus-tmp 下且以 stage- 开头,实际 %s", stagePath)
	}
	if err := f.CommitStaged(ctx, gotHash, stagePath, 0); err != nil {
		t.Fatalf("CommitStaged(0 字节): %v", err)
	}
	if _, err := f.Stat(ctx, gotHash); err != nil {
		t.Fatalf("提交后应能 Stat 到对象: %v", err)
	}
}

// ⑥ 对象不存在:Stat 与 Open 都必须回 ErrNotFound(而不是包装成"IO 故障")。
// 生命周期 worker 按 ErrNotFound 判定"已删"是正常终态;混淆会把它当成故障反复重试。
func TestOpenMissingObjectIsNotFound(t *testing.T) {
	f := newFS(t)
	missing := hashOf([]byte("never-written"))
	if _, err := f.Stat(context.Background(), missing); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat 应 ErrNotFound,实际 %v", err)
	}
	if _, err := f.Open(context.Background(), missing); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open 应 ErrNotFound,实际 %v", err)
	}
}

// ⑦ Open 必须给出**可寻址**流(Range 下载的基础):Seek 到中段读到的必须是那一段。
// 契约里写的是 `io.ReadSeekCloser`,handler 直接把它交给 `http.ServeContent`;
// 若哪天换成不可寻址的流,这条断言先失败。
func TestOpenIsSeekable(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	payload := []byte("0123456789abcdefghij")
	h := hashOf(payload)
	if _, err := f.Write(ctx, h, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	rc, err := f.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	if _, err := rc.Seek(10, io.SeekStart); err != nil {
		t.Fatalf("Open 的流必须可 Seek(Range 下载依赖它): %v", err)
	}
	rest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("Seek 后读取失败: %v", err)
	}
	if string(rest) != "abcdefghij" {
		t.Fatalf("从偏移 10 起应为 \"abcdefghij\",实际 %q", string(rest))
	}
}

// ⑧ 并发提交**同一对象**(不同暂存文件):Windows 的 rename 在目标已存在时会失败,
// 而内容寻址下"另一路刚提交完同一对象"是**正常路径**(秒传/多空间共享同一内容)。
// 这条断言要求:全部成功,且对象内容正确、暂存文件都被清掉。
func TestConcurrentCommitStagedSameHash(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("z"), 2048)
	h := hashOf(payload)

	const n = 4
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		p, gotHash, size, err := f.StageFrom(ctx, bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("StageFrom #%d: %v", i, err)
		}
		if gotHash != h || size != int64(len(payload)) {
			t.Fatalf("暂存结果不符:hash=%s size=%d", gotHash, size)
		}
		paths[i] = p
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = f.CommitStaged(ctx, h, paths[i], int64(len(payload)))
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("并发提交 #%d 失败(内容寻址下应幂等成功): %v", i, err)
		}
	}
	rc, err := f.Open(ctx, h)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取对象失败: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("对象内容被并发提交破坏:%d 字节 vs 期望 %d", len(got), len(payload))
	}
	// 暂存文件不该残留(提交成功后它们已无意义;留着会让 24h 清理任务白扫)
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("提交后暂存文件应已消失:%s", p)
		}
	}
}

// ⑨ 声明大小为负:必须在**碰盘之前**拒绝,而不是"写了一半才发现对不上"。
//
// 判据不写成"err != nil":删掉那道显式校验后,短写检查同样会报错,
// 于是"报错"这件事无法证明校验存在。这里要求错误**不是** ErrShortWrite ——
// 即拒绝发生在创建临时文件之前(否则会先落一个无意义的空临时文件再报错)。
func TestWriteRejectsNegativeSize(t *testing.T) {
	f := newFS(t)
	_, err := f.Write(context.Background(), hashOf([]byte("x")), bytes.NewReader([]byte("x")), -1)
	if err == nil {
		t.Fatal("负数大小必须被拒绝")
	}
	if errors.Is(err, storage.ErrShortWrite) {
		t.Fatalf("负数大小应被提前拒绝,而不是走短写路径: %v", err)
	}
}

// ⑫ 同一哈希的第二个暂存文件:**必须被清理**,且对象保持原样。
//
// 这条是"秒传/多空间共享同一内容"的最常见路径(对象已在,只丢弃暂存)。
// 若这条分支忘了删暂存文件,TUS 暂存区就会随每次秒传持续堆积 ——
// 24h 清理任务能兜住,但那意味着"已经定稿成功的文件"还要再占一天磁盘。
// 刻意用**顺序**两次提交(而不是并发):让"第二次走复用分支"成为确定性事件。
func TestCommitStagedCleansSecondStageFile(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	payload := bytes.Repeat([]byte("q"), 4096)
	h := hashOf(payload)

	first, _, _, err := f.StageFrom(ctx, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("StageFrom #1: %v", err)
	}
	if err := f.CommitStaged(ctx, h, first, int64(len(payload))); err != nil {
		t.Fatalf("CommitStaged #1: %v", err)
	}
	second, _, _, err := f.StageFrom(ctx, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("StageFrom #2: %v", err)
	}
	if err := f.CommitStaged(ctx, h, second, int64(len(payload))); err != nil {
		t.Fatalf("CommitStaged #2(对象已存在,应幂等成功): %v", err)
	}
	if _, err := os.Stat(second); err == nil {
		t.Fatalf("复用已有对象后,第二个暂存文件必须被删除:%s", second)
	}
	size, err := f.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("对象大小应保持 %d,实际 %d", len(payload), size)
	}
	files, bytesUsed, err := f.ObjectUsage(ctx)
	if err != nil {
		t.Fatalf("ObjectUsage: %v", err)
	}
	if files != 1 || bytesUsed != int64(len(payload)) {
		t.Fatalf("对象区应只有 1 个对象/%d 字节,实际 %d 个/%d 字节", len(payload), files, bytesUsed)
	}
	// 暂存区必须干净:复用分支若漏删,这里会看到 1 个残留
	stages, err := f.ListStages()
	if err != nil {
		t.Fatalf("ListStages: %v", err)
	}
	if len(stages) != 0 {
		t.Fatalf("暂存区不应有残留,实际 %v", stages)
	}
}

// ⑩ 空 root:启动即失败(而不是"跑起来才发现数据写到当前目录")。
func TestNewFSRejectsEmptyRoot(t *testing.T) {
	if _, err := storage.NewFS("   "); err == nil {
		t.Fatal("空 root 必须被拒绝")
	}
}

// ⑪ 内容寻址的目录切分必须**真的落到盘上**(不只是 KeyFor 字符串对):
// 写成两个不同前缀的对象后,objects/ 下应出现两个不同的两级目录。
func TestObjectLayoutOnDisk(t *testing.T) {
	f := newFS(t)
	ctx := context.Background()
	// 找两个首位十六进制不同的哈希,确保落在不同桶
	var hashes []string
	seen := map[string]bool{}
	for i := 0; i < 1000 && len(hashes) < 2; i++ {
		data := []byte{byte(i)}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		if !seen[h[0:2]] {
			seen[h[0:2]] = true
			hashes = append(hashes, h)
		}
	}
	if len(hashes) < 2 {
		t.Fatal("构造两个不同前缀的哈希失败")
	}
	for _, h := range hashes {
		if _, err := f.Write(ctx, h, bytes.NewReader([]byte("layout")), int64(len("layout"))); err != nil {
			t.Fatalf("Write %s: %v", h, err)
		}
		obj := filepath.Join(f.Root, "objects", h[0:2], h[2:4], h)
		if _, err := os.Stat(obj); err != nil {
			t.Fatalf("对象应落在 %s: %v", obj, err)
		}
	}
}
