#!/bin/sh
# 网盘部署:安装依赖软件(PG 18 / Redis / Nginx / 备份工具)。
# 以 root 运行;**后台执行并写日志**(apt 装 PG18 远超 su 驱动器的 120s 上限)。
set -eu
export DEBIAN_FRONTEND=noninteractive

echo "[1/6] 基础工具"
apt-get install -y --no-install-recommends curl ca-certificates gnupg rsync borgbackup

echo "[2/6] 加 PGDG 源(ADR-1 基线是 PG 18;Debian 13 自带 17)"
install -d /usr/share/postgresql-common/pgdg
curl -fsSL -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc \
  https://www.postgresql.org/media/keys/ACCC4CF8.asc
echo 'deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] https://apt.postgresql.org/pub/repos/apt trixie-pgdg main' \
  > /etc/apt/sources.list.d/pgdg.list
apt-get update

echo "[3/6] 安装 PostgreSQL 18 / Redis / Nginx"
apt-get install -y --no-install-recommends postgresql-18 redis-server nginx

echo "[4/6] 让出 80/443:停用 Apache(Apache 的 /var/www/html 内容保持原样,不删)"
systemctl disable --now apache2 || true

echo "[5/6] 版本确认"
/usr/lib/postgresql/18/bin/postgres --version
redis-server --version | head -1
nginx -v 2>&1
borg --version

echo "[6/6] 服务状态"
systemctl is-active postgresql redis-server nginx || true
echo "INSTALL_DONE"
