package finalize_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// finalizeUpload 的真实 PG 集成测试(BE-S5-01)。
// 未设置 NETDISK_TEST_DSN 时全部 skip。

type fixture struct {
	svc      *finalize.Service
	database *db.DB
	pool     *pgxpool.Pool
	storage  *storage.FS
	userID   string
	spaceID  string
	rootID   string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	// 每个用例用独立的存储根目录,避免用例间互相看到对象
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	suffix := time.Now().Format("150405.000000000")
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: "fin_" + suffix, Email: "fin_" + suffix + "@example.com",
			DisplayName: "定稿用例用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// 清理必须**彻底**(否则第二次运行会看到上一轮留下的对象行,ref_count 被叠加),
		// 但**绝不能出现全表条件**。
		//
		// 踩过的坑:这里原先有一条 `DELETE FROM file_objects WHERE ref_count <= 0`
		// 作兜底。它是**全表**条件 —— 而 `ref_count = 0` 正是所有"待清理对象"的
		// 共同特征,包括**别的包(lifecycle)正在断言的那些对象**。
		// 于是 finalize 包跑完自己的用例时,会把 lifecycle 包刚造好、正等着被
		// worker 处理的对象删掉,表现为 lifecycle 侧"读对象行失败: no rows",
		// 而两个包单独跑都必过 —— 典型的"全量跑随机挂"。
		//
		// 正确做法:先记下本用例**自己空间**引用的对象 hash(删文件前),
		// 按这份清单限定删除;没有任何无条件 DELETE。
		rows, qerr := database.Pool.Query(bg, `
SELECT DISTINCT hash_sha256 FROM files
 WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL`, u.ID)
		var own []string
		if qerr == nil {
			for rows.Next() {
				var h string
				if err := rows.Scan(&h); err == nil {
					own = append(own, h)
				}
			}
			rows.Close()
		}
		if len(own) > 0 {
			_, _ = database.Pool.Exec(bg, `DELETE FROM file_objects WHERE hash_sha256 = ANY($1::varchar[])`, own)
		}
		_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	root, err := repo.FileRepo{}.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}

	svc := &finalize.Service{
		Pool:    database.Pool,
		Storage: store,
		Spaces:  repo.SpaceRepo{},
		Files:   repo.FileRepo{},
		Name:    namepolicy.Default(),
	}
	return &fixture{
		svc: svc, database: database, pool: database.Pool, storage: store,
		userID: u.ID, spaceID: sp.ID, rootID: root.ID,
	}
}

// content 生成可复现内容的哈希。
func content(seed string, n int) ([]byte, string) {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed[i%len(seed)]
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

func (f *fixture) upload(t *testing.T, name string, data []byte) *finalize.Result {
	t.Helper()
	h := sha256.Sum256(data)
	res, err := f.svc.Finalize(context.Background(), finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: name, DeclaredSize: int64(len(data)),
		DeclaredHash: hex.EncodeToString(h[:]),
		Content:      bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("定稿 %s 失败: %v", name, err)
	}
	return res
}

// uploadOverwrite 显式声明覆盖意图的定稿(模拟 WebDAV PUT / 替换上传)。
func (f *fixture) uploadOverwrite(t *testing.T, name string, data []byte) *finalize.Result {
	t.Helper()
	h := sha256.Sum256(data)
	res, err := f.svc.Finalize(context.Background(), finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: name, DeclaredSize: int64(len(data)),
		DeclaredHash:   hex.EncodeToString(h[:]),
		AllowOverwrite: true,
		Content:        bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("覆盖定稿 %s 失败: %v", name, err)
	}
	return res
}

// ---- 主路径 ----

func TestFinalizeCreatesFileAndObject(t *testing.T) {
	f := setup(t)
	data, wantHash := content("abc", 3000)

	res := f.upload(t, "报告.pdf", data)

	if res.File == nil || res.File.Name != "报告.pdf" {
		t.Fatalf("应返回新建的文件行: %+v", res.File)
	}
	if !res.NewFile {
		t.Error("首次上传应标记 NewFile=true")
	}
	if res.File.Version != 1 {
		t.Errorf("新建文件 version 应为 1,实际 %d", res.File.Version)
	}
	// 服务端实测哈希必须等于真实内容哈希(客户端声明值不参与落库)
	if res.File.HashSHA256 != wantHash {
		t.Fatalf("落库哈希应为服务端实测值 %s,实际 %s", wantHash, res.File.HashSHA256)
	}
	if res.File.Size != int64(len(data)) {
		t.Errorf("大小应为 %d,实际 %d", len(data), res.File.Size)
	}
	if res.File.MimeType != "application/pdf" {
		t.Errorf("应按扩展名推断 mime,实际 %q", res.File.MimeType)
	}
	// 对象行:引用数 1、live
	if res.Object == nil || res.Object.RefCount != 1 || res.Object.State != model.ObjectLive {
		t.Fatalf("对象行应为 ref=1/live,实际 %+v", res.Object)
	}
	// 物理对象已落盘且大小正确(内容寻址路径)
	sz, err := f.storage.Stat(context.Background(), wantHash)
	if err != nil {
		t.Fatalf("对象应在存储中: %v", err)
	}
	if sz != int64(len(data)) {
		t.Fatalf("对象大小应为 %d,实际 %d", len(data), sz)
	}
	// etag 由生成列派生且含版本号
	if res.File.Etag == "" || res.File.Etag[:9] != "00000001-" {
		t.Errorf("etag 应为 8 位十六进制版本号开头,实际 %q", res.File.Etag)
	}
}

// 同名覆盖写:version+1、旧对象引用归零转 pending_delete(6.5 规则 4)
func TestFinalizeOverwriteReleasesOldObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first, hash1 := content("aaa", 1000)
	second, hash2 := content("bbb", 2000)

	r1 := f.upload(t, "覆盖.txt", first)
	if r1.NewFile != true {
		t.Fatal("首次应为新建")
	}
	// 覆盖写必须**显式**声明意图(默认拒绝,见 AllowOverwrite 说明)
	r2 := f.uploadOverwrite(t, "覆盖.txt", second)

	if r2.NewFile {
		t.Error("同名覆盖应标记 NewFile=false")
	}
	if r2.File.ID != r1.File.ID {
		t.Fatalf("覆盖写应更新同一行,实际 %s != %s", r2.File.ID, r1.File.ID)
	}
	if r2.File.Version != 2 {
		t.Fatalf("覆盖后 version 应为 2,实际 %d", r2.File.Version)
	}
	if r2.File.HashSHA256 != hash2 || r2.File.Size != int64(len(second)) {
		t.Fatalf("覆盖后内容指针不对: hash=%s size=%d", r2.File.HashSHA256, r2.File.Size)
	}

	// 新对象 ref=1/live
	if r2.Object.RefCount != 1 || r2.Object.State != model.ObjectLive {
		t.Fatalf("新对象应为 ref=1/live,实际 %+v", r2.Object)
	}
	// 旧对象 ref=0/pending_delete 且 delete_after 在未来(24h 延迟删除窗口)
	var oldRef int
	var oldState string
	var oldAfter *time.Time
	if err := f.pool.QueryRow(ctx,
		`SELECT ref_count, state, delete_after FROM file_objects WHERE hash_sha256=$1`, hash1).
		Scan(&oldRef, &oldState, &oldAfter); err != nil {
		t.Fatalf("读旧对象失败: %v", err)
	}
	if oldRef != 0 {
		t.Fatalf("旧对象引用数应归零,实际 %d", oldRef)
	}
	if oldState != model.ObjectPendingDelete {
		t.Fatalf("旧对象应转 pending_delete,实际 %s", oldState)
	}
	if oldAfter == nil || !oldAfter.After(time.Now()) {
		t.Fatalf("旧对象应有未来的 delete_after,实际 %v", oldAfter)
	}
	// 旧对象**仍在存储中**(延迟删除窗口内不物理删)
	if _, err := f.storage.Stat(ctx, hash1); err != nil {
		t.Fatalf("延迟删除窗口内旧对象应仍在: %v", err)
	}
}

// 不变式:ref_count = 0 ⟺ state <> 'live'(表约束 + 代码两侧都保证)
func TestObjectRefStateInvariant(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data, _ := content("inv", 100)
	f.upload(t, "a.txt", data) // 第一次:ref=1/live

	// 第二次同内容不同文件名 → 复用同一对象,ref=2/live,且**不再写对象**
	before, _ := f.storage.Stat(ctx, func() string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }())
	res := f.upload(t, "b.txt", data)
	if !res.Deduped {
		t.Error("同内容应命中已有对象(Deduped=true)")
	}
	if res.Object.RefCount != 2 {
		t.Fatalf("两个文件指向同一对象,ref 应为 2,实际 %d", res.Object.RefCount)
	}
	after, _ := f.storage.Stat(ctx, func() string { s := sha256.Sum256(data); return hex.EncodeToString(s[:]) }())
	if before != after {
		t.Fatal("复用对象后存储不应变化")
	}

	// 全表扫描不变式(约束本身也会拒绝违规写入,这里是双保险)
	var bad int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM file_objects WHERE (ref_count = 0) <> (state <> 'live')`).Scan(&bad); err != nil {
		t.Fatalf("检查不变式失败: %v", err)
	}
	if bad != 0 {
		t.Fatalf("有 %d 行违反 ref_count/state 不变式", bad)
	}
}

// ---- 配额结算 ----

// 预留后按**实际**大小结算:差额必须释放(S3-06 验收②)
func TestFinalizeSettlesReservedDifference(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	const actualLen = 512 << 10
	if _, err := f.pool.Exec(ctx,
		`UPDATE spaces SET quota_bytes=0, used_bytes=0 WHERE id=$1`, f.spaceID); err != nil {
		t.Fatalf("重置配额失败: %v", err)
	}
	data, hash := content("q2", actualLen)

	// 关键:upload 任务**声明 1MB**(创建时按 1MB 预留),而实际内容只有 512KB。
	// 结算基准必须取这个 1MB(预留账本),而不是本次调用传的声明值 ——
	// 这正是曾经写错的地方(用本次声明当基准 → 差额算成 0 → 预留永不释放)。
	const reserved = int64(1 << 20)
	var uploadID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, ticket_hash, expires_at)
		 VALUES ($1,$2,$3,$4,$5,'beef', now() + interval '24 hours') RETURNING id`,
		f.userID, f.spaceID, f.rootID, "settle.bin", reserved).Scan(&uploadID); err != nil {
		t.Fatalf("造 upload 任务失败: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, f.spaceID, reserved); err != nil {
		t.Fatalf("预占额度失败: %v", err)
	}

	res, err := f.svc.Finalize(ctx, finalize.Input{
		UploadID: uploadID, UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "settle.bin", DeclaredSize: int64(actualLen), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("定稿失败: %v", err)
	}

	var used int64
	if err := f.pool.QueryRow(ctx, `SELECT used_bytes FROM spaces WHERE id=$1`, f.spaceID).Scan(&used); err != nil {
		t.Fatalf("读 used_bytes 失败: %v", err)
	}
	if used != int64(actualLen) {
		t.Fatalf("结算后 used_bytes 应为实际 %d,实际 %d(预留 %d 未释放)",
			actualLen, used, reserved)
	}
	var state string
	var target *string
	if err := f.pool.QueryRow(ctx,
		`SELECT state, target_file_id::text FROM uploads WHERE id=$1`, uploadID).Scan(&state, &target); err != nil {
		t.Fatalf("读 upload 状态失败: %v", err)
	}
	if state != model.UploadFinalized {
		t.Fatalf("upload 应置 finalized,实际 %s", state)
	}
	if target == nil || *target != res.File.ID {
		t.Fatalf("upload 应登记目标 file_id,实际 %v", target)
	}
}

// 无预留(例如 WebDAV PUT 不走 create)时按实际占用;额度不足即拒(507)
func TestFinalizeChargesActualWhenNoReservation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx,
		`UPDATE spaces SET quota_bytes=2048, used_bytes=0 WHERE id=$1`, f.spaceID); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}
	data, hash := content("big", 4096) // 4KB > 2KB 配额
	_, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "too-big.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeQuotaExceeded {
		t.Fatalf("超出额度应 quota_exceeded,得到 %v", err)
	}
	// 事务回滚:不留 files 行
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE space_id=$1 AND name='too-big.bin'`, f.spaceID).Scan(&n)
	if n != 0 {
		t.Fatal("失败的定稿不应留下 files 行")
	}
	var used int64
	_ = f.pool.QueryRow(ctx, `SELECT used_bytes FROM spaces WHERE id=$1`, f.spaceID).Scan(&used)
	if used != 0 {
		t.Fatalf("失败的定稿不应改变 used_bytes,实际 %d", used)
	}
}

// 元数据说对象在、物理对象实际不在 → 必须**重新落盘**,不能只信数据库行。
//
// 这是实测暴露的真实缺口:上一轮清理只删了物理对象(或存储损坏/迁移丢失),
// 数据库行仍标 live。若只信行,就会返回成功、files 行也正常,
// 但用户下载永远 404 —— "元数据说在、实际不在"的悬空指针。
func TestFinalizeRepairsMissingPhysicalObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data, hash := content("dangling", 2048)

	// 第一次正常上传,制造 live 对象 + files 行
	first := f.upload(t, "dangling.bin", data)
	if first.Object == nil || first.Object.RefCount != 1 {
		t.Fatalf("前置条件不成立: %+v", first.Object)
	}
	// 模拟"物理对象丢了":直接从存储删掉,数据库行**保持不变**
	if err := f.storage.Delete(ctx, hash); err != nil {
		t.Fatalf("删除物理对象失败: %v", err)
	}
	if _, err := f.storage.Stat(ctx, hash); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("前置条件:物理对象应已不存在,实际 %v", err)
	}
	// 数据库行仍标 live(悬空状态)
	var state string
	if err := f.pool.QueryRow(ctx,
		`SELECT state FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&state); err != nil {
		t.Fatalf("读对象状态失败: %v", err)
	}
	if state != model.ObjectLive {
		t.Fatalf("前置条件:数据库行应仍为 live,实际 %s", state)
	}

	// 再传同一内容(不同文件名)→ 必须重新落盘
	res, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "repaired.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("定稿失败: %v", err)
	}
	if res.Deduped {
		t.Error("物理对象缺失时不应标记为复用(必须重新落盘)")
	}
	if _, err := f.storage.Stat(ctx, hash); err != nil {
		t.Fatalf("重新落盘后物理对象应存在: %v", err)
	}
	// 引用计数必须正确递增到 2(两个文件指向同一内容)
	if res.Object.RefCount != 2 {
		t.Fatalf("修复后引用数应为 2,实际 %d", res.Object.RefCount)
	}
}

// **直接证明对象锁在定稿的持锁窗口内真的被持有**(BE-S5-02 验收③)。
//
// 为什么必须证明"锁真的被持有"而不是只看代码:ADR-2 的连接纪律
// **写错不报错** —— 用池式拿/解锁时 `pg_advisory_unlock` 返回 false 而无人检查,
// 锁就永久挂在池连接上、同 hash 的一切引用操作被无限阻塞,且没有任何日志线索。
// 这类"看起来对但实际没锁"的实现只能靠**观测锁状态**区分。
//
// 观测时点用 `Service.AfterLock` 钩子(测试专用,nil 时零开销)。
// 不用 sleep 猜测窗口:持锁段包含一次本地文件提交 + 一个短事务,时长毫秒级,
// 靠 sleep 既脆弱又慢(第一版即因此误报)。
func TestFinalizeHoldsObjectLockDuringCommit(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("lockprobe"), 200)
	h := sha256.Sum256(data)
	hash := hex.EncodeToString(h[:])

	type probe struct {
		heldByOther bool
		lockRows    int
		probeError  error
	}
	var got probe
	f.svc.AfterLock = func(h string) {
		if h != hash {
			got.probeError = fmt.Errorf("钩子收到的 hash 不符: %s != %s", h, hash)
			return
		}
		// pg_locks 里必须能看到该 advisory 锁已被授予
		var rows int
		if err := f.pool.QueryRow(ctx, `
SELECT count(*) FROM pg_locks
 WHERE locktype = 'advisory' AND granted
   AND objid = (hashtextextended($1,0) & x'ffffffff'::bigint)`, hash).Scan(&rows); err != nil {
			got.probeError = err
			return
		}
		got.lockRows = rows
		// 非阻塞尝试取同键锁:拿不到 = 已被本次定稿持有
		var acquired bool
		if err := f.pool.QueryRow(ctx,
			`SELECT pg_try_advisory_lock(hashtextextended($1,0))`, hash).Scan(&acquired); err != nil {
			got.probeError = err
			return
		}
		if acquired {
			// 意外拿到 → 立刻释放,避免把锁挂到池连接上污染后续断言
			_, _ = f.pool.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, hash)
		}
		got.heldByOther = !acquired
	}

	res, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "lockprobe.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("定稿失败: %v", err)
	}
	if got.probeError != nil {
		t.Fatalf("观测锁状态失败: %v", got.probeError)
	}
	if !got.heldByOther || got.lockRows == 0 {
		t.Fatalf("定稿持锁期间未观测到对象锁(heldByOther=%v pg_locks=%d)—— "+
			"会话级锁未生效,ADR-2 保护窗口缺失", got.heldByOther, got.lockRows)
	}

	// 定稿结束后锁必须已释放:能立刻取到同键锁
	var canLock bool
	if err := f.pool.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1,0))`, hash).Scan(&canLock); err != nil {
		t.Fatalf("定稿后探测锁失败: %v", err)
	}
	if !canLock {
		t.Fatal("定稿结束后对象锁未释放(锁泄漏:同 hash 的一切引用操作会被无限阻塞)")
	}
	_, _ = f.pool.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, hash)

	if res.File == nil || res.File.HashSHA256 != hash {
		t.Fatal("定稿应产出指向该对象的文件行")
	}
}

// 客户端声明哈希与实际内容不符 → 以**服务端实测**为准落库(R-05 防投毒)
func TestFinalizeTrustsServerHashNotClientClaim(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data, realHash := content("real", 500)
	_, fakeHash := content("fake", 500)

	res, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "spoof.txt", DeclaredSize: 500, DeclaredHash: fakeHash,
		Content: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatalf("定稿失败: %v", err)
	}
	if res.File.HashSHA256 != realHash {
		t.Fatalf("必须落服务端实测哈希 %s,实际 %s", realHash, res.File.HashSHA256)
	}
	// 客户端声明的假哈希不得产生任何对象行
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM file_objects WHERE hash_sha256=$1`, fakeHash).Scan(&n)
	if n != 0 {
		t.Fatal("客户端声明的哈希不应产生对象行")
	}
}

// 声明大小与实际内容不符 → 400(不是 500),且不留残留
func TestFinalizeRejectsSizeMismatch(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data, hash := content("sz", 100)
	_, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "mismatch.bin", DeclaredSize: 9999, DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 400 {
		t.Fatalf("声明与实际不符应 400,得到 %v", err)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE space_id=$1 AND name='mismatch.bin'`, f.spaceID).Scan(&n)
	if n != 0 {
		t.Fatal("失败定稿不应留下 files 行")
	}
}

// 定稿时 namepolicy 复检:创建时合法但定稿时非法的名字必须被拒(6.7)
func TestFinalizeRevalidatesName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data, hash := content("nm", 10)
	_, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "bad:name.txt", DeclaredSize: 10, DeclaredHash: hash,
		Content: bytes.NewReader(data),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeInvalidName {
		t.Fatalf("非法名应 invalid_name,得到 %v", err)
	}
	if ae.Details["rule"] == nil {
		t.Error("应带 rule 明细供客户端引导用户")
	}
}

// 无内容流且存储里没有该对象 → 明确拒绝(秒传误判不能落空文件)
//
// 这里显式声明 AllowTrustedReuse:本用例要验的是"**信任的**调用方在对象缺失时
// 也必须被拒",与"客户端没有证明"是两条不同的路径(后者见 fastupload_test)。
func TestFinalizeRejectsMissingObjectWithoutContent(t *testing.T) {
	f := setup(t)
	_, hash := content("absent", 100)
	_, err := f.svc.Finalize(context.Background(), finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "no-object.bin", DeclaredSize: 100, DeclaredHash: hash,
		Content:           nil,
		AllowTrustedReuse: true,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("无对象无内容应 fast_upload_denied,得到 %v", err)
	}
}

// **R-05 防投毒的最后一道闸**:没有内容、没有持物证明、也没有内部放行 → 必须拒绝。
//
// 这条一旦放行,"秒传"就退化成"报一个 hash 领走别人的文件" ——
// 而哈希不是秘密(分享链接、日志、公开文件里到处都有)。
func TestFinalizeWithoutContentOrProofIsDenied(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 先造一个**真实存在**的对象,确保拒绝的原因不是"对象不存在"
	data, hash := content("exists-for-proof", 4096)
	if _, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "victim.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: bytes.NewReader(data),
	}); err != nil {
		t.Fatalf("前置定稿失败: %v", err)
	}
	// 现在冒充"我也有这个 hash":不给内容、不给 nonce
	_, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "steal.bin", DeclaredSize: int64(len(data)), DeclaredHash: hash,
		Content: nil,
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("无内容无证明必须 fast_upload_denied,得到 %v", err)
	}
	// 且**不能**落下任何行(否则等于"报 hash 就建行,只是没内容")
	var n int
	if qerr := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE name = 'steal.bin'`).Scan(&n); qerr != nil {
		t.Fatalf("查行失败: %v", qerr)
	}
	if n != 0 {
		t.Fatalf("被拒的请求不应留下文件行,实际 %d 行", n)
	}
}

// 给了 nonce 但服务端未装配证明校验器 → 拒绝(而不是静默按信任处理)
//
// 这条防的是"部署时忘了装配 Fast":那种情况下 Content==nil 的请求必须失败,
// 而不是悄悄退化成信任客户端。
func TestFinalizeWithNonceButNoVerifierIsDenied(t *testing.T) {
	f := setup(t)
	data, hash := content("no-verifier", 4096)
	_ = data
	_, err := f.svc.Finalize(context.Background(), finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "nv.bin", DeclaredSize: 4096, DeclaredHash: hash,
		Content: nil, Nonce: "deadbeef", SampleSHA256: []string{"x"},
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeFastUploadDenied {
		t.Fatalf("未装配校验器应 fast_upload_denied,得到 %v", err)
	}
}

// 覆盖目录 → 409(不能把目录变成文件)
func TestFinalizeRejectsOverwritingDirectory(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// Go 不允许在 if 初始化处对复合字面量的方法调用做多值接收,先取变量
	_, derr := repo.FileRepo{}.CreateDir(ctx, f.pool, repo.CreateDirInput{
		SpaceID: f.spaceID, ParentID: f.rootID, OwnerID: f.userID, Name: "adir", Depth: 1,
	})
	if derr != nil {
		t.Fatalf("建目录失败: %v", derr)
	}
	data, hash := content("d", 10)
	_, err := f.svc.Finalize(ctx, finalize.Input{
		UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
		Name: "adir", DeclaredSize: 10, DeclaredHash: hash, Content: bytes.NewReader(data),
	})
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("覆盖目录应 409,得到 %v", err)
	}
}

// ---- 并发 ----

// 同内容并发定稿同名不同位置:对象引用计数必须精确等于文件数(不能少算)。
//
// 这是"复活语句漏 ref_count+1"那类 bug 的回归用例:漏了就会得到
// "文件指向对象、引用数是 0",清理 worker 随后删掉对象 → 悬空指针。
func TestConcurrentFinalizeRefCountExact(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data, hash := content("conc", 4096)

	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := f.svc.Finalize(ctx, finalize.Input{
				UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
				Name: fmt.Sprintf("c%d.bin", i), DeclaredSize: int64(len(data)),
				DeclaredHash: hash, Content: bytes.NewReader(data),
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发定稿 #%d 失败: %v", i, err)
		}
	}

	var ref int
	var state string
	if err := f.pool.QueryRow(ctx,
		`SELECT ref_count, state FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&ref, &state); err != nil {
		t.Fatalf("读对象失败: %v", err)
	}
	if ref != n {
		t.Fatalf("%d 个文件指向同一对象时 ref_count 应为 %d,实际 %d(漏加或多加)", n, n, ref)
	}
	if state != model.ObjectLive {
		t.Fatalf("对象应保持 live,实际 %s", state)
	}
	var files int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE space_id=$1 AND hash_sha256=$2`, f.spaceID, hash).Scan(&files)
	if files != n {
		t.Fatalf("应有 %d 个文件行,实际 %d", n, files)
	}
	// 存储里只有一份物理对象
	if _, err := f.storage.Stat(ctx, hash); err != nil {
		t.Fatalf("物理对象应存在: %v", err)
	}
}

// 同名并发定稿:只允许一个成功,其余 409(绝不静默覆盖)
func TestConcurrentFinalizeSameNameConflicts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dataA, hashA := content("A", 800)
	dataB, hashB := content("B", 800)

	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, in := range []struct {
		data []byte
		hash string
	}{{dataA, hashA}, {dataB, hashB}} {
		wg.Add(1)
		go func(i int, data []byte, hash string) {
			defer wg.Done()
			_, err := f.svc.Finalize(ctx, finalize.Input{
				UserID: f.userID, SpaceID: f.spaceID, ParentID: f.rootID,
				Name: "race.txt", DeclaredSize: int64(len(data)), DeclaredHash: hash,
				Content: bytes.NewReader(data),
			})
			results[i] = err
		}(i, in.data, in.hash)
	}
	wg.Wait()

	okCount, conflictCount := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			okCount++
		default:
			var ae *apierr.Error
			if errors.As(err, &ae) && ae.Status == 409 {
				conflictCount++
				continue
			}
			t.Fatalf("同名校验应返回 409,实际 %v", err)
		}
	}
	if okCount != 1 || conflictCount != 1 {
		t.Fatalf("同名并发应恰好 1 成功 1 冲突,实际 ok=%d conflict=%d", okCount, conflictCount)
	}
}
