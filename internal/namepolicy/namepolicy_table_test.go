package namepolicy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件补上 6.7(文件名规则)与 7.2(冲突截断)的**表驱动单测**缺口。
//
// 与 namepolicy_test.go 的分工:那边是"一条规则一个用例",断言较粗(多数只看 ok);
// 这边把每条规则**逐项钉死** —— 命中哪条规则、违规字符的码点位置、Violation.Char、
// 建议名、以及冲突名的逐字节期望值。每条用例都带 why 说明"哪种**看起来合理**的错实现
// 会在这里变红":一条永远为真的断言不算测试。
//
// 断言全部按**实现真实行为**写(而不是按文档字面),遇到文档与代码不一致的地方在
// 本文件注释里点出来,并由父任务汇总,不在测试里编码一个猜测。

// validateCase 是一条 Validate 用例。
//
// 只断言"合法/非法"不够:实现把拒绝原因拆成规则号 + 码点位置 + 字符 + 建议名,
// 客户端要靠这四项做高亮与"一键转全角",所以这里逐项断言。
type validateCase struct {
	name     string
	input    string
	valid    bool
	rule     int    // 拒绝时命中的规则号(namepolicy.go 顶部常量)
	position int    // 期望的**码点**位置(不是字节位置)
	char     string // 期望 Violation.Char(空 = 该规则不填字符)
	sugg     string // 期望 Violation.Suggestion(空 = 无建议)
	why      string // 哪种错实现会在这里失败
}

func validateCases() []validateCase {
	return []validateCase{
		// ------------------------------------------------------------ 放行面
		{name: "普通 ASCII 名放行", input: "report.xlsx", valid: true},
		{name: "中文与 Emoji 放行", input: "📁 项目资料.txt", valid: true},
		{name: "全角等价符号放行", input: "a：b＊c.txt", valid: true,
			why: "按\"半角/全角都算保留字符\"的过严实现(比如把 U+FF0F 也拉黑)会在这里变红"},
		{name: "保留名的近似名放行", input: "CONS.txt", valid: true,
			why: "保留名是**枚举集合**而不是前缀匹配;用 strings.HasPrefix 会把 CONS/CONSOLE 误拒"},
		{name: "COM0 不在保留名集合内", input: "COM0", valid: true,
			why: "集合是 COM1-9/LPT1-9,不是 COM[0-9] 正则;用正则会把 COM0 误拒"},
		{name: "COM10 不是保留名", input: "COM10", valid: true},
		{name: "尾随全角空格放行", input: "报告\u3000", valid: true,
			why: "实现只按最后一个**字节**判 ASCII 的 '.' 与 ' '(U+3000 的末字节是 0x80);改成 TrimRightFunc(unicode.IsSpace) 会把这条误拒"},
		{name: "点开头且点后是保留名", input: ".CON", valid: true,
			why: "实现取**第一个点之前**的部分比较,点开头得到空前缀,所以不判保留名;按 Windows 真实语义(点后依旧是设备名)会变红"},

		// ------------------------------------------------------------ 规则 9 / 10 / 11
		{name: "空名命中规则 9", input: "", rule: RuleEmpty},
		{name: `"." 命中规则 10`, input: ".", rule: RuleDotEntry},
		{name: `".." 命中规则 10`, input: "..", rule: RuleDotEntry},
		{name: "制表符(整名全空白)命中规则 11", input: "\t", rule: RuleAllSpace,
			why: "制表符末字节不是 ' ',所以先落到\"全空白\"而不是\"尾部空格\";两条规则顺序写反会命中规则 3"},
		{name: "不换行空格(整名全空白)命中规则 11", input: "\u00a0", rule: RuleAllSpace,
			why: "U+00A0 不在 0x00-0x1F/0x7F,只查 ASCII 控制符拦不住;要靠 TrimSpace 才拦得住"},
		{name: "三个 ASCII 空格命中规则 3 而不是 11", input: "   ", rule: RuleTrailingDot, position: 2, char: " ",
			why: "尾部点/空格检查在\"全空白\"之前,所以命中规则 3 且建议名为空;两条规则调换顺序会变红"},

		// ------------------------------------------------------------ 规则 3:尾部点/空格
		{name: "尾部点命中规则 3", input: "report.", rule: RuleTrailingDot, position: 6, char: ".", sugg: "report"},
		{name: "尾部空格命中规则 3", input: "report ", rule: RuleTrailingDot, position: 6, char: " ", sugg: "report"},
		{name: "尾部点+空格命中规则 3", input: "报告 .", rule: RuleTrailingDot, position: 3, char: ".", sugg: "报告",
			why: "位置必须是**码点**下标(3),不是字节下标(7);用字节下标会让前端高亮错位"},
		{name: "中间的点与空格放行", input: "my report v1.2.xlsx", valid: true},
		{name: "尾点优先于保留名", input: "CON.", rule: RuleTrailingDot, position: 3, char: ".", sugg: "CON",
			why: "规则 3 在规则 4 之前判定;先判保留名会命中规则 4"},

		// ------------------------------------------------------------ 规则 4:Win32 保留名
		{name: "CON 命中规则 4", input: "CON", rule: RuleReservedName, char: "CON", sugg: "CON_"},
		{name: "CON.txt 带扩展名仍命中规则 4", input: "CON.txt", rule: RuleReservedName, char: "CON", sugg: "CON.txt_",
			why: "设备名比较取第一个点之前的部分;只比全名会把 CON.txt 放行"},
		{name: "大小写不敏感:CoN.TxT", input: "CoN.TxT", rule: RuleReservedName, char: "CoN", sugg: "CoN.TxT_"},
		{name: "Nul 命中规则 4", input: "Nul", rule: RuleReservedName, char: "Nul", sugg: "Nul_"},
		{name: "COM1 命中规则 4", input: "COM1", rule: RuleReservedName, char: "COM1", sugg: "COM1_"},
		{name: "com9.log 命中规则 4", input: "com9.log", rule: RuleReservedName, char: "com9", sugg: "com9.log_"},
		{name: "lpt9 命中规则 4", input: "lpt9", rule: RuleReservedName, char: "lpt9", sugg: "lpt9_"},
		{name: "aux 命中规则 4", input: "aux", rule: RuleReservedName, char: "aux", sugg: "aux_"},

		// ------------------------------------------------------------ 规则 2:半角禁用字符
		{name: "正斜杠命中规则 2", input: "月/报告.xlsx", rule: RuleForbiddenChar, position: 1, char: "/", sugg: "月／报告.xlsx",
			why: "建议名必须是**逐字符换成视觉等价全角**,而不是去掉或加下划线"},
		{name: "反斜杠命中规则 2", input: "a\\b.txt", rule: RuleForbiddenChar, position: 1, char: "\\", sugg: "a＼b.txt"},
		{name: "冒号命中规则 2", input: "a:b.txt", rule: RuleForbiddenChar, position: 1, char: ":", sugg: "a：b.txt"},
		{name: "星号命中规则 2", input: "a*b.txt", rule: RuleForbiddenChar, position: 1, char: "*", sugg: "a＊b.txt"},
		{name: "问号命中规则 2", input: "a?b.txt", rule: RuleForbiddenChar, position: 1, char: "?", sugg: "a？b.txt"},
		{name: "双引号命中规则 2", input: `a"b.txt`, rule: RuleForbiddenChar, position: 1, char: `"`, sugg: "a＂b.txt"},
		{name: "小于号命中规则 2", input: "a<b.txt", rule: RuleForbiddenChar, position: 1, char: "<", sugg: "a＜b.txt"},
		{name: "大于号命中规则 2", input: "a>b.txt", rule: RuleForbiddenChar, position: 1, char: ">", sugg: "a＞b.txt"},
		{name: "竖线命中规则 2", input: "a|b.txt", rule: RuleForbiddenChar, position: 1, char: "|", sugg: "a｜b.txt"},

		// ------------------------------------------------------------ 规则 2:控制符与零宽
		{name: "NUL 控制符命中规则 2", input: "a\x00b.txt", rule: RuleForbiddenChar, position: 1, char: "U+0000"},
		{name: "0x1F 控制符命中规则 2", input: "a\x1fb.txt", rule: RuleForbiddenChar, position: 1, char: "U+001F"},
		{name: "0x7F 命中规则 2", input: "a\x7fb.txt", rule: RuleForbiddenChar, position: 1, char: "U+007F"},
		{name: "零宽空格(Cf)命中规则 2", input: "a\u200bb.txt", rule: RuleForbiddenChar, position: 1, char: "U+200B",
			why: "Cf 类不在 0x00-0x1F/0x7F 里,只有查 unicode.Cf 才拦得住;漏掉它两端显示会不一致"},
		{name: "方向控制符(Cf)命中规则 2", input: "a\u202eb.txt", rule: RuleForbiddenChar, position: 1, char: "U+202E"},
		{name: "控制符判定先于长度判定", input: "a\x00" + strings.Repeat("b", 300), rule: RuleForbiddenChar, position: 1, char: "U+0000",
			why: "逐码点检查在长度检查之前;顺序反了会命中规则 5"},

		// ------------------------------------------------------------ 规则 5:长度按 UTF-8 字节
		{name: "240 字节 ASCII 正好放行", input: strings.Repeat("a", 240), valid: true,
			why: "上限是**闭区间**(≤ 240);写成 < 240 会误拒边界"},
		{name: "241 字节 ASCII 命中规则 5", input: strings.Repeat("a", 241), rule: RuleTooLong},
		{name: "80 个汉字(240 字节)放行", input: strings.Repeat("报", 80), valid: true},
		{name: "81 个汉字(243 字节)命中规则 5", input: strings.Repeat("报", 81), rule: RuleTooLong,
			why: "81 个**字符** < 240,但 243 > 240 **字节**;按字符数判长度的实现会误放行"},
		{name: "60 个 Emoji(240 字节)放行", input: strings.Repeat("😀", 60), valid: true},
		{name: "61 个 Emoji(244 字节)命中规则 5", input: strings.Repeat("😀", 61), rule: RuleTooLong},
		{name: "超长且尾点:规则 3 优先", input: strings.Repeat("a", 300) + ".", rule: RuleTrailingDot, position: 300, char: ".", sugg: strings.Repeat("a", 300),
			why: "顺序是 空→点项→尾点→全空白→保留名→字符→长度;尾点先命中,不会落到规则 5"},

		// ------------------------------------------------------------ 规则 1:NFC 归一后判长度
		{name: "NFD 360 字节、NFC 后 240 字节:放行", input: strings.Repeat("a\u0308", 120), valid: true,
			why: "长度按 **NFC 之后**的字节数判(120 个 ä = 240 字节);先按原始字节数判会因 360 > 240 误拒"},
	}
}

// whySuffix 把"为什么这条能失败"附在断言消息后面(失败时才知道该去查哪种错实现)。
func whySuffix(why string) string {
	if why == "" {
		return ""
	}
	return "\n  为什么: " + why
}

func TestValidateNameRulesTable(t *testing.T) {
	for _, c := range validateCases() {
		t.Run(c.name, func(t *testing.T) {
			v, ok := Default().Validate(c.input)
			if ok != c.valid {
				t.Fatalf("Valid(%q) = %v,期望 %v(实际 Violation=%+v)%s",
					c.input, ok, c.valid, v, whySuffix(c.why))
			}
			if c.valid {
				return
			}
			if v == nil {
				t.Fatalf("判定非法却返回 nil Violation(name=%q)", c.input)
			}
			if v.Rule != c.rule {
				t.Errorf("name=%q 命中规则 %d,期望 %d(消息=%q)%s",
					c.input, v.Rule, c.rule, v.Message, whySuffix(c.why))
			}
			if v.Position != c.position {
				t.Errorf("name=%q 的违规位置 = %d,期望 %d(必须是码点下标)%s",
					c.input, v.Position, c.position, whySuffix(c.why))
			}
			if v.Char != c.char {
				t.Errorf("name=%q 的 Violation.Char = %q,期望 %q%s",
					c.input, v.Char, c.char, whySuffix(c.why))
			}
			if v.Suggestion != c.sugg {
				t.Errorf("name=%q 的建议名 = %q,期望 %q%s",
					c.input, v.Suggestion, c.sugg, whySuffix(c.why))
			}
		})
	}
}

// TestNormalizeNFCTable 钉死 NFC 归一的**逐字符结果**(规则 1)。
//
// 为什么单独测 Normalize:它是入库/比较/返回的统一形态,一旦某类字符没归一,
// "同一个名字"在库里会有两种字节形态,而判重索引是 lower(name) —— 两种形态会被当成两个名字。
func TestNormalizeNFCTable(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		why  string
	}{
		{"组合分音符", "a\u0308", "ä", ""},
		{"组合重音符", "e\u0301", "é", ""},
		{"大写 A + 组合圈", "\u0041\u030A", "Å", ""},
		{"谚文 Jamo 合成音节", "\u1100\u1161", "가",
			"NFC 会把首声+中声合成为一个音节;不归一会把 1 个字符算成 2 个(长度判定同样会偏)"},
		{"已是 NFC 的名字保持原样", "ä.txt", "ä.txt", ""},
		{"ASCII 不变", "report.xlsx", "report.xlsx", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Normalize(c.in)
			if got != c.want {
				t.Errorf("Normalize(%q) = %q,期望 %q%s", c.in, got, c.want, whySuffix(c.why))
			}
			// 幂等:归一结果再归一必须不变(否则"入库一次、回读一次"会漂移)
			if again := Normalize(got); again != c.want {
				t.Errorf("NFC 必须幂等:Normalize(%q) = %q,期望 %q", got, again, c.want)
			}
		})
	}
}

// TestValidateNFCFormsAgreeTable 钉死"同一个名字的 NFD 与 NFC 形式判定一致"。
//
// 这是客户端最容易踩的坑:macOS 递交的是 NFD,Windows 是 NFC;两端必须得到同一判决,
// 而且**长度按归一后的字节数**算(否则一个 360 字节的 NFD 名会被误拒)。
func TestValidateNFCFormsAgreeTable(t *testing.T) {
	cases := []struct {
		name     string
		nfd      string
		nfc      string
		valid    bool
		rule     int
		position int
		char     string
		sugg     string
		why      string
	}{
		{
			name: "组合符名:NFD 与 NFC 都放行", nfd: "a\u0308.txt", nfc: "ä.txt", valid: true,
		},
		{
			name: "非法字符在归一后的形态上定位一致",
			nfd:  "e\u0301/x", nfc: "é/x",
			valid: false, rule: RuleForbiddenChar, position: 1, char: "/", sugg: "é／x",
			why: "位置必须按**归一后**的码点算(1),建议名也必须由归一后的串生成;用原始串会得到 \"e\u0301／x\",客户端对齐不上",
		},
		{
			name: "超长按 NFC 字节数判定,不是原始字节数",
			nfd:  strings.Repeat("a\u0308", 120), nfc: strings.Repeat("ä", 120), valid: true,
			why: "NFD 是 360 字节(> 240),NFC 是 240 字节(== 上限);不做归一直接按原始字节判会误拒",
		},
		{
			name: "NFC 后仍超长:两形态一致命中规则 5",
			nfd:  strings.Repeat("a\u0308", 121), nfc: strings.Repeat("ä", 121),
			valid: false, rule: RuleTooLong,
			why: "NFC 是 242 字节;钉住\"归一之后仍要按 242 判超长\"(只按原始 363 判会漏掉这类)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := Default()
			for _, in := range []struct {
				form string
				s    string
			}{{"NFD", c.nfd}, {"NFC", c.nfc}} {
				v, ok := p.Validate(in.s)
				if ok != c.valid {
					t.Fatalf("%s 形式 %q 判定 valid=%v,期望 %v(实际 Violation=%+v)%s",
						in.form, in.s, ok, c.valid, v, whySuffix(c.why))
				}
				if c.valid {
					continue
				}
				if v.Rule != c.rule || v.Position != c.position || v.Char != c.char || v.Suggestion != c.sugg {
					t.Errorf("%s 形式 %q 的判决 = {rule:%d position:%d char:%q sugg:%q},期望 {rule:%d position:%d char:%q sugg:%q}%s",
						in.form, in.s, v.Rule, v.Position, v.Char, v.Suggestion,
						c.rule, c.position, c.char, c.sugg, whySuffix(c.why))
				}
			}
		})
	}
}

// conflictCaseTable 是一条冲突截断(ConflictSuffix)用例。
//
// validationRule == 0 表示"结果必须自身合法";非 0 表示"结果被规则 N 拒绝"——
// 那是**实现真实现状**(见以点结尾的用例),不是猜测。
type conflictCaseTable struct {
	name           string
	in             string
	ts             string
	maxBytes       int
	want           string
	distinct       bool // 期望结果 != 输入(冲突名必须真的能区分出副本)
	validationRule int  // 对结果再跑 Validate 时期望的规则号;0 = 期望合法
	why            string
}

// conflictTS 用与 7.2 真实形态一致的时间戳(16 字节,20260911T120000)。
const conflictTS = "20260911T120000"

func conflictTableCases() []conflictCaseTable {
	return []conflictCaseTable{
		{
			name: "普通名保留扩展名", in: "报告.xlsx", ts: conflictTS, maxBytes: 240,
			want: "报告_conflict_" + conflictTS + ".xlsx", distinct: true,
		},
		{
			name: "多段扩展名只保留最后一段", in: "archive.tar.gz", ts: "T", maxBytes: 240,
			want: "archive.tar_conflict_T.gz", distinct: true,
		},
		{
			name: "点开头不当作扩展名", in: ".gitignore", ts: "T", maxBytes: 240,
			want: ".gitignore_conflict_T", distinct: true,
			why: "LastIndex('.') == 0 时不切分;写成 i >= 0 会把整个名字当扩展名,后缀反而被截掉",
		},
		{
			// 时间戳少一位(13 字节)让主体预算 = 240-23-5 = 212:212 落在第 71 个汉字的
			// 第 3 个字节上,必须回退两级到 210(70 个汉字,共 238 字节)。
			name: "中文超长:预算落在汉字中间,回退到码点边界", in: strings.Repeat("报", 80) + ".xlsx", ts: "20260911T1200", maxBytes: 240,
			want: strings.Repeat("报", 70) + "_conflict_20260911T1200.xlsx", distinct: true,
			why: "预算 212 字节落在第 71 个汉字的中间;直接 s[:212] 会切出半个汉字(非法 UTF-8),正确实现回退到 210 字节 —— 总长 238 而不是 240,少了这 2 字节正是回退的证据",
		},
		{
			name: "预算正好落在 4 字节 Emoji 内部", in: "aaa😀" + strings.Repeat("b", 50) + ".bin", ts: "T", maxBytes: 20,
			want: "aaa_conflict_T.bin", distinct: true,
			why: "上限 20 时主体预算只有 5 字节,正好切在 Emoji 的第 2 个字节上;不回退到码点边界就会切出半个 Emoji",
		},
		{
			name: "ASCII 超长:截到预算后总长恰好等于上限", in: strings.Repeat("a", 500), ts: conflictTS, maxBytes: 240,
			want: strings.Repeat("a", 215) + "_conflict_" + conflictTS, distinct: true,
			why: "215 = 240 - len(\"_conflict_\" + ts) = 240 - 25;少算一个字节会超上限,多算一个字节会多截掉内容",
		},
		{
			name: "无扩展名超长中文:回退到码点边界后 238 字节", in: strings.Repeat("报", 80), ts: conflictTS, maxBytes: 240,
			want: strings.Repeat("报", 71) + "_conflict_" + conflictTS, distinct: true,
			why: "预算 215 字节不是 3 的倍数,回退到 213(71 个汉字)后总长 238;若不回退直接 s[:215] 会得到 240 字节但末尾是半个汉字",
		},
		{
			name: "组合符截断后必须 NFC 重归一", in: strings.Repeat("e\u0301", 100) + ".txt", ts: conflictTS, maxBytes: 240,
			want: strings.Repeat("é", 70) + "e_conflict_" + conflictTS + ".txt", distinct: true,
			why: "主体预算 211 字节正好落在组合符的引导字节上:实现停在它之前,截断后剩一个**裸 e**,再由 NFC 归一(70 个 é + e);先归一后截断会得到 105 个 é(239 字节)的完全不同结果",
		},
		{
			name: "后缀占满预算:退化为整体截断(结果与输入相同)", in: "名字.txt", ts: "20260101T000000", maxBytes: 10,
			want: "名字.txt", distinct: false,
			why: "后缀 25 字节 > 上限 10 字节,实现退化为\"整体截断\";钉住极小上限下**不保证唯一性**(真正的防重名要靠 DB 唯一索引)",
		},
		{
			name: "上限充裕时正常加后缀", in: "名字.txt", ts: "20260101T000000", maxBytes: 240,
			want: "名字_conflict_20260101T000000.txt", distinct: true,
		},
		{
			name: "以点结尾的非法名:结果仍以点结尾", in: "name.", ts: "T", maxBytes: 240,
			want: "name_conflict_T.", distinct: true, validationRule: RuleTrailingDot,
			why: "输入的扩展名就是单个 \".\";ConflictSuffix 只保证字节上限与码点边界,不修补合法性(与 desktop/testdata/conflict-cases.json 的 name. 用例一致)",
		},
	}
}

func TestConflictSuffixByteBoundaryTable(t *testing.T) {
	for _, c := range conflictTableCases() {
		t.Run(c.name, func(t *testing.T) {
			p := Policy{MaxBytes: c.maxBytes}
			got := p.ConflictSuffix(c.in, c.ts)
			if got != c.want {
				t.Errorf("ConflictSuffix(in=%q, ts=%q, MaxBytes=%d) = %q,期望 %q%s",
					c.in, c.ts, c.maxBytes, got, c.want, whySuffix(c.why))
			}
			// 尺寸与编码边界:必须独立断言一次(逐字节相等已经覆盖,但这两个约束
			// 是文档明确写下的"保证",失败时要能直接指出是哪条保证破了)
			if n := len(got); n > c.maxBytes {
				t.Errorf("结果 %q 有 %d 字节,超过上限 %d 字节%s", got, n, c.maxBytes, whySuffix(c.why))
			}
			if !utf8.ValidString(got) {
				t.Errorf("结果不是合法 UTF-8(截断切在多字节字符中间): %q", got)
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("结果含替换字符 U+FFFD(截断产生了坏码点): %q", got)
			}
			if normed := Normalize(got); normed != got {
				t.Errorf("冲突名必须是 NFC 形式:Normalize(%q) = %q", got, normed)
			}
			if c.distinct && got == c.in {
				t.Errorf("冲突名与原名相同(%q),无法区分副本%s", got, whySuffix(c.why))
			}
			// 冲突名接着要被 namepolicy 校验(客户端算出、服务端复核),所以结果自身的
			// 合法性也必须钉住 —— 除了**输入本身已非法**的那一类(见 validationRule)。
			v, ok := p.Validate(got)
			if c.validationRule == 0 {
				if !ok {
					t.Errorf("结果的冲突名自身必须合法,实际被拒: %+v(结果 %q)", v, got)
				}
				return
			}
			if ok {
				t.Errorf("结果 %q 期望命中规则 %d(真实现状),实际被判合法", got, c.validationRule)
			} else if v.Rule != c.validationRule {
				t.Errorf("结果 %q 命中规则 %d,期望 %d%s", got, v.Rule, c.validationRule, whySuffix(c.why))
			}
		})
	}
}
