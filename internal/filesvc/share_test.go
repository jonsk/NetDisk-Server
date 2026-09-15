package filesvc_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 共享到空间(BE-S6-03)与目录复制(BE-S7-03)的集成测试。
//
// 两条路径共用同一套"引用 +1"纪律(见 share.go 顶部注释),因此验收点也一致:
// 内容不复制、引用在同事务内 +1、配额按逻辑行计、多对象按 hash 升序取锁。

// ---- 辅助 ----

// countStoredObjects 数本用例存储根下的物理对象文件个数。
//
// 这是"内容没有被复制"的**唯一直接证据**:内容寻址库里同 hash 只对应一个
// 物理文件,共享/复制若误写成真复制,这里就会多出文件。
func (f *fixture) countStoredObjects(t *testing.T) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(f.storeRoot, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历存储根失败: %v", err)
	}
	return n
}

// filesWithHash 数有多少条 files 行指向该 hash(即有多少个逻辑引用)。
func (f *fixture) filesWithHash(t *testing.T, hash string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE hash_sha256 = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("数引用行失败: %v", err)
	}
	return n
}

// spaceUsed 读空间的已用配额。
func (f *fixture) spaceUsed(t *testing.T, spaceID string) int64 {
	t.Helper()
	var used int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT used_bytes FROM spaces WHERE id = $1`, spaceID).Scan(&used); err != nil {
		t.Fatalf("读空间配额失败: %v", err)
	}
	return used
}

// setQuota 设置空间总额度(0 = 不限)。
func (f *fixture) setQuota(t *testing.T, spaceID string, quota int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE spaces SET quota_bytes = $2 WHERE id = $1`, spaceID, quota); err != nil {
		t.Fatalf("设置额度失败: %v", err)
	}
}

// mkTeamSpaceOwned 建一个 f.userID 自己拥有的团队空间(owner = manager,可写)。
func (f *fixture) mkTeamSpaceOwned(t *testing.T, name string) (spaceID, rootID string) {
	t.Helper()
	return f.mkTeamSpace(t, f.userID, name)
}

// holdObjectLock 在**另一条连接**上持有会话级对象锁,返回释放函数。
//
// 用它验证"共享/复制确实取了对象锁":本连接持有同键会话级锁后,
// 被测路径必须在 `pg_advisory_xact_lock` 上阻塞(PG 保证会话级与事务级同键互斥)。
func (f *fixture) holdObjectLock(t *testing.T, hash string) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, hash); err != nil {
		conn.Release()
		t.Fatalf("持锁失败: %v", err)
	}
	// 释放必须幂等:测试里既有 `defer release()` 又有失败路径的显式 release,
	// 二次 Release 会让连接被重复归还(实测:pgxpool 内部 panic)。
	var once sync.Once
	return func() {
		once.Do(func() {
			var ok bool
			_ = conn.QueryRow(context.Background(),
				`SELECT pg_advisory_unlock(hashtextextended($1,0))`, hash).Scan(&ok)
			conn.Release()
		})
	}
}

// setObjectState 直接改对象行(模拟清理 worker 已经接手/已经删完的窗口)。
func (f *fixture) setObjectState(t *testing.T, hash, state string, ref int) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
UPDATE file_objects SET ref_count = $2, state = $3,
       delete_after = CASE WHEN $3 = 'live' THEN NULL ELSE now() + interval '24 hours' END
 WHERE hash_sha256 = $1`, hash, ref, state); err != nil {
		t.Fatalf("改对象状态失败: %v", err)
	}
}

// ============================================================
// BE-S6-03 共享到空间
// ============================================================

// **验收①**:共享只新建元数据行,**内容不复制**(同一物理对象,引用 +1)
func TestShareCreatesMetadataRowWithoutCopyingContent(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("share-payload-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "report.pdf", data)
	teamID, teamRoot := f.mkTeamSpaceOwned(t, "共享目标空间")

	before := f.countStoredObjects(t)

	// 目标空间根目录下的共享(空 TargetParentID → 解析为空间根)
	v, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	if err != nil {
		t.Fatalf("共享失败: %v", err)
	}
	if v.Name != "report.pdf" {
		t.Fatalf("共享后应沿用原名,实际 %s", v.Name)
	}
	if v.HashSHA256 != hash {
		t.Fatalf("共享行必须指向同一内容 hash,实际 %s", v.HashSHA256)
	}
	if v.ID == srcID {
		t.Fatal("共享必须新建一行,不能复用源行")
	}
	if v.ParentID != teamRoot {
		t.Fatalf("应落在目标空间根目录 %s,实际 %s", teamRoot, v.ParentID)
	}

	// 内容不复制:物理文件数不变、对象行仍是那一个、引用变成 2
	if after := f.countStoredObjects(t); after != before {
		t.Fatalf("共享不应写入任何物理对象(前 %d 后 %d)", before, after)
	}
	var objRows int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&objRows); err != nil {
		t.Fatalf("数对象行失败: %v", err)
	}
	if objRows != 1 {
		t.Fatalf("同一内容只应有一个对象行,实际 %d", objRows)
	}
	if ref, state := f.objRef(t, hash); ref != 2 || state != model.ObjectLive {
		t.Fatalf("共享后对象应为 ref=2/live,实际 %d/%s", ref, state)
	}
	if n := f.filesWithHash(t, hash); n != 2 {
		t.Fatalf("应有 2 条逻辑行引用同一对象,实际 %d", n)
	}
}

// **验收③**:配额按**逻辑行**计 —— 目标空间占满 size,源空间不受影响
func TestShareChargesQuotaToTargetSpace(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("quota-share-payload-" + time.Now().Format("150405.000000000"))
	srcID, _ := f.mkFileAt(t, f.rootID, "big.bin", data)
	teamID, _ := f.mkTeamSpaceOwned(t, "配额目标空间")
	f.setQuota(t, teamID, 1<<30)
	f.setQuota(t, f.spaceID, 1<<30)
	// 让源空间的用量反映该文件
	if _, err := f.pool.Exec(ctx, `UPDATE spaces SET used_bytes = $2 WHERE id = $1`,
		f.spaceID, len(data)); err != nil {
		t.Fatalf("设置源用量失败: %v", err)
	}
	srcUsedBefore := f.spaceUsed(t, f.spaceID)

	if _, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	}); err != nil {
		t.Fatalf("共享失败: %v", err)
	}
	if got := f.spaceUsed(t, teamID); got != int64(len(data)) {
		t.Fatalf("目标空间用量应为 %d(按逻辑行计),实际 %d", len(data), got)
	}
	if got := f.spaceUsed(t, f.spaceID); got != srcUsedBefore {
		t.Fatalf("源空间用量不应被共享改动(前 %d 后 %d)", srcUsedBefore, got)
	}
	// 再共享一次(换个名字)→ 目标空间应再占一份(逻辑行计,不去重)
	if _, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID, NewName: "big-2.bin",
	}); err != nil {
		t.Fatalf("第二次共享失败: %v", err)
	}
	if got := f.spaceUsed(t, teamID); got != int64(len(data))*2 {
		t.Fatalf("两次共享应占两份额度(%d),实际 %d", len(data)*2, got)
	}
}

// **注入错误 → 整体回滚**:额度不足时,已加的引用必须回滚(引用+1 与元数据同事务)
func TestShareRollsBackRefWhenQuotaExceeded(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("rollback-share-payload-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "rb.bin", data)
	teamID, teamRoot := f.mkTeamSpaceOwned(t, "额度不足空间")
	// 额度比文件小 1 字节:引用 +1 与插行都会先"成功",配额那一步失败 → 全回滚
	f.setQuota(t, teamID, int64(len(data))-1)

	before := f.filesWithHash(t, hash)

	_, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	if err == nil {
		t.Fatal("额度不足应失败")
	}
	ae := asAPIErr(t, err)
	if ae.Status != 507 {
		t.Fatalf("额度不足应 507,实际 %d(%s): %v", ae.Status, ae.Code, err)
	}
	// 引用与行数都必须回到原样
	if ref, state := f.objRef(t, hash); ref != 1 || state != model.ObjectLive {
		t.Fatalf("失败后引用必须回滚为 1/live,实际 %d/%s", ref, state)
	}
	if n := f.filesWithHash(t, hash); n != before {
		t.Fatalf("失败后不应留下新行(前 %d 后 %d)", before, n)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE parent_id = $1`, teamRoot).Scan(&n)
	if n != 0 {
		t.Fatalf("失败后目标目录应为空,实际 %d 条", n)
	}
	if got := f.spaceUsed(t, teamID); got != 0 {
		t.Fatalf("失败后目标空间用量应为 0,实际 %d", got)
	}
}

// **验收②**:"引用 +1 必须在**持有对象锁**时进行" —— 别人持有同键锁时共享必须等待
func TestShareWaitsForObjectLock(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("locked-share-payload-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "locked.bin", data)
	teamID, teamRoot := f.mkTeamSpaceOwned(t, "锁等待空间")

	release := f.holdObjectLock(t, hash)

	// 短超时:必须在锁上阻塞直到上下文超时
	waitCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	_, err := f.svc.Share(waitCtx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	if err == nil {
		release()
		t.Fatal("对象锁被他人持有时,共享必须等待而不能直接成功")
	}
	// 等待超时后不得留下任何痕迹
	if ref, _ := f.objRef(t, hash); ref != 1 {
		t.Fatalf("等待失败后引用不应变化,实际 %d", ref)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE parent_id = $1`, teamRoot).Scan(&n)
	if n != 0 {
		t.Fatalf("等待失败后不应留下新行,实际 %d", n)
	}

	// 放锁后必须成功(证明刚才确实是在等这把锁,而不是因为别的原因失败)
	release()
	if _, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	}); err != nil {
		t.Fatalf("放锁后共享应成功: %v", err)
	}
	if ref, state := f.objRef(t, hash); ref != 2 || state != model.ObjectLive {
		t.Fatalf("放锁后应为 ref=2/live,实际 %d/%s", ref, state)
	}
}

// 命中 `pending_delete`(清理 worker 已把引用降到 0,但尚未 unlink)时必须**复活**对象行
func TestShareRevivesPendingDeleteObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("revive-share-payload-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "revive.bin", data)
	teamID, _ := f.mkTeamSpaceOwned(t, "复活空间")
	// 模拟竞态窗口:对象的引用已被降到 0 并排入删除队列,但物理对象还在
	f.setObjectState(t, hash, model.ObjectPendingDelete, 0)

	v, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	if err != nil {
		t.Fatalf("共享应复活 pending_delete 对象而不是报错: %v", err)
	}
	if v.HashSHA256 != hash {
		t.Fatalf("应指向原对象,实际 %s", v.HashSHA256)
	}
	ref, state := f.objRef(t, hash)
	if ref != 1 {
		t.Fatalf("复活后引用应为 1(0+1),实际 %d", ref)
	}
	if state != model.ObjectLive {
		t.Fatalf("复活后状态应为 live,实际 %s", state)
	}
	var after *time.Time
	_ = f.pool.QueryRow(ctx, `SELECT delete_after FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&after)
	if after != nil {
		t.Fatalf("复活后必须清掉 delete_after,实际 %v", after)
	}
}

// 对象已 `deleted`(物理文件已删)时**绝不允许**复活:否则得到指向空内容的行
func TestShareRejectsDeletedObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("deleted-share-payload-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "gone.bin", data)
	teamID, teamRoot := f.mkTeamSpaceOwned(t, "已删对象空间")
	f.setObjectState(t, hash, model.ObjectDeleted, 0)

	_, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	if err == nil {
		t.Fatal("对象已物理删除时必须报错,不能建出悬空引用")
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE parent_id = $1`, teamRoot).Scan(&n)
	if n != 0 {
		t.Fatalf("失败后不应留下新行,实际 %d", n)
	}
	if ref, state := f.objRef(t, hash); state != model.ObjectDeleted || ref != 0 {
		t.Fatalf("对象状态不应被改动,实际 %d/%s", ref, state)
	}
}

// 目标目录同名 → 409 reason=name_conflict(绝不静默改名,6.7)
func TestShareNameConflictInTarget(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("name-clash-share-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "dup.txt", data)
	teamID, teamRoot := f.mkTeamSpaceOwned(t, "重名空间")
	// 目标目录里先放一个同名条目
	if _, err := f.pool.Exec(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1,$2,$3,'dup.txt',false,1,1)`, teamID, teamRoot, f.userID); err != nil {
		t.Fatalf("造同名条目失败: %v", err)
	}

	_, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("同名应 409 name_conflict,实际 %d/%s", ae.Status, ae.Code)
	}
	if ae.Details["reason"] != filesvc.ReasonNameConflict {
		t.Fatalf("reason 应为 %s,实际 %v", filesvc.ReasonNameConflict, ae.Details["reason"])
	}
	if ref, _ := f.objRef(t, hash); ref != 1 {
		t.Fatalf("冲突时不应加引用,实际 %d", ref)
	}
}

// 共享目录暂不支持 → 400(目录走复制)
func TestShareDirRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dir := f.mkDirAt(t, f.rootID, "somedir", 1)
	teamID, _ := f.mkTeamSpaceOwned(t, "目录共享空间")
	_, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: dir, TargetSpaceID: teamID,
	})
	if asAPIErr(t, err).Status != 400 {
		t.Fatalf("共享目录应 400,实际 %v", err)
	}
}

// 只读成员不能把文件写进目标空间(4.3:权限判断只在服务端)
func TestShareRequiresWriteOnTarget(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("perm-share-" + time.Now().Format("150405.000000000"))
	srcID, hash := f.mkFileAt(t, f.rootID, "p.bin", data)
	// 目标空间属于**另一个人**,f.userID 只是 reader
	other := setup(t)
	teamID, _ := f.mkTeamSpace(t, other.userID, "别人只读空间")
	if err := (repo.SpaceRepo{}).AddMember(ctx, f.pool, teamID, f.userID, model.PermReader); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}

	_, err := f.svc.Share(ctx, filesvc.ShareInput{
		UserID: f.userID, FileID: srcID, TargetSpaceID: teamID,
	})
	if err == nil {
		t.Fatal("只读成员共享进目标空间应被拒")
	}
	if ref, _ := f.objRef(t, hash); ref != 1 {
		t.Fatalf("被拒时不应加引用,实际 %d", ref)
	}
}

// ============================================================
// BE-S7-03 目录复制
// ============================================================

// mkReusableTree 造一棵"内容有重复"的树:top/{a.txt, sub/{a-same.txt, b.txt}}
//
// a.txt 与 a-same.txt 内容相同 → 复制后该对象的引用应 +2(逐行各 +1),
// 而不是"每对象一次" —— 这正是"逐行 ref_count+1"与"按对象 +1"的差别,
// 也是悬空指针的来源(少加了就会早删)。
func (f *fixture) mkReusableTree(t *testing.T, prefix string) (topID string, hashes map[string]string) {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	shared := []byte(prefix + "-shared-" + suffix)
	unique := []byte(prefix + "-unique-" + suffix)
	top := f.mkDirAt(t, f.rootID, "top-"+suffix, 1)
	sub := f.mkDirAt(t, top, "sub", 2)
	f.mkFileAt(t, top, "a.txt", shared)
	f.mkFileAt(t, sub, "a-same.txt", shared)
	_, uniqueHash := f.mkFileAt(t, sub, "b.txt", unique)
	return top, map[string]string{
		"shared": hashOf(shared),
		"unique": uniqueHash,
	}
}

func hashOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// **验收①**:逐行 ref_count+1 且与元数据**同事务** —— 同内容的两个文件要各 +1
func TestCopyDirectoryIncrementsRefPerRow(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top, hashes := f.mkReusableTree(t, "copy")
	shared, unique := hashes["shared"], hashes["unique"]
	// 源树里 shared 有 2 行、unique 有 1 行
	if ref, _ := f.objRef(t, shared); ref != 2 {
		t.Fatalf("前置:shared 引用应为 2,实际 %d", ref)
	}
	before := f.countStoredObjects(t)
	usedBefore := f.spaceUsed(t, f.spaceID)

	res, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: top})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if res.CopiedFiles != 3 {
		t.Fatalf("应复制 3 个文件,实际 %d", res.CopiedFiles)
	}
	if res.CopiedDirs != 2 {
		t.Fatalf("应复制 2 个目录,实际 %d", res.CopiedDirs)
	}
	if res.ObjectRefsAdded != 2 {
		t.Fatalf("不同物理对象应为 2 个,实际 %d", res.ObjectRefsAdded)
	}
	// **逐行** +1:shared 2→4,unique 1→2
	if ref, state := f.objRef(t, shared); ref != 4 || state != model.ObjectLive {
		t.Fatalf("shared 引用应为 4(逐行 +1),实际 %d/%s", ref, state)
	}
	if ref, _ := f.objRef(t, unique); ref != 2 {
		t.Fatalf("unique 引用应为 2,实际 %d", ref)
	}
	// 逻辑行数翻倍,物理文件数不变(内容去重)
	if n := f.filesWithHash(t, shared); n != 4 {
		t.Fatalf("shared 应有 4 条逻辑行,实际 %d", n)
	}
	if after := f.countStoredObjects(t); after != before {
		t.Fatalf("复制不应写新物理对象(前 %d 后 %d)", before, after)
	}
	// 配额按新增逻辑字节计
	wantAdded := f.subtreeLogicalBytes(t, top)
	if got := f.spaceUsed(t, f.spaceID) - usedBefore; got != wantAdded {
		t.Fatalf("配额增量应为 %d,实际 %d", wantAdded, got)
	}
}

// subtreeLogicalBytes 算子树里非目录行的字节之和。
func (f *fixture) subtreeLogicalBytes(t *testing.T, rootID string) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(), `
WITH RECURSIVE sub AS (
    SELECT id, is_dir, size FROM files WHERE id = $1
    UNION ALL SELECT f.id, f.is_dir, f.size FROM files f JOIN sub ON f.parent_id = sub.id
) SELECT coalesce(sum(CASE WHEN NOT is_dir THEN size ELSE 0 END),0) FROM sub`, rootID).Scan(&n); err != nil {
		t.Fatalf("算子树字节失败: %v", err)
	}
	return n
}

// **验收②**:中途失败 → **整体回滚**(已加的引用与新行都必须消失)
func TestCopyRollsBackEntirelyOnFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top, hashes := f.mkReusableTree(t, "copyrb")
	shared, unique := hashes["shared"], hashes["unique"]
	refSharedBefore, _ := f.objRef(t, shared)
	refUniqueBefore, _ := f.objRef(t, unique)
	rowsBefore := f.countChildRows(t, f.rootID)
	usedBefore := f.spaceUsed(t, f.spaceID)

	// **注入错误**:把额度设成"差 1 字节"—— 引用 +1、新行插入都会先成功,
	// 最后一步配额检查失败,于是能验证前面所有步骤都真的在同一事务里。
	f.setQuota(t, f.spaceID, usedBefore+f.subtreeLogicalBytes(t, top)-1)

	_, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: top})
	if err == nil {
		t.Fatal("额度不足应失败")
	}
	if ae := asAPIErr(t, err); ae.Status != 507 {
		t.Fatalf("额度不足应 507,实际 %d(%s)", ae.Status, ae.Code)
	}
	// 全部回滚
	if ref, _ := f.objRef(t, shared); ref != refSharedBefore {
		t.Fatalf("shared 引用应回滚为 %d,实际 %d", refSharedBefore, ref)
	}
	if ref, _ := f.objRef(t, unique); ref != refUniqueBefore {
		t.Fatalf("unique 引用应回滚为 %d,实际 %d", refUniqueBefore, ref)
	}
	if rows := f.countChildRows(t, f.rootID); rows != rowsBefore {
		t.Fatalf("新行应全部回滚(前 %d 后 %d)", rowsBefore, rows)
	}
	if got := f.spaceUsed(t, f.spaceID); got != usedBefore {
		t.Fatalf("配额应回滚为 %d,实际 %d", usedBefore, got)
	}
}

// countChildRows 数某目录下的直接子行数。
func (f *fixture) countChildRows(t *testing.T, parentID string) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE parent_id = $1`, parentID).Scan(&n); err != nil {
		t.Fatalf("数子行失败: %v", err)
	}
	return n
}

// **验收③**:多对象按 **hash 升序**取锁。
//
// 契约里的"升序"是**实现自己定义的同一个全序**(hash 字符串升序),关键是
// "所有路径都用同一个序" —— 死锁防护靠的是全序,不是某个特定的序。
// 因此测试也按 hash 字符串升序挑出"第 3 个"作为阻塞点。
//
// 做法:从另一条连接持有第 3 个 hash 的会话级锁,然后在 goroutine 里发起复制。
// 若实现确实按升序取锁,它此时应当已经拿到前两个、正卡在第 3 个上、**完全没碰**
// 第 4 个。反过来若实现不排序(按子树遍历顺序),就会出现"第 4 个已被持有时
// 第 1 个还没取到"或"卡在了一个不是第 3 个的 hash 上"。
func TestCopyTakesObjectLocksInAscendingOrder(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	top := f.mkDirAt(t, f.rootID, "lockdir-"+suffix, 1)
	// 四个内容不同的文件 → 四个不同 hash。
	//
	// 关键:要挑一组内容,使 **hash 升序 ≠ 子树遍历序**(遍历序是 `ORDER BY lvl,name`
	// 即 f0,f1,f2,f3)。否则"按遍历顺序取锁"的错误实现也能通过本测试 ——
	// 那正是本节要抓的 bug。
	contents := pickContentsWhereHashOrderDiffersFromNameOrder(suffix)
	var hashes []string
	for i, c := range contents {
		_, h := f.mkFileAt(t, top, fmt.Sprintf("f%d.txt", i), c)
		hashes = append(hashes, h)
	}
	// 与实现同序:hash 字符串升序
	sorted := append([]string(nil), hashes...)
	sort.Strings(sorted)
	blocked := sorted[2] // 第 3 个

	release := f.holdObjectLock(t, blocked)
	defer release()

	done := make(chan error, 1)
	go func() {
		_, cerr := f.svc.Copy(ctx, f.copyInput(top))
		done <- cerr
	}()

	// 等到"第 3 个上出现未授予的等待者"
	var rows []lockState
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rows = f.lockStates(t, hashes)
		if lockBlocked(rows, blocked) {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !lockBlocked(rows, blocked) {
		release()
		<-done
		t.Fatalf("未能观察到复制在第 3 个 hash 上等待(说明取锁序不是 hash 升序):%s", dumpLocks(rows))
	}
	// 已授予:前两个必须都在手上
	for i := 0; i < 2; i++ {
		if !lockGranted(rows, sorted[i]) {
			release()
			<-done
			t.Fatalf("升序取锁时第 %d 个 hash 应先被取得:%s", i+1, dumpLocks(rows))
		}
	}
	// 第 4 个必须**完全没被碰过**(连等待都不该有)
	if lockHeld(rows, sorted[3]) {
		release()
		<-done
		t.Fatalf("升序取锁时不应先碰到第 4 个 hash:%s", dumpLocks(rows))
	}

	// 放锁后复制必须能完成(证明它一直在等这把锁)
	release()
	select {
	case cerr := <-done:
		if cerr != nil {
			t.Fatalf("放锁后复制应成功: %v", cerr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("放锁后复制未完成")
	}
}

// copyInput 构造复制入参(集中一处,便于调整)。
func (f *fixture) copyInput(fileID string) filesvc.CopyInput {
	return filesvc.CopyInput{UserID: f.userID, FileID: fileID}
}

// pickContentsWhereHashOrderDiffersFromNameOrder 返回 4 份内容,使得
// 按 hash 字符串升序排列的**文件下标顺序**不等于 0,1,2,3(即名字序)。
//
// 内容在插入前就能算 hash,因此这里不需要先插再删:直接试到满足条件为止。
// 4 个 hash 恰好与名字序一致的概率是 1/24,试 200 次失败的可能是天文数字;
// 真失败说明测试本身有问题(例如 hashOf 恒定),必须显式报错而不是静默放行。
func pickContentsWhereHashOrderDiffersFromNameOrder(suffix string) [][]byte {
	for attempt := 0; attempt < 200; attempt++ {
		out := make([][]byte, 4)
		for i := range out {
			out[i] = []byte(fmt.Sprintf("lock-order-%s-a%d-f%d", suffix, attempt, i))
		}
		idx := []int{0, 1, 2, 3}
		sort.Slice(idx, func(a, b int) bool {
			return hashOf(out[idx[a]]) < hashOf(out[idx[b]])
		})
		same := true
		for i, v := range idx {
			if v != i {
				same = false
				break
			}
		}
		if !same {
			return out
		}
	}
	panic("无法构造出\"hash 序 ≠ 名字序\"的内容(测试自身有问题)")
}

// lockState 是"某个 hash 对应的 advisory 锁在 pg_locks 里的一行状态"。
//
// 同一个 key 可能出现**多行**:持有者(已授予)与等待者(未授予)各一行 ——
// 这正是本测试要区分的信息,所以不去重。
type lockState struct {
	Hash    string
	Key     int64
	Granted bool
	Held    bool // pg_locks 里是否能看到(授予或等待)
}

// lockStates 查本组 hash 的 advisory 锁现状。
//
// 只统计**本库**的 advisory 锁,并且只保留键属于本组 hash 的行 ——
// 于是别的测试包(并行运行的其它包)的锁不会污染断言。
func (f *fixture) lockStates(t *testing.T, hashes []string) []lockState {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `
WITH mine AS (
    SELECT h AS hash, hashtextextended(h, 0) AS key FROM unnest($1::text[]) AS h
), adv AS (
    SELECT pid, granted,
           CASE WHEN u >= 9223372036854775808
                THEN u - 18446744073709551616 ELSE u END AS key
      FROM (
        SELECT pid, granted,
               (classid::bigint::numeric * 4294967296 + objid::bigint::numeric) AS u
          FROM pg_locks
         WHERE locktype = 'advisory' AND objsubid = 1
           AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
      ) x
)
SELECT m.hash, m.key, coalesce(a.granted, false), a.pid IS NOT NULL
  FROM mine m LEFT JOIN adv a ON a.key = m.key
 ORDER BY m.key`, hashes)
	if err != nil {
		t.Fatalf("查 pg_locks 失败: %v", err)
	}
	defer rows.Close()
	var out []lockState
	for rows.Next() {
		var s lockState
		if err := rows.Scan(&s.Hash, &s.Key, &s.Granted, &s.Held); err != nil {
			t.Fatalf("扫描 pg_locks 失败: %v", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历 pg_locks 失败: %v", err)
	}
	return out
}

// lockBlocked 判断该 hash 上是否有人**正在等待**锁。
func lockBlocked(rows []lockState, hash string) bool {
	for _, s := range rows {
		if s.Hash == hash && s.Held && !s.Granted {
			return true
		}
	}
	return false
}

// lockGranted 判断该 hash 的锁是否**已被授予**。
func lockGranted(rows []lockState, hash string) bool {
	for _, s := range rows {
		if s.Hash == hash && s.Held && s.Granted {
			return true
		}
	}
	return false
}

// lockHeld 判断该 hash 的锁是否被任何人碰过(授予或等待)。
func lockHeld(rows []lockState, hash string) bool {
	for _, s := range rows {
		if s.Hash == hash && s.Held {
			return true
		}
	}
	return false
}

// dumpLocks 把锁状态拼成人可读的一行,失败信息里能直接看出取锁顺序。
func dumpLocks(rows []lockState) string {
	var b []string
	for _, s := range rows {
		short := s.Hash
		if len(short) > 8 {
			short = short[:8]
		}
		switch {
		case !s.Held:
			b = append(b, short+"=未取")
		case s.Granted:
			b = append(b, short+"=已授予")
		default:
			b = append(b, short+"=等待中")
		}
	}
	return "[" + fmt.Sprint(b) + "]"
}

// 复制文件(非目录)只产生一行,引用 +1
func TestCopySingleFile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("single-copy-" + time.Now().Format("150405.000000000"))
	id, hash := f.mkFileAt(t, f.rootID, "one.txt", data)

	res, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: id})
	if err != nil {
		t.Fatalf("复制文件失败: %v", err)
	}
	if res.CopiedFiles != 1 || res.CopiedDirs != 0 {
		t.Fatalf("应复制 1 文件 0 目录,实际 %d/%d", res.CopiedFiles, res.CopiedDirs)
	}
	if res.Root == nil || res.Root.Name != "one.txt (副本)" {
		t.Fatalf("未指定新名时应自动生成 \"one.txt (副本)\",实际 %+v", res.Root)
	}
	if res.Root.HashSHA256 != hash {
		t.Fatalf("副本应指向同一对象,实际 %s", res.Root.HashSHA256)
	}
	if ref, _ := f.objRef(t, hash); ref != 2 {
		t.Fatalf("引用应为 2,实际 %d", ref)
	}
}

// 未指定新名且"原名 (副本)"已被占 → 自动追加序号,而不是报冲突
func TestCopyAutoNameAppendsSequence(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	data := []byte("auto-name-" + suffix)
	id, _ := f.mkFileAt(t, f.rootID, "doc.txt", data)
	// 先占掉 "doc.txt (副本)"
	if _, err := f.pool.Exec(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1,$2,$3,$4,false,1,1)`, f.spaceID, f.rootID, f.userID, "doc.txt (副本)"); err != nil {
		t.Fatalf("占名失败: %v", err)
	}

	res, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: id})
	if err != nil {
		t.Fatalf("复制应自动改名而不是报冲突: %v", err)
	}
	if res.Root.Name != "doc.txt (副本 2)" {
		t.Fatalf("应自动跳到 (副本 2),实际 %s", res.Root.Name)
	}
}

// **显式**指定新名时,同名必须报冲突(6.7:绝不静默改名)
func TestCopyExplicitNameConflictRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	data := []byte("explicit-name-" + suffix)
	id, hash := f.mkFileAt(t, f.rootID, "src.txt", data)
	f.mkFileAt(t, f.rootID, "taken.txt", []byte("other-"+suffix))

	_, err := f.svc.Copy(ctx, filesvc.CopyInput{
		UserID: f.userID, FileID: id, NewName: "taken.txt",
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Code != apierr.CodeNameConflict {
		t.Fatalf("显式重名应 409 name_conflict,实际 %d/%s", ae.Status, ae.Code)
	}
	if ref, _ := f.objRef(t, hash); ref != 1 {
		t.Fatalf("冲突时不应加引用,实际 %d", ref)
	}
}

// 复制到另一个目录,且新名生效;子树的 depth 与父子关系必须重建正确
func TestCopyIntoOtherDirRebuildsTree(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top, _ := f.mkReusableTree(t, "copytree")
	dest := f.mkDirAt(t, f.rootID, "dest", 1)

	res, err := f.svc.Copy(ctx, filesvc.CopyInput{
		UserID: f.userID, FileID: top, TargetParentID: dest, NewName: "top-copy",
	})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	// 深度:dest(1) → top-copy(2) → sub(3) → 文件(4)
	if res.Root.Depth != 2 {
		t.Fatalf("根副本深度应为 2,实际 %d", res.Root.Depth)
	}
	var maxDepth int
	if err := f.pool.QueryRow(ctx, `
WITH RECURSIVE sub AS (
    SELECT id, depth FROM files WHERE id = $1
    UNION ALL SELECT f.id, f.depth FROM files f JOIN sub ON f.parent_id = sub.id
) SELECT coalesce(max(depth),0) FROM sub`, res.Root.ID).Scan(&maxDepth); err != nil {
		t.Fatalf("算副本深度失败: %v", err)
	}
	if maxDepth != 4 {
		t.Fatalf("副本最深层应为 4,实际 %d", maxDepth)
	}
	// 父子关系:副本里的每个目录都要能找到原来的子项
	var subName string
	if err := f.pool.QueryRow(ctx,
		`SELECT name FROM files WHERE parent_id = $1 AND name = 'sub'`, res.Root.ID).Scan(&subName); err != nil {
		t.Fatalf("副本内应含 sub 目录: %v", err)
	}
	// 源树未被改动(top 下直接子项是 a.txt 与 sub 两个)
	if rows := f.countChildRows(t, top); rows != 2 {
		t.Fatalf("源树应保持 2 个子项,实际 %d", rows)
	}
}

// 复制空间根目录 → 400(语义上是"复制整个空间",不在本任务范围)
func TestCopyRootRejected(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Copy(context.Background(), filesvc.CopyInput{
		UserID: f.userID, FileID: f.rootID,
	})
	if asAPIErr(t, err).Status != 400 {
		t.Fatalf("复制根目录应 400,实际 %v", err)
	}
}

// 只读成员不能复制(写操作)
func TestCopyRequiresWritePermission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := setup(t)
	teamID, teamRoot := f.mkTeamSpace(t, owner.userID, "只读复制空间")
	if err := (repo.SpaceRepo{}).AddMember(ctx, f.pool, teamID, f.userID, model.PermReader); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	var id string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1,$2,$3,'ro.txt',false,1,1) RETURNING id`, teamID, teamRoot, owner.userID).Scan(&id); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	if _, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: id}); err == nil {
		t.Fatal("只读成员复制应被拒")
	}
}

// 复制一个内容重复的树后,再删掉副本:引用必须精确回落到原值(不早删不少删)
func TestCopyThenDeleteRestoresRefCount(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top, hashes := f.mkReusableTree(t, "copydel")
	shared := hashes["shared"]
	before, _ := f.objRef(t, shared)

	res, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: top})
	if err != nil {
		t.Fatalf("复制失败: %v", err)
	}
	if mid, _ := f.objRef(t, shared); mid != before*2 {
		t.Fatalf("复制后引用应为 %d,实际 %d", before*2, mid)
	}
	if _, err := f.svc.Delete(ctx, f.userID, res.Root.ID); err != nil {
		t.Fatalf("删副本失败: %v", err)
	}
	if ref, state := f.objRef(t, shared); ref != before || state != model.ObjectLive {
		t.Fatalf("删副本后引用应回到 %d/live,实际 %d/%s", before, ref, state)
	}
}

// 对象物理文件缺失时复制必须失败(引用 +1 的前提是对象可用)
func TestCopyRejectsMissingObject(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top, hashes := f.mkReusableTree(t, "copymiss")
	f.setObjectState(t, hashes["unique"], model.ObjectDeleted, 0)

	_, err := f.svc.Copy(ctx, filesvc.CopyInput{UserID: f.userID, FileID: top})
	if err == nil {
		t.Fatal("对象不可用时复制必须失败")
	}
	// 整体回滚:shared 的引用不能被留下
	if ref, _ := f.objRef(t, hashes["shared"]); ref != 2 {
		t.Fatalf("失败后 shared 引用应回滚为 2,实际 %d", ref)
	}
}

// `released_objects` 必须表示"**回收的物理对象数**",而不是"被递减的引用行数"。
//
// 这条语义由端到端探针发现过:共享/复制出多行后删除其中一行,
// 旧实现报 released_objects=1(其实对象仍被别处引用,一个也没回收),
// 客户端据此算"释放了多少磁盘"会算错。
func TestDeleteReportsOnlyObjectsThatHitZeroRef(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	data := []byte("released-semantics-" + suffix)
	first, hash := f.mkFileAt(t, f.rootID, "first.txt", data)
	// 同一内容再来一行(模拟共享/复制后的状态),引用 1→2
	if _, err := f.pool.Exec(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, hash_sha256, depth)
VALUES ($1,$2,$3,'second.txt',false,$4,$5,1)`,
		f.spaceID, f.rootID, f.userID, len(data), hash); err != nil {
		t.Fatalf("造第二行失败: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE file_objects SET ref_count = ref_count + 1 WHERE hash_sha256 = $1`, hash); err != nil {
		t.Fatalf("加引用失败: %v", err)
	}
	var second string
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM files WHERE space_id=$1 AND name='second.txt'`, f.spaceID).Scan(&second); err != nil {
		t.Fatalf("取第二行 id 失败: %v", err)
	}

	// 删第一行:引用 2→1,对象**没有**回收
	res1, err := f.svc.Delete(ctx, f.userID, first)
	if err != nil {
		t.Fatalf("删第一行失败: %v", err)
	}
	if res1.DistinctObjects != 1 {
		t.Fatalf("涉及的不同对象应为 1,实际 %d", res1.DistinctObjects)
	}
	if res1.ReleasedObjects != 0 {
		t.Fatalf("对象仍被第二行引用时 released_objects 应为 0,实际 %d", res1.ReleasedObjects)
	}
	if ref, state := f.objRef(t, hash); ref != 1 || state != model.ObjectLive {
		t.Fatalf("应为 ref=1/live,实际 %d/%s", ref, state)
	}

	// 删第二行:引用 1→0,这次才真的回收
	res2, err := f.svc.Delete(ctx, f.userID, second)
	if err != nil {
		t.Fatalf("删第二行失败: %v", err)
	}
	if res2.ReleasedObjects != 1 {
		t.Fatalf("最后一个引用消失时 released_objects 应为 1,实际 %d", res2.ReleasedObjects)
	}
	if ref, state := f.objRef(t, hash); ref != 0 || state != model.ObjectPendingDelete {
		t.Fatalf("应为 ref=0/pending_delete,实际 %d/%s", ref, state)
	}
}
