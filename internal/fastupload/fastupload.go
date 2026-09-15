// Package fastupload 实现秒传(预检命中)通道的**持物证明**挑战(6.10 五细则 / R-05 / BE-S4-04)。
//
// # 为什么必须有这个包
//
// 内容寻址让"声明 hash 命中即建行"看起来无成本,但它会给出一条**白拿内容**的通道:
// 任何客户端只要报出别人的 hash(哈希不是秘密:分享链接、日志、公开文件都能拿到),
// 就能在**不持有内容**的情况下让服务端插一行指向那个对象 —— 于是"秒传"变成
// "凭一个字符串领走别人的文件"。这不是理论问题:URL 里带 hash 的下载器、
// 采集器随手就能试。
//
// 因此 6.10 的纪律是:①元数据的 hash **只信服务端实测**,任何未实测内容不落元数据行;
// ②预检命中后必须过**持物证明挑战**;③去重键 (hash,size) 双校验。
// 本包实现②:服务端下发**随机偏移的样本区间**与一次性 nonce,客户端回这些区间内容的
// 摘要;服务端从**已存对象**里读同样的区间来比对 —— 客户端拿不出内容就过不了。
//
// # 五条细则的落点(6.10"挑战五细则")
//
//	① MinSize=32KB 以下不开通道:小文件秒传收益趋零,不值得为它开内容侧信口
//	② 偏移**随机**(crypto/rand)+ 一次性 nonce,TTL 5min,**绑定 upload_id + hash**
//	③ 失败由调用方计入限速并写审计(本包只回答"过/不过",不做惩罚)
//	④ finalize 必须带同一 nonce 且仍在有效期,并**复核对象仍可用**(与清理竞态同源)
//	⑤ 挑战只回区间**偏移**,不回内容;样本值与摘要一律不落日志
//
// # 一次性的实现
//
// nonce 在 finalize 校验时**先取后删**(Redis 单键 DEL 返回 1 的那个调用者才算拿到),
// 于是重放同一个 nonce 的第二个请求必然失败 —— 否则"证明一次、白拿无数次"。
package fastupload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"
)

// 挑战参数(6.10 五细则①)。
const (
	// MinSize 是开放秒传通道的最小文件大小:8 × SampleLen = 32KB。
	//
	// 为什么是"8 个样本"这个数:样本太少(1~2 个)时,客户端只要从别处搞到
	// 少量片段就能蒙过;8 个随机 4KB 区间合起来已覆盖整个文件 1/8 以上的概率极低,
	// 且对 GB 级文件的服务端读取量仍然恒定(32KB)。
	MinSize = 32768
	// SampleLen 是每个样本区间的字节数。
	SampleLen = 4096
	// SampleCount 是样本区间个数。
	SampleCount = 8
	// DefaultTTL 是挑战有效期。
	DefaultTTL = 5 * time.Minute
)

// ErrUnavailable 表示挑战存储(Redis)不可用。
//
// 调用方必须**降级**而不是失败:Redis 挂了不该让普通上传用不了(10.7)。
// 降级的方式是"这次不给秒传通道",客户端老实全量上传即可。
var ErrUnavailable = errors.New("fastupload: 挑战存储不可用")

// ErrNoChallenge / ErrChallengeExpired / ErrChallengeMismatch 是三种"过不了"的原因。
//
// 刻意分开:调用方要据此给出不同提示(重新预检 / 重新拿到挑战 / 老实上传),
// 而"混成一个错误"会让客户端永远重试同一个错误路径。
var (
	ErrNoChallenge       = errors.New("fastupload: 没有该上传任务的挑战(未预检或已被使用)")
	ErrChallengeExpired  = errors.New("fastupload: 挑战已过期")
	ErrChallengeMismatch = errors.New("fastupload: 挑战与本次上传不匹配(hash 或 upload_id 不符)")
	ErrProofFailed       = errors.New("fastupload: 持物证明未通过(样本摘要不匹配)")
)

// KV 是挑战存储所需的最小能力(由 cache.Client 满足)。
//
// Key 也放在这里而不是由本包手拼字符串 —— 键命名规范是 cache 包的职责(6.7),
// 本包若自己拼 `netdisk:fastup:...` 就绕过了它的字符白名单校验。
type KV interface {
	Key(parts ...string) (string, error)
	Set(ctx context.Context, key string, val any, ttl time.Duration) error
	GetString(ctx context.Context, key string) (string, bool, error)
	Del(ctx context.Context, keys ...string) error
}

// Reader 是读取已存对象的能力(与 storage.Storager.Open 同形)。
//
// 只依赖这一个方法:本包不该知道对象存在哪(本地 fs / S3 / 未来后端),
// 而验证"客户端是否持有内容"原则上只需要"能读回对象".
type Reader interface {
	Open(ctx context.Context, hashSHA256 string) (io.ReadSeekCloser, error)
}

// Challenge 是一次持物证明挑战。
//
// 字段都带短 json tag:它同时是**下发给客户端的响应体**与**Redis 里的存储格式**,
// 存储格式不进任何对外契约,因此可以只求紧凑。
type Challenge struct {
	Nonce     string  `json:"n"`
	UploadID  string  `json:"u"`
	Hash      string  `json:"h"`
	Size      int64   `json:"s"`
	Offsets   []int64 `json:"o"`
	ExpireAt  int64   `json:"e"` // Unix 秒;TTL 由 Redis 兜底,这里用于显式判断与测试
	SampleLen int64   `json:"l"`
}

// View 是**下发给客户端**的挑战视图:偏移 + nonce,**不含任何内容**。
//
// 与 Challenge 分开是为了让"能不能回内容"这件事在类型上就无法表达 ——
// 若直接回 Challenge,将来有人往里加一个 Content 字段就会顺手把内容回出去。
type View struct {
	Nonce     string  `json:"nonce"`
	SampleLen int64   `json:"sample_len"`
	Offsets   []int64 `json:"sample_offsets"`
	// ExpiresInSeconds 便于客户端提前重新预检,而不是撞到过期才知道
	ExpiresInSeconds int64 `json:"expires_in_seconds"`
}

// Service 生成与校验挑战。
type Service struct {
	KV     KV
	Reader Reader
	TTL    time.Duration
	// Now 便于测试注入时钟
	Now func() time.Time
	// RandInt 便于测试注入确定性偏移(生产用 crypto/rand)
	RandInt func(n int64) (int64, error)
}

func (s *Service) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultTTL
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// MinSizeFor 返回开放秒传通道的最小大小(可由 config 覆盖,默认 MinSize)。
//
// 之所以可配:不同部署的"小文件"阈值不同(网盘 vs 富媒体),但**下限不可为零** ——
// 传 0 或负值一律回落到默认值,绝不允许"关掉大小门槛"(那等于给所有文件开信口)。
func MinSizeFor(configured int64) int64 {
	if configured <= 0 {
		return MinSize
	}
	return configured
}

// Issue 下发一次挑战(绑定 uploadID + hash),返回给客户端的视图。
func (s *Service) Issue(ctx context.Context, uploadID, hash string, size int64) (*View, error) {
	if s.KV == nil {
		return nil, ErrUnavailable
	}
	if uploadID == "" || hash == "" {
		return nil, fmt.Errorf("fastupload: upload_id 与 hash 不能为空")
	}
	if size < MinSize {
		// 细则①:小文件不开通道(调用方本应先判,这里是第二道闸)
		return nil, fmt.Errorf("fastupload: 文件 %d 字节小于秒传下限 %d", size, MinSize)
	}
	offsets, err := s.sampleOffsets(size)
	if err != nil {
		return nil, err
	}
	nonce, err := newNonce()
	if err != nil {
		return nil, err
	}
	expire := s.now().Add(s.ttl())
	ch := &Challenge{
		Nonce: nonce, UploadID: uploadID, Hash: hash, Size: size,
		Offsets: offsets, ExpireAt: expire.Unix(), SampleLen: SampleLen,
	}
	key, err := s.key(uploadID)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(ch)
	if err != nil {
		return nil, err
	}
	if err := s.KV.Set(ctx, key, string(raw), s.ttl()); err != nil {
		// 存储不可用 → 由调用方降级为"不给秒传提示",而不是让上传失败
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &View{
		Nonce: nonce, SampleLen: SampleLen, Offsets: offsets,
		ExpiresInSeconds: int64(s.ttl().Seconds()),
	}, nil
}

// Take 取出并**一次性作废**挑战。
//
// 校验三件事:存在、未过期、与本次 (uploadID, hash) 绑定一致。
// 取到后立即删除:同一个 nonce 只能完成一次定稿,否则"证明一次、白拿无数次"。
//
// 注意顺序:先校验再删。若先删后校验,一次不匹配的请求就能把别人的挑战作废
// (等于给了一个"拒绝服务"的把手)。
func (s *Service) Take(ctx context.Context, uploadID, hash string) (*Challenge, error) {
	if s.KV == nil {
		return nil, ErrUnavailable
	}
	key, err := s.key(uploadID)
	if err != nil {
		return nil, err
	}
	raw, ok, err := s.KV.GetString(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !ok {
		return nil, ErrNoChallenge
	}
	var ch Challenge
	if err := json.Unmarshal([]byte(raw), &ch); err != nil {
		// 存储里的内容坏了:删掉并当作"没有挑战",让客户端重新预检
		_ = s.KV.Del(ctx, key)
		return nil, ErrNoChallenge
	}
	if ch.UploadID != uploadID || ch.Hash != hash {
		return nil, ErrChallengeMismatch
	}
	if s.now().Unix() > ch.ExpireAt {
		_ = s.KV.Del(ctx, key)
		return nil, ErrChallengeExpired
	}
	if err := s.KV.Del(ctx, key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return &ch, nil
}

// Verify 用客户端回传的样本摘要验证"持有内容":从**已存对象**读同样区间比对。
//
// got 是与 ch.Offsets 一一对应的 sha256 hex 列表(长度不符直接失败)。
//
// 读对象而不是读客户端上传的内容:这正是"免重传"的意义 ——
// 服务端已经有这份内容,只需要确认客户端也有。
func (s *Service) Verify(ctx context.Context, ch *Challenge, got []string) error {
	if s.Reader == nil {
		return errors.New("fastupload: Reader 未装配")
	}
	if ch == nil {
		return ErrNoChallenge
	}
	if len(got) != len(ch.Offsets) {
		return fmt.Errorf("%w: 需要 %d 个样本摘要,收到 %d 个",
			ErrProofFailed, len(ch.Offsets), len(got))
	}
	rc, err := s.Reader.Open(ctx, ch.Hash)
	if err != nil {
		// 数据库行说对象在、物理对象不在:不能放行(否则落一个下载 404 的行),
		// 报 ErrProofFailed 让客户端老实上传 —— finalize 的正常路径会重新落盘。
		return fmt.Errorf("%w: 无法读取已存对象", ErrProofFailed)
	}
	defer func() { _ = rc.Close() }()

	buf := make([]byte, ch.SampleLen)
	for i, off := range ch.Offsets {
		want, derr := hex.DecodeString(got[i])
		if derr != nil || len(want) != sha256.Size {
			return fmt.Errorf("%w: 第 %d 个样本摘要格式非法", ErrProofFailed, i+1)
		}
		if _, serr := rc.Seek(off, io.SeekStart); serr != nil {
			return fmt.Errorf("%w: 定位对象内容失败", ErrProofFailed)
		}
		if _, rerr := io.ReadFull(rc, buf); rerr != nil {
			return fmt.Errorf("%w: 读取对象内容失败", ErrProofFailed)
		}
		sum := sha256.Sum256(buf)
		if !equalBytes(sum[:], want) {
			// 注意:错误信息里**不带任何样本值/摘要**(细则⑤),
			// 只带"第几个不匹配"——足够定位问题,又不构成信息泄露。
			return fmt.Errorf("%w: 第 %d 个样本不匹配", ErrProofFailed, i+1)
		}
	}
	return nil
}

// SampleDigests 计算"内容文件在某组偏移上的摘要"—— 客户端那侧要做的事。
//
// 放在服务端包里是为了让探针与测试能与服务端**用同一份实现**产生应答,
// 否则"客户端算法"与"服务端算法"会各写一遍,漂移后表现为随机失败。
func SampleDigests(r io.ReaderAt, offsets []int64, sampleLen int64) ([]string, error) {
	out := make([]string, len(offsets))
	buf := make([]byte, sampleLen)
	for i, off := range offsets {
		if _, err := r.ReadAt(buf, off); err != nil {
			return nil, fmt.Errorf("读取样本失败: %w", err)
		}
		sum := sha256.Sum256(buf)
		out[i] = hex.EncodeToString(sum[:])
	}
	return out, nil
}

// sampleOffsets 生成 SampleCount 个**随机**偏移(细则②)。
//
// 上界是 size-SampleLen:样本必须整段落在文件内 —— 越界会让服务端读到 EOF
// 而把合法客户端判成"证明失败"。
func (s *Service) sampleOffsets(size int64) ([]int64, error) {
	span := size - SampleLen
	if span < 1 {
		return nil, fmt.Errorf("fastupload: 文件 %d 字节不足以取样", size)
	}
	out := make([]int64, 0, SampleCount)
	for i := 0; i < SampleCount; i++ {
		v, err := s.randInt(span)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// randInt 返回 [0, n) 的随机数。生产用 crypto/rand —— 偏移必须**不可预测**,
// 否则攻击者可以对着哈希预言机逐段重建文件(每次只问可预测的那几段)。
func (s *Service) randInt(n int64) (int64, error) {
	if s.RandInt != nil {
		return s.RandInt(n)
	}
	if n <= 0 {
		return 0, nil
	}
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0, fmt.Errorf("生成随机偏移失败: %w", err)
	}
	return v.Int64(), nil
}

func (s *Service) key(uploadID string) (string, error) {
	return s.KV.Key("fastup", uploadID)
}

func newNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// equalBytes 恒定时间比较(避免用"摘要前几位匹配"做时序侧信道)。
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
