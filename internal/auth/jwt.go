// Package auth 实现 JWT 签发与校验,严格遵守架构文档 6.8 的四条强制编码规范:
//
//  1. 解析必须 WithValidMethods(["HS256"]) —— 算法混淆攻击的唯一闸门
//  2. aud 区分 web|desktop;iss/jti 必填;jti 用于 refresh 重放检测
//  3. 权限绝不写进 token(吊销/降权靠短 TTL + Redis refresh + token_version)
//  4. leeway=60s;nbf/exp 缺失即拒绝
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/reqctx"
)

// Audience 取值(R-14)。管理后台接口只接受 AudienceWeb,以此拦住 desktop token(2.7)。
const (
	AudienceWeb     = "web"
	AudienceDesktop = "desktop"
)

// TokenType 区分 access / refresh,防止用 refresh 直接访问业务接口。
type TokenType string

const (
	TypeAccess  TokenType = "access"
	TypeRefresh TokenType = "refresh"
)

// Claims 是网盘自有 JWT 载荷。
//
// **payload 只有四个自定义字段**:typ(令牌类型)、usr(用户名)、rol(平台角色)、
// tv(token_version)。单测 `TestTokenCarriesNoPermissions` 断言
// `permissions/perms/spaces/is_admin/scope` 一律**不出现**(R-13)。
//
// 关于 `rol`:它是**平台级**角色(super_admin/dept_admin/user),
// 只用于 `RequireRole` 这一处最小校验(谁能进管理后台);
// **空间级权限永远不来自 token** —— 每次请求都由服务端查 `space_members`
// 判定(4.3),这样成员变更才能即时生效、且不受 access token 15min 存活期拖累。
// 两者的区别很关键:把空间权限放进 token 就等于把"权限撤销延迟"写进了设计。
type Claims struct {
	jwt.RegisteredClaims
	Type         TokenType `json:"typ"`
	Username     string    `json:"usr,omitempty"`
	Role         string    `json:"rol,omitempty"`
	TokenVersion int64     `json:"tv"`
}

// Manager 负责签发与校验。
type Manager struct {
	cfg    config.JWT
	secret []byte
	rdb    *redis.Client
	// prefix 用于 refresh / 吊销键命名(6.7:只用 id,不用文件名)
	prefix string
}

// NewManager 构造。secret 由 env 注入(纪律 1),长度由 config.Validate 保证 >= 32。
func NewManager(cfg config.JWT, rdb *redis.Client, keyPrefix string) *Manager {
	return &Manager{cfg: cfg, secret: []byte(cfg.Secret), rdb: rdb, prefix: keyPrefix}
}

// TTLFor 返回该 audience 的 access/refresh 有效期。H5 渠道已随服务端拆除,
// 仅剩 web/desktop 两个分端,统一使用同一对 TTL。
func (m *Manager) TTLFor(audience string) (access, refresh time.Duration) {
	return m.cfg.AccessTTL.Std(), m.cfg.RefreshTTL.Std()
}

// IssueInput 是签发入参。
type IssueInput struct {
	UserID       string
	Username     string
	Role         string
	TokenVersion int64
	Audience     string
}

// Issued 是签发结果。
type Issued struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshIn    int    `json:"refresh_expires_in"`
	Audience     string `json:"audience"`
	// RefreshJTI 是 refresh 的 jti;调用方需要把它落库做权威账本(00002 迁移的 refresh_tokens)
	RefreshJTI   string    `json:"-"`
	RefreshUntil time.Time `json:"-"`
}

// Issue 同时签发 access 与 refresh,并把 refresh 的 jti 写入 Redis(支持重放检测与吊销)。
func (m *Manager) Issue(ctx context.Context, in IssueInput) (*Issued, error) {
	if in.UserID == "" {
		return nil, apierr.Internal(errors.New("auth: empty user id"))
	}
	if in.Audience == "" {
		in.Audience = AudienceWeb
	}
	accessTTL, refreshTTL := m.TTLFor(in.Audience)

	access, err := m.sign(in, TypeAccess, accessTTL)
	if err != nil {
		return nil, err
	}
	refresh, refreshJTI, err := m.signRefresh(in, refreshTTL)
	if err != nil {
		return nil, err
	}
	if m.rdb != nil {
		key := m.refreshKey(in.UserID, refreshJTI)
		if err := m.rdb.Set(ctx, key, in.TokenVersion, refreshTTL).Err(); err != nil {
			return nil, apierr.Internal(fmt.Errorf("store refresh: %w", err))
		}
	}
	return &Issued{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    int(accessTTL.Seconds()),
		RefreshIn:    int(refreshTTL.Seconds()),
		Audience:     in.Audience,
		RefreshJTI:   refreshJTI,
		RefreshUntil: time.Now().Add(refreshTTL),
	}, nil
}

func (m *Manager) sign(in IssueInput, typ TokenType, ttl time.Duration) (string, error) {
	tok, _, err := m.signWithJTI(in, typ, ttl, "")
	return tok, err
}

func (m *Manager) signRefresh(in IssueInput, ttl time.Duration) (string, string, error) {
	jti, err := newJTI()
	if err != nil {
		return "", "", err
	}
	tok, jti, err := m.signWithJTI(in, TypeRefresh, ttl, jti)
	return tok, jti, err
}

func (m *Manager) signWithJTI(in IssueInput, typ TokenType, ttl time.Duration, jti string) (string, string, error) {
	if jti == "" {
		var err error
		if jti, err = newJTI(); err != nil {
			return "", "", err
		}
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.cfg.Issuer,
			Subject:   in.UserID,
			Audience:  jwt.ClaimStrings{in.Audience},
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Type:         typ,
		Username:     in.Username,
		Role:         in.Role,
		TokenVersion: in.TokenVersion,
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
	if err != nil {
		return "", "", apierr.Internal(fmt.Errorf("sign %s: %w", typ, err))
	}
	return signed, jti, nil
}

// parser 统一构造:算法白名单 + leeway(纪律 1、4)。
func (m *Manager) parser() *jwt.Parser {
	return jwt.NewParser(
		jwt.WithValidMethods([]string{"HS256"}),
		jwt.WithIssuer(m.cfg.Issuer),
		jwt.WithLeeway(m.cfg.Leeway.Std()),
		jwt.WithExpirationRequired(), // 纪律 4:exp 缺失即拒绝
		jwt.WithIssuedAt(),
	)
}

// Verify 校验 access token 并转换为 Actor(实现 middleware.TokenVerifier)。
func (m *Manager) Verify(ctx context.Context, token string) (*reqctx.Actor, error) {
	claims, err := m.parse(token)
	if err != nil {
		return nil, err
	}
	if claims.Type != TypeAccess {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "token 类型错误:需要 access token")
	}
	aud := ""
	if len(claims.Audience) > 0 {
		aud = claims.Audience[0]
	}
	return &reqctx.Actor{
		UserID:       claims.Subject,
		Username:     claims.Username,
		Role:         claims.Role,
		Audience:     aud,
		TokenVersion: claims.TokenVersion,
	}, nil
}

// VerifyRefresh 校验 refresh token:类型正确 + Redis 中 jti 未被吊销/重用。
// 传入 expectedVersion 用于"静默退出":token_version 已自增则拒绝(6.8 纪律 3)。
func (m *Manager) VerifyRefresh(ctx context.Context, token string, expectedVersion int64) (*Claims, error) {
	claims, err := m.parse(token)
	if err != nil {
		return nil, err
	}
	if claims.Type != TypeRefresh {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "token 类型错误:需要 refresh token")
	}
	if m.rdb != nil {
		key := m.refreshKey(claims.Subject, claims.ID)
		got, err := m.rdb.Get(ctx, key).Int64()
		if errors.Is(err, redis.Nil) {
			return nil, apierr.Unauthorized(apierr.CodeTokenRevoked, "refresh 已失效,请重新登录")
		}
		if err != nil {
			return nil, apierr.Internal(fmt.Errorf("read refresh: %w", err))
		}
		if got != claims.TokenVersion {
			return nil, apierr.Unauthorized(apierr.CodeTokenRevoked, "登录状态已失效,请重新登录")
		}
	}
	if expectedVersion > 0 && claims.TokenVersion != expectedVersion {
		return nil, apierr.Unauthorized(apierr.CodeTokenRevoked, "登录状态已失效,请重新登录")
	}
	return claims, nil
}

// RevokeRefresh 吊销单条 refresh(登出)。
func (m *Manager) RevokeRefresh(ctx context.Context, userID, jti string) error {
	if m.rdb == nil {
		return nil
	}
	return m.rdb.Del(ctx, m.refreshKey(userID, jti)).Err()
}

func (m *Manager) parse(token string) (*Claims, error) {
	var claims Claims
	// 注意:必须用 m.parser(),禁止包级 jwt.Parse(纪律 1)
	if _, err := m.parser().ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected alg %q", t.Method.Alg())
		}
		return m.secret, nil
	}); err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, apierr.Unauthorized(apierr.CodeTokenExpired, "登录已过期")
		case errors.Is(err, jwt.ErrTokenSignatureInvalid):
			// 与"过期"分开返回,避免客户端把签名错误当成可刷新状态
			return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "登录凭据无效")
		case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
			return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "登录凭据不完整")
		default:
			return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "登录凭据无效")
		}
	}
	// iss/jti 必填(纪律 2);WithIssuer 已校验 iss,jti 在此显式检查
	if claims.ID == "" {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "登录凭据缺少 jti")
	}
	if len(claims.Audience) == 0 {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "登录凭据缺少 aud")
	}
	return &claims, nil
}

// refreshKey 命名规范:netdisk:auth:refresh:{userID}:{jti}
func (m *Manager) refreshKey(userID, jti string) string {
	return m.prefix + "auth:refresh:" + userID + ":" + jti
}

func newJTI() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
