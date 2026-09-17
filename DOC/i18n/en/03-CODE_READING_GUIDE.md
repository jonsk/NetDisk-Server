# Server-com Code Reading Guide

> Applies to: Server-com (Open-Source Edition / Community) · Go single-binary netdisk server
> Audience: engineers and ops folks who need to understand, troubleshoot, or develop this code further.
> This guide covers "how the code is organized, how data flows, and where to look to change X", not line-by-line algorithms.
> Per-module acceptance and development red lines are in `../../DOC/01-功能清单.md` in the same directory; authoritative design is `D:\WorkSpace\GO\Doc\网盘系统架构设计文档.md` (V3.0).

---

## 1. What This Repo Is

`Server-com` is an enterprise netdisk **backend service**, written in Go, ultimately compiled into **a single executable** (single binary).

It provides four types of capabilities externally:

| Capability | Description | Main Entry |
|---|---|---|
| REST admin/file API | admin management, file CRUD, download, share | `/api/v1/*` |
| TUS resumable upload | large-file chunked upload | `/tus/*` |
| WebDAV | desktop-mounted drive (file-protocol compatible) | `/webdav/*` |
| SSE real-time sync | pushes "file changed" to client, client then pulls increment | `/api/v1/events` |

It does **not** handle: HTTP reverse proxy (Nginx), frontend pages (`web` repo), desktop client (`desktop` repo). These three are separate repos or external components.

> In one sentence: this repo = a bunch of Go packages + a set of commands + migration scripts + deployment artifacts, ultimately producing a `netdisk` binary.

---

## 2. Top-Level Directory Structure

```
Server-com/
├── cmd/             CLI programs (8), see §3
├── internal/        core code (36 Go packages), see §4
├── deploy/          deployment artifacts: build scripts / config examples / systemd / nginx / backup / provision scripts
├── DOC/             repo documentation (the directory of this guide)
├── scripts/         helper scripts
├── .github/         CI pipelines
├── go.mod / go.sum Go dependency manifest
└── README.md        repo entry documentation
```

Reading suggestion: **first read the startup wiring in `cmd/netdisk/main.go` (§5)**, then follow "how a request flows" (§6) into `internal/api`, then dive into a specific business package as needed.

---

## 3. Commands (cmd/) — Each Is a Standalone Program

`cmd/` has 8 directories, each a `main` program:

| Command | What It Does | When to Use |
|---|---|---|
| **netdisk** | **Main service**, the only production-facing process | the one started by `systemd` |
| **migrate** | database migration | at deploy `go run ./cmd/migrate up` |
| **passwd** | account ops: create user / reset password / enable-disable | manual account ops by ops |
| **probesmoke** | end-to-end probe: real HTTP through login→upload→download | self-test during development |
| **devdb** | local dev/test database maintenance | for local developer use |
| **depsguard** | architecture gate: checks "kernel packages must not depend on HTTP layer", etc. | CI static check |
| **testsgate** | integration test gate: confirms tests actually ran | CI |
| **coveragegate** | coverage gate: per-package statement coverage statistics | CI |

Production only cares about the first two: `netdisk` and `migrate` (used at deploy). The rest are dev/CI helpers.

---

## 4. internal/ Package Map — Where Code Lives and What It Does

The 36 packages fall into five layers by responsibility. Understanding the layering is the key to reading this repo: **the HTTP layer only "receives requests, calls services, returns responses"; business logic lives in the service layer; actual DB read/write happens in the repo layer; the bottom layer is the database/Redis/disk.**

### 4.1 Entry & HTTP Layer (the first you touch in this repo)
| Package | Responsibility |
|---|---|
| **api** | **Route assembly**. All URL↔handler mappings live in `api.go`. Almost any "which handler does this endpoint call" can be found here |
| **middleware** | middleware chain: request ID, real IP, structured errors, logging, crash recovery. The "security checkpoints" a request passes through |
| **apierr** | unified error wrapper (HTTP status + business code + Chinese message) |
| **webui** | `/admin` admin frontend static-asset entry (embedded into the binary, Nginx no longer hosts it) |
| **reqctx** | tool to put the "current logged-in user" (Actor) into the request context |

### 4.2 Auth & Permissions
| Package | Responsibility |
|---|---|
| **auth** | JWT issuance/validation (the token itself, stateless) |
| **authsvc** | password login/refresh/logout business orchestration (DB lookup, password verify, lockout, token-version linkage) |
| **webdavauth** | WebDAV Basic auth channel |
| **credentials** | password hashing (bcrypt-class) and strength policy |

### 4.3 Business Service Layer (each maps to a business area, most worth reading closely)
| Package | Responsibility |
|---|---|
| **usersvc** | admin user management (create/enable-disable/roles) |
| **orgsvc** | org structure (department tree) |
| **spacesvc** | spaces (personal/team) and member collaboration |
| **filesvc** | file metadata (list/rename/move/delete/mkdir) |
| **uploadsvc** | upload task creation, ticket validation, TUS data plane |
| **fastupload** | "proof of possession" challenge for instant upload |
| **finalize** | **Finalization**—the unique entry where all uploads ultimately write, the most critical write path in the whole system |
| **sharesvc** | share links (the system's only login-free egress) |
| **dirops** | directory-level async task queue (huge directory ops run in background) |
| **quotareconcile** | quota reconciliation and drift alerts |
| **patrol** | object patrol: scan disk for leaks/orphan files |

### 4.4 Domain Model & Sync
| Package | Responsibility |
|---|---|
| **model** | entities (struct) strictly corresponding to DB tables |
| **namepolicy** | the sole judge of filenames (validity, length, depth) |
| **objlock** | object-level lock (mutual-exclusion discipline when writing the same file, prevents concurrent overwrite) |
| **syncfeed** | read/write of the change feed (who changed what) |
| **syncsse** | SSE event channel (pushed to clients) |

### 4.5 Infrastructure (most of the time you only need to know it exists)
| Package | Responsibility |
|---|---|
| **config** | config loading (yaml + environment variables) |
| **db** | PostgreSQL connection pool and transactions |
| **cache** | Redis wrapper and key-naming convention |
| **ratelimit** | Redis-based rate limiting |
| **repo** | SQL repository layer—**all SQL lives here**, the entry point for "looking at data" |
| **storage** | local disk object storage (content-addressed, files stored by hash) |
| **migrate** | database version migration (goose) |
| **obs** | structured logging (slog) |
| **lifecycle** | object lifecycle state machine |
| **condreq** | HTTP conditional requests (If-Match/If-None-Match, optimistic locking) |
| **webdavfs** | the underlying file view backing WebDAV |

> **Reading Quick-Reference**: to change "an HTTP endpoint" → find the route line in `internal/api/api.go` + the corresponding `handlers_*.go`;
> to check "how the DB is read/written" → look in `internal/repo/`;
> to find "where uploads ultimately hit disk" → see `internal/finalize/` (the unique write path).

---

## 5. Startup Wiring — How the Service Is "Assembled"

Everything starts from `cmd/netdisk/main.go`. Following the numbered steps in the comments, the assembly order is:

1. **Config**: defaults ← yaml ← environment variables (later overrides earlier)
2. **Validation**: print all issues at once; refuse to start if any problem (fail-fast)
3. **PostgreSQL**: connect, run migrations
4. **Redis**: refuse to start if it can't connect (auth/rate-limit both depend on it)
5. **Token Management**: initialize JWT issuance, Redis, rate limiter, and various business services
6. **HTTP Service**: mount the routes assembled by `internal/api` onto the port and start listening

This file is where "dependency injection" happens—**which services are newed and which dependencies are passed in are all in this one file**. To understand "how a service is assembled", read it; to add a dependency to the system, edit it too.

---

## 6. How a Request Flows (Understand the Flow, Understand the Repo)

Take "user requests the file list after login" as an example; the chain is:

```
        ┌────────────────────────────────────────────┐
        │  Nginx: HTTPS termination, reverse proxy,   │
        │  source-IP passthrough                      │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  middleware chain (internal/middleware)     │
        │  requestID → realIP → structuredErrors →   │
        │  logging → recoverer (crash fallback)      │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  api route (internal/api/api.go)            │
        │  auth → ratelimit → handler                 │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  handlers_*.go  parse params, call service  │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  business service layer (filesvc/          │
        │  uploadsvc/...)                             │
        └──────────────────┬─────────────────────────┘
                           ▼
        ┌────────────────────────────────────────────┐
        │  repo layer: "the only place for SQL" →     │
        │  PostgreSQL                                │
        │  storage layer: read/write disk objects     │
        └────────────────────────────────────────────┘
```

**Memorize this bolded line**: the `api` layer manages "the external appearance", the `repo` layer manages "dealing with the database", and the service layer in between manages "business rules". The clearer the layering, the more a change in one place affects only that place.

---

## 7. How Configuration Is Managed

See `../../deploy/config/config.example.yaml` (config example, the most authoritative field list), and `internal/config/config.go` (loading logic).

- Config source priority: **code defaults < yaml file < environment variables**.
- **Sensitive items (passwords, secrets) only via environment variables, never in yaml**. E.g. `NETDISK_DB_PASSWORD`, `JWT_SECRET`, `NETDISK_REDIS_PASSWORD`.
- Config blocks include: `server` (port/proxy), `database`, `redis`, `jwt` (token validity), `policy` (quota/depth/size limits), `patrol` (patrol), `webui` (admin prefix), `storage` (storage root dir), `log`, `rate_limits` (rate limits).
- Validation runs at startup; e.g. a rate limit all zeros, or a non-writable storage dir, will be blocked.

> Ops tip: change config → edit yaml or environment variables → restart `netdisk`; **changing the frontend requires recompiling the Go binary** (because pages are embedded), restarting the process alone is not enough.

---

## 8. Database — Core Table Quick Reference

Migrations live in `internal/migrate/sql/` (13 versioned scripts, embedded in the binary). Core business tables:

| Table | Stores |
|---|---|
| `users` | user accounts |
| `spaces` | personal/team spaces |
| `space_members` | space membership relations |
| `groups` / `group_members` | teams and members |
| `departments` / `department_closure` / `user_departments` | org structure (department tree + closure table) |
| `files` | file/directory metadata |
| `file_objects` | actual file objects (content-addressed) |
| `uploads` | upload tasks (TUS sessions) |
| `shares` | share links |
| `file_locks` | file edit locks |
| `refresh_tokens` | persistent login state (refresh token) |
| `sync_feed` / `sync_cursors` | sync change feed and cursors |
| `dir_op_tasks` | directory-level async tasks |

> Note: `audit_logs`, `idp_providers`, `idp_sync_state`, `user_idp_bindings`, `user_sso_bindings` are **legacy tables** left over from early features (audit, identity-source integration); the code no longer reads/writes them. Migration history must not be changed arbitrarily; just keep them normally.

---

## 9. HTTP Interface Quick Reference

All routes are centralized in `internal/api/api.go`. Grouped by function (auth details omitted):

| Group | Example Path | Description |
|---|---|---|
| Health / Version | `GET /healthz`, `GET /api/v1/version` | liveness, version negotiation (no login needed) |
| Auth | `POST /api/v1/auth/login` / `refresh` / `logout` | password login and token renewal |
| My Info | `GET /api/v1/me` | current logged-in user |
| Files | `GET /api/v1/files`, `GET /api/v1/files/{id}/content` | list, download (Range supported) |
| File Write Ops | `PATCH /api/v1/files/{id}`, `DELETE /api/v1/files/{id}`, `POST /api/v1/files/dirs` | rename, delete, mkdir |
| Upload | `POST /api/v1/upload/create`, `/tus/*` | create upload task + TUS chunked upload |
| Instant Upload | `POST /api/v1/upload/{id}/finish` | finalize after proof-of-possession passes |
| Edit Lock | `POST/GET/DELETE /api/v1/files/{id}/lock` | file collaborative edit lock |
| Share | `POST /api/v1/shares`, `GET /api/v1/shares/{token}/meta` | share links (login-free download) |
| Spaces | `GET /api/v1/spaces`, `POST /api/v1/spaces` | my spaces, create space |
| Org/User Mgmt | `/api/v1/admin/departments*`, `/api/v1/admin/users*` | admin management (requires admin) |
| Sync | `GET /api/v1/events` (SSE), `GET /api/v1/changes` | real-time push + incremental pull |
| WebDAV | `/webdav/*` (PROPFIND/PUT/COPY/MOVE/LOCK, etc.) | desktop-mounted drive |
| Tasks | `GET /api/v1/tasks/{id}` | directory-level async task progress query |

> To "find which file implements an endpoint": `api.go` writes it with `mux.Handle("METHOD /path", ...d.handleXxx...)`; `handleXxx` is the implementation function, generally in a `handlers_*.go` in the same directory.

---

## 10. A Few Key Designs to Grasp Before Reading Code

To keep non-senior readers from being lost, build these four mental models first:

1. **JWT endpoints**: tokens are split by endpoint `web` (admin) / `desktop` (desktop), etc.; **admin tokens cannot operate desktop capabilities** and vice versa. Tokens carry no permissions; permissions are always decided server-side.
2. **TUS resumable + single write path**: all uploads (TUS chunks, WebDAV PUT, instant upload) ultimately converge into the single `finalize` "finalization" entry, guaranteeing only one write path for files, so they don't fight each other.
3. **Sync dual-channel**: the server pushes "something changed" via SSE; the client then **pulls** the specific changes via the `/changes` cursor. Push loss is no problem—pull is authoritative. The cursor aligns with a global auto-increment number.
4. **Content-addressed storage**: `storage` stores objects by the file content's Hash; identical content stored only once (dedup), the filename is just metadata.

---

## 11. Third-Party Dependencies (go.mod) Are Lean

After `slimming`, dependencies are greatly reduced; currently the core dependencies are only:

- **pgx** (PostgreSQL driver), **goose** (migration)
- **go-redis** (Redis client)
- **golang-jwt/v5** (JWT)
- **miniredis** (test only), **yaml.v3** (config)

No message queue, no heavy framework, no multi-backend storage SDK (local disk is the only backend). This is friendly for troubleshooting: there isn't much "unfinishable dependency stack" to read.

---

## 12. Build / Run / Deploy Cheat Sheet

```bash
# Local build (requires Go 1.26+)
go build ./...            # compile all (verify no errors)
go vet ./...              # static check
go run ./cmd/depsguard    # architecture gate (also runs in CI)

# The actual deployment artifact (what deploy/build-release.sh does)
# Order matters: build frontend (web) first → then go build single binary → package
```

- The single binary is managed by `systemd`, Nginx does reverse proxy (`../../deploy/nginx/`).
- See `../../deploy/README.md` for detailed deployment.
- To read the main program entry logic directly: `cmd/netdisk/main.go`.

---

## 13. Where to Start Reading (For First-Timers)

1. `cmd/netdisk/main.go` —— read the assembly order (understand the whole in 5 minutes)
2. `internal/api/api.go` —— read all routes (know what APIs exist in 5 minutes)
3. `../../deploy/config/config.example.yaml` —— see what config knobs there are
4. Pick a business area you care about most: file upload → trace `finalize`, org structure → trace `orgsvc`, sync → trace `syncfeed`/`syncsse`
5. Debug a real problem: from `obs` logs / an endpoint error → back to `handlers_*.go` → `repo/*.go` to see the SQL

> Golden rule: **when you hit "what does this do", go read the comment at the top of that package file**—every package/function in this repo has extensive Chinese design comments, a "living document".
