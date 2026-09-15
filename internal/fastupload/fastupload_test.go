package fastupload_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/fastupload"
)

// 持物证明挑战的单元测试(BE-S4-04 / 6.10 五细则)。
//
// 全部走真实 miniredis(而不是内存假实现):挑战的**一次性**与 **TTL**
// 正是靠 Redis 的 DEL/过期语义成立的,用假 KV 测出来的"通过"证明不了什么。

type fakeReader struct {
	content map[string][]byte
	missing map[string]bool
	opens   int
}

func (f *fakeReader) Open(_ context.Context, hash string) (io.ReadSeekCloser, error) {
	f.opens++
	if f.missing[hash] {
		return nil, os.ErrNotExist
	}
	b, ok := f.content[hash]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &memFile{r: bytes.NewReader(b)}, nil
}

type memFile struct{ r *bytes.Reader }

func (m *memFile) Read(p []byte) (int, error)                { return m.r.Read(p) }
func (m *memFile) Seek(off int64, whence int) (int64, error) { return m.r.Seek(off, whence) }
func (m *memFile) Close() error                              { return nil }

func setup(t *testing.T) (*fastupload.Service, *fakeReader, func(time.Duration)) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	rd := &fakeReader{content: map[string][]byte{}, missing: map[string]bool{}}
	// 注入确定性偏移:8 个样本均匀铺开,便于断言"服务端读的正是那几段"
	i := 0
	svc := &fastupload.Service{
		KV:     cache.New(rdb, "netdisk:"),
		Reader: rd,
		RandInt: func(n int64) (int64, error) {
			step := n / fastupload.SampleCount
			v := int64(i) * step
			i++
			if v >= n {
				v = n - 1
			}
			return v, nil
		},
	}
	return svc, rd, mr.FastForward
}

// 内容与它的 hash;同时登记到 fakeReader 的"已存对象"里。
//
// 内容必须**由 seed 的字节**派生(而不是 seed 的长度):否则 "wrong" 与 "other"
// 会生成一模一样的字节,本意是"内容不符"的用例反而变成"内容一致"。
func content(t *testing.T, rd *fakeReader, seed string, size int) ([]byte, string) {
	t.Helper()
	data := make([]byte, size)
	var acc int
	for _, c := range []byte(seed) {
		acc = acc*31 + int(c)
	}
	for i := range data {
		data[i] = byte((i*31 + acc) % 251)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	rd.content[hash] = data
	return data, hash
}

func TestIssueAndVerifyPassesWithRealContent(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "ok", 1<<20)

	view, err := svc.Issue(ctx, "up-1", hash, int64(len(data)))
	if err != nil {
		t.Fatalf("下发挑战失败: %v", err)
	}
	if view.Nonce == "" {
		t.Fatal("必须下发 nonce(一次性凭据)")
	}
	if len(view.Offsets) != fastupload.SampleCount {
		t.Fatalf("应有 %d 个样本,实际 %d", fastupload.SampleCount, len(view.Offsets))
	}
	if view.SampleLen != fastupload.SampleLen {
		t.Fatalf("样本长度应为 %d,实际 %d", fastupload.SampleLen, view.SampleLen)
	}
	// 每个样本必须整段落在文件内
	for _, off := range view.Offsets {
		if off < 0 || off+view.SampleLen > int64(len(data)) {
			t.Fatalf("样本偏移越界: %d(文件 %d 字节)", off, len(data))
		}
	}

	ch, err := svc.Take(ctx, "up-1", hash)
	if err != nil {
		t.Fatalf("取挑战失败: %v", err)
	}
	digests, err := fastupload.SampleDigests(bytes.NewReader(data), ch.Offsets, ch.SampleLen)
	if err != nil {
		t.Fatalf("算样本摘要失败: %v", err)
	}
	if err := svc.Verify(ctx, ch, digests); err != nil {
		t.Fatalf("真实内容应通过证明: %v", err)
	}
}

// **细测②** 一次性:同一个 nonce 不能再用第二次("证明一次,白拿无数次"必须不成立)
func TestNonceIsSingleUse(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "once", 1<<20)
	if _, err := svc.Issue(ctx, "up-2", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	if _, err := svc.Take(ctx, "up-2", hash); err != nil {
		t.Fatalf("第一次取应成功: %v", err)
	}
	if _, err := svc.Take(ctx, "up-2", hash); !errors.Is(err, fastupload.ErrNoChallenge) {
		t.Fatalf("第二次取必须失败(一次性),实际 %v", err)
	}
}

// **细则④** TTL:过期后不能再用(否则挑战等于永久通行证)
func TestChallengeExpires(t *testing.T) {
	svc, rd, advance := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "ttl", 1<<20)
	if _, err := svc.Issue(ctx, "up-3", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	advance(6 * time.Minute)
	if _, err := svc.Take(ctx, "up-3", hash); err == nil {
		t.Fatal("过期后取挑战必须失败")
	}
}

// **细则①** 小文件不开通道(阈值以下一律全量上传)
func TestSmallFileHasNoFastChannel(t *testing.T) {
	svc, rd, _ := setup(t)
	if _, err := svc.Issue(context.Background(), "up-4", "deadbeef", fastupload.MinSize-1); err == nil {
		t.Fatal("小于下限的文件不应拿到挑战")
	}
	if fastupload.MinSize != 8*fastupload.SampleLen {
		t.Fatalf("下限必须是 8×样本长度,实际 %d", fastupload.MinSize)
	}
	// 配置成 0/负数不得关掉门槛(回落默认)
	if fastupload.MinSizeFor(0) != fastupload.MinSize || fastupload.MinSizeFor(-1) != fastupload.MinSize {
		t.Fatal("MinSizeFor 必须把 0/负数回落为默认下限,不允许关掉大小门槛")
	}
	_ = rd
}

// 样本摘要不匹配 → 证明失败(拿不出内容就过不了)
func TestWrongDigestFailsProof(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "wrong", 1<<20)
	if _, err := svc.Issue(ctx, "up-5", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	ch, err := svc.Take(ctx, "up-5", hash)
	if err != nil {
		t.Fatalf("取挑战失败: %v", err)
	}
	// 用**别人的内容**(同样长度)算摘要
	other, _ := content(t, rd, "other", 1<<20)
	digests, _ := fastupload.SampleDigests(bytes.NewReader(other), ch.Offsets, ch.SampleLen)
	if err := svc.Verify(ctx, ch, digests); !errors.Is(err, fastupload.ErrProofFailed) {
		t.Fatalf("内容不符必须证明失败,实际 %v", err)
	}
}

// 摘要个数不符 → 明确失败(而不是"少给几个就算过")
func TestDigestCountMustMatch(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "cnt", 1<<20)
	if _, err := svc.Issue(ctx, "up-6", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	ch, _ := svc.Take(ctx, "up-6", hash)
	if err := svc.Verify(ctx, ch, []string{"00"}); !errors.Is(err, fastupload.ErrProofFailed) {
		t.Fatalf("摘要个数不符应失败,实际 %v", err)
	}
}

// 物理对象不存在(行说在、实际不在)→ 不能放行,让客户端老实上传
func TestMissingPhysicalObjectFailsProof(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "gone", 1<<20)
	if _, err := svc.Issue(ctx, "up-7", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	ch, _ := svc.Take(ctx, "up-7", hash)
	rd.missing[hash] = true
	digests, _ := fastupload.SampleDigests(bytes.NewReader(data), ch.Offsets, ch.SampleLen)
	if err := svc.Verify(ctx, ch, digests); !errors.Is(err, fastupload.ErrProofFailed) {
		t.Fatalf("物理对象缺失必须证明失败,实际 %v", err)
	}
}

// 挑战绑定 upload_id + hash:换个 hash 或换个任务都用不了(细则②)
func TestChallengeIsBoundToUploadAndHash(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "bind", 1<<20)
	if _, err := svc.Issue(ctx, "up-8", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	// 换成另一个 upload_id
	if _, err := svc.Take(ctx, "up-OTHER", hash); !errors.Is(err, fastupload.ErrNoChallenge) {
		t.Fatalf("换 upload_id 应无挑战,实际 %v", err)
	}
	// 原任务、换 hash
	if _, err := svc.Take(ctx, "up-8", "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, fastupload.ErrChallengeMismatch) {
		t.Fatalf("换 hash 应不匹配,实际 %v", err)
	}
	// 不匹配的取用**不能**把挑战作废(否则等于给了一个拒绝服务的把手)
	if _, err := svc.Take(ctx, "up-8", hash); err != nil {
		t.Fatalf("不匹配的尝试不应作废挑战,实际 %v", err)
	}
}

// Redis 不可用 → 明确 ErrUnavailable,由调用方降级(而不是让上传失败)
func TestUnavailableKV(t *testing.T) {
	svc := &fastupload.Service{KV: nil, Reader: &fakeReader{}}
	if _, err := svc.Issue(context.Background(), "up-9", "x", 1<<20); !errors.Is(err, fastupload.ErrUnavailable) {
		t.Fatalf("KV 未装配应 ErrUnavailable,实际 %v", err)
	}
	if _, err := svc.Take(context.Background(), "up-9", "x"); !errors.Is(err, fastupload.ErrUnavailable) {
		t.Fatalf("KV 未装配应 ErrUnavailable,实际 %v", err)
	}
}

// **细则⑤**:错误信息里不得出现样本值/摘要(只报"第几个不匹配")
func TestFailureMessageLeaksNoSampleValues(t *testing.T) {
	svc, rd, _ := setup(t)
	ctx := context.Background()
	data, hash := content(t, rd, "leak", 1<<20)
	if _, err := svc.Issue(ctx, "up-10", hash, int64(len(data))); err != nil {
		t.Fatalf("下发失败: %v", err)
	}
	ch, _ := svc.Take(ctx, "up-10", hash)
	other, _ := content(t, rd, "leak2", 1<<20)
	digests, _ := fastupload.SampleDigests(bytes.NewReader(other), ch.Offsets, ch.SampleLen)
	err := svc.Verify(ctx, ch, digests)
	if err == nil {
		t.Fatal("应失败")
	}
	msg := err.Error()
	for _, d := range digests {
		if bytes.Contains([]byte(msg), []byte(d)) {
			t.Fatalf("错误信息泄露了样本摘要: %s", msg)
		}
	}
	// 也不得回显样本内容(前 8 字节的十六进制形式)
	for _, off := range ch.Offsets {
		seg := other[off : off+8]
		if bytes.Contains([]byte(msg), []byte(hex.EncodeToString(seg))) {
			t.Fatalf("错误信息泄露了样本内容: %s", msg)
		}
	}
}
