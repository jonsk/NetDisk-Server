#!/bin/sh
# ============================================================================
# deploy/backup/netdisk-restore-drill.sh —— 恢复演练(DP-03/DP-04;季度一次)
#
# 演练做三件事,任何一件失败都算演练失败:
#   ① **整库恢复**:把最近一次 pg_dump 恢复到一个**独立的临时库**(绝不碰生产库),
#      再比对关键表的行数与生产库是否一致 —— 行数对不上说明这份备份不能用来救火。
#   ② **对象目录回放**:从 borg 恢复对象目录到临时目录,抽查 PG 里登记的对象键
#      是否真的在恢复出来的数据里存在(反向只靠 dump 恢复会得到"文件全 0 字节")。
#   ③ **记录演练时刻**:写进状态 JSON 的 drill_last_unix → 指标
#      netdisk_restore_drill_last_timestamp_seconds → 超 90 天未演练即告警。
#
# 为什么必须有这条"演练"而不是只有"备份成功":
#   备份成功只证明**写出来了**,不证明**能恢复**。两者的差距只有真正恢复一次才知道:
#   版本不匹配、dump 里缺了某个 schema、对象键与文件不对应 —— 这些在全绿备份里完全看不出来。
#   验收标准写的也是"第三方照做能完成一次恢复",所以这里有**实测 RTO**输出。
# ============================================================================
set -eu

PG_BIN=/usr/lib/postgresql/18/bin
BACKUP_ROOT="${BACKUP_ROOT:-/var/backups/netdisk}"
STATUS_FILE=/var/lib/netdisk/backup-status.json
DRILL_DB=netdisk_drill_$(date +%Y%m%d)
WORK=$(mktemp -d /tmp/netdisk-drill-XXXXXX)
PREFIX=/opt/netdisk
BORG_REPO="${BORG_REPO:-/var/backups/netdisk/borg}"

LOG="$BACKUP_ROOT/logs/drill-$(date +%F).log"
mkdir -p "$BACKUP_ROOT/logs"
exec >>"$LOG" 2>&1

log() { echo "[$(date '+%F %T')] $*"; }
fail() { log "演练失败: $*"; cleanup; exit 1; }
cleanup() { rm -rf "$WORK"; su - postgres -c "$PG_BIN/dropdb --if-exists $DRILL_DB" >/dev/null 2>&1 || true; }

START=$(date +%s)
log "==== 恢复演练开始(drill db=$DRILL_DB,工作目录 $WORK)===="

# ---- 0. 前置:最近一次 dump ----
DUMP=$(ls -t "$BACKUP_ROOT"/pg/netdisk-*.dump 2>/dev/null | head -1 || true)
[ -n "$DUMP" ] || fail "没有可用的 dump(先跑 netdisk-backup.sh full)"
log "使用 dump: $DUMP"

# ---- 1. 整库恢复到临时库 ----
su - postgres -c "$PG_BIN/createdb -O netdisk $DRILL_DB" || fail "createdb $DRILL_DB"
# pg_trgm 属于"迁移里 CREATE EXTENSION",恢复时用超级用户保证它建得上
su - postgres -c "$PG_BIN/psql -q -d $DRILL_DB -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'" || log "WARN: pg_trgm 预建失败(继续)"
su - postgres -c "$PG_BIN/pg_restore --no-owner --no-privileges -d $DRILL_DB $DUMP" || fail "pg_restore 失败"

T_RESTORE=$(( $(date +%s) - START ))
log "整库恢复完成,用时 ${T_RESTORE}s"

# ---- 2. 行数比对(生产 vs 演练库)----
# ⚠ 两个坑都在这里踩过:
#   ① 库名必须写在 **su 引号里面**:`su - postgres -c "psql ... " netdisk` 里的
#      netdisk 是给 su 的参数、不是给 psql 的 → psql 连到默认库、表不存在 → 查询失败;
#   ② 查询失败绝不能算"两边一致":上一版把 `ERR = ERR` 判成 ✓,于是整张表
#      **一个数字都没读到,却全绿**(演练报告"成功"而实际什么都没验证)。
log "---- 关键表行数比对 ----"
MISMATCH=0
for t in users spaces files file_objects space_members sync_feed audit_logs; do
  A=$(su - postgres -c "psql -Atc \"SELECT count(*) FROM $t\" netdisk" 2>/dev/null || echo ERR)
  B=$(su - postgres -c "psql -Atc \"SELECT count(*) FROM $t\" $DRILL_DB" 2>/dev/null || echo ERR)
  case "$A$B" in
    *ERR*) log "  ✗ $t: 生产=$A 恢复=$B(查询失败,不能判定为一致)"; MISMATCH=$((MISMATCH+1)); continue ;;
  esac
  case "$A" in ''|*[!0-9]*) log "  ✗ $t: 生产侧返回非数字 '$A'"; MISMATCH=$((MISMATCH+1)); continue ;; esac
  if [ "$A" = "$B" ]; then
    log "  ✓ $t: $A"
  else
    log "  ✗ $t: 生产=$A 恢复=$B"
    MISMATCH=$((MISMATCH+1))
  fi
done
[ "$MISMATCH" -eq 0 ] || fail "$MISMATCH 张表行数不一致或读不到(这份备份救不了火)"

# ---- 3. 对象目录回放 + 抽查对象**内容**(比对 sha256)----
# 只确认"文件存在"不够:恢复出 0 字节的文件同样"存在"。
# file_objects.sha256 是库里的真相;恢复出来的对象文件必须与它逐字节一致。
# borg 口令**只从受保护文件读**(0600 root),脚本里不留任何默认值:
# 一个写死在仓库里的 borg 口令 = 备份加密形同虚设(任何拿到仓库的人都能解开)。
# 缺文件就**直接失败**,而不是退回一个"大家都知道的"口令。
BORG_PASS_FILE="${BORG_PASS_FILE:-/etc/netdisk/borg.passphrase}"
[ -r "$BORG_PASS_FILE" ] || { log "FAILED: 读不到 $BORG_PASS_FILE(备份仓库口令;见 docs/ops/02)"; exit 1; }
export BORG_REPO
export BORG_PASSPHRASE="$(cat "$BORG_PASS_FILE")"
log "---- 对象目录回放 ----"
LATEST_ARCH=$(borg list --last 1 --format '{archive}' "$BORG_REPO" 2>/dev/null | head -1 || true)
[ -n "$LATEST_ARCH" ] || fail "borg 仓库里没有归档"
log "使用 borg 归档: $LATEST_ARCH"
(cd "$WORK" && borg extract "$BORG_REPO::$LATEST_ARCH") || fail "borg extract 失败"

# 只抽查**存活**对象(state=live;实测枚举值就是 live,不是 active):pending_delete 的行是"墓碑",
# 它们的对象文件已经不再需要存在,拿它们做内容比对会得到一个假的失败。
OBJ_TOTAL=$(su - postgres -c "psql -Atc \"SELECT count(*) FROM file_objects WHERE state = 'live'\" $DRILL_DB" 2>/dev/null || echo ERR)
log "恢复库中存活对象数: $OBJ_TOTAL"
if [ "$OBJ_TOTAL" = "ERR" ]; then
  fail "读不到 file_objects 行数"
elif [ "$OBJ_TOTAL" = "0" ]; then
  log "  ⚠ 库里没有存活对象:本次只验证了「归档可解」,没有验证「对象内容可回放」。"
  log "    要让演练有实质意义:先上传一个文件(WebDAV 路径是 /webdav/<space_id>/<name>),再跑演练。"
else
  su - postgres -c "psql -Atc \"SELECT object_key || chr(9) || hash_sha256 FROM file_objects WHERE state = 'live' ORDER BY object_key LIMIT 5\" $DRILL_DB" 2>/dev/null > "$WORK/keys.tsv" || true
  CHECKED=0; BAD=0
  while IFS="$(printf '\t')" read -r key want; do
    [ -n "$key" ] || continue
    CHECKED=$((CHECKED+1))
    # ⚠ 路径是 **$WORK/opt/netdisk/data/$key**,不是 .../data/objects/$key:
    #   object_key 本身就带 `objects/` 前缀(实测形如 objects/74/63/7463…),
    #   再拼一次 objects/ 会查到一个不存在的路径,于是报"恢复出来是空的"——
    #   而真相是**路径拼错了**,不是备份坏了。
    got=$(sha256sum "$WORK/opt/netdisk/data/$key" 2>/dev/null | awk '{print $1}')
    if [ "$got" = "$want" ]; then
      log "  ✓ 对象内容一致: $key"
    else
      log "  ✗ 对象内容不一致: $key(库=${want} 恢复=${got:-缺失})"
      BAD=$((BAD+1))
    fi
  done < "$WORK/keys.tsv"
  log "对象内容抽查: 一致 $(( CHECKED - BAD )) / $CHECKED"
  [ "$CHECKED" -gt 0 ] || fail "一个对象都没抽查到(无法证明对象可回放)"
  [ "${BAD:-0}" -eq 0 ] || fail "有 $BAD 个对象内容与库中 sha256 不一致(备份与库不匹配)"
fi

# ---- 4. 记录演练时刻(指标 → 超 90 天告警)----
TOTAL=$(( $(date +%s) - START ))
NOW=$(date +%s)
STATUS=$(cat "$STATUS_FILE" 2>/dev/null || echo '{"last_success_unix":0,"duration_seconds":0,"failed_total":0,"drill_last_unix":0}')
LAST_OK=$(echo "$STATUS" | sed -n 's/.*"last_success_unix"[: ]*\([0-9.]*\).*/\1/p')
DUR=$(echo "$STATUS" | sed -n 's/.*"duration_seconds"[: ]*\([0-9.]*\).*/\1/p')
FAILED=$(echo "$STATUS" | sed -n 's/.*"failed_total"[: ]*\([0-9.]*\).*/\1/p')
TMP="$STATUS_FILE.tmp.$$"
printf '{"last_success_unix":%s,"duration_seconds":%s,"failed_total":%s,"drill_last_unix":%s}\n' \
  "${LAST_OK:-0}" "${DUR:-0}" "${FAILED:-0}" "$NOW" > "$TMP"
chmod 0640 "$TMP"; chown root:netdisk "$TMP" 2>/dev/null || true
mv -f "$TMP" "$STATUS_FILE"

cleanup
log "==== 演练成功:整库恢复 ${T_RESTORE}s,总计 ${TOTAL}s(实测 RTO,目标 ≤4h)===="
log "drill_last_unix=$NOW"
echo "DRILL_PASS 整库恢复用时=${T_RESTORE}s 总计=${TOTAL}s"
