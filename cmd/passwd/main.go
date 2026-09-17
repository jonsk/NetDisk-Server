// netdisk-passwd 是账号运维命令:创建后续用户 / 重置口令 / 启停用。
//
// 说明:初始管理员账号(admin/admin123)已在"初始化数据库"时由迁移自动播种,
// 见 internal/migrate/seed.go;本命令用于创建更多用户、给现场换更安全的口令、
// 以及停启用/解锁账号等运维动作。
//
// 用法:
//
//	netdisk-passwd -username admin -role super_admin -display "系统管理员" -prompt
//	netdisk-passwd -username alice -password 'S3cret!pass' -reset
//	netdisk-passwd -username alice -disable
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

func main() {
	var (
		cfgPath  = flag.String("config", "", "配置文件路径(yaml);默认只读 env")
		username = flag.String("username", "", "用户名(必填)")
		email    = flag.String("email", "", "邮箱(可选,可用于登录)")
		display  = flag.String("display", "", "显示名(默认同用户名)")
		role     = flag.String("role", model.RoleUser, "角色: super_admin / dept_admin / user")
		password = flag.String("password", "", "口令;留空则用 -gen 生成或 -prompt 交互输入")
		gen      = flag.Bool("gen", false, "生成一个符合策略的随机口令并打印")
		prompt   = flag.Bool("prompt", false, "交互式输入口令(不回显)")
		reset    = flag.Bool("reset", false, "仅重置口令(用户必须已存在)")
		disable  = flag.Bool("disable", false, "停用账号")
		enable   = flag.Bool("enable", false, "启用账号")
		lock     = flag.Bool("unlock", false, "解除登录锁定")
	)
	flag.Parse()

	if err := run(*cfgPath, args{
		username: *username, email: *email, display: *display, role: *role,
		password: *password, gen: *gen, prompt: *prompt,
		reset: *reset, disable: *disable, enable: *enable, unlock: *lock,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "netdisk-passwd: %v\n", err)
		os.Exit(1)
	}
}

type args struct {
	username, email, display, role, password    string
	gen, prompt, reset, disable, enable, unlock bool
}

func run(cfgPath string, a args) error {
	if a.username == "" {
		return errors.New("-username 必填")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if cfg.Database.Password == "" {
		return errors.New("NETDISK_DB_PASSWORD 未设置(由 env 注入)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	database, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}
	defer database.Close()

	users := repo.UserRepo{}
	pol := credentials.DefaultPolicy()

	// 状态类操作不需要口令
	if a.disable || a.enable || a.unlock {
		if err := updateStatus(ctx, database, users, a); err != nil {
			return err
		}
		return nil
	}

	// 口令来源:显式 / 生成 / 交互
	plain := a.password
	if plain == "" && a.gen {
		plain, err = generatePassword(pol)
		if err != nil {
			return err
		}
		fmt.Printf("生成的口令(请立即保存,仅显示一次): %s\n", plain)
	}
	if plain == "" && a.prompt {
		plain, err = readPasswordTwice()
		if err != nil {
			return err
		}
	}
	if plain == "" {
		return errors.New("需提供口令:使用 -password / -gen / -prompt 之一")
	}
	if err := pol.Validate(plain); err != nil {
		return fmt.Errorf("口令不符合策略: %w", err)
	}
	hash, err := pol.Hash(plain)
	if err != nil {
		return err
	}

	if a.reset {
		return resetPassword(ctx, database, users, a.username, hash)
	}
	return createUser(ctx, database, users, a, hash)
}

func createUser(ctx context.Context, database *db.DB, users repo.UserRepo, a args, hash string) error {
	existing, err := users.GetByUsername(ctx, database.Pool, a.username)
	if err == nil && existing != nil {
		return fmt.Errorf("用户 %q 已存在(id=%s);如需改密请加 -reset", a.username, existing.ID)
	}
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return err
	}

	var created *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		u, err := users.Create(ctx, tx, repo.CreateInput{
			Username: a.username, Email: a.email, DisplayName: a.display, Role: a.role,
		})
		if err != nil {
			return err
		}
		if err := users.SetPassword(ctx, tx, u.ID, hash); err != nil {
			return err
		}
		created = u
		return nil
	}); err != nil {
		return fmt.Errorf("创建用户失败: %w", err)
	}
	fmt.Printf("已创建用户: username=%s id=%s role=%s(个人空间与根目录已自动建立)\n",
		created.Username, created.ID, created.Role)
	return nil
}

func resetPassword(ctx context.Context, database *db.DB, users repo.UserRepo, username, hash string) error {
	u, err := users.GetByUsername(ctx, database.Pool, username)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return fmt.Errorf("用户 %q 不存在", username)
		}
		return err
	}
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		if err := users.SetPassword(ctx, tx, u.ID, hash); err != nil {
			return err
		}
		// 改密即强制下线:重置 token_version 与失败计数
		if _, err := users.RevokeAllForUser(ctx, tx, u.ID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`UPDATE users SET failed_login_count = 0, locked_until = NULL WHERE id = $1`, u.ID)
		return err
	}); err != nil {
		return fmt.Errorf("重置口令失败: %w", err)
	}
	fmt.Printf("已重置口令并强制下线全部会话: username=%s id=%s\n", u.Username, u.ID)
	return nil
}

func updateStatus(ctx context.Context, database *db.DB, users repo.UserRepo, a args) error {
	u, err := users.GetByUsername(ctx, database.Pool, a.username)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return fmt.Errorf("用户 %q 不存在", a.username)
		}
		return err
	}
	if a.unlock {
		if _, err := database.Pool.Exec(ctx,
			`UPDATE users SET failed_login_count = 0, locked_until = NULL, updated_at = now() WHERE id = $1`,
			u.ID); err != nil {
			return err
		}
		fmt.Printf("已解除锁定: %s\n", u.Username)
		return nil
	}
	status := model.StatusDisabled
	verb := "停用"
	if a.enable {
		status = model.StatusActive
		verb = "启用"
	}
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		if err := users.SetStatus(ctx, tx, u.ID, status); err != nil {
			return err
		}
		// 停用必须同时下线:否则既有 access/refresh 仍可用到过期
		if status == model.StatusDisabled {
			if _, err := users.BumpTokenVersion(ctx, tx, u.ID); err != nil {
				return err
			}
			if _, err := users.RevokeAllForUser(ctx, tx, u.ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("已%s账号: %s\n", verb, u.Username)
	return nil
}

// generatePassword 生成满足策略的随机口令(避开易混淆字符)。
func generatePassword(pol credentials.Policy) (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return encodePassword(buf, pol)
}

// encodePassword 把随机字节映射为口令字符(可注入字节源,便于测试)。
func encodePassword(buf []byte, pol credentials.Policy) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789!@#%^*-_=+"
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	s := string(out)
	if err := pol.Validate(s); err != nil {
		// 极端情况下补一个大写字母与数字,保证满足"至少两类字符"
		s = "Aa1" + s
		if err := pol.Validate(s); err != nil {
			return "", fmt.Errorf("生成口令失败: %w", err)
		}
	}
	return s, nil
}

// readPasswordTwice 交互读两次并比对。
func readPasswordTwice() (string, error) {
	r := bufio.NewReader(os.Stdin)
	fmt.Print("请输入口令(至少 8 位,含两类字符): ")
	p1, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	fmt.Print("请再次输入: ")
	p2, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	p1, p2 = strings.TrimRight(p1, "\r\n"), strings.TrimRight(p2, "\r\n")
	if p1 != p2 {
		return "", errors.New("两次输入不一致")
	}
	return p1, nil
}
