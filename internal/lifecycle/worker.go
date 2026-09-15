// Package lifecycle 实现 file_objects 的**引用归零 → 延迟删除 → 物理删除**闭环
// (6.4 四态状态机 / 9.11 / ADR-2)。
//
// # 状态机
//
//	live ──(ref_count 归零)──▶ pending_delete ──(软领取)──▶ deleting ──(物理删成功)──▶ deleted
//	                              ▲                              │
//	                              └────── 看护任务超时复位 ──────┘
//
// 不变式(表上 CHECK 约束与代码两侧共同保证):`ref_count = 0 ⟺ state <> 'live'`。
//
// # 为什么"先物理删对象、再标 deleted"
//
// 早期写法是"DELETE…RETURNING 领取,提交后物理删,再标记 deleted" —— 自相矛盾:
// 行已被摘除,无处可标 deleted。现行写法是**软领取**(只改状态,不删行),
// 于是"先删对象、后标态"成立。顺序反过来的话,标 deleted 之后进程崩溃,
// 对象就永远留在磁盘上且再也没有任何记录指向它(泄漏无人可查)。
//
// # 悬空指针窗口与 ADR-2 锁(这是本项目唯一的真竞态)
//
// 软领取用行锁领取,但**行锁随领取事务提交即释放**。于是存在窗口:
// 领取后、物理删之前,另一个请求命中同一 hash 复活成功 → worker 不再看行、
// 直接删掉对象 → 复活的文件指向不存在的对象(悬空指针,三审 P1-1)。
//
// 对策:worker 以**会话级** `pg_advisory_lock(hashtextextended(hash,0))`
// 贯穿"领取 → 物理删 → 标 deleted",复活方(事务级同键锁)只能排队;
// 序列化后两种结果都正确:
//   - 复活先 → worker 拿锁时行已 live → 领取条件不匹配,跳过删除
//   - worker 先 → 复活拿锁时行已 deleting/deleted → 复活条件命中 deleting
//     (仍可复活,内容还在),或匹配 0 行 → 落新建分支重新落盘
//
// 进程崩溃则连接断、锁自动释放,残留 deleting 由看护任务超时复位(自愈)。
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/objlock"
	"github.com/netdisk/netdisk/internal/storage"
)

// Pool 见 objlock.Pool 的说明(必须是能 Acquire 的那一个)。
type Pool = objlock.Pool

// Service 生命周期 worker。
type Service struct {
	Pool    Pool
	Storage storage.Storager
	Log     *slog.Logger
	// DeleteAfter 是引用归零后的延迟删除窗口(默认 24h)
	DeleteAfter time.Duration
	// ClaimTTL 是软领取的租约(默认 10min):超过即视为 worker 崩溃,由看护复位
	ClaimTTL time.Duration
	// LockTimeout 是单轮持锁的应用层上限(不允许无限挂锁)
	LockTimeout time.Duration
	// Now 便于测试注入时钟
	Now func() time.Time
}

// Result 是一轮清理的统计。
type Result struct {
	// Scanned 本轮领取到的对象数
	Scanned int
	// Deleted 物理删除成功并标记 deleted 的数量
	Deleted int
	// Skipped 因"已被并发复活/已被他人处理"而跳过的数量
	Skipped int
	// Failed 物理删除失败的数量(下轮重试)
	Failed int
	// Reset 看护任务复位的超时 deleting 数量
	Reset int
}

func (s *Service) deleteAfter() time.Duration {
	if s.DeleteAfter > 0 {
		return s.DeleteAfter
	}
	return 24 * time.Hour
}

func (s *Service) claimTTL() time.Duration {
	if s.ClaimTTL > 0 {
		return s.ClaimTTL
	}
	return 10 * time.Minute
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// RunOnce 执行一轮清理:先复位超时领取,再领取并删除到期对象。
//
// 设计为"一轮一函数"而不是常驻循环:便于在定时任务(systemd timer / cron)
// 或进程内 ticker 里复用,也让测试可以精确驱动一轮而不必等时间。
func (s *Service) RunOnce(ctx context.Context, limit int) (*Result, error) {
	return s.run(ctx, limit, nil)
}

// RunOnceFor 只处理**指定 hash 集合**中的对象。
//
// 为什么需要它:`file_objects` 是**全局共享**表,`RunOnce` 会领取"所有到期的
// 无引用对象"。在共用测试库里,这意味着 A 包的 worker 会顺手清掉 B 包正在
// 断言的对象 —— 表现为"单独跑某包必过、全量跑随机挂"(实测:两个包的各自动作
// 互相抢对象)。生产侧定时任务用 RunOnce(全表扫描是正确的),
// 测试与"按对象定向回收"用本函数。
func (s *Service) RunOnceFor(ctx context.Context, hashes []string, limit int) (*Result, error) {
	if len(hashes) == 0 {
		return &Result{}, nil
	}
	return s.run(ctx, limit, hashes)
}

func (s *Service) run(ctx context.Context, limit int, scope []string) (*Result, error) {
	if s.Pool == nil || s.Storage == nil {
		return nil, errors.New("lifecycle: Pool/Storage 未装配")
	}
	if limit <= 0 {
		limit = 100
	}
	res := &Result{}

	// 1) 看护:复位超时的 deleting(worker 崩溃残留),让它重新可被领取
	reset, err := s.ResetStaleClaims(ctx, scope)
	if err != nil {
		return nil, err
	}
	res.Reset = reset
	if reset > 0 {
		s.log().Warn("lifecycle: 复位超时领取的对象(worker 可能崩溃过)",
			"count", reset, "claim_ttl", s.claimTTL().String())
	}

	// 2) 领取一批到期对象
	candidates, err := s.claim(ctx, limit, scope)
	if err != nil {
		return nil, err
	}
	res.Scanned = len(candidates)

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		outcome := s.deleteOne(ctx, c)
		switch outcome {
		case outcomeDeleted:
			res.Deleted++
		case outcomeSkipped:
			res.Skipped++
		case outcomeFailed:
			res.Failed++
		}
	}
	return res, nil
}

// Candidate 是一个已软领取、待物理删除的对象。
type Candidate struct {
	Hash string
	Size int64
}

type outcome int

const (
	outcomeDeleted outcome = iota
	outcomeSkipped
	outcomeFailed
)

// claim 软领取一批到期对象。
//
// SQL 要点:
//   - `state='pending_delete' AND delete_after < now() AND ref_count = 0`:
//     三重条件缺一不可。少了 ref_count=0 会删掉仍被引用的对象(悬空指针)。
//   - `FOR UPDATE SKIP LOCKED`:多 worker 并发时不互相等待(不引 MQ 的代价)。
//   - 领取即置 deleting + 续租 delete_after=now()+claimTTL:`deleting` 是
//     "有人正在处理"的标记,续租让看护任务能在 worker 崩溃后准确判定超时。
//
// 注意:这里只改状态、**不删行** —— 软领取(见包注释)。
func (s *Service) claim(ctx context.Context, limit int, scope []string) ([]Candidate, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: 获取连接失败: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: begin 失败: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// scope 为空 = 全表(生产定时任务);非空 = 只处理指定 hash(定向回收/测试)
	rows, err := tx.Query(ctx, `
SELECT hash_sha256, size FROM file_objects
 WHERE state = 'pending_delete' AND delete_after < now() AND ref_count = 0
   AND ($2::varchar[] IS NULL OR hash_sha256 = ANY($2::varchar[]))
 ORDER BY delete_after
 LIMIT $1
 FOR UPDATE SKIP LOCKED`, limit, nullableScope(scope))
	if err != nil {
		return nil, fmt.Errorf("lifecycle: 领取失败: %w", err)
	}
	var out []Candidate
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.Hash, &c.Size); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, tx.Commit(ctx)
	}

	hashes := make([]string, 0, len(out))
	for _, c := range out {
		hashes = append(hashes, c.Hash)
	}
	if _, err := tx.Exec(ctx, `
UPDATE file_objects
   SET state = 'deleting', delete_after = now() + $2::interval, updated_at = now()
 WHERE hash_sha256 = ANY($1::varchar[]) AND ref_count = 0 AND state = 'pending_delete'`,
		hashes, intervalLiteral(s.claimTTL())); err != nil {
		return nil, fmt.Errorf("lifecycle: 标记领取失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("lifecycle: commit 失败: %w", err)
	}
	return out, nil
}

// deleteOne 删除单个对象:会话级对象锁贯穿"复核 → 物理删 → 标 deleted"。
//
// 三条纪律:
//  1. **先物理删对象、再标 deleted**(崩溃时宁可"对象已删、行仍 deleting",
//     由下轮重试幂等删除;反过来则对象泄漏且无人可查)。
//  2. 物理删在**事务外**(禁长事务跨 IO)。
//  3. 全程持有会话级对象锁,与复活方互斥(ADR-2)。
func (s *Service) deleteOne(ctx context.Context, c Candidate) outcome {
	err := objlock.Session(ctx, s.Pool, c.Hash, s.LockTimeout, func(ctx context.Context, conn *pgx.Conn) error {
		// 锁下复核:此刻状态可能已被并发复活改回 live
		var state string
		var refCount int
		if err := conn.QueryRow(ctx,
			`SELECT state, ref_count FROM file_objects WHERE hash_sha256 = $1`, c.Hash).
			Scan(&state, &refCount); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// 行已不存在(不该发生,但不必报错)→ 跳过
				return errSkip
			}
			return err
		}
		if refCount != 0 || (state != "deleting" && state != "pending_delete") {
			// 已被复活或已被处理 → 不删(这正是会话锁要防的窗口)
			return errSkip
		}

		// 物理删除(**事务外**):Delete 幂等,对象不存在不算失败
		if derr := s.Storage.Delete(ctx, c.Hash); derr != nil {
			// 删除失败:把行退回 pending_delete 让下轮重试,而不是留在 deleting
			// 等 10min 看护(那样会白等一个租约)
			_, _ = conn.Exec(ctx, `
UPDATE file_objects SET state = 'pending_delete', delete_after = now(), updated_at = now()
 WHERE hash_sha256 = $1 AND ref_count = 0`, c.Hash)
			return fmt.Errorf("lifecycle: 物理删除对象失败: %w", derr)
		}

		// 标记 deleted(第二个小事务,与物理删除分开)
		tx, terr := conn.Begin(ctx)
		if terr != nil {
			return terr
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		if _, terr := tx.Exec(ctx, `
UPDATE file_objects SET state = 'deleted', updated_at = now()
 WHERE hash_sha256 = $1 AND ref_count = 0`, c.Hash); terr != nil {
			return terr
		}
		return tx.Commit(ctx)
	})

	switch {
	case err == nil:
		return outcomeDeleted
	case errors.Is(err, errSkip):
		return outcomeSkipped
	default:
		s.log().Error("lifecycle: 删除对象失败(下轮重试)",
			"hash", objlock.HashKey(c.Hash), "err", err)
		return outcomeFailed
	}
}

var errSkip = errors.New("lifecycle: 跳过(已被复活或已被处理)")

// ResetStaleClaims 复位超时的 deleting(worker 崩溃残留)。
//
// 只复位**没有并发复活风险**的那些行:ref_count 仍为 0。若已被复活
// (ref_count>0),行本身已不在 deleting 态(复活语句会置 live),
// 因此这里不需要额外判断 —— 但仍带上 ref_count=0 作为防御。
//
// 复位后 delete_after=now(),下一轮即可重新领取,不需要人工介入。
func (s *Service) ResetStaleClaims(ctx context.Context, scope []string) (int, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("lifecycle: 获取连接失败: %w", err)
	}
	defer conn.Release()

	tag, err := conn.Exec(ctx, `
UPDATE file_objects
   SET state = 'pending_delete', delete_after = now(), updated_at = now()
 WHERE state = 'deleting' AND ref_count = 0 AND delete_after < now()
   AND ($1::varchar[] IS NULL OR hash_sha256 = ANY($1::varchar[]))`, nullableScope(scope))
	if err != nil {
		return 0, fmt.Errorf("lifecycle: 复位超时领取失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// nullableScope 把"空 scope"表达成 SQL NULL(表示不限范围)。
//
// 用 NULL 而不是空数组:空数组会让 `= ANY('{}')` 恒为 false(变成"什么都不处理"),
// 而语义上"没有指定范围"应当是"全表"。这类"空集合 vs 未指定"的混淆
// 是清理类代码里最常见的静默失效来源。
func nullableScope(scope []string) any {
	if len(scope) == 0 {
		return nil
	}
	return scope
}

// MarkPendingDelete 把引用已归零的 live 对象转入延迟删除窗口。
//
// 通常由删除文件的路径**在同一事务内**顺手完成(不等定时任务),这里提供
// 一个批量兜底版本:用于修复"历史数据里 ref_count 已为 0 但仍标 live"的漂移。
//
// 条件里的 `ref_count = 0` 与 `state = 'live'` 同时保证不变式
// `(ref_count = 0) = (state <> 'live')` 不被破坏 —— 少了任一条件,
// 表上的 CHECK 约束会直接拒绝这条 UPDATE。
func (s *Service) MarkPendingDelete(ctx context.Context, limit int) (int, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("lifecycle: 获取连接失败: %w", err)
	}
	defer conn.Release()

	if limit <= 0 {
		limit = 1000
	}
	tag, err := conn.Exec(ctx, `
UPDATE file_objects
   SET state = 'pending_delete',
       delete_after = now() + $2::interval,
       updated_at = now()
 WHERE hash_sha256 IN (
   SELECT hash_sha256 FROM file_objects
    WHERE state = 'live' AND ref_count = 0
    LIMIT $1)
   AND state = 'live' AND ref_count = 0`,
		limit, intervalLiteral(s.deleteAfter()))
	if err != nil {
		return 0, fmt.Errorf("lifecycle: 转入延迟删除失败: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Stats 是生命周期指标(8.4 监控)。
type Stats struct {
	Live          int64 `json:"live"`
	PendingDelete int64 `json:"pending_delete"`
	Deleting      int64 `json:"deleting"`
	Deleted       int64 `json:"deleted"`
	// PendingOverdue 是**已到期但未被清理**的数量(>0 说明 worker 没在跑)
	PendingOverdue int64 `json:"pending_overdue"`
	// DeletingStale 是超时未完成的领取数(>0 说明 worker 崩溃过)
	DeletingStale int64 `json:"deleting_stale"`
}

// Stats 采集四态分布与两个"卡住"信号。
//
// 刻意把 `pending_overdue` 与 `deleting_stale` 单列:它们不是状态分布,
// 而是"worker 是否在工作"的告警源 —— 状态分布正常但这两个指标持续为正,
// 说明清理链路已经断了(磁盘在涨而没人发现)。
func (s *Service) Stats(ctx context.Context) (*Stats, error) {
	return s.stats(ctx, nil)
}

// StatsFor 只统计**指定 hash 集合**内的对象(空集合 = 全表,与 Stats 等价)。
//
// 存在的理由与 `RunOnceFor` 相同:用例必须**在并发运行下也确定**。`file_objects`
// 是全库共享表,别的包(定稿、快照同步、对账)在同一秒里增删行是完全正常的 ——
// 用"全表增量"做断言的用例会因此偶发失败,而它给出的信号是"这个包有 bug",
// 于是有人去改一段本来正确的代码(本项目多次踩到这类假失败)。
// 把统计限定在"本次用例自己造的那几行"上,断言就变成**精确值**,
// 与其它包在跑什么无关。
func (s *Service) StatsFor(ctx context.Context, scope []string) (*Stats, error) {
	return s.stats(ctx, scope)
}

func (s *Service) stats(ctx context.Context, scope []string) (*Stats, error) {
	// nil 与空切片都必须表示"全表":`cardinality(NULL::varchar[])` 是 NULL,
	// 会把 WHERE 整体判成 NULL 从而**统计出全零** —— 一个"看起来一切正常"的
	// 空指标比报错更危险(告警不会响)。这条正是被用例里"全表 ≥ 子集"的断言抓到的。
	if scope == nil {
		scope = []string{}
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: 获取连接失败: %w", err)
	}
	defer conn.Release()

	// scope 为空数组时等价于全表(用 cardinality 而不是拼 SQL 字符串)
	var st Stats
	if err := conn.QueryRow(ctx, `
SELECT
  count(*) FILTER (WHERE state = 'live'),
  count(*) FILTER (WHERE state = 'pending_delete'),
  count(*) FILTER (WHERE state = 'deleting'),
  count(*) FILTER (WHERE state = 'deleted'),
  count(*) FILTER (WHERE state = 'pending_delete' AND delete_after < now()),
  count(*) FILTER (WHERE state = 'deleting' AND delete_after < now())
FROM file_objects
WHERE cardinality($1::varchar[]) = 0 OR hash_sha256 = ANY($1::varchar[])`,
		scope).Scan(
		&st.Live, &st.PendingDelete, &st.Deleting, &st.Deleted,
		&st.PendingOverdue, &st.DeletingStale); err != nil {
		return nil, fmt.Errorf("lifecycle: 统计失败: %w", err)
	}
	return &st, nil
}

// intervalLiteral 把 Duration 转成 PG interval 字面量(秒)。
//
// 不用参数化 interval 的原因:pgx 对 `$1::interval` + time.Duration 的编码
// 在不同版本上行为不一致;这里的值**全部来自服务端配置**(非用户输入),
// 且只保留整数秒,不存在注入面。
func intervalLiteral(d time.Duration) string {
	secs := int64(d.Seconds())
	if secs < 1 {
		secs = 1
	}
	return fmt.Sprintf("%d seconds", secs)
}
