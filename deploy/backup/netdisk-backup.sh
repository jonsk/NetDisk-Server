#!/bin/sh
# ============================================================================
# deploy/backup/netdisk-backup.sh —— PG 备份 + 对象目录备份 + WAL 归档自检(DP-04)
#
# 用法(以 root 运行,由 systemd timer 驱动):
#   netdisk-backup.sh full      # 每日:PG full(pg_dump -Fc)+ 对象目录 + WAL 归档自检
#   netdisk-backup.sh objects   # 每 4 小时:只做对象目录增量(窗口 ≤4h)
#
# 设计要点(每一条都是"备份失效"的常见死法):
#   ① **备份完立刻验读**:`pg_restore --list` 打不开就说明这份 dump 是废的。
#      只写不验的备份 = 一个你以为存在的文件。真正的验证是恢复演练(见 restore-drill)。
#   ② **WAL 归档要自检 failed_count**:archive_mode=on 但 archive_command 一直失败时,
#      pg_wal 会涨到把磁盘写满 —— 而"归档失败"本身没有任何人会发现。
#   ③ **状态 JSON 原子写**(临时文件 + mv):Prometheus 抓到半个 JSON 会直接报错,
#      而"备份状态未知"与"备份失败"是完全不同的事故。
#   ④ **保留期**:dump 14 天、borg 7 日/4 周/6 月;WAL 归档另由 sync-archive.sh 同步出机。
# ============================================================================
set -eu

MODE="${1:-full}"
PREFIX=/opt/netdisk
ETC=/etc/netdisk
BACKUP_ROOT="${BACKUP_ROOT:-/var/backups/netdisk}"
PG_BIN=/usr/lib/postgresql/18/bin
# ⚠ 归档目录**不能**放在 /var/lib/netdisk 下:那是 netdisk:netdisk 0750,
#   postgres 用户没有 x 权限,穿不过去 → 归档 100% 失败(PG 日志:Permission denied),
#   后果是 pg_wal 涨到写满磁盘。放在 postgres 自己的目录树里。
ARCHIVE_DIR=/var/lib/postgresql/wal_archive
ARCH_STATE=/var/lib/netdisk/pg-archiver.state
STATUS_FILE=/var/lib/netdisk/backup-status.json
KEEP_DUMPS_DAYS=14
BORG_REPO="${BORG_REPO:-/var/backups/netdisk/borg}"

log() { echo "[$(date '+%F %T')] $*"; }

mkdir -p "$BACKUP_ROOT/pg" "$BACKUP_ROOT/logs" "$BACKUP_ROOT/borg"
# pg_dump 是以 postgres 身份跑的(su - postgres),所以 dump 目录必须**属于 postgres**:
# root:root 0755 的目录会让 pg_dump 直接 "Permission denied"(而备份日志里
# 只有一行 su 报错,很容易被当成 pg_dump 自身的问题)。
chown postgres:postgres "$BACKUP_ROOT/pg"
chmod 0750 "$BACKUP_ROOT/pg"
LOG="$BACKUP_ROOT/logs/backup-$(date +%F).log"
exec >>"$LOG" 2>&1

# ---- 状态文件读写(原子)----
read_status() {
  if [ -f "$STATUS_FILE" ]; then
    cat "$STATUS_FILE"
  else
    echo '{"last_success_unix":0,"duration_seconds":0,"failed_total":0,"drill_last_unix":0}'
  fi
}
jget() { sed -n "s/.*\"$1\"[: ]*\([0-9.]*\).*/\1/p" <<EOF
$STATUS
EOF
}
write_status() {
  LAST="$1"; DUR="$2"; FAILED="$3"; DRILL="$4"
  TMP="$STATUS_FILE.tmp.$$"
  cat > "$TMP" <<EOF
{"last_success_unix":$LAST,"duration_seconds":$DUR,"failed_total":$FAILED,"drill_last_unix":$DRILL}
EOF
  chmod 0640 "$TMP"
  chown root:netdisk "$TMP" 2>/dev/null || true
  mv -f "$TMP" "$STATUS_FILE"
}

STATUS=$(read_status)
LAST_OK=$(jget last_success_unix); FAILED=$(jget failed_total); DRILL=$(jget drill_last_unix)
LAST_OK=${LAST_OK:-0}; FAILED=${FAILED:-0}; DRILL=${DRILL:-0}

START=$(date +%s)
mark_failed() {
  # 失败也要落状态:否则"备份一直没成功"在监控里看起来和"还没到时间"一样
  write_status "$LAST_OK" "$(( $(date +%s) - START ))" "$(( FAILED + 1 ))" "$DRILL"
  log "FAILED: $*"
  exit 1
}

log "==== 开始 mode=$MODE ===="

# ---- ① WAL 归档自检(两个模式都做:它是 PITR 的前提)----
set -a; . "$ETC/secrets.env"; set +a
ARCH=$(su - postgres -c "psql -Atc 'SELECT archived_count, failed_count FROM pg_stat_archiver'" netdisk 2>/dev/null || echo "0 0")
ARCH_OK=$(echo "$ARCH" | awk '{print $1}')
ARCH_FAILED=$(echo "$ARCH" | awk '{print $2}')
ARCH_FILES=$(find "$ARCHIVE_DIR" -type f 2>/dev/null | wc -l)

# ⚠ **failed_count 是累计值,永不复位**。所以判据必须是"有没有**新增**失败",
#   而不是"是不是 0" —— 后者会让"历史上失败过一次"变成"备份从此永远失败"
#   (本次实测就这么踩了一遍:修好归档后 archived_count=4 但 failed_count=33,
#    脚本仍然每一次都判失败)。基线存在一个小状态文件里。
PREV_FAILED=0
if [ -f "$ARCH_STATE" ]; then PREV_FAILED=$(cat "$ARCH_STATE" 2>/dev/null || echo 0); fi
echo "$ARCH_FAILED" > "$ARCH_STATE"; chmod 0640 "$ARCH_STATE"

log "WAL 归档:archived=$ARCH_OK failed_total=$ARCH_FAILED(上次 $PREV_FAILED) 归档目录文件数=$ARCH_FILES"
if [ "${ARCH_FAILED:-0}" -gt "${PREV_FAILED:-0}" ]; then
  mark_failed "WAL 归档新增 $(( ARCH_FAILED - PREV_FAILED )) 次失败(累计 $ARCH_FAILED)"
fi
if [ "${ARCH_OK:-0}" = "0" ] && [ "${ARCH_FILES:-0}" = "0" ]; then
  mark_failed "WAL 归档一个段都没成功过(archive_command 很可能没生效)"
fi

# ---- ② 对象目录 borg 增量(每个模式都做)----
export BORG_REPO
# borg 口令**只从受保护文件读**(0600 root),脚本里不留任何默认值:
# 写死在仓库里的 borg 口令 = 备份加密形同虚设(拿到仓库的人都能解开);
# 缺文件就**直接失败**,而不是退回一个"大家都知道的"口令。
BORG_PASS_FILE="${BORG_PASS_FILE:-/etc/netdisk/borg.passphrase}"
[ -r "$BORG_PASS_FILE" ] || mark_failed "读不到 $BORG_PASS_FILE(备份仓库口令;见 docs/ops/02)"
export BORG_PASSPHRASE="$(cat "$BORG_PASS_FILE")"
if [ ! -f "$BORG_REPO/config" ]; then
  log "borg 初始化 $BORG_REPO"
  borg init --encryption=repokey "$BORG_REPO" || mark_failed "borg init"
fi
log "borg create(对象目录增量)…"
borg create --stats --compression zstd,3 \
  "::$MODE-{now:%Y-%m-%dT%H:%M:%S}" "$PREFIX/data" || mark_failed "borg create"
# 保留期:7 日 / 4 周 / 6 月(对象是内容寻址的,重复数据不占空间)
borg prune --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --glob-archives "$MODE-*" || log "WARN: prune 失败(不致命)"

if [ "$MODE" = "objects" ]; then
  DUR=$(( $(date +%s) - START ))
  write_status "$(date +%s)" "$DUR" "$FAILED" "$DRILL"
  log "objects 模式完成,用时 ${DUR}s"
  exit 0
fi

# ---- ③ PG 每日 full ----
STAMP=$(date +%F_%H%M)
DUMP="$BACKUP_ROOT/pg/netdisk-$STAMP.dump"
log "pg_dump -Fc → $DUMP"
su - postgres -c "$PG_BIN/pg_dump -Fc -d netdisk -f $DUMP" || mark_failed "pg_dump"
chmod 0640 "$DUMP"

# 验读:打不开的 dump 等于没有备份
if ! su - postgres -c "$PG_BIN/pg_restore --list $DUMP" >/dev/null 2>&1; then
  mark_failed "生成的 dump 无法被 pg_restore 读取(这份备份是废的)"
fi
SZ=$(du -h "$DUMP" | awk '{print $1}')
log "dump 校验通过:大小 $SZ"

# ---- ④ 保留期清理 ----
DEL=$(find "$BACKUP_ROOT/pg" -name 'netdisk-*.dump' -mtime +$KEEP_DUMPS_DAYS | wc -l)
find "$BACKUP_ROOT/pg" -name 'netdisk-*.dump' -mtime +$KEEP_DUMPS_DAYS -delete
log "清理超过 ${KEEP_DUMPS_DAYS} 天的 dump:$DEL 个"

DUR=$(( $(date +%s) - START ))
write_status "$(date +%s)" "$DUR" "$FAILED" "$DRILL"
log "==== full 模式完成,用时 ${DUR}s;dump=$DUMP ===="
