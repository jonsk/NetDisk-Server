package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
)

// SpaceRepo 空间、成员与配额(4.3)。
type SpaceRepo struct{}

const spaceColumns = `id, kind, coalesce(group_id::text,''), owner_id, name,
	quota_bytes, used_bytes, frozen, last_seq, created_at, updated_at`

func scanSpace(row pgx.Row) (*model.Space, error) {
	var s model.Space
	if err := row.Scan(&s.ID, &s.Kind, &s.GroupID, &s.OwnerID, &s.Name,
		&s.QuotaBytes, &s.UsedBytes, &s.Frozen, &s.LastSeq, &s.CreatedAt, &s.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &s, nil
}

// GetByID 按主键取空间。
func (SpaceRepo) GetByID(ctx context.Context, q Querier, id string) (*model.Space, error) {
	return scanSpace(q.QueryRow(ctx, `SELECT `+spaceColumns+` FROM spaces WHERE id = $1`, id))
}

// AdminSpaceRow 是后台空间治理需要的聚合视图(空间 + 所有者 + 成员数)。
//
// 为什么要 JOIN 出所有者用户名:治理界面必须回答"这是谁的空间" ——
// 只给 owner_id 会让管理员去别处再查一次(而那一次查询往往忘了带租户/权限条件)。
type AdminSpaceRow struct {
	model.Space
	OwnerUsername string
	OwnerDisplay  string
	// WarnPercent 是配额预警阈值(4.3;按空间可覆盖,默认 80)
	WarnPercent int
	MemberCount int
	// FilesCount / ObjectsBytes 用于判断"这个空间是不是空壳"
	FilesCount int64
}

// SpaceFilter 是后台空间列表的筛选条件。
type SpaceFilter struct {
	// Search 同时匹配空间名与所有者用户名(大小写不敏感)
	Search string
	Kind   string
	// Frozen 为 nil 表示不过滤冻结状态
	Frozen *bool
	Limit  int
	Offset int
}

// MaxSpaceListLimit 是后台空间列表的单页上限。
const MaxSpaceListLimit = 200

// ListAdmin 分页返回空间治理视图与总数。
//
// 统计字段用**子查询**而不是 JOIN + GROUP BY:成员数与文件数是两个独立的多对一
// 关系,一起 JOIN 会互相放大(经典的行数乘积错误,表现为"成员数无故翻倍")。
func (SpaceRepo) ListAdmin(ctx context.Context, q Querier, f SpaceFilter) ([]AdminSpaceRow, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxSpaceListLimit {
		limit = MaxSpaceListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	search := "%" + escapeLike(strings.TrimSpace(f.Search)) + "%"
	args := []any{search, strings.TrimSpace(f.Kind)}
	where := `WHERE ($1 = '%%'
	           OR s.name ILIKE $1 ESCAPE '\'
	           OR u.username ILIKE $1 ESCAPE '\'
	           OR u.display_name ILIKE $1 ESCAPE '\')
	          AND ($2 = '' OR s.kind = $2)`
	if f.Frozen != nil {
		args = append(args, *f.Frozen)
		where += fmt.Sprintf(" AND s.frozen = $%d", len(args))
	}

	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM spaces s JOIN users u ON u.id = s.owner_id `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, limit, f.Offset)
	rows, err := q.Query(ctx, `
SELECT s.id, s.kind, coalesce(s.group_id::text,''), s.owner_id, s.name,
       s.quota_bytes, s.used_bytes, s.frozen, s.last_seq, s.created_at, s.updated_at,
       u.username, u.display_name, s.quota_warn_percent,
       (SELECT count(*) FROM space_members m WHERE m.space_id = s.id) AS member_count,
       (SELECT count(*) FROM files f2 WHERE f2.space_id = s.id) AS files_count
  FROM spaces s JOIN users u ON u.id = s.owner_id
  `+where+`
 ORDER BY s.created_at DESC, s.id DESC
 LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]AdminSpaceRow, 0, limit)
	for rows.Next() {
		var r AdminSpaceRow
		if serr := rows.Scan(&r.ID, &r.Kind, &r.GroupID, &r.OwnerID, &r.Name,
			&r.QuotaBytes, &r.UsedBytes, &r.Frozen, &r.LastSeq, &r.CreatedAt, &r.UpdatedAt,
			&r.OwnerUsername, &r.OwnerDisplay, &r.WarnPercent, &r.MemberCount, &r.FilesCount); serr != nil {
			return nil, 0, serr
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// CreateSpaceInput 是建团队空间入参。
type CreateSpaceInput struct {
	GroupID    string
	OwnerID    string
	Name       string
	QuotaBytes int64
}

// CreateTeam 建一个团队空间行(kind=team)。
//
// 只做"插一行":groups 行、根目录、owner 的 manager 成员由调用方在**同一事务**
// 里补齐(顺序与职责写在 spacesvc.Create 的注释里)。仓储不替业务决定这些顺序。
func (SpaceRepo) CreateTeam(ctx context.Context, q Querier, in CreateSpaceInput) (*model.Space, error) {
	return scanSpace(q.QueryRow(ctx, `
INSERT INTO spaces (kind, group_id, owner_id, name, quota_bytes)
VALUES ('team', $1, $2, $3, $4)
RETURNING `+spaceColumns, in.GroupID, in.OwnerID, in.Name, in.QuotaBytes))
}

// WarnPercentOf 读某空间的预警阈值(缺行返回 ErrNotFound)。
//
// 单独一个方法而不是扩展 model.Space:预警阈值只有**治理界面**需要,
// 而 model.Space 会跟着登录/列表路径到处流动 —— 为它多读一列不划算。
func (SpaceRepo) WarnPercentOf(ctx context.Context, q Querier, spaceID string) (int, error) {
	var warn int
	if err := q.QueryRow(ctx,
		`SELECT quota_warn_percent FROM spaces WHERE id = $1`, spaceID).Scan(&warn); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return warn, nil
}

// SetQuota 设置配额与预警阈值(4.3;quotaBytes=0 表示不限制)。
//
// 只改这两列、不动 used_bytes:用量**只能由写事务增减**(R-16)——
// 管理后台"顺手把用量也改一下"会让对账把真实漂移当成正常。
func (SpaceRepo) SetQuota(ctx context.Context, q Querier, spaceID string, quotaBytes int64, warnPercent int) error {
	tag, err := q.Exec(ctx, `
UPDATE spaces SET quota_bytes = $2, quota_warn_percent = $3, updated_at = now()
 WHERE id = $1`, spaceID, quotaBytes, warnPercent)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetFrozenByAdmin 冻结/解冻(管理员治理动作;不校验成员身份)。
func (SpaceRepo) SetFrozenByAdmin(ctx context.Context, q Querier, spaceID string, frozen bool) (*model.Space, error) {
	tag, err := q.Exec(ctx,
		`UPDATE spaces SET frozen = $2, updated_at = now() WHERE id = $1`, spaceID, frozen)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, ErrNotFound
	}
	return (SpaceRepo{}).GetByID(ctx, q, spaceID)
}

// ClearMembers 清空某空间的成员(管理员"收回空间"用)。
//
// **刻意只清成员、不删数据**:文档把"回收/保留期限"列为待拍板的裁量项(B 类),
// 因此这里只实现**机制**(收回访问权),不替业务决定"数据该不该删"。
// 删空间/删文件是另一个动作,需要显式确认与单独的保留策略。
// 返回被移除的成员数。
func (SpaceRepo) ClearMembers(ctx context.Context, q Querier, spaceID string) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1`, spaceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PersonalOf 取某用户的个人空间(4.3:个人盘是 kind=personal 的空间行)。
//
// **配额与用量都挂在这一行** —— 调用方不应再去 users 表找配额。
func (SpaceRepo) PersonalOf(ctx context.Context, q Querier, userID string) (*model.Space, error) {
	return scanSpace(q.QueryRow(ctx,
		`SELECT `+spaceColumns+` FROM spaces WHERE kind = 'personal' AND owner_id = $1`, userID))
}

// Membership 是"用户在某空间的权限"投影(4.3 三级权限)。
type Membership struct {
	SpaceID    string
	UserID     string
	Permission string
	IsOwner    bool
}

// GetMembership 返回用户在空间的权限。
//
// 规则(4.3):
//   - personal 空间:仅 owner 本人,权限按 owner(person→manager)
//   - team 空间:查 space_members;若同时是 owner,则提升为 manager
//
// 返回 (nil, nil) 表示**无权限**,由上层映射为 403/410;
// 返回 ErrNotFound 表示空间不存在(映射为 404/410)。
func (r SpaceRepo) GetMembership(ctx context.Context, q Querier, spaceID, userID string) (*Membership, error) {
	sp, err := r.GetByID(ctx, q, spaceID)
	if err != nil {
		return nil, err
	}
	if sp.OwnerID == userID {
		return &Membership{SpaceID: spaceID, UserID: userID, Permission: model.PermManager, IsOwner: true}, nil
	}
	if sp.IsPersonal() {
		// 个人空间不给他人任何权限
		return nil, nil
	}
	var perm string
	err = q.QueryRow(ctx,
		`SELECT permission FROM space_members WHERE space_id = $1 AND user_id = $2`,
		spaceID, userID).Scan(&perm)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &Membership{SpaceID: spaceID, UserID: userID, Permission: perm}, nil
}

// ListVisible 列出用户可见的全部空间(personal + 已加入的 team),用于 SSE 订阅过滤(6.9)。
func (SpaceRepo) ListVisible(ctx context.Context, q Querier, userID string) ([]*model.Space, error) {
	rows, err := q.Query(ctx,
		`SELECT `+spaceColumns+` FROM spaces s
		 WHERE s.owner_id = $1
		    OR EXISTS (SELECT 1 FROM space_members m WHERE m.space_id = s.id AND m.user_id = $1)
		 ORDER BY s.kind DESC, s.name`, userID)
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

// Reserve 预扣额度(4.3 预留-结算两段式)。
//
// 用条件 UPDATE 在**数据库层**保证不超卖:
//
//	quota_bytes = 0            → 不限制
//	used_bytes + size <= quota → 允许
//
// 返回 ErrQuotaExceeded 表示额度不足(**创建上传任务时即拒绝**,不是传完再拒)。
func (SpaceRepo) Reserve(ctx context.Context, q Querier, spaceID string, size int64) (*model.Space, error) {
	if size < 0 {
		return nil, fmt.Errorf("repo: size 不能为负")
	}
	s, err := scanSpace(q.QueryRow(ctx,
		`UPDATE spaces SET used_bytes = used_bytes + $2, updated_at = now()
		 WHERE id = $1 AND (quota_bytes = 0 OR used_bytes + $2 <= quota_bytes)
		 RETURNING `+spaceColumns, spaceID, size))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// 区分"空间不存在"与"额度不足":前者 404,后者 507
			if _, gerr := (&SpaceRepo{}).GetByID(ctx, q, spaceID); gerr == nil {
				return nil, ErrQuotaExceeded
			}
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s, nil
}

// Settle 结算:把预留的 declaredSize 调整为 actualSize(可为负差额)。
func (SpaceRepo) Settle(ctx context.Context, q Querier, spaceID string, declared, actual int64) (*model.Space, error) {
	delta := actual - declared
	s, err := scanSpace(q.QueryRow(ctx,
		`UPDATE spaces
		 SET used_bytes = GREATEST(0, used_bytes + $2), updated_at = now()
		 WHERE id = $1
		 RETURNING `+spaceColumns, spaceID, delta))
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Release 释放预留(取消上传/过期回收)。
func (SpaceRepo) Release(ctx context.Context, q Querier, spaceID string, size int64) error {
	_, err := q.Exec(ctx,
		`UPDATE spaces SET used_bytes = GREATEST(0, used_bytes - $2), updated_at = now() WHERE id = $1`,
		spaceID, size)
	return err
}

// expectedUsedSQL 是 used_bytes 的**权威口径**(4.3 / 6.6)。
//
//	expected = sum(files.size WHERE NOT is_dir) + sum(uploads.declared_size WHERE state='reserved')
//
// **第二项不能漏**:`used_bytes` 是"已占用 + 已预留"的合计 —— 建任务即预留
// (BE-S1-05,防超卖),定稿时才按真实 size 结算。少了预留项,任何有在传文件的
// 空间都会被判成"漂移",而对账任务一旦回写就会**抹掉正在上传的预留** ——
// 那正好打开了预留机制要堵的洞(额度超卖)。
//
// 这是"口径单一"的正面例子:对账与预留/结算必须共用同一个定义,
// 各写一份 SQL 迟早就对不上,而对不上的表现是"对账任务把额度改错"。
const expectedUsedSQL = `(
    COALESCE((SELECT SUM(f.size) FROM files f WHERE f.space_id = s.id AND NOT f.is_dir), 0)
  + COALESCE((SELECT SUM(u.declared_size) FROM uploads u WHERE u.space_id = s.id AND u.state = 'reserved'), 0)
)`

// ReconcileUsed 以"files 实时 SUM + 在飞预留"重算 used_bytes 并返回漂移量(4.3 对账)。
//
// 口径:按逻辑行计(4.5)—— 同一对象被 N 行引用就计 N 份。
func (SpaceRepo) ReconcileUsed(ctx context.Context, q Querier, spaceID string) (stored int64, expected int64, err error) {
	err = q.QueryRow(ctx,
		`SELECT s.used_bytes, `+expectedUsedSQL+` FROM spaces s WHERE s.id = $1`, spaceID).
		Scan(&stored, &expected)
	return
}

// SetUsed 把 used_bytes 直接写成对账得到的期望值(仅对账任务使用)。
//
// `WHERE abs(...) > $2` 让"回写"本身是**条件写**:读取与写入之间的窗口里
// 若恰好有新的预留进来,条件会让这次回写落空(而不是把预留抹掉)。
// 返回 true 表示确实回写了。
func (SpaceRepo) SetUsed(ctx context.Context, q Querier, spaceID string, minDrift int64) (bool, error) {
	tag, err := q.Exec(ctx,
		`UPDATE spaces s
		    SET used_bytes = GREATEST(0, `+expectedUsedSQL+`), updated_at = now()
		  WHERE s.id = $1 AND abs(s.used_bytes - `+expectedUsedSQL+`) > $2`,
		spaceID, minDrift)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ListIDs 列出全部空间 id(后台巡视任务用,例如变更流按空间清理)。
//
// 刻意**不带分页/过滤**:它是运维任务的全量扫描入口,加"只看 visible"之类的
// 过滤会让"某个空间的行永远不被清理"。调用方若担心规模,应自己限批。
func (SpaceRepo) ListIDs(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id::text FROM spaces ORDER BY created_at`)
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

// Drift 是单个空间的对账结果。
type Drift struct {
	SpaceID  string
	Stored   int64
	Expected int64
}

// Delta 是漂移量(stored - expected)。
//
// 正数 = 账面比实际占用多(预留没释放 / 删文件少扣);
// 负数 = 账面比实际少(**超额使用**,比正数严重:用户已经占用了没算钱的字节)。
func (d Drift) Delta() int64 { return d.Stored - d.Expected }

// ListDrifted 找出 used_bytes 与权威口径(文件 + 在飞预留)不一致的空间。
//
// 用**权威口径**而不是只比 sum(files):后者会把每个正在上传的空间都算成漂移
// (见 expectedUsedSQL 的说明),而回写会抹掉预留。
//
// scope 非空时只对账这些空间(为空 = 全部)。运维侧用于"只对账某几个空间";
// 测试侧必须有它:`spaces` 是全局共享表,而对账本身是全局动作 ——
// 没有范围限定的用例会**顺手改掉别的包的空间**(实测:开了 autofix 的用例
// 把并发运行的其它包的空间账面一起回写了,而"单跑必过、全量跑失败")。
func (SpaceRepo) ListDrifted(ctx context.Context, q Querier, limit int, scope []string) ([]Drift, error) {
	if limit <= 0 {
		limit = 200
	}
	var scopeArg any
	if len(scope) > 0 {
		scopeArg = scope
	}
	rows, err := q.Query(ctx,
		`SELECT s.id::text, s.used_bytes, `+expectedUsedSQL+`
		   FROM spaces s
		  WHERE s.used_bytes <> `+expectedUsedSQL+`
		    AND ($2::uuid[] IS NULL OR s.id = ANY($2::uuid[]))
		  ORDER BY s.created_at
		  LIMIT $1`, limit, scopeArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Drift
	for rows.Next() {
		var d Drift
		if err := rows.Scan(&d.SpaceID, &d.Stored, &d.Expected); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AddMember 添加空间成员(4.3 协作管理)。
func (SpaceRepo) AddMember(ctx context.Context, q Querier, spaceID, userID, permission string) error {
	if permission == "" {
		permission = model.PermEditor
	}
	_, err := q.Exec(ctx,
		`INSERT INTO space_members (space_id, user_id, permission) VALUES ($1, $2, $3)
		 ON CONFLICT (space_id, user_id) DO UPDATE SET permission = EXCLUDED.permission`,
		spaceID, userID, permission)
	return err
}

// RemoveMember 移除成员。
func (SpaceRepo) RemoveMember(ctx context.Context, q Querier, spaceID, userID string) error {
	tag, err := q.Exec(ctx, `DELETE FROM space_members WHERE space_id = $1 AND user_id = $2`, spaceID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
