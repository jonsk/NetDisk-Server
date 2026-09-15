package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/config"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 字节

func newManager(t *testing.T) (*auth.Manager, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cfg := config.Default().JWT
	cfg.Secret = testSecret
	return auth.NewManager(cfg, rdb, "netdisk:"), mr
}

func issue(t *testing.T, m *auth.Manager, aud string) *auth.Issued {
	t.Helper()
	out, err := m.Issue(context.Background(), auth.IssueInput{
		UserID: "11111111-1111-7111-8111-111111111111", Username: "alice",
		Role: "user", TokenVersion: 1, Audience: aud,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return out
}

// 纪律 2:aud 分端;access 必须能验,且 Actor 字段正确
func TestIssueAndVerifyAccess(t *testing.T) {
	m, _ := newManager(t)
	out := issue(t, m, auth.AudienceDesktop)
	if out.ExpiresIn != int((15 * time.Minute).Seconds()) {
		t.Errorf("desktop access TTL 应为 15min,got %ds", out.ExpiresIn)
	}
	actor, err := m.Verify(context.Background(), out.AccessToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if actor.Username != "alice" || actor.Audience != auth.AudienceDesktop {
		t.Errorf("Actor 字段错误: %+v", actor)
	}
}

// 纪律 1(最重要):算法白名单 —— 用 RS256 自签的 token 必须被拒
func TestRejectAlgorithmConfusion(t *testing.T) {
	m, _ := newManager(t)

	// 攻击者用 RSA 私钥自签,试图冒充 HS256
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	now := time.Now()
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    "netdisk",
		Subject:   "attacker",
		Audience:  jwt.ClaimStrings{auth.AudienceWeb},
		ID:        "forged",
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	})
	signed, err := forged.SignedString(key)
	if err != nil {
		t.Fatalf("sign rs256: %v", err)
	}
	if _, err := m.Verify(context.Background(), signed); err == nil {
		t.Fatal("RS256 伪造 token 必须被拒绝(算法白名单失效)")
	}

	// 攻击者把 alg 改成 none
	noneTok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{
		Issuer: "netdisk", Subject: "attacker", ID: "x",
		Audience:  jwt.ClaimStrings{auth.AudienceWeb},
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	})
	unsigned, err := noneTok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := m.Verify(context.Background(), unsigned); err == nil {
		t.Fatal("alg=none 必须被拒绝")
	}
}

// 纪律 4:过期即拒;但 leeway=60s 内的轻微偏差应放行(Windows 域时钟偏差)
func TestExpiryAndLeeway(t *testing.T) {
	m, _ := newManager(t)
	cfg := config.Default().JWT
	cfg.Secret = testSecret
	cfg.Leeway = config.Duration(60 * time.Second)
	m2 := auth.NewManager(cfg, nil, "")

	sign := func(exp time.Time) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &auth.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: "netdisk", Subject: "u", ID: "j",
				Audience:  jwt.ClaimStrings{auth.AudienceWeb},
				IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
				NotBefore: jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
				ExpiresAt: jwt.NewNumericDate(exp),
			},
			Type: auth.TypeAccess,
		})
		s, err := tok.SignedString([]byte(testSecret))
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}

	// 已过期 30s:落在 60s leeway 内 → 放行
	if _, err := m2.Verify(context.Background(), sign(time.Now().Add(-30*time.Second))); err != nil {
		t.Errorf("leeway 内的轻微过期应放行: %v", err)
	}
	// 已过期 10 分钟:应拒绝且错误码为 token_expired
	_, err := m2.Verify(context.Background(), sign(time.Now().Add(-10*time.Minute)))
	if err == nil {
		t.Fatal("明显过期必须被拒绝")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeTokenExpired {
		t.Errorf("过期应返回 token_expired,got %v", err)
	}
	_ = m
}

// 纪律 4:exp 缺失即拒绝
func TestMissingExpRejected(t *testing.T) {
	m, _ := newManager(t)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, &auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "netdisk", Subject: "u", ID: "j",
			Audience: jwt.ClaimStrings{auth.AudienceWeb},
		},
		Type: auth.TypeAccess,
	})
	s, _ := tok.SignedString([]byte(testSecret))
	if _, err := m.Verify(context.Background(), s); err == nil {
		t.Fatal("缺少 exp 必须被拒绝")
	}
}

// 纪律 2:refresh 不能当 access 用
func TestRefreshCannotBeUsedAsAccess(t *testing.T) {
	m, _ := newManager(t)
	out := issue(t, m, auth.AudienceWeb)
	if _, err := m.Verify(context.Background(), out.RefreshToken); err == nil {
		t.Fatal("refresh token 不得通过 access 校验")
	}
	// 反向:access 不能当 refresh
	if _, err := m.VerifyRefresh(context.Background(), out.AccessToken, 1); err == nil {
		t.Fatal("access token 不得通过 refresh 校验")
	}
}

// 纪律 3:吊销 refresh(token_version 变化 / 登出)
func TestRefreshRevocation(t *testing.T) {
	m, mr := newManager(t)
	ctx := context.Background()
	out := issue(t, m, auth.AudienceWeb)

	claims, err := m.VerifyRefresh(ctx, out.RefreshToken, 1)
	if err != nil {
		t.Fatalf("首次校验应通过: %v", err)
	}
	// ① 登出吊销
	if err := m.RevokeRefresh(ctx, claims.Subject, claims.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := m.VerifyRefresh(ctx, out.RefreshToken, 1); err == nil {
		t.Fatal("已吊销的 refresh 必须被拒绝")
	}

	// ② token_version 自增(2.7/6.8:静默退出)
	out2 := issue(t, m, auth.AudienceWeb)
	if _, err := m.VerifyRefresh(ctx, out2.RefreshToken, 2); err == nil {
		t.Fatal("token_version 不匹配的 refresh 必须被拒绝")
	}
	// ③ Redis 中的数据丢失(refresh 键过期)也必须拒绝,而不是放行
	mr.FlushAll()
	if _, err := m.VerifyRefresh(ctx, out2.RefreshToken, 1); err == nil {
		t.Fatal("Redis 中无记录时必须拒绝(refresh 续命靠存储)")
	}
}

// 纪律 2:jti 必填;iss 必须匹配
func TestMissingJTIAndWrongIssuer(t *testing.T) {
	m, _ := newManager(t)
	now := time.Now()
	base := jwt.RegisteredClaims{
		Issuer: "netdisk", Subject: "u",
		Audience:  jwt.ClaimStrings{auth.AudienceWeb},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
	}

	// 无 jti
	noJTI := base
	s1, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, &auth.Claims{RegisteredClaims: noJTI, Type: auth.TypeAccess}).SignedString([]byte(testSecret))
	if _, err := m.Verify(context.Background(), s1); err == nil {
		t.Fatal("缺少 jti 必须被拒绝")
	}

	// 错误 issuer
	wrongIss := base
	wrongIss.ID = "j1"
	wrongIss.Issuer = "someone-else"
	s2, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, &auth.Claims{RegisteredClaims: wrongIss, Type: auth.TypeAccess}).SignedString([]byte(testSecret))
	if _, err := m.Verify(context.Background(), s2); err == nil {
		t.Fatal("issuer 不匹配必须被拒绝")
	}
}

// 纪律 3:token 内不得出现权限列表字段
func TestTokenCarriesNoPermissions(t *testing.T) {
	m, _ := newManager(t)
	out := issue(t, m, auth.AudienceWeb)
	parts := strings.Split(out.AccessToken, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 结构异常")
	}
	payload, err := jwt.NewParser().DecodeSegment(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	body := string(payload)
	for _, forbidden := range []string{"permissions", "perms", "spaces", "is_admin", "scope"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("token 载荷不应包含 %q: %s", forbidden, body)
		}
	}
	for _, required := range []string{"typ", "aud", "iss", "jti", "exp", "tv"} {
		if !strings.Contains(body, required) {
			t.Errorf("token 载荷应包含 %q: %s", required, body)
		}
	}
}
