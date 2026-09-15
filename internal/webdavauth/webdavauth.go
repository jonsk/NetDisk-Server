// Package webdavauth 实现 WebDAV 通道的 Basic 认证(6.3:仅桌面客户端)。
//
// 背景:WebDAV 客户端(Windows 资源管理器「映射网络驱动器」、macOS Finder、
// Cyberduck)只会 Basic/Digest,不支持 Bearer。让它们每次请求都拿账密做一次
// bcrypt 校验会非常昂贵(bcrypt cost=12 约 100ms,且资源管理器会并发发几十个请求),
// 所以这里采用**换票**思路:
//
//	第一跳:Authorization: Basic base64(user:pass) → 校验账密 → 签发短期 Basic 会话令牌
//	后续跳:Authorization: Basic base64(user:<短期令牌>) → 直接查会话,不再碰 bcrypt
//
// 安全设计:
//   - 短期令牌有独立前缀,与真实口令不可能混淆(用户口令不允许含该前缀的校验在下层做,
//     这里靠"前缀 + 长度 + 只允许 base64url 字符集"三重约束)
//   - 令牌只在 Redis 存 SHA-256,库/缓存被读也不泄露凭据
//   - **不做长久有效的"应用密码"**:TTL 默认 12h,且随 token_version 自增立即失效
//   - 失败一律 401 + WWW-Authenticate(客户端据此弹密码框),且不区分"用户不存在/口令错"
package webdavauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
)

// TokenPrefix 是短期会话令牌前缀。
//
// 用途:让服务端在**不做 bcrypt 的前提下**就能判断"这是换来的会话令牌,不是口令",
// 从而避免把每个 WebDAV 请求都变成一次昂贵的 bcrypt 校验。
const TokenPrefix = "wd1_"

// Realm 出现在 WWW-Authenticate 里;客户端会把它显示在密码框标题上。
const Realm = "netdisk-webdav"

// DefaultTTL 是短期会话令牌的默认有效期(12h ≈ 桌面端一个工作日)。
//
// 单一来源:Authenticator.ttl() 与 RedisStore 都回退到它 ——
// 两处各写一遍 12*time.Hour 曾导致"Redis 里 key 活 12h,
// 而响应头告诉客户端 0s"这种自相矛盾(客户端会立刻认为凭据已过期)。
const DefaultTTL = 12 * time.Hour

// VerifyDBCredentials 由调用方注入的"校验真实账密"函数
// (通常是 authsvc.Login 的薄包装)。
//
// 必须返回该用户**当前**的 token_version:它会随会话一起落库,
// 后续请求靠它发现"期间改过密/被强制下线"(R-13)。
type VerifyDBCredentials func(ctx context.Context, username, password string) (userID string, tokenVersion int64, err error)

// Store 是短期会话的存储(生产实现是 Redis;测试可用内存实现)。
type Store interface {
	// Put 写入会话:key 为令牌的 SHA-256 hex,value 为 userID;ttl 后自动过期。
	Put(ctx context.Context, tokenHash, userID string, ttl time.Duration, tokenVersion int64) error
	// Get 读会话,返回 (userID, tokenVersion, found)。
	Get(ctx context.Context, tokenHash string) (userID string, tokenVersion int64, ok bool, err error)
	// Delete 删除会话(登出/吊销用)。不存在不算错误。
	Delete(ctx context.Context, tokenHash string) error
}

// ErrUnauthorized 是统一的 Basic 认证失败错误。
//
// 对外文案刻意不区分原因(防用户枚举),但**必须**带 WWW-Authenticate 头。
var ErrUnauthorized = errors.New("WebDAV 认证失败")

// Authenticator 是 Basic 认证入口。
type Authenticator struct {
	Store  Store
	Verify VerifyDBCredentials
	// TTL 是短期会话有效期(默认 12h,与桌面端一个工作日相当)
	TTL time.Duration
	// Now 便于测试注入时钟
	Now func() time.Time
}

// Result 是一次成功的认证结果。
type Result struct {
	UserID string
	// Token 非空表示本次请求**新签发**了短期令牌,调用方必须把它回给客户端
	// (见 WriteToken),否则客户端下一跳只能拿真实口令再走一次 bcrypt。
	Token string
	// FromSession 为 true 表示命中已有会话(未做账密校验)
	FromSession bool
	// TokenVersion 是签发该令牌时的用户 token_version。
	//
	// 调用方必须把它与**当前**用户行的 token_version 比对:不相等说明
	// 期间发生了改密/强制下线,会话应立即失效(与 R-13 的"静默退出"一致)。
	TokenVersion int64
}

// Authenticate 解析 Authorization: Basic 并完成认证。
//
// 两条路径:
//  1. 口令看起来像短期令牌(wd1_ 前缀)→ 查会话,不碰 bcrypt
//  2. 否则 → 用基本身份平台/本地账密校验一次,成功后签发短期令牌
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*Result, error) {
	username, secret, ok := ParseBasic(r.Header.Get("Authorization"))
	if !ok {
		return nil, ErrUnauthorized
	}
	if a.Store == nil || a.Verify == nil {
		return nil, fmt.Errorf("webdavauth: 未装配 Store/Verify")
	}

	// 路径 1:短期令牌
	if IsSessionToken(secret) {
		userID, tv, found, err := a.Store.Get(ctx, HashToken(secret))
		if err != nil {
			return nil, fmt.Errorf("查询 WebDAV 会话失败: %w", err)
		}
		if !found {
			return nil, ErrUnauthorized
		}
		return &Result{UserID: userID, FromSession: true, TokenVersion: tv}, nil
	}

	// 路径 2:真实账密
	userID, tokenVersion, err := a.Verify(ctx, username, secret)
	if err != nil || userID == "" {
		return nil, ErrUnauthorized
	}
	token, err := newSessionToken()
	if err != nil {
		return nil, err
	}
	if err := a.Store.Put(ctx, HashToken(token), userID, a.ttl(), tokenVersion); err != nil {
		return nil, fmt.Errorf("写入 WebDAV 会话失败: %w", err)
	}
	return &Result{UserID: userID, Token: token, TokenVersion: tokenVersion}, nil
}

// Revoke 删除某个短期令牌对应的会话(登出用)。
func (a *Authenticator) Revoke(ctx context.Context, token string) error {
	if a.Store == nil || !IsSessionToken(token) {
		return nil
	}
	return a.Store.Delete(ctx, HashToken(token))
}

func (a *Authenticator) ttl() time.Duration {
	if a.TTL > 0 {
		return a.TTL
	}
	return DefaultTTL
}

// TTLOrDefault 是对外暴露的有效期(handler 写 X-WebDAV-Token-Expires-In 用)。
//
// handler 必须用这个值而不是直接读 a.TTL —— 否则未显式配置时会回 "0 秒",
// 客户端据此判定凭据立即过期,表现就是"输对密码也进不去"。
func (a *Authenticator) TTLOrDefault() time.Duration { return a.ttl() }

// ---- Authorization 头解析 ----

// ParseBasic 解析 `Basic base64(user:pass)`。
//
// 额外约束(都是安全必需,不是洁癖):
//   - scheme 大小写不敏感(客户端实现五花八门)
//   - 用户名不得含 ':'(Basic 里 ':' 是分隔符,含冒号的用户名会造成歧义)
//   - 非 UTF-8 的字节直接拒绝,避免后续比较出现意外
func ParseBasic(header string) (username, password string, ok bool) {
	const prefix = "basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		// 有些客户端不带 padding,再试一次 raw 编码
		raw, err = base64.RawStdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
		if err != nil {
			return "", "", false
		}
	}
	s := string(raw)
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	username, password = s[:i], s[i+1:]
	if username == "" || strings.ContainsAny(username, "\x00\r\n") {
		return "", "", false
	}
	if strings.ContainsAny(password, "\x00\r\n") {
		return "", "", false
	}
	return username, password, true
}

// EncodeBasic 生成 Authorization 头(测试与客户端示例用)。
func EncodeBasic(username, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

// ---- 短期令牌 ----

// IsSessionToken 判断 secret 是否为本包签发的短期令牌。
//
// 必须足够严格,否则用户口令被误判为令牌会导致"直接查会话"路径被走到 ——
// 最坏情况是攻击者用一个形如 wd1_xxx 的口令去猜会话(几乎不可能命中,但仍要收紧)。
// 约束:固定前缀 + 总长度 + base64url 字符集。
func IsSessionToken(secret string) bool {
	if !strings.HasPrefix(secret, TokenPrefix) {
		return false
	}
	body := secret[len(TokenPrefix):]
	// 32 字节随机 → base64url 无 padding 恰为 43 字符
	if len(body) != 43 {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// HashToken 计算短期令牌的 SHA-256 hex(存储键)。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newSessionToken 生成短期会话令牌(43 字符 base64url 随机体 + 固定前缀)。
func newSessionToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 WebDAV 会话令牌失败: %w", err)
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// TokensEqual 恒定时间比较两个令牌(避免时序侧信道)。
func TokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- HTTP 辅助 ----

// WriteChallenge 输出 401 挑战。
//
// 必须同时给 `Basic` 与 `Bearer` 提示:Windows 资源管理器看到 Bearer 时会
// 尝试用当前 Windows 凭据;只给 Basic 时部分客户端(尤其老版 WebDAV 重定向器)
// 不弹密码框而直接失败。
func WriteChallenge(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf("Basic realm=%q, charset=\"UTF-8\"", Realm))
	if err == nil {
		err = ErrUnauthorized
	}
	apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "WebDAV 认证失败: 请提供 Basic 凭据"))
}

// WriteToken 在成功换票后把短期令牌回给客户端。
//
// 用自定义响应头而不是 Cookie:WebDAV 客户端对 Cookie 支持不一致,
// 而且 Cookie 会被浏览器场景自动携带,反而扩大暴露面。
func WriteToken(w http.ResponseWriter, token string, ttl time.Duration) {
	if token == "" {
		return
	}
	w.Header().Set("X-WebDAV-Token", token)
	w.Header().Set("X-WebDAV-Token-Expires-In", fmt.Sprintf("%d", int64(ttl.Seconds())))
}
