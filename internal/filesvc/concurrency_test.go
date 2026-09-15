package filesvc_test

// TS-04 并发与竞态用例集(filesvc 侧):真实 PostgreSQL 18 + 真实本地存储。
//
// 为什么验收点③④落在这个包而不是 uploadsvc:
//   - 乐观锁 `base_version` 的裁决点在本包(filesvc.checkVersion / mapWriteErr);
//   - 目录级操作(COPY / 同步 Move / 超阈值异步 Move/Delete)的编排也全在本包,
//     finalize 只负责"单个文件的定稿"。把这两个点放到 uploadsvc 只能靠桩,
//     而"跨路径的锁序与引用计数"恰恰是不能用桩测的东西。
//
// 纪律与 uploadsvc/concurrency_test.go 一致:
//  1. 并发段跑在 `context.WithTimeout` 之下,到点没跑完即 t.Fatalf 判死锁
//     (不依赖 go test 的全局超时);
//  2. 每个用例结束调用 assertObjectInvariants:ref_count == 引用它的 files 行数,
//     且 live 对象的物理文件必须存在(无悬空对象);
//  3. 断言消息一律带实际值。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/storage"
)

// concurTimeout 是并发段的应用层上限(见 uploadsvc 同名的说明:
// 实测毫秒级完成,留一个数量级余量,既能容忍慢机器又能把真挂死钉死在十几秒内)。
const concurTimeout = 15 * time.Second

// raceOp 是一个并发动作:名字用于超时诊断,fn 返回业务错误。
type raceOp struct {
	name string
	fn   func(ctx context.Context) error
}

// testCtx 返回一个有界的用例级 ctx(理由见 uploadsvc/concurrency_test.go 同名函数:
// 卡住的调用必须带着明确的 deadline 失败,而不是把测试进程挂到 go test 的全局超时 ——
// 卡住的连接还会让 pool.Close 永不返回)。
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// runConcurrent 在有界超时下并发跑完全部动作;超时即判"死锁/挂锁"并报出还在等谁。
func runConcurrent(t *testing.T, timeout time.Duration, ops []raceOp) []error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	errs := make([]error, len(ops))
	var mu sync.Mutex
	finished := make([]bool, len(ops))
	var wg sync.WaitGroup
	for i := range ops {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = ops[i].fn(ctx)
			mu.Lock()
			finished[i] = true
			mu.Unlock()
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-ctx.Done():
		mu.Lock()
		var pending []string
		for i := range ops {
			if !finished[i] {
				pending = append(pending, ops[i].name)
			}
		}
		mu.Unlock()
		t.Fatalf("并发段未在 %v 内完成(疑似死锁:多对象路径的 advisory 锁序被打乱),"+
			"仍在等待的操作=%v(共 %d 个)", timeout, pending, len(ops))
	}
	return errs
}

// uniqueBytes 造 n 字节唯一内容(头部带用例名 + 纳秒时间戳)。
//
// 必须唯一:file_objects 是全库共享表,同内容会命中同一行(ON CONFLICT 改写 ref/state),
// 表现为"断言看到别的用例的状态"(本仓库多处踩过的假失败)。
func uniqueBytes(t *testing.T, seed string, n int) []byte {
	t.Helper()
	head := fmt.Sprintf("%s|%s|%d", t.Name(), seed, time.Now().UnixNano())
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, head...)
	}
	return out[:n]
}

// ---- 最终不变式断言(TS-04 硬要求)----

func (f *fixture) hasObjectRow(t *testing.T, hash string) bool {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("查对象行失败(hash=%s): %v", hash[:12], err)
	}
	return n > 0
}

// objectMeta 读对象行的引用计数 / 四态 / 元数据大小。
func (f *fixture) objectMeta(t *testing.T, hash string) (ref int, state string, size int64) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT ref_count, state, size FROM file_objects WHERE hash_sha256 = $1`, hash).
		Scan(&ref, &state, &size); err != nil {
		t.Fatalf("读对象行失败(hash=%s): %v", hash[:12], err)
	}
	return ref, state, size
}

// assertObjectInvariants 断言 TS-04 要求的最终不变式:
//
//	① ref_count == 引用该对象的 files 行数(不夸大也不遗漏);
//	② 表不变式 (ref_count = 0) ⟺ (state ≠ 'live');
//	③ state='live' ⇒ 物理对象在存储根下存在且大小一致(无悬空对象);
//	④ state='deleted' ⇒ 物理对象确实已删;
//	⑤ 没有任何 files 行指向不存在的对象行(悬空引用)。
func (f *fixture) assertObjectInvariants(t *testing.T, label string, hashes []string) {
	t.Helper()
	// 用有界 ctx(理由同 testCtx):断言阶段的查询也不应无限挂住。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for _, h := range hashes {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true

		files := f.filesWithHash(t, h)
		if !f.hasObjectRow(t, h) {
			if files != 0 {
				t.Fatalf("[%s] 悬空引用:对象 %s 没有 file_objects 行,却有 %d 行 files 指向它",
					label, h[:12], files)
			}
			continue
		}
		ref, state, size := f.objectMeta(t, h)
		if ref != files {
			t.Fatalf("[%s] 不变式破裂:对象 %s 的 ref_count=%d,而引用它的 files 行数=%d",
				label, h[:12], ref, files)
		}
		if (ref == 0) != (state != model.ObjectLive) {
			t.Fatalf("[%s] 违反表不变式 (ref_count=0)⟺(state≠live):对象 %s ref_count=%d state=%s",
				label, h[:12], ref, state)
		}
		switch state {
		case model.ObjectLive:
			got, serr := f.store.Stat(ctx, h)
			if serr != nil {
				t.Fatalf("[%s] 悬空对象:live 对象 %s 在存储根 %s 下缺失(Stat=%v)",
					label, h[:12], f.storeRoot, serr)
			}
			if got != size {
				t.Fatalf("[%s] 对象 %s 物理大小=%d 与元数据 size=%d 不一致", label, h[:12], got, size)
			}
		case model.ObjectDeleted:
			if _, serr := f.store.Stat(ctx, h); !errors.Is(serr, storage.ErrNotFound) {
				t.Fatalf("[%s] deleted 对象 %s 的物理文件应已删除,实际 Stat=%v", label, h[:12], serr)
			}
		}
	}
}

// ---- 目录/文件树夹具 ----

// mkConcurrentTree 造一棵 7 行的子树(3 目录 + 4 文件),其中两行**内容相同**。
//
// 同内容两行是刻意的:内容寻址下它们共享同一个 file_objects 行,于是
// "逐行 ref+1"(而不是"每对象一次")这条纪律才有被测到的机会 ——
// 少加一次,清理 worker 随后就会把仍被引用的对象物理删掉(悬空指针)。
//
// 返回 (子树根, 子目录 d1):d1 用于"把子目录复制到别处"的路径交叉场景。
// 所有 hash 都已进 fixture.made(精确清理 + 不变式断言的取数来源)。
func (f *fixture) mkConcurrentTree(t *testing.T, prefix string) (top string, d1 string) {
	t.Helper()
	shared := uniqueBytes(t, prefix+"-shared", 700)
	u1 := uniqueBytes(t, prefix+"-u1", 900)
	u2 := uniqueBytes(t, prefix+"-u2", 1100)

	top = f.mkDirAt(t, f.rootID, prefix+"-top", 1)
	d1 = f.mkDirAt(t, top, "d1", 2)
	d2 := f.mkDirAt(t, top, "d2", 2)
	f.mkFileAt(t, top, "a.txt", shared)
	f.mkFileAt(t, d1, "b.txt", shared)
	f.mkFileAt(t, d2, "c.txt", u1)
	f.mkFileAt(t, top, "d.txt", u2)

	if ref, _ := f.objRef(t, hashOf(shared)); ref != 2 {
		t.Fatalf("前置:共享内容应被 2 行引用(ref=2),实际 %d", ref)
	}
	return top, d1
}

// ============================================================
// 验收点③:并发双覆盖 → 一成功一 409(filesvc 的 base_version 形态)
// ============================================================

// TestConcurrentBaseVersionRenameConflict:两个请求带**同一个** base_version 并发改名同一行。
//
// 裁决点在本包:checkVersion / repo.Rename 的乐观锁 `WHERE version = expect`。
// 两个请求都通过了"读时校验",但只有一条 UPDATE 能匹配 version=1;另一个
// 匹配 0 行 → repo.ErrConflict → mapWriteErr → 409 + `reason = version_conflict`
// (常量 filesvc.ReasonVersionConflict,客户端据此提示"刷新后重试")。
//
// 断言:恰好一个成功;失败方 409 且 reason=version_conflict 并带 server_version;
// 最终行 version == 胜者写出的版本(2)、name == 胜者的新名字 —— 即**没有半写状态**。
func TestConcurrentBaseVersionRenameConflict(t *testing.T) {
	f := setup(t)
	ctx := testCtx(t)

	id, _ := f.mkFileAt(t, f.rootID, "race-base-version.bin", uniqueBytes(t, "bv", 512))
	// 读取"两个客户端都持有的版本"
	var base int64
	if err := f.pool.QueryRow(ctx, `SELECT version FROM files WHERE id = $1`, id).Scan(&base); err != nil {
		t.Fatalf("读初始版本失败: %v", err)
	}
	if base != 1 {
		t.Fatalf("新建行版本应为 1,实际 %d", base)
	}
	nameA := fmt.Sprintf("bv-win-a-%d.bin", time.Now().UnixNano())
	nameB := fmt.Sprintf("bv-win-b-%d.bin", time.Now().UnixNano())

	errs := runConcurrent(t, concurTimeout, []raceOp{
		{name: "rename-a", fn: func(c context.Context) error {
			_, err := f.svc.Rename(c, filesvc.RenameInput{
				UserID: f.userID, FileID: id, NewName: nameA, BaseVersion: base,
			})
			return err
		}},
		{name: "rename-b", fn: func(c context.Context) error {
			_, err := f.svc.Rename(c, filesvc.RenameInput{
				UserID: f.userID, FileID: id, NewName: nameB, BaseVersion: base,
			})
			return err
		}},
	})

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatalf("两个同 base_version 的并发改名都成功了(必须恰好一个):A=%v B=%v", errs[0], errs[1])
			}
			winner = i
		}
	}
	if winner == -1 {
		t.Fatalf("两个并发改名都失败了(必须恰好一个成功):A=%v B=%v", errs[0], errs[1])
	}
	loserErr := errs[1-winner]
	ae := asAPIErr(t, loserErr)
	if ae.Status != 409 {
		t.Fatalf("base_version 冲突的失败方应 409,实际 %d/%s(%v)", ae.Status, ae.Code, loserErr)
	}
	if got := ae.Details["reason"]; got != filesvc.ReasonVersionConflict {
		t.Fatalf("失败方 reason 应为 %s(常量 filesvc.ReasonVersionConflict),实际 %v(details=%v)",
			filesvc.ReasonVersionConflict, got, ae.Details)
	}
	// 409 必须带 server_version(客户端据此无需再查一次)。
	//
	// 注意这里刻意**不**断言它等于 base+1:两条失败路径在代码里给出的是不同来源的值 ——
	//   - 初始 GetByID 已经读到新版本 → checkVersion 失败 → 带的就是当前版本(2);
	//   - 读的时候还是 1、UPDATE `WHERE version=1` 匹配 0 行 → mapWriteErr(cur,...) 带的是
	//     **该请求自己那份快照**(1)。
	// 后者是"409 里 server_version 可能偏旧"的一个窄窗口(见 commit 的
	// mapWriteErr/conflictOf),但两者都不会低于客户端持有的基线。用例只断言这个下界,
	// 并把实际值打出来;是否要把 mapWriteErr 改成重新读一次当前行是产品决策,不在这里假装。
	raw, ok := ae.Details["server_version"]
	if !ok {
		t.Fatalf("409 必须带 server_version 字段(客户端据此无需再查),实际 details=%v", ae.Details)
	}
	if toI64Test(raw) < base {
		t.Fatalf("409 的 server_version 不应低于客户端持有的基线 %d,实际 %v(details=%v)",
			base, raw, ae.Details)
	}
	t.Logf("失败方 409 details: reason=%v server_version=%v(基线=%d,胜者最终版本=%d)",
		ae.Details["reason"], raw, base, base+1)

	// 失败方不得改动任何状态:版本恰为胜者写出的 version,名字是胜者的名字
	winName := nameA
	if winner == 1 {
		winName = nameB
	}
	gotName, _, version, _ := f.fileRow(t, id)
	if version != base+1 {
		t.Fatalf("最终行版本应为 %d(恰好 +1 一次),实际 %d", base+1, version)
	}
	if gotName != winName {
		t.Fatalf("最终名字应为胜者的 %q,实际 %q(失败方不得半写)", winName, gotName)
	}
	// 失败方的新名字不得在库里出现(它的整条 UPDATE 都没生效)
	loserName := nameA
	if winner == 0 {
		loserName = nameB
	}
	var loserRows int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE space_id = $1 AND lower(name) = lower($2)`,
		f.spaceID, loserName).Scan(&loserRows); err != nil {
		t.Fatalf("数失败方名字的行失败: %v", err)
	}
	if loserRows != 0 {
		t.Fatalf("失败方的新名字 %q 不应在库里出现,实际 %d 行", loserName, loserRows)
	}
	f.assertObjectInvariants(t, "base_version 并发改名", f.made)
}

// toI64Test 把 apierr details 里的数值(可能是 int64/int/float64)转成 int64。
func toI64Test(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return -1
	}
}

// ============================================================
// 验收点④:COPY 大目录并发(同步路径)
// ============================================================

// TestConcurrentCopyOnOverlappingPathsNoDeadlock:6 个目录级操作并发跑在**重叠路径**上。
//
// 重叠点有两处,恰好是死锁最容易发生的地方:
//   - 5 次 COPY 都在读**同一棵源子树**(同一批对象) → 若取对象锁不按 hash 升序,
//     "你等我、我等你"就会在这里出现(objlock.LockManyXact 的升序正是为此);
//   - 有 COPY 把源子树的**子目录**复制进同一个目标目录(路径交叉)。
//
// 另有一个**同步 Move**(操作⑥):COPY 本身没有同步/异步分流,带
// `policy.dir_op_sync_max_rows` 阈值的是 Move/Delete,这里走的是"行数 ≤ 阈值 → 同步事务"
// 那条路径(阈值默认 DefaultDirOpSyncMaxRows=1000,本树 7 行远低于它);
// 超阈值转异步的那条路径见 TestConcurrentAsyncDirOpsNoDeadlock。
//
// 断言:全部操作成功(名字/目标都互不冲突,没有可预见的 409)、
// 物理对象个数不变(COPY 零存储 IO)、引用计数 == 引用行数(无泄漏/无悬空)。
func TestConcurrentCopyOnOverlappingPathsNoDeadlock(t *testing.T) {
	f := setup(t)

	prefix := fmt.Sprintf("ccp-%d", time.Now().UnixNano())
	top, d1 := f.mkConcurrentTree(t, prefix)
	destA := f.mkDirAt(t, f.rootID, prefix+"-destA", 1)
	// 同步 Move 的源目录**必须在并发段之前**造好:并发 goroutine 里不能调用
	// t.Fatalf(t 的分支只在测试主 goroutine 上安全),而且现场造目录会引入
	// 与并发段无关的失败点。
	mvSrc := f.mkDirAt(t, top, fmt.Sprintf("mv-src-%d", time.Now().UnixNano()), 2)
	objectsBefore := f.countStoredObjects(t)

	// **确定性死锁回归**(TS-04 曾实测到 40P01,见下方 TestCopyTakesRowLocksInHashOrder)。
	// 这里保留"COPY + 并发删除副本"的组合作为**概率性**回归网(真交错才会撞上),
	// 确定性证据在下面那条用例里:它把"行锁必须按 hash 升序取"这件事直接钉住 ——
	// 全序一致则死锁面为空,这是可判定的,不需要靠复现概率。
	// 历史:根因是 Copy 按子树遍历序(lvl,name)逐行 bumpObjectRefTx,而 Delete 的
	// deleteSubtree 按 hash 升序释放 —— 两者顺序相反,违反 6.6 全局锁序。**已修**
	// (Copy 现在同样按 hash 升序;见 share.go 第 3 步的注释)。
	copyTop := func(name string, target string) func(context.Context) error {
		return func(c context.Context) error {
			_, cerr := f.svc.Copy(c, filesvc.CopyInput{
				UserID: f.userID, FileID: top, TargetParentID: target, NewName: name,
			})
			return cerr
		}
	}
	ops := []raceOp{
		{name: "copy-top-1", fn: copyTop(prefix+"-copy-1", f.rootID)},
		{name: "copy-top-2", fn: copyTop(prefix+"-copy-2", f.rootID)},
		{name: "copy-sub-into-destA", fn: func(c context.Context) error {
			// 源是 top 的子目录、目标是另一个目录:路径交叉
			_, cerr := f.svc.Copy(c, filesvc.CopyInput{
				UserID: f.userID, FileID: d1, TargetParentID: destA, NewName: prefix + "-subcopy",
			})
			return cerr
		}},
		{name: "copy-top-into-destA", fn: copyTop(prefix+"-copy-3", destA)},
		{name: "move-subdir-sync", fn: func(c context.Context) error {
			// 同步 Move(行数 ≤ dir_op_sync_max_rows)
			_, merr := f.svc.Move(c, filesvc.MoveInput{
				UserID: f.userID, FileID: mvSrc, NewParentID: destA,
				NewName: prefix + "-moved",
			})
			return merr
		}},
		{name: "copy-sub-into-root", fn: func(c context.Context) error {
			_, cerr := f.svc.Copy(c, filesvc.CopyInput{
				UserID: f.userID, FileID: d1, TargetParentID: f.rootID, NewName: prefix + "-subcopy2",
			})
			return cerr
		}},
	}
	errs := runConcurrent(t, concurTimeout, ops)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发目录操作 %s 失败(全部结果=%v):%v", ops[i].name, errs, err)
		}
	}

	// COPY/同步 Move/Delete 都不落新物理对象(内容寻址)
	if after := f.countStoredObjects(t); after != objectsBefore {
		t.Fatalf("COPY/MOVE/DELETE 不应写新物理对象:前 %d 后 %d", objectsBefore, after)
	}
	f.assertObjectInvariants(t, "并发 COPY(同步路径)", f.made)
}

// ============================================================
// 验收点④:**确定性**死锁回归:COPY 的对象行锁必须按 hash 升序取
// ============================================================

// 为什么需要"确定性"用例:死锁只在真交错下出现(TS-04 实测 6 次复现 1 次),
// 靠并发跑几百遍当门禁是**不可靠**的 —— 它会时红时绿,最后被当成"抖动"忽略掉。
// 而死锁的**成因**是可判定的:只要"所有路径按同一个全序取行锁",环就不可能出现。
// 所以这里直接钉住那个全序(而不是试图复现环):
//
//	从另一条连接**持有最大的那个 hash 的行锁** → 升序实现会先把所有更小的 hash 锁上、
//	再卡在最大的那个上。于是:
//	  - 更小的 hash 必须已经被 COPY 的事务锁住(用 FOR UPDATE NOWAIT 观测:55P03);
//	  - 反过来说,若实现按**子树遍历序**取锁(旧缺陷),它会先撞上"最大的那个"并阻塞,
//	    此时更小的 hash **还没有**被锁 → 断言失败。
//
// 反向验证(在本机跑过):把 share.go 第 3 步的 `sort.Strings(ordered)` 去掉
// (退回遍历序),本用例失败;恢复后通过。
func TestCopyTakesRowLocksInHashOrder(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	top := f.mkDirAt(t, f.rootID, "rowlock-"+suffix, 1)

	// 内容挑成"名字序 ≠ hash 序",否则按遍历序取锁的错误实现也能蒙对
	contents := pickContentsWhereHashOrderDiffersFromNameOrder(suffix)
	var hashes []string
	for i, c := range contents {
		_, h := f.mkFileAt(t, top, fmt.Sprintf("f%d.txt", i), c)
		hashes = append(hashes, h)
	}
	sorted := append([]string(nil), hashes...)
	sort.Strings(sorted)
	smallest, largest := sorted[0], sorted[len(sorted)-1]
	if smallest == largest {
		t.Fatal("用例前提不成立:需要 ≥2 个不同 hash")
	}

	// 另一条连接持有**最大** hash 的对象行锁(不是 advisory 锁:要拦的是 UPDATE 的行锁)
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
	if _, err := tx.Exec(ctx,
		`SELECT hash_sha256 FROM file_objects WHERE hash_sha256=$1 FOR UPDATE`, largest); err != nil {
		t.Fatalf("锁最大 hash 的对象行失败: %v", err)
	}

	// 起 COPY(它必须在"最大的那个"上等着)
	done := make(chan error, 1)
	go func() {
		_, cerr := f.svc.Copy(ctx, f.copyInput(top))
		done <- cerr
	}()

	// 有界轮询"最小的 hash 是否已被 COPY 的事务锁住"。
	// 升序实现:很快就能观测到;遍历序实现:永远观测不到(它卡在最大的那个上)。
	lockedSmallest := false
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rowLockedByOther(ctx, t, f, smallest) {
			lockedSmallest = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !lockedSmallest {
		_ = tx.Rollback(ctx)
		<-done
		t.Fatalf("COPY 没有先锁住最小的 hash(%s)—— 说明对象行锁不是按 hash 升序取的;"+
			"这正是 40P01 死锁的成因(与删除侧 sortHashes 顺序相反)", smallest[:12])
	}

	// 放锁后 COPY 必须成功完成(证明它一直只是在等那把行锁,而不是已经坏掉)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	select {
	case cerr := <-done:
		if cerr != nil {
			t.Fatalf("放锁后 COPY 应成功,实际 %v", cerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("放锁后 COPY 仍未完成(可能真的死锁了)")
	}
	f.assertObjectInvariants(t, "COPY 行锁序(确定性)", f.made)
}

// rowLockedByOther 用 FOR UPDATE NOWAIT 探测某 hash 的对象行是否已被**别的事务**锁住。
// 55P03 = lock_not_available(行被别的事务持有)。
func rowLockedByOther(ctx context.Context, t *testing.T, f *fixture, hash string) bool {
	t.Helper()
	c, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	defer c.Release()
	// NOWAIT:不能等,否则本函数自己会阻塞在 COPY 的行锁上
	_, qerr := c.Exec(ctx,
		`SELECT hash_sha256 FROM file_objects WHERE hash_sha256=$1 FOR UPDATE NOWAIT`, hash)
	if qerr == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(qerr, &pgErr) && pgErr.Code == "55P03" {
		return true
	}
	t.Fatalf("探测行锁失败(既不是成功也不是 55P03): %v", qerr)
	return false
}

// ============================================================
// 验收点④:目录操作并发(超阈值 → 异步路径)
// ============================================================

// TestConcurrentAsyncDirOpsNoDeadlock:两棵**超过阈值**的子树并发转异步任务并并发执行。
//
// 阈值:`filesvc.Service.SyncMaxRows`(装配值来自 `policy.dir_op_sync_max_rows`,
// 默认 DefaultDirOpSyncMaxRows=1000)。用例用 withQueue(t, 3) 把阈值压到 3,
// 于是 7 行的树必然走 Map/Delete 的**异步**分支(小阈值只影响这一个整数比较,
// 不必真造 1001 行让用例慢一个数量级)。
//
// 为什么不用 dirops.Worker.RunOnce 驱动:`Queue.Claim` 是**全表**领取(没有 scope 参数),
// 共用测试库里它会抢到别的包正在断言的 pending 任务,而执行体用的是本用例的
// filesvc.Service —— 那会把别的包的 fixture 改坏(同 lifecycle 包专门提供
// RunOnceFor/StatsFor 的理由)。因此这里用**同一条 SQL 但按 task_id 限定**的领取,
// 只驱动本用例自己的任务。
//
// 断言:两个任务都 done、树被删/被移到位、op_state 已清、物理对象数不变、
// 引用计数 == 引用行数(no leaks / no dangling rows)。
func TestConcurrentAsyncDirOpsNoDeadlock(t *testing.T) {
	f := setup(t)
	q := f.withQueue(t, 3)
	ctx := testCtx(t)

	prefix := fmt.Sprintf("cad-%d", time.Now().UnixNano())
	// 两棵树**共享同一份内容**:异步删除与异步移动交错时,同一对象上
	// ref-1 与"整棵移动不动物体"的关系必须保持 ref == 引用行数。
	shared := uniqueBytes(t, "async-shared", 800)
	topA := f.mkDirAt(t, f.rootID, prefix+"-A", 1)
	f.mkDirAt(t, topA, "s1", 2)
	f.mkFileAt(t, topA, "a1.txt", shared)
	f.mkFileAt(t, topA, "a2.txt", shared)
	f.mkFileAt(t, topA, "a3.txt", uniqueBytes(t, "async-a3", 640))

	topB := f.mkDirAt(t, f.rootID, prefix+"-B", 1)
	f.mkDirAt(t, topB, "s2", 2)
	f.mkFileAt(t, topB, "b1.txt", shared)
	f.mkFileAt(t, topB, "b2.txt", uniqueBytes(t, "async-b2", 720))

	dest := f.mkDirAt(t, f.rootID, prefix+"-dest", 1)
	objectsBefore := f.countStoredObjects(t)

	var delRes *filesvc.DeleteResult
	var mvRes *filesvc.MoveResult
	errs := runConcurrent(t, concurTimeout, []raceOp{
		{name: "enqueue-async-delete", fn: func(c context.Context) error {
			r, err := f.svc.Delete(c, f.userID, topA)
			delRes = r
			return err
		}},
		{name: "enqueue-async-move", fn: func(c context.Context) error {
			r, err := f.svc.Move(c, filesvc.MoveInput{
				UserID: f.userID, FileID: topB, NewParentID: dest, NewName: prefix + "-B-moved",
			})
			mvRes = r
			return err
		}},
	})
	for i, err := range errs {
		if err != nil {
			t.Fatalf("超阈值目录操作入队失败(第 %d 个,errs=%v):%v", i, errs, err)
		}
	}
	if delRes == nil || !delRes.Async || delRes.TaskID == "" {
		t.Fatalf("超阈值删除应转异步并返回 task_id,实际 %+v", delRes)
	}
	if mvRes == nil || !mvRes.Async || mvRes.TaskID == "" {
		t.Fatalf("超阈值移动应转异步并返回 task_id,实际 %+v", mvRes)
	}
	if s := f.opState(t, topA); s != "moving" {
		t.Fatalf("入队后删除根应标 moving,实际 %q", s)
	}

	// 并发执行两个任务:领取按 task_id 限定(理由见函数注释),执行体与 worker 同一份
	execTask := func(id string) error {
		tag, err := f.pool.Exec(ctx, `
UPDATE dir_op_tasks SET state = 'running', claim_expires_at = now() + interval '10 minutes',
       updated_at = now()
 WHERE id = $1 AND state = 'pending'`, id)
		if err != nil {
			return fmt.Errorf("领取任务 %s 失败: %w", id, err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("领取任务 %s 应命中 1 行 pending,实际 %d", id, tag.RowsAffected())
		}
		task, err := q.Get(ctx, id)
		if err != nil {
			return fmt.Errorf("读任务 %s 失败: %w", id, err)
		}
		var out dirops.Outcome
		var xerr error
		switch task.Kind {
		case dirops.KindDelete:
			out, xerr = f.svc.ExecDirDelete(ctx, task)
		case dirops.KindMove:
			out, xerr = f.svc.ExecDirMove(ctx, task)
		default:
			return fmt.Errorf("未知任务类型 %q", task.Kind)
		}
		if xerr != nil {
			return fmt.Errorf("执行任务 %s(%s)失败: %w", id, task.Kind, xerr)
		}
		if err := q.Finish(ctx, task, out); err != nil {
			return fmt.Errorf("标记任务 %s 完成失败: %w", id, err)
		}
		return nil
	}
	execErrs := runConcurrent(t, concurTimeout, []raceOp{
		{name: "exec-delete", fn: func(context.Context) error { return execTask(delRes.TaskID) }},
		{name: "exec-move", fn: func(context.Context) error { return execTask(mvRes.TaskID) }},
	})
	for i, err := range execErrs {
		if err != nil {
			t.Fatalf("并发执行异步任务失败(第 %d 个,errs=%v):%v", i, execErrs, err)
		}
	}

	// 任务可轮询且都 done
	for _, id := range []string{delRes.TaskID, mvRes.TaskID} {
		task, err := q.Get(ctx, id)
		if err != nil {
			t.Fatalf("读任务 %s 失败: %v", id, err)
		}
		if task.State != dirops.StateDone {
			t.Fatalf("任务 %s 应为 done,实际 %s(%s/%s)", id, task.State, task.ErrorCode, task.ErrorMessage)
		}
		if task.FinishedAt == nil {
			t.Fatalf("done 任务 %s 必须有 finished_at", id)
		}
	}
	// 删除到位:A 的整棵子树不再存在
	var leftA int
	if err := f.pool.QueryRow(ctx, `
WITH RECURSIVE sub AS (
  SELECT id FROM files WHERE id = $1
  UNION ALL SELECT f.id FROM files f JOIN sub ON f.parent_id = sub.id)
SELECT count(*) FROM sub`, topA).Scan(&leftA); err != nil {
		t.Fatalf("统计 A 残留失败: %v", err)
	}
	if leftA != 0 {
		t.Fatalf("异步删除后 A 子树应清空,实际剩 %d 行", leftA)
	}
	// 移动到位:B 的根挂到 dest 且 op_state 已清
	_, parentB, _, _ := f.fileRow(t, topB)
	if parentB != dest {
		t.Fatalf("异步移动后 B 的父目录应为 %s,实际 %s", dest, parentB)
	}
	if s := f.opState(t, topB); s != "" {
		t.Fatalf("任务完成后 B 的 op_state 应清空,实际 %q", s)
	}

	// 物理对象数不变:删除只解除引用(延迟删除窗口),移动零存储 IO
	if after := f.countStoredObjects(t); after != objectsBefore {
		t.Fatalf("异步目录操作不应改变物理对象个数:前 %d 后 %d", objectsBefore, after)
	}
	f.assertObjectInvariants(t, "并发异步目录操作", f.made)
}
