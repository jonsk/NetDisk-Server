package uploadsvc_test

// TS-04 并发与竞态用例集(真实 PostgreSQL 17 + 真实本地存储 + miniredis 作为挑战存储)。
//
// 三条纪律贯穿本文件:
//
//  1. **并发段必须跑在有界 ctx 下**,到点没跑完就 t.Fatalf 判"死锁/挂锁" ——
//     绝不依赖 `go test` 的全局超时(那会把"挂死"变成一个 10 分钟后的 panic,
//     既看不到是谁在等锁,也留不下可对照的实际值)。
//  2. 每个用例结束都调用 assertObjectInvariants:断言
//     `file_objects.ref_count == 引用它的 files 行数`,且 live 对象在存储里真实存在
//     (即"无悬空对象"),deleted 对象的物理文件确实已删。
//  3. 断言消息一律带上实际值(把 actual 打出来),否则失败时只能靠复现来猜。
//
// 关于"延迟删除窗口"的一个真实约束(见 TestConcurrentCleanupVsReviveInterleave 的注释):
// `delete_after` 由 SQL 写成 `now() + interval '24 hours'`(finalize.releaseObjectRef /
// repo.ReleaseObjectRef),而 cleanup worker 的领取条件是 SQL 的 `delete_after < now()`。
// 因此**没有可注入的时钟缝**:用例只能把窗口强行拨到过去(与 lifecycle 包既有用例
// 同一手法),而不是 sleep 24h 或伪造时间。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/lifecycle"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// concurTimeout 是每个并发段的应用层上限。取值理由:
// 50 路秒传在 8 连接池上串行化对象锁,实测 < 2s;留一个数量级余量,
// 既不误报(慢机器上偶发)又能把"真挂死"在十几秒内钉死。
const concurTimeout = 15 * time.Second

// raceEnv 在 uploadsvc 既有 fixture 之上装配:存储 + 秒传证明服务 + 定稿服务 +
// cleanup worker + filesvc(删除/复制路径要用)。
//
// 刻意在这里重新装配而不是复用 setupTUS:后者构造的 finalize.Service **没有** Fast
// 校验器,而秒传定稿必须经过持物证明(BE-S4-04),拿不到那条缝就测不出真实路径。
type raceEnv struct {
	*fixture
	store *storage.FS
	fin   *finalize.Service
	tus   *uploadsvc.TUSService
	lc    *lifecycle.Service
	files *filesvc.Service
	proof *fastupload.Service
	mr    *miniredis.Miniredis
	// made 记录本用例造出的对象 hash —— 这些行可能在本用例里被清理成 deleted
	// 且不再有 files 行指向,fixture 的"按空间引用清理"覆盖不到它们。
	// 精确删除是**硬要求**:绝不写无条件 DELETE(会删掉并行跑的其它包的断言对象)。
	made []string
}

func setupRace(t *testing.T) *raceEnv {
	t.Helper()
	f := setup(t)

	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	proof := &fastupload.Service{KV: cache.New(rdb, "netdisk:"), Reader: store}
	// 秒传通道的两个开关:预检侧(uploadsvc.Fast)与定稿侧(finalize.Fast)。
	// 少装任何一个,客户端都拿不到/过不了挑战 —— 而用例要测的正是"50 路都过"。
	f.svc.Fast = proof
	f.svc.FastMinSize = fastupload.MinSize

	fin := &finalize.Service{
		Pool: f.database.Pool, Storage: store,
		Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{},
		Name: f.svc.Name, Fast: proof,
		// 给对象锁加一个应用层上限:objlock 的会话级锁是**无超时**的
		// (它是正确的生产语义 —— 生产装配会传 config 里的 lock_timeout),
		// 而用例里若有人把锁写错、锁永久挂在池连接上,没有这个上限就会一路挂到
		// 测试框架的全局超时,且**卡住的连接会让 pool.Close 永不返回**。
		// 15s 远大于本文件所有并发段的实际耗时(实测 < 1s)。
		LockTimeout: concurTimeout,
	}
	e := &raceEnv{
		fixture: f,
		store:   store,
		fin:     fin,
		proof:   proof,
		mr:      mr,
		tus: &uploadsvc.TUSService{
			Service: f.svc, Stager: store, Finalizer: fin, DB: f.svc.DB,
		},
		lc: &lifecycle.Service{
			Pool: f.database.Pool, Storage: store,
			// 真实生产值:24h 窗口 + 10min 领取租约
			DeleteAfter: 24 * time.Hour, ClaimTTL: 10 * time.Minute,
			// 同理给对象锁上限(见 finalize.Service.LockTimeout 的说明)
			LockTimeout: concurTimeout,
		},
		files: &filesvc.Service{
			Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Uploads: repo.UploadRepo{},
			DB: f.svc.DB, Name: f.svc.Name,
		},
	}
	// 本用例自造的对象行按 hash 精确清理(理由见 raceEnv.made);
	// 注册在 setup() 的清理之后 → LIFO 先跑,不会与它的空间级清理打架。
	t.Cleanup(func() {
		if len(e.made) == 0 {
			return
		}
		_, _ = f.database.Pool.Exec(context.Background(),
			`DELETE FROM file_objects WHERE hash_sha256 = ANY($1::varchar[])`, e.made)
	})
	return e
}

// testCtx 返回一个**有界**的用例级 ctx。
//
// 存在的理由(实测踩过一次 10 分钟挂死):本进程里任何"本不该阻塞"的调用若卡住
// (例如锁被误挂在池连接上),而它拿的是 context.Background(),那么 test 会一直挂着;
// 更糟的是卡住的协程占着池连接,`database.Close()`(t.Cleanup)会等所有连接归还,
// 于是整个测试二进制**永不退出**,只能靠 go test 的全局超时兜底 —— 而那正是
// TS-04 明确不允许的"靠框架超时发现死锁"。90s 远大于本文件任何用例的实际耗时(<2s)。
func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// ---- 并发驱动(死锁探测)----

// raceOp 是一个并发动作:名字用于超时诊断,fn 返回业务错误(不是测试失败)。
type raceOp struct {
	name string
	fn   func(ctx context.Context) error
}

// runConcurrent 在**有界超时**下并发跑完全部动作,到点没跑完即判死锁并报出还在等谁。
//
// 与直接 `go func(){...}` + WaitGroup 的差别:t.Fatalf 必须在测试主 goroutine 调用,
// 而"超时"这件事只有主 goroutine 能观测;这里把两者接起来,于是失败信息里能带上
// "哪些操作还没回来",而不是一句干巴巴的 context deadline exceeded。
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
		t.Fatalf("并发段未在 %v 内完成(疑似死锁/对象锁被长期持有),仍在等待的操作=%v(共 %d 个)",
			timeout, pending, len(ops))
	}
	return errs
}

// ---- 公共夹具/断言 ----

// uniqueContent 造 n 字节内容:头部带用例名 + 纳秒时间戳。
//
// 为什么必须唯一:对象表以 hash 为主键且是**全库共享**的,两个用例用同样内容会
// 命中同一行(ON CONFLICT 会改写 ref/state),表现为"断言看到别人的状态"。
func uniqueContent(t *testing.T, seed string, n int) []byte {
	t.Helper()
	head := fmt.Sprintf("%s|%s|%d", t.Name(), seed, time.Now().UnixNano())
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, head...)
	}
	return out[:n]
}

// fastOutcome 是一次秒传请求的结果(成功/失败都记账,便于最后统一分类)。
type fastOutcome struct {
	ok      bool
	newFile bool
	deduped bool
	err     error
}

// fastUploadOnce 走一次完整秒传:建任务(预检命中 → 下发挑战)→ 算样本摘要 →
// 无内容流的 FinishFast(不重传整份内容)。
func (e *raceEnv) fastUploadOnce(ctx context.Context, i int, hash string, data []byte) fastOutcome {
	name := fmt.Sprintf("fast50-%02d.bin", i)
	res, err := e.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: e.userID, SpaceID: e.spaceID, ParentID: e.rootID,
		Name: name, DeclaredSize: int64(len(data)), DeclaredHash: hash,
	})
	if err != nil {
		return fastOutcome{err: err}
	}
	if res.FastUpload == nil {
		// 对象就在库里、size 也一致却拿不到挑战:这不是"文档化冲突",而是预检/挑战
		// 存储出了问题 —— 显式失败,不要静默当成"可以接受的失败"。
		return fastOutcome{err: fmt.Errorf(
			"对象已存在但预检未下发秒传挑战(upload=%s, hash=%s)", res.UploadID, hash[:12])}
	}
	digests, derr := fastupload.SampleDigests(
		bytes.NewReader(data), res.FastUpload.Offsets, res.FastUpload.SampleLen)
	if derr != nil {
		return fastOutcome{err: fmt.Errorf("算样本摘要失败: %w", derr)}
	}
	up, err := e.svc.Authorize(ctx, res.UploadID, e.userID, res.Ticket)
	if err != nil {
		return fastOutcome{err: err}
	}
	fr, err := e.tus.FinishFast(ctx, up, res.FastUpload.Nonce, digests)
	if err != nil {
		return fastOutcome{err: err}
	}
	return fastOutcome{ok: true, newFile: fr.NewFile, deduped: fr.Deduped}
}

// mustFastUpload 与 fastUploadOnce 同路径,但失败即终止用例(用于确定性用例)。
func (e *raceEnv) mustFastUpload(ctx context.Context, t *testing.T, name, hash string, data []byte) *finalize.Result {
	t.Helper()
	res, err := e.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: e.userID, SpaceID: e.spaceID, ParentID: e.rootID,
		Name: name, DeclaredSize: int64(len(data)), DeclaredHash: hash,
	})
	if err != nil {
		t.Fatalf("秒传建任务(%s)失败: %v", name, err)
	}
	if res.FastUpload == nil {
		t.Fatalf("秒传建任务(%s)未下发挑战:对象应已在库中(hash=%s)", name, hash[:12])
	}
	digests, derr := fastupload.SampleDigests(
		bytes.NewReader(data), res.FastUpload.Offsets, res.FastUpload.SampleLen)
	if derr != nil {
		t.Fatalf("秒传算样本摘要(%s)失败: %v", name, derr)
	}
	up, err := e.svc.Authorize(ctx, res.UploadID, e.userID, res.Ticket)
	if err != nil {
		t.Fatalf("秒传 ticket 校验(%s)失败: %v", name, err)
	}
	fr, err := e.tus.FinishFast(ctx, up, res.FastUpload.Nonce, digests)
	if err != nil {
		t.Fatalf("秒传定稿(%s)失败: %v", name, err)
	}
	return fr
}

// seedObjectWithFile 造"一次真实上传完成后的库内状态":物理对象 + live 对象行 + 一行 files。
//
// 用于验收点②(清理 vs 复活):这两条链路的起点就是"对象 + 引用"已经存在,
// 再走完整的全量上传只会让用例慢十倍,而不会多测到任何东西。
func (e *raceEnv) seedObjectWithFile(t *testing.T, name string, data []byte) (hash, fileID string) {
	t.Helper()
	ctx := context.Background()
	hash = hashOf(data)
	if _, err := e.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写物理对象失败: %v", err)
	}
	if _, err := e.database.Pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE
   SET size = EXCLUDED.size, state = 'live', delete_after = NULL,
       ref_count = file_objects.ref_count + 1`,
		hash, len(data), e.store.KeyFor(hash)); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}
	if err := e.database.Pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, hash_sha256, depth)
VALUES ($1, $2, $3, $4, false, $5, $6, 1) RETURNING id`,
		e.spaceID, e.rootID, e.userID, name, len(data), hash).Scan(&fileID); err != nil {
		t.Fatalf("造文件行 %s 失败: %v", name, err)
	}
	e.made = append(e.made, hash)
	return hash, fileID
}

// forceDeleteDue 把延迟删除窗口"拨到过去",模拟 24h 已过。
//
// 这不是"伪造状态机":pending_delete + ref_count=0 是删除路径真实留下的状态,
// 用例只是把 delete_after 从"未来"挪到"过去"—— 也就是让 worker 的领取条件
// (`state='pending_delete' AND delete_after < now() AND ref_count=0`)此刻成立。
// 之所以必须这么做:窗口由 SQL 的 now()+interval '24 hours' 写死,
// service 上没有可注入的时钟能影响它(见文件头注释)。
func (e *raceEnv) forceDeleteDue(t *testing.T, hash string) {
	t.Helper()
	tag, err := e.database.Pool.Exec(context.Background(), `
UPDATE file_objects SET delete_after = now() - interval '1 hour'
 WHERE hash_sha256 = $1 AND ref_count = 0`, hash)
	if err != nil {
		t.Fatalf("拨动删除窗口失败(hash=%s): %v", hash[:12], err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("拨动删除窗口应命中 1 行(ref=0 的待删对象),实际命中 %d 行(hash=%s)",
			tag.RowsAffected(), hash[:12])
	}
}

// ---- DB/存储读数(全部返回实际值,失败信息里直接可见)----

func (e *raceEnv) hasObjectRow(t *testing.T, hash string) bool {
	t.Helper()
	var n int
	if err := e.database.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("查对象行失败(hash=%s): %v", hash[:12], err)
	}
	return n > 0
}

// objectState 读对象行四态 + 引用计数(行不存在即用例失败)。
func (e *raceEnv) objectState(t *testing.T, hash string) (ref int, state string, size int64, deleteAfter *time.Time) {
	t.Helper()
	if err := e.database.Pool.QueryRow(context.Background(), `
SELECT ref_count, state, size, delete_after FROM file_objects WHERE hash_sha256 = $1`, hash).
		Scan(&ref, &state, &size, &deleteAfter); err != nil {
		t.Fatalf("读对象行失败(hash=%s): %v", hash[:12], err)
	}
	return ref, state, size, deleteAfter
}

// countFilesWithHash 数有多少行 files 指向该 hash(全库口径,与 ref_count 同口径)。
func (e *raceEnv) countFilesWithHash(t *testing.T, hash string) int {
	t.Helper()
	var n int
	if err := e.database.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE hash_sha256 = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("数引用行失败(hash=%s): %v", hash[:12], err)
	}
	return n
}

// countObjectFiles 数存储根 `<root>/objects` 下的物理对象文件个数。
//
// 暂存文件在 `<root>/tus-tmp`(不在这里),所以"对象文件数"与"上传了几次"无关:
// 内容寻址下同一 hash 永远只应有一个物理文件。
func (e *raceEnv) countObjectFiles(t *testing.T) int {
	t.Helper()
	root := filepath.Join(e.store.Root, "objects")
	n := 0
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历对象目录 %s 失败: %v", root, err)
	}
	return n
}

// assertObjectInvariants 是 TS-04 要求的**最终不变式断言**:
//
//	① 每个对象:ref_count == 引用它的 files 行数(不夸大也不遗漏);
//	② 表不变式 (ref_count = 0) ⟺ (state ≠ 'live');
//	③ state='live' 的对象,其物理文件在存储根下**真实存在**且大小一致(无悬空对象);
//	④ state='deleted' 的对象,其物理文件确实已删;
//	⑤ 没有任何 files 行指向一个不存在的对象行(悬空引用)。
func (e *raceEnv) assertObjectInvariants(t *testing.T, label string, hashes []string) {
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

		files := e.countFilesWithHash(t, h)
		if !e.hasObjectRow(t, h) {
			if files != 0 {
				t.Fatalf("[%s] 悬空引用:对象 %s 没有 file_objects 行,却有 %d 行 files 指向它",
					label, h[:12], files)
			}
			continue
		}
		ref, state, size, _ := e.objectState(t, h)
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
			got, serr := e.store.Stat(ctx, h)
			if serr != nil {
				t.Fatalf("[%s] 悬空对象:live 对象 %s 在存储根 %s 下缺失(Stat=%v)",
					label, h[:12], e.store.Root, serr)
			}
			if got != size {
				t.Fatalf("[%s] 对象 %s 物理大小=%d 与元数据 size=%d 不一致", label, h[:12], got, size)
			}
		case model.ObjectDeleted:
			if _, serr := e.store.Stat(ctx, h); !errors.Is(serr, storage.ErrNotFound) {
				t.Fatalf("[%s] deleted 对象 %s 的物理文件应已删除,实际 Stat=%v", label, h[:12], serr)
			}
		}
	}
}

// ============================================================
// 验收点①:同 hash 并发秒传 ×50
// ============================================================

// TestConcurrentFastUploadSameHash50:50 个 goroutine 对同一内容 hash 并发秒传。
//
// 前置刻意如此(而不是"先全量上传留一行种子"):种子全量上传后**再删掉那一行**,
// 于是对象行处于删除路径真实留下的 `pending_delete` + ref_count=0 + 24h 窗口,
// 50 路秒传必须并发地把它**复活**成 live。好处有两个:
//   - `files` 行数 == 成功数(没有种子行需要额外记账),可以直接做等值断言;
//   - 顺带覆盖"延迟删除窗口里被并发复活"这条真实路径(与验收点②同源)。
//
// 断言(全部打印实际值):
//   - 每个响应要么成功,要么是**文档化冲突**(apierr 4xx);**禁止 5xx/非结构化错误**;
//   - file_objects 对该 hash **恰好 1 行**(没有并发重复落行);
//   - ref_count == 引用它的 files 行数 == 成功数(50);
//   - 磁盘上**恰好 1 个**物理对象文件(秒传绝不重复落盘);
//   - 50 路全部成功(目标名互不相同,没有任何可预见的冲突)。
func TestConcurrentFastUploadSameHash50(t *testing.T) {
	e := setupRace(t)
	ctx := testCtx(t)
	// 额度不限额:本用例测的是引用计数与对象锁竞态,额度不是被测量对象;
	// 50×64KB 若撞上默认额度会引入与竞态无关的失败。
	e.setQuota(t, 0)

	// 64KB > fastupload.MinSize(32KB):低于下限的文件**刻意不给**秒传通道(细则①)。
	data := uniqueContent(t, "fast50", 64<<10)
	hash := hashOf(data)

	// 种子:走真实全量上传链路(TUS/multipart 同一条 finalizeUpload),让服务端真的
	// 持有该内容 —— 秒传的前提就是"服务端已有这份对象"。
	seed, err := e.svc.Create(ctx, uploadsvc.CreateInput{
		UserID: e.userID, SpaceID: e.spaceID, ParentID: e.rootID,
		Name: "fast50-seed.bin", DeclaredSize: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("种子建任务失败: %v", err)
	}
	seeded, err := e.tus.Patch(ctx, uploadsvc.PatchInput{
		UploadID: seed.UploadID, UserID: e.userID, Ticket: seed.Ticket,
		ExpectOffset: 0, Content: bytes.NewReader(data), ContentLength: int64(len(data)),
	})
	if err != nil {
		t.Fatalf("种子全量上传失败: %v", err)
	}
	e.made = append(e.made, hash)

	// 删掉种子那一行(与生产同一条删除路径):对象进入 24h 延迟删除窗口
	del, err := e.files.Delete(ctx, e.userID, seeded.File.ID)
	if err != nil {
		t.Fatalf("删除种子文件失败: %v", err)
	}
	if del.ReleasedObjects != 1 {
		t.Fatalf("删除最后一个引用应把对象转入延迟删除窗口,实际 ReleasedObjects=%d(%+v)",
			del.ReleasedObjects, del)
	}
	if ref, state, _, after := e.objectState(t, hash); ref != 0 || state != model.ObjectPendingDelete || after == nil {
		t.Fatalf("前置:对象应处于 ref=0/pending_delete/有 delete_after,实际 ref=%d state=%s delete_after=%v",
			ref, state, after)
	}

	const n = 50
	results := make([]fastOutcome, n)
	var finished atomic.Int64
	raceCtx, cancel := context.WithTimeout(ctx, concurTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer finished.Add(1)
			results[i] = e.fastUploadOnce(raceCtx, i, hash, data)
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-raceCtx.Done():
		t.Fatalf("50 路并发秒传未在 %v 内完成(疑似死锁/同 hash 对象锁被长期持有):%d/%d 路已有结果",
			concurTimeout, finished.Load(), n)
	}

	success := 0
	var fatal []string
	var conflicts []string
	for i, r := range results {
		switch {
		case r.err == nil:
			success++
			if !r.newFile || !r.deduped {
				t.Errorf("第 %d 路秒传应新建 files 行且命中去重复用(new_file=%v deduped=%v)",
					i, r.newFile, r.deduped)
			}
		default:
			var ae *apierr.Error
			if !errors.As(r.err, &ae) {
				fatal = append(fatal, fmt.Sprintf("#%d 非结构化错误(内部错误/500 的典型形态):%v", i, r.err))
				continue
			}
			if ae.Status >= 500 {
				fatal = append(fatal, fmt.Sprintf("#%d 5xx:%d/%s %v", i, ae.Status, ae.Code, r.err))
				continue
			}
			conflicts = append(conflicts, fmt.Sprintf("#%d %d/%s", i, ae.Status, ae.Code))
		}
	}
	if len(fatal) > 0 {
		t.Fatalf("并发秒传出现 5xx/内部错误(验收点①明确禁止):实际 %d 例:%v", len(fatal), fatal)
	}
	if success != n {
		t.Fatalf("50 路并发秒传应全部成功(目标名互不相同),实际成功 %d 路、失败 %d 路:%v",
			success, len(conflicts), conflicts)
	}

	// ---- 引用计数语义 ----
	fileRows := e.countFilesWithHash(t, hash)
	ref, state, size, deleteAfter := e.objectState(t, hash)
	if fileRows != success {
		t.Fatalf("files 行数应 == 成功数(种子行已在竞速前删除):成功 %d,实际 files=%d",
			success, fileRows)
	}
	if ref != fileRows {
		t.Fatalf("ref_count 应等于引用它的 files 行数:期望 %d,实际 ref_count=%d", fileRows, ref)
	}
	if state != model.ObjectLive {
		t.Fatalf("50 路秒传复活后对象状态应为 live,实际 %s", state)
	}
	if deleteAfter != nil {
		t.Fatalf("复活后 delete_after 应被清空(否则清理 worker 之后会删掉它),实际 %v", *deleteAfter)
	}
	if size != int64(len(data)) {
		t.Fatalf("对象 size 应为 %d,实际 %d", len(data), size)
	}
	// ---- 无重复对象行 / 恰好一个物理对象 ----
	var objRows int
	if err := e.database.Pool.QueryRow(ctx,
		`SELECT count(*) FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&objRows); err != nil {
		t.Fatalf("数对象行失败: %v", err)
	}
	if objRows != 1 {
		t.Fatalf("同一 hash 只应有 1 行 file_objects(出现 >1 = 并发落行失败),实际 %d", objRows)
	}
	if got := e.countObjectFiles(t); got != 1 {
		t.Fatalf("磁盘上应恰好 1 个物理对象文件(内容寻址),实际 %d", got)
	}
	if got, serr := e.store.Stat(ctx, hash); serr != nil || got != int64(len(data)) {
		t.Fatalf("对象 %s 应可 stat 且大小 %d,实际 size=%d err=%v", hash[:12], len(data), got, serr)
	}
	if ref != n {
		t.Fatalf("期望对象被 %d 行引用(50 路秒传各新建一行),实际 %d", n, ref)
	}
	e.assertObjectInvariants(t, "并发秒传×50", []string{hash})
}

// ============================================================
// 验收点②:清理 vs 复活交错
// ============================================================

// TestConcurrentCleanupNotDeletingLiveObject 是验收点②的**确定性**一半。
//
// 状态机(见 lifecycle 包注释):live ──ref 归零──▶ pending_delete ──领取──▶ deleting ──▶ deleted。
// 代码保证的两件事在下面逐条断言:
//  1. 引用归零立刻进入 pending_delete + `delete_after = now()+24h`(延迟窗口);
//  2. 窗口内 worker 不领取;复活(秒传)把 ref 加回 1 并置 live / 清空 delete_after;
//  3. 复活之后**即使把 delete_after 强行拨到过去**,worker 也不得删除仍被引用的对象
//     (领取条件里的 `ref_count = 0` 就是这条保证)。
func TestConcurrentCleanupNotDeletingLiveObject(t *testing.T) {
	e := setupRace(t)
	ctx := testCtx(t)
	e.setQuota(t, 0)

	data := uniqueContent(t, "cleanup-det", 48<<10)
	hash, fileID := e.seedObjectWithFile(t, "det-del.bin", data)

	// 删除唯一引用(与生产同一条路径:filesvc.Delete → repo.ReleaseObjectRef)
	del, err := e.files.Delete(ctx, e.userID, fileID)
	if err != nil {
		t.Fatalf("删除文件失败: %v", err)
	}
	if del.ReleasedObjects != 1 {
		t.Fatalf("删除最后一个引用应回收 1 个物理对象,实际 ReleasedObjects=%d(%+v)", del.ReleasedObjects, del)
	}
	ref, state, _, after := e.objectState(t, hash)
	if ref != 0 || state != model.ObjectPendingDelete || after == nil {
		t.Fatalf("删除后应进入 ref=0/pending_delete/有 delete_after,实际 ref=%d state=%s delete_after=%v",
			ref, state, after)
	}
	if !after.After(time.Now()) {
		t.Fatalf("延迟删除窗口应在未来(代码写死 now()+24h),实际 delete_after=%v(now=%v)",
			after, time.Now())
	}

	// 窗口未到期 → worker 不得领取/删除(延迟窗口的全部意义就是给复活留时间)
	sweep, err := e.lc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("清理轮失败: %v", err)
	}
	if sweep.Deleted != 0 || sweep.Scanned != 0 {
		t.Fatalf("延迟窗口未到期时不得领取/删除对象,实际 %+v", sweep)
	}
	if _, serr := e.store.Stat(ctx, hash); serr != nil {
		t.Fatalf("窗口内对象必须仍在存储中(否则复活无内容可复用),实际 Stat=%v", serr)
	}

	// 复活:秒传(ref 0 → 1,state → live,delete_after 清空)
	fr := e.mustFastUpload(ctx, t, "det-revive.bin", hash, data)
	if !fr.Deduped {
		t.Fatalf("复活路径应走命中去重(未重写物理对象),实际 Deduped=%v NewFile=%v", fr.Deduped, fr.NewFile)
	}
	ref, state, _, after = e.objectState(t, hash)
	if ref != 1 || state != model.ObjectLive || after != nil {
		t.Fatalf("复活后应为 ref=1/live/delete_after=NULL,实际 ref=%d state=%s delete_after=%v",
			ref, state, after)
	}

	// 把 delete_after 拨到过去(模拟 24h 已过)→ 仍被引用 ⇒ 清理不得删
	if _, err := e.database.Pool.Exec(ctx,
		`UPDATE file_objects SET delete_after = now() - interval '1 hour' WHERE hash_sha256 = $1`, hash); err != nil {
		t.Fatalf("造过期 delete_after 失败: %v", err)
	}
	sweep, err = e.lc.RunOnceFor(ctx, []string{hash}, 10)
	if err != nil {
		t.Fatalf("清理轮失败: %v", err)
	}
	if sweep.Deleted != 0 {
		t.Fatalf("仍被 live 引用时清理不得删除对象,实际 %+v", sweep)
	}
	ref, state, _, _ = e.objectState(t, hash)
	if ref != 1 || state != model.ObjectLive {
		t.Fatalf("对象应保持 live/ref=1,实际 ref=%d state=%s", ref, state)
	}
	got, serr := e.store.Stat(ctx, hash)
	if serr != nil || got != int64(len(data)) {
		t.Fatalf("对象必须仍在且大小正确:stat=%d err=%v(期望 %d)", got, serr, len(data))
	}
	if n := e.countFilesWithHash(t, hash); n != 1 {
		t.Fatalf("复活后应恰有 1 行 files 指向该对象,实际 %d", n)
	}
	e.assertObjectInvariants(t, "清理不删活引用", []string{hash})
}

// TestConcurrentCleanupVsReviveInterleave 是验收点②的**竞态**一半:
// 多轮"清理 worker 领取物理删除"与"秒传复活"同时起跑。
//
// 每轮固定序列:
//
//	删除唯一引用(ref=1→0,pending_delete,窗口 +24h)
//	→ 把窗口拨到过去(worker 此刻真的会去领取)
//	→ 并发:RunOnceFor(hash)  vs  FinishFast(挑战已在竞速前取好)
//
// 合法结局只有两种(下面逐一断言,且**都**要满足 ref_count == count(files)):
//
//	A. 复活先拿到对象锁 → worker 领取条件 ref_count=0 不再成立 → 跳过;
//	   state=live,ref=1,files=1,物理对象在且可读,deleted 计数 0。
//	B. worker 先删掉 → 复活方必须**文档化失败**(fast_upload_denied,非 5xx),
//	   且库里不得留下任何指向该对象的 files 行;worker 把它标成 deleted 并物理删除。
//
// **绝不允许**的第三态是"live 但物理对象不在"(悬空对象)—— 由 assertObjectInvariants 兜底。
func TestConcurrentCleanupVsReviveInterleave(t *testing.T) {
	e := setupRace(t)
	ctx := testCtx(t)
	e.setQuota(t, 0)

	const rounds = 10
	reviveWins, sweepWins := 0, 0
	var sweepDeleted, sweepFailed int

	for round := 0; round < rounds; round++ {
		data := uniqueContent(t, fmt.Sprintf("interleave-%02d", round), 48<<10)
		hash, fileID := e.seedObjectWithFile(t, fmt.Sprintf("il-del-%02d.bin", round), data)

		// 竞速前先建好秒传任务并取到挑战(此刻对象 live ⇒ 预检必命中)。
		// 把建任务排除在竞速之外:Create 走的是"预检 + 挑战下发",那不是本用例要测的竞态。
		name := fmt.Sprintf("il-revive-%02d.bin", round)
		res, err := e.svc.Create(ctx, uploadsvc.CreateInput{
			UserID: e.userID, SpaceID: e.spaceID, ParentID: e.rootID,
			Name: name, DeclaredSize: int64(len(data)), DeclaredHash: hash,
		})
		if err != nil {
			t.Fatalf("第 %d 轮秒传建任务失败: %v", round, err)
		}
		if res.FastUpload == nil {
			t.Fatalf("第 %d 轮:对象 live 且物理存在,预检必须下发挑战(hash=%s)", round, hash[:12])
		}
		digests, derr := fastupload.SampleDigests(
			bytes.NewReader(data), res.FastUpload.Offsets, res.FastUpload.SampleLen)
		if derr != nil {
			t.Fatalf("第 %d 轮算样本摘要失败: %v", round, derr)
		}
		up, err := e.svc.Authorize(ctx, res.UploadID, e.userID, res.Ticket)
		if err != nil {
			t.Fatalf("第 %d 轮 ticket 校验失败: %v", round, err)
		}

		// 删除唯一引用 → 进入延迟删除窗口
		if _, err := e.files.Delete(ctx, e.userID, fileID); err != nil {
			t.Fatalf("第 %d 轮删除文件失败: %v", round, err)
		}
		if ref, state, _, _ := e.objectState(t, hash); ref != 0 || state != model.ObjectPendingDelete {
			t.Fatalf("第 %d 轮:删除后应 ref=0/pending_delete,实际 ref=%d state=%s", round, ref, state)
		}
		e.forceDeleteDue(t, hash)

		var sweep *lifecycle.Result
		errs := runConcurrent(t, concurTimeout, []raceOp{
			{name: fmt.Sprintf("sweep#%d", round), fn: func(c context.Context) error {
				sr, serr := e.lc.RunOnceFor(c, []string{hash}, 10)
				sweep = sr
				return serr
			}},
			{name: fmt.Sprintf("revive#%d", round), fn: func(c context.Context) error {
				_, rerr := e.tus.FinishFast(c, up, res.FastUpload.Nonce, digests)
				return rerr
			}},
		})
		if errs[0] != nil {
			t.Fatalf("第 %d 轮清理 worker 出错: %v", round, errs[0])
		}
		if sweep == nil {
			t.Fatalf("第 %d 轮清理 worker 未返回统计(死锁或实现变更)", round)
		}

		ref, state, _, after := e.objectState(t, hash)
		files := e.countFilesWithHash(t, hash)
		if ref != files {
			t.Fatalf("第 %d 轮不变式破裂:对象 %s 的 ref_count=%d,引用它的 files 行数=%d(state=%s)",
				round, hash[:12], ref, files, state)
		}

		if errs[1] == nil {
			// 结局 A:复活赢
			reviveWins++
			sweepDeleted += sweep.Deleted
			sweepFailed += sweep.Failed
			if state != model.ObjectLive || ref != 1 || files != 1 {
				t.Fatalf("第 %d 轮复活成功后应为 live/ref=1/1 行引用,实际 ref=%d state=%s files=%d",
					round, ref, state, files)
			}
			if after != nil {
				t.Fatalf("第 %d 轮复活后 delete_after 应清空,实际 %v", round, *after)
			}
			if sweep.Deleted != 0 {
				t.Fatalf("第 %d 轮复活已拿到引用,worker 不得删除对象,实际 sweep=%+v", round, sweep)
			}
			if got, serr := e.store.Stat(ctx, hash); serr != nil || got != int64(len(data)) {
				t.Fatalf("第 %d 轮复活对象必须仍在且完整:stat=%d err=%v(期望 %d)", round, got, serr, len(data))
			}
			continue
		}

		// 结局 B:worker 赢(复活方失败)—— 失败必须是**文档化**的 4xx,绝不是 500
		sweepWins++
		sweepDeleted += sweep.Deleted
		sweepFailed += sweep.Failed
		var ae *apierr.Error
		if !errors.As(errs[1], &ae) {
			t.Fatalf("第 %d 轮复活失败方必须是结构化 apierr(而非内部 500),实际 %v", round, errs[1])
		}
		if ae.Status >= 500 {
			t.Fatalf("第 %d 轮复活失败不得是 5xx,实际 %d/%s(%v)", round, ae.Status, ae.Code, errs[1])
		}
		if ae.Code != apierr.CodeFastUploadDenied {
			t.Fatalf("第 %d 轮对象已被清理时复活应报 %s(见 finalize.stage/verifyProof),实际 %s(%v)",
				round, apierr.CodeFastUploadDenied, ae.Code, errs[1])
		}
		if files != 0 {
			t.Fatalf("第 %d 轮复活失败不得留下 files 行(半写状态),实际 %d 行", round, files)
		}
		if ref != 0 || state == model.ObjectLive {
			t.Fatalf("第 %d 轮复活失败后对象不应是 live,实际 ref=%d state=%s", round, ref, state)
		}
		if state == model.ObjectDeleted {
			if _, serr := e.store.Stat(ctx, hash); !errors.Is(serr, storage.ErrNotFound) {
				t.Fatalf("第 %d 轮 deleted 对象的物理文件应已删除,实际 Stat=%v", round, serr)
			}
			if sweep.Deleted != 1 {
				t.Fatalf("第 %d 轮复活失败且行已 deleted 时,worker 应删掉 1 个对象,实际 %+v", round, sweep)
			}
		} else {
			// pending_delete/deleting:worker 这轮没删成(例如物理删除失败会退回 pending_delete 下轮重试),
			// 这是状态机允许的中间态 —— 但必须仍满足"无悬空元数据"。
			t.Logf("第 %d 轮观察中间态:state=%s ref=%d sweep=%+v(下轮重试)", round, state, ref, sweep)
		}
	}

	t.Logf("清理 vs 复活交错完成 %d 轮:复活赢 %d 轮、清理赢 %d 轮;worker 累计 deleted=%d failed=%d;made=%d",
		rounds, reviveWins, sweepWins, sweepDeleted, sweepFailed, len(e.made))
	if reviveWins+sweepWins != rounds {
		t.Fatalf("轮次对不上:复活赢 %d + 清理赢 %d != %d", reviveWins, sweepWins, rounds)
	}
	e.assertObjectInvariants(t, "清理vs复活交错", e.made)
}

// ============================================================
// 验收点③:并发双覆盖 → 一成功一 409(同名并发定稿形态)
// ============================================================

// TestConcurrentFinalizeSameNameOneWins:两个上传任务(不同内容)并发往**同一个名字**定稿。
//
// 代码里的裁决点:`finalize.upsertFileRow` —— 目标同名且未显式声明覆盖时返回
// 409 `name_conflict`("同目录下已存在同名文件: %s"),或唯一索引
// (space_id,parent_id,lower(name)) 冲突被 repo.InsertRow 映射为 ErrConflict 后
// 同样落成 409 name_conflict。因此断言:恰好一个成功、另一个 409/name_conflict。
//
// 失败方**不得**留下任何元数据(它的事务整体回滚),因此这里逐项断言:
// 无 file_objects 行、无 files 行;胜者那一行 version=1、hash=胜者内容、
// ref_count == 1 == 引用行数、物理对象在且大小正确。
func TestConcurrentFinalizeSameNameOneWins(t *testing.T) {
	e := setupRace(t)
	ctx := testCtx(t)
	e.setQuota(t, 0)

	dataA := uniqueContent(t, "fin-a", 16<<10)
	dataB := uniqueContent(t, "fin-b", 16<<10)
	hashA, hashB := hashOf(dataA), hashOf(dataB)
	if hashA == hashB {
		t.Fatal("用例自身有问题:两份内容 hash 相同,测不出并发覆盖")
	}
	target := fmt.Sprintf("race-same-name-%d.bin", time.Now().UnixNano())

	// 两个任务**先用不同名字**建:Create 的 4b 会拦"同目录进行中的同名上传",
	// 因此"同一时刻存在两个指向同名目标的进行中任务"只能这样造出来 ——
	// 这也正是两个客户端各自建任务、并发定稿到同一位置的形态。
	mkTask := func(data []byte, tmp string) (*model.Upload, string) {
		t.Helper()
		res, err := e.svc.Create(ctx, uploadsvc.CreateInput{
			UserID: e.userID, SpaceID: e.spaceID, ParentID: e.rootID,
			Name: tmp, DeclaredSize: int64(len(data)),
		})
		if err != nil {
			t.Fatalf("建任务 %s 失败: %v", tmp, err)
		}
		if _, err := e.database.Pool.Exec(ctx,
			`UPDATE uploads SET name = $2 WHERE id = $1`, res.UploadID, target); err != nil {
			t.Fatalf("把任务 %s 的目标名改成 %s 失败: %v", tmp, target, err)
		}
		up, err := e.svc.Authorize(ctx, res.UploadID, e.userID, res.Ticket)
		if err != nil {
			t.Fatalf("任务 %s ticket 校验失败: %v", tmp, err)
		}
		return up, res.Ticket
	}
	upA, ticketA := mkTask(dataA, "race-tmp-a.bin")
	upB, ticketB := mkTask(dataB, "race-tmp-b.bin")

	var resA, resB *uploadsvc.PatchResult
	errs := runConcurrent(t, concurTimeout, []raceOp{
		{name: "finalize-a", fn: func(c context.Context) error {
			r, err := e.tus.Patch(c, uploadsvc.PatchInput{
				UploadID: upA.ID, UserID: e.userID, Ticket: ticketA,
				ExpectOffset: 0, Content: bytes.NewReader(dataA), ContentLength: int64(len(dataA)),
			})
			resA = r
			return err
		}},
		{name: "finalize-b", fn: func(c context.Context) error {
			r, err := e.tus.Patch(c, uploadsvc.PatchInput{
				UploadID: upB.ID, UserID: e.userID, Ticket: ticketB,
				ExpectOffset: 0, Content: bytes.NewReader(dataB), ContentLength: int64(len(dataB)),
			})
			resB = r
			return err
		}},
	})

	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatalf("两个并发同名单定稿都成功了(必须恰好一个):A=%v B=%v", errs[0], errs[1])
			}
			winner = i
		}
	}
	if winner == -1 {
		t.Fatalf("两个并发同名单定稿都失败了(必须恰好一个成功):A=%v B=%v", errs[0], errs[1])
	}
	loserErr := errs[1-winner]
	var ae *apierr.Error
	if !errors.As(loserErr, &ae) {
		t.Fatalf("失败方必须是结构化 apierr(而不是内部 500),实际 %v", loserErr)
	}
	if ae.Status != 409 {
		t.Fatalf("同名并发定稿的失败方应 409,实际 %d/%s(%v)", ae.Status, ae.Code, loserErr)
	}
	if ae.Code != apierr.CodeNameConflict {
		t.Fatalf("失败方业务码应为 %s(见 finalize.upsertFileRow 的 name_conflict),实际 %s(%v)",
			apierr.CodeNameConflict, ae.Code, loserErr)
	}

	winData, winHash, winRes := dataA, hashA, resA
	loserHash := hashB
	if winner == 1 {
		winData, winHash, winRes, loserHash = dataB, hashB, resB, hashA
	}
	if winRes == nil {
		t.Fatalf("胜者应返回定稿结果,实际 nil(winner=%d)", winner)
	}
	if winRes.NewFile != true {
		t.Fatalf("胜者应新建 files 行,实际 NewFile=%v", winRes.NewFile)
	}

	// 同名行必须恰好一行,且是胜者的内容、version=1
	var rowHash string
	var version, rowSize int64
	var sameName int
	if err := e.database.Pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE space_id=$1 AND lower(name)=lower($2)`,
		e.spaceID, target).Scan(&sameName); err != nil {
		t.Fatalf("数同名行失败: %v", err)
	}
	if sameName != 1 {
		t.Fatalf("同名位置应恰好 1 行 files,实际 %d", sameName)
	}
	if err := e.database.Pool.QueryRow(ctx,
		`SELECT coalesce(hash_sha256,''), version, size FROM files
		  WHERE space_id=$1 AND lower(name)=lower($2)`,
		e.spaceID, target).Scan(&rowHash, &version, &rowSize); err != nil {
		t.Fatalf("读同名行失败: %v", err)
	}
	if rowHash != winHash {
		t.Fatalf("落库内容应是胜者的 hash:期望 %s,实际 %s", winHash[:12], rowHash[:12])
	}
	if version != 1 {
		t.Fatalf("新建行的 version 应为 1(失败方不得改动它),实际 %d", version)
	}
	if rowSize != int64(len(winData)) {
		t.Fatalf("胜者行 size 应为 %d,实际 %d", len(winData), rowSize)
	}
	// 胜者对象:引用数 = 引用行数 = 1,物理对象完整
	ref, state, size, _ := e.objectState(t, winHash)
	if ref != 1 || state != model.ObjectLive {
		t.Fatalf("胜者对象应为 live/ref=1,实际 ref=%d state=%s", ref, state)
	}
	if size != int64(len(winData)) {
		t.Fatalf("胜者对象 size 应为 %d,实际 %d", len(winData), size)
	}
	if got, serr := e.store.Stat(ctx, winHash); serr != nil || got != int64(len(winData)) {
		t.Fatalf("胜者对象必须落盘且完整:stat=%d err=%v(期望 %d)", got, serr, len(winData))
	}
	// 失败方不得留下任何元数据(无悬空行、无半写行)
	if n := e.countFilesWithHash(t, loserHash); n != 0 {
		t.Fatalf("失败方内容不得留下 files 行,实际 %d 行(hash=%s)", n, loserHash[:12])
	}
	if e.hasObjectRow(t, loserHash) {
		t.Fatalf("失败方内容不得留下 file_objects 行(其事务应整体回滚),hash=%s", loserHash[:12])
	}
	// 已知行为(不判失败):finalize 的"先写对象再短事务写元数据"意味着失败方可能
	// 已经在磁盘上留下一个**无元数据指向**的对象文件。它是 patrol 的"对象泄漏
	// (PG < 磁盘)"口径,不是悬空指针(没有任何行指向它)。
	if _, serr := e.store.Stat(ctx, loserHash); serr == nil {
		t.Logf("已知行为:失败方已落盘的物理对象 %s 无元数据指向(patrol 的对象泄漏口径,非悬空指针)",
			loserHash[:12])
	}
	e.made = append(e.made, hashA, hashB)
	e.assertObjectInvariants(t, "同名并发定稿", []string{hashA, hashB})
}
