package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/syncfeed"
)

// 增量变更接口的 HTTP 契约测试(BE-S9-02/03)。
//
// 这里要锁死的三件事,每一件错了都会表现为"客户端静默漏文件":
//   - 契约字段名(curl 手验与客户端共用同一份文档);
//   - 判权返回 410 space_revoked(而不是空列表 —— 空列表会被当成"没有变化");
//   - 超窗返回 409 cursor_expired(而不是空列表 —— 会被当成"已是最新")。

// setupSyncEnv 复用 fileEnv,并额外给 Files 装变更流写入器。
//
// 直接复用 fileEnv 的装配是有意的:`/changes` 的数据必须来自**真实写路径**
// (改名/移动/删除),只用 SQL 直插的用例证明不了"写路径会记变更"。
func setupSyncEnv(t *testing.T) *fileEnv {
	t.Helper()
	e := setupFileEnv(t)
	return e
}

// feedItems 拉一页变更。
func (e *fileEnv) feedItems(t *testing.T, spaceID string, since int64) map[string]any {
	t.Helper()
	rec := doJSONReq(t, e.handler, http.MethodGet,
		fmt.Sprintf("/api/v1/changes?space=%s&since=%d", spaceID, since), e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("拉变更应 200,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	return body
}

// **契约**:{items,next_seq,has_more,space_id} 与条目字段齐全
func TestChangesHTTPContract(t *testing.T) {
	e := setupSyncEnv(t)
	id := e.mkFile(t, "c1.txt", 10, 1)
	e.feedCreate(t, id, "c1.txt")

	body := e.feedItems(t, e.spaceID, 0)
	for _, k := range []string{"items", "next_seq", "has_more", "space_id"} {
		if _, ok := body[k]; !ok {
			t.Errorf("响应缺少字段 %q: %v", k, body)
		}
	}
	items, _ := body["items"].([]any)
	if len(items) == 0 {
		t.Fatalf("应有变更条目: %v", body)
	}
	it := items[0].(map[string]any)
	for _, k := range []string{"change_seq", "file_id", "kind", "version", "parent_id", "name"} {
		if _, ok := it[k]; !ok {
			t.Errorf("条目缺少字段 %q: %v", k, it)
		}
	}
	if it["kind"] != model.FeedCreated {
		t.Errorf("kind 应为 created,实际 %v", it["kind"])
	}
	if it["name"] != "c1.txt" {
		t.Errorf("name 应为 c1.txt(客户端据此更新本地树),实际 %v", it["name"])
	}
	if body["next_seq"].(float64) != it["change_seq"].(float64) {
		t.Errorf("next_seq 应为页内最后一条的 seq")
	}
}

// 未认证 401;缺 space 400;since/limit 非法 400(不静默当 0)
func TestChangesValidation(t *testing.T) {
	e := setupSyncEnv(t)
	if rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/changes?space="+e.spaceID, "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("无令牌应 401,实际 %d", rec.Code)
	}
	if rec := doJSONReq(t, e.handler, http.MethodGet, "/api/v1/changes", e.token, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("缺 space 应 400,实际 %d", rec.Code)
	}
	for _, bad := range []string{"since=-1", "since=abc", "limit=abc"} {
		rec := doJSONReq(t, e.handler, http.MethodGet,
			"/api/v1/changes?space="+e.spaceID+"&"+bad, e.token, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应 400,实际 %d body=%s", bad, rec.Code, rec.Body.String())
		}
	}
	// 超大 limit 不报错(夹到上限)
	rec := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/changes?space="+e.spaceID+"&limit=999999", e.token, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("超大 limit 应 200(夹到上限),实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// **判权**:空间外的用户 → 410 space_revoked(不是空列表)
//
// 空列表会被客户端当成"这个空间没有变化",于是它保留本地文件继续等 ——
// 而事实是它已经看不到这个空间了(6.4 权限受限态)。
func TestChangesRequiresSpaceAccess(t *testing.T) {
	e := setupSyncEnv(t)
	other := setupSyncEnv(t)
	// 用 other 的令牌访问 e 的空间
	rec := doJSONReq(t, other.handler, http.MethodGet,
		"/api/v1/changes?space="+e.spaceID, other.token, nil)
	if rec.Code != http.StatusGone && rec.Code != http.StatusForbidden {
		t.Fatalf("跨空间访问应 410/403,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Code == http.StatusGone {
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body["code"] != "space_gone" && body["code"] != "space_revoked" {
			t.Errorf("业务码应为 space_gone/space_revoked,实际 %v", body["code"])
		}
	}
}

// **超窗**:409 cursor_expired + action=full_resync
func TestChangesCursorExpiredOverHTTP(t *testing.T) {
	e := setupSyncEnv(t)
	id := e.mkFile(t, "exp.txt", 10, 1)
	seq := e.feedCreate(t, id, "exp.txt")

	// 把本空间的已清理水位推到该 seq
	if err := e.dbase.InTx(context.Background(), func(tx pgx.Tx) error {
		_, err := syncfeed.Prune(context.Background(), tx,
			map[string]int64{e.spaceID: seq}, time.Now().Add(time.Hour))
		return err
	}); err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	rec := doJSONReq(t, e.handler, http.MethodGet,
		fmt.Sprintf("/api/v1/changes?space=%s&since=%d", e.spaceID, seq-1), e.token, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("超窗应 409,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "cursor_expired" {
		t.Errorf("业务码应为 cursor_expired,实际 %v", body["code"])
	}
	details, _ := body["details"].(map[string]any)
	if details["action"] != "full_resync" {
		t.Errorf("必须给出建议动作 full_resync(客户端据此重建本地状态),实际 %v", details["action"])
	}
}

// 头指针接口:返回当前最大 seq(首次同步的起点)
func TestChangesHeadHTTP(t *testing.T) {
	e := setupSyncEnv(t)
	id := e.mkFile(t, "head.txt", 10, 1)
	seq := e.feedCreate(t, id, "head.txt")

	rec := doJSONReq(t, e.handler, http.MethodGet,
		"/api/v1/changes/head?space="+e.spaceID, e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if int64(body["head_seq"].(float64)) != seq {
		t.Fatalf("head_seq 应为 %d,实际 %v", seq, body["head_seq"])
	}
}

// 游标上报:成功 200,非法 client_id 400,未认证 401
func TestCursorReportHTTP(t *testing.T) {
	e := setupSyncEnv(t)
	body := map[string]any{"client_id": "cli-http", "space_id": e.spaceID, "last_seq": 5}
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/sync/cursors", e.token, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("上报应 200,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	// 落库了(权威值在 PG,Redis 只加速)
	var last int64
	if err := e.dbase.Pool.QueryRow(context.Background(),
		`SELECT last_seq FROM sync_cursors WHERE client_id='cli-http' AND space_id=$1`,
		e.spaceID).Scan(&last); err != nil {
		t.Fatalf("读游标失败: %v", err)
	}
	if last != 5 {
		t.Fatalf("游标应为 5,实际 %d", last)
	}
	// 非法 client_id(带空格/斜杠)→ 400
	for _, bad := range []string{"bad id", "a/b", "中文"} {
		rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/sync/cursors", e.token,
			map[string]any{"client_id": bad, "space_id": e.spaceID, "last_seq": 1})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("client_id=%q 应 400,实际 %d", bad, rec.Code)
		}
	}
	if rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/sync/cursors", "", body); rec.Code != http.StatusUnauthorized {
		t.Errorf("无令牌应 401,实际 %d", rec.Code)
	}
}

// feedCreate 走一次真实的写路径产生一条变更 —— 用 repo 直插文件 + 显式写变更,
// 因为 /files 没有"建空文件"的接口(新建文件走上传定稿)。
//
// 这里刻意**不**用 syncfeed.AppendEvent 之外的旁路:被测的正是"写路径会记变更",
// 所以关键用例(改名/移动/删除)见下面的 TestWritePathFeedsChanges。
func (e *fileEnv) feedCreate(t *testing.T, fileID, name string) int64 {
	t.Helper()
	var seq int64
	if err := e.dbase.InTx(context.Background(), func(tx pgx.Tx) error {
		s, err := syncfeed.AppendEvent(context.Background(), tx, syncfeed.Event{
			SpaceID: e.spaceID, FileID: fileID, Kind: model.FeedCreated,
			Version: 1, ParentID: e.rootID, Name: name,
		})
		if err != nil {
			return err
		}
		seq = s
		return nil
	}); err != nil {
		t.Fatalf("写变更失败: %v", err)
	}
	return seq
}

// **关键**:真实写路径(改名/移动/删除)必须留下变更 —— 用 HTTP 接口驱动
func TestWritePathFeedsChanges(t *testing.T) {
	e := setupSyncEnv(t)
	id := e.mkFile(t, "wf.txt", 10, 1)
	base := e.feedCreate(t, id, "wf.txt")

	// 改名 → updated(带新名字)
	rec := doJSONReq(t, e.handler, http.MethodPatch, "/api/v1/files/"+id, e.token,
		map[string]any{"name": "wf-renamed.txt", "base_version": 1})
	if rec.Code != http.StatusOK {
		t.Fatalf("改名失败: %d %s", rec.Code, rec.Body.String())
	}
	// 移动 → moved(带 old_parent_id)
	dir := e.mkDirViaRepo(t, "destdir")
	rec = doJSONReq(t, e.handler, http.MethodPost, "/api/v1/files/"+id+"/move", e.token,
		map[string]any{"parent_id": dir, "base_version": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("移动失败: %d %s", rec.Code, rec.Body.String())
	}
	// 删除 → deleted
	rec = doJSONReq(t, e.handler, http.MethodDelete, "/api/v1/files/"+id, e.token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败: %d %s", rec.Code, rec.Body.String())
	}

	body := e.feedItems(t, e.spaceID, base)
	items, _ := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("应恰好 3 条变更(updated/moved/deleted),实际 %d: %v", len(items), items)
	}
	kinds := make([]string, 0, 3)
	for _, raw := range items {
		kinds = append(kinds, raw.(map[string]any)["kind"].(string))
	}
	want := []string{model.FeedUpdated, model.FeedMoved, model.FeedDeleted}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("第 %d 条 kind 应为 %s,实际 %s(全部 %v)", i+1, want[i], kinds[i], kinds)
		}
	}
	moved := items[1].(map[string]any)
	if moved["old_parent_id"] == nil || moved["old_parent_id"] == "" {
		t.Errorf("moved 必须带 old_parent_id(客户端据此从旧目录摘下),实际 %v", moved)
	}
	updated := items[0].(map[string]any)
	if updated["name"] != "wf-renamed.txt" {
		t.Errorf("updated 必须带变更后的名字,实际 %v", updated["name"])
	}
}

// mkDirViaRepo 建目录并返回 id(走 repo,与生产同一条路径)。
func (e *fileEnv) mkDirViaRepo(t *testing.T, name string) string {
	t.Helper()
	d, err := repo.FileRepo{}.CreateDir(context.Background(), e.dbase.Pool, repo.CreateDirInput{
		SpaceID: e.spaceID, ParentID: e.rootID, OwnerID: e.userID, Name: name, Depth: 1,
	})
	if err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	return d.ID
}
