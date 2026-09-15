package api_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/syncsse"
)

// SSE 端点的 HTTP 层测试(BE-S9-04):**真实长连接**。
//
// 这组用例必须用真实 HTTP(httptest.NewServer + http.Client),不能用
// httptest.ResponseRecorder:后者的 Write 从不阻塞、Flush 是空操作,
// 于是"帧真的到达客户端""慢消费者真的被断开"这两条都测不出来 ——
// 而那正是 SSE 最容易出问题的地方。

// sseServer 起一个真实 HTTP 服务,返回 (地址, hub, 关闭函数)。
func sseServer(t *testing.T, e *fileEnv, opts syncsse.Options) (string, *syncsse.Hub) {
	t.Helper()
	// 复用被测 handler 的装配,但把 Hub 换成参数可控的实例(测试要小心跳/短超时)
	hub := syncsse.NewHub(func(ctx context.Context, userID string) ([]string, error) {
		return []string{e.spaceID}, nil
	}, opts)
	e.setEvents(hub)
	srv := httptest.NewServer(e.handler)
	t.Cleanup(srv.Close)
	return srv.URL, hub
}

// readSSE 读一条 SSE 报文(以空行结束),返回原始行。
func readSSE(t *testing.T, r *bufio.Reader, timeout time.Duration) []string {
	t.Helper()
	type res struct {
		lines []string
		err   error
	}
	ch := make(chan res, 1)
	go func() {
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				ch <- res{lines, err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				ch <- res{lines, nil}
				return
			}
			lines = append(lines, line)
		}
	}()
	select {
	case got := <-ch:
		if got.err != nil && len(got.lines) == 0 {
			t.Fatalf("读 SSE 失败: %v", got.err)
		}
		return got.lines
	case <-time.After(timeout):
		t.Fatal("等待 SSE 报文超时")
		return nil
	}
}

// **验收⑥**:响应头必须声明不许缓冲(Nginx 缓冲会让事件延迟数十秒)
// 以及 ready 事件先到、随后能收到真实变更帧(id 为 `{space}:{seq}`)
func TestEventsStreamsFrames(t *testing.T) {
	e := setupSyncEnv(t)
	base, hub := sseServer(t, e, syncsse.Options{Heartbeat: 200 * time.Millisecond, Buffer: 8})

	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 应 200,实际 %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream,实际 %q", ct)
	}
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("必须声明 X-Accel-Buffering: no(否则 Nginx 会缓冲事件),实际 %q",
			resp.Header.Get("X-Accel-Buffering"))
	}
	if resp.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control 应为 no-cache,实际 %q", resp.Header.Get("Cache-Control"))
	}
	r := bufio.NewReader(resp.Body)

	// 第一条:ready
	lines := readSSE(t, r, 3*time.Second)
	if len(lines) < 1 || !strings.HasPrefix(lines[0], "event: ready") {
		t.Fatalf("第一条应为 ready 事件,实际 %v", lines)
	}

	// 推一条真实变更:帧 id 必须是 `{space_id}:{change_seq}`
	hub.Broadcast(context.Background(), syncsse.Frame{
		SpaceID: e.spaceID, ChangeSeq: 77, Kind: "created", FileID: "f-1",
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		lines = readSSE(t, r, 3*time.Second)
		joined := strings.Join(lines, "\n")
		if !strings.Contains(joined, "event: change") {
			continue // 心跳(注释行)跳过
		}
		if !strings.Contains(joined, "id: "+e.spaceID+":77") {
			t.Fatalf("帧 id 应为 {space_id}:{change_seq},实际 %v", lines)
		}
		if !strings.Contains(joined, `"change_seq":77`) {
			t.Fatalf("data 载荷应含 change_seq,实际 %v", lines)
		}
		return
	}
	t.Fatal("未收到变更帧")
}

// **验收②** 心跳:即使没有任何变更,连接也会定期收到注释行(保活)
func TestEventsHeartbeatKeepsAlive(t *testing.T) {
	e := setupSyncEnv(t)
	base, _ := sseServer(t, e, syncsse.Options{Heartbeat: 100 * time.Millisecond})

	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("建连失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	r := bufio.NewReader(resp.Body)
	_ = readSSE(t, r, 3*time.Second) // ready

	lines := readSSE(t, r, 3*time.Second)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], ":") {
		t.Fatalf("应收到心跳注释行,实际 %v", lines)
	}
}

// **验收④** 单用户连接上限 3:第 4 条进来时踢最旧的
func TestEventsConnectionLimit(t *testing.T) {
	e := setupSyncEnv(t)
	base, hub := sseServer(t, e, syncsse.Options{Heartbeat: time.Hour, MaxConnsPerUser: 3})

	client := &http.Client{Timeout: 10 * time.Second}
	var bodies []*bufio.Reader
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+e.token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("第 %d 条连接失败: %v", i+1, err)
		}
		defer func() { _ = resp.Body.Close() }()
		bodies = append(bodies, bufio.NewReader(resp.Body))
		// 确保连接已被 hub 记账
		deadline := time.Now().Add(2 * time.Second)
		for hub.Stats().Open < i+1 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if got := hub.Stats().Open; got != 3 {
		t.Fatalf("应有 3 条连接,实际 %d", got)
	}
	// 第 4 条:应成功建立(踢最旧),总连接数仍是 3
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("第 4 条连接应成功(踢最旧的而不是拒绝新的): %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	for hub.Stats().Rejected == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Stats().Rejected != 1 {
		t.Fatalf("应记录 1 次驱逐,实际 %d", hub.Stats().Rejected)
	}
	if got := hub.Stats().Open; got > 3 {
		t.Fatalf("总连接数不应超过 3,实际 %d", got)
	}
}

// 未认证 → 401(SSE 也是接口,不是匿名广播)
func TestEventsRequiresAuth(t *testing.T) {
	e := setupSyncEnv(t)
	base, _ := sseServer(t, e, syncsse.Options{Heartbeat: time.Hour})
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/events", nil)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401,实际 %d", resp.StatusCode)
	}
}
