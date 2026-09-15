package main

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/netdisk/netdisk/internal/config"
)

// TestPrintConfigWorksWhileServiceIsRunning 是真机部署 1.0.179 时抓到的**真实缺陷**
// (BE-S10-09 的补刀):`-print-config` 原先排在 `config.Preflight` **之后**,而 Preflight
// 会去绑监听端口 —— 于是"服务正在运行时想看生效配置"这条最常用的路径必然失败:
//
//	netdisk: 启动前置检查未通过(1 项):
//	 - 监听地址不可用(127.0.0.1:8080): listen tcp 127.0.0.1:8080: bind: address already in use
//
// 判据刻意用"端口被占 + 数据目录不存在"这两个**必然会**让 Preflight 失败的条件:
// 打印生效配置不该依赖启动检查,而且恰恰在服务运行(端口被占)时最需要它。
//
// ⚠ 本用例是包里**唯一**调用 run() 的用例:run() 用 flag.String 注册全局 flag,
// 同一进程里调用两次会 panic(flag redefined)。
func TestPrintConfigWorksWhileServiceIsRunning(t *testing.T) {
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatalf("占住一个端口失败: %v", lerr)
	}
	defer ln.Close()

	cfg := config.Default()
	cfg.Server.HTTPAddr = ln.Addr().String()                          // 已被占用
	cfg.Storage.Root = filepath.Join(t.TempDir(), "missing-data-dir") // 不存在
	raw, merr := yaml.Marshal(cfg)
	if merr != nil {
		t.Fatalf("序列化配置失败: %v", merr)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if werr := os.WriteFile(path, raw, 0o600); werr != nil {
		t.Fatalf("写临时配置失败: %v", werr)
	}
	t.Setenv("JWT_SECRET", strings.Repeat("k", 32))
	t.Setenv("NETDISK_DB_PASSWORD", "probe-password")

	oldArgs := os.Args
	os.Args = []string{"netdisk", "-config", path, "-print-config"}
	defer func() { os.Args = oldArgs }()

	out := captureStdout(t, func() error { return run() })
	if !strings.Contains(out, "storage.root") {
		t.Fatalf("没有打印出生效配置:\n%s", out)
	}
}

// captureStdout 捕获 f 执行期间的 stdout,并在 f 返回错误时直接判失败。
func captureStdout(t *testing.T, f func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, werr := os.Pipe()
	if werr != nil {
		t.Fatalf("建管道失败: %v", werr)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	err := f()
	w.Close()
	os.Stdout = old
	out := <-done
	if err != nil {
		t.Fatalf("服务正在运行时 -print-config 必须仍能打印生效配置"+
			"(否则排障时看不了生效值,这正是 BE-S10-09 要解决的问题): %v", err)
	}
	return out
}

// lineValue 从 `-print-config` 输出里取某个键的行,返回 `=` 右侧内容。
func lineValue(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), prefix) {
			_, rhs, ok := strings.Cut(ln, "=")
			if !ok {
				t.Fatalf("行 %q 里没有 '='", ln)
			}
			return strings.TrimSpace(rhs)
		}
	}
	t.Fatalf("`-print-config` 输出里没有以 %q 开头的行;运维据此确认生效值,"+
		"少一行就等于该段配置对运维不可见。输出:\n%s", prefix, out)
	return ""
}

// BE-S10-09:`-print-config` 必须覆盖**运维真会问生效值**的段,
// 并且 secret 只报长度、绝不打印明文。
//
// 为什么值得一条用例:这个函数是纯字符串拼接,删掉一行没有任何编译期或运行期信号,
// 而它的用途恰恰是"排障时确认生效值"—— 漏一段的代价是运维只能去翻 secrets.env
// 和启动日志(这正是 BE-S10-09 报的原始缺口)。
func TestPrintConfigCoversStoragePatrolRateLimitsAndMasksSecrets(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Backend = "s3"
	cfg.Storage.Root = "/srv/netdisk-probe-root"
	cfg.Storage.AllowBackendMismatch = true
	cfg.Patrol.Enabled = true
	cfg.Patrol.IntervalHours = 7
	cfg.Patrol.LocalSampleSize = 123
	cfg.RateLimits.Login.PerSecond = 11
	cfg.RateLimits.Login.PerMinute = 111
	cfg.RateLimits.Login.ByIP = false
	const (
		dbPwd    = "PGPASSWORD-must-not-appear"
		redisPwd = "REDISPASSWORD-must-not-appear"
		jwtSec   = "JWTSECRET-must-not-appear-must-not-appear"
	)
	cfg.Database.Password = dbPwd
	cfg.Redis.Password = redisPwd
	cfg.JWT.Secret = jwtSec

	out := redactedText(cfg)

	// ① 存储段:后端 / 根目录 / 一致性开关
	if got := lineValue(t, out, "storage.backend"); got != "s3" {
		t.Errorf("storage.backend = %q,want s3", got)
	}
	if got := lineValue(t, out, "storage.root"); got != "/srv/netdisk-probe-root" {
		t.Errorf("storage.root = %q,want /srv/netdisk-probe-root", got)
	}
	if got := lineValue(t, out, "storage.allow_mismatch"); got != "true" {
		t.Errorf("storage.allow_mismatch = %q,want true", got)
	}

	// ② 巡检段:外置后端也必须打(否则运维没法确认巡检是不是在跑)
	if got := lineValue(t, out, "patrol"); !strings.Contains(got, "enabled=true") ||
		!strings.Contains(got, "interval=7h") || !strings.Contains(got, "local_sample=123") {
		t.Errorf("patrol 行内容不符: %q", got)
	}

	// ③ 限速段:逐族打印,便于对着 429 排障
	if got := lineValue(t, out, "rate_limits.login"); got != "11/s 111/min by_ip=false" {
		t.Errorf("rate_limits.login = %q,want \"11/s 111/min by_ip=false\"", got)
	}
	for _, fam := range []string{"upload", "file_list", "file_read", "file_write", "webdav", "default"} {
		lineValue(t, out, "rate_limits."+fam)
	}

	// ④ secret 一律脱敏:既不能出现明文,又要能看出"配了"
	for _, sec := range []string{dbPwd, redisPwd, jwtSec} {
		if strings.Contains(out, sec) {
			t.Errorf("`-print-config` 打出了 secret 明文(纪律 1):%q\n输出:\n%s", sec, out)
		}
	}
	for _, kv := range []struct{ key, want string }{
		{"database.password", "(set,len=" + strconv.Itoa(len(dbPwd)) + ")"},
		{"redis.password", "(set,len=" + strconv.Itoa(len(redisPwd)) + ")"},
		{"jwt.secret", "(set,len=" + strconv.Itoa(len(jwtSec)) + ")"},
	} {
		if got := lineValue(t, out, kv.key); got != kv.want {
			t.Errorf("%s = %q,want %q", kv.key, got, kv.want)
		}
	}
}

// 空的 secret 要显式写成 (empty),而不是留空白 —— 空白行看起来像"忘了打"。
func TestPrintConfigMarksEmptySecrets(t *testing.T) {
	cfg := config.Default()
	cfg.Database.Password, cfg.Redis.Password, cfg.JWT.Secret = "", "", ""
	out := redactedText(cfg)
	for _, key := range []string{"database.password", "redis.password", "jwt.secret"} {
		if got := lineValue(t, out, key); got != "(empty)" {
			t.Errorf("%s = %q,want (empty)", key, got)
		}
	}
}
