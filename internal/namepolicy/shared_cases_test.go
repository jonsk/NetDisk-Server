package namepolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 共享夹具的双端一致性用例(DE-D-02 ③)。
//
// 夹具在 `desktop/testdata/name-cases.json`,**同一批用例**由:
//   - 本文件(服务端,权威);
//   - `desktop/tests/NameRulesCheck`(客户端 C#)
//
// 各跑一遍。任何一边不一致都要改**客户端**(服务端是唯一权威)。
//
// 为什么夹具放在 desktop/ 而不是 server/:它是两端共享的资产,放在客户端工程下
// 更接近"客户端必须对齐"的语义;服务端这边只是"顺便也读一下"以锁住自己 ——
// 若放服务端目录下,客户端的 CI(可能不构建 Go)会更容易漏掉它。

type nameCase struct {
	Name  string `json:"name"`
	Valid bool   `json:"valid"`
	Note  string `json:"note"`
}

// loadNameCases 读取共享夹具。
//
// 路径用 runtime.Caller 定位(而不是相对工作目录):`go test` 的工作目录是**包目录**,
// 用相对路径会随"从哪个包跑"而变;而 CI 里可能从别处调 go test。
// 夹具不存在时**跳过**(带说明):第 4 卷落地前(或裁剪发行版里)没有 desktop/ 是合法状态。
func loadNameCases(t *testing.T) []nameCase {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件路径")
	}
	// namepolicy → internal → server → netdisk(仓库根)
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(thisFile))))
	fixture := filepath.Join(root, "desktop", "testdata", "name-cases.json")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Skipf("共享名字夹具不存在(%s),跳过双端一致性用例: %v", fixture, err)
	}
	var doc struct {
		Cases []nameCase `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析共享夹具失败: %v", err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("共享夹具里没有用例")
	}
	return doc.Cases
}

// TestSharedNameCasesMatchServer 断言服务端行为与共享夹具一致。
//
// 这条用例的作用不是"测服务端"(服务端的行为本来就是它定义的),而是
// **锁住夹具**:夹具是客户端要对齐的目标,它一旦写错(例如把某个非法名标成合法),
// 客户端就会"对齐到一个错误的规则上",而那种错误在两端都自洽、谁也不会报错。
func TestSharedNameCasesMatchServer(t *testing.T) {
	p := Default()
	for _, c := range loadNameCases(t) {
		v, ok := p.Validate(c.Name)
		if ok != c.Valid {
			detail := ""
			if v != nil {
				detail = fmt.Sprintf("rule=%d %s", v.Rule, v.Message)
			}
			t.Errorf("夹具期望 valid=%v,服务端判定 valid=%v(原因 %s):name=%q(%s)\n"+
				"→ 夹具写错了,或服务端规则变了:两者必须一起改,否则客户端会对齐到错误规则",
				c.Valid, ok, detail, c.Name, c.Note)
		}
	}
}

// TestSharedNameCasesShowRuleCoverage 保证夹具**覆盖到了每一类**规则。
//
// 只断言"逐条一致"的夹具很容易退化成"一堆 a.txt"—— 加上覆盖度断言,
// 才有资格说"双端同源"。这里检查每一类至少有一条用例(合法与非法各一条)。
func TestSharedNameCasesShowRuleCoverage(t *testing.T) {
	cases := loadNameCases(t)
	notes := make([]string, 0, len(cases))
	for _, c := range cases {
		notes = append(notes, fmt.Sprintf("%s|%v", c.Name, c.Valid))
	}
	joined := fmt.Sprint(notes)
	for _, want := range []string{"CON|false", "COM1|false", "report.|false", "a/b.txt|false"} {
		if !contains(joined, want) {
			t.Errorf("夹具缺少覆盖: %s(夹具必须覆盖保留名/结尾点/路径分隔符这几类)", want)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
