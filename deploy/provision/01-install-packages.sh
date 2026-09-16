#!/bin/sh
# 网盘部署:安装依赖软件(PG 17 / Redis / Nginx / 备份工具)。
# 以 root 运行;**后台执行并写日志**(apt 装耗时远超 su 驱动器的 120s 上限)。
set -eu
export DEBIAN_FRONTEND=noninteractive

echo "[1/5] 基础工具"
apt-get install -y --no-install-recommends curl ca-certificates gnupg rsync borgbackup

echo "[2/5] 安装 PostgreSQL 17 / Redis / Nginx (ADR-1 基线是 PG 17)"
# 本项目最低支持 PG 17。Debian 13 系统仓库自带 PostgreSQL 17,直接安装即可,
# 无需 PGDG 源;若现场系统仓库版本低于 17(如 Debian 12 自带 15),再按文档加 PGDG 源装 17。
apt-get install -y --no-install-recommends postgresql-17 redis-server nginx

echo "[3/5] 让出 80/443:停用 Apache(Apache 的 /var/www/html 内容保持原样,不删)"
systemctl disable --now apache2 || true

echo "[4/5] 版本确认"
/usr/lib/postgresql/17/bin/postgres --version
redis-server --version | head -1
nginx -v 2>&1
borg --version

echo "[5/5] 服务状态"
systemctl is-active postgresql redis-server nginx || true
echo "INSTALL_DONE"
