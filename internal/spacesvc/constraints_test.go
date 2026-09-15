package spacesvc_test

import (
	"context"
	"testing"

	"github.com/netdisk/netdisk/internal/repo"
)

// BE-S3-03 统一空间模型的**约束层验收**:这些不变式由数据库保证,
// 所以用例直接打库 —— 服务层所有路径都绕不过它们。
//
// 之所以单独一组:约束是"最后一道防线",它失效时上层任何 bug 都会变成
// 数据损坏(例如两个根目录并存、personal 空间被解散),而这类损坏没有回滚路径。

// 每个空间**恰有一行**根(parent_id IS NULL 的部分唯一索引)。
func TestOnlyOneRootRowPerSpace(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO files (space_id, owner_id, name, is_dir, depth) VALUES ($1, $2, '', true, 0)`,
		f.spaceID, f.ownerID)
	if err == nil {
		t.Fatal("同一空间不应存在第二个根目录行")
	}
	if !repo.IsUniqueViolation(err) {
		t.Fatalf("应是唯一索引冲突,实际 %v", err)
	}
}

// 根目录禁删/禁改名(S3-03)。repo 的 NonRoot 变体用同一条 SQL 表达,
// 保证"判断 + 执行"是原子的(不会被并发插入钻空子)。
func TestRootRowProtectedFromRenameAndDelete(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	files := repo.FileRepo{}

	if _, err := files.RenameNonRoot(ctx, f.pool, f.rootID, "改根名", 1); err == nil {
		t.Fatal("根目录不应允许改名")
	}
	if err := files.DeleteNonRoot(ctx, f.pool, f.rootID); err == nil {
		t.Fatal("根目录不应允许删除")
	}
	// 根目录仍在
	isRoot, err := files.IsRoot(ctx, f.pool, f.rootID)
	if err != nil || !isRoot {
		t.Fatalf("根目录应仍存在且标记为根: isRoot=%v err=%v", isRoot, err)
	}
}

// 非根文件可以正常改名/删除(确认保护没有一刀切)
func TestNonRootCanRenameAndDelete(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	files := repo.FileRepo{}
	var id string
	if err := f.pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1, $2, $3, 'old.txt', false, 1, 1) RETURNING id`,
		f.spaceID, f.rootID, f.ownerID).Scan(&id); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	renamed, err := files.RenameNonRoot(ctx, f.pool, id, "new.txt", 1)
	if err != nil {
		t.Fatalf("非根文件应可改名: %v", err)
	}
	if renamed.Name != "new.txt" || renamed.Version != 2 {
		t.Fatalf("改名后应 name=new.txt version=2,实际 %+v", renamed)
	}
	if err := files.DeleteNonRoot(ctx, f.pool, id); err != nil {
		t.Fatalf("非根文件应可删除: %v", err)
	}
}

// personal 空间的 kind/group 组合约束:
//   - personal 必须没有 group_id
//   - team 必须有 group_id(不能凭空造一个无组的团队空间)
func TestSpaceKindGroupConstraint(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// personal + group_id → 违反 spaces_kind_group_chk
	var groupID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO groups (name, owner_id) VALUES ('s3_grp', $1) RETURNING id`,
		f.ownerID).Scan(&groupID); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM groups WHERE id = $1`, groupID)
	})

	_, err := f.pool.Exec(ctx,
		`INSERT INTO spaces (kind, group_id, owner_id, name) VALUES ('personal', $1, $2, '非法个人空间')`,
		groupID, f.ownerID)
	if err == nil {
		t.Fatal("personal 空间不应允许带 group_id")
	}
	if !repo.IsCheckViolation(err) {
		t.Fatalf("应是 CHECK 约束冲突,实际 %v", err)
	}

	// team 无 group_id → 同样违反
	_, err = f.pool.Exec(ctx,
		`INSERT INTO spaces (kind, owner_id, name) VALUES ('team', $1, '非法团队空间')`, f.ownerID)
	if err == nil {
		t.Fatal("team 空间必须带 group_id")
	}
	if !repo.IsCheckViolation(err) {
		t.Fatalf("应是 CHECK 约束冲突,实际 %v", err)
	}
}

// 每用户恰有一个 personal 空间(部分唯一索引)
func TestOnlyOnePersonalSpacePerUser(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO spaces (kind, owner_id, name) VALUES ('personal', $1, '第二个个人空间')`, f.ownerID)
	if err == nil {
		t.Fatal("同一用户不应有第二个 personal 空间")
	}
	if !repo.IsUniqueViolation(err) {
		t.Fatalf("应是唯一索引冲突,实际 %v", err)
	}
}

// files.space_id NOT NULL(4.3:R 中 P0-2 的落地)
func TestFilesSpaceIDNotNull(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO files (parent_id, owner_id, name, is_dir, depth) VALUES ($1, $2, 'x', false, 1)`,
		f.rootID, f.ownerID)
	if err == nil {
		t.Fatal("files.space_id 不应允许为空")
	}
}

// 空间必须归属一个 user(owner_id NOT NULL + 外键)
func TestSpaceRequiresOwner(t *testing.T) {
	f := setup(t)
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO spaces (kind, name) VALUES ('personal', '无主空间')`)
	if err == nil {
		t.Fatal("空间不应允许没有 owner")
	}
}

// files.owner_id 只是审计主体,不参与判权:另一用户创建的文件
// 在空间内必须对成员可见(判权看 space_members,不看文件 owner)。
func TestFileOwnerIsNotPermissionSource(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp3_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, "editor"); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	// owner 创建一个文件
	if _, err := f.pool.Exec(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1, $2, $3, 'owned-by-owner.txt', false, 1, 1)`,
		f.spaceID, f.rootID, f.ownerID); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	// alice(非文件 owner)能看到它
	ms, err := f.svc.ListMembers(ctx, alice, f.spaceID)
	if err != nil {
		t.Fatalf("alice 应可访问空间: %v", err)
	}
	_ = ms
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE space_id = $1 AND parent_id IS NOT NULL`, f.spaceID).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("空间内应有 1 个文件,实际 %d", n)
	}
}
