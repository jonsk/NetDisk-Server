// Command probesmoke 是本机开发期的端到端探针:
// 用真实 HTTP 走向 netdisk 服务,验证"登录 → 建上传任务 → HEAD 续传进度 → 取消"整条链路。
//
// 为什么用 Go 而不是 curl:本机 curl 的 schannel TLS 初始化失败(SEC_E_NO_CREDENTIALS),
// 而 net/http 走的是 Go 自带 TLS 栈,探测结果可靠。
//
// 用法:go run ./cmd/probesmoke -base http://127.0.0.1:8080 -user admin -pass '...'
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "服务基础地址")
	user := flag.String("user", "admin", "登录名")
	pass := flag.String("pass", "", "密码(必填)")
	flag.Parse()
	if *pass == "" {
		fmt.Fprintln(os.Stderr, "缺少 -pass")
		os.Exit(2)
	}

	c := &http.Client{Timeout: 15 * time.Second}
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
		os.Exit(1)
	}

	// 1) 健康检查
	var health map[string]any
	doJSON(c, http.MethodGet, *base+"/healthz", nil, "", &health)
	fmt.Printf("1) /healthz            -> %v\n", health["status"])

	// 2) 登录
	var login struct {
		Token struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			TokenType    string `json:"token_type"`
			ExpiresIn    int64  `json:"expires_in"`
			Audience     string `json:"audience"`
		} `json:"token"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		PersonalSpace struct {
			ID string `json:"id"`
		} `json:"personal_space"`
	}
	doJSON(c, http.MethodPost, *base+"/api/v1/auth/login",
		map[string]any{"login": *user, "password": *pass, "audience": "desktop"}, "", &login)
	if login.Token.AccessToken == "" {
		fail("登录未拿到 access_token")
	}
	access := login.Token.AccessToken
	fmt.Printf("2) 登录                -> user=%s space=%s aud=%s expires_in=%ds access=%d 字符\n",
		login.User.ID, login.PersonalSpace.ID, login.Token.Audience, login.Token.ExpiresIn, len(access))

	// 3) 建上传任务
	var created struct {
		UploadID   string `json:"upload_id"`
		Ticket     string `json:"upload_ticket"`
		ExpiresAt  string `json:"expires_at"`
		QuotaAfter int64  `json:"quota_after_bytes"`
	}
	name := fmt.Sprintf("探针-%d.bin", time.Now().Unix())
	doJSON(c, http.MethodPost, *base+"/api/v1/upload/create",
		map[string]any{"name": name, "size": 12345, "hash": ""}, access, &created)
	if created.UploadID == "" || created.Ticket == "" {
		fail("建任务未返回 upload_id/ticket")
	}
	fmt.Printf("3) 建上传任务          -> upload=%s 过期=%s 剩余额度=%d\n",
		created.UploadID, created.ExpiresAt, created.QuotaAfter)

	// 4) HEAD 取续传偏移(ticket 走 X-Upload-Token)
	req, _ := http.NewRequest(http.MethodHead, *base+"/api/v1/upload/"+created.UploadID, nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("X-Upload-Token", created.Ticket)
	resp, err := c.Do(req)
	if err != nil {
		fail("HEAD 请求失败: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail("HEAD 应 200,实际 %d", resp.StatusCode)
	}
	fmt.Printf("4) HEAD 续传进度       -> offset=%s length=%s tus=%s\n",
		resp.Header.Get("Upload-Offset"), resp.Header.Get("Upload-Length"), resp.Header.Get("Tus-Resumable"))

	// 5) 无 ticket 必须被拒
	req2, _ := http.NewRequest(http.MethodHead, *base+"/api/v1/upload/"+created.UploadID, nil)
	req2.Header.Set("Authorization", "Bearer "+access)
	resp2, err := c.Do(req2)
	if err != nil {
		fail("无 ticket HEAD 请求失败: %v", err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		fail("无 ticket 的 HEAD 应 401,实际 %d", resp2.StatusCode)
	}
	fmt.Printf("5) 无 ticket HEAD      -> %d(符合预期)\n", resp2.StatusCode)

	// 6) 取消任务(额度即时释放)
	req3, _ := http.NewRequest(http.MethodDelete, *base+"/api/v1/upload/"+created.UploadID, nil)
	req3.Header.Set("Authorization", "Bearer "+access)
	resp3, err := c.Do(req3)
	if err != nil {
		fail("取消请求失败: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusNoContent {
		fail("取消应 204,实际 %d", resp3.StatusCode)
	}
	fmt.Printf("6) 取消上传任务        -> %d\n", resp3.StatusCode)

	// 7) WebDAV 能力探测(免认证)
	req4, _ := http.NewRequest(http.MethodOptions, *base+"/webdav", nil)
	resp4, err := c.Do(req4)
	if err != nil {
		fail("WebDAV OPTIONS 失败: %v", err)
	}
	_ = resp4.Body.Close()
	if resp4.StatusCode != http.StatusOK {
		fail("匿名 OPTIONS /webdav 应 200,实际 %d", resp4.StatusCode)
	}
	fmt.Printf("7) WebDAV OPTIONS      -> %d DAV=%q\n", resp4.StatusCode, resp4.Header.Get("DAV"))

	// 8) WebDAV 匿名访问必须 401 + Basic 挑战
	req5, _ := http.NewRequest("PROPFIND", *base+"/webdav/", nil)
	resp5, err := c.Do(req5)
	if err != nil {
		fail("WebDAV 匿名 PROPFIND 失败: %v", err)
	}
	_ = resp5.Body.Close()
	if resp5.StatusCode != http.StatusUnauthorized {
		fail("匿名 PROPFIND 应 401,实际 %d", resp5.StatusCode)
	}
	if !strings.HasPrefix(resp5.Header.Get("WWW-Authenticate"), "Basic ") {
		fail("必须给出 Basic 挑战,实际 %q", resp5.Header.Get("WWW-Authenticate"))
	}
	fmt.Printf("8) WebDAV 匿名         -> %d WWW-Authenticate=%q\n",
		resp5.StatusCode, resp5.Header.Get("WWW-Authenticate"))

	// 9) WebDAV 账密换票:认证通过(**非 401**)并拿到短期令牌
	//
	// 断言方式刻意用"非 401"而不是某个具体状态码:方法集已经实现(BE-S8),
	// 用"501=未实现"当认证通过的标志会随实现推进而失真;方法集本身的行为
	// 由第 42 步覆盖。
	req6, _ := http.NewRequest("PROPFIND", *base+"/webdav/", nil)
	req6.SetBasicAuth(*user, *pass)
	resp6, err := c.Do(req6)
	if err != nil {
		fail("WebDAV 带凭据 PROPFIND 失败: %v", err)
	}
	_ = resp6.Body.Close()
	if resp6.StatusCode == http.StatusUnauthorized {
		fail("凭据正确时不该 401,实际 %d", resp6.StatusCode)
	}
	wdToken := resp6.Header.Get("X-WebDAV-Token")
	if !strings.HasPrefix(wdToken, "wd1_") {
		fail("应回短期 WebDAV 令牌,实际 %q", wdToken)
	}
	fmt.Printf("9) WebDAV 账密换票     -> %d token=%d 字符 有效期=%ss\n",
		resp6.StatusCode, len(wdToken), resp6.Header.Get("X-WebDAV-Token-Expires-In"))

	// 10) 用短期令牌复用会话(口令位置换成令牌)
	req7, _ := http.NewRequest("PROPFIND", *base+"/webdav/", nil)
	req7.SetBasicAuth(*user, wdToken)
	resp7, err := c.Do(req7)
	if err != nil {
		fail("WebDAV 会话令牌请求失败: %v", err)
	}
	_ = resp7.Body.Close()
	if resp7.StatusCode == http.StatusUnauthorized {
		fail("会话令牌应认证通过(非 401),实际 %d", resp7.StatusCode)
	}
	if again := resp7.Header.Get("X-WebDAV-Token"); again != "" {
		fail("命中会话时不应重复签发令牌,实际 %q", again)
	}
	fmt.Printf("10) WebDAV 会话复用    -> %d(未重复签发)\n", resp7.StatusCode)

	// 11) 伪造令牌必须被拒
	req8, _ := http.NewRequest("PROPFIND", *base+"/webdav/", nil)
	req8.SetBasicAuth(*user, "wd1_"+strings.Repeat("A", 43))
	resp8, err := c.Do(req8)
	if err != nil {
		fail("WebDAV 伪造令牌请求失败: %v", err)
	}
	_ = resp8.Body.Close()
	if resp8.StatusCode != http.StatusUnauthorized {
		fail("伪造会话令牌应 401,实际 %d", resp8.StatusCode)
	}
	fmt.Printf("11) WebDAV 伪造令牌    -> %d\n", resp8.StatusCode)

	// 12) 部门树:建三层 + 重建闭包表 + 查子树
	//
	// 注意:必须先换一枚 **web 受众** 的令牌 —— 后台接口要求 web + 管理员角色,
	// 上面那把 desktop 令牌访问它们会被 403 挡住(这正是 2.7 分端要的效果)。
	var webLogin struct {
		Token struct {
			AccessToken string `json:"access_token"`
		} `json:"token"`
	}
	doJSON(c, http.MethodPost, *base+"/api/v1/auth/login",
		map[string]any{"login": *user, "password": *pass, "audience": "web"}, "", &webLogin)
	if webLogin.Token.AccessToken == "" {
		fail("web 受众登录未拿到 access_token")
	}
	adminTok := webLogin.Token.AccessToken

	adminDir := func(method, path string, payload any, tok string) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		rq, err := http.NewRequest(method, *base+path, body)
		if err != nil {
			fail("构造请求失败: %v", err)
		}
		if payload != nil {
			rq.Header.Set("Content-Type", "application/json")
		}
		rq.Header.Set("Authorization", "Bearer "+tok)
		rs, err := c.Do(rq)
		if err != nil {
			fail("请求 %s %s 失败: %v", method, path, err)
		}
		return rs
	}
	createDept := func(name, parent string) string {
		rs := adminDir(http.MethodPost, "/api/v1/admin/departments",
			map[string]any{"name": name, "parent_id": parent}, adminTok)
		raw, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		if rs.StatusCode != http.StatusCreated {
			fail("建部门 %s 失败: %d %s", name, rs.StatusCode, string(raw))
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		id, _ := m["id"].(string)
		return id
	}
	tag := fmt.Sprintf("探针%d", time.Now().UnixNano()%100000)
	deptRoot := createDept(tag+"总部", "")
	deptMid := createDept(tag+"研发", deptRoot)
	deptLeaf := createDept(tag+"平台组", deptMid)

	rs := adminDir(http.MethodPost, "/api/v1/admin/departments/rebuild-closure", nil, adminTok)
	raw, _ := io.ReadAll(rs.Body)
	_ = rs.Body.Close()
	if rs.StatusCode != http.StatusOK {
		fail("重建闭包表失败: %d %s", rs.StatusCode, string(raw))
	}
	var rebuild map[string]any
	_ = json.Unmarshal(raw, &rebuild)
	// 3 级链:1+2+3 = 6 行闭包
	if n, _ := rebuild["closure_rows"].(float64); n != 6 {
		fail("3 级链应有 6 条闭包行,实际 %v", rebuild["closure_rows"])
	}
	fmt.Printf("12) 部门树+重建闭包表 -> 节点=%v 闭包行=%v 最大深度=%v\n",
		rebuild["dept_count"], rebuild["closure_rows"], rebuild["max_depth"])

	rs2 := adminDir(http.MethodGet, "/api/v1/admin/departments/"+deptRoot+"/subtree", nil, adminTok)
	raw2, _ := io.ReadAll(rs2.Body)
	_ = rs2.Body.Close()
	if rs2.StatusCode != http.StatusOK {
		fail("取子树失败: %d %s", rs2.StatusCode, string(raw2))
	}
	var subtree map[string]any
	_ = json.Unmarshal(raw2, &subtree)
	if n, _ := subtree["total"].(float64); n != 3 {
		fail("子树应有 3 个节点,实际 %v", subtree["total"])
	}
	// 叶子相对深度必须是 2(闭包表递归方向写反时这里会是 0)
	for _, n := range subtree["nodes"].([]any) {
		m := n.(map[string]any)
		if m["id"] == deptLeaf && m["relative_depth"].(float64) != 2 {
			fail("叶子相对深度应为 2,实际 %v(闭包表可能漏行)", m["relative_depth"])
		}
	}
	fmt.Printf("13) 部门子树查询       -> 节点=%v(叶子相对深度=2)\n", subtree["total"])

	// 14) 非 web 令牌必须被拒:组织树是权限的输入,不能让桌面/H5 令牌改动
	rs3 := adminDir(http.MethodGet, "/api/v1/admin/departments", nil, access)
	_ = rs3.Body.Close()
	if rs3.StatusCode != http.StatusForbidden {
		fail("desktop 令牌访问部门接口应 403,实际 %d", rs3.StatusCode)
	}
	fmt.Printf("14) desktop 令牌访问部门 -> %d(符合预期)\n", rs3.StatusCode)

	// 15) 清理:删掉探针建的部门(叶子→中间→根;有子节点时删不动)
	for _, id := range []string{deptLeaf, deptMid, deptRoot} {
		rs := adminDir(http.MethodDelete, "/api/v1/admin/departments/"+id, nil, adminTok)
		_ = rs.Body.Close()
		if rs.StatusCode != http.StatusNoContent {
			fail("删除部门 %s 应 204,实际 %d", id[:8], rs.StatusCode)
		}
	}
	fmt.Printf("15) 清理探针部门       -> 3 个已删除\n")

	// 16) 文件列表 + 详情/HEAD 的 ETag 必须三处一致(R-21)
	listResp := adminDir(http.MethodGet, "/api/v1/files", nil, access)
	listRaw, _ := io.ReadAll(listResp.Body)
	_ = listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		fail("文件列表应 200,实际 %d %s", listResp.StatusCode, string(listRaw))
	}
	var listBody struct {
		SpaceID string `json:"space_id"`
		Entries []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			ETag string `json:"etag"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(listRaw, &listBody)
	fmt.Printf("16) 文件列表           -> space=%s 条目=%d\n", listBody.SpaceID[:8], len(listBody.Entries))

	if len(listBody.Entries) > 0 {
		fid := listBody.Entries[0].ID
		getResp := adminDir(http.MethodGet, "/api/v1/files/"+fid, nil, access)
		getRaw, _ := io.ReadAll(getResp.Body)
		_ = getResp.Body.Close()
		headResp := adminDir(http.MethodHead, "/api/v1/files/"+fid, nil, access)
		_ = headResp.Body.Close()

		var detail struct {
			ETag string `json:"etag"`
		}
		_ = json.Unmarshal(getRaw, &detail)
		bodyTag := detail.ETag
		getTag := getResp.Header.Get("ETag")
		headTag := headResp.Header.Get("ETag")
		if bodyTag == "" || bodyTag != getTag || getTag != headTag {
			fail("ETag 三处不一致(R-21): 响应体=%q GET头=%q HEAD头=%q", bodyTag, getTag, headTag)
		}
		fmt.Printf("17) ETag 三处一致      -> %s\n", bodyTag)
	} else {
		fmt.Printf("17) ETag 契约          -> 跳过(个人空间暂无文件)\n")
	}

	// 18) 空间成员接口:个人空间不支持成员管理 → 403
	memResp := adminDir(http.MethodPut, "/api/v1/spaces/"+listBody.SpaceID+"/members",
		map[string]any{"user_id": login.User.ID, "permission": "reader"}, access)
	memRaw, _ := io.ReadAll(memResp.Body)
	_ = memResp.Body.Close()
	if memResp.StatusCode != http.StatusForbidden {
		fail("个人空间加成员应 403,实际 %d %s", memResp.StatusCode, string(memRaw))
	}
	fmt.Printf("18) 个人空间成员管理   -> %d(拒绝,符合预期)\n", memResp.StatusCode)

	// 19) 管理员冻结空间:冻结后成员访问被拒(用 admin 自己的个人空间验证)
	frzResp := adminDir(http.MethodPost, "/api/v1/admin/spaces/"+listBody.SpaceID+"/freeze",
		map[string]any{"frozen": true}, adminTok)
	frzRaw, _ := io.ReadAll(frzResp.Body)
	_ = frzResp.Body.Close()
	if frzResp.StatusCode != http.StatusOK {
		fail("冻结应 200,实际 %d %s", frzResp.StatusCode, string(frzRaw))
	}
	blocked := adminDir(http.MethodGet, "/api/v1/files", nil, access)
	_ = blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		fail("冻结后读应被拒(403/410),实际 %d", blocked.StatusCode)
	}
	// 解冻恢复
	unfrz := adminDir(http.MethodPost, "/api/v1/admin/spaces/"+listBody.SpaceID+"/freeze",
		map[string]any{"frozen": false}, adminTok)
	_ = unfrz.Body.Close()
	if unfrz.StatusCode != http.StatusOK {
		fail("解冻应 200,实际 %d", unfrz.StatusCode)
	}
	restored := adminDir(http.MethodGet, "/api/v1/files", nil, access)
	_ = restored.Body.Close()
	if restored.StatusCode != http.StatusOK {
		fail("解冻后读应 200,实际 %d", restored.StatusCode)
	}
	fmt.Printf("19) 冻结/解冻           -> 冻结后读=%d 解冻后读=%d\n", blocked.StatusCode, restored.StatusCode)

	// 20) TUS 协议端到端:OPTIONS 能力探测 → POST 建任务 → PATCH 分片 →
	//     HEAD 续传偏移 → 传满即定稿 → 列表可见
	tusOpts, err := http.NewRequest(http.MethodOptions, *base+"/tus", nil)
	if err != nil {
		fail("构造 TUS OPTIONS 失败: %v", err)
	}
	optsResp, err := c.Do(tusOpts)
	if err != nil {
		fail("TUS OPTIONS 失败: %v", err)
	}
	_ = optsResp.Body.Close()
	if optsResp.StatusCode != http.StatusNoContent {
		fail("TUS OPTIONS 应 204,实际 %d", optsResp.StatusCode)
	}
	if optsResp.Header.Get("Tus-Resumable") != "1.0.0" {
		fail("TUS OPTIONS 必须声明 Tus-Resumable,实际 %q", optsResp.Header.Get("Tus-Resumable"))
	}
	fmt.Printf("20) TUS OPTIONS        -> %d extensions=%q\n",
		optsResp.StatusCode, optsResp.Header.Get("Tus-Extension"))

	// 内容分两片,验证断点续传
	//
	// 内容里嵌一个**每次运行都不同的**前缀:探针的内容若是固定字节,
	// 同一内容在内容寻址库里会命中**上一次运行留下的同一对象**,
	// 于是"删掉最后一个引用"永远不成立,引用计数的闭环断言会假失败。
	tusName := fmt.Sprintf("tus-%d.bin", time.Now().Unix())
	nonce := fmt.Sprintf("probe-%d-", time.Now().UnixNano())
	head := []byte(nonce)
	part1 := append(head, bytes.Repeat([]byte("A"), 4096-len(head))...)
	part2 := bytes.Repeat([]byte("B"), 2048)
	total := int64(len(part1) + len(part2))
	sum := sha256.Sum256(append(append([]byte{}, part1...), part2...))
	wantHash := hex.EncodeToString(sum[:])

	// POST 建任务(元数据走 TUS 的 base64 形式)
	meta := "filename " + base64.StdEncoding.EncodeToString([]byte(tusName))
	createReq, _ := http.NewRequest(http.MethodPost, *base+"/tus", nil)
	createReq.Header.Set("Tus-Resumable", "1.0.0")
	createReq.Header.Set("Upload-Length", strconv.FormatInt(total, 10))
	createReq.Header.Set("Upload-Metadata", meta)
	createReq.Header.Set("Authorization", "Bearer "+access)
	createResp, err := c.Do(createReq)
	if err != nil {
		fail("TUS 建任务失败: %v", err)
	}
	createRaw, _ := io.ReadAll(createResp.Body)
	_ = createResp.Body.Close()
	if createResp.StatusCode != http.StatusCreated {
		fail("TUS 建任务应 201,实际 %d %s", createResp.StatusCode, string(createRaw))
	}
	var created2 struct {
		UploadID string `json:"upload_id"`
	}
	_ = json.Unmarshal(createRaw, &created2)
	tusTicket := createResp.Header.Get("X-Upload-Token")
	if created2.UploadID == "" || tusTicket == "" {
		fail("TUS 建任务应返回 upload_id 与 X-Upload-Token 头")
	}
	if loc := createResp.Header.Get("Location"); loc != "/tus/"+created2.UploadID {
		fail("Location 应指向 /tus/{id},实际 %q", loc)
	}
	fmt.Printf("21) TUS 建任务         -> %d upload=%s token=%d 字符\n",
		createResp.StatusCode, created2.UploadID[:8], len(tusTicket))

	// PATCH 第一片
	patch := func(offset int64, data []byte) *http.Response {
		rq, _ := http.NewRequest(http.MethodPatch, *base+"/tus/"+created2.UploadID, bytes.NewReader(data))
		rq.Header.Set("Tus-Resumable", "1.0.0")
		rq.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		rq.Header.Set("Content-Type", "application/offset+octet-stream")
		rq.Header.Set("X-Upload-Token", tusTicket)
		rq.Header.Set("Authorization", "Bearer "+access)
		rq.ContentLength = int64(len(data))
		rs, derr := c.Do(rq)
		if derr != nil {
			fail("PATCH 失败: %v", derr)
		}
		return rs
	}

	p1 := patch(0, part1)
	_ = p1.Body.Close()
	if p1.StatusCode != http.StatusNoContent {
		fail("第一片 PATCH 应 204,实际 %d", p1.StatusCode)
	}
	if got := p1.Header.Get("Upload-Offset"); got != strconv.Itoa(len(part1)) {
		fail("第一片后偏移应为 %d,实际 %q", len(part1), got)
	}

	// HEAD 取续传偏移(模拟断线重连)
	headReq, _ := http.NewRequest(http.MethodHead, *base+"/tus/"+created2.UploadID, nil)
	headReq.Header.Set("Tus-Resumable", "1.0.0")
	headReq.Header.Set("X-Upload-Token", tusTicket)
	headReq.Header.Set("Authorization", "Bearer "+access)
	headResp, err := c.Do(headReq)
	if err != nil {
		fail("TUS HEAD 失败: %v", err)
	}
	_ = headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK {
		fail("TUS HEAD 应 200,实际 %d", headResp.StatusCode)
	}
	if got := headResp.Header.Get("Upload-Offset"); got != strconv.Itoa(len(part1)) {
		fail("HEAD 应回已接收 %d 字节,实际 %q", len(part1), got)
	}
	fmt.Printf("22) TUS 分片+断点续传  -> 第一片 204,HEAD offset=%s length=%s\n",
		headResp.Header.Get("Upload-Offset"), headResp.Header.Get("Upload-Length"))

	// 错误偏移必须 409(客户端据此自我纠正而非重传)
	badPatch, _ := http.NewRequest(http.MethodPatch, *base+"/tus/"+created2.UploadID, bytes.NewReader(part1))
	badPatch.Header.Set("Tus-Resumable", "1.0.0")
	badPatch.Header.Set("Upload-Offset", "0") // 重放
	badPatch.Header.Set("Content-Type", "application/offset+octet-stream")
	badPatch.Header.Set("X-Upload-Token", tusTicket)
	badPatch.Header.Set("Authorization", "Bearer "+access)
	badResp, err := c.Do(badPatch)
	if err != nil {
		fail("重放 PATCH 失败: %v", err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusConflict {
		fail("偏移不符应 409,实际 %d", badResp.StatusCode)
	}
	if got := badResp.Header.Get("Upload-Offset"); got != strconv.Itoa(len(part1)) {
		fail("409 应带真实偏移 %d,实际 %q", len(part1), got)
	}
	fmt.Printf("23) TUS 偏移不符       -> %d 真实偏移=%s\n", badResp.StatusCode, badResp.Header.Get("Upload-Offset"))

	// 传满 → 立刻定稿
	p2 := patch(int64(len(part1)), part2)
	_ = p2.Body.Close()
	if p2.StatusCode != http.StatusOK {
		fail("传满后应 200(定稿完成),实际 %d", p2.StatusCode)
	}
	fileID := p2.Header.Get("X-File-Id")
	if fileID == "" || p2.Header.Get("Upload-Complete") != "true" {
		fail("定稿应回 X-File-Id 与 Upload-Complete: true,实际 %q/%q",
			fileID, p2.Header.Get("Upload-Complete"))
	}
	fmt.Printf("24) TUS 写完即定稿     -> %d file=%s version=%s\n",
		p2.StatusCode, fileID[:8], p2.Header.Get("X-File-Version"))

	// 定稿产物必须可读,哈希为服务端实测值
	detResp := adminDir(http.MethodGet, "/api/v1/files/"+fileID, nil, access)
	detRaw, _ := io.ReadAll(detResp.Body)
	_ = detResp.Body.Close()
	if detResp.StatusCode != http.StatusOK {
		fail("定稿后应能读到文件详情,实际 %d %s", detResp.StatusCode, string(detRaw))
	}
	var det struct {
		Hash string `json:"hash_sha256"`
		Size int64  `json:"size"`
		ETag string `json:"etag"`
	}
	_ = json.Unmarshal(detRaw, &det)
	if det.Hash != wantHash {
		fail("落库哈希应为服务端实测值 %s,实际 %s", wantHash, det.Hash)
	}
	if det.Size != total {
		fail("文件大小应为 %d,实际 %d", total, det.Size)
	}
	if det.ETag != detResp.Header.Get("ETag") {
		fail("ETag 三处出口应一致: body=%q header=%q", det.ETag, detResp.Header.Get("ETag"))
	}
	fmt.Printf("25) TUS 定稿产物校验   -> 哈希一致 size=%d etag=%s\n", det.Size, det.ETag)

	// 26) 下载:完整 + Range + 条件请求
	contentURL := *base + "/api/v1/files/" + fileID + "/content"

	fullReq, _ := http.NewRequest(http.MethodGet, contentURL, nil)
	fullReq.Header.Set("Authorization", "Bearer "+access)
	fullResp, err := c.Do(fullReq)
	if err != nil {
		fail("下载失败: %v", err)
	}
	fullBody, _ := io.ReadAll(fullResp.Body)
	_ = fullResp.Body.Close()
	if fullResp.StatusCode != http.StatusOK {
		fail("完整下载应 200,实际 %d", fullResp.StatusCode)
	}
	if int64(len(fullBody)) != total {
		fail("下载内容长度应为 %d,实际 %d", total, len(fullBody))
	}
	dlSum := sha256.Sum256(fullBody)
	if hex.EncodeToString(dlSum[:]) != wantHash {
		fail("下载内容哈希与上传内容不符(内容被破坏)")
	}
	fmt.Printf("26) 下载完整内容       -> %d 字节,哈希与上传一致\n", len(fullBody))

	rangeReq, _ := http.NewRequest(http.MethodGet, contentURL, nil)
	rangeReq.Header.Set("Authorization", "Bearer "+access)
	rangeReq.Header.Set("Range", "bytes=0-99")
	rangeResp, err := c.Do(rangeReq)
	if err != nil {
		fail("Range 下载失败: %v", err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	_ = rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent {
		fail("Range 请求应 206,实际 %d", rangeResp.StatusCode)
	}
	if len(rangeBody) != 100 {
		fail("Range 应返回 100 字节,实际 %d", len(rangeBody))
	}
	wantCR := fmt.Sprintf("bytes 0-99/%d", total)
	if got := rangeResp.Header.Get("Content-Range"); got != wantCR {
		fail("Content-Range 应为 %q,实际 %q", wantCR, got)
	}
	fmt.Printf("27) 下载 Range         -> %d Content-Range=%q\n", rangeResp.StatusCode, rangeResp.Header.Get("Content-Range"))

	condReq, _ := http.NewRequest(http.MethodGet, contentURL, nil)
	condReq.Header.Set("Authorization", "Bearer "+access)
	condReq.Header.Set("If-None-Match", det.ETag)
	condResp, err := c.Do(condReq)
	if err != nil {
		fail("条件下载失败: %v", err)
	}
	_ = condResp.Body.Close()
	if condResp.StatusCode != http.StatusNotModified {
		fail("If-None-Match 命中应 304,实际 %d", condResp.StatusCode)
	}
	fmt.Printf("28) 条件下载           -> %d(ETag 未变,客户端可跳过)\n", condResp.StatusCode)

	// 29) 写操作闭环:改名(带 base_version)→ 陈旧版本 409 → 影响面预检 → 删除
	doJSONReq2 := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		rq, err := http.NewRequest(method, *base+path, body)
		if err != nil {
			fail("构造请求失败: %v", err)
		}
		if payload != nil {
			rq.Header.Set("Content-Type", "application/json")
		}
		rq.Header.Set("Authorization", "Bearer "+access)
		rs, derr := c.Do(rq)
		if derr != nil {
			fail("请求 %s %s 失败: %v", method, path, derr)
		}
		return rs
	}

	// 改名(带当前版本 1)
	renameResp := doJSONReq2(http.MethodPatch, "/api/v1/files/"+fileID,
		map[string]any{"name": "renamed-" + tusName, "base_version": 1})
	renameRaw, _ := io.ReadAll(renameResp.Body)
	_ = renameResp.Body.Close()
	if renameResp.StatusCode != http.StatusOK {
		fail("改名应 200,实际 %d %s", renameResp.StatusCode, string(renameRaw))
	}
	var renamed struct {
		Name    string `json:"name"`
		Version int64  `json:"version"`
	}
	_ = json.Unmarshal(renameRaw, &renamed)
	if renamed.Version != 2 {
		fail("改名后版本应为 2,实际 %d", renamed.Version)
	}
	fmt.Printf("29) 改名               -> 200 name=%s version=%d\n", renamed.Name, renamed.Version)

	// 用**陈旧版本 1** 再改 → 409,且响应体必须带服务端状态(6.6 并发写矩阵)
	staleResp := doJSONReq2(http.MethodPatch, "/api/v1/files/"+fileID,
		map[string]any{"name": "stale.txt", "base_version": 1})
	staleRaw, _ := io.ReadAll(staleResp.Body)
	_ = staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusConflict {
		fail("陈旧 base_version 应 409,实际 %d %s", staleResp.StatusCode, string(staleRaw))
	}
	var conflict struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(staleRaw, &conflict)
	for _, k := range []string{"reason", "server_version", "server_etag", "server_updated_at"} {
		if conflict.Details[k] == nil {
			fail("409 必须带 %s(客户端据此免二次请求展示冲突): %s", k, string(staleRaw))
		}
	}
	fmt.Printf("30) 陈旧版本 409       -> reason=%v server_version=%v\n",
		conflict.Details["reason"], conflict.Details["server_version"])

	// 影响面预检(删除前二次确认的依据)
	statsResp := doJSONReq2(http.MethodGet, "/api/v1/files/"+fileID+"/subtree-stats", nil)
	statsRaw, _ := io.ReadAll(statsResp.Body)
	_ = statsResp.Body.Close()
	if statsResp.StatusCode != http.StatusOK {
		fail("预检应 200,实际 %d %s", statsResp.StatusCode, string(statsRaw))
	}
	var stats struct {
		FileCount  int64 `json:"file_count"`
		TotalSize  int64 `json:"total_size_bytes"`
		ObjectRefs int64 `json:"object_refs"`
	}
	_ = json.Unmarshal(statsRaw, &stats)
	if stats.FileCount != 1 || stats.TotalSize != total {
		fail("预检应为 1 个文件 / %d 字节,实际 %d/%d", total, stats.FileCount, stats.TotalSize)
	}
	fmt.Printf("31) 影响面预检         -> 文件=%d 字节=%d 对象=%d\n",
		stats.FileCount, stats.TotalSize, stats.ObjectRefs)

	// 32) 共享到空间(BE-S6-03):只新增元数据行,内容不复制
	//
	// 探针里把文件共享到**同一空间的根目录**并另起一个名字:
	// 跨空间要建团队空间(需要建组),而服务端目前没有"建团队空间"的接口;
	// 而共享的代码路径与目标空间是否为同一空间无关(同一份 Share 实现),
	// 用同一空间就能验证"新行 + 引用 +1 + 沿用/改名 + 配额按逻辑行计"。
	shareName := "shared-" + tusName
	shareResp := doJSONReq2(http.MethodPost, "/api/v1/files/"+fileID+"/share-to-space",
		map[string]any{"target_space_id": login.PersonalSpace.ID, "name": shareName})
	shareRaw, _ := io.ReadAll(shareResp.Body)
	_ = shareResp.Body.Close()
	if shareResp.StatusCode != http.StatusCreated {
		fail("共享应 201,实际 %d %s", shareResp.StatusCode, string(shareRaw))
	}
	var shared struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		HashSHA256 string `json:"hash_sha256"`
		Size       int64  `json:"size"`
	}
	_ = json.Unmarshal(shareRaw, &shared)
	if shared.ID == "" || shared.ID == fileID {
		fail("共享必须返回新条目 id,实际 %q(源 %q)", shared.ID, fileID)
	}
	if shared.HashSHA256 != wantHash {
		fail("共享行必须指向同一内容 hash(hash=%s),实际 %q", wantHash, shared.HashSHA256)
	}
	if shared.Size != total {
		fail("共享行大小应为 %d,实际 %d", total, shared.Size)
	}
	fmt.Printf("32) 共享到空间         -> 201 id=%s name=%s 内容 hash 未变\n", short(shared.ID), shared.Name)

	// 33) 复制(BE-S7-03):同样只加引用,并回"新建行数 / 复用对象数"
	copyResp := doJSONReq2(http.MethodPost, "/api/v1/files/"+fileID+"/copy",
		map[string]any{"name": "copy-" + tusName})
	copyRaw, _ := io.ReadAll(copyResp.Body)
	_ = copyResp.Body.Close()
	if copyResp.StatusCode != http.StatusCreated {
		fail("复制应 201,实际 %d %s", copyResp.StatusCode, string(copyRaw))
	}
	var copied struct {
		Root struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			HashSHA256 string `json:"hash_sha256"`
		} `json:"root"`
		CopiedFiles     int64 `json:"copied_files"`
		CopiedDirs      int64 `json:"copied_dirs"`
		ObjectRefsAdded int64 `json:"object_refs_added"`
		SizeBytes       int64 `json:"size_bytes"`
	}
	_ = json.Unmarshal(copyRaw, &copied)
	if copied.CopiedFiles != 1 || copied.CopiedDirs != 0 {
		fail("复制应新建 1 文件 0 目录,实际 %d/%d", copied.CopiedFiles, copied.CopiedDirs)
	}
	if copied.ObjectRefsAdded != 1 {
		fail("复制应复用 1 个物理对象(内容不复制),实际 %d", copied.ObjectRefsAdded)
	}
	if copied.SizeBytes != total {
		fail("复制新增逻辑字节应为 %d,实际 %d", total, copied.SizeBytes)
	}
	if copied.Root.HashSHA256 != wantHash {
		fail("副本应指向同一内容 hash,实际 %q", copied.Root.HashSHA256)
	}
	fmt.Printf("33) 复制               -> 201 文件=%d 复用对象=%d 逻辑字节=%d name=%s\n",
		copied.CopiedFiles, copied.ObjectRefsAdded, copied.SizeBytes, copied.Root.Name)

	// 34) 引用计数闭环:删掉副本与共享行时**对象不该被回收**(原件还引用着),
	//     最后删原件才归零 —— 这条链路只有真跑才能验证(单测里对象是自己造的)。
	for _, c2 := range []struct {
		label, id string
		wantRel   int64
	}{
		{"副本", copied.Root.ID, 0},
		{"共享行", shared.ID, 0},
	} {
		rs := doJSONReq2(http.MethodDelete, "/api/v1/files/"+c2.id, nil)
		raw, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		if rs.StatusCode != http.StatusOK {
			fail("删除%s应 200,实际 %d %s", c2.label, rs.StatusCode, string(raw))
		}
		var d struct {
			ReleasedObjects int64 `json:"released_objects"`
		}
		_ = json.Unmarshal(raw, &d)
		if d.ReleasedObjects != c2.wantRel {
			fail("删除%s时对象不应被回收(原件仍引用),released_objects=%d", c2.label, d.ReleasedObjects)
		}
		fmt.Printf("34) 删%s -> 200 released_objects=%d(对象仍被原件引用)\n",
			c2.label, d.ReleasedObjects)
	}

	// 35) 共享/复制**不改动源文件**:源 ETag 必须与前面读到的一致
	//
	// 若实现误把"共享"做成"移动"或误改了源行版本,这里会立刻暴露。
	srcResp := doJSONReq2(http.MethodGet, "/api/v1/files/"+fileID, nil)
	srcRaw, _ := io.ReadAll(srcResp.Body)
	_ = srcResp.Body.Close()
	if srcResp.StatusCode != http.StatusOK {
		fail("取源文件应 200,实际 %d %s", srcResp.StatusCode, string(srcRaw))
	}
	var srcNow struct {
		Name    string `json:"name"`
		Version int64  `json:"version"`
		ETag    string `json:"etag"`
	}
	_ = json.Unmarshal(srcRaw, &srcNow)
	if srcNow.Version != 2 || srcNow.Name != "renamed-"+tusName {
		fail("共享/复制不应改动源行,实际 version=%d name=%s", srcNow.Version, srcNow.Name)
	}
	fmt.Printf("35) 源文件未受影响     -> version=%d name=%s\n", srcNow.Version, srcNow.Name)

	// 硬删 + 核对影响面与预检一致
	delResp := doJSONReq2(http.MethodDelete, "/api/v1/files/"+fileID, nil)
	delRaw, _ := io.ReadAll(delResp.Body)
	_ = delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		fail("删除应 200,实际 %d %s", delResp.StatusCode, string(delRaw))
	}
	var del struct {
		DeletedFiles    int64 `json:"deleted_files"`
		ReleasedObjects int64 `json:"released_objects"`
		FreedBytes      int64 `json:"freed_bytes"`
	}
	_ = json.Unmarshal(delRaw, &del)
	if del.DeletedFiles != stats.FileCount || del.FreedBytes != stats.TotalSize {
		fail("删除影响面应与预检一致: 预检 %d/%d,实际 %d/%d",
			stats.FileCount, stats.TotalSize, del.DeletedFiles, del.FreedBytes)
	}
	fmt.Printf("36) 硬删               -> 文件=%d 对象=%d 字节=%d(与预检一致)\n",
		del.DeletedFiles, del.ReleasedObjects, del.FreedBytes)

	// 37) 最后一个引用消失时对象必须归零(前面三次删除都没归零,这里必须归零)
	if del.ReleasedObjects != 1 {
		fail("删掉最后一个引用时应有 1 个对象引用归零,实际 %d", del.ReleasedObjects)
	}

	// 38) 秒传:持物证明免重传(BE-S4-04 / 6.10 五细则)
	//
	// 前一步刚把这个内容的对象删成 pending_delete(引用归零),所以这里换一份
	// **新的内容**:先正常传上去(造出"服务端已有该内容"),再用同一份内容
	// 走"预检 → 挑战 → 免重传定稿",验证整条通道在真实服务上闭合。
	//
	// 这也是唯一能验证"**内容真的没有被重写**"的地方:对象与文件都由真实链路产生。
	fastData := make([]byte, 256*1024) // 256KB > 32KB 下限
	for i := range fastData {
		fastData[i] = byte((i*17 + int(time.Now().UnixNano()%97)) % 251)
	}
	fastSum := sha256.Sum256(fastData)
	fastHash := hex.EncodeToString(fastSum[:])
	fastName := fmt.Sprintf("fast-src-%d.bin", time.Now().UnixNano())

	// 38a) 全量上传这份内容(走 H5 multipart 直传:与 TUS 同一个 finalize)
	//
	// multipart 形状(契约):**元数据段在前,文件段在后**。
	// 元数据段字段名 `metadata`,值是 JSON —— 文件名只走 JSON,
	// 不进 URL(否则会落进 Nginx access log,6.7)。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	simpleMeta, _ := json.Marshal(map[string]any{
		"space_id": login.PersonalSpace.ID, "parent_id": "", "name": fastName,
		"size": len(fastData), "hash": fastHash,
	})
	if werr := mw.WriteField("metadata", string(simpleMeta)); werr != nil {
		fail("写元数据段失败: %v", werr)
	}
	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, fastName))
	hdr.Set("Content-Type", "application/octet-stream")
	part, _ := mw.CreatePart(hdr)
	if _, werr := part.Write(fastData); werr != nil {
		fail("构造 multipart 失败: %v", werr)
	}
	_ = mw.Close()
	simpleReq, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/upload/simple", &buf)
	simpleReq.Header.Set("Content-Type", mw.FormDataContentType())
	simpleReq.Header.Set("Authorization", "Bearer "+access)
	simpleResp, err := c.Do(simpleReq)
	if err != nil {
		fail("multipart 直传失败: %v", err)
	}
	simpleRaw, _ := io.ReadAll(simpleResp.Body)
	_ = simpleResp.Body.Close()
	if simpleResp.StatusCode != http.StatusCreated {
		fail("multipart 直传应 201,实际 %d %s", simpleResp.StatusCode, string(simpleRaw))
	}
	fmt.Printf("38) 全量上传基线内容   -> 201 %d 字节 hash=%s…\n", len(fastData), fastHash[:12])

	// 38b) 同一个内容再传一次:预检应命中并下发挑战
	fastName2 := fmt.Sprintf("fast-dup-%d.bin", time.Now().UnixNano())
	dupResp := doJSONReq2(http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": login.PersonalSpace.ID, "parent_id": "", "name": fastName2,
		"size": len(fastData), "hash": fastHash,
	})
	dupRaw, _ := io.ReadAll(dupResp.Body)
	_ = dupResp.Body.Close()
	if dupResp.StatusCode != http.StatusCreated {
		fail("建任务应 201,实际 %d %s", dupResp.StatusCode, string(dupRaw))
	}
	var dup struct {
		UploadID   string `json:"upload_id"`
		Ticket     string `json:"upload_ticket"`
		FastUpload *struct {
			Nonce        string  `json:"nonce"`
			SampleLen    int64   `json:"sample_len"`
			Offsets      []int64 `json:"sample_offsets"`
			ExpiresInSec int64   `json:"expires_in_seconds"`
		} `json:"fast_upload"`
	}
	_ = json.Unmarshal(dupRaw, &dup)
	if dup.FastUpload == nil {
		fail("预检命中必须下发 fast_upload 挑战: %s", string(dupRaw))
	}
	if dup.FastUpload.Nonce == "" || len(dup.FastUpload.Offsets) == 0 {
		fail("挑战必须带 nonce 与样本偏移: %s", string(dupRaw))
	}
	if dup.FastUpload.SampleLen != 4096 {
		fail("样本长度应为 4096,实际 %d", dup.FastUpload.SampleLen)
	}
	fmt.Printf("38) 预检命中下发挑战   -> 样本=%d 长度=%d nonce=%d 字符 TTL=%ds\n",
		len(dup.FastUpload.Offsets), dup.FastUpload.SampleLen, len(dup.FastUpload.Nonce), dup.FastUpload.ExpiresInSec)

	// 38c) 按偏移算样本摘要(客户端要做的事)并免重传定稿
	digests := make([]string, len(dup.FastUpload.Offsets))
	for i, off := range dup.FastUpload.Offsets {
		if off < 0 || off+dup.FastUpload.SampleLen > int64(len(fastData)) {
			fail("样本偏移越界: %d", off)
		}
		sum := sha256.Sum256(fastData[off : off+dup.FastUpload.SampleLen])
		digests[i] = hex.EncodeToString(sum[:])
	}
	finReq, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/upload/"+dup.UploadID+"/finish", nil)
	finBody, _ := json.Marshal(map[string]any{"nonce": dup.FastUpload.Nonce, "sample_sha256": digests})
	finReq.Body = io.NopCloser(bytes.NewReader(finBody))
	finReq.ContentLength = int64(len(finBody))
	finReq.Header.Set("Content-Type", "application/json")
	finReq.Header.Set("Authorization", "Bearer "+access)
	finReq.Header.Set("X-Upload-Token", dup.Ticket)
	finResp, err := c.Do(finReq)
	if err != nil {
		fail("秒传定稿失败: %v", err)
	}
	finRaw, _ := io.ReadAll(finResp.Body)
	_ = finResp.Body.Close()
	if finResp.StatusCode != http.StatusCreated {
		fail("秒传定稿应 201,实际 %d %s", finResp.StatusCode, string(finRaw))
	}
	var fin struct {
		Deduped       bool  `json:"deduped"`
		UploadedBytes int64 `json:"uploaded_bytes"`
		File          struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			HashSHA256 string `json:"hash_sha256"`
			Size       int64  `json:"size"`
		} `json:"file"`
	}
	_ = json.Unmarshal(finRaw, &fin)
	if !fin.Deduped {
		fail("秒传必须命中去重(deduped=true): %s", string(finRaw))
	}
	if fin.UploadedBytes != 0 {
		fail("秒传路径不应上传任何字节,实际 %d", fin.UploadedBytes)
	}
	if fin.File.HashSHA256 != fastHash || fin.File.Size != int64(len(fastData)) {
		fail("秒传结果应指向同一内容,实际 %s/%d", fin.File.HashSHA256, fin.File.Size)
	}
	fmt.Printf("38) 秒传免重传定稿     -> 201 上传字节=%d deduped=%v hash 一致\n",
		fin.UploadedBytes, fin.Deduped)

	// 38d) 挑战是**一次性**的:重放同一 nonce 不能拿到第二个文件
	replayReq, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/upload/"+dup.UploadID+"/finish", bytes.NewReader(finBody))
	replayReq.Header.Set("Content-Type", "application/json")
	replayReq.Header.Set("Authorization", "Bearer "+access)
	replayReq.Header.Set("X-Upload-Token", dup.Ticket)
	replayResp, err := c.Do(replayReq)
	if err != nil {
		fail("重放请求失败: %v", err)
	}
	replayRaw, _ := io.ReadAll(replayResp.Body)
	_ = replayResp.Body.Close()
	// 同一 upload 已定稿 → 幂等重放回既有结果(200),绝不能**再建一行**
	if replayResp.StatusCode != http.StatusOK {
		fail("重放应幂等回 200,实际 %d %s", replayResp.StatusCode, string(replayRaw))
	}
	var replay struct {
		Replayed bool `json:"replayed"`
		File     struct {
			ID string `json:"id"`
		} `json:"file"`
	}
	_ = json.Unmarshal(replayRaw, &replay)
	if !replay.Replayed || replay.File.ID != fin.File.ID {
		fail("幂等重放必须回同一个文件,实际 replayed=%v id=%s(期望 %s)",
			replay.Replayed, replay.File.ID, fin.File.ID)
	}
	fmt.Printf("38) 幂等重放           -> 200 replayed=true 同一文件 id=%s\n", short(replay.File.ID))

	// 38e) 伪造 nonce(不持有内容)→ 必须 403,且不能再落一行
	//
	// 这是 R-05 防投毒的核心断言:"报一个 hash 就白拿内容"必须不成立。
	forgeName := fmt.Sprintf("fast-forge-%d.bin", time.Now().UnixNano())
	forgeCreate := doJSONReq2(http.MethodPost, "/api/v1/upload/create", map[string]any{
		"space_id": login.PersonalSpace.ID, "name": forgeName,
		"size": len(fastData), "hash": fastHash,
	})
	forgeCreated, _ := io.ReadAll(forgeCreate.Body)
	_ = forgeCreate.Body.Close()
	var forged struct {
		UploadID   string `json:"upload_id"`
		Ticket     string `json:"upload_ticket"`
		FastUpload *struct {
			Offsets []int64 `json:"sample_offsets"`
		} `json:"fast_upload"`
	}
	_ = json.Unmarshal(forgeCreated, &forged)
	if forged.FastUpload == nil {
		fail("第二次预检也应命中(内容仍在): %s", string(forgeCreated))
	}
	// 用**错误内容**算摘要(因为"攻击者"并不持有该内容)
	wrong := make([]byte, len(fastData))
	for i := range wrong {
		wrong[i] = byte(i % 253)
	}
	wrongDigests := make([]string, len(forged.FastUpload.Offsets))
	for i, off := range forged.FastUpload.Offsets {
		sum := sha256.Sum256(wrong[off : off+4096])
		wrongDigests[i] = hex.EncodeToString(sum[:])
	}
	forgeReq, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/upload/"+forged.UploadID+"/finish", nil)
	forgeBody, _ := json.Marshal(map[string]any{"nonce": "00000000000000000000000000000000", "sample_sha256": wrongDigests})
	forgeReq.Body = io.NopCloser(bytes.NewReader(forgeBody))
	forgeReq.ContentLength = int64(len(forgeBody))
	forgeReq.Header.Set("Content-Type", "application/json")
	forgeReq.Header.Set("Authorization", "Bearer "+access)
	forgeReq.Header.Set("X-Upload-Token", forged.Ticket)
	forgeResp, err := c.Do(forgeReq)
	if err != nil {
		fail("伪造请求失败: %v", err)
	}
	forgeRaw, _ := io.ReadAll(forgeResp.Body)
	_ = forgeResp.Body.Close()
	if forgeResp.StatusCode != http.StatusForbidden {
		fail("伪造 nonce 必须 403,实际 %d %s", forgeResp.StatusCode, string(forgeRaw))
	}
	fmt.Printf("38) 伪造 nonce 被拒    -> 403(未持有内容拿不到文件)\n")

	// 清理:删掉本轮造的两个文件(保持探针可重复运行)
	for _, fid := range []string{fin.File.ID} {
		rs := doJSONReq2(http.MethodDelete, "/api/v1/files/"+fid, nil)
		_ = rs.Body.Close()
	}
	// 基线文件按名字找出来删掉
	if listResp := doJSONReq2(http.MethodGet, "/api/v1/files?limit=100", nil); listResp != nil {
		listRaw, _ := io.ReadAll(listResp.Body)
		_ = listResp.Body.Close()
		var list struct {
			Entries []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(listRaw, &list)
		for _, e := range list.Entries {
			if e.Name == fastName {
				rs := doJSONReq2(http.MethodDelete, "/api/v1/files/"+e.ID, nil)
				_ = rs.Body.Close()
			}
		}
	}
	fmt.Printf("39) 清理秒传探针文件   -> 完成\n")

	// 40) 增量变更流(BE-S9-01/02):前面每一步写操作都应留下痕迹
	//
	// 这是"客户端不必再每 5 分钟全量扫描"的依据:从 since=0 拉一次,
	// 应能看到本轮的全部变更(created/shared_in/moved/updated/deleted…),
	// 且每条都带 parent_id + name(客户端据此直接更新本地树,无需回查详情)。
	var changes struct {
		Items []struct {
			ChangeSeq   int64  `json:"change_seq"`
			FileID      string `json:"file_id"`
			Kind        string `json:"kind"`
			Version     int64  `json:"version"`
			ParentID    string `json:"parent_id"`
			Name        string `json:"name"`
			OldParentID string `json:"old_parent_id"`
		} `json:"items"`
		NextSeq int64  `json:"next_seq"`
		HasMore bool   `json:"has_more"`
		SpaceID string `json:"space_id"`
	}
	doJSON(c, http.MethodGet, fmt.Sprintf("%s/api/v1/changes?space=%s&since=0&limit=1000",
		*base, login.PersonalSpace.ID), nil, access, &changes)
	if changes.SpaceID != login.PersonalSpace.ID {
		fail("响应 space_id 应为 %s,实际 %s", login.PersonalSpace.ID, changes.SpaceID)
	}
	if len(changes.Items) == 0 {
		fail("本轮做了改名/移动/删除/共享/复制,变更流不应为空")
	}
	kinds := map[string]int{}
	var lastSeq int64
	for _, it := range changes.Items {
		if it.ChangeSeq <= lastSeq {
			fail("变更必须按 change_seq 严格递增:前 %d 后 %d", lastSeq, it.ChangeSeq)
		}
		lastSeq = it.ChangeSeq
		kinds[it.Kind]++
		if it.Kind == "moved" && it.OldParentID == "" {
			fail("moved 事件必须带 old_parent_id(客户端据此从旧目录摘下): %+v", it)
		}
		if it.Kind != "deleted" && it.Name == "" {
			fail("%s 事件必须带 name: %+v", it.Kind, it)
		}
	}
	if changes.NextSeq != lastSeq {
		fail("next_seq 应为页内最后一条的 seq(%d),实际 %d", lastSeq, changes.NextSeq)
	}
	fmt.Printf("40) 增量变更流         -> %d 条 next_seq=%d kinds=%v\n",
		len(changes.Items), changes.NextSeq, kinds)

	// 40b) 幂等:同一个 since 再拉一次,结果必须完全一致
	var again struct {
		Items   []struct{ ChangeSeq int64 } `json:"items"`
		NextSeq int64                       `json:"next_seq"`
	}
	doJSON(c, http.MethodGet, fmt.Sprintf("%s/api/v1/changes?space=%s&since=0&limit=1000",
		*base, login.PersonalSpace.ID), nil, access, &again)
	if len(again.Items) != len(changes.Items) || again.NextSeq != changes.NextSeq {
		fail("同一 since 必须幂等: %d/%d vs %d/%d",
			len(changes.Items), changes.NextSeq, len(again.Items), again.NextSeq)
	}
	fmt.Printf("40) 变更流幂等         -> 同一 since 结果一致(%d 条)\n", len(again.Items))

	// 40c) 游标上报(BE-S9-03):单维取 max,重复/倒退上报不改变已确认进度
	for _, seq := range []int64{changes.NextSeq, 0} {
		doJSON(c, http.MethodPost, *base+"/api/v1/sync/cursors",
			map[string]any{"client_id": "probesmoke", "space_id": login.PersonalSpace.ID, "last_seq": seq},
			access, nil)
	}
	fmt.Printf("40) 游标上报           -> 200(含一次倒退上报,应被 max 忽略)\n")

	// 41) SSE 事件通道(BE-S9-04/05):真实长连接 + 真实写入触发
	//
	// 必须用真实 HTTP 长连接:SSE 最容易出的问题(Flush 被中间件吞掉)只在真实
	// 连接上暴露 —— 实测就是这样抓到的(事件全攒到连接关闭才发,服务端毫无报错)。
	sseReq, _ := http.NewRequest(http.MethodGet, *base+"/api/v1/events", nil)
	sseReq.Header.Set("Authorization", "Bearer "+access)
	sseResp, serr := (&http.Client{Timeout: 20 * time.Second}).Do(sseReq)
	if serr != nil {
		fail("SSE 建连失败: %v", serr)
	}
	defer func() { _ = sseResp.Body.Close() }()
	if sseResp.StatusCode != http.StatusOK {
		fail("SSE 应 200,实际 %d", sseResp.StatusCode)
	}
	if sseResp.Header.Get("X-Accel-Buffering") != "no" {
		fail("SSE 必须声明 X-Accel-Buffering: no(否则 Nginx 会缓冲事件)")
	}
	sseReader := bufio.NewReader(sseResp.Body)
	if _, rerr := sseReader.ReadString('\n'); rerr != nil {
		fail("读 SSE ready 失败: %v", rerr)
	}
	fmt.Printf("41) SSE 建连           -> 200 X-Accel-Buffering=no(ready 已到)\n")

	// 触发一次真实写入(multipart 直传一个 16 字节文件),应收到 change 帧
	trigName := fmt.Sprintf("sse-%d.bin", time.Now().UnixNano())
	var tbuf bytes.Buffer
	tmw := multipart.NewWriter(&tbuf)
	tmeta, _ := json.Marshal(map[string]any{
		"space_id": login.PersonalSpace.ID, "name": trigName, "size": 16,
	})
	_ = tmw.WriteField("metadata", string(tmeta))
	thdr := textproto.MIMEHeader{}
	thdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, trigName))
	thdr.Set("Content-Type", "application/octet-stream")
	tpart, _ := tmw.CreatePart(thdr)
	_, _ = tpart.Write(bytes.Repeat([]byte("S"), 16))
	_ = tmw.Close()
	tReq, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/upload/simple", &tbuf)
	tReq.Header.Set("Content-Type", tmw.FormDataContentType())
	tReq.Header.Set("Authorization", "Bearer "+access)
	tResp, terr := (&http.Client{Timeout: 15 * time.Second}).Do(tReq)
	if terr != nil {
		fail("触发写入失败: %v", terr)
	}
	tRespRaw, _ := io.ReadAll(tResp.Body)
	_ = tResp.Body.Close()
	if tResp.StatusCode != http.StatusCreated {
		fail("触发写入应 201,实际 %d %s", tResp.StatusCode, string(tRespRaw))
	}

	sseDeadline := time.Now().Add(8 * time.Second)
	gotFrame := false
	for time.Now().Before(sseDeadline) {
		line, rerr := sseReader.ReadString('\n')
		if rerr != nil {
			fail("读 SSE 帧失败: %v", rerr)
		}
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "id: ") {
			continue // 心跳(注释行)或 event 行
		}
		if !strings.HasPrefix(line, "id: "+login.PersonalSpace.ID+":") {
			fail("帧 id 必须是 {space_id}:{change_seq},实际 %q", line)
		}
		gotFrame = true
		fmt.Printf("41) SSE 事件帧         -> %s(真实写入触发)\n", line)
		break
	}
	if !gotFrame {
		fail("写入后未在 8s 内收到事件帧(Flush 或扇出链路有问题)")
	}

	// 清理触发用的文件
	if listResp := doJSONReq2(http.MethodGet, "/api/v1/files?limit=100", nil); listResp != nil {
		listRaw, _ := io.ReadAll(listResp.Body)
		_ = listResp.Body.Close()
		var list struct {
			Entries []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"entries"`
		}
		_ = json.Unmarshal(listRaw, &list)
		for _, en := range list.Entries {
			if en.Name == trigName {
				rs := doJSONReq2(http.MethodDelete, "/api/v1/files/"+en.ID, nil)
				_ = rs.Body.Close()
			}
		}
	}

	// 42) WebDAV 方法集(BE-S8):真实 Basic 认证 + MKCOL/PUT/PROPFIND/MOVE/DELETE
	//
	// 用真实 HTTP:DAV 的行为大半在上游 handler 里(207 Multi-Status、Depth、
	// Destination 解析),而"我们的 PG 后端与上游契约对不对得上"只有真跑才看得出来。
	// 实测就抓到过两处:FileInfo 漏实现 ETager 会静默用 ModTime+Size 现算 ETag;
	// 上游把 Rename 的任何错误都映射成 403(状态码错 → 客户端处置动作错)。
	davReq := func(method, path string, body io.Reader, hdr map[string]string) *http.Response {
		rq, rerr := http.NewRequest(method, *base+path, body)
		if rerr != nil {
			fail("构造 WebDAV 请求失败: %v", rerr)
		}
		rq.SetBasicAuth(*user, *pass)
		for k, v := range hdr {
			rq.Header.Set(k, v)
		}
		rsp, derr := c.Do(rq)
		if derr != nil {
			fail("WebDAV %s %s 失败: %v", method, path, derr)
		}
		return rsp
	}

	// MKCOL 建目录
	mk := davReq("MKCOL", "/webdav/"+login.PersonalSpace.ID+"/probe-dav", nil, nil)
	_ = mk.Body.Close()
	if mk.StatusCode != http.StatusCreated {
		fail("WebDAV MKCOL 应 201,实际 %d", mk.StatusCode)
	}
	// PUT 一个文件(走统一定稿:服务端实测 hash + 单事务)
	davContent := []byte("webdav-put-" + fmt.Sprintf("%d", time.Now().UnixNano()))
	pu := davReq(http.MethodPut, "/webdav/"+login.PersonalSpace.ID+"/probe-dav/f.txt",
		bytes.NewReader(davContent), nil)
	_ = pu.Body.Close()
	if pu.StatusCode != http.StatusCreated {
		fail("WebDAV PUT 应 201,实际 %d", pu.StatusCode)
	}
	davETag := pu.Header.Get("ETag")
	if davETag == "" {
		fail("WebDAV PUT 必须回带引号的强 ETag")
	}
	// PROPFIND Depth:1 → 207 且 getetag 与 PUT 返回的一致(BE-S8-02 的实质)
	pf := davReq("PROPFIND", "/webdav/"+login.PersonalSpace.ID+"/probe-dav", nil,
		map[string]string{"Depth": "1"})
	pfBody, _ := io.ReadAll(pf.Body)
	_ = pf.Body.Close()
	if pf.StatusCode != http.StatusMultiStatus {
		fail("WebDAV PROPFIND 应 207,实际 %d %s", pf.StatusCode, string(pfBody))
	}
	if !strings.Contains(string(pfBody), "f.txt") {
		fail("PROPFIND 结果应含 f.txt")
	}
	if !strings.Contains(string(pfBody), davETag) {
		fail("PROPFIND 的 getetag 必须与 PUT 的 ETag 一致(否则条件请求永远不匹配)")
	}
	// 条件请求:If-Match 用**过期值** → 412,且内容不变
	cond := davReq(http.MethodPut, "/webdav/"+login.PersonalSpace.ID+"/probe-dav/f.txt",
		strings.NewReader("should-not-land"), map[string]string{"If-Match": `"00000099-deadbeef"`})
	condBody, _ := io.ReadAll(cond.Body)
	_ = cond.Body.Close()
	if cond.StatusCode != http.StatusPreconditionFailed {
		fail("WebDAV If-Match 不匹配应 412,实际 %d %s", cond.StatusCode, string(condBody))
	}
	// GET 验证内容没被改(412 必须发生在读 body 之前)
	get := davReq(http.MethodGet, "/webdav/"+login.PersonalSpace.ID+"/probe-dav/f.txt", nil, nil)
	getBody, _ := io.ReadAll(get.Body)
	_ = get.Body.Close()
	if !bytes.Equal(getBody, davContent) {
		fail("412 之后内容不应变化")
	}
	// MOVE 改名(Destination 用绝对 URL,资源管理器就是这么发的)
	mv := davReq("MOVE", "/webdav/"+login.PersonalSpace.ID+"/probe-dav/f.txt", nil,
		map[string]string{"Destination": *base + "/webdav/" + login.PersonalSpace.ID + "/probe-dav/g.txt"})
	_ = mv.Body.Close()
	if mv.StatusCode != http.StatusCreated && mv.StatusCode != http.StatusNoContent {
		fail("WebDAV MOVE 应 201/204,实际 %d", mv.StatusCode)
	}
	// LOCK 明确 501(不宣称 class 2)
	lk := davReq("LOCK", "/webdav/"+login.PersonalSpace.ID+"/probe-dav/g.txt",
		strings.NewReader(`<?xml version="1.0"?><lockinfo/>`), nil)
	_, _ = io.ReadAll(lk.Body)
	_ = lk.Body.Close()
	if lk.StatusCode != http.StatusNotImplemented {
		fail("WebDAV LOCK 应 501(明确不实现),实际 %d", lk.StatusCode)
	}
	if strings.Contains(lk.Header.Get("DAV"), "2") {
		fail("未实现 LOCK 就不能宣称 class 2,实际 %q", lk.Header.Get("DAV"))
	}
	// DELETE 清理
	dl := davReq(http.MethodDelete, "/webdav/"+login.PersonalSpace.ID+"/probe-dav/g.txt", nil, nil)
	_ = dl.Body.Close()
	if dl.StatusCode != http.StatusNoContent {
		fail("WebDAV DELETE 应 204,实际 %d", dl.StatusCode)
	}
	dm := davReq(http.MethodDelete, "/webdav/"+login.PersonalSpace.ID+"/probe-dav", nil, nil)
	_ = dm.Body.Close()
	fmt.Printf("42) WebDAV 方法集       -> MKCOL/PUT/PROPFIND(207+getetag 一致)/412/MOVE/501(LOCK)/DELETE 全部符合\n")

	// 43) 编辑锁与分享链接(BE-S6-05 / BE-S6-04)
	//
	// 两者都是"权限之外的凭据":编辑锁要能抢占过期锁、且只拦覆盖写;
	// 分享链接是**系统唯一的免登录出口**,它的 token/密码/次数都必须真的生效。
	lockName := fmt.Sprintf("lock-%d.txt", time.Now().UnixNano())
	// 直接用 multipart 传一个文件(与第 38 步同一路径:建任务 + 定稿一次完成)。
	// 刻意**不**先调 /upload/create:那会先占住名字,随后的 multipart 建任务
	// 撞上"同名上传进行中"而 409(实测踩过)。
	var lbuf bytes.Buffer
	lmw := multipart.NewWriter(&lbuf)
	lmeta, _ := json.Marshal(map[string]any{
		"space_id": login.PersonalSpace.ID, "name": lockName, "size": 4,
	})
	_ = lmw.WriteField("metadata", string(lmeta))
	lhdr := textproto.MIMEHeader{}
	lhdr.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, lockName))
	lhdr.Set("Content-Type", "application/octet-stream")
	lpart, _ := lmw.CreatePart(lhdr)
	_, _ = lpart.Write([]byte("lock"))
	_ = lmw.Close()
	lreq, _ := http.NewRequest(http.MethodPost, *base+"/api/v1/upload/simple", &lbuf)
	lreq.Header.Set("Content-Type", lmw.FormDataContentType())
	lreq.Header.Set("Authorization", "Bearer "+access)
	lresp, lerr := c.Do(lreq)
	if lerr != nil {
		fail("前置上传失败: %v", lerr)
	}
	var lfile struct {
		File struct {
			ID string `json:"id"`
		} `json:"file"`
	}
	lbody, _ := io.ReadAll(lresp.Body)
	_ = lresp.Body.Close()
	_ = json.Unmarshal(lbody, &lfile)
	if lfile.File.ID == "" {
		fail("前置上传未拿到文件 id: %s", string(lbody))
	}

	// 43a) 获取编辑锁 → 200 + TTL 1800s
	lkResp := doJSONReq2(http.MethodPost, "/api/v1/files/"+lfile.File.ID+"/lock", nil)
	lkBody, _ := io.ReadAll(lkResp.Body)
	_ = lkResp.Body.Close()
	if lkResp.StatusCode != http.StatusOK {
		fail("获取编辑锁应 200,实际 %d %s", lkResp.StatusCode, string(lkBody))
	}
	var lkv struct {
		TTLSeconds int64 `json:"ttl_seconds"`
	}
	_ = json.Unmarshal(lkBody, &lkv)
	if lkv.TTLSeconds != 1800 {
		fail("编辑锁 TTL 应为 1800s(30min),实际 %d", lkv.TTLSeconds)
	}
	// 删除锁 → 204,且重复删除仍 204(幂等)
	for i := 0; i < 2; i++ {
		rs := doJSONReq2(http.MethodDelete, "/api/v1/files/"+lfile.File.ID+"/lock", nil)
		_ = rs.Body.Close()
		if rs.StatusCode != http.StatusNoContent {
			fail("第 %d 次释放锁应 204(幂等),实际 %d", i+1, rs.StatusCode)
		}
	}
	fmt.Printf("43) 编辑锁             -> 获取 200(ttl=%ds) 释放 204(重复释放仍 204)\n", lkv.TTLSeconds)

	// 43b) 创建分享 → 免登录 meta → 免登录下载 → 超次 410
	shResp := doJSONReq2(http.MethodPost, "/api/v1/shares", map[string]any{
		"file_id": lfile.File.ID, "max_downloads": 1,
	})
	shBody, _ := io.ReadAll(shResp.Body)
	_ = shResp.Body.Close()
	if shResp.StatusCode != http.StatusCreated {
		fail("创建分享应 201,实际 %d %s", shResp.StatusCode, string(shBody))
	}
	var sh struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(shBody, &sh)
	if len(sh.Token) != 43 {
		fail("分享 token 应为 43 字符(32 字节 base64url),实际 %d", len(sh.Token))
	}
	// meta 免登录(不带 Authorization)
	metaRec := func() int {
		rq, _ := http.NewRequest(http.MethodGet, *base+"/api/v1/shares/"+sh.Token+"/meta", nil)
		rsp, merr := c.Do(rq)
		if merr != nil {
			fail("免登录 meta 失败: %v", merr)
		}
		_ = rsp.Body.Close()
		return rsp.StatusCode
	}
	if code := metaRec(); code != http.StatusOK {
		fail("免登录 meta 应 200,实际 %d", code)
	}
	// 免登录下载(第一次成功)
	dlReq, _ := http.NewRequest(http.MethodGet, *base+"/api/v1/shares/"+sh.Token+"/download", nil)
	dlResp, derr := c.Do(dlReq)
	if derr != nil {
		fail("免登录下载失败: %v", derr)
	}
	dlBody, _ := io.ReadAll(dlResp.Body)
	_ = dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK || string(dlBody) != "lock" {
		fail("免登录下载应 200 且内容一致,实际 %d %q", dlResp.StatusCode, string(dlBody))
	}
	// 超过次数 → 410 + resource_gone
	dl2, _ := http.NewRequest(http.MethodGet, *base+"/api/v1/shares/"+sh.Token+"/download", nil)
	dlResp2, derr2 := c.Do(dl2)
	if derr2 != nil {
		fail("第二次下载失败: %v", derr2)
	}
	dl2Body, _ := io.ReadAll(dlResp2.Body)
	_ = dlResp2.Body.Close()
	if dlResp2.StatusCode != http.StatusGone {
		fail("超过下载次数应 410,实际 %d %s", dlResp2.StatusCode, string(dl2Body))
	}
	var goneBody struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	_ = json.Unmarshal(dl2Body, &goneBody)
	if goneBody.Code != "resource_gone" || goneBody.Details["reason"] != "download_limit_reached" {
		fail("410 应带 code=resource_gone 与 reason=download_limit_reached,实际 %s", string(dl2Body))
	}
	fmt.Printf("43) 分享链接           -> 201 token=%d 字符 免登录 meta 200 / 下载 200 / 超次 410(download_limit_reached)\n", len(sh.Token))

	// 清理
	cln := doJSONReq2(http.MethodDelete, "/api/v1/files/"+lfile.File.ID, nil)
	_ = cln.Body.Close()

	// 44) Prometheus 指标(BE-S10-03):真的能 scrape,且业务指标真的动了
	//
	// 指标最容易写成"永远为 0 的假指标":打点写在某个从不执行的路径上,
	// 面板上一条平线而没人发现。所以这一步先做几个真实请求,再断言对应的
	// 计数**大于 0**;同时断言路由标签是**归一后的模式**(高基数标签会吃光内存)。
	metricsReq, _ := http.NewRequest(http.MethodGet, *base+"/metrics", nil)
	metricsResp, merr := c.Do(metricsReq)
	if merr != nil {
		fail("抓取 /metrics 失败: %v", merr)
	}
	metricsBody, _ := io.ReadAll(metricsResp.Body)
	_ = metricsResp.Body.Close()
	if metricsResp.StatusCode != http.StatusOK {
		fail("/metrics 应 200(同机抓取),实际 %d %s", metricsResp.StatusCode, string(metricsBody))
	}
	if ct := metricsResp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		fail("/metrics 应为 Prometheus 文本格式,实际 %q", ct)
	}
	text := string(metricsBody)
	for _, want := range []string{
		"# TYPE netdisk_http_requests_total counter",
		"# TYPE netdisk_http_request_seconds histogram",
		"netdisk_http_in_flight",
		"netdisk_sse_connections",
		"netdisk_audit_queue_depth",
		"netdisk_sync_feed_rows",
		"netdisk_backup_last_success_timestamp_seconds",
		"netdisk_restore_drill_last_timestamp_seconds",
		"netdisk_tus_active_uploads",
		"netdisk_stage_cleanup_runs_total",
	} {
		if !strings.Contains(text, want) {
			fail("指标输出缺少 %q", want)
		}
	}
	// 业务指标必须真的动过(前面的步骤打了大量请求)
	if !strings.Contains(text, "netdisk_http_requests_total{") {
		fail("请求计数必须出现(否则指标是假的):\n%s", text)
	}
	// 路由标签不得是原始路径(高基数)
	if strings.Contains(text, `route="/api/v1/`) {
		fail("路由标签不得使用原始路径(高基数会吃光内存)")
	}
	// 限速丢弃指标(第 3 步的 TUS 建任务等路径会经过限速中间件)
	if !strings.Contains(text, "netdisk_ratelimit_rejected_total") {
		fail("应有限速丢弃指标(8.4 的'限速丢弃')")
	}
	// 统计出现的族数(便于人工核对覆盖面)
	families := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "# TYPE netdisk_") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				families[f[2]] = true
			}
		}
	}
	fmt.Printf("44) Prometheus 指标     -> 200 共 %d 个指标族(请求/SSE/审计/变更流/备份/暂存回收/对象巡检均已暴露)\n", len(families))

	// 45)~48) 目录级异步任务(BE-S7-02 / 6.11):阈值分流 / 任务期间 409 / 轮询 / 异步删除
	//
	// 阈值是**服务端配置**(policy.dir_op_sync_max_rows,默认 1000),所以这一段
	// 分两种情形:服务端阈值低于本段构造的子树行数 → 必须看到 202 + task_id;
	// 否则走同步 200,打印一行说明"服务端阈值未触发异步"。
	//
	// 但"两种配置下都绿"等于什么都没证明,所以留一个硬开关:
	// `NETDISK_PROBE_EXPECT_ASYNC_DIR_OPS=1` 时**必须**看到异步路径,否则失败。
	// 验收跑用这个开关(配 NETDISK_DIR_OP_SYNC_MAX_ROWS=2 起服务)。
	expectAsync := os.Getenv("NETDISK_PROBE_EXPECT_ASYNC_DIR_OPS") == "1"

	findEntry := func(parent, name string) (probeEntry, bool) {
		q := "/api/v1/files?limit=200"
		if parent != "" {
			q += "&parent=" + parent
		}
		rs := doJSONReq2(http.MethodGet, q, nil)
		raw, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		if rs.StatusCode != http.StatusOK {
			fail("列目录应 200,实际 %d: %s", rs.StatusCode, string(raw))
		}
		var out struct {
			Entries []probeEntry `json:"entries"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			fail("解析目录失败: %v(raw=%s)", err, string(raw))
		}
		for _, e := range out.Entries {
			if e.Name == name {
				return e, true
			}
		}
		return probeEntry{}, false
	}
	mustEntry := func(parent, name string) probeEntry {
		e, ok := findEntry(parent, name)
		if !ok {
			fail("目录 %q 下找不到条目 %q", parent, name)
		}
		return e
	}
	pollTask := func(id, label string) int64 {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			rs := doJSONReq2(http.MethodGet, "/api/v1/tasks/"+id, nil)
			raw, _ := io.ReadAll(rs.Body)
			_ = rs.Body.Close()
			if rs.StatusCode != http.StatusOK {
				fail("轮询任务 %s 应 200,实际 %d: %s", short(id), rs.StatusCode, string(raw))
			}
			var t struct {
				State string `json:"state"`
				Rows  int64  `json:"rows_affected"`
				Code  string `json:"error_code"`
				Msg   string `json:"error_message"`
			}
			if err := json.Unmarshal(raw, &t); err != nil {
				fail("解析任务失败: %v(raw=%s)", err, string(raw))
			}
			switch t.State {
			case "done":
				return t.Rows
			case "failed":
				fail("%s 任务失败: %s / %s", label, t.Code, t.Msg)
			}
			time.Sleep(150 * time.Millisecond)
		}
		fail("%s 任务超时(30s)未完成", label)
		return 0
	}
	// delAny 删除并(必要时)等异步任务做完 —— 用于清理,避免上一轮残留让 MKCOL 405。
	delAny := func(id, label string) {
		rs := doJSONReq2(http.MethodDelete, "/api/v1/files/"+id, nil)
		raw, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		switch rs.StatusCode {
		case http.StatusOK:
			return
		case http.StatusAccepted:
			var b struct {
				TaskID string `json:"task_id"`
			}
			_ = json.Unmarshal(raw, &b)
			if b.TaskID == "" {
				fail("%s 返回 202 但没给 task_id", label)
			}
			pollTask(b.TaskID, label)
		default:
			fail("%s 删除应 200/202,实际 %d: %s", label, rs.StatusCode, string(raw))
		}
	}

	// 清理上一轮可能的残留(上一次失败会留下同名目录)
	for _, nm := range []string{"probe-async", "probe-async-dest"} {
		if e, ok := findEntry("", nm); ok {
			delAny(e.ID, "残留 "+nm)
		}
	}
	mkcolDir := func(p string) {
		rs := davReq("MKCOL", "/webdav/"+login.PersonalSpace.ID+p, nil, nil)
		raw, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		if rs.StatusCode != http.StatusCreated {
			fail("MKCOL %s 应 201,实际 %d: %s", p, rs.StatusCode, string(raw))
		}
	}
	mkcolDir("/probe-async")
	mkcolDir("/probe-async/a")
	mkcolDir("/probe-async/a/b")
	mkcolDir("/probe-async-dest")

	asyncTop := mustEntry("", "probe-async")
	asyncDest := mustEntry("", "probe-async-dest")
	// 后代的 id 必须在**移动之前**取:任务期间连列它的父目录都会 409。
	asyncChild := mustEntry(asyncTop.ID, "a")

	// --- 45) 移动 3 行子树:按阈值分流 ---
	mvResp := doJSONReq2(http.MethodPost, "/api/v1/files/"+asyncTop.ID+"/move",
		map[string]any{"parent_id": asyncDest.ID})
	mvRaw, _ := io.ReadAll(mvResp.Body)
	_ = mvResp.Body.Close()
	var mvBody struct {
		Entry  *probeEntry `json:"entry"`
		TaskID string      `json:"task_id"`
		Async  bool        `json:"async"`
	}
	if err := json.Unmarshal(mvRaw, &mvBody); err != nil {
		fail("解析移动响应失败: %v(raw=%s)", err, string(mvRaw))
	}
	asyncUsed := mvResp.StatusCode == http.StatusAccepted || mvBody.Async
	if expectAsync && !asyncUsed {
		fail("要求异步但服务端走了同步(状态 %d)—— 请用 NETDISK_DIR_OP_SYNC_MAX_ROWS=2 起服务", mvResp.StatusCode)
	}
	if asyncUsed {
		if mvResp.StatusCode != http.StatusAccepted || mvBody.TaskID == "" {
			fail("异步移动应 202 + task_id,实际 %d body=%s", mvResp.StatusCode, string(mvRaw))
		}
		if mvBody.Entry != nil {
			fail("异步分支不应回条目(条目此刻还没动)")
		}
		fmt.Printf("45) 目录异步任务(移动) -> 202 async task=%s(服务端 dir_op_sync_max_rows < 子树行数)\n", short(mvBody.TaskID))
	} else {
		if mvResp.StatusCode != http.StatusOK || mvBody.Entry == nil {
			fail("同步移动应 200 + entry,实际 %d body=%s", mvResp.StatusCode, string(mvRaw))
		}
		fmt.Printf("45) 目录移动(同步路径)  -> 200(服务端阈值未触发异步;验收请设 NETDISK_DIR_OP_SYNC_MAX_ROWS=2)\n")
	}

	// --- 46) 任务期间:该子树的子请求一律 409 dir_op_in_progress ---
	if asyncUsed {
		// 判权/形状顺序:先读(Get)→ 再删。两条都必须 409 而不是 404/500。
		g := doJSONReq2(http.MethodGet, "/api/v1/files/"+asyncTop.ID, nil)
		gRaw, _ := io.ReadAll(g.Body)
		_ = g.Body.Close()
		if g.StatusCode != http.StatusConflict {
			fail("任务期间读该条目应 409,实际 %d: %s", g.StatusCode, string(gRaw))
		}
		var gErr struct {
			Code    string         `json:"code"`
			Details map[string]any `json:"details"`
		}
		_ = json.Unmarshal(gRaw, &gErr)
		if gErr.Details["reason"] != "dir_op_in_progress" {
			fail("409 的 reason 应为 dir_op_in_progress,实际 %v", gErr.Details["reason"])
		}
		// 后代条目同样被拦(守卫沿 parent_id 向上查,而不是只看根自己)
		d := doJSONReq2(http.MethodDelete, "/api/v1/files/"+asyncChild.ID, nil)
		dRaw, _ := io.ReadAll(d.Body)
		_ = d.Body.Close()
		if d.StatusCode != http.StatusConflict {
			fail("任务期间删后代条目应 409,实际 %d: %s", d.StatusCode, string(dRaw))
		}
		fmt.Printf("46) 任务期间子请求      -> 读/删均 409 dir_op_in_progress\n")
	} else {
		fmt.Printf("46) 任务期间子请求      -> 跳过(未走异步,没有\"任务期间\")\n")
	}

	// --- 47) 轮询到 done,且移动真的生效 ---
	if asyncUsed {
		rows := pollTask(mvBody.TaskID, "异步移动")
		if rows != 3 {
			fail("异步移动 rows_affected 应为 3(子树行数),实际 %d", rows)
		}
	}
	movedEntry := mustEntry(asyncDest.ID, "probe-async")
	if movedEntry.Parent != asyncDest.ID {
		fail("移动后 parent_id 应为目标目录,实际 %s", movedEntry.Parent)
	}
	if asyncUsed {
		fmt.Printf("47) 任务轮询            -> done;条目已挂到目标目录(parent=%s)\n", short(asyncDest.ID))
	} else {
		fmt.Printf("47) 移动生效(同步)      -> 条目已挂到目标目录(parent=%s)\n", short(asyncDest.ID))
	}

	// --- 48) 异步删除:入队后行仍在,执行后整棵消失 ---
	adResp := doJSONReq2(http.MethodDelete, "/api/v1/files/"+movedEntry.ID, nil)
	adRaw, _ := io.ReadAll(adResp.Body)
	_ = adResp.Body.Close()
	var delBody struct {
		TaskID string `json:"task_id"`
		Async  bool   `json:"async"`
	}
	if err := json.Unmarshal(adRaw, &delBody); err != nil {
		fail("解析删除响应失败: %v(raw=%s)", err, string(adRaw))
	}
	delAsync := adResp.StatusCode == http.StatusAccepted || delBody.Async
	if expectAsync && !delAsync {
		fail("要求异步删除但服务端走了同步(状态 %d)", adResp.StatusCode)
	}
	if delAsync {
		if delBody.TaskID == "" {
			fail("异步删除应带 task_id,body=%s", string(adRaw))
		}
		// 入队后条目仍在(异步的意义:响应返回时还没删)
		if _, ok := findEntry(asyncDest.ID, "probe-async"); !ok {
			fail("异步删除入队后条目不应立刻消失")
		}
		rows := pollTask(delBody.TaskID, "异步删除")
		if rows != 3 {
			fail("异步删除 rows_affected 应为 3,实际 %d", rows)
		}
	} else {
		if adResp.StatusCode != http.StatusOK {
			fail("同步删除应 200,实际 %d: %s", adResp.StatusCode, string(adRaw))
		}
	}
	if _, ok := findEntry(asyncDest.ID, "probe-async"); ok {
		fail("删除完成后子树应不可见")
	}
	fmt.Printf("48) 目录异步删除        -> 整棵子树已回收(异步=%v)\n", delAsync)

	// 清理
	delAny(asyncDest.ID, "probe-async-dest")




	fmt.Println("\n全部通过")
}

// short 把 uuid 截成前 8 位,只用于打印(日志里能认出是同一个条目即可)。
func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func doJSON(c *http.Client, method, url string, payload any, token string, out any) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(os.Stderr, "序列化失败: %v\n", err)
			os.Exit(1)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "构造请求失败: %v\n", err)
		os.Exit(1)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "请求 %s %s 失败: %v\n", method, url, err)
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "请求 %s %s -> %d: %s\n", method, url, resp.StatusCode, string(raw))
		os.Exit(1)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			fmt.Fprintf(os.Stderr, "解析响应失败: %v(raw=%s)\n", err, string(raw))
			os.Exit(1)
		}
	}
}


// probeEntry 是 /api/v1/files 列表里的一条(45~48 目录任务探针用)。
type probeEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	IsDir  bool   `json:"is_dir"`
	Parent string `json:"parent_id"`
}