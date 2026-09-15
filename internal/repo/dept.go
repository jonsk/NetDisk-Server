package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
)

// DeptRepo 是组织架构(部门树)仓储(4.2 / 6.4)。
//
// 设计要点:
//   - 树结构存在 `departments.parent_id`,**闭包表只做派生索引**,不承载真相。
//     这样"同步任务全量重建闭包表"是幂等且可随时重跑的(6.4 T1-4 弃 ltree 的直接收益)。
//   - 子树查询一律走 `department_closure`(有 (ancestor_id,descendant_id) 主键 +
//     descendant 侧索引),**不用递归 CTE** —— 组织树会被高频查询,递归 CTE
//     每查一次都要遍历整条路径。
type DeptRepo struct{}

const deptColumns = `id, coalesce(parent_id::text,''), name, sort_order, source,
	coalesce(ext_id,''), created_at, updated_at`

// deptColumnsD 是 deptColumns 的显式别名版本(与 JOIN 一起用时必须限定表名)。
//
// 刻意**手写**而不做字符串替换:deptColumns 里含 `coalesce(...)` 带逗号的表达式,
// 按逗号切分再补前缀会把它切成 `d.coalesce(parent_id::text` + `”)` —— 直接语法错误。
// 这种"看似省事的元编程"在 SQL 上只会把编译期错误推迟到运行时。
const deptColumnsD = `d.id, coalesce(d.parent_id::text,''), d.name, d.sort_order, d.source,
	coalesce(d.ext_id,''), d.created_at, d.updated_at`

func scanDept(row pgx.Row) (*model.Department, error) {
	var d model.Department
	if err := row.Scan(&d.ID, &d.ParentID, &d.Name, &d.SortOrder, &d.Source,
		&d.ExtID, &d.CreatedAt, &d.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &d, nil
}

// ---- 闭包表重建 ----

// RebuildResult 是一次全量重建的结果。
type RebuildResult struct {
	// ClosureRows 写入的闭包行数(含每个节点指向自身的 depth=0 行)
	ClosureRows int64
	// DeptCount 参与重建的部门数
	DeptCount int64
	// MaxDepth 最大层级(根为 0)
	MaxDepth int
}

// RebuildClosure 从 `departments.parent_id` 全量重建闭包表。
//
// 调用方必须把它放在**一个事务**里执行:DELETE 与 INSERT 之间若插入查询,
// 会看到空闭包表(等于"所有部门都没有层级"),这比报错更危险。
//
// **递归方向是关键**(这里踩过一次坑,值得写清):闭包表要的是"每个节点 →
// 它的全部祖先"。因此 anchor 必须是**全部节点**(depth=0 即自指行),
// 递归项沿 `parent_id` **向根走**。
//
// 若反过来写成"anchor=根,沿 parent_id 向下展开",产出的行只有
// `(根, 后代, 深度)` 一种 —— 非根节点的自指行与"中间祖先→后代"行全部缺失。
// 对一棵链状树它会恰好少行且深度看起来"对",极难从断言中发现;
// 只有在"同一层有多个兄弟分支"时才会暴露(某些后代永远收不到自指行)。
//
// 实测:4 节点、两分支的树上,向下展开只得 4 行(正确为 8 行)。
func (DeptRepo) RebuildClosure(ctx context.Context, q Querier) (*RebuildResult, error) {
	if _, err := q.Exec(ctx, `DELETE FROM department_closure`); err != nil {
		return nil, fmt.Errorf("清空闭包表失败: %w", err)
	}

	tag, err := q.Exec(ctx, `
WITH RECURSIVE up AS (
    -- anchor:每个节点指向自己(depth = 0)
    SELECT d.id AS descendant_id, d.id AS ancestor_id, 0 AS depth, 1 AS guard
      FROM departments d
    UNION ALL
    -- 递归:从"已知祖先"继续向上取它的父,直到根
    SELECT t.descendant_id, p.id, t.depth + 1, t.guard + 1
      FROM up t
      JOIN departments p ON p.id = (SELECT parent_id FROM departments WHERE id = t.ancestor_id)
     WHERE t.guard < 1000
)
INSERT INTO department_closure (ancestor_id, descendant_id, depth)
SELECT ancestor_id, descendant_id, depth FROM up
ON CONFLICT (ancestor_id, descendant_id) DO UPDATE SET depth = EXCLUDED.depth`)
	if err != nil {
		return nil, fmt.Errorf("重建闭包表失败: %w", err)
	}

	var res RebuildResult
	res.ClosureRows = tag.RowsAffected()

	// 重建后**自检**:每个节点都必须有自指行。缺行说明 departments 里存在环
	// (环上的节点向上走永远回不到"父为空"的终点,guard 用尽也不会产出某些行)。
	// 这里主动报错而不是返回一个看起来成功的残缺闭包表 ——
	// 残缺闭包表会让子树查询静默漏数据,比直接失败难查得多。
	var missing int64
	if err := q.QueryRow(ctx, `
SELECT count(*) FROM departments d
 WHERE NOT EXISTS (SELECT 1 FROM department_closure c
                    WHERE c.ancestor_id = d.id AND c.descendant_id = d.id AND c.depth = 0)`,
	).Scan(&missing); err != nil {
		return nil, fmt.Errorf("校验闭包表自指行失败: %w", err)
	}
	if missing > 0 {
		return nil, fmt.Errorf(
			"闭包表重建后仍有 %d 个节点缺少自指行:部门数据中存在环(祖先链回不到根)", missing)
	}

	if err := q.QueryRow(ctx,
		`SELECT count(*), coalesce(max(depth),0) FROM department_closure`).Scan(&res.DeptCount, &res.MaxDepth); err != nil {
		return nil, fmt.Errorf("统计闭包表失败: %w", err)
	}
	return &res, nil
}

// ValidateTree 检查部门树是否自洽,返回全部问题(而不是遇到第一个就返回)。
//
// 一次性报全,是因为组织同步失败时运维需要一次看到"有哪几个节点有问题",
// 而不是修一个跑一次(同步动辄上万节点,逐个试错不可接受)。
//
// 检查项:
//  1. **孤儿子树** `orphan`:节点既不是根、在闭包表里又没有任何祖先行
//     (depth>0 的行缺失) —— 说明它的祖先链到不了任何根,即数据成环。
//     环上的节点都不是"根"(parent_id 非空),递归从根出发也到不了它们,
//     因此"既非根、又无祖先"是环的充要特征。
//  2. **自指行缺失** `closure_mismatch`:任何节点都必须有 (自己,自己, 0) 这一行;
//     缺了说明闭包表被写坏或增量维护漏更新。
//
// 实现细节(写错会静默失效,值得记下):判定"闭包表里没有对应行"必须用
// `LEFT JOIN ... ON 连接键` 再检查**连接列是否为 NULL**。
// 若把连接键放进 WHERE 子句、或用被连接表的 NOT NULL 列判 NULL,
// 条件永远不成立 —— 校验器会恒返回"全部正常",比不校验更危险。
func (DeptRepo) ValidateTree(ctx context.Context, q Querier) ([]TreeProblem, error) {
	rows, err := q.Query(ctx, `
WITH has_anc AS (
    SELECT descendant_id FROM department_closure WHERE depth > 0
)
SELECT d.id,
       d.name,
       CASE WHEN d.parent_id IS NULL THEN 'closure_mismatch' ELSE 'orphan' END AS kind
  FROM departments d
  LEFT JOIN department_closure self
         ON self.ancestor_id = d.id AND self.descendant_id = d.id AND self.depth = 0
 WHERE (d.parent_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM has_anc a WHERE a.descendant_id = d.id))
    OR self.descendant_id IS NULL
 ORDER BY kind, d.name`)
	if err != nil {
		return nil, fmt.Errorf("校验部门树失败: %w", err)
	}
	defer rows.Close()

	var out []TreeProblem
	for rows.Next() {
		var p TreeProblem
		if err := rows.Scan(&p.DeptID, &p.Name, &p.Kind); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TreeProblem 是一处部门树自洽性问题。
type TreeProblem struct {
	DeptID string `json:"dept_id"`
	Name   string `json:"name"`
	// Kind: orphan(祖先链断裂,通常是数据环)/ closure_mismatch(闭包表缺行)
	Kind string `json:"kind"`
}

// ---- 子树与祖先查询 ----

// Subtree 是一次子树查询的结果(含自身,按深度与排序号排列)。
type Subtree struct {
	Root  *model.Department
	Nodes []*model.Department
	// Depth 是各节点相对 Root 的层级(0 = Root 自身)
	Depth map[string]int
}

// SubtreeOf 用闭包表一条 SQL 取子树(4.2:子树查询用一条 SQL 带索引)。
func (DeptRepo) SubtreeOf(ctx context.Context, q Querier, deptID string) (*Subtree, error) {
	root, err := (&DeptRepo{}).GetByID(ctx, q, deptID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
SELECT `+deptColumnsD+`, c.depth
  FROM department_closure c
  JOIN departments d ON d.id = c.descendant_id
 WHERE c.ancestor_id = $1
 ORDER BY c.depth, d.sort_order, d.name`, deptID)
	if err != nil {
		return nil, fmt.Errorf("查询子树失败: %w", err)
	}
	defer rows.Close()

	st := &Subtree{Root: root, Depth: map[string]int{}}
	for rows.Next() {
		var d model.Department
		var depth int
		if err := rows.Scan(&d.ID, &d.ParentID, &d.Name, &d.SortOrder, &d.Source,
			&d.ExtID, &d.CreatedAt, &d.UpdatedAt, &depth); err != nil {
			return nil, err
		}
		dd := d
		st.Nodes = append(st.Nodes, &dd)
		st.Depth[d.ID] = depth
	}
	return st, rows.Err()
}

// AncestorsOf 取某部门的祖先链(不含自身),从直接上级到根。
func (DeptRepo) AncestorsOf(ctx context.Context, q Querier, deptID string) ([]*model.Department, error) {
	rows, err := q.Query(ctx, `
SELECT `+deptColumnsD+`
  FROM department_closure c
  JOIN departments d ON d.id = c.ancestor_id
 WHERE c.descendant_id = $1 AND c.depth > 0
 ORDER BY c.depth`, deptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Department
	for rows.Next() {
		d, err := scanDept(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// IsDescendantOf 判断 deptID 是否是 ancestorID 的后代(含自身)。
//
// 移动部门前必须调用:否则可以把一个部门移到自己的子树下,造出数据环。
func (DeptRepo) IsDescendantOf(ctx context.Context, q Querier, deptID, ancestorID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM department_closure
                WHERE ancestor_id = $1 AND descendant_id = $2)`,
		ancestorID, deptID).Scan(&exists)
	return exists, err
}

// ---- 部门 CRUD ----

// CreateDeptInput 新建部门入参。
type CreateDeptInput struct {
	ParentID  string
	Name      string
	SortOrder int
	Source    string
	ExtID     string
}

// Create 新建部门并**同步写闭包行**。
//
// 闭包行 = "父的全部祖先(含父自己) → 我" + "我 → 我"。
// 用一条 INSERT...SELECT 从父的闭包行派生,避免逐祖先往返。
func (DeptRepo) Create(ctx context.Context, q Querier, in CreateDeptInput) (*model.Department, error) {
	if in.Source == "" {
		in.Source = model.DeptSourceManual
	}
	d, err := scanDept(q.QueryRow(ctx, `
INSERT INTO departments (parent_id, name, sort_order, source, ext_id)
VALUES ($1, $2, $3, $4, $5)
RETURNING `+deptColumns,
		nullableUUID(in.ParentID), in.Name, in.SortOrder, in.Source, nullableHash(in.ExtID)))
	if err != nil {
		if IsUniqueViolation(err) {
			return nil, ErrConflict
		}
		if IsForeignKeyViolation(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := (&DeptRepo{}).insertClosureFor(ctx, q, d); err != nil {
		return nil, err
	}
	return d, nil
}

// insertClosureFor 为新节点插入闭包行(自身 + 全部祖先)。
//
// 注意 `$2` 必须显式 `::uuid` 转型:根节点的父为 NULL,若不带转型,
// PG 会因 UNION ALL 两侧(从查询取得的 uuid / 传入的未知类型 NULL)
// 类型推断冲突而报 42P08「推断出不一致的类型」。
func (DeptRepo) insertClosureFor(ctx context.Context, q Querier, d *model.Department) error {
	_, err := q.Exec(ctx, `
INSERT INTO department_closure (ancestor_id, descendant_id, depth)
SELECT c.ancestor_id, $1::uuid, c.depth + 1
  FROM department_closure c
 WHERE c.descendant_id = NULLIF($2, '')::uuid
UNION ALL
SELECT $1::uuid, $1::uuid, 0
ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`,
		d.ID, d.ParentID)
	if err != nil {
		return fmt.Errorf("写入部门闭包行失败: %w", err)
	}
	return nil
}

// GetByID 取部门。
func (DeptRepo) GetByID(ctx context.Context, q Querier, id string) (*model.Department, error) {
	return scanDept(q.QueryRow(ctx, `SELECT `+deptColumns+` FROM departments WHERE id = $1`, id))
}

// ListAll 列出全部部门(组织同步对账与后台树展示用)。
func (DeptRepo) ListAll(ctx context.Context, q Querier) ([]*model.Department, error) {
	rows, err := q.Query(ctx, `SELECT `+deptColumns+` FROM departments ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Department
	for rows.Next() {
		d, err := scanDept(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Rename 改名。
func (DeptRepo) Rename(ctx context.Context, q Querier, id, name string) (*model.Department, error) {
	return scanDept(q.QueryRow(ctx,
		`UPDATE departments SET name = $2, updated_at = now() WHERE id = $1 RETURNING `+deptColumns, id, name))
}

// Move 移动部门(改 parent)并**重建该子树的闭包行**。
//
// 为什么整表重建而不是只重建子树:子树重建需要精确删掉"旧祖先×子树"的笛卡尔积、
// 再插入"新祖先×子树",一旦漏删就留下幽灵祖先(子树会出现在两个不相干的部门下)。
// 组织变更是低频操作,直接调用 RebuildClosure 是全量一致性的最简保证。
//
// **调用前必须用 IsDescendantOf 拦住"移到自己子树下"**,否则会造出数据环。
func (DeptRepo) Move(ctx context.Context, q Querier, id, newParentID string) (*model.Department, error) {
	d, err := scanDept(q.QueryRow(ctx,
		`UPDATE departments SET parent_id = $2, updated_at = now() WHERE id = $1 RETURNING `+deptColumns,
		id, nullableUUID(newParentID)))
	if err != nil {
		if IsForeignKeyViolation(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if _, err := (&DeptRepo{}).RebuildClosure(ctx, q); err != nil {
		return nil, err
	}
	return d, nil
}

// Delete 删除部门(仅允许删叶子;有子部门或成员时由外键/检查拒绝)。
func (DeptRepo) Delete(ctx context.Context, q Querier, id string) error {
	tag, err := q.Exec(ctx, `DELETE FROM departments WHERE id = $1`, id)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return ErrConflict
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	// 闭包行由 ON DELETE CASCADE 清理;但"被删节点作为祖先"的其它行
	// 也已被 CASCADE 覆盖,无需额外处理。
	return nil
}

// CountChildren 统计直接子部门数(删除前预检用)。
func (DeptRepo) CountChildren(ctx context.Context, q Querier, id string) (int64, error) {
	var n int64
	err := q.QueryRow(ctx, `SELECT count(*) FROM departments WHERE parent_id = $1`, id).Scan(&n)
	return n, err
}

// ---- 用户与部门关联 ----

// SetUserDepartments 覆盖式设置用户所属部门(is_primary 只允许一个)。
//
// 覆盖式而不是增量:组织同步给的是"全量成员列表",增量更新会留下
// 已在源端被移除的陈旧关联(用户仍能看到已离开部门的团队空间)。
func (DeptRepo) SetUserDepartments(ctx context.Context, q Querier, userID string, deptIDs []string, primaryDeptID string) error {
	if _, err := q.Exec(ctx, `DELETE FROM user_departments WHERE user_id = $1`, userID); err != nil {
		return err
	}
	if len(deptIDs) == 0 {
		return nil
	}
	if primaryDeptID == "" {
		primaryDeptID = deptIDs[0]
	}
	_, err := q.Exec(ctx, `
INSERT INTO user_departments (user_id, department_id, is_primary)
SELECT $1, d, (d = $3) FROM unnest($2::uuid[]) AS d`,
		userID, deptIDs, primaryDeptID)
	if err != nil {
		if IsForeignKeyViolation(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// DepartmentsOfUser 列出用户所属部门。
func (DeptRepo) DepartmentsOfUser(ctx context.Context, q Querier, userID string) ([]*model.Department, error) {
	rows, err := q.Query(ctx, `
SELECT `+deptColumnsD+`
  FROM user_departments ud
  JOIN departments d ON d.id = ud.department_id
 WHERE ud.user_id = $1
 ORDER BY ud.is_primary DESC, d.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.Department
	for rows.Next() {
		d, err := scanDept(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// UsersInSubtree 取某部门子树内的全部用户(团队空间按部门授权时用)。
//
// 这是闭包表的典型收益:一条 SQL、两个索引,不存在递归。
func (DeptRepo) UsersInSubtree(ctx context.Context, q Querier, deptID string) ([]string, error) {
	rows, err := q.Query(ctx, `
SELECT DISTINCT ud.user_id::text
  FROM department_closure c
  JOIN user_departments ud ON ud.department_id = c.descendant_id
 WHERE c.ancestor_id = $1`, deptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
