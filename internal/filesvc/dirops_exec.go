package filesvc

import (
	"context"
	"errors"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/repo"
)

// 目录级异步任务的**执行体**(BE-S7-02 / 6.11)。
//
// 两件事执行体刻意**不做**:
//
//  1. 不做阈值判断 —— 它被调用的前提就是"已经判定要异步";
//  2. 不做权限判断 —— 权限在**入队时**已经判过,而任务执行可能发生在几分钟后,
//     那时用户的成员关系可能已经变了。这里的取舍是显式的:
//     **按"入队时已授权"执行**,而不是执行前重新判权。
//     反过来的话,一个已经被用户确认(且可能已经删掉一半)的操作会在最后关头
//     失败,用户面对的是"任务失败,但目录已经变了一半" —— 比"任务完成"更糟。
//     被移出空间后的可见性由读取路径兜住(410 space_revoked),不靠任务重判。
//
// 执行体与同步路径共用 moveSubtree / deleteSubtree,于是三条纪律
// (全局锁序、聚合事件、幂等)只有一份实现,不会出现"同步对、异步错"。

// ExecDirMove 执行一个异步移动任务。
func (s *Service) ExecDirMove(ctx context.Context, t *dirops.Task) (dirops.Outcome, error) {
	if t.Kind != dirops.KindMove {
		return dirops.Outcome{}, &dirops.ExecError{Code: "task_kind_mismatch",
			Message: "任务类型不是 move"}
	}
	cur, err := s.Files.GetByID(ctx, s.DB, t.FileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 任务执行前该行已被删除(用户在另一个客户端删了它)
			return dirops.Outcome{}, &dirops.ExecError{Code: "file_gone",
				Message: "条目已不存在", Err: err}
		}
		return dirops.Outcome{}, err
	}
	// 幂等:租约超时重试时上一次可能已经移到位。这时直接成功 ——
	// 否则用户会看到一个"失败"的任务,而目录其实已经移好了。
	if cur.ParentID == t.NewParentID && cur.Name == t.NewName {
		return dirops.Outcome{}, nil
	}
	if _, err := s.moveSubtree(ctx, cur, MoveInput{
		UserID: t.UserID, FileID: t.FileID, NewParentID: t.NewParentID,
	}, t.NewName); err != nil {
		return dirops.Outcome{}, asExecError(err)
	}
	rows, rerr := s.Files.SubtreeStatsOf(ctx, s.DB, t.FileID)
	if rerr != nil {
		// 统计失败不影响"操作已经成功"这个事实,只记一笔
		s.log().Warn("filesvc: 异步移动任务完成后统计子树失败",
			"task_id", t.ID, "file_id", t.FileID, "err", rerr)
		return dirops.Outcome{}, nil
	}
	return dirops.Outcome{RowsAffected: rows.FileCount + rows.DirCount}, nil
}

// ExecDirDelete 执行一个异步删除任务。
func (s *Service) ExecDirDelete(ctx context.Context, t *dirops.Task) (dirops.Outcome, error) {
	if t.Kind != dirops.KindDelete {
		return dirops.Outcome{}, &dirops.ExecError{Code: "task_kind_mismatch",
			Message: "任务类型不是 delete"}
	}
	cur, err := s.Files.GetByID(ctx, s.DB, t.FileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 行已经不在 → 幂等成功(deleteSubtree 也会走同一条分支)
			return dirops.Outcome{}, nil
		}
		return dirops.Outcome{}, err
	}
	res, err := s.deleteSubtree(ctx, cur)
	if err != nil {
		return dirops.Outcome{}, asExecError(err)
	}
	return dirops.Outcome{
		RowsAffected:    res.DeletedFiles + res.DeletedDirs,
		FreedBytes:      res.FreedBytes,
		ReleasedObjects: res.ReleasedObjects,
	}, nil
}

// asExecError 把 apierr 的稳定业务码带进任务行,供轮询方按 code 分流。
//
// 不把 500/内部错误也算"稳定码":那类失败会以 internal + 原始文案落库,
// 而客户端对它们的正确反应是"稍后重试",不是"按码分支"。
func asExecError(err error) error {
	var ae *apierr.Error
	if errors.As(err, &ae) {
		// 只有 4xx 才是"用户可据以行动"的稳定结论;5xx(内部错误/超时)保留原始
		// 错误,由 worker 记 full error 到日志 —— 把它们也当稳定码会让客户端
		// 对一个临时故障做出永久性判断(例如"这个目录不能移动")。
		if ae.Status >= 400 && ae.Status < 500 {
			return &dirops.ExecError{Code: string(ae.Code), Message: ae.Message, Err: err}
		}
	}
	return err
}
