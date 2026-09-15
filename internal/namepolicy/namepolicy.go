// Package namepolicy 是文件名的**唯一裁判**(6.7)。
//
// 总原则:服务器是唯一裁判,规则取"最严平台(Windows)的合法字符集"作为全网标准。
// 所有入口(创建/重命名/移动/share-to-space/TUS 完成钩子)必须调用本包,**禁止各自实现**。
//
// 规则(6.7 表):
//  1. NFC 归一(a + 组合符 与 ä 视为同一字符)
//  2. 禁止 / \ : * ? " < > | 与 0x00-0x1F、0x7F、Unicode Cc/Cf
//  3. 禁止尾部 . 与空格(NTFS 会静默剥离)
//  4. 禁止 Win32 保留名(CON/PRN/AUX/NUL/COM1-9/LPT1-9,含带扩展名形式)
//  5. UTF-8 长度 ≤ MaxBytes(默认 240)
//  6. 判重由数据库 (space_id,parent_id,lower(name)) 唯一索引负责
//  7. 全角符号**放行**(不同码点,文件系统安全)
//  8. 空格/中文/Emoji 允许
//
// **拒绝而非清洗**:非法名一律返回结构化错误,绝不静默改名入库。
package namepolicy

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// 违规类型(便于客户端做不同提示与"一键转全角")
const (
	RuleForbiddenChar = 2  // 禁止字符(半角)
	RuleTrailingDot   = 3  // 尾部点/空格
	RuleReservedName  = 4  // Win32 保留名
	RuleTooLong       = 5  // 超长
	RuleEmpty         = 9  // 空名(补充规则)
	RuleDotEntry      = 10 // "." / ".."
	RuleAllSpace      = 11 // 全空白
)

// Violation 描述一次拒绝的细节(6.7:违规字符位置 + 违反规则号 + 建议替换名)。
type Violation struct {
	Rule int `json:"rule"`
	// Position 是**码点**下标(不是字节下标),便于前端高亮
	Position int `json:"position,omitempty"`
	// Char 是触发违规的字符
	Char string `json:"char,omitempty"`
	// Suggestion 是建议替换后的名字(全角等价),空表示无建议
	Suggestion string `json:"suggestion,omitempty"`
	Message    string `json:"message"`
}

func (v *Violation) Error() string {
	if v.Char != "" {
		return fmt.Sprintf("文件名非法(规则 %d,位置 %d,字符 %q): %s", v.Rule, v.Position, v.Char, v.Message)
	}
	return fmt.Sprintf("文件名非法(规则 %d): %s", v.Rule, v.Message)
}

// Policy 是命名策略参数(来自 config.Policy,避免各处硬编码)。
type Policy struct {
	// MaxBytes UTF-8 字节上限(默认 240)
	MaxBytes int
	// MaxPathBytes 累计路径上限(R-11)
	MaxPathBytes int
	// MaxDepth 目录深度上限
	MaxDepth int
}

// Default 与 config 默认值保持一致。
func Default() Policy {
	return Policy{MaxBytes: 240, MaxPathBytes: 240, MaxDepth: 31}
}

// forbidden 是半角禁用字符及其全角建议替换(6.7:客户端可一键替换为视觉等价全角)。
var forbidden = map[rune]rune{
	'/':  '／',
	'\\': '＼',
	':':  '：',
	'*':  '＊',
	'?':  '？',
	'"':  '＂',
	'<':  '＜',
	'>':  '＞',
	'|':  '｜',
}

// reservedNames 是 Win32 保留名(不含扩展名比较,大小写不敏感)。
var reservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// Normalize 做 NFC 归一(入库、比较、返回一律使用归一后的值)。
func Normalize(name string) string { return norm.NFC.String(name) }

// Validate 校验单个文件名(不含路径分隔)。
//
// 返回 (*Violation, false) 表示非法;建议名在 Violation.Suggestion 中(可能为空)。
func (p Policy) Validate(name string) (*Violation, bool) {
	if p.MaxBytes <= 0 {
		p.MaxBytes = 240
	}
	if name == "" {
		return &Violation{Rule: RuleEmpty, Message: "文件名不能为空"}, false
	}
	normalized := Normalize(name)

	// "." 与 ".." 有特殊语义,禁止作为文件名
	if normalized == "." || normalized == ".." {
		return &Violation{Rule: RuleDotEntry, Message: `文件名不能是 "." 或 ".."`}, false
	}
	// 尾部点/空格(NTFS 静默剥离,会导致两端名字永远对不齐)
	if last := normalized[len(normalized)-1]; last == '.' || last == ' ' {
		return &Violation{
			Rule:       RuleTrailingDot,
			Position:   utf8.RuneCountInString(normalized) - 1,
			Char:       string(last),
			Suggestion: suggestTrailing(normalized),
			Message:    "文件名不能以点或空格结尾(NTFS 会静默剥离)",
		}, false
	}
	// 全空白
	if strings.TrimSpace(normalized) == "" {
		return &Violation{Rule: RuleAllSpace, Message: "文件名不能全为空白字符"}, false
	}
	// 保留名(取第一个点之前的部分比较,兼容 "nul.txt")
	base := normalized
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	if _, bad := reservedNames[strings.ToUpper(base)]; bad {
		return &Violation{
			Rule:       RuleReservedName,
			Char:       base,
			Suggestion: suggestReserved(normalized),
			Message:    "该名称是 Windows 保留设备名,请改名",
		}, false
	}

	// 逐码点检查禁用字符与控制符
	pos := 0
	needSuggestion := false
	for _, r := range normalized {
		if repl, bad := forbidden[r]; bad {
			needSuggestion = true
			v := &Violation{
				Rule:     RuleForbiddenChar,
				Position: pos,
				Char:     string(r),
				Message:  "文件名包含系统保留字符",
			}
			if needSuggestion {
				v.Suggestion = replaceAllForbidden(normalized)
			}
			_ = repl
			return v, false
		}
		if r < 0x20 || r == 0x7F || unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return &Violation{
				Rule:     RuleForbiddenChar,
				Position: pos,
				Char:     fmt.Sprintf("U+%04X", r),
				Message:  "文件名包含控制字符或零宽字符",
			}, false
		}
		pos++
	}

	// 长度(UTF-8 字节)
	if n := len(normalized); n > p.MaxBytes {
		return &Violation{
			Rule:    RuleTooLong,
			Message: fmt.Sprintf("文件名过长:%d 字节,上限 %d 字节", n, p.MaxBytes),
		}, false
	}
	return nil, true
}

// CheckPath 校验累计路径长度与深度(R-11 双上限)。
//
// depth 是**该文件所在层数**(根为 0);pathBytes 是相对同步根的累计 UTF-8 字节数。
func (p Policy) CheckPath(displayPath string, depth int) (*Violation, bool) {
	if p.MaxDepth <= 0 {
		p.MaxDepth = 31
	}
	if p.MaxPathBytes <= 0 {
		p.MaxPathBytes = 240
	}
	if depth > p.MaxDepth {
		return &Violation{
			Rule:    RuleTooLong,
			Message: fmt.Sprintf("目录层级过深:第 %d 层,上限 %d 层", depth, p.MaxDepth),
		}, false
	}
	if n := len(Normalize(displayPath)); n > p.MaxPathBytes {
		return &Violation{
			Rule:    RuleTooLong,
			Message: fmt.Sprintf("完整路径过长:%d 字节,上限 %d 字节(考虑缩短目录层级或文件名)", n, p.MaxPathBytes),
		}, false
	}
	return nil, true
}

// ConflictSuffix 生成确定性冲突后缀(6.7 + 7.2:先按码点截断主体,再加后缀,NFC 后重校验)。
//
// 保证:结果总字节 ≤ MaxBytes;截断落在 UTF-8 码点边界;绝不做静默改名(调用方需显式使用)。
func (p Policy) ConflictSuffix(name, timestamp string) string {
	if p.MaxBytes <= 0 {
		p.MaxBytes = 240
	}
	suffix := "_conflict_" + timestamp
	// 扩展名单独保留,避免把 .xlsx 截掉
	ext := ""
	body := name
	if i := strings.LastIndex(name, "."); i > 0 {
		body, ext = name[:i], name[i:]
	}
	// 后缀 + 扩展名已超限:退化为"截断主体 + 后缀"
	budget := p.MaxBytes - len(suffix) - len(ext)
	if budget < 1 {
		ext = ""
		budget = p.MaxBytes - len(suffix)
	}
	if budget < 1 {
		// 极端情况:后缀本身超限,直接截断后缀
		s := Normalize(name + suffix)
		if len(s) > p.MaxBytes {
			s = truncateBytes(s, p.MaxBytes)
		}
		return s
	}
	out := Normalize(truncateBytes(body, budget) + suffix + ext)
	// 截断后可能落在组合符中间,再归一并做长度兜底
	for len(out) > p.MaxBytes {
		out = truncateBytes(out, len(out)-1)
		out = Normalize(out)
	}
	return out
}

// truncateBytes 在 UTF-8 码点边界截断到 ≤ n 字节。
func truncateBytes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// 从 n 往前退到合法边界
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

func replaceAllForbidden(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if repl, bad := forbidden[r]; bad {
			b.WriteRune(repl)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func suggestTrailing(s string) string {
	return strings.TrimRight(s, ". ")
}

func suggestReserved(s string) string {
	return s + "_"
}

// ErrEmpty 兼容 errors.Is 的便捷判断。
var ErrEmpty = errors.New("namepolicy: 文件名为空")
