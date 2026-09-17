# Server-com (Open-Source Edition) Feature List

> Version: compiled 2026-09-15
> This repo comes with no enterprise service commitment; use/modify/redistribute under the open-source license, self-bearing deployment and ops.
> Authoritative design: [02-ARCHITECTURE.md](02-ARCHITECTURE.md).

---

## I. Repository Positioning

| Item | Content |
|---|---|
| Name | **Server-com** (Open-Source Edition / Community) |
| Nature | Open-source release of the netdisk system's **backend service (Go)**; content = server source + deployment artifacts |
| Tech Stack | Go / PostgreSQL 17 / Redis / Nginx / local disk storage |
| External Capabilities | REST API + TUS resumable upload + WebDAV + SSE real-time sync |

**Directories**: `internal/` (core code), `cmd/` (CLI programs), `deploy/` (deployment artifacts), `scripts/` (helper scripts), `DOC/` (docs).

> Reader note: This list is organized around "what the system can do" so you can first grasp the full capability picture; for deeper code structure and entry points, please read the [03-CODE_READING_GUIDE.md](03-CODE_READING_GUIDE.md) in the same directory.

---

## II. What the System Provides Externally

Server-com is an **enterprise netdisk backend** that provides four types of capabilities externally; clients (admin console / desktop / file-protocol tools) operate the netdisk through them:

| Capability | Description | External Appearance |
|---|---|---|
| **REST API** | login, file CRUD, download, share, admin management, etc. | `/api/v1/*` |
| **TUS Resumable Upload** | large-file chunked upload, resumable after network interruption | `/tus/*` |
| **WebDAV** | compatible with the standard file protocol, can be mounted directly as a network drive | `/webdav/*` |
| **SSE Real-Time Sync** | pushes to client as soon as a file changes, then incremental pull | `/api/v1/events` |

---

## III. Functional Module List

### 1) Account & Authentication
- **Password Login**: username/password login, supports refresh-token renewal and logout.
- **Token Mechanism**: an access token is issued upon successful login; tokens are partitioned by **endpoint** (admin / desktop), and tokens for different endpoints have isolated capabilities.
- **Account Management**: the admin console can create users, enable/disable accounts, and adjust roles; supports fine-grained control over upload/download roles.
- **Password Security**: strong password validation and hashed storage; failed attempts trigger a temporary lockout policy against brute force.

### 2) Organization & Spaces
- **Org Structure**: supports maintaining the department tree (department CRUD, view subtree, rebuild closure); departments are one of the inputs to permissions.
- **Spaces**: each user has a **personal space**; can create **team spaces** and invite members to collaborate.
- **Member Management**: add/remove members of a space, adjust roles, transfer ownership, and disband; admins can perform global governance over spaces (quota, freeze, reclaim).

### 3) File Management
- **Metadata Operations**: browse files/directories, create directories, rename, move, delete.
- **Download**: supports streaming download, HTTP Range resume, conditional requests (If-Match, etc., optimistic locking).
- **Instant Upload (Dedup)**: files with identical content can skip redundant upload and directly reuse the stored object.
- **Edit Lock**: files can be locked to prevent multiple people from overwriting each other while editing.
- **Naming & Path Constraints**: filename validity, path length, directory depth, etc. are uniformly validated by the server.
- **Quota**: space quota management, blocks when exceeded; includes quota reconciliation and drift alerts.

### 4) Upload & Write
- **TUS Resumable Upload**: large-file chunked upload, supports interruption resume and concurrent chunks.
- **Unified Write Path**: all uploads (chunks, WebDAV writes) eventually converge into the same finalization entry, guaranteeing "one file has only one write source", naturally avoiding concurrent overwrite.
- **Content-Addressed Storage**: files are stored by content hash, identical content stored only once (dedup).
- **Lifecycle**: objects have a clear state transition from write to "deletable/recyclable", avoiding accidental deletion of in-progress files.

### 5) WebDAV & Sharing
- **WebDAV**: provides file read/write, directory operations, copy/move, locking, etc. via the standard WebDAV protocol, compatible with desktop-mounted drives.
- **Share Links**: files/directories can generate share links (accessible/downloadable without login), the system's **only login-free egress**; can be set to expire or revoked at any time.

### 6) Real-Time Sync
- **SSE Push**: when a file changes, it is pushed to online clients in real time via a long connection.
- **Incremental Pull**: clients incrementally pull changes via a cursor; even if a push is lost, eventual consistency is unaffected (pull is the authoritative path).
- Use case: bidirectional sync between desktop directories and the cloud.

### 7) Admin Console & Security
- **Admin Console**: `/admin` provides the admin management page (users, org, space governance).
- **Access Control**: the `web` endpoint token restricts admin access; H5/desktop tokens cannot enter the admin console.
- **Rate Limiting**: login, upload, file-read, and other API families have independent rate limits, forming a two-tier defense with Nginx; prevents brute force and API abuse.

### 8) Storage & Ops
- **Object Patrol**: the backend periodically scans the disk, finding object leaks (garbage with no references) and reverse orphans (references with no file).
- **Quota Reconciliation**: periodically compares "logical usage vs actual usage"; alerts when drift is too large and can auto-write-back corrections.
- **Deployment Form**: single binary + systemd + Nginx; backup/restore and verification scripts provided with the repo.
- **Storage Form**: local disk by default (content-addressed); local storage only, no third-party object-storage backends.

---

## IV. Deployment & Runtime Form

- **Single Binary**: the entire service compiles into one executable, managed by systemd; Nginx handles reverse proxy and HTTPS.
- **Prerequisites**: PostgreSQL 17 + Redis (auth/rate-limit dependencies; will not start if unavailable).
- **Configuration**: yaml file + environment variables; **passwords/secrets only via environment variables**, never written to yaml or committed to the repo.
- **Database Migration**: the service embeds migration scripts, executed automatically at deployment.
- **Admin Console Frontend**: already `embed`ded into the binary, no separate frontend directory deployment needed.

---

*This list is organized around "what the system can do" so you can first grasp the full capability picture; for deeper code structure and entry points, please read the [03-CODE_READING_GUIDE.md](03-CODE_READING_GUIDE.md) in the same directory.*
