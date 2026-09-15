package filesvc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/repo"
)

// 编辑锁(REST 锁,BE-S6-05 / 6.2 / 6.3):替代 WebDAV LOCK。
//
// # 为什么是 REST 锁而不是 WebDAV LOCK
//
// `x/net/webdav` 的锁是**内存实现**(NewMemLS):进程重启即失、多实例不共享 ——
// 用它保护编辑等于"锁随时可能消失,而客户端以为还锁着"。REST 锁落在 PG
// (`file_locks` 表,迁移 00001 已建),因此重启、多实例都一致。
//
// # 三条语义纪律(每一条写错都表现为"锁形同不存在"或"锁死用户")
//
//  1. **只拦"覆盖写",不拦移动/删除**。锁的用途是"我正在编辑,请勿在他处写入",
//     而重命名/移动/删除是**所有者级别**的操作(用户自己整理文件)—— 拦住它们
//     会让"锁着的时候连改名都不行",而这类操作本身不会损坏编辑中的内容。
//  2. **过期即失效,且可被抢占**。30 分钟没续期就认为持锁者已放弃(客户端崩溃、
//     断电都不发释放请求)。不做过期会让文件被**永久锁死**,而"谁持有锁"这件事
//     没有任何人工解锁入口(那才是真正的故障)。
//  3. **释放幂等**。客户端会在"关闭文档"与"崩溃恢复"两条路径上都发释放;
//     释放一个不存在/已过期的锁必须成功(报错会让客户端卡在"释放失败"的重试里)。
//     但释放**别人的**锁必须拒绝:否则任何人都能解锁别人的编辑会话。
const (
	// DefaultLockTTL 是编辑锁有效期(6.3:30min)
	DefaultLockTTL = 30 * time.Minute
)

// LockView 是编辑锁的对外视图。
type LockView struct {
	FileID string `json:"file_id"`
	// Holder 是持锁者(用户 id);客户端据此显示"正在被他人编辑"
	Holder string `json:"holder_user_id"`
	// Mine 表示是不是当前调用者持有的
	Mine bool `json:"mine"`
	// ExpiresAt 让客户端知道何时需要续期
	ExpiresAt string `json:"expires_at"`
}

// AcquireLock 获取/续期编辑锁。
//
// 语义:
//   - 无人持锁或**已过期** → 取得锁(过期可抢占,见纪律 2)
//   - 自己持有 → 续期(同一客户端的重复获取不该失败)
//   - 他人持有且未过期 → **409** + 持有者信息(客户端据此提示"XX 正在编辑")
func (s *Service) AcquireLock(ctx context.Context, userID, fileID string) (*LockView, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	f, err := s.Files.GetByID(ctx, s.DB, fileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkWrite(ctx, f.SpaceID, userID); err != nil {
		return nil, err
	}
	if f.IsDir {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "目录不能加编辑锁")
	}
	ttl := s.lockTTL()
	var holder string
	var expires time.Time
	// **用一条 UPSERT 完成"抢占 + 续期"**:先读再写的写法在并发下会让两个请求
	// 都认为"没人持锁",于是两人同时拿到锁 —— 而锁的全部意义就是排除这种情况。
	err = s.DB.QueryRow(ctx, `
INSERT INTO file_locks (file_id, user_id, expires_at)
VALUES ($1, $2, now() + $3::interval)
ON CONFLICT (file_id) DO UPDATE
   SET user_id = EXCLUDED.user_id, expires_at = EXCLUDED.expires_at
 WHERE file_locks.expires_at <= now() OR file_locks.user_id = EXCLUDED.user_id
RETURNING user_id::text, expires_at`,
		fileID, userID, fmt.Sprintf("%d seconds", int64(ttl.Seconds()))).Scan(&holder, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT 的 WHERE 不成立 = 别人持锁且未过期
			var h string
			var e time.Time
			if qerr := s.DB.QueryRow(ctx,
				`SELECT user_id::text, expires_at FROM file_locks WHERE file_id = $1`, fileID).
				Scan(&h, &e); qerr == nil {
				ae := apierr.Conflict(apierr.CodeVersionConflict, "该文件正被他人编辑")
				ae.WithDetail("reason", "locked")
				ae.WithDetail("holder_user_id", h)
				ae.WithDetail("expires_at", e.UTC().Format("2006-01-02T15:04:05.000Z"))
				return nil, ae
			}
			return nil, apierr.Conflict(apierr.CodeVersionConflict, "该文件已被锁定")
		}
		return nil, apierr.Internal(err)
	}
	return &LockView{
		FileID: fileID, Holder: holder, Mine: holder == userID,
		ExpiresAt: expires.UTC().Format("2006-01-02T15:04:05.000Z"),
	}, nil
}

// ReleaseLock 释放编辑锁(**幂等**)。
//
//	不存在 / 已过期 → 成功(客户端重试关闭流程不该失败)
//	他人持有        → 403(任何人都能解锁别人的编辑会话是更严重的问题)
func (s *Service) ReleaseLock(ctx context.Context, userID, fileID string) error {
	if s.DB == nil {
		return apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	var holder string
	var expires time.Time
	err := s.DB.QueryRow(ctx,
		`SELECT user_id::text, expires_at FROM file_locks WHERE file_id = $1`, fileID).
		Scan(&holder, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // 幂等:没有锁就是"已经释放"
		}
		return apierr.Internal(err)
	}
	if holder != userID {
		// 已过期的锁:允许任何人清理(它已经不再保护任何东西,
		// 而"过期锁删不掉"会让下一个人拿到锁时看到脏数据)
		if expires.After(time.Now()) {
			return apierr.Forbidden("该锁由他人持有,不能释放")
		}
	}
	if _, derr := s.DB.Exec(ctx, `DELETE FROM file_locks WHERE file_id = $1`, fileID); derr != nil {
		return apierr.Internal(derr)
	}
	return nil
}

// GetLock 读当前锁(无锁返回 nil,不报错)。
func (s *Service) GetLock(ctx context.Context, userID, fileID string) (*LockView, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	var holder string
	var expires time.Time
	err := s.DB.QueryRow(ctx,
		`SELECT user_id::text, expires_at FROM file_locks
		  WHERE file_id = $1 AND expires_at > now()`, fileID).Scan(&holder, &expires)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, apierr.Internal(err)
	}
	return &LockView{
		FileID: fileID, Holder: holder, Mine: holder == userID,
		ExpiresAt: expires.UTC().Format("2006-01-02T15:04:05.000Z"),
	}, nil
}

// CheckLockForOverwrite 判断"是否允许对某文件做覆盖写"。
//
// **只有覆盖写路径调用它**(finalize 的 AllowOverwrite、WebDAV PUT);
// 移动/删除/改名**不**调用(纪律 1)。
//
// 返回 nil 表示无锁或锁是自己或锁已过期 —— 三种情况都放行,因为锁的作用是
// "排除他人在我编辑期间写入",而不是"文件必须由我独占"。
func (s *Service) CheckLockForOverwrite(ctx context.Context, userID, fileID string) error {
	lk, err := s.GetLock(ctx, userID, fileID)
	if err != nil {
		return err
	}
	if lk == nil || lk.Mine {
		return nil
	}
	ae := apierr.Conflict(apierr.CodeVersionConflict, "该文件正被 %s 编辑,请稍后再试或另存为", lk.Holder)
	ae.WithDetail("reason", "locked")
	ae.WithDetail("holder_user_id", lk.Holder)
	ae.WithDetail("expires_at", lk.ExpiresAt)
	return ae
}

// CleanupLocks 清理过期锁行(后台任务:过期判断本来就靠 expires_at,
// 这里只是防止表无限增长 —— 每个被锁过的文件都留一行)。
func (s *Service) CleanupLocks(ctx context.Context, olderThan time.Duration) (int64, error) {
	if s.DB == nil {
		return 0, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	tag, err := s.DB.Exec(ctx,
		`DELETE FROM file_locks WHERE expires_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
	if err != nil {
		return 0, apierr.Internal(err)
	}
	return tag.RowsAffected(), nil
}

// LockTTL 暴露有效期(便于 handler 回显"多久后需要续期")。
func (s *Service) LockTTL() time.Duration { return s.lockTTL() }

func (s *Service) lockTTL() time.Duration {
	if s.LockTTLSeconds > 0 {
		return time.Duration(s.LockTTLSeconds) * time.Second
	}
	return DefaultLockTTL
}
