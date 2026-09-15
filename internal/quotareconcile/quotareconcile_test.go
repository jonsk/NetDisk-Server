package quotareconcile_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/quotareconcile"
	"github.com/netdisk/netdisk/internal/repo"
)

// 配额对账的真实 PG 用例(BE-S3-06 ⑤ / 4.3)。未设置 NETDISK_TEST_DSN 时 skip。

type fixture struct {
	pool     *pgxpool.Pool
	database *db.DB
	userID   string
	spaces   []string
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
	suffix := time.Now().Format("150405.000000000")
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, cerr := users.Create(ctx, tx, repo.CreateInput{
			Username: "qr_" + suffix, Email: "qr_" + suffix + "@example.com",
			DisplayName: "配额对账测试用户", Role: model.RoleUser,
		})
		if cerr != nil {
			return cerr
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	f := &fixture{pool: database.Pool, database: database, userID: u.ID}
	t.Cleanup(func() {
		bg := context.Background()
		// 只清本用例的数据(按 owner_id 限定;绝不无条件 DELETE)
		_, _ = f.pool.Exec(bg, `DELETE FROM uploads WHERE user_id = $1`, f.userID)
		_, _ = f.pool.Exec(bg,
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, f.userID)
		_, _ = f.pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, f.userID)
		_, _ = f.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, f.userID)
	})
	return f
}

// newSpace 造一个个人空间(建空间会连带建根目录行)。
func (f *fixture) newSpace(t *testing.T) (spaceID, rootID string) {
	t.Helper()
	spaces := repo.SpaceRepo{}
	sp, err := spaces.PersonalOf(context.Background(), f.pool, f.userID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	files := repo.FileRepo{}
	root, err := files.GetRoot(context.Background(), f.pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}
	f.spaces = append(f.spaces, sp.ID)
	return sp.ID, root.ID
}

// addFile 直接在库里插一行文件(绕开 finalize:本用例只测对账口径)。
func (f *fixture) addFile(t *testing.T, spaceID, rootID, name string, size int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, hash_sha256, depth)
VALUES ($1, $2, $3, $4, false, $5, $6, 1)`,
		spaceID, rootID, f.userID, name, size,
		// hash 必须唯一(每行一个不同内容),否则会与别的行共享对象
		name+"-"+time.Now().Format("150405.000000000")); err != nil {
		t.Fatalf("插文件行失败: %v", err)
	}
}

// setUsed 直接把 used_bytes 改成指定值(模拟某个写路径漏了增减)。
func (f *fixture) setUsed(t *testing.T, spaceID string, used int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, spaceID, used); err != nil {
		t.Fatalf("改 used_bytes 失败: %v", err)
	}
}

func (f *fixture) usedOf(t *testing.T, spaceID string) int64 {
	t.Helper()
	var used int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT used_bytes FROM spaces WHERE id = $1`, spaceID).Scan(&used); err != nil {
		t.Fatalf("读 used_bytes 失败: %v", err)
	}
	return used
}

// reserve 造一条"在飞预留"(模拟正在上传),并相应增加 used_bytes。
func (f *fixture) reserve(t *testing.T, spaceID, rootID string, size int64) {
	t.Helper()
	ctx := context.Background()
	spaces := repo.SpaceRepo{}
	if _, err := spaces.Reserve(ctx, f.pool, spaceID, size); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, state, ticket_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, 'reserved', $6, now() + interval '24 hours')`,
		f.userID, spaceID, rootID, "in-flight-"+time.Now().Format("150405.000000000"),
		size, "th-"+time.Now().Format("150405.000000000")); err != nil {
		t.Fatalf("插 uploads 行失败: %v", err)
	}
}

// service 造对账服务。**必须带 Scope**:spaces 是全局共享表,而无范围的对账
// 会顺手改掉并发运行的其它包的空间(实测:开了 autofix 的用例把别的包改坏了,
// 现象是"单跑必过、全量跑失败")。
func (f *fixture) service(spaceID string, autofix bool, threshold int64) *quotareconcile.Service {
	return &quotareconcile.Service{
		Spaces: repo.SpaceRepo{}, DB: db.AsQuerier(f.database),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		AlertBytes: threshold, AutoFix: autofix, Batch: 1000,
		Scope: []string{spaceID},
	}
}

// 漂移被检出并回写。
func TestReconcileDetectsAndFixesDrift(t *testing.T) {
	f := setup(t)
	spaceID, rootID := f.newSpace(t)
	f.addFile(t, spaceID, rootID, "a.bin", 3000)
	f.addFile(t, spaceID, rootID, "b.bin", 7000)
	// 真实占用 10000,却把账面写成 1000000(模拟"漏扣")
	f.setUsed(t, spaceID, 1_000_000)

	svc := f.service(spaceID, true, 1)
	rep, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Drifted == 0 {
		t.Fatal("应检出漂移空间")
	}
	if rep.Fixed == 0 {
		t.Fatal("autofix 开启时应回写")
	}
	if rep.MaxAbsDelta < 900_000 {
		t.Fatalf("最大漂移应接近 990000,实际 %d", rep.MaxAbsDelta)
	}
	if got := f.usedOf(t, spaceID); got != 10000 {
		t.Fatalf("回写后 used_bytes 应为 10000,实际 %d", got)
	}
	// 指标必须反映本轮结果(监控看不到就等于没有)
	if rep2, err := svc.RunOnce(context.Background()); err != nil || rep2.Drifted != 0 {
		t.Fatalf("回写后不应再有漂移(err=%v drifted=%d)", err, rep2.Drifted)
	}
}

// **关键回归**:有在飞预留的空间**不算漂移**(预留是 used_bytes 的合法组成)。
//
// 少了这一条,对账会把每个正在上传的空间都判成漂移 —— 而回写会抹掉预留,
// 正好打开预留机制要堵的洞(额度超卖)。
func TestReconcileDoesNotFlagInflightReservation(t *testing.T) {
	f := setup(t)
	spaceID, rootID := f.newSpace(t)
	f.addFile(t, spaceID, rootID, "c.bin", 5000)
	// 先把已落地的 5000 记上账(本用例直接插行,绕开了计费路径),
	// 再预留 200000 —— 这样账面 = 已占用 + 在飞预留,才是"正常状态"
	f.setUsed(t, spaceID, 5000)
	f.reserve(t, spaceID, rootID, 200_000)
	if got := f.usedOf(t, spaceID); got != 205_000 {
		t.Fatalf("预留后账面应为 205000,实际 %d", got)
	}

	svc := f.service(spaceID, true, 1)
	rep, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Drifted != 0 {
		t.Fatalf("有在飞预留不应算漂移(口径必须含 state='reserved'),实际 drifted=%d", rep.Drifted)
	}
	if got := f.usedOf(t, spaceID); got != 205_000 {
		t.Fatalf("预留不能被对账抹掉,账面应仍是 205000,实际 %d", got)
	}
}

// 阈值内的漂移:计数但不告警、不回写。
func TestReconcileRespectsThreshold(t *testing.T) {
	f := setup(t)
	spaceID, rootID := f.newSpace(t)
	f.addFile(t, spaceID, rootID, "d.bin", 1000)
	f.setUsed(t, spaceID, 1500) // 漂移 500

	svc := f.service(spaceID, true, 10_000) // 阈值 10000 > 500
	rep, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.Drifted == 0 {
		t.Fatal("漂移应被计数(即使未超阈值)")
	}
	if rep.Alerted != 0 || rep.Fixed != 0 {
		t.Fatalf("阈值内不应告警/回写,实际 alerted=%d fixed=%d", rep.Alerted, rep.Fixed)
	}
	if got := f.usedOf(t, spaceID); got != 1500 {
		t.Fatalf("阈值内不应改动账面,实际 %d", got)
	}
}

// **负漂移**(账面 < 实际)是超额使用:必须告警(不是静默回写就完事)。
func TestReconcileAlertsOnNegativeDrift(t *testing.T) {
	f := setup(t)
	spaceID, rootID := f.newSpace(t)
	f.addFile(t, spaceID, rootID, "e.bin", 900_000)
	f.setUsed(t, spaceID, 0) // 实际 900000,账面 0 → 超额使用 900000

	svc := f.service(spaceID, false, 1) // 不自动回写:先确认它**报**出来了
	rep, err := svc.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if rep.NegativeOnly == 0 {
		t.Fatal("应识别出负漂移(超额使用)")
	}
	if rep.Alerted == 0 {
		t.Fatal("负漂移超阈值必须告警")
	}
	if rep.MaxAbsDelta != 900_000 {
		t.Fatalf("最大绝对漂移应为 900000,实际 %d", rep.MaxAbsDelta)
	}
	// 未开 autofix:账面不动(诊断模式不该有副作用)
	if got := f.usedOf(t, spaceID); got != 0 {
		t.Fatalf("autofix 关闭时不得改动账面,实际 %d", got)
	}
}

// ---- 表驱动:对账判定(4.3 / 6.6)----
//
// 为什么必须表驱动:对账的每条规则(权威口径、阈值边界、autofix 开关、条件回写)
// 都是"账面状态 → 判定 + 账面结果"的映射,只差一两个输入字段;散成一堆用例之后
// 没人说得清"漂移恰好等于阈值算不算超阈值"这条到底被哪个用例钉住了。

// injectQuerier 在**第一次 Exec 之前**执行一次注入,用来确定性地复现
// "读取(ListDrifted)到回写(SetUsed)之间"的并发窗口。
//
// 不用它就只能靠 sleep 去抢那个窗口,那种用例在 CI 上必然偶发失败 ——
// 而这个窗口恰恰是"条件写"唯一要保护的东西,不能不测。
type injectQuerier struct {
	repo.Querier
	inject   func()
	injected bool
}

// Exec 是 RunOnce 唯一的写入口(SetUsed 的条件 UPDATE),所以注入点就是它。
// RunOnce 是单 goroutine 顺序执行,这里不需要加锁。
func (q *injectQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if !q.injected {
		q.injected = true
		q.inject()
	}
	return q.Querier.Exec(ctx, sql, args...)
}

// TestReconcileDecisionTable 用表驱动钉住对账的**判定规则**。
//
// 每个用例的输入只有"已落地文件 / 账面值 / 在飞预留 / autofix / 阈值",
// 输出是 Report 的计数与对账后的账面值 —— 后者同时验证"该写的写了、
// 不该写的没写"。
func TestReconcileDecisionTable(t *testing.T) {
	cases := []struct {
		name string
		// files 是本用例造出的已落地文件大小(每项一行)
		files []int64
		// stored 是**显式**写入的账面值(在 reserve 之前),用来制造漂移
		stored int64
		// reserve 是在飞预留:它同时增加账面与权威口径(模拟正在上传)
		reserve int64
		// autofix 为真时超阈值的漂移会被回写
		autofix bool
		// threshold 是告警阈值(AlertBytes,同时也是回写条件里的 minDrift)
		threshold int64
		// inject 在"回写那一刻之前"注入并发变更;nil 表示不注入
		inject func(t *testing.T, f *fixture, spaceID, rootID string)

		wantDrifted  int
		wantAlerted  int
		wantFixed    int
		wantNegative int
		// wantUsed 是对账后的账面值
		wantUsed int64
	}{
		{
			name:      "漂移为 0:不计入 drifted,也不改账面",
			files:     []int64{1000},
			stored:    1000,
			autofix:   true,
			threshold: 1,
			wantUsed:  1000,
			// 为什么:口径一致时 `WHERE used_bytes <> expected` 就该把它过滤掉。
			// 注意:漏算在飞预留项在这个用例里看不出来(没有预留),所以另有专门用例。
		},
		{
			name:        "漂移恰好等于阈值:只计数,不告警不回写(严格大于)",
			files:       []int64{1000},
			stored:      1500, // 漂移 +500
			autofix:     true,
			threshold:   500,
			wantDrifted: 1,
			wantUsed:    1500,
			// 为什么:判定是 `abs(delta) <= threshold` 就跳过。写成 `>=` 会在边界
			// 告警并回写 —— 而阈值是"避免告警风暴"的门槛,不是可容忍的误差。
		},
		{
			name:        "阈值 1 且漂移 1 字节:仍不告警",
			files:       []int64{1000},
			stored:      1001, // 漂移 +1
			threshold:   1,
			wantDrifted: 1,
			wantUsed:    1001,
			// 为什么:DefaultAlertBytes 的注释说"想对任何漂移就设成 1",而 1 本身
			// 仍遵守严格大于 —— 把 1 当"任何非零漂移"的哨兵会在边界多报警。
		},
		{
			name:        "阈值 1:任何大于 1 字节的漂移都告警",
			files:       []int64{1000},
			stored:      1002, // 漂移 +2
			threshold:   1,
			wantDrifted: 1,
			wantAlerted: 1,
			wantUsed:    1002,
			// 为什么:实现若把阈值理解成"比例/最小字节数"或把 1 当"不告警"哨兵,
			// 这里就会漏报 —— 而漏报意味着漂移(写路径漏增减)永远没人发现。
		},
		{
			name:      "在飞预留计入权威口径:正在上传不算漂移,账面原样不动",
			files:     []int64{5000},
			stored:    5000,
			reserve:   200_000,
			autofix:   true,
			threshold: 1,
			wantUsed:  205_000,
			// **最关键的一条**:口径漏掉 state='reserved' 时,每个正在上传的空间
			// 都会被判成漂移(这里 delta=-200000),回写会把预留抹掉 —— 正好打开
			// 预留机制要堵的洞(额度超卖)。
		},
		{
			name:        "超过阈值 + autofix 开:告警并回写为含预留的口径",
			files:       []int64{3000, 7000},
			stored:      1_000_000,
			reserve:     200_000,
			autofix:     true,
			threshold:   1,
			wantDrifted: 1,
			wantAlerted: 1,
			wantFixed:   1,
			wantUsed:    210_000,
			// 为什么:回写必须写"含在飞预留"的期望值(10000+200000)。若把
			// sum(files) 当成期望值写回,刚预留的 200000 就没了。
		},
		{
			name:         "autofix 关:负漂移(超额使用)只观测,账面不动",
			files:        []int64{900_000},
			stored:       0, // 实际 900000,账面 0 → 超额使用
			threshold:    1,
			wantDrifted:  1,
			wantAlerted:  1,
			wantNegative: 1,
			wantUsed:     0,
			// 为什么:诊断模式(autofix=false)不该有副作用 —— 否则运维"先看看"
			// 的一次执行就把生产数据改了。
		},
		{
			name:         "autofix 开:负漂移也回写,并单独计数",
			files:        []int64{900_000},
			stored:       0,
			autofix:      true,
			threshold:    1,
			wantDrifted:  1,
			wantAlerted:  1,
			wantFixed:    1,
			wantNegative: 1,
			wantUsed:     900_000,
			// 为什么:两个方向的后果不对称(负漂移=容量在流失),NegativeOnly 必须
			// 单独计数,否则监控分不清"用户被多算"与"超额使用"。
		},
		{
			name:      "条件写:回写前漂移已被补齐时回写落空,不覆盖并发状态",
			files:     []int64{10_000},
			stored:    1_000_000,
			autofix:   true,
			threshold: 500_000,
			inject: func(t *testing.T, f *fixture, spaceID, rootID string) {
				// 模拟"读取之后"发生的两件事:来了一笔新的在飞预留(200000),
				// 且账面已被别的写路径补到只差 100 字节(低于阈值)。
				f.reserve(t, spaceID, rootID, 200_000)
				f.setUsed(t, spaceID, 209_900) // 口径 210000 → 漂移 -100
			},
			wantDrifted: 1,
			wantAlerted: 1,
			wantFixed:   0,
			wantUsed:    209_900,
			// 为什么:SetUsed 的 WHERE 带 `abs(漂移) > 阈值`。没有这个条件,这次
			// 回写会把"读取之后"的并发状态整体覆盖 —— 回写落空(fixed=0)且账面
			// 保持注入后的值,才是条件写真在生效的证据。
		},
		{
			name:      "条件写:回写用写入时刻的口径,读取后新来的预留不能被抹掉",
			files:     []int64{10_000},
			stored:    1_000_000,
			autofix:   true,
			threshold: 500_000,
			inject: func(t *testing.T, f *fixture, spaceID, rootID string) {
				// 只来了新预留,漂移仍是 +990000(远超阈值)→ 这次回写应当成功,
				// 但必须写"写入时刻"的期望值(10000+200000)。
				f.reserve(t, spaceID, rootID, 200_000)
			},
			wantDrifted: 1,
			wantAlerted: 1,
			wantFixed:   1,
			wantUsed:    210_000,
			// 为什么:UPDATE 里的期望值是**当场重算**的。若实现把读到的旧 expected
			// 写回,这里会得到 10000 —— 把刚预留的 200000 抹掉(额度超卖)。
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			spaceID, rootID := f.newSpace(t)
			var landed int64
			for i, size := range c.files {
				f.addFile(t, spaceID, rootID, fmt.Sprintf("f%d.bin", i), size)
				landed += size
			}
			f.setUsed(t, spaceID, c.stored)
			if c.reserve > 0 {
				f.reserve(t, spaceID, rootID, c.reserve)
			}

			svc := f.service(spaceID, c.autofix, c.threshold)
			if c.inject != nil {
				svc.DB = &injectQuerier{Querier: svc.DB, inject: func() { c.inject(t, f, spaceID, rootID) }}
			}
			rep, err := svc.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("对账失败: %v", err)
			}
			if rep.Drifted != c.wantDrifted {
				t.Errorf("Drifted 应为 %d,实际 %d(权威口径 = %d)", c.wantDrifted, rep.Drifted, landed+c.reserve)
			}
			if rep.Alerted != c.wantAlerted {
				t.Errorf("Alerted 应为 %d,实际 %d(阈值 %d,账面 %d,口径 %d)",
					c.wantAlerted, rep.Alerted, c.threshold, c.stored+c.reserve, landed+c.reserve)
			}
			if rep.Fixed != c.wantFixed {
				t.Errorf("Fixed 应为 %d,实际 %d(autofix=%v,账面 %d,口径 %d)",
					c.wantFixed, rep.Fixed, c.autofix, c.stored+c.reserve, landed+c.reserve)
			}
			if rep.NegativeOnly != c.wantNegative {
				t.Errorf("NegativeOnly 应为 %d,实际 %d(账面 %d,口径 %d)",
					c.wantNegative, rep.NegativeOnly, c.stored+c.reserve, landed+c.reserve)
			}
			if got := f.usedOf(t, spaceID); got != c.wantUsed {
				t.Fatalf("对账后账面应为 %d,实际 %d(权威口径 = %d)", c.wantUsed, got, landed+c.reserve)
			}
		})
	}
}

// 未装配 DB 时明确报错(而不是静默"没有漂移" —— 一个不做事的对账任务
// 会让人以为"对账通过")。
func TestReconcileRequiresDB(t *testing.T) {
	svc := &quotareconcile.Service{}
	if _, err := svc.RunOnce(context.Background()); err == nil {
		t.Fatal("未装配 DB 应报错")
	}
}
