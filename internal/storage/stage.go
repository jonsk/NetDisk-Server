package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// 本文件是 **TUS 上传的暂存区读写**(6.1 分片暂存 / 3.4 文件上传流程)。
//
// 暂存区与"内容寻址对象区"刻意分开:
//   - 暂存文件按 **upload_id** 命名(不是内容哈希 —— 此刻还不知道哈希),
//     位置:`<root>/tus-tmp/<upload_id>.chunk`
//   - 运维只需要盯一个目录即可做清理与容量监控;objects 与 tus-tmp 建议分盘
//     (架构 3.1 职责 4:防止上传洪峰挤爆系统盘)
//
// 为什么不把分片合并成"每片一个小文件、最后拼接":追加写单个文件让
// **断点续传的偏移量等于文件长度**(`os.Stat` 即可得),不需要额外的分片索引;
// 而"分片索引"本身就是一类容易与真实落盘不一致的状态。
// 代价是并发分片无法乱序写入 —— 但 TUS 协议本身要求按 offset 顺序 PATCH,
// 所以这不是限制而是协议语义。

// ErrUploadIDInvalid 表示 upload_id 不适合做文件名。
//
// upload_id 来自 URL 路径,而它会**拼进文件路径** —— 必须严格校验,
// 不允许 `/`、`..`、控制字符等(哪怕上游已校验,也不该把安全性建立在
// "调用方会检查"上)。
var ErrUploadIDInvalid = errors.New("storage: upload_id 非法")

// safeUploadID 校验 upload_id 只含 UUID/十六进制类字符。
//
// 允许的字符集刻意收得很窄:`0-9 a-f A-F -`(UUID 形态)。
// 我们的 upload_id 就是 UUID,不需要更宽的字符集;
// 收窄的直接好处是**路径穿越、Windows 保留名、盘符前缀全部不可能**。
func safeUploadID(id string) (string, error) {
	s := strings.TrimSpace(id)
	if len(s) == 0 || len(s) > 64 {
		return "", ErrUploadIDInvalid
	}
	for _, r := range s {
		if unicode.Is(unicode.ASCII_Hex_Digit, r) || r == '-' {
			continue
		}
		return "", ErrUploadIDInvalid
	}
	// 全 '-' 之类退化输入也拒绝(避免与目录名混淆)
	if strings.Trim(s, "-") == "" {
		return "", ErrUploadIDInvalid
	}
	return s, nil
}

// StageDir 返回暂存目录并确保存在。
func (f *FS) StageDir() (string, error) {
	dir := filepath.Join(f.Root, "tus-tmp")
	if err := os.MkdirAll(dir, f.dirMode); err != nil {
		return "", fmt.Errorf("storage: 创建暂存目录失败: %w", err)
	}
	return dir, nil
}

// StagePath 返回某 upload 的暂存文件路径。
func (f *FS) StagePath(uploadID string) (string, error) {
	id, err := safeUploadID(uploadID)
	if err != nil {
		return "", err
	}
	dir, err := f.StageDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".chunk"), nil
}

// StageSize 返回暂存文件当前长度(不存在返回 0,不报错)。
//
// **这是断点续传的偏移量真相**:偏移量取自文件实际长度,而不是数据库里
// 某个可能过期的计数字段。两者若不一致,以文件为准才能保证"写的字节
// 与文件内容"吻合;数据库字段只用于展示与配额判定。
func (f *FS) StageSize(uploadID string) (int64, error) {
	p, err := f.StagePath(uploadID)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("storage: stat 暂存文件失败: %w", err)
	}
	return st.Size(), nil
}

// StageAppend 在指定偏移追加数据(TUS PATCH 的核心)。
//
// **必须校验期望偏移 == 当前文件长度**:
//   - 少了这一步,乱序或重放的分片会被写到错误位置 → 内容损坏,
//     而哈希校验要到定稿才发现 —— 那时用户已经传完了整个文件。
//   - 重放同一分片(客户端重试)时,偏移 < 长度 → 返回 ErrOffsetMismatch,
//     由上层按 TUS 语义返回 409,客户端据此重新 HEAD 取偏移。
//
// 返回写入后的新长度。
func (f *FS) StageAppend(ctx context.Context, uploadID string, expectOffset int64, r io.Reader) (int64, error) {
	if expectOffset < 0 {
		return 0, fmt.Errorf("storage: 偏移不能为负")
	}
	p, err := f.StagePath(uploadID)
	if err != nil {
		return 0, err
	}
	fh, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, f.fileMode)
	if err != nil {
		return 0, fmt.Errorf("storage: 打开暂存文件失败: %w", err)
	}
	defer func() { _ = fh.Close() }()

	st, err := fh.Stat()
	if err != nil {
		return 0, fmt.Errorf("storage: stat 暂存文件失败: %w", err)
	}
	if st.Size() != expectOffset {
		return st.Size(), ErrOffsetMismatch
	}
	if _, err := fh.Seek(expectOffset, io.SeekStart); err != nil {
		return expectOffset, fmt.Errorf("storage: 定位失败: %w", err)
	}
	n, err := f.copyWithContext(ctx, fh, r)
	if err != nil {
		// 写失败时**不回退长度**:已落盘的部分是有效的(这正是断点续传的意义),
		// 客户端下次 HEAD 会拿到实际长度并从那里继续。
		return expectOffset + n, fmt.Errorf("storage: 追写分片失败: %w", err)
	}
	if err := fh.Sync(); err != nil {
		return expectOffset + n, fmt.Errorf("storage: 刷盘失败: %w", err)
	}
	return expectOffset + n, nil
}

// ErrOffsetMismatch 表示客户端给的 Upload-Offset 与暂存文件长度不符。
//
// 上层必须映射为 **409 Conflict**(TUS 规范),并把服务端真实偏移放在
// `Upload-Offset` 响应头里 —— 客户端据此自我纠正,而不是整个重传。
var ErrOffsetMismatch = errors.New("storage: 上传偏移与暂存文件长度不符")

// StageOpen 打开暂存文件供定稿读取;不存在返回 ErrNotFound。
func (f *FS) StageOpen(uploadID string) (*os.File, error) {
	p, err := f.StagePath(uploadID)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: 打开暂存文件失败: %w", err)
	}
	return fh, nil
}

// StageRemove 删除暂存文件(幂等)。定稿成功、取消上传、过期回收都要调用。
func (f *FS) StageRemove(uploadID string) error {
	p, err := f.StagePath(uploadID)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: 删除暂存文件失败: %w", err)
	}
	return nil
}

// ListStages 列出暂存目录中的全部 upload_id(清理任务用)。
//
// 只返回**本函数认得的**文件名形态(纯 upload_id + .chunk):
// 临时文件、人工放进去的东西一律跳过,避免清理任务误删。
func (f *FS) ListStages() ([]string, error) {
	files, err := f.ListStageFiles()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(files))
	for _, sf := range files {
		out = append(out, sf.UploadID)
	}
	return out, nil
}

// StageFile 是一个暂存文件的元信息(回收任务据此判断"够不够老")。
type StageFile struct {
	UploadID string
	Size     int64
	// ModTime 是**最后写入时刻**(不是创建时刻):续传中的文件会不断被刷新,
	// 于是"够老"这个判据天然对续传友好 —— 但**不能只靠它**
	// (见 StageCleaner:还要看任务是否仍活跃),因为客户端可能传完一半去睡觉。
	ModTime time.Time
}

// ListStageFiles 列出暂存文件及其元信息(单次目录扫描,不再逐个 stat)。
//
// 与 ListStages 同一套文件名白名单:只认 `<upload_id>.chunk`,
// 临时文件与人工放进去的东西一律跳过 —— 清理任务**绝不能**误删。
func (f *FS) ListStageFiles() ([]StageFile, error) {
	dir, err := f.StageDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: 读取暂存目录失败: %w", err)
	}
	var out []StageFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".chunk") {
			continue
		}
		id := strings.TrimSuffix(name, ".chunk")
		if _, err := safeUploadID(id); err != nil {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			// 单个文件读不到元信息(正被删除/权限变动)不该让整轮清理失败
			continue
		}
		out = append(out, StageFile{UploadID: id, Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}
