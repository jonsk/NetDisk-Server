package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/config"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	return p
}

// 纪律 2:优先级 Go 默认 < yaml < env
func TestLoadPrecedence(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef" // 32 字节
	// 显式声明所有参与断言的 env,使用例**不依赖调用方的环境**:
	// 否则 CI/本地一旦导出了 NETDISK_*,断言就会随环境漂移(曾实际踩到)。
	t.Setenv("JWT_SECRET", secret)
	t.Setenv("NETDISK_DB_PASSWORD", "dbpass")
	t.Setenv("NETDISK_DB_MAX_CONNS", "16") // env 覆盖
	t.Setenv("NETDISK_HTTP_ADDR", "127.0.0.1:9090")
	// 这几个键故意**不设置**,但必须先清空可能从外层继承的值
	t.Setenv("NETDISK_DB_NAME", "")
	t.Setenv("NETDISK_REDIS_ADDR", "")
	t.Setenv("NETDISK_JWT_ACCESS_TTL", "")
	t.Setenv("NETDISK_JWT_REFRESH_TTL", "")
	t.Setenv("NETDISK_MAX_NAME_BYTES", "")
	t.Setenv("NETDISK_MAX_DEPTH", "")
	t.Setenv("NETDISK_TRUSTED_PROXIES", "")

	p := writeYAML(t, `
server:
  http_addr: 127.0.0.1:8088     # 被 env 覆盖
  max_concurrent_locks: 64      # yaml 覆盖默认
database:
  name: from_yaml
  max_conns: 8                  # 被 env 覆盖
redis:
  addr: 127.0.0.1:6380          # yaml 覆盖默认
jwt:
  access_ttl: 5m                # Duration 自定义解析
`)

	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if c.Server.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("env 应覆盖 yaml: got %q", c.Server.HTTPAddr)
	}
	if c.Server.MaxConcurrentLocks != 64 {
		t.Errorf("yaml 应覆盖默认: got %d", c.Server.MaxConcurrentLocks)
	}
	if c.Database.Name != "from_yaml" {
		t.Errorf("yaml 应覆盖默认: got %q", c.Database.Name)
	}
	if c.Database.MaxConns != 16 {
		t.Errorf("env 应覆盖 yaml: got %d", c.Database.MaxConns)
	}
	if c.Redis.Addr != "127.0.0.1:6380" {
		t.Errorf("yaml 应覆盖默认: got %q", c.Redis.Addr)
	}
	if c.JWT.AccessTTL.Std() != 5*time.Minute {
		t.Errorf("Duration 解析失败: got %v", c.JWT.AccessTTL.Std())
	}
	// 未被覆盖的项应保持默认(默认值单一来源)
	if c.JWT.RefreshTTL.Std() != 7*24*time.Hour {
		t.Errorf("未覆盖项应保持默认: got %v", c.JWT.RefreshTTL.Std())
	}
	if c.Policy.MaxNameBytes != 240 || c.Policy.MaxDepth != 31 {
		t.Errorf("默认策略值被意外改动: name=%d depth=%d", c.Policy.MaxNameBytes, c.Policy.MaxDepth)
	}
	// secret 只从 env
	if c.JWT.Secret != secret || c.Database.Password != "dbpass" {
		t.Errorf("secret 应从 env 注入")
	}
}

// 纪律 3:Duration 支持 "5m" / "1h30m" / "300ms"
func TestDurationForms(t *testing.T) {
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	t.Setenv("NETDISK_DB_PASSWORD", "p")
	p := writeYAML(t, `
server:
  read_header_timeout: 10s
  request_timeout: 1h30m
  shutdown_timeout: 300ms
database:
  conn_max_lifetime: 30m
`)
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"10s", c.Server.ReadHeaderTimeout.Std(), 10 * time.Second},
		{"1h30m", c.Server.RequestTimeout.Std(), 90 * time.Minute},
		{"300ms", c.Server.ShutdownTimeout.Std(), 300 * time.Millisecond},
		{"30m", c.Database.ConnMaxLifetime.Std(), 30 * time.Minute},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("Duration %s: got %v want %v", tc.name, tc.got, tc.want)
		}
	}
}

// 非法 Duration 必须直接报错,不能静默回退默认值
func TestDurationInvalid(t *testing.T) {
	t.Setenv("JWT_SECRET", strings.Repeat("x", 32))
	p := writeYAML(t, "server:\n  request_timeout: 60\n") // 缺单位
	if _, err := config.Load(p); err == nil {
		t.Fatal("非法 Duration 应报错")
	} else if !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("错误信息应指明 duration 非法: %v", err)
	}
}

// 纪律 3:fail-fast 且**打印全部问题**(不是遇到第一个就退出)
func TestValidateCollectsAllProblems(t *testing.T) {
	t.Setenv("JWT_SECRET", "short")         // 问题 1:太短
	t.Setenv("NETDISK_DB_PASSWORD", "")     // 问题 2:空
	t.Setenv("NETDISK_DB_MAX_CONNS", "200") // 问题 3:过大
	t.Setenv("NETDISK_LOG_LEVEL", "debug")

	c, err := config.Load(writeYAML(t, `
policy:
  max_name_bytes: 999
  max_depth: 0
server:
  trusted_proxies: []
`))
	if err != nil {
		t.Fatalf("Load 不应失败: %v", err)
	}
	err = c.Validate()
	if err == nil {
		t.Fatal("校验应失败")
	}
	msg := err.Error()
	for _, want := range []string{
		"JWT_SECRET", "NETDISK_DB_PASSWORD", "max_conns", "max_name_bytes", "max_depth", "trusted_proxies",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("校验信息应包含 %q,实际:\n%s", want, msg)
		}
	}
	if !strings.Contains(msg, "配置校验失败") || !strings.Contains(msg, "6 项") {
		t.Errorf("应汇总问题数量,实际:\n%s", msg)
	}
}

// 默认配置 + 必需 secret 应通过校验(保证默认值可用)
func TestDefaultsValid(t *testing.T) {
	t.Setenv("JWT_SECRET", strings.Repeat("k", 32))
	t.Setenv("NETDISK_DB_PASSWORD", "pw")
	c, err := config.Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
	// 2.6/8.2:默认池上限必须 <= 40
	if c.Database.MaxConns > 40 {
		t.Errorf("默认 max_conns=%d 超过上限 40", c.Database.MaxConns)
	}
	// DSN 必须含密码且不成谜
	dsn := c.Database.DSN()
	for _, want := range []string{"host=127.0.0.1", "dbname=netdisk", "user=netdisk", "password=pw"} {
		if !strings.Contains(dsn, want) {
			t.Errorf("DSN 缺少 %q: %s", want, dsn)
		}
	}
}

// secret 不进 yaml:即使 yaml 里写了 secret 字段也不会被采纳
func TestSecretIgnoredInYAML(t *testing.T) {
	t.Setenv("JWT_SECRET", strings.Repeat("e", 32))
	t.Setenv("NETDISK_DB_PASSWORD", "fromenv")
	p := writeYAML(t, `
jwt:
  secret: yaml-secret-should-be-ignored-at-least-32b
database:
  password: yaml-db-password
`)
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JWT.Secret == "yaml-secret-should-be-ignored-at-least-32b" {
		t.Error("JWT secret 不应从 yaml 采纳(纪律 1)")
	}
	if c.Database.Password != "fromenv" {
		t.Errorf("DB 密码应来自 env,got %q", c.Database.Password)
	}
}
