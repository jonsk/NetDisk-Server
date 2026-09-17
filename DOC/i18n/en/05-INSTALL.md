# Server-com Installation and Deployment

> This document covers **installing from scratch through to going live**; for daily inspection / troubleshooting, see [06-OPS.md](06-OPS.md), and for data protection see [07-BACKUP.md](07-BACKUP.md).
> The companion scripts for this repo live under `deploy/`, and most steps have ready-made scripts (idempotent, repeatable).
>
> **Every step has two approaches — pick either one**: **① Use scripts** (ready-made scripts under `deploy/`, idempotent, recommended) —
> **② Use manual commands** (no scripts at all; type the raw commands line by line, convenient for auditing and verifying each step individually).
> Both approaches produce an identical result, so just pick the one you prefer; you need not do both.

---

## 1. Deployment Form and Prerequisites

The entire netdisk is a **single Go binary** (`netdisk`); only four external components cooperate with it:

```
Nginx(80/443) ─rev-proxy─▶ netdisk(:8080) ─▶ PostgreSQL 17 (:5432)
                                          ─▶ Redis (:6379)
```

| Component | Version requirement | Where installed |
|---|---|---|
| netdisk | built artifact of this repo | local `/opt/netdisk/` |
| PostgreSQL | **minimum 17** (see below) | local |
| Redis | 7+ | local |
| Nginx | 1.26+ recommended | local (production) |

> **Why is the minimum supported PostgreSQL 17?**
> The primary-key default uses `gen_random_uuid()` (see migration script `00001_init.sql`) — this is a UUID generation function **built in since PG 13**, so the version floor is not tied to UUIDs. This project sets the minimum support at **PG 17** (aligned with the versions bundled by current mainstream distros), and the architecture document's ADR-1 also takes this as the standard. On-site deployments on 15/16 will also run (gen_random_uuid is compatible), but the archival / long-term support policy is maintained at 17. The system currently supports only this one database (no MySQL, etc.).

---

## 2. Install Dependency Software

> This step installs: the backup tools (borg/rsync), PostgreSQL 17, Redis, and Nginx.
> Key point: **Debian 13's system repository ships PostgreSQL 17**, which exactly matches this project's minimum supported version — just install it directly, **no need to add the PGDG source** (if the on-site system repository is older, e.g. Debian 12 ships 15, then follow the manual commands below to add PGDG and install 17).

```sh
# Let the script do it: install postgresql-17/redis/nginx/borg → disable Apache to free 80/443
sudo bash deploy/provision/01-install-packages.sh
```

The script prints each component's version number for confirmation, and finally outputs `INSTALL_DONE`.

> **Without relying on the script — do it step by step with manual commands** (equivalent to `01-install-packages.sh`):

```sh
# ① Install PostgreSQL 17 / Redis / Nginx / backup tool borg
#    (Debian 13 ships PG17, install directly; only add the PGDG source first if the
#     system repo version is too low, see note below)
sudo apt-get update
sudo apt-get install -y postgresql-17 redis-server nginx borgbackup

# ② Disable Apache to free 80/443 (only needed if Apache is installed on this machine)
sudo systemctl disable --now apache2 2>/dev/null || echo 'No Apache, skip'
```

> The script version wraps these 3 steps into a single `sudo bash deploy/provision/01-install-packages.sh`, and additionally does "print each component's version for confirmation + output `INSTALL_DONE`".

> Tip: the PGDG official source is slow in some regions; you can switch to a domestic mirror
> (e.g. `https://mirror.nju.edu.cn/postgresql/repos/apt`).

---

## 3. System Preparation (what this step does)

> Goal: create a **non-login** service account, build the program's directories and set their correct "ownership", and generate random passwords.

```sh
sudo bash deploy/provision/02-provision-base.sh
```

Specifically it does three things (each can be verified against the `ls` output):

1. **Create the service account `netdisk`**: an account used only to run the service, that cannot log in
   (given minimal privileges — the program only reads the binary and writes to its own data directory).
2. **Create directories and set ownership** (most critical; wrong ownership causes the service's startup self-check to reject outright):

   | Directory | Owner | What it holds |
   |---|---|---|
   | `/opt/netdisk/data` | netdisk | object and staging root (content-addressed objects, TUS staging) |
   | `/etc/netdisk` | root:netdisk | config files + secrets |
   | `/var/log/netdisk` | netdisk | application logs |
   | `/var/lib/netdisk` | netdisk | backup / drill status JSON |

3. **Generate the random password file `/etc/netdisk/secrets.env`**: one-time generation of the database password, JWT secret,
   Redis password, etc. (**never written into version control, and never written into yaml**), for subsequent scripts and the program to read.

> **Without relying on the script — do it step by step with manual commands** (one-to-one correspondence with the 3 things in `02-provision-base.sh`):

```sh
# ① Create the non-login service account netdisk (system account, no home dir, locked shell)
sudo useradd --system --no-create-home --shell /usr/sbin/nologin netdisk

# ② Create directories and set ownership (wrong ownership → startup self-check rejects outright)
sudo install -d -o netdisk -g netdisk -m 0750 /opt/netdisk/data    # object and staging root
sudo install -d -o root   -g netdisk -m 0750 /etc/netdisk          # config + secrets
sudo install -d -o netdisk -g netdisk -m 0750 /var/log/netdisk     # application logs
sudo install -d -o netdisk -g netdisk -m 0750 /var/lib/netdisk     # backup / drill status

# ③ Generate the random password file secrets.env (fetch a random string first, then write it, then tighten permissions)
DB_PASS=$(openssl rand -base64 24)
REDIS_PASS=$(openssl rand -base64 24)
JWT_SECRET=$(openssl rand -base64 48)
sudo tee /etc/netdisk/secrets.env >/dev/null <<EOF
NETDISK_DB_PASSWORD=$DB_PASS
NETDISK_REDIS_PASSWORD=$REDIS_PASS
NETDISK_JWT_SECRET=$JWT_SECRET
EOF
sudo chown root:netdisk /etc/netdisk/secrets.env
sudo chmod 0640 /etc/netdisk/secrets.env
```

> The field names follow the environment variables the program actually reads (refer to `deploy/systemd/secrets.env.example`).
> The script version combines these 3 steps into one `sudo bash deploy/provision/02-provision-base.sh`.
> Passwords are generated only once; to rotate, modify this file then restart the service.

---

## 4. Configure PostgreSQL

> This step tunes PG into a "suitable for this machine + secure" state: listen only on localhost, adapt to small memory,
> enable WAL archiving (prerequisite for point-in-time recovery), and create the program's dedicated role and database.

```sh
sudo bash deploy/provision/03-provision-postgresql.sh
```

The purpose of each configuration (all written into `/etc/postgresql/17/main/conf.d/`):

| Config | Value | Why |
|---|---|---|
| `listen_addresses` | `localhost` | listen only on localhost; the PG port **does not appear on the network interface** |
| `max_connections` | 100 | the application side uses at most 32 connections, leaving headroom for ops / psql |
| `shared_buffers` | 128MB | adapt to small-memory machines (otherwise the large-memory default would eat all memory) |
| `archive_mode=on` + `archive_command` | — | **WAL archiving**: the prerequisite for point-in-time recovery (PITR); keeps `pg_wal` from filling the disk |
| pg_hba | loopback only | reject LAN direct connections; only localhost access is allowed |

Finally it also:
- creates the role `netdisk` and the database `netdisk` (owner=netdisk);
- creates the `pg_trgm` extension (needed by the table-creation script; created beforehand by the superuser to avoid permission differences during migration).

> **Without relying on the script — do it step by step with manual commands** (equivalent to `03-provision-postgresql.sh`):

```sh
# ① Tuning + enable WAL archiving (written into conf.d, so it survives the next major-version upgrade)
sudo tee -a /etc/postgresql/17/main/conf.d/netdisk.conf >/dev/null <<'EOF'
listen_addresses = 'localhost'
max_connections = 100
shared_buffers = 128MB
archive_mode = on
archive_command = 'test ! -f /var/lib/postgresql/wal_archive/%f && cp %p /var/lib/postgresql/wal_archive/%f'
EOF
sudo install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/wal_archive

# ② Create the program's dedicated role and database (password taken from §3's secrets.env)
sudo -u postgres psql <<'EOF'
CREATE ROLE netdisk LOGIN PASSWORD '<database password>';
CREATE DATABASE netdisk OWNER netdisk ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
EOF

# ③ Create the pg_trgm extension (created in the database by the superuser, for use by migrations)
sudo -u postgres psql -d netdisk -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'

# ④ Restart to apply (Debian's default pg_hba already allows only localhost, no extra change needed)
sudo systemctl restart postgresql
```

> If the on-site database locale is not UTF-8, be sure to use the §2 `TEMPLATE template0` + `LC_COLLATE 'C'` approach to create the database, guaranteeing deterministic byte ordering and proximity to a Linux production environment.
> The script version combines the above 4 steps into one `sudo bash deploy/provision/03-provision-postgresql.sh` (includes version / connectivity self-checks).

---

## 5. Configure Redis

> This step tunes Redis into a state where "logins are not lost on restart, and users are not evicted".

```sh
sudo bash deploy/provision/04-provision-redis.sh
```

What Redis stores is not an ordinary cache, but **tokens / rate-limit windows / sync cursors** — losing it is equivalent to "all users being logged out".
So there are **three hard requirements**; missing any one will cause problems:

| Requirement | Value | What breaks if missing |
|---|---|---|
| Set an access password | `requirepass <password>` | login / rate-limiting entirely fails |
| Enable AOF persistence | `appendonly yes` + `appendfsync everysec` | one restart = all users logged out |
| Disable key eviction | `maxmemory-policy noeviction` | token / cursor keys evicted = users randomly kicked offline |

The script also: binds the listener to localhost (`bind 127.0.0.1`), and after restart actually tests with the password (unauthenticated must be rejected, authenticated can PING).

> The 96MB memory ceiling is for small-memory machines; when it is hit, because of `noeviction`, it will **error** rather than silently drop keys —
> errors expose the problem, whereas eviction would silently kick users offline.

> **Without relying on the script — do it step by step with manual commands** (equivalent to `04-provision-redis.sh`):

```sh
# Three hard requirements + bind localhost + small-memory ceiling, appended as a segment into Debian's Redis config
sudo tee -a /etc/redis/redis.conf >/dev/null <<EOF
bind 127.0.0.1
requirepass <Redis password>
appendonly yes
appendfsync everysec
maxmemory-policy noeviction
maxmemory 96mb
EOF
sudo systemctl restart redis-server

# Actual test: unauthenticated must be rejected, authenticated can PING
redis-cli -a <Redis password> --no-auth-warning PING   # should return PONG
```

> None of the three hard requirements can be omitted (see table above). The script version `sudo bash deploy/provision/04-provision-redis.sh`
> additionally does one thing: after restart, actually test with the password (unauthenticated must be rejected).

---

## 6. Build and Deploy netdisk

### 6.1 Build (order matters)

```sh
# Environment (domestic acceleration + checksum disabled)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off
cd Server-com
# ⚠ The admin console frontend is embedded into the binary at compile time: if you changed the frontend you must run pnpm build before go build,
#    reversing the order will embed the old page (it compiles, but does not error)
deploy/build-release.sh 1.0.0     # → deploy/dist/netdisk-1.0.0-linux-amd64
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<host>:/tmp/
```

> **Without relying on the script — do it step by step with manual commands** (equivalent to `build-release.sh`):
> The core order must not be reversed: **build the frontend with `pnpm build` first, then `go build`** (reversing the order embeds the old page).

```sh
# ① Environment (domestic acceleration + checksum disabled)
export GOPROXY=https://goproxy.cn,direct GOSUMDB=off

# ② Build the admin console frontend (web repo) first, and copy the artifacts into Server-com/internal/webui/dist (for embed)
cd Server-com/web && pnpm install && pnpm build
# After the artifacts are in place, the embed directive packages them into the binary (web/apps/*/dist → internal/webui/dist)

# ③ Compile the server-side single binary
cd ../ && go build -o deploy/dist/netdisk-1.0.0-linux-amd64 ./cmd/netdisk

# ④ Copy to the target machine
scp deploy/dist/netdisk-1.0.0-linux-amd64 root@<host>:/tmp/
```

### 6.2 Deploy to the target machine

```sh
sudo bash deploy/install.sh /tmp/netdisk-1.0.0-linux-amd64 http://<external URL>
```

`install.sh` is idempotent and will: create user / directories → generate / reuse secrets → install binary and config →
install the systemd service → self-check → start → **self-prove version** (on success the last line prints `INSTALL_DONE`).

> **Without relying on the script — do it step by step with manual commands** (equivalent to `install.sh`; the service account / directories / passwords are already prepared in §3):

```sh
# ① Place the binary and config (ownership must be correct, otherwise the self-check will not pass)
sudo install -o root -g netdisk -m 0755 /tmp/netdisk-1.0.0-linux-amd64 /opt/netdisk/netdisk
sudo install -o root -g netdisk -m 0644 deploy/config/config.example.yaml /etc/netdisk/config.yaml
# If §3 did not generate secrets, you need to add a copy (see §3 ③); if already generated, reuse it — do not overwrite

# ② Install the systemd unit and start it (the unit already contains "self-check ExecStartPre → migrate + start ExecStart")
sudo install -o root -g root -m 0644 deploy/systemd/netdisk.service /etc/systemd/system/netdisk.service
sudo systemctl daemon-reload
sudo systemctl enable --now netdisk

# ③ Confirm it is up
sudo systemctl is-active netdisk          # → active
sudo systemctl status netdisk -l --no-pager
```

> Key point: in `netdisk.service`, `ExecStart=/opt/netdisk/netdisk -migrate ...` means "migrate first, then continue starting",
> and **does not exit** — under the manual approach also do not treat it as a one-shot foreground command to wait on; let systemd manage it.

### 6.3 Database Migration

Migration is written into the systemd unit and **runs automatically at service startup**, no manual run needed:

```ini
ExecStartPre=/opt/netdisk/netdisk -check -config /etc/netdisk/config.yaml   # startup self-check
ExecStart=/opt/netdisk/netdisk -migrate -config /etc/netdisk/config.yaml    # migrate first, then continue starting
```

> Note: the semantics of `-migrate` are "run migrations first, then continue starting the service", and **it does not exit** —
> do not wait on it as a foreground command in a deployment script, or it will hang forever.

> If you **do not want to rely on systemd's automatic migration** and want to run the migration separately before startup, you can use goose to run `up` once against the target database
> (migration directory is `internal/migrate/sql`; usually not needed, it completes automatically at startup):

```sh
export NETDISK_DB_DSN='postgres://netdisk:<database password>@127.0.0.1:5432/netdisk?sslmode=disable'
goose -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" status   # check version first
goose -dir internal/migrate/sql postgres "$NETDISK_DB_DSN" up        # run migrations
```

> The two approaches are equivalent; as long as `-migrate` is in the systemd unit, even without manually running goose, migration happens at startup.

---

## 7. Configure the Nginx Reverse Proxy

> Nginx is the only external entry point; netdisk itself only listens on 127.0.0.1:8080.

```sh
# apply: install site / proxy headers / tuning; verify: use nginx -T (effective config) to check each item
sudo bash deploy/nginx/apply.sh
sudo bash deploy/nginx/verify.sh
```

**A few easy-to-trip pitfalls** (handled in the script; explained here for the reasons):
- The upload / TUS interface must turn off `proxy_request_buffering`, otherwise the request body lands on Nginx's temp disk first,
  and resumable upload and progress are no longer real;
- The download interface turns off `proxy_buffering`; the SSE interface turns off `proxy_buffering` and raises `proxy_read_timeout`.

> **Without relying on the script — do it step by step with manual commands** (equivalent to `apply.sh` + `verify.sh`):

```sh
# ① Write the site config (minimal usable, key lines annotated)
sudo tee /etc/nginx/sites-available/netdisk >/dev/null <<'EOF'
server {
    listen 80;
    server_name <external domain>;

    # Generic reverse proxy: pass through the real IP
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }

    # TUS upload: turn off request_buffering (so resumable / progress are real)
    location /uploads/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;
    }

    # WebDAV: also turn off request_buffering
    location /dav/ {
        proxy_pass http://127.0.0.1:8080;
        proxy_request_buffering off;
        proxy_buffering off;
    }

    # SSE: turn off buffering + relax read timeout
    location /sync/events {
        proxy_pass http://127.0.0.1:8080;
        proxy_buffering off;
        proxy_read_timeout 86400s;
    }
}
EOF

# ② Enable the site, check syntax, reload
sudo ln -sf /etc/nginx/sites-available/netdisk /etc/nginx/sites-enabled/
sudo nginx -t                                  # continue only if syntax is correct
sudo systemctl reload nginx

# ③ Replace verify.sh: self-check key items against the "effective config" (not the file on disk)
sudo nginx -T | grep -E 'proxy_(request_)?buffering|proxy_read_timeout'
```

> The easiest things to miss when writing by hand are the three `proxy_*` buffering settings above; if missed, upload / resumable / SSE will behave abnormally.
> The script version uses `deploy/nginx/verify.sh`, based on `nginx -T`, to assert each of these items one by one — it has verified them for you.

---

## 8. Post-Deployment Acceptance

```sh
# 1) Each entry point is reachable
curl -s -o /dev/null -w '%{http_code}\n' http://<host>/admin/    # 200 "netdisk admin console"
curl -s http://127.0.0.1:8080/healthz                            # 200

# 2) The initial admin was created automatically during "database initialization"
#    username admin, initial password admin123 (can be overridden before first startup with NETDISK_BOOTSTRAP_ADMIN_PASSWORD)
#    Please change the password immediately after logging in (recommended):
/opt/netdisk/bin/passwd -config /etc/netdisk/config.yaml \
  -username admin -role super_admin -prompt

# 3) End-to-end probe: connect directly to the app (do NOT go through the reverse proxy — the SSE response headers get consumed by Nginx)
/opt/netdisk/bin/probesmoke -base http://127.0.0.1:8080 -user admin -pass admin123
```

---

## 9. Next Steps After Installation

- Take an **initial backup** and run a recovery drill once (see [07-BACKUP.md](07-BACKUP.md)) — confirm "it can be recovered" before putting it into formal use;
- Configure monitoring to scrape `/metrics` and the alert rules (see [06-OPS.md](06-OPS.md)).
