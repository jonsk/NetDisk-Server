// Package authsvc 编排本地账密登录、refresh 轮换与登出(6.3)。
//
// 与 internal/auth 的分工:
//   - internal/auth  : 纯 JWT 签发/校验(无状态、可单测)
//   - internal/authsvc: 业务编排(查库、验密、锁定、refresh 落库、token_version 联动)
package authsvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 默认锁定策略(6.3 + 4.4:失败尝试既要限速也要留审计)
const (
	defaultFailThreshold = 5
	defaultLockDuration  = 15 * time.Minute
)

// DB 抽象"读走池、写走事务"的组合需求。
//
// 由 db.DB 通过适配器满足(见 Adapter);单测可用替身实现。
type DB interface {
	repo.Querier
	InTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// Service 是认证用例入口。
type Service struct {
	Users  repo.UserRepo
	Spaces repo.SpaceRepo
	Tokens *auth.Manager
	Policy credentials.Policy
	DB     DB
	// FailThreshold / LockDuration 可按环境覆盖
	FailThreshold int
	LockDuration  time.Duration
	// Now 便于测试注入
	Now func() time.Time
}

// LoginInput 登录入参。
type LoginInput struct {
	Login    string // 用户名或邮箱
	Password string
	Audience string // web / h5 / desktop(R-14)
	// IP / UserAgent 仅用于审计
	IP        string
	UserAgent string
}

// LoginResult 登录结果。
type LoginResult struct {
	Token *auth.Issued
	User  *model.User
	// Space 个人空间(前端首屏需要拿空间 id 与配额)
	Space *model.Space
}

// ErrInvalidCredentials 是统一的"认证失败"错误 —— 对外**不区分**用户不存在与口令错误(防用户枚举)。
var ErrInvalidCredentials = apierr.Unauthorized(apierr.CodeUnauthorized, "用户名或密码错误")

// Login 执行本地账密登录。
func (s *Service) Login(ctx context.Context, in LoginInput) (*LoginResult, error) {
	if in.Audience == "" {
		in.Audience = auth.AudienceWeb
	}

	u, err := s.VerifyCredentials(ctx, in.Login, in.Password)
	if err != nil {
		return nil, err
	}

	// 登录成功后:清零失败计数 + 生成 jti 并落库 refresh(权威账本在 PG)
	token, jti, expiresAt, err := s.issue(ctx, u, in.Audience)
	if err != nil {
		return nil, err
	}
	if err := s.persistRefresh(ctx, jti, u.ID, in.Audience, u.TokenVersion, expiresAt); err != nil {
		return nil, err
	}
	if err := s.Users.LoginSucceeded(ctx, s.DB, u.ID); err != nil {
		return nil, apierr.Internal(err)
	}

	space, err := s.Spaces.PersonalOf(ctx, s.DB, u.ID)
	if err != nil {
		// 个人空间缺失属数据不完整,不应阻断登录,但要显式暴露
		space = nil
	}
	return &LoginResult{Token: token, User: u, Space: space}, nil
}

// VerifyCredentials 只做"账密是否正确"的判定,不签发任何令牌。
//
// 为什么单独暴露:WebDAV Basic 认证需要在**不签发 JWT、不写 refresh 表**的前提下
// 校验一次账密(见 internal/webdavauth 的换票设计)。把这段逻辑内联在 Login 里
// 会导致两处实现,而防枚举、锁定、停用检查任何一处漏掉都是安全缺口 ——
// 所以两条通道共用这一个函数。
//
// 返回的用户携带当前 token_version,调用方据此实现"改密即失效"。
func (s *Service) VerifyCredentials(ctx context.Context, login, password string) (*model.User, error) {
	now := s.now()

	creds, err := s.Users.GetCredentials(ctx, s.DB, login)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			// 防枚举:对外与"口令错误"完全一致;仍做一次哈希运算拉平耗时
			_, _ = s.Policy.Hash("dummy-password-for-timing")
			return nil, ErrInvalidCredentials
		}
		return nil, apierr.Internal(err)
	}
	if creds.Locked(now) {
		// 锁定态明确告知剩余时间,便于用户理解(不泄露账号是否存在之外的信息)
		return nil, apierr.RateLimited("账号已临时锁定,请稍后再试").
			WithDetail("locked_until", creds.LockedUntil.UTC().Format(time.RFC3339))
	}
	if creds.Status == model.StatusDisabled {
		return nil, apierr.Forbidden("账号已停用,请联系管理员")
	}
	if creds.Status == model.StatusPending {
		return nil, apierr.Forbidden("账号尚未激活")
	}

	if err := s.Policy.Verify(creds.PasswordHash, password); err != nil {
		if errors.Is(err, credentials.ErrEmptyHash) {
			return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "该账号未设置密码,请使用企业微信/钉钉扫码登录")
		}
		// 记录失败并在达阈值后锁定
		if _, ferr := s.Users.LoginFailed(ctx, s.DB, creds.UserID, s.failThreshold(), s.lockDuration()); ferr != nil {
			return nil, apierr.Internal(ferr)
		}
		return nil, ErrInvalidCredentials
	}

	u, err := s.Users.GetByID(ctx, s.DB, creds.UserID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	return u, nil
}

// Refresh 用 refresh token 换新 access(并轮换 refresh)。
func (s *Service) Refresh(ctx context.Context, refreshToken, audience string) (*auth.Issued, error) {
	claims, err := s.Tokens.VerifyRefresh(ctx, refreshToken, 0)
	if err != nil {
		return nil, err
	}
	if audience != "" && len(claims.Audience) > 0 && claims.Audience[0] != audience {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "令牌客户端类型不匹配")
	}

	// DB 为权威:校验 jti 存在且未吊销、未过期
	state, err := s.Users.GetRefresh(ctx, s.DB, claims.ID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.Unauthorized(apierr.CodeTokenRevoked, "登录状态已失效,请重新登录")
		}
		return nil, apierr.Internal(err)
	}
	if state.RevokedAt != nil {
		return nil, apierr.Unauthorized(apierr.CodeTokenRevoked, "登录状态已失效,请重新登录")
	}
	if s.now().After(state.ExpiresAt) {
		return nil, apierr.Unauthorized(apierr.CodeTokenExpired, "登录已过期,请重新登录")
	}

	u, err := s.Users.GetByID(ctx, s.DB, state.UserID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	// token_version 一致性(6.8 纪律 3):改密/停用/强制下线后旧 refresh 立即失效
	if u.TokenVersion != state.TokenVersion || claims.TokenVersion != state.TokenVersion {
		return nil, apierr.Unauthorized(apierr.CodeTokenRevoked, "登录状态已失效,请重新登录")
	}
	if !u.IsActive() {
		return nil, apierr.Forbidden("账号不可用")
	}

	// 轮换:旧的立即吊销,新的入库
	if err := s.Users.RevokeRefresh(ctx, s.DB, state.JTI); err != nil {
		return nil, apierr.Internal(err)
	}
	token, jti, expiresAt, err := s.issue(ctx, u, state.Audience)
	if err != nil {
		return nil, err
	}
	if err := s.persistRefresh(ctx, jti, u.ID, state.Audience, u.TokenVersion, expiresAt); err != nil {
		return nil, err
	}
	return token, nil
}

// Logout 吊销当前 refresh(登出)。
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	claims, err := s.Tokens.VerifyRefresh(ctx, refreshToken, 0)
	if err != nil {
		// 已失效的令牌登出视为成功(幂等)
		var ae *apierr.Error
		if errors.As(err, &ae) && ae.Code == apierr.CodeTokenRevoked {
			return nil
		}
		return err
	}
	if err := s.Users.RevokeRefresh(ctx, s.DB, claims.ID); err != nil {
		return apierr.Internal(err)
	}
	_ = s.Tokens.RevokeRefresh(ctx, claims.Subject, claims.ID)
	return nil
}

// RevokeAllSessions 强制下线(改密、停用、管理员操作)。
//
// 同时做两件事:自增 token_version(access 立即失效)+ 吊销全部 refresh。
func (s *Service) RevokeAllSessions(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := s.Users.BumpTokenVersion(ctx, tx, userID); err != nil {
		return err
	}
	if _, err := s.Users.RevokeAllForUser(ctx, tx, userID); err != nil {
		return err
	}
	return nil
}

// SetPassword 改密并强制所有会话下线(6.8 纪律 3)。
func (s *Service) SetPassword(ctx context.Context, userID, newPassword string) error {
	hash, err := s.Policy.Hash(newPassword)
	if err != nil {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "%s", err.Error())
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := s.Users.SetPassword(ctx, tx, userID, hash); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("用户不存在")
			}
			return err
		}
		_, err := s.Users.RevokeAllForUser(ctx, tx, userID)
		return err
	})
}

func (s *Service) issue(ctx context.Context, u *model.User, audience string) (*auth.Issued, string, time.Time, error) {
	out, err := s.Tokens.Issue(ctx, auth.IssueInput{
		UserID:       u.ID,
		Username:     u.Username,
		Role:         u.Role,
		TokenVersion: u.TokenVersion,
		Audience:     audience,
	})
	if err != nil {
		return nil, "", time.Time{}, err
	}
	// jti 由 Manager 直接返回,无需解析 token(避免重复解密与额外失败面)
	return out, out.RefreshJTI, out.RefreshUntil, nil
}

func (s *Service) persistRefresh(ctx context.Context, jti, userID, audience string, tv int64, expiresAt time.Time) error {
	if err := s.Users.SaveRefresh(ctx, s.DB, jti, userID, audience, tv, expiresAt); err != nil {
		return apierr.Internal(fmt.Errorf("保存 refresh 失败: %w", err))
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) failThreshold() int {
	if s.FailThreshold > 0 {
		return s.FailThreshold
	}
	return defaultFailThreshold
}

func (s *Service) lockDuration() time.Duration {
	if s.LockDuration > 0 {
		return s.LockDuration
	}
	return defaultLockDuration
}

// newJTI 生成 16 字节随机 id(与 auth.Manager 内部算法一致,便于对齐日志)。
func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
