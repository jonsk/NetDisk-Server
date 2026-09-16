#!/bin/sh
# 修 WAL 归档路径 + 复验 + 重跑备份(一键,便于复现)
#
# 根因(实测):归档目录放在 /var/lib/netdisk 下,而该目录是 drwxr-x--- netdisk:netdisk,
# 于是 **postgres 用户无法穿越它**去写归档 —— 归档目录本身属于 postgres 也没用
# (父目录没有 x 权限)。PG 日志里表现为:
#   cp: cannot stat '/var/lib/netdisk/pg_wal_archive/...': Permission denied
# 归档失败的后果不是"备份少了点东西",而是 pg_wal 会一路涨到写满磁盘。
set -eu

NEW_DIR=/var/lib/postgresql/wal_archive
CONF=/etc/postgresql/17/main/conf.d/20-netdisk-archive.conf

echo "== 1. 归档目录移到 postgres 自己的目录树(父目录可达)=="
install -d -o postgres -g postgres -m 0700 "$NEW_DIR"
rmdir /var/lib/netdisk/pg_wal_archive 2>/dev/null || true

echo "== 2. 更新 archive_command =="
cat > "$CONF" <<EOF
# 归档模式是 PITR 的前提:没有它,"恢复到某个时刻"只能恢复到最近一次 full。
# ⚠ 归档目录**不能**放在 /var/lib/netdisk 下面:那个目录是 netdisk:netdisk 0750,
#   postgres 用户没有 x 权限,穿不过去 → 归档 100% 失败(PG 日志:Permission denied),
#   而后果是 pg_wal 涨到写满磁盘。
archive_mode = on
archive_command = 'test ! -f $NEW_DIR/%f && cp %p $NEW_DIR/%f'
archive_timeout = 300
EOF
cat "$CONF"

echo "== 3. reload(archive_command 可热加载,不必重启)=="
systemctl reload postgresql
sleep 2

echo "== 4. 强制切一个 WAL 段并等归档 =="
su - postgres -c "psql -qAtc 'SELECT pg_switch_wal()'" netdisk
for i in 1 2 3 4 5 6 7 8 9 10; do
  N=$(ls "$NEW_DIR" 2>/dev/null | wc -l)
  [ "$N" -gt 0 ] && break
  sleep 1
done
echo "归档目录文件数=$(ls "$NEW_DIR" 2>/dev/null | wc -l)"
ls -l "$NEW_DIR" | tail -2
su - postgres -c "psql -tAc \"SELECT 'archived='||archived_count||' failed='||failed_count||' last='||coalesce(last_archived_wal,'(none)')\" FROM pg_stat_archiver" netdisk

echo "== 5. 重跑备份(这次 WAL 自检应当通过)=="
/opt/netdisk/bin/netdisk-backup.sh full
echo "backup_exit=$?"
tail -8 /var/backups/netdisk/logs/backup-$(date +%F).log
echo "ARCHIVE_FIX_DONE"
