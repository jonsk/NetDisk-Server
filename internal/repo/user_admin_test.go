package repo_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 后台用户列表与角色调整(FE-W-04 的服务端一半)。

func setupUsers(t *testing.T) *db.DB {
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
	return database
}

// seedAdminUser 造一个专用前缀的账号(清理按前缀精确限定,不碰别的用例)。
func seedAdminUser(t *testing.T, database *db.DB, suffix, username, email, role, status string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		u, err := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: username, Email: email, DisplayName: username, Role: role, Status: status,
		})
		if err != nil {
			return err
		}
		id = u.ID
		return nil
	}); err != nil {
		t.Fatalf("建号失败(%s): %v", username, err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// 搜索必须是**子串匹配且大小写不敏感**,并按角色/状态过滤;总数与当页一致。
func TestUserListSearchAndFilter(t *testing.T) {
	database := setupUsers(t)
	ctx := context.Background()
	base := "lq" + time.Now().Format("150405.000000000")
	u1 := seedAdminUser(t, database, base, base+"_zhang", base+"_zhang@example.com", model.RoleUser, model.StatusActive)
	u2 := seedAdminUser(t, database, base, base+"_li", base+"_li@example.com", model.RoleDeptAdmin, model.StatusDisabled)
	_ = u1

	r := repo.UserRepo{}
	rows, total, err := r.ListWithLogin(ctx, database.Pool, repo.UserFilter{Search: base, Limit: 50})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("应命中本用例的 2 个账号,实际 total=%d rows=%d", total, len(rows))
	}

	// 大小写不敏感
	rows, _, err = r.ListWithLogin(ctx, database.Pool, repo.UserFilter{Search: strings.ToUpper(base), Limit: 50})
	if err != nil {
		t.Fatalf("大写搜索失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("搜索应大小写不敏感,实际命中 %d", len(rows))
	}

	// 邮箱子串
	rows, _, err = r.ListWithLogin(ctx, database.Pool, repo.UserFilter{Search: "_li@", Limit: 50})
	if err != nil {
		t.Fatalf("邮箱搜索失败: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != u2 {
		t.Fatalf("按邮箱子串应命中 1 条,实际 %d", len(rows))
	}

	// 角色/状态过滤(与搜索叠加)
	rows, total, err = r.ListWithLogin(ctx, database.Pool,
		repo.UserFilter{Search: base, Role: model.RoleDeptAdmin, Limit: 50})
	if err != nil {
		t.Fatalf("按角色过滤失败: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].Role != model.RoleDeptAdmin {
		t.Fatalf("按角色过滤应命中 1 条,实际 %d", len(rows))
	}
	rows, _, err = r.ListWithLogin(ctx, database.Pool,
		repo.UserFilter{Search: base, Status: model.StatusDisabled, Limit: 50})
	if err != nil {
		t.Fatalf("按状态过滤失败: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != model.StatusDisabled {
		t.Fatalf("按状态过滤应命中 1 条,实际 %d", len(rows))
	}
}

// LIKE 元字符必须被转义:否则搜 `%` 会"匹配全部",管理员以为在筛选、实际在看全表。
func TestUserListEscapesLikeMetacharacters(t *testing.T) {
	database := setupUsers(t)
	ctx := context.Background()
	base := "lqesc" + time.Now().Format("150405.000000000")
	seedAdminUser(t, database, base, base+"aZb", "", model.RoleUser, model.StatusActive)
	seedAdminUser(t, database, base, base+"aXb", "", model.RoleUser, model.StatusActive)

	r := repo.UserRepo{}
	// 判定必须**限定在自己造的前缀里**:直接搜 `_` 会命中库里成千上万个名字里
	// 本来就有下划线的账号 —— 那恰恰说明转义**生效**了,而不是没转义(实测踩过)。
	// 所以造一对"通配符形状相同、字面量不同"的名字,按前缀搜:
	for _, c := range []struct{ search, what string }{
		{base + "a%b", "百分号"},
		{base + "a_b", "下划线"},
	} {
		rows, total, err := r.ListWithLogin(ctx, database.Pool, repo.UserFilter{Search: c.search, Limit: 50})
		if err != nil {
			t.Fatalf("搜索失败: %v", err)
		}
		if total != 0 || len(rows) != 0 {
			t.Fatalf("%s必须按字面量处理(否则等于通配),搜索 %q 实际命中 %d 行",
				c.what, c.search, total)
		}
	}
	// 对照:同前缀、不带通配符的搜索必须命中,证明上面的 0 不是"什么都搜不到"
	if _, total, err := r.ListWithLogin(ctx, database.Pool, repo.UserFilter{Search: base, Limit: 50}); err != nil {
		t.Fatalf("搜索失败: %v", err)
	} else if total != 2 {
		t.Fatalf("对照搜索应命中本用例的 2 个账号,实际 %d", total)
	}
}

// limit 必须被夹取(不能由 URL 决定一次拉多少)。
func TestUserListClampsLimit(t *testing.T) {
	database := setupUsers(t)
	ctx := context.Background()
	r := repo.UserRepo{}
	rows, _, err := r.List(ctx, database.Pool, repo.UserFilter{Limit: 100000})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(rows) > repo.MaxUserListLimit {
		t.Fatalf("limit 必须夹取到 %d,实际返回 %d 行", repo.MaxUserListLimit, len(rows))
	}
}

// 角色调整:命中返回新值,不存在的账号返回 ErrNotFound。
func TestSetRole(t *testing.T) {
	database := setupUsers(t)
	ctx := context.Background()
	base := "lqrole" + time.Now().Format("150405.000000000")
	id := seedAdminUser(t, database, base, base+"_x", "", model.RoleUser, model.StatusActive)

	r := repo.UserRepo{}
	if err := r.SetRole(ctx, database.Pool, id, model.RoleDeptAdmin); err != nil {
		t.Fatalf("改角色失败: %v", err)
	}
	u, err := r.GetByID(ctx, database.Pool, id)
	if err != nil || u.Role != model.RoleDeptAdmin {
		t.Fatalf("角色应已更新: %+v (err=%v)", u, err)
	}
	if err := r.SetRole(ctx, database.Pool, "01a00000-0000-7000-8000-000000000000", model.RoleUser); err == nil {
		t.Fatal("不存在的账号应返回错误")
	}
}
