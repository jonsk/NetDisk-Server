package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/sharesvc"
)

// 编辑锁(BE-S6-05)与分享链接(BE-S6-04)的 HTTP 契约测试。
//
// 两者都在 fixtures 之外单独装配:编辑锁要直接改 `file_locks` 行来模拟过期,
// 分享要能拿到 token 与密码校验的真实行为 —— 用桩替换任何一处都会让
// "过期/超次/并发递增"这些最容易出错的语义测不出来。

// lockShareEnv 在 fileEnv 的基础上补 Shares 服务。
type lockShareEnv struct {
	*fileEnv
}

func setupLockShareEnv(t *testing.T) *lockShareEnv {
	t.Helper()
	e := setupFileEnv(t)
	// 用与生产同构的装配:Querier 适配器 + 分享密码策略(bcrypt cost 降到 4 加速)
	e.deps.Shares = &sharesvc.Service{
		DB: db.AsQuerier(e.dbase), Policy: credPolicyForTest(),
	}
	e.handler = api.New(e.deps)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.dbase.Pool.Exec(bg, `DELETE FROM shares WHERE user_id = $1`, e.userID)
		_, _ = e.dbase.Pool.Exec(bg, `
DELETE FROM file_locks WHERE user_id = $1 OR file_id IN (
  SELECT id FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1))`, e.userID)
	})
	return &lockShareEnv{e}
}

// mkShareableFile 造一个**有真实物理对象**的文件。
//
// 为什么不能用 mkFile:它只插 files 行(hash 是凑出来的),而分享下载要
// `Objects.Open` 真的读到内容 —— 用假 hash 会得到 500「对象不存在」,
// 那测的是"假数据"而不是分享逻辑。
func (e *lockShareEnv) mkShareableFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if _, err := e.deps.Objects.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写物理对象失败: %v", err)
	}
	if _, err := e.dbase.Pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE SET ref_count = file_objects.ref_count + 1`,
		hash, len(data), e.deps.Objects.KeyFor(hash)); err != nil {
		t.Fatalf("写对象行失败: %v", err)
	}
	var id string
	if err := e.dbase.Pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, version, depth, hash_sha256, mime_type)
VALUES ($1, $2, $3, $4, false, $5, 1, 1, $6, 'text/plain') RETURNING id`,
		e.spaceID, e.rootID, e.userID, name, len(data), hash).Scan(&id); err != nil {
		t.Fatalf("写文件行失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.dbase.Pool.Exec(bg, `DELETE FROM file_objects WHERE hash_sha256 = $1`, hash)
	})
	return id
}

// ---- 编辑锁 ----

// **验收**:获取/续期/释放 + 只拦覆盖写(不拦移动/删除)
func TestEditLockLifecycle(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkFile(t, "doc.txt", 10, 1)

	// 获取
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/lock", e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("获取锁应 200,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	lk, _ := body["lock"].(map[string]any)
	if lk["mine"] != true {
		t.Errorf("自己获取的锁 mine 应为 true,实际 %v", lk)
	}
	if body["ttl_seconds"].(float64) != 1800 {
		t.Errorf("TTL 应为 30min=1800s,实际 %v", body["ttl_seconds"])
	}

	// 读锁
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files/"+id+"/lock", e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("读锁应 200,实际 %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["lock"] == nil {
		t.Error("读锁应返回锁对象")
	}

	// 续期(自己再获取一次)
	if rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/lock", e.token, nil); rec.Code != http.StatusOK {
		t.Fatalf("续期应 200,实际 %d", rec.Code)
	}

	// **锁不拦移动/删除**(纪律 1:锁保护的是"内容不被覆盖",不是"文件被独占")
	rec = doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/files/"+id, e.token,
		map[string]any{"name": "renamed.txt", "base_version": 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("锁不应拦住改名,实际 %d body=%s", rec.Code, rec.Body.String())
	}

	// 释放(幂等:再释放一次仍成功)
	if rec := doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/files/"+id+"/lock", e.token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("释放应 204,实际 %d", rec.Code)
	}
	if rec := doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/files/"+id+"/lock", e.token, nil); rec.Code != http.StatusNoContent {
		t.Fatalf("重复释放应 204(幂等),实际 %d", rec.Code)
	}
	// 释放后读锁 → null
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files/"+id+"/lock", e.token, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["lock"] != nil {
		t.Errorf("释放后应无锁,实际 %v", body["lock"])
	}
}

// **验收**:他人持锁且未过期 → 409 + 持有者信息;过期锁可被抢占
func TestEditLockConflictAndExpiry(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkFile(t, "shared-doc.txt", 10, 1)
	other := setupLockShareEnv(t)
	// 直接造一条"别人持锁"的行(避免依赖另一个用户的 token)
	if _, err := e.dbase.Pool.Exec(context.Background(), `
INSERT INTO file_locks (file_id, user_id, expires_at) VALUES ($1, $2, now() + interval '10 minutes')`,
		id, other.userID); err != nil {
		t.Fatalf("造锁失败: %v", err)
	}
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/lock", e.token, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("他人持锁应 409,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	details, _ := body["details"].(map[string]any)
	if details["holder_user_id"] != other.userID {
		t.Errorf("应告知持有者 id(客户端据此显示'XX 正在编辑'),实际 %v", details)
	}

	// 把锁改成已过期 → 可抢占(否则文件被永久锁死)
	if _, err := e.dbase.Pool.Exec(context.Background(),
		`UPDATE file_locks SET expires_at = now() - interval '1 minute' WHERE file_id = $1`, id); err != nil {
		t.Fatalf("改过期失败: %v", err)
	}
	if rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/lock", e.token, nil); rec.Code != http.StatusOK {
		t.Fatalf("过期锁应可抢占,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 释放**别人**的锁必须被拒(否则任何人都能解锁别人的编辑会话)
func TestEditLockCannotReleaseOthers(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkFile(t, "mine.txt", 10, 1)
	other := setupLockShareEnv(t)
	if _, err := e.dbase.Pool.Exec(context.Background(), `
INSERT INTO file_locks (file_id, user_id, expires_at) VALUES ($1, $2, now() + interval '10 minutes')`,
		id, other.userID); err != nil {
		t.Fatalf("造锁失败: %v", err)
	}
	rec := doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/files/"+id+"/lock", e.token, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("释放他人锁应 403,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// **验收**:锁**只**拦覆盖写 —— 拦住 PUT(覆盖),但不拦移动/删除
//
// 这条纪律的动机:锁的用途是"我正在编辑,请勿在他处写入";重命名/移动/删除是
// 所有者级别的整理操作,拦住它们会让"锁着的时候连改名都不行",
// 而这类操作本身不会损坏编辑中的内容。
func TestEditLockOnlyBlocksOverwrite(t *testing.T) {
	e := setupDAVEnv(t) // DAV fixture:有真实 Basic 认证的写路径
	// 造一个真实文件并锁上
	base := "/webdav/" + e.spaceID
	resp := e.davReq(t, http.MethodPut, base+"/locked.txt", strings.NewReader("v1"), nil)
	_ = drain(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("前置 PUT 失败: %d", resp.StatusCode)
	}
	var fileID string
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT id FROM files WHERE space_id=$1 AND name='locked.txt'`, e.spaceID).Scan(&fileID); err != nil {
		t.Fatalf("取文件 id 失败: %v", err)
	}
	// 由**另一个用户**持锁,当前用户来覆盖
	other := setupDAVEnv(t)
	if _, err := e.dbase.Pool.Exec(context.Background(), `
INSERT INTO file_locks (file_id, user_id, expires_at) VALUES ($1,$2, now() + interval '10 minutes')`,
		fileID, other.userID); err != nil {
		t.Fatalf("造锁失败: %v", err)
	}
	// 覆盖写 → 409(锁生效)
	resp = e.davReq(t, http.MethodPut, base+"/locked.txt", strings.NewReader("v2"), nil)
	body := drain(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("被锁文件的覆盖写应 409,实际 %d %s", resp.StatusCode, body)
	}
	// 移动 → 不受锁影响(锁只拦覆盖写)
	resp = e.davReq(t, "MOVE", base+"/locked.txt", nil, map[string]string{
		"Destination": e.url + base + "/moved.txt",
	})
	_ = drain(t, resp)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("锁不应拦住移动,实际 %d", resp.StatusCode)
	}
	// 删除 → 同样不受影响
	resp = e.davReq(t, http.MethodDelete, base+"/moved.txt", nil, nil)
	_ = drain(t, resp)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("锁不应拦住删除,实际 %d", resp.StatusCode)
	}
}

// ---- 分享链接 ----

// **验收①⑤**:创建返回高熵 token;meta 与下载**免 JWT**
func TestShareCreateMetaDownload(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkShareableFile(t, "报表.txt", []byte("report-body"))

	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", e.token,
		map[string]any{"file_id": id})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建分享应 201,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token, _ := created["token"].(string)
	if len(token) != 43 {
		t.Fatalf("token 应为 43 字符(32 字节 base64url),实际 %d: %q", len(token), token)
	}
	if created["need_password"] != false {
		t.Errorf("未设密码时 need_password 应为 false,实际 %v", created["need_password"])
	}

	// meta **不带令牌**(落地页访问者没有账号)
	metaRec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/meta", "", nil)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("meta 应免登录 200,实际 %d body=%s", metaRec.Code, metaRec.Body.String())
	}
	var meta map[string]any
	_ = json.Unmarshal(metaRec.Body.Bytes(), &meta)
	if meta["name"] != "报表.txt" || meta["need_password"] != false {
		t.Fatalf("meta 内容不符: %v", meta)
	}
	if meta["download_url"] != nil {
		t.Error("meta 不该回下载地址(否则它成了免登录的内容发现接口)")
	}

	// 下载免 JWT
	dl := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/download", "", nil)
	if dl.Code != http.StatusOK {
		t.Fatalf("免登录下载应 200,实际 %d body=%s", dl.Code, dl.Body.String())
	}
	if cd := dl.Header().Get("Content-Disposition"); !strings.Contains(cd, "filename*=UTF-8''") {
		t.Errorf("Content-Disposition 应带 RFC 5987 形式(中文名),实际 %q", cd)
	}
}

// **验收②**:需要密码时,无密码 401(带 need_password),错密码 401(bad_password),对密码放行
func TestSharePasswordFlow(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkShareableFile(t, "secret.txt", []byte("secret-body"))
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", e.token,
		map[string]any{"file_id": id, "password": "Share-Pass-2026!"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建带密码分享应 201,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token := created["token"].(string)
	if created["need_password"] != true {
		t.Errorf("应标记 need_password,实际 %v", created["need_password"])
	}

	// meta 仍然免登录,但要告知需要密码
	metaRec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/meta", "", nil)
	if metaRec.Code != http.StatusOK {
		t.Fatalf("meta 应 200,实际 %d", metaRec.Code)
	}
	var meta map[string]any
	_ = json.Unmarshal(metaRec.Body.Bytes(), &meta)
	if meta["need_password"] != true {
		t.Errorf("meta 应告知需要密码,实际 %v", meta)
	}

	// 不带密码 → 401 + need_password(前端据此弹框)
	noPw := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/download", "", nil)
	if noPw.Code != http.StatusUnauthorized {
		t.Fatalf("缺密码应 401,实际 %d", noPw.Code)
	}
	var noPwBody map[string]any
	_ = json.Unmarshal(noPw.Body.Bytes(), &noPwBody)
	if d, _ := noPwBody["details"].(map[string]any); d["need_password"] != true {
		t.Errorf("401 应带 need_password=true,实际 %v", noPwBody)
	}

	// 错密码 → 401 + bad_password
	bad := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/shares/"+token+"/download?password=wrong", "", nil)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("错密码应 401,实际 %d", bad.Code)
	}
	var badBody map[string]any
	_ = json.Unmarshal(bad.Body.Bytes(), &badBody)
	if d, _ := badBody["details"].(map[string]any); d["reason"] != "bad_password" {
		t.Errorf("错密码应带 reason=bad_password,实际 %v", badBody)
	}

	// 对密码 → 200
	// **注意**:一次错密码不该消耗下载次数(否则攻击者能用试错把次数耗光)
	ok := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/shares/"+token+"/download?password=Share-Pass-2026!", "", nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("密码正确应 200,实际 %d body=%s", ok.Code, ok.Body.String())
	}
	var cnt int
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT download_count FROM shares WHERE token = $1`, token).Scan(&cnt); err != nil {
		t.Fatalf("读计数失败: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("只有成功那次才该计数,实际 %d", cnt)
	}
}

// **验收③④**:download_count 原子递增且不超 max_downloads;超次 → 410 + reason
func TestShareDownloadLimit(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkShareableFile(t, "once.txt", []byte("once"))
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", e.token,
		map[string]any{"file_id": id, "max_downloads": 1})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建应 201,实际 %d", rec.Code)
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token := created["token"].(string)

	if got := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/download", "", nil); got.Code != http.StatusOK {
		t.Fatalf("第一次下载应 200,实际 %d", got.Code)
	}
	second := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/download", "", nil)
	if second.Code != http.StatusGone {
		t.Fatalf("超过次数应 410,实际 %d body=%s", second.Code, second.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &body)
	if body["code"] != "resource_gone" {
		t.Errorf("业务码应为 resource_gone,实际 %v", body["code"])
	}
	if d, _ := body["details"].(map[string]any); d["reason"] != "download_limit_reached" {
		t.Errorf("reason 应为 download_limit_reached,实际 %v", body)
	}
	// 计数不超上限
	var cnt int
	_ = e.dbase.Pool.QueryRow(context.Background(),
		`SELECT download_count FROM shares WHERE token=$1`, token).Scan(&cnt)
	if cnt != 1 {
		t.Fatalf("计数应停在 1(原子递增 + 上限同一条 WHERE),实际 %d", cnt)
	}
}

// 吊销后可诊断;吊销幂等;别人的分享不能吊销
func TestShareRevoke(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkFile(t, "revoke-me.txt", 3, 1)
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", e.token,
		map[string]any{"file_id": id})
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token, shareID := created["token"].(string), created["id"].(string)

	if got := doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/shares/"+shareID, e.token, nil); got.Code != http.StatusNoContent {
		t.Fatalf("吊销应 204,实际 %d", got.Code)
	}
	dl := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/download", "", nil)
	if dl.Code != http.StatusGone {
		t.Fatalf("吊销后应 410,实际 %d", dl.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(dl.Body.Bytes(), &body)
	if d, _ := body["details"].(map[string]any); d["reason"] != "revoked" {
		t.Errorf("reason 应为 revoked,实际 %v", body)
	}
	// 别人的分享:404(不泄露"这个 id 存在")
	other := setupLockShareEnv(t)
	if got := doJSONReq(t, other.handler, http.MethodDelete, "/api/v1/shares/"+shareID, other.token, nil); got.Code != http.StatusNotFound {
		t.Fatalf("吊销他人分享应 404,实际 %d body=%s", got.Code, got.Body.String())
	}
}

// 过期链接 → 410 + reason=expired(不是 404:"曾经存在、现在不可用"是终态)
func TestShareExpired(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkFile(t, "expired.txt", 3, 1)
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", e.token,
		map[string]any{"file_id": id})
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	token := created["token"].(string)
	if _, err := e.dbase.Pool.Exec(context.Background(),
		`UPDATE shares SET expires_at = now() - interval '1 hour' WHERE token = $1`, token); err != nil {
		t.Fatalf("改过期失败: %v", err)
	}
	dl := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/download", "", nil)
	if dl.Code != http.StatusGone {
		t.Fatalf("过期应 410,实际 %d", dl.Code)
	}
	metaRec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+token+"/meta", "", nil)
	if metaRec.Code != http.StatusGone {
		t.Fatalf("过期的 meta 也应 410(而不是显示成 文件不存在),实际 %d", metaRec.Code)
	}
}

// 未知 token → 404(与"曾经存在"区分开)
func TestShareUnknownToken(t *testing.T) {
	e := setupLockShareEnv(t)
	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/does-not-exist/meta", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 token 应 404,实际 %d", rec.Code)
	}
}

// 创建分享需要登录;必须有读权限(分享不是提权手段)
func TestShareCreateRequiresAuthAndRead(t *testing.T) {
	e := setupLockShareEnv(t)
	id := e.mkFile(t, "a.txt", 3, 1)
	if rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", "", map[string]any{"file_id": id}); rec.Code != http.StatusUnauthorized {
		t.Errorf("无令牌创建分享应 401,实际 %d", rec.Code)
	}
	other := setupLockShareEnv(t)
	if rec := doJSONReq(t, other.handler, http.MethodPost, "/api/v1/shares", other.token,
		map[string]any{"file_id": id}); rec.Code == http.StatusCreated {
		t.Error("看不到的文件不该能分享(分享不是提权手段)")
	}
}

// 目录分享 → 501(明确告知而不是回空文件)
func TestShareDirNotImplemented(t *testing.T) {
	e := setupLockShareEnv(t)
	dir := e.mkDirViaRepo(t, "shared-dir")
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/shares", e.token,
		map[string]any{"file_id": dir})
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建目录分享应 201(创建本身允许),实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	dl := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/shares/"+created["token"].(string)+"/download", "", nil)
	if dl.Code != http.StatusNotImplemented {
		t.Fatalf("目录分享下载应 501,实际 %d", dl.Code)
	}
}

// 辅助:让测试拿到 Querier 适配器与口令策略(避免在测试里重复装配)
func credPolicyForTest() credentials.Policy {
	p := credentials.DefaultPolicy()
	p.Cost = 4 // bcrypt 加速(测试专用)
	return p
}
