// Package repo 提供仓储层:薄封装事务边界与错误映射(6.6)。
//
// 纪律:
//   - SQL 只写在这里(未来切 sqlc 时替换为生成物,调用方不变)
//   - 不在事务内做对象 IO(R-04)
//   - 返回领域错误,不把 pgx 错误泄漏到 handler
package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/netdisk/netdisk/internal/model"
)

// 领域错误(handler 映射为 HTTP 码)
var (
	ErrNotFound      = errors.New("repo: not found")
	ErrConflict      = errors.New("repo: conflict")
	ErrQuotaExceeded = errors.New("repo: quota exceeded")
)

// UserRepo 用户与凭证。
type UserRepo struct{}

// userColumns 统一列顺序,避免各处 SELECT * 漂移。
const userColumns = `id, username, coalesce(email,''), display_name, avatar_url,
	role, status, token_version, created_at, updated_at`

func scanUser(row pgx.Row) (*model.User, error) {
	var u model.User
	if err := row.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.AvatarURL,
		&u.Role, &u.Status, &u.TokenVersion, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// GetByID 按主键查用户。
func (UserRepo) GetByID(ctx context.Context, q Querier, id string) (*model.User, error) {
	return scanUser(q.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// GetByUsername 按用户名查(大小写不敏感,与 users_username_key 一致)。
func (UserRepo) GetByUsername(ctx context.Context, q Querier, username string) (*model.User, error) {
	return scanUser(q.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE lower(username) = lower($1)`, strings.TrimSpace(username)))
}

// GetByEmail 按邮箱查。
func (UserRepo) GetByEmail(ctx context.Context, q Querier, email string) (*model.User, error) {
	return scanUser(q.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE lower(email) = lower($1)`, strings.TrimSpace(email)))
}

// FindByLogin 支持"用户名或邮箱"登录。
func (r UserRepo) FindByLogin(ctx context.Context, q Querier, login string) (*model.User, error) {
	login = strings.TrimSpace(login)
	if strings.Contains(login, "@") {
		return r.GetByEmail(ctx, q, login)
	}
	return r.GetByUsername(ctx, q, login)
}

// CreateInput 建用户入参。
type CreateInput struct {
	Username    string
	Email       string
	DisplayName string
	Role        string
	Status      string
}

// Create 建用户并**自动创建个人空间**(4.1 V2.14:个人配额挂在 personal 空间行)。
//
// 必须在同一事务内完成,否则会出现"有用户无个人空间"的半初始化状态。
func (UserRepo) Create(ctx context.Context, tx pgx.Tx, in CreateInput) (*model.User, error) {
	if in.Username == "" {
		return nil, fmt.Errorf("repo: username 不能为空")
	}
	if in.Role == "" {
		in.Role = model.RoleUser
	}
	if in.Status == "" {
		in.Status = model.StatusActive
	}
	if in.DisplayName == "" {
		in.DisplayName = in.Username
	}

	var email any
	if in.Email != "" {
		email = in.Email
	}

	u, err := scanUser(tx.QueryRow(ctx,
		`INSERT INTO users (username, email, display_name, role, status)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+userColumns,
		in.Username, email, in.DisplayName, in.Role, in.Status))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrConflict
		}
		return nil, err
	}

	// 个人空间:配额 0 = 不限制;名称遵循 4.3「{display_name}的网盘」
	spaceName := u.DisplayName + "的网盘"
	if _, err := tx.Exec(ctx,
		`INSERT INTO spaces (kind, owner_id, name, quota_bytes) VALUES ('personal', $1, $2, 0)`,
		u.ID, spaceName); err != nil {
		return nil, fmt.Errorf("创建个人空间失败: %w", err)
	}

	// 个人空间的根目录(每空间恰一行 parent_id IS NULL,4.3)
	if _, err := tx.Exec(ctx,
		`INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, depth)
		 SELECT s.id, NULL, $1, '根目录', true, 0 FROM spaces s
		 WHERE s.owner_id = $1 AND s.kind = 'personal'`, u.ID); err != nil {
		return nil, fmt.Errorf("创建根目录失败: %w", err)
	}

	return u, nil
}

// BumpTokenVersion 自增令牌版本(6.8 纪律 3:实现"静默退出")。
func (UserRepo) BumpTokenVersion(ctx context.Context, q Querier, userID string) (int64, error) {
	var v int64
	if err := q.QueryRow(ctx,
		`UPDATE users SET token_version = token_version + 1, updated_at = now()
		 WHERE id = $1 RETURNING token_version`, userID).Scan(&v); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return v, nil
}

// SetStatus 启停用用户(4.1)。
func (UserRepo) SetStatus(ctx context.Context, q Querier, userID, status string) error {
	tag, err := q.Exec(ctx, `UPDATE users SET status = $2, updated_at = now() WHERE id = $1`, userID, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRole 调整角色(后台用户管理用)。
func (UserRepo) SetRole(ctx context.Context, q Querier, userID, role string) error {
	tag, err := q.Exec(ctx, `UPDATE users SET role = $2, updated_at = now() WHERE id = $1`, userID, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MaxUserListLimit 是后台用户列表的单页上限。
//
// 设上限而不是"要多少给多少":后台列表会被前端一次拉全量(几千人),
// 服务端不封顶就等于把"一次请求序列化几兆 JSON"的能力交给了 URL 参数。
const MaxUserListLimit = 200

// UserFilter 是后台用户列表的筛选条件(7.x 管理后台)。
type UserFilter struct {
	// Search 同时匹配用户名/显示名/邮箱(大小写不敏感,子串)
	Search string
	// Role/Status 为空表示不过滤
	Role   string
	Status string
	Limit  int
	Offset int
}

// escapeLike 转义 LIKE 元字符。
//
// **必须转义**:搜索框里输入一个 `%` 本该"搜不到东西",不转义就变成"匹配全部" ——
// 管理员以为自己在筛选,实际看到的是全表;而 `_` 更隐蔽(单个字符通配)。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// userFilterWhere 生成筛选 WHERE 子句与参数(列表与计数**共用同一份**,
// 否则"翻页翻着翻着总数对不上"这类问题会以最难看的方式暴露)。
func userFilterWhere(f UserFilter) (string, []any) {
	search := "%" + escapeLike(strings.TrimSpace(f.Search)) + "%"
	where := `WHERE ($1 = '%%'
	            OR username ILIKE $1 ESCAPE '\'
	            OR display_name ILIKE $1 ESCAPE '\'
	            OR coalesce(email, '') ILIKE $1 ESCAPE '\')
	          AND ($2 = '' OR role = $2)
	          AND ($3 = '' OR status = $3)`
	return where, []any{search, strings.TrimSpace(f.Role), strings.TrimSpace(f.Status)}
}

// List 按筛选条件分页返回用户与总数。
//
// 这里用 OFFSET 而不是 keyset 游标:后台用户列表是**低频、人工翻阅**的界面,
// 而 offset 能直接支持"第 3 页/共 12 页"这种管理员真正会用的交互;
// keyset 只能"下一页",翻页体验更差。热的、机器驱动的列表(变更流、审计)
// 仍然用 keyset —— 两条路各自用在合适的地方。
func (UserRepo) List(ctx context.Context, q Querier, f UserFilter) ([]model.User, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxUserListLimit {
		limit = MaxUserListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	where, args := userFilterWhere(f)

	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM users `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	rows, err := q.Query(ctx, `SELECT `+userColumns+` FROM users `+where+`
 ORDER BY created_at DESC, id DESC
 LIMIT $4 OFFSET $5`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]model.User, 0, limit)
	for rows.Next() {
		u, serr := scanUser(rows)
		if serr != nil {
			return nil, 0, serr
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// AdminUserRow 是后台列表需要、而 model.User 没带的字段(最后登录时间等)。
type AdminUserRow struct {
	model.User
	LastLoginAt *time.Time
}

// ListWithLogin 在 List 的基础上带上最后登录时间。
//
// 为什么单独一个方法而不是给 model.User 加字段:最后登录时间是**后台视角**的
// 运维信息,登录/鉴权路径不该为了它多读一列(那条路径每秒都在跑)。
func (UserRepo) ListWithLogin(ctx context.Context, q Querier, f UserFilter) ([]AdminUserRow, int, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > MaxUserListLimit {
		limit = MaxUserListLimit
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	where, args := userFilterWhere(f)

	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM users `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := q.Query(ctx, `SELECT `+userColumns+`, last_login_at FROM users `+where+`
 ORDER BY created_at DESC, id DESC
 LIMIT $4 OFFSET $5`, append(args, limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]AdminUserRow, 0, limit)
	for rows.Next() {
		var r AdminUserRow
		if serr := rows.Scan(&r.ID, &r.Username, &r.Email, &r.DisplayName, &r.AvatarURL,
			&r.Role, &r.Status, &r.TokenVersion, &r.CreatedAt, &r.UpdatedAt, &r.LastLoginAt); serr != nil {
			return nil, 0, serr
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// ---- 本地账密(00002 迁移引入的列)----

// PasswordCredentials 是登录校验所需的凭证快照。
type PasswordCredentials struct {
	UserID           string
	PasswordHash     string
	Status           string
	FailedLoginCount int
	LockedUntil      *time.Time
}

// Locked 判断是否处于锁定期。
func (c *PasswordCredentials) Locked(now time.Time) bool {
	return c.LockedUntil != nil && c.LockedUntil.After(now)
}

// GetCredentials 按登录名取凭证(用户名或邮箱)。
//
// 单独一个方法(而不是扩展 GetByLogin)是刻意的:密码哈希只在认证路径上读取,
// 避免它随 User 结构体到处流动而意外写进日志或 API 响应。
func (UserRepo) GetCredentials(ctx context.Context, q Querier, login string) (*PasswordCredentials, error) {
	login = strings.TrimSpace(login)
	var c PasswordCredentials
	err := q.QueryRow(ctx,
		`SELECT id, password_hash, status, failed_login_count, locked_until
		 FROM users
		 WHERE lower(username) = lower($1) OR (email IS NOT NULL AND lower(email) = lower($1))
		 LIMIT 1`, login).Scan(&c.UserID, &c.PasswordHash, &c.Status, &c.FailedLoginCount, &c.LockedUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

// LoginSucceeded 记录成功登录并清零失败计数。
func (UserRepo) LoginSucceeded(ctx context.Context, q Querier, userID string) error {
	_, err := q.Exec(ctx,
		`UPDATE users SET last_login_at = now(), failed_login_count = 0, locked_until = NULL,
		                   updated_at = now()
		 WHERE id = $1`, userID)
	return err
}

// LoginFailed 记录失败登录;达到阈值后锁定一段时间,返回累计失败次数。
//
// 锁定策略在**数据库**层生效(而不是仅靠 Redis 计数),这样 Redis 重启不会
// 让攻击者获得无限次尝试机会 —— 与 10.7「认证必须拒绝,不得降级」一致。
func (UserRepo) LoginFailed(ctx context.Context, q Querier, userID string, threshold int, lockFor time.Duration) (int, error) {
	if threshold <= 0 {
		threshold = 5
	}
	if lockFor <= 0 {
		lockFor = 15 * time.Minute
	}
	var count int
	err := q.QueryRow(ctx,
		`UPDATE users
		 SET failed_login_count = failed_login_count + 1,
		     locked_until = CASE WHEN failed_login_count + 1 >= $2
		                         THEN now() + $3::interval ELSE locked_until END,
		     updated_at = now()
		 WHERE id = $1
		 RETURNING failed_login_count`,
		userID, threshold, fmt.Sprintf("%d seconds", int(lockFor.Seconds()))).Scan(&count)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	return count, nil
}

// SetPassword 设置/重置口令。
func (UserRepo) SetPassword(ctx context.Context, tx pgx.Tx, userID, hash string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE users
		 SET password_hash = $2, password_updated_at = now(),
		     -- 改密后自增 token_version:所有既有会话立即失效(6.8 纪律 3)
		     token_version = token_version + 1,
		     updated_at = now()
		 WHERE id = $1`, userID, hash)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- refresh token 权威存储(00002 迁移)----

// SaveRefresh 落库 refresh token。
func (UserRepo) SaveRefresh(ctx context.Context, q Querier, jti, userID, audience string, tokenVersion int64, expiresAt time.Time) error {
	_, err := q.Exec(ctx,
		`INSERT INTO refresh_tokens (jti, user_id, audience, token_version, expires_at)
		 VALUES ($1, $2, $3, $4, $5)`, jti, userID, audience, tokenVersion, expiresAt)
	return err
}

// RefreshState 是 refresh token 的权威状态。
type RefreshState struct {
	JTI          string
	UserID       string
	Audience     string
	TokenVersion int64
	ExpiresAt    time.Time
	RevokedAt    *time.Time
}

// GetRefresh 读取 refresh 记录;不存在返回 ErrNotFound。
func (UserRepo) GetRefresh(ctx context.Context, q Querier, jti string) (*RefreshState, error) {
	var s RefreshState
	err := q.QueryRow(ctx,
		`SELECT jti, user_id, audience, token_version, expires_at, revoked_at
		 FROM refresh_tokens WHERE jti = $1`, jti).
		Scan(&s.JTI, &s.UserID, &s.Audience, &s.TokenVersion, &s.ExpiresAt, &s.RevokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &s, nil
}

// RevokeRefresh 吊销单条 refresh(登出)。
func (UserRepo) RevokeRefresh(ctx context.Context, q Querier, jti string) error {
	_, err := q.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE jti = $1 AND revoked_at IS NULL`, jti)
	return err
}

// RevokeAllForUser 吊销某用户全部 refresh(改密/停用/管理员强制下线)。
func (UserRepo) RevokeAllForUser(ctx context.Context, q Querier, userID string) (int64, error) {
	tag, err := q.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredRefresh 清理过期记录(定时任务调用)。
func (UserRepo) DeleteExpiredRefresh(ctx context.Context, q Querier) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM refresh_tokens WHERE expires_at < now() - interval '7 days'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
