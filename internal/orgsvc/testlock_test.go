package orgsvc_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 部门树用例的互斥锁。
//
// 为什么需要它:闭包表重建是**全库**操作(DELETE 全表 + 全量 INSERT),
// 而集成测试包之间是并行执行的(api 包也有一组部门接口用例)。
// 两个包同时全库重建会互相删掉对方刚建好的树,表现为随机的
// "闭包行数不对/子树少了节点" —— 这类失败只在跑全量测试时出现,
// 单独跑某个包永远复现不了,极难定位。
//
// 用 PG 的**会话级 advisory lock**串行化:同一个测试库里,任何时刻只有一个
// 包能操作部门树。锁随连接关闭自动释放,测试崩溃也不会留下死锁。
const deptTestLockKey = 0x6E657464 // "netd"

// lockDeptTests 获取部门树测试的全局互斥锁,并在测试结束时释放。
func lockDeptTests(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("获取测试锁连接失败: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, deptTestLockKey); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("获取测试锁失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = conn.Exec(bg, `SELECT pg_advisory_unlock($1)`, deptTestLockKey)
		_ = conn.Close(bg)
	})
}

// snapshotCounts 返回各部门相关表的行数,便于 set -v 日志定位污染。
func snapshotCounts(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	var d, c, ud int
	out := ""
	for _, probe := range []struct {
		name string
		sql  string
		dst  *int
	}{
		{"departments", `SELECT count(*) FROM departments`, &d},
		{"closure", `SELECT count(*) FROM department_closure`, &c},
		{"user_departments", `SELECT count(*) FROM user_departments`, &ud},
	} {
		if err := pool.QueryRow(ctx, probe.sql).Scan(probe.dst); err != nil {
			return out + fmt.Sprintf("%s=? ", probe.name)
		}
		out += fmt.Sprintf("%s=%d ", probe.name, *probe.dst)
	}
	return out
}

// testDSN 读测试库 DSN(未设置时返回空串)。
func testDSN() string { return os.Getenv("NETDISK_TEST_DSN") }
