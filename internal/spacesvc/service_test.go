package spacesvc_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/spacesvc"
)

// 空间与团队成员的用例级验收(BE-S3-04/05/07),真实 PG。

type fixture struct {
	svc     *spacesvc.Service
	pool    *pgxpool.Pool
	ownerID string
	spaceID string
	rootID  string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	ownerID := newUser(t, database, "sp_owner_")
	f := &fixture{
		svc: &spacesvc.Service{
			Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Users: repo.UserRepo{},
			DB: db.AsQuerier(database),
		},
		pool: database.Pool, ownerID: ownerID,
	}
	f.spaceID, f.rootID = f.mkTeamSpace(t, ownerID, "团队空间")
	return f
}

func newUser(t *testing.T, database *db.DB, prefix string) string {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var id string
	err := database.InTx(context.Background(), func(tx pgx.Tx) error {
		u, err := repo.UserRepo{}.Create(context.Background(), tx, repo.CreateInput{
			Username: prefix + suffix, Email: prefix + suffix + "@example.com",
			DisplayName: prefix, Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		id = u.ID
		return nil
	})
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func (f *fixture) mkTeamSpace(t *testing.T, ownerID, name string) (spaceID, rootID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	var groupID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO groups (name, owner_id) VALUES ($1, $2) RETURNING id`,
		"sp_grp_"+suffix, ownerID).Scan(&groupID); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO spaces (kind, group_id, owner_id, name, quota_bytes) VALUES ('team', $1, $2, $3, 0) RETURNING id`,
		groupID, ownerID, name).Scan(&spaceID); err != nil {
		t.Fatalf("建团队空间失败: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO files (space_id, owner_id, name, is_dir, depth) VALUES ($1, $2, '', true, 0) RETURNING id`,
		spaceID, ownerID).Scan(&rootID); err != nil {
		t.Fatalf("建根目录失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.pool.Exec(bg, `DELETE FROM files WHERE space_id = $1`, spaceID)
		_, _ = f.pool.Exec(bg, `DELETE FROM spaces WHERE id = $1`, spaceID)
		_, _ = f.pool.Exec(bg, `DELETE FROM groups WHERE id = $1`, groupID)
	})
	return spaceID, rootID
}

// ---- 成员管理(S3-04)----

func TestAddMemberAndList(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	bob := f.newUser(t, "sp_bob_")

	// owner 加 alice 为 editor
	m, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermEditor)
	if err != nil {
		t.Fatalf("加成员失败: %v", err)
	}
	if m.UserID != alice || m.Permission != model.PermEditor {
		t.Fatalf("返回的成员不对: %+v", m)
	}
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, bob, model.PermReader); err != nil {
		t.Fatalf("加 bob 失败: %v", err)
	}

	ms, err := f.svc.ListMembers(ctx, f.ownerID, f.spaceID)
	if err != nil {
		t.Fatalf("列成员失败: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("应有 3 个成员(owner+alice+bob),实际 %d", len(ms))
	}
	var ownerSeen, aliceSeen bool
	for _, mm := range ms {
		if mm.UserID == f.ownerID {
			ownerSeen = true
			if !mm.IsOwner || mm.Permission != model.PermManager {
				t.Errorf("owner 应标记 IsOwner 且权限为 manager,实际 %+v", mm)
			}
		}
		if mm.UserID == alice && mm.Permission != model.PermEditor {
			t.Errorf("alice 权限应为 editor,实际 %s", mm.Permission)
		}
		if mm.UserID == alice {
			aliceSeen = true
		}
	}
	if !ownerSeen || !aliceSeen {
		t.Fatalf("成员列表缺人: owner=%v alice=%v", ownerSeen, aliceSeen)
	}
	// 重复添加是幂等的(改权限)
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermManager); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	var perm string
	_ = f.pool.QueryRow(ctx,
		`SELECT permission FROM space_members WHERE space_id=$1 AND user_id=$2`, f.spaceID, alice).Scan(&perm)
	if perm != model.PermManager {
		t.Fatalf("权限应已改为 manager,实际 %s", perm)
	}
	if n := f.countMembers(t); n != 2 {
		t.Fatalf("改权限不应新增成员行,期望 2 行(alice+bob),实际 %d 行", n)
	}
}

// editor 不能拉人/改权限(仅 manager)
func TestEditorCannotManageMembers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	carol := f.newUser(t, "sp_carol_")

	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermEditor); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	// alice(editor)尝试加 carol
	_, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, carol, model.PermReader)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("editor 拉人应 403,得到 %v", err)
	}
	// alice 尝试移除别人
	if err := f.svc.RemoveMember(ctx, alice, f.spaceID, f.ownerID); err == nil {
		t.Fatal("editor 移除成员应失败")
	}
}

// reader 也不能
func TestReaderCannotManageMembers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	bob := f.newUser(t, "sp_bob_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermReader); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	_, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, bob, model.PermReader)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("reader 拉人应 403,得到 %v", err)
	}
}

// 非成员尝试拉人 → 410 space_revoked
func TestNonMemberCannotManageMembers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	outsider := f.newUser(t, "sp_out_")
	victim := f.newUser(t, "sp_v_")
	_, err := f.svc.AddOrUpdateMember(ctx, outsider, f.spaceID, victim, model.PermManager)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("非成员应 space_revoked,得到 %v", err)
	}
}

// 不能改自己的权限(防自锁)
func TestCannotChangeOwnPermission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermManager); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	_, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, alice, model.PermReader)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("改自己权限应 403,得到 %v", err)
	}
}

// owner 不是普通成员,权限不可改
func TestCannotChangeOwnerPermission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermManager); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	_, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, f.ownerID, model.PermReader)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("改 owner 权限应 409,得到 %v", err)
	}
}

// 个人空间不支持成员管理
func TestPersonalSpaceHasNoMembers(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, f.pool, f.ownerID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	alice := f.newUser(t, "sp_alice_")
	_, err = f.svc.AddOrUpdateMember(ctx, f.ownerID, sp.ID, alice, model.PermReader)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("个人空间加成员应 403,得到 %v", err)
	}
}

// S3-05 核心验收:**踢人后下一个请求即被拒**(不等 JWT 过期,无缓存延迟)
func TestRemovalTakesEffectImmediately(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermEditor); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}

	// 移除前:可读
	if _, err := f.svc.ListMembers(ctx, alice, f.spaceID); err != nil {
		t.Fatalf("移除前 alice 应可读成员列表: %v", err)
	}

	if err := f.svc.RemoveMember(ctx, f.ownerID, f.spaceID, alice); err != nil {
		t.Fatalf("移除失败: %v", err)
	}

	// 移除后**立刻**再读 → 必须被拒
	_, err := f.svc.ListMembers(ctx, alice, f.spaceID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("移除后应立刻 space_revoked,得到 %v", err)
	}
}

// 降权也即时生效:manager → reader 后再拉人必须失败
func TestDowngradeTakesEffectImmediately(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	bob := f.newUser(t, "sp_bob_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermManager); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	// alice 作为 manager 可以拉人
	if _, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, bob, model.PermReader); err != nil {
		t.Fatalf("manager 应可拉人: %v", err)
	}
	// owner 降权 alice
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermReader); err != nil {
		t.Fatalf("降权失败: %v", err)
	}
	// 立刻再拉人 → 403
	carol := f.newUser(t, "sp_carol_")
	_, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, carol, model.PermReader)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("降权后拉人应立刻 403,得到 %v", err)
	}
}

// 移除不存在的成员 → 404
func TestRemoveNonMember(t *testing.T) {
	f := setup(t)
	outsider := f.newUser(t, "sp_out_")
	err := f.svc.RemoveMember(context.Background(), f.ownerID, f.spaceID, outsider)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("移除非成员应 404,得到 %v", err)
	}
}

// 不能移除 owner
func TestCannotRemoveOwner(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermManager); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	err := f.svc.RemoveMember(ctx, alice, f.spaceID, f.ownerID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("移除 owner 应 409,得到 %v", err)
	}
}

// ---- 退出 / 转让 / 解散 ----

// **owner 未转让前退出 → 409**(S3-04 验收项)
func TestOwnerCannotLeaveBeforeTransfer(t *testing.T) {
	f := setup(t)
	err := f.svc.Leave(context.Background(), f.ownerID, f.spaceID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("owner 退出应 409,得到 %v", err)
	}
}

// 普通成员可以退出
func TestMemberCanLeave(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermEditor); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	if err := f.svc.Leave(ctx, alice, f.spaceID); err != nil {
		t.Fatalf("普通成员应可退出: %v", err)
	}
	// 退出后立刻失去访问
	_, err := f.svc.ListMembers(ctx, alice, f.spaceID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("退出后应 space_revoked,得到 %v", err)
	}
	// 重复退出 → 404
	if err := f.svc.Leave(ctx, alice, f.spaceID); err == nil {
		t.Fatal("重复退出应报错")
	}
}

// 转让:仅 owner;转让后原 owner 不再是 owner 但仍可访问
func TestTransferOwnership(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermEditor); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	// 非 owner 转让 → 403
	if err := f.svc.Transfer(ctx, alice, f.spaceID, f.ownerID); err == nil {
		t.Fatal("非 owner 转让应失败")
	}
	// owner 转让给 alice
	if err := f.svc.Transfer(ctx, f.ownerID, f.spaceID, alice); err != nil {
		t.Fatalf("转让失败: %v", err)
	}
	var newOwner string
	_ = f.pool.QueryRow(ctx, `SELECT owner_id FROM spaces WHERE id=$1`, f.spaceID).Scan(&newOwner)
	if newOwner != alice {
		t.Fatalf("owner 应已变更为 alice,实际 %s", newOwner)
	}
	// 原 owner 仍在成员表里(manager),可继续访问
	m, err := repo.SpaceRepo{}.GetMembership(ctx, f.pool, f.spaceID, f.ownerID)
	if err != nil || m == nil {
		t.Fatalf("原 owner 应仍是成员: m=%+v err=%v", m, err)
	}
	if m.IsOwner {
		t.Fatal("原 owner 不应再标记为 owner")
	}
	if m.Permission != model.PermManager {
		t.Fatalf("原 owner 权限应为 manager,实际 %s", m.Permission)
	}
	// 新 owner 现在可以管理成员
	if _, err := f.svc.AddOrUpdateMember(ctx, alice, f.spaceID, f.ownerID, model.PermEditor); err != nil {
		t.Fatalf("新 owner 应可管理成员: %v", err)
	}
	// 新 owner 现在不能退出(409)
	if err := f.svc.Leave(ctx, alice, f.spaceID); err == nil {
		t.Fatal("新 owner 未转让前退出应失败")
	}
}

// 解散:仅 owner;空间非空 → 409
func TestDissolveRequiresEmptyAndOwner(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermManager); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}
	// 非 owner → 403
	if err := f.svc.Dissolve(ctx, alice, f.spaceID); err == nil {
		t.Fatal("非 owner 解散应失败")
	}
	// 空间非空 → 409
	if _, err := f.pool.Exec(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1, $2, $3, 'x.txt', false, 1, 1)`, f.spaceID, f.rootID, f.ownerID); err != nil {
		t.Fatalf("造文件失败: %v", err)
	}
	err := f.svc.Dissolve(ctx, f.ownerID, f.spaceID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("空间非空应 409,得到 %v", err)
	}
	// 清空后可以解散
	if _, err := f.pool.Exec(ctx, `DELETE FROM files WHERE space_id=$1 AND parent_id IS NOT NULL`, f.spaceID); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	if err := f.svc.Dissolve(ctx, f.ownerID, f.spaceID); err != nil {
		t.Fatalf("清空后应可解散: %v", err)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM spaces WHERE id=$1`, f.spaceID).Scan(&n)
	if n != 0 {
		t.Fatal("解散后空间行应已删除")
	}
}

// 个人空间不能退出/解散/转让
func TestPersonalSpaceCannotLeaveOrDissolve(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	sp, err := repo.SpaceRepo{}.PersonalOf(ctx, f.pool, f.ownerID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	if err := f.svc.Leave(ctx, f.ownerID, sp.ID); err == nil {
		t.Fatal("个人空间退出应失败")
	}
	if err := f.svc.Dissolve(ctx, f.ownerID, sp.ID); err == nil {
		t.Fatal("个人空间解散应失败")
	}
}

// ---- 管理员治理(S3-07)----

// 冻结后成员读写被拒,且原因为"被管理员冻结";解冻后恢复
func TestFreezeBlocksAndUnfreezeRestores(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	alice := f.newUser(t, "sp_alice_")
	if _, err := f.svc.AddOrUpdateMember(ctx, f.ownerID, f.spaceID, alice, model.PermEditor); err != nil {
		t.Fatalf("加 alice 失败: %v", err)
	}

	sp, err := f.svc.SetFrozen(ctx, f.spaceID, true)
	if err != nil {
		t.Fatalf("冻结失败: %v", err)
	}
	if !sp.Frozen {
		t.Fatal("返回的空间应标记 frozen")
	}

	// 冻结后连 owner 也被拒
	_, err = f.svc.ListMembers(ctx, f.ownerID, f.spaceID)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Code != apierr.CodeSpaceRevoked {
		t.Fatalf("冻结后应 space_revoked,得到 %v", err)
	}
	if !stringsContains(ae.Message, "冻结") {
		t.Errorf("文案应说明被冻结,实际 %q", ae.Message)
	}

	// 解冻后恢复
	if _, err := f.svc.SetFrozen(ctx, f.spaceID, false); err != nil {
		t.Fatalf("解冻失败: %v", err)
	}
	if _, err := f.svc.ListMembers(ctx, f.ownerID, f.spaceID); err != nil {
		t.Fatalf("解冻后应恢复访问: %v", err)
	}
}

func TestFreezeMissingSpace(t *testing.T) {
	f := setup(t)
	_, err := f.svc.SetFrozen(context.Background(), "00000000-0000-7000-8000-000000000000", true)
	var ae *apierr.Error
	if !errors.As(err, &ae) || ae.Status != 410 {
		t.Fatalf("不存在的空间应 410,得到 %v", err)
	}
}

// ---- 辅助 ----

func (f *fixture) countMembers(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM space_members WHERE space_id = $1`, f.spaceID).Scan(&n); err != nil {
		t.Fatalf("统计成员失败: %v", err)
	}
	return n
}

func (f *fixture) newUser(t *testing.T, prefix string) string {
	t.Helper()
	suffix := time.Now().Format("150405.000000000")
	var id string
	err := f.pool.QueryRow(context.Background(), `
INSERT INTO users (username, email, display_name, role) VALUES ($1,$2,$3,'user') RETURNING id`,
		prefix+suffix, prefix+suffix+"@example.com", prefix).Scan(&id)
	if err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = f.pool.Exec(bg, `DELETE FROM space_members WHERE user_id = $1`, id)
		_, _ = f.pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
		_, _ = f.pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
		_, _ = f.pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func stringsContains(s, sub string) bool { return strings.Contains(s, sub) }
