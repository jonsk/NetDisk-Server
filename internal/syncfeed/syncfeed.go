// Package syncfeed 是**变更流**(sync_feed)的读写实现(6.4 T2-2 / 6.3 / 6.9 / R-17)。
//
// # 它解决什么问题
//
// 客户端同步曾经靠"每 5 分钟全量扫描远端目录" —— IO 随文件数线性增长(6.9 V2.13)。
// 变更流把"远端发生了什么"变成一条**单调递增的游标**:客户端报"我看到 since=N",
// 服务端回"从 N 之后有这些变化"。于是常态下客户端只读增量,全量扫描只在
// 首次配置目录与游标超窗时发生。
//
// # 四条硬纪律(写错都会静默出错)
//
//  1. **与元数据同事务写**:变更行必须和 files 行的改动在同一事务里提交。
//     分开写会出现"文件变了但客户端不知道"(客户端永远看不到这次变更,直到下次
//     偶然全量扫描),或"客户端收到变更但元数据回滚了"(读到不存在的文件)。
//  2. **seq 是每空间单调的**:用 `next_change_seq(space_id)`(spaces.last_seq + 1)
//     在同一事务里取号。之所以不用全局 BIGSERIAL:全局序列的号段会被**别的空间**
//     的写入消耗,于是同一空间的 change_seq 出现空洞 —— 而客户端是用
//     `since=最大已见 seq` 做游标的,空洞会让"我还漏了多少"无法判断,
//     并且 `has_more`/`next_seq` 的语义会变得依赖全库写入速率。
//  3. **目录自身的变更也要入 feed**。只记文件会让"新建目录"这类结构变化丢失,
//     客户端就必须靠全量扫描来发现空目录 —— 那正是本机制要消灭的东西。
//  4. **不占 seq 的事件另走通道**(`member_changed` / `quota.warning`)。
//     它们不是"某一行被改",没有 file_id/version;硬塞进 sync_feed 会让
//     客户端把"成员变更"当成文件变更去拉取,而 seq 空间被非文件事件污染后,
//     `has_more` 与保留期计算都会失真。它们走 SSE 的独立事件类型(6.9)。
//
// # 保留期与"超窗"
//
// sync_feed 不能无限增长:清理任务按"全局游标水位"删旧行(见 Prune)。
// 删行会留下 `spaces.feed_floor_seq` 作为**已清理水位** —— 客户端报的 since
// 小于它,说明需要的行已经被删掉,必须回 **409 cursor_expired** 让客户端
// 走全量清单。没有这个水位的话,过老游标只会**静默返回空**,
// 客户端以为自己同步完了,而实际上漏掉了一整段(这是最难发现的一类同步 bug)。
package syncfeed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// DefaultLimit / MaxLimit 是 `/changes` 的分页边界。
//
// 上限刻意存在且不大:客户端一次能处理的变更数量有限,而"一次拉 10 万条"
// 会让服务端在单个请求里做巨量序列化(内存与延迟双爆);
// 分页 + has_more 让客户端自己控制节奏。
const (
	DefaultLimit = 1000
	MaxLimit     = 5000
)

// ErrCursorExpired 表示客户端的 since 早于本空间已清理水位。
//
// 调用方必须把它转成 **409 + cursor_expired**(而不是空结果):
// 客户端据此走全量清单重建本地状态(6.4)。
var ErrCursorExpired = errors.New("syncfeed: 游标已超窗(所需变更已被清理)")

// Event 是一条待写入的变更。
//
// 字段与 sync_feed 的列一一对应;不用的字段留零值。
type Event struct {
	SpaceID string
	FileID  string
	Kind    string
	Version int64
	// ParentID / Name 是**变更后**的位置与名字(6.4:客户端据这两列就能更新本地树,
	// 不必再回查文件详情)
	ParentID string
	Name     string
	// OldParentID 只在 moved 时有意义
	OldParentID string
}

// Writer 写入变更流(finalize.FeedWriter 的实现)。
//
// 刻意**只依赖 repo.Querier**:调用方必须在自己的事务里调用它 ——
// 这正是纪律 1("与元数据同事务")在类型上的体现(拿不到池就拿不到独立事务)。
type Writer struct{}

// Append 是 finalize.FeedWriter 的窄接口实现(只带 fileID + kind)。
//
// 它会**回查文件行**补齐 parent_id/name/version:定稿路径调用时手上只有
// file_id 与 kind,而客户端需要"变更后在哪、叫什么"才能更新本地树。
// 回查在**同一事务**内进行,看到的正是本次写入后的新值。
func (Writer) Append(ctx context.Context, q repo.Querier, spaceID, fileID, kind string) (int64, error) {
	var (
		parentID string
		name     string
		version  int64
	)
	if err := q.QueryRow(ctx, `
SELECT coalesce(parent_id::text,''), name, version FROM files WHERE id = $1`, fileID).
		Scan(&parentID, &name, &version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 变更已被同事务删除(例如"建了又删"):不再写 feed,返回 0 表示无变更。
			// 不报错是刻意的:调用方(定稿)已经成功,不该因为变更流没有可报的内容而失败。
			return 0, nil
		}
		return 0, err
	}
	return AppendEvent(ctx, q, Event{
		SpaceID: spaceID, FileID: fileID, Kind: kind,
		Version: version, ParentID: parentID, Name: name,
	})
}

// AppendEvent 写入一条变更并返回它的 change_seq(在调用方事务内)。
//
// 同时提供**包级函数**与 `Writer` 上的方法:包级函数给"手上已经有完整字段"的
// 调用方(改名/移动/删除/共享都在改动**之前**就拿到了新旧位置),
// 方法则让 `Writer` 满足各层声明的窄接口(便于装配与测试替换)。
func (Writer) AppendEvent(ctx context.Context, q repo.Querier, e Event) (int64, error) {
	return AppendEvent(ctx, q, e)
}

// AppendEvent 写入一条变更并返回它的 change_seq(在调用方事务内)。
func AppendEvent(ctx context.Context, q repo.Querier, e Event) (int64, error) {
	if !model.ValidFeedKind(e.Kind) {
		return 0, fmt.Errorf("syncfeed: 非法的变更类型 %q", e.Kind)
	}
	var seq int64
	err := q.QueryRow(ctx, `
INSERT INTO sync_feed (space_id, change_seq, file_id, kind, version, parent_id, name, old_parent_id)
VALUES ($1, next_change_seq($1), $2, $3, NULLIF($4,0), NULLIF($5,'')::uuid, NULLIF($6,''), NULLIF($7,'')::uuid)
RETURNING change_seq`,
		e.SpaceID, nullableUUID(e.FileID), e.Kind, e.Version,
		e.ParentID, e.Name, e.OldParentID).Scan(&seq)
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// Item 是返回给客户端的一条变更(6.4 的响应契约字段)。
type Item struct {
	ChangeSeq   int64  `json:"change_seq"`
	FileID      string `json:"file_id"`
	Kind        string `json:"kind"`
	Version     int64  `json:"version"`
	ParentID    string `json:"parent_id"`
	Name        string `json:"name"`
	OldParentID string `json:"old_parent_id,omitempty"`
}

// Page 是 `/changes` 的响应体(契约见 6.3)。
type Page struct {
	Items []Item `json:"items"`
	// NextSeq 是客户端下次应当携带的 since(本页最后一条的 seq;没有条目时回显入参)
	NextSeq int64 `json:"next_seq"`
	HasMore bool  `json:"has_more"`
	// SpaceID 回显,便于客户端核对(一个客户端同时同步多个空间)
	SpaceID string `json:"space_id"`
}

// Changes 拉取 since 之后的变更(keyset 分页,**不用 OFFSET**)。
//
// 三条保证:
//   - **幂等**:同一个 since 重复请求,只要期间没有新写入,结果完全相同
//     (keyset 查询天然如此;OFFSET 分页在并发写入下会漏条或重复)。
//   - **超窗即报错**:since < feed_floor_seq → ErrCursorExpired,绝不静默返回空。
//   - limit 有上限:超过 MaxLimit 直接夹到 MaxLimit(不报错 —— 客户端要更多
//     就自己再翻一页,报错只会让"想要 1 万条"的客户端多写一段重试逻辑)。
func Changes(ctx context.Context, q repo.Querier, spaceID string, since int64, limit int) (*Page, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	if since < 0 {
		return nil, fmt.Errorf("syncfeed: since 不能为负")
	}

	// 水位检查与取数放在**同一个快照**里做:否则"检查时没超窗、取数时已被清理"
	// 会返回一个残缺的页(客户端据此推进游标 → 永久漏掉被清掉的那段)。
	rows, err := q.Query(ctx, `
WITH floor AS (
    SELECT feed_floor_seq, last_seq FROM spaces WHERE id = $1
)
SELECT f.change_seq, coalesce(f.file_id::text,''), f.kind, coalesce(f.version,0),
       coalesce(f.parent_id::text,''), coalesce(f.name,''), coalesce(f.old_parent_id::text,''),
       (SELECT feed_floor_seq FROM floor)
  FROM sync_feed f
 WHERE f.space_id = $1 AND f.change_seq > $2
 ORDER BY f.change_seq
 LIMIT $3`, spaceID, since, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	page := &Page{Items: make([]Item, 0, limit), NextSeq: since, SpaceID: spaceID}
	var floor int64
	floorSeen := false
	for rows.Next() {
		var it Item
		var f int64
		if err := rows.Scan(&it.ChangeSeq, &it.FileID, &it.Kind, &it.Version,
			&it.ParentID, &it.Name, &it.OldParentID, &f); err != nil {
			return nil, err
		}
		if !floorSeen {
			floor, floorSeen = f, true
		}
		page.Items = append(page.Items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 没有可返回的行时也要拿到水位:用一次独立查询补齐(空页的"超窗"判定同样重要)
	if !floorSeen {
		if err := q.QueryRow(ctx,
			`SELECT feed_floor_seq FROM spaces WHERE id = $1`, spaceID).Scan(&floor); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, repo.ErrNotFound
			}
			return nil, err
		}
	}
	if since < floor {
		return nil, ErrCursorExpired
	}

	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		page.HasMore = true
	}
	if n := len(page.Items); n > 0 {
		page.NextSeq = page.Items[n-1].ChangeSeq
	}
	return page, nil
}

// HeadSeq 返回某空间当前的 change_seq 上界(spaces.last_seq)。
//
// 客户端首次同步时拿它当"起点":从此刻之后的变化才需要增量拉取,
// 之前的靠全量清单覆盖。少了这个值,客户端只能从 0 开始拉 ——
// 那等于把历史变更全看一遍(而且很可能已超窗直接 409)。
func HeadSeq(ctx context.Context, q repo.Querier, spaceID string) (int64, error) {
	var seq, floor int64
	if err := q.QueryRow(ctx,
		`SELECT last_seq, feed_floor_seq FROM spaces WHERE id = $1`, spaceID).Scan(&seq, &floor); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, repo.ErrNotFound
		}
		return 0, err
	}
	return seq, nil
}

// CursorReport 是一次客户端游标上报。
type CursorReport struct {
	ClientID string
	SpaceID  string
	LastSeq  int64
}

// ReportCursor 记录客户端游标,**单维 (client_id, space_id) 取 max**。
//
// 为什么取 max 而不是直接覆盖:
//   - 客户端的上报可能乱序到达(网络重发、多线程)。
//     覆盖会让游标**倒退**,而游标倒退意味着"把已经处理过的变更再拉一遍"
//     (无害但浪费);更糟的是它会把全局水位算低,拖住清理任务。
//   - 取 max 是单调的:乱序与重放都不会让已确认的进度丢失。
func ReportCursor(ctx context.Context, q repo.Querier, r CursorReport) error {
	_, err := q.Exec(ctx, `
INSERT INTO sync_cursors (client_id, space_id, last_seq, last_seen_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (client_id, space_id) DO UPDATE
   SET last_seq = GREATEST(sync_cursors.last_seq, EXCLUDED.last_seq),
       last_seen_at = now()`,
		r.ClientID, r.SpaceID, r.LastSeq)
	return err
}

// Watermark 是全局清理水位及其依据。
type Watermark struct {
	// Seq 是"所有活跃客户端都已确认"的位置:清理只删 <= Seq 的行
	Seq int64
	// Clients 是参与计算的 (client, space) 对数,便于监控"水位为何上不去"
	Clients int
	// SkippedOffline 是因离线过久被剔除的维数
	SkippedOffline int
}

// GlobalWatermark 计算全局清理水位:**跨全部 (client_id, space_id) 取 min**。
//
// 两条纪律(6.4 P1-3/P2-4):
//
//  1. **取 min,不是 max**。取 max 会让"某个客户端已经看到 1000"变成
//     "可以把 1000 之前的都删了" —— 而另一个只看到 10 的客户端会因此
//     永久丢失 11..1000 这段变更(它的游标没超窗,所以也不会被要求全量重建,
//     它会以为自己是最新的)。宁可晚清,不可多清。
//  2. **离线过久的客户端先剔除**。否则一个卸载了客户端的账号会把水位永久钉死
//     (它再也不会上报),sync_feed 无限增长。剔除判据是 cursor_offline_days(默认 30),
//     被剔除的客户端下次回来时游标必然超窗 → 走全量清单,语义闭合。
func GlobalWatermark(ctx context.Context, q repo.Querier, offlineBefore time.Time) (*Watermark, error) {
	var w Watermark
	// min 在 SQL 里做,空集合(min=NULL)时用 0 表示"没有可清理的依据"
	var minSeq *int64
	if err := q.QueryRow(ctx, `
SELECT min(last_seq), count(*)
  FROM sync_cursors
 WHERE last_seen_at >= $1`, offlineBefore).Scan(&minSeq, &w.Clients); err != nil {
		return nil, err
	}
	if minSeq != nil {
		w.Seq = *minSeq
	}
	if err := q.QueryRow(ctx, `
SELECT count(*) FROM sync_cursors WHERE last_seen_at < $1`, offlineBefore).Scan(&w.SkippedOffline); err != nil {
		return nil, err
	}
	return &w, nil
}

// Prune 清理水位之前的变更行,并把 `spaces.feed_floor_seq` 推进到实际删掉的最大 seq。
//
// **必须同时推进水位**:只删行不推进水位,过期游标就会静默拿到空结果
// (见包注释最后一段)。两条语句在**同一事务**里,不可能只发生一半。
//
// 返回 (删除行数, 每个空间推进后的水位)。
func Prune(ctx context.Context, q repo.Querier, upTo map[string]int64, keepBefore time.Time) (int64, error) {
	if len(upTo) == 0 {
		return 0, nil
	}
	var deleted int64
	for spaceID, seq := range upTo {
		if seq <= 0 {
			continue
		}
		// 只删"水位之前"且"够老"的行(keepBefore 是保留期下限,
		// 与水位取更保守者:保留期是为了排障与重放,水位是为了客户端进度)
		tag, err := q.Exec(ctx, `
DELETE FROM sync_feed
 WHERE space_id = $1 AND change_seq <= $2 AND created_at < $3`,
			spaceID, seq, keepBefore)
		if err != nil {
			return deleted, err
		}
		n := tag.RowsAffected()
		deleted += n
		if n == 0 {
			continue
		}
		// 水位推进到**实际删掉的最大 seq**,而不是请求的 seq:
		// 若因保留期少删了一些,水位必须停在真实边界,否则那些行会被
		// 错误地判定为"已清理"→ 客户端明明能拿到却被判超窗。
		var maxDeleted *int64
		if err := q.QueryRow(ctx, `
SELECT max(change_seq) FROM sync_feed
 WHERE space_id = $1 AND change_seq <= $2`, spaceID, seq).Scan(&maxDeleted); err != nil {
			return deleted, err
		}
		// 上一句查的是**剩余**的最大 seq;真正删掉的最大 seq 是
		// "请求水位"与"剩余最大"之间的那一段的边界。
		floor := seq
		if maxDeleted != nil && *maxDeleted < seq {
			// 仍有 <= seq 的行没被删(保留期未到)→ 水位只能推到"剩余最大"之下,
			// 但那样会与"客户端能拿到"矛盾,所以这里选择**不推进到 seq 之后**,
			// 而是推到剩余最大(即这些行还在,水位不该越过它们)。
			floor = *maxDeleted - 1
			if floor < 0 {
				floor = 0
			}
		}
		if _, err := q.Exec(ctx, `
UPDATE spaces SET feed_floor_seq = GREATEST(feed_floor_seq, $2), updated_at = now()
 WHERE id = $1`, spaceID, floor); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

func nullableUUID(v string) any {
	if v == "" {
		return nil
	}
	return v
}
