// Package db 封装 pgx 连接池与事务边界(6.6)。
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/config"
)

// DB 是应用使用的数据库句柄。
type DB struct {
	Pool *pgxpool.Pool
}

// Open 建池并做一次连通性探活。
func Open(ctx context.Context, cfg config.Database) (*DB, error) {
	pc, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// 2.6/8.2:池上限与 PG max_connections 对齐(配置校验已限制 <= 40)
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.ConnMaxLifetime.Std()
	pc.MaxConnIdleTime = 5 * time.Minute
	pc.HealthCheckPeriod = time.Minute
	// 语句级超时:防止慢查询长期占用连接(6.6)
	if d := cfg.StatementTimeout.Std(); d > 0 {
		if pc.ConnConfig.RuntimeParams == nil {
			pc.ConnConfig.RuntimeParams = map[string]string{}
		}
		pc.ConnConfig.RuntimeParams["statement_timeout"] = fmt.Sprintf("%d", d.Milliseconds())
		pc.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "60000"
	}
	if err := pingPool(ctx, pc); err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("pgxpool new: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// OpenWithDSN 直接以 DSN 建池(集成测试与运维脚本用)。
//
// 生产路径请用 Open(config.Database),以保证池参数来自配置校验过的值。
func OpenWithDSN(ctx context.Context, dsn string, maxConns int32) (*DB, error) {
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if maxConns > 0 {
		pc.MaxConns = maxConns
	}
	if pc.MaxConns > 40 {
		// 上限兜底:避免集成测试/脚本把 PG 连接打满
		pc.MaxConns = 40
	}
	pc.MaxConnIdleTime = 5 * time.Minute
	pc.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("pgxpool new: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// pingPool 校验池配置可连(在正式建池前快速失败)。
func pingPool(ctx context.Context, pc *pgxpool.Config) error {
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return fmt.Errorf("pgxpool new: %w", err)
	}
	defer pool.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

func (d *DB) Close() {
	if d.Pool != nil {
		d.Pool.Close()
	}
}

// Ping 供 /healthz 使用。
func (d *DB) Ping(ctx context.Context) error {
	if d.Pool == nil {
		return errors.New("db not initialized")
	}
	return d.Pool.Ping(ctx)
}

// InTx 在事务中执行 fn:
//   - fn 返回错误 → 回滚
//   - fn panic → 回滚并继续 panic(由中间件转 500)
//
// 6.6 纪律:调用方**禁止**在事务内做对象 IO(存储写入/读取、网络调用);
// 本函数不做检测,由代码评审与集成测试保证(R-04)。
func (d *DB) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// 使用独立 context,避免外层 ctx 已取消导致回滚无法执行
			rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}

// AcquireConn 取一条专用连接。
//
// 6.4/ADR-2 连接纪律:会话级 advisory 锁(finalizeUpload / 清理 worker)
// **必须**全程绑在同一条 *pgx.Conn 上,解锁后断言并归还。
// 严禁用 Pool.ExecContext 执行 pg_advisory_unlock —— 池可能给到另一条连接。
func (d *DB) AcquireConn(ctx context.Context) (*pgxpool.Conn, error) {
	return d.Pool.Acquire(ctx)
}

// AsQuerier 返回一个同时具备"池读 + 事务"能力的适配器。
//
// 用途:authsvc 等上层需要"简单读走连接池、复杂写走事务",
// 但 repo.Querier 与 db.DB 是两套接口。用函数类型适配比再造一个结构体更轻。
//
// 用法:
//
//	svc.DB = db.AsQuerier(database)
type QuerierAdapter struct {
	pool *pgxpool.Pool
	db   *DB
}

// AsQuerier 构造适配器。
func AsQuerier(d *DB) *QuerierAdapter {
	return &QuerierAdapter{pool: d.Pool, db: d}
}

func (a *QuerierAdapter) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return a.pool.Exec(ctx, sql, args...)
}

func (a *QuerierAdapter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return a.pool.Query(ctx, sql, args...)
}

func (a *QuerierAdapter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return a.pool.QueryRow(ctx, sql, args...)
}

// InTx 委托给 DB 的事务实现。
func (a *QuerierAdapter) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return a.db.InTx(ctx, fn)
}
