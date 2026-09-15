package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
)

// FileRepo 文件元数据(6.4)。
//
// 注意:files 表**不存** object_key/storage_backend —— 物理落点唯一登记在 file_objects,
// 通过 hash 关联(6.4 的 P0-2 决策,避免双写不一致)。
type FileRepo struct{}

const fileColumns = `id, space_id, coalesce(parent_id::text,''), owner_id, name, is_dir,
	size, mime_type, coalesce(hash_sha256,''), version, depth, etag, created_at, updated_at`

func scanFile(row pgx.Row) (*model.File, error) {
	var f model.File
	if err := row.Scan(&f.ID, &f.SpaceID, &f.ParentID, &f.OwnerID, &f.Name, &f.IsDir,
		&f.Size, &f.MimeType, &f.HashSHA256, &f.Version, &f.Depth, &f.Etag,
		&f.CreatedAt, &f.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &f, nil
}

// GetByID 取文件元数据。
func (FileRepo) GetByID(ctx context.Context, q Querier, id string) (*model.File, error) {
	return scanFile(q.QueryRow(ctx, `SELECT `+fileColumns+` FROM files WHERE id = $1`, id))
}

// GetRoot 取空间根目录行(每空间恰一行 parent_id IS NULL,4.3)。
func (FileRepo) GetRoot(ctx context.Context, q Querier, spaceID string) (*model.File, error) {
	return scanFile(q.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM files WHERE space_id = $1 AND parent_id IS NULL`, spaceID))
}

// GetChildByName 按名称取子项(判重/冲突检测用)。
//
// 名称比较**大小写不敏感**,与唯一索引 (space_id, parent_id, lower(name)) 口径一致(6.7)。
func (FileRepo) GetChildByName(ctx context.Context, q Querier, spaceID, parentID, name string) (*model.File, error) {
	return scanFile(q.QueryRow(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE space_id = $1 AND parent_id = $2 AND lower(name) = lower($3)`,
		spaceID, parentID, name))
}

// ListChildren 列出目录直接子项(keyset 分页:after 为上一页最后的 name)。
func (FileRepo) ListChildren(ctx context.Context, q Querier, spaceID, parentID string, limit int, after string) ([]*model.File, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := q.Query(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE space_id = $1 AND parent_id = $2
		   AND ($3 = '' OR lower(name) > lower($3))
		 ORDER BY lower(name), id
		 LIMIT $4`, spaceID, parentID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SubtreeStats 是目录操作影响面(6.11 预检)。
type SubtreeStats struct {
	FileCount int64 `json:"file_count"`
	DirCount  int64 `json:"dir_count"`
	TotalSize int64 `json:"total_size_bytes"`
}

// SubtreeStatsOf 用递归 CTE 统计子树(6.11:客户端删除确认与容量预估共用)。
func (FileRepo) SubtreeStatsOf(ctx context.Context, q Querier, fileID string) (*SubtreeStats, error) {
	var s SubtreeStats
	err := q.QueryRow(ctx,
		`WITH RECURSIVE sub AS (
		     SELECT id, is_dir, size FROM files WHERE id = $1
		     UNION ALL
		     SELECT f.id, f.is_dir, f.size FROM files f JOIN sub ON f.parent_id = sub.id
		 )
		 SELECT COALESCE(SUM(CASE WHEN NOT is_dir THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN is_dir THEN 1 ELSE 0 END),0),
		        COALESCE(SUM(CASE WHEN NOT is_dir THEN size ELSE 0 END),0)
		 FROM sub`, fileID).Scan(&s.FileCount, &s.DirCount, &s.TotalSize)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateDirInput 建目录入参。
type CreateDirInput struct {
	SpaceID  string
	ParentID string
	OwnerID  string
	Name     string
	Depth    int
}

// CreateDir 建目录;同名(大小写不敏感)冲突返回 ErrConflict。
func (FileRepo) CreateDir(ctx context.Context, q Querier, in CreateDirInput) (*model.File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, depth)
		 VALUES ($1, $2, $3, $4, true, $5)
		 RETURNING `+fileColumns,
		in.SpaceID, in.ParentID, in.OwnerID, in.Name, in.Depth))
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return f, nil
}

// TouchVersion 严格按乐观锁更新版本:不匹配返回 ErrConflict(6.6 并发写矩阵)。
//
// 返回更新后的行,便于把 server_version/server_etag 回给客户端(409 响应体)。
func (FileRepo) TouchVersion(ctx context.Context, q Querier, fileID string, expectVersion int64) (*model.File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`UPDATE files SET version = version + 1, updated_at = now()
		 WHERE id = $1 AND version = $2
		 RETURNING `+fileColumns, fileID, expectVersion))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 区分"不存在"与"版本不匹配"
			if _, gerr := (&FileRepo{}).GetByID(ctx, q, fileID); gerr == nil {
				return nil, ErrConflict
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// Rename 改名(带乐观锁);同目录重名由唯一索引兜底 → ErrConflict。
func (FileRepo) Rename(ctx context.Context, q Querier, fileID, newName string, expectVersion int64) (*model.File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`UPDATE files SET name = $2, version = version + 1, updated_at = now()
		 WHERE id = $1 AND version = $3
		 RETURNING `+fileColumns, fileID, newName, expectVersion))
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrConflict
		}
		if errors.Is(err, ErrNotFound) {
			if _, gerr := (&FileRepo{}).GetByID(ctx, q, fileID); gerr == nil {
				return nil, ErrConflict
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	return f, nil
}

// Delete 硬删一行(无回收站,4.5);调用方负责同步调整 file_objects 引用计数。
func (FileRepo) Delete(ctx context.Context, q Querier, fileID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM files WHERE id = $1`, fileID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Move 移动到新父目录并可选改名(带乐观锁);同目录重名由唯一索引兜底 → ErrConflict。
//
// 只更新**这一行**;目录的子树深度级联由 RecascadeDepth 负责(调用方在同一事务内)。
func (FileRepo) Move(ctx context.Context, q Querier, fileID, newParentID, newName string, newDepth int) (*model.File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`UPDATE files SET parent_id = $2, name = $3, depth = $4,
		                  version = version + 1, updated_at = now()
		 WHERE id = $1
		 RETURNING `+fileColumns, fileID, nullableUUID(newParentID), newName, newDepth))
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return f, nil
}

// RecascadeDepth 重算整棵子树的 depth(移动目录后调用)。
//
// 用一条递归 CTE 完成,而不是逐层循环:一棵 30 层、上万行的子树用循环更新
// 会产生上万次往返;CTE 版本是**一条语句**。
//
// 只更新 depth 与 updated_at,**不动 version**:depth 是派生结构信息,
// 不是用户可感知的元数据变更 —— 若给它 +1,客户端会看到"我只移动了 A,
// 子文件 B 的版本却变了",进而触发无意义的重传。
func (FileRepo) RecascadeDepth(ctx context.Context, q Querier, rootID string, rootDepth int) error {
	_, err := q.Exec(ctx, `
WITH RECURSIVE sub AS (
    SELECT id, 0 AS rel FROM files WHERE id = $1
    UNION ALL
    SELECT f.id, s.rel + 1 FROM files f JOIN sub s ON f.parent_id = s.id
)
UPDATE files SET depth = sub.rel + $2, updated_at = now()
  FROM sub WHERE files.id = sub.id AND sub.rel > 0`,
		rootID, rootDepth)
	return err
}

// ReleaseObjectRef 把某 hash 的引用计数 -1;归零则转入延迟删除窗口。
//
// 返回 `released=true` 表示**这一次递减让引用归零**(对象随即转入
// `pending_delete` + 24h 延迟删除窗口),也就是"这个物理对象从此进入待删队列"。
//
// 为什么返回值不是"受影响行数":调用方(`filesvc.Delete` 的 `released_objects`)
// 要向客户端回答的是"这次删除**回收了几个物理对象**",而不是"改了几行引用计数"。
// 两者在共享内容下差别很大:删掉 5 个引用同一对象的文件,受影响行数是 5、
// 回收对象数是 0(前 4 次)或 1(最后一次)。曾经这里返回行数,
// 于是客户端会看到"删 1 个文件回收了 1 个对象"——**而对象其实还在被别处引用**,
// 容量核对时对不上(由端到端探针发现:删副本后报 released_objects=1,
// 但原件与共享行仍在引用同一对象)。
//
// 实现与 finalize 的 releaseObjectRef 同语义,但**多带一个状态条件**:
// 只有 `ref_count > 0` 才减,避免把计数打成负数。
// 表的 CHECK 不变式 `(ref_count = 0) = (state <> 'live')` 要求"计数归零"与
// "状态离开 live"必须**在同一条 UPDATE 里完成** —— 拆两条会被约束直接拒绝。
//
// 由于该不变式,`ref_count > 0` 当且仅当 `state = 'live'`,因此
// "更新后状态是 pending_delete" 就等价于"这次递减让它归零"。
func (FileRepo) ReleaseObjectRef(ctx context.Context, q Querier, hash string) (released bool, err error) {
	var state string
	err = q.QueryRow(ctx, `
UPDATE file_objects
   SET ref_count = ref_count - 1,
       state = CASE WHEN ref_count - 1 = 0 THEN 'pending_delete' ELSE state END,
       delete_after = CASE WHEN ref_count - 1 = 0 THEN now() + interval '24 hours' ELSE delete_after END,
       updated_at = now()
 WHERE hash_sha256 = $1 AND ref_count > 0
 RETURNING state`, hash).Scan(&state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 对象不存在,或引用数已为 0(说明调用方的元数据与对象表不一致)
			return false, nil
		}
		return false, err
	}
	return state == model.ObjectPendingDelete, nil
}

// CountChildren 判断目录是否为空(删除前校验)。
func (FileRepo) CountChildren(ctx context.Context, q Querier, parentID string) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM files WHERE parent_id = $1`, parentID).Scan(&n)
	return n, err
}

// DirOpInProgress 判断某条目的**祖先链上**(含自身)是否有"移动中"标记(6.11)。
//
// 沿 `parent_id` **向上**走:异步任务只把子树根标成 moving,而被它覆盖的是
// 整棵子树 —— 任何落在子树里的请求都必须 409,否则用户会在任务执行期间
// 往一个正在被搬走的目录里写东西(那些行随后被移动/删除,用户看到自己刚建的
// 文件凭空消失)。用一条递归 CTE 而不是逐层查:深度上限 31,但一次往返比
// ≤31 次往返更稳(也不给"每次读都多打几十次库"的机会)。
//
// 行不存在时返回 false:存在性由调用方各自的 404 分支负责,这里不越权。
func (FileRepo) DirOpInProgress(ctx context.Context, q Querier, fileID string) (bool, error) {
	if fileID == "" {
		return false, nil
	}
	var busy bool
	err := q.QueryRow(ctx, `
WITH RECURSIVE chain AS (
    SELECT id, parent_id, op_state FROM files WHERE id = $1
    UNION ALL
    SELECT f.id, f.parent_id, f.op_state FROM files f JOIN chain c ON f.id = c.parent_id
)
SELECT coalesce(bool_or(op_state <> ''), false) FROM chain`, fileID).Scan(&busy)
	if err != nil {
		return false, err
	}
	return busy, nil
}

// BumpSubtreeVersion 把整棵子树的 version +1(不含根,根的 +1 由 Move/Rename 自己完成)。
//
// 为什么**子行**也要 +1(6.11 定稿,与 RecascadeDepth 原先"不动 version"的注释相反):
// 移动祖先改变了每个后代的**完整路径**,而客户端缓存与 WebDAV 的 ETag 语义是
// 按资源路径比较的 —— 后代 ETag 不变时,客户端会认为"这个文件没有任何变化",
// 于是继续用旧路径(旧 parent_id)去引用它,直到某次操作失败才暴露。
// etag 是 version 的生成列,所以这里不需要单独维护 etag。
//
// 只更新 version/updated_at:size/hash 不变(内容没动),因此不会触发重传。
func (FileRepo) BumpSubtreeVersion(ctx context.Context, q Querier, rootID string) (int64, error) {
	tag, err := q.Exec(ctx, `
WITH RECURSIVE sub AS (
    SELECT id, 0 AS rel FROM files WHERE id = $1
    UNION ALL
    SELECT f.id, s.rel + 1 FROM files f JOIN sub s ON f.parent_id = s.id
)
UPDATE files SET version = version + 1, updated_at = now()
  FROM sub WHERE files.id = sub.id AND sub.rel > 0`, rootID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// InsertRow 插入一条文件行(供 finalizeUpload 使用;version 固定为 1)。
//
// 该函数**不做**配额与引用计数处理 —— 那些必须在同一事务内由 finalize 编排(R-03)。
func (FileRepo) InsertRow(ctx context.Context, q Querier, f *model.File) (*model.File, error) {
	if f.MimeType == "" {
		f.MimeType = "application/octet-stream"
	}
	out, err := scanFile(q.QueryRow(ctx,
		`INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, mime_type, hash_sha256, depth, version)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1)
		 RETURNING `+fileColumns,
		f.SpaceID, nullableUUID(f.ParentID), f.OwnerID, f.Name, f.IsDir, f.Size, f.MimeType, nullableHash(f.HashSHA256), f.Depth))
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrConflict
		}
		return nil, err
	}
	return out, nil
}

// UpdateContentPointer 覆盖写:更新内容指针 + version+1(6.5 规则 4 的第 2 步)。
func (FileRepo) UpdateContentPointer(ctx context.Context, q Querier, fileID string, size int64, hash, mime string) (*model.File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`UPDATE files SET hash_sha256 = $2, size = $3, mime_type = $4,
		                  version = version + 1, updated_at = now()
		 WHERE id = $1
		 RETURNING `+fileColumns, fileID, hash, size, mime))
	if err != nil {
		return nil, err
	}
	return f, nil
}

func nullableUUID(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableHash(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ErrUnsupported 用于尚未实现的目录级操作(6.11 异步任务)。
var ErrUnsupported = fmt.Errorf("repo: 该操作尚未实现")
