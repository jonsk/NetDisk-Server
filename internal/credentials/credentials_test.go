package credentials_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/credentials"
)

func TestValidatePolicy(t *testing.T) {
	p := credentials.DefaultPolicy() // 8~72 字节 + 至少 2 类字符

	ok := []string{"abc12345", "P@ssw0rd", "密码密码1234", "aB3!xyz9"}
	for _, s := range ok {
		if err := p.Validate(s); err != nil {
			t.Errorf("%q 应通过校验: %v", s, err)
		}
	}

	bad := []struct {
		pw   string
		want error
	}{
		{"short1", credentials.ErrTooShort},
		{"12345678", credentials.ErrTooWeak}, // 只有数字一类
		{"aaaaaaaa", credentials.ErrTooWeak}, // 只有字母一类
		{strings.Repeat("a", credentials.MaxPasswordBytes+1), credentials.ErrTooLong},
	}
	for _, tc := range bad {
		err := p.Validate(tc.pw)
		if err == nil {
			t.Errorf("%q 应被拒绝", tc.pw)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%q 错误类型应为 %v, got %v", tc.pw, tc.want, err)
		}
	}
}

// bcrypt 只处理前 72 字节:必须在入口拒绝超长,而不是静默截断
func TestRejectOverlongInsteadOfTruncating(t *testing.T) {
	p := credentials.DefaultPolicy()
	long := strings.Repeat("A", 72) + "different-tail-1"
	hash, err := p.Hash(long)
	if err == nil {
		t.Fatalf("超长口令必须被拒绝,却得到哈希 %q", hash)
	}
	if !errors.Is(err, credentials.ErrTooLong) {
		t.Errorf("应为 ErrTooLong, got %v", err)
	}
}

func TestHashAndVerify(t *testing.T) {
	p := credentials.DefaultPolicy()
	p.Cost = 4 // 测试用低成本,加速

	hash, err := p.Hash("P@ssw0rd123")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Errorf("应为 bcrypt 哈希, got %q", hash)
	}
	if err := p.Verify(hash, "P@ssw0rd123"); err != nil {
		t.Errorf("正确口令应通过: %v", err)
	}
	if err := p.Verify(hash, "P@ssw0rd124"); !errors.Is(err, credentials.ErrMismatch) {
		t.Errorf("错误口令应为 ErrMismatch, got %v", err)
	}
	// 超长输入必须直接判失败,不能因截断而被接受
	if err := p.Verify(hash, "P@ssw0rd123"+strings.Repeat("x", 80)); !errors.Is(err, credentials.ErrMismatch) {
		t.Errorf("超长输入应判失败, got %v", err)
	}
}

// 空哈希 = 该账号只能用 SSO 登录,调用方据此提示而不是"密码错误"
func TestEmptyHashMeansSSOOnly(t *testing.T) {
	p := credentials.DefaultPolicy()
	if err := p.Verify("", "whatever1"); !errors.Is(err, credentials.ErrEmptyHash) {
		t.Errorf("空哈希应为 ErrEmptyHash, got %v", err)
	}
}

// 哈希必须是随机的(同一口令两次哈希不同),防彩虹表
func TestHashIsSalted(t *testing.T) {
	p := credentials.DefaultPolicy()
	p.Cost = 4
	h1, _ := p.Hash("P@ssw0rd123")
	h2, _ := p.Hash("P@ssw0rd123")
	if h1 == h2 {
		t.Error("同一口令两次哈希应不同(bcrypt 自带盐)")
	}
}

func TestNeedsRehash(t *testing.T) {
	low := credentials.Policy{Cost: 4, MinClasses: 2}
	hash, err := low.Hash("P@ssw0rd123")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	high := credentials.Policy{Cost: 12, MinClasses: 2}
	if !high.NeedsRehash(hash) {
		t.Error("cost 提升后应需要重新哈希")
	}
	if low.NeedsRehash(hash) {
		t.Error("同 cost 不应需要重新哈希")
	}
	if !high.NeedsRehash("not-a-bcrypt-hash") {
		t.Error("非法哈希应视为需要重建")
	}
}
