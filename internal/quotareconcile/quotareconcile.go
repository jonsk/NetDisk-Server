// Package quotareconcile 实现**配额对账与漂移告警**(4.3 / 6.6 / R-16,BE-S3-06 ⑤)。
//
// # 为什么需要对账
//
// `spaces.used_bytes` 由写事务增减(预留 / 结算 / 释放),而权威事实是
// `files` 的实时 SUM 加在飞预留。两条路径**理应永远一致**(都在事务里),
// 所以任何漂移都是 bug 信号 —— 这正是 4.3 要把它做成告警的原因:
// 它不是"需要容忍的误差",而是"某个写路径漏了一次增减"的唯一可观测证据。
//
// 两个方向的后果不对称,必须分开看:
//
//   - **正漂移(账面 > 实际)**:用户明明没占用那么多却放不进来 —— 表现为
//     "删了文件还是提示容量不足",支持工单里最难查的一类;
//   - **负漂移(账面 < 实际)**:**超额使用**(用户占用了不算钱的字节),
//     直接损失容量,比正漂移严重。
//
// # 口径必须只有一份
//
// 期望值 = `sum(files.size)` + `sum(uploads.declared_size WHERE state='reserved')`。
// 第二项不能漏:`used_bytes` 是"已占用 + 已预留"的合计(建任务即预留,防超卖),
// 少了它,每个正在上传的空间都会被判成漂移 —— 而对账一旦回写就会**抹掉正在
// 上传的预留**,正好打开预留机制要堵的洞(额度超卖)。
//
// 口径由 `repo.expectedUsedSQL` 单点维护,检测与回写共用同一段 SQL。
package quotareconcile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/netdisk/netdisk/internal/repo"
)

// DefaultAlertBytes 是漂移告警/回写的默认阈值(1MiB)。
//
// 阈值的作用不是"容忍误差"(漂移就是 bug),而是**避免告警风暴**:
// 一次批量删除可能让多个空间同时出现瞬时偏差,而真正要人看的是"持续且明显"
// 的漂移。想对任何漂移告警,把它设成 1。
const DefaultAlertBytes int64 = 1 << 20

// Service 是对账任务。
type Service struct {
	Spaces repo.SpaceRepo
	DB     repo.Querier
	Log    *slog.Logger
	// AlertBytes 超过它就告警(默认 1MiB)
	AlertBytes int64
	// AutoFix 为真时按 4.3 回写漂移(默认由装配方决定;单独跑诊断时应为 false)
	AutoFix bool
	// Batch 是单轮最多处理的漂移空间数(默认 200)
	Batch int
	// Scope 限定对账范围(为空 = 全部空间)。
	//
	// 生产为空;测试必须传自己的空间 —— `spaces` 是全局共享表而对账是全局动作,
	// 没有范围限定的用例会顺手改掉并发运行的其它包的空间(实测过)。
	// 运维侧也用它做"只对账某几个空间"的定向排查。
	Scope []string
}

// Report 是一轮对账的结果。
type Report struct {
	ScanLimit    int
	Drifted      int
	Fixed        int
	Alerted      int
	MaxAbsDelta  int64
	TotalDelta   int64
	PositiveOnly int
	NegativeOnly int
	// Samples 是前几个漂移空间的明细(便于日志直接定位)
	Samples []repo.Drift
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func (s *Service) alertBytes() int64 {
	if s.AlertBytes > 0 {
		return s.AlertBytes
	}
	return DefaultAlertBytes
}

func (s *Service) batch() int {
	if s.Batch > 0 {
		return s.Batch
	}
	return 200
}

// RunOnce 执行一轮对账。
//
// 顺序刻意是 **先读 → 告警 → 条件回写**:回写用一条带条件的 UPDATE
// (`abs(漂移) > 阈值`),因此"读取到回写"之间若恰好有新的预留进来,
// 这次回写会落空而不是把预留抹掉。
func (s *Service) RunOnce(ctx context.Context) (*Report, error) {
	if s.DB == nil {
		return nil, errors.New("quotareconcile: DB 未装配")
	}
	drifts, err := s.Spaces.ListDrifted(ctx, s.DB, s.batch(), s.Scope)
	if err != nil {
		return nil, fmt.Errorf("列出漂移空间失败: %w", err)
	}

	rep := &Report{ScanLimit: s.batch(), Drifted: len(drifts)}
	threshold := s.alertBytes()
	var fixErrs []error

	for _, d := range drifts {
		if ctx.Err() != nil {
			return rep, ctx.Err()
		}
		delta := d.Delta()
		if delta > 0 {
			rep.PositiveOnly++
		} else if delta < 0 {
			rep.NegativeOnly++
		}
		rep.TotalDelta += delta
		if abs64(delta) > rep.MaxAbsDelta {
			rep.MaxAbsDelta = abs64(delta)
		}
		if len(rep.Samples) < 5 {
			rep.Samples = append(rep.Samples, d)
		}
		if abs64(delta) <= threshold {
			continue
		}

		rep.Alerted++
		// 负漂移是超额使用(容量在流失),给 Error;正漂移是"用户被多算",给 Warn。
		msg := "配额对账:used_bytes 与权威口径不一致(账面 > 实际,用户可能被多算)"
		lvl := slog.LevelWarn
		if delta < 0 {
			msg = "配额对账:used_bytes 低于实际占用(**超额使用**,容量在流失)"
			lvl = slog.LevelError
		}
		s.log().Log(ctx, lvl, msg,
			"space_id", d.SpaceID, "stored", d.Stored, "expected", d.Expected,
			"delta", delta, "threshold", threshold, "autofix", s.AutoFix)

		if !s.AutoFix {
			continue
		}
		fixed, ferr := s.Spaces.SetUsed(ctx, s.DB, d.SpaceID, threshold)
		if ferr != nil {
			fixErrs = append(fixErrs, fmt.Errorf("回写空间 %s 失败: %w", d.SpaceID, ferr))
			continue
		}
		if fixed {
			rep.Fixed++
		}
	}

	s.publish(rep)
	if len(fixErrs) > 0 {
		return rep, errors.Join(fixErrs...)
	}
	return rep, nil
}

func (s *Service) publish(_ *Report) {}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
