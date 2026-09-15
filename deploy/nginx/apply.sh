#!/bin/sh
# 安装/更新 netdisk 的 Nginx 配置(DP-01)。以 root 运行,可重复执行。
#
# 做四件事:
#   1. 站点配置 → /etc/nginx/sites-available/netdisk + sites-enabled 软链(移除 default)
#   2. 公共代理头 → /etc/nginx/snippets/netdisk-proxy.conf
#   3. http 级调优 → /etc/nginx/conf.d/netdisk-tuning.conf
#   4. 主配置 worker_connections 8192(DP-01 ③ 硬要求,events 块 conf.d 覆盖不了)
#      —— 幂等:已经是 8192 就跳过;改前备份
# 最后 nginx -t 校验并 reload(失败则退出非 0,不会把坏配置推上线)
set -eu
SRC_DIR="${1:-/home/jonsk/.nd/nginx}"

echo "== 1. 站点 =="
install -m 0644 "$SRC_DIR/netdisk.conf" /etc/nginx/sites-available/netdisk
ln -sf /etc/nginx/sites-available/netdisk /etc/nginx/sites-enabled/netdisk
if [ -e /etc/nginx/sites-enabled/default ]; then
  rm -f /etc/nginx/sites-enabled/default
  echo "已移除 sites-enabled/default(Apache 已停用,80 由本配置接管)"
fi

echo "== 2. 代理头 snippet =="
install -d -m 0755 /etc/nginx/snippets
install -m 0644 "$SRC_DIR/snippets/netdisk-proxy.conf" /etc/nginx/snippets/netdisk-proxy.conf

echo "== 3. http 级调优 =="
install -m 0644 "$SRC_DIR/conf.d/netdisk-tuning.conf" /etc/nginx/conf.d/netdisk-tuning.conf

echo "== 4. worker_connections 8192(DP-01 ③)=="
if grep -qE '^[[:space:]]*worker_connections[[:space:]]+8192;' /etc/nginx/nginx.conf; then
  echo "已是 8192"
else
  cp -n /etc/nginx/nginx.conf /etc/nginx/nginx.conf.bak-$(date +%Y%m%d%H%M%S)
  sed -i -E 's|^([[:space:]]*)worker_connections[[:space:]]+[0-9]+;|\1worker_connections 8192;|' /etc/nginx/nginx.conf
  echo "已改为 8192"
fi
grep -E 'worker_connections|worker_processes' /etc/nginx/nginx.conf

echo "== 4b. server_tokens off(幂等;Debian 13 stock 已有,别的发行版可能没有)=="
# 为什么不写在 conf.d 的 tuning 文件里:nginx 对 http 块的**重复指令是硬错误**,
# 而 stock nginx.conf 已经有一行,重复会直接令 nginx -t 失败(实测踩到)。
if grep -qE '^[[:space:]]*server_tokens[[:space:]]+off;' /etc/nginx/nginx.conf; then
  echo "已是 off"
else
  sed -i -E '/^[[:space:]]*include[[:space:]]+\/etc\/nginx\/mime.types;/a\	server_tokens off; # netdisk: 不暴露版本号' /etc/nginx/nginx.conf
  echo "已补上 server_tokens off"
fi

echo "== 5. 自签证书(内网用;生产换正式证书)=="
install -d -m 0750 /etc/nginx/ssl
if [ ! -f /etc/nginx/ssl/netdisk.crt ]; then
  openssl req -x509 -nodes -newkey rsa:2048 -days 825 \
    -keyout /etc/nginx/ssl/netdisk.key -out /etc/nginx/ssl/netdisk.crt \
    -subj "/C=CN/O=NetDisk/CN=10.14.37.187" \
    -addext "subjectAltName=IP:10.14.37.187,DNS:localhost" >/dev/null 2>&1
  chmod 0600 /etc/nginx/ssl/netdisk.key
  echo "已生成自签证书(CN=10.14.37.187)"
else
  echo "证书已存在,保留"
fi

echo "== 6. 校验并生效 =="
nginx -t
systemctl enable nginx >/dev/null 2>&1 || true
systemctl reload nginx || systemctl restart nginx
sleep 1
systemctl is-active nginx
echo "NGINX_APPLY_DONE"
