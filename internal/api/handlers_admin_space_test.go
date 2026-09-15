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
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
)

// 空间治理(FE-W-06 / 4.3):配额与预警阈值、冻结、收回。
//
// 边界纪律(文档 4.3):后台只做**全局治理**;创建/邀请/退出属协作管理,
// 入口在桌面端与 H5 —— 因此这里**不测**"后台建空间",而是测治理动作本身。

type spaceAdminEnv struct {
	handler  http.Handler
	db       *db.DB
	tokens   *auth.Manager
	adminTok string
	userID   string
	spaceID  string
	base     string
}

func setupSpaceAdmin(t *testing.T) *spaceAdminEnv {
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

	base := "spa" + time.Now().Format("150405.000000000")
	var adminID, userID, spaceID string
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		a, cerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: base + "_admin", DisplayName: "空间治理管理员",
			Role: model.RoleSuperAdmin, Status: model.StatusActive,
		})
		if cerr != nil {
			return cerr
		}
		adminID = a.ID
		u, uerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: base + "_owner", DisplayName: "空间所有者",
			Role: model.RoleUser, Status: model.StatusActive,
		})
		if uerr != nil {
			return uerr
		}
		userID = u.ID
		sp, serr := repo.SpaceRepo{}.PersonalOf(ctx, tx, u.ID)
		if serr != nil {
			return serr
		}
		spaceID = sp.ID
		return nil
	}); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, id := range []string{adminID, userID} {
			_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	h := api.New(api.Deps{Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens})
	out, ierr := tokens.Issue(ctx, auth.IssueInput{
		UserID: adminID, Username: base + "_admin", Role: model.RoleSuperAdmin,
		TokenVersion: 1, Audience: auth.AudienceWeb,
	})
	if ierr != nil {
		t.Fatalf("签发管理员令牌失败: %v", ierr)
	}
	return &spaceAdminEnv{handler: h, db: database, tokens: tokens, adminTok: out.AccessToken,
		userID: userID, spaceID: spaceID, base: base}
}

// 列表:能按所有者/空间名搜索,并带治理需要的字段(用量占比/阈值/成员数)。
func TestAdminSpaceListAndSearch(t *testing.T) {
	e := setupSpaceAdmin(t)
	rec := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/admin/spaces?search="+e.base+"_owner&limit=10", e.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Spaces []map[string]any `json:"spaces"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if body.Total != 1 || len(body.Spaces) != 1 {
		t.Fatalf("应命中本用例的 1 个空间,实际 total=%d", body.Total)
	}
	sp := body.Spaces[0]
	for _, k := range []string{"id", "kind", "name", "owner_username", "quota_bytes",
		"used_bytes", "warn_percent", "used_percent", "frozen", "member_count", "files_count"} {
		if _, ok := sp[k]; !ok {
			t.Fatalf("治理视图缺少字段 %q(前端要显示用量占比/阈值),实际 %v", k, keys(sp))
		}
	}
	if sp["warn_percent"].(float64) != 80 {
		t.Fatalf("预警阈值默认应为 80(文档 4.3),实际 %v", sp["warn_percent"])
	}
}

// 设置配额与预警阈值;只改策略**不改用量**。
func TestAdminSpaceSetQuota(t *testing.T) {
	e := setupSpaceAdmin(t)
	rec := doJSONReq(t, e.handler, http.MethodPatch,
		"/api/v1/admin/spaces/"+e.spaceID+"/quota", e.adminTok,
		map[string]any{"quota_bytes": 1 << 30, "warn_percent": 90})
	if rec.Code != http.StatusOK {
		t.Fatalf("设置配额应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		QuotaBytes  int64 `json:"quota_bytes"`
		UsedBytes   int64 `json:"used_bytes"`
		WarnPercent int   `json:"warn_percent"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.QuotaBytes != 1<<30 || body.WarnPercent != 90 {
		t.Fatalf("配额/阈值未生效: %+v", body)
	}
	if body.UsedBytes != 0 {
		t.Fatalf("用量只能由写事务增减(R-16),后台不得改用量,实际 %d", body.UsedBytes)
	}
	// 只传一个字段时另一个保持原值
	rec = doJSONReq(t, e.handler, http.MethodPatch,
		"/api/v1/admin/spaces/"+e.spaceID+"/quota", e.adminTok, map[string]any{"warn_percent": 50})
	if rec.Code != http.StatusOK {
		t.Fatalf("部分更新应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.WarnPercent != 50 || body.QuotaBytes != 1<<30 {
		t.Fatalf("部分更新不该改动其它字段: %+v", body)
	}
}

// 参数校验:阈值必须 1..100(0 会让预警永不触发,>100 会永远触发)。
func TestAdminSpaceSetQuotaValidation(t *testing.T) {
	e := setupSpaceAdmin(t)
	cases := []map[string]any{
		{"warn_percent": 0},
		{"warn_percent": 101},
		{"quota_bytes": -1},
		{}, // 没有任何字段
	}
	for _, c := range cases {
		rec := doJSONReq(t, e.handler, http.MethodPatch,
			"/api/v1/admin/spaces/"+e.spaceID+"/quota", e.adminTok, c)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("参数非法应 400(%v),实际 %d %s", c, rec.Code, rec.Body.String())
		}
	}
	// 不存在的空间 → 404
	rec := doJSONReq(t, e.handler, http.MethodPatch,
		"/api/v1/admin/spaces/01a00000-0000-7000-8000-000000000000/quota", e.adminTok,
		map[string]any{"quota_bytes": 100})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的空间应 404,实际 %d", rec.Code)
	}
}

// 冻结/解冻 + 收回(收回 = 冻结 + 清空成员,**不删数据**)。
func TestAdminSpaceFreezeAndRevoke(t *testing.T) {
	e := setupSpaceAdmin(t)
	ctx := context.Background()

	// 加一个成员,便于验证"收回会清空成员"
	if err := e.db.InTx(ctx, func(tx pgx.Tx) error {
		a, aerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: e.base + "_member", DisplayName: "被邀请的人",
			Role: model.RoleUser, Status: model.StatusActive,
		})
		if aerr != nil {
			return aerr
		}
		t.Cleanup(func() {
			bg := context.Background()
			_, _ = e.db.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, a.ID)
			_, _ = e.db.Pool.Exec(bg, `DELETE FROM space_members WHERE user_id = $1`, a.ID)
			_, _ = e.db.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, a.ID)
			_, _ = e.db.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, a.ID)
		})
		return repo.SpaceRepo{}.AddMember(ctx, tx, e.spaceID, a.ID, "editor")
	}); err != nil {
		t.Fatalf("加成员失败: %v", err)
	}

	// 冻结
	rec := doJSONReq(t, e.handler, http.MethodPost,
		"/api/v1/admin/spaces/"+e.spaceID+"/freeze", e.adminTok, map[string]any{"frozen": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("冻结应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var frozen bool
	if err := e.db.Pool.QueryRow(ctx, `SELECT frozen FROM spaces WHERE id = $1`, e.spaceID).Scan(&frozen); err != nil {
		t.Fatalf("读冻结状态失败: %v", err)
	}
	if !frozen {
		t.Fatal("冻结未生效")
	}

	// 收回:冻结 + 清空成员,数据保留
	rec = doJSONReq(t, e.handler, http.MethodPost,
		"/api/v1/admin/spaces/"+e.spaceID+"/revoke", e.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("收回应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		MembersRemoved int64  `json:"members_removed"`
		Frozen         bool   `json:"frozen"`
		Note           string `json:"note"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.MembersRemoved < 1 || !body.Frozen {
		t.Fatalf("收回应清空成员并冻结: %+v", body)
	}
	if body.Note == "" {
		t.Fatal("收回响应必须说明'数据未删除'(机制与策略的边界要讲清楚)")
	}
	var members int
	if err := e.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM space_members WHERE space_id = $1`, e.spaceID).Scan(&members); err != nil {
		t.Fatalf("统计成员失败: %v", err)
	}
	if members != 0 {
		t.Fatalf("收回后不应残留成员,实际 %d", members)
	}
	// **空间行仍在**(不是删空间)
	var exists bool
	if err := e.db.Pool.QueryRow(ctx,
		`SELECT true FROM spaces WHERE id = $1`, e.spaceID).Scan(&exists); err != nil {
		t.Fatalf("空间行应保留(收回不等于删除): %v", err)
	}
}

// 权限边界:desktop 令牌与未登录都不得访问治理接口。
func TestAdminSpaceRequiresAdminWeb(t *testing.T) {
	e := setupSpaceAdmin(t)
	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/admin/spaces", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401,实际 %d", rec.Code)
	}
	desk, err := e.tokens.Issue(context.Background(), auth.IssueInput{
		UserID: e.userID, Username: e.base + "_owner", Role: model.RoleSuperAdmin,
		TokenVersion: 1, Audience: auth.AudienceDesktop,
	})
	if err != nil {
		t.Fatalf("签发 desktop 令牌失败: %v", err)
	}
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/admin/spaces", desk.AccessToken, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("desktop 令牌应 403,实际 %d", rec.Code)
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
