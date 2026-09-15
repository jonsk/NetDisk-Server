#!/bin/sh
# ============================================================================
# deploy/backup/netdisk-pitr-drill.sh —— PITR 实机演练(时间旅行)
#
# 与 netdisk-restore-drill.sh 的区别(两者都要做,验证的是不同能力):
#   restore-drill:daily dump → 恢复到**最近一次备份的状态**(能不能读出来)
#   pitr-drill   :base backup + 重放 WAL → 恢复到**指定的过去某一时刻**(能不能回到过去)
#
# 为什么"备份成功"和"dump 能恢复"都不足以证明 PITR 可用:
#   PITR 依赖的是**归档的 WAL 连续可用**。WAL 归档悄悄坏掉(实测踩过:33 次连续失败)
#   时,dump 恢复完全正常 —— 只有真做一次"回到过去"才会暴露。
#
# 演练怎么证明它真的"回到了过去"(不是只证明"能起库"):
#   ① 在库中插入标记 'before'(T0)
#   ② 取基础备份
#   ③ 记录目标时刻 T(用 **PG 自己的时钟**,不能用 shell 时钟)
#   ④ 插入标记 'after'(T1 > T)
#   ⑤ 从基础备份恢复到 T,并强制 promote
#   ⑥ 断言:恢复出的库里 **有 'before'、没有 'after'** —— 这两条同时成立才算真 PITR
#
# 用独立目录 + 独立端口(5433)起恢复实例,**全程不碰生产库**;演练用的标记表
# 在生产库里创建、演练结束后删除(它只有两行,不影响业务)。
# ============================================================================
set -eu

PG_BIN=/usr/lib/postgresql/18/bin
PGVER=18
BASE_ROOT=/var/backups/netdisk/base
RESTORE_DIR=/var/lib/postgresql/$PGVER/pitr-restore
LOG=/var/backups/netdisk/logs/pitr-$(date +%F).log
PORT=5433
TAG=beforedrill

mkdir -p "$BASE_ROOT" "$(dirname "$LOG")"
# 与 dump 目录同一条教训:pg_basebackup 以 **postgres** 身份跑,
# 而 mkdir -p 建出来的是 root:root 0755 → 它连子目录都建不了
# ("could not create directory ... Permission denied")。属主必须给 postgres。
chown postgres:postgres "$BASE_ROOT"
chmod 0750 "$BASE_ROOT"

log() { echo "[$(date '+%F %T')] $*"; }
psql_prod() { su - postgres -c "$PG_BIN/psql -Atc \"$1\" netdisk"; }
psql_rest() { su - postgres -c "$PG_BIN/psql -p $PORT -Atc \"$1\" netdisk"; }

cleanup_restore() {
  if [ -d "$RESTORE_DIR" ]; then
    su - postgres -c "$PG_BIN/pg_ctl -D $RESTORE_DIR -m immediate stop" >/dev/null 2>&1 || true
    rm -rf "$RESTORE_DIR"
  fi
}

START=$(date +%s)

exec >>"$LOG" 2>&1
log "==== PITR 演练开始(恢复实例端口 $PORT)===="

# ---- 0. 前置:归档必须在工作(否则 PITR 无从谈起)----
ARCH=$(psql_prod "SELECT archived_count || ' ' || failed_count FROM pg_stat_archiver")
ARCH_OK=$(echo "$ARCH" | awk '{print $1}')
log "WAL 归档计数: archived=$ARCH"
[ "${ARCH_OK:-0}" -gt 0 ] || { log "FAILED: 还没有任何 WAL 被归档,先修归档"; exit 1; }

# ---- 1. 标记 'before' + 基础备份 ----
psql_prod "CREATE TABLE IF NOT EXISTS pitr_marker(tag text, at timestamptz)" >/dev/null
psql_prod "DELETE FROM pitr_marker" >/dev/null
psql_prod "INSERT INTO pitr_marker VALUES ('before', now())" >/dev/null
log "已插入标记 'before'"

# 用 **plain 格式(-Fp)** 而不是 tar(-Ft):plain 直接落成一个可用数据目录,
# 省掉解包这一步;实测里 `-Ft -z` 产出的 WAL 归档文件名与预期不一致
# (只有 base.tar.gz,没有 pg_wal.tar.gz),而 -Fp -X fetch 会把 WAL 直接放进 pg_wal/。
BASE="$BASE_ROOT/base-$(date +%Y%m%d_%H%M%S)"
rm -rf "$BASE_ROOT"/base-* 2>/dev/null || true
log "取基础备份(pg_basebackup -Fp -X fetch)…"
su - postgres -c "$PG_BIN/pg_basebackup -D $BASE -Fp -X fetch -c fast" || { log "FAILED: pg_basebackup"; exit 1; }
du -sh "$BASE"; ls "$BASE" | head -5

# 目标时刻:必须用 **PG 的时钟**(与 WAL 里记录的时间同源)
TARGET=$(psql_prod "SELECT now()")
log "恢复目标时刻(来自 PG 时钟)= $TARGET"

# ---- 2. 目标时刻之后再写一条,作为"不该出现"的对照 ----
sleep 2
psql_prod "INSERT INTO pitr_marker VALUES ('after', now())" >/dev/null
log "已插入标记 'after'(应当**不在**恢复结果里)"
psql_prod "SELECT pg_switch_wal()" >/dev/null
sleep 3   # 等归档把这一段收走
log "已强制切换 WAL;归档目录文件数=$(ls /var/lib/postgresql/wal_archive | wc -l)"

# ---- 3. 还原到独立目录(在**副本**上演练)----
# ⚠ 必须用副本:promote 之后这个目录会被写入(不再是"那一刻的备份")。
#   基础备份是**制品**,演练只能用它的副本 —— 否则一次演练就把制品污染了,
#   而它正是下次真出事时唯一能用的东西。
cleanup_restore
install -d -o postgres -g postgres -m 0700 "$RESTORE_DIR"
su - postgres -c "cp -a $BASE/. $RESTORE_DIR/" || { log "FAILED: 复制基础备份失败"; exit 1; }

# ⚠ Debian 把 postgresql.conf 放在 **/etc/postgresql/<ver>/main/**(PGDATA 里只是个符号
#   链接),所以 -Fp 基础备份出来的数据目录**没有可用的 postgresql.conf**;
#   直接起会报 "could not access the server configuration file"。
#   这里给演练实例写一份自足的配置(它只需要能起来并把 WAL 重放完)。
rm -f "$RESTORE_DIR/postgresql.conf"
cat > "$RESTORE_DIR/postgresql.conf" <<EOF
# PITR 演练实例(由 netdisk-pitr-drill.sh 生成;只用于重放 WAL,不对外服务)
listen_addresses = '127.0.0.1'
port = $PORT
# 必须 **>= 主库的值**:PG 会拒绝在 max_connections 更小的实例上做恢复
# ("recovery aborted because of insufficient parameter settings"),因为恢复进程
# 需要与主库同等规模的锁表/连接槽。这条报错信息很直白,但只有真跑一次才会遇到。
max_connections = 100
shared_buffers = 64MB
fsync = off
EOF
# 同理:Debian 的 pg_hba.conf 也在 /etc 下,数据目录里没有 → 不写它会被
# "could not load .../pg_hba.conf" 直接 FATAL 掉。演练实例只需要让本机 postgres 连上。
cat > "$RESTORE_DIR/pg_hba.conf" <<EOF
local   all             postgres                                peer
local   all             all                                     peer
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
EOF
: > "$RESTORE_DIR/pg_ident.conf"
cat >> "$RESTORE_DIR/postgresql.auto.conf" <<EOF
# ---- PITR 演练(由 netdisk-pitr-drill.sh 生成)----
# auto.conf 在 postgresql.conf **之后**读,所以这里必须显式关掉生产带过来的归档,
# 否则演练实例会去抢同一个归档目录、往里面重复写。
archive_mode = off
restore_command = 'cp /var/lib/postgresql/wal_archive/%f %p'
recovery_target_time = '$TARGET'
recovery_target_action = 'promote'
EOF
su - postgres -c "touch $RESTORE_DIR/recovery.signal"
chown -R postgres:postgres "$RESTORE_DIR"

# ---- 4. 起恢复实例(独立端口;关掉它自己的归档,免得两边抢归档目录)----
log "启动恢复实例…"
su - postgres -c "$PG_BIN/pg_ctl -D $RESTORE_DIR -l /tmp/pitr-restore.log \
  -o '-p $PORT -c listen_addresses=127.0.0.1 -c archive_mode=off' start" || { log "FAILED: 恢复实例起不来"; tail -20 /tmp/pitr-restore.log; exit 1; }

# 等 promote(恢复完成)
RECOVERING=1
for i in $(seq 1 60); do
  sleep 1
  IN=$(su - postgres -c "$PG_BIN/psql -p $PORT -Atc 'SELECT pg_is_in_recovery()' netdisk" 2>/dev/null || echo t)
  if [ "$IN" = "f" ]; then RECOVERING=0; break; fi
done
if [ "$RECOVERING" -ne 0 ]; then
  log "FAILED: 60s 内没有完成恢复(pg_is_in_recovery 仍为 t)"
  tail -25 /tmp/pitr-restore.log
  cleanup_restore
  exit 1
fi
log "恢复完成并 promote(用时 ${i}s)"
grep -E 'recovery stopping|consistent recovery state|database system is ready' /tmp/pitr-restore.log | tail -3

# ---- 5. 断言:时间旅行确实发生了 ----
BEFORE=$(psql_rest "SELECT count(*) FROM pitr_marker WHERE tag='before'")
AFTER=$(psql_rest "SELECT count(*) FROM pitr_marker WHERE tag='after'")
FILES=$(psql_rest "SELECT count(*) FROM files")
OBJS=$(psql_rest "SELECT count(*) FROM file_objects")
PROD_FILES=$(psql_prod "SELECT count(*) FROM files")
log "恢复实例:before=$BEFORE after=$AFTER files=$FILES file_objects=$OBJS(生产 files=$PROD_FILES)"

FAILN=0
[ "$BEFORE" = "1" ] && log "  ✓ 目标时刻之前的数据在('before' 存在)" || { log "  ✗ 'before' 缺失,恢复没到目标时刻"; FAILN=$((FAILN+1)); }
[ "$AFTER" = "0" ]  && log "  ✓ 目标时刻之后的数据不在('after' 不存在 → 真的回到了过去)" || { log "  ✗ 'after' 也在,说明恢复的是最新状态而非目标时刻"; FAILN=$((FAILN+1)); }

# ---- 6. 收尾 ----
TOTAL=$(( $(date +%s) - START ))
cleanup_restore
psql_prod "DROP TABLE IF EXISTS pitr_marker" >/dev/null
log "已清理恢复实例与标记表"

if [ "$FAILN" -eq 0 ]; then
  log "==== PITR 演练成功:用时 ${TOTAL}s(实测 RTO,目标 ≤4h)===="
  echo "PITR_PASS 目标时刻=$TARGET 用时=${TOTAL}s before=$BEFORE after=$AFTER files=$FILES"
  exit 0
fi
log "==== PITR 演练失败:${FAILN} 条断言不满足 ===="
echo "PITR_FAIL"
exit 1
