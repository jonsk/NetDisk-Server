# Server-com Daily Operations

> This document covers **how to manage after going live**: routine inspection, start/stop, upgrade, monitoring/alerting, common troubleshooting.
> For first-time installation see [05-INSTALL.md](05-INSTALL.md); for data protection see [07-BACKUP.md](07-BACKUP.md).

---

## 1. Routine Inspection

Below is an inspection checklist you can follow directly; it's recommended to go through it daily/weekly.

```sh
# ① Services and dependencies are all running
systemctl is-active postgresql redis-server netdisk nginx

# ② Check logs for anomalies
journalctl -u netdisk -n 50 --no-pager

# ③ Whether the backup succeeded recently (critical! see [07-BACKUP.md](07-BACKUP.md) for details)
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_backup_last_success_timestamp_seconds'

# ④ Whether the disk has enough space (uploads are rejected above 90%)
df -h /opt/netdisk/data /var/lib/postgresql /var/backups/netdisk
```

| Check item | What counts as normal | What to do on anomaly |
|---|---|---|
| All services active | all 4 are active | see §6 troubleshooting |
| PG data growing and WAL archive dir also growing | archiving advancing normally | archiving stuck = `pg_wal` will fill the disk |
| Last backup < 26h | timestamp is recent | check backup log immediately |
| Object patrol running | `netdisk_object_patrol_last_timestamp_seconds` within <48h | patrol stalled = leak/loss unobserved |
| Disk watermark < 90% | — | ≥90% rejects new uploads (returns 507) |

---

## 2. Start / Stop Order

> Order matters: **database first, app second, reverse proxy last**; reverse on stop.

```sh
# Start
systemctl start postgresql
systemctl start redis-server
systemctl start netdisk      # automatically completes DB migration on start
systemctl start nginx
systemctl is-active postgresql redis-server netdisk nginx

# Stop (stop Nginx first then the app, to gracefully drain in-progress uploads/downloads)
systemctl stop nginx
systemctl stop netdisk
systemctl stop redis-server
systemctl stop postgresql
```

---

## 3. Upgrade and Rollback

### 3.1 Upgrade the netdisk binary (routine)

```sh
deploy/build-release.sh <new-version>
scp deploy/dist/netdisk-<new-version>-linux-amd64 root@<host>:/tmp/
# Recommended to use install.sh (idempotent: auto stop old process → self-check → start → version self-proof)
sudo bash deploy/install.sh /tmp/netdisk-<new-version>-linux-amd64 http://<external URL>
```

> Rollback = `install` the **previous** binary back and restart (migrations are forward-compatible append-only, generally no need to roll back the DB).
> If the service was rate-limited and stopped due to repeated crashes (`systemctl start` reports "Start request repeated too quickly"),
> after fixing the root cause, first `systemctl reset-failed netdisk` then start.

### 3.2 Upgrade a PostgreSQL major version (e.g. 17→18)

> The local baseline is 17; if the site upgrades to a higher major version, use `pg_upgrade`. **You must have a backup verified to be recoverable before upgrading**,
> because if `pg_upgrade` fails, both the old and new instances may fail to start.

```sh
systemctl stop netdisk                       # stop the app first to avoid writes during upgrade
su - postgres -c "/usr/lib/postgresql/17/bin/pg_dumpall > /var/backups/netdisk/pre-upgrade.sql"
apt-get install -y postgresql-18             # install new version (PGDG source)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_upgrade \
  --old-datadir=/var/lib/postgresql/17/main --new-datadir=/var/lib/postgresql/18/main \
  --old-bindir=/usr/lib/postgresql/17/bin --new-bindir=/usr/lib/postgresql/17/bin --check"
# After --check passes, re-run without --check, then re-collect statistics
su - postgres -c "/usr/lib/postgresql/17/bin/vacuumdb --all --analyze-in-stages"
```

---

## 4. Monitoring and Alerting

- **Exposure point**: `GET /metrics` (Prometheus text format, a collector **implemented ourselves**).
  By default only **localhost loopback / trusted proxy** may scrape; cross-machine scraping requires `NETDISK_METRICS_TOKEN` with `Authorization: Bearer <token>`.
- **Scrape**: `deploy/prometheus/prometheus.yml` (default 30s interval).
- **Alert rules**: `deploy/prometheus/netdisk-alerts.yml` (a dozen or so), validated with `promtool check rules`.

**The metrics most worth watching** (meaning + criterion):

| Metric | Meaning | When to act |
|---|---|---|
| `netdisk_process_resident_memory_bytes` | process resident memory | near the cap (320M) the process is killed and restarted by systemd |
| `netdisk_disk_used_percent` | object-area disk watermark | ≥90% rejects uploads; `>85` warns first |
| `netdisk_backup_last_success_timestamp_seconds` | time of last successful backup | `now() - it > 26h` alerts (critical) |
| `netdisk_object_missing_total` | objects in DB but not on disk | **≥1 means data is unreadable** (critical) |
| `netdisk_object_leak_bytes` | on disk but not in DB (occupies space) | >64MiB = disk only grows, never shrinks |
| `netdisk_quota_drift_bytes` | difference between quota bookkeeping and real usage | >1MiB = quota calculation suspected buggy |
| `netdisk_restore_drill_last_timestamp_seconds` | time of last recovery drill | alert if not drilled in >90 days |

> **Important principle**: a metric that can't be read **produces no sample** rather than reporting 0 (reporting 0 would make the alert lie).
> For example, when disk-capacity probing fails, the three capacity curves disappear entirely — this is normal "can't be read", not "disk full".

---

## 5. Network Security and Permission Points

- `/metrics` returning **401** is **by design**: it only allows loopback/trusted-proxy scraping. Cross-machine scraping must carry a Bearer token.
- Secrets never go into yaml / the version repo; they exist only in `/etc/netdisk/secrets.env` (rotation = edit this file + restart the service).
- PostgreSQL listens only on loopback, Redis listens only on loopback: these two services' ports **should not appear on the NIC**.

---

## 6. Common Troubleshooting (runbook)

Each entry is written as **symptom → what to check first → common cause and handling**, commands can be pasted directly.

### 6.1 Service won't start

```sh
systemctl status netdisk -l
journalctl -u netdisk -n 60 --no-pager
```

| Symptom | Meaning | Handling |
|---|---|---|
| `ExecStartPre ... status=1/FAILURE` | **startup self-check blocked** (config/dir/port/dependency) | read its once-listed full problem checklist, fix item by item |
| `active (running)` but request 502 | service is up, but listen address doesn't match what Nginx points to | compare `http_addr` in config with the Nginx upstream |
| `Start request repeated too quickly` | repeated crashes triggered rate limiting | after fixing root cause `systemctl reset-failed netdisk` |

Two common self-check errors:
- **Directory not writable**: `chown -R netdisk:netdisk /opt/netdisk/data` (if you once manually created `objects/`, `tus-tmp/` as root, their owner is root).
- **Read-only filesystem**: the service is locked down by `ProtectSystem=strict` — if you changed a path in secrets, you must **also change the systemd unit's `ReadWritePaths`** to add the new path.

### 6.2 Upload/download stuck at 0%

```sh
curl -s http://127.0.0.1:8080/metrics | grep -E 'netdisk_(http|tus|sse)'
tail -f /var/log/nginx/netdisk.access.log | grep -E 'rt=|urt='
```

- `urt=` (upstream time) far less than `rt=` (total time) → time is spent on Nginx → check whether TUS/WebDAV turned off `proxy_request_buffering` (not off = request body lands on Nginx temp disk first, progress and resume are not truthful).
- Slow download → check the download interface's `proxy_buffering off`.
- SSE events delayed by tens of seconds → Nginx buffered the event stream → confirm the SSE interface `proxy_buffering off` + `proxy_read_timeout 86400s`.
  (These three are all asserted in `deploy/nginx/verify.sh`.)

### 6.3 Everyone logged out / logged out immediately after login

```sh
redis-cli -a <password> --no-auth-warning CONFIG GET appendonly appendfsync maxmemory-policy
```

- Any item not satisfied is abnormal — the **three hard requirements** (`requirepass` / `appendonly yes`+`everysec` / `maxmemory-policy noeviction`)
  and their per-item consequences are in [05-INSTALL.md](05-INSTALL.md) §5; fix item by item against the table then restart Redis.
- Remember: Redis stores **not a cache**; what's lost is login state and cursors, not file data, and it doesn't affect files already on disk.

### 6.4 Backup didn't succeed

```sh
systemctl status netdisk-backup.service
journalctl -u netdisk-backup -n 40 --no-pager
cat /var/lib/netdisk/backup-status.json
su - postgres -c "psql -Atc 'SELECT archived_count, failed_count FROM pg_stat_archiver' netdisk"
ls -l /var/lib/postgresql/wal_archive | tail -3
```

- **WAL archiving failed** → nine times out of ten the archive dir is unreachable: the archive dir **must not** be under `/var/lib/netdisk`
  (that's `netdisk:netdisk 0750`, the postgres user has no transit permission), it should be under `/var/lib/postgresql/wal_archive`
  (postgres's own directory tree).
- **`pg_dump` Permission denied** → the dump dir must belong to **postgres** (it runs as the postgres identity).

### 6.5 Object patrol alert

The audit is **read-only** and never auto-fixes — **before finding the cause, don't manually delete objects, don't run cleanup scripts**.
On an `object_missing` alert, use object replay from [07-BACKUP.md](07-BACKUP.md) to locate it.

### 6.6 Disk about to fill

```sh
du -sh /opt/netdisk/data /var/backups/netdisk /var/lib/postgresql/wal_archive /var/log
```

Three places grow: object dir, backups, WAL archive. **Don't** delete the WAL archive dir to free space
(that's the entirety of point-in-time recovery capability). Order: first sync backups off-machine, then delete local old dumps (scripts keep 14 days),
then consider letting the app's 24h reclamation window clean up tombstones of deleted objects (**don't do it manually**).

### 6.7 The most time-saving troubleshooting for "nothing changed but it just doesn't work"

```sh
ss -tlnp | grep -E '80|8080|5432|6379'      # 1. who's listening, where
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz   # 2. bypass proxy, hit app directly
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1/admin/         # 3. then through proxy
journalctl -u netdisk -n 20 --no-pager      # 4. view app logs (is client_ip propagated)
nginx -T | grep -A5 'location /tus'         # 5. view the proxy's "effective" config (not the file on disk)
```

Steps 2 and 3's difference immediately locates the problem to "app" or "proxy";
in step 4, if `client_ip` shows `127.0.0.1` but the actual client is on another machine, it means `X-Real-IP` isn't propagated or the trusted proxy is misconfigured.
