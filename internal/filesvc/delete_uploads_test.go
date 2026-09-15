package filesvc_test

import (
	"context"
	"testing"

	"github.com/netdisk/netdisk/internal/repo"
)

// 删除"曾经有人上传过"的目录/子树(BE-S4-06 / 6.11 的删除路径)。
//
// 背景(实测踩到的真实缺陷):`uploads.parent_id` 是 `ON DELETE RESTRICT`,而
// 上传任务行**从不清扫**(定稿/取消只改 state),于是
//
//	任何曾经有人往里传过文件的目录都删不掉 —— DELETE FROM files 撞外键,
//	接口回 500 internal_error(不是 409、不是任何可读的提示)。
//
// 这组测试同时钉住两件事:
//  1. 删除必须成功(任务行要随子树一起清理);
//  2. **预留额度必须退回**:任务行里的 reserved 行占着 spaces.used_bytes,
//     直接删行会让那部分容量永久泄漏(所以必须先回退再删行)。

// mkUploadTask 造一条上传任务行(真实字段约束:name/declared_size/ticket_hash/expires_at)。
func (f *fixture) mkUploadTask(t *testing.T, parent, name string, size int64, state string) string {
	t.Helper()
	var id string
	err := f.pool.QueryRow(context.Background(), `
INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, state, ticket_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, 'test-ticket-hash', now() + interval '1 hour')
RETURNING id`,
		f.userID, f.spaceID, nullableParent(parent), name, size, state).Scan(&id)
	if err != nil {
		t.Fatalf("造上传任务失败: %v", err)
	}
	return id
}

func nullableParent(parent string) any {
	if parent == "" {
		return nil
	}
	return parent
}

func TestDeleteDirPurgesUploadTasksAndRefundsReserve(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	dirID := f.mkDir(t, f.rootID, "曾经传过文件的目录")
	// 任务1:未定稿(reserved)—— 占着 3000 字节预留
	_ = f.mkUploadTask(t, dirID, "pending.bin", 3000, "reserved")
	// 任务2:已定稿(finalized)—— 不占预留,但同样会挡住删除
	_ = f.mkUploadTask(t, dirID, "done.bin", 5000, "finalized")

	// 预留要真的记到账上,否则"退回"这件事测不出来
	before, err := (repo.SpaceRepo{}).GetByID(ctx, f.pool, f.spaceID)
	if err != nil {
		t.Fatalf("取空间失败: %v", err)
	}
	if _, err := (repo.SpaceRepo{}).Reserve(ctx, f.pool, f.spaceID, 3000); err != nil {
		t.Fatalf("预留额度失败: %v", err)
	}
	reserved, err := (repo.SpaceRepo{}).GetByID(ctx, f.pool, f.spaceID)
	if err != nil {
		t.Fatalf("取空间失败: %v", err)
	}
	if reserved.UsedBytes != before.UsedBytes+3000 {
		t.Fatalf("前置条件不成立:预留后 used_bytes 应为 %d,实际 %d", before.UsedBytes+3000, reserved.UsedBytes)
	}

	// 真正要验证的:删除必须成功(修复前这里会因 uploads_parent_id_fkey 失败)
	del, err := f.svc.Delete(ctx, f.userID, dirID)
	if err != nil {
		t.Fatalf("删有上传历史的目录应当成功,实际失败: %v", err)
	}
	if del.RefundedReservedBytes != 3000 {
		t.Fatalf("退回的预留额应为 3000,实际 %d", del.RefundedReservedBytes)
	}

	after, err := (repo.SpaceRepo{}).GetByID(ctx, f.pool, f.spaceID)
	if err != nil {
		t.Fatalf("取空间失败: %v", err)
	}
	if after.UsedBytes != before.UsedBytes {
		t.Fatalf("预留额度必须回到删除前的 %d,实际 %d(预留泄漏)", before.UsedBytes, after.UsedBytes)
	}

	// 任务行应当随目录一起消失(否则它们会一直指着已删目录)
	var left int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE parent_id = $1`, dirID).Scan(&left); err != nil {
		t.Fatalf("数任务行失败: %v", err)
	}
	if left != 0 {
		t.Fatalf("目录删除后不应残留指向它的上传任务,实际 %d 条", left)
	}
}

// 只清理"落点在这棵子树内"的任务:兄弟目录的任务必须原样保留。
//
// 没有这条断言,一个"WHERE space_id = $1"的过度清理实现也能让上面的测试通过 ——
// 而那会把用户**正在进行的上传**悄悄删掉。
func TestDeleteDirKeepsSiblingUploadTasks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	doomed := f.mkDir(t, f.rootID, "要删的目录")
	kept := f.mkDir(t, f.rootID, "要留的目录")
	doomedTask := f.mkUploadTask(t, doomed, "a.bin", 100, "reserved")
	keptTask := f.mkUploadTask(t, kept, "b.bin", 200, "reserved")

	if _, err := f.svc.Delete(ctx, f.userID, doomed); err != nil {
		t.Fatalf("删目录失败: %v", err)
	}

	var gone, kept2 int64
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE id = $1`, doomedTask).Scan(&gone)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE id = $1`, keptTask).Scan(&kept2)
	if gone != 0 {
		t.Errorf("被删目录的任务应被清理,实际还剩 %d 条", gone)
	}
	if kept2 != 1 {
		t.Errorf("兄弟目录的任务必须保留,实际剩 %d 条", kept2)
	}
}

// 子树删除要覆盖**所有层级**的落点(d1/d2 里的任务都要清掉)。
func TestDeleteSubtreePurgesNestedUploadTasks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	d1 := f.mkDir(t, f.rootID, "d1")
	d2 := f.mkDir(t, d1, "d2")
	_ = f.mkUploadTask(t, d1, "top.bin", 10, "reserved")
	_ = f.mkUploadTask(t, d2, "deep.bin", 20, "reserved")

	if _, err := f.svc.Delete(ctx, f.userID, d1); err != nil {
		t.Fatalf("删子树失败: %v", err)
	}
	var left int64
	if err := f.pool.QueryRow(ctx, `
SELECT count(*) FROM uploads WHERE parent_id = $1 OR parent_id = $2`, d1, d2).Scan(&left); err != nil {
		t.Fatalf("数任务行失败: %v", err)
	}
	if left != 0 {
		t.Fatalf("子树内所有层级的任务都应清理,实际剩 %d 条", left)
	}
}
