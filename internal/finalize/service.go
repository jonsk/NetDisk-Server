// Package finalize 实现 6.10 的**统一定稿路径** finalizeUpload()。
//
// 三条上传入口(TUS post-finish、WebDAV PUT、H5 multipart)必须收敛到这里的
// 同一个函数 —— 一致性规则(服务端实测哈希、引用计数、配额结算、变更流、
// 审计)只写一遍。任何"另起一套落盘逻辑"的写法都会让三条链路的行为漂移,
// 而漂移通常在"某条入口漏了配额结算"这类账实不符上才暴露。
//
// # 锁与事务边界(ADR-2 / 6.4 连接纪律,写错会静默失效)
//
//	conn := pool.Acquire()                        —— 全程仅此一条 *pgx.Conn
//	0. pg_advisory_lock(hashtextextended(hash,0)) —— 进任何事务之前拿锁
//	1. namepolicy 复检
//	2. 读一次内容:落暂存文件 + **同时**算 sha256(服务端实测值)
//	3. 锁下查 (hash,size) 行态(普通 SELECT,无需事务)
//	   - live/pending_delete/deleting 命中 → 复活(+1),**无需写对象**
//	   - 无此对象 → CommitStaged 在**事务外**执行(锁仍持有)
//	4. 单短事务:file_objects 复活/新建 → files 行 → 配额结算 → sync_feed → 审计
//	5. pg_advisory_unlock(断言返回 true) → conn.Release()
//	6. 提交后 PUBLISH 事件(6.9)
//
// **为什么必须会话级锁 + 单连接**:事务级锁随提交即释放,护不住"事务外写对象"
// 的窗口;而把对象写拉进事务又违 6.6 的"禁长事务跨 IO"。会话级锁贯穿
// "查→写对象→落元数据"整段。连接纪律是硬要求:若用 `pool.Exec` 式拿/解锁,
// 池会随机给出另一条连接 —— 在另一条连接上 unlock 返回 false 且无人检查
// (=锁永挂池连接、同 hash 的一切引用操作被阻塞),或连接中途归还
// (=锁静默释放、回到悬空窗口)。所以解锁**必须断言返回 true**。
//
// # 哈希与落盘为什么是"一趟"
//
// 6.10 要求服务端实测 sha256,而哈希只有读一遍内容才知道。若"先读一遍算哈希、
// 再读一遍写对象",每个上传的 IO 翻倍。因此顺序固定为:
// **读一次 → 落暂存文件并同时算哈希 → 用已知哈希提交为内容寻址对象**。
// 暂存文件同时就是 TUS 合并的中间产物(6.1 的 tus-tmp/merging)。
package finalize

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/objlock"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// Pool 是本包需要的最小连接池能力。
//
// 只暴露 Acquire:会话级 advisory 锁要求"同一条连接贯穿全程",
// 所以这里**刻意不提供** Exec/Query —— 一旦能直接查池,
// 就迟早有人写出"锁在 A 连接、查询在 B 连接"的代码。
type Pool interface {
	Acquire(ctx context.Context) (*pgxpool.Conn, error)
}

// Service 定稿用例入口。
type Service struct {
	Pool    Pool
	Storage storage.Storager
	Spaces  repo.SpaceRepo
	Files   repo.FileRepo
	Name    namepolicy.Policy
	// Feed 写入变更流(sync_feed);为 nil 时跳过(S9-01 实现前可空跑)
	Feed FeedWriter
	// Now 便于测试注入时钟
	Now func() time.Time
	// LockTimeout 是单轮持锁的应用层上限(不允许无限挂锁)
	LockTimeout time.Duration
	// AfterLock 是**测试专用**钩子:在拿到对象锁、开始处理之前调用一次。
	//
	// 生产装配下为 nil(零开销)。存在的理由:验证"锁真的被持有"必须**在持有期间**
	// 从另一条连接观测 pg_locks —— 靠 sleep 猜测持锁窗口既脆弱又慢,
	// 而这个不变量恰恰是 ADR-2 里"写错不报错"的那一条,值得一个确定性检查。
	AfterLock func(hash string)
	// OnLockHeld 是**测试专用**钩子:持锁段结束时回调一次,带上本次"锁内耗时"。
	//
	// 生产装配下为 nil(零开销)。存在的理由(BE-S10-06 验收①②):
	// "对象内容写入**不在**锁内"这条纪律**靠读代码守不住** —— 后来者只要把
	// `s.stage()` 挪进锁里(看起来更"内聚"),持锁时间就从毫秒变成"用户网速"下的
	// 秒级,同 hash 的并发定稿全部排队,而**没有任何报错**:只是变慢。
	// 所以用一个可测量的观测点把它钉成断言:P95 < 100ms 且"慢读者"不影响锁内耗时。
	OnLockHeld func(hash string, held time.Duration)
	// Fast 是秒传的持物证明校验器(BE-S4-04);无内容流的定稿必须经过它。
	// 为 nil 时只允许 AllowTrustedReuse 的内部复用路径。
	Fast ProofVerifier
}

// ProofVerifier 校验持物证明并一次性作废 nonce(由 fastupload.Service 满足)。
//
// 刻意只声明"取挑战 + 验证样本"两个方法:finalize 不该知道挑战存在 Redis 还是别处,
// 也不该知道样本怎么生成 —— 它只需要一个"过/不过"的答案。
type ProofVerifier interface {
	Take(ctx context.Context, uploadID, hash string) (*fastupload.Challenge, error)
	Verify(ctx context.Context, ch *fastupload.Challenge, got []string) error
}

// FeedWriter 追加变更流条目(6.9);由 S9-01 实现。
type FeedWriter interface {
	Append(ctx context.Context, q repo.Querier, spaceID, fileID, kind string) (int64, error)
}

// Input 是定稿入参(三条上传入口共用)。
type Input struct {
	// UploadID 非空时走 upload 任务表:定稿成功后置 finalized(幂等载体)
	UploadID string
	UserID   string
	SpaceID  string
	ParentID string
	Name     string
	// DeclaredSize 是客户端声明大小(用于预留与校验)
	DeclaredSize int64
	// DeclaredHash 是客户端声明哈希(**只用于预检,绝不作为落库依据**,R-05 防投毒)
	DeclaredHash string
	// MimeType 空则留默认值
	MimeType string
	// AllowOverwrite 表示调用方**显式**要覆盖同名文件(WebDAV PUT 的覆盖语义、
	// 客户端明确的"替换上传")。
	//
	// 默认 false 时,目标位置已有同名文件 → **409 name_conflict**。
	// 这是关键的数据保护:若默认允许覆盖,两个客户端同时向同名位置上传不同内容时,
	// 后到者会**静默覆盖**先到者 —— 用户看到的是"文件传上去了,内容却变了",
	// 而这正是 6.7"绝不静默改名/绝不静默覆盖"要禁止的。
	AllowOverwrite bool
	// Content 是文件内容流(调用方负责关闭);为 nil 表示纯引用复用,此时**必须**
	// 提供 Nonce + SampleSHA256 走持物证明(BE-S4-04),或由可信内部调用方显式
	// 声明 AllowTrustedReuse(见该字段说明)。
	Content io.Reader
	// Nonce 是秒传预检时下发的持物证明 nonce(6.10 五细则④)。
	Nonce string
	// SampleSHA256 是客户端对上一步下发偏移算出的样本摘要(顺序一一对应)。
	//
	// 只在 Nonce 非空时有意义。**它证明的是"客户端确实持有该内容"**,
	// 而不是"内容等于声明的 hash"(后者由内容寻址的对象行保证)。
	SampleSHA256 []string
	// AllowTrustedReuse 允许"无内容流且无持物证明"的直接引用复用。
	//
	// **只能由服务端内部调用方(测试、运维修复脚本)设置**,HTTP 入口必须走
	// Nonce 路径。存在的理由:没有它,内部调用方就得伪造一个 nonce;
	// 而一旦这个开关是"默认打开",R-05 的防投毒纪律就等于不存在 ——
	// 任何客户端报一个 hash 就能白拿内容。所以默认 false 且 HTTP 层从不设置。
	AllowTrustedReuse bool
}

// Result 是定稿结果。
type Result struct {
	File *model.File
	// Object 是本次涉及的物理对象行(新建或复活后的状态)
	Object *model.FileObject
	// ObjectReused 为 true 表示命中已有对象、**未把暂存文件提交为对象**
	Deduped bool
	// NewFile 为 true 表示新建了 files 行(否则是覆盖写)
	NewFile bool
	// ChangeSeq 是本次变更在变更流中的序号(S9 用于 SSE 帧 id);Feed 为空时为 0
	ChangeSeq int64
	// Took 是定稿端到端耗时
	Took time.Duration
}

// Finalize 执行统一定稿(6.10)。
func (s *Service) Finalize(ctx context.Context, in Input) (*Result, error) {
	started := s.now()
	if err := s.validate(in); err != nil {
		return nil, err
	}

	// 1) namepolicy 复检。
	// Upload-Metadata 里的名字**可能是旧客户端或不校验的客户端产生的**,
	// 所以创建时校验过不算数 —— 定稿是"名字真正落库"的时刻,必须再查一遍(6.7)。
	name := namepolicy.Normalize(in.Name)
	if v, ok := s.Name.Validate(name); !ok {
		return nil, nameViolationErr(v)
	}

	// 2) 读一次内容:落暂存文件 + 同时算实测 sha256
	stagePath, hash, size, err := s.stage(ctx, in)
	if err != nil {
		return nil, err
	}
	if stagePath != "" {
		// 任何路径下都必须清掉暂存文件:提交成功时它已被 rename(Remove 无害),
		// 失败时它是垃圾。放在这里而不是各分支,避免漏清。
		defer func() { _ = os.Remove(stagePath) }()
	}

	// 3~6) 会话级对象锁下的 "查行态 → (必要时)提交对象 → 单短事务"。
	//
	// **锁纪律统一由 objlock.Session 提供**(不再在本文件里手写一遍):
	// 会话级 advisory 锁只作用于拿到它的那条连接,于是"拿锁→干活→解锁"必须
	// 全程绑在同一条 `*pgx.Conn` 上,且解锁必须**断言返回 true**。
	// 这段代码此前在本文件与 lifecycle worker 里各写了一遍 —— 两处实现同一纪律
	// 意味着任何一处将来被改动(例如有人"顺手"改成池式拿锁),另一处不会跟着变,
	// 而这类错误**写错不报错**(表现为锁永久挂在池里、同 hash 的一切引用操作
	// 被无限阻塞,且没有任何日志线索)。所以改为共用同一个函数。
	var res *Result
	reused := false
	err = objlock.Session(ctx, s.Pool, hash, s.LockTimeout, func(ctx context.Context, conn *pgx.Conn) error {
		// 持锁段计时(BE-S10-06 验收②):**回调的第一条语句**就是起点,终点在
		// 回调返回之前 —— 于是"锁内耗时"覆盖整个锁内区间(不含拿锁排队与解锁往返)。
		//
		// 起点必须在这里而不是钩子之后:反向验证时把一段 sleep 插在钩子**之前**,
		// 若计时从钩子后才开始就量不到它 —— 这条断言也就形同虚设(实测踩过)。
		lockStart := time.Now()
		if s.OnLockHeld != nil {
			defer func() { s.OnLockHeld(hash, time.Since(lockStart)) }()
		}
		if s.AfterLock != nil {
			s.AfterLock(hash)
		}
		// 3) 锁下查 (hash,size) 行态(普通 SELECT,无需事务)。
		//    锁下读保证看到的态不会被并发 worker 中途改动 —— 这正是会话锁的意义。
		existing, gerr := getObject(ctx, conn, hash)
		if gerr != nil && !errors.Is(gerr, repo.ErrNotFound) {
			return apierr.Internal(gerr)
		}

		// 4) 决定能否复用已有对象。
		//
		// **必须同时校验"数据库行可用"与"物理对象真的在"**:
		// 只信数据库行会在下列场景产生**悬空指针且请求成功** ——
		//   - 存储侧对象被误删 / 迁移丢失 / 备份回滚后元数据比对象新
		//   - 上一轮清理只删了物理对象、标记 deleted 前进程崩溃
		// 这类"元数据说在、实际不在"的错误必须在这里被拦下并回落为重新写入,
		// 否则用户拿到 201、文件列表也显示正常,但下载永远 404 或返回半截内容。
		objUsable := usableObject(existing, size)
		if objUsable {
			if _, serr := s.Storage.Stat(ctx, hash); serr != nil {
				if !errors.Is(serr, storage.ErrNotFound) {
					return apierr.Internal(fmt.Errorf("校验对象存在性失败: %w", serr))
				}
				objUsable = false
			}
		}
		if !objUsable {
			if stagePath == "" {
				// 无可用对象又没给内容流 → 调用方 bug(或秒传误判)。
				// 显式报错而不是落一个空文件。
				return apierr.BadRequest(apierr.CodeFastUploadDenied,
					"服务端没有该内容的对象,需要上传文件内容")
			}
			sp, ok := s.Storage.(storage.Sponsorable)
			if !ok {
				return apierr.Internal(errors.New("finalize: 存储后端不支持暂存提交"))
			}
			// **对象提交在事务外**(6.6 禁长事务跨 IO):锁仍持有,
			// 并发新建/清理都看不到中间态(6.5 规则 5"先写对象再短事务写元数据")
			if cerr := sp.CommitStaged(ctx, hash, stagePath, size); cerr != nil {
				return apierr.Internal(fmt.Errorf("提交对象失败: %w", cerr))
			}
		} else {
			// 命中已有**且物理存在**的对象:暂存文件没有用了(内容寻址下内容必然相同)
			reused = true
			if stagePath != "" {
				_ = os.Remove(stagePath)
			}
		}

		// 5) 单短事务:引用 + files + 配额 + feed + 审计
		r, cerr := s.commit(ctx, conn, in, name, hash, size, existing)
		if cerr != nil {
			return cerr
		}
		res = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Deduped = reused
	res.Took = time.Since(started)
	return res, nil
}

// stage 读一次内容,返回 (暂存路径, 实测哈希, 字节数)。
//
// 无内容流时走**纯引用复用**分支,该分支必须已被持物证明校验过(BE-S4-04):
//
//	Content == nil + Nonce != ""                → 证明通过后才允许复用(唯一 HTTP 路径)
//	Content == nil + AllowTrustedReuse == true  → 服务端内部调用方(测试/修复脚本)
//	Content == nil + 两者都没有                 → **拒绝**(否则任何人都能凭 hash 白拿内容)
//
// 顺序上先证明再复用:证明失败时**必须**让调用方老实重传,绝不能"证明没过就去读对象" ——
// 那等于把挑战当装饰。
func (s *Service) stage(ctx context.Context, in Input) (string, string, int64, error) {
	if in.Content == nil {
		if in.DeclaredHash == "" {
			return "", "", 0, apierr.BadRequest(apierr.CodeInvalidArgument,
				"缺少内容流时必须提供 hash")
		}
		h, err := storage.HexSHA256(in.DeclaredHash)
		if err != nil {
			return "", "", 0, apierr.BadRequest(apierr.CodeInvalidArgument, "hash 非法")
		}
		if in.Nonce != "" {
			if err := s.verifyProof(ctx, in, h); err != nil {
				return "", "", 0, err
			}
		} else if !in.AllowTrustedReuse {
			// R-05 防投毒的最后一道闸:没有内容、没有证明、也没被内部放行 → 拒绝。
			// 这条分支一旦被放行,"秒传"就退化成"报 hash 领文件"。
			return "", "", 0, apierr.BadRequest(apierr.CodeFastUploadDenied,
				"缺少内容流时必须提供持物证明 nonce")
		}
		return "", h, in.DeclaredSize, nil
	}
	sp, ok := s.Storage.(storage.Sponsorable)
	if !ok {
		return "", "", 0, apierr.Internal(errors.New("finalize: 存储后端不支持暂存"))
	}
	stagePath, hash, size, err := sp.StageFrom(ctx, in.Content)
	if err != nil {
		return "", "", 0, apierr.Internal(fmt.Errorf("读取上传内容失败: %w", err))
	}
	if size != in.DeclaredSize {
		_ = os.Remove(stagePath)
		// 声明大小与实际不符:可能是客户端撒谎,也可能是传输被截断。
		// 用 400 而不是 500 —— 这是客户端的问题,且已有预留额度按实际结算。
		return "", "", 0, apierr.BadRequest(apierr.CodeInvalidArgument,
			"实际大小 %d 与声明 %d 不符", size, in.DeclaredSize)
	}
	return stagePath, hash, size, nil
}

// verifyProof 校验持物证明(6.10 五细则②③④)。
//
// 取挑战是**一次性**的(Take 内部先校验后删除):同一个 nonce 只能完成一次定稿,
// 否则"证明一次、白拿无数次"。失败一律返回 403 `fast_upload_denied` ——
// 调用方(HTTP 层)据此计限速并写审计。
func (s *Service) verifyProof(ctx context.Context, in Input, hash string) error {
	if s.Fast == nil {
		return apierr.BadRequest(apierr.CodeFastUploadDenied,
			"服务端未启用秒传校验,请全量上传")
	}
	if in.UploadID == "" {
		// 挑战绑定 upload_id:没有任务就无法证明"这次请求对应那次预检"
		return apierr.BadRequest(apierr.CodeFastUploadDenied,
			"持物证明必须绑定上传任务")
	}
	ch, err := s.Fast.Take(ctx, in.UploadID, hash)
	if err != nil {
		return apierr.ForbiddenCode(apierr.CodeFastUploadDenied,
			"持物证明无效或已过期,请全量上传").WithCause(err)
	}
	if err := s.Fast.Verify(ctx, ch, in.SampleSHA256); err != nil {
		return apierr.ForbiddenCode(apierr.CodeFastUploadDenied,
			"持物证明未通过(样本摘要不匹配),请全量上传").WithCause(err)
	}
	// 细则④:复核对象仍可用。挑战下发到定稿之间可能已经过期回收/被清理,
	// 那时"内容取自已存对象"这句话就不成立了 —— 必须让客户端全量上传。
	//
	// 注意这里**不锁**:真正的裁决是紧接着在对象锁下做的 getObject + usableObject。
	// 此处只是尽早失败,省掉一次无谓的对象锁竞争。
	ok, qerr := s.objectUsableQuick(ctx, hash, in.DeclaredSize)
	if qerr != nil {
		return apierr.Internal(qerr)
	}
	if !ok {
		return apierr.ForbiddenCode(apierr.CodeFastUploadDenied,
			"该内容已不在服务端,请全量上传")
	}
	return nil
}

// objectUsableQuick 在拿锁之前先看一眼对象行是否可用(尽力而为的快速失败)。
func (s *Service) objectUsableQuick(ctx context.Context, hash string, size int64) (bool, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var n int
	err = conn.QueryRow(ctx, `
SELECT count(*) FROM file_objects
 WHERE hash_sha256 = $1 AND size = $2
   AND state IN ('live','pending_delete','deleting')`, hash, size).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// validate 做便宜的入参校验(在拿锁之前,避免为无效请求占锁)。
func (s *Service) validate(in Input) error {
	if s.Pool == nil {
		return apierr.Internal(errors.New("finalize: Pool 未装配"))
	}
	if s.Storage == nil {
		return apierr.Internal(errors.New("finalize: Storage 未装配"))
	}
	if strings.TrimSpace(in.UserID) == "" {
		return apierr.Unauthorized(apierr.CodeUnauthorized, "未认证")
	}
	if strings.TrimSpace(in.SpaceID) == "" {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "space_id 不能为空")
	}
	if strings.TrimSpace(in.ParentID) == "" {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "parent_id 不能为空")
	}
	if strings.TrimSpace(in.Name) == "" {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "文件名不能为空")
	}
	if in.DeclaredSize < 0 {
		return apierr.BadRequest(apierr.CodeInvalidArgument, "文件大小不能为负")
	}
	if in.DeclaredHash != "" {
		if _, err := storage.HexSHA256(in.DeclaredHash); err != nil {
			return apierr.BadRequest(apierr.CodeInvalidArgument, "hash 必须是 64 位十六进制")
		}
	}
	return nil
}

// usableObject 判断已有对象行能否直接复用。
//
// 只有 live / pending_delete / deleting 且**大小一致**才可复用:
//   - deleted:对象已物理删除(或即将),复用会得到悬空指针
//   - 大小不一致:内容寻址下不可能(同 hash 必然同内容),说明数据被破坏,
//     必须走"重新写入"分支而不是相信旧行
func usableObject(o *model.FileObject, size int64) bool {
	if o == nil || o.Size != size {
		return false
	}
	switch o.State {
	case model.ObjectLive, model.ObjectPendingDelete, model.ObjectDeleting:
		return true
	default:
		return false
	}
}

// getObject 锁下读取对象行(普通 SELECT)。
func getObject(ctx context.Context, q repo.Querier, hash string) (*model.FileObject, error) {
	var o model.FileObject
	err := q.QueryRow(ctx, `
SELECT hash_sha256, size, storage_backend, object_key, ref_count, state,
       delete_after, created_at, updated_at
  FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(
		&o.HashSHA256, &o.Size, &o.StorageBackend, &o.ObjectKey, &o.RefCount, &o.State,
		&o.DeleteAfter, &o.CreatedAt, &o.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, repo.ErrNotFound
		}
		return nil, err
	}
	return &o, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// nameViolationErr 把 namepolicy 违规转成结构化 400(与 uploadsvc 同一形状)。
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
