#!/bin/sh
# ============================================================================
# deploy/verify/05-e2e-sizes.sh —— TS-03:E2E 上传→下载→校验(分档)
#
# 验收:1MB / 100MB / 5GB 三档(本地环境按磁盘能力取档);SHA256 与客户端一致;
#       Range 下载正确。
#
# 走 **TUS**(分片,与真实客户端同一条路径),不直传:大文件走 TUS 才能同时验证
# "分片偏移/断点语义"与"下载校验";用一次 PUT 传 100MB 只能证明"能把字节塞进去"。
#
# 档位可取:默认 1MB + 100MB;5GB 需显式 SIZES 指定(磁盘/时间受限时如实跳过并记录)。
# 用法:BASE=http://127.0.0.1:8080 USER=admin PASS=*** [SIZES="1048576 104857600"] \
#        sh deploy/verify/05-e2e-sizes.sh
# ============================================================================
set -eu

BASE="${BASE:-http://127.0.0.1:8080}"
USER="${USER:-admin}"
PASS="${PASS:?需要 PASS=<口令>}"
SIZES="${SIZES:-1048576 104857600}"
CHUNK="${CHUNK:-8388608}"          # 8MB 分片
WORK="$(mktemp -d /tmp/ts03-XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

fail=0
ok()  { echo "  ✓ $1"; }
bad() { echo "  ✗ $1"; fail=$((fail+1)); }
now() { date +%s%N; }
mbps() { awk -v b="$1" -v ns="$2" 'BEGIN{ if(ns>0) printf "%.1f MB/s", (b/1048576)/(ns/1000000000); else print "n/a" }'; }

echo "== 取令牌 =="
TOKEN=$(curl -s -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"login\":\"$USER\",\"password\":\"$PASS\",\"audience\":\"desktop\"}" \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
[ -n "$TOKEN" ] || { echo "FAIL: 登录失败(拿不到 access_token)"; exit 1; }
echo "  token 长度=${#TOKEN}"

for SIZE in $SIZES; do
  echo ""
  echo "==== 档位 $SIZE 字节($(( SIZE / 1048576 ))MB)===="
  SRC="$WORK/src-$SIZE.bin"
  head -c "$SIZE" /dev/urandom > "$SRC"
  WANT=$(sha256sum "$SRC" | awk '{print $1}')
  echo "  本地 sha256=$WANT"

  # ---- 建任务 ----
  # 名字必须**每次运行都不同**:ADR-5 规定"进行中的上传任务同样占用名字",
  # 所以上一轮跑失败留下的 reserved 任务会让同名建任务直接 409 name_conflict
  # (实测踩到 —— 这本身是 ADR-5 生效的证据,但会让脚本不可重跑)。
  NAME="ts03-$SIZE-$(date +%s)-$$.bin"
  META="filename $(printf '%s' "$NAME" | base64 | tr -d '\n')"
  HDR="$WORK/hdr"; BODY="$WORK/create"
  CODE=$(curl -s -o "$BODY" -D "$HDR" -w '%{http_code}' -X POST "$BASE/tus" \
    -H 'Tus-Resumable: 1.0.0' -H "Upload-Length: $SIZE" -H "Upload-Metadata: $META" \
    -H "Authorization: Bearer $TOKEN")
  [ "$CODE" = "201" ] || { bad "建任务应 201,实际 $CODE"; cat "$BODY"; continue; }
  UID_=$(sed -n 's/.*"upload_id":"\([^"]*\)".*/\1/p' "$BODY")
  TICKET=$(tr -d '\r' < "$HDR" | sed -n 's/^[Xx]-[Uu]pload-[Tt]oken: *//p' | head -1)
  [ -n "$UID_" ] && [ -n "$TICKET" ] || { bad "建任务未返回 upload_id/X-Upload-Token"; continue; }
  # 任何后续失败都要**释放名字**,否则 reserved 任务会一直占着这个名字直到回收班车
  release() { curl -s -o /dev/null -X DELETE "$BASE/tus/$UID_" -H "Authorization: Bearer $TOKEN" -H "X-Upload-Token: $TICKET" || true; }
  ok "建任务 201 upload=$UID_(ticket ${#TICKET} 字符)"

  # ---- 分片上传(每片一次 PATCH;最后一片由服务端定稿)----
  OFF=0; LAST_CODE=""; T0=$(now); NCHUNK=0
  while [ "$OFF" -lt "$SIZE" ]; do
    N=$(( SIZE - OFF )); [ "$N" -gt "$CHUNK" ] && N="$CHUNK"
    tail -c "+$(( OFF + 1 ))" "$SRC" | head -c "$N" > "$WORK/chunk"
    LAST_CODE=$(curl -s -o "$WORK/last" -D "$WORK/lasthdr" -w '%{http_code}' -X PATCH "$BASE/tus/$UID_" \
      -H 'Tus-Resumable: 1.0.0' -H "Upload-Offset: $OFF" -H "X-Upload-Token: $TICKET" \
      -H "Authorization: Bearer $TOKEN" \
      -H 'Content-Type: application/offset+octet-stream' --data-binary @"$WORK/chunk")
    case "$LAST_CODE" in 204|200) ;; *) bad "分片 offset=$OFF 返回 $LAST_CODE"; break ;; esac
    OFF=$(( OFF + N )); NCHUNK=$(( NCHUNK + 1 ))
  done
  T1=$(now)
  UP_NS=$(( T1 - T0 ))
  [ "$OFF" = "$SIZE" ] && ok "$NCHUNK 片上传完成($OFF 字节,$(mbps "$SIZE" "$UP_NS"))" || bad "上传未走完($OFF/$SIZE)"
  if [ "$LAST_CODE" = "200" ]; then ok "最后一片即定稿(200)"; else bad "最后一片应 200 定稿,实际 $LAST_CODE"; release; fi

  # TUS 定稿的产物信息走**响应头**(不是 body):X-File-Id / X-File-Version / Upload-Complete。
  # 上一版只解析 body,于是"定稿成功但拿不到 id" —— 真相是我看错了地方。
  FID=$(tr -d '\r' < "$WORK/lasthdr" | sed -n 's/^[Xx]-[Ff]ile-[Ii]d: *//p' | head -1)
  FVER=$(tr -d '\r' < "$WORK/lasthdr" | sed -n 's/^[Xx]-[Ff]ile-[Vv]ersion: *//p' | head -1)
  UDONE=$(tr -d '\r' < "$WORK/lasthdr" | sed -n 's/^[Uu]pload-[Cc]omplete: *//p' | head -1)
  [ -n "$FID" ] || { bad "定稿未返回 X-File-Id(头:$(tr -d '\r' < "$WORK/lasthdr" | tr '\n' ' ' | head -c 200))"; continue; }
  [ "$UDONE" = "true" ] && ok "定稿 200 X-File-Id=$FID version=$FVER Upload-Complete=true" || bad "Upload-Complete 应为 true,实际 '$UDONE'"

  # 服务端**实测哈希**必须与客户端一致(比只比下载字节更强:它证明的是落库值)
  curl -s -o "$WORK/detail" "$BASE/api/v1/files/$FID" -H "Authorization: Bearer $TOKEN"
  SRV_HASH=$(sed -n 's/.*"hash_sha256":"\([^"]*\)".*/\1/p' "$WORK/detail" | head -1)
  SRV_SIZE=$(sed -n 's/.*"size":\([0-9]*\).*/\1/p' "$WORK/detail" | head -1)
  [ "$SRV_HASH" = "$WANT" ] && ok "服务端落库 hash_sha256 与客户端一致" || bad "落库哈希不一致:客户端=$WANT 服务端=$SRV_HASH"
  [ "$SRV_SIZE" = "$SIZE" ] && ok "服务端 size=$SRV_SIZE" || bad "size 不一致:期望 $SIZE 实际 $SRV_SIZE"

  # ---- 下载整文件并校验 sha256 ----
  T2=$(now)
  DCODE=$(curl -s -o "$WORK/dl" -w '%{http_code}' "$BASE/api/v1/files/$FID/content" -H "Authorization: Bearer $TOKEN")
  T3=$(now)
  DL_NS=$(( T3 - T2 ))
  GOT=$(sha256sum "$WORK/dl" | awk '{print $1}')
  [ "$DCODE" = "200" ] && ok "下载 200($(stat -c%s "$WORK/dl") 字节,$(mbps "$SIZE" "$DL_NS"))" || bad "下载应 200,实际 $DCODE"
  [ "$GOT" = "$WANT" ] && ok "SHA256 与客户端一致" || bad "SHA256 不一致:本地=$WANT 下载=$GOT"

  # ---- Range 下载(首段 + 中段)----
  for RANGE in "0-1023" "$(( SIZE / 2 ))-$(( SIZE / 2 + 1023 ))"; do
    START=${RANGE%-*}
    RCODE=$(curl -s -o "$WORK/rng" -D "$WORK/rnghdr" -w '%{http_code}' -r "$RANGE" \
      "$BASE/api/v1/files/$FID/content" -H "Authorization: Bearer $TOKEN")
    CR=$(tr -d '\r' < "$WORK/rnghdr" | sed -n 's/^[Cc]ontent-[Rr]ange: *//p' | head -1)
    EXPECT="bytes $START-$(( START + 1023 ))/$SIZE"
    SZ=$(stat -c%s "$WORK/rng" 2>/dev/null || echo 0)
    # 分片内容必须与源文件的同一区间逐字节一致
    tail -c "+$(( START + 1 ))" "$SRC" | head -c 1024 > "$WORK/expect"
    if [ "$RCODE" = "206" ] && [ "$CR" = "$EXPECT" ] && [ "$SZ" = "1024" ] && cmp -s "$WORK/rng" "$WORK/expect"; then
      ok "Range $RANGE → 206 $CR,内容逐字节一致"
    else
      bad "Range $RANGE 不符:code=$RCODE Content-Range='$CR'(期望 $EXPECT) 字节=$SZ"
    fi
  done

  # 清理:硬删,留下悬空对象会让后续巡检报警
  DCODE=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "$BASE/api/v1/files/$FID" -H "Authorization: Bearer $TOKEN")
  ok "清理探针文件($DCODE)"
done

echo ""
if [ "$fail" -eq 0 ]; then echo "TS-03 E2E SIZES: ALL PASS"; exit 0; fi
echo "TS-03 E2E SIZES: $fail 项失败"
exit 1
