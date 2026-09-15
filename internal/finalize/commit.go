package finalize

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// commit 是定稿的**单短事务**(6.10 步骤 4)。
//
// 硬性纪律:**事务内零对象 IO**(6.6)。对象字节已在事务外写好,这里只碰元数据。
// 这条纪律的理由不是洁癖:对象 IO 可能耗时数秒(GB 级),把它放进事务会让
// 行锁/连接被长时间占用,而 PostgreSQL 的 MVCC 又会因长事务拖住 vacuum
// (表膨胀、xid 回卷风险)。
//
// 锁序固定为 **spaces → files → file_objects**(6.6 全局锁序,覆盖配额结算、
// COPY、finalize 全路径),否则并发路径会互相死锁:
//   - spaces:配额结算(预留 → 按实际结算,释放差额)
//   - files:目标行的行锁 / 唯一索引(覆盖写、新建)
//   - file_objects:引用计数 +1(复活或新建)
func (s *Service) commit(
	ctx context.Context,
	conn *pgx.Conn,
	in Input,
	name, hash string,
	size int64,
	existing *model.FileObject,
) (*Result, error) {
	res := &Result{}
	err := inTx(ctx, conn, func(tx pgx.Tx) error {
		// ---- (1) spaces:配额结算(按**实际** size 结算,释放预留差额)----
		// 声明 1GB 实际 10MB 是常见情况(客户端按上限预留),差额必须在此释放,
		// 否则用户的额度会被"预留了但没用上"的部分永久占住。
		if err := s.settleQuota(ctx, tx, in, size); err != nil {
			return err
		}

		// ---- (2) files:新建或覆盖写 ----
		target, newFile, prevHash, prevSize, err := s.upsertFileRow(ctx, tx, in, name, hash, size)
		if err != nil {
			return err
		}
		res.File = target
		res.NewFile = newFile

		// ---- (3) file_objects:引用 +1(复活或新建)----
		obj, err := bumpObjectRef(ctx, tx, hash, size, s.Storage.Backend(), s.Storage.KeyFor(hash))
		if err != nil {
			return err
		}
		res.Object = obj

		// 覆盖写:旧内容引用 -1,归零则转 pending_delete(延迟删除窗口)
		if !newFile && prevHash != "" && prevHash != hash {
			if err := releaseObjectRef(ctx, tx, prevHash, prevSize); err != nil {
				return err
			}
		}

		// ---- (4) sync_feed:追加变更(全局 BIGSERIAL change_seq)----
		if s.Feed != nil {
			seq, err := s.Feed.Append(ctx, tx, in.SpaceID, target.ID, feedKind(newFile))
			if err != nil {
				return apierr.Internal(fmt.Errorf("写入变更流失败: %w", err))
			}
			res.ChangeSeq = seq
		}

		// ---- (5) upload 任务表:置 finalized(定稿幂等的载体)----
		if in.UploadID != "" {
			if err := finalizeUploadTask(ctx, tx, in.UploadID, hash, size, target.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// inTx 是本包的事务封装(与 db.InTx 同语义,但作用于已持有的连接)。
//
// 刻意不复用 db.InTx:`db.InTx` 从池里另取连接,而本包**必须**用那条已持锁的
// 连接 —— 会话级 advisory 锁只在那一条连接上有效。这正是包注释里
// "连接纪律"的具体体现:任何"顺手用池"的写法都会让锁失效。
func inTx(ctx context.Context, conn *pgx.Conn, fn func(tx pgx.Tx) error) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return apierr.Internal(fmt.Errorf("begin: %w", err))
	}
	committed := false
	defer func() {
		if !committed {
			// 用独立 context 回滚:外层 ctx 可能已取消,那时回滚会失败并泄漏连接
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rctx)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return apierr.Internal(fmt.Errorf("commit: %w", err))
	}
	committed = true
	return nil
}

// settleQuota 按实际 size 结算配额(4.3 预留-结算两段式)。
//
// **结算基准是"当初真正预留了多少",而不是本次调用传进来的声明值。**
// 预留账本在 `uploads.declared_size`(创建时按它调用了 Reserve),所以必须从
// 库里读它来算差额。曾经用"本次声明的 size"当基准,结果是:
// 创建时按 1MB 预留、定稿时声明也是 1MB 但实际只传 512KB → 差额 0 →
// 那 512KB 的预留**永远释放不掉**,用户额度被永久占住。
//
// 三条路径:
//   - 有 upload 任务:差额 = 实际 - 预留。为正则补扣(条件更新防超卖),
//     为负则释放多占的部分
//   - 无 upload 任务(例如 WebDAV PUT 不走 create):按实际直接占用
func (s *Service) settleQuota(ctx context.Context, tx pgx.Tx, in Input, actual int64) error {
	var reserved int64
	haveReservation := false
	if in.UploadID != "" {
		err := tx.QueryRow(ctx, `SELECT declared_size FROM uploads WHERE id = $1`, in.UploadID).Scan(&reserved)
		switch {
		case err == nil:
			haveReservation = true
		case errors.Is(err, pgx.ErrNoRows):
			// 任务不存在:finalizeUploadTask 稍后会报 404,这里按无预留处理
		default:
			return apierr.Internal(fmt.Errorf("读取预留额度失败: %w", err))
		}
	}

	if !haveReservation {
		if actual == 0 {
			return nil
		}
		if _, err := s.Spaces.Reserve(ctx, tx, in.SpaceID, actual); err != nil {
			if errors.Is(err, repo.ErrQuotaExceeded) {
				return apierr.QuotaExceeded("空间额度不足:需要 %d 字节", actual)
			}
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在")
			}
			return apierr.Internal(err)
		}
		return nil
	}

	switch {
	case actual == reserved:
		return nil
	case actual < reserved:
		// 多预留的部分立即释放(用户额度不该被"预留了但没用上"占住)
		return s.Spaces.Release(ctx, tx, in.SpaceID, reserved-actual)
	default:
		// 实际超出预留:补扣,额度不足在此拒绝并整体回滚
		if _, err := s.Spaces.Reserve(ctx, tx, in.SpaceID, actual-reserved); err != nil {
			if errors.Is(err, repo.ErrQuotaExceeded) {
				return apierr.QuotaExceeded("空间额度不足:需要 %d 字节(已预留 %d)", actual, reserved)
			}
			if errors.Is(err, repo.ErrNotFound) {
				return apierr.SpaceGone("空间不存在")
			}
			return apierr.Internal(err)
		}
		return nil
	}
}

// upsertFileRow 落 files 行:新建或覆盖写(6.5 规则 4 的"覆盖写四步"落点)。
//
// 覆盖写的**四步顺序固定且同一事务**(6.5 规则 4):
//  1. 新 sha256 → 新/复活 file_objects 行(在上面的 bumpObjectRef)
//  2. 更新 files.hash_sha256 / size / **version+1**
//  3. 旧 hash ref_count-1,归零入 pending_delete(下面的 releaseObjectRef)
//  4. 全部在同一事务 —— 任一步失败整体回滚,绝不出现"新内容已指向、旧引用未释放"
//
// 唯一索引 (space_id, parent_id, lower(name)) 冲突 → 409:
// 绝不静默改名(6.7)。客户端需要显式处理冲突(改名或走覆盖写)。
//
// 返回的 prevHash/prevSize 是**被覆盖掉的旧内容**,供调用方释放其引用。
// 刻意用返回值而不是就地改 `Input`:Input 是值传递,就地改会在调用方不可见
// (曾经踩过 —— 旧引用永不释放,对象引用数虚高、垃圾永不回收)。
func (s *Service) upsertFileRow(
	ctx context.Context,
	tx pgx.Tx, in Input, name, hash string, size int64,
) (file *model.File, newFile bool, prevHash string, prevSize int64, err error) {
	// 先看目标位置有没有同名行
	existing, gerr := s.Files.GetChildByName(ctx, tx, in.SpaceID, in.ParentID, name)
	switch {
	case errors.Is(gerr, repo.ErrNotFound):
		// 新建:version=1
		depth, derr := depthOf(ctx, tx, in.ParentID)
		if derr != nil {
			return nil, false, "", 0, derr
		}
		created, cerr := s.Files.InsertRow(ctx, tx, &model.File{
			SpaceID:    in.SpaceID,
			ParentID:   in.ParentID,
			OwnerID:    in.UserID,
			Name:       name,
			IsDir:      false,
			Size:       size,
			MimeType:   mimeOrDefault(in.MimeType, name),
			HashSHA256: hash,
			Depth:      depth,
		})
		if cerr != nil {
			// repo.InsertRow 把唯一索引冲突映射为 ErrConflict,同目录同名 → 409。
			// 绝不静默改名(6.7):客户端需要显式处理冲突。
			if errors.Is(cerr, repo.ErrConflict) || repo.IsUniqueViolation(cerr) {
				return nil, false, "", 0, apierr.Conflict(apierr.CodeNameConflict,
					"同目录下已存在同名文件: %s", name)
			}
			return nil, false, "", 0, apierr.Internal(cerr)
		}
		return created, true, "", 0, nil
	case gerr != nil:
		return nil, false, "", 0, apierr.Internal(gerr)
	}

	// 覆盖写:目标已存在同名行
	if existing.IsDir {
		return nil, false, "", 0, apierr.Conflict(apierr.CodeNameConflict,
			"目标是目录,不能作为文件覆盖: %s", name)
	}
	// **没有显式覆盖意图时拒绝覆盖**。
	//
	// 这一步是数据保护的关键:若默认允许覆盖,两个客户端同时向同名位置上传
	// 不同内容时,后到者会静默覆盖先到者 —— 用户看到"传上去了但内容变了"。
	// 6.7 要求绝不静默改名,**同理也绝不静默覆盖**;要覆盖必须由调用方显式声明
	// (WebDAV PUT 的覆盖语义、客户端明确的"替换上传")。
	if !in.AllowOverwrite {
		return nil, false, "", 0, apierr.Conflict(apierr.CodeNameConflict,
			"同目录下已存在同名文件: %s(如需替换请显式声明覆盖)", name)
	}
	updated, uerr := s.Files.UpdateContentPointer(ctx, tx, existing.ID, size, hash, mimeOrDefault(in.MimeType, name))
	if uerr != nil {
		if errors.Is(uerr, repo.ErrNotFound) {
			return nil, false, "", 0, apierr.NotFound("文件不存在")
		}
		return nil, false, "", 0, apierr.Internal(uerr)
	}
	return updated, false, existing.HashSHA256, existing.Size, nil
}

// depthOf 取父目录深度 +1(新建文件行用)。
func depthOf(ctx context.Context, q repo.Querier, parentID string) (int, error) {
	var d int
	if err := q.QueryRow(ctx, `SELECT depth FROM files WHERE id = $1`, parentID).Scan(&d); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, apierr.NotFound("父目录不存在")
		}
		return 0, apierr.Internal(err)
	}
	return d + 1, nil
}

// bumpObjectRef 引用 +1:复活已有行,或新建行。
//
// **复活必须是"计数+置态"一条 UPDATE**(6.4):早期写法漏了 `ref_count+1`,
// 结果是"文件指向了对象、对象引用数却是 0",清理 worker 随后把它当垃圾删掉 ——
// 悬空指针。这里用一条 UPDATE 同时完成两件事,并在匹配 0 行时回落到新建。
func bumpObjectRef(ctx context.Context, q repo.Querier, hash string, size int64, backend, key string) (*model.FileObject, error) {
	revived, err := scanObject(q.QueryRow(ctx, `
UPDATE file_objects
   SET ref_count = ref_count + 1, state = 'live', delete_after = NULL, updated_at = now()
 WHERE hash_sha256 = $1 AND size = $2 AND state IN ('live','pending_delete','deleting')
 RETURNING hash_sha256, size, storage_backend, object_key, ref_count, state,
           delete_after, created_at, updated_at`, hash, size))
	if err == nil {
		return revived, nil
	}
	if !errors.Is(err, repo.ErrNotFound) {
		return nil, apierr.Internal(err)
	}

	// 匹配 0 行(不存在,或处于已删除态)→ 新建引用计数为 1 的行
	created, cerr := scanObject(q.QueryRow(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, $3, $4, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE
   SET ref_count = file_objects.ref_count + 1,
       state = 'live',
       delete_after = NULL,
       updated_at = now()
 WHERE file_objects.size = EXCLUDED.size
RETURNING hash_sha256, size, storage_backend, object_key, ref_count, state,
          delete_after, created_at, updated_at`, hash, size, backend, key))
	if cerr != nil {
		if errors.Is(cerr, repo.ErrNotFound) {
			// ON CONFLICT ... WHERE 未命中:行存在但大小不同 —— 内容寻址下不可能,
			// 说明存储/元数据被破坏,必须显式暴露而不是继续
			return nil, apierr.Internal(fmt.Errorf(
				"对象 %s 已存在但大小与本次不符(需求 %d)", hash[:12], size))
		}
		return nil, apierr.Internal(cerr)
	}
	return created, nil
}

// releaseObjectRef 引用 -1;归零则置 pending_delete + delete_after(延迟删除窗口)。
//
// 用一条 UPDATE 完成"计数-1 + 归零置态":分成两条会在中间态被并发读取到
// "ref_count=0 且 state=live",而这恰好违反表上的不变式约束
// `(ref_count = 0) = (state <> 'live')` —— 拆开写会直接被约束拒绝。
func releaseObjectRef(ctx context.Context, q repo.Querier, hash string, size int64) error {
	if hash == "" {
		return nil
	}
	_, err := q.Exec(ctx, `
UPDATE file_objects
   SET ref_count = ref_count - 1,
       state = CASE WHEN ref_count - 1 = 0 THEN 'pending_delete' ELSE state END,
       delete_after = CASE WHEN ref_count - 1 = 0 THEN now() + interval '24 hours' ELSE delete_after END,
       updated_at = now()
 WHERE hash_sha256 = $1 AND ref_count > 0`, hash)
	if err != nil {
		return apierr.Internal(fmt.Errorf("释放旧对象引用失败: %w", err))
	}
	_ = size
	return nil
}

// finalizeUploadTask 把 upload 任务置 finalized 并登记目标 file_id。
//
// 条件 `state='reserved'`:并发双 finish 只有一个成功,第二个匹配 0 行 ——
// 此时**读回既有行**返回幂等结果(6.10:重复 finish 返回既有 file_id),
// 而不是报错。
func finalizeUploadTask(ctx context.Context, tx pgx.Tx, uploadID, hash string, size int64, fileID string) error {
	tag, err := tx.Exec(ctx, `
UPDATE uploads
   SET state = 'finalized', actual_hash = $2, actual_size = $3, target_file_id = $4, updated_at = now()
 WHERE id = $1 AND state = 'reserved'`, uploadID, hash, size, fileID)
	if err != nil {
		return apierr.Internal(fmt.Errorf("更新上传任务失败: %w", err))
	}
	if tag.RowsAffected() == 0 {
		// 已被并发定稿 / 已取消 / 不存在。检查是不是"已定稿"(幂等成功)
		var state string
		var target *string
		if qerr := tx.QueryRow(ctx,
			`SELECT state, target_file_id::text FROM uploads WHERE id = $1`, uploadID).Scan(&state, &target); qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return apierr.NotFound("上传任务不存在")
			}
			return apierr.Internal(qerr)
		}
		if state == model.UploadFinalized {
			// 幂等:同一任务重复 finish,返回既有目标(调用方据此返回 200)
			return nil
		}
		return apierr.Conflict(apierr.CodeUploadGone,
			"上传任务已结束(状态 %s),不能定稿", state)
	}
	return nil
}

func scanObject(row pgx.Row) (*model.FileObject, error) {
	var o model.FileObject
	if err := row.Scan(&o.HashSHA256, &o.Size, &o.StorageBackend, &o.ObjectKey,
		&o.RefCount, &o.State, &o.DeleteAfter, &o.CreatedAt, &o.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, repo.ErrNotFound
		}
		return nil, err
	}
	return &o, nil
}

// feedKind 决定变更流条目类型:新建 created,覆盖写 updated。
func feedKind(newFile bool) string {
	if newFile {
		return model.FeedCreated
	}
	return model.FeedUpdated
}

// mimeOrDefault 在未声明 mime 时按扩展名给一个默认值。
//
// 刻意只覆盖最常见的一小撮:完整 MIME 表属客户端/网关的职责,
// 服务端在这里做全表映射既臃肿又必然过时。
func mimeOrDefault(given, filename string) string {
	if given != "" {
		return given
	}
	lower := filename
	for i := len(filename) - 1; i >= 0; i-- {
		if filename[i] == '.' {
			lower = filename[i:]
			break
		}
	}
	switch lower {
	case ".txt", ".log", ".md":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	case ".pdf":
		return "application/pdf"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".zip":
		return "application/zip"
	case ".doc", ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xls", ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	default:
		return "application/octet-stream"
	}
}
