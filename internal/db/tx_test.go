package db_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/db"
)

// BE-S0-05 验收②的实证:`InTx` 遇 error/panic **必回滚**。
//
// 这条此前没有单测 —— 而"事务回滚"恰恰是那种"看起来写了就一定对、
// 实际漏了 defer Rollback 就会静默提交"的代码。这里用真实库验证:
// 建一张隔离的临时表,在事务里插入后返回错误/触发 panic,
// 断言表里**没有行**;并额外验证回滚后的连接可复用(证明没有连接泄漏)。

// pgxTx 是 pgx.Tx 的别名,避免每处都写全名。
type pgxTx = pgx.Tx

func openDB(t *testing.T) *db.DB {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

// withScratchTable 建一张本用例专用的临时表(表名只含小写字母/数字/下划线)。
func withScratchTable(t *testing.T, database *db.DB) string {
	t.Helper()
	ctx := context.Background()
	clean := "txp_"
	for _, r := range t.Name() {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			clean += string(r)
		case r >= 'A' && r <= 'Z':
			clean += string(r + 32)
		}
	}
	if len(clean) > 50 {
		clean = clean[:50]
	}
	if _, err := database.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+clean+` (v int)`); err != nil {
		t.Fatalf("建临时表失败: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `DELETE FROM `+clean); err != nil {
		t.Fatalf("清空临时表失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS `+clean)
	})
	return clean
}

func TestInTxRollsBackOnError(t *testing.T) {
	database := openDB(t)
	table := withScratchTable(t, database)
	ctx := context.Background()

	sentinel := errors.New("故意失败")
	err := database.InTx(ctx, func(tx pgxTx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (1)`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应返回原始错误,实际 %v", err)
	}
	if n := countRows(t, database, table); n != 0 {
		t.Fatalf("事务返回错误后必须回滚,表内却有 %d 行", n)
	}
}

func TestInTxRollsBackOnPanic(t *testing.T) {
	database := openDB(t)
	table := withScratchTable(t, database)
	ctx := context.Background()

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("panic 应继续向外抛(由 middleware.Recoverer 转 500)")
			}
		}()
		_ = database.InTx(ctx, func(tx pgxTx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (2)`); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if n := countRows(t, database, table); n != 0 {
		t.Fatalf("panic 后必须回滚,表内却有 %d 行", n)
	}
}

func TestInTxCommitsOnSuccess(t *testing.T) {
	database := openDB(t)
	table := withScratchTable(t, database)
	ctx := context.Background()

	if err := database.InTx(ctx, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (3)`)
		return err
	}); err != nil {
		t.Fatalf("正常事务应提交: %v", err)
	}
	if n := countRows(t, database, table); n != 1 {
		t.Fatalf("提交后应有 1 行,实际 %d", n)
	}
}

// 回滚后连接必须可复用:池上限只有 4,若回滚泄漏连接这里会超时。
func TestInTxConnectionReusableAfterRollback(t *testing.T) {
	database := openDB(t)
	table := withScratchTable(t, database)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_ = database.InTx(ctx, func(tx pgxTx) error {
			_, _ = tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (9)`)
			return errors.New("rollback please")
		})
	}
	if err := database.InTx(ctx, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (10)`)
		return err
	}); err != nil {
		t.Fatalf("回滚后连接应可复用: %v", err)
	}
	if n := countRows(t, database, table); n != 1 {
		t.Fatalf("只应有最后一次提交的 1 行,实际 %d", n)
	}
}

// QuerierAdapter.InTx 必须委托到同一个事务实现(svc 层都通过它开事务,
// 若它自己另写一份就容易与 DB.InTx 的行为漂移)。
func TestQuerierAdapterInTxDelegates(t *testing.T) {
	database := openDB(t)
	table := withScratchTable(t, database)
	ctx := context.Background()
	q := db.AsQuerier(database)

	if err := q.InTx(ctx, func(tx pgxTx) error {
		_, err := tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (11)`)
		return err
	}); err != nil {
		t.Fatalf("适配器事务应可用: %v", err)
	}
	if n := countRows(t, database, table); n != 1 {
		t.Fatalf("提交后应有 1 行,实际 %d", n)
	}

	err := q.InTx(ctx, func(tx pgxTx) error {
		_, _ = tx.Exec(ctx, `INSERT INTO `+table+` (v) VALUES (12)`)
		return errors.New("fail")
	})
	if err == nil {
		t.Fatal("应返回错误")
	}
	if n := countRows(t, database, table); n != 1 {
		t.Fatalf("失败事务应回滚,行数应仍为 1,实际 %d", n)
	}
}

func countRows(t *testing.T, database *db.DB, table string) int {
	t.Helper()
	var n int
	if err := database.Pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	return n
}
