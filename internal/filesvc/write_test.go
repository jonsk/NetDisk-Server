package filesvc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 文件写操作:改名 / 移动 / 删除 / 影响面预检(BE-S6-01 / BE-S6-02 / BE-S7-01)。

// mkFileAt 在指定父目录下造一个带真实对象的文件(对象行 + files 行)。
func (f *fixture) mkFileAt(t *testing.T, parent, name string, data []byte) (fileID, hash string) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:])
	// 造物理对象与对象行
	if _, err := f.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写对象失败: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE SET ref_count = file_objects.ref_count + 1`,
		hash, len(data), f.store.KeyFor(hash)); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, hash_sha256, depth)
VALUES ($1, $2, $3, $4, false, $5, $6, 1) RETURNING id`,
		f.spaceID, parent, f.userID, name, len(data), hash).Scan(&fileID); err != nil {
		t.Fatalf("造文件行失败: %v", err)
	}
	f.trackHash(hash)
	return fileID, hash
}

func (f *fixture) mkDirAt(t *testing.T, parent, name string, depth int) string {
	t.Helper()
	d, err := repo.FileRepo{}.CreateDir(context.Background(), f.pool, repo.CreateDirInput{
		SpaceID: f.spaceID, ParentID: parent, OwnerID: f.userID, Name: name, Depth: depth,
	})
	if err != nil {
		t.Fatalf("造目录 %s 失败: %v", name, err)
	}
	return d.ID
}

func (f *fixture) fileRow(t *testing.T, id string) (name, parentID string, version int64, depth int) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT name, coalesce(parent_id::text,''), version, depth FROM files WHERE id=$1`, id).
		Scan(&name, &parentID, &version, &depth); err != nil {
		t.Fatalf("读文件行失败: %v", err)
	}
	return
}

func (f *fixture) objRef(t *testing.T, hash string) (ref int, state string) {
	t.Helper()
	if err := f.pool.QueryRow(context.Background(),
		`SELECT ref_count, state FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&ref, &state); err != nil {
		t.Fatalf("读对象行失败: %v", err)
	}
	return
}

func asAPIErr(t *testing.T, err error) *apierr.Error {
	t.Helper()
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("应返回 *apierr.Error,实际 %v", err)
	}
	return ae
}

// ---- 改名 ----

func TestRenameUpdatesVersionAndETag(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.mkFileAt(t, f.rootID, "old.txt", []byte("rename-me"))
	_, _, v0, _ := f.fileRow(t, id)

	v, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id, NewName: "new.txt", BaseVersion: v0,
	})
	if err != nil {
		t.Fatalf("改名失败: %v", err)
	}
	if v.Name != "new.txt" {
		t.Fatalf("名字应为 new.txt,实际 %s", v.Name)
	}
	if v.Version != v0+1 {
		t.Fatalf("改名后 version 应 +1(%d),实际 %d", v0+1, v.Version)
	}
	// etag 由生成列派生,版本变了 etag 必须变;形状为 "00000002-xxxxxxxx"
	wantPrefix := fmt.Sprintf("\"%08x-", v.Version)
	if !strings.HasPrefix(v.ETag, wantPrefix) {
		t.Fatalf("etag 应反映新版本(前缀 %q),实际 %q", wantPrefix, v.ETag)
	}
}

// 同名(仅大小写不同)改名是"修正文件名"的常见操作,应当幂等通过
func TestRenameCaseOnlySucceeds(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.mkFileAt(t, f.rootID, "readme.md", []byte("x"))
	v, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id, NewName: "README.md",
	})
	if err != nil {
		t.Fatalf("仅大小写改名应成功: %v", err)
	}
	if v.Name != "README.md" {
		t.Fatalf("名字应为 README.md,实际 %s", v.Name)
	}
}

// **验收①**:base_version 不匹配 → 409 且带全 `{server_version, server_etag, server_updated_at, reason}`
func TestRenameVersionConflictCarriesServerState(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.mkFileAt(t, f.rootID, "conflict.txt", []byte("v"))
	// 先改一次,让服务端版本前进到 2
	if _, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id, NewName: "conflict-v2.txt", BaseVersion: 1,
	}); err != nil {
		t.Fatalf("首次改名失败: %v", err)
	}
	// 用**过期版本 1** 再改 → 409(服务端已是 2)
	_, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id, NewName: "another.txt", BaseVersion: 1,
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 {
		t.Fatalf("版本不匹配应 409,实际 %d(%s)", ae.Status, ae.Code)
	}
	if ae.Code != apierr.CodeVersionConflict {
		t.Fatalf("业务码应为 version_conflict,实际 %s", ae.Code)
	}
	for _, k := range []string{"reason", "server_version", "server_etag", "server_updated_at"} {
		if ae.Details[k] == nil {
			t.Errorf("409 响应必须带 %q(客户端据此免二次请求展示冲突): %+v", k, ae.Details)
		}
	}
	if ae.Details["reason"] != filesvc.ReasonVersionConflict {
		t.Fatalf("reason 应为 %s,实际 %v", filesvc.ReasonVersionConflict, ae.Details["reason"])
	}
	if sv, _ := ae.Details["server_version"].(int64); sv != 2 {
		t.Fatalf("server_version 应为 2,实际 %v", ae.Details["server_version"])
	}
}

// **验收②**:同目录重名 → 409 且 reason 可区分(客户端提示"换名字"而非"刷新")
func TestRenameNameConflictReasonDistinct(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.mkFileAt(t, f.rootID, "taken.txt", []byte("a"))
	id2, _ := f.mkFileAt(t, f.rootID, "mine.txt", []byte("b"))

	_, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id2, NewName: "taken.txt",
	})
	if err == nil {
		t.Fatal("同目录重名应被拒")
	}
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("应返回 *apierr.Error,实际 %T: %v", err, err)
	}
	if ae.Status != 409 {
		t.Fatalf("同目录重名应 409,实际 %d(%s): %v", ae.Status, ae.Code, err)
	}
	if ae.Code != apierr.CodeNameConflict {
		t.Fatalf("重名的业务码应是 name_conflict(与 version_conflict 区分),实际 %s", ae.Code)
	}
	if ae.Details["reason"] != filesvc.ReasonNameConflict {
		t.Fatalf("reason 应为 %s,实际 %v", filesvc.ReasonNameConflict, ae.Details["reason"])
	}
}

// 不带 base_version(0)时不做乐观锁:允许,但会与并发修改竞速
func TestRenameWithoutBaseVersionAllowed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.mkFileAt(t, f.rootID, "nobase.txt", []byte("x"))
	if _, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id, NewName: "renamed.txt", BaseVersion: 0,
	}); err != nil {
		t.Fatalf("不带 base_version 应允许: %v", err)
	}
}

// 非法新名 → 400 且带规则明细(6.7),且**不改动**原文件
func TestRenameRejectsInvalidName(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.mkFileAt(t, f.rootID, "ok.txt", []byte("x"))
	_, err := f.svc.Rename(ctx, filesvc.RenameInput{
		UserID: f.userID, FileID: id, NewName: "bad:name.txt",
	})
	ae := asAPIErr(t, err)
	if ae.Code != apierr.CodeInvalidName {
		t.Fatalf("非法名应 invalid_name,实际 %s", ae.Code)
	}
	if ae.Details["rule"] == nil {
		t.Error("应带 rule 明细")
	}
	name, _, _, _ := f.fileRow(t, id)
	if name != "ok.txt" {
		t.Fatalf("失败不应改动原名,实际 %s", name)
	}
}

// 根目录禁改名(S3-03)
func TestRenameRootRejected(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Rename(context.Background(), filesvc.RenameInput{
		UserID: f.userID, FileID: f.rootID, NewName: "newname",
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Details["reason"] != filesvc.ReasonRootProtected {
		t.Fatalf("根目录改名应 409 reason=root_protected,实际 %d/%v", ae.Status, ae.Details)
	}
}

// ---- 移动 ----

func TestMoveChangesParentAndDepth(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dirA := f.mkDirAt(t, f.rootID, "A", 1)
	dirB := f.mkDirAt(t, f.rootID, "B", 1)
	id, _ := f.mkFileAt(t, dirA, "f.txt", []byte("x"))

	if _, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: id, NewParentID: dirB, BaseVersion: 1,
	}); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	_, parent, version, depth := f.fileRow(t, id)
	if parent != dirB {
		t.Fatalf("父目录应为 B,实际 %s", parent)
	}
	if version != 2 {
		t.Fatalf("移动后 version 应 +1,实际 %d", version)
	}
	if depth != 2 {
		t.Fatalf("深度应为 2(B 在 1 层),实际 %d", depth)
	}
}

// 移动目录:整棵子树的 depth 级联更新(一条递归 CTE)
func TestMoveDirCascadesDepth(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "top", 1)
	mid := f.mkDirAt(t, top, "mid", 2)
	leafDir := f.mkDirAt(t, mid, "leaf", 3)
	leafFile, _ := f.mkFileAt(t, leafDir, "deep.txt", []byte("x"))

	// 新建一个更深的容器,把 top 移进去
	deep := f.mkDirAt(t, f.rootID, "container", 1)
	inner := f.mkDirAt(t, deep, "inner", 2)

	if _, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: top, NewParentID: inner, BaseVersion: 1,
	}); err != nil {
		t.Fatalf("移动目录失败: %v", err)
	}

	// top 的新深度 = 3(inner 在 2 层)
	_, _, _, topDepth := f.fileRow(t, top)
	if topDepth != 3 {
		t.Fatalf("top 深度应为 3,实际 %d", topDepth)
	}
	_, _, _, midDepth := f.fileRow(t, mid)
	if midDepth != 4 {
		t.Fatalf("mid 深度应级联为 4,实际 %d", midDepth)
	}
	_, _, _, leafDirDepth := f.fileRow(t, leafDir)
	if leafDirDepth != 5 {
		t.Fatalf("leaf 目录深度应级联为 5,实际 %d", leafDirDepth)
	}
	_, _, _, leafFileDepth := f.fileRow(t, leafFile)
	if leafFileDepth != 6 {
		t.Fatalf("叶子文件深度应级联为 6,实际 %d", leafFileDepth)
	}
}

// **环检测**:移到自己或自己的后代下必须被拒
func TestMoveIntoOwnSubtreeRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "top", 1)
	mid := f.mkDirAt(t, top, "mid", 2)
	leaf := f.mkDirAt(t, mid, "leaf", 3)

	for _, target := range []struct {
		name string
		id   string
	}{{"自己", top}, {"直接子目录", mid}, {"深层后代", leaf}} {
		_, err := f.svc.Move(ctx, filesvc.MoveInput{
			UserID: f.userID, FileID: top, NewParentID: target.id, BaseVersion: 1,
		})
		ae := asAPIErr(t, err)
		if ae.Status != 400 {
			t.Errorf("移到%s应 400,实际 %d", target.name, ae.Status)
			continue
		}
	}
	// 树结构必须完好
	_, parent, _, _ := f.fileRow(t, mid)
	if parent != top {
		t.Fatalf("拒绝后 mid 的父应仍为 top,实际 %s", parent)
	}
}

// 跨空间移动被拒(空间即权限边界)
func TestMoveCrossSpaceRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id, _ := f.mkFileAt(t, f.rootID, "mine.txt", []byte("x"))

	other := setup(t)
	_, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: id, NewParentID: other.rootID, BaseVersion: 1,
	})
	ae := asAPIErr(t, err)
	if ae.Status != 403 {
		t.Fatalf("跨空间移动应 403,实际 %d(%s)", ae.Status, ae.Code)
	}
}

// 移动到"文件"而不是目录 → 400
func TestMoveToNonDirRejected(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a, _ := f.mkFileAt(t, f.rootID, "a.txt", []byte("a"))
	b, _ := f.mkFileAt(t, f.rootID, "b.txt", []byte("b"))

	_, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: a, NewParentID: b, BaseVersion: 1,
	})
	if asAPIErr(t, err).Status != 400 {
		t.Fatalf("移动到文件应 400,实际 %v", err)
	}
}

// 目标目录已有同名 → 409 reason=name_conflict
func TestMoveNameConflictInTarget(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dirA := f.mkDirAt(t, f.rootID, "A", 1)
	dirB := f.mkDirAt(t, f.rootID, "B", 1)
	f.mkFileAt(t, dirB, "same.txt", []byte("existing"))
	mine, _ := f.mkFileAt(t, dirA, "same.txt", []byte("mine"))

	_, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: mine, NewParentID: dirB, BaseVersion: 1,
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Details["reason"] != filesvc.ReasonNameConflict {
		t.Fatalf("目标同名应 409 reason=name_conflict,实际 %d/%v", ae.Status, ae.Details)
	}
}

// 移动可同时改名
func TestMoveWithRename(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dirA := f.mkDirAt(t, f.rootID, "A", 1)
	dirB := f.mkDirAt(t, f.rootID, "B", 1)
	id, _ := f.mkFileAt(t, dirA, "before.txt", []byte("x"))

	if _, err := f.svc.Move(ctx, filesvc.MoveInput{
		UserID: f.userID, FileID: id, NewParentID: dirB, NewName: "after.txt", BaseVersion: 1,
	}); err != nil {
		t.Fatalf("移动+改名失败: %v", err)
	}
	name, parent, _, _ := f.fileRow(t, id)
	if name != "after.txt" || parent != dirB {
		t.Fatalf("应为 B/after.txt,实际 %s/%s", parent, name)
	}
}

// 根目录禁移动
func TestMoveRootRejected(t *testing.T) {
	f := setup(t)
	dir := f.mkDirAt(t, f.rootID, "x", 1)
	_, err := f.svc.Move(context.Background(), filesvc.MoveInput{
		UserID: f.userID, FileID: f.rootID, NewParentID: dir,
	})
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Details["reason"] != filesvc.ReasonRootProtected {
		t.Fatalf("根目录移动应 409 reason=root_protected,实际 %d/%v", ae.Status, ae.Details)
	}
}

// ---- 删除(硬删 + 引用计数同事务)----

func TestDeleteReleasesObjectAndQuota(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("delete-me-payload")
	id, hash := f.mkFileAt(t, f.rootID, "del.txt", data)
	// 让配额反映该文件
	if _, err := f.pool.Exec(ctx,
		`UPDATE spaces SET used_bytes = $2 WHERE id = $1`, f.spaceID, len(data)); err != nil {
		t.Fatalf("设置配额失败: %v", err)
	}

	res, err := f.svc.Delete(ctx, f.userID, id)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if res.DeletedFiles != 1 {
		t.Fatalf("应删 1 个文件,实际 %d", res.DeletedFiles)
	}
	if res.FreedBytes != int64(len(data)) {
		t.Fatalf("释放字节应为 %d,实际 %d", len(data), res.FreedBytes)
	}
	if res.ReleasedObjects != 1 {
		t.Fatalf("应有 1 个对象引用归零,实际 %d", res.ReleasedObjects)
	}
	// files 行已硬删(无软删字段)
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE id=$1`, id).Scan(&n)
	if n != 0 {
		t.Fatal("删除后 files 行应消失(硬删,无回收站)")
	}
	// 对象转 pending_delete 并带未来 delete_after(延迟删除窗口)
	ref, state := f.objRef(t, hash)
	if ref != 0 || state != model.ObjectPendingDelete {
		t.Fatalf("对象应为 ref=0/pending_delete,实际 %d/%s", ref, state)
	}
	var after *time.Time
	_ = f.pool.QueryRow(ctx, `SELECT delete_after FROM file_objects WHERE hash_sha256=$1`, hash).Scan(&after)
	if after == nil || !after.After(time.Now()) {
		t.Fatalf("应设置未来的 delete_after,实际 %v", after)
	}
	// 物理对象**仍在**(延迟删除窗口内不删)—— 由生命周期 worker 负责
	if _, err := f.store.Stat(ctx, hash); err != nil {
		t.Fatalf("延迟窗口内物理对象应仍在: %v", err)
	}
	// 配额已释放
	var used int64
	_ = f.pool.QueryRow(ctx, `SELECT used_bytes FROM spaces WHERE id=$1`, f.spaceID).Scan(&used)
	if used != 0 {
		t.Fatalf("配额应释放为 0,实际 %d", used)
	}
}

// 共享同一对象的两个文件:删一个 → ref=1/live;两个都删 → ref=0/pending_delete
func TestDeleteSharedObjectRefCounting(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	data := []byte("shared-content")
	id1, hash := f.mkFileAt(t, f.rootID, "share1.txt", data)
	// 第二个文件引用同一 hash(模拟同内容被两行引用:秒传/共享的结果)
	if _, err := f.pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE SET ref_count = file_objects.ref_count + 1`,
		hash, len(data), f.store.KeyFor(hash)); err != nil {
		t.Fatalf("增加引用失败: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, hash_sha256, depth)
VALUES ($1, $2, $3, 'share2.txt', false, $4, $5, 1) RETURNING id`,
		f.spaceID, f.rootID, f.userID, len(data), hash).Scan(new(string)); err != nil {
		t.Fatalf("造第二个文件失败: %v", err)
	}
	var id2 string
	_ = f.pool.QueryRow(ctx, `SELECT id FROM files WHERE space_id=$1 AND name='share2.txt'`, f.spaceID).Scan(&id2)

	if _, err := f.svc.Delete(ctx, f.userID, id1); err != nil {
		t.Fatalf("删第一个失败: %v", err)
	}
	if ref, state := f.objRef(t, hash); ref != 1 || state != model.ObjectLive {
		t.Fatalf("删一个后应为 ref=1/live,实际 %d/%s", ref, state)
	}
	if _, err := f.svc.Delete(ctx, f.userID, id2); err != nil {
		t.Fatalf("删第二个失败: %v", err)
	}
	if ref, state := f.objRef(t, hash); ref != 0 || state != model.ObjectPendingDelete {
		t.Fatalf("都删后应为 ref=0/pending_delete,实际 %d/%s", ref, state)
	}
}

// 删目录 → 整棵子树硬删,每个对象引用各 -1
func TestDeleteSubtreeReleasesAllObjects(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "top", 1)
	sub := f.mkDirAt(t, top, "sub", 2)
	f1, h1 := f.mkFileAt(t, top, "a.txt", []byte("aaa"))
	_, h2 := f.mkFileAt(t, sub, "b.txt", []byte("bbb"))
	_, h3 := f.mkFileAt(t, sub, "c.txt", []byte("ccc"))
	_ = f1

	res, err := f.svc.Delete(ctx, f.userID, top)
	if err != nil {
		t.Fatalf("删目录失败: %v", err)
	}
	if res.DeletedFiles != 3 {
		t.Fatalf("应删 3 个文件,实际 %d", res.DeletedFiles)
	}
	if res.DeletedDirs != 2 {
		t.Fatalf("应删 2 个目录,实际 %d", res.DeletedDirs)
	}
	if res.ReleasedObjects != 3 {
		t.Fatalf("应有 3 个对象引用归零,实际 %d", res.ReleasedObjects)
	}
	for _, h := range []string{h1, h2, h3} {
		if ref, state := f.objRef(t, h); ref != 0 || state != model.ObjectPendingDelete {
			t.Fatalf("对象 %s 应为 ref=0/pending_delete,实际 %d/%s", h[:8], ref, state)
		}
	}
	// 子树内所有 files 行都消失
	var n int
	_ = f.pool.QueryRow(ctx, `
WITH RECURSIVE sub AS (
  SELECT id FROM files WHERE id=$1
  UNION ALL SELECT f.id FROM files f JOIN sub ON f.parent_id=sub.id
) SELECT count(*) FROM sub`, top).Scan(&n)
	if n != 0 {
		t.Fatalf("子树行应全部删除,剩余 %d", n)
	}
}

// 根目录禁删
func TestDeleteRootRejected(t *testing.T) {
	f := setup(t)
	_, err := f.svc.Delete(context.Background(), f.userID, f.rootID)
	ae := asAPIErr(t, err)
	if ae.Status != 409 || ae.Details["reason"] != filesvc.ReasonRootProtected {
		t.Fatalf("根目录删除应 409 reason=root_protected,实际 %d/%v", ae.Status, ae.Details)
	}
}

// 只读成员不能写(rename/move/delete 三条路径都要拦)
func TestWriteOpsRequireWritePermission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	owner := setup(t)
	teamID, teamRoot := f.mkTeamSpace(t, owner.userID, "只读校验空间")
	if err := (repo.SpaceRepo{}).AddMember(ctx, f.pool, teamID, f.userID, model.PermReader); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	var id string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1,$2,$3,'ro.txt',false,1,1) RETURNING id`, teamID, teamRoot, owner.userID).Scan(&id); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}

	if _, err := f.svc.Rename(ctx, filesvc.RenameInput{UserID: f.userID, FileID: id, NewName: "x.txt"}); err == nil {
		t.Error("只读成员改名应被拒")
	}
	if _, err := f.svc.Move(ctx, filesvc.MoveInput{UserID: f.userID, FileID: id, NewParentID: teamRoot}); err == nil {
		t.Error("只读成员移动应被拒")
	}
	if _, err := f.svc.Delete(ctx, f.userID, id); err == nil {
		t.Error("只读成员删除应被拒")
	}
}

// ---- 影响面预检(BE-S7-01)----

// 预检的数字必须与删除的实际结果**一致**(两者共用同一份统计实现)
func TestSubtreeStatsMatchesActualDelete(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	top := f.mkDirAt(t, f.rootID, "top", 1)
	sub := f.mkDirAt(t, top, "sub", 2)
	// 两个文件内容相同(去重),一个不同
	same := []byte("identical-content")
	f.mkFileAt(t, top, "s1.txt", same)
	f.mkFileAt(t, sub, "s2.txt", same)
	f.mkFileAt(t, sub, "other.txt", []byte("different"))

	stats, err := f.svc.SubtreeStats(ctx, f.userID, top)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if stats.FileCount != 3 {
		t.Fatalf("文件数应为 3,实际 %d", stats.FileCount)
	}
	if stats.DirCount != 2 {
		t.Fatalf("目录数应为 2,实际 %d", stats.DirCount)
	}
	wantTotal := int64(len(same)*2 + len("different"))
	if stats.TotalSize != wantTotal {
		t.Fatalf("总字节应为 %d,实际 %d", wantTotal, stats.TotalSize)
	}
	// 去重后只有 2 个不同对象
	if stats.ObjectRefs != 2 {
		t.Fatalf("去重后对象数应为 2,实际 %d", stats.ObjectRefs)
	}
	// 节省 = 总字节 - 去重后物理占用 = len(same)
	if stats.DedupSavedBytes != int64(len(same)) {
		t.Fatalf("去重节省应为 %d,实际 %d", len(same), stats.DedupSavedBytes)
	}

	// 实际删除后数字必须一致
	del, err := f.svc.Delete(ctx, f.userID, top)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if del.DeletedFiles != stats.FileCount {
		t.Fatalf("预检文件数(%d)与实际删除(%d)不一致", stats.FileCount, del.DeletedFiles)
	}
	if del.DeletedDirs != stats.DirCount {
		t.Fatalf("预检目录数(%d)与实际删除(%d)不一致", stats.DirCount, del.DeletedDirs)
	}
	if del.FreedBytes != stats.TotalSize {
		t.Fatalf("预检字节(%d)与实际释放(%d)不一致", stats.TotalSize, del.FreedBytes)
	}
	if del.DistinctObjects != stats.ObjectRefs {
		t.Fatalf("预检对象数(%d)与实际涉及的不同对象数(%d)不一致", stats.ObjectRefs, del.DistinctObjects)
	}
}

func TestSubtreeStatsForSingleFile(t *testing.T) {
	f := setup(t)
	data := []byte("single")
	id, _ := f.mkFileAt(t, f.rootID, "one.txt", data)
	stats, err := f.svc.SubtreeStats(context.Background(), f.userID, id)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if stats.FileCount != 1 || stats.DirCount != 0 {
		t.Fatalf("单文件应为 1 文件 0 目录,实际 %d/%d", stats.FileCount, stats.DirCount)
	}
	if stats.TotalSize != int64(len(data)) {
		t.Fatalf("字节应为 %d,实际 %d", len(data), stats.TotalSize)
	}
}
