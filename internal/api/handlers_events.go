package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/syncfeed"
	"github.com/netdisk/netdisk/internal/syncsse"
)

// SSE 事件通道(BE-S9-04 / 6.9 / R-18)。
//
// 与 `/changes` 的分工:这里是"快"(秒级知道有变化),那里是"全"(权威、可补拉)。
// 事件允许丢失,客户端订阅失败或丢事件时靠 `/changes` 游标兜底 ——
// 因此本端点**不做**任何可靠投递,只负责尽快把"哪个空间的哪一条 seq 变了"推出去。

// handleEvents 建立 SSE 长连接。
//
// 六条纪律的落地位置(见 syncsse 包注释):
//   - 帧 `id: {space_id}:{change_seq}` 与 `event: change` → syncsse.Encode
//   - 心跳 25s → 本函数的 ticker
//   - 连接内按可见空间过滤(快照 + 60s TTL)→ Conn.visible
//   - 单用户连接上限 3(踢最旧)→ Hub.Subscribe
//   - 慢消费者写阻塞 >10s 断开 → WriteTimeout 写死线
//   - `X-Accel-Buffering: no` → 响应头(Nginx 缓冲会让事件延迟数十秒才到)
func (d Deps) handleEvents(w http.ResponseWriter, r *http.Request) {
	if d.Events == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("事件通道未装配")))
		return
	}
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// 不支持 Flush 的实现(理论上只有测试替身)会让 SSE 退化成"攒够缓冲才发",
		// 必须显式报错而不是静默降级。
		apierr.Write(w, r, apierr.Internal(errors.New("响应写入器不支持流式刷新")))
		return
	}

	conn, err := d.Events.Subscribe(r.Context(), a.UserID)
	if err != nil {
		apierr.Write(w, r, apierr.Internal(err))
		return
	}
	defer conn.Close()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Nginx 必须关缓冲(3.2 职责 4b):否则事件被代理攒着,实时性完全丧失
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// 告诉客户端"连接已就绪 + 从哪个头上开始补拉":客户端在收到 ready 之前
	// 发生的变更它并不知道,ready 里的 head_seq 就是它的补拉起点。
	head := int64(0)
	if d.DB != nil {
		if ids, lerr := d.eventSpaces(r.Context(), a.UserID); lerr == nil && len(ids) > 0 {
			// 取第一个可见空间的头指针做提示即可(head 只用于提示"从哪开始补",
			// 精确的补拉仍由客户端对每个空间各调一次 /changes)
			head = d.syncHead(r.Context(), ids[0])
		}
	}
	ready, _ := json.Marshal(map[string]any{"user_id": a.UserID, "head_seq_hint": head})
	if _, werr := w.Write([]byte("event: ready\ndata: " + string(ready) + "\n\n")); werr != nil {
		return
	}
	flusher.Flush()

	opts := d.Events.Options()
	heartbeat := time.NewTicker(opts.Heartbeat)
	defer heartbeat.Stop()

	rc := http.NewResponseController(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-conn.Closed():
			return
		case <-heartbeat.C:
			// 心跳用注释行(`:` 开头):只保活、不触发客户端的 onmessage
			if err := writeWithDeadline(rc, w, flusher, syncsse.HeartbeatPayload, opts.WriteTimeout); err != nil {
				return
			}
		case f := <-conn.Ch:
			payload, eerr := syncsse.Encode(f)
			if eerr != nil {
				continue
			}
			if err := writeWithDeadline(rc, w, flusher, payload, opts.WriteTimeout); err != nil {
				// 慢消费者(写阻塞超过 WriteTimeout)→ 断开。
				// 断开是**正确**的处理:客户端会重连并从 /changes 补拉,
				// 而一直占着 goroutine 与缓冲区会把服务端拖垮。
				return
			}
		}
	}
}

// writeWithDeadline 带写死线地写一帧并立刻 Flush。
//
// 用 `http.ResponseController.SetWriteDeadline` 而不是"起 goroutine + 计时器":
// 后者在超时后无法真正取消已经陷入内核的写,只能泄漏一个 goroutine,
// 而 SetWriteDeadline 会让写**真的**失败返回错误。
func writeWithDeadline(rc *http.ResponseController, w http.ResponseWriter, f http.Flusher, b []byte, d time.Duration) error {
	if d > 0 {
		if err := rc.SetWriteDeadline(time.Now().Add(d)); err != nil {
			// 底层不支持死线(少数 ResponseWriter 实现):退化为"尽力写",
			// 但**必须**继续 —— 直接失败会让所有 SSE 都连不上。
			_ = err
		}
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	f.Flush()
	return nil
}

// eventSpaces 返回用户可见的空间 id(供 SSE 的 ready 提示用)。
//
// 直接走 repo.ListVisible 而不是 spacesvc:后者的每个用例方法都带"调用者意图"
// (列成员/加成员/转让…),而这里只需要"这个用户能看到哪些空间"这一条读查询 ——
// 判权规则本身在 repo 的 SQL 里(4.3 的唯一口径)。
func (d Deps) eventSpaces(ctx context.Context, userID string) ([]string, error) {
	if d.DB == nil {
		return nil, nil
	}
	spaces, err := repo.SpaceRepo{}.ListVisible(ctx, db.AsQuerier(d.DB), userID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(spaces))
	for _, sp := range spaces {
		out = append(out, sp.ID)
	}
	return out, nil
}

// syncHead 读某空间当前的最大 change_seq(供 SSE ready 提示)。
func (d Deps) syncHead(ctx context.Context, spaceID string) int64 {
	if d.DB == nil {
		return 0
	}
	seq, err := syncfeed.HeadSeq(ctx, db.AsQuerier(d.DB), spaceID)
	if err != nil {
		return 0
	}
	return seq
}

// publishWrite 在**写操作成功提交之后**推送一条事件(BE-S9-05)。
//
// 为什么用"当前头指针"当 change_seq(而不是精确的那一条):
//   - 写路径分散在 filesvc 的五个方法里,让它们各自回传 seq 会改动一批签名;
//   - 而事件对客户端只是**提示**(它收到后按自己的游标去 `/changes` 拉),
//     用一个**不小于**真实 seq 的上界完全够用,且永远不会"报小了导致客户端漏拉";
//   - 多条变更连续发生时会推送若干条同 seq 的事件 → 客户端按 seq 去重(它本来
//     就必须去重:本地扇出 + Redis 回投会重复,网络重发也会)。
//
// 失败**绝不影响响应**:事件允许丢失,权威路径是 `/changes`。
func (d Deps) publishWrite(ctx context.Context, spaceID, fileID, kind string) {
	if d.Events == nil || spaceID == "" {
		return
	}
	d.broadcast(ctx, syncsse.Frame{
		SpaceID: spaceID, ChangeSeq: d.syncHead(ctx, spaceID),
		Kind: kind, FileID: fileID,
	})
}

// broadcast 是唯一的推出口:优先走 Broadcaster(本地 + Redis 跨实例),
// 未装配时退回 Hub 的本地扇出(单实例部署下两者等价)。
func (d Deps) broadcast(ctx context.Context, f syncsse.Frame) {
	if d.Broadcaster != nil {
		d.Broadcaster.Broadcast(ctx, f)
		return
	}
	if d.Events != nil {
		d.Events.Broadcast(ctx, f)
	}
}

// publishSeq 用**已知的精确 seq** 推送事件(定稿路径手上有它)。
func (d Deps) publishSeq(ctx context.Context, spaceID, fileID, kind string, seq int64) {
	if d.Events == nil || spaceID == "" {
		return
	}
	if seq <= 0 {
		d.publishWrite(ctx, spaceID, fileID, kind)
		return
	}
	d.broadcast(ctx, syncsse.Frame{
		SpaceID: spaceID, ChangeSeq: seq, Kind: kind, FileID: fileID,
	})
}
