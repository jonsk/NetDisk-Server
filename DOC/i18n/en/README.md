# NetDisk · Open-Source Netdisk Server

> A self-hosted netdisk backend (single Go binary) for **team file collaboration**.
> It provides team shared spaces, multi-protocol uploads (TUS / WebDAV / multipart), versioned finalization,
> real-time sync awareness (SSE + cursor), and fine-grained permissions and quota governance — deployable as a private netdisk,
> or as an enterprise file hub.
>
> **License:** [Apache-2.0](../../../LICENSE) · **Edition:** Community Edition (Server-com)

---

## 🌐 Multi-language / Translations

[中文](../../../README.md) | [English](../en/README.md) | [Deutsch](../de/README.md) | [Français](../fr/README.md) | [Suomi](../fi/README.md) | [Русский](../ru/README.md)


---

## ✨ Features

| Dimension | Capability |
|---|---|
| 🚀 **Upload & Finalization** | TUS resumable chunked upload / WebDAV PUT / multipart — three entry points sharing one finalize path; content-addressed objects + instant-dedup + anti-poisoning verification |
| 🧠 **Object Lifecycle** | `file_objects` four-state model + object-level lock (two-layer mutual exclusion); write the object first, then a short transaction — eliminating silent overwrite and file drift |
| 💾 **Local Object Storage** | Content-addressed storage: objects are placed on local disk by content hash, identical content is stored only once (deduplication) |
| 📡 **Sync Awareness** | SSE real-time push + `/changes` dual-state cursor (global `change_seq`), zero periodic scanning, coordinated with the desktop client |
| 🔐 **Identity & Permissions** | Self-built account/password login + JWT (HS256), org/department/group, personal space / team space, share links, quota reconciliation |
| 🗂 **Directory Semantics** | Directory depth ≤ 31 / path ≤ 240 B budget; case-insensitive duplicate detection; MOVE/DELETE threshold offloading to async queue |
| 🛡 **Ops & Security** | Quota reconciliation + object patrol (leak/orphan) repair; object-level lock + rate limiting; auditing is a reserved placeholder (not persisted at present) |
| 📦 **Admin Console** | `/admin` admin console (users / orgs / spaces / quota), served by the Go `embed` single binary |

---

## 🏛 Architecture Overview

```
                       ┌──────────────────────────────────────┐
  Web admin console /admin ─▶│                                      │
  WebDAV client         ─WebDAV▶│        netdisk (Go single binary)          │
  TUS uploader          ─TUS─▶│   REST · TUS · WebDAV · SSE unified entry  │
  Desktop sync client   ─SSE──▶│                                      │
                       │   ┌──────────────────────────────┐   │
                       │   │ finalize: the only write path + object lock   │   │
                       │   │ content addressing → local-disk object storage │   │
                       │   └──────────────────────────────┘   │
                       └───────────┬──────────────────────────┘
                                   │
                     ┌─────────────┼─────────────┐
                     ▼             ▼             ▼
               ┌──────────┐  ┌────────┐   ┌────────────┐
               │PostgreSQL│  │ Redis  │   │Nginx(proxy)│
               │  metadata │  │sess/rl  │   │  single port │
               └──────────┘  └────────┘   └────────────┘
```

- **PostgreSQL 17 (minimum supported)** —— the sole metadata engine (with WAL archiving); primary keys use `gen_random_uuid()`, requiring PG ≥ 17
- **Redis** —— sessions / rate limiting / task queue (`SKIP LOCKED`, no MQ introduced)
- **Nginx** —— reverse proxy, single port exposed
- The admin console frontend is copied into `internal/webui/dist/` after `web` repo `pnpm build`, served by **Go `embed`**

---

## 🛠 Tech Stack

| Layer | Choice |
|---|---|
| Language | Go 1.26 |
| HTTP | Standard library `net/http` + `ServeMux` middleware chain |
| DB | pgx v5 + sqlc + goose migrations |
| Auth | golang-jwt v5 (HS256) |
| Storage | Local-disk object storage (content-addressed) |
| Config | yaml.v3 + strict environment-variable validation |
| Deployment | systemd + Nginx + Prometheus (single-port four-piece set) |

---

## 🚀 Quick Start

### Prerequisites

- Go 1.26+
- PostgreSQL 17+ (minimum 17; db: `netdisk` / `netdisk_test`, `LC_COLLATE=C`)
- Redis 7+ (`appendonly yes`, `noeviction`)
- Nginx (production)

### 1. Build

```bash
# Environment (domestic acceleration + checksum disabled)
export GOPROXY=https://goproxy.cn,direct
export GOSUMDB=off

go build ./...     # compile
go vet ./...       # static analysis
```

> **Note (embed):** if the frontend artifacts already exist in `internal/webui/dist/`, they are embedded at compile time;
> after updating the frontend you must run `pnpm build` before `go build`, otherwise the stale artifacts will be embedded.

### 2. Database Migration

```bash
go run ./cmd/migrate -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up
# or use the prebuilt goose binary
```

### 3. Configuration

Copy `deploy/config/config.example.yaml` to `config.yaml` and modify as needed.
**All secret-class config must go through environment variables** (`NETDISK_JWT_SECRET`, etc.; default/missing/too-short values will refuse to start),
never written into yaml — see `deploy/systemd/secrets.env.example`.

### 4. Run

```bash
go run ./cmd/netdisk
# listening on :8080
# /admin    admin console  /api/v1/*  REST  /dav/*  WebDAV  /changes   incremental pull  /sync/events  SSE
```

---

## 🔌 API Capabilities

| Entry | Description |
|---|---|
| `REST /api/v1/*` | auth, users, departments, spaces, files, directories, upload, download, share, events |
| `Download & Range` | streaming `ServeContent`; byte-level Range (200/206/416); If-Match/If-None-Match/If-Range conditional requests |
| `TUS /uploads/*` | chunked, resumable, ticket reuse, staging-area reclamation, disk watermark (>90% → 507) |
| `WebDAV /dav/*` | `x/net/webdav` + PUT interception routed to finalize (≤100MB); LOCK semantics self-implemented to fill gaps |
| `SSE /sync/events` | real-time push of remote changes (self-echo suppression) |
| `cursor /changes` | dual-state cursor incremental pull, global `change_seq` |
| Rate limiting | file_read / file_write tiered; 429 must be retried |

---

## 📂 Project Structure

```
Server-com/
├── cmd/           executable entry points
│   ├── netdisk         main service
│   ├── migrate         database migration
│   ├── passwd         (admin credentials)
│   ├── depsguard       architectural-discipline mechanical check
│   ├── testsgate       integration test gate
│   ├── coveragegate    coverage gate
│   ├── devdb           local dev database bootstrap
│   └── probesmoke      smoke probe
├── internal/          core logic (37 packages)
│   ├── finalize/       unique write path ★
│   ├── objlock/        object-level two-layer lock (ADR-2)
│   ├── storage/        local disk object storage
│   ├── uploadsvc/     TUS upload
│   ├── lifecycle/      object lifecycle worker
│   ├── syncfeed|syncsse/   sync awareness
│   ├── webdavfs|webdavauth/ WebDAV
│   ├── api/           REST handler
│   ├── patrol/        object patrol
│   └── quotareconcile/ quota reconciliation
├── deploy/           deployment artifacts (systemd/Nginx/Prometheus/backup/provision/verify)
├── scripts/          build & generation scripts
├── DOC/              feature list / architecture / installation / daily ops / backup & recovery / testing / API guide / code reading guide
│   └── api/          OpenAPI contract openapi.yaml (maintained in this repo; authoritative copy at GO\Doc\api)
└── LICENSE          Apache-2.0
```

---

## 🧪 Testing & Gates

- `go test ./...` —— unit tests
- `go test -tags=integration ./...` —— integration tests (requires `netdisk_test` db + Redis)
- `cmd/depsguard` —— red-line mechanical check (e.g., kernel packages must not depend on the HTTP layer)
- `cmd/testsgate` / `cmd/coveragegate` —— integration / coverage gates
- recommended to run locally before commit: `go build ./...` / `go vet ./...` / `cmd/depsguard`

---

## 🤝 Contributing

Before submitting, ensure `go build ./...`, `go vet ./...`, and `depsguard` are all green; new behavior must follow the red lines and ADRs in the architecture document V3.0; do not commit any secrets or credentials.

---

## 📄 License

This project is published under the **Apache License 2.0** ([Apache-2.0](../../../LICENSE)).
You are free to use, modify, and distribute it, including for **commercial** purposes (including closed-source derivatives),
but you must **retain the original copyright and license notices** and indicate changes in modified files.
This project is provided **as is** (AS IS), **without any express or implied warranty**; deployment, operations, and compliance responsibilities rest with the user.

### Relationship to the Commercial Edition

`Server-com` (this repo, Community Edition) carries no enterprise service commitments, and contains no credentials or private configuration.

---

*The complete capability list is in [`DOC/i18n/en/01-FEATURE_LIST.md`](DOC/i18n/en/01-FEATURE_LIST.md); deployment and operations are in [`DOC/i18n/en/05-INSTALL.md`](DOC/i18n/en/05-INSTALL.md) · [`DOC/i18n/en/06-OPS.md`](DOC/i18n/en/06-OPS.md) · [`DOC/i18n/en/07-BACKUP.md`](DOC/i18n/en/07-BACKUP.md); API details are in [`DOC/i18n/en/04-API_GUIDE.md`](DOC/i18n/en/04-API_GUIDE.md).*