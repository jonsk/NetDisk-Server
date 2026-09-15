// Package storage 是物理对象存储抽象层(6.5)。
//
// 一期的默认生产后端是**本地磁盘**,对象按内容寻址扁平存放:
//
//	<root>/objects/{xx}/{xx}/{sha256}
//	                 ↑    ↑
//	        前 2 位十六进制  次 2 位十六进制
//
// 为什么内容寻址:同一份内容在库里只有一份物理对象(秒传/多空间引用共享它),
// 而对象**不可变** —— 覆盖写 = 新 sha256 → 新对象,旧对象引用数归零后进延迟删除。
// 这让"对象"与"文件"彻底解耦:Move/Copy 零存储 IO(6.5)。
//
// 为什么两级散列目录:单目录下几十万文件在 Windows/NTFS 与 ext4 上都会退化
// (列目录、创建、杀毒软件扫描全变慢);两级各 256 个桶把单目录文件数压到
// 总量的 1/65536,且目录名固定为两位十六进制、便于人工核对。
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNotFound 表示对象在存储后端不存在。
//
// 必须与"IO 故障"区分:前者是生命周期 worker 的**正常终态**
// (对象已删、行未标 deleted,下轮重扫即可),后者要告警。
var ErrNotFound = errors.New("storage: 对象不存在")

// ErrShortWrite 表示写入的字节数少于声明值(流被提前截断)。
//
// 这是一个**必须致命**的错误:静默接受短写会让 files.size 与实际内容不符,
// 而 etag 用的是内容哈希,后续 GET 会返回长度不匹配的响应。
var ErrShortWrite = errors.New("storage: 写入字节数少于声明大小")

// Backend 标识存储后端类型(写进 file_objects.storage_backend,便于将来多后端并存)。
const (
	BackendFS  = "fs"
	BackendS3  = "s3"
	BackendMem = "memory"
)

// Storager 是对象存储的最小能力集。
//
// 刻意只保留五个方法:一期只用得到这些,而"多后端零改码"的关键在于
// **业务代码只依赖这个接口**,不在接口里泄漏任何后端特有的概念
// (分片上传 ID、预签名 URL 等)。将来接 S3 只需实现本接口。
type Storager interface {
	// Backend 返回后端标识(落库用)
	Backend() string
	// KeyFor 返回内容对应的对象键(纯函数,不触盘)
	KeyFor(hashSHA256 string) string
	// Write 原子写入对象;返回实际写入字节数。
	// size 是**声明值**,实现必须校验实际字节数与之相等,否则返回 ErrShortWrite。
	// 同一 hash 重复写入必须幂等(内容寻址下内容必然相同)。
	Write(ctx context.Context, hashSHA256 string, r io.Reader, size int64) (written int64, err error)
	// Stat 返回对象大小;不存在返回 ErrNotFound。
	Stat(ctx context.Context, hashSHA256 string) (size int64, err error)
	// Open 返回对象内容的只读流;不存在返回 ErrNotFound。
	//
	// 返回 `io.ReadSeekCloser` 而不是 `io.ReadCloser`:下载必须支持 Range,
	// 而 `http.ServeContent` 要求 `io.ReadSeeker`。让存储层直接给出可寻址流,
	// 比在 handler 里把整个对象读进内存再切片要好得多 ——
	// 后者会让下载的内存占用等于文件大小(GB 级文件直接打死进程)。
	Open(ctx context.Context, hashSHA256 string) (io.ReadSeekCloser, error)
	// Delete 删除对象;对象不存在**不算错误**(幂等)。
	Delete(ctx context.Context, hashSHA256 string) error
}

// Sponsorable 表示后端支持"从本地暂存文件提交对象"。
//
// 为什么需要它:6.10 要求**服务端实测 sha256**,而哈希只有把内容读一遍才知道;
// 若先读一遍算哈希、再读一遍写对象,每个上传的 IO 翻倍。
// 正确顺序是"读一次内容 → 落暂存文件并同时算哈希 → 用已知哈希提交为对象"。
// 暂存文件同时是 TUS 合并的中间产物(6.1 的 tus-tmp/merging),
// 因此这个能力对外仍是一个方法,不泄漏任何后端细节。
type Sponsorable interface {
	// StageFrom 把内容读入暂存文件,返回 (暂存路径, 实测哈希, 字节数)。
	// 调用方必须在提交或放弃后自行删除暂存文件。
	StageFrom(ctx context.Context, r io.Reader) (stagePath string, hashSHA256 string, size int64, err error)
	// CommitStaged 用已知哈希把暂存文件提交为对象。
	// 对象已存在且大小一致时**直接删除暂存文件并复用**(秒传/引用复用的常见路径)。
	CommitStaged(ctx context.Context, hashSHA256, stagePath string, size int64) error
}

var _ Sponsorable = (*FS)(nil)

// HexSHA256 校验并归一 64 位小写十六进制哈希。
//
// 全项目只有这一处做该校验:哈希会拼进文件路径,不校验就等于把路径拼接
// 交给了外部输入(哪怕调用方此刻可信,也不该把安全性建立在"调用方会检查"上)。
func HexSHA256(hash string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(hash))
	if len(h) != 64 {
		return "", fmt.Errorf("storage: 哈希长度必须为 64(实际 %d)", len(h))
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return "", fmt.Errorf("storage: 哈希含非十六进制字符 %q", c)
	}
	return h, nil
}

// FS 是本地磁盘后端。
type FS struct {
	// Root 是数据根目录(生产: /opt/netdisk/data)
	Root string
	// dirMode/fileMode 便于测试与运维按需收紧权限
	dirMode  os.FileMode
	fileMode os.FileMode
}

// NewFS 构造本地磁盘后端并确保根目录存在。
//
// 启动时就创建并**写探针文件**验证可写:等到第一次上传才发现数据目录不可写,
// 损失是用户白传一个文件 + 一次 5xx,而这里失败只需重启一次进程。
func NewFS(root string) (*FS, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("storage: root 不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: 解析 root 失败: %w", err)
	}
	f := &FS{Root: abs, dirMode: 0o750, fileMode: 0o640}
	if err := os.MkdirAll(filepath.Join(abs, "objects"), f.dirMode); err != nil {
		return nil, fmt.Errorf("storage: 创建 objects 目录失败: %w", err)
	}
	if err := f.probeWritable(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *FS) probeWritable() error {
	p := filepath.Join(f.Root, ".write-probe")
	if err := os.WriteFile(p, []byte("ok"), f.fileMode); err != nil {
		return fmt.Errorf("storage: 数据目录不可写(%s): %w", f.Root, err)
	}
	return os.Remove(p)
}

// Backend 实现 Storager。
func (f *FS) Backend() string { return BackendFS }

// KeyFor 实现 Storager:返回 objects/{xx}/{xx}/{sha256}。
//
// 返回错误时不产出可用的 key —— 调用方必须处理校验失败(见 HexSHA256 的说明)。
func (f *FS) KeyFor(hashSHA256 string) string {
	h, err := HexSHA256(hashSHA256)
	if err != nil {
		return ""
	}
	return "objects/" + h[0:2] + "/" + h[2:4] + "/" + h
}

// pathFor 是 KeyFor 的绝对路径版本。
func (f *FS) pathFor(hashSHA256 string) (string, error) {
	key := f.KeyFor(hashSHA256)
	if key == "" {
		return "", fmt.Errorf("storage: 非法哈希 %q", hashSHA256)
	}
	return filepath.Join(f.Root, filepath.FromSlash(key)), nil
}

// Write 实现 Storager:先写同目录临时文件再 rename,保证**对象永不半成品可见**。
//
// 为什么必须同目录 rename:跨文件系统的 rename 在 POSIX 下是"复制+删除"
// (非原子),而 Windows 的 `os.Rename` 跨卷会直接失败。临时文件放在目标同目录
// 既保证原子性,也保证 rename 落在同一卷。
//
// 内容寻址让"重复写"极其常见(秒传/多空间共享同一内容),因此:
// **对象已存在且大小一致时直接丢弃输入**,不做第二次落盘。
func (f *FS) Write(ctx context.Context, hashSHA256 string, r io.Reader, size int64) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("storage: size 不能为负")
	}
	dst, err := f.pathFor(hashSHA256)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), f.dirMode); err != nil {
		return 0, fmt.Errorf("storage: 创建对象目录失败: %w", err)
	}

	// 已存在且大小一致 → 幂等跳过(内容寻址下内容必然相同)
	if st, err := os.Stat(dst); err == nil {
		if st.Size() == size {
			// 仍需消费掉 r,否则调用方(TUS 的合并流程)可能阻塞在管道写端
			if _, err := io.Copy(io.Discard, r); err != nil {
				return 0, fmt.Errorf("storage: 排空输入失败: %w", err)
			}
			return size, nil
		}
		// 大小不一致:内容寻址下这**不可能**发生(同 hash 必然同内容),
		// 说明存储被外部破坏或哈希算法被误用 → 显式报错而不是覆盖
		return 0, fmt.Errorf("storage: 同哈希对象已存在但大小不符(%d != %d)", st.Size(), size)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("storage: 创建临时对象失败: %w", err)
	}
	tmpName := tmp.Name()
	// 失败路径统一清理;成功路径 rename 后该文件已不存在
	defer func() { _ = os.Remove(tmpName) }()

	written, copyErr := f.copyWithContext(ctx, tmp, r)
	closeErr := tmp.Close()
	if copyErr != nil {
		return written, fmt.Errorf("storage: 写入临时对象失败: %w", copyErr)
	}
	if closeErr != nil {
		return written, fmt.Errorf("storage: 关闭临时对象失败: %w", closeErr)
	}
	if written != size {
		// 短写必须致命:静默接受会让 files.size 与内容不符
		return written, fmt.Errorf("%w(声明 %d,实际 %d)", ErrShortWrite, size, written)
	}
	if err := os.Chmod(tmpName, f.fileMode); err != nil {
		return written, fmt.Errorf("storage: 设置对象权限失败: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		// 并发写同一 hash 的竞态:**内容寻址下这是正常路径**(秒传、多空间共享
		// 同一内容、多个客户端同时上传同一文件),不是错误。
		//
		// Windows 的 rename 在目标已存在时会直接失败(Access is denied /
		// ERROR_ALREADY_EXISTS),而不是像 POSIX 那样静默覆盖 —— 于是"另一个
		// 协程/进程刚提交完同一对象"会被误报成写入失败。
		// 处理:确认对象确实已在且大小一致,则视为成功(内容必然相同)。
		if st, serr := os.Stat(dst); serr == nil && st.Size() == size {
			return size, nil
		}
		return written, fmt.Errorf("storage: 提交对象失败: %w", err)
	}
	return written, nil
}

// copyWithContext 在拷贝过程中响应 ctx 取消(大文件上传必须可中断)。
func (f *FS) copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 256<<10)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, rerr := src.Read(buf)
		if n > 0 {
			w, werr := dst.Write(buf[:n])
			total += int64(w)
			if werr != nil {
				return total, werr
			}
			if w != n {
				return total, io.ErrShortWrite
			}
		}
		if rerr == io.EOF {
			return total, nil
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// StageFrom 实现 Sponsorable:读一次内容,同时落暂存文件与计算 SHA256。
//
// 暂存文件放在 `<root>/tus-tmp/` 下 —— 与 6.1 的 TUS 分片暂存同一个目录,
// 便于运维用一条规则统一清理(24h 清理任务只需盯这一个目录)。
func (f *FS) StageFrom(ctx context.Context, r io.Reader) (string, string, int64, error) {
	dir := filepath.Join(f.Root, "tus-tmp")
	if err := os.MkdirAll(dir, f.dirMode); err != nil {
		return "", "", 0, fmt.Errorf("storage: 创建暂存目录失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "stage-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("storage: 创建暂存文件失败: %w", err)
	}
	name := tmp.Name()

	hh, sum := NewHasher()
	written, cerr := f.copyWithContext(ctx, hh.Tee(tmp), r)
	closeErr := tmp.Close()
	if cerr != nil {
		_ = os.Remove(name)
		return "", "", 0, fmt.Errorf("storage: 写入暂存文件失败: %w", cerr)
	}
	if closeErr != nil {
		_ = os.Remove(name)
		return "", "", 0, fmt.Errorf("storage: 关闭暂存文件失败: %w", closeErr)
	}
	if err := os.Chmod(name, f.fileMode); err != nil {
		_ = os.Remove(name)
		return "", "", 0, fmt.Errorf("storage: 设置暂存文件权限失败: %w", err)
	}
	return name, sum(), written, nil
}

// CommitStaged 实现 Sponsorable。
//
// 对象已存在且大小一致时**直接丢弃暂存文件**:内容寻址下同 hash 必然同内容,
// 重写一遍纯属浪费 IO(秒传与多空间引用共享同一内容时这是最常走的路径)。
func (f *FS) CommitStaged(ctx context.Context, hashSHA256, stagePath string, size int64) error {
	dst, err := f.pathFor(hashSHA256)
	if err != nil {
		_ = os.Remove(stagePath)
		return err
	}
	if st, serr := os.Stat(dst); serr == nil {
		_ = os.Remove(stagePath)
		if st.Size() != size {
			// 内容寻址下不可能发生 → 存储被外部破坏,显式报错而不是覆盖
			return fmt.Errorf("storage: 同哈希对象已存在但大小不符(%d != %d)", st.Size(), size)
		}
		return nil
	}
	if st, serr := os.Stat(stagePath); serr != nil {
		return fmt.Errorf("storage: 暂存文件不可用: %w", serr)
	} else if st.Size() != size {
		_ = os.Remove(stagePath)
		return fmt.Errorf("%w(声明 %d,暂存 %d)", ErrShortWrite, size, st.Size())
	}
	if err := os.MkdirAll(filepath.Dir(dst), f.dirMode); err != nil {
		_ = os.Remove(stagePath)
		return fmt.Errorf("storage: 创建对象目录失败: %w", err)
	}
	// 同卷 rename 是原子的:对象要么完整可见,要么完全不存在
	if err := os.Rename(stagePath, dst); err != nil {
		// 与 Write 同理:并发提交同一 hash 时另一个提交者可能刚把对象放好,
		// Windows 下 rename 会因目标已存在而失败。确认对象在且大小一致即成功。
		if st, serr := os.Stat(dst); serr == nil && st.Size() == size {
			_ = os.Remove(stagePath)
			return nil
		}
		_ = os.Remove(stagePath)
		return fmt.Errorf("storage: 提交对象失败: %w", err)
	}
	return nil
}

// Open 实现 Storager:返回可寻址的只读流(Range 下载的基础)。
func (f *FS) Open(ctx context.Context, hashSHA256 string) (io.ReadSeekCloser, error) {
	p, err := f.pathFor(hashSHA256)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("storage: 打开对象失败: %w", err)
	}
	return fh, nil
}

// Stat 实现 Storager。
func (f *FS) Stat(ctx context.Context, hashSHA256 string) (int64, error) {
	p, err := f.pathFor(hashSHA256)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("storage: stat 失败: %w", err)
	}
	return st.Size(), nil
}

// Delete 实现 Storager:不存在不算错误(幂等)。
//
// 生命周期 worker 会重试删除,把"已经不在了"当失败会让对象永远停在 deleting。
func (f *FS) Delete(ctx context.Context, hashSHA256 string) error {
	p, err := f.pathFor(hashSHA256)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("storage: 删除对象失败: %w", err)
	}
	return nil
}

// Hasher 是"边写边算 SHA256"的封装(6.10 步骤 2:服务端实测哈希)。
//
// 为什么不在写完之后再读一遍算哈希:那会把每个上传的 IO 量翻倍,
// 且在大文件上等于把"写完到可访问"的延迟又拉长一倍。
type Hasher struct {
	h io.Writer
}

// NewHasher 返回一个 Hasher;把写入流同时喂给它即可得到实测哈希。
func NewHasher() (*Hasher, func() string) {
	hh := sha256.New()
	return &Hasher{h: hh}, func() string { return hex.EncodeToString(hh.Sum(nil)) }
}

// Tee 把 dst 与哈希器合成一个写入端:写进去的字节既落盘也进哈希。
//
// 刻意不命名为 `WriteTo` —— 那会与 `io.WriterTo` 的签名要求冲突
// (`WriteTo(io.Writer) (int64, error)`),`go vet` 会直接报错。
func (h *Hasher) Tee(dst io.Writer) io.Writer {
	return io.MultiWriter(dst, h.h)
}

// ParseSize 是给 TUS/HTTP 头用的宽松解析(空串 → 0)。
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("storage: 非法大小 %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("storage: 大小不能为负: %d", n)
	}
	return n, nil
}
