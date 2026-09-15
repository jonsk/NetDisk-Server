// Package usersvc 是**后台用户管理**用例层(管理后台 FE-W-04)。
//
// # 为什么单独一个包,而不是写在 handler 里
//
// "停用一个账号"不是一个 UPDATE:它必须同时**吊销该用户全部 refresh token 并
// 自增 token_version**,否则已登录的会话可以一直用到 access token 过期(15min),
// 而管理员看到的是"我已经停用了他,他还在传文件"。这类"一个动作牵动多张表"的
// 逻辑放 handler 里,迟早会有人只改一半。
package usersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// DB 是服务需要的读写入口(读走池、写走事务)。
type DB interface {
	repo.Querier
	InTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// Service 是后台用户管理入口。
type Service struct {
	DB    DB
	Users repo.UserRepo
	Log   *slog.Logger
}

// CreateInput 是后台建号入参。
type CreateInput struct {
	Username    string
	Email       string
	DisplayName string
	Role        string
}

// UpdateInput 是后台改用户入参;**指针字段表示"本次不修改"**。
//
// 用指针而不是零值判断:`status: ""` 与"没传 status"必须区分开 ——
// 前者是前端 bug(应报 400),后者是"只改角色"。用零值判断会把前者
// 静默当成"不修改",于是管理员点了"停用"却什么都没发生。
type UpdateInput struct {
	UserID string
	Role   *string
	Status *string
}

// RoleAllowed 判断角色是否合法(与 model 常量同源,新增角色只改一处)。
func RoleAllowed(role string) bool {
	switch role {
	case model.RoleSuperAdmin, model.RoleDeptAdmin, model.RoleUser:
		return true
	default:
		return false
	}
}

// StatusAllowed 判断状态是否合法。
func StatusAllowed(status string) bool {
	switch status {
	case model.StatusActive, model.StatusDisabled, model.StatusPending:
		return true
	default:
		return false
	}
}

// List 分页返回用户(带最后登录时间,后台需要"这人最近用过吗")。
func (s *Service) List(ctx context.Context, f repo.UserFilter) ([]repo.AdminUserRow, int, error) {
	if s.DB == nil {
		return nil, 0, errors.New("usersvc: DB 未装配")
	}
	return s.Users.ListWithLogin(ctx, s.DB, f)
}

// Create 后台建号(含个人空间,与其它入口同一条 repo 路径)。
//
// 唯一冲突必须**指出是哪个字段**:用户名与邮箱撞车的处置完全不同
// (改登录名 vs 改邮箱),而"唯一约束冲突"这句提示让管理员只能靠猜。
// 所以这里**先预检再插入**(与 orgsync 的纪律一致:PG 的唯一冲突会终止事务,
// 之后再查什么都报 25P02)。
func (s *Service) Create(ctx context.Context, in CreateInput) (*model.User, error) {
	if s.DB == nil {
		return nil, errors.New("usersvc: DB 未装配")
	}
	username := strings.TrimSpace(in.Username)
	email := strings.TrimSpace(in.Email)
	if username == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "用户名不能为空")
	}
	role := strings.TrimSpace(in.Role)
	if role == "" {
		role = model.RoleUser
	}
	if !RoleAllowed(role) {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "角色不合法: %s", role)
	}
	display := strings.TrimSpace(in.DisplayName)
	if display == "" {
		display = username
	}

	var created *model.User
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, uerr := s.Users.GetByUsername(ctx, tx, username); uerr == nil {
			return accountConflict("username", username)
		} else if !errors.Is(uerr, repo.ErrNotFound) {
			return uerr
		}
		if email != "" {
			if _, eerr := s.Users.GetByEmail(ctx, tx, email); eerr == nil {
				return accountConflict("email", email)
			} else if !errors.Is(eerr, repo.ErrNotFound) {
				return eerr
			}
		}
		u, cerr := s.Users.Create(ctx, tx, repo.CreateInput{
			Username: username, Email: email, DisplayName: display,
			Role: role, Status: model.StatusActive,
		})
		if cerr != nil {
			// 预检与插入之间仍有极小的并发窗口:此时按 23505 兜底,
			// 但**不能**说清是哪个字段 —— 至少不要把它变成 500
			if errors.Is(cerr, repo.ErrConflict) {
				return accountConflict("username_or_email", username)
			}
			return cerr
		}
		created = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// Update 改角色与/或状态;**停用是一次"生效"操作**(吊销刷新令牌 + 自增令牌版本)。
func (s *Service) Update(ctx context.Context, in UpdateInput) (*model.User, error) {
	if s.DB == nil {
		return nil, errors.New("usersvc: DB 未装配")
	}
	if strings.TrimSpace(in.UserID) == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少用户 id")
	}
	if in.Role == nil && in.Status == nil {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "没有要修改的字段")
	}
	if in.Role != nil {
		role := strings.TrimSpace(*in.Role)
		if !RoleAllowed(role) {
			return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "角色不合法: %s", role)
		}
	}
	if in.Status != nil {
		status := strings.TrimSpace(*in.Status)
		if !StatusAllowed(status) {
			return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "状态不合法: %s", status)
		}
	}

	var updated *model.User
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		current, gerr := s.Users.GetByID(ctx, tx, in.UserID)
		if gerr != nil {
			return gerr
		}
		if in.Role != nil {
			if rerr := s.Users.SetRole(ctx, tx, in.UserID, strings.TrimSpace(*in.Role)); rerr != nil {
				return rerr
			}
		}
		if in.Status != nil {
			status := strings.TrimSpace(*in.Status)
			if serr := s.Users.SetStatus(ctx, tx, in.UserID, status); serr != nil {
				return serr
			}
			if status != model.StatusActive && current.Status == model.StatusActive {
				// **停用/待激活必须立刻生效**:吊销全部 refresh token 并自增
				// token_version —— 只改 status 字段的话,已签发的 access token
				// 仍能用到过期(15min),管理员会看到"停用了但还在传"。
				if _, rerr := s.Users.RevokeAllForUser(ctx, tx, in.UserID); rerr != nil {
					return fmt.Errorf("usersvc: 吊销刷新令牌失败: %w", rerr)
				}
				if _, berr := s.Users.BumpTokenVersion(ctx, tx, in.UserID); berr != nil {
					return fmt.Errorf("usersvc: 自增令牌版本失败: %w", berr)
				}
			}
		}
		after, aerr := s.Users.GetByID(ctx, tx, in.UserID)
		if aerr != nil {
			return aerr
		}
		updated = after
		return nil
	})
	if err != nil {
		// 领域错误 → HTTP 语义只在这一层做(handler 不解析 repo 错误):
		// 不存在的用户是 **404**,而不是"服务端故障 500" —— 后者会让前端
		// 提示"稍后重试",而管理员真正该做的是刷新列表。
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("用户不存在")
		}
		return nil, err
	}
	return updated, nil
}

// accountConflict 构造"账号名/邮箱已被占用"的 409(带 field,前端据此高亮输入框)。
func accountConflict(field, value string) error {
	label := "用户名"
	if field == "email" {
		label = "邮箱"
	}
	return apierr.Conflict(apierr.CodeAccountConflict, "%s已被占用: %s", label, value).
		WithDetail("field", field)
}
