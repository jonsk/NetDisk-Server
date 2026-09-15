package config_test

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/config"
)

// 启动前置检查(BE-S10-05 / 6.8 纪律 3):fs 路径可写、端口未占用。
//
// 这些检查刻意与 Validate 分开(会碰文件系统与内核),因此单测也必须
// 真的碰它们 —— 用替身去"模拟不可写目录"测的只是替身。

// preflightConfig 造一份"除被测项外全部合法"的配置。
//
// 关键点:http_addr 用端口 0(内核分配任意空闲端口),这样默认用例不会与
// 开发机上的 8080 冲突 —— 否则测试会因为"本机正跑着服务"而随机失败,
// 而失败信息还指向被测代码。
func preflightConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Server.HTTPAddr = "127.0.0.1:0"
	cfg.Storage.Root = t.TempDir()
	return cfg
}

func TestPreflightAcceptsHealthyConfig(t *testing.T) {
	cfg := preflightConfig(t)
	if err := config.Preflight(cfg); err != nil {
		t.Fatalf("正常配置应通过前置检查: %v", err)
	}
	// 探针文件不能残留(残留会被下一次检查/备份脚本当成垃圾,也可能掩盖权限问题)
	for _, sub := range []string{"", "objects", "tus-tmp"} {
		p := filepath.Join(cfg.Storage.Root, sub, ".netdisk-preflight")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("探针文件应被清理: %s", p)
		}
	}
	// 暂存目录必须被**真的建出来**:生产里 tus-tmp 可能与 objects 分盘,
	// "objects 可写而 tus-tmp 不可写"是完全可能的配置
	if _, err := os.Stat(filepath.Join(cfg.Storage.Root, "tus-tmp")); err != nil {
		t.Fatalf("tus-tmp 应被创建: %v", err)
	}
}

// 数据目录位置被一个**文件**占着 → 必须在启动时就被拦下。
func TestPreflightRejectsFileWhereDirExpected(t *testing.T) {
	cfg := preflightConfig(t)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("造阻塞文件失败: %v", err)
	}
	// objects 位置被文件占住:MkdirAll 会失败
	if err := os.WriteFile(filepath.Join(cfg.Storage.Root, "objects"), []byte("x"), 0o600); err != nil {
		t.Fatalf("造阻塞文件失败: %v", err)
	}
	err := config.Preflight(cfg)
	if err == nil {
		t.Fatal("objects 被文件占住时应拒绝启动")
	}
	if !strings.Contains(err.Error(), "objects") {
		t.Fatalf("错误信息应点名具体路径项,实际: %v", err)
	}
}

// tus-tmp 不可用也必须单独被拦(不能只查 objects)。
func TestPreflightRejectsStageDirAsFile(t *testing.T) {
	cfg := preflightConfig(t)
	if err := os.WriteFile(filepath.Join(cfg.Storage.Root, "tus-tmp"), []byte("x"), 0o600); err != nil {
		t.Fatalf("造阻塞文件失败: %v", err)
	}
	err := config.Preflight(cfg)
	if err == nil || !strings.Contains(err.Error(), "tus-tmp") {
		t.Fatalf("tus-tmp 被文件占住时应拒绝启动并点名该项,实际: %v", err)
	}
}

// 端口被占用必须被拦下 —— 这是"另一个实例还在跑"最常见的表现。
func TestPreflightRejectsOccupiedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer func() { _ = ln.Close() }()

	cfg := preflightConfig(t)
	cfg.Server.HTTPAddr = ln.Addr().String() // 已被上面这个 listener 占着
	perr := config.Preflight(cfg)
	if perr == nil {
		t.Fatal("端口被占用时应拒绝启动")
	}
	if !strings.Contains(perr.Error(), cfg.Server.HTTPAddr) {
		t.Fatalf("错误信息应包含具体地址,实际: %v", perr)
	}
}

// 一次报全部问题(6.8 纪律 3 的"打印全部缺失/非法项后拒绝启动")。
func TestPreflightReportsAllProblemsAtOnce(t *testing.T) {
	cfg := preflightConfig(t)
	// 同时制造两个问题:objects 被文件占住 + 端口被占
	if err := os.WriteFile(filepath.Join(cfg.Storage.Root, "objects"), []byte("x"), 0o600); err != nil {
		t.Fatalf("造阻塞文件失败: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	defer func() { _ = ln.Close() }()
	cfg.Server.HTTPAddr = ln.Addr().String()

	perr := config.Preflight(cfg)
	if perr == nil {
		t.Fatal("两个问题并存时应拒绝启动")
	}
	msg := perr.Error()
	if !strings.Contains(msg, "objects") || !strings.Contains(msg, cfg.Server.HTTPAddr) {
		t.Fatalf("应一次报出全部问题(既是目录问题也是端口问题),实际: %v", msg)
	}
	if !strings.Contains(msg, "2 项") {
		t.Fatalf("应给出问题条数,实际: %v", msg)
	}
}
