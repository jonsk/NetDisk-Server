package objlock_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/objlock"
)

// objlock 的真实 PG 测试(ADR-2 连接纪律)。
//
// 这组用例的价值在于:连接纪律**写错不报错** —— 用池式拿/解锁时,
// `pg_advisory_unlock` 返回 false 而无人检查,后果是锁永久挂在池连接上、
// 同 hash 的一切引用操作被无限阻塞(且没有任何日志线索)。
// 所以这里既验正常路径,也验"锁真的互斥"与"解锁后能被再次取得"。

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return database.Pool
}

func TestSessionRunsCallbackOnSameConnection(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	var backendPID int

	err := objlock.Session(ctx, pool, "aabbccdd", 0, func(ctx context.Context, conn *pgx.Conn) error {
		// 拿到连接的真实后端 PID,用于后续证明"锁确实在这条连接上"
		return conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID)
	})
	if err != nil {
		t.Fatalf("Session 失败: %v", err)
	}
	if backendPID == 0 {
		t.Fatal("回调应能在持锁连接上执行 SQL")
	}
	// 按**同一锁键**检查锁已释放(不能数全实例的 advisory 锁:别的包也有)
	if lockHeld(t, pool, "aabbccdd") {
		t.Fatal("Session 返回后仍持有该 hash 的 advisory 锁")
	}
}

// 同一 hash 的锁必须**互斥**:第二个 Session 要等第一个退出回调才能进。
func TestSessionIsMutuallyExclusive(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	const hash = "deadbeefcafe0001"

	var inside int32
	var order []string
	var mu sync.Mutex
	record := func(s string) {
		mu.Lock()
		order = append(order, s)
		mu.Unlock()
	}

	release := make(chan struct{})
	firstIn := make(chan struct{})
	go func() {
		_ = objlock.Session(ctx, pool, hash, 0, func(ctx context.Context, conn *pgx.Conn) error {
			atomic.AddInt32(&inside, 1)
			record("first-enter")
			close(firstIn)
			<-release // 持有锁直到测试放行
			record("first-exit")
			atomic.AddInt32(&inside, -1)
			return nil
		})
	}()

	<-firstIn
	// 第二个 goroutine 尝试拿同键锁:应被阻塞,回调不能开始
	secondStarted := make(chan struct{})
	go func() {
		_ = objlock.Session(ctx, pool, hash, 0, func(ctx context.Context, conn *pgx.Conn) error {
			close(secondStarted)
			record("second-enter")
			return nil
		})
	}()

	select {
	case <-secondStarted:
		t.Fatal("同键锁未互斥:第二个回调在第一个持锁期间就开始了")
	case <-time.After(300 * time.Millisecond):
		// 正确:被阻塞
	}

	close(release)
	select {
	case <-secondStarted:
		// 正确:第一个退出后第二个才能开始
	case <-time.After(5 * time.Second):
		t.Fatal("第一个释放锁后,第二个仍拿不到锁(锁未释放?)")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 3 || order[0] != "first-enter" || order[1] != "first-exit" || order[2] != "second-enter" {
		t.Fatalf("执行顺序不对(锁未串行化): %v", order)
	}
}

// 不同 hash 之间不得互相阻塞(锁键必须按 hash 区分)
func TestSessionDifferentHashesDoNotBlock(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	release := make(chan struct{})
	gotA := make(chan struct{})

	go func() {
		_ = objlock.Session(ctx, pool, "hashAAAA", 0, func(ctx context.Context, conn *pgx.Conn) error {
			close(gotA)
			<-release
			return nil
		})
	}()
	<-gotA

	done := make(chan struct{})
	go func() {
		_ = objlock.Session(ctx, pool, "hashBBBB", 0, func(ctx context.Context, conn *pgx.Conn) error {
			return nil
		})
		close(done)
	}()
	select {
	case <-done:
		// 正确:不同键不互斥
	case <-time.After(3 * time.Second):
		t.Fatal("不同 hash 之间不应互相阻塞(锁键未按 hash 区分)")
	}
	close(release)
}

// 回调返回错误:仍必须解锁并归还连接(否则锁会被永久占用)
func TestSessionReleasesLockOnCallbackError(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	const hash = "eeee0000ffff0001"
	sentinel := errors.New("回调失败")

	err := objlock.Session(ctx, pool, hash, 0, func(ctx context.Context, conn *pgx.Conn) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应返回回调的原始错误,实际 %v", err)
	}
	// 锁必须已释放:立刻再拿同键锁应当成功且不超时
	done := make(chan struct{})
	go func() {
		_ = objlock.Session(ctx, pool, hash, 2*time.Second, func(ctx context.Context, conn *pgx.Conn) error {
			return nil
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("回调出错后锁未释放,同键锁再也拿不到")
	}
}

// lockHeld 判断"本进程是否仍持有某 hash 的 advisory 锁"。
//
// 必须**按锁键**查,而不是"实例上还有没有 advisory 锁":共用测试库里
// 别的包也在用 advisory 锁(部门闭包表串行化就用了一把),全实例计数
// 会把别人的锁算进来 —— 表现为"单独跑必过、全量跑必挂"。
func lockHeld(t *testing.T, pool *pgxpool.Pool, hash string) bool {
	t.Helper()
	var n int
	// hashtextextended 与实现里用的完全同一个函数,保证查的是同一把锁
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks
		  WHERE locktype = 'advisory' AND granted
		    AND objid = (hashtextextended($1,0) & x'ffffffff'::bigint)`, hash).Scan(&n)
	if err != nil {
		// objid 的拆分方式随 PG 版本略有差异,退化为"尝试非阻塞获取"探测
		var acquired bool
		if qerr := pool.QueryRow(context.Background(),
			`SELECT pg_try_advisory_lock(hashtextextended($1,0))`, hash).Scan(&acquired); qerr != nil {
			t.Fatalf("探测锁失败: %v / %v", err, qerr)
		}
		if acquired {
			_, _ = pool.Exec(context.Background(),
				`SELECT pg_advisory_unlock(hashtextextended($1,0))`, hash)
			return false
		}
		return true
	}
	return n > 0
}

// grantedLocksOf 数某条后端连接当前持有的 **advisory** 锁数量。
//
// 用例里只有 lockMany 会取 advisory 锁,所以这个计数就是"这条事务取了几把锁"
// 的可观测证据 —— 用它断言"被阻塞前只拿到了更小的那把"、"重复项只取一把"。
func grantedLocksOf(t *testing.T, pool *pgxpool.Pool, pid int) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted AND pid=$1`, pid).Scan(&n); err != nil {
		t.Fatalf("数后端 %d 的 advisory 锁失败: %v", pid, err)
	}
	return n
}

// waitUntilLocked 轮询等这些 hash 都真的被锁住。
//
// 为什么不用固定 sleep:「被阻塞」是异步发生的,慢机器上 sleep 完可能还没走到
// 第二把锁,断言会偶发失败;轮询到"已锁住"再断言才稳(有上限,真错了照样失败)。
func waitUntilLocked(t *testing.T, pool *pgxpool.Pool, hashes []string, within time.Duration) {
	t.Helper()
	if len(hashes) == 0 {
		return
	}
	deadline := time.Now().Add(within)
	for {
		all := true
		for _, h := range hashes {
			if !lockHeld(t, pool, h) {
				all = false
				break
			}
		}
		if all {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等 %v 被锁住超时:可能没按 hash 升序取锁(按调用方顺序取锁的实现"+
				"会一上来就阻塞,更小的那把一把都拿不到)", hashes)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 回调 panic:必须解锁、归还连接,并把 panic 继续外抛
func TestSessionReleasesLockOnPanic(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	const hash = "12340000abcd0001"

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic 应继续外抛(由 middleware.Recoverer 转 500)")
			}
		}()
		_ = objlock.Session(ctx, pool, hash, 0, func(ctx context.Context, conn *pgx.Conn) error {
			panic("boom")
		})
	}()

	// 锁必须已释放(兜底解锁生效):按**同一个锁键**检查
	if lockHeld(t, pool, hash) {
		t.Fatal("panic 后仍持有该 hash 的 advisory 锁(兜底解锁未生效)")
	}
	// 并且能立刻重新取得同键锁
	done := make(chan struct{})
	go func() {
		_ = objlock.Session(ctx, pool, hash, 2*time.Second, func(ctx context.Context, conn *pgx.Conn) error {
			return nil
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("panic 后同键锁再也拿不到(锁泄漏)")
	}
}

// 池上限很小时,锁在回调内被正确持有:回调内部再取连接也不会死锁
// (证明锁不额外占用连接名额 —— 它就在已 Acquire 的那条连接上)
func TestSessionHoldsLockOnAcquiredConnOnly(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	const hash = "99990000aaaa0001"

	err := objlock.Session(ctx, pool, hash, 0, func(ctx context.Context, conn *pgx.Conn) error {
		// 池上限是 4;回调内只用自己的连接,不应再 Acquire
		var pid int
		if err := conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			return err
		}
		// 同一连接上必须能看到自己持有的 advisory 锁
		var n int
		if err := conn.QueryRow(ctx, `
SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND pid = pg_backend_pid() AND granted`).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return errors.New("持锁连接上应能看到 advisory 锁")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Session 失败: %v", err)
	}
}

func TestSessionValidatesInput(t *testing.T) {
	pool := openPool(t)
	ctx := context.Background()
	if err := objlock.Session(ctx, nil, "h", 0, func(context.Context, *pgx.Conn) error { return nil }); err == nil {
		t.Fatal("Pool 为 nil 必须报错")
	}
	if err := objlock.Session(ctx, pool, "", 0, func(context.Context, *pgx.Conn) error { return nil }); err == nil {
		t.Fatal("空 hash 必须报错")
	}
}

func TestHashKeyIsShortButIdentifiable(t *testing.T) {
	long := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if got := objlock.HashKey(long); got != "0123456789ab" {
		t.Fatalf("应截断为前 12 位,实际 %q", got)
	}
	if got := objlock.HashKey("short"); got != "short" {
		t.Fatalf("短值应原样返回,实际 %q", got)
	}
}

// ---- 多 hash 事务级锁的测试小工具 ----

// inTx 是测试用的事务封装(生产侧由 db.InTx / finalize 自己的 inTx 提供)。
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

func lockMany(t *testing.T, ctx context.Context, tx pgx.Tx, hashes []string) error {
	t.Helper()
	return objlock.LockManyXact(ctx, tx, hashes)
}
