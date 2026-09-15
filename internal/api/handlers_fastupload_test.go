package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/ratelimit"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/uploadsvc"
)

// 秒传定稿端点的 HTTP 契约测试(BE-S4-04)。
//
// 走**真实**装配:真实 PG + 真实存储 + miniredis 作为挑战存储 + 真实限速器 ——
// 因为要验的三件事(免重传、失败惩罚、幂等重放)分别落在服务层、限速层与任务状态上,
// 用桩替换任何一层都测不出真实行为。

type fastEnv struct {
	handler http.Handler
	token   string
	userID  string
	spaceID string
	rootID  string
	dbase   *db.DB
	store   *storage.FS
	proof   *fastupload.Service
	mr      *miniredis.Miniredis
	cache   *cache.Client
	limiter *ratelimit.Limiter
}

func setupFastEnv(t *testing.T) *fastEnv {
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

	suffix := time.Now().Format("150405.000000000")
	var u *model.User
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		created, err := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: "fu_" + suffix, Email: "fu_" + suffix + "@example.com",
			DisplayName: "秒传接口测试用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
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

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cacheClient := cache.New(rdb, "netdisk:")
	limiter := ratelimit.New(rdb)
	proof := &fastupload.Service{KV: cacheClient, Reader: store}

	namePol := namepolicy.Default()
	uploadService := &uploadsvc.Service{
		Spaces: spaces, Files: files, Uploads: repo.UploadRepo{},
		DB: db.AsQuerier(database), Name: namePol,
		Fast: proof, FastMinSize: fastupload.MinSize,
	}
	finSvc := &finalize.Service{
		Pool: database.Pool, Storage: store,
		Spaces: spaces, Files: files, Name: namePol, Fast: proof,
	}

	t.Cleanup(func() {
		bg := context.Background()
		_, _ = database.Pool.Exec(bg, `DELETE FROM uploads WHERE user_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `
DELETE FROM file_objects WHERE hash_sha256 IN (
  SELECT DISTINCT hash_sha256 FROM files
   WHERE space_id IN (SELECT id FROM spaces WHERE owner_id = $1) AND hash_sha256 IS NOT NULL)`, u.ID)
		_, _ = database.Pool.Exec(bg,
			`DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, u.ID)
		_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, u.ID)
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	out, err := tokens.Issue(ctx, auth.IssueInput{
		UserID: u.ID, Username: u.Username, Role: u.Role, TokenVersion: u.TokenVersion,
		Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Redis: rdb, Tokens: tokens,
		Cache: cacheClient, Limiter: limiter,
		Uploads: uploadService,
		Files:   &filesvc.Service{Spaces: spaces, Files: files, DB: db.AsQuerier(database)},
		TUS:     &uploadsvc.TUSService{Service: uploadService, Stager: store, Finalizer: finSvc, DB: db.AsQuerier(database)},
		Objects: store,
	})
	return &fastEnv{
		handler: h, token: out.AccessToken, userID: u.ID,
		spaceID: sp.ID, rootID: root.ID, dbase: database, store: store,
		proof: proof, mr: mr, cache: cacheClient, limiter: limiter,
	}
}

// putObject 往存储里真的写一份内容并登记对象行(模拟"服务端已经有这份内容")。
func (e *fastEnv) putObject(t *testing.T, data []byte) string {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if _, err := e.store.Write(ctx, hash, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("写对象失败: %v", err)
	}
	if _, err := e.dbase.Pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')
ON CONFLICT (hash_sha256) DO UPDATE SET ref_count = file_objects.ref_count + 1`,
		hash, len(data), e.store.KeyFor(hash)); err != nil {
		t.Fatalf("写对象行失败: %v", err)
	}
	return hash
}

// createUpload 走 HTTP 建上传任务,返回 (upload_id, ticket, fast_upload 提示)。
func (e *fastEnv) createUpload(t *testing.T, name, hash string, size int64) (string, string, map[string]any) {
	t.Helper()
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/upload/create", e.token,
		map[string]any{"space_id": e.spaceID, "parent_id": e.rootID, "name": name, "size": size, "hash": hash})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建任务应 201,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		UploadID   string         `json:"upload_id"`
		Ticket     string         `json:"upload_ticket"`
		FastUpload map[string]any `json:"fast_upload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析建任务响应失败: %v", err)
	}
	return body.UploadID, body.Ticket, body.FastUpload
}

// 从下发的视图算样本摘要(客户端要做的事)。
func digestsFromView(t *testing.T, data []byte, view map[string]any) []string {
	t.Helper()
	rawOffsets, _ := view["sample_offsets"].([]any)
	offsets := make([]int64, 0, len(rawOffsets))
	for _, v := range rawOffsets {
		offsets = append(offsets, int64(v.(float64)))
	}
	sampleLen := int64(view["sample_len"].(float64))
	out, err := fastupload.SampleDigests(bytes.NewReader(data), offsets, sampleLen)
	if err != nil {
		t.Fatalf("算样本摘要失败: %v", err)
	}
	return out
}

// finish 发一次秒传定稿请求(ticket 走 X-Upload-Token 头,与 TUS 的凭据通道一致)。
func (e *fastEnv) finish(t *testing.T, uploadID, ticket string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload/"+uploadID+"/finish", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Upload-Token", ticket)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// **验收②④**:预检命中 → 挑战 → 免重传定稿成功(201),且内容没有被重写
func TestFastUploadFinishOverHTTP(t *testing.T) {
	e := setupFastEnv(t)
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte(i % 251)
	}
	hash := e.putObject(t, data)

	id, ticket, hint := e.createUpload(t, "fast-http.bin", hash, int64(len(data)))
	if hint == nil {
		t.Fatal("预检命中必须在建任务响应里下发 fast_upload 提示")
	}
	nonce, _ := hint["nonce"].(string)
	if nonce == "" {
		t.Fatal("提示里必须带 nonce")
	}
	digests := digestsFromView(t, data, hint)

	rec := e.finish(t, id, ticket, map[string]any{"nonce": nonce, "sample_sha256": digests})
	if rec.Code != http.StatusCreated {
		t.Fatalf("秒传定稿应 201,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["deduped"] != true {
		t.Errorf("应命中去重(deduped=true),实际 %v", body["deduped"])
	}
	if body["uploaded_bytes"].(float64) != 0 {
		t.Errorf("秒传路径一个字节都不该上传,实际 %v", body["uploaded_bytes"])
	}
	file, _ := body["file"].(map[string]any)
	if file["hash_sha256"] != hash {
		t.Errorf("新行应指向同一 hash,实际 %v", file["hash_sha256"])
	}
}

// **幂等重放**:同一 upload 再 finish 一次 → 200 + replayed(for upload_id 本身已定稿)
//
// 这条对秒传尤其重要:nonce 是一次性的,响应丢包后重试时它已失效;
// 若不靠任务状态幂等,客户端会因为"证明过期"而重传一个早就传完的文件。
func TestFastUploadFinishIsIdempotentOnReplay(t *testing.T) {
	e := setupFastEnv(t)
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte((i * 7) % 253)
	}
	hash := e.putObject(t, data)
	id, ticket, hint := e.createUpload(t, "idem.bin", hash, int64(len(data)))
	digests := digestsFromView(t, data, hint)

	first := e.finish(t, id, ticket, map[string]any{"nonce": hint["nonce"], "sample_sha256": digests})
	if first.Code != http.StatusCreated {
		t.Fatalf("首次应 201,实际 %d %s", first.Code, first.Body.String())
	}
	// 用**同一个已失效的 nonce** 重试(模拟响应丢包后的重试)
	second := e.finish(t, id, ticket, map[string]any{"nonce": hint["nonce"], "sample_sha256": digests})
	if second.Code != http.StatusOK {
		t.Fatalf("重放应 200(幂等回既有结果),实际 %d %s", second.Code, second.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &body)
	if body["replayed"] != true {
		t.Errorf("重放响应必须标明 replayed=true,实际 %v", body)
	}
	file, _ := body["file"].(map[string]any)
	if file["hash_sha256"] != hash {
		t.Errorf("重放应回同一文件,实际 %v", file["hash_sha256"])
	}
}

// **验收③**:证明失败 → 403 且**额外扣限速配额**(拿别人 hash 反复探样本必须不划算)
func TestFastUploadFailureIsPenalized(t *testing.T) {
	e := setupFastEnv(t)
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte((i * 11) % 247)
	}
	hash := e.putObject(t, data)
	id, ticket, hint := e.createUpload(t, "penalty.bin", hash, int64(len(data)))

	// 用**别的内容**算摘要 → 证明必不过
	other := make([]byte, 1<<20)
	for i := range other {
		other[i] = byte((i * 13) % 241)
	}
	digests := digestsFromView(t, other, hint)

	// 先记下当前窗口剩余额度(惩罚前)
	key, err := e.cache.Key("rl", "upload", "u", e.userID)
	if err != nil {
		t.Fatalf("构造限速键失败: %v", err)
	}
	before := e.remaining(t, key)

	rec := e.finish(t, id, ticket, map[string]any{"nonce": hint["nonce"], "sample_sha256": digests})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("证明失败应 403,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "fast_upload_denied" {
		t.Errorf("业务码应为 fast_upload_denied,实际 %v", body["code"])
	}
	after := e.remaining(t, key)
	if !(after < before-1) {
		t.Fatalf("证明失败必须**额外**扣配额(前 %d 后 %d)—— 否则猜样本是无成本的", before, after)
	}
}

// remaining 读分钟窗的剩余额度(直接问限速器,避免依赖响应头)。
func (e *fastEnv) remaining(t *testing.T, base string) int64 {
	t.Helper()
	lim := e.cfgLimit(t)
	res, err := e.limiter.Allow(context.Background(), base+":m", lim, 1)
	if err != nil {
		t.Fatalf("读限速状态失败: %v", err)
	}
	return res.Remaining
}

// cfgLimit 返回 upload 族的分钟窗阈值(与路由装配同源)。
func (e *fastEnv) cfgLimit(t *testing.T) ratelimit.Limit {
	t.Helper()
	cfg := testConfig(t)
	rl := cfg.RateLimits.For("upload")
	return ratelimit.Limit{Window: time.Minute, Max: int64(rl.PerMinute)}
}

// 没有 nonce / 没有样本 → 400(而不是默默按信任处理)
func TestFastUploadFinishRejectsMissingProof(t *testing.T) {
	e := setupFastEnv(t)
	data := make([]byte, 1<<20)
	hash := e.putObject(t, data)
	id, ticket, _ := e.createUpload(t, "noproof.bin", hash, int64(len(data)))

	rec := e.finish(t, id, ticket, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 nonce 应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 伪造一个 nonce 也过不了(挑战不存在)
	rec2 := e.finish(t, id, ticket, map[string]any{"nonce": "deadbeef", "sample_sha256": []string{}})
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("伪造 nonce 应 403,实际 %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// 摘要个数异常(超上限/长度不对)→ 400,不进入服务层
func TestFastUploadFinishValidatesSampleShape(t *testing.T) {
	e := setupFastEnv(t)
	data := make([]byte, 1<<20)
	hash := e.putObject(t, data)
	id, ticket, _ := e.createUpload(t, "shape.bin", hash, int64(len(data)))

	// 摘要不是 64 位十六进制
	rec := e.finish(t, id, ticket, map[string]any{"nonce": "x", "sample_sha256": []string{"abc"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法摘要应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 数量超上限(封顶服务端读取量)
	many := make([]string, 100)
	for i := range many {
		many[i] = "00"
	}
	rec2 := e.finish(t, id, ticket, map[string]any{"nonce": "x", "sample_sha256": many})
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("摘要数量超限应 400,实际 %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// 小文件(无 fast_upload 提示)也必须**不能**走免重传:直接 finish 会被拒
func TestFastUploadFinishDeniedForSmallFile(t *testing.T) {
	e := setupFastEnv(t)
	data := []byte("tiny-content-for-fast-upload-test")
	hash := e.putObject(t, data)
	id, ticket, hint := e.createUpload(t, "tiny.bin", hash, int64(len(data)))
	if hint != nil {
		t.Fatal("小文件不应拿到秒传提示")
	}
	rec := e.finish(t, id, ticket, map[string]any{"nonce": "made-up", "sample_sha256": []string{}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("小文件伪造 nonce 应 403,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 内容行不能在库里凭空出现
	var n int
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM files WHERE name = 'tiny.bin'`).Scan(&n); err != nil {
		t.Fatalf("查行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("被拒的秒传不应留下文件行,实际 %d", n)
	}
}
