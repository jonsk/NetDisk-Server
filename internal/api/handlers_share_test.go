package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/netdisk/netdisk/internal/model"
)

// 共享到空间(BE-S6-03)与复制(BE-S7-03)的 HTTP 层契约测试。
//
// 这两条路径在服务层的核心是"引用 +1 而非复制内容",HTTP 层要保证的是
// 契约与可观测性:201(新建了条目)、结构化冲突码、以及"复制了多少行 /
// 复用了多少对象"这几个客户端要用来提示用户的数字。

// mkFileWithObject 造一个"文件行 + 可用对象行"的文件。
//
// 与 handlers_file_test.go 里的 mkFile 的差别:**必须有 file_objects 行** ——
// 共享/复制是"引用 +1"路径,没有对象行会被正当拒绝(那正是我们要防的悬空引用)。
func (e *fileEnv) mkFileWithObject(t *testing.T, name string, data []byte) (fileID, hash string) {
	t.Helper()
	ctx := context.Background()
	hash = fmt.Sprintf("%064x", len(name)*7+len(data)*13+int(time.Now().UnixNano()%100000))
	if _, err := e.dbase.Pool.Exec(ctx, `
INSERT INTO file_objects (hash_sha256, size, storage_backend, object_key, ref_count, state)
VALUES ($1, $2, 'fs', $3, 1, 'live')`, hash, len(data), "objects/"+hash[:2]+"/"+hash); err != nil {
		t.Fatalf("造对象行失败: %v", err)
	}
	if err := e.dbase.Pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, version, depth, hash_sha256)
VALUES ($1, $2, $3, $4, false, $5, 1, 1, $6) RETURNING id`,
		e.spaceID, e.rootID, e.userID, name, len(data), hash).Scan(&fileID); err != nil {
		t.Fatalf("造文件行失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.dbase.Pool.Exec(bg, `DELETE FROM file_objects WHERE hash_sha256 = $1`, hash)
	})
	return fileID, hash
}

// mkTeamSpaceFor 建一个属于 e.userID 的团队空间(owner = manager,可写)。
func (e *fileEnv) mkTeamSpaceFor(t *testing.T, name string) (spaceID, rootID string) {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("150405.000000000")
	var groupID string
	if err := e.dbase.Pool.QueryRow(ctx,
		`INSERT INTO groups (name, owner_id) VALUES ($1, $2) RETURNING id`,
		"fh_grp_"+suffix, e.userID).Scan(&groupID); err != nil {
		t.Fatalf("建组失败: %v", err)
	}
	if err := e.dbase.Pool.QueryRow(ctx, `
INSERT INTO spaces (kind, group_id, owner_id, name, quota_bytes)
VALUES ('team', $1, $2, $3, 0) RETURNING id`, groupID, e.userID, name).Scan(&spaceID); err != nil {
		t.Fatalf("建团队空间失败: %v", err)
	}
	if err := e.dbase.Pool.QueryRow(ctx,
		`INSERT INTO files (space_id, owner_id, name, is_dir, depth) VALUES ($1,$2,'',true,0) RETURNING id`,
		spaceID, e.userID).Scan(&rootID); err != nil {
		t.Fatalf("建团队根目录失败: %v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = e.dbase.Pool.Exec(bg, `DELETE FROM files WHERE space_id = $1`, spaceID)
		_, _ = e.dbase.Pool.Exec(bg, `DELETE FROM spaces WHERE id = $1`, spaceID)
		_, _ = e.dbase.Pool.Exec(bg, `DELETE FROM groups WHERE id = $1`, groupID)
	})
	return spaceID, rootID
}

// 未认证 → 401(权限判断只在服务端,但接口首先要求登录)
func TestFileShareAndCopyRequireAuth(t *testing.T) {
	e := setupFileEnv(t)
	id, _ := e.mkFileWithObject(t, "a.txt", []byte("payload"))
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/files/" + id + "/share-to-space"},
		{http.MethodPost, "/api/v1/files/" + id + "/copy"},
	} {
		rec := doJSONReq(t, e.handler, c.method, c.path, "", map[string]any{})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s 无令牌应 401,实际 %d", c.method, c.path, rec.Code)
		}
	}
}

// **契约**:共享成功 → 201,响应体是新条目视图,且库里引用变成 2
func TestFileShareHTTPContract(t *testing.T) {
	e := setupFileEnv(t)
	data := []byte("share-over-http-payload")
	id, hash := e.mkFileWithObject(t, "合同.pdf", data)
	teamID, teamRoot := e.mkTeamSpaceFor(t, "共享接口空间")

	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/share-to-space", e.token,
		map[string]any{"target_space_id": teamID})
	if rec.Code != http.StatusCreated {
		t.Fatalf("共享应 201(新建了条目),实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	for _, k := range []string{"id", "parent_id", "name", "is_dir", "size", "hash_sha256", "version", "etag"} {
		if _, ok := body[k]; !ok {
			t.Errorf("响应缺少字段 %q: %s", k, rec.Body.String())
		}
	}
	if body["name"] != "合同.pdf" {
		t.Errorf("应沿用原名,实际 %v", body["name"])
	}
	if body["parent_id"] != teamRoot {
		t.Errorf("应落在目标空间根目录,实际 %v", body["parent_id"])
	}
	if body["hash_sha256"] != hash {
		t.Errorf("应指向同一内容 hash,实际 %v", body["hash_sha256"])
	}
	if body["id"] == id {
		t.Error("共享必须返回新条目 id")
	}
	// 引用 +1,内容不复制
	var ref int
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT ref_count FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&ref); err != nil {
		t.Fatalf("读引用失败: %v", err)
	}
	if ref != 2 {
		t.Fatalf("共享后引用应为 2,实际 %d", ref)
	}
}

// 缺少目标空间 → 400 invalid_argument(而不是 500)
func TestFileShareMissingTargetSpaceIs400(t *testing.T) {
	e := setupFileEnv(t)
	id, _ := e.mkFileWithObject(t, "x.txt", []byte("x"))
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/share-to-space", e.token,
		map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少目标空间应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "invalid_argument" {
		t.Errorf("业务码应为 invalid_argument,实际 %v", body["code"])
	}
}

// 目标空间下同名 → 409 + name_conflict(客户端据此提示"换个名字")
func TestFileShareNameConflictIs409(t *testing.T) {
	e := setupFileEnv(t)
	id, _ := e.mkFileWithObject(t, "dup.txt", []byte("mine"))
	teamID, teamRoot := e.mkTeamSpaceFor(t, "接口重名空间")
	if _, err := e.dbase.Pool.Exec(context.Background(), `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, depth)
VALUES ($1,$2,$3,'dup.txt',false,1,1)`, teamID, teamRoot, e.userID); err != nil {
		t.Fatalf("造同名条目失败: %v", err)
	}
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/share-to-space", e.token,
		map[string]any{"target_space_id": teamID})
	if rec.Code != http.StatusConflict {
		t.Fatalf("同名应 409,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "name_conflict" {
		t.Errorf("业务码应为 name_conflict,实际 %v", body["code"])
	}
	details, _ := body["details"].(map[string]any)
	if details["reason"] != "name_conflict" {
		t.Errorf("details.reason 应为 name_conflict,实际 %v", details["reason"])
	}
}

// **契约**:复制成功 → 201,带上"新建行数 / 复用对象数 / 新增逻辑字节"
func TestFileCopyHTTPContract(t *testing.T) {
	e := setupFileEnv(t)
	data := []byte("copy-over-http-payload")
	id, hash := e.mkFileWithObject(t, "原始.txt", data)

	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/copy", e.token,
		map[string]any{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("复制应 201,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	for _, k := range []string{"root", "copied_files", "copied_dirs", "object_refs_added", "size_bytes"} {
		if _, ok := body[k]; !ok {
			t.Errorf("响应缺少字段 %q: %s", k, rec.Body.String())
		}
	}
	if body["copied_files"].(float64) != 1 || body["copied_dirs"].(float64) != 0 {
		t.Errorf("应复制 1 文件 0 目录,实际 %v/%v", body["copied_files"], body["copied_dirs"])
	}
	if body["object_refs_added"].(float64) != 1 {
		t.Errorf("应复用 1 个对象,实际 %v", body["object_refs_added"])
	}
	if body["size_bytes"].(float64) != float64(len(data)) {
		t.Errorf("新增逻辑字节应为 %d,实际 %v", len(data), body["size_bytes"])
	}
	root := body["root"].(map[string]any)
	if root["name"] != "原始.txt (副本)" {
		t.Errorf("未指定名字应自动生成 (副本),实际 %v", root["name"])
	}
	if root["hash_sha256"] != hash {
		t.Errorf("副本应指向同一对象,实际 %v", root["hash_sha256"])
	}
}

// 目标目录不属于本空间/不存在 → 400/404,而不是 500
func TestFileCopyBadTarget(t *testing.T) {
	e := setupFileEnv(t)
	id, _ := e.mkFileWithObject(t, "t.txt", []byte("t"))
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/copy", e.token,
		map[string]any{"target_parent_id": "00000000-0000-7000-8000-000000000000"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("目标目录不存在应 404,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 源不存在 → 404(共享与复制都要)
func TestFileShareCopySourceMissingIs404(t *testing.T) {
	e := setupFileEnv(t)
	const ghost = "00000000-0000-7000-8000-000000000000"
	// 请求体字段按端点区分:解码器开启了 DisallowUnknownFields,
	// 给 /copy 送 target_space_id 会先被 400 拦下(那样就测不到 404 了)。
	cases := []struct {
		path string
		body map[string]any
	}{
		{"/share-to-space", map[string]any{"target_space_id": ghost}},
		{"/copy", map[string]any{}},
	}
	for _, c := range cases {
		rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+ghost+c.path, e.token, c.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST %s 源不存在应 404,实际 %d body=%s", c.path, rec.Code, rec.Body.String())
		}
	}
}

// 方法契约:这两个端点只接受 POST → 用 GET 应 405 + Allow(而不是 404)
func TestFileShareCopyMethodContract(t *testing.T) {
	e := setupFileEnv(t)
	id, _ := e.mkFileWithObject(t, "m.txt", []byte("m"))
	for _, p := range []string{"/share-to-space", "/copy"} {
		rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/files/"+id+p, e.token, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s 应 405,实际 %d", p, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow == "" {
			t.Errorf("GET %s 的 405 必须带 Allow 头", p)
		}
	}
}

// 目录共享被拒(目录走复制)→ 400
func TestFileShareDirIs400OverHTTP(t *testing.T) {
	e := setupFileEnv(t)
	ctx := context.Background()
	var dirID string
	if err := e.dbase.Pool.QueryRow(ctx, `
INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, depth)
VALUES ($1,$2,$3,'dir',true,1) RETURNING id`, e.spaceID, e.rootID, e.userID).Scan(&dirID); err != nil {
		t.Fatalf("造目录失败: %v", err)
	}
	teamID, _ := e.mkTeamSpaceFor(t, "目录共享接口空间")
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+dirID+"/share-to-space", e.token,
		map[string]any{"target_space_id": teamID})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("共享目录应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 只读成员对目标空间的共享必须被拒(403/410),且**不进**任何数据
func TestFileShareIntoReadOnlySpaceRejected(t *testing.T) {
	e := setupFileEnv(t)
	id, hash := e.mkFileWithObject(t, "ro.txt", []byte("ro"))
	// 让 e.userID 只以 reader 身份加入目标空间(owner 是另一个人)
	other := setupFileEnv(t)
	teamID, _ := other.mkTeamSpaceFor(t, "只读目标空间")
	if _, err := e.dbase.Pool.Exec(context.Background(), `
INSERT INTO space_members (space_id, user_id, permission) VALUES ($1,$2,$3)`,
		teamID, e.userID, model.PermReader); err != nil {
		t.Fatalf("加只读成员失败: %v", err)
	}
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/share-to-space", e.token,
		map[string]any{"target_space_id": teamID})
	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		t.Fatalf("只读成员共享进目标空间应被拒,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var ref int
	_ = e.dbase.Pool.QueryRow(context.Background(),
		`SELECT ref_count FROM file_objects WHERE hash_sha256 = $1`, hash).Scan(&ref)
	if ref != 1 {
		t.Fatalf("被拒时不应加引用,实际 %d", ref)
	}
}
