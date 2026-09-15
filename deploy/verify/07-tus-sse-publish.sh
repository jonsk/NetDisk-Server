#!/bin/sh
# TUS 定稿是否推 SSE 事件(修复前实测 tus_sse_frames_in_4s=0;修复后应 >0)
# 做法:后台订阅 SSE → 走 TUS 传一个小文件 → 数 4 秒内收到的帧(排除 ready 帧)。
set -u
BASE=${BASE:-http://127.0.0.1:8080}
PASS=${PASS:?需要 PASS=<口令>}

TOK=$(curl -s -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"login\":\"admin\",\"password\":\"$PASS\",\"audience\":\"desktop\"}" \
  | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')
[ -n "$TOK" ] || { echo "FAIL: 登录失败"; exit 1; }

# 后台订阅(只收 5 秒),输出到临时文件
SSE=$(mktemp /tmp/sse-XXXXXX)
( timeout 5 curl -sN -H "Authorization: Bearer $TOK" "$BASE/api/v1/events" > "$SSE" ) &
SSEPID=$!
sleep 1   # 等 ready 帧

# TUS 上传 8KB
NAME="tus-sse-$(date +%s).bin"
head -c 8192 /dev/urandom > /tmp/tus-sse.bin
HDR=$(mktemp /tmp/hdr-XXXXXX)
CODE=$(curl -s -o /tmp/tus-create.json -D "$HDR" -w '%{http_code}' -X POST "$BASE/tus" \
  -H 'Tus-Resumable: 1.0.0' -H 'Upload-Length: 8192' \
  -H "Upload-Metadata: filename $(printf '%s' "$NAME" | base64 | tr -d '\n')" \
  -H "Authorization: Bearer $TOK")
UID_=$(sed -n 's/.*"upload_id":"\([^"]*\)".*/\1/p' /tmp/tus-create.json)
TICKET=$(tr -d '\r' < "$HDR" | sed -n 's/^[Xx]-[Uu]pload-[Tt]oken: *//p' | head -1)
if [ "$CODE" != "201" ] || [ -z "$UID_" ]; then echo "FAIL: 建任务 $CODE"; exit 1; fi

FIN=$(curl -s -o /dev/null -D /tmp/tus-fin.hdr -w '%{http_code}' -X PATCH "$BASE/tus/$UID_" \
  -H 'Tus-Resumable: 1.0.0' -H 'Upload-Offset: 0' -H "X-Upload-Token: $TICKET" \
  -H "Authorization: Bearer $TOK" -H 'Content-Type: application/offset+octet-stream' \
  --data-binary @/tmp/tus-sse.bin)
FID=$(tr -d '\r' < /tmp/tus-fin.hdr | sed -n 's/^[Xx]-[Ff]ile-[Ii]d: *//p' | head -1)
echo "TUS 定稿 HTTP=$FIN file=$FID"

wait $SSEPID 2>/dev/null || true
FRAMES=$(grep -c '^event:' "$SSE" 2>/dev/null || echo 0)
NONREADY=$(grep '^event:' "$SSE" 2>/dev/null | grep -vc '^event: ready' || echo 0)
echo "SSE 帧总数=$FRAMES(其中非 ready=$NONREADY)"
echo "---- SSE 内容 ----"; head -12 "$SSE"
echo "tus_sse_frames_in_4s=$NONREADY"
if [ "$NONREADY" -gt 0 ]; then
  echo "PASS: TUS 定稿推动了 SSE 事件(修复生效)"; exit 0
fi
echo "FAIL: TUS 定稿后 4 秒内没有收到任何事件(仍未推)"
exit 1
