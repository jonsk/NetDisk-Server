package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/storage"
)

// 存储配置的**可文档化性**断言。
//
// 为什么值得单独一组:存储是唯一"配错了会导致用户数据不可读"的配置段,
// 而它现在的现状是**只有一项**(storage.root)。越是只有一项,越容易在示例配置里
// 被漏掉 —— 运维照着示例部署,拿到的是 Go 默认值 `./data`。
//
// Server-com 开源版:go-storage 已整体拆除,**仅支持本地 fs**。这三条同时把
// "示例配置有 root / env 覆盖它 / 没有被假装出来的后端开关"钉住。

// ① 示例配置必须**显式**给出 storage.root。
func TestExampleConfigDocumentsStorageRoot(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读示例配置失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析示例配置失败: %v", err)
	}
	sec, ok := doc["storage"].(map[string]any)
	if !ok {
		t.Fatal("示例配置里没有 storage 段")
	}
	root, ok := sec["root"].(string)
	if !ok || strings.TrimSpace(root) == "" {
		t.Fatalf("storage.root 必须是非空字符串,实际 %#v", sec["root"])
	}
	if want := config.Default().Storage.Root; root != want {
		t.Fatalf("示例 storage.root = %q,与 Go 默认值 %q 不一致", root, want)
	}
}

// ② env 必须能覆盖 yaml:生产的实际路径就是靠 secrets.env 里的
// NETDISK_STORAGE_ROOT 定的。
func TestStorageRootEnvOverridesYAML(t *testing.T) {
	yamlRoot := filepath.Join(t.TempDir(), "from-yaml")
	envRoot := filepath.Join(t.TempDir(), "from-env")
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "storage:\n  root: '" + yamlRoot + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	t.Setenv("JWT_SECRET", strings.Repeat("k", 32))

	t.Setenv("NETDISK_STORAGE_ROOT", "")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Storage.Root != yamlRoot {
		t.Fatalf("未设置 env 时应采用 yaml 值 %q,实际 %q", yamlRoot, cfg.Storage.Root)
	}

	t.Setenv("NETDISK_STORAGE_ROOT", envRoot)
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Storage.Root != envRoot {
		t.Fatalf("env 应覆盖 yaml,实际 %q", cfg.Storage.Root)
	}
}

// ③ 后端选择:**运行时只有一个后端,且只能是本地 fs**。
func TestStorageBackendDefaultsToFS(t *testing.T) {
	cfg := config.Default()
	if cfg.Storage.Backend != config.BackendFS {
		t.Fatalf("默认后端应为 %q,实际 %q", config.BackendFS, cfg.Storage.Backend)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  http_addr: \"127.0.0.1:9999\"\n"), 0o600); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	t.Setenv("JWT_SECRET", strings.Repeat("k", 32))
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if loaded.Storage.Backend != config.BackendFS {
		t.Fatalf("没有 backend 键的老配置应回落到 fs,实际 %q", loaded.Storage.Backend)
	}
}

func TestStorageBackendNormalization(t *testing.T) {
	for _, in := range []string{"FS", " fs ", "Fs"} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte("storage:\n  backend: \""+in+"\"\n  root: \"./data\"\n"), 0o600); err != nil {
			t.Fatalf("写临时配置失败: %v", err)
		}
		t.Setenv("JWT_SECRET", strings.Repeat("k", 32))
		t.Setenv("NETDISK_DB_PASSWORD", strings.Repeat("p", 24))
		t.Setenv("NETDISK_REDIS_PASSWORD", strings.Repeat("r", 24))
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("加载配置失败: %v", err)
		}
		if cfg.Storage.Backend != config.BackendFS {
			t.Errorf("backend=%q 应归一为 %q,实际 %q", in, config.BackendFS, cfg.Storage.Backend)
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("backend=%q 归一后应通过校验: %v", in, err)
		}
	}
}

// 任何非 fs 后端都必须被拒绝(go-storage 已拆除;绝不静默回落到 fs)。
func TestStorageRejectsNonFSBackend(t *testing.T) {
	for _, b := range []string{"s3", "minio", "oss", "dropbox", "memory", "webdav", "qingstor", "kodo", "onedrive", "storj", "azfile", "dropbocks"} {
		cfg := config.Default()
		cfg.Storage.Backend = b
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("后端 %q 必须被拒绝(本版本仅支持本地 fs)", b)
		}
		if !strings.Contains(err.Error(), "不支持") {
			t.Errorf("backend=%q 的错误应说明「不支持」: %v", b, err)
		}
	}
}

// ④ 后端一致性自检:库里的对象必须属于当前后端,否则拒绝启动(除非显式放行)。
func TestBackendMismatchCheck(t *testing.T) {
	if warn, err := config.CheckBackendMismatch("fs", 0, false); err != nil || warn != "" {
		t.Fatalf("无跨后端对象时不应报错/告警,实际 warn=%q err=%v", warn, err)
	}
	warn, err := config.CheckBackendMismatch("s3", 7, false)
	if err == nil {
		t.Fatal("库中存在其它后端的对象时必须拒绝启动")
	}
	for _, want := range []string{"7", "s3", "迁移"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息应包含 %q: %v", want, err)
		}
	}
	if warn != "" {
		t.Errorf("拒绝启动时不应再返回 warn: %q", warn)
	}
	warn, err = config.CheckBackendMismatch("s3", 7, true)
	if err != nil {
		t.Fatalf("allow_backend_mismatch=true 时应放行: %v", err)
	}
	if warn == "" || !strings.Contains(warn, "7") {
		t.Fatalf("放行时必须给出带数量的 WARN: %q", warn)
	}
}

// ⑤ 示例配置必须把"后端"讲清楚,且只认 fs。
func TestExampleConfigDocumentsBackend(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "config", "config.example.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读示例配置失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析示例配置失败: %v", err)
	}
	sec, ok := doc["storage"].(map[string]any)
	if !ok {
		t.Fatal("示例配置里没有 storage 段")
	}
	if got, _ := sec["backend"].(string); got != config.BackendFS {
		t.Fatalf("示例配置的 storage.backend 应为 %q(唯一支持),实际 %#v", config.BackendFS, sec["backend"])
	}
	// 非 fs 后端**只能出现在注释里**:YAML 解析后不该看到这些键
	for _, bad := range []string{"s3", "minio", "azblob", "gcs", "oss", "cos", "webdav", "sftp", "ftp", "ipfs", "dropbox"} {
		if _, exists := sec[bad]; exists {
			t.Errorf("示例配置把不支持的后端 %q 写成了真实配置(%#v)", bad, sec[bad])
		}
	}
	// fs-only:不允许真实的 remote 参数段(会误导读者以为能配云存储),但注释里可提
	text := string(raw)
	for _, want := range []string{"backend", "fs", "NETDISK_STORAGE_ROOT"} {
		if !strings.Contains(text, want) {
			t.Errorf("示例配置的注释里应提到 %q", want)
		}
	}
}

// 配置层常量与 storage 包必须同值(它要落进 file_objects.storage_backend)。
func TestBackendConstantMatchesStoragePackage(t *testing.T) {
	if config.BackendFS != storage.BackendFS {
		t.Errorf("config.BackendFS %q 与 storage.BackendFS %q 必须同值", config.BackendFS, storage.BackendFS)
	}
	if !config.StorageBackendImplemented(config.BackendFS) {
		t.Error("fs 必须被识别为已实现")
	}
	if config.StorageBackendImplemented("s3") {
		t.Error("s3 已被拆除,配置层不得认它")
	}
}
