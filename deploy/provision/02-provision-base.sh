#!/bin/sh
# 网盘部署 1/3:系统用户、目录、secrets(以 root 运行,可重复执行)
set -eu

echo "== 系统用户 netdisk =="
if id netdisk >/dev/null 2>&1; then
  echo "已存在"
else
  # 无登录 shell 的服务账号:这个账号只需要读二进制、写数据目录
  useradd --system --home-dir /opt/netdisk --shell /usr/sbin/nologin --comment "netdisk service" netdisk
  echo "已创建"
fi

echo "== 目录 =="
# 与 deploy/systemd/netdisk.service 的 ReadWritePaths 保持一致
install -d -o netdisk -g netdisk -m 0750 /opt/netdisk
install -d -o netdisk -g netdisk -m 0750 /opt/netdisk/data        # 对象与暂存根(6.5)
install -d -o netdisk -g netdisk -m 0750 /var/log/netdisk
install -d -o root    -g netdisk -m 0750 /etc/netdisk             # 配置 + secrets
install -d -o netdisk -g netdisk -m 0750 /var/lib/netdisk         # 备份/演练状态 JSON
install -d -o postgres -g postgres -m 0700 /var/lib/netdisk/pg_wal_archive   # DP-04 WAL 归档

echo "== secrets(/etc/netdisk/secrets.env,0640 root:netdisk)=="
SECRETS=/etc/netdisk/secrets.env
if [ -f "$SECRETS" ]; then
  echo "已存在,保留现值(轮换请手工改这一个文件后 systemctl restart netdisk)"
else
  DB_PW=$(openssl rand -base64 24 | tr -d '\n')
  JWT=$(openssl rand -base64 48 | tr -d '\n')
  REDIS_PW=$(openssl rand -base64 32 | tr -d '\n')
  umask 077
  cat > "$SECRETS" <<EOF
# 网盘服务 secrets(6.8 纪律 1:secret 只走 env,永不进 yaml,也永不进版本库)
# 本文件由部署脚本生成,值是随机口令;轮换 = 改这里 + systemctl restart netdisk
NETDISK_DB_PASSWORD=$DB_PW
JWT_SECRET=$JWT
NETDISK_REDIS_PASSWORD=$REDIS_PW
NETDISK_STORAGE_ROOT=/opt/netdisk/data
EOF
  chown root:netdisk "$SECRETS"
  chmod 0640 "$SECRETS"
  echo "已生成(口令随机,不落任何日志)"
fi

# 供后续脚本读取的口令(只在本机 shell 内传递,不打印)
set -a
. "$SECRETS"
set +a
printf '%s' "$NETDISK_DB_PASSWORD" > /root/.nd_db_pw
printf '%s' "$NETDISK_REDIS_PASSWORD" > /root/.nd_redis_pw
chmod 600 /root/.nd_db_pw /root/.nd_redis_pw

echo "== 目录清单 =="
ls -ld /opt/netdisk /opt/netdisk/data /etc/netdisk /var/log/netdisk /var/lib/netdisk /var/lib/netdisk/pg_wal_archive
echo "PROVISION_BASE_DONE"
