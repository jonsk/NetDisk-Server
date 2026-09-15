package namepolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// 共享夹具 desktop/testdata/conflict-cases.json —— 冲突命名的**双端一致性**验收(DE-D-12)。
//
// 为什么要双端同一批用例:冲突副本名由**客户端**算出来,再由**服务端**的 namepolicy 校验。
// 两边算法一旦漂移,用户会看到"客户端说这个名字可以,服务端 400"——而且只会在
// **特定长度/特定字符**的名字上出现(超长中文名、带组合符的名字、表情符号跨边界…),
// 靠人肉比对算法永远查不全。所以让两侧跑**同一批输入 + 同一批期望输出**。
//
// 与 DE-D-02 的名字规则夹具同一个套路,但方向相反:那边是"客户端对齐服务端规则",
// 这边是"两端实现同一套确定性算法",所以这里的夹具**由 Go 实现生成**,
// 客户端只能对齐它(服务端是唯一权威)。

type conflictCase struct {
	Name      string `json:"name"`
	Timestamp string `json:"timestamp"`
	MaxBytes  int    `json:"max_bytes"`
	Expected  string `json:"expected"`
	Note      string `json:"note"`
}

type conflictFixture struct {
	Note  string         `json:"note"`
	Cases []conflictCase `json:"cases"`
}

// conflictCases 是"必须覆盖到的形态"。加这一层是因为:只断言"两侧一致"的夹具会退化 ——
// 全填几个 `a.txt` 也能让两侧一致,却什么都没锁住。
func conflictCases() []conflictCase {
	ts := "20260912T101500"
	return []conflictCase{
		{Name: "report.xlsx", Timestamp: ts, MaxBytes: 240, Note: "普通 ASCII 名 + 扩展名"},
		{Name: "季度报告.xlsx", Timestamp: ts, MaxBytes: 240, Note: "普通中文名"},
		{Name: "archive.tar.gz", Timestamp: ts, MaxBytes: 240, Note: "多段扩展名:只保留最后一段"},
		{Name: "name.", Timestamp: ts, MaxBytes: 240, Note: "以点结尾(扩展名为单个点)"},
		{Name: ".gitignore", Timestamp: ts, MaxBytes: 240, Note: "点开头:不能把整个名字当扩展名"},
		{Name: repeat("报", 80) + ".xlsx", Timestamp: ts, MaxBytes: 240, Note: "中文超长:240 字节主体 + 扩展名"},
		{Name: repeat("a", 300) + ".txt", Timestamp: ts, MaxBytes: 240, Note: "ASCII 超长"},
		{Name: repeat("a", 230) + "😀" + ".bin", Timestamp: ts, MaxBytes: 240, Note: "4 字节字符跨截断边界:必须落在码点边界"},
		{Name: repeat("e\u0301", 100) + ".txt", Timestamp: ts, MaxBytes: 240, Note: "组合符:截断后必须 NFC 重归一"},
		{Name: repeat("报", 100), Timestamp: ts, MaxBytes: 240, Note: "无扩展名的超长中文"},
		{Name: "短名.txt", Timestamp: "20260101T000000", MaxBytes: 40, Note: "小预算:扩展名 + 后缀几乎占满"},
		{Name: "名字.txt", Timestamp: "20260101T000000", MaxBytes: 10, Note: "极端:后缀本身就超限,退化为截断"},
	}
}

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

// TestConflictCasesMatchFixture 锁住夹具:每条期望值都必须由**当前实现**算得出来。
// 夹具一旦被手改错(或实现被改而夹具没跟),这里立刻红。
func TestConflictCasesMatchFixture(t *testing.T) {
	path := fixturePath(t)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读不到共享夹具 %s: %v", path, err)
	}
	var fx conflictFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("夹具不是合法 JSON: %v", err)
	}

	// 覆盖度断言:夹具必须包含这些形态,否则"两侧一致"毫无意义
	required := []string{"超长", "扩展名", "点开头", "组合符", "码点边界", "小预算", "极端"}
	notes := ""
	for _, c := range fx.Cases {
		notes += c.Note + "\n"
	}
	for _, r := range required {
		found := false
		for _, c := range fx.Cases {
			if strings.Contains(c.Note, r) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("夹具缺少「%s」形态的用例(只有几个 a.txt 的夹具锁不住任何东西)", r)
		}
	}
	_ = notes

	if len(fx.Cases) < len(conflictCases()) {
		t.Errorf("夹具用例数 %d 少于应当覆盖的形态数 %d", len(fx.Cases), len(conflictCases()))
	}

	for _, c := range fx.Cases {
		p := Policy{MaxBytes: c.MaxBytes}
		got := p.ConflictSuffix(c.Name, c.Timestamp)
		if got != c.Expected {
			t.Errorf("夹具与实现不一致(name=%q max=%d note=%s):\n  期望 %q\n  实际 %q",
				abbrev(c.Name), c.MaxBytes, c.Note, c.Expected, got)
		}
		if n := len([]byte(got)); n > c.MaxBytes {
			t.Errorf("冲突名超过 %d 字节上限: %d 字节(name=%q)", c.MaxBytes, n, abbrev(c.Name))
		}
	}
}

// TestEmitConflictFixture 重新生成夹具(NETDISK_EMIT_CONFLICT_FIXTURE=1)。
//
// 生成方向是 **Go → 夹具 → 客户端**,因为服务端是唯一权威:客户端只能对齐服务端。
func TestEmitConflictFixture(t *testing.T) {
	if os.Getenv("NETDISK_EMIT_CONFLICT_FIXTURE") == "" {
		t.Skip("未设置 NETDISK_EMIT_CONFLICT_FIXTURE,跳过夹具生成")
	}
	cases := conflictCases()
	for i := range cases {
		p := Policy{MaxBytes: cases[i].MaxBytes}
		cases[i].Expected = p.ConflictSuffix(cases[i].Name, cases[i].Timestamp)
	}
	sort.SliceStable(cases, func(a, b int) bool { return cases[a].Note < cases[b].Note })
	fx := conflictFixture{
		Note: "冲突命名双端一致性夹具(DE-D-12)。**由服务端实现生成**(NETDISK_EMIT_CONFLICT_FIXTURE=1 go test ./internal/namepolicy -run TestEmitConflictFixture -v)," +
			"客户端 NetDisk.ClientCore.ConflictNaming 必须逐字节对齐;" +
			"任一侧算法演进时先改服务端、再重新生成夹具。",
		Cases: cases,
	}
	raw, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatalf("序列化夹具失败: %v", err)
	}
	fmt.Printf("FIXTURE_JSON_BEGIN\n%s\nFIXTURE_JSON_END\n", raw)
}

func fixturePath(t *testing.T) string {
	t.Helper()
	// server/internal/namepolicy → 仓库根 → desktop/testdata
	for _, rel := range []string{"../../../desktop/testdata/conflict-cases.json", "../../desktop/testdata/conflict-cases.json"} {
		if _, err := os.Stat(rel); err == nil {
			return rel
		}
	}
	t.Fatalf("找不到 desktop/testdata/conflict-cases.json(从 %s 起找不到)", "server/internal/namepolicy")
	return ""
}

func abbrev(s string) string {
	if len(s) <= 24 {
		return s
	}
	return s[:24] + "…"
}
