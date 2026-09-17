// Command devdb 是本机开发/测试库的维护小工具。
//
// 为什么需要它:集成测试共用 `netdisk_test` 库,而部门闭包表的重建是**全库**操作
// (TRUNCATE + 全量 INSERT)。只要库里残留一棵有病的数据(最常见是历史用例
// 崩溃留下的半截树),重建后的自洽性校验就会如实报错 —— 这不是代码 bug,
// 而是共用库缺少一个"一键回到干净状态"的手段。
//
// 用法(DSN 从环境变量 NETDISK_TEST_DSN 读,或用 -dsn 显式给):
//
//	go run ./cmd/devdb -list                     # 看库里有什么
//	go run ./cmd/devdb -purge-depts              # 删掉全部部门(自底向上)
//	go run ./cmd/devdb -purge-test-users         # 删掉测试用户(前缀匹配)
//	go run ./cmd/devdb -reset                    # 上述全做,并重建闭包表
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := flag.String("dsn", "", "测试库 DSN(默认取 NETDISK_TEST_DSN)")
	purgeDepts := flag.Bool("purge-depts", false, "删除全部部门(自底向上,绕过 ON DELETE RESTRICT)")
	purgeUsersFlag := flag.Bool("purge-test-users", false, "删除测试用户(按用户名前缀)")
	userPrefix := flag.String("user-prefix", "", "用户名前缀(默认空 = 全部用户)")
	list := flag.Bool("list", false, "列出库中各表行数")
	reset := flag.Bool("reset", false, "等价于 -purge-test-users -purge-depts")
	force := flag.Bool("force", false, "允许前缀为空(清理全部用户)——防止误清非测试库")
	flag.Parse()

	target := *dsn
	if target == "" {
		target = os.Getenv("NETDISK_TEST_DSN")
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "缺少 DSN:请设置 NETDISK_TEST_DSN 或用 -dsn 指定")
		os.Exit(2)
	}
	if *reset {
		*purgeUsersFlag, *purgeDepts = true, true
	}
	if !*purgeDepts && !*purgeUsersFlag && !*list {
		flag.Usage()
		os.Exit(2)
	}
	// 前缀为空 = 删全部用户,是个危险操作:要求显式 -force,
	// 并把库名打出来让人一眼看到自己连的是哪个库。
	if *purgeUsersFlag && *userPrefix == "" && !*force {
		fmt.Fprintf(os.Stderr,
			"拒绝执行:前缀为空会删除**全部用户**。确认目标是测试库后加 -force。\n目标库: %s\n",
			redactDSN(target))
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, target)
	if err != nil {
		fmt.Fprintf(os.Stderr, "连接失败: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if *purgeUsersFlag {
		n, err := purgeUsers(ctx, pool, *userPrefix)
		if err != nil {
			fmt.Fprintf(os.Stderr, "删测试用户失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已删除测试用户 %d 个(前缀 %s)\n", n, *userPrefix)
	}

	if *purgeDepts {
		n, err := purgeDepartments(ctx, pool)
		if err != nil {
			fmt.Fprintf(os.Stderr, "删部门失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("已删除部门 %d 个(含闭包表级联)\n", n)
	}

	if *list {
		if err := listCounts(ctx, pool); err != nil {
			fmt.Fprintf(os.Stderr, "统计失败: %v\n", err)
			os.Exit(1)
		}
	}
}

// purgeUsers 删除匹配前缀的用户,并**按外键链自底向上**清掉它们的全部数据。
//
// 为什么需要这一长串:`users` 被 10 张表以 ON DELETE RESTRICT 引用,
// 直接 DELETE 必然撞外键。集成测试反复运行会累积几百个测试用户,
// 而它们各自带走一个个人空间 + 根目录 —— 不清的话测试库会越来越慢,
// 且"按用户名前缀清理"的用例会越跑越多。
//
// 顺序即依赖顺序(被引用者最后删),全部在一个事务里,失败整体回滚。
func purgeUsers(ctx context.Context, pool *pgxpool.Pool, prefix string) (int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ids := `SELECT id FROM users WHERE username LIKE $1`
	// 待清理的个人空间(个人空间与用户一一对应)
	spaces := `SELECT id FROM spaces WHERE owner_id IN (` + ids + `)`
	// 待清理的文件:测试用户个人空间内的全部文件(含 owner 是其它用户的极少数情况)
	files := `SELECT id FROM files WHERE space_id IN (` + spaces + `)`

	// 步骤顺序即依赖顺序:先拆"引用别人"的关联,再删被引用的实体。
	steps := []struct {
		what string
		sql  string
	}{
		{"上传任务", `DELETE FROM uploads WHERE user_id IN (` + ids + `)`},
		{"文件锁", `DELETE FROM file_locks WHERE file_id IN (` + files + `)`},
		{"分享链接", `DELETE FROM shares WHERE file_id IN (` + files + `)`},
		{"同步游标", `DELETE FROM sync_cursors WHERE space_id IN (` + spaces + `)`},
		{"空间成员", `DELETE FROM space_members WHERE space_id IN (` + spaces + `) OR user_id IN (` + ids + `)`},
		{"文件元数据", `DELETE FROM files WHERE space_id IN (` + spaces + `)`},
		{"个人空间", `DELETE FROM spaces WHERE id IN (` + spaces + `)`},
		{"部门成员关联", `DELETE FROM user_departments WHERE user_id IN (` + ids + `)`},
		{"SSO 绑定", `DELETE FROM user_sso_bindings WHERE user_id IN (` + ids + `)`},
		{"refresh 令牌", `DELETE FROM refresh_tokens WHERE user_id IN (` + ids + `)`},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, st.sql, prefix+"%"); err != nil {
			return 0, fmt.Errorf("清理%s失败: %w", st.what, err)
		}
	}

	// file_objects 是内容寻址的共享行:ref_count 归零的行才能删,
	// 且必须**重算**而不是减一 —— 否则误删一个仍被引用的对象会造成悬空指针。
	if _, err := tx.Exec(ctx, `
DELETE FROM file_objects fo
 WHERE fo.ref_count = 0
    OR NOT EXISTS (SELECT 1 FROM files f WHERE f.hash_sha256 = fo.hash_sha256)`); err != nil {
		return 0, fmt.Errorf("清理对象记录失败: %w", err)
	}

	// 组:先删成员,再删"只属于待删用户"的组(owner 是待删用户)
	if _, err := tx.Exec(ctx, `DELETE FROM group_members WHERE user_id IN (`+ids+`)`, prefix+"%"); err != nil {
		return 0, fmt.Errorf("清理组成员失败: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM groups WHERE owner_id IN (`+ids+`)`, prefix+"%"); err != nil {
		return 0, fmt.Errorf("清理组失败: %w", err)
	}

	tag, err := tx.Exec(ctx, `DELETE FROM users WHERE username LIKE $1`, prefix+"%")
	if err != nil {
		return 0, fmt.Errorf("删除用户失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// purgeDepartments 自底向上删除全部部门。
//
// 为什么不能直接 `DELETE FROM departments`:parent_id 是 ON DELETE RESTRICT,
// 一次删全部会撞外键。按"后代数降序"逐层删即可(叶子先走)。
// 用递归 CTE 算每个节点的后代数,而不是靠闭包表 —— 闭包表可能本身就是坏的
// (这正是需要清理的场景),不能拿可能坏的数据当依据。
func purgeDepartments(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
WITH RECURSIVE descend(depth, id) AS (
    SELECT 0, src.id
      FROM departments src
     WHERE NOT EXISTS (SELECT 1 FROM departments c WHERE c.parent_id = src.id)
    UNION ALL
    SELECT d.depth + 1, p.id
      FROM descend d
      JOIN departments p ON p.id = (SELECT parent_id FROM departments WHERE id = d.id)
     WHERE p.id IS NOT NULL
)
DELETE FROM departments WHERE id IN (SELECT id FROM descend)`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func listCounts(ctx context.Context, pool *pgxpool.Pool) error {
	tables := []string{"users", "departments", "department_closure", "user_departments",
		"groups", "spaces", "files", "file_objects", "uploads"}
	for _, tbl := range tables {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+tbl).Scan(&n); err != nil {
			return fmt.Errorf("统计 %s 失败: %w", tbl, err)
		}
		fmt.Printf("%-22s %d\n", tbl, n)
	}
	return nil
}

// redactDSN 去掉 DSN 里的口令后再打印(避免口令进日志)。
func redactDSN(dsn string) string {
	const key = "password="
	i := strings.Index(dsn, key)
	if i < 0 {
		return dsn
	}
	head := dsn[:i+len(key)]
	rest := dsn[i+len(key):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return head + "***" + rest[j:]
	}
	return head + "***"
}
