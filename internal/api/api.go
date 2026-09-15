// Package api 组装 HTTP 入口:标准库 ServeMux(Go 1.22+) + func(http.Handler) 中间件链(6.8)。
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/apierr"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/middleware"
	"github.com/netdisk/netdisk/internal/orgsvc"
	"github.com/netdisk/netdisk/internal/ratelimit"
	"github.com/netdisk/netdisk/internal/reqctx"
	"github.com/netdisk/netdisk/internal/sharesvc"
	"github.com/netdisk/netdisk/internal/usersvc"
	"github.com/netdisk/netdisk/internal/spacesvc"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/syncsse"
	"github.com/netdisk/netdisk/internal/uploadsvc"
	"github.com/netdisk/netdisk/internal/webdavauth"
	"github.com/netdisk/netdisk/internal/webui"
)

// Deps 是路由所需的依赖。后续模块(handler 包)在此基础上扩展。
type Deps struct {
	Cfg     *config.Config
	Log     *slog.Logger
	DB      *db.DB
	Redis   *redis.Client
	Tokens  *auth.Manager
	Cache   *cache.Client
	Limiter *ratelimit.Limiter
	// Auth 是认证用例服务(login/refresh/logout);为 nil 时相关端点返回 500
	Auth  *authsvc.Service
	// Uploads 是上传任务用例服务(建任务/预留/取消/续传偏移);为 nil 时相关端点返回 500
	Uploads *uploadsvc.Service
	// WebDAV 是 WebDAV Basic 认证通道(BE-S1-06);为 nil 时 /webdav/ 返回 500
	WebDAV *webdavauth.Authenticator
	// Org 是组织架构(部门树)用例服务(BE-S3-01);为 nil 时相关端点返回 500
	Org *orgsvc.Service
	// Files 是文件元数据用例服务(BE-S4-05);为 nil 时相关端点返回 500
	Files *filesvc.Service
	// Spaces 是空间与团队成员用例服务(BE-S3-04/07);为 nil 时相关端点返回 500
	Spaces *spacesvc.Service
	// TUS 是 TUS 协议数据面(BE-S5-03);为 nil 时 /tus/* 返回 500
	TUS *uploadsvc.TUSService
	// Objects 是对象存储(下载流式读取用,BE-S6-06);为 nil 时下载端点返回 500
	Objects storage.Storager
	// Events 是 SSE 事件通道的**连接治理与本地扇出**(BE-S9-04);为 nil 时 /events 返回 500
	Events *syncsse.Hub
	// Broadcaster 是"把事件推出去"的出口(本地 + Redis 跨实例);为 nil 时退回只本地扇出
	Broadcaster syncsse.Broadcaster
	// Finalizer 是统一定稿路径(6.10);WebDAV PUT 与 MOVE/DELETE 的请求内定稿走它
	Finalizer Finalizer
	// Shares 是分享链接用例服务(BE-S6-04);为 nil 时相关端点返回 500
	Shares *sharesvc.Service
	// DirOps 是目录级异步任务队列(BE-S7-02);为 nil 时 /api/v1/tasks/{id} 返回 500
	DirOps *dirops.Queue
	// UserAdmin 是后台用户管理用例入口(FE-W-04:列表/建号/启停用/角色调整)。
	// 为 nil 时三个 /admin/users 端点返 500(而不是静默成功)。
	UserAdmin *usersvc.Service
}

// Finalizer 是定稿能力(finalize.Service 实现)。
//
// 声明成窄接口而不是直接依赖 *finalize.Service:handler 只需要"给我一个定稿函数",
// 而 TUS/WebDAV/multipart 三条入口本来就共用同一个实现(6.10 单一写路径)。
type Finalizer interface {
	Finalize(ctx context.Context, in finalize.Input) (*finalize.Result, error)
}

// New 构造根 handler。
func New(d Deps) http.Handler {
	mux := http.NewServeMux()

	// ---- 健康与版本(无鉴权;供 systemd/Prometheus/负载探测)----
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		status := map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339)}
		code := http.StatusOK
		if d.DB != nil {
			if err := d.DB.Ping(ctx); err != nil {
				status["postgres"] = "down"
				status["status"] = "degraded"
				code = http.StatusServiceUnavailable
			} else {
				status["postgres"] = "up"
			}
		}
		if d.Redis != nil {
			if err := d.Redis.Ping(ctx).Err(); err != nil {
				status["redis"] = "down"
				status["status"] = "degraded"
				code = http.StatusServiceUnavailable
			} else {
				status["redis"] = "up"
			}
		}
		apierr.WriteOK(w, r, code, status)
	})

	// GET /api/v1/version:桌面端据此协商"最低支持客户端版本"(11 章版本与发布)
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		apierr.WriteOK(w, r, http.StatusOK, map[string]any{
			"server_version":       BuildVersion,
			"min_client_version":   MinClientVersion,
			"api_contract_version": APIContractVersion,
			"change_seq_global":    true,
			"cursor_expire_days":   d.Cfg.Policy.SyncFeedKeepDays,
		})
	})

	// ---- SSE 长连接(BE-S9-04 / 6.9)----
	// 事件是**尽力而为**的加速通道:丢了客户端靠 /changes 游标补拉(权威路径)。
	// 关键响应头(X-Accel-Buffering: no)在 handler 里设 —— Nginx 缓冲会让事件
	// 延迟数十秒才到达,而那种"看起来连着、就是不实时"最难排查。
	mux.Handle("GET /api/v1/events", middleware.Auth(d.Tokens)(http.HandlerFunc(d.handleEvents)))

	// ---- 静态资源:/admin 由 Go embed 提供(ADR-4 / R-01 R-02)----
	// 注意:只需注册 "/admin/{rest...}" —— 它同时匹配 "/admin/" 与更深路径;
	// 若再注册 "/admin/" 会被 ServeMux 判定为模式冲突(启动即 panic,这是 6.8 期望的行为)。
	admin := webui.Handler(d.Cfg.WebUI.AdminPrefix, "admin", true)
	mux.Handle("GET "+d.Cfg.WebUI.AdminPrefix+"{rest...}", admin)

	// ---- 受保护示例端点:用于验证鉴权链与 audience 分端 ----
	authed := middleware.Auth(d.Tokens) // 不限 audience
	mux.Handle("GET /api/v1/me", authed(http.HandlerFunc(handleMe)))

	// ---- 限速(3.2 职责 7):按接口族独立阈值;未注入 Limiter 时自动跳过 ----
	// 认证类接口按 IP 限速(未登录无 userID),文件类按用户限速。
	loginLimit := d.rateLimit("login")
	mux.Handle("POST /api/v1/auth/login", loginLimit(http.HandlerFunc(d.handleLogin)))
	mux.Handle("POST /api/v1/auth/refresh", http.HandlerFunc(d.handleRefresh))
	mux.Handle("POST /api/v1/auth/logout", http.HandlerFunc(d.handleLogout))

	// 登录页可用的平台列表(FE-W-08):**公开** —— 用户此时还没有令牌。
	// 只回 kind/name/corp_id(公开标识),绝不回 secret;限速与登录口同档,
	// 避免被用来枚举"这台机器上配了哪些企业"。
	// (IdP/SSO 已移除,本接口不再暴露。)

	adminOnly := middleware.Chain(
		http.HandlerFunc(handleAdminProbe),
		middleware.Auth(d.Tokens, auth.AudienceWeb), // H5 token 不得访问后台(2.7)
		middleware.RequireRole("super_admin", "dept_admin"),
	)
	mux.Handle("GET /api/v1/admin/ping", adminOnly)

	// adminChain 是"web audience + 管理员角色"的统一链,供全部 /admin/* 使用。
	//
	// 必须限定 audience:**H5 与桌面端令牌即使角色是管理员也不得访问后台**(2.7)——
	// 分端的意义在于"令牌在哪个客户端泄露,损失就限制在那个客户端的能力范围内"。
	adminChain := func(h http.HandlerFunc) http.Handler {
		return middleware.Chain(h, middleware.Auth(d.Tokens, auth.AudienceWeb),
			middleware.RequireRole("super_admin", "dept_admin"))
	}

	// ---- 组织架构(部门树)后台接口(BE-S3-01)----
	// 组织树是权限的输入(它决定谁能看到哪些团队空间),不能让 H5/桌面端令牌改动。
	mux.Handle("GET /api/v1/admin/departments", adminChain(d.handleDeptTree))
	mux.Handle("POST /api/v1/admin/departments", adminChain(d.handleDeptCreate))
	mux.Handle("DELETE /api/v1/admin/departments/{id}", adminChain(d.handleDeptDelete))
	mux.Handle("GET /api/v1/admin/departments/{id}/subtree", adminChain(d.handleDeptSubtree))
	// 重建闭包表:幂等,组织同步后调用(post 语义:它改变了派生索引)
	mux.Handle("POST /api/v1/admin/departments/rebuild-closure", adminChain(d.handleDeptRebuild))

	// ---- 用户管理后台接口(FE-W-04)----
	// 与部门同理:用户与角色是权限的输入。停用会在服务层**立刻**吊销会话
	// (吊销 refresh + 自增 token_version),不是"只改一个字段"。
	mux.Handle("GET /api/v1/admin/users", adminChain(d.handleAdminUserList))
	mux.Handle("POST /api/v1/admin/users", adminChain(d.handleAdminUserCreate))
	mux.Handle("PATCH /api/v1/admin/users/{id}", adminChain(d.handleAdminUserUpdate))

	// ---- 上传任务(4.3 预留 + 6.10 单一上传通道)----
	//
	// **中间件顺序:鉴权在内、限速在外(写作 `auth(limit(h))`)**。
	//
	// 理由是限速的"主体"选择:已认证请求按 userID 限速,未认证才按 IP 兜底
	// (否则同一 NAT 后的不同用户会互相挤占配额)。userID 只有**鉴权中间件跑过
	// 之后**才在 context 里 —— 顺序反了限速只看得到 IP;更糟的是 scope 未开 ByIP
	// 时会因"既无 userID 也无 IP 主体"直接返回 500。
	//
	// 这个顺序错误**只在注入真实 Cache/Limiter 时暴露**:单测里限速中间件因依赖
	// 未装配而透传,整条链看不出异常。实测就是靠真实二进制跑端到端才发现的。
	uploadLimit := d.rateLimit("upload")
	uploadAuth := middleware.Auth(d.Tokens)
	mux.Handle("POST /api/v1/upload/create", uploadAuth(uploadLimit(http.HandlerFunc(d.handleUploadCreate))))
	mux.Handle("DELETE /api/v1/upload/{id}", uploadAuth(http.HandlerFunc(d.handleUploadCancel)))
	mux.Handle("HEAD /api/v1/upload/{id}", uploadAuth(http.HandlerFunc(d.handleUploadHead)))
	// 秒传定稿(BE-S4-04):无内容流,靠持物证明通过后直接复用已存对象。
	// 与其它上传路径同一档限速;证明失败时 handler 会**额外扣配额**(防猜样本)。
	mux.Handle("POST /api/v1/upload/{id}/finish", uploadAuth(uploadLimit(http.HandlerFunc(d.handleUploadFinish))))

	// ---- 文件元数据(BE-S4-05)----
	// 列表按用户限速(读路径高频);详情/HEAD 用更宽的一档(同步前探测)
	fileRead := middleware.Auth(d.Tokens)
	mux.Handle("GET /api/v1/files", fileRead(d.rateLimit("file_list")(http.HandlerFunc(d.handleFileList))))
	mux.Handle("GET /api/v1/files/{id}", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleFileGet))))
	mux.Handle("HEAD /api/v1/files/{id}", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleFileHead))))
	// 下载(BE-S6-06):流式 + Range + 条件请求;HEAD 用于同步前比对 ETag
	mux.Handle("GET /api/v1/files/{id}/content", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleFileDownload))))
	mux.Handle("HEAD /api/v1/files/{id}/content", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleFileDownloadHead))))
	// 写操作(BE-S6-01/02):改名 / 移动 / 删除 + 删除前影响面预检(BE-S7-01)
	// 建目录(BE-S6-01 补):令牌可用的 REST 入口,补 WebDAV MKCOL 走 Basic 账密的缺口
	mux.Handle("POST /api/v1/files/dirs", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileCreateDir))))
	mux.Handle("PATCH /api/v1/files/{id}", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileRename))))
	mux.Handle("POST /api/v1/files/{id}/move", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileMove))))
	mux.Handle("DELETE /api/v1/files/{id}", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileDelete))))
	mux.Handle("GET /api/v1/files/{id}/subtree-stats", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleSubtreeStats))))

	// 目录级异步任务轮询(BE-S7-02 / 6.11 API 表:"任务 GET /api/v1/tasks/:id")
	mux.Handle("GET /api/v1/tasks/{id}", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleTaskGet))))

	// 增量变更流(BE-S9-02/03):同步主路径 —— 客户端常态只读增量,
	// 全量扫描只在首次配置目录与游标超窗(409 cursor_expired)时发生。
	mux.Handle("GET /api/v1/changes", fileRead(d.rateLimit("file_list")(http.HandlerFunc(d.handleChanges))))
	mux.Handle("GET /api/v1/changes/head", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleChangesHead))))
	mux.Handle("POST /api/v1/sync/cursors", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleCursorReport))))

	// ---- 编辑锁(BE-S6-05:REST 锁,替代 WebDAV LOCK)----
	// 为什么不用 x/net/webdav 的锁:它是内存实现(重启即失、多实例不共享),
	// 用它保护编辑等于"锁随时可能消失,而客户端以为还锁着"。
	mux.Handle("POST /api/v1/files/{id}/lock", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileLock))))
	mux.Handle("GET /api/v1/files/{id}/lock", fileRead(d.rateLimit("file_read")(http.HandlerFunc(d.handleFileLockGet))))
	mux.Handle("DELETE /api/v1/files/{id}/lock", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileUnlock))))

	// ---- 分享链接(BE-S6-04:系统唯一的免登录出口)----
	// 创建/管理需要登录;meta 与下载**免 JWT**(落地页访问者没有账号),
	// 因此两条免登录路由走登录那档限速(按来源 IP 收得最紧)—— 密码校验是暴力破解面。
	mux.Handle("POST /api/v1/shares", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleShareCreate))))
	mux.Handle("GET /api/v1/shares", fileRead(d.rateLimit("file_list")(http.HandlerFunc(d.handleShareList))))
	mux.Handle("DELETE /api/v1/shares/{id}", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleShareRevoke))))
	mux.HandleFunc("GET /api/v1/shares/{token}/meta", d.handleShareMeta)
	// 落地页下载走登录那档限速(按来源 IP,收得最紧):密码校验是暴力破解面,
	// 而访问者没有账号 —— 不限速等于送出一个可无限试密码的接口。
	shareLimit := d.rateLimit("login")
	mux.Handle("POST /api/v1/shares/{token}/download", shareLimit(http.HandlerFunc(d.handleShareDownload)))
	mux.Handle("GET /api/v1/shares/{token}/download", shareLimit(http.HandlerFunc(d.handleShareDownload)))
	// 共享到空间(BE-S6-03)与复制(BE-S7-03):都是"引用 +1"路径 —— 只新增元数据行,
	// 内容靠内容寻址复用(ref_count+1),因此不额外占磁盘,但**按逻辑行占配额**。
	//
	// 路径与架构文档 6.3 的接口表**逐字一致**(`share-to-space`),便于对照验收;
	// 复制用对称的短名 `copy`(文档 6.11 规定了 COPY 语义,接口表 V3.0 未登记该行)。
	mux.Handle("POST /api/v1/files/{id}/share-to-space", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileShare))))
	mux.Handle("POST /api/v1/files/{id}/copy", fileRead(d.rateLimit("file_write")(http.HandlerFunc(d.handleFileCopy))))

	// ---- 空间与团队成员(BE-S3-04)----
	// 判权在 spacesvc 内完成(4.3);这里只做鉴权(必须登录)
	// ---- 团队空间协作(FE-W-11 / 4.3)----
	//
	// 创建/查询/解散/转让/邀请。**入口在桌面端与 H5**(后台只做全局治理),
	// 两边调同一组接口。解散是硬删(4.5 无回收站),前端必须二次确认。
	mux.Handle("GET /api/v1/spaces", fileRead(http.HandlerFunc(d.handleSpaceListMine)))
	mux.Handle("POST /api/v1/spaces", fileRead(http.HandlerFunc(d.handleSpaceCreate)))
	mux.Handle("DELETE /api/v1/spaces/{id}", fileRead(http.HandlerFunc(d.handleSpaceDissolve)))
	mux.Handle("POST /api/v1/spaces/{id}/transfer", fileRead(http.HandlerFunc(d.handleSpaceTransfer)))
	// 邀请:文档口径是 POST /members,实现里 PUT 是幂等 upsert;两者同义
	mux.Handle("POST /api/v1/spaces/{id}/members", fileRead(http.HandlerFunc(d.handleSpaceInvite)))

	mux.Handle("GET /api/v1/spaces/{id}/members", fileRead(http.HandlerFunc(d.handleMemberList)))
	mux.Handle("PUT /api/v1/spaces/{id}/members", fileRead(http.HandlerFunc(d.handleMemberUpsert)))
	mux.Handle("DELETE /api/v1/spaces/{id}/members/{userId}", fileRead(http.HandlerFunc(d.handleMemberRemove)))
	mux.Handle("POST /api/v1/spaces/{id}/leave", fileRead(http.HandlerFunc(d.handleSpaceLeave)))

	// ---- 管理员治理(S3-07):与部门接口同一条 web+管理员 链 ----
	// 冻结/解冻:走**治理**实现(直连仓储 + 审计),不依赖 spacesvc 的成员视角
	// —— 管理员通常不是空间成员,用成员视角的服务会出现"冻结不了别人的空间"。
	mux.Handle("POST /api/v1/admin/spaces/{id}/freeze", adminChain(d.handleAdminSpaceFreeze))

	// ---- 空间治理(FE-W-06 / 4.3)----
	// 只做**全局治理**(配额/预警阈值/冻结/收回);创建/邀请/退出属协作管理,
	// 入口在桌面端与 H5(4.3 决策)。
	mux.Handle("GET /api/v1/admin/spaces", adminChain(d.handleAdminSpaceList))
	mux.Handle("PATCH /api/v1/admin/spaces/{id}/quota", adminChain(d.handleAdminSpaceQuota))
	mux.Handle("POST /api/v1/admin/spaces/{id}/revoke", adminChain(d.handleAdminSpaceRevoke))

	// ---- TUS 上传协议(BE-S5-03:6.1 / 3.4)----
	//
	// OPTIONS 免认证:客户端在有任何凭据之前就要探测服务端能力。
	// 其余方法走鉴权(access token)+ upload ticket 双凭据:令牌决定"你是谁",
	// ticket 决定"你有权写哪个 upload"——两者绑定缺一不可(6.3)。
	tusAuth := middleware.Auth(d.Tokens)
	tusLimit := d.rateLimit("upload")
	mux.HandleFunc("OPTIONS /tus", d.handleTUSOptions)
	// 鉴权在内、限速在外(理由见上面上传任务处的注释)
	mux.Handle("POST /tus", tusAuth(tusLimit(http.HandlerFunc(d.handleTUSCreate))))
	mux.Handle("HEAD /tus/{id}", tusAuth(http.HandlerFunc(d.handleTUSHead)))
	// PATCH 按"次"限速会误伤分片上传(一个大文件几百个分片),
	// 因此用更宽的一档;同样保持鉴权在内
	mux.Handle("PATCH /tus/{id}", tusAuth(d.rateLimit("webdav")(http.HandlerFunc(d.handleTUSPatch))))
	mux.Handle("DELETE /tus/{id}", tusAuth(http.HandlerFunc(d.handleTUSDelete)))

	// ---- WebDAV(BE-S1-06:Basic 认证通道;方法集见 BE-S8)----
	// 单独注册各方法而不是 "/webdav/{rest...}":ServeMux 不允许同一个模式
	// 混用方法限定的通配与无限定注册,逐方法列出也让"支持哪些 DAV 方法"一目了然。
	for _, m := range []string{
		http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodOptions,
		http.MethodHead, http.MethodPost, http.MethodPatch,
		"PROPFIND", "PROPPATCH", "MKCOL", "COPY", "MOVE", "LOCK", "UNLOCK", "REPORT",
	} {
		mux.Handle(m+" /webdav/{rest...}", d.rateLimit("webdav")(http.HandlerFunc(d.handleWebDAV)))
	}
	// OPTIONS 需要免认证应答(客户端靠它探测 DAV 能力),这里给一条独立处理:
	// 仍走认证,但匿名 OPTIONS 返回 401+DAV 头而不是 403,便于客户端继续握手。
	mux.HandleFunc("OPTIONS /webdav", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("DAV", "1, 2")
		w.Header().Set("MS-Author-Via", "DAV")
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, DELETE, PROPFIND, PROPPATCH, MKCOL, COPY, MOVE")
		w.WriteHeader(http.StatusOK)
	})

	// ---- 根路径:避免暴露内部信息 ----
	//
	// **这里刻意只注册精确的 "/",不再注册 "/" 通配兜底。**
	//
	// 原因(实测缺陷):注册 `mux.HandleFunc("/", ...)` 会匹配一切未命中路径,
	// 于是"路径对、方法不对"的请求(POST /api/v1/files、DELETE /api/v1/version)
	// 都会被这个兜底接走并返回 404 —— 把 ServeMux 内建的
	// **405 + Allow** 语义彻底吞掉。客户端因此无法区分"接口不存在"与
	// "接口存在但你用错了方法",排查方向会被带偏(典型的"明明有这接口却 404")。
	//
	// 不注册通配兜底后,未匹配路径由 ServeMux 返回 405(有 Allow)或 404,
	// 两种响应体再由 middleware.StructuredErrors 统一转成结构化 JSON。
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		apierr.WriteOK(w, r, http.StatusOK, map[string]string{"service": "netdisk", "docs": "/admin/"})
	})

	// 中间件顺序:requestID → realIP → structuredErrors → logging → recoverer
	//
	// StructuredErrors 必须在内层:它要能在 ServeMux 自己写出 405/404 之后
	// 改写响应体,而 Logging/Recoverer 关注的是状态码,放在它外层即可。
	return middleware.Chain(mux,
		middleware.RequestID(),
		middleware.RealIP(d.Cfg.Server.TrustedProxies),
		middleware.StructuredErrors(d.Log),
		middleware.Logging(d.Log),
		middleware.Recoverer(d.Log),
	)
}

// 版本信息:由 CI 通过 -ldflags 注入(11 章:单一 tag 注入三端)
var (
	BuildVersion       = "dev"
	MinClientVersion   = "0.0.0"
	APIContractVersion = "v1"
)

func handleMe(w http.ResponseWriter, r *http.Request) {
	a := reqctx.ActorFrom(r.Context())
	if a == nil {
		apierr.Write(w, r, apierr.Unauthorized(apierr.CodeUnauthorized, "未认证"))
		return
	}
	// 注意:响应里刻意不含任何权限列表(R-13:权限由服务端按 space 判定)
	apierr.WriteOK(w, r, http.StatusOK, map[string]any{
		"user_id":    a.UserID,
		"username":   a.Username,
		"role":       a.Role,
		"audience":   a.Audience,
		"request_id": reqctx.RequestID(r.Context()),
	})
}

func handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	a := reqctx.ActorFrom(r.Context())
	if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "role": a.Role}); err != nil {
		apierr.Write(w, r, apierr.Internal(err))
	}
}

// rateLimit 返回限速中间件;未注入 Cache/Limiter 时返回透传中间件,
// 使单元测试与"无 Redis 的降级场景"都能构建路由。
//
// family 是**接口族**名(login/upload/file_list/...),阈值来自
// `config.RateLimits.For(family)` —— 各接口族独立阈值且可配(BE-S0-08 验收)。
// 未登记的族回落到 default,而不是"不限速":限速缺失通常只在被打时才暴露。
func (d Deps) rateLimit(family string) middleware.Middleware {
	if d.Cache == nil || d.Limiter == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return middleware.RateLimit(d.Cache, d.Limiter, d.rateLimitScope(family))
}

// rateLimitScope 返回某接口族的限速作用域。
//
// **唯一来源**:限速中间件与"失败惩罚"(middleware.Penalize)必须用同一个 scope,
// 否则惩罚扣的是另一个额度桶 —— 表现为"惩罚看起来执行了,但攻击者一点不受影响"。
func (d Deps) rateLimitScope(family string) middleware.Scope {
	rl := d.Cfg.RateLimits.For(family)
	return middleware.Scope{
		Name:      family,
		PerSecond: ratelimit.Limit{Window: time.Second, Max: int64(rl.PerSecond)},
		PerMinute: ratelimit.Limit{Window: time.Minute, Max: int64(rl.PerMinute)},
		Cost:      rl.Cost,
		ByIP:      rl.ByIP,
	}
}

// handleNotImplemented 用于尚未实现的端点:返回 501 而不是静默 200,
// 避免客户端把"没实现"当成"成功但空数据"。
func handleNotImplemented(what string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apierr.Write(w, r, apierr.BadRequest(apierr.CodeInvalidArgument, "%s 尚未实现", what))
	}
}
