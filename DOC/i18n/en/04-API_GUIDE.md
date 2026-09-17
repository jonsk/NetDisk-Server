# Server-com API Guide

> Spec file: the authoritative contract is `DOC/api/openapi.yaml` (OpenAPI v3); this document is its **human-readable guide**.
> Related: [02-ARCHITECTURE.md](02-ARCHITECTURE.md), [05-INSTALL.md](05-INSTALL.md), [06-OPS.md](06-OPS.md), [07-BACKUP.md](07-BACKUP.md)

---

## 1. Authentication Model

The service uses **JWT (HS256)** for authentication. After login you get an `access_token`, then send it on every request:

```
Authorization: Bearer <access_token>
```

**Tokens are split by end (R-14)**: tokens for the web end (admin console) and the desktop end (desktop client) are independent,
so a leak in one place does not affect everything. Tokens / refresh tokens are stored in Redis, supporting revocation and "single-flight refresh".

| Interface | Description |
|---|---|
| `POST /api/v1/auth/login` | Password login, returns `access_token` + `refresh_token` |
| `POST /api/v1/auth/refresh` | Exchange refresh token for a new access token |
| `POST /api/v1/auth/logout` | Logout, revoke token |
| `GET /api/v1/me` | Current user info |
| `GET /api/v1/version` | Server version |

> The community edition keeps only **password login**; WeCom/DingTalk/third-party IdP login has been removed in the community edition.
> Exceeding the window returns **429**, and the client should retry with backoff (see §7).

---

## 2. General Conventions

- **Base URL**: `/api/v1`, exposed via a single Nginx port in production.
- **Request/Response**: JSON (`Content-Type: application/json`).
- **Pagination**: list interfaces use `limit` + `after` (keyset cursor) for paging; `limit` max is **999**
  (exceeding it falls back to the default 200).
- **Download**: streaming response + byte-level `Range` (200/206/416) + conditional request headers (`If-Match`/`If-None-Match`/`If-Range`).
- **Rate limit**: per interface family (login/upload/file_list/file_read/file_write/webdav), two windows (second/minute) both active, taking the stricter one.

---

## 3. REST Interface Groups

### 3.1 Files and Directories

| Method and path | Description |
|---|---|
| `GET /api/v1/files?space=<id>&parent_id=&limit=&after=` | List directory (default 200 per page; supports keyset paging) |
| `POST /api/v1/files` | Create file / directory entry |
| `GET /api/v1/files/dirs` | List directory (directory-specific) |
| `GET /api/v1/files/{id}` | File / directory detail |
| `GET /api/v1/files/{id}/content` | Download content (supports Range) |
| `POST /api/v1/files/{id}/move` | Move / rename |
| `POST /api/v1/files/{id}/copy` | Copy |
| `POST /api/v1/files/{id}/share-to-space` | Copy / share into another space (via the object reference +1 path) |
| `POST /api/v1/files/{id}/lock` | Lock / unlock (object-level lock) |
| `GET /api/v1/files/{id}/subtree-stats` | Subtree statistics |

> A directory-level "move/delete subtree" exceeding a threshold (default 1000 rows) is converted into an **async task**:
> `POST` returns a `task_id`, and the client polls `GET /api/v1/tasks/{id}` for progress.

### 3.2 Upload (multipart direct upload)

| Method and path | Description |
|---|---|
| `POST /api/v1/upload/create` | Create upload (reserve quota, return upload token) |
| `POST /api/v1/upload/simple` | multipart direct upload (small file; finalized on a single write) |
| `POST /api/v1/upload/{id}/finish` | Complete / finalize |
| `GET/PATCH /api/v1/upload/{id}` | Query / resume |

> Large files go through the **TUS** protocol (see §4).

### 3.3 Changes and Sync

| Method and path | Description |
|---|---|
| `GET /api/v1/changes?since=<cursor>` | Cursor incremental pull (the "reliable" channel of the dual mode) |
| `GET /api/v1/changes/head` | Get the current latest cursor |
| `GET /api/v1/sync/cursors` | Manage sync cursors |
| `GET /sync/events` | SSE real-time push (the "real-time" channel of the dual mode) |

> Both share the same global `change_seq`: SSE may drop frames, but you can catch up by cursor via `/changes`, guaranteeing nothing is missed.

### 3.4 Shares

| Method and path | Description |
|---|---|
| `POST /api/v1/shares` | Create share link |
| `GET /api/v1/shares` | List of shares I created |
| `DELETE /api/v1/shares/{id}` | Revoke share |
| `GET /api/v1/shares/{token}/meta` | Share meta info (for landing page) |
| `GET /api/v1/shares/{token}/download` | Download by share token |

### 3.5 Spaces

| Method and path | Description |
|---|---|
| `POST /api/v1/spaces` | Create space (personal space / team space) |
| `GET /api/v1/spaces/{id}` | Space detail (includes quota) |
| `POST /api/v1/spaces/{id}/transfer` | Transfer space |
| `GET /api/v1/spaces/{id}/members` | Member list |
| `PUT/DELETE /api/v1/spaces/{id}/members/{userId}` | Add / remove member |
| `POST /api/v1/spaces/{id}/leave` | Leave space |

### 3.6 Admin Console (super_admin only)

| Method and path | Description |
|---|---|
| `POST/GET /api/v1/admin/departments` | Department management |
| `PUT/DELETE /api/v1/admin/departments/{id}` | Department edit / delete |
| `POST /api/v1/admin/spaces/{id}/freeze` | Freeze space (stop sync) |
| `GET /api/v1/audit/logs` | Audit logs (reserved placeholder in community edition) |

> The admin-console UI for users/organizations/spaces/quota is served by Go `embed` (`/admin/*`).
> Also: user-management related interfaces (create user, change role, configure quota) go through `/api/v1/admin/*` (see the full openapi).

---

## 4. TUS Chunked Upload (large files)

A resumable upload protocol for desktop clients / large files (`cmd/probesmoke` in `deploy` is a reference implementation):

| Step | Method | Key headers |
|---|---|---|
| Create task | `POST /tus` | `Tus-Resumable: 1.0.0`, `Upload-Length`, `Upload-Metadata: filename <base64>`, `Authorization: Bearer` |
| Upload chunk | `PATCH /tus/{id}` | `Tus-Resumable`, `Upload-Offset`, `X-Upload-Token`, `Content-Type: application/offset+octet-stream` |
| Finish | last chunk | response `200` + `Upload-Complete: true` + `X-File-Id` means finalized |

- **Resumable upload**: the client reports `Upload-Offset`, and the server continues from that offset;
- **Ticket reuse**: the upload ticket can be reused before finalization, idempotent;
- **Disk watermark**: when the staging area usage ≥ threshold (default 90%) it returns **507 Insufficient Storage**;
- **Staging reclamation**: staging files not completed in time are reclaimed by a background task (about once every 10 minutes).

---

## 5. WebDAV

Path prefix `/dav/*`, based on the standard `x/net/webdav`, most commonly used for "map network drive / file explorer":

- **PUT goes through the finalize path**: ≤100MB finalized directly (over the limit it prompts to use TUS);
- **LOCK semantics self-developed to complete** (the standard library only has an in-memory implementation);
- Range / conditional requests share the same origin as REST;
- Directory browsing sends multiple requests at once → hence `rate_limits.webdav` is configured fairly wide (60/s).

---

## 6. Health / Monitoring

| Path | Description |
|---|---|
| `GET /healthz` | Liveness check (200) |
| `GET /metrics` | Prometheus metrics; by default only loopback / trusted proxy, cross-machine needs `NETDISK_METRICS_TOKEN` Bearer |
| `GET /api/v1/version` | Version |

---

## 7. Status Codes and Semantics

| Status code | Semantics | Description |
|---|---|---|
| 200 / 201 | Success | Finalization-class success carries `X-File-Id` |
| 206 / 416 | Partial content / range not satisfiable | Download Range |
| 400 / 422 | Parameter error | Validation failure |
| 401 | Unauthenticated | Token missing / expired |
| 403 / 401 | Permission restricted | **Pause that space's sync, never delete locally** (client behavior) |
| 404 / 410 | Not found / gone | 410 = object / space migrated or deleted |
| 409 | Conflict | Version conflict, duplicate name (case-insensitive dedup) |
| 429 | Rate limited | Need backoff retry |
| 500 | Server error | See [06-OPS.md](06-OPS.md) for incident handling |
| 507 | Insufficient storage | Disk watermark reached threshold |

**Client handling of 429**: project experience is "429 backoff retry is mandatory" — when clearing probe files,
`DELETE` hit the `file_write` rate limit, and without backoff it would **silently under-delete**; hitting the upload-create rate limit without backoff shows up as "the file never finishes uploading".

---

## 8. Relationship with the Spec File

- This document is a human-readable guide; the **authoritative contract** is the machine-readable `DOC/api/openapi.yaml`.
- The web repo uses it for frontend contract verification (`pnpm check:api`); the desktop side uses gen-csharp to generate client code.
- When the contract changes, `web` and `desktop` each hold a vendored copy that needs **three-place sync**.
