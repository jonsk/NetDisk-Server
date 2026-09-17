package migrate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// TestSeedAdmin 验证初始管理员播种:创建 → 幂等跳过 → 口令可校验。
//
// 用独立用户名(seed_test_admin)而非默认 admin,避免与并行用例/重复运行冲突;
// 默认 admin 的"已存在即跳过"语义由二次播种断言覆盖。
func TestSeedAdmin(t *testing.T) {
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)

	if err := Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	const (
		username = "seed_test_admin"
		password = "SeedPass!x7" // 满足默认策略:>=8 位、含字母+数字+符号
	)
	r, err := SeedAdmin(ctx, database.Pool, SeedOptions{
		Username: username, Password: password, Role: model.RoleSuperAdmin,
	})
	if err != nil {
		t.Fatalf("首次播种失败: %v", err)
	}
	if r.Skipped {
		t.Fatalf("首次播种不应 Skipped")
	}
	if r.Username != username {
		t.Fatalf("返回用户名=%q,期望 %q", r.Username, username)
	}

	// 二次播种应幂等跳过(不覆盖口令/角色)。
	r2, err := SeedAdmin(ctx, database.Pool, SeedOptions{Username: username, Password: "Another!p9"})
	if err != nil {
		t.Fatalf("二次播种失败: %v", err)
	}
	if !r2.Skipped {
		t.Fatalf("二次播种应 Skipped(已存在),实际创建了")
	}

	// 口令校验:拿凭证快照,用默认策略 Verify。
	var users repo.UserRepo
	cred, err := users.GetCredentials(ctx, database.Pool, username)
	if err != nil {
		t.Fatalf("读取凭证失败: %v", err)
	}
	if cred.Status != model.StatusActive {
		t.Fatalf("初始管理员状态=%q,期望 active", cred.Status)
	}
	pol := credentials.DefaultPolicy()
	if err := pol.Verify(cred.PasswordHash, password); err != nil {
		t.Fatalf("初始口令校验失败: %v", err)
	}
	// 二次播种传入的另一个口令不应生效(跳过=不覆盖)。
	if err := pol.Verify(cred.PasswordHash, "Another!p9"); err == nil {
		t.Fatalf("二次播种覆盖了口令(不应发生):另一个口令也能通过")
	}

	// 角色应为 super_admin。
	u, err := users.GetByUsername(ctx, database.Pool, username)
	if err != nil {
		t.Fatalf("按用户名查用户失败: %v", err)
	}
	if u.Role != model.RoleSuperAdmin {
		t.Fatalf("初始管理员角色=%q,期望 super_admin", u.Role)
	}
}

// TestSeedAdminRejectsWeakPassword 环境覆盖口令过弱时应拒绝(不产生半初始化用户)。
func TestSeedAdminRejectsWeakPassword(t *testing.T) {
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 2)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)

	_, err = SeedAdmin(ctx, database.Pool, SeedOptions{
		Username: "seed_weak_pw", Password: "short", // 过短,不合策略
	})
	if err == nil {
		t.Fatalf("过弱口令不应播种成功")
	}
	// 未产生用户。
	var users repo.UserRepo
	if _, gerr := users.GetByUsername(ctx, database.Pool, "seed_weak_pw"); !errors.Is(gerr, repo.ErrNotFound) {
		t.Fatalf("过弱口令播种本应不落任何用户,实际 err=%v", gerr)
	}
}
