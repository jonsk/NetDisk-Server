package syncfeed_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 变更流的真实 PG 集成测试(BE-S9-01/02/03)。
//
// 这组用例的重点是**两条会静默出错的纪律**:
//   - 变更行必须与元数据同事务(否则"文件改了但客户端不知道");
//   - 超窗必须报错而不能返回空(否则客户端以为自己已最新,永久漏掉一段)。

type fixture struct {
	database *db.DB
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
	database, err := db.OpenWithDSN(ctx, dsn, 6)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	suffix := time.Now().Format("150405.000000000")
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: "sf_" + suffix, Email: "sf_" + suffix + "@example.com",
			DisplayName: "变更流测试用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	root, err := repo.FileRepo{}.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// 没有任何无条件 DELETE:先按本用户的空间限定,再按 id 删用户
		_, _ = database.Pool.Exec(bg, `DELETE FROM sync_feed WHERE space_id = $1`, sp.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM sync_cursors WHERE space_id = $1`, sp.ID)
		_, _ = database.Pool.Exec(bg, `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, u.ID)
		_, _ = database.Pool.Exec(bg,
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM uploads WHERE user_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})
	return &fixture{database: database, userID: u.ID, spaceID: sp.ID, rootID: root.ID}
}

// mkFile 造一个文件行(直接插库,本组用例只关心变更流)。
func (f *fixture) mkFile(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := f.database.Pool.QueryRow(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1,$2,$3,$4,false,1,1) RETURNING id`, f.spaceID, f.rootID, f.userID, name).Scan(&id); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	return id
}

func (f *fixture) mkDir(t *testing.T, name string) string {
	t.Helper()
	d, err := repo.FileRepo{}.CreateDir(context.Background(), f.database.Pool, repo.CreateDirInput{
		SpaceID: f.spaceID, ParentID: f.rootID, OwnerID: f.userID, Name: name, Depth: 1,
	})
	if err != nil {
		t.Fatalf("造目录失败: %v", err)
	}
	return d.ID
}

func (f *fixture) lastSeq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT last_seq FROM spaces WHERE id=$1`, f.spaceID).Scan(&seq); err != nil {
		t.Fatalf("读 last_seq 失败: %v", err)
	}
	return seq
}

func (f *fixture) floorSeq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT feed_floor_seq FROM spaces WHERE id=$1`, f.spaceID).Scan(&seq); err != nil {
		t.Fatalf("读 feed_floor_seq 失败: %v", err)
	}
	return seq
}

// appendTx 在一个事务里调 AppendEvent(模拟写路径的真实用法)。
func (f *fixture) appendTx(t *testing.T, e syncfeed.Event) int64 {
	t.Helper()
	var seq int64
	if err := f.database.InTx(context.Background(), func(tx pgx.Tx) error {
		s, err := syncfeed.AppendEvent(context.Background(), tx, e)
		if err != nil {
			return err
		}
		seq = s
		return nil
	}); err != nil {
		t.Fatalf("写变更失败: %v", err)
	}
	return seq
}

// **验收①** 与元数据同事务:事务回滚 → 变更行**不留下**
//
// 这是"客户端收到变更但元数据回滚了"那一半:分开写会造出指向不存在文件的变更,
// 客户端照它去拉取只会拿到 404,而它会一直重试。
func TestFeedRollsBackWithMetadata(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.mkFile(t, "rollback.txt")

	sentinel := errors.New("故意失败")
	err := f.database.InTx(ctx, func(tx pgx.Tx) error {
		if _, aerr := syncfeed.AppendEvent(ctx, tx, syncfeed.Event{
			SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
			ParentID: f.rootID, Name: "rollback.txt",
		}); aerr != nil {
			return aerr
		}
		return sentinel // 变更写了、元数据"失败" → 整体回滚
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("应回滚,实际 %v", err)
	}
	var n int
	if err := f.database.Pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_feed WHERE space_id=$1`, f.spaceID).Scan(&n); err != nil {
		t.Fatalf("查变更行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("回滚后不应留下变更行,实际 %d", n)
	}
}

// **验收②** 字段契约:`parent_id/name` 必填;moved 必须带 `old_parent_id`
func TestFeedCarriesLocationAndOldParent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.mkFile(t, "a.txt")
	dest := f.mkDir(t, "dest")

	seqCreate := f.appendTx(t, syncfeed.Event{
		SpaceID: f.spaceID, FileID: id, Kind: model.FeedMoved, Version: 2,
		ParentID: dest, Name: "a-renamed.txt", OldParentID: f.rootID,
	})
	page, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, 0, 100)
	if err != nil {
		t.Fatalf("拉变更失败: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("应有 1 条,实际 %d", len(page.Items))
	}
	it := page.Items[0]
	if it.ChangeSeq != seqCreate {
		t.Fatalf("change_seq 应为 %d,实际 %d", seqCreate, it.ChangeSeq)
	}
	if it.Kind != model.FeedMoved || it.Name != "a-renamed.txt" ||
		it.ParentID != dest || it.OldParentID != f.rootID || it.Version != 2 {
		t.Fatalf("moved 事件字段不符: %+v", it)
	}
	if it.FileID != id {
		t.Fatalf("file_id 应为 %s,实际 %s", id, it.FileID)
	}
}

// **验收③** 目录自身的变更也入 feed(否则"新建空目录"客户端永远看不到)
func TestFeedRecordsDirectoryItself(t *testing.T) {
	f := setup(t)
	dir := f.mkDir(t, "empty-dir")
	f.appendTx(t, syncfeed.Event{
		SpaceID: f.spaceID, FileID: dir, Kind: model.FeedCreated, Version: 1,
		ParentID: f.rootID, Name: "empty-dir",
	})
	page, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, 0, 10)
	if err != nil {
		t.Fatalf("拉变更失败: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].FileID != dir {
		t.Fatalf("目录变更必须入 feed,实际 %+v", page.Items)
	}
}

// **验收②(接口)** 契约:{items,next_seq,has_more} 且分页不重不漏
func TestChangesPaginationIsKeyset(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		id := f.mkFile(t, nameI(i))
		f.appendTx(t, syncfeed.Event{
			SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
			ParentID: f.rootID, Name: nameI(i),
		})
	}
	first, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, 0, 2)
	if err != nil {
		t.Fatalf("首页失败: %v", err)
	}
	if len(first.Items) != 2 || !first.HasMore {
		t.Fatalf("首页应为 2 条且 has_more,实际 %d/%v", len(first.Items), first.HasMore)
	}
	if first.NextSeq != first.Items[len(first.Items)-1].ChangeSeq {
		t.Fatalf("next_seq 应为页内最后一条的 seq")
	}
	// 用 next_seq 翻页:不重不漏
	second, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, first.NextSeq, 2)
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if len(second.Items) != 2 || !second.HasMore {
		t.Fatalf("第二页应为 2 条且 has_more,实际 %d/%v", len(second.Items), second.HasMore)
	}
	third, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, second.NextSeq, 2)
	if err != nil {
		t.Fatalf("第三页失败: %v", err)
	}
	if len(third.Items) != 1 || third.HasMore {
		t.Fatalf("第三页应为 1 条且无更多,实际 %d/%v", len(third.Items), third.HasMore)
	}
	seen := map[int64]bool{}
	for _, pg := range []*syncfeed.Page{first, second, third} {
		for _, it := range pg.Items {
			if seen[it.ChangeSeq] {
				t.Fatalf("分页出现重复 seq %d", it.ChangeSeq)
			}
			seen[it.ChangeSeq] = true
		}
	}
	if len(seen) != 5 {
		t.Fatalf("合计应看到 5 条,实际 %d", len(seen))
	}
}

// **验收②(接口)** limit 有上限:超限**夹到上限**而不是报错
func TestChangesLimitIsCapped(t *testing.T) {
	f := setup(t)
	id := f.mkFile(t, "cap.txt")
	f.appendTx(t, syncfeed.Event{
		SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
		ParentID: f.rootID, Name: "cap.txt",
	})
	// 要一个天文数字:不应报错,也不应把整个表读出来
	page, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, 0, 1<<30)
	if err != nil {
		t.Fatalf("超大 limit 应被夹到上限而不是报错: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("应返回 1 条,实际 %d", len(page.Items))
	}
}

// **验收④(接口)** 同一个 since 重复请求结果幂等
func TestChangesIsIdempotentForSameSince(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id := f.mkFile(t, nameI(i))
		f.appendTx(t, syncfeed.Event{
			SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
			ParentID: f.rootID, Name: nameI(i),
		})
	}
	a, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, 0, 100)
	if err != nil {
		t.Fatalf("第一次失败: %v", err)
	}
	b, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, 0, 100)
	if err != nil {
		t.Fatalf("第二次失败: %v", err)
	}
	if len(a.Items) != len(b.Items) || a.NextSeq != b.NextSeq {
		t.Fatalf("同一 since 必须幂等: %d/%d vs %d/%d", len(a.Items), a.NextSeq, len(b.Items), b.NextSeq)
	}
	for i := range a.Items {
		if a.Items[i] != b.Items[i] {
			t.Fatalf("第 %d 条不一致: %+v vs %+v", i, a.Items[i], b.Items[i])
		}
	}
}

// **验收③(接口)** 超窗 → ErrCursorExpired(绝不能静默返回空)
func TestChangesReportsCursorExpired(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id := f.mkFile(t, nameI(i))
		f.appendTx(t, syncfeed.Event{
			SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
			ParentID: f.rootID, Name: nameI(i),
		})
	}
	// 清掉前两条(水位推到第 2 条的真实 seq)
	floorSeq := f.seqsOf(t, 3)[1]
	if err := f.database.InTx(ctx, func(tx pgx.Tx) error {
		_, err := syncfeed.Prune(ctx, tx, map[string]int64{f.spaceID: floorSeq}, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if floor := f.floorSeq(t); floor != floorSeq {
		t.Fatalf("清理后水位应为 %d,实际 %d", floorSeq, floor)
	}
	// since=0 < 水位 → 超窗
	if _, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, 0, 100); !errors.Is(err, syncfeed.ErrCursorExpired) {
		t.Fatalf("过老游标应报 ErrCursorExpired,实际 %v", err)
	}
	// since=水位(正好在边界)== 不超窗:客户端确实从水位之后开始要
	page, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, floorSeq, 100)
	if err != nil {
		t.Fatalf("since=水位 不应超窗: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("应剩 1 条,实际 %d", len(page.Items))
	}
}

// **验收④(接口)** 全表被清空后,过老游标**依然**要判超窗
//
// 这是没有水位列时最阴的 bug:表空了 → 查不到任何行 → 静默返回空 →
// 客户端以为已同步完,永久漏掉整段变更。
func TestChangesExpiredEvenWhenFeedIsEmpty(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.mkFile(t, "only.txt")
	only := f.appendTx(t, syncfeed.Event{
		SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
		ParentID: f.rootID, Name: "only.txt",
	})
	if err := f.database.InTx(ctx, func(tx pgx.Tx) error {
		_, err := syncfeed.Prune(ctx, tx, map[string]int64{f.spaceID: only}, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	var n int
	_ = f.database.Pool.QueryRow(ctx,
		`SELECT count(*) FROM sync_feed WHERE space_id=$1`, f.spaceID).Scan(&n)
	if n != 0 {
		t.Fatalf("前置:表应已清空,实际 %d 行", n)
	}
	if _, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, 0, 100); !errors.Is(err, syncfeed.ErrCursorExpired) {
		t.Fatalf("空表 + 过老游标必须报告超窗(而不是静默返回空),实际 %v", err)
	}
	// 而 since=水位 应返回正常空页
	page, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, only, 100)
	if err != nil {
		t.Fatalf("水位之后的游标不应超窗: %v", err)
	}
	if len(page.Items) != 0 || page.NextSeq != only {
		t.Fatalf("应为空页且回显 next_seq=%d,实际 %+v", only, page)
	}
}

// **验收①(游标)** 单维 (client_id, space_id) 取 **max**(乱序/重放不倒退)
func TestCursorReportTakesMaxPerDimension(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	report := func(seq int64) {
		t.Helper()
		if err := syncfeed.ReportCursor(ctx, f.database.Pool, syncfeed.CursorReport{
			ClientID: "cli-1", SpaceID: f.spaceID, LastSeq: seq,
		}); err != nil {
			t.Fatalf("上报失败: %v", err)
		}
	}
	report(50)
	report(10) // 乱序/重放:必须被忽略
	report(30)
	var last int64
	if err := f.database.Pool.QueryRow(ctx,
		`SELECT last_seq FROM sync_cursors WHERE client_id='cli-1' AND space_id=$1`, f.spaceID).Scan(&last); err != nil {
		t.Fatalf("读游标失败: %v", err)
	}
	if last != 50 {
		t.Fatalf("游标应为 50(max 语义),实际 %d", last)
	}
}

// **验收②(游标)** 全局水位 = 跨全维取 **min**(宁可晚清,不可多清)
func TestGlobalWatermarkTakesMinAcrossDimensions(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	// 三个 (client, space) 维:两个在本空间,一个在另一个空间
	other := setup(t)
	pairs := []syncfeed.CursorReport{
		{ClientID: "cli-a", SpaceID: f.spaceID, LastSeq: 100},
		{ClientID: "cli-b", SpaceID: f.spaceID, LastSeq: 20},
		{ClientID: "cli-a", SpaceID: other.spaceID, LastSeq: 7},
	}
	for _, p := range pairs {
		if err := syncfeed.ReportCursor(ctx, f.database.Pool, p); err != nil {
			t.Fatalf("上报失败: %v", err)
		}
	}
	w, err := syncfeed.GlobalWatermark(ctx, f.database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("算水位失败: %v", err)
	}
	if w.Seq > 7 {
		t.Fatalf("全局水位必须 <= 最小已确认(7),实际 %d —— 取 max 会让落后客户端永久漏变更", w.Seq)
	}
	if w.Clients < 3 {
		t.Fatalf("应统计到 3 个维,实际 %d", w.Clients)
	}
}

// **验收③(游标)** 离线超过阈值的维**先剔除**(否则水位被永久钉死)
func TestGlobalWatermarkExcludesStaleClients(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if err := syncfeed.ReportCursor(ctx, f.database.Pool, syncfeed.CursorReport{
		ClientID: "cli-stale", SpaceID: f.spaceID, LastSeq: 1,
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	// 把它的上报时间推到 40 天前
	if _, err := f.database.Pool.Exec(ctx, `
UPDATE sync_cursors SET last_seen_at = now() - interval '40 days'
 WHERE client_id = 'cli-stale' AND space_id = $1`, f.spaceID); err != nil {
		t.Fatalf("改时间失败: %v", err)
	}
	if err := syncfeed.ReportCursor(ctx, f.database.Pool, syncfeed.CursorReport{
		ClientID: "cli-live", SpaceID: f.spaceID, LastSeq: 500,
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	w, err := syncfeed.GlobalWatermark(ctx, f.database.Pool, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("算水位失败: %v", err)
	}
	if w.SkippedOffline != 1 {
		t.Fatalf("应剔除 1 个离线维,实际 %d", w.SkippedOffline)
	}
	if w.Seq != 500 {
		t.Fatalf("剔除后水位应为 500,实际 %d(被离线客户端钉死说明剔除没生效)", w.Seq)
	}
}

// **验收⑤(游标)** 清理只删水位之前的行,并把已清理水位推进到真实边界
func TestPruneOnlyRemovesBeforeWatermarkAndAdvancesFloor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	var seqs []int64
	for i := 0; i < 5; i++ {
		id := f.mkFile(t, nameI(i))
		seqs = append(seqs, f.appendTx(t, syncfeed.Event{
			SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
			ParentID: f.rootID, Name: nameI(i),
		}))
	}
	// 水位 3:只应删 seq<=3 的三条(保留期给足)
	if err := f.database.InTx(ctx, func(tx pgx.Tx) error {
		_, err := syncfeed.Prune(ctx, tx, map[string]int64{f.spaceID: seqs[2]}, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	page, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, seqs[2], 100)
	if err != nil {
		t.Fatalf("拉变更失败: %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("水位之后应剩 2 条,实际 %d", len(page.Items))
	}
	if floor := f.floorSeq(t); floor != seqs[2] {
		t.Fatalf("水位应推进到 %d,实际 %d", seqs[2], floor)
	}
	// 保留期未到 → 一行都不该删(保留期与水位取更保守者)
	if err := f.database.InTx(ctx, func(tx pgx.Tx) error {
		_, err := syncfeed.Prune(ctx, tx, map[string]int64{f.spaceID: seqs[4]}, time.Now().Add(-time.Hour))
		return err
	}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	page2, err := syncfeed.Changes(ctx, f.database.Pool, f.spaceID, seqs[2], 100)
	if err != nil {
		t.Fatalf("拉变更失败: %v", err)
	}
	if len(page2.Items) != 2 {
		t.Fatalf("保留期未到不应删除,实际剩 %d 条", len(page2.Items))
	}
}

// **验收④(接口)** last_seq 只被**文件变更**推进:游标上报不占 seq
//
// `member_changed` / `quota.warning` 这类"不是某一行被改"的事件走 SSE 独立类型,
// 不进 sync_feed —— 否则客户端会把它们当文件变更去拉,而 seq 空间被污染后
// has_more 与保留期计算都会失真。
func TestCursorReportDoesNotConsumeSeq(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	before := f.lastSeq(t)
	if err := syncfeed.ReportCursor(ctx, f.database.Pool, syncfeed.CursorReport{
		ClientID: "cli-x", SpaceID: f.spaceID, LastSeq: 999,
	}); err != nil {
		t.Fatalf("上报失败: %v", err)
	}
	if after := f.lastSeq(t); after != before {
		t.Fatalf("游标上报不该推进 change_seq(前 %d 后 %d)", before, after)
	}
	// 而非法的 kind 必须被拒绝(与表的 CHECK 同源,避免"业务放行、DB 拒绝"的 500)
	if _, err := syncfeed.AppendEvent(ctx, f.database.Pool, syncfeed.Event{
		SpaceID: f.spaceID, FileID: f.rootID, Kind: "member_changed",
	}); err == nil {
		t.Fatal("非文件事件不该能写进 sync_feed")
	}
}

// HeadSeq 返回当前最大 seq(首次同步的起点)
func TestHeadSeq(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.mkFile(t, "head.txt")
	seq := f.appendTx(t, syncfeed.Event{
		SpaceID: f.spaceID, FileID: id, Kind: model.FeedCreated, Version: 1,
		ParentID: f.rootID, Name: "head.txt",
	})
	head, err := syncfeed.HeadSeq(ctx, f.database.Pool, f.spaceID)
	if err != nil {
		t.Fatalf("取头指针失败: %v", err)
	}
	if head != seq {
		t.Fatalf("头指针应为 %d,实际 %d", seq, head)
	}
}

// seqsOf 返回本空间最近的 n 条 change_seq(升序),用于"水位边界"类断言。
//
// 必须查真实值:change_seq 的种子是 epoch 秒(00001 里 spaces.last_seq 的默认值),
// 所以写死小整数(1/2)会一条都匹配不到 —— 那正是本文件先前假失败的原因。
func (f *fixture) seqsOf(t *testing.T, n int) []int64 {
	t.Helper()
	rows, err := f.database.Pool.Query(context.Background(),
		`SELECT change_seq FROM sync_feed WHERE space_id = $1 ORDER BY change_seq LIMIT $2`,
		f.spaceID, n)
	if err != nil {
		t.Fatalf("查 seq 失败: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		out = append(out, v)
	}
	if len(out) < n {
		t.Fatalf("期望至少 %d 条变更,实际 %d", n, len(out))
	}
	return out
}

func nameI(i int) string { return "f-" + string(rune('a'+i)) + ".txt" }
