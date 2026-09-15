package uploadsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// 本文件是**暂存区回收**(BE-S5-04;按 6.2 V2.14 后的口径,
// 原清单的"TUS 合并 worker"已不存在 —— 分片是追加写同一个文件、到齐即定稿,
// "合并"这一步消失了,剩下的是"暂存文件与过期预留的回收")。
//
// 三件事,以及每件为什么必须这么做:
//
//  1. **过期预留的回收不能只靠客户端取消**。客户端崩溃/断网/关页面时不会有
//     DELETE /upload/{id};而预留占的是**用户额度**(4.3),不回收就是"用户什么
//     都没传成,额度却少了 10GB"。所以这条兜底路径必须存在并真的在跑。
//  2. **暂存文件清理要先确认任务真的死了**。只按文件年龄删会误删**正在续传**的
//     大文件(用户传了 20 小时、最后一小时被清理 → 断点续传从头再来)。
//     所以判定是"文件够老 **且** 对应任务不是活跃的 reserved"。
//  3. **删除失败要重试,但不能无限重试**。删除失败通常是权限/占用问题,
//     重试 3 次仍失败就置错误态并留下 ERROR 日志 —— 无限重试会刷满日志、
//     掩盖真正的问题;而静默放弃会让暂存盘慢慢被填满(最后表现为"磁盘满")。

// StageStore 是暂存区回收所需的最小能力(由 storage.FS 实现)。
type StageStore interface {
	ListStageFiles() ([]storage.StageFile, error)
	StageRemove(uploadID string) error
}

// StageCleaner 回收过期预留与陈旧暂存文件。
type StageCleaner struct {
	Stager  StageStore
	Uploads repo.UploadRepo
	Spaces  repo.SpaceRepo
	DB      DB
	// TTL 是暂存文件的保留时长(默认 24h,与 uploads 的 ticket TTL 同窗,6.1)
	TTL time.Duration
	// Batch 是单轮处理上限(避免一轮扫完整个目录把 IO 打满)
	Batch int
	// MaxAttempts 是单个文件的删除重试上限(默认 3)
	MaxAttempts int
	Now         func() time.Time
	Log         *slog.Logger
	// attempts 记录同一文件的连续失败次数(进程内)。
	//
	// 刻意不落库:暂存文件没有自己的表行,为它加一张表只为记重试次数不值;
	// 而"进程重启后重新计一次"对本操作是无害的(删除是幂等的)。
	attempts map[string]int
}

// StageCleanResult 是一轮清理的结果(可直接进监控指标)。
type StageCleanResult struct {
	// Scanned 是本轮看到的暂存文件数
	Scanned int
	// RemovedStages / FreedBytes 是被清掉的文件与其字节数
	RemovedStages int
	FreedBytes    int64
	// KeptYoung 是"还没到清理窗口"而保留的
	KeptYoung int
	// KeptActive 是"任务还活着(可能是正在续传)"而保留的
	KeptActive int
	// ReclaimedReservations / ReclaimedBytes 是回收的过期预留
	ReclaimedReservations int
	ReclaimedBytes        int64
	// Failed 是本轮放弃的文件数(连续失败达到 MaxAttempts)
	Failed int
	// Errors 是本轮遇到的错误数(不中断整轮)
	Errors int
}

func (c *StageCleaner) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return 24 * time.Hour
}

func (c *StageCleaner) batch() int {
	if c.Batch > 0 {
		return c.Batch
	}
	return 500
}

func (c *StageCleaner) maxAttempts() int {
	if c.MaxAttempts > 0 {
		return c.MaxAttempts
	}
	return 3
}

func (c *StageCleaner) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *StageCleaner) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// RunOnce 跑一轮回收,返回统计。**不返回错误**是刻意的:
//
// 后台巡视任务里"某一项失败"不该让整轮看起来失败 —— 否则监控上只会看到
// "清理任务报错",而真正需要知道的是"3 个文件删不掉"。失败计数在结果里。
func (c *StageCleaner) RunOnce(ctx context.Context) StageCleanResult {
	var res StageCleanResult
	if c.Stager == nil || c.DB == nil {
		return res
	}
	c.reclaimExpiredReservations(ctx, &res)
	c.cleanStages(ctx, &res)
	return res
}

// reclaimExpiredReservations 原子认领过期预留并回退额度(见 repo.ClaimExpiredReleases)。
func (c *StageCleaner) reclaimExpiredReservations(ctx context.Context, res *StageCleanResult) {
	claimed, err := c.Uploads.ClaimExpiredReleases(ctx, c.DB, c.batch())
	if err != nil {
		c.log().Error("回收过期预留失败", "err", err)
		res.Errors++
		return
	}
	for _, cl := range claimed {
		// 额度回退独立成事务:一条回退失败不该让其余已认领的行"既不释放额度
		// 也不归还额度"。已置 released 的行不会再被认领,所以这里失败要显式报错。
		if rerr := c.DB.InTx(ctx, func(tx pgx.Tx) error {
			return c.Spaces.Release(ctx, tx, cl.SpaceID, cl.DeclaredSize)
		}); rerr != nil {
			c.log().Error("回退过期预留言失败", "upload", cl.UploadID, "space", cl.SpaceID,
				"bytes", cl.DeclaredSize, "err", rerr)
			res.Errors++
			continue
		}
		res.ReclaimedReservations++
		res.ReclaimedBytes += cl.DeclaredSize
		// 任务已死,它的暂存文件也该清 —— 直接删,不等年龄判定
		if rerr := c.Stager.StageRemove(cl.UploadID); rerr == nil {
			res.RemovedStages++
		}
		delete(c.attempts, cl.UploadID)
	}
}

// cleanStages 清理"够老且任务已死"的暂存文件。
func (c *StageCleaner) cleanStages(ctx context.Context, res *StageCleanResult) {
	files, err := c.Stager.ListStageFiles()
	if err != nil {
		c.log().Error("列出暂存文件失败", "err", err)
		res.Errors++
		return
	}
	cutoff := c.now().Add(-c.ttl())
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		res.Scanned++
		if f.ModTime.After(cutoff) {
			res.KeptYoung++
			continue
		}
		// 文件够老了,但任务可能还在传(大文件跨天续传是正常路径)
		active, aerr := c.uploadActive(ctx, f.UploadID)
		if aerr != nil {
			c.log().Error("查上传任务状态失败", "upload", f.UploadID, "err", aerr)
			res.Errors++
			continue
		}
		if active {
			res.KeptActive++
			continue
		}
		if rerr := c.Stager.StageRemove(f.UploadID); rerr != nil {
			if errors.Is(rerr, storage.ErrNotFound) {
				// 已经被别人删掉了:当作成功(幂等)
				delete(c.attempts, f.UploadID)
				c.afterRemove(res, f)
				continue
			}
			n := c.bumpAttempt(f.UploadID)
			if n >= c.maxAttempts() {
				// 重试到顶:留一条 ERROR 让人能查到"哪个文件删不掉、为什么"
				c.log().Error("暂存文件删除连续失败,已放弃本轮", "upload", f.UploadID,
					"attempts", n, "err", rerr)
				res.Failed++
			} else {
				c.log().Warn("暂存文件删除失败,将重试", "upload", f.UploadID,
					"attempt", n, "err", rerr)
			}
			res.Errors++
			continue
		}
		delete(c.attempts, f.UploadID)
		c.afterRemove(res, f)
	}
}

func (c *StageCleaner) afterRemove(res *StageCleanResult, f storage.StageFile) {
	res.RemovedStages++
	res.FreedBytes += f.Size
}

// bumpAttempt 累加某文件的连续失败次数(懒初始化 map)。
//
// 必须懒初始化:`StageCleaner` 是**值语义**的结构体,装配方(生产 main 与测试)
// 都会直接写字面量构造 —— 一旦要求"必须记得初始化 attempts",漏掉的那次
// 会在第一次删除失败时 panic(nil map 赋值),而这条路径只在"删除失败"时才走到,
// 于是它是一个**平时测不出来的崩溃**。
func (c *StageCleaner) bumpAttempt(uploadID string) int {
	if c.attempts == nil {
		c.attempts = make(map[string]int)
	}
	c.attempts[uploadID]++
	return c.attempts[uploadID]
}

// uploadActive 判断暂存文件对应的任务是否**仍在活跃预留中**。
//
// 三种情况:
//   - 查不到任务行:孤儿暂存文件(建任务后回滚/手工清理)→ 可删
//   - state='reserved' 且未过期:正在上传(可能正在续传)→ **保留**
//   - 其它状态(finalized/released/failed)或已过期:任务已结束 → 可删
func (c *StageCleaner) uploadActive(ctx context.Context, uploadID string) (bool, error) {
	var state string
	var expires time.Time
	err := c.DB.QueryRow(ctx, `SELECT state, expires_at FROM uploads WHERE id = $1`, uploadID).
		Scan(&state, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		// upload_id 不是合法 uuid(手工塞进来的文件):当作孤儿,可删
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "22P02" {
			return false, nil
		}
		return false, fmt.Errorf("查上传任务失败: %w", err)
	}
	if state != "reserved" {
		return false, nil
	}
	return expires.After(c.now()), nil
}
