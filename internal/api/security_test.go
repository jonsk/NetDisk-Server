package api_test

// TS-09 安全与越权用例(五项验收):
//
//	①跨空间访问全被拒  ②被踢成员立即失效  ③分享 token 不可枚举 + 密码爆破被限速
//	④秒传投毒(声明他人 hash)拿不到内容  ⑤JWT 算法混淆被拒
//
// # 为什么整份文件走"真实路由 + 真实 PostgreSQL + 真实 Redis(miniredis)"
//
// 这五条全都是"判权落在哪一层、哪个入口漏了"的问题。服务层单测能证明
// `filesvc.checkRead` 返回 403,却证明不了**每个入口都真的调用了它** ——
// 少一个入口(例如 /changes 忘了 CheckReadable、WebDAV 忘了 resolveDAVTarget 判权)
// 单测照样全绿,而线上就是一条实打实的越权读路径。所以这里一律经 `api.New`
// 组装出的**完整路由**发请求,连限速器与分享/WebDAV/TUS 也都用生产同构装配。
//
// # 状态码一律按**实现读出来的**断言,不按直觉猜 403/404
//
// 本项目同一件事在不同入口的语义并不统一,而且是有意为之:
//   - 已知存在的他人资源(文件/空间):403 `space_revoked`
//     —— filesvc.checkRead/checkWrite 走 apierr.SpaceRevoked(见 apierr.go:140,
//     SpaceRevoked = StatusForbidden)。**注意**:filesvc.checkRead 的注释与架构
//     文档写的是"被移出空间返回 410",实现是 403;客户端行为(保留本地文件、
//     不误判为远端删除)两者一致,故本文件按实现的 403 断言并在报告中说明。
//   - 他人的**上传任务**:404 `not_found` —— uploadsvc.loadAuthorized 刻意
//     "不暴露'存在但不属于你'"(上传凭据是私密的,连存在性都不该确认)。
//   - 不存在的空间:410 `space_gone`。
//
// 每条失败断言都会把**观察到的状态码与响应体**打出来(secResp.String()),
// 于是"测试挂了"本身就能自证现场,不需要再手工复现。

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/ratelimit"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/sharesvc"
	"github.com/netdisk/netdisk/internal/spacesvc"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/syncfeed"
	"github.com/netdisk/netdisk/internal/uploadsvc"
	"github.com/netdisk/netdisk/internal/usersvc"
	"github.com/netdisk/netdisk/internal/webdavauth"
)

// secPassword 是三个测试账号共用的口令(≥8 字节 + 三类字符,过 credentials.DefaultPolicy)。
const secPassword = "Sec-P@ssw0rd-1"

// secPayloadMarker 是 victim 文件的内容标记:任何响应体里出现它就说明**内容泄露**。
const secPayloadMarker = "TS09-SECRET-CROSS-SPACE-PAYLOAD"

// ---- 环境装配 ----

type secEnv struct {
	// srv 是真实 HTTP 服务(而不是直接喂 ResponseRecorder):
	// 只有经过真实 server,HEAD 的响应体抑制、Content-Length、RemoteAddr(=127.0.0.1,
	// 限速按 IP 的键)才是线上语义。
	srv    *httptest.Server
	client *http.Client
	cfg    *config.Config
	dbase  *db.DB
	store  *storage.FS

	// attacker(A):普通用户,不是 victim 任何空间的成员 —— 越权用例的主角
	attackerID       string
	attackerToken    string
	attackerRefresh  string
	attackerPersonal string
	// attackerRoot 是 A 个人空间的根目录(move 用例要把 victim 的文件挪到这里)
	attackerRoot string
	// davAttacker 是 A 的 WebDAV Basic 头(会话令牌已直接写入 Store)
	davAttacker string

	// victim(B):团队空间 owner,文件内容的真正主人
	victimID       string
	victimToken    string
	victimPersonal string

	// admin(C):super_admin,持 web audience 令牌(停用账号走它)
	adminID    string
	adminToken string

	// teamID 是 B 的团队空间,**A 默认不是成员**(验收①的越权目标)
	teamID   string
	teamRoot string
	// teamFile* 是团队空间里一个"有真实物理对象"的文件
	teamFileID   string
	teamFileName string
	teamHash     string
}

func setupSecurityEnv(t *testing.T) *secEnv {
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
	store, err := storage.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	pol := credPolicyForTest() // bcrypt cost=4:测试加速(策略强度不变)
	usersRepo := repo.UserRepo{}
	spacesRepo := repo.SpaceRepo{}
	filesRepo := repo.FileRepo{}
	suffix := time.Now().Format("150405.000000000")

	mkUser := func(prefix, role string) *model.User {
		var u *model.User
		name := prefix + "_" + suffix
		if cerr := database.InTx(ctx, func(tx pgx.Tx) error {
			created, e1 := usersRepo.Create(ctx, tx, repo.CreateInput{
				Username: name, Email: name + "@example.com",
				DisplayName: "TS09 " + prefix, Role: role, Status: model.StatusActive,
			})
			if e1 != nil {
				return e1
			}
			hash, e2 := pol.Hash(secPassword)
			if e2 != nil {
				return e2
			}
			if e3 := usersRepo.SetPassword(ctx, tx, created.ID, hash); e3 != nil {
				return e3
			}
			u = created
			return nil
		}); cerr != nil {
			t.Fatalf("建测试用户 %s 失败: %v", prefix, cerr)
		}
		// **必须重读一次**:SetPassword 会自增 token_version("改密即静默退出",见
		// repo/user.go:382),Create 那一刻返回的 tv 已经过期。拿着旧 tv 去签发令牌
		// 或写 WebDAV 会话,会让它们在下一次比对时被判为"已失效"——
		// 表现为整片 401,而原因与被测行为毫无关系(本用例踩过一次)。
		fresh, ferr := usersRepo.GetByID(ctx, database.Pool, u.ID)
		if ferr != nil {
			t.Fatalf("重读测试用户 %s 失败: %v", prefix, ferr)
		}
		return fresh
	}
	attacker := mkUser("sec_a", model.RoleUser)
	victim := mkUser("sec_b", model.RoleUser)
	admin := mkUser("sec_admin", model.RoleSuperAdmin)

	// 清理:按 owner 精确删(绝不无条件 DELETE),对象行按**本用例自己造的 hash** 删
	// —— hash 是全库共享的去重键,按 owner 清会把别人的对象行也删掉。
	var createdHash string
	t.Cleanup(func() {
		bg := context.Background()
		for _, id := range []string{attacker.ID, victim.ID, admin.ID} {
			_, _ = database.Pool.Exec(bg, `DELETE FROM shares WHERE user_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM uploads WHERE user_id = $1`, id)
			_, _ = database.Pool.Exec(bg,
				`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, id)
			_, _ = database.Pool.Exec(bg,
				`DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1)`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM groups WHERE owner_id = $1`, id)
		}
		if createdHash != "" {
			_, _ = database.Pool.Exec(bg, `DELETE FROM file_objects WHERE hash_sha256 = $1`, createdHash)
		}
		for _, id := range []string{attacker.ID, victim.ID, admin.ID} {
			_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})

	cfg := testConfig(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cacheClient := cache.New(rdb, cfg.Redis.KeyPrefix)
	limiter := ratelimit.New(rdb)
	tokens := newTokens(t, cfg)

	// 秒传预检/证明要真实的挑战存储 + 真实的已存对象读取
	proof := &fastupload.Service{KV: cacheClient, Reader: store}
	namePol := namepolicy.Default()
	uploadSvc := &uploadsvc.Service{
		Spaces: spacesRepo, Files: filesRepo, Uploads: repo.UploadRepo{},
		DB: db.AsQuerier(database), Name: namePol,
		Fast: proof, FastMinSize: fastupload.MinSize,
	}
	finSvc := &finalize.Service{
		Pool: database.Pool, Storage: store, Spaces: spacesRepo, Files: filesRepo,
		Name: namePol, Fast: proof,
	}
	authSvc := &authsvc.Service{
		Users: usersRepo, Spaces: spacesRepo, Tokens: tokens, Policy: pol,
		DB: db.AsQuerier(database),
	}
	davStore := &webdavauth.RedisStore{Cache: cacheClient}
	davVerify := func(ctx context.Context, username, password string) (string, int64, error) {
		v, verr := authSvc.VerifyCredentials(ctx, username, password)
		if verr != nil || v == nil {
			return "", 0, webdavauth.ErrUnauthorized
		}
		return v.ID, v.TokenVersion, nil
	}

	e := &secEnv{
		cfg: cfg, dbase: database, store: store,
		attackerID: attacker.ID, victimID: victim.ID, adminID: admin.ID,
	}
	davAuth := &webdavauth.Authenticator{Store: davStore, Verify: davVerify}
	handler := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Redis: rdb, Tokens: tokens,
		Cache: cacheClient, Limiter: limiter, Auth: authSvc,
		Files: &filesvc.Service{
			Spaces: spacesRepo, Files: filesRepo, DB: db.AsQuerier(database),
			Name: namePol, Feed: syncfeed.Writer{},
		},
		Spaces:    &spacesvc.Service{DB: db.AsQuerier(database), Spaces: spacesRepo, Files: filesRepo, Users: usersRepo},
		Uploads:   uploadSvc,
		TUS:       &uploadsvc.TUSService{Service: uploadSvc, Stager: store, Finalizer: finSvc, DB: db.AsQuerier(database)},
		Finalizer: finSvc,
		Objects:   store,
		Shares:    &sharesvc.Service{DB: db.AsQuerier(database), Policy: pol},
		UserAdmin: &usersvc.Service{DB: db.AsQuerier(database), Users: usersRepo, Log: testLogger()},
		WebDAV:    davAuth,
	})
	e.srv = httptest.NewServer(handler)
	t.Cleanup(e.srv.Close)
	e.client = &http.Client{Timeout: 30 * time.Second}

	// **真实登录**拿令牌与 refresh(验收②要验"refresh 也被拒",必须真的落库)
	login := func(u *model.User) (string, string) {
		res := e.doJSON(t, http.MethodPost, "/api/v1/auth/login", "",
			map[string]any{"login": u.Username, "password": secPassword, "audience": auth.AudienceDesktop}, nil)
		res.want(t, http.StatusOK, "登录 "+u.Username)
		var out struct {
			Token struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			} `json:"token"`
		}
		if err := json.Unmarshal(res.body, &out); err != nil {
			t.Fatalf("解析登录响应失败: %v body=%s", err, res.body)
		}
		if out.Token.AccessToken == "" || out.Token.RefreshToken == "" {
			t.Fatalf("登录未回令牌: %s", res)
		}
		return out.Token.AccessToken, out.Token.RefreshToken
	}
	e.attackerToken, e.attackerRefresh = login(attacker)
	e.victimToken, _ = login(victim)

	// 管理员令牌:web audience + super_admin(后台链路只接受 web,见 api.go adminChain)
	adminIssued, err := tokens.Issue(ctx, auth.IssueInput{
		UserID: admin.ID, Username: admin.Username, Role: model.RoleSuperAdmin,
		TokenVersion: admin.TokenVersion, Audience: auth.AudienceWeb,
	})
	if err != nil {
		t.Fatalf("签发管理员令牌失败: %v", err)
	}
	e.adminToken = adminIssued.AccessToken

	// **victim 用真实接口建团队空间**(不手写 SQL:空间创建要连带建根目录,
	// 漏了那一步会让后面全是 500 而看不出原因)
	sp := e.doJSON(t, http.MethodPost, "/api/v1/spaces", e.victimToken,
		map[string]any{"name": "sec-team-" + suffix}, nil)
	sp.want(t, http.StatusCreated, "victim 建团队空间")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(sp.body, &created); err != nil || created.ID == "" {
		t.Fatalf("解析建空间响应失败: %v body=%s", err, sp.body)
	}
	e.teamID = created.ID
	teamRootRow, err := filesRepo.GetRoot(ctx, database.Pool, e.teamID)
	if err != nil {
		t.Fatalf("取团队空间根目录失败: %v", err)
	}
	e.teamRoot = teamRootRow.ID
	aSp, err := spacesRepo.PersonalOf(ctx, database.Pool, attacker.ID)
	if err != nil {
		t.Fatalf("取 A 个人空间失败: %v", err)
	}
	e.attackerPersonal = aSp.ID
	aRootRow, err := filesRepo.GetRoot(ctx, database.Pool, aSp.ID)
	if err != nil {
		t.Fatalf("取 A 个人空间根目录失败: %v", err)
	}
	e.attackerRoot = aRootRow.ID
	vSp, err := spacesRepo.PersonalOf(ctx, database.Pool, victim.ID)
	if err != nil {
		t.Fatalf("取 B 个人空间失败: %v", err)
	}
	e.victimPersonal = vSp.ID

	// 团队空间里放一个**有真实物理对象**的文件(越权下载若真放行,内容就在那里可取)
	data := bytes.Repeat([]byte(secPayloadMarker), 64)
	e.teamFileName = "机密-合同.pdf"
	e.teamFileID, e.teamHash = e.putContentFile(t, e.teamID, e.teamRoot, victim.ID, e.teamFileName, data)
	createdHash = e.teamHash

	// 给 A 造一枚 WebDAV 短期会话:等价于他用 Basic 账密换到的那枚 `wd1_` 令牌。
	// 直接写 Store 是为了跳过"换票"这一步(换票本身由 handlers_webdav_test.go 覆盖),
	// 本文件要验的是"换到票之后,跨空间依然进不去"。
	davTok := webdavauth.TokenPrefix + strings.Repeat("A", 43)
	if err := davStore.Put(ctx, webdavauth.HashToken(davTok), attacker.ID, 12*time.Hour, attacker.TokenVersion); err != nil {
		t.Fatalf("写入 WebDAV 会话失败: %v", err)
	}
	if uid, tv, found, gerr := davStore.Get(ctx, webdavauth.HashToken(davTok)); gerr != nil || !found {
		t.Fatalf("WebDAV 会话写入后读不到: found=%v err=%v", found, gerr)
	} else {
		t.Logf("WebDAV 会话已就绪: user=%s tv=%d(DB 中 tv=%d)", uid, tv, attacker.TokenVersion)
	}
	e.davAttacker = webdavauth.EncodeBasic(attacker.Username, davTok)

	// 认证自检:用与 handler **同一条** Authenticate 路径先解析一次 ——
	// 否则后面任何 401 都只能看到"认证失败"这一句笼统文案,定位成本极高。
	probe, _ := http.NewRequest("PROPFIND", "/webdav/", nil)
	probe.Header.Set("Authorization", e.davAttacker)
	davRes, davErr := davAuth.Authenticate(ctx, probe)
	if davErr != nil {
		t.Fatalf("WebDAV 认证自检失败(用例装配问题,不是被测行为): %v", davErr)
	}
	var dbTV int64
	var dbStatus string
	if err := database.Pool.QueryRow(ctx,
		`SELECT token_version, status FROM users WHERE id = $1`, attacker.ID).Scan(&dbTV, &dbStatus); err != nil {
		t.Fatalf("读用户 token_version 失败: %v", err)
	}
	t.Logf("WebDAV 认证自检: user=%s session_tv=%d db_tv=%d status=%s",
		davRes.UserID, davRes.TokenVersion, dbTV, dbStatus)
	if davRes.TokenVersion != dbTV {
		t.Fatalf("会话里的 tv(%d)与库里的 tv(%d)不一致,WebDAV 必然 401", davRes.TokenVersion, dbTV)
	}

	return e
}

// putContentFile 造一个"文件行 + 真实物理对象 + file_objects 行"的文件。
//
// 为什么不能只插 files 行(像 handlers_file_test.go 的 mkFile 那样):
// 越权下载用例要证明"**内容**没有泄露",如果对象根本不存在,那么无论判权对不对
// 拿到的都是 404/500 —— 测不出任何东西。
func (e *secEnv) putContentFile(t *testing.T, spaceID, rootID, ownerID, name string, data []byte) (fileID, hash string) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:])
	if _, err := e.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写物理对象失败: %v", err)
	}
	if _, err := e.dbase.Pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE SET ref_count = file_objects.ref_count + 1`,
		hash, len(data), e.store.KeyFor(hash)); err != nil {
		t.Fatalf("写对象行失败: %v", err)
	}
	if err := e.dbase.Pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, version, depth, hash_sha256, mime_type)
VALUES ($1, $2, $3, $4, false, $5, 1, 1, $6, 'application/octet-stream') RETURNING id`,
		spaceID, rootID, ownerID, name, len(data), hash).Scan(&fileID); err != nil {
		t.Fatalf("写文件行失败: %v", err)
	}
	return fileID, hash
}

// ---- 请求与断言小工具 ----

type secResp struct {
	method string
	path   string
	status int
	header http.Header
	body   []byte
}

// String 把"观察到什么"打成一行,失败断言一律带上它(现场自证)。
func (r secResp) String() string {
	return fmt.Sprintf("%s %s -> %d body=%s", r.method, r.path, r.status, strings.TrimSpace(string(r.body)))
}

func (r secResp) code() string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Code
}

// detail 把 details.<key> 打平成字符串(string / bool / number 都可能出现),
// 键不存在返回空串。
func (r secResp) detail(key string) string {
	var b struct {
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(r.body, &b)
	if b.Details == nil {
		return ""
	}
	v, ok := b.Details[key]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}

func (r secResp) want(t *testing.T, status int, what string) secResp {
	t.Helper()
	if r.status != status {
		t.Errorf("%s: 期望状态码 %d,实际 %s", what, status, r)
	}
	return r
}

func (r secResp) wantNot(t *testing.T, status int, what string) secResp {
	t.Helper()
	if r.status == status {
		t.Errorf("%s: 不该出现状态码 %d,实际 %s", what, status, r)
	}
	return r
}

func (r secResp) wantCode(t *testing.T, code, what string) secResp {
	t.Helper()
	if got := r.code(); got != code {
		t.Errorf("%s: 期望业务码 %q,实际 %q(%s)", what, code, got, r)
	}
	return r
}

// wantDenied 是本文件的**主断言**:状态码 + 业务码一起对,两者都对才叫"按实现的语义拒绝"。
func (r secResp) wantDenied(t *testing.T, status int, code, what string) secResp {
	t.Helper()
	r.want(t, status, what)
	r.wantCode(t, code, what)
	return r
}

// noLeak 断言响应体里不含任何敏感值(内容标记/文件名/hash/id)。
func (r secResp) noLeak(t *testing.T, what string, secrets ...string) secResp {
	t.Helper()
	body := string(r.body)
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(body, s) {
			t.Errorf("%s: 响应体泄露了 %q —— %s", what, s, r)
		}
	}
	return r
}

// noHeader 断言响应头里没有泄露元数据(例如 403 还带着受害者的 ETag/文件名)。
func (r secResp) noHeader(t *testing.T, what string, keys ...string) secResp {
	t.Helper()
	for _, k := range keys {
		if v := r.header.Get(k); v != "" {
			t.Errorf("%s: 响应头 %s 泄露了 %q —— %s", what, k, v, r)
		}
	}
	return r
}

func (e *secEnv) doRaw(t *testing.T, method, path string, body io.Reader, hdr map[string]string) secResp {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("读取响应体失败(%s %s): %v", method, path, rerr)
	}
	return secResp{method: method, path: path, status: resp.StatusCode, header: resp.Header, body: raw}
}

// doJSON 发一次带 Bearer 的 JSON 请求(token 为空 = 匿名)。
func (e *secEnv) doJSON(t *testing.T, method, path, token string, body any, hdr map[string]string) secResp {
	t.Helper()
	h := map[string]string{}
	for k, v := range hdr {
		h[k] = v
	}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		rdr = bytes.NewReader(raw)
		h["Content-Type"] = "application/json"
	}
	return e.doRaw(t, method, path, rdr, h)
}

// ---- 验收①:跨空间访问全被拒 ----

// TestSecurityCrossSpaceAccessDeniedAllEntryPoints 对应验收 **①跨空间访问全被拒**。
//
// 覆盖入口:文件列表(团队空间 + 他人个人空间)、详情、HEAD、下载、下载 HEAD、
// 改名、移动(试图挪进自己的空间)、删除、TUS 创建、TUS 写分片、WebDAV PROPFIND、
// WebDAV PUT、创建分享、/changes、/changes/head、游标上报、成员列表。
//
// 除了"状态码对",还断言三件事:
//   - **响应体不泄露**:不含内容标记/文件名/hash/空间 id/用户 id;
//   - **写操作真的没发生**:文件仍在原空间原名原版本,对象引用计数不变;
//   - **正向对照**:victim 自己能读能下 —— 否则"一片 403"也可能只是入口坏了。
func TestSecurityCrossSpaceAccessDeniedAllEntryPoints(t *testing.T) {
	e := setupSecurityEnv(t)
	leaks := []string{secPayloadMarker, e.teamFileName, e.teamHash, e.teamID, e.victimID}

	// ---- 正向对照:victim(B)自己必须能读能下 ----
	okList := e.doJSON(t, http.MethodGet, "/api/v1/files?space="+e.teamID, e.victimToken, nil, nil)
	okList.want(t, http.StatusOK, "正向对照:victim 列自己的团队空间")
	if !strings.Contains(string(okList.body), e.teamFileName) {
		t.Fatalf("正向对照失败:victim 的列表里看不到自己的文件(%s)", okList)
	}
	okDl := e.doJSON(t, http.MethodGet, "/api/v1/files/"+e.teamFileID+"/content", e.victimToken, nil, nil)
	okDl.want(t, http.StatusOK, "正向对照:victim 下载自己的文件")
	if !strings.Contains(string(okDl.body), secPayloadMarker) {
		t.Fatalf("正向对照失败:victim 下载到的内容不对(%d 字节)", len(okDl.body))
	}

	// ---- 越权:同一个文件,victim 的空间,attacker(A)在每个入口都要被拒 ----
	cases := []struct {
		name       string
		method     string
		path       string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"文件列表(他人团队空间)", http.MethodGet, "/api/v1/files?space=" + e.teamID, nil, http.StatusForbidden, "space_revoked"},
		{"文件列表(他人个人空间)", http.MethodGet, "/api/v1/files?space=" + e.victimPersonal, nil, http.StatusForbidden, "space_revoked"},
		{"文件详情", http.MethodGet, "/api/v1/files/" + e.teamFileID, nil, http.StatusForbidden, "space_revoked"},
		{"文件元数据 HEAD", http.MethodHead, "/api/v1/files/" + e.teamFileID, nil, http.StatusForbidden, "space_revoked"},
		{"下载", http.MethodGet, "/api/v1/files/" + e.teamFileID + "/content", nil, http.StatusForbidden, "space_revoked"},
		{"下载 HEAD", http.MethodHead, "/api/v1/files/" + e.teamFileID + "/content", nil, http.StatusForbidden, "space_revoked"},
		{"改名", http.MethodPatch, "/api/v1/files/" + e.teamFileID, map[string]any{"name": "pwned.txt"}, http.StatusForbidden, "space_revoked"},
		// 移动的目标故意是 **A 自己的个人空间根**:若判权只看目标(或干脆不判),这一条就会把
		// 受害者的文件搬走 —— 是最有威胁的一种越权写法,所以单独列出来打。
		{"移动(挪进自己的空间)", http.MethodPost, "/api/v1/files/" + e.teamFileID + "/move", map[string]any{"parent_id": e.attackerRoot}, http.StatusForbidden, "space_revoked"},
		{"删除", http.MethodDelete, "/api/v1/files/" + e.teamFileID, nil, http.StatusForbidden, "space_revoked"},
		{"创建分享", http.MethodPost, "/api/v1/shares", map[string]any{"file_id": e.teamFileID}, http.StatusForbidden, "space_revoked"},
		{"同步 /changes", http.MethodGet, "/api/v1/changes?space=" + e.teamID, nil, http.StatusForbidden, "space_revoked"},
		{"同步 /changes/head", http.MethodGet, "/api/v1/changes/head?space=" + e.teamID, nil, http.StatusForbidden, "space_revoked"},
		{"游标上报", http.MethodPost, "/api/v1/sync/cursors", map[string]any{"client_id": "sec-probe", "space_id": e.teamID, "last_seq": 1}, http.StatusForbidden, "space_revoked"},
		{"空间成员列表", http.MethodGet, "/api/v1/spaces/" + e.teamID + "/members", nil, http.StatusForbidden, "space_revoked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := e.doJSON(t, c.method, c.path, e.attackerToken, c.body, nil)
			if c.method == http.MethodHead {
				// HEAD **必须**没有响应体(HTTP 语义,真实 server 会抑制 body)——
				// 因此 HEAD 上只能断言状态码:业务码在 HEAD 里取不到。
				// (用 httptest.NewRecorder 直连 handler 时反倒能看到 JSON 体,
				//  那是 recorder 的假象,不是线上行为。)
				r.want(t, c.wantStatus, "越权 HEAD:"+c.name)
				// 元数据头也必须没有(ETag/文件名/版本都是"资源存在"的旁证)
				r.noHeader(t, "越权 HEAD:"+c.name, "ETag", "X-File-Name", "X-File-Version")
			} else {
				r.wantDenied(t, c.wantStatus, c.wantCode, "越权:"+c.name)
			}
			r.noLeak(t, "越权:"+c.name, leaks...)
		})
	}

	// ---- TUS:建任务(跨空间)与写分片(他人的任务) ----
	t.Run("TUS 建任务(写入他人空间)", func(t *testing.T) {
		md := fmt.Sprintf("filename %s,space_id %s",
			base64.StdEncoding.EncodeToString([]byte("pwn.bin")),
			base64.StdEncoding.EncodeToString([]byte(e.teamID)))
		r := e.doRaw(t, http.MethodPost, "/tus", nil, map[string]string{
			"Authorization":   "Bearer " + e.attackerToken,
			"Tus-Resumable":   "1.0.0",
			"Upload-Length":   "16",
			"Upload-Metadata": md,
		})
		// uploadsvc.checkWritePerm:无成员关系 → SpaceRevoked(403 space_revoked)
		r.wantDenied(t, http.StatusForbidden, "space_revoked", "越权:TUS 建任务到他人空间")
		r.noLeak(t, "越权:TUS 建任务到他人空间", leaks...)
	})

	// 先让 victim 建一个上传任务(拿 upload_id 与 ticket),再让 A 去写它
	create := e.doJSON(t, http.MethodPost, "/api/v1/upload/create", e.victimToken,
		map[string]any{"space_id": e.teamID, "parent_id": e.teamRoot, "name": "victim-upload.bin", "size": 8}, nil)
	create.want(t, http.StatusCreated, "victim 建上传任务")
	var up struct {
		UploadID string `json:"upload_id"`
		Ticket   string `json:"upload_ticket"`
	}
	if err := json.Unmarshal(create.body, &up); err != nil || up.UploadID == "" {
		t.Fatalf("解析建任务响应失败: %v body=%s", err, create.body)
	}
	t.Run("TUS 写分片(他人的上传任务)", func(t *testing.T) {
		// ①不带任何上传凭据 → 401「缺少上传凭据」(HEAD:只断言状态码,见上)
		noTicket := e.doRaw(t, http.MethodHead, "/tus/"+up.UploadID, nil, map[string]string{
			"Authorization": "Bearer " + e.attackerToken, "Tus-Resumable": "1.0.0",
		})
		noTicket.want(t, http.StatusUnauthorized, "越权:TUS HEAD 无凭据")
		// ②**即使拿到 victim 的 ticket**(最坏情况:凭据泄露)→ 404,连存在性都不确认
		//    uploadsvc.loadAuthorized:"不暴露'存在但不属于你'的细节,统一 404 语义更安全"
		withTicket := e.doRaw(t, http.MethodHead, "/tus/"+up.UploadID, nil, map[string]string{
			"Authorization": "Bearer " + e.attackerToken, "Tus-Resumable": "1.0.0",
			"X-Upload-Token": up.Ticket,
		})
		withTicket.want(t, http.StatusNotFound, "越权:TUS HEAD 带他人 ticket")
		withTicket.noHeader(t, "越权:TUS HEAD 带他人 ticket", "Upload-Length", "Upload-Offset")

		// PATCH 同理(带 victim 的 ticket 也只能拿到 404)
		patch := e.doRaw(t, http.MethodPatch, "/tus/"+up.UploadID, strings.NewReader("pwned!!!"), map[string]string{
			"Authorization": "Bearer " + e.attackerToken, "Tus-Resumable": "1.0.0",
			"Content-Type": "application/offset+octet-stream", "Upload-Offset": "0",
			"X-Upload-Token": up.Ticket,
		})
		patch.wantDenied(t, http.StatusNotFound, "not_found", "越权:TUS PATCH 带他人 ticket")
		patch.noLeak(t, "越权:TUS PATCH", leaks...)
		// 一个字节都没写进去(uploaded_bytes 仍为 0)
		var uploaded int64
		if err := e.dbase.Pool.QueryRow(context.Background(),
			`SELECT uploaded_bytes FROM uploads WHERE id = $1`, up.UploadID).Scan(&uploaded); err != nil {
			t.Fatalf("读上传偏移失败: %v", err)
		}
		if uploaded != 0 {
			t.Errorf("越权的 PATCH 竟然写入了数据:uploaded_bytes=%d", uploaded)
		}
	})

	// ---- WebDAV:同一件事换个协议通道,同样要被拒 ----
	t.Run("WebDAV PROPFIND 他人空间", func(t *testing.T) {
		r := e.doRaw(t, "PROPFIND", "/webdav/"+e.teamID, nil, map[string]string{
			"Authorization": e.davAttacker, "Depth": "1",
		})
		// webdavfs/handlers_webdav.go 的 resolveDAVTarget 复用 filesvc.CheckReadable,
		// 因此与 REST 侧**同一个** 403 space_revoked(不是"另一套判权")
		r.wantDenied(t, http.StatusForbidden, "space_revoked", "越权:WebDAV PROPFIND")
		r.noLeak(t, "越权:WebDAV PROPFIND", leaks...)
	})
	t.Run("WebDAV PUT 他人空间", func(t *testing.T) {
		r := e.doRaw(t, http.MethodPut, "/webdav/"+e.teamID+"/pwned.txt", strings.NewReader("pwn"), map[string]string{
			"Authorization": e.davAttacker,
		})
		r.wantDenied(t, http.StatusForbidden, "space_revoked", "越权:WebDAV PUT")
	})
	// 正向对照:A 对自己的个人空间必须能 PROPFIND(证明上面的 403 来自判权,不是通道坏了)
	t.Run("WebDAV 自己的空间可用_对照", func(t *testing.T) {
		r := e.doRaw(t, "PROPFIND", "/webdav/"+e.attackerPersonal, nil, map[string]string{
			"Authorization": e.davAttacker, "Depth": "0",
		})
		r.wantNot(t, http.StatusUnauthorized, "对照:WebDAV 会话应通过认证")
		r.wantNot(t, http.StatusForbidden, "对照:A 访问自己的空间不该 403")
	})

	// ---- 写操作真的没发生(光看状态码不够:曾经有实现"先删了再报无权限") ----
	var name, spaceID, parentID string
	var version int64
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT name, space_id, parent_id, version FROM files WHERE id = $1`, e.teamFileID).
		Scan(&name, &spaceID, &parentID, &version); err != nil {
		t.Fatalf("受害者文件行不见了(被越权删除?): %v", err)
	}
	if name != e.teamFileName || spaceID != e.teamID || parentID != e.teamRoot || version != 1 {
		t.Errorf("受害者文件被改动:name=%q(期望 %q) space=%q(期望 %q) parent=%q(期望 %q) version=%d",
			name, e.teamFileName, spaceID, e.teamID, parentID, e.teamRoot, version)
	}
	var refCount int64
	var objState string
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT ref_count, state FROM file_objects WHERE hash_sha256 = $1`, e.teamHash).Scan(&refCount, &objState); err != nil {
		t.Fatalf("对象行不见了: %v", err)
	}
	if refCount != 1 || objState != "live" {
		t.Errorf("对象引用计数被改动:ref_count=%d state=%s", refCount, objState)
	}
	// A 自己的空间必须仍然是空的(移动/复制若真的部分执行了,这里会看到行)
	var cnt int64
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE space_id = $1 AND parent_id = $2`, e.attackerPersonal, e.attackerRoot).Scan(&cnt); err != nil {
		t.Fatalf("统计 A 空间失败: %v", err)
	}
	if cnt != 0 {
		t.Errorf("越权操作在 A 的空间里留下了 %d 个条目", cnt)
	}
	// 分享行也不能出现(创建分享的入口若漏判权,会留下一个免登录出口 —— 最危险的一种")
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM shares WHERE file_id = $1`, e.teamFileID).Scan(&cnt); err != nil {
		t.Fatalf("统计分享失败: %v", err)
	}
	if cnt != 0 {
		t.Errorf("越权创建分享竟然落库了 %d 行", cnt)
	}
}

// ---- 验收②:被踢成员立即失效 ----

// TestSecurityKickedMemberLosesAccessImmediately 对应验收 **②被踢成员立即失效**。
//
// 三件事:
//  1. **下一个请求**就失效(不是等 JWT 过期):spacesvc 的判权每次都读 space_members,
//     刻意不引入权限缓存 —— 缓存失效是这类需求最容易写错的一环,直接删掉。
//  2. 失效的原因就是代码定义的那一个:403 + `space_revoked`
//     (apierr.SpaceRevoked,见 apierr.go:140;注意 filesvc.checkRead 的注释与架构
//     文档写的是 410,实现是 403,本用例按实现断言)。
//  3. **被踢的人手里不留任何能重新进来的东西**:新签发的 access token 依然进不去
//     (空间权限从不进 token,R-13);账号被停用后 refresh 立即 401 `token_revoked`
//     (authsvc.Refresh 比对 users.token_version,见 authsvc/service.go:187)。
func TestSecurityKickedMemberLosesAccessImmediately(t *testing.T) {
	e := setupSecurityEnv(t)
	ctx := context.Background()

	// B 邀请 A 进团队空间(真实接口,不手写 space_members)
	inv := e.doJSON(t, http.MethodPost, "/api/v1/spaces/"+e.teamID+"/members", e.victimToken,
		map[string]any{"user_id": e.attackerID, "permission": model.PermEditor}, nil)
	if inv.status != http.StatusOK && inv.status != http.StatusCreated {
		t.Fatalf("邀请成员失败: %s", inv)
	}
	before := e.doJSON(t, http.MethodGet, "/api/v1/files?space="+e.teamID, e.attackerToken, nil, nil)
	before.want(t, http.StatusOK, "被邀请后 A 应能读该空间")

	// B 踢人 → A 的**下一个请求**就必须被拒
	rm := e.doJSON(t, http.MethodDelete,
		"/api/v1/spaces/"+e.teamID+"/members/"+e.attackerID, e.victimToken, nil, nil)
	rm.want(t, http.StatusNoContent, "移除成员")

	var members int64
	if err := e.dbase.Pool.QueryRow(ctx,
		`SELECT count(*) FROM space_members WHERE space_id = $1 AND user_id = $2`,
		e.teamID, e.attackerID).Scan(&members); err != nil {
		t.Fatalf("查成员关系失败: %v", err)
	}
	if members != 0 {
		t.Fatalf("成员关系没被删除(仍 %d 行),后面的断言就失去意义了", members)
	}

	for _, c := range []struct{ name, method, path string }{
		{"列表", http.MethodGet, "/api/v1/files?space=" + e.teamID},
		{"详情", http.MethodGet, "/api/v1/files/" + e.teamFileID},
		{"下载", http.MethodGet, "/api/v1/files/" + e.teamFileID + "/content"},
		{"增量流", http.MethodGet, "/api/v1/changes?space=" + e.teamID},
	} {
		r := e.doJSON(t, c.method, c.path, e.attackerToken, nil, nil)
		r.wantDenied(t, http.StatusForbidden, "space_revoked", "被踢后 "+c.name)
		r.noLeak(t, "被踢后 "+c.name, secPayloadMarker, e.teamFileName, e.teamHash, e.teamID)
	}

	// 手里不留东西(1):**新签发**的 access token 也进不去 —— 空间权限从不进 token,
	// 所以"重新登录/刷新一次就能回去"这条想象中的后门并不存在。
	ref := e.doJSON(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]any{"refresh_token": e.attackerRefresh}, nil)
	ref.want(t, http.StatusOK, "被踢 ≠ 账号失效,refresh 仍可换取新令牌")
	var rotated struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal(ref.body, &rotated); err != nil || rotated.Token.AccessToken == "" {
		t.Fatalf("解析 refresh 响应失败: %v body=%s", err, ref.body)
	}
	fresh := e.doJSON(t, http.MethodGet, "/api/v1/files?space="+e.teamID, rotated.Token.AccessToken, nil, nil)
	fresh.wantDenied(t, http.StatusForbidden, "space_revoked", "被踢后用新令牌仍应被拒")

	// 手里不留东西(2):账号被**停用**后,refresh 立即被拒
	// (后台停用的动作在 usersvc.Update 里是"吊销全部 refresh + 自增 token_version",
	//  见 usersvc/usersvc.go:187;这里走真实后台接口,不手写 SQL)
	dis := e.doJSON(t, http.MethodPatch, "/api/v1/admin/users/"+e.attackerID, e.adminToken,
		map[string]any{"status": model.StatusDisabled}, nil)
	dis.want(t, http.StatusOK, "管理员停用账号")

	// 此时 A 的**已签发 access token** 在自身 TTL 内仍然有效 —— 这是架构文档 6.8/R-13
	// 明写的取舍("短 TTL 换零额外查询",见架构文档 L683),不是本用例要报的缺陷;
	// 但它对**这个空间**已经无效(上面已验证),所以"能进空间"这件事没有残留。
	stillOK := e.doJSON(t, http.MethodGet, "/api/v1/me", e.attackerToken, nil, nil)
	stillOK.want(t, http.StatusOK, "停用后旧 access token 在 TTL 内仍可用(文档化的取舍)")

	afterDisable := e.doJSON(t, http.MethodPost, "/api/v1/auth/refresh", "",
		map[string]any{"refresh_token": rotated.Token.RefreshToken}, nil)
	afterDisable.wantDenied(t, http.StatusUnauthorized, "token_revoked", "停用后 refresh 必须立即失效")

	// WebDAV 会话同样按 token_version 立即失效(handlers_webdav.go:60 的三重校验:
	// 用户存在 + active + token_version 与签发时一致)
	dav := e.doRaw(t, "PROPFIND", "/webdav/"+e.attackerPersonal, nil, map[string]string{
		"Authorization": e.davAttacker, "Depth": "0",
	})
	dav.want(t, http.StatusUnauthorized, "停用后 WebDAV 会话必须立即失效")
	if wa := dav.header.Get("WWW-Authenticate"); !strings.HasPrefix(wa, "Basic ") {
		t.Errorf("WebDAV 401 必须带 Basic 挑战头,实际 %q", wa)
	}
}

// ---- 验收③:分享 token 不可枚举 + 密码爆破被限速 ----

// TestSecurityShareTokenNotEnumerableAndPasswordBruteForceLimited 对应验收
// **③分享 token 不可枚举、密码爆破被限速**。
//
// (a) token 熵与格式:sharesvc.NewToken = 32 字节 crypto/rand 的 base64url(43 字符,
// 256 位)—— 用例不仅看长度,还把 base64url 解回 32 字节。
// 然后**随机改写** token 里的一个字符,做 ~200 次枚举尝试,断言**全部**被拒(真实
// 状态码 404 not_found:sharesvc.Load 查不到行)。
//
// (b) 密码爆破:落地页下载走的是**登录那一档**限速(`shareLimit := d.rateLimit("login")`,
// api.go:304;login 族 ByIP=true,默认 5/s + 30/min)。用例不猜 N,而是从
// cfg.RateLimits.For("login").PerMinute 取真实阈值,断言"第 N 次之内必出现 429",
// 且 429 之前每一次都是 401 + details.reason=bad_password,并断言 429 带 Retry-After。
// 同时断言**密码错不消耗下载次数**(sharesvc.Authorize 把密码校验放在原子递增之前,
// 否则攻击者可以用试错把正常用户的链接次数耗光)。
func TestSecurityShareTokenNotEnumerableAndPasswordBruteForceLimited(t *testing.T) {
	e := setupSecurityEnv(t)
	ctx := context.Background()
	leaks := []string{secPayloadMarker, e.teamFileName, e.teamHash}

	// B 给团队空间里的文件建两个分享(一个公开、一个带密码)
	open := e.doJSON(t, http.MethodPost, "/api/v1/shares", e.victimToken,
		map[string]any{"file_id": e.teamFileID}, nil)
	open.want(t, http.StatusCreated, "建公开分享")
	locked := e.doJSON(t, http.MethodPost, "/api/v1/shares", e.victimToken,
		map[string]any{"file_id": e.teamFileID, "password": "Share-P@ssw0rd"}, nil)
	locked.want(t, http.StatusCreated, "建带密码分享")

	var openOut, lockedOut struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(open.body, &openOut); err != nil {
		t.Fatalf("解析分享响应失败: %v body=%s", err, open.body)
	}
	if err := json.Unmarshal(locked.body, &lockedOut); err != nil {
		t.Fatalf("解析分享响应失败: %v body=%s", err, locked.body)
	}
	tok := openOut.Token

	// ---- (a) 格式与熵 ----
	if len(tok) != 43 {
		t.Errorf("分享 token 长度应为 43(base64url 无填充的 32 字节),实际 %d: %q", len(tok), tok)
	}
	raw, derr := base64.RawURLEncoding.DecodeString(tok)
	if derr != nil || len(raw) != sharesvc.TokenBytes {
		t.Errorf("分享 token 不是 32 字节 base64url 随机数:err=%v 解出 %d 字节", derr, len(raw))
	}
	if base64.RawURLEncoding.EncodeToString(raw) != tok {
		t.Errorf("token 不是合法的 base64url 无填充编码: %q", tok)
	}
	if tok == lockedOut.Token {
		t.Error("两次创建的 token 不该相同")
	}
	// 正向对照:真 token 的 meta 必须 200(证明下面一片 404 是"token 不存在",不是接口坏了)
	real := e.doJSON(t, http.MethodGet, "/api/v1/shares/"+tok+"/meta", "", nil, nil)
	real.want(t, http.StatusOK, "正向对照:真 token 的 meta")
	if !strings.Contains(string(real.body), e.teamFileName) {
		t.Errorf("正向对照失败:meta 应回文件名(%s)", real)
	}

	// ---- (a) 不可枚举:~200 次"改一个字符"的尝试,全部 404 ----
	const attempts = 200
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	bad := 0
	for i := 0; i < attempts; i++ {
		pos := i % len(tok)
		idx := strings.IndexByte(alphabet, tok[pos])
		if idx < 0 {
			t.Fatalf("token 含非 base64url 字符: %q", tok)
		}
		mut := tok[:pos] + string(alphabet[(idx+1+i)%len(alphabet)]) + tok[pos+1:]
		if mut == tok {
			continue
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/shares/"+mut+"/meta", "", nil, nil)
		if r.status != http.StatusNotFound {
			bad++
			if bad <= 3 {
				t.Errorf("枚举尝试 #%d 未被拒(应为 404 not_found):%s", i, r)
			}
		}
		r.noLeak(t, fmt.Sprintf("枚举尝试 #%d", i), leaks...)
	}
	if bad != 0 {
		t.Errorf("%d/%d 次 token 枚举尝试没有返回 404 —— 分享 token 可能可枚举", bad, attempts)
	}

	// ---- (b) 密码爆破被限速 ----
	// 先确认"错口令"的真实语义:401 + details.reason=bad_password + need_password
	//
	// 注意:本用例只做过 2 次登录(装配阶段),login 族配额(默认 5/s)因而还有余量,
	// 所以**第一次**尝试必然是 401 而不是 429 —— 下面的循环据此判定"限速确实是
	// 被这串爆破打出来的",而不是被前面的登录挤出来的。
	first := e.doJSON(t, http.MethodGet, "/api/v1/shares/"+lockedOut.Token+"/download?password=wrong-1", "", nil, nil)
	first.wantDenied(t, http.StatusUnauthorized, "unauthorized", "第一次错口令")
	if got := first.detail("reason"); got != "bad_password" {
		t.Errorf("错口令的 details.reason 应为 bad_password,实际 %q(%s)", got, first)
	}
	if got := first.detail("need_password"); got != "true" {
		t.Errorf("错口令应提示 need_password,实际 %q(%s)", got, first)
	}
	first.noLeak(t, "错口令响应", leaks...)

	loginMinute := e.cfg.RateLimits.For("login").PerMinute
	if loginMinute <= 0 {
		t.Fatalf("login 族的 PerMinute 配置为 %d,无法断言限速阈值", loginMinute)
	}
	firstLimited := 0
	// 上限取真实分钟窗阈值 + 5:第 N 次以内没出现 429 就说明限速没接上
	for i := 0; i < loginMinute+5; i++ {
		r := e.doJSON(t, http.MethodGet,
			fmt.Sprintf("/api/v1/shares/%s/download?password=wrong-%d", lockedOut.Token, i+2), "", nil, nil)
		switch r.status {
		case http.StatusUnauthorized:
			if r.code() != "unauthorized" {
				t.Errorf("爆破第 %d 次:401 的业务码应为 unauthorized,实际 %s", i+2, r)
			}
			r.noLeak(t, fmt.Sprintf("爆破第 %d 次", i+2), leaks...)
		case http.StatusTooManyRequests:
			r.wantCode(t, "rate_limited", "限速响应业务码")
			if ra := r.header.Get("Retry-After"); ra == "" {
				t.Errorf("429 必须带 Retry-After(客户端据此退避),实际 %s", r)
			}
			if got := r.detail("scope"); got != "login" {
				t.Errorf("限速 scope 应为 login(与路由装配同源),实际 %q(%s)", got, r)
			}
			firstLimited = i + 2
		default:
			t.Errorf("爆破第 %d 次出现了既非 401 也非 429 的状态(可能猜中了密码/泄露了内容):%s", i+2, r)
		}
		if firstLimited > 0 {
			break
		}
	}
	if firstLimited == 0 {
		t.Fatalf("连续 %d 次错口令都没有触发限速(login 族 PerMinute=%d)—— 密码爆破无成本",
			loginMinute+5, loginMinute)
	}
	if firstLimited > loginMinute {
		t.Errorf("限速来得太晚:第 %d 次才 429,超过 login 族分钟窗阈值 %d", firstLimited, loginMinute)
	}
	t.Logf("错口令在第 %d 次被 login 族限速拦下(PerSecond=%d PerMinute=%d)",
		firstLimited, e.cfg.RateLimits.For("login").PerSecond, loginMinute)

	// 错口令**不消耗下载次数**(sharesvc.Authorize 把密码校验放在计数之前)
	var count int
	if err := e.dbase.Pool.QueryRow(ctx,
		`SELECT download_count FROM shares WHERE token = $1`, lockedOut.Token).Scan(&count); err != nil {
		t.Fatalf("读下载次数失败: %v", err)
	}
	if count != 0 {
		t.Errorf("错口令不该消耗下载次数(否则可用试错把别人的链接次数耗光),实际 %d", count)
	}
	// 爆破全程没有拿到过任何内容
	var objRefs int64
	if err := e.dbase.Pool.QueryRow(ctx,
		`SELECT ref_count FROM file_objects WHERE hash_sha256 = $1`, e.teamHash).Scan(&objRefs); err != nil {
		t.Fatalf("读对象引用失败: %v", err)
	}
	if objRefs != 1 {
		t.Errorf("爆破过程中对象引用发生变化(%d),说明有请求真的走完了下载链路", objRefs)
	}
}

// ---- 验收④:秒传投毒不可拿到内容 ----

// TestSecurityFastUploadPoisonCannotFetchContent 对应验收
// **④秒传投毒(声明他人 hash)不可拿到内容**。
//
// 攻击面的来源(6.10 / R-05):内容寻址下 hash 不是秘密(分享链接、日志、公开文件
// 都能拿到),如果"报出 hash 就能建一行指向那个对象",秒传就退化成"凭一个字符串
// 领走别人的文件"。这里的断言链:
//
//  1. 服务端确实认识这个 hash(受害者真有一个 live 对象)→ 攻击者声明它,拿到挑战;
//  2. **挑战里没有内容**:只有随机偏移 + 一次性 nonce(细则⑤);
//  3. **伪造 nonce** → 403 `fast_upload_denied`;
//  4. **用自己猜的内容算摘要**(同长度、不同字节)→ 403 `fast_upload_denied`;
//  5. 两次失败之后仍然拿不到这个 hash 的任何内容:文件行/详情/下载全部被拒,
//     且攻击者空间里不会凭空出现指向该对象的行。
//
// 关于 nonce 的一点如实说明:finalize.verifyProof 用 `Take(ctx, uploadID, hash)`
// 取挑战,而**挑战是按 upload_id 存在 Redis 里的**(nonce 只是下发给客户端的把手),
// 所以"nonce 值本身"并不逐字比对 —— 真正的闸门是"挑战一次性 + 绑定 upload_id/hash
// + 样本摘要必须与已存对象逐段吻合"。因此伪造 nonce 会走到样本校验并被拒 403;
// 想蒙过去必须真的持有那份内容(那时内容寻址语义下你本来就有这份文件)。
func TestSecurityFastUploadPoisonCannotFetchContent(t *testing.T) {
	e := setupSecurityEnv(t)
	ctx := context.Background()
	leaks := []string{secPayloadMarker, e.teamFileName, e.teamHash}

	// 攻击者能拿到的全部信息:hash 与 size(内容不在他手里)
	data := make([]byte, 1<<20) // 1MB ≥ fastupload.MinSize(32KB)才开秒传通道
	for i := range data {
		data[i] = byte((i*7 + 13) % 251)
	}
	sum := sha256.Sum256(data)
	victimHash := hex.EncodeToString(sum[:])
	// 受害者侧先存在这份内容:对象字节写进存储 + file_objects 行 + files 行
	// (这就是"服务端已经有这个 hash"的状态,秒传预检认的就是它)。
	// 走的是与生产同形的存储接口,不是桩 —— 否则"证明失败"可能只是对象读不到。
	victimFileID, gotHash := e.putContentFile(t, e.teamID, e.teamRoot, e.victimID, "victim-big.bin", data)
	if gotHash != victimHash {
		t.Fatalf("用例自身的 hash 计算不一致: %s vs %s", gotHash, victimHash)
	}
	t.Cleanup(func() {
		_, _ = e.dbase.Pool.Exec(context.Background(), `DELETE FROM file_objects WHERE hash_sha256 = $1`, victimHash)
	})

	// ① 攻击者声明他人 hash → 服务端下发挑战(因为对象确实存在)
	first := e.createPoisonUpload(t, "poison-1.bin", victimHash, int64(len(data)), leaks...)
	// ② 挑战里没有内容:只有 nonce/sample_len/sample_offsets/expires_in_seconds
	for k := range first.challenge {
		switch k {
		case "nonce", "sample_len", "sample_offsets", "expires_in_seconds":
		default:
			t.Errorf("挑战里出现了不该有的字段 %q —— 秒传挑战绝不应回内容或摘要:%v", k, first.challenge)
		}
	}
	guesses := secGuessedDigests(t, first.challenge)

	// ③ 用**真实挑战 nonce + 自己猜的内容摘要** → 403 fast_upload_denied。
	//    错误文案是"样本摘要不匹配",证明拒绝发生在**逐段比对**这一步
	//    (而不是"没有挑战"),也就是"拿不出内容就过不了"。
	wrong := e.finishFast(t, first.uploadID, first.ticket, map[string]any{
		"nonce": first.nonce, "sample_sha256": guesses,
	})
	wrong.wantDenied(t, http.StatusForbidden, "fast_upload_denied", "猜内容的秒传定稿")
	if msg := wrong.message(); !strings.Contains(msg, "样本摘要不匹配") {
		t.Errorf("猜内容的拒绝理由应指向样本比对,实际 %q(%s)", msg, wrong)
	}
	wrong.noLeak(t, "猜内容的响应", leaks...)

	// ④ **同一个任务再试一次** → 依然 403,但理由变成"挑战已失效/无挑战":
	//    finalize.verifyProof 的 Take 是**一次性**的(先校验后删除),
	//    否则"证明一次就能白拿无数次"。伪造的 nonce 值本身不构成额外通道。
	forged := e.finishFast(t, first.uploadID, first.ticket, map[string]any{
		"nonce": "0000000000000000deadbeefdeadbeef", "sample_sha256": guesses,
	})
	forged.wantDenied(t, http.StatusForbidden, "fast_upload_denied", "伪造 nonce 的秒传定稿")
	if msg := forged.message(); !strings.Contains(msg, "无效或已过期") {
		t.Errorf("挑战被消费后应报无效或已过期,实际 %q(%s)", msg, forged)
	}
	forged.noLeak(t, "伪造 nonce 的响应", leaks...)

	// ⑤ 换一个全新任务,直接拿一个**编造的 nonce** 去定稿:
	//    挑战是按 upload_id 存在 Redis 里的(nonce 只是下发给客户端的把手),
	//    所以"nonce 值"并不逐字比对 —— 这一条如实记录该语义:因为样本猜不对,
	//    结果同样是 403。想蒙过去必须真的持有那份内容。
	second := e.createPoisonUpload(t, "poison-2.bin", victimHash, int64(len(data)), leaks...)
	forged2 := e.finishFast(t, second.uploadID, second.ticket, map[string]any{
		"nonce": "forged-nonce-not-issued-by-server", "sample_sha256": guesses,
	})
	forged2.wantDenied(t, http.StatusForbidden, "fast_upload_denied", "编造 nonce 的秒传定稿")
	t.Logf("编造 nonce 的拒绝文案=%q(nonce 是 upload_id 维度的把手,真正的闸门是样本摘要)", forged2.message())

	// ⑥ 投毒之后仍然拿不到内容:攻击者对受害者的文件没有任何读路径
	for _, c := range []struct{ name, method, path string }{
		{"详情", http.MethodGet, "/api/v1/files/" + victimFileID},
		{"下载", http.MethodGet, "/api/v1/files/" + victimFileID + "/content"},
		{"下载 HEAD", http.MethodHead, "/api/v1/files/" + victimFileID + "/content"},
	} {
		r := e.doJSON(t, c.method, c.path, e.attackerToken, nil, nil)
		if c.method == http.MethodHead {
			r.want(t, http.StatusForbidden, "投毒后仍应拿不到内容的:"+c.name) // HEAD 无响应体,只能断言状态码
		} else {
			r.wantDenied(t, http.StatusForbidden, "space_revoked", "投毒后仍应拿不到内容的:"+c.name)
		}
		r.noLeak(t, "投毒后的"+c.name, leaks...)
	}
	// 攻击者自己的空间里不能凭空出现指向该 hash 的行(那正是投毒想要的结果)
	var rows int64
	if err := e.dbase.Pool.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE space_id = $1 AND hash_sha256 = $2`,
		e.attackerPersonal, victimHash).Scan(&rows); err != nil {
		t.Fatalf("统计攻击者空间失败: %v", err)
	}
	if rows != 0 {
		t.Errorf("被拒的秒传竟然在攻击者空间留下 %d 行(声明 hash 即白拿内容)", rows)
	}
	// 上传任务也不该被当成"已定稿"(否则失败惩罚形同虚设)
	for _, id := range []string{first.uploadID, second.uploadID} {
		var state string
		if err := e.dbase.Pool.QueryRow(ctx,
			`SELECT state FROM uploads WHERE id = $1`, id).Scan(&state); err != nil {
			t.Fatalf("读上传任务状态失败: %v", err)
		}
		if state != model.UploadReserved {
			t.Errorf("证明失败的秒传任务不该被定稿,实际 state=%s(id=%s)", state, id)
		}
	}
	// 对象引用计数不变(没有任何新行引用它)
	var refs int64
	if err := e.dbase.Pool.QueryRow(ctx,
		`SELECT ref_count FROM file_objects WHERE hash_sha256 = $1`, victimHash).Scan(&refs); err != nil {
		t.Fatalf("读对象引用失败: %v", err)
	}
	if refs != 1 {
		t.Errorf("对象引用应仍为 1(只有受害者那一行),实际 %d", refs)
	}
}

// message 返回错误响应体的 message 字段(用于区分"哪种拒绝理由"):
// 本项目把"样本不匹配"与"挑战无效/过期"写成不同文案,于是测试能精确断言
// 拒绝发生在哪一步,而不是笼统地"只要 403 就算过"。
func (r secResp) message() string {
	var b struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Message
}

// secPoisonUpload 是一次"声明他人 hash"的上传任务(含下发的挑战)。
type secPoisonUpload struct {
	uploadID  string
	ticket    string
	challenge map[string]any
	nonce     string
}

// createPoisonUpload 用**攻击者令牌**声明 victim 的 hash,并断言服务端下发了挑战。
func (e *secEnv) createPoisonUpload(t *testing.T, name, hash string, size int64, leaks ...string) secPoisonUpload {
	t.Helper()
	create := e.doJSON(t, http.MethodPost, "/api/v1/upload/create", e.attackerToken, map[string]any{
		"space_id": e.attackerPersonal, "parent_id": e.attackerRoot,
		"name": name, "size": size, "hash": hash,
	}, nil)
	create.want(t, http.StatusCreated, "攻击者声明他人 hash 建任务("+name+")")
	var out struct {
		UploadID   string         `json:"upload_id"`
		Ticket     string         `json:"upload_ticket"`
		FastUpload map[string]any `json:"fast_upload"`
	}
	if err := json.Unmarshal(create.body, &out); err != nil {
		t.Fatalf("解析建任务响应失败: %v body=%s", err, create.body)
	}
	if out.UploadID == "" || out.Ticket == "" {
		t.Fatalf("建任务响应缺少 upload_id/ticket: %s", create)
	}
	if out.FastUpload == nil {
		t.Fatalf("服务端应认识这个 hash 并下发秒传挑战(否则本用例没测到投毒路径):%s", create)
	}
	nonce, _ := out.FastUpload["nonce"].(string)
	if nonce == "" {
		t.Fatalf("挑战必须带 nonce:%v", out.FastUpload)
	}
	create.noLeak(t, "建任务响应("+name+")", leaks...)
	return secPoisonUpload{uploadID: out.UploadID, ticket: out.Ticket, challenge: out.FastUpload, nonce: nonce}
}

// secGuessedDigests 模拟"攻击者只有 hash、没有内容":他只能拿**自己猜的内容**
// (这里用全零字节)去算每个样本区间的 sha256 —— 与真实对象必然不同。
//
// 注意 offsets 本身是服务端随机下发的(crypto/rand),攻击者连"该猜哪几段"都不知道;
// 这里把 offset 忽略掉(全零内容在任意偏移上的摘要都相同),结论不变。
func secGuessedDigests(t *testing.T, challenge map[string]any) []string {
	t.Helper()
	rawLen, ok := challenge["sample_len"].(float64)
	if !ok {
		t.Fatalf("挑战缺少 sample_len:%v", challenge)
	}
	rawOffsets, ok := challenge["sample_offsets"].([]any)
	if !ok || len(rawOffsets) == 0 {
		t.Fatalf("挑战缺少 sample_offsets:%v", challenge)
	}
	buf := make([]byte, int64(rawLen))
	sum := sha256.Sum256(buf)
	digest := hex.EncodeToString(sum[:])
	out := make([]string, len(rawOffsets))
	for i := range out {
		out[i] = digest
	}
	return out
}

// finishFast 发一次秒传定稿请求(ticket 走 X-Upload-Token,与 TUS 的凭据通道一致)。
func (e *secEnv) finishFast(t *testing.T, uploadID, ticket string, body map[string]any) secResp {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	return e.doRaw(t, http.MethodPost, "/api/v1/upload/"+uploadID+"/finish", bytes.NewReader(raw), map[string]string{
		"Authorization":  "Bearer " + e.attackerToken,
		"Content-Type":   "application/json",
		"X-Upload-Token": ticket,
	})
}

// ---- 验收⑤:JWT 算法混淆被拒 ----

// TestSecurityJWTAlgorithmConfusionRejected 对应验收 **⑤JWT 算法混淆被拒**。
//
// 拦截点只有一处,但它是"漏一行就全线失守"的那种:
//
//	internal/auth/jwt.go 的 parser():jwt.WithValidMethods([]string{"HS256"})
//	  + keyfunc 里再确认 t.Method.Alg() == HS256      ← R-14 的算法白名单闸门
//	middleware.Auth(middleware.go:159)                ← 统一入口,401 由它写出
//
// R-14 同时被 server/cmd/depsguard 静态检查(见报告里粘贴的输出)。
//
// 逐项断言(状态码一律按实现):
//
//	alg=none                → 401 unauthorized
//	RS256(算法混淆)        → 401 unauthorized(白名单在验签之前就否掉)
//	公钥当 HMAC 密钥        → 401 unauthorized(签名不匹配)
//	payload 被篡改          → 401 unauthorized(签名不匹配)
//	过期超过 leeway(60s)   → 401 token_expired
//	过期但在 leeway 内       → 200(证明 leeway 真的是 60s,不是"碰巧被拒")
//	aud 不符                → 403 forbidden —— **注意这里不是 401**:
//	                          aud 校验发生在 middleware.Auth 的 audience 白名单
//	                          (middleware.go:176),业务码 forbidden。
//	                          这一点与"算法类必须 401"不同,按实现断言。
//	token_version 不符      → **不做逐请求比对**(200):见下方注释,是文档化的取舍。
func TestSecurityJWTAlgorithmConfusionRejected(t *testing.T) {
	e := setupSecurityEnv(t)
	now := time.Now()
	leeway := e.cfg.JWT.Leeway.Std()

	// 正向对照:合法令牌必须能过(否则"全部 401"可能只是链路坏了)
	valid := e.doJSON(t, http.MethodGet, "/api/v1/me", e.adminToken, nil, nil)
	valid.want(t, http.StatusOK, "正向对照:合法管理员令牌")
	validAdmin := e.doJSON(t, http.MethodGet, "/api/v1/admin/ping", e.adminToken, nil, nil)
	validAdmin.want(t, http.StatusOK, "正向对照:合法管理员令牌访问后台")

	claim := func(sub, role, aud string, tv int64, iat, exp time.Time) jwt.MapClaims {
		return jwt.MapClaims{
			"iss": e.cfg.JWT.Issuer,
			"sub": sub,
			"aud": []string{aud},
			"jti": fmt.Sprintf("sec-%d", now.UnixNano()),
			"iat": iat.Unix(),
			"nbf": iat.Unix(),
			"exp": exp.Unix(),
			"typ": string(auth.TypeAccess),
			"usr": "sec-forged",
			"rol": role,
			"tv":  tv,
		}
	}
	// 攻击者视角:他只知道"某个普通用户的 sub",不知道密钥
	desktopUser := claim(e.attackerID, model.RoleUser, auth.AudienceDesktop, 1, now.Add(-time.Minute), now.Add(10*time.Minute))

	t.Run("alg_none_被拒", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, desktopUser)
		raw, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if err != nil {
			t.Fatalf("构造 alg=none 令牌失败: %v", err)
		}
		if !strings.Contains(strings.Split(raw, ".")[0], "bm9uZQ") {
			// header 段 base64url("alg":"none") 解出来必须真的写着 none,
			// 否则用例本身就没构造出 alg=none(那会让这条断言变成假绿)
			head, herr := base64.RawURLEncoding.DecodeString(strings.Split(raw, ".")[0])
			if herr != nil || !strings.Contains(string(head), `"alg":"none"`) {
				t.Fatalf("构造出的 token 头部不是 alg=none(用例自身有问题): %s", raw)
			}
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/me", raw, nil, nil)
		r.wantDenied(t, http.StatusUnauthorized, "unauthorized", "alg=none 令牌")
	})

	t.Run("RS256_算法混淆被拒", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatalf("生成 RSA 密钥失败: %v", err)
		}
		raw, err := jwt.NewWithClaims(jwt.SigningMethodRS256, desktopUser).SignedString(key)
		if err != nil {
			t.Fatalf("构造 RS256 令牌失败: %v", err)
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/me", raw, nil, nil)
		r.wantDenied(t, http.StatusUnauthorized, "unauthorized", "RS256 混淆令牌")
	})

	t.Run("公钥当HMAC密钥被拒", func(t *testing.T) {
		key, err := rsa.GenerateKey(rand.Reader, 1024)
		if err != nil {
			t.Fatalf("生成 RSA 密钥失败: %v", err)
		}
		pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatalf("序列化公钥失败: %v", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
		// 经典混淆:把"公钥文本"当 HMAC 密钥去签 HS256(在只钉算法的实现里会失效)
		raw, err := jwt.NewWithClaims(jwt.SigningMethodHS256, desktopUser).SignedString(pemBytes)
		if err != nil {
			t.Fatalf("构造公钥当密钥的令牌失败: %v", err)
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/me", raw, nil, nil)
		r.wantDenied(t, http.StatusUnauthorized, "unauthorized", "公钥当 HMAC 密钥的令牌")
	})

	t.Run("payload被篡改被拒", func(t *testing.T) {
		// 先签一个**合法**的 web+普通用户令牌(它访问后台应 403 角色不足)
		orig := claim(e.attackerID, model.RoleUser, auth.AudienceWeb, 1, now.Add(-time.Minute), now.Add(10*time.Minute))
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, orig).SignedString([]byte(e.cfg.JWT.Secret))
		if err != nil {
			t.Fatalf("签发合法令牌失败: %v", err)
		}
		origResp := e.doJSON(t, http.MethodGet, "/api/v1/admin/ping", signed, nil, nil)
		origResp.wantDenied(t, http.StatusForbidden, "forbidden", "原令牌:普通用户访问后台")

		parts := strings.Split(signed, ".")
		if len(parts) != 3 {
			t.Fatalf("令牌不是三段式: %q", signed)
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("解 payload 失败: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			t.Fatalf("解 payload JSON 失败: %v", err)
		}
		m["rol"] = model.RoleSuperAdmin // 把自己改成超管
		m["sub"] = e.adminID            // 并冒用管理员的 id
		newPayload, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("重编码 payload 失败: %v", err)
		}
		tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(newPayload) + "." + parts[2]

		r := e.doJSON(t, http.MethodGet, "/api/v1/admin/ping", tampered, nil, nil)
		r.wantDenied(t, http.StatusUnauthorized, "unauthorized", "篡改 payload 的令牌")
	})

	t.Run("过期超过leeway被拒", func(t *testing.T) {
		// 过期 10 分钟,远超 leeway(60s)
		expired := claim(e.attackerID, model.RoleUser, auth.AudienceDesktop, 1,
			now.Add(-20*time.Minute), now.Add(-10*time.Minute))
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, expired).SignedString([]byte(e.cfg.JWT.Secret))
		if err != nil {
			t.Fatalf("签发过期令牌失败: %v", err)
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/me", signed, nil, nil)
		r.wantDenied(t, http.StatusUnauthorized, "token_expired", "过期超过 leeway 的令牌")
	})

	t.Run("leeway内仍有效", func(t *testing.T) {
		if leeway <= 5*time.Second {
			t.Skipf("leeway 配置为 %v,不足以做内外边界断言", leeway)
		}
		// 刚过期 10 秒(在 60s leeway 内):必须放行 —— 证明上面的 401 是"真过期",
		// 而不是"任何 exp 偏早都被拒"
		inside := claim(e.attackerID, model.RoleUser, auth.AudienceDesktop, 1,
			now.Add(-2*time.Minute), now.Add(-10*time.Second))
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, inside).SignedString([]byte(e.cfg.JWT.Secret))
		if err != nil {
			t.Fatalf("签发 leeway 内令牌失败: %v", err)
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/me", signed, nil, nil)
		r.want(t, http.StatusOK, "过期 10s(在 60s leeway 内)应放行")
	})

	t.Run("audience不符被拒_403", func(t *testing.T) {
		// 合法签名的 desktop 令牌 + 超管角色 → 后台只接受 web audience
		// middleware.Auth(d.Tokens, auth.AudienceWeb) 的 audience 白名单命中
		// → apierr.Forbidden(403 forbidden),**不是 401**
		desktopAdmin := claim(e.adminID, model.RoleSuperAdmin, auth.AudienceDesktop, 1,
			now.Add(-time.Minute), now.Add(10*time.Minute))
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, desktopAdmin).SignedString([]byte(e.cfg.JWT.Secret))
		if err != nil {
			t.Fatalf("签发 desktop 超管令牌失败: %v", err)
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/admin/ping", signed, nil, nil)
		r.wantDenied(t, http.StatusForbidden, "forbidden", "aud=desktop 访问 web-only 后台")
		// audience 不是后台接口的地方(不限 audience)本来就不该拒绝:如实记下来,
		// 免得有人把"aud 只在分端接口上被校验"误当成缺陷
		me := e.doJSON(t, http.MethodGet, "/api/v1/me", signed, nil, nil)
		me.want(t, http.StatusOK, "aud 不匹配但接口不限 audience:按实现放行")
	})

	t.Run("token_version_不做逐请求比对", func(t *testing.T) {
		// **如实断言实现的语义,而不是我希望的语义**:
		// access token 的 tv 不参与逐请求校验 —— 吊销/降权靠"短 TTL + refresh 侧比对"
		// (authsvc.Refresh 比对 users.token_version,见 authsvc/service.go:187;
		//  WebDAV 会话另比对,见 handlers_webdav.go:60)。架构文档 6.8/R-13 明确写着
		// "已签发的 access token 在自身 TTL 内仍有效(web 15min / h5 5min)——这是
		// '短 TTL 换零额外查询'的明确取舍,不是遗漏"(架构文档 L683)。
		// 而且"伪造 tv"本身需要密钥(否则签名就不过),所以它不是攻击向量。
		// 真正会"拒不出去"的路径由验收②的用例断言(停用后 refresh 立即 401 token_revoked)。
		forged := claim(e.attackerID, model.RoleUser, auth.AudienceDesktop, 9999,
			now.Add(-time.Minute), now.Add(10*time.Minute))
		signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, forged).SignedString([]byte(e.cfg.JWT.Secret))
		if err != nil {
			t.Fatalf("签发 tv 不符令牌失败: %v", err)
		}
		r := e.doJSON(t, http.MethodGet, "/api/v1/me", signed, nil, nil)
		r.want(t, http.StatusOK, "access token 的 tv 不参与逐请求校验(文档化取舍)")
		if r.status == http.StatusOK {
			t.Log("已确认:tv=9999 的 access token 仍被放行;refresh/WebDAV 路径才比对 tv(见验收②)")
		}
	})
}
