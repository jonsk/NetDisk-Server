package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
	"github.com/netdisk/netdisk/internal/webdavauth"
)

// WebDAV 方法集的真实 HTTP 测试(BE-S8-01~06 / BE-S5-06)。
//
// 为什么必须用真实 server:WebDAV 的行为大半在**上游 handler**里
// (它按 RFC 4918 决定状态码、Depth、207 Multi-Status),用 Recorder 只能测我们
// 自己那几行;而"我们的 FileSystem 与上游契约对不对得上"恰恰是最容易出问题的地方
// (比如 FileInfo 漏实现 ETager 时**不会报错**,只会静默用 ModTime+Size 现算)。

type davEnv struct {
	handler http.Handler
	url     string
	token   string
	userID  string
	spaceID string
	rootID  string
	dbase   *db.DB
	store   *storage.FS
	// basic 是 WebDAV 的账密(真实口令)
	login    string
	password string
	authHdr  string
}

func setupDAVEnv(t *testing.T) *davEnv {
	t.Helper()
	dsn := os.Getenv("NETDISK_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 NETDISK_TEST_DSN,跳过需要数据库的测试")
	}
	ctx := context.Background()
	database, err := db.OpenWithDSN(ctx, dsn, 8)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(database.Close)
	if err := migrate.Up(ctx, database.Pool); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	suffix := time.Now().Format("150405.000000")
	login := "dav_" + suffix
	password := "Dav-P@ssw0rd" + time.Now().Format("05.000")
	// bcrypt cost=4:测试加速(生产用默认 cost)
	pol := credentials.DefaultPolicy()
	pol.Cost = 4
	users := repo.UserRepo{}
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := users.Create(ctx, tx, repo.CreateInput{
			Username: login, Email: login + "@example.com",
			DisplayName: "WebDAV 方法集测试用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		hash, herr := pol.Hash(password)
		if herr != nil {
			return herr
		}
		if serr := users.SetPassword(ctx, tx, created.ID, hash); serr != nil {
			return serr
		}
		u = created
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	spaces := repo.SpaceRepo{}
	sp, err := spaces.PersonalOf(ctx, database.Pool, u.ID)
	if err != nil {
		t.Fatalf("取个人空间失败: %v", err)
	}
	files := repo.FileRepo{}
	root, err := files.GetRoot(ctx, database.Pool, sp.ID)
	if err != nil {
		t.Fatalf("取根目录失败: %v", err)
	}
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = database.Pool.Exec(bg, `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, u.ID)
		_, _ = database.Pool.Exec(bg,
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM uploads WHERE user_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	authSvc := &authsvc.Service{
		Users: users, Spaces: spaces, Tokens: tokens, Policy: pol,
		DB: db.AsQuerier(database),
	}
	// 账密校验复用 authsvc.VerifyCredentials(防枚举/锁定只有一份实现)
	verify := func(ctx context.Context, username, pass string) (string, int64, error) {
		v, verr := authSvc.VerifyCredentials(ctx, username, pass)
		if verr != nil || v == nil {
			return "", 0, webdavauth.ErrUnauthorized
		}
		return v.ID, v.TokenVersion, nil
	}

	namePol := namepolicy.Default()
	fileSvc := &filesvc.Service{
		Spaces: spaces, Files: files, DB: db.AsQuerier(database), Name: namePol,
	}
	finSvc := &finalize.Service{
		Pool: database.Pool, Storage: store, Spaces: spaces, Files: files, Name: namePol,
	}
	uploadSvc := &uploadsvc.Service{
		Spaces: spaces, Files: files, Uploads: repo.UploadRepo{},
		DB: db.AsQuerier(database), Name: namePol,
	}
	handler := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens, Auth: authSvc,
		Files: fileSvc, Uploads: uploadSvc,
		WebDAV: &webdavauth.Authenticator{
			Store:  &webdavauth.RedisStore{Cache: newCache(t, cfg.Redis.KeyPrefix)},
			Verify: verify,
		},
		TUS: &uploadsvc.TUSService{
			Service: uploadSvc, Stager: store, Finalizer: finSvc, DB: db.AsQuerier(database),
		},
		Objects:   store,
		Finalizer: finSvc,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &davEnv{
		handler: handler, url: srv.URL,
		userID: u.ID, spaceID: sp.ID, rootID: root.ID, dbase: database, store: store,
		login: login, password: password,
		authHdr: webdavauth.EncodeBasic(login, password),
	}
}

// davReq 发一次 WebDAV 请求(带 Basic 凭据)。
func (e *davEnv) davReq(t *testing.T, method, path string, body io.Reader, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.url+path, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Authorization", e.authHdr)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	return resp
}

func drain(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// **验收(BE-S8-01)** 五方法映射:MKCOL 建目录 → PROPFIND 看得到 → MOVE 改名 → DELETE 删除
func TestWebDAVMethodMapping(t *testing.T) {
	e := setupDAVEnv(t)
	base := "/webdav/" + e.spaceID

	// MKCOL
	resp := e.davReq(t, "MKCOL", base+"/davdir", nil, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL 应 201,实际 %d headers=%v %s", resp.StatusCode, resp.Header, drain(t, resp))
	}
	_ = drain(t, resp)

	// MKCOL 重复 → 405(Method Not Allowed,上游语义)
	resp = e.davReq(t, "MKCOL", base+"/davdir", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("重复 MKCOL 应 405,实际 %d", resp.StatusCode)
	}
	_ = drain(t, resp)

	// PUT 一个文件
	content := []byte("webdav-put-content")
	resp = e.davReq(t, http.MethodPut, base+"/davdir/f.txt", bytes.NewReader(content), nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT 新文件应 201,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	etag := resp.Header.Get("ETag")
	_ = drain(t, resp)
	if etag == "" || !strings.HasPrefix(etag, `"`) {
		t.Fatalf("PUT 必须回带引号的强 ETag,实际 %q", etag)
	}
	// 库里真的有内容且哈希正确
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	var n int
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE space_id=$1 AND name='f.txt' AND hash_sha256=$2`,
		e.spaceID, hash).Scan(&n); err != nil {
		t.Fatalf("查文件失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("PUT 后应有一行且内容哈希正确,实际 %d", n)
	}

	// PROPFIND Depth:1 → 207,且能找到刚才的文件
	resp = e.davReq(t, "PROPFIND", base+"/davdir", nil, map[string]string{"Depth": "1"})
	body := drain(t, resp)
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND 应 207,实际 %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "f.txt") {
		t.Fatalf("PROPFIND 结果应包含 f.txt: %s", body)
	}
	// **验收(BE-S8-02)**:getetag 必须是 files.etag(经 QuoteETag),而不是 ModTime+Size
	if !strings.Contains(body, etag) {
		t.Fatalf("PROPFIND 的 getetag 必须与 PUT 返回的 ETag 一致(%s),实际: %s", etag, body)
	}

	// GET 内容
	resp = e.davReq(t, http.MethodGet, base+"/davdir/f.txt", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("GET 应 200 且内容一致,实际 %d len=%d", resp.StatusCode, len(got))
	}

	// MOVE 改名
	resp = e.davReq(t, "MOVE", base+"/davdir/f.txt", nil, map[string]string{
		"Destination": e.url + base + "/davdir/g.txt",
	})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE 应 201/204,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)

	// DELETE
	resp = e.davReq(t, http.MethodDelete, base+"/davdir/g.txt", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE 应 204,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)
}

// **验收(BE-S8-03)** 条件请求:If-Match 不匹配 → 412;命中 → 继续
func TestWebDAVConditionalRequests(t *testing.T) {
	e := setupDAVEnv(t)
	base := "/webdav/" + e.spaceID

	resp := e.davReq(t, http.MethodPut, base+"/cond.txt", strings.NewReader("v1"), nil)
	etag := resp.Header.Get("ETag")
	_ = drain(t, resp)
	if etag == "" {
		t.Fatal("需要 ETag 才能测条件请求")
	}

	// If-Match 不匹配 → 412(上游不处理这个头,靠我们的中间件)
	resp = e.davReq(t, http.MethodPut, base+"/cond.txt", strings.NewReader("v2"),
		map[string]string{"If-Match": `"00000099-deadbeef"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("If-Match 不匹配应 412,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)
	// 且**内容没有被改**:412 必须发生在读 body/写盘**之前**
	resp = e.davReq(t, http.MethodGet, base+"/cond.txt", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(got) != "v1" {
		t.Fatalf("412 之后内容不应变化,实际 %q", string(got))
	}

	// If-Match 命中 → 覆盖成功(204)
	resp = e.davReq(t, http.MethodPut, base+"/cond.txt", strings.NewReader("v2"),
		map[string]string{"If-Match": etag})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("If-Match 命中应 204(覆盖),实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)

	// If-None-Match: * + 已存在 → 412(不覆盖)
	resp = e.davReq(t, http.MethodPut, base+"/cond.txt", strings.NewReader("v3"),
		map[string]string{"If-None-Match": "*"})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("If-None-Match:* 且目标存在应 412,实际 %d", resp.StatusCode)
	}
	_ = drain(t, resp)

	// DELETE 也吃条件头
	resp = e.davReq(t, http.MethodDelete, base+"/cond.txt", nil,
		map[string]string{"If-Match": `"00000099-deadbeef"`})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("DELETE 的 If-Match 不匹配应 412,实际 %d", resp.StatusCode)
	}
	_ = drain(t, resp)
}

// **验收(BE-S8-05)** Overwrite: F + 目标占用 → 412;Destination 绝对 URL 归一
func TestWebDAVOverwriteAndDestination(t *testing.T) {
	e := setupDAVEnv(t)
	base := "/webdav/" + e.spaceID

	for _, n := range []string{"src.txt", "dst.txt"} {
		resp := e.davReq(t, http.MethodPut, base+"/"+n, strings.NewReader("x"), nil)
		_ = drain(t, resp)
	}
	// COPY with Overwrite: F → 目标存在 → 412
	resp := e.davReq(t, "COPY", base+"/src.txt", nil, map[string]string{
		"Destination": e.url + base + "/dst.txt",
		"Overwrite":   "F",
	})
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("Overwrite:F 且目标存在应 412,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)

	// COPY with Overwrite: T → 覆盖成功(204)且**没有多出条目**
	resp = e.davReq(t, "COPY", base+"/src.txt", nil, map[string]string{
		"Destination": e.url + base + "/dst.txt",
		"Overwrite":   "T",
	})
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusCreated {
		t.Fatalf("Overwrite:T 应成功,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)

	// Destination 用**相对路径**(部分客户端会这样发)—— 也必须能工作
	resp = e.davReq(t, "COPY", base+"/src.txt", nil, map[string]string{
		"Destination": base + "/rel.txt",
		"Overwrite":   "T",
	})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("相对 Destination 应成功,实际 %d %s", resp.StatusCode, drain(t, resp))
	}
	_ = drain(t, resp)
}

// **验收(BE-S8-06)** LOCK/UNLOCK 明确 501 且指出替代路径;不宣称 class 2
func TestWebDAVLockNotSupported(t *testing.T) {
	e := setupDAVEnv(t)
	base := "/webdav/" + e.spaceID
	resp := e.davReq(t, "LOCK", base+"/x.txt", strings.NewReader(`<?xml version="1.0"?><lockinfo/>`), nil)
	body := drain(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("LOCK 应 501,实际 %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "REST") {
		t.Errorf("501 必须指出替代路径(REST 锁),实际 %s", body)
	}
	// DAV 头只能宣称 class 1
	if dav := resp.Header.Get("DAV"); strings.Contains(dav, "2") {
		t.Fatalf("未实现 LOCK 就不能宣称 class 2,实际 DAV: %q", dav)
	}
	resp = e.davReq(t, "UNLOCK", base+"/x.txt", nil, nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("UNLOCK 应 501,实际 %d", resp.StatusCode)
	}
	_ = drain(t, resp)
}

// **验收(BE-S8-01)** 权限:跨空间访问必须 400/404(不能悄悄让人看到别人的空间)
func TestWebDAVAccessControl(t *testing.T) {
	e := setupDAVEnv(t)
	other := setupDAVEnv(t)
	// 用 e 的凭据访问 other 的空间
	resp := e.davReq(t, "PROPFIND", "/webdav/"+other.spaceID, nil, map[string]string{"Depth": "0"})
	body := drain(t, resp)
	if resp.StatusCode == http.StatusMultiStatus || resp.StatusCode == http.StatusOK {
		t.Fatalf("跨空间 PROPFIND 不该成功,实际 %d %s", resp.StatusCode, body)
	}
}

// 未认证 → 401 + 挑战头(客户端据此弹密码框)。
//
// 同名用例已在 handlers_webdav_test.go 覆盖"完全没有凭据"的分支;
// 这条补的是"WebDAV 方法集挂上之后依然如此"(方法分发不能绕过认证)。
func TestWebDAVMethodsRequireAuth(t *testing.T) {
	e := setupDAVEnv(t)
	req, _ := http.NewRequest("PROPFIND", e.url+"/webdav/"+e.spaceID, nil)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未认证应 401,实际 %d", resp.StatusCode)
	}
	if ch := resp.Header.Get("WWW-Authenticate"); ch == "" {
		t.Fatal("401 必须带 WWW-Authenticate 挑战头,否则客户端不会弹密码框")
	}
}

// PUT 超过上限 → 413 且提示走 TUS(BE-S5-06 验收)
func TestWebDAVPutTooLarge(t *testing.T) {
	e := setupDAVEnv(t)
	base := "/webdav/" + e.spaceID
	big := make([]byte, (100<<20)+1)
	resp := e.davReq(t, http.MethodPut, base+"/big.bin", bytes.NewReader(big), nil)
	body := drain(t, resp)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限 PUT 应 413,实际 %d %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "TUS") {
		t.Errorf("413 应提示改走 TUS,实际 %s", body)
	}
}
