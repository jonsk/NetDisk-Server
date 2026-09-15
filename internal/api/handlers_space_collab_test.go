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
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/spacesvc"
)

// 团队空间协作(FE-W-11 / 4.3):建 / 查 / 邀请 / 退出 / 转让 / 解散。
//
// 验收里最要紧的一条:**owner 未转让就退出 → 409 提示**(而不是 500 或静默成功)。

type collabEnv struct {
	handler http.Handler
	db      *db.DB
	tokens  *auth.Manager
	ownerID string
	owner   string // owner 的 token
	otherID string
	other   string // 另一个用户的 token
	base    string
}

func setupCollab(t *testing.T) *collabEnv {
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

	base := "col" + time.Now().Format("150405.000000000")
	var ownerID, otherID string
	if err := database.InTx(ctx, func(tx pgx.Tx) error {
		o, cerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: base + "_owner", DisplayName: "空间所有者",
			Role: model.RoleUser, Status: model.StatusActive,
		})
		if cerr != nil {
			return cerr
		}
		ownerID = o.ID
		u, uerr := repo.UserRepo{}.Create(ctx, tx, repo.CreateInput{
			Username: base + "_other", DisplayName: "另一个用户",
			Role: model.RoleUser, Status: model.StatusActive,
		})
		if uerr != nil {
			return uerr
		}
		otherID = u.ID
		return nil
	}); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		// 团队空间硬删会连带 files/spaces;这里按 owner 精确清理,绝不无条件 DELETE
		for _, id := range []string{ownerID, otherID} {
			_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM space_members WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM groups WHERE owner_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM files WHERE space_id IN (SELECT id FROM spaces WHERE owner_id=$1)`, id)
		}
		for _, id := range []string{ownerID, otherID} {
			_, _ = database.Pool.Exec(bg, `DELETE FROM spaces WHERE owner_id = $1`, id)
			_, _ = database.Pool.Exec(bg, `DELETE FROM users WHERE id = $1`, id)
		}
	})

	cfg := testConfig(t)
	tokens := newTokens(t, cfg)
	h := api.New(api.Deps{
		Cfg: cfg, Log: testLogger(), DB: database, Tokens: tokens,
		Spaces: &spacesvc.Service{DB: db.AsQuerier(database), Spaces: repo.SpaceRepo{}, Users: repo.UserRepo{}},
		// 协作用例要顺带验证"新空间的根目录已建"与"退出后读不到" —— 那要真的走
		// 文件列表接口,所以 filesvc 也必须装配(否则 500"文件服务未装配"，
		// 而失败信息会指向一个与本用例无关的地方)
		Files: &filesvc.Service{Spaces: repo.SpaceRepo{}, Files: repo.FileRepo{}, DB: db.AsQuerier(database)},
		// 权限用例要证明"reader 写不了" —— 用一个真实的写入口(建上传任务)来验,
		// 所以 uploadsvc 也要装配;否则得到的是 500"未装配"而不是 403(判权没被验到)
		Uploads: newUploadSvc(database),
	})
	issue := func(id, name string) string {
		out, ierr := tokens.Issue(ctx, auth.IssueInput{
			UserID: id, Username: name, Role: model.RoleUser,
			TokenVersion: 1, Audience: auth.AudienceDesktop, // H5 与桌面端都走同一组接口
		})
		if ierr != nil {
			t.Fatalf("签发令牌失败: %v", ierr)
		}
		return out.AccessToken
	}
	return &collabEnv{handler: h, db: database, tokens: tokens,
		ownerID: ownerID, owner: issue(ownerID, base+"_owner"),
		otherID: otherID, other: issue(otherID, base+"_other"), base: base}
}

func (e *collabEnv) createSpace(t *testing.T, name string) string {
	t.Helper()
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces", e.owner,
		map[string]any{"name": name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("建空间应 201,实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		IsOwner bool   `json:"is_owner"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Kind != "team" || !body.IsOwner {
		t.Fatalf("创建者应是 team 空间的 owner: %+v", body)
	}
	return body.ID
}

// 建空间 → 列表可见(owner 与成员各看各的)→ 建号即带根目录。
func TestSpaceCreateAndList(t *testing.T) {
	e := setupCollab(t)
	id := e.createSpace(t, "协作空间-"+e.base)

	rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/spaces", e.owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Spaces []map[string]any `json:"spaces"`
		Total  int              `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	found := false
	for _, sp := range list.Spaces {
		if sp["id"] == id {
			found = true
			if sp["is_owner"] != true {
				t.Fatalf("owner 视角 is_owner 应为 true: %+v", sp)
			}
		}
	}
	if !found {
		t.Fatalf("新建的空间应出现在列表里(共 %d 个)", list.Total)
	}
	// 另一个用户**看不到**这个空间(服务端按权限过滤,不是前端过滤)
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/spaces", e.other, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	for _, sp := range list.Spaces {
		if sp["id"] == id {
			t.Fatal("非成员不该在列表里看到别人的团队空间")
		}
	}
	// 根目录已建(否则列表 500)
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?space="+id, e.owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("新空间的列表应可用(说明根目录已建),实际 %d %s", rec.Code, rec.Body.String())
	}
}

// 邀请成员 → 对方能看到并读;改权限到 reader → 写被拒。
func TestSpaceInviteAndPermission(t *testing.T) {
	e := setupCollab(t)
	id := e.createSpace(t, "邀请空间-"+e.base)

	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/members", e.owner,
		map[string]any{"user_id": e.otherID, "permission": "editor"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("邀请应成功,实际 %d %s", rec.Code, rec.Body.String())
	}
	// 被邀请者:能看到空间,且能读文件列表
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?space="+id, e.other, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("被邀请者应能读,实际 %d %s", rec.Code, rec.Body.String())
	}
	// 降为 reader:写请求应被拒(403)
	rec = doJSONReq(t, e.handler, http.MethodPut, "/api/v1/spaces/"+id+"/members", e.owner,
		map[string]any{"user_id": e.otherID, "permission": "reader"})
	if rec.Code != http.StatusOK {
		t.Fatalf("改权限应成功,实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/upload/create", e.other,
		map[string]any{"space_id": id, "name": "x.bin", "size": 10})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reader 不该能上传,实际 %d %s", rec.Code, rec.Body.String())
	}
}

// **验收核心**:owner 未转让就退出 → 409 且提示"先转让"。
func TestSpaceLeaveByOwnerConflicts(t *testing.T) {
	e := setupCollab(t)
	id := e.createSpace(t, "退出空间-"+e.base)

	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/leave", e.owner, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("owner 退出应 409(先转让),实际 %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !containsHint(body.Message, "转让") {
		t.Fatalf("409 的提示应说明要先转让,实际 %q", body.Message)
	}

	// 非 owner 的成员退出 → 200,且之后读不到该空间
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/members", e.owner,
		map[string]any{"user_id": e.otherID, "permission": "editor"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("邀请失败: %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/leave", e.other, nil)
	// 退出成功是 **204**(无内容);200 也行,但实现选 204 更贴合"没有响应体"
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
		t.Fatalf("成员退出应 204/200,实际 %d %s", rec.Code, rec.Body.String())
	}
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?space="+id, e.other, nil)
	if rec.Code != http.StatusForbidden && rec.Code != http.StatusGone {
		t.Fatalf("退出后不该再能访问(403/410),实际 %d", rec.Code)
	}
}

// 转让:仅 owner 可转让;转让后原 owner 变普通成员,新 owner 可解散。
func TestSpaceTransferAndDissolve(t *testing.T) {
	e := setupCollab(t)
	id := e.createSpace(t, "转让空间-"+e.base)

	// 先让 other 成为成员(转让要求目标已是成员/UpsertMember 会补)
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/members", e.owner,
		map[string]any{"user_id": e.otherID, "permission": "editor"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
		t.Fatalf("邀请失败: %d %s", rec.Code, rec.Body.String())
	}
	// 非 owner 不能转让
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/transfer", e.other,
		map[string]any{"new_owner_id": e.ownerID})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 owner 转让应 403,实际 %d %s", rec.Code, rec.Body.String())
	}
	// owner 转让给 other
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/spaces/"+id+"/transfer", e.owner,
		map[string]any{"new_owner_id": e.otherID})
	if rec.Code != http.StatusOK {
		t.Fatalf("转让应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	// 新 owner 现在可以退出?不 —— 他成了 owner,退出仍应 409;但可以解散
	rec = doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/spaces/"+id, e.owner, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("原 owner 转让后不应还能解散,实际 %d %s", rec.Code, rec.Body.String())
	}
	// 空间为空(只有根目录)→ 解散应成功
	rec = doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/spaces/"+id, e.other, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("新 owner 解散应 200,实际 %d %s", rec.Code, rec.Body.String())
	}
	// 解散后读应 410(空间已不存在,而"曾经存在"这一点对客户端有意义)
	rec = doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files?space="+id, e.other, nil)
	if rec.Code != http.StatusGone && rec.Code != http.StatusForbidden {
		t.Fatalf("解散后应 410/403,实际 %d", rec.Code)
	}
}

// 未登录一律 401(协作接口不是匿名入口)。
func TestSpaceCollabRequiresAuth(t *testing.T) {
	e := setupCollab(t)
	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/spaces"},
		{http.MethodPost, "/api/v1/spaces"},
		{http.MethodDelete, "/api/v1/spaces/01a00000-0000-7000-8000-000000000000"},
		{http.MethodPost, "/api/v1/spaces/01a00000-0000-7000-8000-000000000000/transfer"},
	} {
		rec := doJSONReq(t, e.handler, c.method, c.path, "", map[string]any{})
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录应 401,实际 %d", c.method, c.path, rec.Code)
		}
	}
}

// containsHint 判字符串包含(小工具,避免为一句断言引入 strings 依赖混淆)。
func containsHint(h, sub string) bool {
	for i := 0; i+len(sub) <= len(h); i++ {
		if h[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
