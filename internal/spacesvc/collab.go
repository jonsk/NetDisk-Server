package spacesvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 团队空间协作(FE-W-11 / 4.3):创建 / 查询 / 解散 / 转让 / 邀请。
//
// 这一组动作的入口在**桌面客户端与工作台 H5**,不在 Web 管理后台
// (4.3 决策:后台只做全局治理)。H5 与桌面端调的是**同一组接口**,
// 所以语义只在这一层写一次。
//
// 三条纪律:
//   - **创建者即 owner 且必须是 manager 成员**:只写 owner_id 会让
//     "owner 不在成员表"成为常态,而某些判权路径恰好靠 owner_id 兜住 ——
//     于是问题只会在转让/移除成员时才暴露;
//   - **解散仅 owner**(且要显式确认):它是硬删(4.5 无回收站),
//     权限判定必须比"是成员"更严;
//   - **退出前必须转让**:owner 直接退出会让空间失去所有者(见 Leave 的 409)。

// CreateInput 是建团队空间入参。
type CreateInput struct {
	ActorID string
	Name    string
	// QuotaBytes 0 = 不限制(默认;管理员可在后台按空间调整)
	QuotaBytes int64
}

// Create 创建团队空间:一次事务里建 groups 行 + spaces 行 + owner 的 manager 成员。
//
// 为什么需要 `groups` 行:`spaces` 上有约束
// `kind='team' ⇒ group_id IS NOT NULL`,团队空间在数据模型上就是"某个组的盘"。
// 三件事必须在同一事务:只建了 space 没建成员 → 这个空间只有 owner 能靠
// owner_id 访问;只建了 group 没建 space → 留下孤儿组。
func (s *Service) Create(ctx context.Context, in CreateInput) (*model.Space, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("spacesvc: DB 未装配"))
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "空间名称不能为空")
	}
	var out *model.Space
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		var groupID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO groups (name, owner_id) VALUES ($1, $2) RETURNING id::text`,
			name, in.ActorID).Scan(&groupID); err != nil {
			return apierr.Internal(fmt.Errorf("创建团队失败: %w", err))
		}
		sp, cerr := s.Spaces.CreateTeam(ctx, tx, repo.CreateSpaceInput{
			GroupID: groupID, OwnerID: in.ActorID, Name: name, QuotaBytes: in.QuotaBytes,
		})
		if cerr != nil {
			return apierr.Internal(cerr)
		}
		// 根目录:与个人空间一致(每个空间恰有一行 parent_id IS NULL,4.3)
		if _, rerr := tx.Exec(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, depth)
VALUES ($1, NULL, $2, '根目录', true, 0)`, sp.ID, in.ActorID); rerr != nil {
			return apierr.Internal(fmt.Errorf("创建根目录失败: %w", rerr))
		}
		if merr := s.Spaces.UpsertMember(ctx, tx, sp.ID, in.ActorID, model.PermManager); merr != nil {
			return apierr.Internal(merr)
		}
		out = sp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListMine 返回"我能看到的空间"(自己的 + 我参与的),并带上我在其中的角色。
//
// 为什么不是 `/me` 那种只回个人空间:协作界面第一屏就是"我的团队空间列表",
// 而它必须是**服务端按权限过滤**的结果 —— 前端过滤等于把别人的空间列表发给了客户端。
func (s *Service) ListMine(ctx context.Context, userID string) ([]SpaceView, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("spacesvc: DB 未装配"))
	}
	spaces, err := s.Spaces.ListVisible(ctx, s.DB, userID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	out := make([]SpaceView, 0, len(spaces))
	for _, sp := range spaces {
		v := SpaceView{Space: sp, MyPermission: ""}
		if sp.OwnerID == userID {
			// owner 的权限就是 manager(不必再查成员表)
			v.MyPermission = model.PermManager
			v.IsOwner = true
		} else if m, merr := s.Spaces.GetMembership(ctx, s.DB, sp.ID, userID); merr == nil && m != nil {
			v.MyPermission = m.Permission
		}
		out = append(out, v)
	}
	return out, nil
}

// SpaceView 是协作界面需要的空间视图(空间 + 我在其中的角色)。
type SpaceView struct {
	Space *model.Space
	// MyPermission 空表示"我是 owner 但成员表里没有行"(理论上不会,仅防御)
	MyPermission string
	IsOwner      bool
}

// Transfer 转让 owner(仅 owner;顺序见原实现注释)。
func (s *Service) TransferOwnership(ctx context.Context, actorID, spaceID, newOwnerID string) error {
	return s.Transfer(ctx, actorID, spaceID, newOwnerID)
}

// DissolveSpace 解散(仅 owner,硬删)。
func (s *Service) DissolveSpace(ctx context.Context, actorID, spaceID string) error {
	return s.Dissolve(ctx, actorID, spaceID)
}
