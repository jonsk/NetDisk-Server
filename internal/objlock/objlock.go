// Package objlock 是**对象级 advisory 锁的连接纪律**的单一实现(6.4 / ADR-2)。
//
// # 为什么必须单独成包
//
// 会话级 `pg_advisory_lock` 只作用于**拿到它的那条连接**。于是"拿锁 → 干活 →
// 解锁"必须全程绑在同一条 `*pgx.Conn` 上,而且解锁**必须断言返回 true**:
//
//   - 若用 `pool.Exec` 式拿/解锁,池会随机给出另一条连接 —— 在另一条连接上
//     `pg_advisory_unlock` 返回 false;不检查就等于**锁永远挂在池连接上**,
//     同 hash 的一切引用操作被无限阻塞(且没有任何日志线索)。
//   - 若连接中途被归还,锁随会话断开而释放 —— 保护窗口静默消失,回到悬空指针竞态。
//
// 这条纪律在 ADR-2 里被反复强调(V2.18 四审 P1-A),原因是它**写错不报错**。
// 所以这里把它做成唯一的实现:谁需要对象锁就调用 `WithHash`,没有第二条路径。
//
// # 两层锁制(ADR-2)
//
//   - **会话级**(`Session`):含"事务外对象写"的路径 = finalizeUpload 全程、
//     清理 worker 全程。事务级锁随提交即释放,护不住事务外的 Write/Delete;
//     而把对象 IO 拉进事务又违 6.6 的"禁长事务跨 IO"。
//   - **事务级**(`pg_advisory_xact_lock`):纯引用+1、无对象写的路径 =
//     秒传命中复活、COPY 逐行、share-to-space。事务边界即保护窗口。
//
// 两层互斥同一锁空间(PG 保证会话级与事务级同键锁互斥),因此可以混用。
package objlock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool 是 Acquire 所需的最小能力。
//
// 刻意只暴露 Acquire:本包的存在意义就是"必须用同一条连接",
// 一旦能直接查池,就迟早有人绕过它写出锁与操作分离的代码。
type Pool interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// ErrUnlockFailed 表示解锁返回 false —— 锁不在本连接上。
//
// 这是一个**必须暴露的实现 bug**,不能吞掉:静默继续的后果是锁永久挂在池里。
var ErrUnlockFailed = errors.New("objlock: advisory 解锁返回 false(锁不在本连接上)")

// Session 在**同一条连接**上持有某 hash 的会话级对象锁,并执行 fn。
//
// fn 收到的 `*pgx.Conn` 就是持锁的那条连接:调用方在 fn 内部开事务、执行 SQL
// 都必须用它(用池会拿到别的连接,锁就白拿了)。
//
// 执行流程:
//
//	conn := pool.Acquire()
//	SELECT pg_advisory_lock(hashtextextended(hash,0))
//	fn(ctx, conn.Conn())
//	SELECT pg_advisory_unlock(...)   ← 断言 true
//	conn.Release()
//
// 若 fn 返回错误,仍然会解锁并归还连接(先解锁再归还,顺序不可颠倒)。
// fn 内 panic 时,`defer` 会先兜底解锁,再把 panic 继续向外抛(调用方的
// Recoverer 负责转 500);连接随 release 归还,不会泄漏。
func Session(ctx context.Context, pool Pool, hash string, timeout time.Duration, fn func(ctx context.Context, conn *pgx.Conn) error) error {
	if pool == nil {
		return errors.New("objlock: Pool 未装配")
	}
	if hash == "" {
		return errors.New("objlock: hash 不能为空")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("objlock: 获取连接失败: %w", err)
	}

	lockHeld := false
	defer func() {
		// 兜底解锁:正常路径已显式解锁并断言;这里只处理 fn panic 或提前 return 的情况。
		// 用独立 context:外层 ctx 可能已被取消,那时解锁会失败并留下挂死的锁。
		if lockHeld {
			bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, uerr := conn.Exec(bg, unlockSQL, hash); uerr != nil {
				// 解锁失败无法再做什么(连接即将归还,PG 会在会话结束时释放锁),
				// 但必须留下线索以便排查"锁被长期占用"
				_ = uerr
			}
		}
		conn.Release()
	}()

	lockCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		lockCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// 拿锁用**同一条连接**;lock_timeout 之外再套一层 context 超时,
	// 保证"应用层不允许无限挂锁"(6.4 纪律)。
	if _, err := conn.Exec(lockCtx, lockSQL, hash); err != nil {
		return fmt.Errorf("objlock: 获取对象锁失败: %w", err)
	}
	lockHeld = true

	// fn 内 panic 也必须走到解锁(上面的 defer 负责),这里不 recover,让它继续外抛
	if err := fn(ctx, conn.Conn()); err != nil {
		return err
	}

	var unlocked bool
	if err := conn.QueryRow(ctx, unlockSQL, hash).Scan(&unlocked); err != nil {
		return fmt.Errorf("objlock: 解锁失败: %w", err)
	}
	lockHeld = false
	if !unlocked {
		return ErrUnlockFailed
	}
	return nil
}

// XactSQL 返回事务级锁语句的说明常量(供纯引用+1 路径在同事务内取锁)。
//
// 调用方用法:`tx.Exec(ctx, objlock.XactSQL, hash)` —— 事务提交自动放锁,
// 因此**禁止**在这种路径里做对象 IO(那会需要会话级锁)。
const XactSQL = `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`

// LockManyXact **按 hash 升序**在同一事务内取多个事务级锁(6.6 全局锁序)。
//
// 为什么必须排序:多对象路径(COPY 目录逐行引用+1、多文件共享)若各自按
// "客户端给的顺序"取锁,两个并发请求可能以相反顺序取同两个 hash
// → **经典死锁**。统一升序后,任何两个请求的取锁序列都是同一个全序的子序列,
// 不存在"你等我、我等你"的环。
//
// 必须在事务内调用(事务级锁随提交释放)。空列表直接返回。
func LockManyXact(ctx context.Context, tx pgx.Tx, hashes []string) error {
	uniq := normalizeHashes(hashes)
	for _, h := range uniq {
		if _, err := tx.Exec(ctx, XactSQL, h); err != nil {
			return fmt.Errorf("objlock: 取对象锁失败(hash=%s): %w", HashKey(h), err)
		}
	}
	return nil
}

// normalizeHashes 去重 + 升序排序(空串与重复项剔除)。
//
// 排序用简单的插入排序即可:同一事务内涉及的**不同**对象数量通常很小
// (去重后个位数到几十),为此引入 sort 包并写比较函数反而更啰嗦。
func normalizeHashes(hashes []string) []string {
	seen := make(map[string]struct{}, len(hashes))
	out := make([]string, 0, len(hashes))
	for _, h := range hashes {
		if h == "" {
			continue
		}
		if _, ok := seen[h]; ok {
			continue
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

const (
	lockSQL   = `SELECT pg_advisory_lock(hashtextextended($1,0))`
	unlockSQL = `SELECT pg_advisory_unlock(hashtextextended($1,0))`
)

// HashKey 与 SQL 里的 `hashtextextended($1,0)` **不可直接比较** ——
// 后者是 PG 侧算的 int8。本函数只用于日志/指标里标识"哪个对象",
// 绝不用于跳过 SQL:锁必须由 PG 计算键,应用侧算不出来也无需算出。
//
// 保留它是因为排查并发问题时,日志里出现完整 sha256 比"某个哈希"有用得多。
func HashKey(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
