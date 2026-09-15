// Package migrate 用 goose 执行版本化 SQL 迁移(6.6)。
//
// SQL 文件内嵌进二进制(embed),生产部署无需附带迁移脚本目录。
package migrate

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed sql/*.sql
var sqlFS embed.FS

// Up 执行全部未应用的迁移。
func Up(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	goose.SetBaseFS(sqlFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, db, "sql"); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// Status 返回当前迁移版本信息(启动自检用)。
func Status(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	goose.SetBaseFS(sqlFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return "", err
	}
	if err := goose.StatusContext(ctx, db, "sql"); err != nil {
		return "", err
	}
	v, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("db_version=%d", v), nil
}
