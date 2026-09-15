// Package uploadsvc 实现上传任务的创建与 ticket 校验(4.3 预留-结算 + 6.3/6.10)。
//
// 关键设计:
//   - **ticket 生命周期覆盖整个 upload**,可多次 PATCH 复用(修 V2.14"一次性"与 TUS 断点续传的冲突);
//     明文 ticket 只在创建时返回一次,库里只存 SHA-256(库被读也不泄露凭据)
//   - **额度在创建时预留**,额度不足创建即拒(4.3:不是传完 10GB 才说不给)
//   - 预留账本落在 uploads 行(不落 Redis),避免 Redis 重启丢账(10.7)
package uploadsvc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
)

// DB 与 authsvc.DB 同构:读走池、写走事务。
//
// 刻意复用 authsvc.DB 而不是再声明一个结构相同的接口 ——
// 两个模块共享同一个"读走池 / 写走事务"契约,重复声明只会让适配器实现两遍。
type DB = authsvc.DB

// Service 上传用例入口。
type Service struct {
	Spaces  repo.SpaceRepo
	Files   repo.FileRepo
	Uploads repo.UploadRepo
	DB      DB
	Name    namepolicy.Policy
	// TicketTTL 覆盖整个 upload 生命周期(默认 24h,与 6.1 临时文件清理同窗)
	TicketTTL time.Duration
	// StaleUploadAfter 覆盖"空任务闲置多久算死掉"(0 用 DefaultStaleUploadAfter=30min);
	// 建任务时若发现同目录同名有一个**自己创建、一字节未收、且闲置超过它**的任务,就把它回收掉
	// (见 Create 的 4b 判定:否则崩溃/失败的上传会把文件名锁到票过期)。
	StaleUploadAfter time.Duration
	Now              func() time.Time
	// Fast 是秒传的持物证明挑战(BE-S4-04);为 nil 或 Redis 不可用时不提供秒传通道
	Fast *fastupload.Service
	// FastMinSize 覆盖开放秒传通道的最小大小(0 用 fastupload.MinSize)
	FastMinSize int64
}

// CreateInput 创建上传任务入参。
type CreateInput struct {
	UserID       string
	SpaceID      string // 空则用用户个人空间
	ParentID     string // 空则用空间根目录
	Name         string
	DeclaredSize int64
	DeclaredHash string
	// AllowOverwrite 为真表示调用方**显式**要覆盖同目录同名文件(客户端"替换上传")。
	// 默认 false 时同名直接 409 —— 绝不静默覆盖(6.7);真正的数据保护在 finalize,
	// 这里只是把用户的显式意图放行到定稿阶段。
	AllowOverwrite bool
}

// CreateResult 返回给客户端的结果。
//
// **Ticket 只在此处返回一次**;后续 PATCH/HEAD 通过 X-Upload-Token 头携带。
type CreateResult struct {
	UploadID  string    `json:"upload_id"`
	Ticket    string    `json:"upload_ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	// ResumableHint 提示客户端可用该 upload_id 续传
	SpaceID  string `json:"space_id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	// QuotaAfter 预留后的剩余额度(-1 = 不限制),便于前端即时反馈
	QuotaAfter int64 `json:"quota_after_bytes"`
	// FastUpload 非空表示预检命中且服务端已下发持物证明挑战 ——
	// 客户端按 sample_offsets 算出样本摘要,走 `POST /upload/{id}/finish` 直接定稿,
	// **无需重传整份内容**(6.10 五细则 / BE-S4-04)。
	//
	// 刻意是"提示"而不是"许可":挑战过了才算数,没过就得老实全量上传。
	FastUpload *fastupload.View `json:"fast_upload,omitempty"`
}

// Create 创建上传任务:校验命名 → 校验权限 → **预留额度** → 落 uploads 行。
//
// 顺序刻意如此:先做所有"便宜且会失败"的校验,最后才落库,
// 避免为无效请求频繁开启事务。
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	if s.DB == nil {
		return nil, apierr.Internal(errors.New("uploadsvc: DB 未装配"))
	}
	if in.DeclaredSize < 0 {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "文件大小不能为负")
	}
	// 1) 命名规则(6.7:服务器是唯一裁判,拒绝而非清洗)
	normName := namepolicy.Normalize(in.Name)
	if v, ok := s.Name.Validate(normName); !ok {
		return nil, nameViolationErr(v)
	}

	// 2) 定位空间与父目录
	spaceID := in.SpaceID
	if spaceID == "" {
		sp, err := s.Spaces.PersonalOf(ctx, s.DB, in.UserID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return nil, apierr.Internal(fmt.Errorf("用户缺少个人空间(数据不完整)"))
			}
			return nil, apierr.Internal(err)
		}
		spaceID = sp.ID
	}
	space, err := s.Spaces.GetByID(ctx, s.DB, spaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.SpaceGone("空间不存在或已被解散")
		}
		return nil, apierr.Internal(err)
	}
	if space.Frozen {
		return nil, apierr.SpaceRevoked("空间已被管理员冻结,暂不能上传")
	}

	parentID := in.ParentID
	if parentID == "" {
		root, err := s.Files.GetRoot(ctx, s.DB, spaceID)
		if err != nil {
			return nil, apierr.Internal(fmt.Errorf("空间根目录缺失: %w", err))
		}
		parentID = root.ID
	} else {
		// 父目录必须存在且属于同一空间(防跨空间越权)
		parent, err := s.Files.GetByID(ctx, s.DB, parentID)
		if err != nil {
			if errors.Is(err, repo.ErrNotFound) {
				return nil, apierr.NotFound("目标目录不存在")
			}
			return nil, apierr.Internal(err)
		}
		if parent.SpaceID != spaceID {
			return nil, apierr.Forbidden("目标目录不属于该空间")
		}
		if !parent.IsDir {
			return nil, apierr.BadRequest(apierr.CodeInvalidArgument, "目标不是目录")
		}
		// R-11:深度上限(累计路径字节数由 finalize/列表接口按完整路径校验)
		if v, ok := s.Name.CheckPath("", parent.Depth+1); !ok {
			return nil, nameViolationErr(v)
		}
	}

	// 3) 权限校验:当前用户必须对该空间有写权限(4.3 三级权限)
	if err := s.checkWritePerm(ctx, spaceID, in.UserID); err != nil {
		return nil, err
	}

	// 4) 命名冲突:同目录同名(大小写不敏感)直接拒绝,让客户端显式处理
	//    (6.7:绝不静默改名;覆盖写请走 finalize 的 base_version 语义)
	if _, err := s.Files.GetChildByName(ctx, s.DB, spaceID, parentID, normName); err == nil {
		// 显式覆盖:允许同名存在,定稿时让该 files 行**原地升版本**
		// (finalize.Input.AllowOverwrite,WebDAV PUT 走的就是同一条)
		if !in.AllowOverwrite {
			return nil, apierr.Conflict(apierr.CodeNameConflict, "同目录下已存在同名文件: %s", normName)
		}
	} else if !errors.Is(err, repo.ErrNotFound) {
		return nil, apierr.Internal(err)
	}
	// 4b) 还要挡住"进行中的同名上传":files 行要定稿才出现,只查 files 会漏掉
	//     两个客户端同时传同名文件的情况(第二个会白传整个文件才在定稿时失败)。
	//
	// 但"进行中"必须排除**已经死掉的**任务,否则一个失败/崩溃的上传会把名字锁到过期(默认 24h):
	// 实测(2026-09-13,用户报告):0 字节文件的旧客户端在定稿时调了秒传端点被 400 拒掉、
	// 又没有取消任务,于是同一目录下**三个空文件**(两小时前与十分钟前)全都卡在
	// 「同目录下已有一个正在上传的同名文件」—— 用户看到的是"这个文件怎么都同步不了"。
	// 两条判定(都要求**同一个用户**,绝不动别人的上传):
	//   · 已过期:票已经失效,直接回收;
	//   · 空任务且闲置超过 StaleUploadAfter:客户端建完任务**几秒内**就会发第一片,
	//     闲置这么久只说明它已经死了(崩溃/被杀/网络断),留着只会让用户无限期传不上去。
	if existing, err := s.Uploads.GetActiveByName(ctx, s.DB, spaceID, parentID, normName); err == nil {
		if existing.UserID == in.UserID && s.isStaleUpload(existing) {
			// 回收动作与客户端显式取消**走同一条路**(释放预留 + 清暂存),不是另一套清理逻辑
			if cerr := s.Cancel(ctx, existing.ID, existing.UserID); cerr != nil {
				// 回收失败就按原样拒绝:不能假装能建新任务(会留下两条同名任务)
				return nil, apierr.Conflict(apierr.CodeNameConflict,
					"同目录下已有一个正在上传的同名文件: %s(请稍后重试)", normName)
			}
		} else {
			return nil, apierr.Conflict(apierr.CodeNameConflict,
				"同目录下已有一个正在上传的同名文件: %s(请先取消或等待其完成)", normName)
		}
	} else if !errors.Is(err, repo.ErrNotFound) {
		return nil, apierr.Internal(err)
	}

	// 5) 生成 ticket(明文只返回一次,库内只存 SHA-256)
	ticket, ticketHash, err := NewTicket()
	if err != nil {
		return nil, apierr.Internal(err)
	}
	// 过期时刻只用**本服务时钟**算一次,并同时用于入库与响应(见 repo.CreateUploadInput.ExpiresAt)。
	expiresAt := s.now().Add(s.ticketTTL())

	var res *CreateResult
	err = s.DB.InTx(ctx, func(tx pgx.Tx) error {
		// 5a) 预留额度:条件 UPDATE 在 DB 层保证不超卖(4.3)
		sp, err := s.Spaces.Reserve(ctx, tx, spaceID, in.DeclaredSize)
		if err != nil {
			if errors.Is(err, repo.ErrQuotaExceeded) {
				return apierr.QuotaExceeded("空间额度不足:需要 %d 字节,剩余 %d 字节",
					in.DeclaredSize, maxInt64(0, space.QuotaBytes-space.UsedBytes))
			}
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在")
			}
			return apierr.Internal(err)
		}

		// 5b) 落任务行
		up, err := s.Uploads.Create(ctx, tx, repo.CreateUploadInput{
			UserID:         in.UserID,
			SpaceID:        spaceID,
			ParentID:       parentID,
			Name:           normName,
			DeclaredSize:   in.DeclaredSize,
			DeclaredHash:   in.DeclaredHash,
			TicketHash:     ticketHash,
			AllowOverwrite: in.AllowOverwrite,
			ExpiresAt:      expiresAt,
		})
		if err != nil {
			// 任务行写失败 → 预留必须回滚(0 行也算失败)
			return apierr.Internal(err)
		}

		after := int64(-1)
		if sp.QuotaBytes > 0 {
			after = sp.QuotaBytes - sp.UsedBytes
		}
		res = &CreateResult{
			UploadID:   up.ID,
			Ticket:     ticket,
			ExpiresAt:  up.ExpiresAt,
			SpaceID:    spaceID,
			ParentID:   parentID,
			Name:       normName,
			QuotaAfter: after,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// 6) 秒传预检(在事务之外做,且**失败不影响上传**)
	//
	// 为什么放在最后:挑战必须绑定 upload_id(上一步才生成),而"预检命中"这件事
	// 失败或 Redis 不可用都不该让建任务失败 —— 秒传是**体验优化**,
	// 全量上传才是保底路径(10.7:Redis 不做权威账)。
	res.FastUpload = s.precheckFastUpload(ctx, res.UploadID, in.DeclaredHash, in.DeclaredSize)
	return res, nil
}

// precheckFastUpload 判断能否给客户端"免重传"的机会,能则下发持物证明挑战。
//
// 三条纪律(6.10 防投毒):
//  1. **只信服务端已存的对象行** —— 客户端声明的 hash 只用于查表,不代表内容存在;
//     查到的对象必须可用(live/pending_delete/deleting)且 size 一致(去重键双校验)。
//  2. **任何异常都返回 nil**:Redis 不可用、挑战存储失败、查询出错都只是"这次不给秒传",
//     绝不能把 500 抛给一个本来能正常全量上传的客户端。
//  3. 小文件(默认 <32KB)一律不给通道:收益趋零,不值得为它开内容侧信口。
func (s *Service) precheckFastUpload(ctx context.Context, uploadID, declaredHash string, size int64) *fastupload.View {
	if s.Fast == nil || declaredHash == "" || size < s.fastMinSize() {
		return nil
	}
	ok, err := s.objectUsable(ctx, declaredHash, size)
	if err != nil || !ok {
		return nil
	}
	view, err := s.Fast.Issue(ctx, uploadID, declaredHash, size)
	if err != nil {
		return nil
	}
	return view
}

// objectUsable 查 (hash,size) 是否有可直接复用的对象行。
//
// 只读不写、也不需要事务:真正的裁决在 finalize(它会在对象锁下重查一次)。
// 这里只是"值不值得发挑战"的预判,所以允许有并发窗口 —— 预判错了最坏结果是
// 客户端拿到挑战却在 finish 时发现对象没了,那时 finalize 会明确拒绝并让它全量上传。
func (s *Service) objectUsable(ctx context.Context, hash string, size int64) (bool, error) {
	var n int
	err := s.DB.QueryRow(ctx, `
SELECT count(*) FROM file_objects
 WHERE hash_sha256 = $1 AND size = $2
   AND state IN ('live','pending_delete','deleting')`, hash, size).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Service) fastMinSize() int64 {
	return fastupload.MinSizeFor(s.FastMinSize)
}

// Authorize 校验 ticket 并返回任务(供 TUS PATCH/HEAD 调用)。
//
// 校验项:①upload 存在 ②ticket 匹配 ③属于同一用户 ④未过期 ⑤状态为 reserved。
// **ticket 可重复使用**(整个 upload 生命周期内),这与 TUS 多次 PATCH 的语义匹配。
func (s *Service) Authorize(ctx context.Context, uploadID, userID, ticket string) (*model.Upload, error) {
	up, err := s.loadAuthorized(ctx, uploadID, userID, ticket)
	if err != nil {
		return nil, err
	}
	if up.State != model.UploadReserved {
		return nil, apierr.Conflict(apierr.CodeUploadGone, "上传任务已结束(状态 %s)", up.State)
	}
	return up, nil
}

// AuthorizeOrFinished 与 Authorize 相同,但**允许已定稿**的任务通过,并指出这一点。
//
// 存在的理由(6.10 幂等):客户端可能在"服务端已定稿、响应在路上丢了"之后重试。
// 若这时按"任务已结束"报 409,客户端只能重新建任务、重新上传整份内容 ——
// 而服务端其实早已有结果。返回 finished=true 让调用方直接回既有结果。
//
// 注意 ticket 仍然必须校验:幂等重放不等于"凭 upload_id 就能查任何人的文件"。
func (s *Service) AuthorizeOrFinished(ctx context.Context, uploadID, userID, ticket string) (*model.Upload, bool, error) {
	up, err := s.loadAuthorized(ctx, uploadID, userID, ticket)
	if err != nil {
		return nil, false, err
	}
	switch up.State {
	case model.UploadFinalized:
		return up, true, nil
	case model.UploadReserved:
		return up, false, nil
	default:
		return nil, false, apierr.Conflict(apierr.CodeUploadGone, "上传任务已结束(状态 %s)", up.State)
	}
}

// loadAuthorized 做 ticket/归属/过期的公共校验(不判状态)。
func (s *Service) loadAuthorized(ctx context.Context, uploadID, userID, ticket string) (*model.Upload, error) {
	if uploadID == "" || ticket == "" {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "缺少上传凭据")
	}
	up, err := s.Uploads.GetByID(ctx, s.DB, uploadID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.NotFound("上传任务不存在")
		}
		return nil, apierr.Internal(err)
	}
	if up.UserID != userID {
		// 不暴露"存在但不属于你"的细节,统一 404 语义更安全
		return nil, apierr.NotFound("上传任务不存在")
	}
	if !TicketMatches(up, ticket) {
		return nil, apierr.Unauthorized(apierr.CodeUnauthorized, "上传凭据无效")
	}
	if s.now().After(up.ExpiresAt) {
		return nil, apierr.Unauthorized(apierr.CodeUploadGone, "上传凭据已过期,请重新创建上传任务")
	}
	return up, nil
}

// Cancel 取消上传并**即时释放预留**(6.3:不等 24h 回收班车)。
func (s *Service) Cancel(ctx context.Context, uploadID, userID string) error {
	up, err := s.Uploads.GetByID(ctx, s.DB, uploadID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.NotFound("上传任务不存在")
		}
		return apierr.Internal(err)
	}
	if up.UserID != userID {
		return apierr.NotFound("上传任务不存在")
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		released, err := s.Uploads.Release(ctx, tx, uploadID)
		if err != nil {
			if errors.Is(err, repo.ErrConflict) {
				return nil // 已完成或已释放:幂等
			}
			return apierr.Internal(err)
		}
		if err := s.Spaces.Release(ctx, tx, released.SpaceID, released.DeclaredSize); err != nil {
			return apierr.Internal(err)
		}
		return nil
	})
}

// ReleaseExpired 回收过期预留(兜底路径,主路径是客户端显式取消)。
//
// 走 `ClaimExpiredReleases` 的**原子认领**:并发的两个回收者拿到不相交的行,
// 且"置 released"与"拿到额度账本"是同一次原子操作 —— 不可能重复回退额度。
// (旧实现是"先列一批 id、再逐个条件更新",重复回退的风险靠调用方判对
// `Release` 的返回值来规避,而那种"靠自觉"的写法迟早被改坏。)
func (s *Service) ReleaseExpired(ctx context.Context, limit int) (int, error) {
	claimed, err := s.Uploads.ClaimExpiredReleases(ctx, s.DB, limit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, cl := range claimed {
		if rerr := s.DB.InTx(ctx, func(tx pgx.Tx) error {
			return s.Spaces.Release(ctx, tx, cl.SpaceID, cl.DeclaredSize)
		}); rerr != nil {
			return n, rerr
		}
		n++
	}
	return n, nil
}

// checkWritePerm 校验写权限(4.3)。
func (s *Service) checkWritePerm(ctx context.Context, spaceID, userID string) error {
	m, err := s.Spaces.GetMembership(ctx, s.DB, spaceID, userID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return apierr.SpaceGone("空间不存在或已被解散")
		}
		return apierr.Internal(err)
	}
	if m == nil {
		// 无成员关系(非 owner、也未被邀请)
		return apierr.SpaceRevoked("你没有该空间的访问权限")
	}
	if m.Permission == model.PermReader {
		return apierr.Forbidden("你在该空间的权限为只读,不能上传")
	}
	return nil
}

func (s *Service) ticketTTL() time.Duration {
	if s.TicketTTL > 0 {
		return s.TicketTTL
	}
	return 24 * time.Hour
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// DefaultStaleUploadAfter 是"空任务闲置多久算死掉"的默认阈值(见 create 的 4b 判定)。
//
// 30 分钟的依据:客户端**建完任务几秒内**就会发第一片 —— 能闲置半小时说明它已经死了。
// 阈值不能太短:慢网络下第一片(8MB)可能要几分钟;也不能太长:那等于让用户等一个
// 已经没救的任务(旧行为是等到票过期,默认 24h)。
const DefaultStaleUploadAfter = 30 * time.Minute

// isStaleUpload 判断一个**同名进行中**的任务是不是已经死掉、可以回收(调用方已确认同属一人)。
func (s *Service) isStaleUpload(up *model.Upload) bool {
	if up == nil {
		return false
	}
	if s.now().After(up.ExpiresAt) {
		return true // 票已失效:任务不可能再被推进
	}
	idle := s.now().Sub(up.CreatedAt)
	threshold := s.StaleUploadAfter
	if threshold <= 0 {
		threshold = DefaultStaleUploadAfter
	}
	// **只回收"一个字节都没收到"的任务**:已有进度的任务可能是用户暂停后续传的,
	// 回收它等于把人家已经传完的部分扔掉(那时用户看到的是"重传了整个文件")。
	return up.UploadedBytes == 0 && idle > threshold
}

// ---- ticket 生成与校验 ----

// NewTicket 生成 (明文, SHA-256 hex)。
//
// 明文 = base64url(32 随机字节),长度 43;入库只存 sha256 的 hex(64)。
func NewTicket() (plain string, hash string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("生成上传凭据失败: %w", err)
	}
	plain = base64.RawURLEncoding.EncodeToString(b[:])
	return plain, HashTicket(plain), nil
}

// HashTicket 计算 ticket 的 SHA-256(hex,小写)。
func HashTicket(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// TicketMatches 恒定时间比较 ticket 哈希(避免时序侧信道)。
func TicketMatches(up *model.Upload, plain string) bool {
	if up == nil {
		return false
	}
	got := HashTicket(plain)
	want := up.TicketHash
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

// nameViolationErr 把 namepolicy 违规转成结构化 400(6.7:违规位置 + 规则号 + 建议名)。
func nameViolationErr(v *namepolicy.Violation) *apierr.Error {
	e := apierr.BadRequest(apierr.CodeInvalidName, "%s", v.Message)
	e.WithDetail("rule", v.Rule)
	if v.Char != "" {
		e.WithDetail("char", v.Char)
	}
	if v.Position > 0 || v.Rule == namepolicy.RuleForbiddenChar {
		e.WithDetail("position", v.Position)
	}
	if v.Suggestion != "" {
		e.WithDetail("suggestion", v.Suggestion)
	}
	return e
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
