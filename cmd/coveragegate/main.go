// Command coveragegate 是**覆盖率门禁**:把 `go test -coverprofile` 的产物
// 按 import path 汇总成"按包 / 总体"的语句覆盖率,再与阈值配置比对,
// 任何一项不达标即非 0 退出。
//
// 为什么需要一个独立工具,而不是在 CI 里 grep `go tool cover -func` 的输出:
//
//  1. 门禁要守的是"**关键包**不低于 X%"。`go tool cover -func` 只给按文件、
//     按函数的百分比,文件一拆分数字就漂移,阈值无法稳定表达;
//  2. 阈值必须能表达"这个包**必须被测到**"。只按已有数据算百分比时,
//     一个从未出现在 profile 里的包会被静默跳过 —— 门禁于是永远为绿,
//     而"没人写测试"恰恰是最需要拦住的情形。本工具把
//     "配置里写了阈值、profile 里没有数据"显式判为失败;
//  3. 退出码必须只有一个可信口径:解析/配置/参数的任何问题都**响亮失败**,
//     绝不"看不懂就当通过"。
//
// 解析与判定全部是纯函数(parseProfile / summarize / evaluate / writeReport),
// 表驱动测试可以直接喂合成 profile,不必先构建二进制再 shell 出去跑一遍。
//
// 用法(在 server/ 目录下):
//
//	go run ./cmd/coveragegate -profile cover.out -config cmd/coveragegate/thresholds.json
//
// 退出码:0 = 全部阈值达标;1 = 有阈值未达标;2 = 输入或配置非法(不是"通过")。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// modulePath 是 server 模块的 import 前缀。
//
// 为什么写死:覆盖率 profile 里**只有文件路径**(编译器写入的路径),
// 没有任何模块信息;要把文件路径还原成 import path,必须知道模块前缀。
// 该值由 server/go.mod 的 module 行决定(github.com/netdisk/netdisk),
// 属于仓库级常量。写死比"运行时再去满世界找 go.mod"更可预测,
// 也让 deriveImportPath 保持纯函数(表驱动测试可直接验证)。
const modulePath = "github.com/netdisk/netdisk"

// 退出码。分开"门禁拦住"与"门禁自己坏了"是为了让 CI 能区分:
// 前者要有人去补测试,后者要有人去修门禁。
const (
	exitOK      = 0
	exitGate    = 1
	exitInvalid = 2
)

// kindOverall 是 violation 的类型标记(总体 / 包)。
const (
	kindOverall = "overall"
	kindPackage = "package"
)

// block 是 profile 中的一条记录:一段连续代码区间的语句数与执行次数。
type block struct {
	file       string // 原始文件路径(可能是 Windows 反斜杠形式)
	statements int    // numStatements,该区间的语句数
	count      int    // count,> 0 即视为该区间已覆盖
}

// profile 是解析后的覆盖率文件。
type profile struct {
	mode   string
	blocks []block
}

// pkgStat 是某个 import path(或总体)的语句统计。
type pkgStat struct {
	ImportPath string
	Total      int
	Covered    int
}

// percent 返回覆盖率(0-100)。
//
// Total == 0 时返回 0 而不是 NaN:NaN 参与任何比较都是 false,
// 会让阈值检查整体静默通过 —— 那正是本工具最不能犯的错。
func (s pkgStat) percent() float64 {
	if s.Total <= 0 {
		return 0
	}
	return float64(s.Covered) / float64(s.Total) * 100
}

// summary 是整份 profile 的汇总结果。
type summary struct {
	Mode     string
	Overall  pkgStat            // ImportPath 为空
	Packages map[string]pkgStat // key = import path
	// Skipped 记录无法反推 import path 的文件。
	//
	// 为什么不直接报错:这类文件不构成"门禁被绕过"(它们既不在阈值里,
	// 也不会让某个包凭空达标),但必须**打印出来** —— 否则"统计口径悄悄少了
	// 几个文件"就会变成一份看起来正常的报告。
	Skipped []string
}

// configFile 是阈值配置(JSON)。
//
// 为什么 critical 必须在 packages 里同时给出数值:critical 表达的是
// "这些包属于关键路径、掉下去必须拦",它本身不带数值。若允许 critical
// 单独存在,配置就退化成"标了关键却没有门槛"的空声明 —— 那不是门禁,是注释。
type configFile struct {
	Overall  float64            `json:"overall"`
	Packages map[string]float64 `json:"packages"`
	Critical []string           `json:"critical"`
}

// violation 是一条"阈值未达标"记录。
type violation struct {
	Kind     string // kindOverall | kindPackage
	Target   string // 包 import path(overall 时为空)
	Actual   float64
	Required float64
	Missing  bool // 该包在 profile 中**完全没有**数据
	Critical bool // 该包被配置标为 critical
}

func (v violation) String() string {
	tag := ""
	if v.Critical {
		tag = "[critical] "
	}
	if v.Kind == kindOverall {
		return fmt.Sprintf("%s总体覆盖率 %.2f%% 低于要求 %.2f%%", tag, v.Actual, v.Required)
	}
	if v.Missing {
		return fmt.Sprintf("%s包 %s 在 profile 中无覆盖率数据(要求 ≥ %.2f%%;从未被测到的包不得静默通过)",
			tag, v.Target, v.Required)
	}
	return fmt.Sprintf("%s包 %s 覆盖率 %.2f%% 低于要求 %.2f%%", tag, v.Target, v.Actual, v.Required)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是 main 的可测试外壳:参数与输出流都从外面传入,返回退出码。
//
// 测试因此可以用 t.TempDir() 里的合成 profile 直接跑完整判定链路,
// 而不必"先 go build 再 shell 出去"——后者会拖慢测试,还会把
// "参数解析失败"和"门禁拦住"两类问题混成同一个非 0 退出码。
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("coveragegate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profilePath := fs.String("profile", "", "go test -coverprofile 生成的覆盖率文件路径(必填)")
	configPath := fs.String("config", "", "阈值配置 JSON 路径(可选)")
	minOverall := fs.Float64("min", 0, "总体覆盖率下限(百分比;0 = 关闭总体检查)")
	top := fs.Int("top", 10, "报告中列出覆盖率最低的包数量(<=0 表示全部列出)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "coveragegate:覆盖率门禁(按包与总体语句覆盖率判定)")
		fmt.Fprintln(stderr, "用法: coveragegate -profile <cover.out> [-config <thresholds.json>] [-min 55] [-top 10]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitInvalid // flag 包已把具体错误写进 stderr
	}
	if strings.TrimSpace(*profilePath) == "" {
		fmt.Fprintln(stderr, "FAIL: 必须用 -profile 指定覆盖率文件(没有 profile 就无法判定任何阈值)")
		fs.Usage()
		return exitInvalid
	}
	if *minOverall < 0 || *minOverall > 100 {
		fmt.Fprintf(stderr, "FAIL: -min %v 不在 [0,100] 区间\n", *minOverall)
		return exitInvalid
	}

	f, err := os.Open(*profilePath)
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: 打开覆盖率文件失败: %v\n", err)
		return exitInvalid
	}
	defer f.Close()

	p, err := parseProfile(f)
	if err != nil {
		fmt.Fprintf(stderr, "FAIL: %v\n", err)
		return exitInvalid
	}
	sum := summarize(p)

	var cfg *configFile
	if strings.TrimSpace(*configPath) != "" {
		cfg, err = loadConfig(*configPath)
		if err != nil {
			fmt.Fprintf(stderr, "FAIL: %v\n", err)
			return exitInvalid
		}
	}

	viol := evaluate(sum, cfg, *minOverall)
	writeReport(stdout, sum, cfg, *minOverall, *top, viol)
	if len(viol) == 0 {
		return exitOK
	}
	// 失败项同时写 stderr:CI 上报告可能很长,把"哪条没过"单独浮出来,
	// 值班的人不必在几百行里找。
	for _, v := range viol {
		fmt.Fprintf(stderr, "FAIL: %s\n", v)
	}
	fmt.Fprintf(stderr, "FAIL: 覆盖率门禁未通过(%d 项阈值未达标)\n", len(viol))
	return exitGate
}

// parseProfile 解析 `go test -coverprofile` 的文件格式。
//
// 格式:首行 `mode: set|counter|atomic`,其后每行
// `file:startLine.startCol,endLine.endCol numStatements count`。
//
// 每个字段都做显式校验并**指名道姓地报错**:profile 通常是机器生成的,
// 一旦格式不对就说明文件被截断、被改写或根本不是 profile ——
// 这种情况下"少读几行然后照常算百分比"会让门禁给出一个看似合理的假结论。
func parseProfile(r io.Reader) (*profile, error) {
	sc := bufio.NewScanner(r)
	// 单行可能很长(长路径 + 大计数)。默认 64KiB 上限在极端路径下会触发
	// bufio.ErrTooLong,那会被误当成"格式错误",白白浪费一次排查。
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("读取覆盖率文件失败: %w", err)
		}
		return nil, errors.New("覆盖率文件为空:缺少 `mode:` 头")
	}
	mode, err := parseModeHeader(strings.TrimSpace(sc.Text()))
	if err != nil {
		return nil, err
	}

	p := &profile{mode: mode}
	for lineNo := 1; sc.Scan(); lineNo++ {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			return nil, fmt.Errorf("覆盖率文件第 %d 行为空行:profile 不允许空行(文件可能被手工改过)", lineNo+1)
		}
		b, err := parseBlockLine(raw)
		if err != nil {
			return nil, fmt.Errorf("覆盖率文件第 %d 行格式错误: %w(原始内容 %q)", lineNo+1, err, raw)
		}
		p.blocks = append(p.blocks, b)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取覆盖率文件失败: %w", err)
	}
	if len(p.blocks) == 0 {
		// 只有 mode 头的 profile 通常意味着 `go test` 中途失败、
		// 或被 `-coverpkg` 配置成了不含任何包。它绝不能算"0 项不达标"。
		return nil, errors.New("覆盖率文件只有 mode 头、不含任何语句统计(profile 可能被截断)")
	}
	return p, nil
}

// parseModeHeader 校验首行的 mode 声明。
func parseModeHeader(line string) (string, error) {
	const prefix = "mode:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("覆盖率文件缺少 `mode:` 头(首行为 %q)", line)
	}
	mode := strings.TrimSpace(line[len(prefix):])
	switch mode {
	case "set", "counter", "atomic":
		return mode, nil
	}
	return "", fmt.Errorf("未知的覆盖率模式 %q(仅支持 set/counter/atomic;mode 行可能被写坏)", mode)
}

// parseBlockLine 解析一条记录:`file:startLine.startCol,endLine.endCol numStatements count`。
func parseBlockLine(line string) (block, error) {
	// 从右往左切最后两段:文件路径理论上可能含空格(仓库路径由人命名),
	// 而 go 保证最后两段是 numStatements 与 count。
	// strings.Fields 全切会把含空格的路径切碎,再报一个指向错误位置的错误。
	countStr, rest, ok := cutLastField(line)
	if !ok {
		return block{}, errors.New("缺少 count 字段(期望 `file:start,end numStatements count`)")
	}
	numStr, posSpec, ok := cutLastField(rest)
	if !ok {
		return block{}, errors.New("缺少 numStatements 字段(期望 `file:start,end numStatements count`)")
	}

	statements, err := strconv.Atoi(numStr)
	if err != nil {
		return block{}, fmt.Errorf("numStatements %q 不是整数", numStr)
	}
	if statements < 0 {
		return block{}, fmt.Errorf("numStatements %d 为负数", statements)
	}
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return block{}, fmt.Errorf("count %q 不是整数", countStr)
	}
	if count < 0 {
		return block{}, fmt.Errorf("count %d 为负数", count)
	}

	// 用**最后**一个冒号切文件与位置:Windows 盘符(D:\...)本身带冒号,
	// 而位置部分是唯一不含冒号的一段。
	colon := strings.LastIndexByte(posSpec, ':')
	if colon <= 0 || colon == len(posSpec)-1 {
		return block{}, fmt.Errorf("缺少 `文件:起止位置` 结构(片段 %q)", posSpec)
	}
	if err := validateRange(posSpec[colon+1:]); err != nil {
		return block{}, err
	}
	return block{file: posSpec[:colon], statements: statements, count: count}, nil
}

// validateRange 校验 `startLine.startCol,endLine.endCol`。
func validateRange(pos string) error {
	comma := strings.IndexByte(pos, ',')
	if comma <= 0 || comma == len(pos)-1 {
		return fmt.Errorf("位置 %q 缺少 `起始,结束` 结构", pos)
	}
	for _, half := range []string{pos[:comma], pos[comma+1:]} {
		dot := strings.IndexByte(half, '.')
		if dot <= 0 || dot == len(half)-1 {
			return fmt.Errorf("位置 %q 不是 `行.列` 结构", half)
		}
		line, err := strconv.Atoi(half[:dot])
		if err != nil || line < 1 {
			return fmt.Errorf("位置 %q 的行号非法", half)
		}
		col, err := strconv.Atoi(half[dot+1:])
		if err != nil || col < 1 {
			return fmt.Errorf("位置 %q 的列号非法", half)
		}
	}
	return nil
}

// cutLastField 按最后一个空格切成 (末段, 其余, 是否成功)。
func cutLastField(s string) (string, string, bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return "", "", false
	}
	return s[i+1:], s[:i], true
}

// deriveImportPath 从 profile 里的文件路径反推 import path。
//
// 为什么要反推:profile 里是**文件路径**(编译器写入,可能是 Windows 反斜杠
// 绝对路径,也可能是模块内相对路径),而阈值配置写的是 import path。
//
// 为什么按 `/internal/`、`/cmd/` 切:本仓库的包全部落在 server 模块的
// internal/ 或 cmd/ 之下,因此"最后一个 /internal/ 或 /cmd/ 之前的部分"
// 就是模块根,之后的部分就是模块内相对包路径。
//
// 为什么取**最后**一个而不是第一个:一旦出现 .../a/internal/x/internal/y 这类
// 嵌套同名目录,只有最后一个才紧邻真正的包路径;取第一个会把包名算错,
// 而算错的包名会以"配置里的包没有数据"的形式误报,指向完全错误的地方。
func deriveImportPath(file string) (string, error) {
	norm := strings.ReplaceAll(strings.TrimSpace(file), `\`, "/")
	if norm == "" {
		return "", errors.New("空文件路径")
	}
	best := -1
	for _, seg := range []string{"/internal/", "/cmd/"} {
		if i := strings.LastIndex(norm, seg); i > best {
			best = i
		}
	}
	if best < 0 {
		return "", fmt.Errorf("路径 %q 中找不到 /internal/ 或 /cmd/ 片段,无法反推 import path", file)
	}
	rel := norm[best+1:] // 形如 internal/namepolicy/namepolicy.go
	slash := strings.LastIndexByte(rel, '/')
	if slash <= 0 {
		return "", fmt.Errorf("路径 %q 中 %q 之后没有包目录", file, rel)
	}
	return modulePath + "/" + rel[:slash], nil
}

// summarize 把解析结果汇总为"总体 + 按包"的语句覆盖。
//
// 口径说明:一个包(或总体)的覆盖率 = 已覆盖语句数 / 语句总数,
// 其中"已覆盖"指该语句所在区间的 count > 0。
//
// 为什么按**语句数**加权而不是"按区间取平均":覆盖率的意义是
// "多少行代码被执行过"。按区间平均会让一个 500 行的巨型函数
// 和一个 2 行的 getter 权重相同 —— 门禁于是可以用一堆琐碎小函数刷绿。
func summarize(p *profile) *summary {
	s := &summary{Mode: p.mode, Packages: map[string]pkgStat{}}
	for _, b := range p.blocks {
		ip, err := deriveImportPath(b.file)
		if err != nil {
			s.Skipped = append(s.Skipped, fmt.Sprintf("%s(%v)", b.file, err))
			continue
		}
		st := s.Packages[ip]
		st.ImportPath = ip
		st.Total += b.statements
		if b.count > 0 {
			st.Covered += b.statements
		}
		s.Packages[ip] = st

		s.Overall.Total += b.statements
		if b.count > 0 {
			s.Overall.Covered += b.statements
		}
	}
	sort.Strings(s.Skipped)
	return s
}

// loadConfig 读取并校验阈值配置。
func loadConfig(path string) (*configFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取阈值配置失败: %w", err)
	}
	cfg := &configFile{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// 拒绝未知字段:把 packages 敲成 package 会让**全部包阈值静默消失**,
	// 门禁随即变成一句空话。拼错必须报错,而不是当没写。
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("阈值配置 %s 不是合法 JSON 对象(或含未知字段): %w", path, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("阈值配置 %s 含多余内容(只允许一个 JSON 对象)", path)
	}
	if err := validateConfig(cfg); err != nil {
		return nil, fmt.Errorf("阈值配置 %s 非法: %w", path, err)
	}
	return cfg, nil
}

// validateConfig 校验阈值配置自身的自洽性(纯函数,便于表驱动测试)。
func validateConfig(cfg *configFile) error {
	if cfg.Overall < 0 || cfg.Overall > 100 {
		return fmt.Errorf("overall 阈值 %v 不在 [0,100] 区间", cfg.Overall)
	}
	for ip, thr := range cfg.Packages {
		if strings.TrimSpace(ip) == "" {
			return errors.New("packages 中存在空 import path")
		}
		if thr < 0 || thr > 100 {
			return fmt.Errorf("包 %s 的阈值 %v 不在 [0,100] 区间", ip, thr)
		}
	}
	seen := map[string]bool{}
	for _, ip := range cfg.Critical {
		if seen[ip] {
			continue
		}
		seen[ip] = true
		if _, ok := cfg.Packages[ip]; !ok {
			return fmt.Errorf("critical 包 %s 未在 packages 中给出阈值(critical 只是"+
				"关键路径标记、不带数值;缺了数值就成了一句空声明)", ip)
		}
	}
	return nil
}

// overallRequired 返回总体覆盖率真正的要求值:取 -min 与配置 overall 中**更严**的一者。
//
// 为什么取更严而不是"配置覆盖命令行":两者语义都是下限。取严可以保证
// CI 上临时加严与仓库基线互相都不会被对方悄悄放宽 ——
// 门禁被意外的宽松值放行,比门禁误报危险得多。
func overallRequired(cfg *configFile, minOverall float64) float64 {
	req := minOverall
	if cfg != nil && cfg.Overall > req {
		req = cfg.Overall
	}
	return req
}

// criticalSet 把配置里的 critical 列表变成集合。
func criticalSet(cfg *configFile) map[string]bool {
	m := map[string]bool{}
	if cfg == nil {
		return m
	}
	for _, ip := range cfg.Critical {
		m[ip] = true
	}
	return m
}

// evaluate 是门禁判定核心(纯函数):返回全部未达标项,返回空切片即通过。
//
// 两类判定的差别值得留意:
//   - 包**在 profile 里但没有达到阈值** → 实际值不足;
//   - 包**根本不在 profile 里** → 判失败,而不是"没数据就跳过"。
//     这是本工具存在的首要理由:阈值写了 85%,而该包被整个删掉/改名之后,
//     门禁必须变红而不是变绿。
func evaluate(sum *summary, cfg *configFile, minOverall float64) []violation {
	var out []violation
	crit := criticalSet(cfg)

	if req := overallRequired(cfg, minOverall); req > 0 {
		if actual := sum.Overall.percent(); actual < req {
			out = append(out, violation{Kind: kindOverall, Actual: actual, Required: req})
		}
	}

	if cfg == nil {
		return out
	}
	// 按 import path 排序遍历:map 顺序是随机的,而报告要能被 diff ——
	// 顺序抖动会让"两次跑出门禁差异"变成一次无意义的人工比对。
	names := make([]string, 0, len(cfg.Packages))
	for ip := range cfg.Packages {
		names = append(names, ip)
	}
	sort.Strings(names)
	for _, ip := range names {
		thr := cfg.Packages[ip]
		st, ok := sum.Packages[ip]
		if !ok {
			out = append(out, violation{
				Kind: kindPackage, Target: ip, Required: thr, Missing: true, Critical: crit[ip],
			})
			continue
		}
		if actual := st.percent(); actual < thr {
			out = append(out, violation{
				Kind: kindPackage, Target: ip, Actual: actual, Required: thr, Critical: crit[ip],
			})
		}
	}
	return out
}

// writeReport 输出中文报告:总体、按包阈值、覆盖率最低的包、未达标清单。
//
// top <= 0 表示列出全部包。列表按 (百分比升序, import path 升序) 排序,
// 同一百分比时顺序稳定,报告可以直接 diff。
func writeReport(w io.Writer, sum *summary, cfg *configFile, minOverall float64, top int, viol []violation) {
	fmt.Fprintln(w, "=== 覆盖率门禁报告 ===")
	fmt.Fprintf(w, "覆盖率模式: %s;包 %d 个;语句 %d 条\n", sum.Mode, len(sum.Packages), sum.Overall.Total)

	if req := overallRequired(cfg, minOverall); req > 0 {
		mark := "✓"
		if sum.Overall.percent() < req {
			mark = "✗"
		}
		fmt.Fprintf(w, "总体覆盖率: %.2f%%(要求 ≥ %.2f%%)%s\n", sum.Overall.percent(), req, mark)
	} else {
		// 明说"未设下限":否则读者会以为 40% 的总体覆盖率也是"通过"。
		fmt.Fprintf(w, "总体覆盖率: %.2f%%(未设总体下限,仅检查按包阈值)\n", sum.Overall.percent())
	}

	if len(sum.Skipped) > 0 {
		fmt.Fprintf(w, "警告: 有 %d 条记录无法反推 import path,未纳入统计(统计口径小于 profile 全体):\n", len(sum.Skipped))
		for _, s := range sum.Skipped {
			fmt.Fprintf(w, "  - %s\n", s)
		}
	}

	crit := criticalSet(cfg)
	if cfg != nil && len(cfg.Packages) > 0 {
		fmt.Fprintf(w, "\n按包阈值(%d 个):\n", len(cfg.Packages))
		for _, ip := range sortedPkgNames(cfg) {
			thr := cfg.Packages[ip]
			tag := ""
			if crit[ip] {
				tag = " [critical]"
			}
			st, ok := sum.Packages[ip]
			switch {
			case !ok:
				fmt.Fprintf(w, "  ✗ %s 无覆盖率数据(profile 中未出现,要求 ≥ %.2f%%)%s\n", ip, thr, tag)
			case st.percent() < thr:
				fmt.Fprintf(w, "  ✗ %s %.2f%% < 要求 ≥ %.2f%%%s\n", ip, st.percent(), thr, tag)
			default:
				fmt.Fprintf(w, "  ✓ %s %.2f%%(要求 ≥ %.2f%%)%s\n", ip, st.percent(), thr, tag)
			}
		}
	}

	stats := make([]pkgStat, 0, len(sum.Packages))
	for _, st := range sum.Packages {
		stats = append(stats, st)
	}
	sort.Slice(stats, func(i, j int) bool {
		pi, pj := stats[i].percent(), stats[j].percent()
		if pi != pj {
			return pi < pj
		}
		return stats[i].ImportPath < stats[j].ImportPath
	})
	limit := top
	if limit <= 0 || limit > len(stats) {
		limit = len(stats)
	}
	fmt.Fprintf(w, "\n覆盖率最低的 %d 个包(共 %d 个):\n", limit, len(stats))
	for i := 0; i < limit; i++ {
		s := stats[i]
		fmt.Fprintf(w, "  %2d. %6.2f%%  %s(%d/%d)\n", i+1, s.percent(), s.ImportPath, s.Covered, s.Total)
	}

	if len(viol) == 0 {
		fmt.Fprintln(w, "\n✓ 门禁通过:全部阈值达标。")
		return
	}
	fmt.Fprintf(w, "\n✗ 门禁未通过(%d 项阈值未达标):\n", len(viol))
	for _, v := range viol {
		fmt.Fprintf(w, "  - %s\n", v)
	}
}

// sortedPkgNames 返回排序后的配置包名(与 evaluate 的遍历顺序保持一致)。
func sortedPkgNames(cfg *configFile) []string {
	names := make([]string, 0, len(cfg.Packages))
	for ip := range cfg.Packages {
		names = append(names, ip)
	}
	sort.Strings(names)
	return names
}
