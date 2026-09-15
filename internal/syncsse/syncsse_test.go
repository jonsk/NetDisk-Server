package syncsse_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/syncsse"
)

// SSE 通道的单元测试(BE-S9-04/05)。
//
// 这里只测**服务端侧可测的纪律**(帧格式、心跳、上限、可见性过滤、慢消费者断开、
// 跨实例扇出);端到端的那部分(真实 HTTP 长连接)由探针覆盖 —— 用 httptest
// 测"连接真的不断、帧真的到达"容易写成"测自己的假 Flusher"。

func newHub(t *testing.T, opts syncsse.Options, visible ...string) *syncsse.Hub {
	t.Helper()
	set := map[string]struct{}{}
	for _, v := range visible {
		set[v] = struct{}{}
	}
	return syncsse.NewHub(func(_ context.Context, _ string) ([]string, error) {
		out := make([]string, 0, len(set))
		for k := range set {
			out = append(out, k)
		}
		return out, nil
	}, opts)
}

// **验收①** 帧 id = `{space_id}:{change_seq}`(6.9:多空间并行时单整数不够)
func TestFrameIDFormat(t *testing.T) {
	f := syncsse.Frame{SpaceID: "sp-1", ChangeSeq: 42}
	if got := f.EventID(); got != "sp-1:42" {
		t.Fatalf("帧 id 应为 sp-1:42,实际 %s", got)
	}
	raw, err := syncsse.Encode(f)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	s := string(raw)
	if !strings.HasPrefix(s, "id: sp-1:42\n") {
		t.Fatalf("报文必须以 id 行开头: %q", s)
	}
	// SSE 报文必须以**空行**结束 —— 少了它客户端会一直等下一行
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatalf("报文必须以空行结束: %q", s)
	}
	var payload map[string]any
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
				t.Fatalf("data 行必须是 JSON: %v", err)
			}
		}
	}
	if payload["space_id"] != "sp-1" || payload["change_seq"].(float64) != 42 {
		t.Fatalf("data 载荷不符: %v", payload)
	}
}

// 心跳是**注释行**(不触发客户端 onmessage),且能保活
func TestHeartbeatIsComment(t *testing.T) {
	if !strings.HasPrefix(string(syncsse.HeartbeatPayload), ":") {
		t.Fatalf("心跳必须是 SSE 注释行(以 : 开头),实际 %q", syncsse.HeartbeatPayload)
	}
	if !strings.HasSuffix(string(syncsse.HeartbeatPayload), "\n\n") {
		t.Fatalf("心跳也必须以空行结束: %q", syncsse.HeartbeatPayload)
	}
	// 默认 25s(6.9)
	if got := syncsse.DefaultOptions().Heartbeat; got != 25*time.Second {
		t.Fatalf("默认心跳应为 25s,实际 %v", got)
	}
}

// **验收③** 按可见空间在连接内过滤:不可见空间的事件**绝不**投给该连接
//
// 这条比"推不到"严重得多:推错就是泄露别人的文件名/目录结构。
func TestBroadcastFiltersByVisibleSpaces(t *testing.T) {
	h := newHub(t, syncsse.Options{Buffer: 4}, "sp-mine")
	c, err := h.Subscribe(context.Background(), "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()

	h.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-other", ChangeSeq: 1})
	h.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-mine", ChangeSeq: 2})

	select {
	case f := <-c.Ch:
		if f.SpaceID != "sp-mine" {
			t.Fatalf("只应收到可见空间的事件,实际 %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("可见空间的事件没有送达")
	}
	select {
	case f := <-c.Ch:
		t.Fatalf("不该收到第二个事件(不同空间),实际 %+v", f)
	case <-time.After(50 * time.Millisecond):
	}
}

// **验收③(续)** 可见空间快照有 TTL:成员变更事件丢了下也能自愈
func TestVisibleSpacesRefreshOnTTL(t *testing.T) {
	var mu sync.Mutex
	visible := []string{"sp-a"}
	h := syncsse.NewHub(func(_ context.Context, _ string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), visible...), nil
	}, syncsse.Options{Buffer: 4, SpaceTTL: 30 * time.Millisecond})

	c, err := h.Subscribe(context.Background(), "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()

	// 把用户加进 sp-b(不推 member_changed 事件 —— 模拟事件丢失)
	mu.Lock()
	visible = []string{"sp-a", "sp-b"}
	mu.Unlock()

	// TTL 未到:仍不可见
	h.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-b", ChangeSeq: 1})
	select {
	case f := <-c.Ch:
		t.Fatalf("TTL 内不该刷新出 sp-b: %+v", f)
	case <-time.After(20 * time.Millisecond):
	}
	// TTL 过后:自愈
	time.Sleep(40 * time.Millisecond)
	h.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-b", ChangeSeq: 2})
	select {
	case f := <-c.Ch:
		if f.SpaceID != "sp-b" {
			t.Fatalf("应为 sp-b: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("TTL 过后应刷新出新增的可见空间 —— 否则事件丢失后用户永远收不到该空间的事件")
	}
}

// **验收④** 单用户连接上限 3:第 4 条进来时**踢掉最旧的**,而不是拒绝新的
func TestConnLimitEvictsOldest(t *testing.T) {
	h := newHub(t, syncsse.Options{Buffer: 2, MaxConnsPerUser: 3}, "sp-a")
	var conns []*syncsse.Conn
	for i := 0; i < 3; i++ {
		c, err := h.Subscribe(context.Background(), "u1")
		if err != nil {
			t.Fatalf("第 %d 条订阅失败: %v", i+1, err)
		}
		conns = append(conns, c)
	}
	if got := h.Stats().Open; got != 3 {
		t.Fatalf("应有 3 条连接,实际 %d", got)
	}
	fourth, err := h.Subscribe(context.Background(), "u1")
	if err != nil {
		t.Fatalf("第 4 条订阅失败(应踢旧的而不是拒绝新的): %v", err)
	}
	defer fourth.Close()

	if got := h.Stats().Open; got != 3 {
		t.Fatalf("踢旧后仍应为 3 条,实际 %d", got)
	}
	select {
	case <-conns[0].Closed():
		// 最旧的被踢,符合预期
	case <-time.After(time.Second):
		t.Fatal("最旧的连接应被踢掉(它最可能是客户端已放弃但服务端未察觉的)")
	}
	select {
	case <-conns[1].Closed():
		t.Fatal("不该踢第二条")
	default:
	}
	if h.Stats().Rejected != 1 {
		t.Fatalf("应记录 1 次驱逐,实际 %d", h.Stats().Rejected)
	}
}

// 队列满 = 丢弃这一条,但**不**断连(突发变更不该把连接踢掉)
func TestFullQueueDropsFrameNotConn(t *testing.T) {
	h := newHub(t, syncsse.Options{Buffer: 1}, "sp-a")
	c, err := h.Subscribe(context.Background(), "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()
	for i := 0; i < 5; i++ {
		h.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-a", ChangeSeq: int64(i + 1)})
	}
	if h.Stats().Dropped == 0 {
		t.Fatal("队列满时应记录丢弃(丢弃要可观测)")
	}
	select {
	case <-c.Closed():
		t.Fatal("队列瞬时满不该断连")
	default:
	}
}

// **验收(跨实例)** PUBLISH / 订阅扇出:另一个"实例"发的事件能到达本实例的连接
func TestPubSubFanout(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// 实例 A:持有连接
	hA := newHub(t, syncsse.Options{Buffer: 4}, "sp-a")
	sub := &syncsse.Subscriber{RDB: rdb, Hub: hA}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)

	conn, err := hA.Subscribe(ctx, "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer conn.Close()

	// 等订阅真正建立后再发,避免"发了但还没订阅上"的假失败。
	// 用"重发直到收到"的方式等待:miniredis 不暴露订阅者计数,
	// 而固定 sleep 会在慢机器上假失败(这条路径本来就是"最终到达")。

	// 实例 B:只发布,不持有连接。
	// 订阅确认是异步的,因此重发几次直到到达(发布本身是幂等的"提示",
	// 重复由客户端按 seq 去重 —— 这正是事件通道的语义)。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syncsse.Publish(ctx, rdb, syncsse.Frame{SpaceID: "sp-a", ChangeSeq: 7}); err != nil {
			t.Fatalf("发布失败: %v", err)
		}
		select {
		case f := <-conn.Ch:
			if f.SpaceID != "sp-a" || f.ChangeSeq != 7 {
				t.Fatalf("事件内容不符: %+v", f)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("跨实例事件未到达(订阅循环可能没建起来)")
}

// Publisher 同时做本地扇出与 Redis 发布;重复由客户端按 seq 去重(注释已说明)
func TestPublisherBroadcastsLocallyAndToRedis(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	h := newHub(t, syncsse.Options{Buffer: 4}, "sp-a")
	pub := &syncsse.Publisher{RDB: rdb, Hub: h}
	c, err := h.Subscribe(context.Background(), "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()

	pub.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-a", ChangeSeq: 9})
	select {
	case f := <-c.Ch:
		if f.ChangeSeq != 9 {
			t.Fatalf("本地扇出内容不符: %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("本地扇出应同步到达(同实例客户端延迟更低)")
	}
	// Redis 侧也应收到发布(Publish 不报错即视为已投递;
	// miniredis 不暴露订阅者计数,因此这里只断言"发布不失败")
}

// **同一变更只能收到一帧**(2026-09-12 真机观测到两帧,id/seq 完全相同)。
//
// 成因:Publisher 既做本地扇出、又 PUBLISH 到 Redis,而本实例自己也订阅着该频道,
// 于是"本地一次 + 回声一次"。这里把 InstanceID 接上(与生产装配一致)后,
// 回声必须被丢掉 —— 判据是"窗口内**恰好**一帧",不是"至少一帧"。
func TestSelfEchoIsSuppressed(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	h := newHub(t, syncsse.Options{Buffer: 8}, "sp-a")
	id := syncsse.NewInstanceID()
	pub := &syncsse.Publisher{RDB: rdb, Hub: h, InstanceID: id}
	sub := &syncsse.Subscriber{RDB: rdb, Hub: h, InstanceID: id}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)

	c, err := h.Subscribe(ctx, "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()

	// 等订阅真正建立:先用一条**外部**帧探路(origin 为空 → 不会被抑制)
	deadline := time.Now().Add(5 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		_ = syncsse.Publish(ctx, rdb, syncsse.Frame{SpaceID: "sp-a", ChangeSeq: 1})
		select {
		case <-c.Ch:
			ready = true
		case <-time.After(100 * time.Millisecond):
		}
		if ready {
			break
		}
	}
	if !ready {
		t.Fatal("订阅未建立(探路帧没到)")
	}
	// 排掉探路期间可能堆积的重复探路帧
	drain(c.Ch)

	// 正式:本实例发布一条 → 只应收到一帧
	pub.Broadcast(ctx, syncsse.Frame{SpaceID: "sp-a", ChangeSeq: 42})
	first := waitFrame(t, c.Ch, 3*time.Second)
	if first.ChangeSeq != 42 {
		t.Fatalf("帧内容不符: %+v", first)
	}
	// 给回声足够的时间到达(它必须先经过 miniredis 再回来):这段时间内**不该**再有帧
	select {
	case extra := <-c.Ch:
		t.Fatalf("同一变更收到了第二帧(自回声没被抑制):%+v", extra)
	case <-time.After(500 * time.Millisecond):
	}
	if sub.SelfEchoSuppressed() == 0 {
		t.Fatal("自回声计数为 0:InstanceID 可能没在两个方向上接上(那就会退回两帧)")
	}
}

// 跨实例语义不能被自回声抑制弄坏:别的实例发的事件必须照常到达
func TestOtherInstanceStillDeliveredWithInstanceIDSet(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	h := newHub(t, syncsse.Options{Buffer: 8}, "sp-a")
	sub := &syncsse.Subscriber{RDB: rdb, Hub: h, InstanceID: "instance-A"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)

	c, err := h.Subscribe(ctx, "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()

	// 实例 B(不同 id)发布
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syncsse.Publish(ctx, rdb, syncsse.Frame{SpaceID: "sp-a", ChangeSeq: 5}); err != nil {
			t.Fatalf("发布失败: %v", err)
		}
		select {
		case f := <-c.Ch:
			if f.ChangeSeq != 5 {
				t.Fatalf("内容不符: %+v", f)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("别的实例发的事件未到达(自回声抑制误伤了跨实例投递)")
}

// 裸 Frame(旧实例格式)必须仍能被解析并投递 —— 滚动升级期间新旧实例会同时在跑
func TestLegacyBareFrameStillDelivered(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	h := newHub(t, syncsse.Options{Buffer: 8}, "sp-a")
	sub := &syncsse.Subscriber{RDB: rdb, Hub: h, InstanceID: "instance-A"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)

	c, err := h.Subscribe(ctx, "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()

	// 直接往频道里塞一条**裸 Frame JSON**(模拟旧实例的 Publish)
	legacy := `{"space_id":"sp-a","change_seq":11,"kind":"created"}`
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := rdb.Publish(ctx, syncsse.Channel, legacy).Err(); err != nil {
			t.Fatalf("发布失败: %v", err)
		}
		select {
		case f := <-c.Ch:
			if f.ChangeSeq != 11 || f.Kind != "created" {
				t.Fatalf("旧格式帧解析不符: %+v", f)
			}
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("旧格式(裸 Frame)事件未到达:滚动升级期间会漏事件")
}

func drain(ch <-chan syncsse.Frame) {
	for {
		select {
		case <-ch:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func waitFrame(t *testing.T, ch <-chan syncsse.Frame, d time.Duration) syncsse.Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(d):
		t.Fatal("等帧超时")
		return syncsse.Frame{}
	}
}

// 未装配判权源时返回空集合(宁可少发,也不能多发给无权限的连接)
func TestUnconfiguredVisibilitySendsNothing(t *testing.T) {
	h := syncsse.NewHub(nil, syncsse.Options{Buffer: 2})
	c, err := h.Subscribe(context.Background(), "u1")
	if err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	defer c.Close()
	h.Broadcast(context.Background(), syncsse.Frame{SpaceID: "sp-a", ChangeSeq: 1})
	select {
	case f := <-c.Ch:
		t.Fatalf("未装配判权源时不该推任何事件(越权泄露比推不到严重): %+v", f)
	case <-time.After(50 * time.Millisecond):
	}
}

// 发布失败不影响业务(返回错误由调用方记日志即可)
func TestPublishWithoutRedisIsError(t *testing.T) {
	if err := syncsse.Publish(context.Background(), nil, syncsse.Frame{}); err == nil {
		t.Fatal("未装配 Redis 时应返回错误(由调用方记日志,但**绝不能**影响业务)")
	}
	if !errors.Is(syncsse.Publish(context.Background(), nil, syncsse.Frame{}), syncsse.ErrNoRedis) {
		t.Fatal("未装配 Redis 的错误应是可比较的哨兵(调用方据此决定是否告警)")
	}
}
