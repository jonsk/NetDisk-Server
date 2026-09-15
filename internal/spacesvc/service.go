// Package spacesvc 实现空间与团队成员管理(4.3 / 6.11)。
//
// 两条硬性纪律:
//  1. **判权只在服务端**:handler 与客户端都不持有任何权限判断逻辑;
//  2. **成员变更即时生效**:踢人/降权后下一个请求就必须被拒,不等 JWT 过期。
//     实现上不引入"权限缓存"是最省事也最正确的方式 —— 判权每次都读
//     `space_members`(有主键索引,单行走索引查询),把"缓存失效"这个
//     最容易写错的环节直接删掉。Redis 缓存留到有实测压力时再加,
//     且必须配"成员变更时删除缓存键"。
package spacesvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// DB 抽象"读走池、写走事务",与其它 svc 同构。
type DB interface {
	repo.Querier
	InTx(ctx context.Context, fn func(tx pgx.Tx) error) error
}

// Service 空间与成员用例入口。
type Service struct {
	Spaces repo.SpaceRepo
	Files  repo.FileRepo
	Users  repo.UserRepo
	DB     DB
}

// MemberView 是成员对外视图。
type MemberView struct {
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Permission  string `json:"permission"`
	IsOwner     bool   `json:"is_owner"`
}

// ListMembers 列出成员(仅空间可见者)。
func (s *Service) ListMembers(ctx context.Context, userID, spaceID string) ([]MemberView, error) {
	if err := s.requireVisible(ctx, spaceID, userID); err != nil {
		return nil, err
	}
	ms, err := s.Spaces.ListMembers(ctx, s.DB, spaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.SpaceGone("空间不存在或已被解散")
		}
		return nil, apierr.Internal(err)
	}
	out := make([]MemberView, 0, len(ms))
	for _, m := range ms {
		out = append(out, MemberView{
			UserID: m.UserID, Username: m.Username, DisplayName: m.DisplayName,
			Permission: m.Permission, IsOwner: m.IsOwner,
		})
	}
	return out, nil
}

// AddOrUpdateMember 新增成员或改权限(4.3:仅 manager 可操作)。
//
// 三条必须拦住的越权/自锁路径:
//  1. 非 manager 拉人/改权限 → 403
//  2. 改**自己**的权限 → 403(防"把自己降成 reader 后再也管不了"的自锁,
//     也防 owner 之外的人借改权限夺取控制权)
//  3. 改 **owner** 的权限 → 409(owner 的权限不是成员行,改不了也不该改)
func (s *Service) AddOrUpdateMember(ctx context.Context, actorID, spaceID, targetUserID, permission string) (*MemberView, error) {
	if !model.ValidPermission(permission) {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument,
			"权限只能是 manager / editor / reader")
	}
	if actorID == targetUserID {
		return nil, apierr.Forbidden("不能修改自己的成员权限")
	}
	var out *MemberView
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sp, err := s.Spaces.GetByID(ctx, tx, spaceID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在或已被解散")
			}
			return apierr.Internal(err)
		}
		if sp.IsPersonal() {
			return apierr.Forbidden("个人空间不支持添加成员")
		}
		if err := s.requireManager(ctx, tx, sp.ID, actorID); err != nil {
			return err
		}
		if targetUserID == sp.OwnerID {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"空间所有者不是普通成员,其权限不可修改(如需变更请走转让)")
		}
		u, err := s.Users.GetByID(ctx, tx, targetUserID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("用户不存在")
			}
			return apierr.Internal(err)
		}
		if err := s.Spaces.UpsertMember(ctx, tx, sp.ID, targetUserID, permission); err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return apierr.BadRequest(apierr.CodeInvalidArgument, "权限取值非法")
			}
			return apierr.Internal(err)
		}
		out = &MemberView{
			UserID: u.ID, Username: u.Username, DisplayName: u.DisplayName,
			Permission: permission,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveMember 移除成员(4.3:仅 manager;不能移除 owner)。
//
// **即时生效**:移除后下一个请求就会因为 `space_members` 里查不到而拿到
// 410 + space_revoked,不需要等 JWT 过期,也不需要任何缓存失效动作。
func (s *Service) RemoveMember(ctx context.Context, actorID, spaceID, targetUserID string) error {
	if actorID == targetUserID {
		return apierr.Forbidden("不能移除自己(如需退出请走退出空间)")
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sp, err := s.Spaces.GetByID(ctx, tx, spaceID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在或已被解散")
			}
			return apierr.Internal(err)
		}
		if sp.IsPersonal() {
			return apierr.Forbidden("个人空间不支持移除成员")
		}
		if err := s.requireManager(ctx, tx, sp.ID, actorID); err != nil {
			return err
		}
		if targetUserID == sp.OwnerID {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"不能移除空间所有者(如需解散请走解散空间)")
		}
		if err := s.Spaces.RemoveMember(ctx, tx, sp.ID, targetUserID); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("该用户不是本空间成员")
			}
			return apierr.Internal(err)
		}
		return nil
	})
}

// Leave 主动退出团队空间(4.3:owner 未转让前退出 → 409)。
//
// 为什么 owner 不能直接退出:团队空间必须有主,否则会成为无人能管的孤儿空间
// (改配额、加成员、解散都需要 owner)。409 让客户端明确提示"请先转让"。
func (s *Service) Leave(ctx context.Context, userID, spaceID string) error {
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sp, err := s.Spaces.GetByID(ctx, tx, spaceID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在或已被解散")
			}
			return apierr.Internal(err)
		}
		if sp.IsPersonal() {
			return apierr.Forbidden("个人空间不能退出")
		}
		if sp.OwnerID == userID {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"你是该空间的所有者,请先转让空间再退出")
		}
		if err := s.Spaces.RemoveMember(ctx, tx, sp.ID, userID); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("你不是该空间成员")
			}
			return apierr.Internal(err)
		}
		return nil
	})
}

// Transfer 转让团队空间所有权(仅 owner)。
//
// 顺序很重要:**先把新 owner 写成 manager 成员,再改 owner_id**。
// 反过来的话,若中途失败会得到一个"owner 不在成员表里"的空间,
// 新 owner 的判权会走到 `sp.OwnerID == userID` 分支而侥幸能用,
// 但一旦 owner_id 再变就彻底失去访问 —— 数据处于难以恢复的中间态。
func (s *Service) Transfer(ctx context.Context, actorID, spaceID, newOwnerID string) error {
	if actorID == newOwnerID {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "不能转让给自己")
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sp, err := s.Spaces.GetByID(ctx, tx, spaceID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在或已被解散")
			}
			return apierr.Internal(err)
		}
		if sp.IsPersonal() {
			return apierr.Forbidden("个人空间不能转让")
		}
		if sp.OwnerID != actorID {
			return apierr.Forbidden("只有空间所有者可以转让空间")
		}
		if _, err := s.Users.GetByID(ctx, tx, newOwnerID); err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.NotFound("目标用户不存在")
			}
			return apierr.Internal(err)
		}
		if err := s.Spaces.UpsertMember(ctx, tx, sp.ID, newOwnerID, model.PermManager); err != nil {
			return apierr.Internal(err)
		}
		if err := s.Spaces.TransferOwner(ctx, tx, sp.ID, newOwnerID); err != nil {
			return apierr.Internal(err)
		}
		// 原 owner 变成普通 manager 成员(不能继续靠 owner_id 免判权)
		if err := s.Spaces.UpsertMember(ctx, tx, sp.ID, actorID, model.PermManager); err != nil {
			return apierr.Internal(err)
		}
		return nil
	})
}

// Dissolve 解散团队空间(仅 owner)。
//
// 前置检查:空间内**除根目录外**必须没有任何文件行,且根目录本身必须没有子项。
// 最后删除时要把根目录一并删掉 —— `files.space_id` 是 RESTRICT,
// 留着根目录会让 `DELETE FROM spaces` 撞外键(表现为 500)。
//
// 为什么要求"先清空"而不是级联删:删掉整个空间的内容是"删除影响面"极大的
// 不可逆操作,不该由一次点击静默完成;拆成"先清空 → 再解散"两步,
// 让用户在第一步就通过影响面预检(6.11)看到自己删了什么。
func (s *Service) Dissolve(ctx context.Context, actorID, spaceID string) error {
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sp, err := s.Spaces.GetByID(ctx, tx, spaceID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在或已被解散")
			}
			return apierr.Internal(err)
		}
		if sp.IsPersonal() {
			return apierr.Forbidden("个人空间不能解散")
		}
		if sp.OwnerID != actorID {
			return apierr.Forbidden("只有空间所有者可以解散空间")
		}
		// 除根目录外的文件数(根目录即将随空间一起删除,不计入)
		var files int64
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM files WHERE space_id = $1 AND parent_id IS NOT NULL`, spaceID).Scan(&files); err != nil {
			return apierr.Internal(err)
		}
		if files > 0 {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"空间内仍有 %d 个条目,请先清空再解散", files)
		}
		// 根目录之上不允许有子项(理论上上面已覆盖,这里是防御性复查:
		// 数据异常时若根目录下挂了子项,删除根会留下孤儿行)
		var children int64
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM files c JOIN files r ON r.id = c.parent_id
			  WHERE r.space_id = $1 AND r.parent_id IS NULL`, spaceID).Scan(&children); err != nil {
			return apierr.Internal(err)
		}
		if children > 0 {
			return apierr.Conflict(apierr.CodeInvalidArgument,
				"空间根目录下仍有 %d 个条目,请先清空", children)
		}
		// 上传任务行必须先清理:`uploads.parent_id/space_id` 都是 ON DELETE
		// RESTRICT,而任务行从不清扫(定稿/取消只改 state),不清理就会让
		// "这个空间曾经有人传过文件"变成 500(实测,且空间从此永远删不掉)。
		// 顺序:先回退 reserved 的预留额度(否则删行后额度无处可退),
		// 再删任务行 —— 见 repo.UploadRepo 上方的完整说明。
		uploads := repo.UploadRepo{}
		if _, rerr := uploads.ReleaseReservedOfSpace(ctx, tx, spaceID); rerr != nil {
			return apierr.Internal(fmt.Errorf("解散前回退上传预留失败: %w", rerr))
		}
		if _, derr := uploads.DeleteTasksOfSpace(ctx, tx, spaceID); derr != nil {
			return apierr.Internal(fmt.Errorf("解散前清理上传任务失败: %w", derr))
		}
		// 删根目录(必须,否则删空间撞 files_space_id_fkey RESTRICT)
		if _, err := tx.Exec(ctx,
			`DELETE FROM files WHERE space_id = $1 AND parent_id IS NULL`, spaceID); err != nil {
			if repo.IsForeignKeyViolation(err) {
				// 兜底:不在上面那几类里的引用 → 给一个能读的 409,而不是 500
				return apierr.Conflict(apierr.CodeInvalidArgument,
					"空间根目录仍被其它记录引用,无法解散")
			}
			return apierr.Internal(err)
		}
		if err := s.Spaces.DeleteSpace(ctx, tx, spaceID); err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return apierr.Conflict(apierr.CodeInvalidArgument, "空间内仍有数据,无法解散")
			}
			return apierr.Internal(err)
		}
		return nil
	})
}

// ---- 管理员治理(S3-07)----

// SetFrozen 冻结/解冻空间(仅管理员;由 handler 的角色中间件保证)。
//
// 冻结的语义与"被移出"一致:成员的一切读写都返回 space_revoked,
// 客户端据此**保留本地文件**并等待恢复(7.2 P1-2)。
func (s *Service) SetFrozen(ctx context.Context, spaceID string, frozen bool) (*model.Space, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("spacesvc: DB 未装配"))
	}
	var out *model.Space
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sp, err := s.Spaces.SetFrozen(ctx, tx, spaceID, frozen)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在")
			}
			return apierr.Internal(err)
		}
		out = sp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 判权辅助 ----

// requireVisible 要求调用者对该空间至少是 reader,且空间未被冻结。
//
// **冻结检查必须在这里**,而不是只在"写操作"里:冻结的语义是
// "该空间对成员整体封锁"(管理员介入中),此时连成员列表都不该暴露。
// 只在写路径检查会留下"冻结了但成员照样能读、能列成员"的漏洞 ——
// 而冻结恰恰发生在需要**止血**的场景(疑似数据外泄/合规审查),
// 读不封锁就等于没冻。
func (s *Service) requireVisible(ctx context.Context, spaceID, userID string) error {
	sp, err := s.Spaces.GetByID(ctx, s.DB, spaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.SpaceGone("空间不存在或已被解散")
		}
		return apierr.Internal(err)
	}
	if sp.Frozen {
		return apierr.SpaceRevoked("空间已被管理员冻结")
	}
	m, err := s.Spaces.GetMembership(ctx, s.DB, sp.ID, userID)
	if err != nil {
		return apierr.Internal(err)
	}
	if m == nil {
		return apierr.SpaceRevoked("你没有该空间的访问权限")
	}
	return nil
}

// requireManager 要求调用者在该空间是 manager,且空间未被冻结。
func (s *Service) requireManager(ctx context.Context, q repo.Querier, spaceID, userID string) error {
	sp, err := s.Spaces.GetByID(ctx, q, spaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.SpaceGone("空间不存在或已被解散")
		}
		return apierr.Internal(err)
	}
	if sp.Frozen {
		return apierr.SpaceRevoked("空间已被管理员冻结")
	}
	m, err := s.Spaces.GetMembership(ctx, q, spaceID, userID)
	if err != nil {
		return apierr.Internal(err)
	}
	if m == nil {
		return apierr.SpaceRevoked("你没有该空间的访问权限")
	}
	if m.Permission != model.PermManager {
		return apierr.Forbidden("只有空间管理者可以执行该操作")
	}
	return nil
}

// CanWrite 判断用户能否在空间写入(reader 不行)。
//
// 导出是给 uploadsvc / finalize 复用的唯一判权入口 —— 避免各处各写一套
// "permission != reader" 的判断,那正是权限漏洞的常见来源。
func CanWrite(m *repo.Membership) bool {
	if m == nil {
		return false
	}
	return m.Permission == model.PermManager || m.Permission == model.PermEditor
}

// NormalizePermission 归一权限字符串(大小写与空白容错)。
func NormalizePermission(p string) string {
	return strings.ToLower(strings.TrimSpace(p))
}

// 保持 time 引用(成员视图的 JoinedAt 在 repo 层,这里预留格式化入口)。
var _ = time.RFC3339
