// Package filesvc 实现文件元数据用例(6.4 / 6.6 / 6.7)。
//
// 本包只处理**元数据**;对象字节与引用计数归 finalize 与生命周期 worker(S5)。
package filesvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// DB 与 authsvc.DB / uploadsvc.DB 同构:读走池、写走事务。
//
// 刻意复用 authsvc.DB 而不是再声明一个结构相同的接口 —— 三个模块共享同一个
// "读走池 / 写走事务"契约,重复声明只会让适配器实现两遍。
type DB = authsvc.DB

// Service 文件元数据用例入口。
type Service struct {
	Spaces repo.SpaceRepo
	Files  repo.FileRepo
	// Uploads 用于在删除子树/目录前清理指向它的上传任务行(见 repo.UploadRepo
	// 的"任务行清理"说明):uploads.parent_id 是 RESTRICT 且任务行从不清扫,
	// 不清理的话"曾经有人传过文件的目录"永远删不掉。
	Uploads repo.UploadRepo
	DB      DB
	// Name 是命名策略(改名/移动时复检);零值由 namepolicy 内部兜默认值
	Name namepolicy.Policy
	// LockTTLSeconds 覆盖编辑锁有效期(0 用 DefaultLockTTL = 30min,BE-S6-05)
	LockTTLSeconds int
	// SyncMaxRows 是目录级操作(移动/删除子树)走**同步事务**的行数上限(6.11)。
	// <=0 用 DefaultDirOpSyncMaxRows(1000)。
	SyncMaxRows int
	// Queue 是目录级异步任务队列(BE-S7-02)。为 nil 时**全部走同步**:
	// 功能仍然正确,只是超大树会长时间持锁。
	//
	// 刻意不做"未装配队列就返回 501":把服务端漏装配一个组件变成用户可见的失败,
	// 比"慢一点但能用"更糟;但超阈值时会记一条 Warn(见 enqueueDirOp)。
	Queue *dirops.Queue
	// Log 记录异步降级等运维信号
	Log *slog.Logger
	// Feed 追加变更流条目(6.4 T2-2);为 nil 时不记变更(S9-01 实现前的兼容口)。
	//
	// **必须在调用方事务内写**:变更行与 files 行的改动同事务提交,
	// 否则会出现"文件变了但客户端不知道"或"客户端收到变更但元数据回滚"(见 syncfeed 包注释)。
	Feed FeedAppender
}

// DefaultDirOpSyncMaxRows 是 6.11 的默认同步阈值(行)。
const DefaultDirOpSyncMaxRows = 1000

func (s *Service) syncMaxRows() int {
	if s.SyncMaxRows > 0 {
		return s.SyncMaxRows
	}
	return DefaultDirOpSyncMaxRows
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// guardDirOp 在条目祖先链上有"移动中"标记时拒绝(6.11:任务期间其子请求一律 409)。
//
// 用 409 而不是 423/503:客户端已有的 409 处理路径(带 reason 的结构化冲突体)
// 正好能用上 —— 它读到 `dir_op_in_progress` 就知道"该刷新列表并提示稍后再试",
// 而不是把用户引向"重试"按钮(重试必然还是 409,直到任务结束)。
func (s *Service) guardDirOp(ctx context.Context, q repo.Querier, fileID string) error {
	busy, err := s.Files.DirOpInProgress(ctx, q, fileID)
	if err != nil {
		return apierr.Internal(err)
	}
	if busy {
		c := &ConflictError{
			Reason:  ReasonDirOpInProgress,
			Message: "该目录正在执行移动/删除任务,请稍后再试",
		}
		return c.Err()
	}
	return nil
}

// FeedAppender 是变更流的窄接口(由 syncfeed.Writer 实现)。
//
// 用窄接口而不是具体类型:filesvc 不该知道变更流存在哪张表、怎么取号,
// 它只需要"在同一个事务里记一笔"。
type FeedAppender interface {
	AppendEvent(ctx context.Context, q repo.Querier, e syncfeed.Event) (int64, error)
}

// feed 在**同一事务**内记一笔变更;未装配时静默跳过。
func (s *Service) feed(ctx context.Context, tx pgx.Tx, e syncfeed.Event) error {
	if s.Feed == nil {
		return nil
	}
	if _, err := s.Feed.AppendEvent(ctx, tx, e); err != nil {
		return apierr.Internal(fmt.Errorf("写入变更流失败: %w", err))
	}
	return nil
}

// SubtreeStats 是子树影响面(6.11 预检:删除确认与容量预估共用)。
//
// 与删除走**同一份统计实现**(repo.SubtreeStatsOf),因此"预检说 3 个文件"
// 与"删除时确实删了 3 个"不会漂移 —— 若各写一份 SQL,两边迟早对不上,
// 而用户是拿预检结果做的决定。
type SubtreeStats struct {
	FileID    string `json:"file_id"`
	Name      string `json:"name"`
	IsDir     bool   `json:"is_dir"`
	FileCount int64  `json:"file_count"`
	DirCount  int64  `json:"dir_count"`
	TotalSize int64  `json:"total_size_bytes"`
	// ObjectRefs 是子树内不同物理对象的个数(去重后),即删除会影响的 ref_count 行数
	ObjectRefs int64 `json:"object_refs"`
	// DedupSavedBytes = TotalSize - 去重后的物理占用,即内容寻址省下的空间
	DedupSavedBytes int64 `json:"dedup_saved_bytes"`
}

// SubtreeStats 返回子树影响面(需要读权限)。
func (s *Service) SubtreeStats(ctx context.Context, userID, fileID string) (*SubtreeStats, error) {
	f, err := s.Files.GetByID(ctx, s.DB, fileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkRead(ctx, f.SpaceID, userID); err != nil {
		return nil, err
	}
	st, err := s.Files.SubtreeStatsOf(ctx, s.DB, fileID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	// 去重后物理占用:按 (hash,size) 去重后的 size 之和。
	// 这正是"预检能告诉用户删掉它实际释放多少磁盘"的意义 ——
	// 逻辑字节数(TotalSize)与物理释放量在内容寻址下通常不同。
	var distinctBytes, distinctObjs int64
	if err := s.DB.QueryRow(ctx, `
WITH RECURSIVE sub AS (
    SELECT id, is_dir, size, hash_sha256 FROM files WHERE id = $1
    UNION ALL
    SELECT f.id, f.is_dir, f.size, f.hash_sha256 FROM files f JOIN sub ON f.parent_id = sub.id
)
SELECT coalesce(sum(size), 0), count(*) FROM (
    SELECT DISTINCT hash_sha256, size FROM sub WHERE NOT is_dir AND hash_sha256 IS NOT NULL
) d`, fileID).Scan(&distinctBytes, &distinctObjs); err != nil {
		return nil, apierr.Internal(err)
	}
	return &SubtreeStats{
		FileID:          f.ID,
		Name:            f.Name,
		IsDir:           f.IsDir,
		FileCount:       st.FileCount,
		DirCount:        st.DirCount,
		TotalSize:       st.TotalSize,
		ObjectRefs:      distinctObjs,
		DedupSavedBytes: st.TotalSize - distinctBytes,
	}, nil
}

// ListInput 列目录入参。
type ListInput struct {
	UserID   string
	SpaceID  string
	ParentID string // 空 = 空间根目录
	// After 是 keyset 分页游标(上一页最后一个条目的 lower(name));空 = 首页
	After string
	Limit int
}

// ListResult 列目录结果。
type ListResult struct {
	SpaceID  string      `json:"space_id"`
	ParentID string      `json:"parent_id"`
	Entries  []EntryView `json:"entries"`
	// NextAfter 非空表示还有下一页,客户端把它原样回传即可
	NextAfter string `json:"next_after,omitempty"`
}

// EntryView 是列表条目对外字段(6.4:必须带 version/etag/size/mtime/mime)。
type EntryView struct {
	ID string `json:"id"`
	// SpaceID 是条目所属空间:客户端同时同步多个空间时需要它把条目归位,
	// 事件推送(SSE)也要靠它做可见性过滤。
	SpaceID    string `json:"space_id,omitempty"`
	ParentID   string `json:"parent_id"`
	Name       string `json:"name"`
	IsDir      bool   `json:"is_dir"`
	Size       int64  `json:"size"`
	MimeType   string `json:"mime_type"`
	HashSHA256 string `json:"hash_sha256,omitempty"`
	Version    int64  `json:"version"`
	// ETag 是 **已加引号** 的值(见 QuoteETag);客户端可直接放进 If-Match
	ETag      string `json:"etag"`
	Depth     int    `json:"depth"`
	UpdatedAt string `json:"updated_at"`
	CreatedAt string `json:"created_at"`
}

// List 列出目录直接子项(keyset 分页 + 权限过滤)。
//
// 权限:先解析空间成员关系,无权限直接 403/410 —— **绝不返回部分数据后由前端过滤**
// (4.3:权限判断只在服务端,端上无判权代码)。
func (s *Service) List(ctx context.Context, in ListInput) (*ListResult, error) {
	spaceID, parentID, err := s.resolve(ctx, in.UserID, in.SpaceID, in.ParentID)
	if err != nil {
		return nil, err
	}
	if in.Limit <= 0 || in.Limit > 1000 {
		in.Limit = 200
	}
	// 多取一条用于判断"是否还有下一页",返回时去掉
	rows, err := s.Files.ListChildren(ctx, s.DB, spaceID, parentID, in.Limit+1, in.After)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	res := &ListResult{SpaceID: spaceID, ParentID: parentID, Entries: make([]EntryView, 0, len(rows))}
	if len(rows) > in.Limit {
		rows = rows[:in.Limit]
		res.NextAfter = strings.ToLower(rows[len(rows)-1].Name)
	}
	for _, f := range rows {
		res.Entries = append(res.Entries, View(f))
	}
	return res, nil
}

// Get 取单个文件的元数据(含权限校验)。
func (s *Service) Get(ctx context.Context, userID, fileID string) (*EntryView, error) {
	f, err := s.Files.GetByID(ctx, s.DB, fileID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	if err := s.checkRead(ctx, f.SpaceID, userID); err != nil {
		return nil, err
	}
	if err := s.guardDirOp(ctx, s.DB, f.ID); err != nil {
		return nil, err
	}
	v := View(f)
	return &v, nil
}

// resolve 解析并校验 (space, parent)。
//
// 三种情况都要处理:
//   - spaceID 为空 → 用调用者的个人空间
//   - parentID 为空 → 用空间根目录
//   - 给了 parentID → 必须是同空间的目录(防跨空间越权)
func (s *Service) resolve(ctx context.Context, userID, spaceID, parentID string) (string, string, error) {
	if spaceID == "" {
		sp, err := s.Spaces.PersonalOf(ctx, s.DB, userID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return "", "", apierr.Internal(fmt.Errorf("用户缺少个人空间(数据不完整)"))
			}
			return "", "", apierr.Internal(err)
		}
		spaceID = sp.ID
	}
	if err := s.checkRead(ctx, spaceID, userID); err != nil {
		return "", "", err
	}
	if parentID == "" {
		root, err := s.Files.GetRoot(ctx, s.DB, spaceID)
		if err != nil {
			return "", "", apierr.Internal(fmt.Errorf("空间根目录缺失: %w", err))
		}
		return spaceID, root.ID, nil
	}
	parent, err := s.Files.GetByID(ctx, s.DB, parentID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return "", "", apierr.NotFound("目录不存在")
		}
		return "", "", apierr.Internal(err)
	}
	if parent.SpaceID != spaceID {
		return "", "", apierr.Forbidden("目标目录不属于该空间")
	}
	if !parent.IsDir {
		return "", "", apierr.BadRequest(apierr.CodeInvalidArgument, "目标不是目录")
	}
	if err := s.guardDirOp(ctx, s.DB, parent.ID); err != nil {
		return "", "", err
	}
	return spaceID, parentID, nil
}

// CheckReadable 校验读权限(供**非本包**的读路径复用,例如 /changes 与 SSE)。
//
// 导出的理由:`/changes` 与 SSE 都要先判权再返回数据,而"判权规则"必须只有一份 ——
// 若它们各自实现一遍,某天有人改了"被移出空间返回 410 而不是 404"这条(6.4 P1-2),
// 那些路径不会跟着变,于是客户端在"被移出"时会误判为"远端删了"而删掉本地文件。
func (s *Service) CheckReadable(ctx context.Context, userID, spaceID string) error {
	return s.checkRead(ctx, spaceID, userID)
}

// CheckWritable 校验写权限(供 WebDAV 适配层复用,理由同 CheckReadable)。
func (s *Service) CheckWritable(ctx context.Context, userID, spaceID string) error {
	return s.checkWrite(ctx, spaceID, userID)
}

// checkRead 校验读权限(4.3 三级权限:reader/editor/manager 都可读)。
//
// 被移出空间返回 410 + space_revoked,而不是 404 —— 客户端据此**保留本地文件**
// 并等待成员事件恢复(7.2 P1-2 / R-13);若返回 404 客户端会误判为"远端删了"而删本地。
func (s *Service) checkRead(ctx context.Context, spaceID, userID string) error {
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
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.SpaceGone("空间不存在或已被解散")
		}
		return apierr.Internal(err)
	}
	if m == nil {
		// 无成员关系 —— 对团队空间而言等价于"被移出"
		return apierr.SpaceRevoked("你没有该空间的访问权限")
	}
	return nil
}

// View 把 model.File 转成对外视图(含已加引号的 ETag)。
func View(f *model.File) EntryView {
	if f == nil {
		return EntryView{}
	}
	return EntryView{
		ID:         f.ID,
		SpaceID:    f.SpaceID,
		ParentID:   f.ParentID,
		Name:       f.Name,
		IsDir:      f.IsDir,
		Size:       f.Size,
		MimeType:   f.MimeType,
		HashSHA256: f.HashSHA256,
		Version:    f.Version,
		ETag:       QuoteETag(f.Etag),
		Depth:      f.Depth,
		UpdatedAt:  f.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		CreatedAt:  f.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

// ---- ETag 单一出口(R-21)----

// QuoteETag 把数据库里的裸 etag(`files.etag` 生成列**不含引号**)转成 HTTP 合法的 ETag。
//
// **这是全项目唯一允许加引号的地方。** 三处出口必须走它,否则会出现
// "同一个文件的 ETag 在列表里是 `abc`、在响应头里是 `"abc"`,
// 客户端拿列表里的值去发 If-Match 永远不匹配" —— 表现为**条件请求恒 412**,
// 而每个出口单独看都"没错"。
//
// 三处出口:
//  1. REST 响应体(列表/详情)的 `etag` 字段 —— EntryView.ETag
//  2. HTTP 响应头 `ETag`(GET/HEAD)—— WriteETagHeader
//  3. WebDAV PROPFIND 的 `getetag` 属性(S8)—— 复用本函数
//
// 刻意**不加 `W/` 弱标记**:`files.etag` 由 version + 内容哈希派生,
// 内容或元数据一变它就变,语义上就是强校验器;标成弱 ETag 会让
// `If-Match`(要求强校验)在合规客户端上失效。
func QuoteETag(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 目录在生成列里是 version-00000000,不会为空;真为空说明数据异常。
		// 返回空串让调用方跳过该头,而不是编造一个可能碰撞的值。
		return ""
	}
	// 幂等:已经是带引号的形式就原样返回(避免出现 `""abc""`)
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw
	}
	return `"` + raw + `"`
}

// WriteETagHeader 写 HTTP ETag 响应头(出口 2)。
//
// 空 etag 直接跳过:**不要写 `ETag: ""`** —— 那是个合法但永不匹配的值,
// 会让客户端的条件请求陷入"永远 412"。
func WriteETagHeader(set func(k, v string), raw string) {
	if q := QuoteETag(raw); q != "" {
		set("ETag", q)
	}
}
