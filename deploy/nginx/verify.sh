#!/bin/sh
# 校验 Nginx 侧是否真的落地了 DP-01 的 7 项职责。
#
# 为什么用 `nginx -T`(打印**生效**配置)而不是读 sites-available 那份:
# 文件写得对不等于**生效**:可能被 include 顺序覆盖、可能 reload 没生效、
# 可能线上还是旧文件。这里断言的是运行时真正生效的配置。
#
# ⚠ 上一版验证器**自己被证明是坏的**:它用 awk 区间 + BRE 转义去抓
# `location ~ ^/api/v1/files/[^/]+/content$`,正则一次都没命中,于是把一份**正确**
# 的配置报成"② 下载路径未关 proxy_buffering"(而 nginx -T 里那行明明在)。
# 教训与订正⑨③、四审"假门禁"同源:**验证动作本身也要被验证**。
# 所以这一版:①一律用 `grep -F`(定长串,不玩转义);②末尾加一条"验证器自检" ——
# 故意问一个**不存在**的指令,必须得到"否",否则说明匹配逻辑恒真(那它就是在装饰)。
set -u
fail=0
ok()  { echo "  ✓ $1"; }
bad() { echo "  ✗ $1"; fail=$((fail+1)); }

T=$(nginx -T 2>/dev/null)

# 在某个 location 行之后的 N 行窗口里找期望指令(定长匹配,窗口覆盖整个 block)
loc_has() {
  echo "$T" | grep -A "$3" -F "$1" | grep -qF "$2"
}

echo "== DP-01 七项职责(基于 nginx -T 生效配置)=="

# ① /tus、/webdav 关 proxy_request_buffering
loc_has 'location /tus {'    'proxy_request_buffering off;' 6 \
  && ok "① /tus 关 proxy_request_buffering"    || bad "① /tus 未关 proxy_request_buffering"
loc_has 'location /webdav {' 'proxy_request_buffering off;' 6 \
  && ok "① /webdav 关 proxy_request_buffering" || bad "① /webdav 未关 proxy_request_buffering"

# ② 下载关 proxy_buffering
loc_has 'location ~ ^/api/v1/files/[^/]+/content$ {' 'proxy_buffering off;' 6 \
  && ok "② 下载路径关 proxy_buffering" || bad "② 下载路径未关 proxy_buffering"

# ③ SSE 关缓冲 + read_timeout 86400s + events worker_connections 8192
loc_has 'location = /api/v1/events {' 'proxy_buffering off;' 10 \
  && ok "③ SSE 关 proxy_buffering" || bad "③ SSE 未关 proxy_buffering"
loc_has 'location = /api/v1/events {' 'proxy_read_timeout 86400s;' 10 \
  && ok "③ SSE read_timeout 86400s" || bad "③ SSE read_timeout 不是 86400s"
echo "$T" | grep -qE '^[[:space:]]*worker_connections[[:space:]]+8192;' \
  && ok "③ worker_connections 8192" || bad "③ worker_connections 不是 8192"

# ④ 真实 IP 透传头齐备
for h in 'X-Real-IP' 'X-Forwarded-For' 'X-Forwarded-Proto'; do
  echo "$T" | grep -qF "proxy_set_header $h" && ok "④ 透传 $h" || bad "④ 未透传 $h"
done

# ⑤ /admin、/h5 走 proxy_pass(不再 alias)
if echo "$T" | grep -qE 'alias .*/(admin|h5)'; then
  bad "⑤ 仍存在 alias 到前端的写法(ADR-4 要求 embed + proxy_pass)"
else
  ok "⑤ 无 alias;前端由 Go embed 提供"
fi
echo "$T" | grep -qF 'proxy_pass http://netdisk_app;' \
  && ok "⑤ 默认 location 走 proxy_pass" || bad "⑤ 默认 location 未走 proxy_pass"

# 附加:安全头 / 粗限流 / 无 default 站点
echo "$T" | grep -qF 'X-Content-Type-Options' && ok "附加 安全头 nosniff" || bad "附加 缺安全头"
echo "$T" | grep -qF 'zone=netdisk_login' && ok "附加 登录粗限流 zone" || bad "附加 缺登录限流"
if ls /etc/nginx/sites-enabled/ | grep -q '^default$'; then
  bad "附加 default 站点仍启用(会抢 80)"
else
  ok "附加 default 站点已移除"
fi

# 验证器自检:问一个**必然不存在**的指令,必须得到"否"
if loc_has 'location /tus {' 'proxy_buffering on;' 6; then
  bad "附加 验证器自检失败:对不存在的指令也返回匹配(验证恒真 = 装饰)"
else
  ok "附加 验证器自检:能对不存在的指令判否"
fi

echo ""
if [ "$fail" -eq 0 ]; then
  echo "DP-01 NGINX VERIFY: ALL PASS"
  exit 0
fi
echo "DP-01 NGINX VERIFY: $fail 项失败"
exit 1
