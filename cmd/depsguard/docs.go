package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 本文件是**文档与运维制品的一致性门禁**(V2.77 新增,服务于用户要求③"检查所有文档资料")。
//
// 为什么必须是机械检查:文档里的路径、指标名、配置键**全是"对外承诺"**,而写错它们的
// 失败方式全都是静默的 ——
//
//	① 文档指向一个不存在的文件      → 读者以为那份材料存在(本次审计一上来就抓到一条)
//	② 告警表达式引用不存在的指标    → 规则**永远不触发**,而面板上一切正常(最危险的失效)
//	③ 示例配置里的键在 Go 里不存在  → yaml 静默忽略,运维"配上了"却没生效
//	④ 指标存在但文档里一个字都没有  → 出事时没人知道该看哪条曲线
//
// "读一遍文档"对 ④ 尤其无效:没有人能靠阅读记住 40 个指标名与 60 个配置键的差集。
//
// 判据的**作用域规则**(刻意的,不是偷懒):
//   - 路径引用、指标名、配置键的检查对全部文档生效;
//   - "反向指标(指标是否被文档提到)"只在**活的规格文档**上要求:
//     ops/、架构设计文档、交付物清单、红线核对表、存储配置指南。
//     `变更历史`/`*-审报告`/`评审与优化建议`/`任务清单的修订记录`是**历史档案**——
//     里面出现当年被改名或删掉的指标名是记录,不是缺陷。把它们算进来只会制造噪声,
//     而噪声会让人把门禁关掉。

var (
	// 文档里指向仓库内文件的引用:`Doc/...` 或 `docs/...`
	docRefRe = regexp.MustCompile(`(?:Doc|docs)/[A-Za-z0-9_./\x{4e00}-\x{9fff}-]+\.(?:md|yaml|yml|sql|sh|ps1|json)`)
	// 指标名(只看小写+下划线;末尾下划线是文档里的省略写法,会被丢弃)
	metricRe = regexp.MustCompile(`\bnetdisk_[a-z0-9_]+`)
	// Go 结构体里的 yaml 标签
	yamlTagRe = regexp.MustCompile("yaml:\"([^\"]+)\"")
	// 示例配置里的键(行首缩进后的 `key:`,跳过注释行)
	yamlKeyRe = regexp.MustCompile(`(?m)^\s*([a-z][a-z0-9_]*):`)
	// 文档里的**代码文件**引用(顶层目录 + 已知扩展名;可带行号)
	codePathRe = regexp.MustCompile(`(?:server/)?(?:internal|cmd|desktop|deploy|web|scripts)/[A-Za-z0-9_./-]+\.(?:go|cs|csproj|wxs|wixproj|sql|sh|ps1|mjs|ts|vue|json|ya?ml|cmd|sln|md)(?::[0-9]+(?:-[0-9]+)?)?`)
	// 文档点名的 Go 测试函数
	goTestNameRe = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]{4,}\b`)
)

// 反向指标检查的"活文档"清单(相对 docs/ 的路径)
var liveSpecDocs = []string{
	"网盘系统架构设计文档.md",
	"ops/01-安装与部署.md",
	"ops/02-备份与恢复.md",
	"ops/03-故障处置.md",
	"ops/04-演练记录.md",
	"ops/05-监控指标与告警.md",
	"ops/存储配置指南.md",
	"交付物清单.md",
	"网盘系统-红线核对表.md",
	"dev/开发指南.md",
}

// reverseMetricExempt:指标**故意**不要求在文档里出现(每条都必须写明理由)。
var reverseMetricExempt = map[string]string{
	// 自监控:采集器自身的错误计数,排障时才需要,不进运维面板
	"netdisk_metrics_collector_errors_total": "采集器自监控,不面向运维面板",
}

// metricExempt:文档/告警里允许出现但**不是**本服务注册的指标(外部提供方)。
var metricExempt = map[string]string{
	"up": "Prometheus 自身的抓取健康指标,不是 netdisk 注册的",
	// 测试库名(DSN 里的 dbname=netdisk_test)长得像指标,但它是数据库名
	"netdisk_test": "测试库名(DSN),不是指标",
}

// yamlKeyExempt:结构体里有、但**故意不写进示例配置**的键(每条写明理由)。
var yamlKeyExempt = map[string]string{}

func checkDocsAndOps(root string) ([]string, error) {
	var problems []string

	docsDir := filepath.Join(root, "DOC")
	mdFiles, err := collectMarkdown(docsDir, root)
	if err != nil {
		return nil, err
	}
	if len(mdFiles) == 0 {
		return []string{"文档一致性无法校验:DOC/ 下没有找到任何 .md(路径或 junction 是否正常?)"}, nil
	}

	// ---------- ① 文档互引路径必须存在 ----------
	// 允许 `Doc/` 与 `docs/` 两种写法(仓库里 docs 是指向 Doc 的 junction)
	for _, f := range mdFiles {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		text := string(raw)
		for _, ref := range uniqueMatches(docRefRe, text) {
			if strings.ContainsAny(ref, "*{}") {
				continue // 通配写法(如 `-*-审报告.md`)不构成单文件承诺
			}
			rel := ref
			if strings.HasPrefix(rel, "Doc/") {
				rel = "docs/" + strings.TrimPrefix(rel, "Doc/")
			}
			if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); serr != nil {
				problems = append(problems, fmt.Sprintf(
					"文档引用了一个**不存在**的文件:%s 里的 %q(读者会以为它存在)", rel1(root, f), ref))
			}
		}
	}

	// ---------- ②(已移除)metrics 指标名一致性 ----------
	// /metrics 端点已随开源版 slimming 整体拆除,"文档里的指标名必须存在"这类校验
	// 已失去比较基准(internal/metrics 不存在),故本段删除;若将来恢复 metrics 再补回。

	// ---------- ⑥ 文档点名的**代码文件**必须存在 ----------
	//
	// 为什么单列一类:文档里最有用的一句话常常是"证据在 `internal/migrate/migrate_sql_test.go`"
	// 或"实现见 `server/internal/storage/storage.go`"。文件一旦改名/搬家,这类引用会**静默失效** ——
	// 读者照着找,找不到,然后开始怀疑整份文档(而不是怀疑那一行)。
	// 覆盖范围刻意收到"带已知扩展名 + 带已知顶层目录"的写法:只写包名(`internal/lifecycle`)不算文件承诺。
	for _, f := range mdFiles {
		if isHistoricalDoc(root, f) {
			continue // 历史档案里指向"当时那个文件"是记录
		}
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		for _, ref := range uniqueMatches(codePathRe, string(raw)) {
			if strings.ContainsAny(ref, "*{}") {
				continue
			}
			// 去掉行号/列号后缀(`foo_test.go:61`、`bar.go:12-30`)
			clean := codePathRe.FindString(ref)
			clean = strings.TrimSuffix(clean, ":")
			if i := strings.LastIndex(clean, ":"); i > 0 && isAllDigits(clean[i+1:]) {
				clean = clean[:i]
			}
			if pathExistsInRepo(root, clean) {
				continue
			}
			if isNonExistenceMention(string(raw), clean) {
				continue // 文档在**说明某个东西不存在/未采用**(那是信息,不是承诺)
			}
			problems = append(problems, fmt.Sprintf(
				"文档引用了一个**不存在**的代码文件:%s 里的 %q(改名/搬家后没人回头改文档)",
				rel1(root, f), clean))
		}
	}

	// ---------- ⑦ 文档点名的 Go 测试函数必须存在 ----------
	//
	// 与 ⑥ 同一动机,但抓的是更细一层:验收证据常常直接点名用例
	// (`TestUploadsAllowOverwriteColumnApplied`、`TestKeyForContentAddressed`)。
	// 用例被改名或删掉时,文档会继续宣称"这条有机械断言" —— 而**没有任何自动化在断言这件事**,
	// 于是"有证据"变成一句无法核实的旧话。这一条把名字变成可核实的引用。
	testFuncs, terr2 := collectGoTestFuncs(root)
	if terr2 != nil {
		return nil, terr2
	}
	for _, f := range mdFiles {
		if isHistoricalDoc(root, f) {
			continue
		}
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		for _, name := range uniqueMatches(goTestNameRe, string(raw)) {
			if testFuncs[name] {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"文档点名的测试用例 %q 在仓库里**找不到**(%s):用例改名/删除后文档仍在宣称有这条断言",
				name, rel1(root, f)))
		}
	}

	// ---------- ③(已移除)Prometheus 告警规则指标一致性 ----------
	// 与 ② 同理:/metrics 已整体拆除,deploy/prometheus 告警规则不再有比较基准。

	// ---------- ⑤ 示例配置的键必须真实存在(反向:每个键都要有文档或显式豁免)----------
	cfgPath := filepath.Join(root, "deploy", "config", "config.example.yaml")
	cfgRaw, cerr := os.ReadFile(cfgPath)
	if cerr != nil {
		problems = append(problems, "读不到 deploy/config/config.example.yaml(配置文档无法校验)")
	} else {
		exampleKeys := map[string]bool{}
		for _, m := range yamlKeyRe.FindAllStringSubmatch(string(cfgRaw), -1) {
			exampleKeys[m[1]] = true
		}
		tags, terr := configYAMLTags(root)
		if terr != nil {
			return nil, terr
		}
		if len(tags) == 0 {
			return []string{"配置一致性无法校验:没有从 internal/config 解析出 yaml 标签"}, nil
		}
		for _, k := range sortedKeys(exampleKeys) {
			if _, ok := tags[k]; !ok {
				problems = append(problems, fmt.Sprintf(
					"示例配置里的键 %q 在 Go 结构体里**不存在**:yaml 会静默忽略它,运维以为配上了", k))
			}
		}
		for _, k := range sortedKeys(tags) {
			if exampleKeys[k] {
				continue
			}
			if _, ok := yamlKeyExempt[k]; ok {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"配置项 %q 在示例配置里**没有出现**,也没有豁免理由:运维只能去读 Go 源码才知道有这个旋钮", k))
		}
	}

	return problems, nil
}

// ---- 辅助 ----

func rel1(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return filepath.ToSlash(r)
	}
	return p
}

// collectMarkdown 收集 docs/ 下的全部 .md,外加仓库根 README.md 与 deploy/README.md。
//
// **必须自己递归**:docs 是指向 Doc 的 junction(Windows reparse point),而
// filepath.WalkDir **不跟随**链接 —— 用它会在 Windows 上把整棵 docs 树当成空目录,
// 门禁于是"通过"而其实一个文件都没看(这正是本仓记录过的"假绿"模式)。
func collectMarkdown(docsDir, root string) ([]string, error) {
	var out []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if e.IsDir() {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			// junction/符号链接目录:ReadDir 的 DirEntry 不会标 IsDir,需 stat 兜一层
			if e.Type()&fs.ModeSymlink != 0 {
				if st, serr := os.Stat(p); serr == nil && st.IsDir() {
					if err := walk(p); err != nil {
						return err
					}
					continue
				}
			}
			if strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
				out = append(out, p)
			}
		}
		return nil
	}
	if err := walk(docsDir); err != nil {
		return nil, fmt.Errorf("遍历 docs/ 失败: %w", err)
	}
	for _, extra := range []string{
		filepath.Join(root, "README.md"),
		filepath.Join(root, "deploy", "README.md"),
	} {
		if _, err := os.Stat(extra); err == nil {
			out = append(out, extra)
		}
	}
	sort.Strings(out)
	return out, nil
}

// declaredMetrics 收集 internal/metrics 里注册的全部 netdisk_* 指标名。
func declaredMetrics(root string) (map[string]bool, error) {
	dir := filepath.Join(root, "server", "internal", "metrics")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读 internal/metrics 失败: %w", err)
	}
	decl := map[string]bool{}
	// 只认"完整指标名"字面量:整行/整串就是 netdisk_xxx(排除 HELP 文案里的句子)
	full := regexp.MustCompile(`"((?:netdisk_)[a-z0-9_]+)"`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		for _, m := range full.FindAllStringSubmatch(string(raw), -1) {
			decl[m[1]] = true
		}
	}
	return decl, nil
}

// configYAMLTags 收集 internal/config 全部结构体的 yaml 标签(排除 `-` 与嵌套路径)。
func configYAMLTags(root string) (map[string]bool, error) {
	dir := filepath.Join(root, "internal", "config")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读 internal/config 失败: %w", err)
	}
	tags := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		for _, m := range yamlTagRe.FindAllStringSubmatch(string(raw), -1) {
			tag := m[1]
			if tag == "-" || tag == "" {
				continue
			}
			if i := strings.IndexByte(tag, ','); i >= 0 {
				tag = tag[:i] // `yaml:"a,omitempty"` → a
			}
			tags[tag] = true
		}
	}
	return tags, nil
}

func uniqueMatches(re *regexp.Regexp, text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range re.FindAllString(text, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// isElidedMetric 判断是否是文档里的"省略写法"(如 `netdisk_stage_cleanup_..._total`
// 会被正则截成 `netdisk_stage_cleanup_`)。
func isElidedMetric(m string) bool {
	return strings.HasSuffix(m, "_") || strings.HasSuffix(m, "netdisk")
}

// isGrepPrefixElision 判断某个"不存在的指标名"其实是**某个真实指标名的前缀**,
// 且出现在 `grep`/`grep -E` 这类上下文里 —— 例如交付物清单里的
// `curl -s .../metrics | grep netdisk_backup`(运维照抄的过滤命令)。
//
// 为什么要专门判这一条:这种写法**必须**允许(它就是给人抄的),
// 但如果不加区分地"允许一切前缀",一个真正的错别字(`netdisk_object_leak`)
// 也会被放过。所以判据收紧到"同一行里出现 grep 且该串是真实指标的前缀"。
func isGrepPrefixElision(docText, tok string, declared map[string]bool) bool {
	isPrefix := false
	for d := range declared {
		if strings.HasPrefix(d, tok) {
			isPrefix = true
			break
		}
	}
	if !isPrefix {
		return false
	}
	for _, line := range strings.Split(docText, "\n") {
		if !strings.Contains(line, tok) {
			continue
		}
		if strings.Contains(line, "grep") {
			return true
		}
	}
	return false
}

func isHistoricalDoc(root, f string) bool {
	rel := rel1(root, f)
	for _, marker := range []string{
		"变更历史", "二审报告", "三审报告", "四审报告", "评审与优化建议",
		"网盘系统开发任务清单.md", "工作量与费用估算.md", "H5-实机回归清单.md",
		"客户端验收清单.md", "性能基线.md", "集成测试对照表.md",
	} {
		if strings.Contains(rel, marker) {
			return true
		}
	}
	return false
}

func isLiveSpecDoc(root, f string) bool {
	rel := strings.TrimPrefix(rel1(root, f), "docs/")
	for _, want := range liveSpecDocs {
		if rel == want {
			return true
		}
	}
	return false
}

// isNonExistenceMention 判断某个"不存在的代码文件"其实是文档**在明说它不存在/未采用**。
//
// 为什么需要这条:文档有正当理由提到不存在的东西 —— WiX 指南要记录"方案 C(脚本生成组件清单)
// **未采用**,实际做法是单文件载荷",任务清单要写"该文件已删除/改名"。这类引用是**信息**,
// 不是承诺;按承诺去查会得到一条假红,而假红会让人把整条规则关掉。
//
// 判据收得很紧:必须与"未采用/不存在/未创建/已删除/改名/~~"出现在**同一行**。
func isNonExistenceMention(docText, ref string) bool {
	for _, line := range strings.Split(docText, "\n") {
		if !strings.Contains(line, ref) {
			continue
		}
		for _, marker := range []string{"未采用", "不存在", "未创建", "已删除", "已改名", "~~"} {
			if strings.Contains(line, marker) {
				return true
			}
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// pathExistsInRepo 判断文档里的代码路径是否存在。
//
// 两种写法都要支持:仓库根相对(`server/internal/...`)与服务端目录相对(`internal/...`),
// 因为文档两种都写(前者出现在运维/清单里,后者出现在开发指南里)。
func pathExistsInRepo(root, p string) bool {
	candidates := []string{
		filepath.Join(root, filepath.FromSlash(p)),
		filepath.Join(root, "server", filepath.FromSlash(p)),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return true
		}
	}
	return false
}

// collectGoTestFuncs 收集仓库里**全部** `func TestXxx(` 的名字(含桌面端无关,只扫 Go)。
func collectGoTestFuncs(root string) (map[string]bool, error) {
	out := map[string]bool{}
	// 四仓拆分后 Go 源码直接在仓库根 internal/ 与 cmd/(不再是原 monorepo 的 server/ 子目录)
	roots := []string{filepath.Join(root, "internal"), filepath.Join(root, "cmd")}
	for _, base := range roots {
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return nil // 单个目录读不动不该让门禁失效(下面还有"一个都没扫到"的兜底)
			}
			if d.IsDir() {
				// 跳过缓存/依赖目录:它们里面可能有几万个 .go,而且不是我们的代码
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), "_test.go") && !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			raw, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			for _, m := range regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`).FindAllStringSubmatch(string(raw), -1) {
				out[m[1]] = true
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有扫到任何 Go 测试函数(路径或遍历逻辑有问题,不能判成通过)")
	}
	return out, nil
}
