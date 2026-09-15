// Package config 实现架构文档 6.8 定稿的配置加载:gopkg.in/yaml.v3 + os.Getenv,手写约 100 行。
//
// 三条纪律(6.8):
//  1. secret 私钥只走 env,永不进 yaml
//  2. 默认值只在 Go 结构体构造处维护一份,yaml/env 只做覆盖
//  3. Duration 自定义 UnmarshalText + 启动 fail-fast 全量校验
//
// 优先级:Go 默认值 < config.yaml < 环境变量
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 yaml 支持 "5m" / "1h30m" / "300ms" 写法(6.8 纪律 3)。
type Duration time.Duration

func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", text, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Std 返回标准库类型,便于业务代码使用。
func (d Duration) Std() time.Duration { return time.Duration(d) }

type Server struct {
	HTTPAddr           string   `yaml:"http_addr"`
	PublicURL          string   `yaml:"public_url"`
	TrustedProxies     []string `yaml:"trusted_proxies"`
	ReadHeaderTimeout  Duration `yaml:"read_header_timeout"`
	RequestTimeout     Duration `yaml:"request_timeout"`
	ShutdownTimeout    Duration `yaml:"shutdown_timeout"`
	MaxConcurrentLocks int      `yaml:"max_concurrent_locks"`
}

type Database struct {
	Host             string   `yaml:"host"`
	Port             int      `yaml:"port"`
	Name             string   `yaml:"name"`
	User             string   `yaml:"user"`
	SSLMode          string   `yaml:"ssl_mode"`
	MaxConns         int32    `yaml:"max_conns"`
	MinConns         int32    `yaml:"min_conns"`
	ConnMaxLifetime  Duration `yaml:"conn_max_lifetime"`
	StatementTimeout Duration `yaml:"statement_timeout"`
	// Password 只从 env 注入(纪律 1),故不提供 yaml tag 的默认读取。
	Password string `yaml:"-"`
}

// DSN 组装 pgx 连接串。密码只在此处使用,不落日志。
func (d Database) DSN() string {
	parts := []string{
		"host=" + d.Host,
		"port=" + strconv.Itoa(d.Port),
		"dbname=" + d.Name,
		"user=" + d.User,
		"sslmode=" + d.SSLMode,
		"pool_max_conns=" + strconv.Itoa(int(d.MaxConns)),
	}
	if d.Password != "" {
		parts = append(parts, "password="+d.Password)
	}
	return strings.Join(parts, " ")
}

type Redis struct {
	Addr      string `yaml:"addr"`
	DB        int    `yaml:"db"`
	KeyPrefix string `yaml:"key_prefix"`
	// Password 只从 env 注入(纪律 1)
	Password string `yaml:"-"`
}

type JWT struct {
	AccessTTL  Duration `yaml:"access_ttl"`
	RefreshTTL Duration `yaml:"refresh_ttl"`
	Leeway     Duration `yaml:"leeway"`
	Issuer     string   `yaml:"issuer"`
	// Secret 只从 env 注入(纪律 1),强制 >= 32 字节
	Secret string `yaml:"-"`
}

type Policy struct {
	MaxNameBytes        int   `yaml:"max_name_bytes"`
	MaxDepth            int   `yaml:"max_depth"`
	MaxPathBytes        int   `yaml:"max_path_bytes"`
	FastUploadMinSize   int64 `yaml:"fast_upload_min_size"`
	WebDAVPutMaxBytes   int64 `yaml:"webdav_put_max_bytes"`
	DefaultQuotaBytes   int64 `yaml:"default_quota_bytes"`
	ObjectDeleteDelayHr int   `yaml:"object_delete_delay_hours"`
	SyncFeedKeepDays    int   `yaml:"sync_feed_keep_days"`
	CursorOfflineDays   int   `yaml:"cursor_offline_days"`
	// DiskWatermarkPercent 是存储卷"已用百分比"达到多少就拒绝新上传(9.1 默认 90;
	// >=100 表示关闭;<=0 用默认值 —— **不允许用 0 关闭**,见 uploadsvc.checkDiskWatermark)
	DiskWatermarkPercent int `yaml:"disk_watermark_percent"`
	// DirOpSyncMaxRows 是目录级操作(移动/删除子树)走**同步事务**的行数上限
	// (6.11 默认 1000,"阈值可配")。超过则转异步任务并返回 task_id。
	//
	// 配大一点:小规模操作的用户体验最好(点完就看到结果);
	// 配小一点:超大树不会长时间持锁,但每次移动大目录都要轮询。
	DirOpSyncMaxRows int `yaml:"dir_op_sync_max_rows"`
	// QuotaDriftAlertBytes 是配额对账的漂移告警/回写阈值(4.3,默认 1MiB)。
	//
	// 阈值不是"容忍误差"(used_bytes 的每笔增减都在事务里,对不上就是 bug),
	// 而是**避免告警风暴**:想对任何漂移告警就设成 1。
	QuotaDriftAlertBytes int64 `yaml:"quota_drift_alert_bytes"`
	// QuotaDriftAutoFix 为真时把漂移**回写**(4.3 定稿:漂移超阈值回写并告警)。
	// 回写口径已含在飞预留(见 repo.expectedUsedSQL),不会抹掉正在上传的预留。
	QuotaDriftAutoFix bool `yaml:"quota_drift_autofix"`
}

type WebUI struct {
	AdminPrefix string `yaml:"admin_prefix"`
}

// Patrol 是对象泄漏/反向孤儿巡检(BE-S10-04 / 8.4)。
//
// 抽样量按**后端分级**(V2.18 四审 P2-B):本地 fs 抽 1000 行/日是廉价的
// stat 系统调用;S3/OSS/MinIO 类远端后端每个对象一次 HEAD(计费 + 高延迟),
// 千行级巡检本身就是成本项,故 100 行/日。后端类型取自 `Storager.Backend()`,
// 不由配置声明 —— 配置写"我是本地"而实际接了 S3 时,巡检会以本地强度
// 去打远端后端(账单与超时都会教人做人)。
type Patrol struct {
	// Enabled 默认 true:**默认开启**是有意的 —— 关掉它意味着"磁盘只涨不跌"
	// 与"对象丢了"都没有任何观测手段,而那正是 8.4 要解决的问题。
	Enabled bool `yaml:"enabled"`
	// IntervalHours 巡检周期(默认 24)
	IntervalHours int `yaml:"interval_hours"`
	// LocalSampleSize 本地 fs 后端每轮抽样的行数(默认 1000)
	LocalSampleSize int `yaml:"local_sample_size"`
	// RemoteSampleSize 远端后端每轮抽样的行数(默认 100)
	RemoteSampleSize int `yaml:"remote_sample_size"`
	// LeakAlertBytes 正向差值告警阈值(默认 64MiB)。设为 0 表示用默认值;
	// 想"只暴露指标不打日志"就把阈值设得极大(而不是关掉巡检)。
	LeakAlertBytes int64 `yaml:"leak_alert_bytes"`
}

// Storage 是对象存储与暂存目录(6.5 / 6.1 / 架构 3.1 职责 4)。
//
// `Root` 之下是两个固定子目录,由 storage.FS 管理:
//
//	<root>/objects/   内容寻址的正式对象(objects/{xx}/{xx}/{sha256})
//	<root>/tus-tmp/   TUS 分片暂存与定稿前的合并中间产物
//
// **生产环境建议分盘**(架构 3.1 职责 4:objects 分区与 tus-tmp 分区物理或逻辑
// 分离),因为上传洪峰会先写满暂存区;同盘时暂存写满会连带挤爆正式存储。
// 一期用同一个 Root 也能跑,分盘是部署期把它们挂到不同分区即可,无需改码。
type Storage struct {
	// Backend 选择**运行时唯一**的后端类型(V2.79 新增)。
	//
	// 为什么是"唯一"而不是"多后端并存":对象在库里以 (hash, size) 为键、落点由
	// `file_objects.storage_backend` 记录;一个进程装配**一个** Storager 才能保证
	// "谁写的"和"谁来读"永远一致。多后端并存会让同一对象出现两个真相。
	//
	// 取值与行为(见 Doc/dev/多后端存储改造任务清单.md):
	//   - `fs`(默认):本地磁盘,用下面的 Root;
	//   - `s3`:S3 兼容对象存储(参数见 S3 段);
	//   - `memory`:进程内内存后端(数据不落盘,**仅用于试验/自证**);
	//   - 其它已知后端(minio/azblob/gcs/oss/cos/obs/bos/uss/ftp/sftp/webdav/ipfs/hdfs/
	//     dropbox/gdrive)**本版本尚未实现** —— 选它们会**拒绝启动**,不会静默回落到 fs。
	//     (静默回落的后果是数据写到你以为不是的地方,且没有任何报错。)
	Backend string `yaml:"backend"`
	// AllowBackendMismatch 允许"库里已有对象属于**别的**后端"时仍然启动(默认 false = 拒绝启动)。
	//
	// 默认拒绝的理由:换了后端却不迁移对象时,老行的 storage_backend 与对象实际位置都在旧后端,
	// 下载会 404 —— 这是"启动时就该拦住"的事故,而不是等用户点开文件才发现。
	//
	// 注:本项目**不做**对象迁移工具(用户 2026-09-13 明确"没有这个需求"),所以这个开关在
	// 实务上的用途是"切后端前先手工确认数据去向";不要用它长期带着不匹配的状态跑。
	AllowBackendMismatch bool `yaml:"allow_backend_mismatch"`
	// Root 是本地磁盘的数据根,同时也是**所有后端的本地暂存根**
	// (TUS 分片与定稿中间态永远落在本地磁盘上,与对象存到哪无关)。
	Root string `yaml:"root"`
	// Remote 是**所有远端对象后端**的参数(s3/minio/azblob/gcs/oss/cos/bos/obs/uss/
	// ftp/ipfs/hdfs/gdrive/dropbox 共用同一组字段)。
	//
	// 为什么一套字段喂十四个后端:它们在 go-storage 里要的东西就这四样 —— 桶名、端点、
	// 前缀、凭据;差异(要不要 bucket/endpoint、凭据是什么协议)登记在
	// `gostorage` 的后端注册表里,配置层只做"这一项该不该有"的 fail-fast
	// (两张表由 `TestConfigRequirementsMatchRegistry` 交叉断言)。
	Remote StorageRemote `yaml:"remote"`
	// Credential 是 go-storage 的**凭据字符串**,只从 env 注入(R-15):
	// `NETDISK_STORAGE_CREDENTIAL`,形如 `hmac:AK:SK` / `basic:user:pass` /
	// `file:/etc/netdisk/gcs.json` / `base64:...` / `env:VAR` / `apikey:TOKEN`。
	//
	// 为什么不拆成十几个字段:凭据协议由上游定义,再拆一层只会多一处可能不同步的映射。
	// yaml 里刻意没有这个键(进了 yaml 就会进版本库)。
	Credential string `yaml:"-"`
}

// StorageRemote 是远端对象后端的参数(所有非 fs 后端共用)。
type StorageRemote struct {
	// Endpoint 形如 `http://127.0.0.1:9000`(MinIO/Azurite/自建);留空则用云厂商默认端点。
	// 也接受上游格式 `http:127.0.0.1:9000`(见 gostorage.NormalizeEndpoint)。
	Endpoint string `yaml:"endpoint"`
	// Region 区域(AWS 必填;自建服务可留空)
	Region string `yaml:"region"`
	// Bucket 桶名 / 容器名 / 根目录名(s3/minio/azblob/gcs/oss/cos/bos/obs/uss/ftp/hdfs)。
	// IPFS 与 Dropbox 没有桶的概念,留空即可;gdrive 里它是目录 id。
	Bucket string `yaml:"bucket"`
	// Prefix 对象键前缀(留空则对象直接落在桶根下的 objects/…)
	Prefix string `yaml:"prefix"`
	// ProjectID 仅 gcs 需要(上游连接串 `gcs://bucket/path?...&project_id=...` 里就有它;
	// 实测不给会报 `pair required, [project_id]`)
	ProjectID string `yaml:"project_id"`
	// PathStyle 自建 S3 服务(MinIO 等)需要 path-style 寻址(仅 s3 后端使用)
	PathStyle bool `yaml:"path_style"`
	// AccessKey / SecretKey 是**兼容旧写法的 HMAC 凭据**,只从 env 注入(R-15):
	// NETDISK_S3_ACCESS_KEY / NETDISK_S3_SECRET_KEY。
	//
	// 与 Credential 的关系:**Credential 优先**;两者都为空、或只给 AccessKey/SecretKey 时,
	// 由 CredentialString() 负责折算成 `hmac:AK:SK`(折算只有一处,免得两边各写一遍)。
	AccessKey string `yaml:"-"`
	SecretKey string `yaml:"-"`
}

// CredentialString 返回要传给 go-storage 的凭据字符串。
//
// 优先级:`NETDISK_STORAGE_CREDENTIAL`(通用)> `NETDISK_S3_ACCESS_KEY/_SECRET_KEY`(旧写法,
// 折算成 hmac)。两者都没给时返回空串 —— 由后端注册表决定"这个后端是否必须有凭据"。
func (s Storage) CredentialString() string {
	if v := strings.TrimSpace(s.Credential); v != "" {
		return v
	}
	ak := strings.TrimSpace(s.Remote.AccessKey)
	sk := strings.TrimSpace(s.Remote.SecretKey)
	if ak == "" && sk == "" {
		return ""
	}
	return "hmac:" + ak + ":" + sk
}

// 后端标识:与 storage / gostorage 包的常量**同值**(有单测交叉断言,防止两处漂移)。
const (
	// BackendFS 本地磁盘(生产默认)
	BackendFS = "fs"
	// BackendS3 S3 兼容对象存储
	BackendS3 = "s3"
)

// storageBackendPlanned 给出"上游有实现、但我们还没接入"的后端对应的任务号,让报错直接指路。
//
// 为什么把它写进配置层而不是 storage 层:这里是**拒绝启动**的现场,读者是运维/开发者,
// 他需要的是"这个后端现在能不能用、什么时候能用",而 storage 层不需要知道任务清单。
//
// ⚠ 2026-09-13(V2.81)之后这张表**只剩真正没接入的**:go-storage 里有真实现、且只依赖
// `go-storage/v4` 的 14 个服务已全部接进 `gostorage` 注册表(见 remote.go)。
// 剩下的这些是**上游根本没有实现**(azfile/kodo/onedrive/storj 在 beyondstorage 组织里
// 找不到有实现的服务模块)—— 它们不是"排队等接入",而是"上游没有"。
var storageBackendPlanned = map[string]string{
	"azfile":   "上游无实现(beyondstorage 组织内没有该服务模块)",
	"kodo":     "上游无实现(七牛 Kodo 无对应服务模块)",
	"onedrive": "上游无实现(OneDrive 无对应服务模块)",
	"storj":    "上游无实现(Storj 无对应服务模块)",
}

// storageBackendExcluded 是**明确排除**的后端,连同排除理由。
//
// 为什么要写成数据结构而不是删掉了事:这些名字一定会被人再提起(尤其 webdav 与 sftp 是常见需求),
// 到时候需要的是"为什么不行"而不是"当初没做"。每条理由都是**实测结论**:
//   - memory:用户 2026-09-13 决定不作为可配置后端(数据不落盘,重启即丢);
//   - sftp  :go-storage 上游**没有**这个服务(beyondstorage 组织 28 个服务仓库里只有 ftp);
//   - webdav/cephfs/ocios/us3/zip:上游模块存在但**是空壳** —— 六个方法全部 `panic("not implemented")`
//     (实测:拉下模块 zip 逐个方法检查;webdav 是独立仓库 `go-service-webdav` 的 2021 伪版本,同样是空壳);
//   - tar   :包装类服务(把文件打进归档再落到别的后端)且只实现 3/6 个方法;
//   - qingstor:上游两条路径(aos-dev 时代与 `go-service-qingstor`)都会再挂一套**另一套核心**
//     (`go-storage` v3 或 `aos-dev/go-storage/v3`),破坏"只有一套核心"这条硬约束(用户已放弃)。
var storageBackendExcluded = map[string]string{
	"memory":   "用户 2026-09-13 决定不作为可配置后端(数据不落盘);适配层契约测试仍用它作零环境夹具",
	"sftp":     "go-storage 上游没有该服务(用户 2026-09-13 决定砍掉需求)",
	"webdav":   "go-storage 上游该模块是空壳(六个方法全部 panic;用户 2026-09-13 决定砍掉)",
	"cephfs":   "go-storage 上游该模块是空壳(六个方法全部 panic)",
	"ocios":    "go-storage 上游该模块是空壳(六个方法全部 panic)",
	"us3":      "go-storage 上游该模块是空壳(六个方法全部 panic)",
	"zip":      "go-storage 上游该模块是空壳,且属包装类服务",
	"tar":      "包装类服务(打进归档再落到别的后端)且只实现部分方法",
	"qingstor": "会把另一套核心(go-storage v3 / aos-dev 路径)一起挂进来,破坏核心唯一性(用户已放弃支持)",
}

// storageRemoteNeeds 是"每个远端后端需要哪些参数"的**配置层镜像**。
//
// 它与 `gostorage` 的后端注册表是同一份知识的两个视角:那边决定**怎么构造**,这边决定
// **配置阶段该拦住什么**(早一步报错,且不需要连数据库就能在 `-check` 里看到)。
// 两张表漂移的症状很典型:"配置通过了但装配失败"或"配置拒绝了一个能用的后端" ——
// 所以 `TestConfigRequirementsMatchRegistry` 会**按注册表的 Requirements() 逐项**构造配置:
// 只给必需项必须通过,缺任意一项必须被拒(2026-09-13 加严:原先的"给全参数"式断言
// 容忍多余参数,漏掉了三处漂移 —— oss/bos/obs 的 endpoint、ftp/hdfs/dropbox 的 bucket)。
var storageRemoteNeeds = map[string]struct{ bucket, endpoint, credential, projectID bool }{
	// s3 特例:云厂商靠凭据(用默认端点)、自建靠 endpoint,二选一 → 见 Validate
	"s3": {bucket: true},
	// minio 是独立实现(端点必填)
	"minio": {bucket: true, endpoint: true, credential: true},
	"azblob": {
		bucket:     true,
		credential: true,
	},
	// gcs:凭据**必需**(构造期强制),因此模拟器端到端走不通(见注册表 note)
	"gcs": {bucket: true, credential: true, projectID: true},
	// oss/bos/obs 实测要求显式 endpoint(不给会在构造期报 pair required / parse 错)
	"oss": {bucket: true, endpoint: true, credential: true},
	"cos": {bucket: true, credential: true},
	"bos": {bucket: true, endpoint: true, credential: true},
	"obs": {bucket: true, endpoint: true, credential: true},
	"uss": {bucket: true, credential: true},
	// ftp:上游**不读 name**,根目录用 prefix;它是这类后端里唯一需要 basic 凭据的
	"ftp": {endpoint: true, credential: true},
	// ipfs:只有 endpoint(IPFS HTTP API)
	"ipfs": {endpoint: true},
	// hdfs:同 ftp 不读 name;端点用 tcp:
	"hdfs": {endpoint: true},
	"gdrive": {
		bucket:     true,
		credential: true,
	},
	"dropbox": {credential: true},
}

// StorageBackendImplemented 报告某后端本版本是否**真的能装配**。
//
// 本版本(Server-com 开源版)仅支持本地 fs(go-storage 已整体拆除,
// 其余云端后端不再提供)。
func StorageBackendImplemented(backend string) bool {
	return backend == BackendFS
}

// implementedBackendNames 返回全部可用后端名(fs + 远端),按固定顺序便于报错信息稳定。
func implementedBackendNames() []string {
	out := []string{BackendFS}
	for _, n := range []string{
		"s3", "minio", "azblob", "gcs", "oss", "cos", "bos", "obs", "uss",
		"ftp", "ipfs", "hdfs", "gdrive", "dropbox",
	} {
		if _, ok := storageRemoteNeeds[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// CheckBackendMismatch 判断"库里已有对象属于别的后端"时该不该放行。
//
// 抽成纯函数(而不是写在 main.go 里)是为了能**被单测与反向验证盯住**:
// 这条判断一旦退化成"永远放行",生产上的表现是"换后端后下载大面积 404"。
// 返回 (warn, err):err 非空 = 拒绝启动;warn 非空 = 放行但必须在启动日志里说出来。
func CheckBackendMismatch(active string, foreignRows int64, allow bool) (string, error) {
	if foreignRows <= 0 {
		return "", nil
	}
	if allow {
		return fmt.Sprintf("库中存在 %d 个属于其它后端的对象行(当前后端 %s);"+
			"allow_backend_mismatch 已开启 —— 这些对象在迁移完成前无法下载,请尽快完成迁移",
			foreignRows, active), nil
	}
	return "", fmt.Errorf("库中存在 %d 个对象属于其它后端(当前 storage.backend=%s):"+
		"换后端必须先把已有对象迁移到新后端,否则这些文件下载会 404。"+
		"正在迁移中可临时设置 storage.allow_backend_mismatch=true(启动会打 WARN)", foreignRows, active)
}

// RateLimit 是**单个接口族**的限速阈值(BE-S0-08 验收:各接口族独立阈值且可配)。
//
// 为什么按接口族而不是全局一刀切:登录口是暴力破解面(要收得很紧),
// 文件列表/HEAD 是同步高频路径(收紧了会误伤正常客户端),
// 上传口单次成本高(按次数限即可)。三者用同一个阈值必然顾此失彼。
//
// PerSecond/PerMinute 同时生效、**取更严者**:短窗口挡突发、长窗口挡慢速刷。
type RateLimit struct {
	// PerSecond 每秒上限(0 = 该窗口不限制)
	PerSecond int `yaml:"per_second"`
	// PerMinute 每分钟上限(0 = 该窗口不限制)
	PerMinute int `yaml:"per_minute"`
	// Cost 单次请求消耗的配额(默认 1;重操作可设大,例如批量导出)
	Cost int64 `yaml:"cost"`
	// ByIP 未认证请求按客户端 IP 限速(登录口必须为 true:此时还没有 userID)
	ByIP bool `yaml:"by_ip"`
}

// RateLimits 按接口族聚合阈值。
//
// 新增接口族时应同时在此登记并在路由处引用 —— 漏登记 = 该接口无限速。
type RateLimits struct {
	Login    RateLimit `yaml:"login"`
	Upload   RateLimit `yaml:"upload"`
	FileList RateLimit `yaml:"file_list"`
	FileRead RateLimit `yaml:"file_read"`
	// FileWrite 是改名/移动/删除这类写操作的阈值(比读严格:写会改元数据与引用计数)
	FileWrite RateLimit `yaml:"file_write"`
	WebDAV    RateLimit `yaml:"webdav"`
	Default   RateLimit `yaml:"default"`
}

// For 取某接口族的阈值;未登记时回落到 Default。
//
// 刻意不做"未登记就完全不限速":那会让新增接口在开发者毫无察觉的情况下
// 变成无限速入口(而限速缺失通常只在被打时才暴露)。
func (r RateLimits) For(family string) RateLimit {
	switch family {
	case "login":
		return pick(r.Login, r.Default)
	case "upload":
		return pick(r.Upload, r.Default)
	case "file_list":
		return pick(r.FileList, r.Default)
	case "file_read":
		return pick(r.FileRead, r.Default)
	case "file_write":
		return pick(r.FileWrite, r.Default)
	case "webdav":
		return pick(r.WebDAV, r.Default)
	default:
		return r.Default
	}
}

func pick(v, fallback RateLimit) RateLimit {
	if v.PerSecond == 0 && v.PerMinute == 0 {
		return fallback
	}
	if v.Cost <= 0 {
		v.Cost = 1
	}
	return v
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Config 是应用的全部配置。
type Config struct {
	Server     Server     `yaml:"server"`
	Database   Database   `yaml:"database"`
	Redis      Redis      `yaml:"redis"`
	JWT        JWT        `yaml:"jwt"`
	Policy     Policy     `yaml:"policy"`
	Patrol     Patrol     `yaml:"patrol"`
	WebUI      WebUI      `yaml:"webui"`
	Storage    Storage    `yaml:"storage"`
	Log        Log        `yaml:"log"`
	RateLimits RateLimits `yaml:"rate_limits"`
}

// Default 返回唯一一份默认值(纪律 2)。
func Default() *Config {
	return &Config{
		Server: Server{
			HTTPAddr:           "127.0.0.1:8080",
			TrustedProxies:     []string{"127.0.0.1", "::1"},
			ReadHeaderTimeout:  Duration(10 * time.Second),
			RequestTimeout:     Duration(60 * time.Second),
			ShutdownTimeout:    Duration(20 * time.Second),
			MaxConcurrentLocks: 256,
		},
		Database: Database{
			Host:             "127.0.0.1",
			Port:             5432,
			Name:             "netdisk",
			User:             "netdisk",
			SSLMode:          "disable",
			MaxConns:         32, // 架构 2.6/8.2:与 PG max_connections=100 对齐,留余量
			MinConns:         2,
			ConnMaxLifetime:  Duration(30 * time.Minute),
			StatementTimeout: Duration(30 * time.Second),
		},
		Redis: Redis{
			Addr:      "127.0.0.1:6379",
			DB:        0,
			KeyPrefix: "netdisk:",
		},
		JWT: JWT{
			AccessTTL:  Duration(15 * time.Minute),
			RefreshTTL: Duration(7 * 24 * time.Hour),
			Leeway:     Duration(60 * time.Second), // 6.8 纪律 4:Windows 域环境时钟偏差
			Issuer:     "netdisk",
		},
		Policy: Policy{
			MaxNameBytes:        240,      // 6.7 规则 5
			MaxDepth:            31,       // 6.4 目录深度硬上限
			MaxPathBytes:        240,      // R-11 累计路径上限(可配)
			FastUploadMinSize:   32 << 10, // 6.10 五细则:≥ 8×4KB 才开秒传
			WebDAVPutMaxBytes:   100 << 20,
			DefaultQuotaBytes:   0, // 0 = 不限制
			ObjectDeleteDelayHr: 24,
			SyncFeedKeepDays:    90,
			CursorOfflineDays:   30,
			// 9.1:磁盘已用 >90% 拒绝新上传
			DiskWatermarkPercent: 90,
			// 6.11:≤1000 行同步,超阈值转异步任务
			DirOpSyncMaxRows: 1000,
			// 4.3:配额对账漂移告警/回写阈值(默认 1MiB)与回写开关
			QuotaDriftAlertBytes: 1 << 20,
			QuotaDriftAutoFix:    true,
		},
		WebUI: WebUI{AdminPrefix: "/admin/"},
		// 8.4:对象巡检默认开启、每日一轮;抽样量按后端分级(BE-S10-04)
		Patrol: Patrol{
			Enabled:          true,
			IntervalHours:    24,
			LocalSampleSize:  1000,
			RemoteSampleSize: 100,
			LeakAlertBytes:   64 << 20,
		},
		// 生产:/opt/netdisk/data(与 tus-tmp 建议分盘,见 Storage 注释)
		// Backend 默认 fs:老配置(没有 backend 键)行为完全不变
		Storage: Storage{Backend: BackendFS, Root: "./data"},
		Log:     Log{Level: "info", Format: "text"},
		// 限速默认值:各接口族独立(3.2 职责 7 与 Nginx 粗限流构成双层)
		RateLimits: RateLimits{
			// 登录是暴力破解面,收得最紧;按 IP(此时还没有 userID)
			Login: RateLimit{PerSecond: 5, PerMinute: 30, Cost: 1, ByIP: true},
			// 上传单次成本高(建任务+预留额度),按次限;按用户(已认证)
			Upload: RateLimit{PerSecond: 10, PerMinute: 300, Cost: 1},
			// 列表是同步高频路径,放宽但不放开
			FileList: RateLimit{PerSecond: 20, PerMinute: 600, Cost: 1},
			// 详情/HEAD 用于同步前探测,允许更高频
			FileRead: RateLimit{PerSecond: 50, PerMinute: 2000, Cost: 1},
			// 写操作(改名/移动/删除)比读严格:会改元数据、引用计数与配额
			FileWrite: RateLimit{PerSecond: 20, PerMinute: 600, Cost: 1},
			// WebDAV 客户端(资源管理器)一次目录浏览会连发数十请求
			WebDAV:  RateLimit{PerSecond: 60, PerMinute: 3000, Cost: 1, ByIP: true},
			Default: RateLimit{PerSecond: 20, PerMinute: 600, Cost: 1, ByIP: true},
		},
	}
}

// envReader 便于测试替换。
type envReader func(string) (string, bool)

func osEnv(k string) (string, bool) { return os.LookupEnv(k) }

// Load 读取 yaml(可选)后用 env 覆盖,返回**未校验**的配置。
// 校验由 Validate 负责,便于调用方决定打印方式。
func Load(path string) (*Config, error) { return load(path, osEnv) }

func load(path string, getenv envReader) (*Config, error) {
	c := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(raw, c); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
		case errors.Is(err, os.ErrNotExist):
			// 允许只靠 env 运行(容器/CI),但由 Validate 决定是否致命
		default:
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
	}

	applyEnv(c, getenv)
	normalizeStorageBackend(c)
	return c, nil
}

// normalizeStorageBackend 把 backend 归一化(去空白、转小写),空值回落默认 fs。
//
// 为什么归一化放在 Load 而不是 Validate:Validate 是纯校验(不改配置),
// 而归一化是"用户写了 `FS` / ` fs ` / 留空"这类书写差异的收敛 —— 它必须在
// 校验之前发生,否则 `FS` 会被判成"未知后端"这种令人困惑的错误。
func normalizeStorageBackend(c *Config) {
	c.Storage.Backend = strings.ToLower(strings.TrimSpace(c.Storage.Backend))
	if c.Storage.Backend == "" {
		c.Storage.Backend = BackendFS
	}
}

func applyEnv(c *Config, getenv envReader) {
	// 语义约定:**空串 == 未设置**。
	// 理由:systemd EnvironmentFile 里 `NETDISK_DB_NAME=` 这类空值是常态,
	// 若把空串当"有效覆盖",会把 yaml/默认值清空 —— 这正是 6.8 纪律 2 要防的漂移。
	str := func(key string, dst *string) {
		if v, ok := getenv(key); ok && v != "" {
			*dst = v
		}
	}
	num := func(key string, dst *int) {
		if v, ok := getenv(key); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	i32 := func(key string, dst *int32) {
		if v, ok := getenv(key); ok && v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = int32(n)
			}
		}
	}
	i64 := func(key string, dst *int64) {
		if v, ok := getenv(key); ok && v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				*dst = n
			}
		}
	}
	dur := func(key string, dst *Duration) {
		if v, ok := getenv(key); ok && v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				*dst = Duration(d)
			}
		}
	}
	// boolean 只接受 Go 的 ParseBool 词法(1/0/t/f/true/false/yes/no 的一部分):
	// 不接受 "on"/"off"/"启用" 这类自造词 —— 一个不被识别的值**静默保留默认值**
	// 是最糟的失败方式(运维以为关掉了巡检,而它还在跑)。
	boolean := func(key string, dst *bool) {
		if v, ok := getenv(key); ok && v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}

	// secrets(纪律 1:只走 env)
	str("NETDISK_STORAGE_ROOT", &c.Storage.Root)
	str("NETDISK_STORAGE_BACKEND", &c.Storage.Backend)
	// 远端对象后端的参数与凭据(凭据只走 env,见 Storage/Credential 注释)
	//
	// 环境变量名保留 `NETDISK_S3_*`:它们从 V2.79 起就在 secrets.env/文档里,
	// 而"这套参数现在也喂 minio/azblob/…"不构成改名理由(改名会让已部署机器的 env 静默失效)。
	str("NETDISK_S3_ENDPOINT", &c.Storage.Remote.Endpoint)
	str("NETDISK_S3_REGION", &c.Storage.Remote.Region)
	str("NETDISK_S3_BUCKET", &c.Storage.Remote.Bucket)
	str("NETDISK_S3_PREFIX", &c.Storage.Remote.Prefix)
	str("NETDISK_S3_PROJECT_ID", &c.Storage.Remote.ProjectID)
	str("NETDISK_S3_ACCESS_KEY", &c.Storage.Remote.AccessKey)
	str("NETDISK_S3_SECRET_KEY", &c.Storage.Remote.SecretKey)
	boolean("NETDISK_S3_PATH_STYLE", &c.Storage.Remote.PathStyle)
	// 通用凭据(所有远端后端都能用;优先于上面的 hmac 对)
	str("NETDISK_STORAGE_CREDENTIAL", &c.Storage.Credential)
	str("JWT_SECRET", &c.JWT.Secret)
	str("NETDISK_DB_PASSWORD", &c.Database.Password)
	str("NETDISK_REDIS_PASSWORD", &c.Redis.Password)

	// 非敏感项也允许 env 覆盖(systemd EnvironmentFile 场景)
	str("NETDISK_HTTP_ADDR", &c.Server.HTTPAddr)
	str("NETDISK_PUBLIC_URL", &c.Server.PublicURL)
	str("NETDISK_DB_HOST", &c.Database.Host)
	num("NETDISK_DB_PORT", &c.Database.Port)
	str("NETDISK_DB_NAME", &c.Database.Name)
	str("NETDISK_DB_USER", &c.Database.User)
	str("NETDISK_DB_SSLMODE", &c.Database.SSLMode)
	i32("NETDISK_DB_MAX_CONNS", &c.Database.MaxConns)
	i32("NETDISK_DB_MIN_CONNS", &c.Database.MinConns)
	dur("NETDISK_DB_STMT_TIMEOUT", &c.Database.StatementTimeout)
	str("NETDISK_REDIS_ADDR", &c.Redis.Addr)
	num("NETDISK_REDIS_DB", &c.Redis.DB)
	str("NETDISK_REDIS_KEY_PREFIX", &c.Redis.KeyPrefix)
	dur("NETDISK_JWT_ACCESS_TTL", &c.JWT.AccessTTL)
	dur("NETDISK_JWT_REFRESH_TTL", &c.JWT.RefreshTTL)
	dur("NETDISK_JWT_LEEWAY", &c.JWT.Leeway)
	str("NETDISK_JWT_ISSUER", &c.JWT.Issuer)
	i64("NETDISK_DEFAULT_QUOTA_BYTES", &c.Policy.DefaultQuotaBytes)
	i64("NETDISK_WEBDAV_PUT_MAX_BYTES", &c.Policy.WebDAVPutMaxBytes)
	num("NETDISK_MAX_NAME_BYTES", &c.Policy.MaxNameBytes)
	num("NETDISK_MAX_DEPTH", &c.Policy.MaxDepth)
	num("NETDISK_MAX_PATH_BYTES", &c.Policy.MaxPathBytes)
	num("NETDISK_OBJECT_DELETE_DELAY_HOURS", &c.Policy.ObjectDeleteDelayHr)
	num("NETDISK_SYNC_FEED_KEEP_DAYS", &c.Policy.SyncFeedKeepDays)
	num("NETDISK_CURSOR_OFFLINE_DAYS", &c.Policy.CursorOfflineDays)
	num("NETDISK_DIR_OP_SYNC_MAX_ROWS", &c.Policy.DirOpSyncMaxRows)
	i64("NETDISK_QUOTA_DRIFT_ALERT_BYTES", &c.Policy.QuotaDriftAlertBytes)
	boolean("NETDISK_QUOTA_DRIFT_AUTOFIX", &c.Policy.QuotaDriftAutoFix)
	// 对象巡检(BE-S10-04)
	boolean("NETDISK_PATROL_ENABLED", &c.Patrol.Enabled)
	num("NETDISK_PATROL_INTERVAL_HOURS", &c.Patrol.IntervalHours)
	num("NETDISK_PATROL_LOCAL_SAMPLE_SIZE", &c.Patrol.LocalSampleSize)
	num("NETDISK_PATROL_REMOTE_SAMPLE_SIZE", &c.Patrol.RemoteSampleSize)
	i64("NETDISK_PATROL_LEAK_ALERT_BYTES", &c.Patrol.LeakAlertBytes)
	str("NETDISK_LOG_LEVEL", &c.Log.Level)
	str("NETDISK_LOG_FORMAT", &c.Log.Format)

	if v, ok := getenv("NETDISK_TRUSTED_PROXIES"); ok {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		c.Server.TrustedProxies = out
	}
}

// Validate 对全部配置做启动期强校验(纪律 3):一次性收集所有问题,由调用方打印后拒绝启动。
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	// secrets
	if len(c.JWT.Secret) < 32 {
		add("JWT_SECRET 必须 >= 32 字节(当前 %d);由 env 注入,禁止写入 yaml", len(c.JWT.Secret))
	}
	if c.Database.Password == "" {
		add("NETDISK_DB_PASSWORD 未设置(由 env 注入)")
	}

	// server
	if c.Server.HTTPAddr == "" {
		add("server.http_addr 不能为空")
	}
	if c.Server.MaxConcurrentLocks <= 0 {
		add("server.max_concurrent_locks 必须 > 0")
	}
	if len(c.Server.TrustedProxies) == 0 {
		add("server.trusted_proxies 不能为空(3.2:只信 Nginx 回环地址)")
	}

	// database
	if c.Database.Host == "" || c.Database.Name == "" || c.Database.User == "" {
		add("database 的 host/name/user 不能为空")
	}
	if c.Database.Port <= 0 || c.Database.Port > 65535 {
		add("database.port 非法: %d", c.Database.Port)
	}
	if c.Database.MaxConns <= 0 {
		add("database.max_conns 必须 > 0")
	}
	if c.Database.MinConns < 0 || c.Database.MinConns > c.Database.MaxConns {
		add("database.min_conns 非法: %d(须 0 <= min <= max)", c.Database.MinConns)
	}
	if c.Database.MaxConns > 40 {
		add("database.max_conns=%d 过大:本机 PG max_connections=100,建议 <= 40(2.6/8.2)", c.Database.MaxConns)
	}

	// redis
	if c.Redis.Addr == "" {
		add("redis.addr 不能为空")
	}

	// jwt
	if c.JWT.Issuer == "" {
		add("jwt.issuer 不能为空")
	}
	if c.JWT.AccessTTL.Std() <= 0 || c.JWT.RefreshTTL.Std() <= 0 {
		add("jwt access/refresh TTL 必须 > 0")
	}
	if c.JWT.AccessTTL.Std() >= c.JWT.RefreshTTL.Std() {
		add("jwt.access_ttl 必须小于 refresh_ttl")
	}
	if c.JWT.Leeway.Std() < 0 {
		add("jwt.leeway 不能为负")
	}

	// policy(6.7 / R-11)
	if c.Policy.MaxNameBytes <= 0 || c.Policy.MaxNameBytes > 255 {
		add("policy.max_name_bytes 非法: %d(须 1..255)", c.Policy.MaxNameBytes)
	}
	if c.Policy.MaxDepth <= 0 || c.Policy.MaxDepth > 128 {
		add("policy.max_depth 非法: %d", c.Policy.MaxDepth)
	}
	if c.Policy.MaxPathBytes <= 0 {
		add("policy.max_path_bytes 必须 > 0")
	}
	if c.Policy.FastUploadMinSize < 0 {
		add("policy.fast_upload_min_size 不能为负")
	}
	if c.Policy.WebDAVPutMaxBytes <= 0 {
		add("上传上限必须 > 0")
	}
	if c.Policy.DefaultQuotaBytes < 0 {
		add("policy.default_quota_bytes 不能为负(0 表示不限制)")
	}
	if c.Policy.ObjectDeleteDelayHr < 0 {
		add("policy.object_delete_delay_hours 不能为负")
	}
	if c.Policy.SyncFeedKeepDays <= 0 || c.Policy.CursorOfflineDays <= 0 {
		add("policy sync_feed_keep_days / cursor_offline_days 必须 > 0")
	}
	if c.Policy.DiskWatermarkPercent < 0 || c.Policy.DiskWatermarkPercent > 100 {
		// 100 是"关闭"的显式写法;>100 无意义;负数无意义(0 用默认值)
		add("policy.disk_watermark_percent 非法: %d(0=用默认 90,1..99=阈值,100=关闭)",
			c.Policy.DiskWatermarkPercent)
	}
	if c.Policy.DirOpSyncMaxRows < 0 {
		// 0 用默认 1000;负数无意义(而且会让**每一次**目录操作都异步,
		// 那不是"更安全",而是把同步语义整体废掉)
		add("policy.dir_op_sync_max_rows 不能为负(0 用默认 1000)")
	}
	if c.Patrol.Enabled {
		if c.Patrol.IntervalHours <= 0 {
			// 0/负数会让 ticker 变成"每 0 秒跑一次"或 panic;**关掉巡检要用 enabled: false**,
			// 而不是把周期设成 0 —— 后者读起来像"不限制",实际是"疯狂巡检"
			add("patrol.interval_hours 必须 > 0(关闭巡检请用 patrol.enabled=false)")
		}
	}
	if c.Patrol.LocalSampleSize < 0 || c.Patrol.RemoteSampleSize < 0 {
		add("patrol 抽样量不能为负")
	}
	if c.Patrol.LeakAlertBytes < 0 {
		add("patrol.leak_alert_bytes 不能为负")
	}
	if c.Policy.QuotaDriftAlertBytes < 0 {
		add("policy.quota_drift_alert_bytes 不能为负(0 用默认 1MiB)")
	}

	if strings.TrimSpace(c.Storage.Root) == "" {
		add("storage.root 不能为空(对象与暂存目录的根)")
	}

	// 后端选择(Server-com 开源版):仅支持本地 fs。
	// go-storage 已整体拆除,其余后端(含 s3/minio/oss 等)一律拒绝,
	// 且**绝不允许静默回落到 fs** —— 数据写到运维以为不是的地方是事故。
	if c.Storage.Backend != BackendFS {
		add("storage.backend=%q 不支持:本版本仅提供本地 fs(go-storage 已拆除,不再支持云存储后端)",
			c.Storage.Backend)
	}


	// 限速:必须是"有上限"的配置。全 0 意味着该接口族**完全不限速**,
	// 属于极易被忽略的高危配置(默认值有值,只有手写 yaml 才可能清零),
	// 所以这里直接拒绝而不是容忍。
	for _, rl := range []struct {
		name string
		v    RateLimit
	}{
		{"login", c.RateLimits.Login},
		{"upload", c.RateLimits.Upload},
		{"file_list", c.RateLimits.FileList},
		{"file_read", c.RateLimits.FileRead},
		{"file_write", c.RateLimits.FileWrite},
		{"webdav", c.RateLimits.WebDAV},
		{"default", c.RateLimits.Default},
	} {
		if rl.v.PerSecond < 0 || rl.v.PerMinute < 0 {
			add("rate_limits.%s 阈值不能为负", rl.name)
		}
		if rl.v.Cost < 0 {
			add("rate_limits.%s.cost 不能为负", rl.name)
		}
		if rl.v.PerSecond == 0 && rl.v.PerMinute == 0 {
			add("rate_limits.%s 至少需要一个窗口上限(全 0 = 该接口族不限速,危险)", rl.name)
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("配置校验失败(%d 项):\n  - %s", len(errs), strings.Join(errs, "\n  - "))
}
