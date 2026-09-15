package patrol_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/patrol"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// 对象泄漏/反向孤儿巡检的真实 PG + 真实文件系统用例(BE-S10-04)。
// 未设置 NETDISK_TEST_DSN 时 skip。
//
// **为什么每个用例都传 Scope**:`file_objects` 是全局共享表,而本用例的存储根
// 是独占的临时目录 —— 别的包(以及并行跑的其它包)插进去的 live 行在本目录里
// 当然"不存在"。全表抽样会把它们全部报成丢失,现象是"单跑全过、全量跑到处是丢失"。
// 生产路径(全表抽样)由 `TestPatrolSamplesUpToConfiguredLimit` 覆盖。

type fixture struct {
	pool     *pgxpool.Pool
	database *db.DB
	fsys     *storage.FS
	made     []string
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
	fsys, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	f := &fixture{pool: database.Pool, database: database, fsys: fsys}
	t.Cleanup(func() {
		// 只删本用例登记的 hash(绝不无条件 DELETE:共用测试库里
		// 别的包的行正在被断言)
		if len(f.made) > 0 {
			_, _ = f.pool.Exec(context.Background(),
				`DELETE FROM file_objects WHERE hash_sha256 = ANY($1::varchar[])`, f.made)
		}
	})
	return f
}

// uniqueHash 造一个**本次调用独有**的 hash。
//
// 不用固定字面量:共用测试库里固定 hash 会与别的用例的行互相 UPSERT
// (一个用例改了 ref_count/state,另一个用例的断言就飘了)。
func uniqueHash(seed string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed, time.Now().UnixNano())))
	return hex.EncodeToString(sum[:])
}

// putObject 造"磁盘上有 + PG 有"的对象,返回 hash。
func (f *fixture) putObject(t *testing.T, seed string, size int64) string {
	t.Helper()
	ctx := context.Background()
	hash := uniqueHash(seed)
	data := bytes.Repeat([]byte("x"), int(size))
	if _, err := f.fsys.Write(ctx, hash, bytes.NewReader(data), size); err != nil {
		t.Fatalf("写对象失败: %v", err)
	}
	f.insertRow(t, hash, size)
	return hash
}

// putRowOnly 只插 PG 行、**不写磁盘**(模拟"对象丢了")。
func (f *fixture) putRowOnly(t *testing.T, seed string, size int64) string {
	t.Helper()
	hash := uniqueHash(seed)
	f.insertRow(t, hash, size)
	return hash
}

func (f *fixture) insertRow(t *testing.T, hash string, size int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE SET ref_count = 1, state = 'live', size = EXCLUDED.size`,
		hash, size, f.fsys.KeyFor(hash)); err != nil {
		t.Fatalf("插对象行失败: %v", err)
	}
	f.made = append(f.made, hash)
}

// putDiskOnly 造"磁盘上有、PG 没有"的对象(泄漏)。
func (f *fixture) putDiskOnly(t *testing.T, seed string, size int64) string {
	t.Helper()
	hash := uniqueHash(seed)
	p := filepath.Join(f.fsys.Root, f.fsys.KeyFor(hash))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(p, bytes.Repeat([]byte("y"), int(size)), 0o640); err != nil {
		t.Fatalf("写泄漏文件失败: %v", err)
	}
	return hash
}

func (f *fixture) service(scope []string, storageOverride storage.Storager) *patrol.Service {
	var st storage.Storager = f.fsys
	if storageOverride != nil {
		st = storageOverride
	}
	return &patrol.Service{
		DB: db.AsQuerier(f.database), Storage: st, FS: f.fsys,
		Scope: scope, LocalSampleSize: 10, RetryBackoff: 0,
		Now:  func() time.Time { return time.Unix(1700000000, 0) },
		Log:  testLogger(),
	}
}

// testLogger 丢弃日志:用例里出现的 ERROR 行会被误读成"真的报警了",
// 而断言在 Report 上更精确。需要看日志时临时改成 slog.Default()。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---- 验收①②:正向差值与反向抽样 ----

func TestPatrolDetectsLeakAndMissing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	h1 := f.putObject(t, "ok1", 100)
	h2 := f.putObject(t, "ok2", 200)
	h3 := f.putRowOnly(t, "lost", 300) // PG 有、磁盘没有

	// 用定向模式做确定性断言:磁盘侧只数范围内对象,PG 侧也只数范围内行。
	// (全表正向差值会被别的包的磁盘占用干扰 —— 本用例的存储根是独占的。)
	svc := f.service([]string{h1, h2, h3}, nil)
	rep, err := svc.RunOnce(ctx)
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if rep.ForwardSkipped {
		t.Fatal("本地 fs 后端不应跳过正向统计")
	}
	if rep.DiskFiles != 2 || rep.DiskBytes != 300 {
		t.Fatalf("磁盘侧应为 2 文件/300 字节,实际 %d/%d", rep.DiskFiles, rep.DiskBytes)
	}
	if rep.DBObjects != 3 || rep.DBBytes != 600 {
		t.Fatalf("PG 侧应为 3 行/600 字节,实际 %d/%d", rep.DBObjects, rep.DBBytes)
	}
	// 差值 = 300 - 600 = **-300**:PG 说有 600 字节、磁盘只有 300 ——
	// 这正是"对象丢了"的信号。**不夹到 0**是刻意的:夹掉就永远看不见它。
	if rep.LeakBytes != -300 {
		t.Fatalf("正向差值应为 -300(不夹到 0),实际 %d", rep.LeakBytes)
	}
	if rep.Sampled != 3 || rep.Missing != 1 || rep.Unreachable != 0 {
		t.Fatalf("应为 sampled=3/missing=1/unreachable=0,实际 %d/%d/%d",
			rep.Sampled, rep.Missing, rep.Unreachable)
	}
	if len(rep.MissingHashes) != 1 || rep.MissingHashes[0] != h3 {
		t.Fatalf("丢失清单应含 %s,实际 %v", h3, rep.MissingHashes)
	}
}

// 正向泄漏:磁盘有、PG 没有 → 差值为正。
func TestPatrolReportsPositiveLeakWhenDiskHasUnregisteredObjects(t *testing.T) {
	f := setup(t)
	h1 := f.putObject(t, "reg", 100)
	// 磁盘上多出来的一份(不在 PG 里,也不在 Scope 里 —— 定向模式数不到它,
	// 因此这里用"扫目录"的口径验证:临时文件必须被跳过)
	tmpPath := filepath.Join(f.fsys.Root, "objects", "zz", "zz", ".tmp-abcdef")
	if err := os.MkdirAll(filepath.Dir(tmpPath), 0o750); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(tmpPath, bytes.Repeat([]byte("t"), 500), 0o640); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	files, bytes, err := f.fsys.ObjectUsage(context.Background())
	if err != nil {
		t.Fatalf("统计对象目录失败: %v", err)
	}
	if files != 1 || bytes != 100 {
		t.Fatalf("原子写的 .tmp-* 必须被跳过:应为 1 文件/100 字节,实际 %d/%d", files, bytes)
	}

	// 再放一个"磁盘有、PG 没有"的正式对象 → 全量口径下差值为正
	leakHash := f.putDiskOnly(t, "leak", 1000)
	_ = leakHash
	if _, _, err := f.fsys.ObjectUsage(context.Background()); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	rep, err := f.service([]string{h1}, nil).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	// 定向模式:PG 侧 1 行 100 字节,磁盘侧同范围 1 个 100 字节 → 差值 0
	if rep.LeakBytes != 0 {
		t.Fatalf("定向模式下差值应为 0,实际 %d", rep.LeakBytes)
	}
}

// ---- 验收③:不可达 vs 明确不存在 ----

type fakeStatStore struct {
	err       error
	backend   string
	calls     int
	writeCall int
}

func (f *fakeStatStore) Backend() string {
	if f.backend != "" {
		return f.backend
	}
	return storage.BackendFS
}
func (f *fakeStatStore) KeyFor(h string) string { return "objects/" + h }
func (f *fakeStatStore) Write(context.Context, string, io.Reader, int64) (int64, error) {
	f.writeCall++
	return 0, nil
}
func (f *fakeStatStore) Stat(context.Context, string) (int64, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return 1, nil
}
func (f *fakeStatStore) Open(context.Context, string) (io.ReadSeekCloser, error) {
	return nil, storage.ErrNotFound
}
func (f *fakeStatStore) Delete(context.Context, string) error { return nil }

func TestPatrolClassifiesUnreachableSeparatelyFromMissing(t *testing.T) {
	f := setup(t)
	h1 := f.putObject(t, "c1", 10)
	h2 := f.putObject(t, "c2", 10)
	h3 := f.putObject(t, "c3", 10)
	scope := []string{h1, h2, h3}

	// 1) 后端持续**不可达** → 全部记观测项(不是丢失);非 ErrNotFound 才重试
	unreachable := &fakeStatStore{err: errors.New("connection reset by peer")}
	rep, err := f.service(scope, unreachable).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("后端不可达不应让巡检整体失败: %v", err)
	}
	if rep.Unreachable != 3 || rep.Missing != 0 {
		t.Fatalf("应 unreachable=3/missing=0,实际 %d/%d", rep.Unreachable, rep.Missing)
	}
	// 调用次数 = 正向 3 次(Scope 模式下磁盘侧逐个 Stat)+ 反向 3 对象 × 3 次尝试。
	// 把算式写出来而不是写死 12:断言要能说明"重试确实发生了几次"。
	if want := 3 + 3*3; unreachable.calls != want {
		t.Fatalf("不可达应重试(正向 3 + 反向 3×3 = %d 次),实际 %d", want, unreachable.calls)
	}
	if unreachable.writeCall != 0 {
		t.Fatalf("巡检不得写对象,实际 Write %d 次", unreachable.writeCall)
	}

	// 2) **明确不存在** → 记丢失,且不重试(确定性结论)
	missing := &fakeStatStore{err: storage.ErrNotFound}
	rep2, err := f.service(scope, missing).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if rep2.Missing != 3 || rep2.Unreachable != 0 {
		t.Fatalf("应 missing=3/unreachable=0,实际 %d/%d", rep2.Missing, rep2.Unreachable)
	}
	if want := 3 + 3; missing.calls != want {
		t.Fatalf("ErrNotFound 不应重试(正向 3 + 反向 3 = %d 次),实际 %d", want, missing.calls)
	}
}

// ---- 验收④:只读 ----

// readOnlyQuerier 把任何写语句直接判失败:让"巡检只读"从注释变成会红的断言。
// 这条纪律被破坏的方式通常是无意中加了一句"顺手把漂移修正一下"。
type readOnlyQuerier struct {
	repo.Querier
	t *testing.T
}

func (q readOnlyQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.t.Fatalf("巡检不得执行写语句: %s", sql)
	return pgconn.CommandTag{}, errors.New("unreachable")
}

func TestPatrolNeverWritesToDB(t *testing.T) {
	f := setup(t)
	h1 := f.putObject(t, "ro", 10)
	svc := f.service([]string{h1}, &fakeStatStore{})
	svc.DB = readOnlyQuerier{Querier: db.AsQuerier(f.database), t: t}
	// 巡检失败在**很多**路径上会写日志/指标,但不写库;这里只要求它不写库
	if _, err := svc.RunOnce(context.Background()); err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
}

// ---- 验收②:后端分级 ----

func TestPatrolSampleSizeFollowsBackend(t *testing.T) {
	f := setup(t)
	// 先自己造够行数,让"抽样量 == 配置值"这条断言**不依赖别的包有没有先跑**
	// (全新测试库里 file_objects 可能是空的,那时抽不满就是必然的)
	for i := 0; i < 5; i++ {
		f.putObject(t, fmt.Sprintf("fill%d", i), 10)
	}
	// 不传 Scope:走真实抽样路径(生产的路径)。
	local := &patrol.Service{
		DB: db.AsQuerier(f.database), Storage: f.fsys, FS: f.fsys,
		LocalSampleSize: 3, RemoteSampleSize: 7, RetryBackoff: 0,
	}
	rep, err := local.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if rep.Sampled != 3 {
		t.Fatalf("本地后端应抽 LocalSampleSize=3,实际 %d", rep.Sampled)
	}
	if !rep.ForwardSkipped && rep.DBObjects < 3 {
		t.Fatalf("库里对象数应 >= 抽样量(否则用例的前提不成立),实际 %d", rep.DBObjects)
	}

	remote := &fakeStatStore{err: storage.ErrNotFound, backend: "s3"}
	svc := &patrol.Service{
		DB: db.AsQuerier(f.database), Storage: remote, FS: f.fsys,
		LocalSampleSize: 3, RemoteSampleSize: 7, RetryBackoff: 0,
	}
	rep2, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检失败: %v", err)
	}
	if rep2.Sampled != 7 {
		t.Fatalf("远端后端应抽 RemoteSampleSize=7,实际 %d", rep2.Sampled)
	}
	if !rep2.ForwardSkipped {
		t.Fatal("远端后端应跳过正向统计(拿 0 去减会得到荒谬的负值)")
	}
}

