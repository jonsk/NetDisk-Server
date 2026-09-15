package dirops_test

// 目录级异步队列的 **SKIP LOCKED 分支**断言(BE-S7-02 / 6.11)。
//
// 为什么单独写它:`internal/dirops` 此前**一个测试文件都没有**,而 `Claim` 里的
// `FOR UPDATE SKIP LOCKED` 是"多 worker 不互相等待"的唯一保证 ——
// 把 SKIP LOCKED 删掉,整个仓库都不会变红(TS-02 记录的缺口类型:
// 没有断言的纪律 = 没有纪律)。

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

type qFixture struct {
	db     *db.DB
	pool   *pgxpool.Pool
	q      *dirops.Queue
	userID string
	spaceID string
}

func setupQueue(t *testing.T) *qFixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()

	// **必须隔离 schema**(实测踩到):`Claim` 是**全表**扫描(生产语义就是如此),
	// 于是"同一张 dir_op_tasks 表"上的用例会互相抢行 ——
	//   · 我插入的 pending 任务会被 filesvc 的目录操作 worker 领走;
	//   · 它们插入的任务会被我的 Claim 领走(我的断言就拿到"不该拿的那条")。
	// 表现是 `go test ./...`(包并行)下随机假红,而单跑某个包永远绿。
	// 因此本文件在**独立 schema** 里建表:两边互相看不见,而 SQL 语义一字不改。
	admin, err := db.OpenWithDSN(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	schema := "dirops_claim_" + time.Now().Format("150405000000")
	if _, err := admin.Pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("建隔离 schema 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	// options 是 libpq 关键字:让**该连接**的 search_path 指向隔离 schema
	isoDSN := dsn + " options='-c search_path=" + schema + "'"
	database, err := db.OpenWithDSN(ctx, isoDSN, 8)
	if err != nil {
		t.Fatalf("连接隔离 schema 失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("隔离 schema 迁移失败: %v", err)
	}

	suffix := time.Now().Format("150405.000000")
	var userID string
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		u, cerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: "dq_" + suffix, Email: "dq_" + suffix + "@example.com",
			DisplayName: "dir-queue", Role: model.RoleUser,
		})
		if cerr != nil {
			return cerr
		}
		userID = u.ID
		return nil
	}); err != nil {
		t.Fatalf("造用户失败: %v", err)
	}
	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, database.Pool, userID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	return &qFixture{
		db: database, pool: database.Pool,
		// 与生产装配一致:不是直接把 *db.DB 塞进去,而是走 db.AsQuerier 适配
		// (dirops.DB 要的是"读走池、写走事务"的窄接口)
		q:       &dirops.Queue{DB: db.AsQuerier(database)},
		userID:  userID,
		spaceID: sp.ID,
	}
}

// mkPending 直接插一条 pending 任务(不走 Enqueue:这里只需要"队列里有活")。
func (f *qFixture) mkPending(t *testing.T, createdAt time.Time) string {
	t.Helper()
	var id string
	// file_id 没有外键(任务完成后该行可能已被删除),所以这里用随机 uuid 即可;
	// 注意 dir_op_tasks 上有"同一 file_id 只允许一个在飞任务"的唯一索引 → 每条都要不同。
	if err := f.pool.QueryRow(context.Background(), `
INSERT INTO dir_op_tasks (kind, space_id, file_id, user_id, created_at, updated_at)
VALUES ('delete', $1, gen_random_uuid(), $2, $3, $3)
RETURNING id`, f.spaceID, f.userID, createdAt).Scan(&id); err != nil {
		t.Fatalf("插入 pending 任务失败: %v", err)
	}
	return id
}

// Claim 必须**跳过**被别的会话锁住的行,而不是等在那里。
//
// 判据是"不阻塞 + 领到的是另一条":后台 worker 遇到别人正在处理的任务要毫秒级跳过、
// 下一轮再来;堵住会让整批目录操作停摆(用户看到的是"删除一直没完成",且没有任何报错)。
func TestClaimSkipsRowLockedByAnotherSession(t *testing.T) {
	f := setupQueue(t)
	ctx := context.Background()
	// created_at 升序 = Claim 的取行顺序:让**被锁住的那条排在前面**,
	// 这样"不跳过"的实现会先撞上它并阻塞(SKIP LOCKED 才有区分度)。
	locked := f.mkPending(t, time.Now().Add(-2*time.Hour))
	other := f.mkPending(t, time.Now().Add(-1*time.Hour))

	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM dir_op_tasks WHERE id=$1 FOR UPDATE`, locked); err != nil {
		t.Fatalf("锁任务行失败: %v", err)
	}
	// 先证明锁真的持有(否则用例可能空过)
	{
		c2, aerr := f.pool.Acquire(ctx)
		if aerr != nil {
			t.Fatalf("取连接失败: %v", aerr)
		}
		_, lerr := c2.Exec(ctx, `SELECT id FROM dir_op_tasks WHERE id=$1 FOR UPDATE NOWAIT`, locked)
		c2.Release()
		var pgErr *pgconn.PgError
		if !errors.As(lerr, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("预期该行已被锁住(55P03),实际 err=%v", lerr)
		}
	}

	type outcome struct {
		task *dirops.Task
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		tk, cerr := f.q.Claim(ctx, time.Minute)
		done <- outcome{tk, cerr}
	}()
	var got outcome
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Claim 被锁住的行**堵住**了(SQL 的 SKIP LOCKED 失效):应当毫秒级跳过")
	}
	if got.err != nil {
		t.Fatalf("Claim 失败: %v", got.err)
	}
	if got.task == nil {
		t.Fatal("应当领到未被锁住的那一条,实际 nil")
	}
	if got.task.ID != other {
		t.Fatalf("应跳过被锁住的 %s,实际领到 %s", locked, got.task.ID)
	}
	var state string
	if err := f.pool.QueryRow(ctx, `SELECT state FROM dir_op_tasks WHERE id=$1`, locked).Scan(&state); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if state != "pending" {
		t.Fatalf("被别的会话锁住的任务不该被动,实际 state=%s", state)
	}

	// 放锁后必须能领到它 —— 证明上一步是"跳过",而不是"永远轮不到"
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	tk, err := f.q.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatalf("放锁后 Claim 失败: %v", err)
	}
	if tk == nil || tk.ID != locked {
		t.Fatalf("放锁后应领到被跳过的那条 %s,实际 %+v", locked, tk)
	}
}
