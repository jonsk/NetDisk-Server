package main

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 合成路径:刻意一半用 Windows 反斜杠、一半用斜杠 ——
// 真实 profile 两种写法都会出现(编译器写盘用平台风格,
// 手工拼的 profile 又常是斜杠),import path 反推必须都能吃下。
const (
	windowsNamepolicyFile = `D:\WorkSpace\GO\netdisk\server\internal\namepolicy\namepolicy.go`
	slashObjlockFile      = "D:/WorkSpace/GO/netdisk/server/internal/objlock/lock.go"
	slashFilesvcFile      = "D:/WorkSpace/GO/netdisk/server/internal/filesvc/service.go"
	windowsGateFile       = `D:\WorkSpace\GO\netdisk\server\cmd\coveragegate\main.go`

	npPkg      = "github.com/netdisk/netdisk/internal/namepolicy"
	objlockPkg = "github.com/netdisk/netdisk/internal/objlock"
	filesvcPkg = "github.com/netdisk/netdisk/internal/filesvc"
	gatePkg    = "github.com/netdisk/netdisk/cmd/coveragegate"
)

// ---- 合成 profile 的小工具 ----

// covRec 拼一条 profile 记录。起止位置固定(门禁只关心 numStatements 与 count),
// 但保持真实格式,好让解析器走完整条链路而不是被特例短路。
func covRec(file string, statements, count int) string {
	return fmt.Sprintf("%s:1.1,1.20 %d %d", file, statements, count)
}

// covProfile 拼一份 set 模式的 profile。
func covProfile(recs ...string) string {
	return "mode: set\n" + strings.Join(recs, "\n") + "\n"
}

// writeTemp 在 t.TempDir() 下写一个文件并返回路径。
func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时文件 %s 失败: %v", name, err)
	}
	return p
}

// almostEqual 比较百分比。除法 + 乘 100 会出现 1e-15 量级的表示误差,
// 直接 == 会让测试在无关紧要的位模式上翻车。
func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// ---- (a)~(f)端到端:合成 profile 与配置都落在 t.TempDir() ----

// TestRunGate 是门禁的端到端表驱动测试。
//
// 为什么在进程内调用 run() 而不是 build 出二进制再 shell:
// 一是慢;二是 shell 只能看到合并输出与退出码 —— "输入非法"(exitInvalid)
// 与"阈值不达标"(exitGate)就分不开了,而这两者对使用者意味着完全不同的动作。
func TestRunGate(t *testing.T) {
	cases := []struct {
		name       string
		profile    string // profile 文件内容
		config     string // 空 = 不传 -config
		min        string // 空 = 不传 -min
		top        string // 空 = 用默认
		wantExit   int
		wantOut    []string // 报告中必须出现的子串
		wantNotOut []string // 报告中不得出现的子串
	}{
		{
			// (a) 全部达标 → 0
			name:     "a_全部阈值达标退出0",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			config:   `{"overall": 60.0, "packages": {"` + npPkg + `": 85.0}, "critical": ["` + npPkg + `"]}`,
			wantExit: exitOK,
			wantOut: []string{
				"门禁通过", "总体覆盖率: 100.00%(要求 ≥ 60.00%)",
				"✓ " + npPkg + " 100.00%(要求 ≥ 85.00%) [critical]",
			},
		},
		{
			// (a) 边界:实际值 == 阈值必须算**达标**(< 才算不达标),
			// 否则"刚好达标"会在升级依赖/换 Go 版本后被莫名其妙地判红。
			name:     "a_恰好等于阈值退出0",
			profile:  covProfile(covRec(windowsNamepolicyFile, 1, 1), covRec(windowsNamepolicyFile, 4, 0)),
			config:   `{"overall": 20.0, "packages": {"` + npPkg + `": 20.0}}`,
			wantExit: exitOK,
			wantOut:  []string{"总体覆盖率: 20.00%(要求 ≥ 20.00%)", "门禁通过"},
		},
		{
			// (b) 总体不达标 → 1,且必须点名"总体"并同时给出实际值与要求值
			name:     "b_总体不达标退出1",
			profile:  covProfile(covRec(windowsNamepolicyFile, 1, 1), covRec(windowsNamepolicyFile, 9, 0)),
			config:   `{"overall": 60.0, "packages": {"` + npPkg + `": 5.0}}`,
			wantExit: exitGate,
			wantOut: []string{
				"总体覆盖率: 10.00%(要求 ≥ 60.00%)✗",
				"总体覆盖率 10.00% 低于要求 60.00%",
				"门禁未通过(1 项阈值未达标)",
			},
		},
		{
			// (b') 只给 -min(无配置文件)时总体检查同样生效
			name:     "b_仅min不达标退出1",
			profile:  covProfile(covRec(windowsNamepolicyFile, 1, 1), covRec(windowsNamepolicyFile, 9, 0)),
			min:      "50",
			wantExit: exitGate,
			wantOut:  []string{"总体覆盖率 10.00% 低于要求 50.00%"},
		},
		{
			// (b'') -min 0 = 关闭总体检查;没有配置时就应该通过
			name:     "b_min为0关闭总体检查退出0",
			profile:  covProfile(covRec(windowsNamepolicyFile, 1, 1), covRec(windowsNamepolicyFile, 9, 0)),
			min:      "0",
			wantExit: exitOK,
			wantOut:  []string{"未设总体下限", "门禁通过"},
		},
		{
			// (c) 单个包不达标 → 1,点名的是**这个包**,不能牵连达标的包
			name: "c_单包不达标点名该包",
			profile: covProfile(
				covRec(windowsNamepolicyFile, 10, 1),
				covRec(slashObjlockFile, 2, 1),
				covRec(slashObjlockFile, 8, 0),
			),
			config:   `{"overall": 5.0, "packages": {"` + npPkg + `": 90.0, "` + objlockPkg + `": 80.0}}`,
			wantExit: exitGate,
			wantOut: []string{
				"包 " + objlockPkg + " 覆盖率 20.00% 低于要求 80.00%",
				"✓ " + npPkg + " 100.00%(要求 ≥ 90.00%)",
			},
			wantNotOut: []string{"包 " + npPkg + " 覆盖率"},
		},
		{
			// (d) 配了阈值但 profile 里根本没有这个包 → 必须失败
			// (这正是"删掉包/改包名"能悄悄让门禁变绿的那条缝)
			name:     "d_包无数据判失败",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			config:   `{"overall": 1.0, "packages": {"` + filesvcPkg + `": 50.0}, "critical": ["` + filesvcPkg + `"]}`,
			wantExit: exitGate,
			wantOut: []string{
				"[critical] 包 " + filesvcPkg + " 在 profile 中无覆盖率数据",
				"✗ " + filesvcPkg + " 无覆盖率数据(profile 中未出现,要求 ≥ 50.00%) [critical]",
			},
		},
		{
			// (e) 未知 mode → 非 0(exitInvalid),不能被当成"没有语句所以通过"
			name:     "e_未知mode被拒",
			profile:  "mode: bogus\n" + covRec(windowsNamepolicyFile, 1, 1) + "\n",
			wantExit: exitInvalid,
			wantOut:  []string{"未知的覆盖率模式"},
		},
		{
			// (e) 数字字段非数字 → 非 0
			name:     "e_numStatements非数字被拒",
			profile:  "mode: set\n" + windowsNamepolicyFile + ":1.1,1.20 abc 1\n",
			wantExit: exitInvalid,
			wantOut:  []string{"numStatements", "不是整数"},
		},
		{
			// (e) count 非数字 → 非 0
			name:     "e_count非数字被拒",
			profile:  "mode: set\n" + windowsNamepolicyFile + ":1.1,1.20 3 xyz\n",
			wantExit: exitInvalid,
			wantOut:  []string{"count", "不是整数"},
		},
		{
			// (e) 空文件(缺 mode 头)→ 非 0
			name:     "e_空文件被拒",
			profile:  "",
			wantExit: exitInvalid,
			wantOut:  []string{"缺少 `mode:` 头"},
		},
		{
			// (e) 只有 mode 头(profile 被截断)→ 非 0
			name:     "e_只有mode头被拒",
			profile:  "mode: set\n",
			wantExit: exitInvalid,
			wantOut:  []string{"不含任何语句统计"},
		},
		{
			// (e) 位置字段坏掉 → 非 0
			name:     "e_位置非法被拒",
			profile:  "mode: set\n" + windowsNamepolicyFile + ":1.x,2.2 3 1\n",
			wantExit: exitInvalid,
			wantOut:  []string{"的列号非法"},
		},
		{
			// (e) 混入空行(典型的手工编辑痕迹)→ 非 0,而不是"跳过就好"
			name:     "e_空行被拒",
			profile:  "mode: set\n\n",
			wantExit: exitInvalid,
			wantOut:  []string{"空行"},
		},
		{
			// (f) critical 不在 packages 里 → 配置非法,非 0
			name:     "f_critical缺阈值被拒",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			config:   `{"overall": 1.0, "packages": {"` + npPkg + `": 80.0}, "critical": ["` + objlockPkg + `"]}`,
			wantExit: exitInvalid,
			wantOut:  []string{"critical 包 " + objlockPkg + " 未在 packages 中给出阈值"},
		},
		{
			// (f) 字段名拼错(packages → package)必须报错:
			// 若静默忽略,全部包阈值会一起消失,门禁变成一句空话
			name:     "f_未知字段被拒",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			config:   `{"overall": 1.0, "package": {"` + npPkg + `": 80.0}}`,
			wantExit: exitInvalid,
			wantOut:  []string{"未知字段"},
		},
		{
			// (f) 阈值越界 → 配置非法
			name:     "f_阈值越界被拒",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			config:   `{"packages": {"` + npPkg + `": 150.0}}`,
			wantExit: exitInvalid,
			wantOut:  []string{"不在 [0,100] 区间"},
		},
		{
			// (f) 配置根本不是 JSON → 非 0
			name:     "f_配置非JSON被拒",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			config:   `{`,
			wantExit: exitInvalid,
			wantOut:  []string{"不是合法 JSON"},
		},
		{
			// -top 控制"最低覆盖率"列表长度
			name: "a_top限制列表长度",
			profile: covProfile(
				covRec(windowsNamepolicyFile, 10, 1),
				covRec(slashObjlockFile, 10, 0),
			),
			top:      "1",
			wantExit: exitOK,
			wantOut:  []string{"覆盖率最低的 1 个包(共 2 个)", "0.00%  " + objlockPkg},
		},
		{
			// 参数越界同样是"门禁坏了",不是"通过"
			name:     "f_min越界被拒",
			profile:  covProfile(covRec(windowsNamepolicyFile, 10, 1)),
			min:      "101",
			wantExit: exitInvalid,
			wantOut:  []string{"-min 101 不在 [0,100] 区间"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			args := []string{"-profile", writeTemp(t, dir, "cover.out", tc.profile)}
			if tc.config != "" {
				args = append(args, "-config", writeTemp(t, dir, "thresholds.json", tc.config))
			}
			if tc.min != "" {
				args = append(args, "-min", tc.min)
			}
			if tc.top != "" {
				args = append(args, "-top", tc.top)
			}

			// stdout 与 stderr 合流:报告与失败行都应该被人看到,
			// 而退出码已经足以区分"拦住"与"自己坏了"。
			var out bytes.Buffer
			got := run(args, &out, &out)
			if got != tc.wantExit {
				t.Fatalf("退出码 = %d,期望 %d\n输出:\n%s", got, tc.wantExit, out.String())
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("输出缺少 %q\n实际输出:\n%s", want, out.String())
				}
			}
			for _, notWant := range tc.wantNotOut {
				if strings.Contains(out.String(), notWant) {
					t.Errorf("输出不应包含 %q\n实际输出:\n%s", notWant, out.String())
				}
			}
		})
	}
}

// TestRunRejectsMissingFiles 覆盖"文件不存在"这一类输入错误:
// 找不到 profile 或找不到配置都必须响亮失败,绝不能"读不到就当没有阈值"。
func TestRunRejectsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	missingProfile := filepath.Join(dir, "not-there.out")
	realProfile := writeTemp(t, dir, "cover.out", covProfile(covRec(windowsNamepolicyFile, 1, 1)))

	cases := []struct {
		name     string
		args     []string
		wantExit int
		wantOut  string
	}{
		{
			name:     "缺-profile参数",
			args:     nil,
			wantExit: exitInvalid,
			wantOut:  "必须用 -profile 指定覆盖率文件",
		},
		{
			name:     "profile文件不存在",
			args:     []string{"-profile", missingProfile},
			wantExit: exitInvalid,
			wantOut:  "打开覆盖率文件失败",
		},
		{
			name:     "config文件不存在",
			args:     []string{"-profile", realProfile, "-config", filepath.Join(dir, "no.json")},
			wantExit: exitInvalid,
			wantOut:  "读取阈值配置失败",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			got := run(tc.args, &out, &out)
			if got != tc.wantExit {
				t.Fatalf("退出码 = %d,期望 %d\n输出:\n%s", got, tc.wantExit, out.String())
			}
			if !strings.Contains(out.String(), tc.wantOut) {
				t.Errorf("输出缺少 %q\n实际输出:\n%s", tc.wantOut, out.String())
			}
		})
	}
}

// ---- (g) 文件路径 → import path ----

func TestDeriveImportPath(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		want    string
		wantErr string // 非空表示期望报错(报错信息需包含该子串)
	}{
		{
			name: "Windows反斜杠绝对路径",
			file: windowsNamepolicyFile,
			want: npPkg,
		},
		{
			name: "斜杠绝对路径",
			file: slashObjlockFile,
			want: objlockPkg,
		},
		{
			name: "cmd下的包",
			file: windowsGateFile,
			want: gatePkg,
		},
		{
			// profile 有时已经是"模块内相对路径"(实测本仓库 go test 就是这么写的)
			name: "模块内相对路径",
			file: "github.com/netdisk/netdisk/internal/filesvc/service.go",
			want: filesvcPkg,
		},
		{
			// 嵌套同名目录时取**最后**一个 /internal/:
			// 取第一个会把包算成 internal/x,于是配置里的包"没有数据"被误报
			name: "取最后一个internal片段",
			file: "D:/repo/server/internal/outer/internal/inner/deep.go",
			want: "github.com/netdisk/netdisk/internal/inner",
		},
		{
			name:    "找不到internal或cmd片段",
			file:    "D:/repo/server/pkg/thing/thing.go",
			wantErr: "找不到 /internal/ 或 /cmd/ 片段",
		},
		{
			name:    "空路径",
			file:    "   ",
			wantErr: "空文件路径",
		},
		{
			name:    "internal后面没有包目录",
			file:    "D:/repo/server/internal",
			wantErr: "找不到 /internal/ 或 /cmd/ 片段",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := deriveImportPath(tc.file)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("期望报错(%s),实际得到 %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("报错信息 %q 未包含 %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != tc.want {
				t.Fatalf("import path = %q,期望 %q", got, tc.want)
			}
		})
	}
}

// ---- 解析:合法输入的字段级校验 ----

func TestParseProfileFields(t *testing.T) {
	// 混入 CRLF、盘符冒号、两种路径风格与 0 计数:
	// 这些都是真实 profile 里出现过、且最容易把解析器写坏的地方。
	in := "mode: counter\r\n" +
		"github.com/netdisk/netdisk/internal/namepolicy/namepolicy.go:12.3,14.9 5 7\r\n" +
		`D:\WorkSpace\GO\netdisk\server\cmd\coveragegate\main.go:1.1,2.2 3 0` + "\n"

	p, err := parseProfile(strings.NewReader(in))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if p.mode != "counter" {
		t.Fatalf("mode = %q,期望 counter", p.mode)
	}
	want := []block{
		{file: "github.com/netdisk/netdisk/internal/namepolicy/namepolicy.go", statements: 5, count: 7},
		{file: `D:\WorkSpace\GO\netdisk\server\cmd\coveragegate\main.go`, statements: 3, count: 0},
	}
	if len(p.blocks) != len(want) {
		t.Fatalf("记录数 = %d,期望 %d", len(p.blocks), len(want))
	}
	for i := range want {
		if p.blocks[i] != want[i] {
			t.Errorf("第 %d 条 = %+v,期望 %+v", i+1, p.blocks[i], want[i])
		}
	}
}

func TestParseProfileAcceptsAllModes(t *testing.T) {
	for _, mode := range []string{"set", "counter", "atomic"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := parseProfile(strings.NewReader("mode: " + mode + "\n" + covRec(windowsNamepolicyFile, 1, 1) + "\n")); err != nil {
				t.Fatalf("mode %q 应被接受: %v", mode, err)
			}
		})
	}
}

// TestParseProfileRejectsMalformed 把"坏输入"逐项列清。
//
// 这里每一条都对应一种"如果宽容处理就会静默给出假结论"的输入:
// 少一个字段、数字写成词、位置坏掉、文件被截断 —— 全都必须报错。
func TestParseProfileRejectsMalformed(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantErr string
	}{
		{"空文件", "", "缺少 `mode:` 头"},
		{"首行不是mode", "set\n" + covRec(windowsNamepolicyFile, 1, 1) + "\n", "缺少 `mode:` 头"},
		{"未知mode", "mode: count\n" + covRec(windowsNamepolicyFile, 1, 1) + "\n", "未知的覆盖率模式"},
		{"mode后为空", "mode:\n" + covRec(windowsNamepolicyFile, 1, 1) + "\n", "未知的覆盖率模式"},
		{"只有mode头", "mode: set\n", "不含任何语句统计"},
		{"缺count字段", "mode: set\n" + windowsNamepolicyFile + ":1.1,2.2 3\n", "缺少 numStatements 字段"},
		{"缺numStatements字段", "mode: set\n" + windowsNamepolicyFile + " 1\n", "缺少 numStatements 字段"},
		{"numStatements非数字", "mode: set\n" + windowsNamepolicyFile + ":1.1,2.2 abc 1\n", "不是整数"},
		{"count非数字", "mode: set\n" + windowsNamepolicyFile + ":1.1,2.2 3 1x\n", "不是整数"},
		{"numStatements为负", "mode: set\n" + windowsNamepolicyFile + ":1.1,2.2 -3 1\n", "为负数"},
		{"count为负", "mode: set\n" + windowsNamepolicyFile + ":1.1,2.2 3 -1\n", "为负数"},
		{"位置缺冒号", "mode: set\nfile.go 3 1\n", "缺少 `文件:起止位置` 结构"},
		{"位置缺逗号", "mode: set\n" + windowsNamepolicyFile + ":1.1 3 1\n", "缺少 `起始,结束` 结构"},
		{"位置缺列号", "mode: set\n" + windowsNamepolicyFile + ":1.,2.2 3 1\n", "不是 `行.列` 结构"},
		{"位置行号非数字", "mode: set\n" + windowsNamepolicyFile + ":x.1,2.2 3 1\n", "的行号非法"},
		{"位置列号非数字", "mode: set\n" + windowsNamepolicyFile + ":1.x,2.2 3 1\n", "的列号非法"},
		{"空行", "mode: set\n\n" + covRec(windowsNamepolicyFile, 1, 1) + "\n", "空行"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProfile(strings.NewReader(tc.in))
			if err == nil {
				t.Fatal("期望解析报错,实际成功")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("报错信息 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// ---- (h) 语句数加权 ----

// TestSummarizeStatementWeighting 锁定"按语句数加权"的口径。
//
// 关键一条:一个 1 语句的大块被覆盖 + 9 条未覆盖语句,
// 覆盖率必须是 10% 而不是 100% —— 若按"块数/文件数"取平均,
// 门禁可以用一堆琐碎小函数刷绿,失去意义。
func TestSummarizeStatementWeighting(t *testing.T) {
	cases := []struct {
		name        string
		profile     string
		wantTotal   int
		wantCovered int
		wantPercent float64
		wantPkgs    int
	}{
		{
			name: "已覆盖大块不能被少数未覆盖语句稀释成100",
			profile: covProfile(
				covRec(windowsNamepolicyFile, 1, 1),
				covRec(windowsNamepolicyFile, 9, 0),
			),
			wantTotal:   10,
			wantCovered: 1,
			wantPercent: 10,
			wantPkgs:    1,
		},
		{
			name:        "全覆",
			profile:     covProfile(covRec(windowsNamepolicyFile, 7, 1)),
			wantTotal:   7,
			wantCovered: 7,
			wantPercent: 100,
			wantPkgs:    1,
		},
		{
			// 同一包被拆到两个文件里:必须合并成一条,而不是各算各的
			name: "同包多文件合并",
			profile: covProfile(
				covRec(windowsNamepolicyFile, 3, 1),
				covRec(`D:\WorkSpace\GO\netdisk\server\internal\namepolicy\shared_cases.go`, 1, 0),
			),
			wantTotal:   4,
			wantCovered: 3,
			wantPercent: 75,
			wantPkgs:    1,
		},
		{
			// set 模式 count 只有 0/1;counter 模式的大计数同样只算"覆盖一次",
			// 否则热点函数会把百分比抬到荒谬的高度
			name: "大计数只算覆盖不算加权",
			profile: covProfile(
				covRec(windowsNamepolicyFile, 2, 1000000),
				covRec(slashObjlockFile, 2, 0),
			),
			wantTotal:   4,
			wantCovered: 2,
			wantPercent: 50,
			wantPkgs:    2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseProfile(strings.NewReader(tc.profile))
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			sum := summarize(p)
			if len(sum.Packages) != tc.wantPkgs {
				t.Errorf("包数 = %d,期望 %d", len(sum.Packages), tc.wantPkgs)
			}
			if sum.Overall.Total != tc.wantTotal || sum.Overall.Covered != tc.wantCovered {
				t.Errorf("总体 = %d/%d,期望 %d/%d",
					sum.Overall.Covered, sum.Overall.Total, tc.wantCovered, tc.wantTotal)
			}
			if !almostEqual(sum.Overall.percent(), tc.wantPercent) {
				t.Errorf("总体百分比 = %v,期望 %v", sum.Overall.percent(), tc.wantPercent)
			}
		})
	}

	// (h) 的判定侧:10% 的包配 85% 阈值必须被判不达标
	t.Run("加权后的百分比参与判定", func(t *testing.T) {
		p, err := parseProfile(strings.NewReader(covProfile(
			covRec(windowsNamepolicyFile, 1, 1),
			covRec(windowsNamepolicyFile, 9, 0),
		)))
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		sum := summarize(p)
		cfg := &configFile{Overall: 0, Packages: map[string]float64{npPkg: 85}}
		viol := evaluate(sum, cfg, 0)
		if len(viol) != 1 {
			t.Fatalf("未达标项 = %d,期望 1:%+v", len(viol), viol)
		}
		if viol[0].Target != npPkg || !almostEqual(viol[0].Actual, 10) || !almostEqual(viol[0].Required, 85) {
			t.Fatalf("未达标项 = %+v,期望 %s 实际 10 要求 85", viol[0], npPkg)
		}
	})
}

// TestSummarizeSkipsUnderivableFiles 记录"无法反推 import path"的处理方式:
// 不参与统计,但必须出现在 Skipped 里被打印出来 —— 静默少算等于口径失真。
func TestSummarizeSkipsUnderivableFiles(t *testing.T) {
	p, err := parseProfile(strings.NewReader(covProfile(
		covRec(windowsNamepolicyFile, 10, 1),
		covRec("D:/repo/server/pkg/thing/thing.go", 90, 0),
	)))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	sum := summarize(p)
	if len(sum.Skipped) != 1 {
		t.Fatalf("Skipped = %d 条,期望 1:%v", len(sum.Skipped), sum.Skipped)
	}
	if sum.Overall.Total != 10 || !almostEqual(sum.Overall.percent(), 100) {
		t.Fatalf("统计口径 = %d/%d,期望只算可归属的 10/10", sum.Overall.Covered, sum.Overall.Total)
	}
	var out bytes.Buffer
	writeReport(&out, sum, nil, 0, 10, nil)
	if !strings.Contains(out.String(), "无法反推 import path") {
		t.Errorf("报告应提示口径缺失,实际:\n%s", out.String())
	}
}

// ---- 判定与配置校验(纯函数) ----

func TestEvaluate(t *testing.T) {
	sum := &summary{
		Mode: "set",
		Packages: map[string]pkgStat{
			npPkg:      {ImportPath: npPkg, Total: 10, Covered: 10},
			objlockPkg: {ImportPath: objlockPkg, Total: 10, Covered: 2},
		},
		Overall: pkgStat{Total: 20, Covered: 12}, // 60%
	}
	wantRef := &configFile{
		Overall:  61,
		Packages: map[string]float64{npPkg: 100, objlockPkg: 80},
		Critical: []string{objlockPkg},
	}

	cases := []struct {
		name           string
		cfg            *configFile
		min            float64
		want           []violation
		wantViolations int
	}{
		{
			name:           "全部达标",
			cfg:            &configFile{Overall: 60, Packages: map[string]float64{npPkg: 100, objlockPkg: 20}},
			wantViolations: 0,
		},
		{
			// 边界:实际 == 要求 视为达标
			name:           "恰好等于要求算达标",
			cfg:            &configFile{Overall: 60, Packages: map[string]float64{objlockPkg: 20}},
			wantViolations: 0,
		},
		{
			name: "总体与单包同时不达标",
			cfg:  &configFile{Overall: 61, Packages: map[string]float64{objlockPkg: 80}},
			want: []violation{
				{Kind: kindOverall, Actual: 60, Required: 61},
				{Kind: kindPackage, Target: objlockPkg, Actual: 20, Required: 80},
			},
			wantViolations: 2,
		},
		{
			name: "critical标记带到未达标项上",
			cfg:  wantRef,
			want: []violation{
				{Kind: kindOverall, Actual: 60, Required: 61},
				{Kind: kindPackage, Target: objlockPkg, Actual: 20, Required: 80, Critical: true},
			},
			wantViolations: 2,
		},
		{
			// 60 == 60 → 达标(边界算过);下面一条用 61 验证它确实会拦
			name:           "无配置时-min恰好等于实际值算达标",
			cfg:            nil,
			min:            60,
			wantViolations: 0,
		},
		{
			name:           "无配置时-min拦住",
			cfg:            nil,
			min:            61,
			want:           []violation{{Kind: kindOverall, Actual: 60, Required: 61}},
			wantViolations: 1,
		},
		{
			// -min 与配置 overall 取更严的一者:两边都不能把对方放宽
			name:           "min比配置严时以min为准",
			cfg:            &configFile{Overall: 10},
			min:            70,
			want:           []violation{{Kind: kindOverall, Actual: 60, Required: 70}},
			wantViolations: 1,
		},
		{
			name:           "配置比min严时以配置为准",
			cfg:            &configFile{Overall: 70},
			min:            10,
			want:           []violation{{Kind: kindOverall, Actual: 60, Required: 70}},
			wantViolations: 1,
		},
		{
			// 配了阈值但 profile 里没有这个包 → 失败,且标出 Missing
			name: "包无数据判失败",
			cfg:  &configFile{Packages: map[string]float64{filesvcPkg: 1}, Critical: []string{filesvcPkg}},
			want: []violation{
				{Kind: kindPackage, Target: filesvcPkg, Required: 1, Missing: true, Critical: true},
			},
			wantViolations: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluate(sum, tc.cfg, tc.min)
			if len(got) != tc.wantViolations {
				t.Fatalf("未达标项 = %d 条,期望 %d:%+v", len(got), tc.wantViolations, got)
			}
			for i, w := range tc.want {
				g := got[i]
				if g.Kind != w.Kind || g.Target != w.Target || g.Missing != w.Missing || g.Critical != w.Critical {
					t.Errorf("第 %d 项 = %+v,期望 %+v", i+1, g, w)
				}
				if !almostEqual(g.Actual, w.Actual) || !almostEqual(g.Required, w.Required) {
					t.Errorf("第 %d 项 实际/要求 = %v/%v,期望 %v/%v",
						i+1, g.Actual, g.Required, w.Actual, w.Required)
				}
			}
		})
	}
}

// TestViolationMessage 锁定失败行的措辞:必须同时给出**实际值**与**要求值**,
// 否则值班的人只能知道"红了",不知道该补多少测试。
func TestViolationMessage(t *testing.T) {
	cases := []struct {
		name string
		v    violation
		want []string
	}{
		{
			name: "总体",
			v:    violation{Kind: kindOverall, Actual: 10, Required: 60},
			want: []string{"总体覆盖率", "10.00%", "60.00%"},
		},
		{
			name: "单包",
			v:    violation{Kind: kindPackage, Target: filesvcPkg, Actual: 12.345, Required: 80},
			want: []string{filesvcPkg, "12.35%", "80.00%", "低于要求"},
		},
		{
			name: "单包无数据",
			v:    violation{Kind: kindPackage, Target: filesvcPkg, Required: 80, Missing: true, Critical: true},
			want: []string{"[critical]", filesvcPkg, "无覆盖率数据", "80.00%"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg := tc.v.String()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("失败行 %q 缺少 %q", msg, want)
				}
			}
		})
	}
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     configFile
		wantErr string
	}{
		{
			name: "合法",
			cfg: configFile{
				Overall:  55,
				Packages: map[string]float64{npPkg: 85},
				Critical: []string{npPkg},
			},
		},
		{
			name:    "critical缺阈值",
			cfg:     configFile{Packages: map[string]float64{npPkg: 85}, Critical: []string{objlockPkg}},
			wantErr: "未在 packages 中给出阈值",
		},
		{
			name: "critical为空字符串也算缺阈值",
			cfg:  configFile{Packages: map[string]float64{npPkg: 85}, Critical: []string{""}},
			// 空 import path 不在 packages 里 → 同样报错(而不是被当成"没写")
			wantErr: "未在 packages 中给出阈值",
		},
		{
			name:    "overall越界",
			cfg:     configFile{Overall: 101},
			wantErr: "overall 阈值 101 不在 [0,100] 区间",
		},
		{
			name:    "包阈值越界",
			cfg:     configFile{Packages: map[string]float64{npPkg: -1}},
			wantErr: "不在 [0,100] 区间",
		},
		{
			name:    "包名为空",
			cfg:     configFile{Packages: map[string]float64{"  ": 50}},
			wantErr: "空 import path",
		},
		{
			name: "critical重复不报错",
			cfg: configFile{
				Packages: map[string]float64{npPkg: 85},
				Critical: []string{npPkg, npPkg},
			},
		},
		{
			name: "空包列表",
			cfg:  configFile{Overall: 30},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConfig(&tc.cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("不应报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报错,实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("报错信息 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRunUsesPurePipeline 确认 run() 与纯函数口径一致:
// 同一份 profile 经 run() 得到的总体百分比,必须与 summarize() 算出的一致。
//
// 若哪天有人把报告路径改成"自己再算一遍",这条会先红 ——
// 否则报告与实际判定会各自漂移,而两边看起来都很合理。
func TestRunUsesPurePipeline(t *testing.T) {
	dir := t.TempDir()
	profile := covProfile(
		covRec(windowsNamepolicyFile, 3, 1),
		covRec(windowsNamepolicyFile, 1, 0),
		covRec(slashFilesvcFile, 2, 0),
	)
	profPath := writeTemp(t, dir, "cover.out", profile)

	p, err := parseProfile(strings.NewReader(profile))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	want := summarize(p)

	var out bytes.Buffer
	if code := run([]string{"-profile", profPath}, &out, &out); code != exitOK {
		t.Fatalf("无阈值时应退出 0,实际 %d\n输出:\n%s", code, out.String())
	}
	marker := fmt.Sprintf("总体覆盖率: %.2f%%", want.Overall.percent())
	if !strings.Contains(out.String(), marker) {
		t.Errorf("报告缺少 %q\n实际输出:\n%s", marker, out.String())
	}
	if !almostEqual(want.Overall.percent(), 50) {
		t.Errorf("总体百分比 = %v,期望 50", want.Overall.percent())
	}
}
