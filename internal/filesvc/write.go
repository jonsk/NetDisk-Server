package filesvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 文件写操作:改名 / 移动 / 删除(BE-S6-01 / BE-S6-02)。
//
// 本文件把 6.6 并发写矩阵落到代码上。三条纪律:
//
//  1. **冲突裁决以服务端 version 为准**,绝不用 mtime —— 时钟偏差会让
//     "谁后改"不可判定(架构 7.7 A-6/风险 #6)。
//  2. 冲突响应体必须带全 `{server_version, server_etag, server_updated_at, reason}`:
//     客户端据此**无需再发一次请求**就能展示"远端已被改成 X"并让用户选择
//     (7.2 的 Conflict 态)。少了任何一个字段,客户端都只能再查一次,而那次查询
//     看到的可能又是新版本(用户永远追不上)。
//  3. 同目录目标名被占 → **409 且 reason 可区分**(让客户端能给出不同提示:
//     "版本冲突,请刷新"vs"该名字已被占用")。

// 冲突原因码(客户端按 reason 分支,不解析文案)
const (
	ReasonVersionConflict = "version_conflict"
	ReasonNameConflict    = "name_conflict"
	ReasonIsDirectory     = "target_is_directory"
	ReasonRootProtected   = "root_protected"
	// ReasonDirOpInProgress:该条目所在子树正在跑异步目录任务(BE-S7-02 / 6.11),
	// 客户端应提示"稍后再试"并刷新列表,而不是重试(重试必然还是 409)。
	ReasonDirOpInProgress = "dir_op_in_progress"
)

// ConflictError 是 409 的统一形态,携带服务端当前状态。
//
// 之所以单独一个类型而不是拼 map:字段名是**客户端契约**,
// 用结构体可以保证四处出口(改名/移动/删除/base_version 校验)永远一致。
type ConflictError struct {
	ServerVersion   int64
	ServerETag      string
	ServerUpdatedAt string
	Reason          string
	Message         string
}

// Err 把 ConflictError 转成 apierr.Error(带结构化 details)。
func (c *ConflictError) Err() *apierr.Error {
	e := apierr.Conflict(apierr.CodeVersionConflict, "%s", c.Message)
	e.WithDetail("reason", c.Reason)
	e.WithDetail("server_version", c.ServerVersion)
	if c.ServerETag != "" {
		e.WithDetail("server_etag", c.ServerETag)
	}
	if c.ServerUpdatedAt != "" {
		e.WithDetail("server_updated_at", c.ServerUpdatedAt)
	}
	// 同目录重名时业务码要区分:`name_conflict` 让客户端提示"换个名字"，
	// 而 `version_conflict` 提示"刷新后重试" —— 两者的用户动作完全不同
	if c.Reason == ReasonNameConflict {
		e.Code = apierr.CodeNameConflict
	}
	return e
}

// conflictOf 用当前行构造 ConflictError。
func conflictOf(f *model.File, reason, msg string) *ConflictError {
	c := &ConflictError{Reason: reason, Message: msg}
	if f != nil {
		c.ServerVersion = f.Version
		c.ServerETag = QuoteETag(f.Etag)
		c.ServerUpdatedAt = f.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	return c
}

// RenameInput 改名为新名字(不换目录)。
type RenameInput struct {
	UserID  string
	FileID  string
	NewName string
	// BaseVersion 是客户端持有的版本;**0 表示客户端未做乐观锁**(允许,但会
	// 与并发修改竞速)。带上时严格比对。
	BaseVersion int64
}

// MoveInput 移动到新父目录(可同时改名)。
type MoveInput struct {
	UserID      string
	FileID      string
	NewParentID string
	// NewName 为空表示保持原名
	NewName     string
	BaseVersion int64
}

// Rename 改名(携带 base_version 的乐观锁)。
func (s *Service) Rename(ctx context.Context, in RenameInput) (*EntryView, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	name := namepolicy.Normalize(strings.TrimSpace(in.NewName))
	if name == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "新名字不能为空")
	}
	if v, ok := s.Name.Validate(name); !ok {
		return nil, nameViolationErr(v)
	}

	cur, err := s.Files.GetByID(ctx, s.DB, in.FileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkWrite(ctx, cur.SpaceID, in.UserID); err != nil {
		return nil, err
	}
	if err := s.guardDirOp(ctx, s.DB, cur.ID); err != nil {
		return nil, err
	}
	if cur.ParentID == "" {
		return nil, conflictOf(cur, ReasonRootProtected, "空间根目录不能改名").Err()
	}
	// 完全相同(归一后逐字节相同)→ 真正的空操作,直接返回当前状态。
	//
	// 注意这里**不能用大小写不敏感比较**:"README.md" → "readme.md" 是用户
	// 修正文件名的常见操作,必须真的写库;而唯一索引是 lower(name),
	// 所以这种改名不会与"同目录重名"冲突。
	if namepolicy.Normalize(cur.Name) == name {
		v := View(cur)
		return &v, nil
	}
	if err := s.checkVersion(cur, in.BaseVersion, "改名"); err != nil {
		return nil, err
	}
	// 未带 base_version(0)时以"刚读到的当前版本"作为期望值:
	// repo.Rename 的乐观锁条件需要具体版本号,传 0 会永远匹配 0 行并误报冲突。
	// 传快照版本 —— 语义上等价于"不带乐观锁,但不与刚读到的状态自相矛盾"。
	expect := in.BaseVersion
	if expect == 0 {
		expect = cur.Version
	}

	if err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		// **写之前**查重名:唯一约束违反会终止事务,失败后再查是无效的
		dup, derr := s.precheckNameConflict(ctx, tx, cur.SpaceID, cur.ParentID, name, cur.ID)
		if derr != nil {
			return derr
		}
		if dup {
			return conflictOf(cur, ReasonNameConflict,
				fmt.Sprintf("同目录下已存在同名条目: %s", name)).Err()
		}
		target, rerr := s.Files.Rename(ctx, tx, in.FileID, name, expect)
		if rerr != nil {
			return s.mapWriteErr(cur, rerr, dup, name)
		}
		// 变更流:改名写 updated(带**变更后**的名字,客户端据此直接更新本地树)
		if ferr := s.feed(ctx, tx, syncfeed.Event{
			SpaceID: target.SpaceID, FileID: target.ID, Kind: model.FeedUpdated,
			Version: target.Version, ParentID: target.ParentID, Name: target.Name,
		}); ferr != nil {
			return ferr
		}
		cur = target
		return nil
	}); err != nil {
		return nil, err
	}
	v := View(cur)
	return &v, nil
}

// MoveResult 是移动的结果。
//
// 同步走通时 Entry 非空(与旧行为一致,前端可立刻更新);超阈值转异步时
// TaskID 非空、Entry 为 nil —— 用同一个结构体而不是两个接口,客户端只需
// 判断 `async` 一个字段,不必为"大目录"单独写一条调用路径。
type MoveResult struct {
	Entry  *EntryView `json:"entry,omitempty"`
	TaskID string     `json:"task_id,omitempty"`
	// Async 为 true 表示已转异步任务,客户端应轮询 `/api/v1/tasks/{id}`
	Async bool `json:"async"`
}

// Move 移动目录/文件到新父(可同时改名)。
//
// 三条必须拦住的路径:
//  1. **移到自己或自己的后代下** → 造出数据环,子树在树里不可达
//  2. **换空间**:本项目空间即权限边界,移动不跨空间(跨空间请走共享/COPY)
//  3. 目标目录被占(移动而不同名)→ 409 with reason
//
// 深度级联:移动目录会让整棵子树的 depth 变化 —— 用一条递归 CTE 更新
// (不是逐层循环,否则大目录要几十次往返);整棵子树的 version 也 +1(6.11),
// 因为后代的**完整路径**变了(见 repo.BumpSubtreeVersion 的说明)。
//
// 子树行数超过 SyncMaxRows 时转异步任务(BE-S7-02 / 6.11):一条 10 万行的
// 事务会持锁到提交,期间整个空间卡住、连接被占满;改为返回 task_id 供轮询。
func (s *Service) Move(ctx context.Context, in MoveInput) (*MoveResult, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	cur, err := s.Files.GetByID(ctx, s.DB, in.FileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkWrite(ctx, cur.SpaceID, in.UserID); err != nil {
		return nil, err
	}
	if err := s.guardDirOp(ctx, s.DB, cur.ID); err != nil {
		return nil, err
	}
	if cur.ParentID == "" {
		return nil, conflictOf(cur, ReasonRootProtected, "空间根目录不能移动").Err()
	}
	if in.NewParentID == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少目标目录")
	}
	if in.NewParentID == in.FileID {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "不能把条目移动到它自己下面")
	}

	newName := cur.Name
	if n := namepolicy.Normalize(strings.TrimSpace(in.NewName)); n != "" {
		if v, ok := s.Name.Validate(n); !ok {
			return nil, nameViolationErr(v)
		}
		newName = n
	}
	if err := s.checkVersion(cur, in.BaseVersion, "移动"); err != nil {
		return nil, err
	}

	rows, err := s.subtreeRows(ctx, cur)
	if err != nil {
		return nil, err
	}
	if s.shouldRunAsync(cur, rows) {
		return s.enqueueMove(ctx, cur, in, newName)
	}

	moved, err := s.moveSubtree(ctx, cur, in, newName)
	if err != nil {
		return nil, err
	}
	v := View(moved)
	return &MoveResult{Entry: &v}, nil
}

// subtreeRows 返回子树的行数(文件 + 目录),即 6.11 的阈值口径。
//
// 与 `/subtree-stats` 预检共用同一份统计(repo.SubtreeStatsOf):各写一份 SQL 的话,
// 用户会看到"预检说 900 个文件,却走了异步"这种自相矛盾的行为。
func (s *Service) subtreeRows(ctx context.Context, cur *model.File) (int64, error) {
	if !cur.IsDir {
		return 1, nil
	}
	st, err := s.Files.SubtreeStatsOf(ctx, s.DB, cur.ID)
	if err != nil {
		return 0, apierr.Internal(err)
	}
	return st.FileCount + st.DirCount, nil
}

// shouldRunAsync 判定是否转异步。
//
// 未装配队列时**保持同步**并记 Warn:功能正确,只是大树会长时间持锁。
// 见 Service.Queue 的说明(宁可慢,也不要"服务端少装配了一个组件"变成
// 用户可见的失败 —— 前者是性能问题,后者是可用性问题)。
func (s *Service) shouldRunAsync(cur *model.File, rows int64) bool {
	if !cur.IsDir || rows <= int64(s.syncMaxRows()) {
		return false
	}
	if s.Queue == nil {
		s.log().Warn("filesvc: 子树超过同步阈值但目录任务队列未装配,退化为同步事务",
			"file_id", cur.ID, "rows", rows, "threshold", s.syncMaxRows())
		return false
	}
	return true
}

// enqueueMove 入队异步移动任务并返回 task_id。
//
// 入队前先做一次**快速校验**:环、跨空间、目标非目录、目标重名这些错误现在
// 就能判定,直接回给用户,比"让用户去轮询一个注定失败的任务"好得多
// (失败的可见性差得多:用户看到的是"卡住",而不是"不能这么移")。
// 权威校验仍在 worker 的事务里重做一遍 —— 共用 checkMoveTarget,不会漂移。
func (s *Service) enqueueMove(ctx context.Context, cur *model.File, in MoveInput, newName string) (*MoveResult, error) {
	if _, err := s.checkMoveTarget(ctx, s.DB, cur, in.NewParentID, newName); err != nil {
		return nil, err
	}
	t, err := s.Queue.Create(ctx, &dirops.Task{
		Kind: dirops.KindMove, SpaceID: cur.SpaceID, FileID: cur.ID, UserID: in.UserID,
		NewParentID: in.NewParentID, NewName: newName,
		RootVersion: cur.Version, RootParentID: cur.ParentID, RootName: cur.Name,
		IsDir: cur.IsDir,
	})
	if err != nil {
		if errors.Is(err, dirops.ErrAlreadyInFlight) {
			return nil, conflictOf(cur, ReasonDirOpInProgress,
				"该目录上已有进行中的目录操作").Err()
		}
		return nil, apierr.Internal(err)
	}
	return &MoveResult{TaskID: t.ID, Async: true}, nil
}

// checkMoveTarget 校验移动目标(可在池或事务上执行),返回目标深度。
//
// 抽成一个函数并被**三处共用**:同步路径、异步入队前校验、worker 执行体。
// 三份实现迟早会漂移,而漂移的后果正是"入队时说可以、执行时失败"。
func (s *Service) checkMoveTarget(ctx context.Context, q repo.Querier, cur *model.File, newParentID, newName string) (int, error) {
	parent, err := s.Files.GetByID(ctx, q, newParentID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return 0, apierr.NotFound("目标目录不存在")
		}
		return 0, apierr.Internal(err)
	}
	if !parent.IsDir {
		return 0, apierr.BadRequest(apierr.CodeInvalidArgument, "目标不是目录")
	}
	if parent.SpaceID != cur.SpaceID {
		// 空间即权限边界:跨空间移动意味着"未经对方空间成员同意把内容搬进去"
		return 0, apierr.Forbidden("不能跨空间移动(请使用共享或复制)")
	}
	// R-11:移动后的深度不能超限
	if v, ok := s.Name.CheckPath("", parent.Depth+1); !ok {
		return 0, nameViolationErr(v)
	}
	// 环检测:目标是自己的后代(含自己)即拒绝。用递归 CTE 一条 SQL 判定,
	// 不逐层查 —— 目录很深时逐层查会变成 N 次往返。
	var cycle bool
	if qerr := q.QueryRow(ctx, `
WITH RECURSIVE sub AS (
    SELECT id FROM files WHERE id = $1
    UNION ALL
    SELECT f.id FROM files f JOIN sub ON f.parent_id = sub.id
)
SELECT EXISTS (SELECT 1 FROM sub WHERE id = $2)`, cur.ID, newParentID).Scan(&cycle); qerr != nil {
		return 0, apierr.Internal(qerr)
	}
	if cycle {
		return 0, apierr.BadRequest(apierr.CodeInvalidArgument,
			"不能移动到自己的下级目录(会造成目录成环)")
	}
	// 目标目录下同名被占 → 409,reason 区分(客户端提示"换个名字"而非"刷新")
	if existing, gerr := s.Files.GetChildByName(ctx, q, cur.SpaceID, newParentID, newName); gerr == nil {
		if existing.ID != cur.ID {
			return 0, conflictOf(existing, ReasonNameConflict,
				fmt.Sprintf("目标目录下已存在同名条目: %s", newName)).Err()
		}
	} else if !errors.Is(gerr, repo.ErrNotFound) {
		return 0, apierr.Internal(gerr)
	}
	return parent.Depth + 1, nil
}

// moveSubtree 是一次**实际执行**的子树移动(根 + 深度级联 + 全子树 version+1
// + 一条 moved 聚合事件),同步路径与异步 worker 共用。
func (s *Service) moveSubtree(ctx context.Context, cur *model.File, in MoveInput, newName string) (*model.File, error) {
	var moved *model.File
	if err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		newDepth, err := s.checkMoveTarget(ctx, tx, cur, in.NewParentID, newName)
		if err != nil {
			return err
		}
		// 写之前查重名(失败后再查无效)
		dup, derr := s.precheckNameConflict(ctx, tx, cur.SpaceID, in.NewParentID, newName, cur.ID)
		if derr != nil {
			return derr
		}
		target, merr := s.Files.Move(ctx, tx, cur.ID, in.NewParentID, newName, newDepth)
		if merr != nil {
			return s.mapWriteErr(cur, merr, dup, newName)
		}
		if cur.IsDir {
			// 整棵子树的 depth 级联更新(一条递归 CTE)
			if rerr := s.Files.RecascadeDepth(ctx, tx, cur.ID, target.Depth); rerr != nil {
				return apierr.Internal(rerr)
			}
			// 6.11:整棵子树 version+1(后代的完整路径变了)
			if _, berr := s.Files.BumpSubtreeVersion(ctx, tx, cur.ID); berr != nil {
				return apierr.Internal(berr)
			}
		}
		// 变更流:移动写 moved 并带 **old_parent_id** —— 客户端靠它把条目从旧目录
		// 摘下来、挂到新目录;少了这一列客户端只能整棵重扫(那正是本机制要消灭的)。
		// 整棵子树只写**根的一行聚合事件**(6.11):逐行写会让"移动一个 10 万文件的
		// 目录"产生 10 万条变更,而客户端收到根那一条就会整棵重挂。
		if ferr := s.feed(ctx, tx, syncfeed.Event{
			SpaceID: target.SpaceID, FileID: target.ID, Kind: model.FeedMoved,
			Version: target.Version, ParentID: target.ParentID, Name: target.Name,
			OldParentID: cur.ParentID,
		}); ferr != nil {
			return ferr
		}
		moved = target
		return nil
	}); err != nil {
		return nil, err
	}
	return moved, nil
}

// DeleteResult 是删除结果。
type DeleteResult struct {
	// DeletedFiles / DeletedDirs 是被硬删的行数(目录删除含整棵子树)
	DeletedFiles int64 `json:"deleted_files"`
	DeletedDirs  int64 `json:"deleted_dirs"`
	// ReleasedObjects 是**因引用归零而转入延迟删除**的对象数(回收的物理对象数)。
	//
	// 注意它**不是**"被递减了引用计数的对象数":删掉 5 个引用同一内容的文件时,
	// 只有删最后一个才会让对象归零,因此 ReleasedObjects 与 DistinctObjects
	// 在存在共享内容时**必然不同**。客户端拿它做"这次释放了几个物理对象"的提示。
	ReleasedObjects int64 `json:"released_objects"`
	// DistinctObjects 是子树内**不同物理对象**的个数(去重后)。
	// 它与预检的 `object_refs` **同口径**,两边可直接比对;
	// 而"引用递减次数"在多个文件共享同一内容时会大于它
	// (曾因此让"预检与实际一致"的断言误报)。
	DistinctObjects int64 `json:"distinct_objects"`
	// FreedBytes 是释放的逻辑字节数(按 files.size 累计)
	FreedBytes int64 `json:"freed_bytes"`
	// RefundedReservedBytes 是随子树一起被清理的上传任务所**退回的预留额度**
	// (uploads.state='reserved' 的 declared_size 之和)。
	//
	// 它与 FreedBytes 是两笔不同的账:前者是"还没传完的任务占着的预留",
	// 后者是"已经落盘的文件占着的实际容量";两者相加才是这次删除真正让空间
	// 变空的字节数(曾经只有 FreedBytes,预留那部分会被静默漏掉)。
	RefundedReservedBytes int64 `json:"refunded_reserved_bytes"`
	// TaskID 非空表示超阈值已转异步任务(BE-S7-02):上面那些计数此刻都是 0,
	// 真正的结果要去 `/api/v1/tasks/{id}` 取。
	TaskID string `json:"task_id,omitempty"`
	// Async 与 TaskID 同义(留给客户端一个显式布尔,避免判断"字段是否为空")
	Async bool `json:"async"`
}

// Delete 硬删(无回收站,4.5):files 行与引用计数**同事务**。
//
// 引用计数处理顺序(与 finalize 的全局锁序一致:spaces → files → file_objects):
//  1. 先取出子树里全部"非目录行"的 hash 与 size(此刻行还在)
//  2. 删 files 行(整棵子树,一条递归 CTE)
//  3. 按 hash 升序逐个 ref_count-1;归零则转 pending_delete + delete_after
//     (24h 延迟窗口,由生命周期 worker 物理删除)
//  4. 释放配额(按实际删掉的字节)
//
// **按 hash 升序**是为了与其它多对象路径共享锁序,避免死锁(6.6 全局锁序)。
//
// 子树行数超过 SyncMaxRows 时转异步任务(BE-S7-02 / 6.11):一次删 10 万行
// 会在提交前一直持锁,期间整个空间不可写、连接被占满。
func (s *Service) Delete(ctx context.Context, userID, fileID string) (*DeleteResult, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	cur, err := s.Files.GetByID(ctx, s.DB, fileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkWrite(ctx, cur.SpaceID, userID); err != nil {
		return nil, err
	}
	if err := s.guardDirOp(ctx, s.DB, cur.ID); err != nil {
		return nil, err
	}
	if cur.ParentID == "" {
		// 根目录禁删(删掉整个空间的内容是"解散空间"的语义,不该由删除条目触发)
		return nil, conflictOf(cur, ReasonRootProtected, "空间根目录不能删除").Err()
	}

	rows, err := s.subtreeRows(ctx, cur)
	if err != nil {
		return nil, err
	}
	if s.shouldRunAsync(cur, rows) {
		return s.enqueueDelete(ctx, cur, userID)
	}
	return s.deleteSubtree(ctx, cur)
}

// enqueueDelete 入队异步删除任务并返回 task_id。
func (s *Service) enqueueDelete(ctx context.Context, cur *model.File, userID string) (*DeleteResult, error) {
	t, err := s.Queue.Create(ctx, &dirops.Task{
		Kind: dirops.KindDelete, SpaceID: cur.SpaceID, FileID: cur.ID, UserID: userID,
		RootVersion: cur.Version, RootParentID: cur.ParentID, RootName: cur.Name,
		IsDir: cur.IsDir,
	})
	if err != nil {
		if errors.Is(err, dirops.ErrAlreadyInFlight) {
			return nil, conflictOf(cur, ReasonDirOpInProgress,
				"该目录上已有进行中的目录操作").Err()
		}
		return nil, apierr.Internal(err)
	}
	return &DeleteResult{TaskID: t.ID, Async: true}, nil
}

// deleteSubtree 是一次**实际执行**的子树删除,同步路径与异步 worker 共用。
//
// 幂等:根行已不存在时视为"已经删过了"并返回成功 —— worker 的租约超时重试
// (任务执行到一半进程崩溃)必然走到这条路径,报错只会让任务永远失败。
func (s *Service) deleteSubtree(ctx context.Context, cur *model.File) (*DeleteResult, error) {
	fileID := cur.ID
	res := &DeleteResult{}
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		// 0) 行已不在 → 视作已完成(重试幂等)
		if _, gerr := s.Files.GetByID(ctx, tx, fileID); gerr != nil {
			if errors.Is(gerr, repo.ErrNotFound) {
				return errDeleteDone
			}
			return apierr.Internal(gerr)
		}
		// 1) 取子树内所有非目录行的哈希与大小(删行前取)
		rows, qerr := tx.Query(ctx, `
WITH RECURSIVE sub AS (
    SELECT id, is_dir, size, hash_sha256 FROM files WHERE id = $1
    UNION ALL
    SELECT f.id, f.is_dir, f.size, f.hash_sha256 FROM files f JOIN sub ON f.parent_id = sub.id
)
SELECT is_dir, coalesce(hash_sha256,''), size FROM sub`, fileID)
		if qerr != nil {
			return apierr.Internal(qerr)
		}
		var refs []objRef
		for rows.Next() {
			var isDir bool
			var hash string
			var size int64
			if serr := rows.Scan(&isDir, &hash, &size); serr != nil {
				rows.Close()
				return apierr.Internal(serr)
			}
			if isDir {
				res.DeletedDirs++
				continue
			}
			res.DeletedFiles++
			res.FreedBytes += size
			if hash != "" {
				refs = append(refs, objRef{Hash: hash, Size: size})
			}
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return apierr.Internal(rerr)
		}

		// 1.5) 先清理"落点在这棵子树里"的上传任务行。
		//
		// uploads.parent_id 是 ON DELETE RESTRICT,而任务行从不清扫 —— 不清理的话
		// **任何曾经有人传过文件的目录都删不掉**(实测:DELETE 撞外键 → 500)。
		// 顺序固定为"先回退 reserved 预留额度,再删行",完整理由见 repo.UploadRepo。
		// 顺带把释放的额度记进统计,便于调用方/审计看到这次删除到底退回了多少。
		if refunded, rerr := s.Uploads.ReleaseReservedOfSubtree(ctx, tx, fileID); rerr != nil {
			return apierr.Internal(rerr)
		} else {
			res.RefundedReservedBytes = refunded
		}
		if _, terr := s.Uploads.DeleteTasksOfSubtree(ctx, tx, fileID); terr != nil {
			return apierr.Internal(terr)
		}

		// 2) 删 files 行(整棵子树,一条递归 CTE)
		if _, derr := tx.Exec(ctx, `
WITH RECURSIVE sub AS (
    SELECT id FROM files WHERE id = $1
    UNION ALL
    SELECT f.id FROM files f JOIN sub ON f.parent_id = sub.id
)
DELETE FROM files WHERE id IN (SELECT id FROM sub)`, fileID); derr != nil {
			return apierr.Internal(derr)
		}

		// 3) 引用计数:-1 并按 hash 升序(与其它多对象路径共享锁序)
		sortHashes(refs)
		// 去重后的对象数(与预检的 object_refs 同口径)
		{
			seen := map[string]struct{}{}
			for _, r := range refs {
				if _, ok := seen[r.Hash]; !ok {
					seen[r.Hash] = struct{}{}
				}
			}
			res.DistinctObjects = int64(len(seen))
		}
		for _, r := range refs {
			released, rerr := s.Files.ReleaseObjectRef(ctx, tx, r.Hash)
			if rerr != nil {
				return apierr.Internal(rerr)
			}
			if released {
				res.ReleasedObjects++
			}
		}

		// 4) 释放配额(按实际删掉的逻辑字节)
		if res.FreedBytes > 0 {
			if serr := s.Spaces.Release(ctx, tx, cur.SpaceID, res.FreedBytes); serr != nil {
				return apierr.Internal(serr)
			}
		}
		// 5) 变更流:删整棵子树只写**根的一行**(聚合事件)。
		//
		// 逐行写 deleted 会让"删一个 10 万文件的目录"产生 10 万条变更 ——
		// 而客户端收到根的那一条就会把本地整棵子树删掉,中间的每一条都是纯开销
		// (而且会把一次操作拆成 10 万个 SSE 帧,足以压垮慢消费者)。
		if ferr := s.feed(ctx, tx, syncfeed.Event{
			SpaceID: cur.SpaceID, FileID: cur.ID, Kind: model.FeedDeleted,
			Version: cur.Version, ParentID: cur.ParentID, Name: cur.Name,
		}); ferr != nil {
			return ferr
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errDeleteDone) {
			return res, nil
		}
		return nil, err
	}
	return res, nil
}

// errDeleteDone 是内部哨兵:根行已不存在(重试幂等),不是错误。
var errDeleteDone = errors.New("filesvc: 子树已删除(幂等重试)")

// checkVersion 校验乐观锁;0 表示客户端未带 base_version。
func (s *Service) checkVersion(f *model.File, base int64, op string) error {
	if base == 0 {
		return nil
	}
	if f.Version != base {
		return conflictOf(f, ReasonVersionConflict,
			fmt.Sprintf("%s失败:该条目已被他人修改(服务端版本 %d,你持有 %d)", op, f.Version, base)).Err()
	}
	return nil
}

// mapWriteErr 把 repo 的写入错误映射成对外的 409/404。
//
// repo 层把"版本不匹配"与"同目录重名"都映射为 ErrConflict(它们来自同一条
// UPDATE 的不同失败方式),所以这里必须**再查一次当前行**来区分:
// 版本不匹配 → reason=version_conflict;重名 → reason=name_conflict。
// 客户端据此给不同提示,而这两种提示的用户动作完全不同。
func (s *Service) mapWriteErr(cur *model.File, err error, dup bool, name string) error {
	if !errors.Is(err, repo.ErrConflict) {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("文件不存在")
		}
		return apierr.Internal(err)
	}
	if dup {
		return conflictOf(cur, ReasonNameConflict,
			fmt.Sprintf("同目录下已存在同名条目: %s", name)).Err()
	}
	return conflictOf(cur, ReasonVersionConflict, "该条目已被他人修改,请刷新后重试").Err()
}

// precheckNameConflict 在**写之前**判断目标位置是否已被他人占用。
//
// 必须在写之前做(而不是失败后再查):PostgreSQL 里唯一约束违反会**终止整个
// 事务**(SQLSTATE 25P02),之后在同一事务里再发任何查询都只会得到
// "当前事务被终止,事务块结束之前的查询被忽略" —— 于是"写失败后再查一次当前行
// 来区分原因"这种写法**根本不可能工作**(实测表现:本该 409 却返回 500)。
//
// 写前查仍有并发窗口(查完到写之间别人插了同名),那种情况由唯一索引兜底;
// 此时 dup 传 false 会报"版本冲突"而不是"重名",提示略不精确但**不会误放行**,
// 且客户端刷新后重试即可。
func (s *Service) precheckNameConflict(
	ctx context.Context, q pgx.Tx, spaceID, parentID, name, excludeID string,
) (bool, error) {
	if parentID == "" {
		return false, nil
	}
	sibling, err := s.Files.GetChildByName(ctx, q, spaceID, parentID, name)
	switch {
	case err == nil:
		return sibling.ID != excludeID, nil
	case errors.Is(err, repo.ErrNotFound):
		return false, nil
	default:
		return false, apierr.Internal(err)
	}
}

// checkWrite 校验写权限(4.3:reader 不能写)。
func (s *Service) checkWrite(ctx context.Context, spaceID, userID string) error {
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
	m, err := s.Spaces.GetMembership(ctx, s.DB, spaceID, userID)
	if err != nil {
		return apierr.Internal(err)
	}
	if m == nil {
		return apierr.SpaceRevoked("你没有该空间的访问权限")
	}
	if m.Permission == model.PermReader {
		return apierr.Forbidden("你在该空间的权限为只读,不能修改文件")
	}
	return nil
}

// objRef 是"文件行 → 物理对象"的引用关系(删除时用于引用计数 -1)。
type objRef struct {
	Hash string
	Size int64
}

// sortHashes 按 hash 升序排序(共享全局锁序,防死锁)。
func sortHashes(refs []objRef) {
	// 简单插入排序:同一子树里的不同对象数量通常很小(去重后个位数),
	// 为此引入 sort 包并写比较函数反而更啰嗦。冒泡/插入在 n<32 时与快排无差别。
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j-1].Hash > refs[j].Hash; j-- {
			refs[j-1], refs[j] = refs[j], refs[j-1]
		}
	}
}

// nameViolationErr 把 namepolicy 违规转成结构化 400(与其它模块同一形状)。
func nameViolationErr(v *namepolicy.Violation) *apierr.Error {
	e := apierr.BadRequest(apierr.CodeInvalidName, "%s", v.Message)
	e.WithDetail("rule", v.Rule)
	if v.Char != "" {
		e.WithDetail("char", v.Char)
	}
	if v.Position > 0 {
		e.WithDetail("position", v.Position)
	}
	if v.Suggestion != "" {
		e.WithDetail("suggestion", v.Suggestion)
	}
	return e
}
