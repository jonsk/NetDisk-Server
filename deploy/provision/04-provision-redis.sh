#!/bin/sh
# 网盘部署 3/3:Redis(requirepass + AOF + noeviction + 只绑回环)
# 以 root 运行,可重复执行。
#
# E-06 的三条硬要求(架构 8.2),缺一条都会出问题:
#   ① requirepass <口令>          —— 否则登录/限速全线失效
#   ② appendonly yes + everysec   —— token/refresh/限速窗口/游标键必须跨重启存活
#                                    (关掉 AOF 时"重启一次 = 全体用户被登出")
#   ③ maxmemory-policy noeviction —— token/游标键不得被淘汰(淘汰 = 随机把人踢下线)
set -eu
. /etc/netdisk/secrets.env

CONF=/etc/redis/redis.conf
echo "== 1. 先确保配置文件里的关键行处于我们要求的状态 =="
cp -n "$CONF" "$CONF.orig" 2>/dev/null || true

set_kv() {
  key="$1"; value="$2"
  if grep -qE "^[[:space:]]*#?[[:space:]]*$key[[:space:]]" "$CONF"; then
    sed -i -E "s|^[[:space:]]*#?[[:space:]]*$key[[:space:]].*|$key $value|" "$CONF"
  else
    echo "$key $value" >> "$CONF"
  fi
}

set_kv bind "127.0.0.1 -::1"
set_kv protected-mode "yes"
set_kv port "6379"
set_kv requirepass "$NETDISK_REDIS_PASSWORD"
set_kv appendonly "yes"
set_kv appendfsync "everysec"
# AOF 目录用**绝对路径**:相对路径随启动目录变,换个地方启动就会"另起一份数据"
set_kv dir "/var/lib/redis"
set_kv appenddirname "appendonlydir"
# 96MB:本机总内存 742MB,给 PG/应用留足;策略是 noeviction,所以到顶是**报错**
# 而不是静默丢键 —— 报错会暴露问题,淘汰会静默踢人下线
set_kv maxmemory "96mb"
set_kv maxmemory-policy "noeviction"
set_kv save "900 1"

echo "== 2. 重启并复核 =="
systemctl enable redis-server >/dev/null 2>&1 || true
systemctl restart redis-server
sleep 2
systemctl is-active redis-server

echo "== 3. 用口令实测(未认证必须被拒,认证后 PING 通)=="
set +e
NOAUTH=$(redis-cli PING 2>&1 | head -1)
AUTHED=$(redis-cli -a "$NETDISK_REDIS_PASSWORD" --no-auth-warning PING 2>&1 | head -1)
AOF=$(redis-cli -a "$NETDISK_REDIS_PASSWORD" --no-auth-warning CONFIG GET appendonly 2>/dev/null | tail -1)
POLICY=$(redis-cli -a "$NETDISK_REDIS_PASSWORD" --no-auth-warning CONFIG GET maxmemory-policy 2>/dev/null | tail -1)
BIND=$(ss -tlnp 2>/dev/null | grep 6379 || true)
set -e
echo "未认证 PING -> $NOAUTH"
echo "认证后 PING -> $AUTHED"
echo "appendonly   -> $AOF"
echo "maxmemory-policy -> $POLICY"
echo "监听         -> $BIND"
echo "PROVISION_REDIS_DONE"
