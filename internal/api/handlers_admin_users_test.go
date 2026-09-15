package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/usersvc"
)

// 后台用户管理的 HTTP 用例(FE-W-04 的服务端一半)。
//
// 验收里最要紧的一条是"**禁用用户后其登录被拒**" —— 它不只是"改一个字段":
// 已经登录的会话也必须立刻失效(吊销 refresh + 自增 token_version),
// 否则管理员看到的是"我停用了他,他还在传文件"。

type userAdminEnv struct {
	handler http.Handler
	db      *db.DB
	tokens  *auth.Manager
	adminID string
	userID  string
	base    string
}

func setupUserAdmin(t *testing.T) *userAdminEnv {
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

	base := "ua" + time.Now().Format("150405.000000000")
	pass := "Admin_Passw0rd!" + base
	var adminID, userID string
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		a, cerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: base + "_admin", Email: base + "_admin@example.com",
			DisplayName: "管理员", Role: model.RoleSuperAdmin, Status: model.StatusActive,
		})
		if cerr != nil {
			return cerr
		}
		adminID = a.ID
		pol := credentials.DefaultPolicy()
		pw, herr := pol.Hash(pass)
		if herr != nil {
			return herr
		}
		if _, serr := tx.Exec(ctx,
			`UPDATE users SET password_hash = $2 WHERE id = $1`, a.ID, pw); serr != nil {
			return serr
		}
		u, uerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: base + "_user", Email: base + "_user@example.com",
			DisplayName: "普通用户", Role: model.RoleUser, Status: model.StatusActive,
		})
		if uerr != nil {
			return uerr
		}
		userID = u.ID
		if _, serr := tx.Exec(ctx,
			`UPDATE users SET password_hash = $2 WHERE id = $1`, u.ID, pw); serr != nil {
			return serr
		}
		return nil
	}); err != nil {
		t.Fatalf("建测试账号失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, id := range []string{adminID, userID} {
			_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM refresh_tokens WHERE user_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM user_sso_bindings WHERE user_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens,
		Auth: &authsvc.Service{
			Users: repo.UserRepo{}, Spaces: repo.SpaceRepo{},
			Tokens: tokens, DB: db.AsQuerier(database),
		},
		UserAdmin: &usersvc.Service{DB: db.AsQuerier(database), Users: repo.UserRepo{}, Log: testLogger()},
	})
	return &userAdminEnv{handler: h, db: database, tokens: tokens, adminID: adminID, userID: userID, base: base}
}

func (e *userAdminEnv) adminToken(t *testing.T) string {
	t.Helper()
	out, err := e.tokens.Issue(context.Background(), auth.IssueInput{
		UserID: e.adminID, Username: e.base + "_admin", Role: model.RoleSuperAdmin,
		TokenVersion: 1, Audience: auth.AudienceWeb,
	})
	if err != nil {
		t.Fatalf("签发管理员令牌失败: %v", err)
	}
	return out.AccessToken
}

// 用户与角色是权限的输入:**H5 令牌与普通用户令牌都不得访问**(2.7 分端)。
func TestAdminUsersRequiresAdminWeb(t *testing.T) {
	e := setupUserAdmin(t)
	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/admin/users", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401,实际 %d", rec.Code)
	}
	// H5 audience 的管理员令牌:仍应 403(渠道纪律)
	h5, err := e.tokens.Issue(context.Background(), auth.IssueInput{
		UserID: e.adminID, Username: e.base + "_admin", Role: model.RoleSuperAdmin,
		TokenVersion: 1, Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发 H5 令牌失败: %v", err)
	}
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/admin/users", h5.AccessToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("H5 令牌访问后台应 403,实际 %d", rec.Code)
	}
}

// 列表 + 搜索,并断言"总数 = 命中数"。
func TestAdminUserListAndSearch(t *testing.T) {
	e := setupUserAdmin(t)
	tok := e.adminToken(t)
	rec := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/admin/users?search="+e.base+"&limit=10", tok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Users []map[string]any `json:"users"`
		Total int              `json:"total"`
		Limit int              `json:"limit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是预期 JSON: %v", err)
	}
	if body.Total != 2 || len(body.Users) != 2 {
		t.Fatalf("应命中本用例的 2 个账号,实际 total=%d users=%d", body.Total, len(body.Users))
	}
	// 后台视图必须带运维字段(邮箱/最后登录/创建时间)
	first := body.Users[0]
	if _, ok := first["email"]; !ok {
		t.Fatal("后台用户视图必须带 email")
	}
	if _, ok := first["last_login_at"]; !ok {
		t.Fatal("后台用户视图必须带 last_login_at(判断'这人最近用过吗')")
	}
}

// 建号:唯一冲突必须**指出字段**(前端据此高亮输入框)。
func TestAdminUserCreateConflictNamesField(t *testing.T) {
	e := setupUserAdmin(t)
	tok := e.adminToken(t)

	// 用户名撞车
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/admin/users", tok, map[string]any{
		"username": e.base + "_user", "email": e.base + "_new@example.com", "role": "user",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("用户名撞车应 409,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Code != "account_conflict" || body.Details["field"] != "username" {
		t.Fatalf("应返回 account_conflict 且 field=username,实际 %s %v", body.Code, body.Details)
	}

	// 邮箱撞车(用户名换一个)
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/admin/users", tok, map[string]any{
		"username": e.base + "_new2", "email": e.base + "_user@example.com", "role": "user",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("邮箱撞车应 409,实际 %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Details["field"] != "email" {
		t.Fatalf("应指出 field=email,实际 %v", body.Details)
	}

	// 正常建号 → 201,并带上个人空间(建号即可用)
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/admin/users", tok, map[string]any{
		"username": e.base + "_new3", "email": e.base + "_new3@example.com",
		"display_name": "新同事", "role": "user",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建号应 201,实际 %d %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.db.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (
			SELECT id FROM spaces WHERE owner_id IN (SELECT id FROM users WHERE username = $1))`, e.base+"_new3")
		_, _ = e.db.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id IN (SELECT id FROM users WHERE username = $1)`, e.base+"_new3")
		_, _ = e.db.Pool.Exec(bg, `DELETE FROM users WHERE username = $1`, e.base+"_new3")
	})
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	var spaces int
	if err := e.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM spaces WHERE owner_id = $1 AND kind = 'personal'`, created.ID).Scan(&spaces); err != nil {
		t.Fatalf("查个人空间失败: %v", err)
	}
	if spaces != 1 {
		t.Fatalf("建号必须同时建个人空间(否则登录后文件列表 500),实际 %d 个", spaces)
	}

	// 角色不合法 → 400
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/admin/users", tok, map[string]any{
		"username": e.base + "_new4", "role": "root",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法角色应 400,实际 %d", rec.Code)
	}
}

// **验收核心**:停用后其登录立即被拒,且**刷新链路立刻失效**(会话在 access 到期后必死)。
//
// 这里刻意把"立刻"的边界写清楚(R-13 的取舍):
//   - **登录**:立刻拒(403);
//   - **刷新**:立刻拒(`/auth/refresh` 会查 DB 的 revoked_at + token_version + IsActive);
//   - **已签发的 access token**:在它自己的 TTL 内仍然有效(web 15min / h5 5min)。
//
// 这是文档 6.8 明确选择的取舍:**不把权限/状态写进每次请求都要查的地方**,
// 用短 TTL 换"零额外查询"。所以本用例最后一条断言是"旧 access token 仍可用",
// 它把这条**设计决定**钉在测试里 —— 将来若改成逐请求校验,这条会红,
// 提醒实现者同时更新文档(而不是悄悄改变语义)。
func TestAdminUserDisableBlocksLoginAndKillsRefresh(t *testing.T) {
	e := setupUserAdmin(t)
	tok := e.adminToken(t)
	ctx := context.Background()

	// 先真实登录一次,拿到一对真实令牌(而不是手工签一个假的)
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"login": e.base + "_user", "password": "Admin_Passw0rd!" + e.base, "audience": "web",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("停用前应能登录,实际 %d %s", rec.Code, rec.Body.String())
	}
	var login struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
		t.Fatalf("登录响应解析失败: %v", err)
	}
	if _, err := e.tokens.Verify(ctx, login.Token.AccessToken); err != nil {
		t.Fatalf("停用前 access token 应有效: %v", err)
	}
	// 停用前刷新应成功(证明下面的失败是"停用"造成的,而不是令牌本来就坏)
	if rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refresh_token": login.Token.RefreshToken,
	}); rec.Code != http.StatusOK {
		t.Fatalf("停用前刷新应成功,实际 %d %s", rec.Code, rec.Body.String())
	}

	// 停用
	rec = doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/admin/users/"+e.userID, tok,
		map[string]any{"status": "disabled"})
	if rec.Code != http.StatusOK {
		t.Fatalf("停用应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Status       string `json:"status"`
		TokenVersion int64  `json:"token_version"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Status != "disabled" {
		t.Fatalf("状态应为 disabled,实际 %q", body.Status)
	}
	// token_version 必须自增:这是"静默退出"的机制(6.8 纪律 3)
	if body.TokenVersion <= 1 {
		t.Fatalf("停用必须自增 token_version(否则旧 refresh 还能换票),实际 %d", body.TokenVersion)
	}

	// ① 登录立刻被拒(403,不是 500、更不是"登录成功")
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"login": e.base + "_user", "password": "Admin_Passw0rd!" + e.base, "audience": "web",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("停用后登录应 403,实际 %d %s", rec.Code, rec.Body.String())
	}

	// ② 刷新立刻失败(即使 refresh token 本身没过期)
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/auth/refresh", "", map[string]any{
		"refresh_token": login.Token.RefreshToken,
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("停用后刷新应 401(会话立刻死),实际 %d %s", rec.Code, rec.Body.String())
	}
	var rerr struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rerr)
	if rerr.Code != "token_revoked" {
		t.Fatalf("应给出 token_revoked(前端据此重新登录),实际 %q", rerr.Code)
	}

	// ③ 文档化的窗口:已签发的 access token 在自身 TTL 内仍有效(R-13 的取舍)
	if _, err := e.tokens.Verify(ctx, login.Token.AccessToken); err != nil {
		t.Fatalf("access token 在 TTL 内仍应有效(短 TTL 换零额外查询,6.8);若已改成逐请求校验,请同步更新文档与本断言: %v", err)
	}

	// ④ 重新启用后可以再登录(停用是可逆的)
	rec = doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/admin/users/"+e.userID, tok,
		map[string]any{"status": "active"})
	if rec.Code != http.StatusOK {
		t.Fatalf("启用应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/auth/login", "", map[string]any{
		"login": e.base + "_user", "password": "Admin_Passw0rd!" + e.base, "audience": "web",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("启用后应能登录,实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 角色调整 + 空值/缺字段的边界。
func TestAdminUserRoleChangeAndBadInput(t *testing.T) {
	e := setupUserAdmin(t)
	tok := e.adminToken(t)

	rec := doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/admin/users/"+e.userID, tok,
		map[string]any{"role": "dept_admin"})
	if rec.Code != http.StatusOK {
		t.Fatalf("改角色应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Role != "dept_admin" {
		t.Fatalf("角色应为 dept_admin,实际 %q", body.Role)
	}

	// 传了空 status:必须 400(而不是静默忽略 —— 那会让"点停用没反应")
	rec = doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/admin/users/"+e.userID, tok,
		map[string]any{"status": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空 status 应 400,实际 %d %s", rec.Code, rec.Body.String())
	}
	// 什么字段都不传 → 400
	rec = doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/admin/users/"+e.userID, tok, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没有任何可改字段应 400,实际 %d", rec.Code)
	}
	// 不存在的用户 → 404
	rec = doJSONReq(t, e.handler, http.MethodPatch,
		"/api/v1/admin/users/01a00000-0000-7000-8000-000000000000", tok, map[string]any{"role": "user"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的用户应 404,实际 %d", rec.Code)
	}
}
