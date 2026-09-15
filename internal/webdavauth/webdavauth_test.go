package webdavauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/webdavauth"
)

// memStore 是 Store 的内存实现(单测不依赖 Redis 进程)。
type memStore struct {
	mu   sync.Mutex
	data map[string]memEntry
	// puts 记录写入次数,用于证明"令牌路径没有重复换票"
	puts int
}

type memEntry struct {
	userID string
	tv     int64
	expire time.Time
}

func newMemStore() *memStore { return &memStore{data: map[string]memEntry{}} }

func (m *memStore) Put(_ context.Context, h, userID string, ttl time.Duration, tv int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts++
	m.data[h] = memEntry{userID: userID, tv: tv, expire: time.Now().Add(ttl)}
	return nil
}

func (m *memStore) Get(_ context.Context, h string) (string, int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[h]
	if !ok || time.Now().After(e.expire) {
		return "", 0, false, nil
	}
	return e.userID, e.tv, true, nil
}

func (m *memStore) Delete(_ context.Context, h string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, h)
	return nil
}

// countingVerify 记录账密校验被调用的次数。
// 这是本包最核心的性能断言:WebDAV 每请求都会带 Basic 头,
// 若每次都做 bcrypt,资源管理器并发几十个请求就会把 CPU 打满。
func countingVerify(t *testing.T, calls *int, userID string) webdavauth.VerifyDBCredentials {
	t.Helper()
	return func(_ context.Context, username, password string) (string, int64, error) {
		*calls++
		if username != "alice" || password != "correct-horse" {
			return "", 0, errors.New("bad credentials")
		}
		return userID, 7, nil
	}
}

func basicReq(user, pass string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/webdav/", nil)
	r.Header.Set("Authorization", webdavauth.EncodeBasic(user, pass))
	return r
}

// ---- Authorization 头解析 ----

func TestParseBasic(t *testing.T) {
	cases := []struct {
		name             string
		header           string
		wantUser, wantPS string
		wantOK           bool
	}{
		{"标准", webdavauth.EncodeBasic("alice", "s3cret"), "alice", "s3cret", true},
		{"scheme 大小写不敏感", "bAsIc " + strings.TrimPrefix(webdavauth.EncodeBasic("alice", "s3cret"), "Basic "), "alice", "s3cret", true},
		{"口令含冒号", webdavauth.EncodeBasic("alice", "a:b:c"), "alice", "a:b:c", true},
		{"空口令", webdavauth.EncodeBasic("alice", ""), "alice", "", true},
		{"无冒号", "Basic " + "YWxpY2U=", "", "", false}, // base64("alice")
		{"空用户名", "Basic " + "OnBhc3M=", "", "", false},
		{"非 Basic", "Bearer abc", "", "", false},
		{"非法 base64", "Basic !!!not-base64!!!", "", "", false},
		{"空头", "", "", "", false},
		{"仅 scheme", "Basic ", "", "", false},
		{"用户名含 CRLF 注入", "Basic " + "YTpiDQpYLUhlYWRlcjogYQ==", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, p, ok := webdavauth.ParseBasic(c.header)
			if ok != c.wantOK {
				t.Fatalf("ok 期望 %v 实际 %v", c.wantOK, ok)
			}
			if !c.wantOK {
				return
			}
			if u != c.wantUser || p != c.wantPS {
				t.Fatalf("期望 (%q,%q),实际 (%q,%q)", c.wantUser, c.wantPS, u, p)
			}
		})
	}
}

// ---- 换票 ----

func TestAuthenticateIssuesToken(t *testing.T) {
	calls := 0
	st := newMemStore()
	a := &webdavauth.Authenticator{Store: st, Verify: countingVerify(t, &calls, "u-1")}

	res, err := a.Authenticate(context.Background(), basicReq("alice", "correct-horse"))
	if err != nil {
		t.Fatalf("首次认证应成功: %v", err)
	}
	if res.UserID != "u-1" || res.Token == "" || res.FromSession {
		t.Fatalf("首次认证应签发令牌: %+v", res)
	}
	if res.TokenVersion != 7 {
		t.Fatalf("应带上用户当前 token_version,实际 %d", res.TokenVersion)
	}
	if !webdavauth.IsSessionToken(res.Token) {
		t.Fatalf("签发的令牌不符合会话令牌格式: %q", res.Token)
	}
	if calls != 1 {
		t.Fatalf("账密校验应恰好 1 次,实际 %d", calls)
	}
}

// 核心性能断言:同一会话内的后续请求**不再**触发账密校验。
func TestSessionTokenSkipsBcrypt(t *testing.T) {
	calls := 0
	st := newMemStore()
	a := &webdavauth.Authenticator{Store: st, Verify: countingVerify(t, &calls, "u-1")}

	first, err := a.Authenticate(context.Background(), basicReq("alice", "correct-horse"))
	if err != nil {
		t.Fatalf("首次认证失败: %v", err)
	}
	// 模拟资源管理器连发 50 个请求
	for i := 0; i < 50; i++ {
		res, err := a.Authenticate(context.Background(), basicReq("alice", first.Token))
		if err != nil {
			t.Fatalf("第 %d 次会话认证应成功: %v", i, err)
		}
		if !res.FromSession {
			t.Fatalf("第 %d 次应命中会话", i)
		}
		if res.UserID != "u-1" || res.TokenVersion != 7 {
			t.Fatalf("第 %d 次会话信息不对: %+v", i, res)
		}
	}
	if calls != 1 {
		t.Fatalf("账密校验总次数应为 1(仅首次),实际 %d", calls)
	}
}

func TestAuthenticateRejectsWrongPassword(t *testing.T) {
	calls := 0
	a := &webdavauth.Authenticator{Store: newMemStore(), Verify: countingVerify(t, &calls, "u-1")}
	_, err := a.Authenticate(context.Background(), basicReq("alice", "wrong"))
	if !errors.Is(err, webdavauth.ErrUnauthorized) {
		t.Fatalf("错误口令应 ErrUnauthorized,实际 %v", err)
	}
	_, err = a.Authenticate(context.Background(), basicReq("bob", "correct-horse"))
	if !errors.Is(err, webdavauth.ErrUnauthorized) {
		t.Fatalf("未知用户应 ErrUnauthorized(不泄露存在性),实际 %v", err)
	}
}

func TestAuthenticateRejectsMissingHeader(t *testing.T) {
	calls := 0
	a := &webdavauth.Authenticator{Store: newMemStore(), Verify: countingVerify(t, &calls, "u-1")}
	_, err := a.Authenticate(context.Background(), httptest.NewRequest(http.MethodGet, "/webdav/", nil))
	if !errors.Is(err, webdavauth.ErrUnauthorized) {
		t.Fatalf("无 Authorization 应 ErrUnauthorized,实际 %v", err)
	}
	if calls != 0 {
		t.Fatalf("无凭据不应触发账密校验,实际 %d 次", calls)
	}
}

// 未知/伪造的会话令牌必须被拒,且**不得**回退到"当作口令再 bcrypt 一次"
// (否则攻击者可以用随机 wd1_ 令牌刷爆 CPU)。
func TestUnknownSessionTokenRejectedWithoutBcrypt(t *testing.T) {
	calls := 0
	a := &webdavauth.Authenticator{Store: newMemStore(), Verify: countingVerify(t, &calls, "u-1")}
	fake := webdavauth.TokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	_, err := a.Authenticate(context.Background(), basicReq("alice", fake))
	if !errors.Is(err, webdavauth.ErrUnauthorized) {
		t.Fatalf("伪造令牌应 ErrUnauthorized,实际 %v", err)
	}
	if calls != 0 {
		t.Fatalf("伪造令牌不得触发账密校验,实际 %d 次", calls)
	}
}

// 撤销后令牌立即失效(登出场景)。
func TestRevoke(t *testing.T) {
	calls := 0
	a := &webdavauth.Authenticator{Store: newMemStore(), Verify: countingVerify(t, &calls, "u-1")}
	res, err := a.Authenticate(context.Background(), basicReq("alice", "correct-horse"))
	if err != nil {
		t.Fatalf("首次认证失败: %v", err)
	}
	if err := a.Revoke(context.Background(), res.Token); err != nil {
		t.Fatalf("Revoke 失败: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), basicReq("alice", res.Token)); !errors.Is(err, webdavauth.ErrUnauthorized) {
		t.Fatalf("撤销后应 ErrUnauthorized,实际 %v", err)
	}
}

// ---- 会话令牌格式 ----

func TestIsSessionToken(t *testing.T) {
	cases := map[string]bool{
		webdavauth.TokenPrefix + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA": true,
		webdavauth.TokenPrefix + strings.Repeat("a", 42):                       false, // 太短
		webdavauth.TokenPrefix + strings.Repeat("a", 44):                       false, // 太长
		webdavauth.TokenPrefix + strings.Repeat("a", 42) + "!":                 false, // 非法字符
		"wd2_" + strings.Repeat("a", 43):                                       false, // 前缀不对
		"correct-horse":                                                        false, // 普通口令
		"":                                                                     false,
	}
	for tok, want := range cases {
		if got := webdavauth.IsSessionToken(tok); got != want {
			t.Errorf("IsSessionToken(%q) = %v,期望 %v", tok, got, want)
		}
	}
}

// 哈希必须稳定且不等于明文(存储键安全)。
func TestHashToken(t *testing.T) {
	tok := webdavauth.TokenPrefix + strings.Repeat("z", 43)
	h1, h2 := webdavauth.HashToken(tok), webdavauth.HashToken(tok)
	if h1 != h2 {
		t.Fatal("同一令牌的哈希必须稳定")
	}
	if len(h1) != 64 {
		t.Fatalf("应为 sha256 hex(64),实际 %d", len(h1))
	}
	if strings.Contains(h1, tok) {
		t.Fatal("哈希不得包含明文")
	}
	if webdavauth.HashToken(tok+"x") == h1 {
		t.Fatal("不同令牌不应产生相同哈希")
	}
}

// ---- 401 挑战 ----

func TestWriteChallengeSetsWWWAuthenticate(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/webdav/", nil)
	webdavauth.WriteChallenge(rec, req, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("应 401,实际 %d", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(wa, "Basic ") || !strings.Contains(wa, webdavauth.Realm) {
		t.Fatalf("必须给出 Basic realm 挑战,实际 %q", wa)
	}
	if !strings.Contains(rec.Body.String(), "unauthorized") {
		t.Fatalf("应返回结构化错误体,实际 %s", rec.Body.String())
	}
}

func TestWriteTokenHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	webdavauth.WriteToken(rec, "wd1_abc", 12*time.Hour)
	if rec.Header().Get("X-WebDAV-Token") != "wd1_abc" {
		t.Fatalf("应回短期令牌,实际 %q", rec.Header().Get("X-WebDAV-Token"))
	}
	if rec.Header().Get("X-WebDAV-Token-Expires-In") != "43200" {
		t.Fatalf("应回有效期秒数,实际 %q", rec.Header().Get("X-WebDAV-Token-Expires-In"))
	}
	// 空令牌不应写入任何头
	rec2 := httptest.NewRecorder()
	webdavauth.WriteToken(rec2, "", time.Hour)
	if rec2.Header().Get("X-WebDAV-Token") != "" {
		t.Fatal("空令牌不应写响应头")
	}
}

// 未装配 Store/Verify 时必须报错而不是静默通过(否则是一个认证绕过)。
func TestUnassembledFailsClosed(t *testing.T) {
	a := &webdavauth.Authenticator{}
	_, err := a.Authenticate(context.Background(), basicReq("alice", "x"))
	if err == nil {
		t.Fatal("未装配时必须失败,不能放行")
	}
	if errors.Is(err, webdavauth.ErrUnauthorized) {
		t.Fatalf("装配缺失应区别于凭据错误(便于运维定位),实际 %v", err)
	}
}
