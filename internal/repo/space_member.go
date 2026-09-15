package repo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
)

// ---- 根目录保护(S3-03:根行禁删/禁改名/禁移动)----
//
// 数据库层没有约束能表达"这行是根",所以保护必须在 SQL 的 WHERE 里:
// 只要把 `parent_id IS NULL` 写进条件,任何试图删/改名/移动根目录的调用
// 都会匹配 0 行 → ErrNotFound,而不是"成功地把空间根改名成 xxx"。
//
// 为什么不在 service 层判断?—— 判断与执行之间隔着一次网络往返,
// 并发下仍可能把根改掉;写在同一条 UPDATE/DELETE 里才是原子的。

// RenameNonRoot 改名,**拒绝根目录**(S3-03)。
func (FileRepo) RenameNonRoot(ctx context.Context, q Querier, fileID, newName string, expectVersion int64) (*model.File, error) {
	f, err := scanFile(q.QueryRow(ctx,
		`UPDATE files SET name = $2, version = version + 1, updated_at = now()
		 WHERE id = $1 AND version = $3 AND parent_id IS NOT NULL
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

// DeleteNonRoot 硬删一行,**拒绝根目录**(S3-03)。
func (FileRepo) DeleteNonRoot(ctx context.Context, q Querier, fileID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM files WHERE id = $1 AND parent_id IS NOT NULL`, fileID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IsRoot 判断某文件行是否是空间根。
func (FileRepo) IsRoot(ctx context.Context, q Querier, fileID string) (bool, error) {
	var isRoot bool
	err := q.QueryRow(ctx,
		`SELECT (parent_id IS NULL) FROM files WHERE id = $1`, fileID).Scan(&isRoot)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return isRoot, err
}

// ListChildrenAll 列出目录全部子项(不带分页),供目录级操作预检与递归使用。
//
// 与 ListChildren 的区别:不设 200 条默认上限,调用方自行控制规模
// (目录级操作已在 6.11 用 SubtreeStatsOf 预检影响面)。
func (FileRepo) ListChildrenAll(ctx context.Context, q Querier, spaceID, parentID string) ([]*model.File, error) {
	rows, err := q.Query(ctx,
		`SELECT `+fileColumns+` FROM files
		 WHERE space_id = $1 AND parent_id = $2
		 ORDER BY lower(name), id`, spaceID, parentID)
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

// ---- 空间成员管理(S3-04)----

// SpaceMember 是空间成员视图(含用户名,后台列表要用)。
type SpaceMember struct {
	UserID      string    `json:"user_id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	Permission  string    `json:"permission"`
	IsOwner     bool      `json:"is_owner"`
	JoinedAt    time.Time `json:"joined_at"`
}

// ListMembers 列出空间成员(owner 排在最前)。
func (SpaceRepo) ListMembers(ctx context.Context, q Querier, spaceID string) ([]*SpaceMember, error) {
	sp, err := (&SpaceRepo{}).GetByID(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
SELECT u.id, u.username, u.display_name, m.permission, m.joined_at
  FROM space_members m
  JOIN users u ON u.id = m.user_id
 WHERE m.space_id = $1
 ORDER BY u.username`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*SpaceMember{}
	if sp.OwnerID != "" {
		// owner 可能不在 space_members 里(个人空间就是),单独补一行
		o, err := (&UserRepo{}).GetByID(ctx, q, sp.OwnerID)
		if err == nil {
			out = append(out, &SpaceMember{
				UserID: o.ID, Username: o.Username, DisplayName: o.DisplayName,
				Permission: model.PermManager, IsOwner: true,
			})
		}
	}
	for rows.Next() {
		var m SpaceMember
		if err := rows.Scan(&m.UserID, &m.Username, &m.DisplayName, &m.Permission, &m.JoinedAt); err != nil {
			return nil, err
		}
		if m.UserID == sp.OwnerID {
			continue // 上面已补
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// UpsertMember 新增或改权限(幂等)。
func (SpaceRepo) UpsertMember(ctx context.Context, q Querier, spaceID, userID, permission string) error {
	_, err := q.Exec(ctx, `
INSERT INTO space_members (space_id, user_id, permission)
VALUES ($1, $2, $3)
ON CONFLICT (space_id, user_id) DO UPDATE SET permission = EXCLUDED.permission`,
		spaceID, userID, permission)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return ErrNotFound
		}
		if IsCheckViolation(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

// SetFrozen 冻结/解冻空间(4.3 管理员治理)。
func (SpaceRepo) SetFrozen(ctx context.Context, q Querier, spaceID string, frozen bool) (*model.Space, error) {
	return scanSpace(q.QueryRow(ctx,
		`UPDATE spaces SET frozen = $2, updated_at = now() WHERE id = $1 RETURNING `+spaceColumns,
		spaceID, frozen))
}

// TransferOwner 转让空间所有权。
//
// **只改 owner_id,不碰 space_members** —— 成员的增删由调用方在同一事务里
// 按正确顺序完成(先给新 owner 写 manager 行,再改 owner_id)。
// 把这些揉进一条 SQL 会让"顺序错了会得到什么中间态"变得不可见。
func (SpaceRepo) TransferOwner(ctx context.Context, q Querier, spaceID, newOwnerID string) error {
	tag, err := q.Exec(ctx,
		`UPDATE spaces SET owner_id = $2, updated_at = now() WHERE id = $1 AND kind = 'team'`,
		spaceID, newOwnerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSpace 删除空间行(解散的最后一步)。
//
// 前置条件由调用方保证(空间内已无文件)。级联删除只覆盖 space_members
// 与 sync_feed 这类从属数据;files 的 space_id 是 RESTRICT,
// 所以"空间非空就删不掉"是数据库层面的兜底,不依赖调用方自觉。
func (SpaceRepo) DeleteSpace(ctx context.Context, q Querier, spaceID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM spaces WHERE id = $1 AND kind = 'team'`, spaceID)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return ErrConflict
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTeamSpacesOf 列出某用户拥有或参与的团队空间(空间管理页用)。
func (SpaceRepo) ListTeamSpacesOf(ctx context.Context, q Querier, userID string) ([]*model.Space, error) {
	rows, err := q.Query(ctx,
		`SELECT `+spaceColumns+` FROM spaces s
		 WHERE s.kind = 'team'
		   AND (s.owner_id = $1
		        OR EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = s.id AND m.user_id = $1))
		 ORDER BY s.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Space
	for rows.Next() {
		s, err := scanSpace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
