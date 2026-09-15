package uploadsvc_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 暂存区回收(BE-S5-04)的集成测试。
//
// 这组用例的重点全是"**什么时候不该删**":清理任务最容易造成的伤害不是漏删
// (磁盘慢慢满,有监控能发现),而是误删一个正在续传的暂存文件 ——
// 用户传了 20 小时的文件从头再来,而且服务端没有任何报错。

// ---- 小工具 ----

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func osChtimes(path string, t time.Time) error { return os.Chtimes(path, t, t) }

func nameN(i int) string { return fmt.Sprintf("stage-%d.bin", i) }

// failingStager 包住真实暂存实现,让指定 upload_id 的删除"总是失败"。
//
// 用它验证"重试 3 次后放弃"这条纪律:真实文件系统很难制造稳定的删除失败
// (权限位在 Windows 上不生效、占用需要另一进程配合),而这条纪律恰恰是
// "不能无限重试、也不能静默放弃"的分界。
type failingStager struct {
	inner    *storage.FS
	failFor  string
	notFound bool
	attempts int
}

func (f *failingStager) ListStageFiles() ([]storage.StageFile, error) {
	return f.inner.ListStageFiles()
}

func (f *failingStager) StageRemove(uploadID string) error {
	if uploadID == f.failFor {
		f.attempts++
		if f.notFound {
			return storage.ErrNotFound
		}
		return errors.New("模拟删除失败(文件被占用)")
	}
	return f.inner.StageRemove(uploadID)
}

// stageEnv 是一套带真实存储目录 + 真实 uploads 行的清理环境。
type stageEnv struct {
	cleaner *uploadsvc.StageCleaner
	store   *storage.FS
	f       *fixture
}

func setupStage(t *testing.T) *stageEnv {
	t.Helper()
	f := setup(t)
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	cl := &uploadsvc.StageCleaner{
		Stager: store, Uploads: repo.UploadRepo{}, Spaces: repo.SpaceRepo{},
		DB: f.svc.DB, Log: nil,
	}
	return &stageEnv{cleaner: cl, store: store, f: f}
}

// writeStage 写一个暂存文件并把 mtime 推到指定年龄。
func (e *stageEnv) writeStage(t *testing.T, uploadID string, size int, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	data := make([]byte, size)
	if _, err := e.store.StageAppend(ctx, uploadID, 0, bytesReader(data)); err != nil {
		t.Fatalf("写暂存失败: %v", err)
	}
	if age > 0 {
		path, err := e.store.StagePath(uploadID)
		if err != nil {
			t.Fatalf("取暂存路径失败: %v", err)
		}
		old := time.Now().Add(-age)
		if err := osChtimes(path, old); err != nil {
			t.Fatalf("改 mtime 失败: %v", err)
		}
	}
}

func (e *stageEnv) stageExists(t *testing.T, uploadID string) bool {
	t.Helper()
	// 注意:不能用 StageSize 判存在 —— 它对"文件不存在"返回 (0, nil)
	// (TUS 语义下"没有分片"就是 0 字节,不是错误)。用它的返回值判存在
	// 会把"已删除"误判成"还在",于是本组用例集体假失败。
	path, err := e.store.StagePath(uploadID)
	if err != nil {
		t.Fatalf("取暂存路径失败: %v", err)
	}
	_, serr := os.Stat(path)
	if serr == nil {
		return true
	}
	if os.IsNotExist(serr) {
		return false
	}
	t.Fatalf("stat 暂存失败: %v", serr)
	return false
}

// mkReservedUpload 建一条 reserved 上传任务(可用 ttl 控制过期)。
func (e *stageEnv) mkReservedUpload(t *testing.T, name string, size int64, ttl time.Duration) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := e.f.database.InTx(ctx, func(tx pgx.Tx) error {
		up, err := repo.UploadRepo{}.Create(ctx, tx, repo.CreateUploadInput{
			UserID: e.f.userID, SpaceID: e.f.spaceID, ParentID: e.f.rootID,
			Name: name, DeclaredSize: size, TicketHash: "0",
			ExpiresAt: time.Now().Add(ttl),
		})
		if err != nil {
			return err
		}
		id = up.ID
		return nil
	}); err != nil {
		t.Fatalf("造上传任务失败: %v", err)
	}
	return id
}

// **不该删**:任务仍活跃(reserved 且未过期)时,再老的暂存文件也要留
//
// 场景:用户传了一个 30GB 文件,传了 25 小时(超过 24h TTL)还没传完 ——
// 按"只看年龄"清理就会把 25 小时的进度删掉。
func TestStageCleanerKeepsActiveUploadEvenIfOld(t *testing.T) {
	e := setupStage(t)
	up := e.mkReservedUpload(t, "slow.bin", 1024, 24*time.Hour)
	e.writeStage(t, up, 512, 48*time.Hour)

	res := e.cleaner.RunOnce(context.Background())
	if !e.stageExists(t, up) {
		t.Fatalf("活跃任务的暂存文件被误删(用户进度丢失): %+v", res)
	}
	if res.KeptActive != 1 {
		t.Fatalf("应记为 KeptActive=1,实际 %+v", res)
	}
}

// **该删**:任务已结束(finalized)→ 暂存文件清掉
func TestStageCleanerRemovesStageOfFinishedUpload(t *testing.T) {
	e := setupStage(t)
	up := e.mkReservedUpload(t, "done.bin", 1024, 24*time.Hour)
	e.writeStage(t, up, 700, 48*time.Hour)
	ctx := context.Background()
	if err := e.f.database.InTx(ctx, func(tx pgx.Tx) error {
		_, err := repo.UploadRepo{}.Finalize(ctx, tx, up, "h", 1024, e.f.rootID)
		return err
	}); err != nil {
		// Finalize 要写 target_file_id(外键到 files),用 root 目录行即可满足
		t.Fatalf("置 finalized 失败: %v", err)
	}

	res := e.cleaner.RunOnce(ctx)
	if e.stageExists(t, up) {
		t.Fatalf("已定稿任务的暂存文件应被清理: %+v", res)
	}
	if res.RemovedStages != 1 || res.FreedBytes != 700 {
		t.Fatalf("应清 1 个文件 700 字节,实际 %+v", res)
	}
}

// **该删**:孤儿暂存文件(没有对应任务行)→ 清掉
func TestStageCleanerRemovesOrphanStage(t *testing.T) {
	e := setupStage(t)
	// 合法 uuid 形态但库里没有这条任务
	orphan := "01a09000-0000-7000-8000-00000000beef"
	e.writeStage(t, orphan, 300, 48*time.Hour)

	res := e.cleaner.RunOnce(context.Background())
	if e.stageExists(t, orphan) {
		t.Fatalf("孤儿暂存文件应被清理: %+v", res)
	}
}

// **不该删**:还没到清理窗口(即使任务是孤儿)
func TestStageCleanerKeepsYoungStage(t *testing.T) {
	e := setupStage(t)
	orphan := "01a09000-0000-7000-8000-00000000beef"
	e.writeStage(t, orphan, 100, 0) // 刚写

	res := e.cleaner.RunOnce(context.Background())
	if !e.stageExists(t, orphan) {
		t.Fatalf("未到清理窗口的文件不该被删: %+v", res)
	}
	if res.KeptYoung != 1 {
		t.Fatalf("应记为 KeptYoung=1,实际 %+v", res)
	}
}

// **过期预留回收**:额度回退 + 暂存清理,且统计正确
func TestStageCleanerReclaimsExpiredReservationAndQuota(t *testing.T) {
	e := setupStage(t)
	// 让空间先占上额度(模拟建任务时预留)
	const size = 4096
	up := e.mkReservedUpload(t, "expired.bin", size, -time.Minute) // 已过期
	if _, err := e.f.database.Pool.Exec(context.Background(),
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, e.f.spaceID, size); err != nil {
		t.Fatalf("设置用量失败: %v", err)
	}
	e.writeStage(t, up, size, time.Hour)

	res := e.cleaner.RunOnce(context.Background())
	if res.ReclaimedReservations != 1 {
		t.Fatalf("应回收 1 条过期预留,实际 %+v", res)
	}
	if res.ReclaimedBytes != size {
		t.Fatalf("应回退 %d 字节,实际 %d", size, res.ReclaimedBytes)
	}
	var used int64
	if err := e.f.database.Pool.QueryRow(context.Background(),
		`SELECT used_bytes FROM spaces WHERE id=$1`, e.f.spaceID).Scan(&used); err != nil {
		t.Fatalf("读用量失败: %v", err)
	}
	if used != 0 {
		t.Fatalf("配额应回退为 0,实际 %d", used)
	}
	if e.stageExists(t, up) {
		t.Fatal("过期任务的暂存文件应一并清理")
	}
}

// **SKIP LOCKED 分支必须有断言**(TS-02 记下的缺口):"被另一个会话锁住的行"这条
// 分支以前**无人断言** —— 把 SQL 里的 `SKIP LOCKED` 删掉,既有用例照样全绿
// (它们从不并发持有行锁)。实测反向验证见本函数末尾注释。
//
// 判据是**不阻塞**,不是"结果对不对":回收是后台兜底任务,遇到别人正在处理的行必须
// **跳过**(毫秒级返回,下一轮再来)。堵在那里会让整批回收停摆,而现象只是
// "磁盘迟迟不降" —— 没有任何报错。
func TestStageCleanerSkipsRowLockedByAnotherSession(t *testing.T) {
	e := setupStage(t)
	ctx := context.Background()
	const size = 4096
	locked := e.mkReservedUpload(t, "locked.bin", size, -time.Minute)
	e.mkReservedUpload(t, "free.bin", size, -time.Minute) // 保证本轮**有**可认领的行

	// 在**另一条连接**上开事务把这行锁住(不提交):模拟"另一个 worker 正在处理它"
	conn, err := e.f.database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM uploads WHERE id=$1 FOR UPDATE`, locked); err != nil {
		t.Fatalf("锁行失败: %v", err)
	}

	// 先证明锁**真的**持有:第三条连接 NOWAIT 取同一行必须拿到 55P03(lock_not_available)。
	// 少了这一步,整条用例可能因为"锁根本没生效"而空过(那正是这次要补的断言类型)。
	{
		c2, aerr := e.f.database.Pool.Acquire(ctx)
		if aerr != nil {
			t.Fatalf("取连接失败: %v", aerr)
		}
		_, lerr := c2.Exec(ctx, `SELECT id FROM uploads WHERE id=$1 FOR UPDATE NOWAIT`, locked)
		c2.Release()
		var pgErr *pgconn.PgError
		if !errors.As(lerr, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("预期该行已被锁住(55P03 lock_not_available),实际 err=%v", lerr)
		}
	}

	// 回收必须**跳过**被锁住的行:有界等待,把"阻塞"变成可读的失败而不是用例挂住
	done := make(chan uploadsvc.StageCleanResult, 1)
	go func() { done <- e.cleaner.RunOnce(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("回收被锁住的行时**堵住**了(SQL 的 SKIP LOCKED 失效):应当毫秒级跳过,而不是等锁")
	}
	// 断言**只看自己那两行**,不看全局计数:`ClaimExpiredReleases` 是全表扫描,
	// 并行跑的其它包也可能有到期行(或被别的 cleaner 领走)——拿"本轮回收了 N 条"
	// 做等值断言会在包并行时假红(这类干扰在 dirops 用例上已经真实踩到过)。
	var state string
	if err := e.f.database.Pool.QueryRow(ctx,
		`SELECT state FROM uploads WHERE id=$1`, locked).Scan(&state); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if state != "reserved" {
		t.Fatalf("被别的会话锁住的行不该被认领,实际 state=%s", state)
	}

	// 放锁后的下一轮**必须**认领到它 —— 证明上一步是"跳过",而不是"永远轮不到"。
	// 用有界轮询而不是"再跑一次就必须是它":并行用例可能同时也在回收,
	// 只要**最终**被认领即可(行状态而非计数才是这里的判据)。
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = e.cleaner.RunOnce(ctx)
		var st string
		if qerr := e.f.database.Pool.QueryRow(ctx,
			`SELECT state FROM uploads WHERE id=$1`, locked).Scan(&st); qerr != nil {
			t.Fatalf("读状态失败: %v", qerr)
		}
		if st != "reserved" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("放锁后仍未被回收:说明它被永久跳过(而不是'下一轮再来')")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// **配额绝不重复回退**:同一批跑两轮,第二轮不应再回退一次
//
// 这是"先读一批再逐个 Release"最容易出的错:重复回退会让 used_bytes 被减两次,
// 用户凭空多出容量,而且没有任何报错。
func TestStageCleanerDoesNotDoubleRefundQuota(t *testing.T) {
	e := setupStage(t)
	const size = 8192
	up := e.mkReservedUpload(t, "double.bin", size, -time.Minute)
	if _, err := e.f.database.Pool.Exec(context.Background(),
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, e.f.spaceID, size); err != nil {
		t.Fatalf("设置用量失败: %v", err)
	}
	ctx := context.Background()

	first := e.cleaner.RunOnce(ctx)
	second := e.cleaner.RunOnce(ctx)
	if first.ReclaimedReservations != 1 {
		t.Fatalf("第一轮应回收 1 条,实际 %+v", first)
	}
	if second.ReclaimedReservations != 0 {
		t.Fatalf("第二轮不该再回退(否则额度被重复退回): %+v", second)
	}
	var used int64
	_ = e.f.database.Pool.QueryRow(ctx,
		`SELECT used_bytes FROM spaces WHERE id=$1`, e.f.spaceID).Scan(&used)
	if used != 0 {
		t.Fatalf("用量应为 0(只回退一次),实际 %d", used)
	}
	_ = up
}

// **并发认领不相交**:两个清理器同时跑,同一批过期预留只能被一方拿走
func TestStageCleanerConcurrentClaimIsDisjoint(t *testing.T) {
	e := setupStage(t)
	ctx := context.Background()
	const size = 1024
	for i := 0; i < 6; i++ {
		e.mkReservedUpload(t, nameN(i), size, -time.Minute)
	}
	if _, err := e.f.database.Pool.Exec(ctx,
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, e.f.spaceID, size*6); err != nil {
		t.Fatalf("设置用量失败: %v", err)
	}
	other := &uploadsvc.StageCleaner{
		Stager: e.store, Uploads: repo.UploadRepo{}, Spaces: repo.SpaceRepo{}, DB: e.f.svc.DB,
	}
	type out struct{ n int }
	ch := make(chan out, 2)
	run := func(c *uploadsvc.StageCleaner) {
		ch <- out{c.RunOnce(ctx).ReclaimedReservations}
	}
	go run(e.cleaner)
	go run(other)
	a, b := <-ch, <-ch
	if a.n+b.n != 6 {
		t.Fatalf("两个清理器合计应恰好认领 6 条(不相交),实际 %d + %d", a.n, b.n)
	}
	var used int64
	_ = e.f.database.Pool.QueryRow(ctx,
		`SELECT used_bytes FROM spaces WHERE id=$1`, e.f.spaceID).Scan(&used)
	if used != 0 {
		t.Fatalf("6 条各回退一次后应为 0,实际 %d(重复回退会让它变成负数再被 GREATEST 截成 0,但这里应恰好为 0)", used)
	}
}

// **删除失败重试 3 次后放弃**(不无限刷日志,也不静默)
func TestStageCleanerGivesUpAfterMaxAttempts(t *testing.T) {
	e := setupStage(t)
	orphan := "01a09000-0000-7000-8000-00000000beef"
	e.writeStage(t, orphan, 10, 48*time.Hour)
	// 用一个"总是失败"的 Stager 包住真实实现
	flaky := &failingStager{inner: e.store, failFor: orphan}
	e.cleaner.Stager = flaky
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		res := e.cleaner.RunOnce(ctx)
		if i < 3 && res.Failed != 0 {
			t.Fatalf("第 %d 次不该记为放弃,实际 %+v", i, res)
		}
		if i == 3 && res.Failed != 1 {
			t.Fatalf("第 3 次应记为放弃(Failed=1),实际 %+v", res)
		}
		if flaky.attempts != i {
			t.Fatalf("第 %d 次后尝试次数应为 %d,实际 %d", i, i, flaky.attempts)
		}
	}
}

// **不计入失败**:文件已被别人删掉(ErrNotFound)视为成功(幂等)
func TestStageCleanerTreatsAlreadyRemovedAsSuccess(t *testing.T) {
	e := setupStage(t)
	orphan := "01a09000-0000-7000-8000-00000000beef"
	e.writeStage(t, orphan, 10, 48*time.Hour)
	e.cleaner.Stager = &failingStager{inner: e.store, failFor: orphan, notFound: true}

	res := e.cleaner.RunOnce(context.Background())
	if res.Failed != 0 || res.Errors != 0 {
		t.Fatalf("已是 NotFound 应视为成功,实际 %+v", res)
	}
	if res.RemovedStages != 1 {
		t.Fatalf("应计入已清理,实际 %+v", res)
	}
}
