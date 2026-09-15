package uploadsvc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 秒传**预检**的集成测试(BE-S4-04 验收①②)。
//
// 预检这件事的价值全在"什么时候**不**给通道"上:
//   - 小文件不给(收益趋零,不值得开内容侧信口)
//   - 服务端没有该对象不给(客户端声明的 hash 不算数)
//   - 对象 size 不一致不给(去重键是 (hash,size) 双校验)
//   - Redis 不可用**只影响秒传提示**,绝不失败整个建任务
//
// 这四条各有一个用例;少了任何一条,"声称一个 hash 白拿内容"或"Redis 抖动导致
// 上传全挂"就会直接上线。

// withFast 给 fixture 装上持物证明服务(miniredis 作为挑战存储)。
func (f *fixture) withFast(t *testing.T) *fastupload.Service {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := &fastupload.Service{KV: cache.New(rdb, "netdisk:")}
	f.svc.Fast = svc
	return svc
}

// mkObject 直接造一条可用的对象行(本组用例只关心预检,不需要真实字节)。
//
// ref_count 必须按状态给:表上有不变式 `(ref_count = 0) = (state <> 'live')`,
// 给 deleted/pending_delete 行填 1 会被 CHECK 直接拒绝
// (这也顺带说明不变式确实在生效)。
func (f *fixture) mkObject(t *testing.T, hash string, size int64, state string) {
	t.Helper()
	ref := 0
	if state == "live" {
		ref = 1
	}
	if _, err := f.database.Pool.Exec(context.Background(), `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state, delete_after)
VALUES ($1, $2, 'fs', $3, $5, $4,
        CASE WHEN $4 = 'live' THEN NULL ELSE now() + interval '24 hours' END)
ON CONFLICT (hash_sha256) DO UPDATE
   SET size = EXCLUDED.size, state = EXCLUDED.state, ref_count = EXCLUDED.ref_count`,
		hash, size, "objects/"+hash[:2]+"/"+hash, state, ref); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}
}

// fakeHash 生成"看起来合法"的 sha256(内容不需要真的存在)。
func fakeHash(seed string, n int) (string, int64) {
	b := make([]byte, 64)
	for i := range b {
		b[i] = seed[i%len(seed)]
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), int64(n)
}

// **验收②**:预检命中 → 下发随机偏移样本 + nonce(绑 upload_id + hash)
func TestCreateIssuesChallengeWhenObjectExists(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	hash, size := fakeHash("fast", 1<<20)
	f.mkObject(t, hash, size, "live")

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "fast.bin", DeclaredSize: size, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload == nil {
		t.Fatal("预检命中必须下发持物证明挑战(否则秒传形同不存在)")
	}
	if res.FastUpload.Nonce == "" {
		t.Error("挑战必须带 nonce")
	}
	if len(res.FastUpload.Offsets) != fastupload.SampleCount {
		t.Errorf("应有 %d 个样本偏移,实际 %d", fastupload.SampleCount, len(res.FastUpload.Offsets))
	}
	if res.FastUpload.SampleLen != fastupload.SampleLen {
		t.Errorf("样本长度应为 %d,实际 %d", fastupload.SampleLen, res.FastUpload.SampleLen)
	}
	for _, off := range res.FastUpload.Offsets {
		if off < 0 || off+fastupload.SampleLen > size {
			t.Fatalf("样本偏移越界: %d(文件 %d 字节)", off, size)
		}
	}
	if res.FastUpload.ExpiresInSeconds <= 0 {
		t.Error("必须告知客户端有效期,避免它撞到过期才知道")
	}
}

// **验收①**:小文件(<32KB)一律不给秒传通道
func TestCreateNoChallengeForSmallFile(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	hash, _ := fakeHash("small", 1024)
	f.mkObject(t, hash, 1024, "live")

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "small.bin", DeclaredSize: 1024, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload != nil {
		t.Fatal("小于下限的文件不应拿到秒传通道")
	}
}

// 服务端**没有**该对象 → 不给通道(客户端声明的 hash 只用于查表)
func TestCreateNoChallengeWhenObjectMissing(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	hash, size := fakeHash("absent", 1<<20)

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "absent.bin", DeclaredSize: size, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload != nil {
		t.Fatal("对象不存在时必须全量上传")
	}
}

// **去重键双校验**:hash 同、size 不同 → 不给通道(理论碰撞或恶意声明)
func TestCreateNoChallengeOnSizeMismatch(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	hash, size := fakeHash("sizemismatch", 1<<20)
	// 库里存的是另一个大小
	f.mkObject(t, hash, size+1, "live")

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "mismatch.bin", DeclaredSize: size, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload != nil {
		t.Fatal("(hash,size) 双校验不通过时必须全量上传")
	}
}

// 对象已 deleted → 不给通道(内容已物理删除)
func TestCreateNoChallengeWhenObjectDeleted(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	hash, size := fakeHash("deleted", 1<<20)
	f.mkObject(t, hash, size, "deleted")

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "deleted.bin", DeclaredSize: size, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload != nil {
		t.Fatal("对象已物理删除时不能给秒传通道(会落出下载 404 的行)")
	}
}

// pending_delete(清理队列里但物理还在)→ **可以**给通道;finalize 会复活引用
func TestCreateChallengeForPendingDeleteObject(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	hash, size := fakeHash("pending", 1<<20)
	f.mkObject(t, hash, size, "pending_delete")

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "pending.bin", DeclaredSize: size, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload == nil {
		t.Fatal("pending_delete 的对象物理仍在,应当可秒传(finalize 负责复活)")
	}
}

// **降级**:挑战存储(Redis)不可用时,建任务必须**成功**,只是没有秒传提示。
//
// 这条防的是"Redis 抖动导致全站上传失败" —— 10.7 明确 Redis 不做权威账,
// 而秒传只是体验优化,全量上传才是保底路径。
func TestCreateSucceedsWhenChallengeStoreUnavailable(t *testing.T) {
	f := setup(t)
	// Fast 装配了,但 KV 为 nil → Issue 必然失败
	f.svc.Fast = &fastupload.Service{KV: nil}
	hash, size := fakeHash("redis-down", 1<<20)
	f.mkObject(t, hash, size, "live")

	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "degraded.bin", DeclaredSize: size, DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("挑战存储不可用时建任务仍须成功(降级为全量上传): %v", err)
	}
	if res.FastUpload != nil {
		t.Fatal("挑战存储不可用时不应给出秒传提示")
	}
	if res.UploadID == "" || res.Ticket == "" {
		t.Fatal("降级后仍应返回可用的 upload_id 与 ticket")
	}
}

// 没带 hash 的建任务永远不会拿到通道(客户端不做预检)
func TestCreateNoChallengeWithoutHash(t *testing.T) {
	f := setup(t)
	f.withFast(t)
	res, err := f.svc.Create(context.Background(), uploadsvc.CreateInput{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "nohash.bin", DeclaredSize: 1 << 20,
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if res.FastUpload != nil {
		t.Fatal("未声明 hash 时不应有秒传通道")
	}
}
