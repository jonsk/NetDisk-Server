package repo

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
)

// 组织同步需要的两个"按外部 id 定位"的查询。
//
// 为什么不让同步逻辑自己按名字匹配:`departments.name` 允许重名(不同分支下同名部门
// 很常见),而 `(source, ext_id)` 才是提供方给的**稳定标识**。按名字匹配的后果是
// "改个部门名就多出一个部门",且没有任何唯一约束能拦。

// GetDeptByExt 按 (source, ext_id) 找部门;不存在返回 ErrNotFound。
func (DeptRepo) GetDeptByExt(ctx context.Context, q Querier, source, extID string) (*model.Department, error) {
	if source == "" || extID == "" {
		return nil, ErrNotFound
	}
	d, err := scanDept(q.QueryRow(ctx,
		`SELECT `+deptColumns+` FROM departments WHERE source = $1 AND ext_id = $2`,
		source, extID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return d, nil
}

// ListDeptsBySource 返回某来源的全部部门(对账用)。
func (DeptRepo) ListDeptsBySource(ctx context.Context, q Querier, source string) ([]*model.Department, error) {
	rows, err := q.Query(ctx,
		`SELECT `+deptColumns+` FROM departments WHERE source = $1`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Department
	for rows.Next() {
		d, serr := scanDept(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
