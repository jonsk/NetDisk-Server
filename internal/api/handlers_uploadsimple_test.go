package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"testing"
)

// mustCtx 返回一个可复用的 context(测试里只需要"不是 nil")。
func mustCtx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// H5 multipart 直传入口(BE-S5-05)的 HTTP 契约测试。
//
// **这组用例是补的,并且它抓到了一个真实缺陷**:上一版 handler 先
// `decodeJSON(r.Body)` 再 `r.MultipartReader()` —— 也就是说它把 multipart 体
// 当 JSON 解,这条入口**从来不可能工作**。之所以一直没被发现:
// ①它当时没有任何测试;②唯一的调用者(端到端探针)还没写到这里。
// 现在把 multipart 的"元数据段 + 文件段"形状固定成契约并用例锁住。

// multipartBody 构造 `metadata 段 + 文件段` 的请求体。
func multipartBody(t *testing.T, meta any, fieldName, fileName string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	raw, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("序列化元数据失败: %v", err)
	}
	if err := mw.WriteField("metadata", string(raw)); err != nil {
		t.Fatalf("写元数据段失败: %v", err)
	}
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fieldName, fileName))
	hdr.Set("Content-Type", "application/octet-stream")
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("建文件段失败: %v", err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatalf("写文件段失败: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("收尾 multipart 失败: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

// postMultipart 发一次 multipart 请求。
func (e *fileEnv) postMultipart(t *testing.T, path string, body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+e.token)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

// **契约**:元数据段在前 + 文件段 → 201,且内容真的落库(版本 1、大小一致)
func TestUploadSimpleMultipartSucceeds(t *testing.T) {
	e := setupFileEnv(t)
	data := []byte("multipart-payload-for-simple-upload")

	body, ct := multipartBody(t, map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "h5.txt", "size": len(data),
	}, "file", "h5.txt", data)

	rec := e.postMultipart(t, "/api/v1/upload/simple", body, ct)
	if rec.Code != http.StatusCreated {
		t.Fatalf("multipart 直传应 201,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应应为 JSON: %v", err)
	}
	file, _ := resp["file"].(map[string]any)
	if file["name"] != "h5.txt" {
		t.Errorf("应落原名,实际 %v", file["name"])
	}
	if int64(file["size"].(float64)) != int64(len(data)) {
		t.Errorf("大小应为 %d,实际 %v", len(data), file["size"])
	}
	if resp["upload_id"] == nil || resp["upload_id"] == "" {
		t.Error("响应必须带 upload_id(便于排障与幂等)")
	}
	// 文件行真的在库里
	var n int
	if err := e.dbase.Pool.QueryRow(mustCtx(t),
		`SELECT count(*) FROM files WHERE space_id=$1 AND name='h5.txt'`, e.spaceID).Scan(&n); err != nil {
		t.Fatalf("查文件行失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应有 1 行,实际 %d", n)
	}
}

// 元数据段**在文件段之后** → 400 且明确说明顺序契约
//
// 不支持"文件在前"是刻意的:文件是流式交给定稿链路的(可能几百 MB),
// 想在读到文件后再拿元数据就必须先把它缓存到临时文件 —— 为容忍字段顺序
// 付出一次全量磁盘往返。契约显式拒绝更诚实。
func TestUploadSimpleRejectsFileBeforeMetadata(t *testing.T) {
	e := setupFileEnv(t)
	data := []byte("payload")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="late.txt"`)
	hdr.Set("Content-Type", "application/octet-stream")
	part, _ := mw.CreatePart(hdr)
	_, _ = part.Write(data)
	raw, _ := json.Marshal(map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "late.txt", "size": len(data),
	})
	_ = mw.WriteField("metadata", string(raw))
	_ = mw.Close()

	rec := e.postMultipart(t, "/api/v1/upload/simple", &buf, mw.FormDataContentType())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("文件段在前应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "invalid_argument" {
		t.Errorf("业务码应为 invalid_argument,实际 %v", body["code"])
	}
}

// 非 multipart 请求 → 400(而不是 500)
func TestUploadSimpleRejectsNonMultipart(t *testing.T) {
	e := setupFileEnv(t)
	rec := doJSONReq(t, e.handler, http.MethodPost, "/api/v1/upload/simple", e.token,
		map[string]any{"name": "x.txt", "size": 1})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非 multipart 应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 声明大小超过 H5 上限 → 413(在**读 body 之前**就拒,由 Content-Length 判定)
func TestUploadSimpleRejectsOversize(t *testing.T) {
	e := setupFileEnv(t)
	// 构造一个"声明 200MB"的元数据;body 很小,但 Content-Length 检查先命中
	body, ct := multipartBody(t, map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "big.bin", "size": 200 << 20,
	}, "file", "big.bin", []byte("small"))

	rec := e.postMultipart(t, "/api/v1/upload/simple", body, ct)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超上限应 413,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 元数据段缺 name → 400
func TestUploadSimpleRequiresName(t *testing.T) {
	e := setupFileEnv(t)
	body, ct := multipartBody(t, map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "size": 3,
	}, "file", "noname.bin", []byte("abc"))
	rec := e.postMultipart(t, "/api/v1/upload/simple", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 name 应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 元数据段有未知字段 → 400(与 JSON body 同一套严格解码规则)
func TestUploadSimpleRejectsUnknownMetadataField(t *testing.T) {
	e := setupFileEnv(t)
	body, ct := multipartBody(t, map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "strict.txt",
		"size": 3, "bogus_field": "x",
	}, "file", "strict.txt", []byte("abc"))
	rec := e.postMultipart(t, "/api/v1/upload/simple", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知字段应 400(与 JSON body 同规则),实际 %d body=%s", rec.Code, rec.Body.String())
	}
}

// 实际字节数与声明不符 → 400,且**不留**文件行(避免"库里说有、内容却是半截")
func TestUploadSimpleRejectsSizeMismatch(t *testing.T) {
	e := setupFileEnv(t)
	body, ct := multipartBody(t, map[string]any{
		"space_id": e.spaceID, "parent_id": e.rootID, "name": "short.txt", "size": 100,
	}, "file", "short.txt", []byte("only-10byt"))

	rec := e.postMultipart(t, "/api/v1/upload/simple", body, ct)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("字节数不符应 400,实际 %d body=%s", rec.Code, rec.Body.String())
	}
	var n int
	if err := e.dbase.Pool.QueryRow(mustCtx(t),
		`SELECT count(*) FROM files WHERE space_id=$1 AND name='short.txt'`, e.spaceID).Scan(&n); err != nil {
		t.Fatalf("查行失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("失败不应留下文件行,实际 %d", n)
	}
}

// 未认证 → 401
func TestUploadSimpleRequiresAuth(t *testing.T) {
	e := setupFileEnv(t)
	body, ct := multipartBody(t, map[string]any{"name": "a.txt", "size": 1}, "file", "a.txt", []byte("a"))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upload/simple", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无令牌应 401,实际 %d", rec.Code)
	}
}
