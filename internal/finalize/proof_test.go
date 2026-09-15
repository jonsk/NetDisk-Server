package finalize_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// **持物证明与无内容流定稿的集成测试(BE-S4-04 / 6.10 五细则)**。
//
// 这是 R-05 防投毒纪律真正生效的地方:单元测试能证明"摘要不匹配会失败",
// 但只有走完整条 finalize 才能验证**证明通过后内容确实没有被重写**、
// **证明没过时一行都不落**、**同一个 nonce 不能用第二次**。

// withProof 给 fixture 装上真实的持物证明服务(KV 走 miniredis,Reader 就是本用例的存储)。
func withProof(t *testing.T, f *fixture) *fastupload.Service {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := &fastupload.Service{KV: cache.New(rdb, "netdisk:"), Reader: f.storage}
	f.svc.Fast = svc
	return svc
}

// countObjects 数本用例存储根下的物理对象文件个数。
func (f *fixture) countObjects(t *testing.T) int {
	t.Helper()
	n := 0
	// 存储根由 storage.NewFS(t.TempDir()) 建立,t.TempDir() 的父目录即本用例的临时目录;
	// 直接遍历整棵临时目录树即可(里面只有本用例的对象与暂存目录)。
	root := filepath.Dir(f.storage.KeyFor("x"))
	// KeyFor 给出 objects/xx/xx/hash,上溯三级到根
	for i := 0; i < 3; i++ {
		root = filepath.Dir(root)
	}
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历存储根失败: %v", err)
	}
	return n
}

// objectRefState 读对象行的 (ref_count, state)。
func (f *fixture) objectRefState(t *testing.T, hash string) (int, string) {
	t.Helper()
	var ref int
	var state string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT ref_count, state FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&ref, &state); err != nil {
		t.Fatalf("读对象行失败: %v", err)
	}
	return ref, state
}

func (f *fixture) fileCount(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE space_id = $1 AND name = $2`, f.spaceID, name).Scan(&n); err != nil {
		t.Fatalf("数文件行失败: %v", err)
	}
	return n
}

// mkUpload 造一条**真实的** uploads 行并返回其 id。
//
// 为什么必须是真的:finalize 的配额结算以 `uploads.declared_size` 为预留账本
// (BE-S5-01 修正过"用调用方声明值"的 bug),定稿幂等也靠 `uploads.state`。
// 塞一个假 id 会在结算那一步炸成 500(SQLSTATE 22P02),而不是测出秒传逻辑。
func (f *fixture) mkUpload(t *testing.T, name, hash string, size int64) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := f.database.InTx(ctx, func(tx pgx.Tx) error {
		up, err := repo.UploadRepo{}.Create(ctx, tx, repo.CreateUploadInput{
			UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
			Name: name, DeclaredSize: size, DeclaredHash: hash,
			// ticket 只存哈希;本用例不走 ticket 校验(那是 uploadsvc 的职责)
			TicketHash: strings.Repeat("0", 64),
			ExpiresAt:  time.Now().Add(time.Hour),
		})
		if err != nil {
			return err
		}
		id = up.ID
		return nil
	}); err != nil {
		t.Fatalf("造 uploads 行失败: %v", err)
	}
	return id
}

// digestsFor 按**下发给客户端的视图**算样本摘要(客户端该做的事)。
//
// 刻意用 view.Offsets 而不是先 Take 出内部 Challenge:Take 是**一次性**的,
// 测试若先取一次,finalize 里那次 Take 就取不到了 —— 于是"证明不匹配"的用例
// 会变成"没有挑战"而**通过**,看起来绿了却什么都没验证(本文件踩过一次这个坑)。
func digestsFor(t *testing.T, data []byte, view *fastupload.View) []string {
	t.Helper()
	out, err := fastupload.SampleDigests(bytes.NewReader(data), view.Offsets, view.SampleLen)
	if err != nil {
		t.Fatalf("算摘要失败: %v", err)
	}
	return out
}

// **验收①②④**:证明通过 → 免重传定稿成功,且**内容没有被重写**
func TestFastUploadFinalizesWithoutContent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	proof := withProof(t, f)

	// 先正常上传一份内容(造出"服务端已有该内容"的状态)
	data, hash := content("proof-ok", 1<<20)
	if _, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "victim.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("前置上传失败: %v", err)
	}
	before := f.countObjects(t)
	refBefore, _ := f.objectRefState(t, hash)

	// 预检下发挑战(绑真实的 upload_id —— 挑战与定稿共用同一个任务)
	uploadID := f.mkUpload(t, "second.bin", hash, int64(len(data)))
	view, err := proof.Issue(ctx, uploadID, hash, int64(len(data)))
	if err != nil {
		t.Fatalf("下发挑战失败: %v", err)
	}
	digests := digestsFor(t, data, view)

	// 无内容流定稿
	res, err := f.svc.Finalize(ctx, finalize.Input{
		UploadID: uploadID,
		UserID:   f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "second.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Nonce: view.Nonce, SampleSHA256: digests,
	})
	if err != nil {
		t.Fatalf("秒传定稿应成功: %v", err)
	}
	if !res.Deduped {
		t.Fatal("秒传必须命中已有对象(Deduped=true)")
	}
	if res.File.HashSHA256 != hash {
		t.Fatalf("新行应指向同一 hash,实际 %s", res.File.HashSHA256)
	}
	// **内容没有被重写**:物理文件数不变、引用 +1
	if after := f.countObjects(t); after != before {
		t.Fatalf("秒传不应写任何物理对象(前 %d 后 %d)", before, after)
	}
	if ref, state := f.objectRefState(t, hash); ref != refBefore+1 || state != model.ObjectLive {
		t.Fatalf("引用应 +1 为 %d/live,实际 %d/%s", refBefore+1, ref, state)
	}
}

// **验收③**:证明没过 → 拒绝 + 不落任何行(且 nonce 已作废)
func TestFastUploadWrongSamplesDenied(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	proof := withProof(t, f)

	data, hash := content("proof-bad", 1<<20)
	if _, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "victim2.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("前置上传失败: %v", err)
	}
	refBefore, _ := f.objectRefState(t, hash)

	uploadID := f.mkUpload(t, "stolen.bin", hash, int64(len(data)))
	view, err := proof.Issue(ctx, uploadID, hash, int64(len(data)))
	if err != nil {
		t.Fatalf("下发挑战失败: %v", err)
	}
	// 用**别的内容**算摘要(同样长度):证明必须不过
	other, _ := content("proof-other", 1<<20)
	digests := digestsFor(t, other, view)

	_, err = f.svc.Finalize(ctx, finalize.Input{
		UploadID: uploadID,
		UserID:   f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "stolen.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Nonce: view.Nonce, SampleSHA256: digests,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("证明不匹配应 fast_upload_denied,实际 %v", err)
	}
	if ae.Status != 403 {
		t.Fatalf("证明不过应是 403(拒绝),实际 %d", ae.Status)
	}
	if n := f.fileCount(t, "stolen.bin"); n != 0 {
		t.Fatalf("证明不过不应留下文件行,实际 %d", n)
	}
	if ref, _ := f.objectRefState(t, hash); ref != refBefore {
		t.Fatalf("证明不过不应改引用,实际 %d", ref)
	}
}

// **一次性**:同一个 nonce 只能用一次(否则"证明一次、白拿无数次")
func TestFastUploadNonceCannotBeReused(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	proof := withProof(t, f)

	data, hash := content("proof-once", 1<<20)
	if _, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "victim3.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("前置上传失败: %v", err)
	}
	uploadID := f.mkUpload(t, "once-1.bin", hash, int64(len(data)))
	view, err := proof.Issue(ctx, uploadID, hash, int64(len(data)))
	if err != nil {
		t.Fatalf("下发挑战失败: %v", err)
	}
	digests := digestsFor(t, data, view)

	in := finalize.Input{
		UploadID: uploadID,
		UserID:   f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "once-1.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Nonce: view.Nonce, SampleSHA256: digests,
	}
	if _, err := f.svc.Finalize(ctx, in); err != nil {
		t.Fatalf("第一次应成功: %v", err)
	}
	in.Name = "once-2.bin"
	_, err = f.svc.Finalize(ctx, in)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("重放同一 nonce 必须被拒,实际 %v", err)
	}
	if n := f.fileCount(t, "once-2.bin"); n != 0 {
		t.Fatalf("重放不应留下文件行,实际 %d", n)
	}
}

// **验收④**:挑战必须绑定上传任务;没有 upload_id 的 nonce 一律拒绝
func TestFastUploadRequiresUploadID(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	proof := withProof(t, f)

	data, hash := content("proof-noupload", 1<<20)
	if _, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "victim4.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("前置上传失败: %v", err)
	}
	uploadID := f.mkUpload(t, "no-upload.bin", hash, int64(len(data)))
	view, err := proof.Issue(ctx, uploadID, hash, int64(len(data)))
	if err != nil {
		t.Fatalf("下发挑战失败: %v", err)
	}
	// 不带 UploadID(等价于"随便编一个 nonce 想蒙过去")
	_, err = f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "no-upload.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Nonce: view.Nonce, SampleSHA256: digestsFor(t, data, view),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("缺 upload_id 应被拒,实际 %v", err)
	}
}

// **验收④**:挑战下发后对象被清理/标记删除 → 定稿必须拒绝(让客户端全量上传)
func TestFastUploadDeniedWhenObjectNoLongerUsable(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	proof := withProof(t, f)

	data, hash := content("proof-gone", 1<<20)
	if _, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "victim5.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("前置上传失败: %v", err)
	}
	uploadID := f.mkUpload(t, "late.bin", hash, int64(len(data)))
	view, err := proof.Issue(ctx, uploadID, hash, int64(len(data)))
	if err != nil {
		t.Fatalf("下发挑战失败: %v", err)
	}
	// 模拟"挑战下发后对象已被物理删除"(清理 worker 走完)
	if _, err := f.pool.Exec(ctx, `
UPDATE file_objects SET ref_count = 0, state = 'deleted', delete_after = NULL
 WHERE hash_sha256 = $1`, hash); err != nil {
		t.Fatalf("改对象状态失败: %v", err)
	}

	_, err = f.svc.Finalize(ctx, finalize.Input{
		UploadID: uploadID,
		UserID:   f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "late.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Nonce: view.Nonce, SampleSHA256: digestsFor(t, data, view),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("对象已不可用应 fast_upload_denied,实际 %v", err)
	}
	if n := f.fileCount(t, "late.bin"); n != 0 {
		t.Fatalf("不应留下文件行,实际 %d", n)
	}
}

// 回归:没有内容流也没有证明时,声明的假 hash **绝不能**建出对象行
// (否则"报一个 hash"就能在对象表里凭空造出记录,后续清理逻辑全乱)
func TestFastUploadDeniedLeavesNoObjectRow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	withProof(t, f)
	fake := sha256.Sum256([]byte("never-uploaded-content"))
	fakeHash := hex.EncodeToString(fake[:])

	_, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "ghost.bin", DeclaredSize: 1 << 20, DeclaredHash: fakeHash,
		Content: nil,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("应 fast_upload_denied,实际 %v", err)
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM file_objects WHERE hash_sha256 = $1`, fakeHash).Scan(&n); err != nil {
		t.Fatalf("查对象行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("凭空声明的 hash 不应产生对象行,实际 %d", n)
	}
}
