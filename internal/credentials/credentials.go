// Package credentials 负责口令哈希与强度策略(6.3)。
//
// 决策:
//   - 哈希用 **bcrypt**(6.3 明确定稿),cost 取自 config,默认 12
//   - bcrypt 只取前 72 字节,**必须**在入口显式拒绝超长口令,
//     否则"两个不同长口令产生同一哈希"会造成难以理解的登录行为
//   - 强度策略在服务端强制执行,前端校验只作提示
package credentials

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

// bcryptMaxBytes 是 bcrypt 的输入上限。
const bcryptMaxBytes = 72

// MinPasswordBytes / MaxPasswordBytes 是口令长度边界。
const (
	MinPasswordBytes = 8
	MaxPasswordBytes = bcryptMaxBytes
)

var (
	ErrMismatch  = errors.New("credentials: 口令不匹配")
	ErrTooShort  = fmt.Errorf("credentials: 口令至少 %d 个字符", MinPasswordBytes)
	ErrTooLong   = fmt.Errorf("credentials: 口令不得超过 %d 字节(bcrypt 上限)", MaxPasswordBytes)
	ErrTooWeak   = errors.New("credentials: 口令强度不足")
	ErrEmptyHash = errors.New("credentials: 该账号未设置口令,请使用单点登录")
)

// Policy 是口令强度策略。
type Policy struct {
	// Cost bcrypt 代价因子;0 表示使用 bcrypt.DefaultCost
	Cost int
	// RequireDigit / RequireLetter / RequireUpper 为真时强制对应字符类
	RequireDigit  bool
	RequireLetter bool
	RequireUpper  bool
	// MinClasses 要求至少包含的字符类数量(0 = 不检查)
	MinClasses int
}

// DefaultPolicy 给出默认策略:8~72 字节 + 至少两类字符。
func DefaultPolicy() Policy {
	return Policy{Cost: 12, MinClasses: 2}
}

// Validate 校验口令强度;返回可直接展示给用户的错误。
func (p Policy) Validate(plain string) error {
	n := len(plain) // 按字节,bcrypt 限制是字节
	if n < MinPasswordBytes {
		return ErrTooShort
	}
	if n > MaxPasswordBytes {
		return ErrTooLong
	}
	classes := 0
	hasDigit, hasLower, hasUpper, hasOther := false, false, false, false
	for _, r := range plain {
		switch {
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsUpper(r):
			hasUpper = true
		default:
			hasOther = true
		}
	}
	if hasDigit {
		classes++
	}
	if hasLower || hasUpper {
		classes++
	}
	if hasOther {
		classes++
	}
	if p.RequireDigit && !hasDigit {
		return fmt.Errorf("%w:必须包含数字", ErrTooWeak)
	}
	if p.RequireLetter && !(hasLower || hasUpper) {
		return fmt.Errorf("%w:必须包含字母", ErrTooWeak)
	}
	if p.RequireUpper && !hasUpper {
		return fmt.Errorf("%w:必须包含大写字母", ErrTooWeak)
	}
	if p.MinClasses > 0 && classes < p.MinClasses {
		return fmt.Errorf("%w:至少包含 %d 类字符(数字/字母/符号)", ErrTooWeak, p.MinClasses)
	}
	return nil
}

// Hash 生成 bcrypt 哈希。
func (p Policy) Hash(plain string) (string, error) {
	if err := p.Validate(plain); err != nil {
		return "", err
	}
	cost := p.Cost
	if cost <= 0 {
		cost = bcrypt.DefaultCost
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", fmt.Errorf("credentials: 生成口令哈希失败: %w", err)
	}
	return string(h), nil
}

// Verify 校验口令。
//
// 返回 ErrEmptyHash 表示该账号没有本地口令(只能 SSO 登录)。
// 注意:调用方**不应**区分"用户不存在"与"口令错误"的对外文案(防用户枚举)。
func (p Policy) Verify(hash, plain string) error {
	if strings.TrimSpace(hash) == "" {
		return ErrEmptyHash
	}
	if len(plain) > bcryptMaxBytes {
		// 超长直接判失败,避免 bcrypt 静默截断造成误判
		return ErrMismatch
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)); err != nil {
		return ErrMismatch
	}
	return nil
}

// NeedsRehash 判断哈希是否需要用当前 cost 重新生成(登录成功后顺手升级)。
func (p Policy) NeedsRehash(hash string) bool {
	if hash == "" {
		return false
	}
	cost, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return true
	}
	want := p.Cost
	if want <= 0 {
		want = bcrypt.DefaultCost
	}
	return cost < want
}
