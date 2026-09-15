package syncfeed_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 本文件补上"游标水位"(6.4 T2-4 / 6.9 / R-17)的表驱动单测缺口。
//
// syncfeed_test.go 那 14 条是"每个结论一个用例"(可读性好),这里换一种组织:
// 同一类语义的所有边界放进一张表,逐条钉死**实现真正承诺的东西**。
//
// 三处实现细节决定了所有断言,读代码时请注意:
//   - ReportCursor 用 `GREATEST(sync_cursors.last_seq, EXCLUDED.last_seq)`:单维水位只增不减;
//   - Prune 用 `GREATEST(spaces.feed_floor_seq, ...)`:清理水位也只增不减,且只有真删了行才推进;
//   - Changes 在 `since < feed_floor_seq` 时报 ErrCursorExpired(不是空页、不是裁剪),
//     `since == floor` 放行;limit<=0 取 DefaultLimit、>MaxLimit 夹到 MaxLimit。
//
// 依赖真实 PG(NETDISK_TEST_DSN),夹具与 helper 沿用 syncfeed_test.go 的 setup/mkFile。

// seedFeed 在一个事务里用**真实写入口**写 n 条变更,返回升序的 change_seq。
//
// 为什么不用直接 INSERT:change_seq 必须由 next_change_seq() 取号。手写 seq 会绕过
// "每空间单调"这条纪律,那么基于 seq 的分页/水位边界断言就不再对应生产行为。
func (f *fixture) seedFeed(t *testing.T, n int) []int64 {
	t.Helper()
	seqs := make([]int64, 0, n)
	if err := f.database.InTx(context.Background(), func(tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			s, err := syncfeed.AppendEvent(context.Background(), tx, syncfeed.Event{
				SpaceID: f.spaceID, Kind: model.FeedCreated,
				Name: fmt.Sprintf("bulk-%05d", i),
			})
			if err != nil {
				return err
			}
			seqs = append(seqs, s)
		}
		return nil
	}); err != nil {
		t.Fatalf("播种 %d 条变更失败: %v", n, err)
	}
	return seqs
}

// pruneTo 在一个事务里把本空间的水位清理到 seq,返回实际删除的行数。
func (f *fixture) pruneTo(t *testing.T, seq int64, keepBefore time.Time) int64 {
	t.Helper()
	var deleted int64
	if err := f.database.InTx(context.Background(), func(tx pgx.Tx) error {
		d, err := syncfeed.Prune(context.Background(), tx, map[string]int64{f.spaceID: seq}, keepBefore)
		deleted = d
		return err
	}); err != nil {
		t.Fatalf("清理水位到 %d 失败: %v", seq, err)
	}
	return deleted
}

// readCursor 读本空间的某个客户端游标;第二个返回值表示该 (client, space) 维是否存在。
func (f *fixture) readCursor(t *testing.T, clientID string) (int64, bool) {
	t.Helper()
	var seq int64
	err := f.database.Pool.QueryRow(context.Background(),
		`SELECT last_seq FROM sync_cursors WHERE client_id = $1 AND space_id = $2`,
		clientID, f.spaceID).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("读 (client=%s, space=%s) 游标失败: %v", clientID, f.spaceID, err)
	}
	return seq, true
}

// whySuffix 把"哪种错实现会在这里失败"附在断言消息后面。
func whySuffix(why string) string {
	if why == "" {
		return ""
	}
	return "\n  为什么: " + why
}

// TestCursorWatermarkMonotonicTable 钉死"单维游标只增不减"。
//
// 客户端上报会乱序/重放/清零到达;覆盖语义(最后一次胜)会让已确认的进度丢失,
// 而 GlobalWatermark 取的是全维 min —— 一个被覆盖成 0 的维会把**全局水位永久钉死**。
func TestCursorWatermarkMonotonicTable(t *testing.T) {
	cases := []struct {
		name    string
		reports []int64
		want    int64
		why     string
	}{
		{"逐次推进", []int64{10, 20, 30}, 30, ""},
		{"更小的上报不得让水位倒退", []int64{50, 10, 30}, 50,
			"网络重发/多线程会让上报乱序;覆盖语义(最后一次胜)会得到 30,已确认的进度就丢了"},
		{"重复上报同一个值", []int64{7, 7}, 7, ""},
		{"零值不得把水位打回 0", []int64{5, 0}, 5,
			"新装/重置的客户端会报 0;覆盖会把全局水位(min)算低,清理任务被永久拖住"},
		{"先大后小再回升", []int64{9, 3, 4}, 9, ""},
	}

	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			clientID := fmt.Sprintf("tbl-mono-%d", i)
			for _, seq := range c.reports {
				if err := syncfeed.ReportCursor(context.Background(), f.database.Pool, syncfeed.CursorReport{
					ClientID: clientID, SpaceID: f.spaceID, LastSeq: seq,
				}); err != nil {
					t.Fatalf("上报 last_seq=%d 失败: %v", seq, err)
				}
			}
			got, exists := f.readCursor(t, clientID)
			if !exists {
				t.Fatalf("上报 %v 之后 (client=%s, space=%s) 仍无游标行", c.reports, clientID, f.spaceID)
			}
			if got != c.want {
				t.Errorf("依次上报 %v 之后 last_seq = %d,期望 %d%s", c.reports, got, c.want, whySuffix(c.why))
			}
		})
	}
}

// spaceReport 是一次"向某个空间"的游标上报,空间用 A/B 指代。
type spaceReport struct {
	space string
	seq   int64
}

// TestCursorWatermarkPerSpaceTable 钉死"游标按 (client_id, space_id) 独立"。
//
// 一个客户端会同时同步多个空间;若游标只按 client_id 存,一个空间的进度会覆盖另一个空间,
// 落后的那个空间就会永久漏变更(它的 since 被抬到了自己没看过的位置)。
func TestCursorWatermarkPerSpaceTable(t *testing.T) {
	cases := []struct {
		name        string
		reports     []spaceReport
		wantA       int64
		wantB       int64
		wantBExists bool
		why         string
	}{
		{"两个空间各记各的水位", []spaceReport{{"A", 100}, {"B", 7}}, 100, 7, true, ""},
		{"A 的倒退上报不影响 B", []spaceReport{{"A", 100}, {"B", 7}, {"A", 50}}, 100, 7, true,
			"水位按 (client_id, space_id) 独立;若只按 client_id 存,B 的 7 会被 A 的 100 覆盖成 100"},
		{"只上报 A 时 B 没有游标行", []spaceReport{{"A", 5}}, 5, 0, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fa, fb := setup(t), setup(t)
			const clientID = "cli-shared"
			for _, r := range c.reports {
				sid := fa.spaceID
				if r.space == "B" {
					sid = fb.spaceID
				}
				if err := syncfeed.ReportCursor(context.Background(), fa.database.Pool, syncfeed.CursorReport{
					ClientID: clientID, SpaceID: sid, LastSeq: r.seq,
				}); err != nil {
					t.Fatalf("向空间 %s 上报 last_seq=%d 失败: %v", r.space, r.seq, err)
				}
			}
			gotA, existsA := fa.readCursor(t, clientID)
			if !existsA {
				t.Fatalf("空间 A 上应存在游标行(reports=%+v)", c.reports)
			}
			if gotA != c.wantA {
				t.Errorf("空间 A 的 last_seq = %d,期望 %d(reports=%+v)%s", gotA, c.wantA, c.reports, whySuffix(c.why))
			}
			gotB, existsB := fb.readCursor(t, clientID)
			if existsB != c.wantBExists {
				t.Errorf("空间 B 游标行存在性 = %v,期望 %v(last_seq=%d)", existsB, c.wantBExists, gotB)
			}
			if c.wantBExists && gotB != c.wantB {
				t.Errorf("空间 B 的 last_seq = %d,期望 %d(reports=%+v)%s", gotB, c.wantB, c.reports, whySuffix(c.why))
			}
		})
	}
}

// TestChangesCursorWatermarkTable 钉死"since 与水位比较"的真实语义。
//
// 实现只有一种处理:`since < floor` → ErrCursorExpired(409 + 全量重建)。
// 它**不做**裁剪、也**不**返回空页;空页在那条路径上永远不会出现(空表也一样报超窗)。
func TestChangesCursorWatermarkTable(t *testing.T) {
	sinceZero := func([]int64, int64) int64 { return 0 }
	sinceFloorMinus1 := func(_ []int64, floor int64) int64 { return floor - 1 }
	sinceFloor := func(_ []int64, floor int64) int64 { return floor }
	sinceAt := func(idx int) func([]int64, int64) int64 {
		return func(seqs []int64, _ int64) int64 { return seqs[idx] }
	}
	sinceNegative := func([]int64, int64) int64 { return -1 }

	cases := []struct {
		name        string
		seed        int
		pruneIdx    int // -1 = 不清理;否则以第 pruneIdx 条 seq 为水位清理
		since       func(seqs []int64, floor int64) int64
		wantExpired bool
		wantItems   int
		wantErrText string // 非空:期望普通 error(不是超窗哨兵)且消息含该子串
		why         string
	}{
		{
			name: "since 早于水位:报超窗", seed: 5, pruneIdx: 1, since: sinceFloorMinus1,
			wantExpired: true,
			why:         "静默返回空会让客户端以为已同步完,永久漏掉被清掉的那一段(最难发现的一类同步 bug)",
		},
		{
			name: "since 正好等于水位:不超窗,返回水位之后的行", seed: 5, pruneIdx: 1, since: sinceFloor,
			wantItems: 3,
			why:       "边界是 since < floor 才算超窗;写成 <= 会把\"正好从水位开始\"的客户端打回全量重建",
		},
		{
			name: "since 大于水位:跳过已读的行", seed: 5, pruneIdx: 1, since: sinceAt(2),
			wantItems: 2,
		},
		{
			name: "since 等于最大 seq:空页且回显入参", seed: 5, pruneIdx: 1, since: sinceAt(4),
			wantItems: 0,
		},
		{
			name: "从未清理(水位 0):since=0 可全量拉取", seed: 3, pruneIdx: -1, since: sinceZero,
			wantItems: 3,
		},
		{
			name: "清理到最新 seq 后 since=0 超窗", seed: 3, pruneIdx: 2, since: sinceZero,
			wantExpired: true,
		},
		{
			name: "since 为负:调用方错误,不是超窗", seed: 1, pruneIdx: -1, since: sinceNegative,
			wantErrText: "不能为负",
			why:         "负 since 与\"游标超窗\"是两类问题:前者该 400(调用方 bug),后者该 409 + 全量重拉;混成一个哨兵会让客户端做错决定",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			seqs := f.seedFeed(t, c.seed)

			floor := int64(0)
			if c.pruneIdx >= 0 {
				f.pruneTo(t, seqs[c.pruneIdx], time.Now().Add(time.Hour))
				floor = f.floorSeq(t)
				if floor != seqs[c.pruneIdx] {
					t.Fatalf("前置:清理后水位 = %d,期望 %d", floor, seqs[c.pruneIdx])
				}
			}
			since := c.since(seqs, floor)

			page, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, since, 100)
			if c.wantErrText != "" {
				if err == nil {
					t.Fatalf("since=%d 应报含 %q 的错误,实际成功返回 %+v", since, c.wantErrText, page)
				}
				if errors.Is(err, syncfeed.ErrCursorExpired) {
					t.Errorf("since=%d 是调用方错误,不该被当成超窗哨兵: %v", since, err)
				}
				if !strings.Contains(err.Error(), c.wantErrText) {
					t.Errorf("since=%d 的错误消息 = %q,期望包含 %q", since, err.Error(), c.wantErrText)
				}
				if page != nil {
					t.Errorf("报错时必须返回 nil 页,实际 %+v", page)
				}
				return
			}
			if c.wantExpired {
				if !errors.Is(err, syncfeed.ErrCursorExpired) {
					t.Fatalf("since=%d < 水位 %d 应报 ErrCursorExpired(而不是空页/裁剪),实际 err=%v page=%+v%s",
						since, floor, err, page, whySuffix(c.why))
				}
				if page != nil {
					t.Errorf("超窗必须返回 nil 页(客户端据 err 走全量),实际 %+v", page)
				}
				return
			}
			if err != nil {
				t.Fatalf("since=%d(水位 %d)不应报错,实际 %v%s", since, floor, err, whySuffix(c.why))
			}
			if len(page.Items) != c.wantItems {
				t.Errorf("since=%d 返回 %d 条,期望 %d(实际 items=%+v)%s",
					since, len(page.Items), c.wantItems, page.Items, whySuffix(c.why))
			}
			for _, it := range page.Items {
				if it.ChangeSeq <= since {
					t.Errorf("返回了 <= since 的行: change_seq=%d since=%d", it.ChangeSeq, since)
				}
			}
			if c.wantItems == 0 {
				if page.NextSeq != since {
					t.Errorf("空页必须回显 next_seq=入参 since=%d,实际 %d", since, page.NextSeq)
				}
			} else if last := page.Items[len(page.Items)-1].ChangeSeq; page.NextSeq != last {
				t.Errorf("next_seq = %d,期望页内最后一条的 change_seq %d", page.NextSeq, last)
			}
		})
	}
}

// TestChangesLimitBoundaryTable 钉死分页边界:limit<=0 取 DefaultLimit,>MaxLimit 夹到 MaxLimit。
//
// 要真正观察到夹取,必须种出比上限更多的行(1002 / 5002 条),否则 "limit=0 返回 3 条"
// 这种断言既可能来自 DefaultLimit 也可能来自"根本没夹";这里用逐条精确条数把它分开。
func TestChangesLimitBoundaryTable(t *testing.T) {
	cases := []struct {
		name        string
		seed        int
		limit       int
		wantItems   int
		wantHasMore bool
		why         string
	}{
		{"limit=0 取 DefaultLimit", 1002, 0, syncfeed.DefaultLimit, true,
			"0 不能被当成\"零条\":直接 LIMIT 0 会返回空页,客户端永远同步不动"},
		{"limit 为负数取 DefaultLimit", 1002, -7, syncfeed.DefaultLimit, true, ""},
		{"limit=1 最小页", 3, 1, 1, true, ""},
		{"limit 超过 MaxLimit 夹到 MaxLimit(不报错)", 5002, syncfeed.MaxLimit + 1, syncfeed.MaxLimit, true,
			"报错会让\"想要更多\"的客户端多写一段重试逻辑;夹住才对(客户端自己翻页)"},
		{"limit 天文数字同样夹到 MaxLimit", 5002, 1 << 30, syncfeed.MaxLimit, true, ""},
		{"limit 大于行数:一次返回全部且 has_more=false", 5, 100, 5, false, ""},
		{"空空间:空页且 has_more=false", 0, 0, 0, false,
			"空页也要走水位判定(表空 + 过老游标必须报超窗);这里水位是 0,所以正常空页"},
	}

	// 同一个 seed 只建一个空间(Changes 是只读的):否则 5002 行的空间要建两遍。
	// 夹具挂在父 t 上,生命周期覆盖全部子用例。
	seeds := map[int]bool{}
	for _, c := range cases {
		seeds[c.seed] = true
	}
	fixtures := map[int]*fixture{}
	seqsBySeed := map[int][]int64{}
	for seed := range seeds {
		f := setup(t)
		fixtures[seed] = f
		seqsBySeed[seed] = f.seedFeed(t, seed)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, seqs := fixtures[c.seed], seqsBySeed[c.seed]
			page, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, 0, c.limit)
			if err != nil {
				t.Fatalf("limit=%d(seed=%d)不应报错,实际 %v%s", c.limit, c.seed, err, whySuffix(c.why))
			}
			if len(page.Items) != c.wantItems {
				t.Errorf("limit=%d 返回 %d 条,期望 %d%s", c.limit, len(page.Items), c.wantItems, whySuffix(c.why))
			}
			if page.HasMore != c.wantHasMore {
				t.Errorf("limit=%d has_more=%v,期望 %v", c.limit, page.HasMore, c.wantHasMore)
			}
			if c.wantItems == 0 {
				if page.NextSeq != 0 {
					t.Errorf("空页 next_seq=%d,期望回显 since=0", page.NextSeq)
				}
				return
			}
			if page.Items[0].ChangeSeq != seqs[0] {
				t.Errorf("首页第一条 change_seq=%d,期望最小 seq %d(从 since=0 起必须从头给)",
					page.Items[0].ChangeSeq, seqs[0])
			}
			if last := page.Items[len(page.Items)-1].ChangeSeq; page.NextSeq != last {
				t.Errorf("next_seq=%d,期望页内最后一条 %d", page.NextSeq, last)
			}
			// 用 next_seq 翻下一页时必须正好接上被截掉的那一条(证明"截掉的是尾部"而不是随机丢)
			if page.NextSeq != seqs[c.wantItems-1] {
				t.Errorf("next_seq=%d,期望第 %d 条 seq %d", page.NextSeq, c.wantItems, seqs[c.wantItems-1])
			}
		})
	}
}

// TestPruneWatermarkOnlyMovesForwardTable 钉死 Prune 的水位语义。
//
// 三条都在实现里:`DELETE ... AND created_at < keepBefore`(保留期与水位取更保守者)、
// 只有真删了行才推进水位、推进用 `GREATEST(旧值, ...)`(只增不减)。
type pruneStep struct {
	atIdx      int           // 用第 atIdx 条 seq 当水位
	beyond     int64         // atIdx == -1 时:在最大 seq 之上再加这么多
	keepBefore time.Duration // 保留期下限(相对 now)
	freezeIdx  int           // >= 0:先把该条 seq 的 created_at 推到未来,模拟"保留期未到"
}

func TestPruneWatermarkOnlyMovesForwardTable(t *testing.T) {
	cases := []struct {
		name        string
		seed        int
		steps       []pruneStep
		wantDeleted []int64
		wantFloor   func(seqs []int64) []int64
		post        func(t *testing.T, f *fixture, seqs []int64, floors []int64)
		why         string
	}{
		{
			name: "清理到第 3 条:水位等于被清理到的序号", seed: 5,
			steps:       []pruneStep{{atIdx: 2, keepBefore: time.Hour}},
			wantDeleted: []int64{3},
			wantFloor:   func(seqs []int64) []int64 { return []int64{seqs[2]} },
		},
		{
			name: "清理到最大 seq:水位等于最大 seq", seed: 5,
			steps:       []pruneStep{{atIdx: 4, keepBefore: time.Hour}},
			wantDeleted: []int64{5},
			wantFloor:   func(seqs []int64) []int64 { return []int64{seqs[4]} },
		},
		{
			name: "保留期未到:一行不删、水位不动", seed: 5,
			steps:       []pruneStep{{atIdx: 4, keepBefore: -time.Hour}},
			wantDeleted: []int64{0},
			wantFloor:   func([]int64) []int64 { return []int64{0} },
			why:         "保留期是为了排障与重放,与水位取更保守者;若只要水位到了就推进,排障窗口会被吃掉",
		},
		{
			name: "第二步用更小的水位清理:不得让水位倒退", seed: 5,
			steps:       []pruneStep{{atIdx: 4, keepBefore: time.Hour}, {atIdx: 1, keepBefore: time.Hour}},
			wantDeleted: []int64{5, 0},
			wantFloor:   func(seqs []int64) []int64 { return []int64{seqs[4], seqs[4]} },
			post: func(t *testing.T, f *fixture, seqs []int64, floors []int64) {
				if _, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, seqs[1], 100); !errors.Is(err, syncfeed.ErrCursorExpired) {
					t.Errorf("第二次没删行但水位仍是 %d,since=%d 应继续被判超窗,实际 err=%v", floors[1], seqs[1], err)
				}
			},
			why: "推进语句是 GREATEST(feed_floor_seq, $2);写成就地赋值会被一个更小的入参把水位拉回去",
		},
		{
			name: "水位可以超过本空间实际最大 seq", seed: 5,
			steps:       []pruneStep{{atIdx: -1, beyond: 100, keepBefore: time.Hour}},
			wantDeleted: []int64{5},
			wantFloor:   func(seqs []int64) []int64 { return []int64{seqs[4] + 100} },
			post: func(t *testing.T, f *fixture, seqs []int64, floors []int64) {
				if _, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, seqs[4], 100); !errors.Is(err, syncfeed.ErrCursorExpired) {
					t.Errorf("水位 %d 高于所有行之后,连 since=%d 也应超窗,实际 err=%v", floors[0], seqs[4], err)
				}
			},
			why: "生产调用方给每个空间传的是同一个**全局**水位(cmd/netdisk/main.go),它可能高于本空间 last_seq;此时该空间的老游标全部被判超窗",
		},
		{
			name: "有行因保留期未到而留下:水位停在剩余最大之下", seed: 5,
			steps:       []pruneStep{{atIdx: 4, keepBefore: 30 * time.Minute, freezeIdx: 3}},
			wantDeleted: []int64{4},
			wantFloor:   func(seqs []int64) []int64 { return []int64{seqs[3] - 1} },
			post: func(t *testing.T, f *fixture, seqs []int64, floors []int64) {
				page, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, floors[0], 100)
				if err != nil {
					t.Fatalf("since=水位 %d 应能拉到留下的那一行(seq=%d),实际 err=%v", floors[0], seqs[3], err)
				}
				if len(page.Items) != 1 || page.Items[0].ChangeSeq != seqs[3] {
					t.Errorf("应恰好返回 seq=%d 这一行,实际 %+v", seqs[3], page.Items)
				}
			},
			why: "水位若越过仍然存在的行,客户端会被判超窗去全量重建 —— 明明能拿到却说拿不到",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setup(t)
			seqs := f.seedFeed(t, c.seed)
			wantFloors := c.wantFloor(seqs)
			if len(wantFloors) != len(c.steps) {
				t.Fatalf("用例自身写错了:期望水位个数 %d != 步数 %d", len(wantFloors), len(c.steps))
			}
			floors := make([]int64, 0, len(c.steps))
			for si, st := range c.steps {
				if st.freezeIdx >= 0 {
					if _, err := f.database.Pool.Exec(context.Background(),
						`UPDATE sync_feed SET created_at = now() + interval '1 hour' WHERE space_id = $1 AND change_seq = $2`,
						f.spaceID, seqs[st.freezeIdx]); err != nil {
						t.Fatalf("把第 %d 条的 created_at 推到未来失败: %v", st.freezeIdx, err)
					}
				}
				target := seqs[len(seqs)-1] + st.beyond
				if st.atIdx >= 0 {
					target = seqs[st.atIdx]
				}
				deleted := f.pruneTo(t, target, time.Now().Add(st.keepBefore))
				if deleted != c.wantDeleted[si] {
					t.Errorf("第 %d 步(target=%d, keepBefore=%v)删除 %d 行,期望 %d%s",
						si+1, target, st.keepBefore, deleted, c.wantDeleted[si], whySuffix(c.why))
				}
				floor := f.floorSeq(t)
				if floor != wantFloors[si] {
					t.Errorf("第 %d 步(target=%d)之后 feed_floor_seq=%d,期望 %d%s",
						si+1, target, floor, wantFloors[si], whySuffix(c.why))
				}
				floors = append(floors, floor)
			}
			if c.post != nil {
				c.post(t, f, seqs, floors)
			}
		})
	}
}

// TestWatermarkPerSpaceIsolationTable 钉死"清理水位按空间独立"。
//
// 水位存在 spaces.feed_floor_seq 上;它是**每空间**的。若做成全局一列,
// 清理一个空间会把另一个空间的老游标全部误判超窗(能拿到却被要求全量重建)。
func TestWatermarkPerSpaceIsolationTable(t *testing.T) {
	cases := []struct {
		name   string
		pruneA bool
		pruneB bool
		why    string
	}{
		{"只清理 A:B 的水位不动,且 B 仍能从 0 全量拉取", true, false,
			"水位按 space 独立;全局一列会让 B 的 since=0 被 A 的清理误判超窗"},
		{"只清理 B:A 的水位不动,且 A 仍能从 0 全量拉取", false, true, ""},
		{"都不清理:两边水位都为 0", false, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fa, fb := setup(t), setup(t)
			seqsA := fa.seedFeed(t, 3)
			seqsB := fb.seedFeed(t, 2)

			wantFloorA, wantFloorB := int64(0), int64(0)
			if c.pruneA {
				fa.pruneTo(t, seqsA[2], time.Now().Add(time.Hour))
				wantFloorA = seqsA[2]
			}
			if c.pruneB {
				fb.pruneTo(t, seqsB[1], time.Now().Add(time.Hour))
				wantFloorB = seqsB[1]
			}
			if got := fa.floorSeq(t); got != wantFloorA {
				t.Errorf("A 的 feed_floor_seq = %d,期望 %d%s", got, wantFloorA, whySuffix(c.why))
			}
			if got := fb.floorSeq(t); got != wantFloorB {
				t.Errorf("B 的 feed_floor_seq = %d,期望 %d%s", got, wantFloorB, whySuffix(c.why))
			}

			// 清理过的空间:since=0 必须超窗;没清理的空间:since=0 必须能全量拉取。
			check := func(tag string, f *fixture, pruned bool, wantItems int) {
				t.Helper()
				page, err := syncfeed.Changes(context.Background(), f.database.Pool, f.spaceID, 0, 100)
				if pruned {
					if !errors.Is(err, syncfeed.ErrCursorExpired) {
						t.Errorf("%s 已清理,since=0 应报超窗,实际 err=%v page=%+v", tag, err, page)
					}
					return
				}
				if err != nil {
					t.Fatalf("%s 未清理,since=0 应能全量拉取,实际 err=%v%s", tag, err, whySuffix(c.why))
				}
				if len(page.Items) != wantItems {
					t.Errorf("%s 未清理,应返回 %d 条,实际 %d", tag, wantItems, len(page.Items))
				}
			}
			check("A", fa, c.pruneA, 3)
			check("B", fb, c.pruneB, 2)
		})
	}
}
