package filesvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 建目录的 REST 入口(契约 `POST /api/v1/files/dirs`)。
//
// 为什么需要它:目录创建原来**只有 WebDAV MKCOL** 一个入口,而 MKCOL 走
// Basic 账密换票(ADR-6)。桌面客户端按设计只持有 Bearer 令牌、没有账密 ——
// 实测报错是 `WebDAV 认证失败: 请提供 Basic 凭据`,于是客户端**建不了目录**,
// 连带"任何要放进子目录的文件"都传不上去(此前所有同步用例都只在同步根放
// 文件,所以没暴露)。这里把同一件事补成令牌可用的 REST 写操作。
//
// 刻意**不**顺手返回"已存在的同名目录":严格新建才能让客户端区分
// "我刚建了它"与"它本来就存在" —— 同步逻辑要的正是这个区别。

// CreateDirInput 建目录入参。
type CreateDirInput struct {
	UserID string
	// SpaceID / ParentID 允许为空:分别落到调用者的个人空间与空间根目录
	// (与 List 的两个参数同义)。
	SpaceID  string
	ParentID string
	Name     string
}

// CreateDir 在 (space, parent) 下严格新建一个目录;同名占用返回 409 name_conflict。
func (s *Service) CreateDir(ctx context.Context, in CreateDirInput) (*EntryView, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	name := namepolicy.Normalize(strings.TrimSpace(in.Name))
	if name == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "目录名不能为空")
	}
	if v, ok := s.Name.Validate(name); !ok {
		return nil, nameViolationErr(v)
	}

	// resolve 一次做掉四件事:空间缺省=个人空间、父缺省=空间根、父必须同空间
	// 且是目录、祖先链上有目录级任务时 409(与移动/删除同一道闸)。
	spaceID, parentID, err := s.resolve(ctx, in.UserID, in.SpaceID, in.ParentID)
	if err != nil {
		return nil, err
	}
	// resolve 只校验**读**权限;建目录要写权限(4.3:reader 不能写)。
	// 少了这一句,只读成员就能往别人的空间里塞目录 —— 且 WebDAV 那条路也有同样的闸
	// (`webdavfs` 自己调 CheckWritable),两条路必须一致。
	if err := s.checkWrite(ctx, spaceID, in.UserID); err != nil {
		return nil, err
	}
	parent, err := s.Files.GetByID(ctx, s.DB, parentID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("目录不存在")
		}
		return nil, apierr.Internal(err)
	}

	var created *model.File
	if err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		// 写之前查重名:唯一约束违反会**终止整个事务**,失败后再查是无效的
		// (SQLSTATE 25P02,见 precheckNameConflict 的说明)。这里不用
		// precheckNameConflict 是因为冲突响应体要带**已有条目**的版本/etag ——
		// 客户端据此能直接展示"远端已有同名条目"而不必再查一次。
		sibling, serr := s.Files.GetChildByName(ctx, tx, spaceID, parentID, name)
		switch {
		case serr == nil:
			return conflictOf(sibling, ReasonNameConflict,
				fmt.Sprintf("同目录下已存在同名条目: %s", name)).Err()
		case errors.Is(serr, repo.ErrNotFound):
			// 正常:这个名字没人占
		default:
			return apierr.Internal(serr)
		}
		f, cerr := s.Files.CreateDir(ctx, tx, repo.CreateDirInput{
			SpaceID: spaceID, ParentID: parentID, OwnerID: in.UserID,
			Name: name, Depth: parent.Depth + 1,
		})
		if cerr != nil {
			// 查完之后才有人插了同名 → 唯一索引兜底;此时拿不到冲突行(事务已终止),
			// 所以 cur 传 nil:响应里没有 server_version 但**不会误放行**,
			// 客户端刷新后重试即可。
			return s.mapWriteErr(nil, cerr, true, name)
		}
		// 变更流:新建写 created(带变更后的位置与名字,其它客户端据此更新本地树)
		if ferr := s.feed(ctx, tx, syncfeed.Event{
			SpaceID: f.SpaceID, FileID: f.ID, Kind: model.FeedCreated,
			Version: f.Version, ParentID: f.ParentID, Name: f.Name,
		}); ferr != nil {
			return ferr
		}
		created = f
		return nil
	}); err != nil {
		return nil, err
	}
	v := View(created)
	return &v, nil
}
