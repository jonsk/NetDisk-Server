package uploadsvc

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
)

// 本文件是 **TUS 协议的数据面**(6.1 / 6.2 / 3.4):
// POST 建任务 → PATCH 追写分片 → HEAD 取偏移 → DELETE 取消,分片落 tus-tmp,
// **写完即定稿**(3.4 步骤 3~4:post-finish 合并 → finalizeUpload)。
//
// 为什么"写完即定稿"而不是"合并后异步定稿":
// 架构 6.2 明确废止了"直存暂存区、异步转存储后端"的旧设计 —— 异步化会让
// 客户端拿到 2xx 之后立刻 GET/PROPFIND 却读不到内容(6.5 规则 5 的矛盾)。
// 请求内定稿的代价只是一次落盘 + 一个短事务,换来"2xx 之后必得完整文件"。

// TUSStager 是暂存区能力(由 storage.FS 实现)。
//
// 单独声明接口而不是直接用 *storage.FS:便于测试注入"写一半失败"等场景,
// 也让 uploadsvc 不依赖具体存储后端。
type TUSStager interface {
	StagePath(uploadID string) (string, error)
	StageSize(uploadID string) (int64, error)
	StageAppend(ctx context.Context, uploadID string, expectOffset int64, r io.Reader) (int64, error)
	StageOpen(uploadID string) (*os.File, error)
	StageRemove(uploadID string) error
}

// Finalizer 是定稿能力(finalize.Service 实现)。
//
// uploadsvc 只依赖这个窄接口,因此 TUS 数据面与定稿逻辑可以独立测试。
type Finalizer interface {
	Finalize(ctx context.Context, in finalize.Input) (*finalize.Result, error)
}

// TUSService 承载 TUS 数据面。
type TUSService struct {
	Service   *Service  // 复用建任务/ticket 校验/取消
	Stager    TUSStager //
	Finalizer Finalizer
	// DB 用于读取 upload 行(定稿前的最后状态检查)
	DB DB
}

// TUSStatus 是 HEAD 的返回。
type TUSStatus struct {
	UploadID string
	Offset   int64
	Length   int64
	State    string
}

// Head 返回当前已接收偏移与声明长度(TUS HEAD 语义)。
//
// 偏移取自**暂存文件实际长度**(断点续传的真相),而不是 uploads 行里的计数字段:
// 两者不一致时以文件为准,否则客户端会按错误偏移继续写,导致内容损坏。
// 若文件比数据库记录长(上次 PATCH 写完但更新计数失败),顺带把计数修正过来。
func (s *TUSService) Head(ctx context.Context, uploadID, userID, ticket string) (*TUSStatus, error) {
	up, err := s.Service.Authorize(ctx, uploadID, userID, ticket)
	if err != nil {
		return nil, err
	}
	size, err := s.Stager.StageSize(uploadID)
	if err != nil {
		return nil, apierr.Internal(err)
	}
	if size != up.UploadedBytes {
		// 以文件为准并修正计数(单调推进,不会回退)
		if _, uerr := s.Service.Uploads.SetUploaded(ctx, s.DB, uploadID, size); uerr != nil &&
			!errors.Is(uerr, repo.ErrConflict) {
			return nil, apierr.Internal(uerr)
		}
	}
	return &TUSStatus{
		UploadID: uploadID, Offset: size, Length: up.DeclaredSize, State: up.State,
	}, nil
}

// PatchResult 是 PATCH 的结果。
type PatchResult struct {
	Offset int64
	// Finalized 为 true 表示本次 PATCH 使数据达到声明长度并已定稿
	Finalized bool
	// File 定稿产物(仅 Finalized 时非 nil)
	File *model.File
	// NewFile 表示本次定稿**新建**了文件(而不是覆盖既有文件)。
	// api 层据此推 FeedCreated / FeedUpdated —— 变更流的 kind 决定客户端
	// 是"新增一个条目"还是"更新已有条目",猜错会让新文件在客户端侧丢失。
	NewFile bool
}

// PatchInput 是一次 TUS PATCH 的入参。
type PatchInput struct {
	UploadID string
	UserID   string
	Ticket   string
	// ExpectOffset 是客户端声称的起始偏移(Upload-Offset 头)
	ExpectOffset int64
	// Content 是分片数据流
	Content io.Reader
	// ContentLength 是分片长度;**-1 表示未知**(分块传输)。
	// 已知时用于"落盘前先做上界校验",避免先写超量数据再被数据库约束拒绝(500)。
	ContentLength int64
}

// Patch 追写一段分片(TUS PATCH 语义)。
//
// 步骤与约束:
//  1. 校验 ticket 与任务状态(reserved)
//  2. 已知长度时先做上界校验(避免超量写入后才报错,那会变成 500 而不是 400)
//  3. 期望偏移必须等于**暂存文件实际长度**,否则 409 + 真实偏移
//  4. 追加写(落 tus-tmp),然后**单调推进** uploads.uploaded_bytes
//  5. 若达到声明长度 → 打开暂存文件、调用统一定稿函数 finalizeUpload(6.10),
//     成功后清理暂存文件
//
// 为什么先写文件再更新计数:反过来的话,计数先行而写盘失败,客户端下次 HEAD
// 会拿到一个"声称已接收但文件里没有"的偏移,继续写就会**在错误位置落数据**。
// 文件先行的代价只是"文件比计数长",而 HEAD 会以文件为准并把计数修正过来。
func (s *TUSService) Patch(ctx context.Context, in PatchInput) (*PatchResult, error) {
	up, err := s.Service.Authorize(ctx, in.UploadID, in.UserID, in.Ticket)
	if err != nil {
		return nil, err
	}
	uploadID := in.UploadID

	// **先按声明长度做上界校验,再落盘**。
	//
	// 顺序很重要:若先写再校验,超量分片已经落进暂存文件,而
	// `uploads.uploaded_bytes` 上的 CHECK 约束(`uploaded_bytes <= declared_size`)
	// 会拒绝推进计数 —— 于是抛出一个**数据库约束错误**,被映射成 500 而不是 400。
	// 客户端看到 500 会认为"服务端故障"并重试,而真正原因是它自己的请求超量。
	if in.ContentLength > 0 && in.ExpectOffset+in.ContentLength > up.DeclaredSize {
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument,
			"上传数据超出声明长度:偏移 %d + 本次 %d > 声明 %d",
			in.ExpectOffset, in.ContentLength, up.DeclaredSize)
	}

	newOffset, err := s.Stager.StageAppend(ctx, uploadID, in.ExpectOffset, in.Content)
	if err != nil {
		if errors.Is(err, storage.ErrOffsetMismatch) {
			// TUS 规范:偏移不符返回 409,并在响应头带服务端真实偏移,
			// 客户端据此自我纠正而不是整个重传
			e := apierr.Conflict(apierr.CodeVersionConflict,
				"上传偏移不符:服务端已接收 %d 字节,本次请求声称从 %d 开始", newOffset, in.ExpectOffset)
			return nil, e.WithDetail("upload_offset", newOffset)
		}
		return nil, apierr.Internal(err)
	}

	// 单调推进(重复/乱序分片不会把进度写回去;ErrConflict 表示该分片是重放)
	if _, uerr := s.Service.Uploads.SetUploaded(ctx, s.DB, uploadID, newOffset); uerr != nil {
		if !errors.Is(uerr, repo.ErrConflict) {
			return nil, apierr.Internal(uerr)
		}
		// 计数未推进(重放),但文件已按偏移写入正确位置 → 视为成功
	}

	res := &PatchResult{Offset: newOffset}
	if newOffset < up.DeclaredSize {
		return res, nil
	}
	if newOffset > up.DeclaredSize {
		// 超出声明长度:数据已经写多了,后续定稿必然按声明长度截断,
		// 直接报错让客户端重新建任务更清晰(而不是静默丢尾)
		_ = s.Stager.StageRemove(uploadID)
		return nil, apierr.BadRequest(apierr.CodeInvalidArgument,
			"上传数据超出声明长度(%d > %d)", newOffset, up.DeclaredSize)
	}

	// ---- 写完即定稿(3.4 步骤 3~4:post-finish 合并 → 统一定稿函数) ----
	final, err := s.finalizeFromStage(ctx, up)
	if err != nil {
		// 定稿失败:uploads 行仍为 reserved、暂存文件**保留**,
		// 客户端可 HEAD 看偏移(已是全长)后重新 PATCH 触发定稿重试。
		return nil, err
	}
	res.Finalized = true
	res.File = final.File
	// NewFile 透传定稿结果的新建标记:api 层据此决定推 FeedCreated 还是 FeedUpdated。
	// **不能由调用方猜**(例如"version==1 即新建")—— 版本号在覆盖上传等路径上
	// 并不等价于"新建",猜错会让客户端把新文件当更新处理,从而**漏掉这个文件**。
	res.NewFile = final.NewFile
	// 定稿成功后暂存文件已无用(内容已成为内容寻址对象)。
	// 清理失败不影响结果:残留文件由暂存目录清理任务兜底(6.1 的 24h 清理)。
	_ = s.Stager.StageRemove(uploadID)
	return res, nil
}

// FinishFast 完成**秒传定稿**:不带内容流,靠持物证明通过后直接复用已存对象。
//
// 与 PATCH 的关系:同样是"收敛到同一个 finalizeUpload",
// 差别只在**内容从哪来** —— 前者是客户端刚传上来的字节,这里是服务端**已有的对象**。
// 因此这里绝不做任何对象写:证明通过 = 客户端确实持有该内容,而内容寻址保证
// 那份内容就是声明的 hash 对应的字节。
//
// 若证明没过,finalize 会返回 `fast_upload_denied`;调用方(HTTP 层)据此计限速并写审计,
// 客户端则回到全量上传路径。**这条失败路径必须存在**:否则"声称一个 hash 就白拿内容"
// 会变成一条无成本的攻击通道(R-05)。
func (s *TUSService) FinishFast(ctx context.Context, up *model.Upload, nonce string, samples []string) (*finalize.Result, error) {
	if up == nil {
		return nil, apierr.Internal(errors.New("FinishFast: upload 任务为空"))
	}
	if nonce == "" {
		return nil, apierr.BadRequest(apierr.CodeFastUploadDenied, "缺少持物证明 nonce")
	}
	if up.DeclaredHash == "" {
		// 预检时没带 hash 就不该有挑战;真出现说明客户端在编
		return nil, apierr.BadRequest(apierr.CodeFastUploadDenied,
			"该上传任务没有声明 hash,不能走秒传")
	}
	res, err := s.Finalizer.Finalize(ctx, finalize.Input{
		UploadID:     up.ID,
		UserID:       up.UserID,
		SpaceID:      up.SpaceID,
		ParentID:     up.ParentID,
		Name:         up.Name,
		DeclaredSize: up.DeclaredSize,
		DeclaredHash: up.DeclaredHash,
		AllowOverwrite: up.AllowOverwrite,
		Nonce:        nonce,
		SampleSHA256: samples,
	})
	if err != nil {
		return nil, err
	}
	// 秒传路径没有暂存文件;若上一轮全量上传留下过残留,顺手清掉(内容已入库,残留无用)
	_ = s.Stager.StageRemove(up.ID)
	return res, nil
}

// finalizeFromStage 打开暂存文件并交给统一定稿函数。
//
// 关键:定稿时把声明哈希留空 —— **不接受客户端声明值作为落库依据**,
// 由 finalize 内部实测 sha256(R-05 防投毒)。
func (s *TUSService) finalizeFromStage(ctx context.Context, up *model.Upload) (*finalize.Result, error) {
	fh, err := s.Stager.StageOpen(up.ID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, apierr.NotFound("上传数据不完整:暂存文件不存在")
		}
		return nil, apierr.Internal(err)
	}
	defer func() { _ = fh.Close() }()

	res, err := s.Finalizer.Finalize(ctx, finalize.Input{
		UploadID:     up.ID,
		UserID:       up.UserID,
		SpaceID:      up.SpaceID,
		ParentID:     up.ParentID,
		Name:         up.Name,
		DeclaredSize: up.DeclaredSize,
		// DeclaredHash 留空:6.10 要求服务端实测哈希;客户端声明值只用于秒传预检(S4-04)
		Content: fh,
		// 覆盖意图**必须**从任务行带过来:全量上传(TUS)走的正是这个调用点,
		// 漏掉它的话"改本地已有文件"在最后一片定稿时会被 409 name_conflict 拒掉,
		// 而前面所有字节都已经传完 —— 白传。
		AllowOverwrite: up.AllowOverwrite,
	})
	if err != nil {
		// 定稿失败:uploads 行仍为 reserved、暂存文件保留,
		// 客户端可以 HEAD 看偏移(已是全长)后重新 PATCH 触发定稿重试。
		return nil, err
	}
	return res, nil
}

// Cancel 取消上传:释放预留额度并**删除暂存文件**(6.3:取消即释放,不等回收班车)。
func (s *TUSService) Cancel(ctx context.Context, uploadID, userID string) error {
	if err := s.Service.Cancel(ctx, uploadID, userID); err != nil {
		return err
	}
	// 额度已释放;暂存文件删除失败不应让客户端以为取消失败
	_ = s.Stager.StageRemove(uploadID)
	return nil
}
