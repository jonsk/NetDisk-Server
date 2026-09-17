# NetDisk · 开源网盘服务端

> 一个面向**团队文件协作**的自托管网盘后端（Go 单二进制）。
> 提供团队共享空间、TUS / WebDAV / multipart 多协议上传、版本化定稿、
> 实时同步感知（SSE + 游标）、细粒度权限与配额治理——既可部署为私有网盘，
> 也能作为企业的文件中枢。
>
> **协议：** [Apache-2.0](LICENSE) · **形态：** 社区版（Server-com）

---

## 🌐 多语言 / Translations

[中文](README.md) | [English](DOC/i18n/en/README.md) | [Deutsch](DOC/i18n/de/README.md) | [Français](DOC/i18n/fr/README.md) | [Suomi](DOC/i18n/fi/README.md) | [Русский](DOC/i18n/ru/README.md)

---

## ✨ 特性

| 维度 | 能力 |
|---|---|
| 🚀 **上传与定稿** | TUS 分片断点续传 / WebDAV PUT / multipart 三条入口共用同一定稿路径；内容寻址对象 + 秒传去重 + 防投毒校验 |
| 🧠 **对象生命周期** | `file_objects` 四态 + 对象级锁（双层互斥制），先写对象再短事务，杜绝静默覆盖与文件漂移 |
| 💾 **本地对象存储** | 内容寻址存储：对象按内容哈希存放于本地磁盘，同内容只存一份（去重） |
| 📡 **同步感知** | SSE 实时推送 + `/changes` 游标双态（全局 `change_seq`），零周期扫描，与桌面客户端协同 |
| 🔐 **身份与权限** | 自研账密登录 + JWT（HS256）、组织/部门/群组、个人空间 / 团队空间、分享链接、配额对账 |
| 🗂 **目录语义** | 目录深度 ≤31 / 路径 ≤240B 预算；大小写不敏感判重；MOVE/DELETE 阈值分流异步队列 |
| 🛡 **运维与安全** | 配额对账 + 对象巡检（泄漏/孤儿）修复；对象级锁 + 接口限速；审计为预留占位（当前不持久化） |
| 📦 **管理后台** | `/admin` 管理后台（用户 / 组织 / 空间 / 配额），由 Go `embed` 单二进制托出 |

---

## 🏛 架构一览

```
                       ┌──────────────────────────────────────┐
  Web 管理后台 /admin ─▶│                                      │
  WebDAV 客户端  ─WebDAV▶│        netdisk (Go 单二进制)          │
  TUS 上传器      ─TUS─▶│   REST · TUS · WebDAV · SSE 统一入口  │
  桌面同步客户端  ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize 唯一写路径 + 对象锁   │   │
                       │   │ 内容寻址 → 本地磁盘对象存储     │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(反代) │
               │  元数据   │  │会话/限速│   │  单端口    │
               └──────────┘  └────────┘   └────────────┘
```

- **PostgreSQL 17（最低支持）** —— 唯一元数据引擎（含 WAL 归档）；主键用 `gen_random_uuid()`，需 PG ≥ 17
- **Redis** —— 会话 / 限速 / 任务队列（`SKIP LOCKED`，不引入 MQ）
- **Nginx** —— 反向代理，单端口对外
- 管理后台前端经 `web` 仓 `pnpm build` 后拷入 `internal/webui/dist/`，由 **Go `embed`** 提供

---

## 🛠 技术栈

| 层 | 选型 |
|---|---|
| 语言 | Go 1.26 |
| HTTP | 标准库 `net/http` + `ServeMux` 中间件链 |
| DB | pgx v5 + sqlc + goose 迁移 |
| 认证 | golang-jwt v5（HS256） |
| 存储 | 本地磁盘对象存储（内容寻址） |
| 配置 | yaml.v3 + 环境变量强校验 |
| 部署 | systemd + Nginx + Prometheus（单端口四件套） |

---

## 🚀 快速开始

### 前置要求

- Go 1.26+
- PostgreSQL 17+（最低 17；库：`netdisk` / `netdisk_test`，`LC_COLLATE=C`）
- Redis 7+（`appendonly yes`，`noeviction`）
- Nginx（生产）

### 1. 构建

```bash
# 环境（国内加速 + 关闭校验）
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off

go build ./...     # 编译
go vet ./...       # 静态检查
```

> **注意（embed）：** 若前端产物已存在于 `internal/webui/dist/`，会在编译时被嵌入；
> 更新前端后须先 `pnpm build` 再 `go build`，否则会嵌入旧产物。

### 2. 数据库迁移

```bash
go run ./cmd/migrate -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up
# 或使用已构建的 goose 二进制
```

### 3. 配置

复制 `deploy/config/config.example.yaml` 为 `config.yaml` 并按需修改。
**密钥类配置一律走环境变量**（`NETDISK_JWT_SECRET` 等，缺省/过短将拒绝启动），
绝不写入 yaml —— 详见 `deploy/systemd/secrets.env.example`。

### 4. 运行

```bash
go run ./cmd/netdisk
# 监听 :8080
# /admin   管理后台  /api/v1/*  REST  /dav/*  WebDAV  /changes  增量拉取  /sync/events  SSE
```

---

## 🔌 接口能力

| 入口 | 说明 |
|---|---|
| `REST /api/v1/*` | 认证、用户、部门、空间、文件、目录、上传、下载、分享、事件 |
| `下载 & Range` | 流式 `ServeContent`；字节级 Range（200/206/416）；If-Match/If-None-Match/If-Range 条件请求 |
| `TUS /uploads/*` | 分片、断点续传、ticket 复用、暂存区回收、磁盘水位（>90% → 507） |
| `WebDAV /dav/*` | `x/net/webdav` + PUT 拦截走定稿（≤100MB）；LOCK 语义自研补齐 |
| `SSE /sync/events` | 远端变更实时推送（自回声抑制） |
| `cursor /changes` | 游标双态增量拉取，全局 `change_seq` |
| 限流 | file_read / file_write 分级；429 须重试 |

---

## 📂 项目结构

```
Server-com/
├── cmd/          可执行入口
│   ├── netdisk        主服务
│   ├── migrate        数据库迁移
│   ├── passwd         （管理员凭据）
│   ├── depsguard      架构纪律机械校验
│   ├── testsgate      集成测试门禁
│   ├── coveragegate   覆盖率门禁
│   ├── devdb          本地开发建库
│   └── probesmoke     冒烟探针
├── internal/         核心逻辑（37 个包）
│   ├── finalize/      唯一写路径 ★
│   ├── objlock/       对象级双层锁（ADR-2）
│   ├── storage/       本地磁盘对象存储
│   ├── uploadsvc/     TUS 上传
│   ├── lifecycle/     对象生命周期 worker
│   ├── syncfeed|syncsse/  同步感知
│   ├── webdavfs|webdavauth/ WebDAV
│   ├── api/           REST handler
│   ├── patrol/        对象巡检
│   └── quotareconcile/ 配额对账
├── deploy/          部署物料（systemd/Nginx/Prometheus/备份/供给/验证）
├── scripts/         构建与生成脚本
├── DOC/             功能清单 / 架构 / 安装部署 / 日常运维 / 备份恢复 / 测试 / API 指南 / 代码阅读指南
│   └── api/          OpenAPI 契约 openapi.yaml（本仓自持，权威在 GO\Doc\api）
└── LICENSE          Apache-2.0
```

---

## 🧪 测试与门禁

- `go test ./...` —— 单元测试
- `go test -tags=integration ./...` —— 集成测试（需 `netdisk_test` 库 + Redis）
- `cmd/depsguard` —— 红线机械校验（如内核包不依赖 HTTP 层）
- `cmd/testsgate` / `cmd/coveragegate` —— 集成/覆盖率门禁
- 提交前建议本地跑：`go build ./...` / `go vet ./...` / `cmd/depsguard`

---

## 🤝 贡献

提交前请确保：`go build ./...`、`go vet ./...`、`depsguard` 全绿；新增行为遵守架构文档 V3.0 的红线与 ADR；不要提交任何密钥或凭据。

---

## 📄 许可证

本项目以 **Apache License 2.0**（[Apache-2.0](LICENSE)）发布。
你可以自由地使用、修改、分发，并用于**商业**目的（含闭源衍生品），
但必须**保留原始版权与协议声明**，并在修改文件中注明改动。
本项目**按原样**（AS IS）提供，**不提供任何明示或默示的担保**；部署、运维与合规责任由使用者自行承担。

### 与商业版的关系

`Server-com`（本仓，社区版）不附带任何企业服务承诺，亦不含任何凭据或私密配置。

---

*完整能力清单见 [`DOC/01-功能清单.md`](DOC/01-功能清单.md)；部署运维见 [`DOC/05-安装部署.md`](DOC/05-安装部署.md) · [`DOC/06-日常运维.md`](DOC/06-日常运维.md) · [`DOC/07-备份与恢复.md`](DOC/07-备份与恢复.md)；接口细节见 [`DOC/04-API指南.md`](DOC/04-API指南.md)。*
