package filesvc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 目录级操作:同步阈值分流 + 异步任务队列(BE-S7-02 / 6.11)。
//
// 队列的 SQL(领取/租约/唯一索引)也在这里测,而不是在 internal/dirops 另开一套
// 夹具:队列的每一行都挂着 spaces/users 外键,另起一套就要再写一遍"建用户、建空间、
// 精确清理"的 60 行夹具,而这里已经有现成的。取舍写在这里,免得后来者以为是漏了。

// withQueue 给夹具装上队列与阈值,并返回队列本体。
//
// 阈值设得极小(3):用例只需造 5 行就能跨过阈值,不必真的造 1001 行
// (那会让用例慢一个数量级,而阈值本身只是个整数比较)。
func (f *fixture) withQueue(t *testing.T, maxRows int) *dirops.Queue {
	t.Helper()
	q := &dirops.Queue{DB: db.AsQuerier(f.database)}
	f.svc.Queue = q
	f.svc.SyncMaxRows = maxRows
	f.svc.Feed = syncfeed.Writer{}
	return q
}

// execWorker 跑一轮 worker(与 cmd/netdisk 里装配的是同一套执行体)。
func (f *fixture) execWorker(t *testing.T) bool {
	t.Helper()
	w := &dirops.Worker{
		Queue: f.svc.Queue,
		Exec: func(ctx context.Context, task *dirops.Task) (dirops.Outcome, error) {
			switch task.Kind {
			case dirops.KindMove:
				return f.svc.ExecDirMove(ctx, task)
			case dirops.KindDelete:
				return f.svc.ExecDirDelete(ctx, task)
			}
			return dirops.Outcome{}, errors.New("未知任务类型")
		},
	}
	ran, err := w.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("worker 执行失败: %v", err)
	}
	return ran
}

// mkTree 造一棵 5 行的子树:top/{d1/{f1},d2/{f2}} + top 自身,返回各行 id。
func (f *fixture) mkTree(t *testing.T, parent, topName string) (top string, kids []string) {
	t.Helper()
	top = f.mkDirAt(t, parent, topName, 1)
	d1 := f.mkDirAt(t, top, "d1", 2)
	d2 := f.mkDirAt(t, top, "d2", 2)
	f1, _ := f.mkFileAt(t, d1, "f1.txt", []byte("tree-f1-"+topName))
	f2, _ := f.mkFileAt(t, d2, "f2.txt", []byte("tree-f2-"+topName))
	return top, []string{d1, d2, f1, f2}
}

// opState 读 op_state 标记("" = 未标记)。
func (f *fixture) opState(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT op_state FROM files WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("读 op_state 失败: %v", err)
	}
	return s
}

// countFeedRows 统计本空间内某条目的变更行数 —— 用来证明"整棵子树只写一行聚合事件"。
func (f *fixture) countFeedRows(t *testing.T, fileID string, kind string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `
SELECT count(*) FROM sync_feed WHERE space_id = $1 AND file_id = $2 AND kind = $3`,
		f.spaceID, fileID, kind).Scan(&n); err != nil {
		t.Fatalf("统计变更行失败: %v", err)
	}
	return n
}

// countSpaceFeedRows 统计本空间全部变更行数(用于断言"只有根那一行")。
func (f *fixture) countSpaceFeedRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sync_feed WHERE space_id = $1`, f.spaceID).Scan(&n); err != nil {
		t.Fatalf("统计空间变更行失败: %v", err)
	}
	return n
}

// ---- ① 同步路径:每行 version+1(6.11) ----

func TestMoveSubtreeBumpsEveryRowVersion(t *testing.T) {
	f := setup(t)
	f.svc.Feed = syncfeed.Writer{}
	ctx := context.Background()
	top, kids := f.mkTree(t, f.rootID, "top")
	dest := f.mkDirAt(t, f.rootID, "dest", 1)

	// 记录移动前的版本
	before := map[string]int64{}
	for _, id := range append([]string{top}, kids...) {
		_, _, v, _ := f.fileRow(t, id)
		before[id] = v
	}

	res, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: top, NewParentID: dest,
	})
	if err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	if res.Async {
		t.Fatalf("5 行远低于阈值,不应转异步")
	}
	if res.Entry == nil || res.Entry.ID != top {
		t.Fatalf("同步移动应返回根条目,实际 %+v", res)
	}

	// 整棵子树(含根)每行 version+1:后代的**完整路径**变了,ETag 必须跟着变,
	// 否则客户端会认为"这个文件没变化"而继续用旧 parent_id 引用它。
	for _, id := range append([]string{top}, kids...) {
		_, _, v, _ := f.fileRow(t, id)
		if v != before[id]+1 {
			t.Fatalf("条目 %s 的 version 应为 %d,实际 %d", id, before[id]+1, v)
		}
	}
	// 深度级联仍然正确(dest 在 1 层 → top 2 层 → d1/d2 3 层 → 文件 4 层)
	_, _, _, topDepth := f.fileRow(t, top)
	if topDepth != 2 {
		t.Fatalf("top 深度应为 2,实际 %d", topDepth)
	}
	_, _, _, d1Depth := f.fileRow(t, kids[0])
	if d1Depth != 3 {
		t.Fatalf("d1 深度应为 3,实际 %d", d1Depth)
	}
	_, _, _, f1Depth := f.fileRow(t, kids[2])
	if f1Depth != 4 {
		t.Fatalf("f1 深度应为 4,实际 %d", f1Depth)
	}

	// 验收④:整棵子树只写**根的一行** moved 聚合事件
	if n := f.countFeedRows(t, top, "moved"); n != 1 {
		t.Fatalf("根应恰好有 1 行 moved 事件,实际 %d", n)
	}
	if n := f.countSpaceFeedRows(t); n != 1 {
		t.Fatalf("整棵子树只应产生 1 行变更(聚合事件),实际 %d 行", n)
	}
}

// ---- ② 超阈值:转异步并返回 task_id;任务期间子请求 409 ----

func TestMoveOverThresholdEnqueuesTaskAndBlocksSubtree(t *testing.T) {
	f := setup(t)
	q := f.withQueue(t, 3)
	ctx := context.Background()
	top, kids := f.mkTree(t, f.rootID, "big")
	dest := f.mkDirAt(t, f.rootID, "dest2", 1)

	res, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: top, NewParentID: dest,
	})
	if err != nil {
		t.Fatalf("超阈值移动应入队而不是报错: %v", err)
	}
	if !res.Async || res.TaskID == "" {
		t.Fatalf("超阈值应返回 async + task_id,实际 %+v", res)
	}
	if res.Entry != nil {
		t.Fatalf("异步分支不应返回条目(条目还没动)")
	}

	// 入队即标"移动中"
	if s := f.opState(t, top); s != "moving" {
		t.Fatalf("子树根应被标为 moving,实际 %q", s)
	}
	// 目标尚未移动(异步的意义:响应返回时操作还没做)
	_, parent, _, _ := f.fileRow(t, top)
	if parent != f.rootID {
		t.Fatalf("异步分支不应立刻改 parent,实际 %s", parent)
	}

	// 验收③:任务期间"其子请求一律 409"
	blocked := []struct {
		name string
		call func() error
	}{
		{"Get(根自身)", func() error {
			_, e := f.svc.Get(ctx, f.userID, top)
			return e
		}},
		{"Get(后代)", func() error {
			_, e := f.svc.Get(ctx, f.userID, kids[2])
			return e
		}},
		{"List(根目录)", func() error {
			_, e := f.svc.List(ctx, filesvc.ListInput{UserID: f.userID, SpaceID: f.spaceID, ParentID: top})
			return e
		}},
		{"Rename(后代)", func() error {
			_, e := f.svc.Rename(ctx, filesvc.RenameInput{UserID: f.userID, FileID: kids[0], NewName: "x"})
			return e
		}},
		{"Delete(后代)", func() error {
			_, e := f.svc.Delete(ctx, f.userID, kids[2])
			return e
		}},
		{"Copy(根)", func() error {
			_, e := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: top, TargetParentID: f.rootID})
			return e
		}},
		{"重复移动(根)", func() error {
			_, e := f.svc.Move(ctx, filesvc.MoveInput{UserID: f.userID, FileID: top, NewParentID: f.rootID})
			return e
		}},
	}
	for _, c := range blocked {
		ae := asAPIErr(t, c.call())
		if ae.Status != 409 {
			t.Fatalf("%s 应 409,实际 %d(%s)", c.name, ae.Status, ae.Code)
		}
		if ae.Details["reason"] != filesvc.ReasonDirOpInProgress {
			t.Fatalf("%s 的 reason 应为 %s,实际 %v", c.name,
				filesvc.ReasonDirOpInProgress, ae.Details["reason"])
		}
	}

	// 队列里确实有一条 pending 的 move 任务
	inflight, err := q.ListInflight(ctx, top)
	if err != nil || len(inflight) != 1 {
		t.Fatalf("应恰好有 1 条在飞任务,实际 %d(err=%v)", len(inflight), err)
	}
	if inflight[0].Kind != dirops.KindMove || inflight[0].State != dirops.StatePending {
		t.Fatalf("任务类型/状态不对: %+v", inflight[0])
	}
}

// ---- ③ worker 执行异步移动:移到位、清标记、写一行聚合事件 ----

func TestDirOpWorkerCompletesAsyncMove(t *testing.T) {
	f := setup(t)
	f.withQueue(t, 3)
	ctx := context.Background()
	top, kids := f.mkTree(t, f.rootID, "worker")
	dest := f.mkDirAt(t, f.rootID, "dest3", 1)

	before := map[string]int64{}
	for _, id := range append([]string{top}, kids...) {
		_, _, v, _ := f.fileRow(t, id)
		before[id] = v
	}
	res, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: top, NewParentID: dest, BaseVersion: before[top],
	})
	if err != nil || !res.Async {
		t.Fatalf("应入队,实际 res=%+v err=%v", res, err)
	}

	if !f.execWorker(t) {
		t.Fatal("worker 应执行到 1 个任务")
	}

	// 移动到位 + 标记清除 + 版本仍按 6.11 全子树 +1
	_, parent, _, _ := f.fileRow(t, top)
	if parent != dest {
		t.Fatalf("根应已移到 dest,实际 %s", parent)
	}
	if s := f.opState(t, top); s != "" {
		t.Fatalf("任务完成后 op_state 应清空,实际 %q", s)
	}
	for _, id := range append([]string{top}, kids...) {
		_, _, v, _ := f.fileRow(t, id)
		if v != before[id]+1 {
			t.Fatalf("异步移动也应让条目 %s 的 version +1(实际 %d,期望 %d)", id, v, before[id]+1)
		}
	}
	if n := f.countSpaceFeedRows(t); n != 1 {
		t.Fatalf("异步移动也只应写 1 行聚合事件,实际 %d", n)
	}
	if n := f.countFeedRows(t, top, "moved"); n != 1 {
		t.Fatalf("应写 1 行 moved 事件,实际 %d", n)
	}

	// 任务状态可轮询:done
	inflight, err := f.svc.Queue.ListInflight(ctx, top)
	if err != nil {
		t.Fatalf("查在飞任务失败: %v", err)
	}
	if len(inflight) != 0 {
		t.Fatalf("任务完成后不应再有在飞任务,实际 %d", len(inflight))
	}
	task, err := f.svc.Queue.Get(ctx, res.TaskID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if task.State != dirops.StateDone {
		t.Fatalf("任务应为 done,实际 %s(错误 %s/%s)", task.State, task.ErrorCode, task.ErrorMessage)
	}
	if task.RowsAffected != 5 {
		t.Fatalf("rows_affected 应为 5(子树行数),实际 %d", task.RowsAffected)
	}
	if task.FinishedAt == nil {
		t.Fatal("done 的任务必须有 finished_at")
	}

	// 标记清除后子树重新可写
	if _, err := f.svc.Rename(ctx, filesvc.RenameInput{UserID: f.userID, FileID: kids[0], NewName: "d1b"}); err != nil {
		t.Fatalf("任务结束后子树应恢复可写: %v", err)
	}
}

// ---- ④ 超阈值删除:行在任务执行前**不**消失,执行后整棵释放 ----

func TestDirOpWorkerCompletesAsyncDelete(t *testing.T) {
	f := setup(t)
	f.withQueue(t, 3)
	ctx := context.Background()
	top, kids := f.mkTree(t, f.rootID, "delbig")

	res, err := f.svc.Delete(ctx, f.userID, top)
	if err != nil {
		t.Fatalf("超阈值删除应入队而不是报错: %v", err)
	}
	if !res.Async || res.TaskID == "" {
		t.Fatalf("应返回 async + task_id,实际 %+v", res)
	}
	if res.DeletedFiles != 0 || res.FreedBytes != 0 {
		t.Fatalf("异步分支的计数此刻必须是 0(真正的结果在任务里): %+v", res)
	}
	// 入队后行仍在(否则用户会看到一个"已经删了但还在列表里"的错觉)
	var exists bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM files WHERE id=$1)`, top).Scan(&exists); err != nil {
		t.Fatalf("查行失败: %v", err)
	}
	if !exists {
		t.Fatal("异步删除入队后行不应立刻消失")
	}

	if !f.execWorker(t) {
		t.Fatal("worker 应执行到 1 个任务")
	}

	// 整棵子树(5 行)都删掉了
	var left int
	if err := f.pool.QueryRow(ctx, `
WITH RECURSIVE sub AS (
  SELECT id FROM files WHERE id = $1
  UNION ALL SELECT f.id FROM files f JOIN sub ON f.parent_id = sub.id)
SELECT count(*) FROM sub`, top).Scan(&left); err != nil {
		t.Fatalf("统计残留失败: %v", err)
	}
	if left != 0 {
		t.Fatalf("异步删除后子树应清空,实际剩 %d 行", left)
	}
	// feed 的 deleted 事件仍按根写一行(行已不在,靠入队快照)
	if n := f.countFeedRows(t, top, "deleted"); n != 1 {
		t.Fatalf("应有 1 行 deleted 聚合事件,实际 %d", n)
	}
	task, err := f.svc.Queue.Get(ctx, res.TaskID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if task.State != dirops.StateDone {
		t.Fatalf("任务应为 done,实际 %s(%s/%s)", task.State, task.ErrorCode, task.ErrorMessage)
	}
	if task.RowsAffected != 5 {
		t.Fatalf("rows_affected 应为 5,实际 %d", task.RowsAffected)
	}
	if task.FreedBytes <= 0 {
		t.Fatalf("freed_bytes 应 > 0,实际 %d", task.FreedBytes)
	}
	if len(kids) != 4 {
		t.Fatalf("夹具应造出 4 个子行")
	}
}

// ---- ⑤ 队列语义:唯一在飞任务 / 领取 / 租约复位 / 失败落库 ----

func TestQueueRejectsSecondInflightTaskOnSameRoot(t *testing.T) {
	f := setup(t)
	q := f.withQueue(t, 1)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "one-inflight", 1)

	mk := func() error {
		_, err := q.Create(ctx, &dirops.Task{
			Kind: dirops.KindDelete, SpaceID: f.spaceID, FileID: top, UserID: f.userID,
		})
		return err
	}
	if err := mk(); err != nil {
		t.Fatalf("首次入队应成功: %v", err)
	}
	// 唯一索引 dir_op_tasks_inflight_key:"先查再插"在并发下必然漏,
	// 漏掉的那个会让两个 worker 同时改同一棵子树。
	if err := mk(); !errors.Is(err, dirops.ErrAlreadyInFlight) {
		t.Fatalf("同根第二次入队应 ErrAlreadyInFlight,实际 %v", err)
	}
}

func TestQueueClaimLeaseAndStaleReset(t *testing.T) {
	f := setup(t)
	q := f.withQueue(t, 1)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "lease", 1)

	created, err := q.Create(ctx, &dirops.Task{
		Kind: dirops.KindDelete, SpaceID: f.spaceID, FileID: top, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	got, err := q.Get(ctx, created.ID)
	if err != nil || got.State != dirops.StatePending {
		t.Fatalf("新任务应为 pending,实际 %+v err=%v", got, err)
	}

	claimed, err := q.Claim(ctx, 10*time.Minute)
	if err != nil {
		t.Fatalf("领取失败: %v", err)
	}
	if claimed.ID != created.ID || claimed.State != dirops.StateRunning {
		t.Fatalf("领取结果不对: %+v", claimed)
	}
	// 已被领取 → 第二次领取应"队列为空"(不能两个 worker 拿同一个任务)
	if _, err := q.Claim(ctx, time.Minute); !errors.Is(err, dirops.ErrNoClaim) {
		t.Fatalf("第二次领取应 ErrNoClaim,实际 %v", err)
	}

	// 租约超时(worker 崩溃)→ 复位为 pending,下一轮重试。
	// 直接把 claim_expires_at 改到过去,而不是真的 sleep(避免用例等待)。
	if _, err := f.pool.Exec(ctx,
		`UPDATE dir_op_tasks SET claim_expires_at = now() - interval '1 minute' WHERE id = $1`,
		created.ID); err != nil {
		t.Fatalf("造超时租约失败: %v", err)
	}
	n, err := q.ResetStale(ctx)
	if err != nil || n != 1 {
		t.Fatalf("应复位 1 条超时任务,实际 %d(err=%v)", n, err)
	}
	again, err := q.Get(ctx, created.ID)
	if err != nil || again.State != dirops.StatePending {
		t.Fatalf("复位后应为 pending,实际 %+v err=%v", again, err)
	}
	if again.ClaimExpiresAt != nil {
		t.Fatalf("复位后租约应清空,实际 %v", again.ClaimExpiresAt)
	}
}

func TestQueueFailRecordsStableCodeAndClearsOpState(t *testing.T) {
	f := setup(t)
	q := f.withQueue(t, 1)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "fail", 1)

	created, err := q.Create(ctx, &dirops.Task{
		Kind: dirops.KindDelete, SpaceID: f.spaceID, FileID: top, UserID: f.userID,
	})
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if s := f.opState(t, top); s != "moving" {
		t.Fatalf("入队后应标记 moving,实际 %q", s)
	}

	w := &dirops.Worker{
		Queue: q,
		Exec: func(ctx context.Context, task *dirops.Task) (dirops.Outcome, error) {
			return dirops.Outcome{}, &dirops.ExecError{
				Code: "forbidden", Message: "测试注入的失败",
				Err: errors.New("boom"),
			}
		},
	}
	ran, err := w.RunOnce(ctx)
	if err != nil || !ran {
		t.Fatalf("worker 应执行到任务(ran=%v err=%v)", ran, err)
	}
	task, err := q.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if task.State != dirops.StateFailed {
		t.Fatalf("任务应为 failed,实际 %s", task.State)
	}
	if task.ErrorCode != "forbidden" || task.ErrorMessage != "测试注入的失败" {
		t.Fatalf("失败码/文案应落库: %q / %q", task.ErrorCode, task.ErrorMessage)
	}
	// 关键:失败也必须清掉"移动中"标记,否则这个目录永久动不了
	if s := f.opState(t, top); s != "" {
		t.Fatalf("失败后也应清空 op_state,实际 %q", s)
	}
}

// ---- ⑥ 未装配队列时保持同步(宁可慢,不要让漏装配变成用户可见的失败) ----

func TestOverThresholdWithoutQueueFallsBackToSync(t *testing.T) {
	f := setup(t)
	f.svc.SyncMaxRows = 1 // 阈值 1:5 行必然超阈值
	ctx := context.Background()
	top, kids := f.mkTree(t, f.rootID, "noqueue")
	dest := f.mkDirAt(t, f.rootID, "destnq", 1)

	res, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: top, NewParentID: dest,
	})
	if err != nil {
		t.Fatalf("未装配队列时应退化为同步而非报错: %v", err)
	}
	if res.Async || res.Entry == nil {
		t.Fatalf("未装配队列时应同步完成,实际 %+v", res)
	}
	_, parent, _, _ := f.fileRow(t, top)
	if parent != dest {
		t.Fatalf("同步兜底也应真的移动,实际 parent=%s", parent)
	}
	_ = kids
}

// ---- ⑦ 幂等:租约超时重试时,已移到位/已删掉的任务应视为成功 ----

func TestWorkerRetryIsIdempotent(t *testing.T) {
	f := setup(t)
	q := f.withQueue(t, 3)
	ctx := context.Background()
	top, _ := f.mkTree(t, f.rootID, "retry")
	dest := f.mkDirAt(t, f.rootID, "destretry", 1)

	created, err := q.Create(ctx, &dirops.Task{
		Kind: dirops.KindMove, SpaceID: f.spaceID, FileID: top, UserID: f.userID,
		NewParentID: dest, NewName: "retry", RootParentID: f.rootID, RootName: "retry",
		IsDir: true,
	})
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 第一次执行成功
	if !f.execWorker(t) {
		t.Fatal("应执行到任务")
	}
	// 手动把它退回 pending(模拟"worker 在置 done 前崩溃"这一窄窗口)
	if _, err := f.pool.Exec(ctx, `
UPDATE dir_op_tasks SET state='pending', claim_expires_at=NULL, finished_at=NULL WHERE id=$1`,
		created.ID); err != nil {
		t.Fatalf("重置任务状态失败: %v", err)
	}
	// 第二次执行必须是**成功**(已移到位 → 幂等返回),而不是失败
	if !f.execWorker(t) {
		t.Fatal("应再次执行到任务")
	}
	task, err := q.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("读任务失败: %v", err)
	}
	if task.State != dirops.StateDone {
		t.Fatalf("重试应成功(done),实际 %s(%s/%s)", task.State, task.ErrorCode, task.ErrorMessage)
	}
	// 而且不能把子树再"移一次"(parent 仍是 dest,版本没有再涨)
	_, parent, v, _ := f.fileRow(t, top)
	if parent != dest {
		t.Fatalf("重试后 parent 应仍是 dest,实际 %s", parent)
	}
	if v != 2 {
		t.Fatalf("重试不应再 +1 版本(实际 %d)", v)
	}
	// 变更流也不会重复写(幂等分支直接返回)
	if n := f.countSpaceFeedRows(t); n > 1 {
		t.Fatalf("幂等重试最多只应有 1 行变更,实际 %d", n)
	}
}
