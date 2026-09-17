# Server-com Backup and Recovery

> One-sentence core goal: **at any moment, restore "database + objects" to the same point in time**.
> Companion scripts live under `deploy/backup/`, driven automatically by systemd timers.

---

## 1. Backup Goals and Strategy

The netdisk has two kinds of data, **which must be restorable together to the same moment**:

1. **Metadata** (users/spaces/directory-entries/quota in PostgreSQL…);
2. **Object files** (the actual content, stored locally on disk by content hash).

Backing up only one but not the other yields a corrupted state of "file exists but DB points to a different object" or "file is all 0 bytes".

| Data | Method | Frequency | Retention |
|---|---|---|---|
| PostgreSQL | `pg_dump -Fc` (daily full-db backup) + **WAL archiving** (enables point-in-time recovery) | full db daily 02:30; WAL real-time | dump 14 days |
| Object dir `/opt/netdisk/data` | borg (dedup incremental) | **every 4 hours** | 7d / 4w / 6mo |
| Redis (AOF) | goes into the backup together with the object dir | same as above | same as above |

Two timers trigger automatically (both driven by `deploy/backup/netdisk-backup.sh`):

```sh
netdisk-backup.sh full      # daily: WAL self-check + object increment + PG full db + read-verify + retention cleanup
netdisk-backup.sh objects   # every 4 hours: object-dir increment only
```

### Three "failure-proof" designs in the backup implementation

1. **Verify by reading immediately after backup**: `pg_restore --list <dump>` failing to open is judged a failure — a backup that only writes but doesn't verify is just a file "you think exists".
2. **WAL archiving looks at "new failures" rather than "whether it's zero"**: `failed_count` is a cumulative value, never reset.
   Judging "whether it's 0" would turn one historical failure into "backup fails forever from then on". Now it records a baseline and only alerts on the **increment**.
3. **Atomic write of the status JSON** (temp file + `mv`): avoids Prometheus grabbing a half-written file.
   "Backup status unknown" and "backup failed" are two completely different incidents.

---

## 2. Can the backup actually be recovered? — Recovery Drill

> Backup ≠ recoverable. The real verification is the **recovery drill**: once per quarter, `deploy/backup/netdisk-restore-drill.sh`.

The drill does four things, **any one failing counts as a drill failure**:

1. Restore the most recent backup **to an independent temporary db** (`netdisk_drill_<date>`, **never touch the production db**);
2. **Key-table row-count comparison** production vs restored db (users/spaces/files/file_objects/space_members/sync_feed/audit_logs);
3. **Object-dir replay**: restore from backup to a temp dir, compare content hash (sha256) of each live object one by one;
4. Write the drill timestamp; alert if **>90 days since last drill**.

```sh
sudo /opt/netdisk/bin/netdisk-restore-drill.sh     # logged to /var/backups/netdisk/logs/drill-<date>.log
```

> You must keep at least one live object, otherwise the drill can only verify "archive can be decompressed", not "content can be replayed"
> (a restored 0-byte file is still "present"). You can create one via WebDAV direct upload:
> `curl -u admin:<password> -T <local-file> http://127.0.0.1:8080/webdav/<space_id>/<filename>`
> (the path **must include space_id**).

---

## 3. Point-in-Time Recovery (PITR): restore to a certain moment

> Prerequisite: `archive_mode=on` + `archive_command` working (configured at install), and **a base physical backup** as the starting point.

```sh
# ① Take a base backup (physical backup, combined with WAL to "go back in time")
su - postgres -c "/usr/lib/postgresql/17/bin/pg_basebackup -D /var/backups/netdisk/base -Ft -z -X fetch"

# ② Restore to an "independent instance dir" (don't overwrite production data)
install -d -o postgres -g postgres -m 0700 /var/lib/postgresql/17/restore
tar -xzf /var/backups/netdisk/base/base.tar.gz -C /var/lib/postgresql/17/restore

# ③ Write the recovery target (restore to the moment 2026-09-12 19:53)
cat >> /var/lib/postgresql/17/restore/postgresql.auto.conf <<'EOF'
restore_command = 'cp /var/lib/postgresql/wal_archive/%f %p'
recovery_target_time = '2026-09-12 19:53:00+08'
recovery_target_action = 'promote'
EOF
touch /var/lib/postgresql/17/restore/recovery.signal
chown -R postgres:postgres /var/lib/postgresql/17/restore

# ④ Start the recovery instance on another port (coexists with production, doesn't touch production)
su - postgres -c "/usr/lib/postgresql/17/bin/pg_ctl -D /var/lib/postgresql/17/restore \
  -o '-p 5433' -l /tmp/pitr.log start"
su - postgres -c "psql -p 5433 -Atc 'SELECT count(*) FROM files' netdisk"
```

The **PITR drill script** (`netdisk-pitr-drill.sh`) automates this set and really "does a time travel",
the criterion is not "the db can start", but **data after the target moment must not be present**:
insert `before`(T0) → take physical backup → record target moment T → insert `after`(T1>T) →
restore from backup to T → assert: `before` exists in db, `after` **does not exist**.

Practical points:
- The base backup uses **plain format** (`-Fp`, directly landing as a usable data dir) which is easier than `-Ft` (compressed package);
- The recovery instance's `max_connections` etc. **must be ≥ the primary**, otherwise PG directly refuses recovery;
- Drill on a **replica**: after promote the dir has been written to, no longer "the backup of that moment",
  the base backup is an artifact, the drill may only touch its replica.

---

## 4. Object File Replay (done separately)

```sh
export BORG_REPO=/var/backups/netdisk/borg BORG_PASSPHRASE=$(cat /etc/netdisk/borg.passphrase)
borg list --last 3 "$BORG_REPO"                  # view the most recent few
cd /tmp/restore && borg extract "$BORG_REPO::full-2026-09-12T19:53:38"
# restored as /tmp/restore/opt/netdisk/data/objects/xx/yy/<sha256>
```

> **Objects and db must come from the same moment**: replaying only objects without the db = "file exists but db points to a different object";
> replaying only the db without objects = "file is all 0 bytes". The drill script binds the two together precisely to prevent this.

---

## 5. Redis AOF

What Redis stores is login state and cursors (**not a cache**; the "three hard requirements" for persistence that must be on are in [05-INSTALL.md](05-INSTALL.md) §5). The point of this section is to also back up the AOF:

```sh
redis-cli -a <password> --no-auth-warning BGREWRITEAOF      # manually trigger an AOF rewrite
ls -l /var/lib/redis/appendonlydir/                     # AOF and manifest land here
```

Backup method: go into borg together with the object dir, or `cp` separately after daily `BGREWRITEAOF`. **Losing AOF → all users re-login** (data not lost).

---

## 6. Corresponding Alerts

Alerts strongly related to backup/recovery (rules in `deploy/prometheus/netdisk-alerts.yml`, metric criteria in [06-OPS.md](06-OPS.md) §4):

- `NetdiskBackupStale` / `NetdiskObjectsBackupStale`: full-db/object backup didn't succeed past the expected frequency;
- `NetdiskWalArchiveFailing`: WAL archiving keeps failing, `pg_wal` will fill the disk;
- `NetdiskRestoreDrillOverdue`: >90 days since last drill, time to do a recovery drill.

---

## 7. Not Yet Done (stated honestly)

- **Weekly base physical backup** (the "starting point" of PITR). Currently `netdisk-backup.sh` only does `pg_dump` (logical backup),
  and does not produce a physical base backup — so the range that can "go back in time" is limited by the base backup left by manual drills.
  Recommended to add a **weekly `pg_basebackup` timer** before going live (keep 4 copies).
- **Off-site replica**. Currently all backup replicas are on the same disk — **disk dies = data and backup both gone**,
  this is the biggest single point of failure in the current scheme. Recommended to add a borg remote repo or object-storage replica, syncing backups off-machine.
