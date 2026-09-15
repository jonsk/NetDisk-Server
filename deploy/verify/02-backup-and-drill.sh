#!/bin/sh
# 备份 + 恢复演练(归档修好之后)
set -u

echo "== 归档器状态 =="
su - postgres -c "psql -Atc 'SELECT archived_count, failed_count, coalesce(last_archived_wal, chr(45)) FROM pg_stat_archiver'" netdisk

echo ""
echo "== 1. 完整备份 =="
/opt/netdisk/bin/netdisk-backup.sh full
echo "backup_exit=$?"
tail -10 /var/backups/netdisk/logs/backup-$(date +%F).log

echo ""
echo "== 2. 产物与状态 =="
ls -lh /var/backups/netdisk/pg/ | tail -2
cat /var/lib/netdisk/backup-status.json; echo
echo "-- borg --"
BORG_REPO=/var/backups/netdisk/borg BORG_PASSPHRASE="$(cat /etc/netdisk/borg.passphrase)" borg list /var/backups/netdisk/borg 2>&1 | tail -3

echo ""
echo "== 3. 恢复演练(整库恢复到临时库 + 对象回放 + 行数比对)=="
/opt/netdisk/bin/netdisk-restore-drill.sh
echo "drill_exit=$?"
tail -22 /var/backups/netdisk/logs/drill-$(date +%F).log

echo ""
echo "== 4. 状态 =="
cat /var/lib/netdisk/backup-status.json; echo
echo "BACKUP_AND_DRILL_DONE"
