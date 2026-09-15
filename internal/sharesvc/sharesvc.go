// Package sharesvc 实现分享链接(6.3 / 1.1 / BE-S6-04)。
//
// # 它是系统唯一的"免登录出口"
//
// 分享落地页的访问者**没有账号** —— 这决定了三件事:
//
//  1. **token 必须不可枚举**。它是唯一的凭据(密码是可选的第二因子),
//     所以用 32 字节密码学随机数(crypto/rand)的 base64url 形式(43 字符),
//     而不是自增 id 或时间戳派生值。可枚举的 token 等于"把所有人的文件放在
//     一个可遍历的命名空间里"。
//  2. **密码校验是暴力破解面**。落地页免登录 ⇒ 攻击者不需要账号就能无限试密码,
//     因此必须计入限速(复用登录那档阈值:按来源 IP 收得最紧)。
//  3. **下载计数必须原子递增且不超上限**。计数与上限的比较必须落在**同一条 UPDATE**
//     里:`SELECT count` 再 `UPDATE` 的写法在并发下会让两个人同时通过检查,
//     于是"限 1 次"的链接被下载了多次(而分享者以为它已经失效)。
//
// # 过期/超次/吊销都用 410 而不是 404
//
// 三者对**已持有链接的人**而言语义相同:"这个链接不再可用"。用 410 Gone 表达
// "曾经存在、现在不可用",与"从来没存在过"(404)区分开 —— 前者是**终态**,
// 客户端/用户不该反复重试;后者可能只是拼错了,值得重打一次。
package sharesvc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/repo"
)

// TokenBytes 是 token 的随机字节数(32 字节 = 256 位熵)。
const TokenBytes = 32

// DefaultTTL 是未指定有效期时的默认值(7 天)。
//
// 默认**不是永久**:永久有效的分享链接一旦泄漏就永远泄漏(而分享者往往已经忘了
// 自己分享过什么)。有明确需求时可以显式传更长的 expires_at。
const DefaultTTL = 7 * 24 * time.Hour

// ErrGone 表示分享链接已不可用(过期/超次/吊销/文件已删)。
var ErrGone = errors.New("sharesvc: 分享链接已不可用")

// Querier 是本包需要的数据库能力(与 filesvc.DB 同构:读走池、写走事务)。
//
// 刻意复用 repo 的 Querier 而不是 *db.DB:本包的每条 SQL 都是单语句
// (没有跨表事务),于是"谁提供连接"留给调用方,测试也能直接用池。
type Querier = repo.Querier

// Service 分享用例入口。
type Service struct {
	DB     Querier
	Policy credentials.Policy
	Now    func() time.Time
}

// CreateInput 是创建分享的入参。
type CreateInput struct {
	UserID string
	FileID string
	// Password 为空表示不需要密码(公开链接)
	Password string
	// ExpiresAt 为零值时用 DefaultTTL
	ExpiresAt time.Time
	// MaxDownloads 0 = 不限
	MaxDownloads int
}

// Share 是一条分享记录。
type Share struct {
	ID            string
	Token         string
	FileID        string
	UserID        string
	HasPassword   bool
	ExpiresAt     *time.Time
	MaxDownloads  int
	DownloadCount int
	Revoked       bool
	CreatedAt     time.Time
}

// Meta 是落地页可见的信息(**不含敏感字段**)。
type Meta struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
	// NeedPassword 让前端决定是否先弹密码框(而不是先下载再 401)
	NeedPassword bool `json:"need_password"`
	// ExpiresAt 空表示用不过期
	ExpiresAt string `json:"expires_at,omitempty"`
	// DownloadCount / MaxDownloads 便于分享者与访问者看到剩余次数
	DownloadCount int `json:"download_count"`
	MaxDownloads  int `json:"max_downloads"`
	// MimeType 落地页展示用
	MimeType string `json:"mime_type"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NewToken 生成分享 token(43 字符 base64url,256 位熵)。
func NewToken() (string, error) {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成分享 token 失败: %w", err)
	}
	// RawURLEncoding:不带 `=` 填充,可直接放进 URL 路径
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Create 创建分享链接。
//
// 权限:只有**能看到该文件**的人才能分享它(读权限即可 —— 分享是把"我能看的"
// 给出去,不需要写权限);这条判定由调用方在构造输入前完成(见 api 层),
// 本函数只负责落库与密码哈希。
func (s *Service) Create(ctx context.Context, in CreateInput) (*Share, error) {
	if in.UserID == "" || in.FileID == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少用户或文件")
	}
	if in.MaxDownloads < 0 {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "max_downloads 不能为负")
	}
	hash := ""
	if strings.TrimSpace(in.Password) != "" {
		h, err := s.Policy.Hash(in.Password)
		if err != nil {
			// 口令强度不合格 → 400(与注册同一条规则:两处不一致会出现
			// "注册时能过、分享时过不了"这类分裂)
			return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "分享密码不符合要求: %s", err.Error())
		}
		hash = h
	}
	expires := in.ExpiresAt
	if expires.IsZero() {
		expires = s.now().Add(DefaultTTL)
	}
	if !expires.After(s.now()) {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "过期时间必须晚于当前时间")
	}

	token, err := NewToken()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	var sh Share
	var exp *time.Time
	err = s.DB.QueryRow(ctx, `
INSERT INTO shares (token, file_id, user_id, password_hash, expires_at, max_downloads)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, token, file_id::text, user_id::text, password_hash <> '', expires_at,
          max_downloads, download_count, revoked, created_at`,
		token, in.FileID, in.UserID, hash, expires, in.MaxDownloads).
		Scan(&sh.ID, &sh.Token, &sh.FileID, &sh.UserID, &sh.HasPassword, &exp,
			&sh.MaxDownloads, &sh.DownloadCount, &sh.Revoked, &sh.CreatedAt)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	sh.ExpiresAt = exp
	return &sh, nil
}

// Load 按 token 取分享(附带文件元信息)。
//
// 不做"是否可用"的判断 —— 那一步在 Authorize 里,因为它要根据**是否提供了密码**
// 决定错误类型(需密码 vs 已失效),混在一起会让落地页无法区分。
func (s *Service) Load(ctx context.Context, token string) (*Share, *Meta, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, nil, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少分享 token")
	}
	var sh Share
	var exp *time.Time
	var name, mime string
	var size int64
	var isDir bool
	err := s.DB.QueryRow(ctx, `
SELECT s.id, s.token, s.file_id::text, s.user_id::text, s.password_hash <> '', s.expires_at,
       s.max_downloads, s.download_count, s.revoked, s.created_at,
       f.name, f.size, f.is_dir, f.mime_type
  FROM shares s
  JOIN files f ON f.id = s.file_id
 WHERE s.token = $1`, token).
		Scan(&sh.ID, &sh.Token, &sh.FileID, &sh.UserID, &sh.HasPassword, &exp,
			&sh.MaxDownloads, &sh.DownloadCount, &sh.Revoked, &sh.CreatedAt,
			&name, &size, &isDir, &mime)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, apierr.NotFound("分享不存在或已被删除")
		}
		return nil, nil, apierr.Internal(err)
	}
	sh.ExpiresAt = exp
	meta := &Meta{
		Token: sh.Token, Name: name, Size: size, IsDir: isDir,
		NeedPassword: sh.HasPassword, DownloadCount: sh.DownloadCount,
		MaxDownloads: sh.MaxDownloads, MimeType: mime,
	}
	if exp != nil {
		meta.ExpiresAt = exp.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return &sh, meta, nil
}

// Authorize 校验链接可用性并**原子占用一次下载额度**,返回分享记录。
//
// 三条纪律都在这一条 UPDATE 里:
//   - **原子递增**:`download_count + 1` 与上限比较写在同一 WHERE 里,
//     并发下不可能有两个请求同时通过(先查后改会让"限 1 次"的链接被下多次);
//   - **过期/吊销在同一条件内**:否则会出现"检查通过、更新时已过期"的窗口;
//   - **返回递增后的行**:调用方据此回显剩余次数。
func (s *Service) Authorize(ctx context.Context, token, password string) (*Share, *Meta, error) {
	sh, meta, err := s.Load(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	// 密码校验放在最前:未通过密码就**不该**消耗下载次数
	// (否则攻击者可以用试错把正常用户的链接次数耗光 —— 一种低成本的拒绝服务)
	if sh.HasPassword {
		var hash string
		if qerr := s.DB.QueryRow(ctx,
			`SELECT password_hash FROM shares WHERE id = $1`, sh.ID).Scan(&hash); qerr != nil {
			return nil, nil, apierr.Internal(qerr)
		}
		if strings.TrimSpace(password) == "" {
			e := apierr.Unauthorized(apierr.CodeUnauthorized, "该分享需要访问密码")
			e.WithDetail("need_password", true)
			return nil, nil, e
		}
		if verr := s.Policy.Verify(hash, password); verr != nil {
			// 密码错误:明确区分于"链接失效",前端据此提示"密码错误"
			e := apierr.Unauthorized(apierr.CodeUnauthorized, "访问密码错误")
			e.WithDetail("need_password", true)
			e.WithDetail("reason", "bad_password")
			return nil, nil, e
		}
	}

	var exp *time.Time
	var count, max int
	err = s.DB.QueryRow(ctx, `
UPDATE shares
   SET download_count = download_count + 1
 WHERE token = $1
   AND revoked = false
   AND (expires_at IS NULL OR expires_at > now())
   AND (max_downloads = 0 OR download_count < max_downloads)
 RETURNING download_count, max_downloads, expires_at`, token).Scan(&count, &max, &exp)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 走到这里说明"链接存在但不可用":过期/超次/吊销
			return nil, nil, goneErr(sh)
		}
		return nil, nil, apierr.Internal(err)
	}
	sh.DownloadCount, sh.MaxDownloads, sh.ExpiresAt = count, max, exp
	meta.DownloadCount, meta.MaxDownloads = count, max
	return sh, meta, nil
}

// goneErr 给出**可区分**的失效原因(前端与分享者都需要知道是哪种)。
func goneErr(sh *Share) error {
	reason := "revoked"
	switch {
	case sh.Revoked:
		reason = "revoked"
	case sh.ExpiresAt != nil && !sh.ExpiresAt.After(time.Now()):
		reason = "expired"
	case sh.MaxDownloads > 0 && sh.DownloadCount >= sh.MaxDownloads:
		reason = "download_limit_reached"
	}
	e := apierr.SpaceGone("分享链接已不可用")
	e.Code = apierr.CodeResourceGone
	e.WithDetail("reason", reason)
	return e
}

// Revoke 吊销分享(幂等:重复吊销不报错)。
func (s *Service) Revoke(ctx context.Context, userID, shareID string) error {
	tag, err := s.DB.Exec(ctx,
		`UPDATE shares SET revoked = true WHERE id = $1 AND user_id = $2`, shareID, userID)
	if err != nil {
		return apierr.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		// 不存在或不是自己的:用 404 而不是 403,避免"这个 id 存在"的信息泄露
		return apierr.NotFound("分享不存在")
	}
	return nil
}

// ListMine 列出我创建的分享(便于分享者管理与吊销)。
func (s *Service) ListMine(ctx context.Context, userID string, limit int) ([]*Share, []*Meta, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.Query(ctx, `
SELECT s.id, s.token, s.file_id::text, s.user_id::text, s.password_hash <> '', s.expires_at,
       s.max_downloads, s.download_count, s.revoked, s.created_at,
       f.name, f.size, f.is_dir, f.mime_type
  FROM shares s JOIN files f ON f.id = s.file_id
 WHERE s.user_id = $1
 ORDER BY s.created_at DESC
 LIMIT $2`, userID, limit)
	if err != nil {
		return nil, nil, apierr.Internal(err)
	}
	defer rows.Close()
	var shares []*Share
	var metas []*Meta
	for rows.Next() {
		var sh Share
		var m Meta
		var exp *time.Time
		if err := rows.Scan(&sh.ID, &sh.Token, &sh.FileID, &sh.UserID, &sh.HasPassword, &exp,
			&sh.MaxDownloads, &sh.DownloadCount, &sh.Revoked, &sh.CreatedAt,
			&m.Name, &m.Size, &m.IsDir, &m.MimeType); err != nil {
			return nil, nil, apierr.Internal(err)
		}
		sh.ExpiresAt = exp
		m.Token = sh.Token
		m.NeedPassword = sh.HasPassword
		m.DownloadCount, m.MaxDownloads = sh.DownloadCount, sh.MaxDownloads
		if exp != nil {
			m.ExpiresAt = exp.UTC().Format("2006-01-02T15:04:05.000Z")
		}
		shares = append(shares, &sh)
		metas = append(metas, &m)
	}
	return shares, metas, rows.Err()
}

// CleanupExpired 删除**无引用价值**的分享行(过期且从未被下载 + 已吊销超过 30 天)。
//
// 为什么不一过期就删:分享者可能还想在列表里看到"它已经过期了"，
// 而访问者需要拿到 410(而不是 404),两者都依赖行还在。真正该删的是
// "早已吊销或过期且无人问津"的行 —— 它们只占空间,不会再被任何路径读到。
func (s *Service) CleanupExpired(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.DB.Exec(ctx, `
DELETE FROM shares
 WHERE (revoked = true AND created_at < now() - $1::interval)
    OR (expires_at IS NOT NULL AND expires_at < now() - $1::interval)`,
		fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
	if err != nil {
		return 0, apierr.Internal(err)
	}
	return tag.RowsAffected(), nil
}
