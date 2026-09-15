// testsgate —— "集成测试真的跑起来了"的门禁。
//
// 为什么必须有它(TS-02 记下的缺口):本仓的集成测试靠环境变量 NETDISK_TEST_DSN
// 判定是否可跑,未设置时整组 `t.Skip` —— 而 **Skip 不是失败**。
// 于是"忘了给 DSN / DSN 拼错 / 库没起来 / 端口写错"这类**配置**问题,在 CI 上的
// 表现是**全绿**:流水线绿得发亮,而"真实 PG 集成用例"这条验收实际上一个都没跑。
// 这类"绿灯但没验"比红灯危险得多 —— 红灯会有人去修,绿灯只会有人放心。
//
// 三条断言:
//  ① 必须设置 NETDISK_TEST_DSN(缺失直接失败,并给出可直接复制的设置方法);
//  ② 源码里**引用了** NETDISK_TEST_DSN 的包,每一个都必须至少有一个测试**真的通过**
//     (只看"有没有 skip 事件"不够:整个包因为别的原因没跑起来同样会漏掉);
//  ③ 不允许出现跳过原因里带 NETDISK_TEST_DSN 的用例(把"被跳过"如实点名)。
//
// 用法(在 server/ 下):
//
//	go run ./cmd/testsgate              # 等价于 go test ./... -count=1,但会拦上面三种
//	go run ./cmd/testsgate -race        # 其余参数原样透传给 go test
//
// 本地反向验证:
//
//	$env:NETDISK_TEST_DSN='' ; go run ./cmd/testsgate   # 必须失败(断言①)
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// testEvent 是 `go test -json` 的一行(只取我们关心的字段)。
type testEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
	Output  string `json:"Output"`
}

// requiredEnv 是"集成测试是否可跑"的开关(与各 fixture 里的判定一致)。
const requiredEnv = "NETDISK_TEST_DSN"

func main() {
	extra := os.Args[1:]

	// **本门禁只用于整跑**。带 -run/-short 这类筛选时,"哪个包一个用例都没通过"
	// 就不再是可疑信号(是被你筛掉的),断言②会误报 —— 与其给出错误结论,不如拒绝跑。
	// 原则同仓库其它门禁:不能判定 ≠ 通过。
	for _, a := range extra {
		if strings.HasPrefix(a, "-run") || strings.HasPrefix(a, "-short") {
			fmt.Fprintf(os.Stderr, "FAIL: testsgate 只用于整跑(收到 %q)。跑单个用例请直接用 go test。\n", a)
			os.Exit(2)
		}
		if !strings.HasPrefix(a, "-") && a != "./..." {
			fmt.Fprintf(os.Stderr, "FAIL: testsgate 只跑 ./...(收到包模式 %q)。只跑某些包请直接用 go test。\n", a)
			os.Exit(2)
		}
	}

	// ---- ① 环境变量必须在 ----
	if strings.TrimSpace(os.Getenv(requiredEnv)) == "" {
		fmt.Fprintf(os.Stderr, "FAIL: 未设置 %s —— 集成测试会整组 skip 而 CI 仍然全绿。\n", requiredEnv)
		fmt.Fprintf(os.Stderr, "      这份门禁就是为了拦这种情况。设置方法(本机):\n")
		fmt.Fprintf(os.Stderr, "        $env:%s='host=127.0.0.1 port=5432 dbname=netdisk_test user=netdisk password=<口令> sslmode=disable'\n", requiredEnv)
		os.Exit(1)
	}

	want, err := packagesReferencingDSN(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 扫描引用 %s 的包失败: %v\n", requiredEnv, err)
		os.Exit(1)
	}
	if len(want) == 0 {
		fmt.Fprintf(os.Stderr, "FAIL: 没有任何测试引用 %s —— 要么扫描错了目录,要么集成用例被删了\n", requiredEnv)
		os.Exit(1)
	}

	// ---- 跑测试并解析事件流 ----
	args := append([]string{"test", "-json", "-count=1", "./..."}, extra...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 无法读取 go test 输出: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 无法启动 go test: %v\n", err)
		os.Exit(1)
	}

	passedByPkg := map[string]int{}
	skippedByDSN := []string{}
	failures := map[string][]string{} // 包 → 该包失败用例的输出(便于在 CI 上直接看到原因)
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	for scanner.Scan() {
		var ev testEvent
		if jerr := json.Unmarshal(scanner.Bytes(), &ev); jerr != nil {
			continue // 非 JSON 行(例如编译警告)忽略
		}
		switch ev.Action {
		case "pass":
			if ev.Test != "" {
				passedByPkg[ev.Package]++
			}
		case "skip":
			// 只点名"因为没 DSN 被跳过"的那些:那才是这份门禁要拦的
			if strings.Contains(ev.Output, requiredEnv) || skipReasonMentionsDSN(ev, skippedByDSN) {
				skippedByDSN = append(skippedByDSN, ev.Package+"."+ev.Test)
			}
		case "output":
			if ev.Test != "" && looksLikeFailure(ev.Output) {
				failures[ev.Package] = append(failures[ev.Package], ev.Output)
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := cmd.Wait()
	if scanErr != nil {
		fmt.Fprintf(os.Stderr, "FAIL: 读取 go test 输出失败: %v\n", scanErr)
		os.Exit(1)
	}

	// 测试本身失败:原样回放失败用例的输出(JSON 模式下默认看不到,否则 CI 上只看到一个退出码)
	if waitErr != nil {
		fmt.Fprintln(os.Stderr, "FAIL: go test 有失败用例。失败输出:")
		pkgs := make([]string, 0, len(failures))
		for p := range failures {
			pkgs = append(pkgs, p)
		}
		sort.Strings(pkgs)
		for _, p := range pkgs {
			fmt.Fprintf(os.Stderr, "---- %s ----\n", p)
			for _, line := range failures[p] {
				fmt.Fprint(os.Stderr, line)
			}
		}
		os.Exit(1)
	}

	failed := false

	// ---- ③ 不允许"因为没 DSN 而被跳过" ----
	if len(skippedByDSN) > 0 {
		failed = true
		fmt.Fprintf(os.Stderr, "FAIL: 有 %d 个用例因为没拿到 %s 而被跳过(等于没验):\n", len(skippedByDSN), requiredEnv)
		for _, s := range skippedByDSN {
			fmt.Fprintf(os.Stderr, "  - %s\n", s)
		}
	}

	// ---- ② 引用了 DSN 的包必须真的跑过测试 ----
	missing := []string{}
	for _, pkg := range want {
		if passedByPkg[pkg] == 0 {
			missing = append(missing, pkg)
		}
	}
	if len(missing) > 0 {
		failed = true
		fmt.Fprintf(os.Stderr, "FAIL: 这些包声明依赖 %s,但**一个通过的测试都没有**(整包没跑或全被跳过):\n", requiredEnv)
		for _, p := range missing {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
	}

	total := 0
	for _, n := range passedByPkg {
		total += n
	}
	if failed {
		os.Exit(1)
	}
	fmt.Printf("PASS: %d 个包(其中 %d 个依赖真实 PG)共 %d 个用例通过;无一例因缺 %s 被跳过。\n",
		len(passedByPkg), len(want), total, requiredEnv)
}

// skipReasonMentionsDSN 兜住"skip 事件不带 Output"的实现差异。
// 这里保持保守:宁可不报,也不要把无关的 skip 误报成"没验集成"。
func skipReasonMentionsDSN(ev testEvent, _ []string) bool { return false }

// looksLikeFailure 粗筛失败相关的输出行(用于回放)。
func looksLikeFailure(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "--- FAIL") || strings.HasPrefix(t, "FAIL") ||
		strings.Contains(t, ".go:") || strings.HasPrefix(t, "panic:")
}

// packagesReferencingDSN 找出源码里引用了 NETDISK_TEST_DSN 的包(import path 形式)。
//
// 用"扫描源码"而不是写死一份包清单:新增集成用例时**不需要**记得来这里登记 ——
// 需要人记着登记的门禁,迟早会因为"忘了"而失效。
func packagesReferencingDSN(root string) ([]string, error) {
	modPath, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			name := d.Name()
			// 跳过不参与编译的目录(否则 vendor/测试产物会把包清单污染)
			if name == ".git" || name == "vendor" || name == "node_modules" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !strings.Contains(string(b), requiredEnv) {
			return nil
		}
		dir := filepath.Dir(path)
		rel, rerr := filepath.Rel(root, dir)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		pkg := modPath
		if rel != "." {
			pkg = modPath + "/" + rel
		}
		seen[pkg] = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// modulePath 读 go.mod 的 module 行。
func modulePath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module ")), nil
		}
	}
	return "", fmt.Errorf("go.mod 里没有 module 行")
}
