package lifecycle_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/lifecycle"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// file_objects 生命周期 worker 的真实 PG 集成测试(BE-S5-07 / ADR-2)。
// 未设置 NETDISK_TEST_DSN 时全部 skip。

type fixture struct {
	svc      *lifecycle.Service
	database *db.DB
	pool     *pgxpool.Pool
	store    *storage.FS
	userID   string
	spaceID  string
	rootID   string
	// made 记录本用例**直接造出来**的对象 hash。
	//
	// 必须显式记录:这些对象行没有对应的 files 行(用例是直接插 file_objects 的),
	// 因此"按空间引用的 hash 清理"覆盖不到它们。
	// 而**绝不能**用全表条件(如 `DELETE FROM file_objects WHERE ref_count <= 0`)
	// 兜底 —— 那会把同包其它用例正在断言的对象一起删掉,表现为
	// "读对象行失败: no rows"(实测踩过)。
	made []string
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
		created, err := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: "lc_" + suffix, Email: "lc_" + suffix + "@example.com",
			DisplayName: "生命周期用例用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	// 注意:cleanup 注册在下面 fx 造好之后 —— 它要用 fx.made 记录的对象 hash。

	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	root, err := repo.FileRepo{}.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}

	fx := &fixture{userID: u.ID, spaceID: sp.ID, rootID: root.ID}
	fx.svc = &lifecycle.Service{
		Pool:        database.Pool,
		Storage:     store,
		DeleteAfter: time.Hour,
		ClaimTTL:    10 * time.Minute,
	}
	fx.database = database
	fx.pool = database.Pool
	fx.store = store
	// 把 cleanup 注册放在 fx 造好之后:它要用 fx.made 记录的对象 hash
	t.Cleanup(func() { fx.cleanup(t, database) })
	return fx
}

// cleanup 见 setup 里的说明(抽出来便于在 setup 尾部注册)。
func (f *fixture) cleanup(t *testing.T, database *db.DB) {
	bg := context.Background()
	t.Helper()
	if len(f.made) > 0 {
		_, _ = database.Pool.Exec(bg, `DELETE FROM file_objects WHERE hash_sha256 = ANY($1::varchar[])`, f.made)
	}
	_, _ = database.Pool.Exec(bg, `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, f.userID)
	_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, f.userID)
	_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, f.userID)
	_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, f.userID)
}

// mkObject 造一个对象:物理落盘 + 建 file_objects 行(可选建 files 行引用它)。
//
// 内容里混入**用例名 + 纳秒时间戳**:内容寻址下"相同内容 = 相同 hash",
// 若两个用例用同样的 seed 与长度,第二个会覆盖第一个的对象行
// (ON CONFLICT 会改写 state/ref_count),导致断言看到的是别人的状态。
// 这个坑实测踩过(Stats 用例的 live 增量成了 0 —— 对象被另一个用例的行覆盖)。
func (f *fixture) mkObject(t *testing.T, seed string, refCount int, state string, deleteAfter *time.Time) string {
	t.Helper()
	ctx := context.Background()
	unique := fmt.Sprintf("%s|%s|%d", t.Name(), seed, time.Now().UnixNano())
	data := []byte(unique)
	for len(data) < 256 {
		data = append(data, []byte(unique)...)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	if _, err := f.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写对象失败: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state, delete_after)
VALUES ($1, $2, 'fs', $3, $4, $5, $6)`,
		hash, len(data), f.store.KeyFor(hash), refCount, state, deleteAfter); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}
	// 登记以便 cleanup 精确删除(这些对象行没有 files 行指向,按空间清理覆盖不到)
	f.made = append(f.made, hash)
	return hash
}

// mkFileRef 建一行 files 指向该 hash(用于制造"仍被引用"的场景)
func (f *fixture) mkFileRef(t *testing.T, hash, name string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, hash_sha256, depth)
VALUES ($1, $2, $3, $4, false, 256, $5, 1) RETURNING id`,
		f.spaceID, f.rootID, f.userID, name, hash).Scan(&id); err != nil {
		t.Fatalf("造文件行失败: %v", err)
	}
	return id
}

func (f *fixture) objState(t *testing.T, hash string) (string, int, *time.Time) {
	t.Helper()
	var state string
	var ref int
	var after *time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT state, ref_count, delete_after FROM file_objects WHERE hash_sha256=$1`, hash).
		Scan(&state, &ref, &after); err != nil {
		t.Fatalf("读对象行失败: %v", err)
	}
	return state, ref, after
}

func pastTime() *time.Time { t := time.Now().Add(-time.Hour); return &t }

// ---- 主路径:到期 → 领取 → 物理删 → 标 deleted ----

func TestRunOnceDeletesExpiredObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	hash := f.mkObject(t, "gone", 0, model.ObjectPendingDelete, pastTime())

	res, err := f.svc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("应删除 1 个对象,实际 %+v", res)
	}
	// 物理对象必须已删
	if _, err := f.store.Stat(ctx, hash); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("物理对象应已删除,实际 Stat=%v", err)
	}
	// 行标记 deleted(不是被摘除)
	state, ref, _ := f.objState(t, hash)
	if state != model.ObjectDeleted {
		t.Fatalf("状态应为 deleted,实际 %s", state)
	}
	if ref != 0 {
		t.Fatalf("引用数应保持 0,实际 %d", ref)
	}
}

// **SKIP LOCKED 分支:被别的会话锁住的行必须被跳过,而不是让 worker 等在那里**
//
// TS-02 记下的缺口:`claim` 里的 `FOR UPDATE SKIP LOCKED` 这条分支**无人断言** ——
// 把 SKIP LOCKED 删掉,既有用例照样全绿(它们从不并发持有行锁)。
// 判据是**不阻塞**:回收是后台兜底,遇到别人正在处理的行要毫秒级跳过、下一轮再来;
// 堵住会让整批回收停摆,而现象只是"磁盘迟迟不降"(没有任何报错)。
func TestRunOnceSkipsObjectLockedByAnotherSession(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// delete_after 升序 = claim 的取行顺序:把**被锁住的那条排在前面**,
	// 这样"不跳过"的实现会先撞上它并阻塞(SKIP LOCKED 才有区分度)。
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	locked := f.mkObject(t, "locked", 0, model.ObjectPendingDelete, &older)
	free := f.mkObject(t, "free", 0, model.ObjectPendingDelete, &newer)

	// 另一条连接上锁住"排在前面"的那一行(事务不提交)
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx,
		`SELECT hash_sha256 FROM file_objects WHERE hash_sha256=$1 FOR UPDATE`, locked); err != nil {
		t.Fatalf("锁对象行失败: %v", err)
	}
	// 先证明锁**真的**持有(NOWAIT 必须拿 55P03),否则用例可能空过
	{
		c2, aerr := f.pool.Acquire(ctx)
		if aerr != nil {
			t.Fatalf("取连接失败: %v", aerr)
		}
		_, lerr := c2.Exec(ctx,
			`SELECT hash_sha256 FROM file_objects WHERE hash_sha256=$1 FOR UPDATE NOWAIT`, locked)
		c2.Release()
		var pgErr *pgconn.PgError
		if !errors.As(lerr, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("预期该行已被锁住(55P03 lock_not_available),实际 err=%v", lerr)
		}
	}

	type outcome struct {
		res *lifecycle.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, rerr := f.svc.RunOnceFor(ctx, []string{locked, free}, 10)
		done <- outcome{r, rerr}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("被锁住的对象把 worker **堵住**了(SQL 的 SKIP LOCKED 失效):应当毫秒级跳过")
	}
	if got.err != nil {
		t.Fatalf("RunOnce 失败: %v", got.err)
	}
	if got.res.Deleted != 1 {
		t.Fatalf("应恰好处理未被锁住的那 1 个,实际 %+v", got.res)
	}
	if state, _, _ := f.objState(t, free); state != model.ObjectDeleted {
		t.Fatalf("未被锁住的对象应已删除,实际 %s", state)
	}
	if state, _, _ := f.objState(t, locked); state != model.ObjectPendingDelete {
		t.Fatalf("被别的会话锁住的对象不该被动,实际 %s", state)
	}

	// 放锁后的下一轮**必须**能处理它 —— 证明上一步是"跳过",而不是"永远轮不到"
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	res2, err := f.svc.RunOnceFor(ctx, []string{locked}, 10)
	if err != nil {
		t.Fatalf("第二轮 RunOnce 失败: %v", err)
	}
	if res2.Deleted != 1 {
		t.Fatalf("放锁后应能处理被跳过的那条,实际 %+v", res2)
	}
}

// 未到期不能删(延迟删除窗口的意义:给并发复活留出时间)
func TestRunOnceSkipsNotYetDue(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour)
	hash := f.mkObject(t, "notyet", 0, model.ObjectPendingDelete, &future)

	res, err := f.svc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("未到期不应删除,实际 %+v", res)
	}
	if _, err := f.store.Stat(ctx, hash); err != nil {
		t.Fatalf("未到期对象必须仍在: %v", err)
	}
	state, _, _ := f.objState(t, hash)
	if state != model.ObjectPendingDelete {
		t.Fatalf("状态应保持 pending_delete,实际 %s", state)
	}
}

// **ref_count > 0 绝对不能删**(少了这个条件就是悬空指针)
func TestRunOnceNeverDeletesReferencedObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 造一个"状态标了 pending_delete 但引用数不为 0"的坏数据(绕过应用层直接写)
	hash := f.mkObject(t, "referenced", 1, model.ObjectLive, nil)
	if _, err := f.pool.Exec(ctx,
		`UPDATE file_objects SET state='pending_delete', delete_after=now()-interval '1 hour' WHERE hash_sha256=$1`,
		hash); err != nil {
		// 表上的 CHECK 不变式应当直接拒绝这种"ref>0 且 state<>live"的写法
		if !repo.IsCheckViolation(err) {
			t.Fatalf("预期 CHECK 约束拒绝或成功,实际 %v", err)
		}
		t.Log("CHECK 约束正确拒绝了 ref>0 却非 live 的行 —— 不变式由数据库兜底")
		return
	}
	t.Fatal("ref_count>0 且 state<>live 的写入应被表约束拒绝(不变式失效)")
}

// 对象物理文件已不在(被外部删除)→ 删除仍应成功并把行标 deleted(幂等)
func TestRunOnceSucceedsWhenObjectAlreadyGone(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	hash := f.mkObject(t, "alreadygone", 0, model.ObjectPendingDelete, pastTime())
	if err := f.store.Delete(ctx, hash); err != nil {
		t.Fatalf("预删物理对象失败: %v", err)
	}

	res, err := f.svc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("对象已不在时应视为删除成功,实际 %+v", res)
	}
	state, _, _ := f.objState(t, hash)
	if state != model.ObjectDeleted {
		t.Fatalf("状态应为 deleted,实际 %s", state)
	}
}

// RunOnce(全表扫描)是**生产路径**(定时任务用它),必须仍有用例覆盖 ——
// 但它**不能**用"全表计数"做断言:file_objects 是全库共享表,别的包在同一秒里
// 造/清对象完全正常,于是"应删除 1 个"会随机变成 2 或 3(实测被 consistency 包
// 咬到过)。这里只断言"调用成功 + 我自己的那个对象被删掉",与并行的别人无关。
func TestRunOnceGlobalScopeStillWorks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	hash := f.mkObject(t, "global", 0, model.ObjectPendingDelete, pastTime())

	if _, err := f.svc.RunOnce(ctx, 100); err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	state, _, _ := f.objState(t, hash)
	if state != model.ObjectDeleted {
		t.Fatalf("全表扫描必须能清到我这个到期对象,实际状态 %s", state)
	}
}

// ---- 看护:复位超时领取 ----

func TestResetStaleClaims(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// worker 崩溃残留:deleting 且租约已过期
	stale := f.mkObject(t, "stale", 0, model.ObjectDeleting, pastTime())
	// 正常处理中:deleting 但租约未过期
	fresh := time.Now().Add(5 * time.Minute)
	active := f.mkObject(t, "active", 0, model.ObjectDeleting, &fresh)

	n, err := f.svc.RunOnceFor(ctx, []string{stale, active}, 100)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if n.Reset != 1 {
		t.Fatalf("应复位 1 个超时领取,实际 %+v", n)
	}
	if st, _, _ := f.objState(t, stale); st != model.ObjectDeleted {
		t.Fatalf("超时残留应被复位并删除,实际 %s", st)
	}
	// 未超时的不该被动
	if st, _, _ := f.objState(t, active); st != model.ObjectDeleting {
		t.Fatalf("未超时的领取不应被动,实际 %s", st)
	}
}

// ---- 复活互斥:ADR-2 的真实竞态 ----

// **核心用例**:worker 领取后、物理删之前,并发请求命中同一 hash 复活。
//
// 断言的是**安全性质**而不是某个具体结局:
//   - 复活成功(ref>0)→ 状态必须 live 且物理对象必须仍在(可读)
//   - 复活失败(worker 先删完)→ 状态必须 deleted 且 ref 必须为 0
//   - **禁止的结局**:状态已删但 ref>0 —— 那正是悬空指针
//
// 不断言"复活一定成功":两者是并发竞速,worker 完全可能先跑完;那时代复活语句
// 匹配 0 行、调用方落"新建分支"重新落盘 —— 同样正确。断言具体结局会让用例
// 随机失败(实测过),而随机失败的用例比没有用例更糟(浪费排查时间还掩盖真问题)。
func TestConcurrentReviveDuringClaimNeverLosesObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 内容必须**唯一**:内容寻址下同内容 = 同 hash,固定内容会在第二次运行
	// 撞上"上一轮残留的行"而报重复键(实测),失败现象完全指向错误的地方。
	seed := fmt.Sprintf("%s|%d", t.Name(), time.Now().UnixNano())
	data := bytes.Repeat([]byte(seed), 32)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	// 对象到期待删(ref=0),物理内容仍在
	if _, err := f.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写对象失败: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state, delete_after)
VALUES ($1, $2, 'fs', $3, 0, 'pending_delete', now() - interval '1 hour')`,
		hash, len(data), f.store.KeyFor(hash)); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}
	var existsBefore int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&existsBefore); err != nil {
		t.Fatalf("前置查询失败: %v", err)
	}
	if existsBefore != 1 {
		t.Fatalf("前置条件:插入后对象行应存在,实际 %d", existsBefore)
	}

	var wg sync.WaitGroup
	var workerRes *lifecycle.Result
	var workerErr, reviveErr error
	var revived bool
	var existsAfter int
	wg.Add(2)
	go func() {
		defer wg.Done()
		workerRes, workerErr = f.svc.RunOnceFor(ctx, []string{hash}, 10)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(2 * time.Millisecond)
		tag, err := f.pool.Exec(ctx, `
UPDATE file_objects
   SET ref_count = ref_count + 1, state = 'live', delete_after = NULL, updated_at = now()
 WHERE hash_sha256 = $1 AND size = $2 AND state IN ('live','pending_delete','deleting')`, hash, len(data))
		reviveErr = err
		revived = err == nil && tag.RowsAffected() > 0
	}()
	wg.Wait()
	if workerErr != nil {
		t.Fatalf("worker 出错: %v", workerErr)
	}
	if reviveErr != nil {
		t.Fatalf("复活出错: %v", reviveErr)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&existsAfter); err != nil {
		t.Fatalf("后置查询失败: %v", err)
	}
	if existsAfter == 0 {
		// 行被**删除**了(而不是状态变化)—— 说明有其它代码在删这张表。
		// 这条守卫是真踩过的坑:finalize 包的测试清理里曾有一条**全表**
		// `DELETE FROM file_objects WHERE ref_count <= 0`,而 ref_count=0
		// 正是"待清理对象"的共同特征 —— 它会把本包正在断言的对象一起删掉,
		// 表现为"读对象行失败: no rows",单独跑却必过。
		t.Fatalf("对象行在本用例期间被删除(前=%d 后=%d,hash=%s worker=%+v revived=%v)",
			existsBefore, existsAfter, hash[:12], workerRes, revived)
	}

	state, ref, _ := f.objState(t, hash)
	if ref > 0 {
		if state != model.ObjectLive {
			t.Fatalf("复活成功后状态应为 live,实际 %s(悬空指针)", state)
		}
		if _, err := f.store.Stat(ctx, hash); err != nil {
			t.Fatalf("复活成功后物理对象必须仍在(否则文件不可读): %v", err)
		}
	} else {
		if state != model.ObjectDeleted {
			t.Fatalf("复活失败时状态应为 deleted,实际 %s", state)
		}
		if _, err := f.store.Stat(ctx, hash); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal("复活失败时物理对象应已彻底删除")
		}
	}
	t.Logf("结局:state=%s ref=%d revived=%v worker=%+v(两种结局均安全)", state, ref, revived, workerRes)
}

// **确定性**覆盖"复活先于 worker"这一分支:直接造出"已被软领取(deleting)"
// 的对象行,在其上复活,再让 worker 处理同一 hash —— worker 拿锁后复核发现
// ref>0/state=live,必须跳过删除。
//
// 并发用例只能证明"不出错",这一条才能确定性地证明"复活的对象不会被删"。
func TestWorkerSkipsObjectRevivedWhileClaimPending(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 内容必须**唯一**(内容寻址下同内容=同 hash,会撞上一次运行的残留行)
	seed := fmt.Sprintf("%s|%d", t.Name(), time.Now().UnixNano())
	data := bytes.Repeat([]byte(seed), 32)
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	if _, err := f.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写对象失败: %v", err)
	}
	// 造"已被软领取"的行(deleting),模拟 worker 领取后尚未物理删的瞬间
	if _, err := f.pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state, delete_after)
VALUES ($1, $2, 'fs', $3, 0, 'deleting', now() - interval '1 hour')`,
		hash, len(data), f.store.KeyFor(hash)); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}

	// 复活(与 finalize 的复活语句同构):deleting 态仍可复活,内容还在
	tag, err := f.pool.Exec(ctx, `
UPDATE file_objects
   SET ref_count = ref_count + 1, state = 'live', delete_after = NULL, updated_at = now()
 WHERE hash_sha256 = $1 AND size = $2 AND state IN ('live','pending_delete','deleting')`, hash, len(data))
	if err != nil {
		t.Fatalf("复活失败: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatal("deleting 态应仍可复活(内容还在)—— 复活语句条件写错了?")
	}

	// worker 处理:看到 ref>0 → 必须跳过
	res, err := f.svc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("已被复活的对象不得被删除,实际 %+v", res)
	}
	if _, err := f.store.Stat(ctx, hash); err != nil {
		t.Fatalf("复活后物理对象必须仍在: %v", err)
	}
	state, ref, after := f.objState(t, hash)
	if state != model.ObjectLive || ref != 1 {
		t.Fatalf("应为 live/ref=1,实际 %s/%d", state, ref)
	}
	if after != nil {
		t.Fatalf("复活后 delete_after 应清空,实际 %v", after)
	}
}

// 复活后 worker **不得**删除对象(串行场景,确定性验证)
func TestWorkerSkipsRevivedObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	hash := f.mkObject(t, "revived", 0, model.ObjectPendingDelete, pastTime())

	// 先复活(ref=1, state=live)——模拟复活方先拿到锁
	if _, err := f.pool.Exec(ctx, `
UPDATE file_objects SET ref_count = ref_count + 1, state='live', delete_after=NULL
 WHERE hash_sha256=$1`, hash); err != nil {
		t.Fatalf("复活失败: %v", err)
	}

	res, err := f.svc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("RunOnce 失败: %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("已被复活的对象不应被删除,实际 %+v", res)
	}
	if _, err := f.store.Stat(ctx, hash); err != nil {
		t.Fatalf("复活对象必须仍在: %v", err)
	}
	state, ref, _ := f.objState(t, hash)
	if state != model.ObjectLive || ref != 1 {
		t.Fatalf("复活后应为 live/ref=1,实际 %s/%d", state, ref)
	}
}

// ---- 兜底:把漂移的 live+ref=0 转入延迟删除 ----

// MarkPendingDelete 把 ref=0 的 live 对象转入延迟删除。
//
// 注意:表上的不变式 `(ref_count = 0) = (state <> 'live')` 使得
// "ref=0 且 live"这种行**根本无法写入** —— 所以这个函数在生产里不是
// 修复漂移用的,而是给"删除文件时忘了同步置态"的**兜底**:
// 正常情况下删除路径会在同一事务里把行置 pending_delete,这里只是
// 万一漏了的一道保险。用例通过**临时禁用触发器之外的方式**无法造出这种行,
// 因此这里验证的是"函数本身正确且不误伤仍被引用的对象"。
func TestMarkPendingDeleteOnlyTouchesUnreferenced(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 一个仍被引用的对象(必须不被碰)
	live := f.mkObject(t, "stillref", 1, model.ObjectLive, nil)
	// 一个已归零、已到期待清的对象(应被本函数与后续清理处理)
	pend := f.mkObject(t, "already", 0, model.ObjectPendingDelete, pastTime())

	if _, err := f.svc.MarkPendingDelete(ctx, 100); err != nil {
		t.Fatalf("MarkPendingDelete 失败: %v", err)
	}
	if state, ref, _ := f.objState(t, live); state != model.ObjectLive || ref != 1 {
		t.Fatalf("仍被引用的对象不得被动: %s/%d", state, ref)
	}
	if state, _, _ := f.objState(t, pend); state != model.ObjectPendingDelete {
		t.Fatalf("已归零对象应保持 pending_delete,实际 %s", state)
	}
	// 直接验证:尝试写入"ref=0 且 live"必须被约束拒绝(不变式由数据库兜底)
	_, err := f.pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES (repeat('f', 64), 1, 'fs', 'objects/ff/ff/' || repeat('f', 64), 0, 'live')`)
	if err == nil {
		t.Fatal("ref=0 且 state=live 的写入必须被 CHECK 约束拒绝")
	}
	if !repo.IsCheckViolation(err) {
		t.Fatalf("应是 CHECK 约束冲突,实际 %v", err)
	}
}

// ---- 指标 ----

func TestStatsReportsStuckSignals(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// **用自己造的那几行做精确断言**,不看全表增量:
	// `file_objects` 是全库共享表,其它包在同一秒里增删行会让"增量"随机对不上,
	// 于是用例偶发失败并指向一段本来正确的代码(本项目多次踩到这类假失败)。
	liveH := f.mkObject(t, "live1", 1, model.ObjectLive, nil)
	overdueH := f.mkObject(t, "pend1", 0, model.ObjectPendingDelete, pastTime()) // 已到期 → overdue
	future := time.Now().Add(time.Hour)
	pendingH := f.mkObject(t, "pend2", 0, model.ObjectPendingDelete, &future) // 未到期
	staleT := time.Now().Add(-time.Minute)
	staleH := f.mkObject(t, "stale1", 0, model.ObjectDeleting, &staleT) // 超时 → stale

	got, err := f.svc.StatsFor(ctx, []string{liveH, overdueH, pendingH, staleH})
	if err != nil {
		t.Fatalf("StatsFor 失败: %v", err)
	}
	if got.Live != 1 || got.PendingDelete != 2 || got.Deleting != 1 || got.Deleted != 0 {
		t.Errorf("四态分布应为 live=1/pending_delete=2/deleting=1/deleted=0,实际 %+v", *got)
	}
	// 这两个是"worker 是否在工作"的告警源
	if got.PendingOverdue != 1 {
		t.Errorf("已到期未清理应为 1(worker 没跑的信号),实际 %d", got.PendingOverdue)
	}
	if got.DeletingStale != 1 {
		t.Errorf("超时领取应为 1(worker 崩溃的信号),实际 %d", got.DeletingStale)
	}

	// **作用域本身要被验证**:若实现忽略了 scope,上面的断言会在"恰好没别人跑"
	// 时通过 —— 用一个不存在的 hash 断言全零,作用域失效时必然失败。
	none, err := f.svc.StatsFor(ctx, []string{"0" + strings.Repeat("0", 63)})
	if err != nil {
		t.Fatalf("StatsFor(不存在) 失败: %v", err)
	}
	if *none != (lifecycle.Stats{}) {
		t.Errorf("限定到不存在的对象时应全为 0,实际 %+v", *none)
	}

	// Stats(全表)必须仍然可用(生产路径),且不小于上面那份子集统计
	all, err := f.svc.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if all.Live < got.Live || all.PendingDelete < got.PendingDelete {
		t.Errorf("全表统计不应小于子集统计: all=%+v subset=%+v", *all, *got)
	}
}

// 多 worker 并发清理不同对象:不得重复删、不得互相阻塞
func TestConcurrentWorkersDoNotDoubleDelete(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	const n = 10
	hashes := make([]string, n)
	for i := 0; i < n; i++ {
		hashes[i] = f.mkObject(t, fmt.Sprintf("multi%d", i), 0, model.ObjectPendingDelete, pastTime())
	}

	var wg sync.WaitGroup
	total := make([]int, 3)
	for w := 0; w < 3; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			res, err := f.svc.RunOnceFor(ctx, hashes, 5)
			if err != nil {
				return
			}
			total[w] = res.Deleted
		}(w)
	}
	wg.Wait()

	sum := 0
	for _, n := range total {
		sum += n
	}
	if sum != n {
		t.Fatalf("%d 个对象应恰好被删 %d 次(不重复),实际 %d: %v", n, n, sum, total)
	}
	for _, h := range hashes {
		state, _, _ := f.objState(t, h)
		if state != model.ObjectDeleted {
			t.Fatalf("对象 %s 状态应为 deleted,实际 %s", h[:8], state)
		}
		if _, err := f.store.Stat(ctx, h); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("对象 %s 物理文件应已删除", h[:8])
		}
	}
}

var _ = io.Discard
