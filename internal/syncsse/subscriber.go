package syncsse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis 频道名:全局一个频道(而不是"每空间一个")。
//
// 为什么不在 Redis 侧按空间分频道:进程内扇出本来就要遍历本进程的连接
// (可见空间过滤是**按用户**的,Redis 不知道谁可见哪个空间),
// 分成 N 个频道只会让订阅数随空间数增长,而过滤逻辑一行都省不掉。
const Channel = "netdisk:events"

// ErrNoRedis 表示未装配 Redis(单实例部署下的正常退化:只做本地扇出)。
var ErrNoRedis = errors.New("syncsse: redis 未装配")

// Publish 把一条变更事件广播给所有实例(BE-S9-05:提交后 PUBLISH)。
//
// **允许失败**:事件通道是尽力而为的 —— 发不出去只记日志,
// 绝不能让"已经提交的写操作"因为推送失败而回滚或报错(客户端会靠
// `/changes` 游标补拉,这才是权威路径)。
//
// origin 为空表示"没有来源实例"(测试/外部发布):此时订阅端**不做自回声抑制**
// (见 Subscriber.subscribeOnce),保证跨实例语义在任何调用方式下都成立。
func Publish(ctx context.Context, rdb *redis.Client, f Frame) error {
	return publishFrom(ctx, rdb, "", f)
}

// redisEnvelope 是**实例之间**的传输外壳:帧本体 + 发布者实例 id。
//
// 为什么需要它(2026-09-12 查明"同一变更投递两帧"):Publisher.Broadcast 会
// ①本地同步扇出(延迟更低)②再 PUBLISH 到 Redis;而**本实例自己也订阅着** Redis,
// 于是同一条变更会被本实例的客户端收到**两次**(id/seq 完全相同)。
// 这不是丢数据的 bug(客户端必须按 seq 去重,网络重发也会重复),但它是**双倍**的
// SSE 流量与双倍的客户端唤醒。
// 有了来源标记,订阅端就能把"自己刚发出去的那一份回声"丢掉,同时保留两条路径的
// 好处:本地路径给延迟、Redis 路径给跨实例与"Redis 短暂不可用时本地仍然送达"。
//
// ⚠ origin **只在实例之间传输**,绝不进客户端报文:客户端看到的永远是 Frame 本体
// (见 Encode)。因此这个字段对契约与两端客户端都是透明的。
type redisEnvelope struct {
	Origin string `json:"origin,omitempty"`
	Frame  Frame  `json:"frame"`
}

func publishFrom(ctx context.Context, rdb *redis.Client, origin string, f Frame) error {
	if rdb == nil {
		return ErrNoRedis
	}
	// 兼容性:origin 为空时仍然发**裸 Frame**(与历史格式一致),
	// 这样"新代码 + 旧实例"混跑时旧实例照样能解出事件。
	var payload any = f
	if origin != "" {
		payload = redisEnvelope{Origin: origin, Frame: f}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return rdb.Publish(ctx, Channel, body).Err()
}

// NewInstanceID 生成一个实例标识(仅用于区分"这条事件是不是我自己发的")。
// 不是安全标识,只用 crypto/rand 避免多实例重建后撞号即可。
func NewInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 取不到随机数也不能让服务起不来:退化成时间戳(极端情况下可能撞号,
		// 后果只是"自回声没被抑制"=退回今天的行为,不丢事件)。
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// Subscriber 订阅 Redis 频道并把事件扇出到本进程的连接。
//
// # 刻意不做可靠投递(6.9 / 3.2 职责 4b)
//
// 这里**没有**重试、没有本地积压、没有"未确认则重发":
//
//   - 事件丢失是可接受的:客户端有 `/changes` 游标(权威)与周期兜底扫描;
//   - 半吊子可靠(只重试一次、只积压 1000 条)会更糟 —— 它让"事件是否一定到达"
//     变成不确定,而客户端就无法决定"要不要靠游标兜底"(要么永远兜底=可靠投递没必要,
//     要么永远不兜底=漏事件就是漏文件)。
//
// 所以这里明确:**收到就发,发不出去就丢**;权威状态在 `/changes`。
type Subscriber struct {
	RDB *redis.Client
	Hub *Hub
	// InstanceID 本实例标识:与 Publisher 用**同一个**值时,自己发出去又经 Redis
	// 回来的那一份会被丢弃(自回声抑制)。空值 = 不抑制(退回历史行为)。
	InstanceID string
	// Log 逐条错误日志(可空)
	Log func(msg string, args ...any)
	// dropCount 统计丢弃的帧(丢弃本身就是正常路径,但要可观测)
	dropCount atomic.Int64
	// selfEchoCount 统计被抑制的自回声(诊断:它应当与本地写入次数同量级,
	// 长期为 0 说明 InstanceID 没接上 —— 那就又变成"每条变更收两帧"了)
	selfEchoCount atomic.Int64
}

// Dropped 返回因队列满而丢弃的帧数(监控用)。
func (s *Subscriber) Dropped() int64 { return s.dropCount.Load() }

// Run 阻塞运行订阅循环,直到 ctx 取消。
//
// 断线重连策略:Redis 客户端自身会重连,**但订阅不会自动恢复** ——
// 因此这里在外层循环里重建订阅。少了这层循环,Redis 一次重启就会让
// 所有实例静默失去事件源(客户端表现为"只剩 5 分钟兜底扫描"),
// 而这不会有任何报错。
func (s *Subscriber) Run(ctx context.Context) {
	if s.RDB == nil || s.Hub == nil {
		return
	}
	for {
		if err := s.subscribeOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logf("SSE 订阅中断,2s 后重连", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		return
	}
}

func (s *Subscriber) subscribeOnce(ctx context.Context) error {
	sub := s.RDB.Subscribe(ctx, Channel)
	defer func() { _ = sub.Close() }()
	// 收到订阅确认才算真的连上(否则会把"连接建立失败"当成"暂时没有事件")
	if _, err := sub.Receive(ctx); err != nil {
		return err
	}
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return errors.New("syncsse: 订阅通道已关闭")
			}
			f, self := s.decode(msg.Payload)
			if self {
				// 自己发出去、又经 Redis 回来的那一份:本地扇出已经发过了(见 Publisher),
				// 丢掉它以去掉"同一变更两帧"。**只丢回声,不丢事件**:
				// origin 不等于本实例、或载荷根本无法解析时,一律照发。
				s.selfEchoCount.Add(1)
				continue
			}
			if f == nil {
				// 载荷坏了:丢一条,不改整体行为
				s.dropCount.Add(1)
				continue
			}
			s.Hub.Broadcast(ctx, *f)
		}
	}
}

// decode 解析 Redis 载荷,返回帧与"这是不是我自己发的回声"。
//
// 兼容两种格式(滚动升级时新旧实例会同时在跑):
//   - 新:{"origin":"...","frame":{...}}
//   - 旧:{"space_id":"...",...}(裸 Frame)
//
// 解析失败返回 (nil,false) —— 由调用方计入 dropCount。
func (s *Subscriber) decode(payload string) (*Frame, bool) {
	var env redisEnvelope
	if err := json.Unmarshal([]byte(payload), &env); err == nil && env.Frame.SpaceID != "" {
		if s.InstanceID != "" && env.Origin == s.InstanceID {
			return &env.Frame, true
		}
		return &env.Frame, false
	}
	var f Frame
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return nil, false
	}
	return &f, false
}

// SelfEchoSuppressed 返回被抑制的自回声条数(监控/诊断用)。
//
// 它应当与"本实例的写入次数"同量级;长期恒为 0 说明 InstanceID 没有在
// Publisher 与 Subscriber 之间接上 —— 那意味着**每条变更又会被投递两帧**。
func (s *Subscriber) SelfEchoSuppressed() int64 { return s.selfEchoCount.Load() }

func (s *Subscriber) logf(msg string, args ...any) {
	if s.Log != nil {
		s.Log(msg, args...)
	}
}

// Broadcaster 是"把事件推出去"的窄接口(供写入路径依赖)。
//
// 写入路径(定稿/改名/…)只应依赖这一个方法:它不该知道事件走 Redis 还是别的,
// 也不该知道扇出细节 —— 更关键的是**它不能因为这个调用失败而回滚业务事务**
// (所以实现必须是"尽力而为",见 Publish)。
type Broadcaster interface {
	Broadcast(ctx context.Context, f Frame)
}

// Publisher 组合"发布到 Redis"与"本地直接扇出"。
//
// 为什么要**本地也发一次**:Redis pub/sub 只把消息投给**订阅者**,
// 而写入所在的实例自己也订阅着(见 Subscriber)——那份是异步的:
// 客户端在同一个实例上写完立刻等事件时,走本地同步扇出延迟更低。
//
// ⚠ 曾经这里必然产生**重复投递**(本地一次 + 订阅回声一次,id/seq 完全相同,
// 2026-09-12 真机观测到"同一变更两帧")。现在由 InstanceID + 订阅端自回声抑制
// 去掉了回声:客户端**每条变更只收到一帧**,而两条路径的容错都在
// (Redis 不可用时本地路径依旧送达;跨实例依旧经 Redis)。
type Publisher struct {
	RDB *redis.Client
	Hub *Hub
	// InstanceID 本实例标识;空值则首次 Broadcast 时自动生成。
	// 必须与 Subscriber.InstanceID 一致,否则回声不会被抑制(见 selfID)。
	InstanceID string
	Log        func(msg string, args ...any)

	idOnce sync.Once
}

// selfID 返回本实例标识(惰性生成,避免"忘了在装配处设置"导致退化)。
func (p *Publisher) selfID() string {
	p.idOnce.Do(func() {
		if p.InstanceID == "" {
			p.InstanceID = NewInstanceID()
		}
	})
	return p.InstanceID
}

// Broadcast 先本地扇出,再发布到 Redis(失败只记日志)。
func (p *Publisher) Broadcast(ctx context.Context, f Frame) {
	if p.Hub != nil {
		p.Hub.Broadcast(ctx, f)
	}
	if p.RDB == nil {
		return
	}
	if err := publishFrom(ctx, p.RDB, p.selfID(), f); err != nil && p.Log != nil {
		p.Log("发布 SSE 事件失败(事件允许丢失,客户端会靠 /changes 补拉)", "err", err)
	}
}

// 编译期确认 Publisher 满足 Broadcaster(写入路径只依赖窄接口)。
var _ Broadcaster = (*Publisher)(nil)

// 保证 sync 被使用(mu 供将来加"订阅健康"状态用)
var _ = sync.Mutex{}
