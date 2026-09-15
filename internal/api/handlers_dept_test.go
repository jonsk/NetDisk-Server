package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/orgsvc"
	"github.com/netdisk/netdisk/internal/repo"
)

// 部门树后台接口的 HTTP 层测试(BE-S3-01)。
//
// 重点是**权限边界**:组织树是"谁能看到哪些团队空间"的输入,
// 因此 web+管理员才能改;H5/桌面令牌与普通用户必须被挡在外面(2.7)。

type deptEnv struct {
	handler    http.Handler
	adminTok   string
	userTok    string
	h5Tok      string
	desktopTok string
	dbase      *db.DB
}

func setupDeptEnv(t *testing.T) *deptEnv {
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
	// 与 orgsvc 包的部门树用例串行化:闭包表重建是全库操作,
	// 两个测试包并行会互相清表,表现为"单独跑能过、全量跑随机挂"。
	lockDeptTests(t, dsn)
	// 部门树的操作是全库性的,先清干净避免与其它用例互相影响
	for _, sql := range []string{
		`DELETE FROM user_departments`, `DELETE FROM department_closure`, `DELETE FROM departments`,
	} {
		if _, err := database.Pool.Exec(ctx, sql); err != nil {
			t.Fatalf("清理部门数据失败: %v", err)
		}
	}

	suffix := time.Now().Format("150405.000000000")
	users := repo.UserRepo{}
	var adminID, userID string
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		a, err := users.Create(ctx, tx, repo.CreateInput{
			Username: "dept_admin_" + suffix, Email: "dept_admin_" + suffix + "@example.com",
			DisplayName: "部门管理员", Role: model.RoleSuperAdmin,
		})
		if err != nil {
			return err
		}
		adminID = a.ID
		u, err := users.Create(ctx, tx, repo.CreateInput{
			Username: "dept_user_" + suffix, Email: "dept_user_" + suffix + "@example.com",
			DisplayName: "普通用户", Role: model.RoleUser,
		})
		if err != nil {
			return err
		}
		userID = u.ID
		return nil
	}); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM user_departments`)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM department_closure`)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM departments`)
		_, _ = database.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = ANY($1::uuid[])`,
			[]string{adminID, userID})
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	issue := func(uid, uname, role, aud string) string {
		out, err := tokens.Issue(ctx, auth.IssueInput{
			UserID: uid, Username: uname, Role: role, TokenVersion: 1, Audience: aud,
		})
		if err != nil {
			t.Fatalf("签发令牌失败: %v", err)
		}
		return out.AccessToken
	}

	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens,
		Org: &orgsvc.Service{Depts: repo.DeptRepo{}, DB: db.AsQuerier(database)},
	})
	return &deptEnv{
		handler:    h,
		adminTok:   issue(adminID, "dept_admin_"+suffix, model.RoleSuperAdmin, auth.AudienceWeb),
		userTok:    issue(userID, "dept_user_"+suffix, model.RoleUser, auth.AudienceWeb),
		h5Tok:      issue(adminID, "dept_admin_"+suffix, model.RoleSuperAdmin, auth.AudienceDesktop),
		desktopTok: issue(adminID, "dept_admin_"+suffix, model.RoleSuperAdmin, auth.AudienceDesktop),
		dbase:      database,
	}
}

func (e *deptEnv) do(t *testing.T, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doJSONReq(t, e.handler, method, path, token, body)
}

// 未认证 → 401
func TestDeptRequiresAuth(t *testing.T) {
	e := setupDeptEnv(t)
	rec := e.do(t, http.MethodGet, "/api/v1/admin/departments", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401,got %d", rec.Code)
	}
}

// 普通用户(web)→ 403
func TestDeptForbiddenForNormalUser(t *testing.T) {
	e := setupDeptEnv(t)
	rec := e.do(t, http.MethodGet, "/api/v1/admin/departments", e.userTok, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户应 403,got %d body=%s", rec.Code, rec.Body.String())
	}
}

// H5 与桌面令牌即使角色是管理员也必须被拒(2.7:分端)
func TestDeptRejectsNonWebAudience(t *testing.T) {
	e := setupDeptEnv(t)
	for name, tok := range map[string]string{"h5": e.h5Tok, "desktop": e.desktopTok} {
		rec := e.do(t, http.MethodGet, "/api/v1/admin/departments", tok, nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 令牌应 403,got %d body=%s", name, rec.Code, rec.Body.String())
		}
		// 写操作同样被挡
		rec2 := e.do(t, http.MethodPost, "/api/v1/admin/departments", tok, map[string]any{"name": "x"})
		if rec2.Code != http.StatusForbidden {
			t.Errorf("%s 令牌建部门应 403,got %d", name, rec2.Code)
		}
	}
}

// 建部门 → 201,字段齐全;树接口能看到它
func TestDeptCreateAndTree(t *testing.T) {
	e := setupDeptEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/admin/departments", e.adminTok,
		map[string]any{"name": "研发中心", "sort_order": 3})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建部门应 201,got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	for _, k := range []string{"id", "name", "parent_id", "source", "is_root"} {
		if _, ok := created[k]; !ok {
			t.Errorf("缺少字段 %q: %s", k, rec.Body.String())
		}
	}
	if created["is_root"] != true {
		t.Errorf("无 parent_id 时应标记为根,got %v", created["is_root"])
	}

	tree := e.do(t, http.MethodGet, "/api/v1/admin/departments", e.adminTok, nil)
	if tree.Code != http.StatusOK {
		t.Fatalf("取树应 200,got %d", tree.Code)
	}
	var tv map[string]any
	_ = json.Unmarshal(tree.Body.Bytes(), &tv)
	if n, _ := tv["total"].(float64); n != 1 {
		t.Fatalf("树应有 1 个部门,got %v", tv["total"])
	}
}

// 空名 → 400;父不存在 → 404
func TestDeptCreateValidation(t *testing.T) {
	e := setupDeptEnv(t)
	rec := e.do(t, http.MethodPost, "/api/v1/admin/departments", e.adminTok, map[string]any{"name": "  "})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空名应 400,got %d body=%s", rec.Code, rec.Body.String())
	}
	rec2 := e.do(t, http.MethodPost, "/api/v1/admin/departments", e.adminTok, map[string]any{
		"name": "孤儿部", "parent_id": "00000000-0000-7000-8000-000000000000",
	})
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("父不存在应 404,got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// 子树接口:带上 relative_depth,便于前端一次渲染
func TestDeptSubtreeEndpoint(t *testing.T) {
	e := setupDeptEnv(t)
	rootID := createDept(t, e, "", "总部")
	aID := createDept(t, e, rootID, "研发")
	createDept(t, e, aID, "平台组")

	rec := e.do(t, http.MethodGet, "/api/v1/admin/departments/"+rootID+"/subtree", e.adminTok, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("取子树应 200,got %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if n, _ := body["total"].(float64); n != 3 {
		t.Fatalf("子树应有 3 个节点,got %v", body["total"])
	}
	nodes, _ := body["nodes"].([]any)
	depths := map[string]float64{}
	for _, n := range nodes {
		m := n.(map[string]any)
		depths[m["name"].(string)] = m["relative_depth"].(float64)
	}
	if depths["总部"] != 0 || depths["研发"] != 1 || depths["平台组"] != 2 {
		t.Fatalf("相对深度不对: %v", depths)
	}
}

// 重建闭包表:200 且返回统计;幂等(两次结果行数一致)
func TestDeptRebuildEndpointIsIdempotent(t *testing.T) {
	e := setupDeptEnv(t)
	rootID := createDept(t, e, "", "总部")
	aID := createDept(t, e, rootID, "研发")
	createDept(t, e, aID, "平台组")
	// 再开一个分支,确保分支场景被覆盖(闭包表递归方向写反时就是这里会缺行)
	bID := createDept(t, e, rootID, "市场")
	createDept(t, e, bID, "品牌组")

	first := e.do(t, http.MethodPost, "/api/v1/admin/departments/rebuild-closure", e.adminTok, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("重建应 200,got %d body=%s", first.Code, first.Body.String())
	}
	var r1 map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &r1)
	// 5 个节点:root:1 + 研发:2 + 平台组:3 + 市场:2 + 品牌组:3 = 11
	if n, _ := r1["closure_rows"].(float64); n != 11 {
		t.Fatalf("闭包行数应为 11,got %v(重建可能漏行)", r1["closure_rows"])
	}
	if r1["rebuilt"] != true {
		t.Errorf("应标记 rebuilt=true,got %v", r1["rebuilt"])
	}

	second := e.do(t, http.MethodPost, "/api/v1/admin/departments/rebuild-closure", e.adminTok, nil)
	var r2 map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &r2)
	if r2["closure_rows"] != r1["closure_rows"] {
		t.Fatalf("重建不幂等: %v vs %v", r1["closure_rows"], r2["closure_rows"])
	}
}

// 删叶子 204;删有子节点的 409
func TestDeptDeleteEndpoints(t *testing.T) {
	e := setupDeptEnv(t)
	rootID := createDept(t, e, "", "总部")
	childID := createDept(t, e, rootID, "研发")

	rec := e.do(t, http.MethodDelete, "/api/v1/admin/departments/"+rootID, e.adminTok, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("删非叶子应 409,got %d body=%s", rec.Code, rec.Body.String())
	}
	rec2 := e.do(t, http.MethodDelete, "/api/v1/admin/departments/"+childID, e.adminTok, nil)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("删叶子应 204,got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

func createDept(t *testing.T, e *deptEnv, parent, name string) string {
	t.Helper()
	rec := e.do(t, http.MethodPost, "/api/v1/admin/departments", e.adminTok,
		map[string]any{"name": name, "parent_id": parent})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建部门 %s 失败: %d %s", name, rec.Code, rec.Body.String())
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("建部门 %s 未返回 id", name)
	}
	return id
}
