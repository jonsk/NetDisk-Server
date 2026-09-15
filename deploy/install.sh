#!/bin/sh
# ============================================================================
# deploy/install.sh —— 在目标机部署 netdisk 单二进制(DP-02)
#
# 用法(以 root 在目标机执行):
#   deploy/install.sh <netdisk-linux-amd64 路径> [对外 URL] [配置示例路径]
# 例:
#   deploy/install.sh /tmp/netdisk-1.0.0-linux-amd64 http://10.14.37.187
#
# 幂等:可重复执行(升级 = 再跑一次并换二进制;配置与 secrets 不覆盖已有值)。
#
# 为什么"部署"必须是一个脚本而不是一堆手敲命令:
#   手册里的一句话与可执行的 40 行脚本之间,差的正是**可重复性** ——
#   下一次换机器/换人,手敲一定会漏掉某一步(最常见的是忘了 -migrate,
#   表现是服务起来了、第一个请求 500 "relation does not exist")。
# ============================================================================
set -eu

BIN_SRC="${1:?用法: install.sh <二进制路径> [对外URL] [配置示例路径]}"
PUBLIC_URL="${2:-http://$(hostname -I | awk '{print $1}')}"
CONFIG_EXAMPLE="${3:-/home/jonsk/.nd/config.example.yaml}"

PREFIX=/opt/netdisk
ETC=/etc/netdisk
SECRETS="$ETC/secrets.env"

echo "==== 1. 前置检查 ===="
[ "$(id -u)" = "0" ] || { echo "FAIL: 需要 root"; exit 1; }
[ -f "$BIN_SRC" ] || { echo "FAIL: 找不到二进制 $BIN_SRC"; exit 1; }
for s in postgresql redis-server; do
  systemctl is-active "$s" >/dev/null 2>&1 || { echo "FAIL: $s 未运行(先装并启动依赖)"; exit 1; }
done
echo "依赖服务在跑;二进制 = $BIN_SRC;PUBLIC_URL = $PUBLIC_URL"

echo "==== 2. 用户与目录 ===="
id netdisk >/dev/null 2>&1 || useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin --comment "netdisk service" netdisk
install -d -o netdisk -g netdisk -m 0750 "$PREFIX" "$PREFIX/data" /var/log/netdisk /var/lib/netdisk
install -d -o root -g netdisk -m 0750 "$ETC"
# 数据子目录必须属于 netdisk。⚠ 实测踩到过:在装服务之前手工用 root 跑过一次
# `netdisk -migrate`(它会把 objects/ 与 tus-tmp/ 建出来),于是这两个目录归 root,
# 服务以 netdisk 身份启动时自检报 "objects 不可写" 并拒绝启动。
# 所以这里**无条件**修正属主(幂等),而不是只依赖"目录已存在"。
install -d -o netdisk -g netdisk -m 0750 "$PREFIX/data/objects" "$PREFIX/data/tus-tmp"
chown -R netdisk:netdisk "$PREFIX/data"
chown netdisk:netdisk /var/log/netdisk /var/lib/netdisk

echo "==== 3. secrets(存在则保留;轮换请改这一个文件后 restart)===="
if [ ! -f "$SECRETS" ]; then
  umask 077
  cat > "$SECRETS" <<EOF
NETDISK_DB_PASSWORD=$(openssl rand -base64 24 | tr -d '\n')
JWT_SECRET=$(openssl rand -base64 48 | tr -d '\n')
NETDISK_REDIS_PASSWORD=$(openssl rand -base64 32 | tr -d '\n')
NETDISK_STORAGE_ROOT=$PREFIX/data
NETDISK_METRICS_BACKUP_STATUS_FILE=/var/lib/netdisk/backup-status.json
EOF
  chown root:netdisk "$SECRETS"; chmod 0640 "$SECRETS"
  echo "已生成新 secrets"
else
  echo "已有 secrets,保留"
fi

echo "==== 4. 装二进制 ===="
install -m 0755 -o root -g root "$BIN_SRC" "$PREFIX/netdisk"
"$PREFIX/netdisk" -h 2>&1 | head -1 || echo "(版本号需要服务启动日志;此处忽略)"

echo "==== 5. 配置 ===="
# 从示例生成:只改主机相关项,**不覆盖已有配置**(升级时保留现场调整)
if [ -f "$ETC/config.yaml" ]; then
  echo "已有 $ETC/config.yaml,保留(如需用示例覆盖:手工替换后 systemctl restart netdisk)"
else
  [ -f "$CONFIG_EXAMPLE" ] || { echo "FAIL: 找不到配置示例 $CONFIG_EXAMPLE"; exit 1; }
  sed -e "s|^  public_url: .*|  public_url: \"$PUBLIC_URL\"|" \
      -e "s|^  trusted_proxies: .*|  trusted_proxies: [\"127.0.0.1\", \"::1\"]|" \
      "$CONFIG_EXAMPLE" > "$ETC/config.yaml"
  chown root:netdisk "$ETC/config.yaml"; chmod 0640 "$ETC/config.yaml"
  echo "已生成 $ETC/config.yaml(public_url=$PUBLIC_URL)"
fi

echo "==== 6. systemd 单元 ===="
install -m 0644 /home/jonsk/.nd/netdisk.service /etc/systemd/system/netdisk.service
# 小内存机 override:仓库里的单元按 2G 生产基线写,本机只有 742MB。
# 用 drop-in 而不是改单元:单元是通用基线,机器差异属于**本机**事实。
install -d -m 0755 /etc/systemd/system/netdisk.service.d
install -m 0644 /home/jonsk/.nd/10-small-box.conf /etc/systemd/system/netdisk.service.d/10-small-box.conf
systemctl daemon-reload

echo "==== 7. 数据库迁移 ===="
# ⚠ 这里**不能**执行 `netdisk -migrate` 并等它返回:`-migrate` 的语义是
# "启动前先跑迁移,然后继续启动服务",它不会退出(会一直前台服务)。
# 实测等在它上面 = 永远挂住(迁移其实已成功,但脚本看起来卡死,随后的 -check
# 还会报"监听地址不可用"——因为正是它自己占着 8080)。
# 迁移写在 systemd 单元的 ExecStart 里(开机/重启即自动补齐版本),这里只做**检查**:
set -a; . "$SECRETS"; set +a
echo "(迁移由 systemd ExecStart 的 -migrate 执行;此处仅确认能连库)"
timeout 20 "$PREFIX/netdisk" -check -config "$ETC/config.yaml" >/dev/null 2>&1 && echo "依赖与配置自检通过(含数据库连通)"

echo "==== 8. 启动前自检(6.8 纪律 3;失败会打印全部问题并以非 0 退出)===="
# ⚠ 自检里包含"监听地址是否可用",而**升级**时旧进程正占着 8080 —— 于是这一步
# 必然失败,set -eu 直接退出,服务永远升不上去(实测:二进制已装成新版,进程却
# 还跑着旧版,而且脚本 exit 1 看起来像"部署失败")。
# 正确顺序是:先停旧进程 → 自检(此时端口空闲,检查才有意义) → 再启动。
systemctl stop netdisk >/dev/null 2>&1 || true
for i in $(seq 1 20); do
  ss -tln 2>/dev/null | grep -q '127.0.0.1:8080' || break
  sleep 1
done
"$PREFIX/netdisk" -check -config "$ETC/config.yaml"

echo "==== 9. 启动并等就绪 ===="
systemctl enable netdisk >/dev/null 2>&1 || true
systemctl restart netdisk
for i in $(seq 1 30); do
  if ss -tln | grep -q '127.0.0.1:8080'; then echo "已监听 127.0.0.1:8080(用时 ${i}s)"; break; fi
  sleep 1
done
systemctl is-active netdisk
echo "---- 启动日志(含生效的池上限)----"
journalctl -u netdisk -n 8 --no-pager

echo "==== 10. 版本自证(运行的到底是哪一版,必须能从服务自己嘴里读到)===="
# 起因(2026-09 实测):build-release.sh 曾写 `-X main.BuildVersion=$VERSION`,
# 而 BuildVersion 定义在 internal/api。go 对"符号不存在"的 -X **静默忽略**,
# 于是每个发布产物启动都报 version=dev —— 版本号形同虚设,除了翻文件名无法自证。
# 构建脚本已修正为完整导入路径;这里再从**运行中的服务启动日志**确认一次:
# 二进制内部搜字符串不可靠(值可能在依赖里偶然出现),服务自报才是硬证据。
# 产物名可能是 netdisk-1.0.4-linux-amd64,也可能是传上去时的短名 netdisk-1.0.4
EXPECT="${NETDISK_EXPECT_VERSION:-$(basename "$BIN_SRC" | sed -e 's/^netdisk-//' -e 's/-linux-amd64$//')}"
if [ -z "$EXPECT" ]; then
  echo "跳过版本自证:无法从文件名推断版本,可用 NETDISK_EXPECT_VERSION= 显式指定"
else
  LOGLINE=""
  for i in $(seq 1 15); do
    LOGLINE="$(journalctl -u netdisk --since '-3min' --no-pager 2>/dev/null | grep -m1 'starting netdisk' || true)"
    [ -n "$LOGLINE" ] && break
    sleep 1
  done
  case "$LOGLINE" in
    *version="$EXPECT"*) echo "版本自证通过: version=$EXPECT" ;;
    *version=dev*) echo "FAIL: 服务自报 version=dev(期望 $EXPECT)—— 产物是用错误符号路径编的,重新执行 deploy/build-release.sh"; exit 1 ;;
    *) echo "FAIL: 服务自报版本与期望不符(期望 $EXPECT):$LOGLINE"; exit 1 ;;
  esac
fi
echo "INSTALL_DONE"
