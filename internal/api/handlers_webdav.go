package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/webdav"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/condreq"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/model"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/webdavauth"
	"github.com/netdisk/netdisk/internal/webdavfs"
)

// WebDAV 方法分发(BE-S8 / 6.2 / 6.12)。
//
// 分工:**能自己精确控制语义的方法自己处理,PHP 之外一律交给上游 handler**:
//
//	PUT     → 自己处理。上游 handlePut 会调 FileSystem.OpenFile+Write+Close,
//	          而 finalize(6.10)是"读一次内容、同事务结引用与配额"的事务性操作 ——
//	          把它塞进 Write/Close 会在 Close 失败时**无法把错误回给客户端**
//	          (上游只记日志),表现为"PUT 说成功了、文件其实是空的"。
//	          ⇒ 在进入上游之前拦截(这正是 BE-S5-06 的要求)。
//	DELETE/MOVE/COPY → 交给上游,但**先做条件请求校验**(缺口 1:上游不处理 If-Match)。
//	PROPFIND/GET/HEAD/MKCOL/PROPPATCH → 交给上游(Depth 0/1 与 207 由它按 RFC 处理)。
//	LOCK/UNLOCK      → **明确 501**(缺口 6.12:上游有内存锁实现,重启即失、多实例不共享,
//	          不能宣称 class 2;独占编辑走 REST 锁)。
const (
	// davAllow 是**我们真正支持**的方法集(不含 LOCK/UNLOCK)
	davAllow = "OPTIONS, GET, HEAD, PUT, DELETE, MKCOL, PROPFIND, PROPPATCH, COPY, MOVE"
	// davHeaderValue 只宣称 class 1(不实现 LOCK 就不能说 class 2)
	davHeaderValue = "1"
)

// handleWebDAV 是 WebDAV 的认证入口与方法分发(BE-S1-06 认证 + BE-S8 方法集)。
func (d Deps) handleWebDAV(w http.ResponseWriter, r *http.Request) {
	if d.WebDAV == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("WebDAV 认证未装配")))
		return
	}
	res, err := d.WebDAV.Authenticate(r.Context(), r)
	if err != nil {
		// 认证失败一律 401 + 挑战头,客户端据此弹密码框
		webdavauth.WriteChallenge(w, r, err)
		return
	}
	// 会话里的 token_version 必须与当前用户一致:改密/强制下线后会话立即失效(R-13)
	if d.DB != nil {
		u, uerr := repo.UserRepo{}.GetByID(r.Context(), db.AsQuerier(d.DB), res.UserID)
		if uerr != nil || u.Status != model.StatusActive || u.TokenVersion != res.TokenVersion {
			webdavauth.WriteChallenge(w, r, webdavauth.ErrUnauthorized)
			return
		}
	}
	if res.Token != "" {
		webdavauth.WriteToken(w, res.Token, d.WebDAV.TTLOrDefault())
	}

	// 统一响应头:class 1(不宣称 2)、MS-Author-Via 让资源管理器走 DAV
	w.Header().Set("DAV", davHeaderValue)
	w.Header().Set("MS-Author-Via", "DAV")

	// LOCK/UNLOCK:明确 501 + 可诊断提示(绝不能假装支持)
	if r.Method == "LOCK" || r.Method == "UNLOCK" {
		e := apierr.NotImplemented(
			"WebDAV LOCK 未启用:上游锁为内存实现(进程重启即失、多实例不共享),不满足生产要求;" +
				"并发保护请用 If-Match/ETag,独占编辑请用 REST 锁 POST /api/v1/files/{id}/lock")
		e.WithDetail("alternative", "if_match_or_rest_lock")
		apierr.Write(w, r, e)
		return
	}

	// 空间与路径解析:第一段是空间 id;缺省 → 调用者的个人空间
	spaceID, davPath, perr := d.resolveDAVTarget(r.Context(), res.UserID, r.PathValue("rest"))
	if perr != nil {
		apierr.Write(w, r, perr)
		return
	}
	fs, ferr := d.newDAVFS(r.Context(), res.UserID, spaceID)
	if ferr != nil {
		apierr.Write(w, r, ferr)
		return
	}

	// OPTIONS 自己答:上游会宣称 `DAV: 1, 2`,而我们不实现 LOCK(见文件头说明)
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", davAllow)
		w.WriteHeader(http.StatusOK)
		return
	}

	switch r.Method {
	case http.MethodPut:
		d.webdavPut(w, r, fs, davPath)
		return
	case "COPY":
		d.webdavCopy(w, r, fs, spaceID, davPath)
		return
	case "MOVE":
		d.webdavMove(w, r, fs, spaceID, davPath)
		return
	case "MKCOL":
		// 建目录也要判写权限(上游不会替我们判;4.3 权限只在服务端)
		if werr := d.Files.CheckWritable(r.Context(), fs.UserID, fs.SpaceID); werr != nil {
			apierr.Write(w, r, werr)
			return
		}
	}

	// 条件请求(缺口 1):上游 handlePut/handleDelete/handleMove/handleCopy 都不处理
	// If-Match/If-None-Match,不补这一课客户端就能"盲写"覆盖别人的修改。
	if err := d.checkDAVConditions(r, fs, davPath); err != nil {
		apierr.Write(w, r, err)
		return
	}

	h := &webdav.Handler{
		// Prefix 留空:路径已在上面归一为空间内路径(见 handleWebDAV 的分工说明)
		FileSystem: fs,
		LockSystem: davLockSystem,
		Logger: func(req *http.Request, herr error) {
			if d.Log != nil && herr != nil {
				d.Log.Warn("webdav 请求失败", "method", req.Method, "path", req.URL.Path, "err", herr)
			}
		},
	}
	// 路径空间归一:上游 handler 只看自己的前缀之后的路径,把空间段留在里面
	// 会让它去找一个叫 `webdav/{spaceID}` 的文件(表现为 MKCOL 405 / PROPFIND 空)。
	davReq := r.Clone(r.Context())
	davReq.URL = cloneURL(r.URL)
	davReq.URL.Path = davPath
	davReq.URL.RawPath = ""
	h.ServeHTTP(w, davReq)
}

// davLockSystem 是上游要求的锁系统。
//
// 用 NewMemLS 而不是 nil:nil 会让上游 panic;而内存锁**不对外宣称**
// (见 davHeaderValue 与 LOCK 的 501),它只用于满足 handler 的内部调用约定。
var davLockSystem = webdav.NewMemLS()

// resolveDAVTarget 把 `/webdav/{rest...}` 拆成 (spaceID, DAV 路径)。
//
// 约定:第一段是空间 id(客户端"映射一个网络位置 = 一个空间");
// 缺省(即 `/webdav/`)用调用者的个人空间 —— 探测请求与快速挂载都走这条。
func (d Deps) resolveDAVTarget(ctx context.Context, userID, rest string) (string, string, error) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		sp, err := repo.SpaceRepo{}.PersonalOf(ctx, db.AsQuerier(d.DB), userID)
		if err != nil {
			return "", "", apierr.Internal(fmt.Errorf("取个人空间失败: %w", err))
		}
		return sp.ID, "/", nil
	}
	segs := strings.SplitN(rest, "/", 2)
	spaceID := segs[0]
	p := "/"
	if len(segs) == 2 && segs[1] != "" {
		p = "/" + segs[1]
	}
	// 判权:被移出空间要 410(不是 404),客户端据此保留本地文件并在恢复后继续
	if d.Files != nil {
		if err := d.Files.CheckReadable(ctx, userID, spaceID); err != nil {
			return "", "", err
		}
	}
	return spaceID, p, nil
}

// newDAVFS 构造绑定 (user, space) 的 PG 后端文件系统。
func (d Deps) newDAVFS(ctx context.Context, userID, spaceID string) (*webdavfs.FileSystem, error) {
	if d.Files == nil || d.Files.DB == nil {
		return nil, apierr.Internal(errors.New("文件服务未装配"))
	}
	fs := &webdavfs.FileSystem{
		Spaces: d.Files.Spaces, Files: d.Files.Files, Svc: d.Files,
		Objects: d.Objects, DB: d.Files.DB,
		UserID: userID, SpaceID: spaceID,
	}
	// 根行 id 提前取一次:几乎所有操作都要从根开始逐段解析
	root, err := d.Files.Files.GetRoot(ctx, d.Files.DB, spaceID)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			return nil, apierr.SpaceGone("空间不存在或已被解散")
		}
		return nil, apierr.Internal(err)
	}
	fs.RootID = root.ID
	return fs, nil
}

// checkDAVConditions 校验 If-Match / If-None-Match(缺口 1)。
//
// 语义与 REST 侧的 `internal/condreq` **同源**:两处若各写一遍,迟早出现
// "REST 拒绝、WebDAV 放行"的覆盖冲突(而客户端正是靠这两条通道同时改写同一份数据)。
func (d Deps) checkDAVConditions(r *http.Request, fs *webdavfs.FileSystem, davPath string) error {
	hasIfMatch := r.Header.Get("If-Match") != ""
	hasIfNone := r.Header.Get("If-None-Match") != ""
	if !hasIfMatch && !hasIfNone {
		return nil
	}
	row, err := fs.Resolve(r.Context(), davPath)
	current := ""
	exists := err == nil
	if err != nil && !errors.Is(err, repo.ErrNotFound) {
		return apierr.Internal(err)
	}
	if exists {
		current = filesvc.QuoteETag(row.Etag)
	}
	res := condreq.Evaluate(r.Header, condreq.Current{Exists: exists, ETag: current})
	if res.Fail {
		// 412:与本项目 REST 侧同一业务码(precondition_failed),
		// 客户端据 reason 分流(刷新后重试 / 提示冲突)
		e := apierr.PreconditionFailed("%s", res.Message)
		if res.Reason != "" {
			e.WithDetail("reason", res.Reason)
		}
		if exists {
			e.WithDetail("server_etag", current)
			e.WithDetail("server_version", row.Version)
		}
		return e
	}
	return nil
}

// webdavPut 处理 PUT:请求内定稿(6.10 单一写路径),≤100MB(超限 413 提示走 TUS)。
//
// 为什么自己处理而不是交给上游 handlePut(见文件头注释):上游的写路径是
// OpenFile+Write+Close,而 finalize 的失败**必须**回给客户端 ——
// Close 的错误会被上游丢掉,于是"PUT 成功但内容是空的"会成为可能的输出。
func (d Deps) webdavPut(w http.ResponseWriter, r *http.Request, fs *webdavfs.FileSystem, davPath string) {
	if d.Finalizer == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("定稿服务未装配")))
		return
	}
	if davPath == "/" {
		apierr.Write(w, r, apierr.MethodNotAllowed("不能对空间根执行 PUT"))
		return
	}
	maxBytes := d.Cfg.Policy.WebDAVPutMaxBytes
	if r.ContentLength > maxBytes {
		apierr.Write(w, r, apierr.TooLarge(
			"WebDAV PUT 上限 %d 字节,本次 %d 字节;更大文件请走 TUS 分片上传",
			maxBytes, r.ContentLength))
		return
	}
	parentPath, name := webdavfs.SplitPath(davPath)
	parent, perr := fs.Resolve(r.Context(), parentPath)
	if perr != nil {
		if errors.Is(perr, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.Conflict(apierr.CodeNameConflict,
				"目标目录不存在: %s(WebDAV 不隐式建目录,请先 MKCOL)", parentPath))
			return
		}
		apierr.Write(w, r, apierr.Internal(perr))
		return
	}
	if !parent.IsDir {
		apierr.Write(w, r, apierr.Conflict(apierr.CodeNameConflict, "目标不是目录: %s", parentPath))
		return
	}
	// 判权:写权限在服务端判定(4.3);reader 不能 PUT
	if err := d.Files.CheckWritable(r.Context(), fs.UserID, fs.SpaceID); err != nil {
		apierr.Write(w, r, err)
		return
	}

	existing, gerr := fs.Resolve(r.Context(), davPath)
	existed := gerr == nil
	if gerr != nil && !errors.Is(gerr, repo.ErrNotFound) {
		apierr.Write(w, r, apierr.Internal(gerr))
		return
	}

	// 条件请求在**读 body 之前**判:否则会先把 100MB 收下来再告诉客户端 412
	if err := d.checkDAVConditions(r, fs, davPath); err != nil {
		apierr.Write(w, r, err)
		return
	}
	// 编辑锁:只拦**覆盖写**(BE-S6-05 纪律 1)。PUT 到不存在的位置是"新建",
	// 不涉及覆盖别人的编辑内容,因此只在目标已存在时检查。
	if existed {
		if lerr := d.Files.CheckLockForOverwrite(r.Context(), fs.UserID, existing.ID); lerr != nil {
			apierr.Write(w, r, lerr)
			return
		}
	}

	start := time.Now()
	body := http.MaxBytesReader(w, r.Body, maxBytes)
	res, ferr := d.Finalizer.Finalize(r.Context(), finalize.Input{
		UserID: fs.UserID, SpaceID: fs.SpaceID, ParentID: parent.ID,
		Name: name, DeclaredSize: r.ContentLength,
		// PUT 的语义就是"写入并替换" ⇒ 显式声明覆盖意图(6.7:绝不静默覆盖,
		// 但这里"覆盖"是 HTTP 方法本身的定义,不是实现擅自决定的)
		AllowOverwrite: true,
		Content:        body,
	})
	d.auditAction(r, "webdav.put", fs.SpaceID, "file", existingID(existing, name), ferr, r.ContentLength, time.Since(start))
	if ferr != nil {
		apierr.Write(w, r, ferr)
		return
	}
	// 事件推送:PUT 也是一次内容变更(客户端据此刷新)
	d.publishSeq(r.Context(), res.File.SpaceID, res.File.ID, feedKindForPut(res.NewFile), res.ChangeSeq)

	if et := filesvc.QuoteETag(res.File.Etag); et != "" {
		w.Header().Set("ETag", et)
	}
	// 201 = 新建,204 = 覆盖(6.2 的 PUT 语义)
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

func existingID(f *model.File, fallback string) string {
	if f == nil {
		return fallback
	}
	return f.ID
}

func feedKindForPut(newFile bool) string {
	if newFile {
		return model.FeedCreated
	}
	return model.FeedUpdated
}

// parseDestination 把 MOVE/COPY 的 Destination 头归一为**空间内 DAV 路径**。
//
// 三种形态都要接受(6.2 联调清单):
//   - 绝对 URL:`http://host/webdav/{space}/a/b`(资源管理器)
//   - 带前缀的相对路径:`/webdav/{space}/a/b`
//   - 纯相对路径:`/a/b` 或 `a/b`(部分客户端)
//
// 不做归一时 Destination 会带着 `/webdav/{space}` 前缀进上游,于是它被当成
// 文件路径的一部分 —— 表现为"移动到了不存在的地方"或 404。
func (d Deps) parseDestination(r *http.Request, spaceID string) (string, error) {
	raw := strings.TrimSpace(r.Header.Get("Destination"))
	if raw == "" {
		return "", apierr.BadRequest(apierr.CodeInvalidArgument, "缺少 Destination 头")
	}
	p := raw
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		// 绝对 URL 或带路径的相对 URL:只取 Path(**丢掉 scheme/host/query**,
		// 否则外部域名会被当成路径的一部分)
		p = u.Path
	}
	p = strings.TrimPrefix(p, "/webdav/"+spaceID)
	p = strings.TrimPrefix(p, "/webdav")
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	// 目标只能是本空间内的路径:跨空间复制语义上等于"共享到别的空间",
	// 那是另一条接口(有独立的权限与配额判定),不能从 WebDAV 悄悄做掉
	if strings.Contains(p, "..") {
		return "", apierr.BadRequest(apierr.CodeInvalidArgument, "Destination 含非法路径段")
	}
	return p, nil
}

func cloneURL(u *url.URL) *url.URL {
	c := *u
	return &c
}

// webdavCopy 处理 COPY:走 filesvc.Copy(**逻辑复制 + 引用 +1**),不搬一个字节。
//
// 为什么不用上游的 COPY:上游把复制实现成"读源、写目标"(逐文件全量读写),
// 而本项目是内容寻址 + 引用计数 —— 复制一份只该多一行元数据与一次 ref+1。
//
// Overwrite 语义(6.2):
//
//	目标不存在                    → 201
//	目标存在 + Overwrite: T(默认) → 204(覆盖)
//	目标存在 + Overwrite: F       → **412**(RFC 4918 的规定,也是客户端"不要覆盖我"
//	                               的意图,必须照做 —— 静默覆盖是数据丢失)
func (d Deps) webdavCopy(w http.ResponseWriter, r *http.Request, fs *webdavfs.FileSystem, spaceID, davPath string) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	src, serr := fs.Resolve(r.Context(), davPath)
	if serr != nil {
		if errors.Is(serr, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.NotFound("源不存在: %s", davPath))
			return
		}
		apierr.Write(w, r, apierr.Internal(serr))
		return
	}
	dstPath, derr := d.parseDestination(r, spaceID)
	if derr != nil {
		apierr.Write(w, r, derr)
		return
	}
	if dstPath == "/" {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "不能覆盖空间根"))
		return
	}
	dstParentPath, dstName := webdavfs.SplitPath(dstPath)
	dstParent, perr := fs.Resolve(r.Context(), dstParentPath)
	if perr != nil {
		if errors.Is(perr, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.Conflict(apierr.CodeNameConflict, "目标目录不存在: %s", dstParentPath))
			return
		}
		apierr.Write(w, r, apierr.Internal(perr))
		return
	}
	if !dstParent.IsDir {
		apierr.Write(w, r, apierr.Conflict(apierr.CodeNameConflict, "目标不是目录: %s", dstParentPath))
		return
	}
	existing, gerr := fs.Resolve(r.Context(), dstPath)
	existed := gerr == nil
	if gerr != nil && !errors.Is(gerr, repo.ErrNotFound) {
		apierr.Write(w, r, apierr.Internal(gerr))
		return
	}
	// Overwrite: F 且目标存在 → 412(必须在**动手之前**判,否则已经覆盖了)
	if existed && strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F") {
		apierr.Write(w, r, apierr.PreconditionFailed("目标已存在且 Overwrite: F: %s", dstPath))
		return
	}
	// 覆盖:先删掉目标(内容寻址下删除只减引用;不删则复制会因同名 409)
	if existed {
		if _, delErr := d.Files.Delete(r.Context(), fs.UserID, existing.ID); delErr != nil {
			apierr.Write(w, r, delErr)
			return
		}
	}
	start := time.Now()
	res, cerr := d.Files.Copy(r.Context(), filesvc.CopyInput{
		UserID: fs.UserID, FileID: src.ID,
		TargetParentID: dstParent.ID, NewName: dstName,
	})
	d.auditAction(r, "webdav.copy", spaceID, "file", src.ID, cerr, src.Size, time.Since(start))
	if cerr != nil {
		apierr.Write(w, r, cerr)
		return
	}
	if res.Root != nil && res.Root.SpaceID != "" {
		d.publishWrite(r.Context(), res.Root.SpaceID, res.Root.ID, model.FeedCreated)
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// webdavMove 处理 MOVE:走 filesvc.Move(子树深度级联 + 变更流 + 乐观锁)。
//
// 为什么不让上游处理:**上游把 `FileSystem.Rename` 的任何错误都映射成 403**
// (`net/webdav/file.go` 的 moveFiles)。那意味着:
//   - 同目录重名 / 版本冲突(409)→ 客户端看到"没有权限",于是它不会提示用户
//     "目标已存在",而是让用户去找管理员;
//   - 源不存在(404)→ 同样变 403,客户端以为是权限问题而不会清理本地记录。
//
// 状态码是**客户端行为的开关**,映射错了就是把用户带向错误的处置动作。
//
// Overwrite 语义与 COPY 一致(目标存在 + Overwrite: F → 412)。
func (d Deps) webdavMove(w http.ResponseWriter, r *http.Request, fs *webdavfs.FileSystem, spaceID, davPath string) {
	if d.Files == nil {
		apierr.Write(w, r, apierr.Internal(errors.New("文件服务未装配")))
		return
	}
	src, serr := fs.Resolve(r.Context(), davPath)
	if serr != nil {
		if errors.Is(serr, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.NotFound("源不存在: %s", davPath))
			return
		}
		apierr.Write(w, r, apierr.Internal(serr))
		return
	}
	dstPath, derr := d.parseDestination(r, spaceID)
	if derr != nil {
		apierr.Write(w, r, derr)
		return
	}
	if dstPath == "/" || dstPath == davPath {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument,
			"目标与源相同或指向空间根: %s", dstPath))
		return
	}
	dstParentPath, dstName := webdavfs.SplitPath(dstPath)
	dstParent, perr := fs.Resolve(r.Context(), dstParentPath)
	if perr != nil {
		if errors.Is(perr, repo.ErrNotFound) {
			apierr.Write(w, r, apierr.Conflict(apierr.CodeNameConflict, "目标目录不存在: %s", dstParentPath))
			return
		}
		apierr.Write(w, r, apierr.Internal(perr))
		return
	}
	if !dstParent.IsDir {
		apierr.Write(w, r, apierr.Conflict(apierr.CodeNameConflict, "目标不是目录: %s", dstParentPath))
		return
	}
	dst, gerr := fs.Resolve(r.Context(), dstPath)
	existed := gerr == nil
	if gerr != nil && !errors.Is(gerr, repo.ErrNotFound) {
		apierr.Write(w, r, apierr.Internal(gerr))
		return
	}
	if existed {
		// Overwrite 缺省为 T(RFC 4918:不带头等价于 Overwrite: T)
		if strings.EqualFold(strings.TrimSpace(r.Header.Get("Overwrite")), "F") {
			apierr.Write(w, r, apierr.PreconditionFailed("目标已存在且 Overwrite: F: %s", dstPath))
			return
		}
		// 覆盖:先删目标(否则 Move 会因同名返回 409)
		if _, delErr := d.Files.Delete(r.Context(), fs.UserID, dst.ID); delErr != nil {
			apierr.Write(w, r, delErr)
			return
		}
	}
	start := time.Now()
	res, merr := d.Files.Move(r.Context(), filesvc.MoveInput{
		UserID: fs.UserID, FileID: src.ID,
		NewParentID: dstParent.ID, NewName: dstName,
	})
	d.auditAction(r, "webdav.move", spaceID, "file", src.ID, merr, src.Size, time.Since(start))
	if merr != nil {
		apierr.Write(w, r, merr)
		return
	}
	// WebDAV 的 MOVE 是**同步语义**(客户端要求返回时目标已就位),而 6.11 的
	// 异步阈值对超大树仍然生效 —— 此时我们无法在一个响应里"等它完成"。
	// 取舍:返回 202 而不是假装成功。假装成功会让 WebDAV 客户端认为目标已存在,
	// 随后它访问目标得到 404(甚至把本地文件也删掉)。
	if res.Async {
		apierr.WriteOK(w, r, http.StatusAccepted, res)
		return
	}
	if res.Entry != nil && res.Entry.SpaceID != "" {
		d.publishWrite(r.Context(), res.Entry.SpaceID, res.Entry.ID, model.FeedMoved)
	}
	if existed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusCreated)
}
