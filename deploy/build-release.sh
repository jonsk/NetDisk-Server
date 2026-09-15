#!/usr/bin/env bash
# ============================================================================
# deploy/build-release.sh —— 产出"一步部署产物"(DP-02 ①)
#
# 顺序不可颠倒(ADR-4):
#   1) pnpm install --frozen-lockfile   依赖可复现
#   2) pnpm build                       前端产物
#   3) 拷 dist → server/internal/webui/dist/admin
#      —— go:embed 只在**编译期**读;先 go build 再拷 dist 会得到一个
#      "能编译、页面却是上一版"的二进制(这种错**不会**报错,只会让人怀疑缓存)
#   4) GOOS=linux go build              交叉编译单二进制(前端已 embed)
#
# 用法:deploy/build-release.sh [版本号] [输出目录]
#   版本号默认取 git describe;输出默认 deploy/dist/
# ============================================================================
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="${1:-$(cd "$ROOT" && git describe --tags --always --dirty 2>/dev/null || echo dev)}"
OUT_DIR="${2:-$ROOT/deploy/dist}"
TARGET_OS="${TARGET_OS:-linux}"
TARGET_ARCH="${TARGET_ARCH:-amd64}"

echo "[1/5] 版本 = $VERSION;目标 = $TARGET_OS/$TARGET_ARCH"

echo "[2/5] pnpm install + build ..."
(cd "$ROOT/web" && pnpm install --frozen-lockfile && pnpm build)

echo "[3/5] 拷贝前端产物到 embed 目录 ..."
mkdir -p "$ROOT/server/internal/webui/dist/admin"
if command -v rsync >/dev/null 2>&1; then
  # --delete:残留的旧哈希资源会让"清理过的部署"仍能访问上一版文件
  rsync -a --delete "$ROOT/web/apps/admin/dist/" "$ROOT/server/internal/webui/dist/admin/"
else
  rm -rf "$ROOT/server/internal/webui/dist/admin"
  mkdir -p "$ROOT/server/internal/webui/dist/admin"
  cp -R "$ROOT/web/apps/admin/dist/." "$ROOT/server/internal/webui/dist/admin/"
fi

echo "[4/5] 交叉编译(GOOS=$TARGET_OS GOARCH=$TARGET_ARCH CGO_ENABLED=0)…"
mkdir -p "$OUT_DIR"
BIN="$OUT_DIR/netdisk-$VERSION-$TARGET_OS-$TARGET_ARCH"
# 注意 -X 的目标必须是**变量的完整导入路径**:BuildVersion 定义在 internal/api
# (不是 main),写成 `main.BuildVersion` 时 go 不报错、只是**静默不生效** ——
# 实测所有发布产物都对外报 version=dev,"部署的是哪一版"因此无法自证。
(cd "$ROOT/server" && CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH" \
  go build -trimpath \
  -ldflags "-s -w -X github.com/netdisk/netdisk/internal/api.BuildVersion=$VERSION" \
  -o "$BIN" ./cmd/netdisk)

echo "[4b/5] 校验版本号真的嵌进去了(防止上面的静默失效复发)"
# 不执行产物(它是 linux/amd64,构建机未必能跑;而且服务端也没有 -version 子命令),
# 直接从二进制里读 go 记录的 ldflags —— 这条路对交叉编译产物同样有效。
# 必须匹配**完整符号路径**:`go version -m` 只是把 -ldflags 原样回显,写错的
# `main.BuildVersion=1.0.3` 同样会出现在里面(却根本没生效),只 grep 名字会被骗。
SYMBOL="github.com/netdisk/netdisk/internal/api.BuildVersion"
if ! go version -m "$BIN" | grep -q "$SYMBOL=$VERSION"; then
  echo "版本自证失败:二进制里没有 $SYMBOL=$VERSION"
  go version -m "$BIN" | sed -n '1,12p'
  exit 1
fi
echo "版本自证通过: $SYMBOL=$VERSION"

echo "[5/5] 产物信息"
ls -l "$BIN"
if command -v file >/dev/null 2>&1; then file "$BIN"; fi
echo "SHA256: $(sha256sum "$BIN" | awk '{print $1}')"
cat <<EOF

完成后部署:
  scp $BIN <host>:/tmp/ && ssh <host> 'install -m 0755 /tmp/$(basename "$BIN") /opt/netdisk/netdisk && systemctl restart netdisk'
产物的**自检**(不启动服务也能验证配置与依赖):
  /opt/netdisk/netdisk -check -config /etc/netdisk/config.yaml
EOF
