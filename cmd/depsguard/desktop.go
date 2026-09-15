package main

// 桌面端工程的**架构纪律静态校验**(DE-D-01 验收 / DE-D-02 ② / 7.7)。
//
// 放在 depsguard 而不是另起一个工具:它已经是"架构纪律的静态校检"这一件事的
// 唯一入口,CI 里只需要跑一条命令;分成两个工具后必然有一个被漏跑,
// 而漏跑的那条纪律会在几个月后以"想拆仓/想写单测时才发现全绑死了"的形式暴露。
//
// 检查四条(**每一条都能在编译期通过的情况下被破坏**,所以必须静态盯):
//
//	① 只有 NetDisk.App 可以开 UseWPF(Setup 是打包工程,不涉及 WPF)
//	② 只有 NetDisk.App 可以开 UseWindowsForms
//	③ NetDisk.ClientCore 必须是平台无关的 `net10.0`(不带 `-windows` 后缀),
//	   且源码里不得出现 Windows API / WPF 类型
//	④ 依赖方向:App → SyncEngine/Transport → ClientCore,**反向引用一律拒绝**
//
// 为什么 ③ 还要扫源码而不只看 csproj:csproj 只声明框架,`System.Windows` 与
// `DllImport` 这些**类型**是靠 `using` 引进来的 —— 大框架(如 net10.0-windows)
// 里两者都能编过,只有源码扫描能发现。

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// desktopProjects 是 5 个工程的目录名与它们的框架纪律。
//
//   - `windowsOK`:该工程是否允许出现 `-windows` 目标框架(即允许用 Windows 能力)
//   - `refsAllowed`:该工程允许引用的其它工程(反向引用即违规)
var desktopProjects = map[string]struct {
	windowsOK   bool
	refsAllowed []string
}{
	"NetDisk.ClientCore": {windowsOK: false, refsAllowed: nil},
	"NetDisk.Transport":  {windowsOK: false, refsAllowed: []string{"NetDisk.ClientCore"}},
	"NetDisk.SyncEngine": {windowsOK: true, refsAllowed: []string{"NetDisk.ClientCore", "NetDisk.Transport"}},
	"NetDisk.App":        {windowsOK: true, refsAllowed: []string{"NetDisk.ClientCore", "NetDisk.Transport", "NetDisk.SyncEngine"}},
	"NetDisk.Setup":      {windowsOK: true, refsAllowed: nil}, // 打包工程不引用代码工程
}

// clientCoreForbidden 是 ClientCore 源码里**不得出现**的模式(DE-D-02 ②)。
//
// 每一条都对应一类"能编译、但把内核绑死在 Windows 上"的写法:
//   - `System.Windows`        → WPF 类型;
//   - `System.Windows.Forms`  → WinForms;
//   - `System.Runtime.InteropServices` → DllImport/P-Invoke(注册表、文件 ID、DPAPI 都从这里进来);
//   - `Microsoft.Win32`       → 注册表/系统事件;
//   - `[DllImport`            → 直接的 P/Invoke 声明。
var clientCoreForbidden = []*regexp.Regexp{
	regexp.MustCompile(`using\s+System\.Windows\s*;`),
	regexp.MustCompile(`using\s+System\.Windows\.`),
	regexp.MustCompile(`using\s+System\.Runtime\.InteropServices`),
	regexp.MustCompile(`using\s+Microsoft\.Win32`),
	regexp.MustCompile(`\[\s*DllImport`),
}

// uiForbiddenInEngine 是**不是 UI 层**的工程里不得出现的 UI 线程设施(DE-D-21)。
//
// 验收原文:"同步引擎与 UI 线程严格分离"。
//
// 为什么必须静态盯:把 `Dispatcher` 引到同步引擎里,编译**一定**过,运行时也**大
// 多数时候**看起来正常 —— 直到某天同步循环在 UI 线程上跑哈希导致"未响应",
// 或者后台线程直接碰 UI 对象导致偶发崩溃/句柄泄漏。到那时已经没人记得是谁引进来的。
//
// 注意:这里扫的是**去掉注释后的源码**。规则本身会让注释里出现 "Dispatcher" 这样的
// 词(比如解释"为什么不能引 Dispatcher"),若不剥注释,规则禁止的东西与解释它的注释
// 就会互相打架 —— 那是规则写错,不是代码写错。
var uiForbiddenInEngine = []*regexp.Regexp{
	regexp.MustCompile(`using\s+System\.Windows\s*;`),
	regexp.MustCompile(`using\s+System\.Windows\.`),
	regexp.MustCompile(`using\s+System\.Windows\.Forms`),
	regexp.MustCompile(`\bDispatcher\b`),
	regexp.MustCompile(`\bSynchronizationContext\b`),
	regexp.MustCompile(`\bNotifyIcon\b`),
	regexp.MustCompile(`\bApplication\.Current\b`),
}

// engineProjects 是"绝不能碰 UI 线程设施"的三个工程。
var engineProjects = []string{"NetDisk.ClientCore", "NetDisk.Transport", "NetDisk.SyncEngine"}

// checkThreadDiscipline 是 depsguard 的第五组桌面纪律(DE-D-21)。
//
// 三部分,缺一不可 —— 只做第一部分的话,"违规检测为 0"完全可以靠
// **从不启用探测器**来达成(那正是这一项最容易做假的地方):
//
//	a. 内核/传输/同步引擎**不得**出现 WPF/WinForms/Dispatcher/SynchronizationContext;
//	b. App 必须**起誓** UI 线程(`ThreadDiscipline.MarkUiThread()`)—— 探测器被真正启用;
//	c. 托盘适配器必须**marshal 回 UI 线程**(`CheckAccess()` + `BeginInvoke(`),
//	   并在真正碰 UI 对象处记账(`AssertOnUiThread`)—— 后台通知不许直接碰 UI 对象。
func checkThreadDiscipline(repoRoot string) ([]string, error) {
	desktopDir := filepath.Join(repoRoot, "desktop")
	if _, err := os.Stat(desktopDir); err != nil {
		return nil, nil
	}
	var problems []string

	// ---- a. 引擎侧不得触碰 UI 线程设施 ----
	for _, name := range engineProjects {
		dir := filepath.Join(desktopDir, "src", name)
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if base := filepath.Base(path); base == "bin" || base == "obj" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".cs") {
				return nil
			}
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			code := stripCsharpComments(string(raw))
			for _, re := range uiForbiddenInEngine {
				if loc := re.FindString(code); loc != "" {
					rel, _ := filepath.Rel(repoRoot, path)
					problems = append(problems, fmt.Sprintf(
						"%s 出现 %q —— %s 必须与 UI 线程严格分离(DE-D-21):UI 设施只能在 App 里,后台线程要碰 UI 必须 marshal",
						filepath.ToSlash(rel), strings.TrimSpace(loc), name))
				}
			}
			return nil
		})
	}

	// ---- b. App 必须启用线程纪律探测器 ----
	appEntry := filepath.Join(desktopDir, "src", "NetDisk.App", "App.xaml.cs")
	if raw, err := os.ReadFile(appEntry); err == nil {
		code := stripCsharpComments(string(raw))
		if !strings.Contains(code, "ThreadDiscipline.MarkUiThread()") {
			problems = append(problems,
				"desktop/src/NetDisk.App/App.xaml.cs 没有调用 ThreadDiscipline.MarkUiThread() —— "+
					"探测器没启用时「Dispatcher 违规检测为 0」是空话(不能判定不等于通过),DE-D-21 要求 App 起誓 UI 线程")
		}
	}

	// ---- c. 托盘适配器必须 marshal ----
	tray := filepath.Join(desktopDir, "src", "NetDisk.App", "Notify", "TrayNotifier.cs")
	if raw, err := os.ReadFile(tray); err == nil {
		code := stripCsharpComments(string(raw))
		if !strings.Contains(code, "CheckAccess()") || !strings.Contains(code, "BeginInvoke(") {
			problems = append(problems,
				"desktop/src/NetDisk.App/Notify/TrayNotifier.cs 缺少 CheckAccess()/BeginInvoke(:后台通知来自同步引擎线程,"+
					"直接碰 NotifyIcon 会偶发不显示/句柄泄漏,必须先 marshal 回 UI 线程(DE-D-21)")
		}
		if !regexp.MustCompile(`AssertOnUiThread\([^\n]*\);\s*\n\s*ShowBalloonTip\(`).MatchString(code) {
			problems = append(problems,
				"desktop/src/NetDisk.App/Notify/TrayNotifier.cs 的 marshal 分支里没有「记账后立刻显示」的调用序列 —— "+
					"要求 `AssertOnUiThread(...);` 紧接 `ShowBalloonTip(n);`:marshal 被删掉时(refresh 回后台线程)"+
					"必须留下信号(DE-D-21)。只在构造函数里记账是不够的:真正碰 UI 对象的是显示气泡那一步")
		}
	}

	return problems, nil
}

// stripCsharpComments 去掉 `//` 行注释与 `/* */` 块注释。
//
// 只服务于"扫关键字"这一用途:不解析字符串字面量,所以 `"https://x"` 这种会把它
// 后面的内容切掉 —— 对禁止项(UI 类型名)的判断没有影响,而反过来(因为一句注释里
// 提到 Dispatcher 就报违规)会逼着人把解释删掉,那才是真的坏结果。
func stripCsharpComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	inBlock := false
	for i := 0; i < len(src); i++ {
		if inBlock {
			if i+1 < len(src) && src[i] == '*' && src[i+1] == '/' {
				inBlock = false
				i++
			}
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			inBlock = true
			i++
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				b.WriteByte('\n')
			}
			continue
		}
		b.WriteByte(src[i])
	}
	return b.String()
}

// checkDesktop 返回违规描述列表(空 = 全部通过)。
func checkDesktop(repoRoot string) ([]string, error) {
	desktopDir := filepath.Join(repoRoot, "desktop")
	if _, err := os.Stat(desktopDir); err != nil {
		// 桌面端还没落地(第 4 卷之前)时**不算违规**:空目录不存在是合法状态
		return nil, nil
	}
	var problems []string

	// ---- ① ② 框架/UI 开关 + ④ 依赖方向 ----
	for name, rule := range desktopProjects {
		projDir := filepath.Join(desktopDir, "src", name)
		entries, err := os.ReadDir(projDir)
		if err != nil {
			problems = append(problems, fmt.Sprintf("缺少工程目录 desktop/src/%s(11 章的 5 工程骨架)", name))
			continue
		}
		var projFile string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".csproj") || strings.HasSuffix(e.Name(), ".wixproj") {
				projFile = filepath.Join(projDir, e.Name())
			}
		}
		if projFile == "" {
			problems = append(problems, fmt.Sprintf("desktop/src/%s 下没有工程文件", name))
			continue
		}
		raw, err := os.ReadFile(projFile)
		if err != nil {
			return nil, err
		}
		text := string(raw)

		// ① UseWPF 只属于 App
		if name != "NetDisk.App" && strings.Contains(strings.ToLower(text), "<usewpf>true") {
			problems = append(problems,
				fmt.Sprintf("%s 开了 UseWPF —— 只有 NetDisk.App 可以有 WPF(7.7:App 之外禁止引用 WPF 类型)", name))
		}
		// ② UseWindowsForms 同理
		if name != "NetDisk.App" && strings.Contains(strings.ToLower(text), "<usewindowsforms>true") {
			problems = append(problems,
				fmt.Sprintf("%s 开了 UseWindowsForms —— 只有 NetDisk.App 可以有 UI 框架", name))
		}
		// ③ 框架后缀
		if !rule.windowsOK {
			if m := regexp.MustCompile(`<TargetFramework>\s*([^<]+?)\s*</TargetFramework>`).FindStringSubmatch(text); m != nil {
				if strings.Contains(m[1], "-windows") {
					problems = append(problems,
						fmt.Sprintf("%s 的目标框架是 %s —— 该工程必须平台无关(ADR-4:零 Windows API,故不能带 -windows 后缀)",
							name, m[1]))
				}
			}
		}
		// ④ 依赖方向
		refs := regexp.MustCompile(`ProjectReference\s+Include="[^"]*?\\(NetDisk\.[A-Za-z]+)\\`).FindAllStringSubmatch(text, -1)
		for _, r := range refs {
			target := r[1]
			allowed := false
			for _, a := range rule.refsAllowed {
				if a == target {
					allowed = true
					break
				}
			}
			if !allowed {
				problems = append(problems,
					fmt.Sprintf("%s 引用了 %s —— 违反依赖方向(App → SyncEngine/Transport → ClientCore,反向引用会让内核无法被单测/复用)",
						name, target))
			}
		}
	}

	// ---- ③ ClientCore 源码不得出现 Windows/WPF 类型 ----
	coreDir := filepath.Join(desktopDir, "src", "NetDisk.ClientCore")
	_ = filepath.WalkDir(coreDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			// 跳过生成物与构建输出:Generated/Models.g.cs 是契约生成的 DTO,
			// 但它同样受纪律约束(它只用 System.Text.Json 与 BCL 类型),所以**不跳过**
			if err != nil {
				return nil
			}
			base := filepath.Base(path)
			if base == "bin" || base == "obj" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".cs") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, re := range clientCoreForbidden {
			if loc := re.FindString(string(raw)); loc != "" {
				rel, _ := filepath.Rel(repoRoot, path)
				problems = append(problems,
					fmt.Sprintf("%s 出现 %q —— ClientCore 必须零 WPF / 零 Windows API(ADR-4)",
						filepath.ToSlash(rel), strings.TrimSpace(loc)))
			}
		}
		return nil
	})

	return problems, nil
}
