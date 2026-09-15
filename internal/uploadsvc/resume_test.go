package uploadsvc_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 本文件是 **TS-05 弱网与断点续传** 的验收测试
// ("上传/下载中途断网、进程崩溃、代理超时后均能从断点继续;ticket 全程有效")。
//
// 为什么这一组用例走**真实路由 + 真实 TCP**(api.New + httptest.NewServer),而不是
// 直接调 uploadsvc 的方法:
//   - "中途断网"与"代理超时"这两种故障**只存在于连接层**。在服务层看,它们都只是
//     "io.Reader 提前结束"—— 而验收要问的是"连接被放弃之后,偏移还可信吗、还能接着传吗",
//     这只有在请求体真的断在半路、连接真的被掐掉时才问得出来。
//   - "进程崩溃"要求把内存态整个丢掉。这里用**新建服务实例 + 新建存储实例 + 新建路由**
//     来模拟重启,只有数据库与存储根目录沿用 —— 于是"偏移还在"只可能来自持久化状态。
//
// "断网"最贴近诚实的模拟方式:单进程测试里不可能真的拔网线,于是让请求体只产出半截、
// 然后返回连接错误,由 transport 放弃这次请求(见 abandonAfter / timeoutBody 的注释)。
// 服务端看到的就是它真正会遇到的东西:r.Body 读到一半 unexpected EOF;
// 且"服务端已落盘多少"由 waitStageSize 变成确定值,断言不依赖调度时序。
//
// 真实 Postgres (NETDISK_TEST_DSN) + 真实暂存目录,不用任何 mock。

const resumeTestSecret = "0123456789abcdef0123456789abcdef"

// resumeLogger 返回丢弃式 logger(中间件链要求非 nil)。
func resumeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// resumeEnv 是测试环境:复用 uploadsvc 的 fixture(真实库 + 用户/空间/根目录),
// 另加一个**只建一次的存储根目录**。
//
// 根目录只用 t.TempDir() 建一次并记住路径:模拟"进程崩溃后重启"时,新进程必须能读到
// 同一个目录里的暂存文件 —— 换一个目录等于换了台机器,那测的就不是"崩溃后恢复"。
type resumeEnv struct {
	*fixture
	storageRoot string
	cfg         *config.Config
	tokens      *auth.Manager
	token       string
}

func setupResume(t *testing.T) *resumeEnv {
	t.Helper()
	f := setup(t)

	var username, role string
	var tokenVersion int64
	if err := f.database.Pool.QueryRow(context.Background(),
		`SELECT username, role, token_version FROM users WHERE id = $1`, f.userID).
		Scan(&username, &role, &tokenVersion); err != nil {
		t.Fatalf("读测试用户失败: %v", err)
	}

	cfg := config.Default()
	cfg.JWT.Secret = resumeTestSecret
	cfg.Log.Level = "error"

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	tokens := auth.NewManager(cfg.JWT, rdb, cfg.Redis.KeyPrefix)

	out, err := tokens.Issue(context.Background(), auth.IssueInput{
		UserID: f.userID, Username: username, Role: role,
		TokenVersion: tokenVersion, Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发测试令牌失败: %v", err)
	}
	return &resumeEnv{fixture: f, storageRoot: t.TempDir(), cfg: cfg, tokens: tokens, token: out.AccessToken}
}

// resumeProc 是"一次进程":全新的服务实例 + 全新的存储实例 + 全新的路由。
type resumeProc struct {
	handler http.Handler
	store   *storage.FS
}

// boot 模拟一次进程启动。所有服务对象都是新的(没有任何跨请求的内存状态),
// 但**数据库与存储根目录沿用** —— 这正是"崩溃后重启"的定义。
func (e *resumeEnv) boot(t *testing.T) *resumeProc {
	t.Helper()
	store, err := storage.NewFS(e.storageRoot)
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	namePol := namepolicy.Default()
	q := db.AsQuerier(e.database)

	uploads := &uploadsvc.Service{
		Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Uploads: repo.UploadRepo{},
		DB: q, Name: namePol,
	}
	fin := &finalize.Service{
		Pool: e.database.Pool, Storage: store,
		Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, Name: namePol,
	}
	tus := &uploadsvc.TUSService{Service: uploads, Stager: store, Finalizer: fin, DB: q}
	files := &filesvc.Service{Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, DB: q}

	h := api.New(api.Deps{
		Cfg: e.cfg, Log: resumeLogger(), DB: e.database, Tokens: e.tokens,
		Uploads: uploads, TUS: tus, Files: files, Objects: store, Finalizer: fin,
	})
	return &resumeProc{handler: h, store: store}
}

// ---- HTTP 小工具(TUS 数据面)----

// resumeTry 发一次请求,并把**连接层失败**也交回调用方 ——
// 断网/超时的表现正是 client.Do 返回错误,而不是某个 HTTP 状态码。
func resumeTry(client *http.Client, req *http.Request) (*http.Response, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		return resp, raw, rerr
	}
	return resp, raw, nil
}

// resumeMust 与 resumeTry 相同,但把连接层错误直接判为失败(正常路径不允许断网)。
func resumeMust(t *testing.T, client *http.Client, req *http.Request, what string) (*http.Response, []byte) {
	t.Helper()
	resp, raw, err := resumeTry(client, req)
	if err != nil {
		t.Fatalf("%s 连接层失败: %v", what, err)
	}
	return resp, raw
}

// resumeHash 是源内容的 SHA-256(最终对象的哈希必须与它逐字节相等)。
func resumeHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// resumePayload 造一段每个字节都与位置相关的内容,便于"前缀/偏移"断言定位错位。
func resumePayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('A' + (i % 26))
	}
	return b
}

// resumeAPICode 从结构化错误体里取业务码(失败信息里会带上原始响应体)。
func resumeAPICode(raw []byte) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	return b.Code
}

// tusCreate 走真实路由建任务(POST /tus,即 TUS `creation` 扩展)。
func (e *resumeEnv) tusCreate(t *testing.T, srv *httptest.Server, client *http.Client, name string, size int64) (string, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/tus", nil)
	if err != nil {
		t.Fatalf("构造建任务请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Upload-Length", strconv.FormatInt(size, 10))
	req.Header.Set("Upload-Metadata", "filename "+
		base64.StdEncoding.EncodeToString([]byte(name))+
		",space_id "+base64.StdEncoding.EncodeToString([]byte(e.spaceID))+
		",parent_id "+base64.StdEncoding.EncodeToString([]byte(e.rootID)))

	resp, raw := resumeMust(t, client, req, "POST /tus")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("建任务应 201,实际 %d body=%s", resp.StatusCode, raw)
	}
	id := strings.TrimPrefix(resp.Header.Get("Location"), "/tus/")
	ticket := resp.Header.Get("X-Upload-Token")
	if id == "" || ticket == "" {
		t.Fatalf("建任务响应缺少 Location/X-Upload-Token:Location=%q token=%q body=%s",
			resp.Header.Get("Location"), ticket, raw)
	}
	return id, ticket
}

// tusPatchReq 构造 TUS PATCH 请求。body 为 nil 时发空体。
func (e *resumeEnv) tusPatchReq(t *testing.T, srv *httptest.Server, id, ticket string, offset int64, body io.Reader, contentLength int64) *http.Request {
	t.Helper()
	if body == nil {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/tus/"+id, body)
	if err != nil {
		t.Fatalf("构造 PATCH 请求失败: %v", err)
	}
	// 显式声明体积:一是让请求头带 Content-Length(服务端据此做上界校验),
	// 二是让"断网"场景里服务端知道还差多少字节 —— 截断才表现为 unexpected EOF。
	req.ContentLength = contentLength
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	req.Header.Set("X-Upload-Token", ticket)
	return req
}

// patch 发一片数据并要求连接层成功返回(业务状态由调用方断言)。
func (e *resumeEnv) patch(t *testing.T, srv *httptest.Server, client *http.Client, id, ticket string, offset int64, data []byte) (*http.Response, []byte) {
	t.Helper()
	req := e.tusPatchReq(t, srv, id, ticket, offset, bytes.NewReader(data), int64(len(data)))
	return resumeMust(t, client, req, "PATCH(offset="+strconv.FormatInt(offset, 10)+", len="+strconv.Itoa(len(data))+")")
}

// head 走真实路由取偏移(TUS HEAD),返回 (偏移, 声明长度, 状态码, 响应体)。
func (e *resumeEnv) head(t *testing.T, srv *httptest.Server, client *http.Client, id, ticket string) (int64, int64, int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, srv.URL+"/tus/"+id, nil)
	if err != nil {
		t.Fatalf("构造 HEAD 请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("X-Upload-Token", ticket)

	resp, raw := resumeMust(t, client, req, "HEAD /tus/"+id)
	offset, _ := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	length, _ := strconv.ParseInt(resp.Header.Get("Upload-Length"), 10, 64)
	return offset, length, resp.StatusCode, raw
}

// headInProc 用**同一个路由**在进程内执行 HEAD,并返回 (状态码, 偏移, 响应体)。
//
// 为什么错误路径要它:HTTP 语义规定 HEAD 响应没有响应体 —— 真实客户端读不到服务端
// 写出的 JSON 错误体(实测:body 为空,业务码断言拿不到值)。要断言 HEAD 的业务码,
// 只能直接看 handler 写出的字节;路由与 handler 仍是同一套,只有传输层被绕过。
func (e *resumeEnv) headInProc(t *testing.T, h http.Handler, id, ticket string) (int, int64, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodHead, "/tus/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Tus-Resumable", "1.0.0")
	req.Header.Set("X-Upload-Token", ticket)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	offset, _ := strconv.ParseInt(rec.Header().Get("Upload-Offset"), 10, 64)
	return rec.Code, offset, rec.Body.Bytes()
}

func (e *resumeEnv) tusDelete(t *testing.T, srv *httptest.Server, id string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/tus/"+id, nil)
	if err != nil {
		t.Fatalf("构造 DELETE 请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	return resumeMust(t, srv.Client(), req, "DELETE /tus/"+id)
}

// download 走真实下载端点(GET /api/v1/files/{id}/content),支持 Range 头。
func (e *resumeEnv) download(t *testing.T, srv *httptest.Server, fileID string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/files/"+fileID+"/content", nil)
	if err != nil {
		t.Fatalf("构造下载请求失败: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return resumeMust(t, srv.Client(), req, "GET content")
}

// ---- 持久化状态读取(断言"偏移到底存在哪里")----

// uploadedBytes 读回 uploads.uploaded_bytes(直接查库,不信服务端内存态)。
func (e *resumeEnv) uploadedBytes(t *testing.T, id string) int64 {
	t.Helper()
	var n int64
	if err := e.database.Pool.QueryRow(context.Background(),
		`SELECT uploaded_bytes FROM uploads WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("读 uploads.uploaded_bytes 失败(id=%s): %v", id, err)
	}
	return n
}

// fileOf 读回定稿产物的 (内容哈希, 大小)。
func (e *resumeEnv) fileOf(t *testing.T, fileID string) (string, int64) {
	t.Helper()
	var hash string
	var size int64
	if err := e.database.Pool.QueryRow(context.Background(),
		`SELECT coalesce(hash_sha256,''), size FROM files WHERE id = $1`, fileID).Scan(&hash, &size); err != nil {
		t.Fatalf("读 files 行失败(id=%s): %v", fileID, err)
	}
	return hash, size
}

// readStage 读出暂存文件当前内容(断点续传的真相来源)。
func readStage(t *testing.T, store *storage.FS, uploadID string) []byte {
	t.Helper()
	fh, err := store.StageOpen(uploadID)
	if err != nil {
		t.Fatalf("打开暂存文件失败(upload=%s): %v", uploadID, err)
	}
	defer func() { _ = fh.Close() }()
	b, err := io.ReadAll(fh)
	if err != nil {
		t.Fatalf("读暂存文件失败(upload=%s): %v", uploadID, err)
	}
	return b
}

// waitStageSize 等到暂存文件长度恰好达到 want。
//
// 用途:把"服务端已经收了多少字节"变成**确定值** —— 断网/超时场景如果不先确认
// 服务端已落盘,断言就会依赖 TCP 缓冲与 goroutine 调度,变成随机通过/失败。
func waitStageSize(t *testing.T, store *storage.FS, uploadID string, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got int64
	for time.Now().Before(deadline) {
		n, err := store.StageSize(uploadID)
		if err != nil {
			t.Fatalf("读暂存长度失败(upload=%s): %v", uploadID, err)
		}
		got = n
		if n == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待暂存文件达到 %d 字节超时(upload=%s),实际 %d", want, uploadID, got)
}

// ---- 故障模拟用的请求体 ----

// errConnAbandoned 表示"客户端进程消失/网线被拔":连接层直接断掉,不再发剩余字节。
var errConnAbandoned = errors.New("模拟:连接被放弃")

// abandonAfter 先产出 prefix,然后阻塞到 release 被关闭,再返回错误。
//
// 为什么必须"阻塞到测试观测到字节已落盘":这样"服务端收了多少"是确定值。
// 不阻塞的话,服务端读到多少取决于 TCP 缓冲与调度,断点断言会变成碰运气。
type abandonAfter struct {
	prefix  []byte
	release <-chan struct{}
	err     error
	done    bool
}

func (b *abandonAfter) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	if b.done {
		return 0, b.err
	}
	<-b.release
	b.done = true
	return 0, b.err
}

// errProxyTimeout 表示"这一片传得太慢,代理/网关到点后掐断了连接"。
var errProxyTimeout = errors.New("模拟:代理超时")

// timeoutBody 模拟"慢分片被掐断":先产出 prefix,然后一直挂到 deadline,
// 最后返回超时错误(剩余字节永远不会送达)。
//
// 为什么不用 http.Client.Timeout 来模拟代理超时:**那条路会挂死测试**。
// transport 在取消请求后要等写循环结束才让 Do 返回(`mapRoundTripError` 里的
// `<-pc.writeLoopDone`),而写循环正卡在这个 reader 上 —— 于是 Do 永远不返回。
// 让 reader 自己到点报错,既等价于"连接被掐断",又是确定可返回的。
type timeoutBody struct {
	prefix   []byte
	deadline time.Time
	err      error
	done     bool
}

func (b *timeoutBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	if b.done {
		return 0, b.err
	}
	if wait := time.Until(b.deadline); wait > 0 {
		time.Sleep(wait)
	}
	b.done = true
	return 0, b.err
}

// ---- 验收用例 ----

// TestResumeWeakNetwork 覆盖 TS-05 的四条验收子句。每个子测试名就是验收子句本身。
func TestResumeWeakNetwork(t *testing.T) {
	t.Run("中途断网_放弃连接后HEAD报真实偏移并可续完", func(t *testing.T) {
		e := setupResume(t)
		p := e.boot(t)
		srv := httptest.NewServer(p.handler)
		client := srv.Client()

		whole := resumePayload(200_000)
		const cutAt = 60_000
		id, ticket := e.tusCreate(t, srv, client, "断网续传.bin", int64(len(whole)))

		// ① 发一半就断网:先确认服务端已落盘 cutAt 字节,再让请求体报错。
		release := make(chan struct{})
		var relOnce sync.Once
		releaseReader := func() { relOnce.Do(func() { close(release) }) }
		// 先放行被卡住的 reader,再关服务器 —— 否则 srv.Close 会等一个永远不结束的请求。
		t.Cleanup(func() { releaseReader(); srv.Close() })

		body := &abandonAfter{prefix: whole[:cutAt], release: release, err: errConnAbandoned}
		req := e.tusPatchReq(t, srv, id, ticket, 0, body, int64(len(whole)))
		done := make(chan error, 1)
		go func() {
			_, _, err := resumeTry(client, req)
			done <- err
		}()
		waitStageSize(t, p.store, id, cutAt)
		releaseReader()
		if err := <-done; err == nil {
			t.Fatal("连接被放弃后 PATCH 不应成功返回(期望连接层错误)")
		}

		// ② HEAD 必须报出**真实**偏移(= 已落盘字节),而不是 0 或声明长度
		offset, length, status, raw := e.head(t, srv, client, id, ticket)
		if status != http.StatusOK {
			t.Fatalf("断网后 HEAD 应 200,实际 %d body=%s", status, raw)
		}
		if offset != cutAt {
			t.Fatalf("断网后 HEAD 偏移应为已落盘的 %d 字节,实际 %d(body=%s)", cutAt, offset, raw)
		}
		if length != int64(len(whole)) {
			t.Fatalf("HEAD 声明长度应为 %d,实际 %d", len(whole), length)
		}
		// ③ 已接收的字节必须是源内容的**前缀**(错位就意味着续传必然写坏内容)
		if staged := readStage(t, p.store, id); !bytes.Equal(staged, whole[:cutAt]) {
			t.Fatalf("暂存内容与源前缀不一致:暂存 %d 字节,期望前缀 %d 字节", len(staged), cutAt)
		}

		// ④ 从断点续完 → 写完即定稿
		resp, raw := e.patch(t, srv, client, id, ticket, cutAt, whole[cutAt:])
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("续传最后一片应 200(写完即定稿),实际 %d body=%s", resp.StatusCode, raw)
		}
		fileID := resp.Header.Get("X-File-Id")
		if fileID == "" {
			t.Fatalf("定稿响应必须带 X-File-Id,实际响应头=%v body=%s", resp.Header, raw)
		}
		// ⑤ 最终对象哈希必须等于源哈希:一个字节都没丢、也没重复
		hash, size := e.fileOf(t, fileID)
		if want := resumeHash(whole); hash != want {
			t.Fatalf("续传后哈希应等于源哈希 %s,实际 %s(字节丢失或重复)", want, hash)
		}
		if size != int64(len(whole)) {
			t.Fatalf("续传后文件大小应为 %d,实际 %d", len(whole), size)
		}
		if n, err := p.store.Stat(context.Background(), hash); err != nil || n != int64(len(whole)) {
			t.Fatalf("对象应真实落在存储里且大小正确(期望 %d):size=%d err=%v", len(whole), n, err)
		}

		// ⑥ 下载侧的"中途断网"就是 HTTP Range:客户端拿着半截文件从偏移继续。
		//    两段拼接必须与源逐字节相等 —— 否则"能续"只是假象。
		first, firstBody := e.download(t, srv, fileID, map[string]string{"Range": "bytes=0-79999"})
		if first.StatusCode != http.StatusPartialContent {
			t.Fatalf("分段下载第一段应 206,实际 %d body=%s", first.StatusCode, firstBody)
		}
		second, secondBody := e.download(t, srv, fileID, map[string]string{"Range": "bytes=80000-"})
		if second.StatusCode != http.StatusPartialContent {
			t.Fatalf("分段下载第二段应 206,实际 %d body=%s", second.StatusCode, secondBody)
		}
		joined := append(append([]byte{}, firstBody...), secondBody...)
		if !bytes.Equal(joined, whole) {
			t.Fatalf("分段下载拼接后与源不一致(第一段 %d 字节 + 第二段 %d 字节,源 %d 字节)",
				len(firstBody), len(secondBody), len(whole))
		}
	})

	t.Run("进程崩溃_重启后从持久化偏移续传", func(t *testing.T) {
		e := setupResume(t)
		const part1 = 50_000
		whole := resumePayload(150_000)

		// ---- 进程 1 ----
		p1 := e.boot(t)
		srv1 := httptest.NewServer(p1.handler)
		id, ticket := e.tusCreate(t, srv1, srv1.Client(), "崩溃续传.bin", int64(len(whole)))

		resp, raw := e.patch(t, srv1, srv1.Client(), id, ticket, 0, whole[:part1])
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("第一片应 204(未传完),实际 %d body=%s", resp.StatusCode, raw)
		}
		waitStageSize(t, p1.store, id, part1)
		if got := e.uploadedBytes(t, id); got != part1 {
			t.Fatalf("第一片后 uploads.uploaded_bytes 应为 %d,实际 %d", part1, got)
		}
		// 模拟"数据已落盘、计数还没推进就被 kill -9"的崩溃窗口 ——
		// tus.go 刻意先写文件再更新计数,正是为了让这个窗口可恢复。
		// 把计数改回 0 不改变任何真实字节,只是把那个窗口**放大到可观测**。
		if _, err := e.database.Pool.Exec(context.Background(),
			`UPDATE uploads SET uploaded_bytes = 0 WHERE id = $1`, id); err != nil {
			t.Fatalf("重置计数失败: %v", err)
		}
		// 进程崩溃:不调用任何收尾逻辑,直接抛弃进程 1 的全部内存态。
		srv1.Close()

		// ---- 进程 2(重启):全新服务实例 + 全新存储实例 + 全新路由 ----
		p2 := e.boot(t)
		srv2 := httptest.NewServer(p2.handler)
		defer srv2.Close()
		client2 := srv2.Client()

		offset, _, status, raw2 := e.head(t, srv2, client2, id, ticket)
		if status != http.StatusOK {
			t.Fatalf("重启后 HEAD 应 200(同一 ticket 仍有效),实际 %d body=%s", status, raw2)
		}
		if offset != part1 {
			t.Fatalf("重启后偏移必须来自持久化状态:期望暂存文件长度 %d,实际 %d"+
				"(计数已被重置为 0;若这里回 0,说明偏移读的是内存或计数而不是落盘真相)", part1, offset)
		}
		// HEAD 以文件为准并把计数修正回来(tus.go 的文档行为)
		if got := e.uploadedBytes(t, id); got != part1 {
			t.Fatalf("HEAD 应把计数修正回暂存文件长度 %d,实际 %d", part1, got)
		}

		resp2, raw3 := e.patch(t, srv2, client2, id, ticket, part1, whole[part1:])
		if resp2.StatusCode != http.StatusOK {
			t.Fatalf("重启后续传最后一片应 200(定稿),实际 %d body=%s", resp2.StatusCode, raw3)
		}
		fileID := resp2.Header.Get("X-File-Id")
		if fileID == "" {
			t.Fatalf("定稿响应必须带 X-File-Id,实际响应头=%v body=%s", resp2.Header, raw3)
		}
		hash, size := e.fileOf(t, fileID)
		if want := resumeHash(whole); hash != want {
			t.Fatalf("重启后续传的哈希应等于源哈希 %s,实际 %s", want, hash)
		}
		if size != int64(len(whole)) {
			t.Fatalf("重启续传后文件大小应为 %d,实际 %d", len(whole), size)
		}
		// 新进程的存储实例必须能读到同一个对象(证明"沿用同一个存储根目录")
		if n, err := p2.store.Stat(context.Background(), hash); err != nil || n != int64(len(whole)) {
			t.Fatalf("重启后对象应仍可读:size=%d err=%v", n, err)
		}
	})

	t.Run("代理超时_同一偏移重试被接受且不重复落字节", func(t *testing.T) {
		e := setupResume(t)
		p := e.boot(t)
		srv := httptest.NewServer(p.handler)
		defer srv.Close()
		client := srv.Client()

		whole := resumePayload(120_000)

		// ① 形态一:分片"传得太慢",到点被掐断,**一个字节都没送达**。
		//    这时进度必须还是 0,客户端从**同一偏移**重试必须被接受(幂等)。
		id, ticket := e.tusCreate(t, srv, client, "代理超时未送达.bin", int64(len(whole)))
		tb := &timeoutBody{deadline: time.Now().Add(300 * time.Millisecond), err: errProxyTimeout}
		req := e.tusPatchReq(t, srv, id, ticket, 0, tb, int64(len(whole)))
		if _, _, err := resumeTry(client, req); err == nil {
			t.Fatal("模拟代理超时:慢分片应在请求体超时后以连接层错误结束")
		}
		offset, _, status, raw := e.head(t, srv, client, id, ticket)
		if status != http.StatusOK {
			t.Fatalf("超时后 HEAD 应 200,实际 %d body=%s", status, raw)
		}
		if offset != 0 {
			t.Fatalf("超时请求一个字节都没送达,偏移应为 0,实际 %d", offset)
		}
		if got := e.uploadedBytes(t, id); got != 0 {
			t.Fatalf("超时请求不得推进 uploads.uploaded_bytes,实际 %d", got)
		}
		if n, _ := p.store.StageSize(id); n != 0 {
			t.Fatalf("超时请求不得留下暂存数据,实际 %d 字节", n)
		}
		resp, raw := e.patch(t, srv, client, id, ticket, 0, whole)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("超时后从同一偏移重试应被接受并定稿(200),实际 %d body=%s", resp.StatusCode, raw)
		}
		fileID := resp.Header.Get("X-File-Id")
		hash, size := e.fileOf(t, fileID)
		if want := resumeHash(whole); hash != want {
			t.Fatalf("超时重试后哈希应等于源哈希 %s,实际 %s(字节被重复或损坏)", want, hash)
		}
		if size != int64(len(whole)) {
			t.Fatalf("超时重试后文件大小应为 %d,实际 %d", len(whole), size)
		}

		// ② 形态二:分片**切在半路**(已送出 40000 字节后连接被掐断)。
		//    重试"同一偏移"会被 409 拒(文档行为:回真实偏移),客户端据此纠正;
		//    按真实偏移续完后,内容仍必须与源逐字节相等(不重复、不损坏)。
		const cut = 40_000
		id2, ticket2 := e.tusCreate(t, srv, client, "代理超时切半.bin", int64(len(whole)))
		tb2 := &timeoutBody{prefix: whole[:cut], deadline: time.Now().Add(300 * time.Millisecond), err: errProxyTimeout}
		req2 := e.tusPatchReq(t, srv, id2, ticket2, 0, tb2, int64(len(whole)))
		if _, _, err := resumeTry(client, req2); err == nil {
			t.Fatal("模拟代理超时:切在半路的分片应以连接层错误结束")
		}
		// 等服务端确实落盘了它收到的那一段,断言才不依赖调度时序
		waitStageSize(t, p.store, id2, cut)
		offset2, _, status2, raw2 := e.head(t, srv, client, id2, ticket2)
		if status2 != http.StatusOK {
			t.Fatalf("切半后 HEAD 应 200,实际 %d body=%s", status2, raw2)
		}
		if offset2 != cut {
			t.Fatalf("切半后 HEAD 偏移应为已落盘的 %d 字节,实际 %d", cut, offset2)
		}
		// 用**老偏移**重放 → 409 + 真实偏移(而不是再写一遍造成重复)
		replay := e.tusPatchReq(t, srv, id2, ticket2, 0, bytes.NewReader(whole[:cut]), cut)
		resp3, raw3 := resumeMust(t, client, replay, "PATCH(同偏移重放)")
		if resp3.StatusCode != http.StatusConflict {
			t.Fatalf("同偏移重放应 409(文档行为:回真实偏移让客户端自我纠正),实际 %d body=%s",
				resp3.StatusCode, raw3)
		}
		if code := resumeAPICode(raw3); code != "version_conflict" {
			t.Fatalf("偏移不符的业务码应为 version_conflict(TUS 偏移语义),实际 %q body=%s", code, raw3)
		}
		if got := resp3.Header.Get("Upload-Offset"); got != "40000" {
			t.Fatalf("409 必须回真实偏移 40000(客户端据此继续),实际 Upload-Offset=%q", got)
		}
		if n, _ := p.store.StageSize(id2); n != cut {
			t.Fatalf("被拒的重放不得改变暂存长度(期望 %d),实际 %d", cut, n)
		}
		// 按真实偏移续传 → 定稿且内容不重复不损坏
		resp4, raw4 := e.patch(t, srv, client, id2, ticket2, cut, whole[cut:])
		if resp4.StatusCode != http.StatusOK {
			t.Fatalf("按真实偏移续传应 200(定稿),实际 %d body=%s", resp4.StatusCode, raw4)
		}
		hash2, size2 := e.fileOf(t, resp4.Header.Get("X-File-Id"))
		if want := resumeHash(whole); hash2 != want {
			t.Fatalf("重放场景最终哈希应等于源哈希 %s,实际 %s", want, hash2)
		}
		if size2 != int64(len(whole)) {
			t.Fatalf("重放场景最终大小应为 %d,实际 %d", len(whole), size2)
		}
	})

	t.Run("Ticket全程有效_每片可用_错票拒绝_结束后失效_过期按文档处理", func(t *testing.T) {
		e := setupResume(t)
		p := e.boot(t)
		srv := httptest.NewServer(p.handler)
		defer srv.Close()
		client := srv.Client()

		whole := resumePayload(90_000)
		const chunk = 30_000
		id, ticket := e.tusCreate(t, srv, client, "ticket全程.bin", int64(len(whole)))
		if len(ticket) < 32 {
			t.Fatalf("ticket 太短,可能可预测: %q", ticket)
		}

		// ① 错误 ticket → 401 unauthorized(HEAD 与 PATCH 都必须拒)
		if status, _, raw := e.headInProc(t, p.handler, id, ticket+"-tampered"); status != http.StatusUnauthorized {
			t.Fatalf("错误 ticket 的 HEAD 应 401,实际 %d body=%s", status, raw)
		} else if code := resumeAPICode(raw); code != "unauthorized" {
			t.Fatalf("错误 ticket 的业务码应为 unauthorized,实际 %q body=%s", code, raw)
		}
		wrongReq := e.tusPatchReq(t, srv, id, ticket+"-tampered", 0, bytes.NewReader(whole[:1000]), 1000)
		wresp, wraw := resumeMust(t, client, wrongReq, "PATCH(错误 ticket)")
		if wresp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("错误 ticket 的 PATCH 应 401,实际 %d body=%s", wresp.StatusCode, wraw)
		}

		// ② 每一片都用**同一个** ticket:HEAD 与 PATCH 全程都必须被接受
		for i := 0; i < 2; i++ {
			start := i * chunk
			resp, raw := e.patch(t, srv, client, id, ticket, int64(start), whole[start:start+chunk])
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("第 %d 片用同一 ticket 应 204,实际 %d body=%s", i+1, resp.StatusCode, raw)
			}
			off, _, hstatus, hraw := e.head(t, srv, client, id, ticket)
			if hstatus != http.StatusOK {
				t.Fatalf("第 %d 片后 HEAD 应 200(ticket 全程有效),实际 %d body=%s", i+1, hstatus, hraw)
			}
			if want := int64(start + chunk); off != want {
				t.Fatalf("第 %d 片后偏移应为 %d,实际 %d", i+1, want, off)
			}
		}
		// 最后一片 → 定稿
		const last = 2 * chunk
		resp, raw := e.patch(t, srv, client, id, ticket, last, whole[last:])
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("最后一片应 200(定稿),实际 %d body=%s", resp.StatusCode, raw)
		}
		fileID := resp.Header.Get("X-File-Id")
		if hash, _ := e.fileOf(t, fileID); hash != resumeHash(whole) {
			t.Fatalf("三片上传后哈希应为 %s,实际 %s", resumeHash(whole), hash)
		}

		// ③ 定稿之后 ticket 失效:任务已结束 → 409 upload_gone
		if status, _, fraw := e.headInProc(t, p.handler, id, ticket); status != http.StatusConflict {
			t.Fatalf("定稿后 HEAD 应 409(upload_gone),实际 %d body=%s", status, fraw)
		} else if code := resumeAPICode(fraw); code != "upload_gone" {
			t.Fatalf("定稿后 HEAD 的业务码应为 upload_gone,实际 %q body=%s", code, fraw)
		}
		postReq := e.tusPatchReq(t, srv, id, ticket, int64(len(whole)), nil, 0)
		presp, praw := resumeMust(t, client, postReq, "PATCH(定稿后)")
		if presp.StatusCode != http.StatusConflict {
			t.Fatalf("定稿后 PATCH 应 409,实际 %d body=%s", presp.StatusCode, praw)
		} else if code := resumeAPICode(praw); code != "upload_gone" {
			t.Fatalf("定稿后 PATCH 的业务码应为 upload_gone,实际 %q body=%s", code, praw)
		}

		// ④ 取消之后同样失效(且额度/名字被立刻释放,同名可重建)
		id2, ticket2 := e.tusCreate(t, srv, client, "ticket取消.bin", 4096)
		dresp, draw := e.tusDelete(t, srv, id2)
		if dresp.StatusCode != http.StatusNoContent {
			t.Fatalf("取消应 204,实际 %d body=%s", dresp.StatusCode, draw)
		}
		if status, _, craw := e.headInProc(t, p.handler, id2, ticket2); status != http.StatusConflict {
			t.Fatalf("取消后 HEAD 应 409(upload_gone),实际 %d body=%s", status, craw)
		} else if code := resumeAPICode(craw); code != "upload_gone" {
			t.Fatalf("取消后 HEAD 的业务码应为 upload_gone,实际 %q body=%s", code, craw)
		}
		if _, ticket3 := e.tusCreate(t, srv, client, "ticket取消.bin", 4096); ticket3 == "" {
			t.Fatal("取消后同名上传应可重建(否则用户会被自己刚取消的任务卡住名字)")
		}

		// ⑤ TTL:代码里的过期判定是 `now > expires_at` → **401 + upload_gone**
		//    (见 Service.loadAuthorized;与"任务已结束"的 409 刻意区分:
		//     前者要重新建任务,后者只是这个任务被用完了)。
		id4, ticket4 := e.tusCreate(t, srv, client, "ticket过期.bin", 4096)
		if _, err := e.database.Pool.Exec(context.Background(),
			`UPDATE uploads SET expires_at = now() - interval '1 minute' WHERE id = $1`, id4); err != nil {
			t.Fatalf("把任务改成已过期失败: %v", err)
		}
		off, _, status, eraw := e.head(t, srv, client, id4, ticket4)
		if status != http.StatusUnauthorized {
			t.Fatalf("过期 ticket 的 HEAD 应 401(文档状态),实际 %d body=%s", status, eraw)
		}
		if off != 0 {
			t.Fatalf("过期任务的 HEAD 不应回偏移,实际 Upload-Offset=%d", off)
		}
		// HEAD 没有响应体,业务码只能从同一路由的处理器输出里看(见 headInProc)
		if hstatus, _, hraw := e.headInProc(t, p.handler, id4, ticket4); hstatus != http.StatusUnauthorized {
			t.Fatalf("过期 ticket 的 HEAD 应 401,实际 %d body=%s", hstatus, hraw)
		} else if code := resumeAPICode(hraw); code != "upload_gone" {
			t.Fatalf("过期 ticket 的业务码应为 upload_gone,实际 %q body=%s", code, hraw)
		}
		expReq := e.tusPatchReq(t, srv, id4, ticket4, 0, bytes.NewReader(whole[:16]), 16)
		eresp, eraw2 := resumeMust(t, client, expReq, "PATCH(过期 ticket)")
		if eresp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("过期 ticket 的 PATCH 应 401,实际 %d body=%s", eresp.StatusCode, eraw2)
		} else if code := resumeAPICode(eraw2); code != "upload_gone" {
			t.Fatalf("过期 ticket 的 PATCH 业务码应为 upload_gone,实际 %q body=%s", code, eraw2)
		}
	})
}
