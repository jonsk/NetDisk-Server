# NetDisk · 企业级开源网盘服务端

> 一个面向**团队文件协作**的自托管网盘后端（Go 单二进制）。
> 提供团队共享空间、TUS / WebDAV / multipart 多协议上传、版本化定稿、
> 实时同步感知（SSE + 游标）、细粒度权限与全量审计——既可部署为私有网盘，
> 也能作为企业的文件中枢。
>
> **协议：** [Apache-2.0](LICENSE) · **形态：** 社区版（Server-com）

---

## ✨ 特性

| 维度 | 能力 |
|---|---|
| 🚀 **上传与定稿** | TUS 分片断点续传 / WebDAV PUT / multipart 三条入口共用同一定稿路径；内容寻址对象 + 秒传去重 + 防投毒校验 |
| 🧠 **对象生命周期** | `file_objects` 四态 + 对象级锁（双层互斥制），先写对象再短事务，杜绝静默覆盖与文件漂移 |
| 🔄 **多后端存储** | go-storage 适配 14 种后端（本地盘 / MinIO / S3 / OSS / COS / GCS / Azure …）；能力矩阵入库，未实现后端构造期拒启 |
| 📡 **同步感知** | SSE 实时推送 + `/changes` 游标双态（全局 `change_seq`），零周期扫描，与桌面客户端协同 |
| 🔐 **身份与权限** | 自研 IdP 抽象（企业微信 / 钉钉回调）、JWT（HS256）、组织/部门/群组、团队共享空间、配额对账 |
| 🗂 **目录语义** | 目录深度 ≤31 / 路径 ≤240B 预算；大小写不敏感判重；MOVE/DELETE 阈值分流异步队列 |
| 📜 **全量审计** | 审计事件落 PG 分区表 + 查询/导出；Prometheus 指标 + 15 条告警规则 + 后台巡检 |
| 📦 **管理后台 & H5** | `/admin` 管理后台（用户/空间/配额/审计/IdP）+ `/h5` 移动轻访问，由 Go `embed` 单二进制托出 |

---

## 🏛 架构一览

```
                       ┌──────────────────────────────────────┐
  Web 管理后台 /admin ─▶│                                      │
  H5 移动端 /h5       ─▶│          netdisk (Go 单二进制)         │
  WebDAV 客户端   ─TUS─▶│   REST · TUS · WebDAV · SSE 统一入口  │
  TUS 上传器      ─PUT─▶│                                      │
  桌面同步客户端  ─SSE──▶│    ┌───────────┐   ┌──────────────┐  │
                       │    │ finalize 唯一│   │ go-storage   │  │
                       │    │ 写路径+对象锁 │   │ 本地盘/14后端 │  │
                       │    └───────────┘   └──────────────┘  │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(反代) │
               │  元数据   │  │会话/限速│   │  单端口    │
               └──────────┘  └────────┘   └────────────┘
```

- **PostgreSQL 18** —— 唯一元数据引擎（含审计分区表 / WAL 归档）
- **Redis** —— 会话 / 限速 / 任务队列（`SKIP LOCKED`，不引入 MQ）
- **Nginx** —— 反向代理，单端口对外
- 前端产物经 `web` 仓 `pnpm build` 后拷入 `internal/webui/dist/`，由 **Go `embed`** 提供

---

## 🛠 技术栈

| 层 | 选型 |
|---|---|
| 语言 | Go 1.26 |
| HTTP | 标准库 `net/http` + `ServeMux` 中间件链 |
| DB | pgx v5 + sqlc + goose 迁移 |
| 认证 | golang-jwt v5（HS256） |
| 存储 | go-storage（多后端能力矩阵） |
| 配置 | yaml.v3 + 环境变量强校验 |
| 部署 | systemd + Nginx + Prometheus（单端口四件套） |

---

## 🚀 快速开始

### 前置要求

- Go 1.26+
- PostgreSQL 18（库：`netdisk` / `netdisk_test`，`LC_COLLATE=C`）
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
# /admin   管理后台  /h5  移动端  /api/v1/*  REST  /dav/*  WebDAV  /changes /sync/events  SSE
```

---

## 🔌 接口能力

| 入口 | 说明 |
|---|---|
| `REST /api/v1/*` | 认证、用户、部门、空间、文件、目录、上传、下载、分享、审计、IdP、事件 |
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
├── internal/         核心逻辑（38 个包）
│   ├── finalize/      唯一写路径 ★
│   ├── objlock/       对象级双层锁（ADR-2）
│   ├── storage/       go-storage 多后端
│   ├── uploadsvc/     TUS 上传
│   ├── lifecycle/     对象生命周期 worker
│   ├── syncfeed|syncsse/  同步感知
│   ├── webdavfs|webdavauth/ WebDAV
│   ├── api/           REST handler
│   └── audit|metrics|patrol/ 审计与运维
├── deploy/          部署物料（systemd/Nginx/Prometheus/备份/供给/验证）
├── scripts/         构建与生成脚本
├── DOC/             功能清单与开发任务清单
└── LICENSE          Apache-2.0
```

---

## 🧪 测试与门禁

- `go test ./...` —— 单元测试
- `go test -tags=integration ./...` —— 集成测试（需 `netdisk_test` 库 + Redis）
- `cmd/depsguard` —— 9 条红线机械校验（如 `finalize` 永不 import `api`）
- `cmd/testsgate` / `cmd/coveragegate` —— CI 中的集成/覆盖率门禁
- CI（`.github/workflows/ci.yml`）：build / vet / goose / testsgate(-race) / depsguard

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

*查看完整能力清单见 [`DOC/功能清单与开发任务清单.md`](DOC/功能清单与开发任务清单.md)。*
