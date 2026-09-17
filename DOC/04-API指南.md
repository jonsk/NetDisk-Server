# Server-com API 指南

> 规范文件：权威契约在 `DOC/api/openapi.yaml`（OpenAPI v3）；本文是它的**人读导读**。
> 关联：`DOC/02-架构设计文档.md`、`DOC/05-安装部署.md`、`DOC/06-日常运维.md`、`DOC/07-备份与恢复.md`

---

## 1. 认证模型

服务用 **JWT（HS256）** 做认证，登录后拿 `access_token`，之后每个请求带：

```
Authorization: Bearer <access_token>
```

**令牌分端（R-14）**：web 端（管理后台）与 desktop 端（桌面客户端）的令牌互相独立，
避免一处泄露影响全身。令牌/刷新令牌存 Redis，支持吊销与"单飞刷新"。

| 接口 | 说明 |
|---|---|
| `POST /api/v1/auth/login` | 账密登录，返回 `access_token` + `refresh_token` |
| `POST /api/v1/auth/refresh` | 用刷新令牌换新 access 令牌 |
| `POST /api/v1/auth/logout` | 登出，吊销令牌 |
| `GET /api/v1/me` | 当前用户信息 |
| `GET /api/v1/version` | 服务端版本 |

> 社区版只保留**密码登录**；企业微信/钉钉/第三方 IdP 登录已在社区版移除。
> 超过窗口会返回 **429**，客户端应退避重试（见 §7）。

---

## 2. 通用约定

- **Base URL**：`/api/v1`，生产经 Nginx 单端口对外。
- **请求/响应**：JSON（`Content-Type: application/json`）。
- **分页**：列表接口用 `limit` + `after`（keyset 游标）翻页；`limit` 上限 **999**
  （超过会被回落为默认 200）。
- **下载**：流式返回 + 字节级 `Range`（200/206/416）+ 条件请求头（`If-Match`/`If-None-Match`/`If-Range`）。
- **限速**：分接口族（login/upload/file_list/file_read/file_write/webdav），两窗口（秒/分）同时生效取更严者。

---

## 3. REST 接口分组

### 3.1 文件与目录

| 方法与路径 | 说明 |
|---|---|
| `GET /api/v1/files?space=<id>&parent_id=&limit=&after=` | 列目录（默认 200 条/页；支持 keyset 翻页） |
| `POST /api/v1/files` | 建文件/目录项 |
| `GET /api/v1/files/dirs` | 列目录（目录专用） |
| `GET /api/v1/files/{id}` | 文件/目录详情 |
| `GET /api/v1/files/{id}/content` | 下载内容（支持 Range） |
| `POST /api/v1/files/{id}/move` | 移动/改名 |
| `POST /api/v1/files/{id}/copy` | 复制 |
| `POST /api/v1/files/{id}/share-to-space` | 复制/共享到另一空间（走对象的引用 +1 路径） |
| `POST /api/v1/files/{id}/lock` | 加/解锁（对象级锁） |
| `GET /api/v1/files/{id}/subtree-stats` | 子树统计 |

> 目录级"移动/删除子树"超过阈值（默认 1000 行）会转**异步任务**：
> `POST` 返回 `task_id`，客户端轮询 `GET /api/v1/tasks/{id}` 查询进度。

### 3.2 上传（multipart 直传）

| 方法与路径 | 说明 |
|---|---|
| `POST /api/v1/upload/create` | 建上传（预留额度，返回上传令牌） |
| `POST /api/v1/upload/simple` | multipart 直传（小文件；写一次即定稿） |
| `POST /api/v1/upload/{id}/finish` | 完成/定稿 |
| `GET/PATCH /api/v1/upload/{id}` | 查询/续传 |

> 大文件走 **TUS** 协议（见 §4）。

### 3.3 变更与同步

| 方法与路径 | 说明 |
|---|---|
| `GET /api/v1/changes?since=<cursor>` | 游标增量拉取（双态之「可靠」通道） |
| `GET /api/v1/changes/head` | 取当前最新游标 |
| `GET /api/v1/sync/cursors` | 管理同步游标 |
| `GET /sync/events` | SSE 实时推送（双态之「实时」通道） |

> 两者共用同一个全局 `change_seq`：SSE 可能丢帧，但可以用 `/changes` 按游标补拉，保证不漏。

### 3.4 分享

| 方法与路径 | 说明 |
|---|---|
| `POST /api/v1/shares` | 创建分享链接 |
| `GET /api/v1/shares` | 我创建的分享列表 |
| `DELETE /api/v1/shares/{id}` | 撤销分享 |
| `GET /api/v1/shares/{token}/meta` | 分享元信息（落地页用） |
| `GET /api/v1/shares/{token}/download` | 按分享 token 下载 |

### 3.5 空间

| 方法与路径 | 说明 |
|---|---|
| `POST /api/v1/spaces` | 建空间（个人空间 / 团队空间） |
| `GET /api/v1/spaces/{id}` | 空间详情（含配额） |
| `POST /api/v1/spaces/{id}/transfer` | 空间转交 |
| `GET /api/v1/spaces/{id}/members` | 成员列表 |
| `PUT/DELETE /api/v1/spaces/{id}/members/{userId}` | 加 / 移除成员 |
| `POST /api/v1/spaces/{id}/leave` | 退出空间 |

### 3.6 管理后台（仅 super_admin）

| 方法与路径 | 说明 |
|---|---|
| `POST/GET /api/v1/admin/departments` | 部门管理 |
| `PUT/DELETE /api/v1/admin/departments/{id}` | 部门改 / 删 |
| `POST /api/v1/admin/spaces/{id}/freeze` | 冻结空间（停止同步） |
| `GET /api/v1/audit/logs` | 审计日志（社区版为预留占位） |

> 用户/组织/空间/配额的管理后台界面由 Go `embed` 提供（`/admin/*`）。
> 另：用户管理相关接口（建用户、改角色、配配额）走 `/api/v1/admin/*`（见 openapi 全量）。

---

## 4. TUS 分片上传（大文件）

面向桌面客户端 / 大文件的断点续传协议（`deploy` 里 `cmd/probesmoke` 用作参考实现）：

| 步骤 | 方法 | 关键头 |
|---|---|---|
| 建任务 | `POST /tus` | `Tus-Resumable: 1.0.0`、`Upload-Length`、`Upload-Metadata: filename <base64>`、`Authorization: Bearer` |
| 传分片 | `PATCH /tus/{id}` | `Tus-Resumable`、`Upload-Offset`、`X-Upload-Token`、`Content-Type: application/offset+octet-stream` |
| 完成 | 末片 | 响应 `200` + `Upload-Complete: true` + `X-File-Id` 即定稿 |

- **断点续传**：客户端上报 `Upload-Offset`，服务端从该偏移继续；
- **ticket 复用**：上传票可在定稿前复用，幂等；
- **磁盘水位**：暂存区用量 ≥ 阈值（默认 90%）返回 **507 Insufficient Storage**；
- **暂存回收**：超时未完成的暂存文件由后台任务（约 10 分钟一轮）回收。

---

## 5. WebDAV

路径前缀 `/dav/*`，基于标准 `x/net/webdav`，最常用于"映射网络驱动器 / 资源管理器"：

- **PUT 走定稿路径**：≤100MB 直接定稿（超限提示走 TUS）；
- **LOCK 语义自研补齐**（标准库仅内存实现）；
- Range / 条件请求与 REST 同源；
- 目录浏览会一次发多个请求 → 对应 `rate_limits.webdav` 配得较宽（60/s）。

---

## 6. 健康 / 监控

| 路径 | 说明 |
|---|---|
| `GET /healthz` | 存活检查（200） |
| `GET /metrics` | Prometheus 指标；默认仅回环 / 可信代理，跨机需 `NETDISK_METRICS_TOKEN` Bearer |
| `GET /api/v1/version` | 版本 |

---

## 7. 状态码与语义

| 状态码 | 语义 | 说明 |
|---|---|---|
| 200 / 201 | 成功 | 定稿类成功会带 `X-File-Id` |
| 206 / 416 | 部分内容 / 范围越界 | 下载 Range |
| 400 / 422 | 参数错误 | 校验失败 |
| 401 | 未认证 | 令牌缺失/过期 |
| 403 / 401 | 权限受限 | **暂停该空间同步、永不删本地**（客户端行为） |
| 404 / 410 | 不存在 / 已移除 | 410 = 对象/空间已迁移或删除 |
| 409 | 冲突 | 版本冲突、重名（大小写不敏感判重） |
| 429 | 限速 | 需退避重试 |
| 500 | 服务端错误 | 见《日常运维》故障处置 |
| 507 | 存储空间不足 | 磁盘水位达阈值 |

**客户端对 429 的处置**：项目经验是"429 退避重试是必须的"——实测清理探针文件时
`DELETE` 撞 `file_write` 限速，不退避会**静默少删**；上传建任务撞限速不退避会表现为"文件永远传不上去"。

---

## 8. 与规范文件的关系

- 本文是人读导读；**权威契约**是可机读的 `DOC/api/openapi.yaml`。
- web 仓库基于它做前端契约校验（`pnpm check:api`）；桌面端使用 gen-csharp 生成客户端代码。
- 契约变更时，`web` 与 `desktop` 各有一份 vendored 副本需**三处同步**。
