// netdisk 服务端唯一入口(3.3:单二进制,systemd 管理)。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/netdisk/netdisk/internal/api"
	"github.com/netdisk/netdisk/internal/auth"
	"github.com/netdisk/netdisk/internal/authsvc"
	"github.com/netdisk/netdisk/internal/cache"
	"github.com/netdisk/netdisk/internal/config"
	"github.com/netdisk/netdisk/internal/credentials"
	"github.com/netdisk/netdisk/internal/db"
	"github.com/netdisk/netdisk/internal/dirops"
	"github.com/netdisk/netdisk/internal/fastupload"
	"github.com/netdisk/netdisk/internal/filesvc"
	"github.com/netdisk/netdisk/internal/finalize"
	"github.com/netdisk/netdisk/internal/lifecycle"
	"github.com/netdisk/netdisk/internal/migrate"
	"github.com/netdisk/netdisk/internal/namepolicy"
	"github.com/netdisk/netdisk/internal/obs"
	"github.com/netdisk/netdisk/internal/orgsvc"
	"github.com/netdisk/netdisk/internal/patrol"
	"github.com/netdisk/netdisk/internal/quotareconcile"
	"github.com/netdisk/netdisk/internal/ratelimit"
	"github.com/netdisk/netdisk/internal/repo"
	"github.com/netdisk/netdisk/internal/sharesvc"
	"github.com/netdisk/netdisk/internal/spacesvc"
	"github.com/netdisk/netdisk/internal/storage"
	"github.com/netdisk/netdisk/internal/syncfeed"
	"github.com/netdisk/netdisk/internal/syncsse"
	"github.com/netdisk/netdisk/internal/uploadsvc"
	"github.com/netdisk/netdisk/internal/usersvc"
	"github.com/netdisk/netdisk/internal/webdavauth"
)

// webdavVerify 把 authsvc 的账密校验适配成 webdavauth 需要的形状。
//
// 复用 authsvc.VerifyCredentials 而不是自己查库:防枚举、失败锁定、停用检查
// 只应有一份实现,否则 WebDAV 通道很容易成为一个绕过锁定策略的后门。
func webdavVerify(svc *authsvc.Service) webdavauth.VerifyDBCredentials {
	return func(ctx context.Context, username, password string) (string, int64, error) {
		u, err := svc.VerifyCredentials(ctx, username, password)
		if err != nil || u == nil {
			// 对外统一为"认证失败",不区分用户不存在与口令错误(防枚举)
			return "", 0, webdavauth.ErrUnauthorized
		}
		return u.ID, u.TokenVersion, nil
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "netdisk: %v\n", err)
		os.Exit(1)
	}
}

// envOr 读环境变量,为空时返回默认值(初始管理员播种的可配置入口)。
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// objectStorager 汇总各服务对"对象存储"的全部能力需求。
//
// 为什么在 main.go 里定义一个本地接口,而不是直接用 `*storage.FS` 或 gostorage.Adapter:
//   - 用具体类型会让"换后端"变成改一片调用点(本地 FS 与适配器是两个类型);
//   - 在装配点声明**窄接口**,装配处就是唯一需要知道"当前是哪个后端"的地方,
//     业务服务只依赖方法集合(与 depsguard 那条"内核不依赖 HTTP 层"同一种思路)。
type objectStorager interface {
	storage.Storager
	storage.Sponsorable
	// uploadsvc.DiskUsage(水位保护用;远端后端返回错误 → fail-open)
	Usage() (total, free int64, err error)
	// uploadsvc.StageStore(暂存回收)
	ListStageFiles() ([]storage.StageFile, error)
	StageRemove(uploadID string) error
	// uploadsvc.TUSStager(TUS 数据面)
	StagePath(uploadID string) (string, error)
	StageSize(uploadID string) (int64, error)
	StageAppend(ctx context.Context, uploadID string, expectOffset int64, r io.Reader) (int64, error)
	StageOpen(uploadID string) (*os.File, error)
}

func run() error {
	var (
		cfgPath   = flag.String("config", "/etc/netdisk/config.yaml", "配置文件路径(yaml)")
		doMigrate = flag.Bool("migrate", false, "启动前执行数据库迁移(goose up)")
		checkOnly = flag.Bool("check", false, "只做配置与依赖自检后退出")
		printCfg  = flag.Bool("print-config", false, "打印生效配置(已脱敏)后退出")
	)
	flag.Parse()

	// 1) 配置:默认值 < yaml < env(6.8)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	// 2) fail-fast:一次性打印全部问题后拒绝启动(纪律 3)
	if err := cfg.Validate(); err != nil {
		return err
	}
	// 2a) `-print-config` 只读:在**启动前置检查之前**返回(BE-S10-09 真机发现的缺陷)。
	//
	// 为什么顺序很重要:`Preflight` 会做"端口可绑 / 目录可写"这类**启动**检查,而对
	// `-print-config` 来说它们既无关又有害 —— 运维最需要看生效配置的时刻正是
	// **服务正在运行**的时候,而那时端口必然被自己占着,于是命令必然失败并报
	// "监听地址不可用(端口已被占用)"(真机实测 1.0.179 部署时踩到)。
	// 配置解析与 `Validate()` 仍然在前面:格式错/取值非法照样 fail-fast。
	if *printCfg {
		return printRedacted(cfg)
	}
	// 2b) 启动前置检查(6.8 纪律 3 的"fs 路径可写、端口未占用"):
	// 纯配置校验看不出这两件事 —— 它们只有真的碰了文件系统/内核才知道。
	if err := config.Preflight(cfg); err != nil {
		return err
	}

	logger := obs.New(cfg.Log.Level, cfg.Log.Format)
	logger.Info("starting netdisk",
		"version", api.BuildVersion,
		"http_addr", cfg.Server.HTTPAddr,
		"db", fmt.Sprintf("%s@%s:%d/%s", cfg.Database.User, cfg.Database.Host, cfg.Database.Port, cfg.Database.Name),
		// E-02 验收②:把**实际生效的池上限**打进启动日志。
		// 不打印的话"池上限配了多少"只能去翻配置文件,而现场排查连接耗尽时
		// 第一句要问的就是它;PG max_connections 是硬上限,池加起来超过它
		// 会让连接在 PG 侧被拒(报错却是"认证失败/too many clients",极易误判)。
		"db_pool_max", cfg.Database.MaxConns,
		"redis", cfg.Redis.Addr,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 3) PostgreSQL
	database, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer database.Close()

	if *doMigrate {
		if err := migrate.Up(ctx, database.Pool); err != nil {
			return fmt.Errorf("迁移失败: %w", err)
		}
		status, _ := migrate.Status(ctx, database.Pool)
		logger.Info("migrations applied", "status", status)

		// 初始化数据库时自动建立初始管理员账号(幂等;admin/admin123,可用 env 覆盖)。
		// 放在迁移之后:建表完成才播种,重复启动仅跳过不再覆盖。
		if res, serr := migrate.SeedAdmin(ctx, database.Pool, migrate.SeedOptions{
			Username: envOr("NETDISK_BOOTSTRAP_ADMIN_USERNAME", "admin"),
			Password: envOr("NETDISK_BOOTSTRAP_ADMIN_PASSWORD", "admin123"),
		}); serr != nil {
			// 播种失败不算致命(可能已有同名用户或口令策略冲突),但要打日志暴露。
			logger.Warn("初始化管理员账号播种失败", "err", serr)
		} else if res.Skipped {
			logger.Info("初始管理员已存在,跳过播种", "username", res.Username)
		} else {
			if res.UsedDefaultPassword {
				logger.Warn("已创建初始管理员(使用默认口令,请尽快修改)",
					"username", res.Username)
			} else {
				logger.Info("已创建初始管理员", "username", res.Username)
			}
		}
	}

	// 4) Redis(10.7:不可用时认证必须拒绝,故启动期直接失败)
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		return fmt.Errorf("连接 Redis 失败: %w", err)
	}
	defer rdb.Close()

	// 5) 令牌管理
	tokens := auth.NewManager(cfg.JWT, rdb, cfg.Redis.KeyPrefix)

	// 5a) Redis 封装与限速器(3.2 职责 7:与 Nginx 双层限流)
	cacheClient := cache.New(rdb, cfg.Redis.KeyPrefix)
	limiter := ratelimit.New(rdb)

	// 5b) 仓储与认证用例服务(4.3/6.3)
	repos := repo.New()
	pol := credentials.DefaultPolicy()
	authService := &authsvc.Service{
		Users:  repos.User,
		Spaces: repos.Space,
		Tokens: tokens,
		Policy: pol,
		DB:     db.AsQuerier(database),
	}

	// 5c) 上传用例服务(4.3 预留 + 6.10 ticket)
	uploadService := &uploadsvc.Service{
		Spaces:  repos.Space,
		Files:   repos.File,
		Uploads: repos.Upload,
		DB:      db.AsQuerier(database),
		Name: namepolicy.Policy{
			MaxBytes:     cfg.Policy.MaxNameBytes,
			MaxPathBytes: cfg.Policy.MaxPathBytes,
			MaxDepth:     cfg.Policy.MaxDepth,
		},
	}

	// 5d) WebDAV Basic 认证通道(BE-S1-06):真实账密只校验一次,之后复用短期会话
	webdavAuth := &webdavauth.Authenticator{
		Store:  &webdavauth.RedisStore{Cache: cacheClient},
		Verify: webdavVerify(authService),
	}

	// 5e) 组织架构用例服务(BE-S3-01)
	orgService := &orgsvc.Service{Depts: repos.Dept, DB: db.AsQuerier(database)}

	// 5f) 文件元数据用例服务(BE-S4-05)
	//
	// Feed 让所有写路径(改名/移动/删除/复制/共享)在**自己的事务内**记一笔变更 ——
	// 少了它,客户端只能靠全量扫描发现变化(6.9 V2.13 要消灭的正是这个)。
	//
	// SyncMaxRows + Queue:6.11 的目录级操作阈值分流(BE-S7-02)——
	// 超阈值的子树移动/删除转异步任务,不再让一个大事务把整个空间锁住。
	dirOpQueue := &dirops.Queue{DB: db.AsQuerier(database)}
	fileService := &filesvc.Service{
		Spaces: repos.Space, Files: repos.File, DB: db.AsQuerier(database),
		Feed:        syncfeed.Writer{},
		SyncMaxRows: cfg.Policy.DirOpSyncMaxRows,
		Queue:       dirOpQueue,
		Log:         logger,
	}

	// 5g) 空间与团队成员用例服务(BE-S3-04/07)
	spaceService := &spacesvc.Service{
		Spaces: repos.Space, Files: repos.File, Users: repos.User, DB: db.AsQuerier(database),
	}

	// 5h) 对象存储(6.5 内容寻址)与 6.10 定稿服务
	//
	// 存储根目录必须在**启动时**创建并验证可写:等第一次上传才发现数据目录
	// 不可写,损失是用户白传一个文件 + 一次 5xx。
	//
	// **运行时只有一个后端**(V2.79):本版本仅支持本地 fs(go-storage 已拆除,
	// 其余云端后端不再提供)。`storage.backend` 在 Validate() 已强制为 fs;这里直接装配。
	//
	// **暂存永远在本地磁盘上**(TUS 分片与定稿中间态),所以本地 FS 一定要有:
	// 它既是 fs 后端的对象存储,也是暂存区与巡检目录统计对象。
	localFS, err := storage.NewFS(cfg.Storage.Root)
	if err != nil {
		return err
	}
	var objectStore objectStorager = localFS

	// 5h-0b) 对象区临时文件回收(BE-S5-10):10 分钟一轮,与暂存回收同频。
	//
	// 只对**本地磁盘**后端有意义(远端后端的临时对象由服务自己的生命周期管);
	// 阈值 24h:远超任何一次正常写入,不会碰到"正在写的临时文件"。
	if cfg.Storage.Backend == config.BackendFS {
		go func() {
			runOnce := func() {
				n, bytes, rerr := localFS.ReapTempObjects(ctx, 24*time.Hour, time.Now())
				if rerr != nil {
					logger.Warn("对象区临时文件回收有失败项", "removed", n, "bytes", bytes, "err", rerr)
				} else if n > 0 {
					logger.Info("对象区临时文件回收完成", "removed", n, "freed_bytes", bytes)
				}
			}
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runOnce()
				}
			}
		}()
	}
	// 5h-0) 后端一致性自检(V2.79):库里已有的对象必须属于**当前**后端。
	//
	// 换了后端却没迁移对象时,老行的 storage_backend 与对象实际位置都还在旧后端 —— 下载会 404。
	// 默认**拒绝启动**(这是启动时就该拦住的事故,而不是等用户点开文件才发现);正在迁移中
	// 可用 `storage.allow_backend_mismatch: true` 显式放行,放行时一定会在日志里留下 WARN。
	{
		var foreign int64
		if qerr := database.Pool.QueryRow(ctx,
			`SELECT count(*) FROM file_objects WHERE storage_backend <> $1`,
			cfg.Storage.Backend).Scan(&foreign); qerr != nil {
			// 自检查询失败:既不拦死也不假装通过 —— 打 WARN 继续(与 ObjectUsage 同一条纪律:
			// 一个统计查询失败不该让整站起不来,但必须让人看见)
			logger.Warn("后端一致性自检无法执行(file_objects 查询失败)", "err", qerr)
		} else if warn, cerr := config.CheckBackendMismatch(cfg.Storage.Backend, foreign, cfg.Storage.AllowBackendMismatch); cerr != nil {
			return cerr
		} else if warn != "" {
			logger.Warn(warn)
		}
	}
	// 5h-0c) object_key 一致性自检(BE-S5-11):库里的落点必须与当前布局算出来的一致。
	//
	// 这个列此前**只写不读**,所以"布局变了、老记录错了"没有任何观测手段。
	// 这里在启动时抽查一次(上限 10000 行):不一致**只告警不改数据** ——
	// 自动改写会把"布局真的变了"这件事静默掩盖掉,而那正是要发现的东西。
	if rows, qerr := database.Pool.Query(ctx,
		`SELECT hash_sha256, object_key FROM file_objects WHERE storage_backend = $1 LIMIT 10000`,
		cfg.Storage.Backend); qerr != nil {
		logger.Warn("object_key 一致性自检无法执行", "err", qerr)
	} else {
		var pairs [][2]string
		for rows.Next() {
			var h, k string
			if serr := rows.Scan(&h, &k); serr != nil {
				break
			}
			pairs = append(pairs, [2]string{h, k})
		}
		rows.Close()
		if bad := storage.MismatchedObjectKeys(objectStore.KeyFor, pairs); len(bad) > 0 {
			sample := bad
			if len(sample) > 3 {
				sample = sample[:3]
			}
			logger.Warn("发现 object_key 与当前布局不一致的行(只记录,不自动改)", "count", len(bad), "sample", sample)
		} else {
			logger.Info("object_key 一致性自检通过", "checked", len(pairs))
		}
	}
	// 5h-1) 秒传的持物证明(BE-S4-04):挑战存 Redis(TTL 5min,一次性),
	// 校验时从**已存对象**读样本区间比对。同一实例同时给建任务(下发挑战)
	// 与定稿(验证挑战)使用 —— 两边必须共享同一份 KV 与 Reader。
	fastUpload := &fastupload.Service{KV: cacheClient, Reader: objectStore}
	finalizeService := &finalize.Service{
		Pool: database.Pool, Storage: objectStore,
		Spaces: repos.Space, Files: repos.File,
		Name: namepolicy.Policy{
			MaxBytes:     cfg.Policy.MaxNameBytes,
			MaxPathBytes: cfg.Policy.MaxPathBytes,
			MaxDepth:     cfg.Policy.MaxDepth,
		},
		Fast: fastUpload,
		// Feed:定稿路径的变更流写入(created/updated),与元数据同事务
		Feed: syncfeed.Writer{},
	}
	uploadService.Fast = fastUpload
	uploadService.FastMinSize = cfg.Policy.FastUploadMinSize

	// 5h-3) 变更流保留期清理(BE-S9-03):按**全局游标水位**删旧行。
	//
	// 水位取"跨全部 (client, space) 的最小已确认 seq"(不是最大)——取最大会让
	// "某个客户端已看到 1000"变成"1000 之前的都能删",而另一个只看到 10 的客户端
	// 会永久丢失 11..1000 且**不会**被判超窗(它以为自己是最新的)。
	// 离线超过 cursor_offline_days 的维先剔除,否则卸载了客户端的账号会把水位钉死。
	go func() {
		ticker := time.NewTicker(6 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				offlineBefore := time.Now().Add(-time.Duration(cfg.Policy.CursorOfflineDays) * 24 * time.Hour)
				keepBefore := time.Now().Add(-time.Duration(cfg.Policy.SyncFeedKeepDays) * 24 * time.Hour)
				q := db.AsQuerier(database)
				w, werr := syncfeed.GlobalWatermark(ctx, q, offlineBefore)
				if werr != nil {
					logger.Error("计算变更流清理水位失败", "err", werr)
					continue
				}
				if w.Seq <= 0 {
					continue
				}
				// 水位是全局最小值,而清理是**按空间**做的:逐空间用同一水位
				// (空间维度不同不影响正确性 —— 只会少清,不会多清)。
				spaces, serr := repos.Space.ListIDs(ctx, q)
				if serr != nil {
					logger.Error("列出空间失败", "err", serr)
					continue
				}
				targets := make(map[string]int64, len(spaces))
				for _, sid := range spaces {
					targets[sid] = w.Seq
				}
				var deleted int64
				if derr := database.InTx(ctx, func(tx pgx.Tx) error {
					n, perr := syncfeed.Prune(ctx, tx, targets, keepBefore)
					deleted = n
					return perr
				}); derr != nil {
					logger.Error("清理变更流失败", "err", derr)
					continue
				}
				logger.Info("变更流清理完成",
					"watermark", w.Seq, "clients", w.Clients,
					"skipped_offline", w.SkippedOffline, "deleted", deleted)
			}
		}
	}()

	// 5h-2) 暂存区回收(BE-S5-04):过期预留 + 陈旧暂存文件 + 失败重试。
	//
	// 后台巡视任务(与上传链路解耦):客户端崩溃不会发取消,预留占的是用户额度;
	// 不回收就是"用户什么都没传成、额度却少了 10GB"。暂存文件同理会慢慢填满磁盘。
	stageCleaner := &uploadsvc.StageCleaner{
		Stager: objectStore, Uploads: repos.Upload, Spaces: repos.Space,
		DB: db.AsQuerier(database), Log: logger,
		TTL: 24 * time.Hour,
	}
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				res := stageCleaner.RunOnce(ctx)
				if res.Scanned > 0 || res.ReclaimedReservations > 0 || res.Errors > 0 || res.Failed > 0 {
					logger.Info("暂存区回收完成",
						"scanned", res.Scanned, "removed", res.RemovedStages,
						"freed_bytes", res.FreedBytes, "kept_active", res.KeptActive,
						"reclaimed_reservations", res.ReclaimedReservations,
						"reclaimed_bytes", res.ReclaimedBytes,
						"failed", res.Failed, "errors", res.Errors)
				}
			}
		}
	}()

	// 5h-2b) 目录级异步任务 worker(BE-S7-02 / 6.11)。
	//
	// 一轮只做一个任务:一个目录任务可能是十万行的写,并发执行不会更快
	// (它们争同一批行锁),只会更容易把连接池占满。
	// 5s 的轮询间隔是"用户可接受的完成延迟"与"空转查询量"之间的折中 ——
	// 队列为空时那条 SQL 走 dir_op_tasks_claim_idx 的部分索引,代价可忽略。
	dirOpWorker := &dirops.Worker{
		Queue: dirOpQueue,
		Log:   logger,
		Exec: func(ctx context.Context, t *dirops.Task) (dirops.Outcome, error) {
			switch t.Kind {
			case dirops.KindMove:
				return fileService.ExecDirMove(ctx, t)
			case dirops.KindDelete:
				return fileService.ExecDirDelete(ctx, t)
			default:
				return dirops.Outcome{}, fmt.Errorf("未知的目录任务类型: %s", t.Kind)
			}
		},
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// 返回值(本轮是否真的执行了任务)只用于日志噪音控制:
				// 每 5s 打一条"队列为空"才是真的把有用的日志淹掉。
				if _, werr := dirOpWorker.RunOnce(ctx); werr != nil {
					logger.Error("目录任务 worker 失败", "err", werr)
				}
			}
		}
	}()

	// 5h-2c) 对象泄漏与反向孤儿巡检(BE-S10-04 / 8.4)。
	//
	// 每日一轮:正向(磁盘实际字节 vs PG 未删除对象字节和)+ 反向(抽样 Stat)。
	// **启动后先等一轮**(而不是立刻跑):正向要 walk 整棵 objects 树,
	// 启动时跑会与"服务刚起来的一波请求"抢磁盘 IO,而巡检本身不紧急。
	// 关掉它请用 patrol.enabled=false —— 巡检只读,不会影响业务数据。
	if cfg.Patrol.Enabled && cfg.Storage.Backend != config.BackendFS {
		// 远端后端的"对象区实际占用"需要 List + 逐对象 HEAD(计费 + 延迟),本版本未实现
		// (见 BE-S5-19)。**明确说出来**而不是静默少做一件事:运维要知道"这个后端现在没有
		// 正向对账能力",否则会把"指标为 0"读成"没有泄漏"。
		logger.Warn("对象巡检在非本地磁盘后端下暂不可用(BE-S5-19)", "backend", cfg.Storage.Backend)
	}
	if cfg.Patrol.Enabled && cfg.Storage.Backend == config.BackendFS {
		patrolService := &patrol.Service{
			DB:               db.AsQuerier(database),
			Storage:          objectStore,
			FS:               localFS,
			Log:              logger,
			LocalSampleSize:  cfg.Patrol.LocalSampleSize,
			RemoteSampleSize: cfg.Patrol.RemoteSampleSize,
			LeakAlertBytes:   cfg.Patrol.LeakAlertBytes,
		}
		interval := time.Duration(cfg.Patrol.IntervalHours) * time.Hour
		go func() {
			// 首次延迟 10 分钟:避开启动窗口(迁移、SSE 重连风暴、客户端全量对账)
			first := time.NewTimer(10 * time.Minute)
			defer first.Stop()
			select {
			case <-ctx.Done():
				return
			case <-first.C:
			}
			if _, err := patrolService.RunOnce(ctx); err != nil {
				logger.Error("对象巡检失败", "err", err)
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := patrolService.RunOnce(ctx); err != nil {
						logger.Error("对象巡检失败", "err", err)
					}
				}
			}
		}()
	}

	// 5h-2d) 配额对账(4.3 / BE-S3-06 ⑤):每小时比对 used_bytes 与权威口径
	// (files 实时 SUM + 在飞预留),漂移超阈值即告警并按配置回写。
	//
	// **漂移就是 bug 信号**,不是"可容忍误差":used_bytes 的每笔增减都在写事务里,
	// 对不上说明某个写路径漏了一次。定时的意义在于把它变成"当轮就能发现",
	// 而不是等用户报"删了文件还是说容量不足"。
	go func() {
		reconciler := &quotareconcile.Service{
			Spaces:     repos.Space,
			DB:         db.AsQuerier(database),
			Log:        logger,
			AlertBytes: cfg.Policy.QuotaDriftAlertBytes,
			AutoFix:    cfg.Policy.QuotaDriftAutoFix,
		}
		// 首次延迟 1 分钟:启动窗口里可能正有一批上传在预留/结算,
		// 那时读到瞬时偏差是正常的(虽然口径已含预留,结算与文件行之间
		// 仍是两条语句,虽然同事务,但跨事务的窗口仍存在)
		first := time.NewTimer(time.Minute)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		runOnce := func() {
			rep, rerr := reconciler.RunOnce(ctx)
			if rerr != nil {
				logger.Error("配额对账失败", "err", rerr)
			}
			if rep != nil && rep.Drifted > 0 {
				logger.Warn("配额对账:发现漂移空间",
					"drifted", rep.Drifted, "fixed", rep.Fixed, "alerted", rep.Alerted,
					"max_abs_delta", rep.MaxAbsDelta)
			}
		}
		runOnce()
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()

	// 5h-2e) 对象延迟删除(6.4 四态状态机 / 9.11 / ADR-2)。
	//
	// **这一环此前从未接线**(2026-09-13 真机复现):`internal/lifecycle` 包本身是
	// 完整的 —— 状态机、对象级会话锁、看护复位、12 项集成用例都在,但 `main.go`
	// 从未构造它。后果不是"报错",而是**静默**:引用归零的对象永远停在
	// `pending_delete`,物理文件留在盘上,`policy.object_delete_delay_hours`
	// 只是个没人读的数字(真机把 `delete_after` 置为过去,24 小时无人认领,
	// 状态与文件原封不动 = 磁盘"只涨不跌")。
	//
	// 为什么这个缺口能活到现在:对象巡检比较的是"磁盘字节 vs `state <> 'deleted'`
	// 的字节和",卡住的 `pending_delete` 仍在其中 → **巡检看不出**;四态分布也
	// "正常"。唯一会说话的是"到期未清理数",而它当时没有任何出口。所以这次同时
	// 延迟删除 worker(6.4 / 2b 指标现已由日志承担;metrics 已拆除)。
	//
	// 轮询 5 分钟:认领租约是 10 分钟、删除幂等;间隔短于租约能让
	// "worker 崩溃 → 看护复位 → 下轮重删"在一个租约量级内收敛。一轮的全表扫描
	// 走 `file_objects_pending_idx` 部分索引,空队列时开销可忽略。
	lifecycleWorker := &lifecycle.Service{
		Pool:        database.Pool,
		Storage:     objectStore,
		Log:         logger,
		DeleteAfter: time.Duration(cfg.Policy.ObjectDeleteDelayHr) * time.Hour,
		ClaimTTL:    10 * time.Minute,
		LockTimeout: 30 * time.Second,
	}
	go func() {
		runOnce := func() {
			res, lerr := lifecycleWorker.RunOnce(ctx, 200)
			if lerr != nil {
				logger.Error("对象延迟删除失败", "err", lerr)
			}
			if res != nil {
				if res.Deleted > 0 || res.Reset > 0 || res.Failed > 0 {
					logger.Info("对象延迟删除完成", "deleted", res.Deleted, "skipped", res.Skipped,
						"failed", res.Failed, "reset", res.Reset)
				}
			}
			// 积压信号:到期未清理数 > 0 就是"清理链路停摆"
			st, serr := lifecycleWorker.Stats(ctx)
			if serr != nil {
				logger.Error("采集对象四态分布失败", "err", serr)
				return
			}
			if st.PendingOverdue > 0 {
				logger.Warn("存在已到期但未被清理的对象(清理链路可能停摆)",
					"pending_overdue", st.PendingOverdue, "pending_delete", st.PendingDelete,
					"deleting_stale", st.DeletingStale)
			}
		}
		// 启动后先等 30s:清理不紧急,让它排在"服务可用"之后
		// (与迁移、SSE 重连风暴、客户端全量对账错开)
		first := time.NewTimer(30 * time.Second)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		runOnce()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()

	// 5i) TUS 数据面(BE-S5-03):分片暂存与 REST 建任务共用同一个 uploadsvc
	tusService := &uploadsvc.TUSService{
		Service:   uploadService,
		Stager:    objectStore,
		Finalizer: finalizeService,
		DB:        db.AsQuerier(database),
	}

	// 5k) SSE 事件通道(BE-S9-04/05):Hub(进程内扇出)+ Redis 订阅(跨实例扇出)
	//
	// 判权源用 repo.ListVisible:连接内按"用户可见空间"过滤 —— 越权推送比"推不到"
	// 严重得多(推不到只会让客户端等一次 /changes,推错就是泄露别人的文件名)。
	eventHub := syncsse.NewHub(func(ctx context.Context, userID string) ([]string, error) {
		spaces, err := repos.Space.ListVisible(ctx, db.AsQuerier(database), userID)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(spaces))
		for _, sp := range spaces {
			out = append(out, sp.ID)
		}
		return out, nil
	}, syncsse.Options{Logger: logger})
	// 实例标识:Publisher 与 Subscriber 必须用**同一个**值,否则自己发出去又经
	// Redis 回来的那一份不会被抑制 → 同一变更被投递两帧(实测过)。
	instanceID := syncsse.NewInstanceID()
	eventPublisher := &syncsse.Publisher{RDB: rdb, Hub: eventHub, Log: logger.Warn, InstanceID: instanceID}
	subscriber := &syncsse.Subscriber{RDB: rdb, Hub: eventHub, Log: logger.Warn, InstanceID: instanceID}
	go subscriber.Run(ctx)

	// 6) 路由与 HTTP 服务
	handler := api.New(api.Deps{
		Cfg: cfg, Log: logger, DB: database, Redis: rdb, Tokens: tokens,
		Cache: cacheClient, Limiter: limiter, Auth: authService,
		Uploads:     uploadService,
		WebDAV:      webdavAuth,
		Org:         orgService,
		Files:       fileService,
		DirOps:      dirOpQueue,
		// 后台用户管理(FE-W-04):停用要立刻吊销会话,故它是一个用例服务而不是裸 SQL
		UserAdmin: &usersvc.Service{DB: db.AsQuerier(database), Users: repos.User, Log: logger},
		Spaces:   spaceService,
		TUS:      tusService,
		// Objects 直连对象存储(metrics 已拆除,不再包指标门面)
		Objects:     objectStore,
		Events:      eventHub,
		Broadcaster: eventPublisher,
		// WebDAV PUT 走同一个 finalizeUpload(6.10 单一写路径)
		Finalizer: finalizeService,
		// 分享链接(BE-S6-04):系统唯一的免登录出口
		Shares: &sharesvc.Service{
			DB: db.AsQuerier(database),
			// 分享密码用与账号口令同一套策略(两处不一致会出现
			// "注册能过的口令在分享里过不了")
			Policy: credentials.DefaultPolicy(),
		},
	})
	srv := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
		// 注意:大文件上传/下载与 SSE 不能被 WriteTimeout 掐断(3.2 职责 5),
		// 故不设 WriteTimeout;超时由 Nginx 与各 handler 自行控制。
		IdleTimeout: 120 * time.Second,
	}

	if *checkOnly {
		logger.Info("self-check passed")
		return nil
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", cfg.Server.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	}

	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout.Std())
	defer cancel2()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// printRedacted 打印生效配置,secret 一律脱敏(纪律 1)。
//
// BE-S10-09 补齐:此前只打印 server/database/redis/jwt/policy/log,**不打印 storage/patrol/限速** ——
// 运维想确认"生效的存储路径"只能翻 secrets.env 与启动日志。现在顺序是"存储 → 巡检 → 限速 → 基础段",
// 每段都是运维真会问生效值的东西;secret 仍然只报 `(set,len=N)`。
func redactedText(cfg *config.Config) string {
	mask := func(s string) string {
		if s == "" {
			return "(empty)"
		}
		return fmt.Sprintf("(set,len=%d)", len(s))
	}
	out := fmt.Sprintf(`storage.backend         = %s
storage.root            = %s
storage.allow_mismatch  = %v
storage.remote.endpoint = %s
storage.remote.bucket   = %s
storage.remote.prefix   = %s
storage.remote.region   = %s
storage.remote.path_style = %v
storage.remote.project_id = %s
storage.credential      = %s
patrol                  = enabled=%v interval=%dh local_sample=%d remote_sample=%d leak_alert_bytes=%d
rate_limits.login       = %d/s %d/min by_ip=%v
rate_limits.upload      = %d/s %d/min by_ip=%v
rate_limits.file_list   = %d/s %d/min by_ip=%v
rate_limits.file_read   = %d/s %d/min by_ip=%v
rate_limits.file_write  = %d/s %d/min by_ip=%v
rate_limits.webdav      = %d/s %d/min by_ip=%v
rate_limits.default     = %d/s %d/min by_ip=%v
`,
		cfg.Storage.Backend, cfg.Storage.Root, cfg.Storage.AllowBackendMismatch,
		cfg.Storage.Remote.Endpoint, cfg.Storage.Remote.Bucket, cfg.Storage.Remote.Prefix,
		cfg.Storage.Remote.Region, cfg.Storage.Remote.PathStyle, cfg.Storage.Remote.ProjectID,
		mask(cfg.Storage.CredentialString()),
		cfg.Patrol.Enabled, cfg.Patrol.IntervalHours, cfg.Patrol.LocalSampleSize, cfg.Patrol.RemoteSampleSize, cfg.Patrol.LeakAlertBytes,
		cfg.RateLimits.Login.PerSecond, cfg.RateLimits.Login.PerMinute, cfg.RateLimits.Login.ByIP,
		cfg.RateLimits.Upload.PerSecond, cfg.RateLimits.Upload.PerMinute, cfg.RateLimits.Upload.ByIP,
		cfg.RateLimits.FileList.PerSecond, cfg.RateLimits.FileList.PerMinute, cfg.RateLimits.FileList.ByIP,
		cfg.RateLimits.FileRead.PerSecond, cfg.RateLimits.FileRead.PerMinute, cfg.RateLimits.FileRead.ByIP,
		cfg.RateLimits.FileWrite.PerSecond, cfg.RateLimits.FileWrite.PerMinute, cfg.RateLimits.FileWrite.ByIP,
		cfg.RateLimits.WebDAV.PerSecond, cfg.RateLimits.WebDAV.PerMinute, cfg.RateLimits.WebDAV.ByIP,
		cfg.RateLimits.Default.PerSecond, cfg.RateLimits.Default.PerMinute, cfg.RateLimits.Default.ByIP,
	) + fmt.Sprintf(`server.http_addr        = %s
server.trusted_proxies  = %v
database                = %s@%s:%d/%s max_conns=%d
database.password       = %s
redis.addr              = %s db=%d key_prefix=%s
redis.password          = %s
jwt.issuer              = %s access_ttl=%s refresh_ttl=%s leeway=%s
jwt.secret              = %s
policy.max_name_bytes   = %d max_depth=%d max_path_bytes=%d
policy.fast_upload_min  = %d bytes
policy.webdav_put_max   = %d bytes
policy.object_delay_hr  = %d sync_feed_keep_days=%d cursor_offline_days=%d
log.level/format        = %s/%s
`,
		cfg.Server.HTTPAddr, cfg.Server.TrustedProxies,
		cfg.Database.User, cfg.Database.Host, cfg.Database.Port, cfg.Database.Name, cfg.Database.MaxConns,
		mask(cfg.Database.Password),
		cfg.Redis.Addr, cfg.Redis.DB, cfg.Redis.KeyPrefix,
		mask(cfg.Redis.Password),
		cfg.JWT.Issuer, cfg.JWT.AccessTTL.Std(), cfg.JWT.RefreshTTL.Std(), cfg.JWT.Leeway.Std(),
		mask(cfg.JWT.Secret),
		cfg.Policy.MaxNameBytes, cfg.Policy.MaxDepth, cfg.Policy.MaxPathBytes,
		cfg.Policy.FastUploadMinSize,
		cfg.Policy.WebDAVPutMaxBytes,
		cfg.Policy.ObjectDeleteDelayHr, cfg.Policy.SyncFeedKeepDays, cfg.Policy.CursorOfflineDays,
		cfg.Log.Level, cfg.Log.Format,
	)
	return out
}

// printRedacted 打印生效配置(secret 已脱敏)。
func printRedacted(cfg *config.Config) error {
	fmt.Print(redactedText(cfg))
	return nil
}
