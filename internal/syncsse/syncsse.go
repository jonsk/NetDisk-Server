// Package syncsse 实现 SSE 事件通道的服务端(BE-S9-04/05 / 6.9 / R-18)。
//
// # 定位
//
// 远端变更的**推送**通道。它是 `/changes` 的"加速器"而不是替代品:
// 事件允许丢失(3.2 职责 4b 明文),丢了客户端靠 5 分钟兜底扫描与
// `/changes` 游标补拉 —— 因此这里的实现刻意**不做可靠投递**
// (不重发、不积压、不落盘),否则会把一条"尽力而为"的通道做成半个消息队列,
// 而消息队列该有的消费位点/重投/死信全都没有(半吊子可靠性比明确尽力而为更危险)。
//
// # 六条实现纪律(每一条都对应一次真实故障模式)
//
//  1. **帧 id = `{space_id}:{change_seq}`**(6.9):客户端用 Last-Event-ID 续传,
//     单一整数不够 —— 多空间并行时"我看到的最后一个 seq"必须带空间维度,
//     否则客户端会把 A 空间的 seq 当成 B 空间的游标(跨空间错位,漏文件)。
//  2. **心跳 25s**:中间层(Nginx/防火墙/NAT)会静默回收空闲连接。没有心跳时
//     客户端表现为"看起来连着但永远收不到事件",而服务端日志一切正常。
//  3. **按可见空间在连接内过滤**(而不是给每个空间开一个 Redis 订阅):
//     可见空间快照 + `member_changed` 时刷新 + **60s TTL 兜底**。TTL 是必要的:
//     成员变更事件本身可能丢失(纪律 0),只靠事件刷新会让"刚被拉进空间的人"
//     永远收不到该空间的事件。
//  4. **单用户连接上限 3**:浏览器/客户端重连风暴或标签页泄漏会让连接数无限增长。
//     超限时**踢掉最旧的那条**(而不是拒绝新的):拒绝新的会让用户陷入
//     "刷新也连不上"的死局,而最旧的通常是已经死掉的连接。
//  5. **慢消费者(写阻塞 >10s)直接断开**:SSE 是"推"的,推不动的连接会一直占着
//     goroutine + 缓冲区。断开后客户端会重连并从 `/changes` 补拉 —— 这正是
//     事件允许丢失的前提。
//  6. **`X-Accel-Buffering: no`**:不声明的话 Nginx 会缓冲响应,
//     事件被攒在代理里(实测表现为"事件延迟数十秒才到"),实时性完全丧失。
package syncsse

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Frame 是一条待推送的事件。
//
// 只带"哪个空间的哪一条变更"与最小负载:客户端收到后按游标去 `/changes` 拉详情。
// 刻意**不把完整 file 行推过去** —— 那会让事件体积随文件元数据膨胀,
// 而且客户端仍需要一套"按 seq 拉齐"的逻辑(事件会丢),等于把同一份数据做两遍。
type Frame struct {
	SpaceID   string `json:"space_id"`
	ChangeSeq int64  `json:"change_seq"`
	// Kind 便于客户端做"是否需要立刻处理"的粗筛(例如 shared_in 要弹提示)
	Kind string `json:"kind,omitempty"`
	// FileID 便于客户端直接定位条目
	FileID string `json:"file_id,omitempty"`
}

// EventID 是 SSE 的 `id:` 字段值(6.9:`{space_id}:{change_seq}`)。
func (f Frame) EventID() string {
	return fmt.Sprintf("%s:%d", f.SpaceID, f.ChangeSeq)
}

// Options 是通道参数(生产用 DefaultOptions,测试可调小)。
type Options struct {
	// Heartbeat 心跳间隔(默认 25s)
	Heartbeat time.Duration
	// WriteTimeout 单次写的最长等待(超过即认定慢消费者,默认 10s)
	WriteTimeout time.Duration
	// Buffer 每条连接的待发队列长度(默认 64)
	Buffer int
	// MaxConnsPerUser 单用户连接上限(默认 3)
	MaxConnsPerUser int
	// SpaceTTL 可见空间快照的存活时间(默认 60s)
	SpaceTTL time.Duration
	// Logger 事件级日志(可空)
	Logger *slog.Logger
}

// DefaultOptions 与架构文档的取值一致(6.9)。
func DefaultOptions() Options {
	return Options{
		Heartbeat:       25 * time.Second,
		WriteTimeout:    10 * time.Second,
		Buffer:          64,
		MaxConnsPerUser: 3,
		SpaceTTL:        60 * time.Second,
	}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.Heartbeat <= 0 {
		o.Heartbeat = d.Heartbeat
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = d.WriteTimeout
	}
	if o.Buffer <= 0 {
		o.Buffer = d.Buffer
	}
	if o.MaxConnsPerUser <= 0 {
		o.MaxConnsPerUser = d.MaxConnsPerUser
	}
	if o.SpaceTTL <= 0 {
		o.SpaceTTL = d.SpaceTTL
	}
	return o
}

// SpaceLister 返回用户**当前可见**的空间 id(判权在服务端,4.3)。
type SpaceLister func(ctx context.Context, userID string) ([]string, error)

// Conn 是一条已建立的 SSE 连接(由 Hub 管理)。
type Conn struct {
	UserID string
	// Ch 是待发帧队列。**满即视为慢消费者**(不阻塞发送方)
	Ch chan Frame
	// closed 保护:重复 Close 幂等
	closeOnce sync.Once
	closed    chan struct{}
	// created 用于"超限时踢最旧的"
	created time.Time

	// 可见空间快照(连接内缓存)
	mu         sync.Mutex
	spaces     map[string]struct{}
	spacesAt   time.Time
	spaceCache time.Duration

	hub *Hub
}

// Close 关闭连接(幂等)。
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.hub.remove(c)
	})
}

// Closed 在连接被关闭时关闭(供 handler 的 select 使用)。
func (c *Conn) Closed() <-chan struct{} { return c.closed }

// visible 判断某空间是否对该连接可见(带 TTL 兜底刷新)。
//
// TTL 存在的理由:成员变更事件本身可能丢(事件通道不保证投递),
// 只靠事件刷新会让"刚被拉进空间的人"永远收不到该空间的事件 ——
// 而 60s 之后自动刷新就把这类偏差收敛到一个可接受的窗口内。
func (c *Conn) visible(ctx context.Context, spaceID string) bool {
	c.mu.Lock()
	stale := c.spaces == nil || time.Since(c.spacesAt) > c.spaceCache
	c.mu.Unlock()
	if stale {
		if err := c.refreshSpaces(ctx); err != nil {
			// 刷新失败:按"当前快照"继续(过期的快照比"什么都不发"更接近正确),
			// 真正的判权在客户端拉 /changes 时还会再做一次(服务端唯一裁判)。
			c.hub.log().Warn("刷新可见空间失败", "user", c.UserID, "err", err)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.spaces[spaceID]
	return ok
}

func (c *Conn) refreshSpaces(ctx context.Context) error {
	ids, err := c.hub.listSpaces(ctx, c.UserID)
	if err != nil {
		return err
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	c.mu.Lock()
	c.spaces, c.spacesAt = set, time.Now()
	c.mu.Unlock()
	return nil
}

// Hub 管理全部本地连接并负责扇出。
//
// "本地"很关键:多个进程实例各自持有一批连接,而 Redis pub/sub 是**跨实例**
// 的扇出层(见 subscriber.go)。Hub 只做进程内的分发与连接治理。
type Hub struct {
	opts Options
	list SpaceLister

	mu    sync.Mutex
	conns map[string][]*Conn // userID → 连接(按创建时间升序)
	all   map[*Conn]struct{}

	// stats
	opened   int64
	rejected int64
	dropped  int64
}

// NewHub 构造 Hub。
func NewHub(list SpaceLister, opts Options) *Hub {
	return &Hub{opts: opts.withDefaults(), list: list, conns: map[string][]*Conn{}, all: map[*Conn]struct{}{}}
}

func (h *Hub) log() *slog.Logger {
	if h.opts.Logger != nil {
		return h.opts.Logger
	}
	return slog.Default()
}

// listSpaces 取用户当前可见的空间 id(判权在服务端;这里是 SSE 的连接内过滤依据)。
func (h *Hub) listSpaces(ctx context.Context, userID string) ([]string, error) {
	if h.list == nil {
		// 未装配判权源:返回空集合而不是"全部可见" —— 宁可少发也不能多发给
		// 一个没有权限的连接(多发的后果是**越权泄露**)。
		return nil, nil
	}
	return h.list(ctx, userID)
}

// Stats 是通道的累计计数(供监控:连接数、被踢数、慢消费者丢弃数)。
type Stats struct {
	Open     int   `json:"open"`
	Users    int   `json:"users"`
	Opened   int64 `json:"opened_total"`
	Rejected int64 `json:"rejected_total"`
	Dropped  int64 `json:"dropped_total"`
}

// Stats 读当前统计。
func (h *Hub) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return Stats{
		Open: len(h.all), Users: len(h.conns),
		Opened: h.opened, Rejected: h.rejected, Dropped: h.dropped,
	}
}

// Subscribe 为某用户建立一条连接。
//
// 单用户超限时**踢掉最旧的那条**(见包注释纪律 4),而不是拒绝新连接。
func (h *Hub) Subscribe(ctx context.Context, userID string) (*Conn, error) {
	c := &Conn{
		UserID: userID, Ch: make(chan Frame, h.opts.Buffer),
		closed: make(chan struct{}), created: time.Now(),
		spaceCache: h.opts.SpaceTTL, hub: h,
	}
	// 建连即取一次可见空间快照:第一条事件到达时不必先等一次 IO
	if err := c.refreshSpaces(ctx); err != nil {
		return nil, err
	}

	h.mu.Lock()
	existing := h.conns[userID]
	if len(existing) >= h.opts.MaxConnsPerUser {
		// 踢最旧(它最可能是客户端已经放弃、但服务端还没察觉的连接)
		oldest := existing[0]
		existing = existing[1:]
		h.rejected++
		h.mu.Unlock()
		oldest.Close()
		h.mu.Lock()
	}
	h.conns[userID] = append(existing, c)
	h.all[c] = struct{}{}
	h.opened++
	h.mu.Unlock()
	return c, nil
}

func (h *Hub) remove(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.conns[c.UserID]
	for i, x := range list {
		if x == c {
			list = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(list) == 0 {
		delete(h.conns, c.UserID)
	} else {
		h.conns[c.UserID] = list
	}
	delete(h.all, c)
}

// Broadcast 把一条事件扇出给**本进程**所有可见该空间的连接。
//
// 队列满 = 慢消费者 → 立刻关闭该连接(纪律 5):断开后客户端会重连并
// 从 `/changes` 补拉,而事件本身就是允许丢失的。
func (h *Hub) Broadcast(ctx context.Context, f Frame) {
	h.mu.Lock()
	targets := make([]*Conn, 0, len(h.all))
	for c := range h.all {
		targets = append(targets, c)
	}
	h.mu.Unlock()

	for _, c := range targets {
		if !c.visible(ctx, f.SpaceID) {
			continue
		}
		select {
		case c.Ch <- f:
		default:
			// 队列满:不断开连接本身,只丢弃这一条(客户端下一次 /changes 会补齐)。
			// 之所以不在这里 Close:写侧阻塞超过 WriteTimeout 才是"真的慢"，
			// 而"队列瞬时满"可能只是突发变更(例如一次大规模复制)。
			h.mu.Lock()
			h.dropped++
			h.mu.Unlock()
		}
	}
}

// Options 返回生效参数(handler 需要心跳与写超时)。
func (h *Hub) Options() Options { return h.opts }

// Encode 把帧编码为 SSE 报文(含 `id:` 与 `data:`)。
//
// 单独成函数是为了让"帧格式"只有一处定义:客户端与服务端的契约测试
// 都对着它,避免手写字符串时少一个冒号或漏一个空行(SSE 的报文以空行结束,
// 少了它客户端会一直等下去)。
func Encode(f Frame) ([]byte, error) {
	body, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("id: %s\nevent: change\ndata: %s\n\n", f.EventID(), body)), nil
}

// HeartbeatPayload 是心跳报文的注释行(SSE 的注释以 `:` 开头)。
//
// 用心跳而不是空 data 帧:注释不会触发客户端的 onmessage,
// 于是它只起到"保活 + 让中间层看到有流量"的作用,不会污染客户的变更处理逻辑。
var HeartbeatPayload = []byte(": keep-alive\n\n")
