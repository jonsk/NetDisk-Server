// Package patrol 实现**对象泄漏与反向孤儿巡检**(8.4 / BE-S10-04)。
//
// # 为什么需要它
//
// 内容寻址 + 延迟删除的组合下,磁盘与 PG 是两本账:PG 说"这些对象还有引用",
// 磁盘说"我这儿有这么多字节"。两者一致是**设计假设**,而不是被持续验证的事实。
// 没有巡检时,两类事故都表现为"用户报障时才被发现":
//
//   - **正向泄漏(磁盘 > PG)**:对象被物理删除流程漏掉、或引用了不存在的对象行,
//     结果磁盘只涨不跌 —— 几个月后才发现时,已经无法回溯是哪次操作造成的;
//   - **反向孤儿(PG > 磁盘)**:对象文件丢了(误删/坏盘/恢复演练只恢复了库),
//     PG 说文件在、`GET` 才 404。这是**数据不可读**,比占空间严重得多。
//
// 两个方向必须分列上报(合成一个"不一致数"会让人无法判断先查哪个)。
//
// # 四条纪律(8.4 / V2.18 四审 P2-B)
//
//  1. **只读**。绝不自动修复、绝不改写 `file_objects` 状态 ——
//     抖动/误判被自动修复放大就是数据损坏,而巡检的判断依据(文件是否存在)
//     本身就可能因为一次网络抖动而错。
//  2. **抽样量按后端分级**:本地 fs 1000 行/日(stat 系统调用,廉价);
//     S3/OSS/MinIO 类远端后端 100 行/日(HEAD 计费 + 高延迟,千行级巡检本身
//     成为成本项)。后端类型取自 `Storager.Backend()`,不靠配置声明。
//  3. **"不可达"与"明确不存在"分开**:网络抖动 → 重试后仍失败 → 记**观测项**;
//     `ErrNotFound` → **告警**。把抖动当丢失会产出假警报,而假警报会训练运维
//     忽略这个指标 —— 那等于没有监控。
//  4. **正向差值可能为负**:PG > 磁盘时说明有对象文件不见了(反向问题),
//     照原样上报,不取绝对值、不夹到 0 —— 夹掉就再也看不见那个信号。
package patrol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// Service 是巡检服务。
type Service struct {
	// DB 只用于**读**(Querier 而非 authsvc.DB:巡检没有任何写需求,
	// 用只读接口把"只读"这条纪律变成类型约束)
	DB      repo.Querier
	Storage storage.Storager
	// FS 非空时才做正向(目录实际字节)统计 —— 远端后端没有本地目录,
	// 正向统计在那里应由对象存储自身的用量指标承担。
	FS  *storage.FS
	Log *slog.Logger
	// LocalSampleSize 是本地 fs 后端的每日抽样行数(默认 1000)
	LocalSampleSize int
	// RemoteSampleSize 是远端后端的每日抽样行数(默认 100)
	RemoteSampleSize int
	// LeakAlertBytes 是正向差值的告警阈值(默认 64MiB)
	LeakAlertBytes int64
	// UnreachableRetry 是不可达的重试次数(默认 2 次,即最多 3 次尝试)
	UnreachableRetry int
	// RetryBackoff 是重试间隔(默认 200ms;测试可设 0)
	RetryBackoff time.Duration
	// Now 便于测试注入时钟
	Now func() time.Time
	// Rand 便于测试固定抽样起点
	Rand *rand.Rand

	// Scope 限定巡检范围(为空 = 全表,生产正确行为)。
	//
	// 两个用途:
	//  1. **定向排查**:怀疑某几个对象有问题时,只查这几个(不然要等一轮全表抽样);
	//  2. **测试**:`file_objects` 是**全局共享**表,而测试用的存储根是每个用例
	//     独占的临时目录 —— 别的包插进去的行在这个目录里当然"不存在",
	//     全表抽样会把它们全部报成丢失(现象是"单跑必过、全量跑到处是丢失")。
	//     与 lifecycle.RunOnceFor 是同一个教训、同一个解法。
	Scope []string
}

// Report 是一轮巡检的结果。
type Report struct {
	CheckedAt time.Time
	// ---- 正向 ----
	DiskBytes   int64
	DiskFiles   int64
	DBBytes     int64
	DBObjects   int64
	LeakBytes   int64
	LeakObjects int64
	// ForwardSkipped 表示本轮没做正向统计(非本地后端或未装配 FS)
	ForwardSkipped bool
	// ---- 反向 ----
	Sampled     int
	Missing     int
	Unreachable int
	// MissingHashes 是发现丢失的 hash(截断前若干条,便于运维直接去查)
	MissingHashes []string
}

const (
	defaultLocalSample  = 1000
	defaultRemoteSample = 100
	defaultLeakAlert    = 64 << 20
)

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) rand() *rand.Rand {
	if s.Rand != nil {
		return s.Rand
	}
	return rand.New(rand.NewSource(s.now().UnixNano()))
}

// sampleSize 按后端类型返回抽样行数(纪律 2)。
func (s *Service) sampleSize() int {
	backend := ""
	if s.Storage != nil {
		backend = s.Storage.Backend()
	}
	if backend == storage.BackendFS || backend == storage.BackendMem {
		if s.LocalSampleSize > 0 {
			return s.LocalSampleSize
		}
		return defaultLocalSample
	}
	if s.RemoteSampleSize > 0 {
		return s.RemoteSampleSize
	}
	return defaultRemoteSample
}

// RunOnce 执行一轮巡检:正向差值 + 反向抽样。
//
// 正向失败**不影响**反向:两个方向的数据源不同,一个查不动就报一个,
// 另一个照做 —— 因为"磁盘目录读不了"不该顺带让"对象是否还在"也失去观测。
func (s *Service) RunOnce(ctx context.Context) (*Report, error) {
	if s.DB == nil || s.Storage == nil {
		return nil, errors.New("patrol: DB/Storage 未装配")
	}
	rep := &Report{CheckedAt: s.now()}
	var errs []error

	if err := s.forward(ctx, rep); err != nil {
		errs = append(errs, err)
		s.log().Error("对象巡检:正向差值统计失败", "err", err)
	}
	if err := s.reverse(ctx, rep); err != nil {
		errs = append(errs, err)
		s.log().Error("对象巡检:反向抽样失败", "err", err)
	}

	s.report(rep)

	if len(errs) > 0 {
		return rep, errors.Join(errs...)
	}
	return rep, nil
}

// forward 计算"磁盘实际字节 vs PG 未删除对象字节和"(验收①)。
func (s *Service) forward(ctx context.Context, rep *Report) error {
	// PG 侧口径:state <> 'deleted'。
	// 含 pending_delete/deleting(对象**物理上应该还在**,只是进入了延迟窗口)
	// —— 这正是要抓的:延迟删除窗口过去后对象还在盘上而状态已 deleted,
	// 差值就会显现出来。
	//
	// Scope 非空时两边都限定同一组 hash(否则"磁盘只数这几个对象、PG 数全表"
	// 会得到一个毫无意义的巨额负数)。
	if len(s.Scope) > 0 {
		if err := s.DB.QueryRow(ctx, `
SELECT coalesce(sum(size), 0), count(*) FROM file_objects
 WHERE state <> 'deleted' AND hash_sha256 = ANY($1::varchar[])`, s.Scope).
			Scan(&rep.DBBytes, &rep.DBObjects); err != nil {
			return fmt.Errorf("统计对象表失败: %w", err)
		}
	} else if err := s.DB.QueryRow(ctx, `
SELECT coalesce(sum(size), 0), count(*) FROM file_objects WHERE state <> 'deleted'`).
		Scan(&rep.DBBytes, &rep.DBObjects); err != nil {
		return fmt.Errorf("统计对象表失败: %w", err)
	}

	if s.FS == nil || s.Storage.Backend() != storage.BackendFS {
		// 非本地后端:正向统计交给对象存储自身的用量指标,这里如实标注"跳过"
		// 而不是拿 0 去减(会得到"泄漏 = -DBBytes"这种荒谬结论)
		rep.ForwardSkipped = true
		return nil
	}
	files, bytes, err := s.FS.ObjectUsage(ctx)
	if err != nil {
		return fmt.Errorf("统计对象目录失败: %w", err)
	}
	if len(s.Scope) > 0 {
		// 定向模式下磁盘侧也只数范围内的对象:逐个 Stat 累加。
		// (目录整体大小是全量的,与限定范围不可比。)
		files, bytes = 0, 0
		for _, h := range s.Scope {
			if sz, serr := s.Storage.Stat(ctx, h); serr == nil {
				files++
				bytes += sz
			}
		}
	}
	rep.DiskFiles, rep.DiskBytes = files, bytes
	// 不取绝对值、不夹到 0:负值本身是"PG 说有、磁盘没有"的信号(纪律 4)
	rep.LeakBytes = rep.DiskBytes - rep.DBBytes
	rep.LeakObjects = rep.DiskFiles - rep.DBObjects
	return nil
}

// reverse 抽样 `state='live'` 的对象逐个 Stat(验收②③)。
//
// 抽样用**按主键范围随机起步**而不是 `ORDER BY random()`:后者在百万行表上
// 是一次全表扫描 + 排序,而这只是每天一次的巡检,不该把 IO 打满。
// hash 是定长十六进制文本,`>= 随机起点` 走主键索引,取随后的 N 行即可。
func (s *Service) reverse(ctx context.Context, rep *Report) error {
	var hashes []string
	if len(s.Scope) > 0 {
		// 定向模式:不做抽样,逐个查(见 Scope 的说明)
		hashes = append(hashes, s.Scope...)
	} else {
		n := s.sampleSize()
		start := randomHexStart(s.rand())
		picked, err := s.sample(ctx, start, n)
		if err != nil {
			return err
		}
		hashes = picked
		if len(hashes) < n {
			// 起点靠后时尾部不足,从头补齐(保证"库里对象很少"时也能全查一遍)。
			// 共用测试库里行数总是 >= 抽样量,这条分支只在**小库**上真的走到,
			// 因此它的正确性靠代码本身足够简单(3 行)+ 单机小库实测来保证。
			more, merr := s.sample(ctx, "", n-len(hashes))
			if merr != nil {
				return merr
			}
			seen := map[string]bool{}
			for _, h := range hashes {
				seen[h] = true
			}
			for _, h := range more {
				if !seen[h] {
					hashes = append(hashes, h)
				}
			}
		}
	}
	rep.Sampled = len(hashes)

	for _, h := range hashes {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, serr := s.statWithRetry(ctx, h)
		switch {
		case serr == nil:
			// 命中:正常
		case errors.Is(serr, storage.ErrNotFound):
			rep.Missing++
			if len(rep.MissingHashes) < 10 {
				rep.MissingHashes = append(rep.MissingHashes, h)
			}
		default:
			// 不可达:重试后仍失败 → 观测项(纪律 3)
			rep.Unreachable++
		}
	}
	return nil
}

// statWithRetry 在**非** ErrNotFound 的错误上重试:ErrNotFound 是确定性结论,
// 重试它只是浪费(而且会让"丢失"的发现延迟一天);其它错误(e.g. 网络)才值得重试。
func (s *Service) statWithRetry(ctx context.Context, hash string) (int64, error) {
	retries := s.UnreachableRetry
	if retries <= 0 {
		retries = 2
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		size, err := s.Storage.Stat(ctx, hash)
		if err == nil {
			return size, nil
		}
		if errors.Is(err, storage.ErrNotFound) {
			return 0, err
		}
		lastErr = err
		if attempt < retries && s.RetryBackoff > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(s.RetryBackoff):
			}
		}
	}
	return 0, lastErr
}

func (s *Service) sample(ctx context.Context, from string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.DB.Query(ctx, `
SELECT hash_sha256 FROM file_objects
 WHERE state = 'live' AND hash_sha256 >= $1
 ORDER BY hash_sha256
 LIMIT $2`, from, limit)
	if err != nil {
		return nil, fmt.Errorf("抽样对象失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if serr := rows.Scan(&h); serr != nil {
			return nil, serr
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// publish 已移除:指标系统(metrics)在 Server-com 开源版整体拆除。
// 巡检结果的可观测性改由 `report` 的日志告警承担(见下方 report)。

// report 输出人类可读的告警/观测日志。
//
// 分级(与纪律 3 一致):
//   - **丢失** → Error(数据不可读,必须有人看);
//   - **正向差值超阈值** → Error(磁盘只涨不跌);
//   - **不可达** → Warn(重试已失败,先当观测);
//   - 一切正常 → Info(保留一行,用于回答"巡检到底跑没跑")。
func (s *Service) report(rep *Report) {
	log := s.log()
	leakAlert := s.LeakAlertBytes
	if leakAlert <= 0 {
		leakAlert = defaultLeakAlert
	}
	base := []any{
		"sampled", rep.Sampled, "missing", rep.Missing, "unreachable", rep.Unreachable,
		"disk_bytes", rep.DiskBytes, "db_bytes", rep.DBBytes,
		"leak_bytes", rep.LeakBytes, "leak_objects", rep.LeakObjects,
		"forward_skipped", rep.ForwardSkipped,
	}
	if rep.Missing > 0 {
		log.Error("对象巡检:发现**明确不存在**的对象(PG 说在、磁盘没有)"+
			" —— 这是数据不可读,不是占空间",
			append(base, "sample_hashes", rep.MissingHashes)...)
	}
	if !rep.ForwardSkipped && rep.LeakBytes > leakAlert {
		log.Error("对象巡检:正向差值超阈值(磁盘只涨不跌)",
			append(base, "threshold_bytes", leakAlert)...)
	}
	if rep.Unreachable > 0 {
		log.Warn("对象巡检:抽样期间后端不可达(观测项,不判定为丢失)",
			append(base, "retry", s.UnreachableRetry)...)
	}
	if rep.Missing == 0 && (rep.ForwardSkipped || rep.LeakBytes <= leakAlert) && rep.Unreachable == 0 {
		log.Info("对象巡检完成:未发现泄漏与丢失", base...)
	}
}

// randomHexStart 生成一个随机的 64 位十六进制起点(与 hash_sha256 定长对齐)。
//
// 用定长随机起点而不是 `ORDER BY random()`:前者走主键索引(O(log n)),
// 后者是百万行表的全扫 + 排序 —— 巡检是后台任务,不该和用户请求抢 IO。
func randomHexStart(r *rand.Rand) string {
	const hexDigits = "0123456789abcdef"
	b := make([]byte, 64)
	for i := range b {
		b[i] = hexDigits[r.Intn(len(hexDigits))]
	}
	return string(b)
}
