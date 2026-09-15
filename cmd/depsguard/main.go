// Command depsguard 是**架构纪律的静态校验**:检查"内核包不得依赖 api 包"。
//
// 为什么需要它:BE-S5-01 验收③要求 `finalize` 包不 import `api`。
// 这条纪律用"代码审查"保证不住 —— 一次 `import` 自动补全就能破坏它,
// 而破坏后的症状是**编译期仍然通过**,只在架构上形成"业务逻辑依赖 HTTP 层"
// 的倒置(后续想被 TUS/WebDAV/worker 复用时才发现拿不动)。
//
// 用 `go list -deps` 做**传递**依赖检查,而不是匹配源码里的 import 行:
// 直接 import 检查会漏掉 "finalize → X → api" 这类间接依赖,
// 而那同样构成分层倒置。
//
// 用法(在 server/ 目录下):
//
//	go run ./cmd/depsguard
//
// 退出码非 0 表示纪律被破坏,可直接用于 CI 门禁。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 内核包:承载业务规则、必须能被任意入口复用,因此**不得**依赖 HTTP 层。
var kernelPkgs = []string{
	"github.com/netdisk/netdisk/internal/finalize",
	"github.com/netdisk/netdisk/internal/lifecycle",
	"github.com/netdisk/netdisk/internal/objlock",
	"github.com/netdisk/netdisk/internal/uploadsvc",
	"github.com/netdisk/netdisk/internal/filesvc",
	"github.com/netdisk/netdisk/internal/spacesvc",
	"github.com/netdisk/netdisk/internal/orgsvc",
	"github.com/netdisk/netdisk/internal/authsvc",
	"github.com/netdisk/netdisk/internal/storage",
	"github.com/netdisk/netdisk/internal/repo",
	"github.com/netdisk/netdisk/internal/namepolicy",
}

// 禁止依赖的目标(HTTP/路由层)。
//
// 注意 `apierr` **不在此列**:它是纯错误码/响应体的数据定义,
// 被各层共用是刻意的(否则内核层无法产出结构化错误给 handler)。
const forbidden = "github.com/netdisk/netdisk/internal/api"

func main() {
	broken := false
	for _, pkg := range kernelPkgs {
		deps, err := listDeps(pkg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: 无法列出 %s 的依赖: %v\n", pkg, err)
			os.Exit(2)
		}
		hits := []string{}
		for _, d := range deps {
			if d == forbidden {
				hits = append(hits, d)
			}
		}
		if len(hits) > 0 {
			broken = true
			fmt.Printf("✗ %s **依赖了** %s(分层倒置:内核包不得依赖 HTTP 层)\n", pkg, forbidden)
		} else {
			fmt.Printf("✓ %s 不依赖 %s(传递依赖共 %d 个)\n", pkg, forbidden, len(deps))
		}
	}
	if broken {
		fmt.Fprintln(os.Stderr, "\n架构纪律被破坏:内核包必须可被任意入口(TUS/WebDAV/worker)复用。")
		os.Exit(1)
	}
	fmt.Println("\n全部通过:内核包与 HTTP 层解耦。")

	// 桌面端工程纪律(DE-D-01 验收 ③ / DE-D-02 ②)
	root, rerr := moduleRoot()
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 无法定位仓库根(桌面端纪律未检查): %v\n", rerr)
		os.Exit(2)
	}
	dprobs, derr := checkDesktop(root)
	if derr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 检查桌面端工程失败: %v\n", derr)
		os.Exit(2)
	}
	if len(dprobs) > 0 {
		for _, p := range dprobs {
			fmt.Printf("✗ %s\n", p)
		}
		fmt.Fprintln(os.Stderr, "\n桌面端纪律被破坏:App 之外不得用 WPF;ClientCore 必须平台无关且零 Windows API;依赖方向单向。")
		os.Exit(1)
	}
	fmt.Println("全部通过:桌面端 5 工程纪律(UseWPF 只在 App / ClientCore 平台无关 / 依赖方向单向)。")

	// 第五组:线程纪律(DE-D-21)—— 同步引擎与 UI 线程严格分离,且探测器必须被启用
	tprobs, terr := checkThreadDiscipline(root)
	if terr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 检查线程纪律失败: %v\n", terr)
		os.Exit(2)
	}
	if len(tprobs) > 0 {
		for _, p := range tprobs {
			fmt.Printf("✗ %s\n", p)
		}
		fmt.Fprintln(os.Stderr, "\n线程纪律被破坏:引擎侧不得触碰 UI 线程设施;App 必须启用探测器;托盘必须 marshal 回 UI 线程。")
		os.Exit(1)
	}
	fmt.Println("全部通过:线程纪律(引擎零 UI 设施 / App 起誓 UI 线程 / 托盘 marshal 且记账)。")

	// 第 1 卷红线里**机械可检**的三条(R-24 不得有 Rust / R-25 WiX 固定 6.0.2 / R-26 MSI perUser+MajorUpgrade)
	rl, rerr2 := checkRedlines(root)
	if rerr2 != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 红线检查失败: %v\n", rerr2)
		os.Exit(2)
	}
	if len(rl) > 0 {
		for _, p := range rl {
			fmt.Printf("✗ %s\n", p)
		}
		fmt.Fprintln(os.Stderr, "\n红线被破坏(见上)。")
		os.Exit(1)
	}
	fmt.Println("全部通过:红线机械可检项(R-01 前端 embed / R-02 缓存头两档 / R-14 JWT 算法白名单 / " +
		"R-15 secret 不进 yaml / R-20 无 LOCK 且实现 ETager / R-21 etag 生成列无引号且出口唯一 / " +
		"R-24 无 Rust / R-25 WiX=6.0.2 / R-26 MSI perUser+MajorUpgrade)。")

	// 第六组:文档与运维制品一致性(用户要求③"检查所有文档资料"的机械部分)
	dprobs2, derr2 := checkDocsAndOps(root)
	if derr2 != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 文档一致性检查失败: %v\n", derr2)
		os.Exit(2)
	}
	if len(dprobs2) > 0 {
		for _, p := range dprobs2 {
			fmt.Printf("✗ %s\n", p)
		}
		fmt.Fprintf(os.Stderr, "\n文档/运维制品不一致(%d 项,见上)。\n", len(dprobs2))
		os.Exit(1)
	}
	fmt.Println("全部通过:文档与运维制品一致性(互引路径存在 / 文档与告警引用的指标都存在 / " +
		"活文档覆盖全部指标 / 示例配置与 Go 配置项一一对应 / 文档点名的代码文件存在 / " +
		"文档点名的 Go 测试用例存在)。")
}

// moduleRoot 返回仓库根目录(server/ 的上一层)。
//
// 用 `go env GOMOD` 而不是相对路径:本工具会被 CI 从不同工作目录调用,
// 而 GOMOD 永远指向当前模块的 go.mod(其父目录就是 server/,再上一层是仓库根)。
func moduleRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", err
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("go env GOMOD 为空(不在模块内?)")
	}
	// 四仓拆分后 go.mod 就在仓库根(Server-com/),不再是原 monorepo 的 server/ 子目录,
	// 因此取一层即可拿到底层 repo 根;再往上取就会逃出本仓。
	return filepath.Dir(gomod), nil
}

// listDeps 返回某包的**全部传递**依赖(含自身)。
func listDeps(pkg string) ([]string, error) {
	out, err := exec.Command("go", "list", "-deps", "-json", pkg).Output()
	if err != nil {
		// 退化为纯文本输出(便于在 go 版本差异下仍可用)
		raw, err2 := exec.Command("go", "list", "-deps", pkg).Output()
		if err2 != nil {
			return nil, err
		}
		return strings.Fields(string(raw)), nil
	}
	var deps []string
	dec := json.NewDecoder(strings.NewReader(string(out)))
	for dec.More() {
		var item struct {
			ImportPath string
		}
		if err := dec.Decode(&item); err != nil {
			break
		}
		if item.ImportPath != "" {
			deps = append(deps, item.ImportPath)
		}
	}
	return deps, nil
}
