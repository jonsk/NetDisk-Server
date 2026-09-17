// 初始管理员账号播种(初始化数据库时自动建立)。
//
// 需求:初始化数据库后默认自动建立一个管理员账号(admin/admin123)。
//
// 为什么放在 migrate 包而不是单独命令:
//   - 与"数据库初始化"这一动作强绑定——迁移建好表之后立刻播种,
//     首次部署无需额外手工开通步骤即得到一个可登录的超管;
//   - 幂等:已存在同名账号时跳过,不覆盖既有口令/角色,重复启动无副作用。
//
// 安全约定:
//   - 用户名与**初始口令**均允许通过环境变量覆盖(见 main 的取参),未设置时才用
//     admin/admin123 默认值;
//   - 播种成功日志会明确提示"初始口径是默认口令,请尽快修改",避免把默认口令当成
//     生产长期口令。
package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// SeedOptions 播种参数。
type SeedOptions struct {
	// Username 初始管理员用户名;空则用 "admin"。
	Username string
	// Password 初始口令;空则用 "admin123"(用户需求默认值)。
	Password string
	// DisplayName 显示名;空则用 "系统管理员"。
	DisplayName string
	// Role 平台角色;空则用 super_admin。
	Role string
}

// SeedResult 播种结果(供调用方打日志)。
type SeedResult struct {
	// Username 实际使用的用户名。
	Username string
	// Skipped 为 true 表示已存在、未做任何修改。
	Skipped bool
	// UsedDefaultPassword 为 true 表示口令用了默认值(非环境变量覆盖),日志提示改密。
	UsedDefaultPassword bool
}

// SeedAdmin 在已迁移的数据库上播种初始管理员账号(幂等)。
//
// 流程:先查同名用户,存在则跳过;否则在同一事务内创建用户(自动附带个人空间
// 与根目录,见 repo.UserRepo.Create)并设置口令。
func SeedAdmin(ctx context.Context, pool *pgxpool.Pool, o SeedOptions) (*SeedResult, error) {
	if strings.TrimSpace(o.Username) == "" {
		o.Username = "admin"
	}
	if o.DisplayName == "" {
		o.DisplayName = "系统管理员"
	}
	if o.Role == "" {
		o.Role = model.RoleSuperAdmin
	}
	if o.Password == "" {
		o.Password = "admin123"
	}

	pol := credentials.DefaultPolicy()
	if err := pol.Validate(o.Password); err != nil {
		// 环境覆盖的口令过弱时直接拒绝,避免"初始化成功却永远登不上"(策略在登录侧还会再拦一次)。
		return nil, fmt.Errorf("初始管理员口令不符合策略: %w", err)
	}
	hash, err := pol.Hash(o.Password)
	if err != nil {
		return nil, fmt.Errorf("生成初始口令哈希失败: %w", err)
	}

	users := repo.UserRepo{}
	existing, err := users.GetByUsername(ctx, pool, o.Username)
	if err == nil && existing != nil {
		return &SeedResult{Username: o.Username, Skipped: true}, nil
	}
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("检查初始管理员是否已存在: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启播种事务失败: %w", err)
	}
	defer tx.Rollback(ctx)

	u, err := users.Create(ctx, tx, repo.CreateInput{
		Username:    o.Username,
		DisplayName: o.DisplayName,
		Role:        o.Role,
		Status:      model.StatusActive,
	})
	if err != nil {
		return nil, fmt.Errorf("创建初始管理员失败: %w", err)
	}
	if err := users.SetPassword(ctx, tx, u.ID, hash); err != nil {
		return nil, fmt.Errorf("设置初始管理员口令失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交播种事务失败: %w", err)
	}

	return &SeedResult{
		Username:            o.Username,
		UsedDefaultPassword: o.Password == "admin123",
	}, nil
}
