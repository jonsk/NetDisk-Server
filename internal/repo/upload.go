package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
)

// UploadRepo 上传任务与额度预留载体(4.3 / 6.10 / 6.3)。
//
// 设计要点:
//   - 预留账本**落在 uploads 行**上(而不是 Redis),避免 Redis 重启丢账(10.7)
//   - ticket 只存哈希(ticket_hash),明文只在签发那一刻返回给客户端
type UploadRepo struct{}

const uploadColumns = `id, user_id, space_id, coalesce(parent_id::text,''), name,
	declared_size, coalesce(declared_hash,''), coalesce(actual_size,0), coalesce(actual_hash,''),
	state, coalesce(target_file_id::text,''), ticket_hash, uploaded_bytes, allow_overwrite, expires_at, created_at, updated_at`

func scanUpload(row pgx.Row) (*model.Upload, error) {
	var u model.Upload
	if err := row.Scan(&u.ID, &u.UserID, &u.SpaceID, &u.ParentID, &u.Name,
		&u.DeclaredSize, &u.DeclaredHash, &u.ActualSize, &u.ActualHash,
		&u.State, &u.TargetFileID, &u.TicketHash, &u.UploadedBytes, &u.AllowOverwrite,
		&u.ExpiresAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// CreateInput 创建上传任务(预留阶段)。
type CreateUploadInput struct {
	UserID       string
	SpaceID      string
	ParentID     string
	Name         string
	DeclaredSize int64
	DeclaredHash string
	TicketHash   string
	// AllowOverwrite 透传"覆盖同名文件"的意图(默认 false)
	AllowOverwrite bool
	// ExpiresAt 由调用方按**自己的时钟**算好传入,而不是让 SQL 用 now() 加 TTL。
	//
	// 原因:服务端要在响应里告诉客户端"凭据何时失效",若这里用数据库 now()、
	// 响应却用另一个来源的时间,两边时钟一有偏差(测试用假时钟、或多实例时钟漂移)
	// 就会出现"响应说到期时间 X,库里却是 Y" —— 客户端据此提前放弃或过期后才重试。
	// 零值表示用数据库 now() + TTL(仅为兼容默认行为)。
	ExpiresAt time.Time
	TTL       time.Duration
}

// Create 落一条 reserved 状态的任务;调用方需已在同一事务内完成额度预留(4.3)。
func (UploadRepo) Create(ctx context.Context, q Querier, in CreateUploadInput) (*model.Upload, error) {
	if in.ExpiresAt.IsZero() {
		if in.TTL <= 0 {
			in.TTL = 24 * time.Hour // 与 6.1 临时文件清理窗口一致
		}
		return scanUpload(q.QueryRow(ctx,
			`INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, declared_hash, ticket_hash, expires_at, allow_overwrite)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, now() + $8::interval, $9)
			 RETURNING `+uploadColumns,
			in.UserID, in.SpaceID, nullableUUID(in.ParentID), in.Name,
			in.DeclaredSize, nullableHash(in.DeclaredHash), in.TicketHash,
			formatSeconds(in.TTL), in.AllowOverwrite))
	}
	return scanUpload(q.QueryRow(ctx,
		`INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, declared_hash, ticket_hash, expires_at, allow_overwrite)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING `+uploadColumns,
		in.UserID, in.SpaceID, nullableUUID(in.ParentID), in.Name,
		in.DeclaredSize, nullableHash(in.DeclaredHash), in.TicketHash, in.ExpiresAt, in.AllowOverwrite))
}

// GetByID 取上传任务(定稿幂等与 ticket 校验都走这里)。
func (UploadRepo) GetByID(ctx context.Context, q Querier, id string) (*model.Upload, error) {
	return scanUpload(q.QueryRow(ctx, `SELECT `+uploadColumns+` FROM uploads WHERE id = $1`, id))
}

// Finalize 把任务置为已定稿并登记目标 file_id(6.10 幂等:重复调用返回既有行)。
//
// 用条件 UPDATE 保证**并发双 finish 只有一个成功**:
// 第二个调用因 state 已不是 reserved 而返回 0 行 → 由调用方读取既有 target_file_id。
func (UploadRepo) Finalize(ctx context.Context, q Querier, id, actualHash string, actualSize int64, fileID string) (*model.Upload, error) {
	u, err := scanUpload(q.QueryRow(ctx,
		`UPDATE uploads
		 SET state = 'finalized', actual_hash = $2, actual_size = $3, target_file_id = $4, updated_at = now()
		 WHERE id = $1 AND state = 'reserved'
		 RETURNING `+uploadColumns, id, actualHash, actualSize, fileID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrConflict // 已被并发定稿或已释放
		}
		return nil, err
	}
	return u, nil
}

// Release 取消上传并释放预留(6.3:取消/过期即时释放,不等 24h 回收班车)。
func (UploadRepo) Release(ctx context.Context, q Querier, id string) (*model.Upload, error) {
	u, err := scanUpload(q.QueryRow(ctx,
		`UPDATE uploads SET state = 'released', updated_at = now()
		 WHERE id = $1 AND state = 'reserved'
		 RETURNING `+uploadColumns, id))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return u, nil
}

// GetActiveByName 查同目录下是否已有**进行中**的同名上传任务(6.7 冲突检测)。
//
// 为什么需要它:files 行要到定稿(finalize)才出现,所以"只看 files"会漏掉
// "两个客户端同时上传同名文件"这种情况 —— 两者都能建任务、都能传完,
// 最后第二个在定稿时才失败,用户白传了整个文件。
//
// 名称比较与唯一索引 (space_id, parent_id, lower(name)) 口径一致。
// 只认 reserved 状态:已取消/已失败/已定稿的任务不占名(定稿后由 files 行接管)。
func (UploadRepo) GetActiveByName(ctx context.Context, q Querier, spaceID, parentID, name string) (*model.Upload, error) {
	return scanUpload(q.QueryRow(ctx,
		`SELECT `+uploadColumns+` FROM uploads
		 WHERE space_id = $1 AND parent_id = $2 AND lower(name) = lower($3)
		   AND state = 'reserved'
		 ORDER BY created_at
		 LIMIT 1`, spaceID, nullableUUID(parentID), name))
}

// SetUploaded 单调推进已落盘字节数(6.10 TUS 断点续传)。
//
// 两个关键约束都在 SQL 里表达,而不是靠调用方自觉:
//   - `uploaded_bytes < $2`:只允许**前进**,重复或乱序到达的分片不会把进度写回去;
//   - `state = 'reserved'`:已定稿/已释放的任务不再接受数据。
//
// 返回 ErrConflict 表示该分片是重复的或任务已结束(调用方据此返回 204 幂等)。
func (UploadRepo) SetUploaded(ctx context.Context, q Querier, id string, uploaded int64) (*model.Upload, error) {
	if uploaded < 0 {
		return nil, fmt.Errorf("repo: uploaded 不能为负")
	}
	u, err := scanUpload(q.QueryRow(ctx,
		`UPDATE uploads SET uploaded_bytes = $2, updated_at = now()
		 WHERE id = $1 AND state = 'reserved' AND uploaded_bytes < $2
		 RETURNING `+uploadColumns, id, uploaded))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return u, nil
}

// MarkFailed 标记失败(合并失败等),额度由调用方释放。
func (UploadRepo) MarkFailed(ctx context.Context, q Querier, id string) error {
	_, err := q.Exec(ctx,
		`UPDATE uploads SET state = 'failed', updated_at = now() WHERE id = $1 AND state = 'reserved'`, id)
	return err
}

// ListExpiredReserved 扫描过期未定稿的任务(定时任务据此释放预留,兜底路径)。
func (UploadRepo) ListExpiredReserved(ctx context.Context, q Querier, limit int) ([]*model.Upload, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.Query(ctx,
		`SELECT `+uploadColumns+` FROM uploads
		 WHERE state = 'reserved' AND expires_at < now()
		 ORDER BY expires_at
		 LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ExpiredRelease 是一条"已认领并释放"的过期预留所对应的额度回退。
type ExpiredRelease struct {
	UploadID     string
	SpaceID      string
	DeclaredSize int64
}

// ClaimExpiredReleases **原子认领**过期未定稿的任务并置为 released,返回需要回退的额度。
//
// 与 ListExpiredReserved + 逐个 Release 的差别(为什么必须换掉):
//   - 后者是"先读一批 id、再逐个条件更新" —— 两个 worker 同时跑会读到同一批,
//     其中一方 UPDATE 匹配 0 行而空转(幂等,但白跑);更糟的是**配额回退**
//     依赖"Release 是否成功"来判定,一旦有人把判定写错就会**重复回退额度**
//     (空间 used_bytes 被减两次,用户凭空多出容量,而没有任何报错)。
//   - 这里用 `FOR UPDATE SKIP LOCKED` 在**一条语句**里认领:并发的两个 worker
//     各自拿到**不相交**的行,且"置 state"与"拿到额度账本"是同一次原子操作 ——
//     谁拿到返回行谁就负责回退,不可能重复。
//
// SKIP LOCKED 而不是等待:清理是后台兜底任务,遇到被别人锁住的行应当跳过、
// 下一轮再来,而不是堵在那里(release 只在毫秒级,但堵住会让整批清理停摆)。
func (UploadRepo) ClaimExpiredReleases(ctx context.Context, q Querier, limit int) ([]ExpiredRelease, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.Query(ctx, `
UPDATE uploads SET state = 'released', updated_at = now()
 WHERE id IN (
   SELECT id FROM uploads
    WHERE state = 'reserved' AND expires_at < now()
    ORDER BY expires_at
    LIMIT $1
    FOR UPDATE SKIP LOCKED
 )
 RETURNING id::text, space_id::text, declared_size`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExpiredRelease
	for rows.Next() {
		var r ExpiredRelease
		if err := rows.Scan(&r.UploadID, &r.SpaceID, &r.DeclaredSize); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- 任务行清理(删除落点/空间之前必须做)----------------------------
//
// 为什么需要这一组:uploads.parent_id 与 uploads.space_id 都是
// **ON DELETE RESTRICT**,而任务行从不清扫(定稿/取消只改 state,见 Finalize /
// Release),于是"这个目录/空间曾经有人传过文件"会让 DELETE 撞外键 ——
// 实测:未定稿的任务会让"解散空间"返回 500 internal_error,空间永远删不掉;
// 删一个曾经有上传任务的目录同样 500。
//
// 为什么不在迁移里把外键改成 CASCADE / SET NULL(两条都比"显式清理"更糟):
//   - CASCADE:reserved 行被数据库直接删掉,而 spaces.used_bytes 里的预留额
//     就没有任何行可以用来回退了 —— 用户凭空少一块容量(静默泄漏,无报错);
//   - SET NULL:代码里 parent_id 为空**就是"空间根目录"**(见 CreateInput.ParentID
//     的注释),未完成的任务会被静默改落到根目录 —— "数据放错地方"比报错严重得多。
// 所以这里先按 declared_size 回退 reserved 行的预留额,再删行;
// 两步都在调用方的事务里,顺序固定为"先 spaces 后 uploads"(与 Reserve 的加锁
// 顺序一致,避免与并发的创建上传任务形成锁序反转)。
//
// 删掉任务行会不会丢审计:不会。上传的 create/finish/cancel 都写了 audit_logs,
// 任务行本身是**协议状态**(断点续传偏移 + ticket 哈希 + 预留账本),落点没了
// 它就没有任何意义。

// ReleaseReservedOfSpace 回退某空间全部 reserved 任务的预留额度,返回回退的字节数。
func (UploadRepo) ReleaseReservedOfSpace(ctx context.Context, q Querier, spaceID string) (int64, error) {
	var refunded int64
	err := q.QueryRow(ctx, `
WITH res AS (
    SELECT space_id, sum(declared_size) AS bytes
      FROM uploads WHERE space_id = $1 AND state = 'reserved'
     GROUP BY space_id
), upd AS (
    UPDATE spaces s SET used_bytes = GREATEST(0, s.used_bytes - r.bytes), updated_at = now()
      FROM res r WHERE s.id = r.space_id
    RETURNING r.bytes
)
SELECT coalesce(sum(bytes), 0) FROM upd`, spaceID).Scan(&refunded)
	return refunded, err
}

// ReleaseReservedOfSubtree 回退"落点在某棵子树内"的 reserved 任务的预留额度。
//
// 按 space_id 分组,所以一次子树删除跨空间(理论上不会发生,但防御)也能正确回退。
func (UploadRepo) ReleaseReservedOfSubtree(ctx context.Context, q Querier, rootFileID string) (int64, error) {
	var refunded int64
	err := q.QueryRow(ctx, `
WITH RECURSIVE sub AS (
    SELECT id FROM files WHERE id = $1
    UNION ALL
    SELECT f.id FROM files f JOIN sub ON f.parent_id = sub.id
), res AS (
    SELECT space_id, sum(declared_size) AS bytes
      FROM uploads
     WHERE state = 'reserved' AND parent_id IN (SELECT id FROM sub)
     GROUP BY space_id
), upd AS (
    UPDATE spaces s SET used_bytes = GREATEST(0, s.used_bytes - r.bytes), updated_at = now()
      FROM res r WHERE s.id = r.space_id
    RETURNING r.bytes
)
SELECT coalesce(sum(bytes), 0) FROM upd`, rootFileID).Scan(&refunded)
	return refunded, err
}

// DeleteTasksOfSpace 删除某空间的全部上传任务行,返回删除行数。
func (UploadRepo) DeleteTasksOfSpace(ctx context.Context, q Querier, spaceID string) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM uploads WHERE space_id = $1`, spaceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteTasksOfSubtree 删除"落点在某棵子树内"的全部上传任务行,返回删除行数。
func (UploadRepo) DeleteTasksOfSubtree(ctx context.Context, q Querier, rootFileID string) (int64, error) {
	tag, err := q.Exec(ctx, `
WITH RECURSIVE sub AS (
    SELECT id FROM files WHERE id = $1
    UNION ALL
    SELECT f.id FROM files f JOIN sub ON f.parent_id = sub.id
)
DELETE FROM uploads WHERE parent_id IN (SELECT id FROM sub)`, rootFileID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func formatSeconds(d time.Duration) string {
	return itoaSeconds(int64(d.Seconds()))
}

func itoaSeconds(v int64) string {
	if v <= 0 {
		return "0 seconds"
	}
	// 简单整数转字符串,避免引入 strconv 以外的依赖
	var b [24]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:]) + " seconds"
}
