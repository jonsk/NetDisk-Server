package main

// 第 1 卷红线的**机械可检**部分(R-24 / R-25 / R-26)。
//
// 26 条红线里只有一部分能用"读文件"判定;这里只挑**能机械判定且一破就静默出问题**
// 的三条,其余(时序/并发/权限类)靠用例与反向验证,硬塞进静态检查只会制造假信心。
//
// 为什么这三条值得静态盯:
//   - R-24(不做二期、代码里不得出现 Rust):一旦有人"顺手"加个 Cargo.toml,
//     构建链就多了一套工具链与一条只有它才走得通的路径;而它**不影响 Go 构建**,
//     评审时极易被当成无害新增。
//   - R-25(WiX 固定 6.0.2):v7 有 OSMF 授权门槛(E-04 实测)。版本一旦漂移,
//     现场构建会突然要授权,而本地"编得过"让人以为没问题。
//   - R-26(MSI 必须 perUser + MajorUpgrade):这两项是"企业里能不能装、能不能升级"
//     的前提;漏掉 MajorUpgrade 的表现是"升级时装出两个版本共存",只有在真机上升级
//     一次才会暴露。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// checkRedlines 返回违规描述(空 = 通过)。
func checkRedlines(repoRoot string) ([]string, error) {
	var problems []string
	problems = append(problems, checkR24Rust(repoRoot)...)
	problems = append(problems, checkR25R26Wix(repoRoot)...)
	problems = append(problems, checkR01R02WebUI(repoRoot)...)
	problems = append(problems, checkR14JWT(repoRoot)...)
	problems = append(problems, checkR20WebDAV(repoRoot)...)
	problems = append(problems, checkR21ETag(repoRoot)...)
	problems = append(problems, checkR15Secrets(repoRoot)...)
	return problems, nil
}

// readIfExists 读文件;不存在返回空串(调用方据此判"缺文件"这种更严重的违规)。
func readIfExists(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// stripComments 去掉 `//` 行注释。
//
// **反向验证发现的坑**:这些检查原本直接扫整份源码,于是**文档注释里的关键字**
// 就能把它骗过 —— 例如 jwt.go 顶部写着"解析必须 WithValidMethods([...])",
// 把真正的调用删掉、检查照样通过。注释是给人看的,门禁必须只认代码。
func stripComments(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if idx := strings.Index(l, "//"); idx >= 0 {
			lines[i] = l[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// ---- R-01 / R-02:前端产物必须 go:embed 进二进制,且缓存头分两档 ----
//
// 这两条一破就是**线上表现**问题:静态文件改由 Nginx 托管会让"单二进制部署"失效
// (运维要同时同步两份产物,而版本不一致时用户看到旧界面);缓存头写错则表现为
// "发版后用户拿不到新前端"(index.html 被缓存)或"每次刷新都重下 1MB"(资源不缓存)。
func checkR01R02WebUI(repoRoot string) []string {
	var problems []string
	webui := filepath.Join(repoRoot, "internal", "webui", "webui.go")
	raw := readIfExists(webui)
	if raw == "" {
		return []string{"R-01 无法校验:找不到 server/internal/webui/webui.go"}
	}
	// R-01 必须看**原始文本**:`//go:embed` 本身就是一条指令注释,去注释会把它删掉
	if !strings.Contains(raw, "//go:embed") {
		problems = append(problems,
			"R-01 被破坏:webui 未使用 go:embed(前端产物必须编进 netdisk 二进制,Nginx 不再托管静态文件)")
	}
	// R-02 只看代码:注释里写着"index.html 必须 no-cache"不算实现(见 stripComments)
	code := stripComments(raw)
	if !strings.Contains(code, "no-cache") {
		problems = append(problems,
			"R-02 被破坏:未见 no-cache(index.html 被缓存会让发版后用户一直看到旧界面)")
	}
	if !strings.Contains(code, "max-age=31536000") || !strings.Contains(code, "immutable") {
		problems = append(problems,
			"R-02 被破坏:带内容哈希的 assets 未见 max-age=31536000, immutable(否则每次刷新都重下)")
	}
	return problems
}

// ---- R-14:JWT 解析必须钉算法白名单 ----
//
// 不钉 `WithValidMethods` 就是算法混淆攻击(把 alg 改成 none/RS256 让验签失效)。
// 这是"漏一行就全线失守"的类型,而且**功能测试全绿** —— 因为它只在攻击下表现不同。
func checkR14JWT(repoRoot string) []string {
	var problems []string
	jwtGo := filepath.Join(repoRoot, "internal", "auth", "jwt.go")
	// 去注释后再查:文件顶部就写着"解析必须 WithValidMethods([...])",
	// 直接扫全文会让**文档把门禁骗过**(反向验证实测踩到)
	code := stripComments(readIfExists(jwtGo))
	if code == "" {
		return []string{"R-14 无法校验:找不到 server/internal/auth/jwt.go"}
	}
	if !strings.Contains(code, "jwt.WithValidMethods(") {
		problems = append(problems, "R-14 被破坏:JWT 解析缺少 WithValidMethods(算法混淆攻击的闸门)")
	}
	if !strings.Contains(code, `"HS256"`) {
		problems = append(problems, "R-14 被破坏:JWT 解析未限定 HS256")
	}
	if !strings.Contains(code, "jwt.WithLeeway(") {
		problems = append(problems, "R-14 被破坏:JWT 解析缺少 leeway(时钟偏差会让刚签发的令牌被判过期)")
	}
	return problems
}

// ---- R-20:WebDAV 不提供 LOCK,且 FileInfo 必须实现 ETager ----
//
// 漏实现 ETager 是**静默**的:上游会用 ModTime 伪造 ETag,于是乐观锁形同虚设
// (两个客户端各自 If-Match 都会通过)。实现 LOCK 则等于把"REST 锁"这套
// 单一锁语义拆成两套,锁状态会互相看不见。
func checkR20WebDAV(repoRoot string) []string {
	var problems []string
	dir := filepath.Join(repoRoot, "internal", "webdavfs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{"R-20 无法校验:找不到 server/internal/webdavfs"}
	}
	etagImpl := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		text := stripComments(readIfExists(filepath.Join(dir, e.Name())))
		if strings.Contains(text, `case "LOCK"`) || strings.Contains(text, `"LOCK":`) {
			problems = append(problems,
				"R-20 被破坏:"+e.Name()+" 实现了 LOCK(WebDAV 锁由 REST 锁替代,两套锁会互相看不见)")
		}
		if strings.Contains(text, ") ETag(ctx context.Context)") || strings.Contains(text, ") ETag(context.Context)") {
			etagImpl = true
		}
	}
	if !etagImpl {
		problems = append(problems,
			"R-20 被破坏:webdavfs 里没有任何类型实现 ETager(缺了它上游用 ModTime 伪造 ETag,乐观锁静默失效)")
	}
	return problems
}

// ---- R-21:etag 必须是 STORED 生成列且**不含引号**,引号只有一个出口 ----
//
// 生成列表达式里带引号 → 同一份数据在不同出口字节不同(客户端比对永远不相等);
// 引号逻辑散在多处 → 迟早有一处忘了加,而那一处的表现是"条件请求永远 412"。
func checkR21ETag(repoRoot string) []string {
	var problems []string
	sqlDir := filepath.Join(repoRoot, "internal", "migrate", "sql")
	entries, err := os.ReadDir(sqlDir)
	if err != nil {
		return []string{"R-21 无法校验:找不到迁移 SQL 目录"}
	}
	sawEtag := false
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		text := readIfExists(filepath.Join(sqlDir, e.Name()))
		idx := strings.Index(text, "etag         text GENERATED ALWAYS AS")
		if idx < 0 {
			idx = strings.Index(text, "etag text GENERATED ALWAYS AS")
		}
		if idx < 0 {
			continue
		}
		sawEtag = true
		// 取该语句到分号为止
		stmt := text[idx:]
		if end := strings.Index(stmt, ";"); end > 0 {
			stmt = stmt[:end]
		}
		if !strings.Contains(stmt, "STORED") {
			problems = append(problems, "R-21 被破坏:"+e.Name()+" 的 etag 生成列不是 STORED")
		}
		if strings.Contains(stmt, `'"'`) || strings.Contains(stmt, `'"' ||`) {
			problems = append(problems,
				"R-21 被破坏:"+e.Name()+" 的 etag 生成列表达式里出现了双引号(引号只能由 QuoteETag 统一加)")
		}
	}
	if !sawEtag {
		problems = append(problems, "R-21 无法校验:迁移里找不到 files.etag 的生成列定义")
	}
	// 引号出口必须唯一
	count := 0
	_ = filepath.WalkDir(filepath.Join(repoRoot, "internal"), func(path string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.Contains(readIfExists(path), "func QuoteETag(") {
			count++
		}
		return nil
	})
	if count != 1 {
		problems = append(problems,
			"R-21 被破坏:QuoteETag 的定义处应为恰好 1 个,实际 "+itoa(count)+"(多个出口必然有一处漏加引号)")
	}
	return problems
}

// ---- R-15:secret 只走 env,永不进 yaml ----
//
// 示例配置里填了真值,后来者就会照抄,而"配置文件进版本库"是安全事故的常见形态。
func checkR15Secrets(repoRoot string) []string {
	var problems []string
	cfg := filepath.Join(repoRoot, "deploy", "config", "config.example.yaml")
	text := readIfExists(cfg)
	if text == "" {
		// 文件不存在不判违规(示例配置可以放在别处),但要有 deploy 目录
		return nil
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		lower := strings.ToLower(trimmed)
		// token 类键在 2026-09-13 补进来:示例配置新增了 `metrics.token`(跨机抓取
		// /metrics 用的 Bearer 令牌)。它同样是秘密,而"示例里填了真值就会被照抄"
		// 这条风险与 password 完全一样 —— 漏掉它等于给"只走 env"的纪律留一个后门。
		for _, key := range []string{"password:", "secret:", "secret_key:", "redis_password:", "token:"} {
			if strings.HasPrefix(lower, key) {
				val := strings.TrimSpace(strings.TrimPrefix(trimmed, trimmed[:len(key)]))
				val = strings.Trim(val, `"'`)
				if val != "" && val != `""` && val != "''" {
					problems = append(problems,
						"R-15 被破坏:deploy/config/config.example.yaml 里 "+key+" 带了值("+trimmed+")—— secret 只走 env")
				}
			}
		}
	}
	return problems
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// checkR24Rust 检查 R-24:不得出现 Rust 工程。
func checkR24Rust(repoRoot string) []string {
	var problems []string
	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := filepath.Base(path)
		if d.IsDir() {
			switch base {
			case ".git", "node_modules", "bin", "obj", ".gocache", ".gomodcache", ".pnpm-store", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case base == "Cargo.toml" || base == "Cargo.lock" || strings.HasSuffix(base, ".rs"):
			rel, _ := filepath.Rel(repoRoot, path)
			problems = append(problems,
				"R-24 被破坏:出现 Rust 工程/源码 "+filepath.ToSlash(rel)+"(一期不做二期,同步引擎只有一套 C# 实现)")
		}
		return nil
	})
	if err != nil {
		return []string{"R-24 无法校验:" + err.Error()}
	}
	return problems
}

// checkR25R26Wix 检查 R-25(WiX 固定 6.0.2)与 R-26(MSI perUser + MajorUpgrade)。
func checkR25R26Wix(repoRoot string) []string {
	var problems []string

	// ---- R-25:WiX 版本必须到处钉 6.0.2 ----
	_ = filepath.WalkDir(filepath.Join(repoRoot, "desktop"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".wixproj") && !strings.HasSuffix(path, ".props") {
			return nil
		}
		text := readIfExists(path)
		if text == "" {
			return nil
		}
		// SDK 形式:`Sdk="WixToolset.Sdk/6.0.2"`;包形式:`Include="WixToolset.UI.wixext" Version="6.0.2"`
		for _, m := range regexp.MustCompile(`WixToolset\.Sdk/([0-9][^"\s]*)`).FindAllStringSubmatch(text, -1) {
			if m[1] != "6.0.2" {
				rel, _ := filepath.Rel(repoRoot, path)
				problems = append(problems, "R-25 被破坏:"+filepath.ToSlash(rel)+" 的 WiX SDK 是 "+m[1]+",必须固定 6.0.2(v7 有 OSMF 授权门槛)")
			}
		}
		for _, m := range regexp.MustCompile(`WixToolset[.\w]*"\s+Version="([^"]+)"`).FindAllStringSubmatch(text, -1) {
			if m[1] != "6.0.2" {
				rel, _ := filepath.Rel(repoRoot, path)
				problems = append(problems, "R-25 被破坏:"+filepath.ToSlash(rel)+" 的 WiX 包版本是 "+m[1]+",必须固定 6.0.2")
			}
		}
		return nil
	})

	// ---- R-26:MSI 必须 perUser + MajorUpgrade ----
	setupDir := filepath.Join(repoRoot, "desktop", "src", "NetDisk.Setup")
	if entries, derr := os.ReadDir(setupDir); derr == nil {
		found := false
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".wxs") {
				continue
			}
			found = true
			text := readIfExists(filepath.Join(setupDir, e.Name()))
			if !strings.Contains(text, `Scope="perUser"`) {
				problems = append(problems,
					"R-26 被破坏:"+e.Name()+" 缺少 Scope=\"perUser\"(要求提权才能装是企业落地的最大阻力)")
			}
			if !strings.Contains(text, "MajorUpgrade") {
				problems = append(problems,
					"R-26 被破坏:"+e.Name()+" 缺少 MajorUpgrade(自己排 RemoveExistingProducts 顺序错了会先删旧版再装新版)")
			}
		}
		if !found {
			problems = append(problems, "R-26 无法校验:desktop/src/NetDisk.Setup 下没有 .wxs")
		}
	}

	// ---- R-26 附加:ICE 抑制清单必须"被 perUser 撑住"(DE-D-20)----
	//
	// 为什么把抑制清单也纳入检查:perUser 包必须抑制 ICE38/ICE60/ICE64/ICE91
	// (这四条的前提都是 per-machine 安装),但"抑制"本身是危险动作 —— 一旦有人把包改成
	// perMachine,这些抑制就从"官方建议"变成"掩盖真问题"。所以这里两头都钉:
	// ①包必须仍是 perUser(上面已查);②抑制清单必须**恰好**是这五条(有人加第六条就要解释)。
	//
	// **2026-09-13 订正(此前这条门禁一直是红的)**:`ICE61` 是后加的第五条,理由是
	// `Product.wxs` 打开了 `AllowSameVersionUpgrades="yes"`(否则开发期"同版本、不同 ProductCode"
	// 的新包会被当成另一个产品,叠加安装、卸载只摘一层)。该开关会让生成的 Upgrade 表上界
	// 等于当前版本,ICE61 必然报 "The Maximum version is not less than the current product" ——
	// 也就是**由该开关直接导致**的告警,官方建议抑制,理由已写在 `NetDisk.Setup.wixproj` 注释里。
	// 当时改了 wixproj 却**忘了同步这条检查**,于是门禁一直红着(而红着的门禁等于没有门禁)。
	// 现在把 ICE61 纳入期望清单 —— 门禁的价值不变:**任何第六个抑制都必须显式改这里并写明理由**。
	wixproj := filepath.Join(setupDir, "NetDisk.Setup.wixproj")
	if text := readIfExists(wixproj); text != "" {
		want := "ICE38;ICE60;ICE61;ICE64;ICE91"
		got := ""
		if m := regexp.MustCompile(`<SuppressIces>([^<]*)</SuppressIces>`).FindStringSubmatch(text); m != nil {
			got = strings.TrimSpace(m[1])
		}
		if got != want {
			problems = append(problems,
				"R-26 被破坏:ICE 抑制清单是 "+strconv.Quote(got)+",应为 "+strconv.Quote(want)+
					"(四条 per-machine 前提 + ICE61=AllowSameVersionUpgrades 的直接后果;"+
					"perUser-only 包才允许抑制,改动必须同步改这条检查与注释)")
		}
	}
	return problems
}
