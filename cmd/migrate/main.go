// 迁移驱动:go run ./cmd/migrate up|status|down
//
// 单独成命令而不是塞进 netdisk 主程序,原因是:
//   - 生产迁移需要在停写窗口内单独执行(1024.6);
//   - 避免"每次启动都尝试迁移"带来的权限与并发问题。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
)

func main() {
	cfgPath := flag.String("config", "", "配置文件路径(yaml);默认只读 env")
	flag.Parse()

	cmd := "up"
	if flag.NArg() > 0 {
		cmd = flag.Arg(0)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail(err)
	}
	if err := cfg.Validate(); err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	database, err := db.Open(ctx, cfg.Database)
	if err != nil {
		fail(err)
	}
	defer database.Close()

	switch cmd {
	case "up":
		if err := migrate.Up(ctx, database.Pool); err != nil {
			fail(err)
		}
		status, _ := migrate.Status(ctx, database.Pool)
		fmt.Println("migrate up done;", status)
	case "status":
		status, err := migrate.Status(ctx, database.Pool)
		if err != nil {
			fail(err)
		}
		fmt.Println(status)
	default:
		fail(fmt.Errorf("未知子命令 %q(支持: up|status)", cmd))
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
	os.Exit(1)
}
