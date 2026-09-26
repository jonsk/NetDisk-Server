// Package orgsvc 实现组织架构(部门树)用例(4.2 / 6.4)。
//
// 组织树的真相是 `departments.parent_id`,闭包表是派生索引 ——
// 本包的核心职责是保证"派生索引永远与真相一致",并提供一条幂等的全量重建。
package orgsvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// DB 抽象"读走池、写走事务",与 authsvc.DB / uploadsvc.DB 同构。
//
// 由 db.QuerierAdapter 满足(db.AsQuerier(database))。
type DB interface {
	repo.Querier
	InTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// Service 组织架构用例入口。
type Service struct {
	Depts repo.DeptRepo
	DB    DB
}

// RebuildResult 是重建结果。
type RebuildResult struct {
	// ClosureRows 写入的闭包行数(含每节点指向自身的 depth=0 行)
	ClosureRows int64 `json:"closure_rows"`
	// DeptCount 闭包表中出现的节点数(应等于部门总数)
	DeptCount int64 `json:"dept_count"`
	// MaxDepth 最大层级(根为 0)
	MaxDepth int `json:"max_depth"`
	// Rebuilt 为 true 表示校验通过、事务已提交
	Rebuilt bool `json:"rebuilt"`
}

// Rebuild 全量重建闭包表,并在重建后校验自洽性。
//
// 幂等:连续调用两次结果相同 —— 这正是"组织同步可随时重跑"的前提(6.4)。
//
// 校验失败时**整体回滚**且不自动修复:闭包表能由 parent_id 完全推导,
// 所以"闭包不一致"必然源于 parent_id 本身有病(最常见是数据环)。
// 静默修复只会把环藏起来,下次同步照旧出错。
func (s *Service) Rebuild(ctx context.Context) (*RebuildResult, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	var res *RebuildResult
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		r, err := s.Depts.RebuildClosure(ctx, tx)
		if err != nil {
			return apierr.Internal(err)
		}
		problems, err := s.Depts.ValidateTree(ctx, tx)
		if err != nil {
			return apierr.Internal(err)
		}
		res = &RebuildResult{
			ClosureRows: r.ClosureRows,
			DeptCount:   r.DeptCount,
			MaxDepth:    r.MaxDepth,
		}
		if len(problems) > 0 {
			return apierr.Internal(fmt.Errorf(
				"部门树自洽性校验失败(%d 处问题: 首个=%s/%s),闭包表已回滚",
				len(problems), problems[0].Kind, problems[0].Name))
		}
		res.Rebuilt = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// CreateInput 新建部门入参。
type CreateInput struct {
	ParentID  string
	Name      string
	SortOrder int
	Source    string
}

// Create 新建部门(同步维护闭包行)。
func (s *Service) Create(ctx context.Context, in CreateInput) (*model.Department, error) {
	if err := validateDeptName(in.Name); err != nil {
		return nil, err
	}
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	var out *model.Department
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if in.ParentID == "" {
			// 建根:拦截"第二个根"。合并页"单根"假设(左树以根为顶级)依赖此守卫。
			ok, err := s.Depts.RootExists(ctx, tx)
			if err != nil {
				return apierr.Internal(err)
			}
			if ok {
				return apierr.Conflict(apierr.CodeInvalidArgument,
					"已存在机构根节点,不能创建第二个根;顶层机构请在某个子节点下新建")
			}
		} else {
			// 父必须存在,否则会把"打错 id"变成一个孤立子树
			if _, err := s.Depts.GetByID(ctx, tx, in.ParentID); err != nil {
				if errors.Is(err, repo.ErrNotFound) {
					return apierr.NotFound("上级部门不存在")
				}
				return apierr.Internal(err)
			}
		}
		d, err := s.Depts.Create(ctx, tx, repo.CreateDeptInput{
			ParentID: in.ParentID, Name: in.Name, SortOrder: in.SortOrder, Source: in.Source,
		})
		if err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return apierr.Conflict(apierr.CodeNameConflict, "同源同 ext_id 的部门已存在")
			}
			return apierr.Internal(err)
		}
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Move 移动部门。
//
// **必须拦住的两种情况**:
//  1. 移到自己或自己的后代下 → 造出数据环(闭包表会出现"我是我自己的祖先")
//  2. 移到不存在/已被删的父下
//
// 环检查走闭包表 IsDescendantOf(一条 SQL),不递归遍历。
func (s *Service) Move(ctx context.Context, deptID, newParentID string) (*model.Department, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	if deptID == newParentID {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "不能把部门移动到它自己下面")
	}
	var out *model.Department
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := s.Depts.GetByID(ctx, tx, deptID); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("部门不存在")
			}
			return apierr.Internal(err)
		}
		if newParentID != "" {
			if _, err := s.Depts.GetByID(ctx, tx, newParentID); err != nil {
				if errors.Is(err, repo.ErrNotFound) {
					return apierr.NotFound("目标上级部门不存在")
				}
				return apierr.Internal(err)
			}
			// 目标必须**不是**自己的后代(含自己)
			if ok, err := s.Depts.IsDescendantOf(ctx, tx, newParentID, deptID); err != nil {
				return apierr.Internal(err)
			} else if ok {
				return apierr.BadRequest(apierr.CodeInvalidArgument,
					"不能把部门移动到它自己的下级部门下(会造成组织树成环)")
			}
		}
		d, err := s.Depts.Move(ctx, tx, deptID, newParentID)
		if err != nil {
			return apierr.Internal(err)
		}
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Delete 拒绝式删除机构(前置校验,非级联)。
//
// 语义:删除机构前,必须先删完它的全部子部门和直属人员。
//   - 根节点 → 始终拒绝("机构根节点不可删除,只能改名")
//   - 还有子部门 → 409
//   - 还有直属人员 → 409
//   - 两者都清空 → 允许删除
//
// ⚠ 守卫必须位于 repo.Delete 之前(先查后删):repo.Delete 的 DELETE 会触发
// user_departments.department_id 的 FK CASCADE(连带清成员行)。因为本方法在
// repo.Delete 前用 CountChildren/CountUsersInDept 拦住了"有子部门/有成员",
// CASCADE 只会在"确实为空"时兜底,行为正确。切勿因加了 CountUsersInDept 就去掉
// 根保护或子部门检查 —— CASCADE 只是空节点删除时的兜底清理,不是结构/成员保护。
func (s *Service) Delete(ctx context.Context, deptID string) error {
	if s.DB == nil {
		return apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		target, err := s.Depts.GetByID(ctx, tx, deptID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("机构不存在")
			}
			return apierr.Internal(err)
		}
		// 根保护
		if target.ParentID == "" {
			return apierr.Conflict(apierr.CodeInvalidArgument, "机构根节点不可删除，只能改名")
		}

		// 检查子部门
		childCount, err := s.Depts.CountChildren(ctx, tx, deptID)
		if err != nil {
			return apierr.Internal(err)
		}
		if childCount > 0 {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"该机构下还有 %d 个下属机构，请先删除", childCount)
		}

		// 检查直属人员(子部门已清空,只需查本部门)
		userCount, err := s.Depts.CountUsersInDept(ctx, tx, deptID)
		if err != nil {
			return apierr.Internal(err)
		}
		if userCount > 0 {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"该机构下还有 %d 名人员，请先移除", userCount)
		}

		if err := s.Depts.Delete(ctx, tx, deptID); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("机构不存在")
			}
			if errors.Is(err, repo.ErrConflict) {
				return apierr.Conflict(apierr.CodeInvalidArgument, "该机构仍有成员或关联数据,无法删除")
			}
			return apierr.Internal(err)
		}
		return nil
	})
}

// DeleteCheckResult 是删除前预检结果(前端据此决定删除按钮是否可点)。
type DeleteCheckResult struct {
	CanDelete  bool  `json:"can_delete"`
	ChildCount int64 `json:"child_count"`
	UserCount  int64 `json:"user_count"` // 本部门直属人员(子部门人员由各自的 delete-check 负责)
}

// DeleteCheck 返回某机构的删除前置条件状态。
//
// 与 Delete 的 409 根保护对齐:根节点(parent_id IS NULL)直接返回 can_delete=false,
// 即使前端对根节点不显示删除按钮,后端也要拦。
func (s *Service) DeleteCheck(ctx context.Context, deptID string) (*DeleteCheckResult, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	var res *DeleteCheckResult
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		target, err := s.Depts.GetByID(ctx, tx, deptID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("机构不存在")
			}
			return apierr.Internal(err)
		}
		r := &DeleteCheckResult{}
		if target.ParentID == "" {
			// 根节点照常算出计数(前端可复用文案),但 can_delete 恒 false。
			r.CanDelete = false
		} else {
			r.CanDelete = true
		}
		childCount, err := s.Depts.CountChildren(ctx, tx, deptID)
		if err != nil {
			return apierr.Internal(err)
		}
		userCount, err := s.Depts.CountUsersInDept(ctx, tx, deptID)
		if err != nil {
			return apierr.Internal(err)
		}
		r.ChildCount = childCount
		r.UserCount = userCount
		if r.CanDelete && (childCount > 0 || userCount > 0) {
			r.CanDelete = false
		}
		res = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Children 返回某节点的直接子部门(一层,懒加载)。parentID 为空返回根节点。
func (s *Service) Children(ctx context.Context, parentID string) ([]repo.DeptWithChildren, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	return s.Depts.Children(ctx, s.DB, parentID)
}

// Rename 改机构/部门名。
func (s *Service) Rename(ctx context.Context, deptID, name string) (*model.Department, error) {
	if err := validateDeptName(name); err != nil {
		return nil, err
	}
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	var out *model.Department
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		d, err := s.Depts.Rename(ctx, tx, deptID, name)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("机构不存在")
			}
			return apierr.Internal(err)
		}
		out = d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Subtree 返回子树(含自身),供后台树展示与"按部门授权"预览影响面。
func (s *Service) Subtree(ctx context.Context, deptID string) (*repo.Subtree, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	st, err := s.Depts.SubtreeOf(ctx, s.DB, deptID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("部门不存在")
		}
		return nil, apierr.Internal(err)
	}
	return st, nil
}

// Tree 返回全部部门(前端一次拉全量渲染树;组织规模在千级以内)。
func (s *Service) Tree(ctx context.Context) ([]*model.Department, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	all, err := s.Depts.ListAll(ctx, s.DB)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	return all, nil
}

// SetUserDepartments 覆盖式设置用户所属部门。
func (s *Service) SetUserDepartments(ctx context.Context, userID string, deptIDs []string, primary string) error {
	if s.DB == nil {
		return apierr.Internal(errors.New("orgsvc: DB 未装配"))
	}
	if len(deptIDs) == 0 {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "至少需要指定一个部门")
	}
	// 去重(同步源重复给同一部门是常见现象)
	seen := map[string]bool{}
	uniq := make([]string, 0, len(deptIDs))
	for _, d := range deptIDs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		uniq = append(uniq, d)
	}
	if len(uniq) == 0 {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "部门 id 不能为空")
	}
	if primary != "" && !seen[primary] {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "主部门必须在部门列表中")
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		if err := s.Depts.SetUserDepartments(ctx, tx, userID, uniq, primary); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("用户或部门不存在")
			}
			return apierr.Internal(err)
		}
		return nil
	})
}

// validateDeptName 校验部门名(4.2 未定义专门规则,只做基本防御)。
//
// 刻意**不复用 namepolicy**:那是文件名规则(禁 `/`、禁 Win32 保留名等),
// 而部门名含 `/`(如"研发/平台组")是常见诉求,套文件名规则会造成误拒。
func validateDeptName(name string) error {
	if name == "" {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "部门名不能为空")
	}
	if len(name) > 128 {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "部门名过长(上限 128 字节)")
	}
	return nil
}
