package filesvc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/objlock"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 本文件是**"引用 +1"路径**(4.5 逻辑复制 + 物理去重 / ADR-2 双层锁制的②层)。
//
// 共同纪律(三条,缺一即悬空指针):
//
//  1. 引用 +1 只允许指向**可用对象**(live / pending_delete / deleting),
//     且必须在**同事务内先取对象锁**。不取锁就可能潜入清理 worker
//     "已领取(deleting)、尚未 unlink"的窗口 —— 引用加上去了、对象随后被删,
//     于是得到一个指向不存在内容的 files 行(ADR-2 / 三审 P1-1)。
//  2. 用**事务级**锁(`objlock.LockManyXact`):这些路径没有对象 IO,
//     事务边界即保护窗口;会话级锁在这里是浪费(见 ADR-2 的双层锁制)。
//  3. 多对象场景**按 hash 升序**取锁(死锁防护,6.6 全局锁序)。
//
// 与 finalize 的差别也正在于此:finalize 要做事务外对象写,所以必须会话级锁;
// 而共享/复制只动元数据与计数 —— 事务一提交锁就自动释放,不需要额外收尾。

// ShareInput 是把一个文件共享到另一个空间。
type ShareInput struct {
	UserID string
	FileID string
	// TargetSpaceID 目标空间(通常是团队空间)
	TargetSpaceID string
	// TargetParentID 目标目录;空则用目标空间根目录
	TargetParentID string
	// NewName 目标名;空则沿用原名
	NewName string
}

// Share 把文件"逻辑复制"到目标空间(4.5)。
//
// **只新建元数据行,内容不复制**:两行指向同一 hash,物理对象靠 ref_count 共享。
// 因此这里绝不能去 Open/Write 对象 —— 那会把"共享"退化成"复制"。
//
// 配额**按逻辑行计**:目标空间按 size 占额度。这一点不能省 ——
// 否则"共享"就成了绕过配额的通道(把一个 10GB 文件共享到 10 个空间,
// 物理只存一份,但每个空间都该看到自己的配额被占用)。
func (s *Service) Share(ctx context.Context, in ShareInput) (*EntryView, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	if strings.TrimSpace(in.TargetSpaceID) == "" {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "缺少目标空间")
	}
	src, err := s.Files.GetByID(ctx, s.DB, in.FileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if src.IsDir {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument,
			"暂不支持共享目录(请使用复制)")
	}
	if src.HashSHA256 == "" {
		return nil, apierr.Internal(fmt.Errorf("源文件缺少内容哈希(数据不完整)"))
	}
	// 源:至少可读;目标:必须可写(4.3)
	if err := s.checkRead(ctx, src.SpaceID, in.UserID); err != nil {
		return nil, err
	}
	if err := s.checkWrite(ctx, in.TargetSpaceID, in.UserID); err != nil {
		return nil, err
	}
	// 6.11:源或目标所在子树正在跑目录任务 → 409
	if err := s.guardDirOp(ctx, s.DB, src.ID); err != nil {
		return nil, err
	}
	if in.TargetParentID != "" {
		if err := s.guardDirOp(ctx, s.DB, in.TargetParentID); err != nil {
			return nil, err
		}
	}
	name := src.Name
	if n := namepolicy.Normalize(strings.TrimSpace(in.NewName)); n != "" {
		if v, ok := s.Name.Validate(n); !ok {
			return nil, nameViolationErr(v)
		}
		name = n
	}

	var out *model.File
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		parentID := in.TargetParentID
		parentDepth := 0
		if parentID == "" {
			root, rerr := s.Files.GetRoot(ctx, tx, in.TargetSpaceID)
			if rerr != nil {
				return apierr.Internal(fmt.Errorf("目标空间根目录缺失: %w", rerr))
			}
			parentID, parentDepth = root.ID, root.Depth
		} else {
			parent, perr := s.Files.GetByID(ctx, tx, parentID)
			if perr != nil {
				if errors.Is(perr, repo.ErrNotFound) {
					return apierr.NotFound("目标目录不存在")
				}
				return apierr.Internal(perr)
			}
			if !parent.IsDir || parent.SpaceID != in.TargetSpaceID {
				return apierr.BadRequest(apierr.CodeInvalidArgument, "目标不是该空间内的目录")
			}
			parentDepth = parent.Depth
		}
		// 同名冲突:共享是"新建一行",同名必须显式拦下(绝不静默改名,6.7)
		dup, derr := s.precheckNameConflict(ctx, tx, in.TargetSpaceID, parentID, name, "")
		if derr != nil {
			return derr
		}
		if dup {
			return conflictOf(nil, ReasonNameConflict,
				fmt.Sprintf("目标目录下已存在同名条目: %s", name)).Err()
		}

		// (a) 先取对象锁(事务级,同事务)
		if lerr := objlock.LockManyXact(ctx, tx, []string{src.HashSHA256}); lerr != nil {
			return apierr.Internal(lerr)
		}
		// (b) 引用 +1(条件更新:只允许指向可用对象;命中 pending_delete 则复活)
		if berr := bumpObjectRefTx(ctx, tx, src.HashSHA256); berr != nil {
			return berr
		}
		// (c) 新建元数据行(内容不复制)
		created, cerr := s.Files.InsertRow(ctx, tx, &model.File{
			SpaceID: in.TargetSpaceID, ParentID: parentID, OwnerID: in.UserID,
			Name: name, IsDir: false, Size: src.Size, MimeType: src.MimeType,
			HashSHA256: src.HashSHA256, Depth: parentDepth + 1,
		})
		if cerr != nil {
			if errors.Is(cerr, repo.ErrConflict) {
				return conflictOf(nil, ReasonNameConflict,
					fmt.Sprintf("目标目录下已存在同名条目: %s", name)).Err()
			}
			return apierr.Internal(cerr)
		}
		// (d) 配额按逻辑行计:目标空间占满 size
		if src.Size > 0 {
			if _, qerr := s.Spaces.Reserve(ctx, tx, in.TargetSpaceID, src.Size); qerr != nil {
				if errors.Is(qerr, repo.ErrQuotaExceeded) {
					return apierr.QuotaExceeded("目标空间额度不足:需要 %d 字节", src.Size)
				}
				if errors.Is(qerr, repo.ErrNotFound) {
					return apierr.SpaceGone("目标空间不存在")
				}
				return apierr.Internal(qerr)
			}
		}
		// (e) 变更流:写进**目标空间**的 feed,kind=shared_in。
		//
		// 必须写目标空间而不是源空间:目标空间的成员需要知道"多了一个文件";
		// 源空间什么都没变(个人文件还在原处),给他们推变更只会让所有客户端白拉一次。
		// kind 用 shared_in 让客户端能区分"共享进来的"与"我自己新建的"
		// (前者常需要提示"XX 共享了一个文件给你")。
		if ferr := s.feed(ctx, tx, syncfeed.Event{
			SpaceID: created.SpaceID, FileID: created.ID, Kind: model.FeedSharedIn,
			Version: created.Version, ParentID: created.ParentID, Name: created.Name,
		}); ferr != nil {
			return ferr
		}
		out = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	v := View(out)
	return &v, nil
}

// CopyInput 是复制(同一空间内,可指定目标目录与名字)。
type CopyInput struct {
	UserID string
	FileID string
	// TargetParentID 目标目录;空则与源同目录
	TargetParentID string
	// NewName 目标名;空则自动生成"原名 (副本)"(冲突时再加序号)
	NewName string
}

// CopyResult 是复制结果。
type CopyResult struct {
	Root *EntryView `json:"root"`
	// CopiedFiles / CopiedDirs 是新建的行数
	CopiedFiles int64 `json:"copied_files"`
	CopiedDirs  int64 `json:"copied_dirs"`
	// ObjectRefsAdded 是不同物理对象的引用增量(去重后)
	ObjectRefsAdded int64 `json:"object_refs_added"`
	// SizeBytes 是新增的逻辑字节数(用于配额提示)
	SizeBytes int64 `json:"size_bytes"`
}

// Copy 复制文件或目录(BE-S7-03)。
//
// 语义与共享同源(**逻辑复制 + 物理去重**),差别只在"目标是否同空间、是否递归"。
// 因此"每行 ref_count+1 与元数据同事务"这条纪律在这里必须逐行成立 ——
// 漏掉任何一行的 +1,那一行就会成为悬空指针来源(它指向的对象引用数少 1,
// 清理 worker 会提前把它删掉)。
//
// 实现顺序刻意如此(与 ADR-2 / 6.6 锁序一致):
//
//  1. 先**读完**整棵子树的元数据(此刻行还在),得到全部 hash
//  2. **按 hash 升序一次性取全部对象锁**(事务级)
//  3. 逐行 ref_count+1(去重后每对象一次)
//  4. 递归插入新的元数据行(重新映射 parent_id)
//  5. 配额按新增逻辑字节计入
//
// 第 1 步必须在第 2 步之前:取锁要用到 hash 清单,而清单来自读树。
// 第 2 步一次性取全部锁(而不是"插一行取一把"):取锁顺序必须是全局升序,
// 而升序只有在"已知全集"时才能保证 —— 边插边取时后面才遇到的 hash
// 可能小于已取过的,锁序就不再是全序的子序列,死锁防护失效。
func (s *Service) Copy(ctx context.Context, in CopyInput) (*CopyResult, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("filesvc: DB 未装配"))
	}
	src, err := s.Files.GetByID(ctx, s.DB, in.FileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkRead(ctx, src.SpaceID, in.UserID); err != nil {
		return nil, err
	}
	if err := s.checkWrite(ctx, src.SpaceID, in.UserID); err != nil {
		return nil, err
	}
	// 6.11:源或目标所在子树正在跑目录任务 → 409
	if err := s.guardDirOp(ctx, s.DB, src.ID); err != nil {
		return nil, err
	}
	if in.TargetParentID != "" {
		if err := s.guardDirOp(ctx, s.DB, in.TargetParentID); err != nil {
			return nil, err
		}
	}

	res := &CopyResult{}
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		// 目标父目录
		targetParent := in.TargetParentID
		if targetParent == "" {
			if src.ParentID == "" {
				return apierr.BadRequest(apierr.CodeInvalidArgument, "不能复制空间根目录")
			}
			targetParent = src.ParentID
		}
		parent, perr := s.Files.GetByID(ctx, tx, targetParent)
		if perr != nil {
			if errors.Is(perr, repo.ErrNotFound) {
				return apierr.NotFound("目标目录不存在")
			}
			return apierr.Internal(perr)
		}
		if !parent.IsDir || parent.SpaceID != src.SpaceID {
			return apierr.BadRequest(apierr.CodeInvalidArgument, "目标必须是同空间内的目录")
		}
		if v, ok := s.Name.CheckPath("", parent.Depth+1); !ok {
			return nameViolationErr(v)
		}

		// 目标名:显式给出则校验冲突;未给出则自动生成"原名 (副本)"
		rootName := namepolicy.Normalize(strings.TrimSpace(in.NewName))
		if rootName == "" {
			var nerr error
			rootName, nerr = s.autoCopyName(ctx, tx, src.SpaceID, targetParent, src.Name)
			if nerr != nil {
				return nerr
			}
		} else {
			if v, ok := s.Name.Validate(rootName); !ok {
				return nameViolationErr(v)
			}
			dup, derr := s.precheckNameConflict(ctx, tx, src.SpaceID, targetParent, rootName, "")
			if derr != nil {
				return derr
			}
			if dup {
				return conflictOf(nil, ReasonNameConflict,
					fmt.Sprintf("目标目录下已存在同名条目: %s", rootName)).Err()
			}
		}

		// ---- 1) 读整棵子树(含自身),按层级顺序返回 ----
		type node struct {
			SrcID    string
			ParentID string
			Name     string
			IsDir    bool
			Size     int64
			Mime     string
			Hash     string
			Depth    int
		}
		rows, qerr := tx.Query(ctx, `
WITH RECURSIVE sub AS (
    SELECT id, parent_id, name, is_dir, size, mime_type,
           coalesce(hash_sha256,'') AS hash, depth, 0 AS lvl
      FROM files WHERE id = $1
    UNION ALL
    SELECT f.id, f.parent_id, f.name, f.is_dir, f.size, f.mime_type,
           coalesce(f.hash_sha256,''), f.depth, s.lvl + 1
      FROM files f JOIN sub s ON f.parent_id = s.id
)
SELECT id, coalesce(parent_id::text,''), name, is_dir, size, mime_type, hash
  FROM sub ORDER BY lvl, name`, src.ID)
		if qerr != nil {
			return apierr.Internal(qerr)
		}
		var nodes []node
		hashes := make([]string, 0, 8)
		for rows.Next() {
			var n node
			if serr := rows.Scan(&n.SrcID, &n.ParentID, &n.Name, &n.IsDir,
				&n.Size, &n.Mime, &n.Hash); serr != nil {
				rows.Close()
				return apierr.Internal(serr)
			}
			nodes = append(nodes, n)
			if !n.IsDir && n.Hash != "" {
				hashes = append(hashes, n.Hash)
			}
		}
		rows.Close()
		if rerr := rows.Err(); rerr != nil {
			return apierr.Internal(rerr)
		}
		if len(nodes) == 0 {
			return apierr.Internal(fmt.Errorf("复制的源条目在事务内不可见(数据异常)"))
		}

		// ---- 2) 按 hash 升序一次性取全部对象锁(事务级) ----
		if lerr := objlock.LockManyXact(ctx, tx, hashes); lerr != nil {
			return apierr.Internal(lerr)
		}
		// ---- 3) 引用 +1(同一事务;任何一行失败 → 整体回滚) ----
		//
		// ⚠ **必须按 hash 升序执行,不能用 nodes 的子树遍历序**。
		// 原因:`bumpObjectRefTx` 是 `UPDATE file_objects WHERE hash_sha256=$1`,
		// 它会**取该行的行锁**。第 2 步的 objlock.LockManyXact 只锁了 advisory 锁,
		// 行锁是这里才取的 —— 若按遍历序(lvl,name)取行锁,就与删除侧
		// (deleteSubtree 按 **hash 升序**释放引用)形成**相反的加锁顺序**,
		// 并发 "COPY 大目录 + 删除其中一份副本" 时复现 PostgreSQL 死锁
		// (SQLSTATE 40P01 → 500;实测 6 次中 1 次)。这与 6.6 的全局锁序纪律
		// (R-09 同源)是同一件事:**任何一批行锁都必须按同一个全序取**。
		// 所以先把每个 hash 需要 +1 的次数数出来(纯内存,不加锁),
		// 再按升序逐个执行,与删除侧落在同一全序上。
		{
			need := make(map[string]int, len(nodes))
			for _, n := range nodes {
				if n.IsDir || n.Hash == "" {
					continue
				}
				need[n.Hash]++
				res.SizeBytes += n.Size
			}
			ordered := make([]string, 0, len(need))
			for h := range need {
				ordered = append(ordered, h)
			}
			sort.Strings(ordered)
			for _, h := range ordered {
				for i := 0; i < need[h]; i++ {
					if berr := bumpObjectRefTx(ctx, tx, h); berr != nil {
						return berr
					}
				}
			}
		}
		// 引用增量按**去重后**的对象数报(与预检 object_refs 同口径)
		{
			seen := make(map[string]struct{}, len(hashes))
			for _, h := range hashes {
				seen[h] = struct{}{}
			}
			res.ObjectRefsAdded = int64(len(seen))
		}

		// ---- 4) 递归插入新元数据行(parent_id 重映射) ----
		// 用"源 id → 新 id/新深度"映射逐层展开:COPY 后的父子关系必须与源一一对应,
		// 靠名字匹配会在"同名不同层"时错连。
		idMap := make(map[string]string, len(nodes))
		depthMap := make(map[string]int, len(nodes))
		for i, n := range nodes {
			newParent := targetParent
			depth := parent.Depth + 1
			if i > 0 {
				mapped, ok := idMap[n.ParentID]
				if !ok {
					return apierr.Internal(fmt.Errorf(
						"复制子树时父节点尚未处理(数据异常): %s", n.ParentID))
				}
				d, ok2 := depthMap[n.ParentID]
				if !ok2 {
					return apierr.Internal(fmt.Errorf(
						"复制子树时父节点深度缺失(数据异常): %s", n.ParentID))
				}
				newParent, depth = mapped, d+1
			}
			name := n.Name
			if i == 0 {
				name = rootName
			}
			created, cerr := s.Files.InsertRow(ctx, tx, &model.File{
				SpaceID: src.SpaceID, ParentID: newParent, OwnerID: in.UserID,
				Name: name, IsDir: n.IsDir, Size: n.Size, MimeType: n.Mime,
				HashSHA256: n.Hash, Depth: depth,
			})
			if cerr != nil {
				if errors.Is(cerr, repo.ErrConflict) {
					return conflictOf(nil, ReasonNameConflict,
						fmt.Sprintf("目标位置已存在同名条目: %s", name)).Err()
				}
				return apierr.Internal(cerr)
			}
			idMap[n.SrcID] = created.ID
			depthMap[n.SrcID] = created.Depth
			if n.IsDir {
				res.CopiedDirs++
			} else {
				res.CopiedFiles++
			}
			if i == 0 {
				v := View(created)
				res.Root = &v
			}
		}
		// 源行在事务内被删(并发删除)时,CTE 只剩自身之外可能为空;此处再确认一次
		if res.Root == nil {
			return apierr.Internal(fmt.Errorf("复制未产生任何元数据行(数据异常)"))
		}

		// ---- 5) 配额按新增逻辑字节计入 ----
		if res.SizeBytes > 0 {
			if _, qerr2 := s.Spaces.Reserve(ctx, tx, src.SpaceID, res.SizeBytes); qerr2 != nil {
				if errors.Is(qerr2, repo.ErrQuotaExceeded) {
					return apierr.QuotaExceeded("空间额度不足:复制需要 %d 字节", res.SizeBytes)
				}
				return apierr.Internal(qerr2)
			}
		}
		// ---- 6) 变更流:复制整棵子树只写**根的一行**(聚合事件) ----
		//
		// 与删除同理:逐行写 created 会让"复制一个 10 万文件的目录"产生 10 万条变更,
		// 而客户端收到根的那一条就会去拉取整棵子树(它本来就有这个接口)。
		// 逐行推送只会把一次操作变成 10 万个 SSE 帧(足以压垮慢消费者)。
		if res.Root != nil {
			if ferr := s.feed(ctx, tx, syncfeed.Event{
				SpaceID: src.SpaceID, FileID: res.Root.ID, Kind: model.FeedCreated,
				Version: res.Root.Version, ParentID: res.Root.ParentID, Name: res.Root.Name,
			}); ferr != nil {
				return ferr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// autoCopyName 生成"原名 (副本)"并在冲突时追加序号(2)(3)…
//
// 为什么不直接报 409:用户点"复制"时并没有指定名字,报冲突等于让他手动想一个
// 新名字 —— 那正是"复制"这个动作本身该替他做的事。但**只在未显式给名时**才自动;
// 显式给了名字就必须报冲突(6.7:绝不静默改名)。
//
// 序号上限 100:超过说明目录里堆了 100 份同名副本,继续自动加号只会让文件名
// 越来越长且毫无意义 —— 此时报冲突让用户自己决定。
func (s *Service) autoCopyName(ctx context.Context, q pgx.Tx, spaceID, parentID, base string) (string, error) {
	maxBytes := s.Name.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 240
	}
	mk := func(suffix string) string {
		if len(namepolicy.Normalize(base))+len(suffix) <= maxBytes {
			return base + suffix
		}
		// 先按码点截断主体再加后缀,保证结果不超长且不切断 UTF-8
		return truncateForSuffix(base, maxBytes-len(suffix)) + suffix
	}
	candidate := mk(" (副本)")
	free, err := s.nameIsFree(ctx, q, spaceID, parentID, candidate)
	if err != nil {
		return "", err
	}
	if free {
		return candidate, nil
	}
	for i := 2; i <= 100; i++ {
		candidate = mk(fmt.Sprintf(" (副本 %d)", i))
		free, err := s.nameIsFree(ctx, q, spaceID, parentID, candidate)
		if err != nil {
			return "", err
		}
		if free {
			return candidate, nil
		}
	}
	return "", apierr.Conflict(apierr.CodeNameConflict,
		"目标目录下同名副本过多,请手动指定名称")
}

// nameIsFree 判断目标目录下该名字是否可用(大小写不敏感,与唯一索引同口径)。
func (s *Service) nameIsFree(ctx context.Context, q pgx.Tx, spaceID, parentID, name string) (bool, error) {
	_, err := s.Files.GetChildByName(ctx, q, spaceID, parentID, name)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, repo.ErrNotFound):
		return true, nil
	default:
		return false, apierr.Internal(err)
	}
}

// truncateForSuffix 按 UTF-8 码点边界截断到不超过 max 字节。
//
// 直接切片会在多字节字符中间切断,产生非法 UTF-8 文件名 ——
// 而 6.7 的整个命名规范都建立在"名字是合法 UTF-8"之上。
func truncateForSuffix(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	b := []byte(s)
	// 回退到码点边界(续字节形如 10xxxxxx)
	cut := max
	for cut > 0 && (b[cut]&0xC0) == 0x80 {
		cut--
	}
	return string(b[:cut])
}

// bumpObjectRefTx 在**同一事务内**把某对象的引用 +1(条件更新,允许复活)。
//
// 这是全项目"引用 +1"的唯一实现:share / copy / 后续 WebDAV COPY 都走它。
// 语义与 finalize 的 bumpObjectRef 一致,差别是**明确要求调用方已持锁**
// (函数名与注释都在提醒这一点)。
//
// 为什么条件里带 state:只允许指向 live / pending_delete / deleting ——
// 命中 pending_delete/deleting 时同一条 UPDATE **顺便复活**(置 live、清 delete_after)。
// 这正是"根除刚加到 1 的对象已被清理"的关键(4.5 最后一条)。
// `state='deleted'` **不能**复活:对象已物理删除,复活会得到指向空内容的行。
//
// 为什么按 hash 匹配而**不带 size**:`file_objects.hash_sha256` 是主键,
// 内容寻址下 size 由内容唯一确定;把 size 也放进 WHERE 只会在
// "files.size 与对象实际大小漂移"时把错误伪装成"对象不可用",
// 掩盖真正的问题(数据漂移应另有对账任务发现,见 4.3 ReconcileUsed)。
func bumpObjectRefTx(ctx context.Context, tx pgx.Tx, hash string) error {
	tag, err := tx.Exec(ctx, `
UPDATE file_objects
   SET ref_count = ref_count + 1, state = 'live', delete_after = NULL, updated_at = now()
 WHERE hash_sha256 = $1
   AND state IN ('live','pending_delete','deleting')`, hash)
	if err != nil {
		return apierr.Internal(err)
	}
	if tag.RowsAffected() == 0 {
		// 匹配 0 行:对象不存在,或已 deleted,或 state 非法。
		//
		// 这里**不能**像 finalize 那样"回落为新建 walk" —— 共享/复制不携带内容,
		// 无法重新落盘。所以必须显式报错,而不是插一行指向不存在的对象。
		return apierr.Internal(fmt.Errorf(
			"对象 %s 不可用(不存在或已删除),无法建立引用", objlock.HashKey(hash)))
	}
	return nil
}
