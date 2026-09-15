package namepolicy

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeNFC(t *testing.T) {
	// "a" + U+0308(组合分音符) 应归一为 ä(U+00E4)
	composed := "a\u0308"
	normed := Normalize(composed)
	if normed != "ä" {
		t.Fatalf("NFC 归一失败: %q -> %q", composed, normed)
	}
	if normed == composed {
		t.Error("归一后应与输入不同(NFC 生效)")
	}
}

// 规则 2:禁止半角非法字符;规则 7:全角放行
func TestForbiddenVsFullWidth(t *testing.T) {
	p := Default()

	bad := []string{
		"月/报告.xlsx", "a\\b.txt", "a:b.txt", "a*b.txt", "a?b.txt",
		`a"b.txt`, "a<b.txt", "a>b.txt", "a|b.txt",
	}
	for _, name := range bad {
		v, ok := p.Validate(name)
		if ok {
			t.Errorf("%q 应被拒绝", name)
			continue
		}
		if v.Rule != RuleForbiddenChar {
			t.Errorf("%q 应命中规则 2, got %d", name, v.Rule)
		}
		if v.Char == "" || v.Position < 0 {
			t.Errorf("%q 应给出字符与位置, got %+v", name, v)
		}
	}

	// 全角等价形式必须放行(不同码点,文件系统安全)
	good := []string{
		"月／报告.xlsx", "a＼b.txt", "a：b.txt", "a＊b.txt", "a？b.txt",
		"a＜b.txt", "a＞b.txt", "a｜b.txt",
	}
	for _, name := range good {
		if v, ok := p.Validate(name); !ok {
			t.Errorf("全角 %q 应放行, got %+v", name, v)
		}
	}
}

// 建议名必须是"视觉等价全角",且建议本身必须合法(否则客户端一键替换后会再次被拒)
func TestSuggestionIsValid(t *testing.T) {
	p := Default()
	v, ok := p.Validate("月/报告.xlsx")
	if ok {
		t.Fatal("应被拒绝")
	}
	if v.Suggestion == "" {
		t.Fatal("应给出建议名")
	}
	if v.Suggestion != "月／报告.xlsx" {
		t.Errorf("建议名应为一键转全角, got %q", v.Suggestion)
	}
	if _, ok := p.Validate(v.Suggestion); !ok {
		t.Errorf("建议名自身必须合法: %q", v.Suggestion)
	}
}

// 规则 1:控制符与零宽字符
func TestControlAndZeroWidth(t *testing.T) {
	p := Default()
	bad := []string{
		"a\x00b.txt", "a\x1fb.txt", "a\x7fb.txt",
		"a\u200bb.txt", // 零宽空格(Cf)
		"a\u202eb.txt", // 方向控制(Cf)
	}
	for _, name := range bad {
		if v, ok := p.Validate(name); ok {
			t.Errorf("%q 应被拒绝(控制符/零宽)", name)
		} else if v.Rule != RuleForbiddenChar {
			t.Errorf("%q 规则应为 2, got %d", name, v.Rule)
		}
	}
}

// 规则 3:尾部点/空格(NTFS 静默剥离)
func TestTrailingDotAndSpace(t *testing.T) {
	p := Default()
	for _, name := range []string{"report.", "report ", "report. ", "报告 ."} {
		v, ok := p.Validate(name)
		if ok {
			t.Errorf("%q 应被拒绝(尾部点/空格)", name)
			continue
		}
		if v.Rule != RuleTrailingDot {
			t.Errorf("%q 应命中规则 3, got %d", name, v.Rule)
		}
		if v.Suggestion == "" {
			t.Errorf("%q 应给出建议(去掉尾部)", name)
		}
	}
	// 中间的点与空格必须放行
	for _, name := range []string{"my report v1.2.xlsx", "a. b.txt"} {
		if v, ok := p.Validate(name); !ok {
			t.Errorf("%q 应放行, got %+v", name, v)
		}
	}
}

// 规则 4:Win32 保留名(含带扩展名、任意大小写)
func TestReservedNames(t *testing.T) {
	p := Default()
	for _, name := range []string{"CON", "con", "Nul", "nul.txt", "COM1", "com9.log", "LPT1", "aux", "PRN.txt"} {
		v, ok := p.Validate(name)
		if ok {
			t.Errorf("%q 应被拒绝(保留名)", name)
			continue
		}
		if v.Rule != RuleReservedName {
			t.Errorf("%q 应命中规则 4, got %d", name, v.Rule)
		}
	}
	// 相似但不保留的名字必须放行
	for _, name := range []string{"CONS", "CONSOLE.txt", "COM10", "LPT0", "nulll.txt", "con_1.txt"} {
		if v, ok := p.Validate(name); !ok {
			t.Errorf("%q 应放行, got %+v", name, v)
		}
	}
}

// 规则 5:长度按 UTF-8 字节;规则 8:中文与 Emoji 允许
func TestLengthAndUnicode(t *testing.T) {
	p := Default()
	if v, ok := p.Validate(strings.Repeat("a", 240)); !ok {
		t.Errorf("240 个 ASCII 应放行, got %+v", v)
	}
	if v, ok := p.Validate(strings.Repeat("a", 241)); ok {
		t.Error("241 字节应被拒绝")
	} else if v.Rule != RuleTooLong {
		t.Errorf("应命中规则 5, got %d", v.Rule)
	}
	// 中文:每字 3 字节,80 字 = 240 字节应放行,81 字应拒绝
	if v, ok := p.Validate(strings.Repeat("报", 80)); !ok {
		t.Errorf("80 个汉字(240 字节)应放行, got %+v", v)
	}
	if _, ok := p.Validate(strings.Repeat("报", 81)); ok {
		t.Error("81 个汉字(243 字节)应被拒绝")
	}
	// Emoji(4 字节)允许
	if v, ok := p.Validate("📁 项目资料.txt"); !ok {
		t.Errorf("Emoji 应放行, got %+v", v)
	}
}

// 空名与特殊项
func TestEmptyAndDotEntries(t *testing.T) {
	p := Default()
	if v, ok := p.Validate(""); ok {
		t.Error("空名应被拒绝")
	} else if v.Rule != RuleEmpty {
		t.Errorf("空名规则应为 9, got %d", v.Rule)
	}
	for _, name := range []string{".", ".."} {
		if v, ok := p.Validate(name); ok {
			t.Errorf("%q 应被拒绝", name)
		} else if v.Rule != RuleDotEntry {
			t.Errorf("%q 规则应为 10, got %d", name, v.Rule)
		}
	}
	if _, ok := p.Validate("   "); ok {
		t.Error("全空白应被拒绝")
	}
}

// R-11:路径双上限(深度 + 累计字节)
func TestCheckPath(t *testing.T) {
	p := Default()
	if v, ok := p.CheckPath("a/b/c.txt", 3); !ok {
		t.Errorf("正常路径应放行, got %+v", v)
	}
	if v, ok := p.CheckPath("deep", 32); ok {
		t.Error("深度 32 应被拒绝")
	} else if !strings.Contains(v.Message, "层级过深") {
		t.Errorf("错误信息应说明深度问题: %s", v.Message)
	}
	long := strings.Repeat("目录/", 100) + "file.txt"
	if v, ok := p.CheckPath(long, 5); ok {
		t.Error("超长路径应被拒绝")
	} else if !strings.Contains(v.Message, "路径过长") {
		t.Errorf("错误信息应说明路径长度问题: %s", v.Message)
	}
}

// 冲突名:确定性、保留扩展名、不超限、结果合法(7.2 + 6.7)
func TestConflictSuffixDeterministicAndValid(t *testing.T) {
	p := Default()
	ts := "20260911T120000"

	got := p.ConflictSuffix("报告.xlsx", ts)
	want := "报告_conflict_" + ts + ".xlsx"
	if got != want {
		t.Errorf("ConflictSuffix = %q want %q", got, want)
	}
	// 确定性:同输入同输出
	if again := p.ConflictSuffix("报告.xlsx", ts); again != got {
		t.Error("同输入必须同输出(确定性)")
	}
	if v, ok := p.Validate(got); !ok {
		t.Errorf("冲突名本身必须合法: %+v", v)
	}

	// 超长名:截断主体、保留扩展名、总长 ≤ 240、按码点边界
	longName := strings.Repeat("报", 80) + ".xlsx" // 240 + 5 字节
	c := p.ConflictSuffix(longName, ts)
	if len(c) > 240 {
		t.Errorf("冲突名超长:%d 字节", len(c))
	}
	if !strings.HasSuffix(c, ".xlsx") {
		t.Errorf("应保留扩展名: %q", c)
	}
	if !utf8.ValidString(c) {
		t.Errorf("截断必须落在 UTF-8 码点边界: %q", c)
	}
	if v, ok := p.Validate(c); !ok {
		t.Errorf("截断后的冲突名必须合法: %+v (%q)", v, c)
	}
	// 极长后缀场景:仍不得超限
	weird := p.ConflictSuffix(strings.Repeat("a", 500), strings.Repeat("9", 60))
	if len(weird) > 240 {
		t.Errorf("极端场景仍不得超限: %d", len(weird))
	}
}

// 多段扩展名与无扩展名
func TestConflictSuffixExtensions(t *testing.T) {
	p := Default()
	ts := "T"
	if got := p.ConflictSuffix("archive.tar.gz", ts); !strings.HasSuffix(got, ".gz") {
		t.Errorf("应按最后一段扩展名保留: %q", got)
	}
	if got := p.ConflictSuffix("noext", ts); !strings.HasSuffix(got, "_conflict_T") {
		t.Errorf("无扩展名时直接加后缀: %q", got)
	}
	// 以点开头的隐藏文件:不应被当作扩展名切分
	got := p.ConflictSuffix(".gitignore", ts)
	if !strings.HasPrefix(got, ".gitignore_conflict_") {
		t.Errorf("隐藏文件处理错误: %q", got)
	}
}
