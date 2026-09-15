package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/netdisk/netdisk/internal/config"
)

// BE-S0-08 验收①:各接口族**独立阈值且可配**。
//
// 这条此前不成立:路由里全部调用 `middleware.DefaultScope(name)`,
// 名字虽不同但阈值是同一个常量(20/s、600/min),也就是"看起来分族、
// 实际一刀切"。对登录口(暴力破解面)与同步高频路径(文件列表/HEAD)
// 用同一个阈值必然顾此失彼。现在阈值来自 `config.RateLimits`。

func TestRateLimitsArePerFamily(t *testing.T) {
	cfg := config.Default()
	login := cfg.RateLimits.For("login")
	fileRead := cfg.RateLimits.For("file_read")

	if login.PerSecond == 0 || login.PerMinute == 0 {
		t.Fatalf("login 必须有阈值: %+v", login)
	}
	if fileRead.PerSecond == 0 || fileRead.PerMinute == 0 {
		t.Fatalf("file_read 必须有阈值: %+v", fileRead)
	}
	// 登录必须比同步读路径更严(否则分族没有意义)
	if login.PerSecond >= fileRead.PerSecond {
		t.Errorf("登录每秒上限(%d)应严于文件详情(%d)—— 登录是暴力破解面",
			login.PerSecond, fileRead.PerSecond)
	}
	if login.PerMinute >= fileRead.PerMinute {
		t.Errorf("登录每分钟上限(%d)应严于文件详情(%d)", login.PerMinute, fileRead.PerMinute)
	}
	// 登录按 IP(此时还没有 userID)
	if !login.ByIP {
		t.Error("登录口必须按 IP 限速(未认证请求没有 userID)")
	}
}

// 示例配置必须**逐个登记**接口族。
//
// 为什么单列一条:漏登记的族不会报错,而是**静默回落到 default**,而 default 是
// by_ip=true —— 同一出口 IP 下的所有用户共享一份配额。这类"少写一段 yaml"的缺口
// 在评审里几乎看不出来,却会在真机上表现成"客户端偶尔什么都传不上去"(实测:
// 客户端把 429 当永久失败,状态停在待上传且日志里没有痕迹)。
func TestExampleConfigRegistersEveryRateLimitFamily(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读示例配置失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析示例配置失败: %v", err)
	}
	rl, ok := doc["rate_limits"].(map[string]any)
	if !ok {
		t.Fatal("示例配置里没有 rate_limits 段")
	}
	// 与 config.RateLimits 的字段一一对应(新增族时这里也要加,否则新族没有任何文档)
	families := []string{"login", "upload", "file_list", "file_read", "file_write", "webdav", "default"}
	for _, f := range families {
		if _, ok := rl[f]; !ok {
			t.Errorf("示例配置没有登记接口族 %q:它会静默回落到 default(by_ip=true)", f)
		}
	}
}

// 未登记的族回落到 default,而**不是不限速**
func TestRateLimitsUnknownFamilyFallsBackToDefault(t *testing.T) {
	cfg := config.Default()
	got := cfg.RateLimits.For("some_future_family")
	def := cfg.RateLimits.Default
	if got.PerSecond != def.PerSecond || got.PerMinute != def.PerMinute {
		t.Fatalf("未知族应回落 default(%+v),实际 %+v", def, got)
	}
	if got.PerSecond == 0 && got.PerMinute == 0 {
		t.Fatal("回落结果不得是不限速")
	}
}

// 阈值可从 yaml 覆盖(可配性)
func TestRateLimitsLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
jwt:
  secret: ignored-because-env-only
server:
  http_addr: "127.0.0.1:9999"
rate_limits:
  login:
    per_second: 1
    per_minute: 3
    by_ip: true
  file_read:
    per_second: 100
    per_minute: 5000
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	t.Setenv("JWT_SECRET", strings.Repeat("k", 32))

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	login := cfg.RateLimits.For("login")
	if login.PerSecond != 1 || login.PerMinute != 3 {
		t.Fatalf("login 阈值应被 yaml 覆盖为 1/3,实际 %+v", login)
	}
	if fr := cfg.RateLimits.For("file_read"); fr.PerSecond != 100 || fr.PerMinute != 5000 {
		t.Fatalf("file_read 阈值应被 yaml 覆盖为 100/5000,实际 %+v", fr)
	}
}

// 全 0 的接口族必须被 fail-fast 拒绝:全 0 = 该族完全不限速
func TestValidateRejectsUnlimitedRateLimit(t *testing.T) {
	cfg := config.Default()
	cfg.RateLimits.Login = config.RateLimit{} // 故意清零
	cfg.JWT.Secret = strings.Repeat("k", 32)

	err := cfg.Validate()
	if err == nil {
		t.Fatal("全 0 的限速配置必须被拒绝(否则该接口族不限速)")
	}
	if !strings.Contains(err.Error(), "rate_limits.login") {
		t.Fatalf("错误信息应指明是哪个族,实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不限速") {
		t.Errorf("错误信息应说明后果,实际 %v", err)
	}
}

// 负数阈值同样拒绝
func TestValidateRejectsNegativeRateLimit(t *testing.T) {
	cfg := config.Default()
	cfg.RateLimits.Upload = config.RateLimit{PerSecond: -1, PerMinute: 10}
	cfg.JWT.Secret = strings.Repeat("k", 32)

	if err := cfg.Validate(); err == nil {
		t.Fatal("负数阈值必须被拒绝")
	}
}
