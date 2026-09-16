#!/bin/sh
# 网盘部署 2/3:PostgreSQL 17(小内存适配 + WAL 归档 + 角色/库)
# 以 root 运行,可重复执行。
set -eu
. /etc/netdisk/secrets.env

PGVER=17
PGDATA=/var/lib/postgresql/$PGVER/main
CONF=/etc/postgresql/$PGVER/main
ARCHIVE_DIR=/var/lib/netdisk/pg_wal_archive

echo "== 1. 小内存适配(本机 742MB RAM;PG 默认值是按大内存机给的)=="
cat > "$CONF/conf.d/10-netdisk.conf" <<'EOF'
# 由网盘部署脚本写入(DP-01/DP-04 相关)
# 本机内存只有 742MB:shared_buffers 128MB + 32 个应用连接是能稳住的量级;
# 用默认 128MB 但把 work_mem 收小,避免"一个大排序就换页"。
listen_addresses = 'localhost'
port = 5432
max_connections = 100
shared_buffers = 128MB
work_mem = 4MB
maintenance_work_mem = 32MB
effective_cache_size = 256MB
wal_keep_size = 64MB
log_min_duration_statement = 1000
log_line_prefix = '%m [%p] %q%u@%d '
EOF

echo "== 2. WAL 归档(DP-04:PG WAL 归档 + 每日 full 的基础)=="
cat > "$CONF/conf.d/20-netdisk-archive.conf" <<EOF
# 归档模式是 PITR 的前提:没有它,"恢复到某个时刻"只能恢复到最近一次 full。
archive_mode = on
archive_command = 'test ! -f $ARCHIVE_DIR/%f && cp %p $ARCHIVE_DIR/%f'
archive_timeout = 300
EOF

echo "== 3. pg_hba:只放行回环(与 E-02 同一纪律)=="
if ! grep -q 'E-02: 只放行回环' "$CONF/pg_hba.conf"; then
  cp "$CONF/pg_hba.conf" "$CONF/pg_hba.conf.bak"
  cat > "$CONF/pg_hba.conf" <<'EOF'
# E-02: 只放行回环(局域网一律拒绝)。改这里之前先想清楚:PG 端口不该出现在网卡上。
local   all             postgres                                peer
local   all             all                                     peer
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
local   replication     all                                     peer
host    replication     all             127.0.0.1/32            scram-sha-256
host    replication     all             ::1/128                 scram-sha-256
EOF
fi

echo "== 4. 重启 PG 使配置生效 =="
systemctl restart postgresql
sleep 2
systemctl is-active postgresql

echo "== 5. 角色与库 =="
su - postgres -c "psql -v ON_ERROR_STOP=1 -q -c \"DO \\\$\\\$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'netdisk') THEN
    CREATE ROLE netdisk LOGIN PASSWORD '$NETDISK_DB_PASSWORD';
  ELSE
    ALTER ROLE netdisk LOGIN PASSWORD '$NETDISK_DB_PASSWORD';
  END IF;
END \\\$\\\$;\""

if ! su - postgres -c "psql -tAc \"SELECT 1 FROM pg_database WHERE datname='netdisk'\"" | grep -q 1; then
  su - postgres -c "createdb -O netdisk netdisk"
  echo "已建库 netdisk"
else
  echo "库 netdisk 已存在"
fi

echo "== 6. 扩展(pg_trgm:00001_init.sql 需要;先由超级用户建好,迁移里就只剩 IF NOT EXISTS 空转)=="
su - postgres -c "psql -v ON_ERROR_STOP=1 -q -d netdisk -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'"

echo "== 7. 生效值复核 =="
su - postgres -c "psql -tAc \"SHOW listen_addresses; SHOW max_connections; SHOW archive_mode; SHOW shared_buffers;\""
su - postgres -c "psql -tAc \"SELECT rolname FROM pg_roles WHERE rolname='netdisk'\""
su - postgres -c "psql -tAc \"SELECT extname FROM pg_extension ORDER BY 1\" -d netdisk"
echo "PROVISION_PG_DONE"
