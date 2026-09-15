#!/bin/sh
# DP-02 ② 反向验证(修正版):启动自检必须拒绝坏环境并**一次列全**问题。
#
# 上一版有两个**验证器自身**的毛病(证据其实是对的,是判据写错了):
#   ① 数问题行用了 '^ - ',而实际行首是两个空格 → 数出 0 条,误判成 FAIL;
#   ② "还原后自检通过"是在**服务还在跑**的时候跑的 —— 它自己占着 127.0.0.1:8080,
#      于是自检如实报"监听地址不可用"。那不是配置坏了,是端口被自己占着。
#      这恰好说明自检**在启动时会正确拦住重复实例**。
set -u
PREFIX=/opt/netdisk
CFG=/etc/netdisk/config.yaml
set -a; . /etc/netdisk/secrets.env; set +a
count_problems() { grep -cE '^[[:space:]]+- ' ; }

echo "=========== 场景 A:依赖不可用(PG 停 + 本服务停,腾出端口)==========="
systemctl stop netdisk
systemctl stop postgresql
sleep 2
OUT=$("$PREFIX/netdisk" -check -config "$CFG" 2>&1); RC=$?
echo "$OUT" | head -8
echo "问题数=$(echo "$OUT" | count_problems);退出码=$RC"
if [ "$RC" -ne 0 ]; then echo "PASS: 依赖不可用时拒绝启动"; else echo "FAIL: 竟然通过了"; fi
systemctl start postgresql
sleep 3

echo ""
echo "=========== 场景 B:坏配置(6 个接口族限速全 0)+ 只读存储根 ==========="
cp "$CFG" /tmp/nd-cfg-backup.yaml
cp /etc/netdisk/secrets.env /tmp/nd-secrets-backup.env
awk '/^rate_limits:/{f=1} f&&/per_second:/{sub(/[0-9]+/,"0")} f&&/per_minute:/{sub(/[0-9]+/,"0")} {print}' "$CFG" > /tmp/nd-cfg-bad.yaml
sed -i 's|^NETDISK_STORAGE_ROOT=.*|NETDISK_STORAGE_ROOT=/proc/readonly|' /etc/netdisk/secrets.env
set -a; . /etc/netdisk/secrets.env; set +a
OUT=$("$PREFIX/netdisk" -check -config /tmp/nd-cfg-bad.yaml 2>&1); RC=$?
echo "$OUT" | head -12
N=$(echo "$OUT" | count_problems)
echo "问题数=$N;退出码=$RC"
if [ "$RC" -ne 0 ] && [ "$N" -ge 2 ]; then
  echo "PASS: 坏配置被拒,且一次列出 $N 条问题(不是报一个改一个)"
else
  echo "FAIL: 期望非 0 且 >=2 条问题,实际 N=$N RC=$RC"
fi

echo ""
echo "=========== 场景 C:重复实例(服务在跑时再自检,应拒绝绑端口)==========="
cp /tmp/nd-cfg-backup.yaml "$CFG"
cp /tmp/nd-secrets-backup.env /etc/netdisk/secrets.env
set -a; . /etc/netdisk/secrets.env; set +a
systemctl start netdisk; sleep 3
OUT=$("$PREFIX/netdisk" -check -config "$CFG" 2>&1); RC=$?
echo "$OUT" | head -3
if [ "$RC" -ne 0 ] && echo "$OUT" | grep -q '监听地址不可用'; then
  echo "PASS: 重复实例被自检拦住(端口占用)"
else
  echo "FAIL: 重复实例未被拦住"
fi

echo ""
echo "=========== 还原后(停服务再自检)应当通过 ==========="
systemctl stop netdisk; sleep 2
"$PREFIX/netdisk" -check -config "$CFG" 2>&1 | tail -2
systemctl start netdisk; sleep 3
echo "服务状态=$(systemctl is-active netdisk)"
rm -f /tmp/nd-cfg-bad.yaml /tmp/nd-cfg-backup.yaml /tmp/nd-secrets-backup.env
echo "SELFCHECK_NEGATIVE_DONE"
