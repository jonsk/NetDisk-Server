package spacesvc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/spacesvc"
)

// 解散空间时必须清理"指向这个空间的上传任务行"。
//
// 实测缺陷:`uploads.space_id` / `uploads.parent_id` 都是 `ON DELETE RESTRICT`,
// 而任务行从不清扫(定稿/取消只改 state),于是
//
//	只要这个空间曾经有人建过上传任务(哪怕最后没传完),
//	解散就返回 500 internal_error,空间**永远删不掉**。
//
// 这组测试钉住:解散成功 + 预留额度的账要平 + 任务行随之消失。

func mkUploadTaskInSpace(t *testing.T, f *fixture, parent, name string, size int64, state string) string {
	t.Helper()
	var parentArg any
	if parent != "" {
		parentArg = parent
	}
	var id string
	err := f.pool.QueryRow(context.Background(), `
INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, state, ticket_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, 'test-ticket-hash', now() + interval '1 hour')
RETURNING id`, f.ownerID, f.spaceID, parentArg, name, size, state).Scan(&id)
	if err != nil {
		t.Fatalf("造上传任务失败: %v", err)
	}
	return id
}

func TestDissolveWithPendingUploadTask(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// 根目录上挂一个未定稿任务:它既挡住 root 的删除,也挡住 spaces 的删除
	taskID := mkUploadTaskInSpace(t, f, f.rootID, "pending.bin", 4096, "reserved")

	if _, err := (repo.SpaceRepo{}).Reserve(ctx, f.pool, f.spaceID, 4096); err != nil {
		t.Fatalf("预留额度失败: %v", err)
	}

	if err := f.svc.DissolveSpace(ctx, f.ownerID, f.spaceID); err != nil {
		t.Fatalf("有未完成任务时解散应当成功,实际失败: %v", err)
	}

	// 空间行必须真的没了(而不是"返回成功但行还在")
	if _, err := (repo.SpaceRepo{}).GetByID(ctx, f.pool, f.spaceID); !errors.Is(err, repo.ErrNotFound) {
		t.Fatalf("解散后空间应当不存在,实际 err=%v", err)
	}
	var tasks int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE id = $1`, taskID).Scan(&tasks); err != nil {
		t.Fatalf("数任务行失败: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("解散后不应残留该空间的上传任务,实际 %d 条", tasks)
	}
	var files int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE space_id = $1`, f.spaceID).Scan(&files); err != nil {
		t.Fatalf("数文件行失败: %v", err)
	}
	if files != 0 {
		t.Fatalf("解散后空间内不应残留文件行(含根目录),实际 %d 条", files)
	}
}

// 已定稿的任务行同样会挡住删除(它们既不在 reserved 也不影响额度)。
func TestDissolveWithFinalizedUploadTask(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mkUploadTaskInSpace(t, f, f.rootID, "done.bin", 1024, "finalized")

	if err := f.svc.DissolveSpace(ctx, f.ownerID, f.spaceID); err != nil {
		t.Fatalf("有已定稿任务时解散应当成功,实际失败: %v", err)
	}
	var tasks int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE space_id = $1`, f.spaceID).Scan(&tasks); err != nil {
		t.Fatalf("数任务行失败: %v", err)
	}
	if tasks != 0 {
		t.Fatalf("解散后该空间的任务行应当清空,实际 %d 条", tasks)
	}
}

// 清理必须**限定在本空间**:别的空间正在上传的任务不能被顺手删掉。
func TestDissolveKeepsOtherSpacesUploadTasks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	otherSpace, otherRoot := f.mkTeamSpace(t, f.ownerID, "另一个团队空间")
	mkUploadTaskInSpace(t, f, f.rootID, "mine.bin", 64, "reserved")

	// 另一个空间的任务(直接插,复用它自己的 space/root)
	var keepID string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO uploads (user_id, space_id, parent_id, name, declared_size, state, ticket_hash, expires_at)
VALUES ($1, $2, $3, 'keep.bin', 128, 'reserved', 'h', now() + interval '1 hour')
RETURNING id`, f.ownerID, otherSpace, otherRoot).Scan(&keepID); err != nil {
		t.Fatalf("造另一个空间的任务失败: %v", err)
	}

	if err := f.svc.DissolveSpace(ctx, f.ownerID, f.spaceID); err != nil {
		t.Fatalf("解散失败: %v", err)
	}
	var kept int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE id = $1`, keepID).Scan(&kept); err != nil {
		t.Fatalf("数任务行失败: %v", err)
	}
	if kept != 1 {
		t.Fatalf("另一个空间的任务必须保留,实际剩 %d 条", kept)
	}
}

// 非 owner 仍不能靠"清理任务行"绕过权限:解散的判权必须先生效。
func TestDissolveStillRequiresOwnerWithTasks(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mkUploadTaskInSpace(t, f, f.rootID, "x.bin", 16, "reserved")
	stranger := f.newUser(t, "sp_stranger_")

	err := f.svc.DissolveSpace(ctx, stranger, f.spaceID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeForbidden {
		t.Fatalf("非 owner 解散应 403,实际 %v", err)
	}
	// 失败必须整体回滚:任务行还在,空间还在
	var tasks int64
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM uploads WHERE space_id = $1`, f.spaceID).Scan(&tasks); err != nil {
		t.Fatalf("数任务行失败: %v", err)
	}
	if tasks != 1 {
		t.Fatalf("被拒的解散不能改动数据,任务行应仍为 1,实际 %d", tasks)
	}
	if _, err := (repo.SpaceRepo{}).GetByID(ctx, f.pool, f.spaceID); err != nil {
		t.Fatalf("被拒的解散不应影响空间,实际 %v", err)
	}
}

var _ = spacesvc.CanWrite
