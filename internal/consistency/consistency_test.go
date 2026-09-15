// Package consistency 放**跨服务**的一致性用例:它们是"两个服务在并发/崩溃下
// 共同维持的一个性质",放在任何一个服务自己的包里都会变成"只测了一半"。
//
// BE-S5-08(三审 P1-1 真竞态)的三条验收:
//
//	① worker claim 后、unlink 前并发秒传 → 对象**不得**被删,文件仍可下载
//	② unlink 后并发复活 → 复活落新建分支(重新落盘),下载内容正确
//	③ 进程 kill -9 后重启 → 无悬空 files 行(抽样 Stat 全命中)
//
// 为什么要专门测"秒传/复活"与"清理 worker"的交叉:这两条链路各自都有完整用例,
// 但它们的**交叉点**才是三审指出的真竞态 —— 复活方只看元数据行,worker 只看
// 它自己领取到的那一行,而"物理对象被删掉"这件事只发生在 worker 侧。
// 交叉点错了的后果是**文件列表正常但下载 404**(或下到半截内容),而且不报错。
package consistency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/lifecycle"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

type fixture struct {
	database *db.DB
	store    *storage.FS
	fin      *finalize.Service
	userID   string
	spaceID  string
	rootID   string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	suffix := time.Now().Format("150405.000000000")
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, cerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: "cons_" + suffix, Email: "cons_" + suffix + "@example.com",
			DisplayName: "一致性用例用户", Role: model.RoleUser,
		})
		if cerr != nil {
			return cerr
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// 清理顺序:先记下本用例空间引用过的对象(删文件前),再按 id 逐表删。
		rows, qerr := database.Pool.Query(bg, `
SELECT DISTINCT hash_sha256 FROM files
 WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL`, u.ID)
		var own []string
		if qerr == nil {
			for rows.Next() {
				var h string
				if err := rows.Scan(&h); err == nil {
					own = append(own, h)
				}
			}
			rows.Close()
		}
		if len(own) > 0 {
			_, _ = database.Pool.Exec(bg, `DELETE FROM file_objects WHERE hash_sha256 = ANY($1::varchar[])`, own)
		}
		_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	root, err := repo.FileRepo{}.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}
	return &fixture{
		database: database, store: store,
		fin: &finalize.Service{
			Pool: database.Pool, Storage: store,
			Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Name: namepolicy.Default(),
		},
		userID: u.ID, spaceID: sp.ID, rootID: root.ID,
	}
}

// worker 造一个 lifecycle worker。**每次调用都是新实例** ——
// 用例里的"重启"就用它表达(进程内换实例 = 连接断、内存态清空、会话锁自动释放)。
func (f *fixture) worker() *lifecycle.Service {
	return &lifecycle.Service{
		Pool: f.database.Pool, Storage: f.store,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		DeleteAfter: time.Hour, ClaimTTL: 10 * time.Minute,
	}
}

// upload 用定稿链路造一个真实文件(元数据 + 物理对象)。
func (f *fixture) upload(t *testing.T, name string, data []byte) (fileID, hash string) {
	t.Helper()
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:])
	res, err := f.fin.Finalize(context.Background(), finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: name, DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("定稿 %s 失败: %v", name, err)
	}
	return res.File.ID, hash
}

// garbage 把对象做成"刚被删掉、正等清理"的样子(引用归零 + 延迟删除窗口已过)。
// 直接改库而不是走删除接口:本包要测的是**清理与复活交叉**,
// 而不是"删除接口是否把引用减到位"(那有各自的用例)。
func (f *fixture) garbage(t *testing.T, hash string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.database.Pool.Exec(ctx, `
UPDATE file_objects
   SET ref_count = 0, state = 'pending_delete', delete_after = now() - interval '1 hour', updated_at = now()
 WHERE hash_sha256 = $1`, hash); err != nil {
		t.Fatalf("造垃圾对象失败: %v", err)
	}
	// 引用归零 ⇒ 不再有 files 行指向它(与真实删除一致)
	if _, err := f.database.Pool.Exec(ctx,
		`DELETE FROM files WHERE hash_sha256 = $1 AND space_id = $2`, hash, f.spaceID); err != nil {
		t.Fatalf("删除 files 行失败: %v", err)
	}
}

func (f *fixture) objState(t *testing.T, hash string) (string, int) {
	t.Helper()
	var state string
	var ref int
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT state, ref_count FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&state, &ref); err != nil {
		t.Fatalf("读对象行失败: %v", err)
	}
	return state, ref
}

// readBack 走**下载同一条路**(storage.Open)读回内容并校验哈希。
//
// 只断言"Stat 成功"是不够的:Stat 命中的可能是上一轮的残留文件或半截内容。
// 验收①说的是"文件可下载",所以这里读出来算哈希。
func (f *fixture) readBack(t *testing.T, hash string) {
	t.Helper()
	rc, err := f.store.Open(context.Background(), hash)
	if err != nil {
		t.Fatalf("下载打开失败(对象 %s 不存在?): %v", hash[:12], err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != hash {
		t.Fatalf("下载内容与内容寻址不符: 期望 %s,实际 %s", hash[:12], hex.EncodeToString(sum[:])[:12])
	}
}

// ①worker 已 claim(行处于 deleting)、尚未 unlink 时并发"秒传/复活":
// 对象**不得**被删,复活后的文件可下载,随后 worker 也必须跳过它。
func TestClaimedObjectSurvivesConcurrentRevive(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("claimed-object"), 1024)
	_, hash := f.upload(t, "claimed.bin", data)
	f.garbage(t, hash)

	// 模拟"worker 已软领取":state=deleting + 租约(delete_after 在未来)
	if _, err := f.database.Pool.Exec(ctx, `
UPDATE file_objects SET state = 'deleting', delete_after = now() + interval '10 minutes' WHERE hash_sha256 = $1`,
		hash); err != nil {
		t.Fatalf("模拟领取失败: %v", err)
	}

	// 并发复活方:内容仍在,应当**原地复活**而不是重新落盘
	if _, err := f.fin.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "revived.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("领取窗口内复活失败: %v", err)
	}
	if state, ref := f.objState(t, hash); state != model.ObjectLive || ref != 1 {
		t.Fatalf("复活后应为 live/ref=1,实际 %s/%d", state, ref)
	}
	f.readBack(t, hash) // 验收①:文件可下载

	// 现在让"那个 paused 的 worker"继续跑:它必须发现行已复活而跳过删除
	res, err := f.worker().RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("worker 运行失败: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("已复活的对象不得被删除,实际 Deleted=%d", res.Deleted)
	}
	if state, ref := f.objState(t, hash); state != model.ObjectLive || ref != 1 {
		t.Fatalf("worker 跑完后仍应为 live/ref=1,实际 %s/%d", state, ref)
	}
	f.readBack(t, hash)
}

// gatedPool 在第 blockOn 次 Acquire 上停住,把 worker 精确卡在
// "已软领取、尚未 unlink"的窗口里。
//
// 为什么必须这样而不是靠 sleep 撞窗口:`run()` 的顺序是
// ResetStaleClaims(#1 次 Acquire)→ claim(#2)→ deleteOne(拿会话锁,#3),
// 而"领取之后、物理删之前"正是三审 P1-1 的原始竞态窗口。用时间片去撞它
// 是概率性的 —— 实测把竞速用例跑 10 轮,删掉锁下复核也照样全过(反向验证失败),
// 也就是说那样的用例**根本没在盯这件事**。这里用可编程的停顿把它变成确定性的。
type gatedPool struct {
	inner   *pgxpool.Pool
	blockOn int32
	calls   int32
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newGatedPool(inner *pgxpool.Pool, blockOn int32) *gatedPool {
	return &gatedPool{
		inner: inner, blockOn: blockOn,
		entered: make(chan struct{}), release: make(chan struct{}),
	}
}

func (p *gatedPool) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	if atomic.AddInt32(&p.calls, 1) == p.blockOn {
		p.once.Do(func() { close(p.entered) })
		<-p.release
	}
	return p.inner.Acquire(ctx)
}

// ①'worker 已领取、尚未 unlink 的**精确窗口**内并发复活:必须跳过删除。
//
// 这条与上面那条的区别:上面是"复活完再让 worker 跑"(领取条件已不匹配),
// 这里是把 worker 卡在窗口里 —— 此时 worker 手上已经有一个"要删这个对象"的决定,
// 唯一能拦住它的是**锁下复核**。删掉那条复核,本用例必须失败。
func TestReviveInsideClaimUnlinkWindowIsNotDeleted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("window-object"), 512)
	_, hash := f.upload(t, "window.bin", data)
	f.garbage(t, hash)

	// worker 前三步恰好是 ResetStaleClaims(#1)、claim(#2)、deleteOne 的会话锁(#3)
	gate := newGatedPool(f.database.Pool, 3)
	w := f.worker()
	w.Pool = gate

	done := make(chan struct {
		res *lifecycle.Result
		err error
	}, 1)
	go func() {
		res, err := w.RunOnceFor(ctx, []string{hash}, 10)
		done <- struct {
			res *lifecycle.Result
			err error
		}{res, err}
	}()

	select {
	case <-gate.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("worker 未在预期步骤停顿(领取窗口没有出现)")
	}
	// 此刻:worker 已把行标成 deleting 并决定删除,但还没拿对象锁
	if state, ref := f.objState(t, hash); state != model.ObjectDeleting || ref != 0 {
		close(gate.release)
		t.Fatalf("停顿点应处于已领取(deleting/ref=0),实际 %s/%d", state, ref)
	}

	// 窗口内复活(内容相同 → 原地复活)
	if _, err := f.fin.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "window-revived.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		close(gate.release)
		t.Fatalf("窗口内复活失败: %v", err)
	}
	close(gate.release)

	out := <-done
	if out.err != nil {
		t.Fatalf("worker 运行失败: %v", out.err)
	}
	if out.res.Deleted != 0 {
		t.Fatalf("已被复活的对象不得被删,实际 Deleted=%d", out.res.Deleted)
	}
	if state, ref := f.objState(t, hash); state != model.ObjectLive || ref != 1 {
		t.Fatalf("worker 跑完后应为 live/ref=1(不得悬空),实际 %s/%d", state, ref)
	}
	f.readBack(t, hash) // 验收①:文件可下载
}

// ②worker 真的把对象删掉(行已 deleted)之后才复活:
// 必须落"新建分支"重新落盘,且下载内容正确。
//
// 这是最能暴露顺序错误的一条:若复活方只看数据库行(不信物理对象),
// 就会把 ref_count 加回去、状态置 live,而物理文件已经没了 ——
// 文件列表正常、下载 404。
func TestReviveAfterUnlinkRewritesObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("unlinked-object"), 2048)
	_, hash := f.upload(t, "unlinked.bin", data)
	f.garbage(t, hash)

	// worker 真删:物理对象消失、行标 deleted
	res, err := f.worker().RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("worker 运行失败: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("应恰好物理删除 1 个对象,实际 %d", res.Deleted)
	}
	if _, err := f.store.Stat(ctx, hash); err == nil {
		t.Fatal("物理对象应已被删除")
	}
	if state, ref := f.objState(t, hash); state != model.ObjectDeleted || ref != 0 {
		t.Fatalf("删除后应为 deleted/ref=0,实际 %s/%d", state, ref)
	}

	// 已删除之后复活:内容必须**重新落盘**
	if _, err := f.fin.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "revived-late.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("删除后复活失败: %v", err)
	}
	if state, ref := f.objState(t, hash); state != model.ObjectLive || ref != 1 {
		t.Fatalf("复活后应为 live/ref=1,实际 %s/%d", state, ref)
	}
	if _, err := f.store.Stat(ctx, hash); err != nil {
		t.Fatalf("复活必须重新落盘(否则是悬空指针): %v", err)
	}
	f.readBack(t, hash)
}

// ③进程 kill -9 后重启:无悬空 files 行(抽样 Stat 全命中)。
//
// "kill -9"用两种残留态表达(都来自 worker 在中间步骤崩掉):
//   - 崩在"已领取、还没 unlink":行 deleting + 租约(连接已断 ⇒ 会话锁自动释放)
//   - 崩在"已 unlink、还没标 deleted":物理对象没了,行仍是 deleting
//
// 重启后用**新的 worker 实例**(新连接、新内存态)做看护复位 + 跑一轮,
// 然后抽样:本用例空间里每个 files 行的 hash 都必须能 Stat 命中 ——
// 这正是"无悬空指针"的可判定形式。
func TestCrashRestartLeavesNoDanglingFiles(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// 三个正常文件(重启后必须全部可下载)
	var keep []string
	for i := 0; i < 3; i++ {
		_, h := f.upload(t, "keep-"+string(rune('a'+i))+".bin", bytes.Repeat([]byte{byte(i + 1)}, 512))
		keep = append(keep, h)
	}
	// 崩在 unlink 前
	_, beforeUnlink := f.upload(t, "crash-before.bin", bytes.Repeat([]byte("x"), 300))
	f.garbage(t, beforeUnlink)
	if _, err := f.database.Pool.Exec(ctx, `
UPDATE file_objects SET state = 'deleting', delete_after = now() + interval '10 minutes' WHERE hash_sha256 = $1`,
		beforeUnlink); err != nil {
		t.Fatalf("造残留态失败: %v", err)
	}
	// 崩在标 deleted 前:物理对象已被删,行还停在 deleting
	_, afterUnlink := f.upload(t, "crash-after.bin", bytes.Repeat([]byte("y"), 400))
	f.garbage(t, afterUnlink)
	if err := f.store.Delete(ctx, afterUnlink); err != nil {
		t.Fatalf("物理删除失败: %v", err)
	}
	if _, err := f.database.Pool.Exec(ctx, `
UPDATE file_objects SET state = 'deleting', delete_after = now() - interval '1 hour' WHERE hash_sha256 = $1`,
		afterUnlink); err != nil {
		t.Fatalf("造残留态失败: %v", err)
	}

	// ---- 重启:全新的 worker 实例(会话锁随旧连接断开而释放)----
	w := f.worker()
	scope := append(append([]string{}, keep...), beforeUnlink, afterUnlink)
	// 先复位"崩在 unlink 前"那条的**过期**租约(beforeUnlink 的租约未过期,看护不该动它)
	if n, err := w.ResetStaleClaims(ctx, scope); err != nil {
		t.Fatalf("看护复位失败: %v", err)
	} else if n != 1 {
		t.Fatalf("应恰好复位 1 条超时 deleting(未过期的那条不能被动),实际 %d", n)
	}
	if state, _ := f.objState(t, beforeUnlink); state != model.ObjectDeleting {
		t.Fatalf("租约未过期的行不得被复位,实际 %s", state)
	}
	if state, _ := f.objState(t, afterUnlink); state != model.ObjectPendingDelete {
		t.Fatalf("超时行应复位为 pending_delete,实际 %s", state)
	}

	if _, err := w.RunOnceFor(ctx, scope, 50); err != nil {
		t.Fatalf("重启后清理失败: %v", err)
	}
	// 崩在"已 unlink、还没标 deleted"的那条:看护复位后可被重新领取 → deleted
	if state, ref := f.objState(t, afterUnlink); state != model.ObjectDeleted || ref != 0 {
		t.Fatalf("重启后该垃圾对象应为 deleted/ref=0,实际 %s/%d", state, ref)
	}
	// 崩在"unlink 前"的那条:**租约还没过期,重启后的 worker 不得抢**。
	// 这是刻意的:租约的意义就是"这轮清理归某个 worker",抢租约会让
	// "对象正在被删"与"刚被复活"撞在一起(三审 P1-1 的原始场景)。
	if state, _ := f.objState(t, beforeUnlink); state != model.ObjectDeleting {
		t.Fatalf("租约未过期的行不得被重启的 worker 处理,实际 %s", state)
	}
	if _, err := f.store.Stat(ctx, beforeUnlink); err != nil {
		t.Fatalf("租约未过期的对象不该被删掉: %v", err)
	}
	// 租约到期后(等价于"等 ClaimTTL 过去",这里直接把租约推到期)自愈为 deleted
	if _, err := f.database.Pool.Exec(ctx, `
UPDATE file_objects SET delete_after = now() - interval '1 hour' WHERE hash_sha256 = $1`,
		beforeUnlink); err != nil {
		t.Fatalf("推过期租约失败: %v", err)
	}
	if n, err := w.ResetStaleClaims(ctx, scope); err != nil {
		t.Fatalf("二次看护复位失败: %v", err)
	} else if n != 1 {
		t.Fatalf("应复位 1 条超时 deleting,实际 %d", n)
	}
	if _, err := w.RunOnceFor(ctx, scope, 50); err != nil {
		t.Fatalf("二次清理失败: %v", err)
	}
	if state, ref := f.objState(t, beforeUnlink); state != model.ObjectDeleted || ref != 0 {
		t.Fatalf("租约到期后应被清理为 deleted/ref=0,实际 %s/%d", state, ref)
	}

	// ---- 抽样:本用例空间里所有 files 行都必须 Stat 命中 ----
	rows, err := f.database.Pool.Query(ctx, `
SELECT f.name, f.hash_sha256 FROM files f
 WHERE f.space_id = $1 AND f.hash_sha256 IS NOT NULL`, f.spaceID)
	if err != nil {
		t.Fatalf("列出 files 失败: %v", err)
	}
	type row struct{ name, hash string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.name, &r.hash); err != nil {
			rows.Close()
			t.Fatalf("扫描失败: %v", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if len(all) != len(keep) {
		t.Fatalf("应有 %d 个存活文件行(垃圾对象不应再有 files 行),实际 %d", len(keep), len(all))
	}
	for _, r := range all {
		if _, err := f.store.Stat(ctx, r.hash); err != nil {
			t.Fatalf("悬空指针:文件 %q 指向的对象不存在(%v)", r.name, err)
		}
		f.readBack(t, r.hash)
	}
}
