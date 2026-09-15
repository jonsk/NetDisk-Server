package authsvc_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 本文件**不带 build tag**:普通单测与 integration 标签下都编译。
// 通过环境变量 NETDISK_TEST_DSN 判定是否具备连库条件;缺失则 skip。
//
// 这与任务清单 E-03 的定稿一致:本机无 Docker,集成测试直连 netdisk_test 库。

const testSecret = "0123456789abcdef0123456789abcdef"

// Fixture 是一次测试所需的全部依赖与已建好的测试用户。
type Fixture struct {
	Svc      *authsvc.Service
	DB       *db.DB
	Users    repo.UserRepo
	Files    repo.FileRepo
	Spaces   repo.SpaceRepo
	Tokens   *auth.Manager
	UserID   string
	Login    string
	Password string
	Policy   credentials.Policy
	Redis    *miniredis.Miniredis
}

// timeNow 与 authsvc 内部使用的口令时间格式保持一致(用于生成唯一登录名)。
func timeNow() time.Time { return time.Now() }

// Setup 建测试用户 + 个人空间 + Redis + TokenManager。
//
// 注意:调用 SetPassword 会自增 token_version,因此最后要重新读取用户,
// 保证 Fixture.UserID 对应的 token_version 是最新的。
func Setup(t *testing.T) *Fixture {
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

	// 唯一登录名,避免用例间互相污染
	suffix := timeNow().Format("150405.000000")
	login := "it_" + suffix
	pass := "P@ssw0rd" + timeNow().Format("05.000")

	pol := credentials.DefaultPolicy()
	pol.Cost = 4 // 测试加速

	users := repo.UserRepo{}
	var created *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		u, err := users.Create(ctx, tx, repo.CreateInput{
			Username:    login,
			Email:       login + "@example.com",
			DisplayName: "集成测试用户",
			Role:        model.RoleUser,
		})
		if err != nil {
			return err
		}
		hash, err := pol.Hash(pass)
		if err != nil {
			return err
		}
		if err := users.SetPassword(ctx, tx, u.ID, hash); err != nil {
			return err
		}
		created = u
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}

	u, err := users.GetByID(ctx, database.Pool, created.ID)
	if err != nil {
		t.Fatalf("读取用户失败: %v", err)
	}

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	jwtCfg := config.Default().JWT
	jwtCfg.Secret = testSecret
	tokens := auth.NewManager(jwtCfg, rdb, "netdisk:")

	svc := &authsvc.Service{
		Users:  users,
		Spaces: repo.SpaceRepo{},
		Tokens: tokens,
		Policy: pol,
		DB:     db.AsQuerier(database),
	}

	t.Cleanup(func() {
		// 删用户会级联删除 personal 空间与 refresh_tokens
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, u.ID)
	})

	return &Fixture{
		Svc: svc, DB: database, Users: users, Files: repo.FileRepo{}, Spaces: repo.SpaceRepo{},
		Tokens: tokens, UserID: u.ID, Login: login, Password: pass, Policy: pol, Redis: mr,
	}
}
