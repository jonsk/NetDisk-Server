# Server-com Architecture Design Document

> Version: ships with the Server-com community edition
> Related: [05-INSTALL.md](05-INSTALL.md) (installation), [06-OPS.md](06-OPS.md) (daily operations), [07-BACKUP.md](07-BACKUP.md) (backup/recovery), [08-TEST.md](08-TEST.md) (testing), [04-API_GUIDE.md](04-API_GUIDE.md) (API), [03-CODE_READING_GUIDE.md](03-CODE_READING_GUIDE.md) (code reading)

---

## 1. Overview and Positioning

`Server-com` is a **netdisk server backend**, written in **Go**, and compiled into **a single executable** (single binary).
It provides team shared spaces, multi-protocol uploads (TUS / WebDAV / multipart), versioned finalization, real-time sync awareness,
fine-grained permissions and quota governance. It can be deployed as a private netdisk or serve as an enterprise file hub.

The community edition (Server-com) keeps only **password login + admin console**, and is a functional subset of the commercial edition (Server-Ent).

**Core mental model: the whole service is a "big box", with four external components cooperating around it.**

## 2. Overall Architecture

### 2.1 Deployment Composition (the four-piece set)

| Component | Role | Notes |
|---|---|---|
| **netdisk** (product of this repo) | Receives HTTP requests, processes business logic, reads/writes metadata and objects | A single binary carrying REST / WebDAV / TUS / SSE / the embedded admin console |
| **PostgreSQL 17 (minimum supported)** | The sole metadata engine (everything — "directory entries / users / spaces / quotas" — lives here) | Includes WAL archiving for point-in-time recovery. **Minimum 17**: primary-key default uses `gen_random_uuid()` (built in since PG 13), so the version floor is not set by UUIDs; minimum is 17 (aligned with mainstream distro versions). Version requirements in [05-INSTALL.md](05-INSTALL.md) §1 |
| **Redis** | Sessions (tokens) / rate-limit windows / change-feed cursors / task queue | **Not a cache** — lose the tokens and everyone gets logged out |
| **Nginx** | Reverse proxy, single port outward | Only needed in production; in development you can hit the app directly |

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

### 2.2 Why a Single Binary + Embedded Frontend

The admin-console frontend, once built by the web repo, has its artifacts **compiled in at build time** into the Go binary (`internal/webui/dist/`),
and is served directly by Go via `embed` (`/admin`). Benefit: distribute a single file — no need to deploy a separate frontend static site on the server.

> ⚠️ Therefore, **changing the frontend requires recompiling the Go binary**: run `pnpm build` first, then `go build`.
> Reversing the order embeds stale artifacts (it compiles, but the page is the old version, and no error is raised).

## 3. Layered Architecture (internal package layout)

Code is split into 5 layers by responsibility; an upper layer may only depend on a lower layer (mechanically enforced by `depsguard`):

```
┌─────────────────────────────────────────────────────────────┐
│ 4. Entry layer   cmd/  (netdisk / migrate / passwd / depsguard …)  │
├─────────────────────────────────────────────────────────────┤
│ 3. Interface layer   internal/api/  (HTTP handlers, routing, middleware)   │
│            internal/webdavfs/ webdavauth/                     │
├─────────────────────────────────────────────────────────────┤
│ 2. Business layer   usersvc/orgsvc/spacesvc/filesvc/sharesvc/ │
│            uploadsvc/finalize/lifecycle/syncfeed/syncsse/    │
│            patrol/quotareconcile/consistency/credentials/    │
├─────────────────────────────────────────────────────────────┤
│ 1. Domain/data  repo/(data access) model/(domain model) objlock/ storage/  │
│            authsvc/ cache/ auth/ apierr/reqctx/middleware/    │
├─────────────────────────────────────────────────────────────┤
│ 0. Base      config/ db/ migrate/ ratelimit/ namepolicy/      │
│            dirops/ fastupload/ condreq/ webui/ obs/ syncssem  │
└─────────────────────────────────────────────────────────────┘
```

**Discipline (the core of the red-line R-xx family)**: "kernel packages" such as `objlock` (lock), `storage` (object storage), and `model` (domain model)
**must never depend on `internal/api` (the HTTP layer)**. Business logic must be decoupled from HTTP,
so that even if the access method changes (WebDAV/TUS/internal calls), the business rules stay the same.

## 4. Core Design Mechanisms

### 4.1 finalize — The Only Write Path

All uploads (TUS chunks, WebDAV PUT, multipart direct upload) ultimately **converge into the same finalization logic** `finalizeUpload()`:
1. First verify instant upload (if the content hash already exists, reuse it directly, guarding against "poisoning");
2. Write the content into the object area and obtain the **content-addressed** object;
3. In a short transaction, write the directory entry and reference count;
4. Write the change feed (sync_feed) to trigger sync awareness.

Benefit: the three upload channels behave exactly the same, eliminating the loophole of "one channel bypassing one rule".

### 4.2 Content-Addressed Object Storage

Objects are stored by **content hash**, with a path like `objects/xx/xx/<sha256>`:
- **Identical content is stored only once** (natural dedup): two users uploading the same file occupy only one copy on disk;
- File name is separated from content: renaming does not copy data, only metadata changes;
- Immutable objects: finalized content is never overwritten, eliminating "silent overwrite / file drift".

### 4.3 Two-Level Object Lock (ADR-2)

To prevent multiple requests from simultaneously finalizing/deleting the same object and corrupting data, an object-level lock is introduced:

| Level | Purpose | How the lock is held |
|---|---|---|
| **Session-level lock** | The entire finalize process, lifecycle worker (the real write path) | A dedicated single DB connection runs through it; holding the lock has an upper bound (capped) to prevent an overlong critical section |
| **Transaction-level lock** | Pure reference +1 (copy, share into a space) | Held only within a single short transaction |

The lock key is determined by the object hash, and locks are acquired in ascending hash order to avoid deadlock.

### 4.4 Dual-Mode Sync (SSE + Cursor)

Desktop clients need "as soon as the remote changes, I know":
- **SSE push** (`/sync/events`): pushes changes to online clients in real time (fast, but may drop frames);
- **Cursor pull** (`/changes`): client pulls incrementally with a cursor (`since`) (reliable, the fallback).

Both share **the same global `change_seq`** (one `sync_feed` table with a globally increasing sequence number),
so the client may alternate between the two, using `/changes` as the source of truth for catch-up pulls. This avoids periodic full scans while never losing changes.

### 4.5 JWT Split-End Authentication

After a successful login a JWT (HS256) is issued, split across two ends:
- The **web** end (admin console) and the **desktop** end (desktop client) have independent tokens (R-14),
  so a leak in one place does not affect everything;
- The access/refresh tokens are stored in Redis, supporting revocation and "single-flight refresh" (only one refresh request succeeds at a time).

The community edition keeps only **password login** (`/api/v1/auth/login`).

### 4.6 Directory Semantics and Naming Constraints

- Directory depth ≤ 31 levels AND a single cumulative path ≤ 240 bytes (two caps, configurable);
- File-name duplicate detection is case-insensitive (`lower(name)` + unique index);
- A directory-level "move/delete subtree" exceeding a threshold (default 1000 rows) is converted into an **async task**, returning a `task_id` for the client to poll.

## 5. Data Model (core tables)

| Table | Meaning |
|---|---|
| `users` | Users (password hash, email, display name, role) |
| `organizations` / `departments` / `groups` | Organization / department / group |
| `spaces` | Spaces (personal space + team space; includes quota `used_bytes`, `last_seq`) |
| `space_members` | Space members and their permissions |
| `files` | Directory entries (files and directories; `is_dir`, `parent_id`, `name`, `depth`, `size`) |
| `file_objects` | Content-addressed objects (hash, four states: live/pending_delete…, reference count `ref_count`) |
| `sync_feed` | Change feed (global `change_seq`, 90-day retention) |
| `shares` | Share links (token, permissions, expiration) |
| `audit_logs` | Audit logs (currently a reserved placeholder, not persisted in the community edition) |

## 6. Configuration Design

- **Defaults live in exactly one place**: in the Go struct `internal/config.Default()`; deleting a config section does not become zero-valued, but falls back to the default;
- **Secrets never go into yaml**: `JWT_SECRET`, the DB password, and the Redis password only travel via **environment variables** (`NETDISK_*`);
- Priority: Go default < config file < environment variable;
- Startup self-check (`netdisk -config ... -check`): any illegal item is **listed all at once** and exits non-zero, instead of waiting for the first request to 500;
- Durations use a human-readable format (`5m`/`30s`/`24h`).

## 7. Key Flows

### 7.1 Lifecycle of One Upload (TUS)

```
client ─PATCH chunks→ staging area (tus-tmp) ─all received→ finalizeUpload()
    finalize: compute hash → instant-upload dedup check → write object area (objects/) → short txn write files+file_objects
             → write sync_feed (change_seq++) → push SSE → return X-File-Id, done
```

### 7.2 Processing Chain of One Request

```
Nginx ─→ middleware chain (logging/recovery/rate-limit/auth) ─→ router (ServeMux) ─→ handler ─→ business service ─→ repo (DBA)
```

## 8. Interface Entry Overview

| Entry | Description |
|---|---|
| `REST /api/v1/*` | Auth, users, departments, spaces, files, directories, upload, download, share, change events |
| `Download & Range` | Streaming download, byte-level Range / conditional requests (If-Match, etc.) |
| `TUS /uploads/*` | Chunked resumable upload, ticket reuse, staging reclamation, disk watermark (>90% → 507) |
| `WebDAV /dav/*` | Standard WebDAV + PUT goes through finalize (≤100MB) |
| `SSE /sync/events` | Real-time change push |
| `cursor /changes` | Cursor incremental pull |
| `GET /metrics` | Prometheus monitoring metrics (by default only loopback / trusted proxy) |
| `GET /healthz` | Health check |
| `/admin/*` | Embedded admin console (Go embed) |

## 9. Architecture Red Lines (excerpt)

- Kernel packages must not depend on the HTTP/API layer (R-01 family);
- Global lock order is fixed: spaces row → files row → file_objects (advisory + row lock, ascending hash);
- OAuth / keys always go into environment variables, never into the version control repo;
- The patrol is **read-only**: it never auto-fixes, to prevent a misjudgment from being amplified into data corruption;
- Quota reconciliation, object patrol, and change-feed cleanup are all controlled background tasks, with no unbounded expansion.

## 10. Tech Stack

| Layer | Choice |
|---|---|
| Language | Go 1.26 |
| HTTP | Standard library `net/http` + `ServeMux` middleware chain |
| DB | pgx v5 + sqlc + goose migrations (PostgreSQL only) |
| Auth | golang-jwt v5 (HS256) |
| Storage | Local-disk object storage (content addressing; the `storage` package is extensible to multiple backends) |
| Config | yaml.v3 + strict environment-variable validation |
| Deployment | systemd + Nginx + Prometheus (single-port four-piece set, no containers) |

---

Installation and operations: [05-INSTALL.md](05-INSTALL.md) / [06-OPS.md](06-OPS.md) / [07-BACKUP.md](07-BACKUP.md); the capability list is in [01-FEATURE_LIST.md](01-FEATURE_LIST.md); API details are in [04-API_GUIDE.md](04-API_GUIDE.md).
