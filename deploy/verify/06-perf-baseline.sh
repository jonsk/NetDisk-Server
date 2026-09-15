#!/bin/sh
# ============================================================================
# deploy/verify/06-perf-baseline.sh —— TS-08 性能基线(自包含、可重复运行)
#
# 三个验收点,每一条只报**实测值**:
#   1) 目录列表(10 万行空间)P95 ≤ 500ms
#   2) 远端变更感知 P95 ≤ 3s(API 写 → /changes 可见;并单独测 SSE 帧到达)
#   3) 并发上传/下载吞吐(8 / 16 并发,TUS 上传 + 下载)
#
# 用法:
#   BASE=http://127.0.0.1:8080 USER=admin PASS='***' sh deploy/verify/06-perf-baseline.sh
# 可选环境变量:
#   ROWS       目录行数(默认 100000)      NREP   列表重复次数(默认 60)
#   NFEED      变更感知轮数(默认 30)       SAR    并发档(默认 "8 16")
#   UPFILE     上传探测文件字节(默认 8388608,即 8MB)
#   KEEP=1     保留探测数据(排障用;**会留下 10 万行**;默认清理)
#
# 末尾输出机器可读摘要(每行一个 key=value),含原始逐次样本与直方图。
#
# ---- 改脚本前必读(实测踩到的坑)--------------------------------------------
#   * 10 万行走**直接 INSERT files**(父 = 临时空间的根目录),不建 file_objects、
#     不动 used_bytes/quota:列表延迟只取决于 files 行数与索引,与物理对象无关。
#   * **绝不直接 INSERT sync_feed "制造变更"**:每空间 change_seq 计数器是
#     spaces.last_seq(由 next_change_seq() 自增)。手插一行会让"计数器 < 表内最大
#     seq",下一次真实写入即 primary key 冲突 → **全站写操作 500**。本脚本的服务端
#     时间基准改用 sync_feed.created_at(同机,无时钟漂移)。
#   * **TUS 定稿不推 SSE 事件**(handlers_tus.go 的 PATCH 成功分支没有 publish*,
#     multipart 直传 / WebDAV PUT / 改名 / 移动 / 删除 / 共享都有)。因此 SSE 帧到达
#     的样本走 multipart 直传;脚本另做一次 TUS→SSE 反证探针并如实记录。
#   * 列表 limit 上限实测为 **999**(=1000 会被当成越界回落 200);默认页是 200。
#   * 临时空间用 API 建、结束时用 SQL 删行删空间(10 万行走 API 硬删会产生 10 万次
#     审计写入,不是本次要测的东西);删除后脚本给出"行数/空间/列表 HTTP"三项证据。
# ============================================================================
set -u

BASE="${BASE:-http://127.0.0.1:8080}"
USER="${USER:-admin}"
PASS="${PASS:?需要 PASS=<口令>}"
ROWS="${ROWS:-100000}"
NREP="${NREP:-60}"
NFEED="${NFEED:-30}"
SAR="${SAR:-8 16}"
UPFILE="${UPFILE:-8388608}"
KEEP="${KEEP:-0}"
POLL_INTERVAL_MS="${POLL_INTERVAL_MS:-5}"
POLL_TIMEOUT_MS="${POLL_TIMEOUT_MS:-5000}"
PSQL="${PSQL:-sudo -u postgres psql -d netdisk -q -t -A -v ON_ERROR_STOP=1}"
RUNID="$$-$(date +%s)"
# 前缀必须是**所有文件名开头都带上的一整串**:
# 上传/下载探测文件的 STAMP 直接用 shell 的 RUNID(不要再用 python 自己的 pid-时间戳,
# 否则清理时按前缀匹配不到 —— 实测踩过)。
PREFIX="ts08-$RUNID"
export STAMP="$RUNID"
WORK="$(mktemp -d /tmp/ts08-perf-XXXXXX)"
SUMMARY="$WORK/summary.txt"
: > "$SUMMARY"
mkdir -p "$WORK"

say() { printf '%s\n' "$*"; }
sum() { printf '%s\n' "$*" | tee -a "$SUMMARY"; }
die() { say "FAIL: $*"; exit 1; }
now_ns() { date +%s%N; }
q1() { $PSQL -c "$1" 2>/dev/null; }

TOKEN=""; PERSONAL=""; BIGSPACE=""; BASE_ROWS=""

cleanup() {
  rc=$?
  kill "${SAMPLER_PID:-0}" 2>/dev/null || true
  if [ "$KEEP" = "1" ]; then
    say ""
    say "== KEEP=1:保留探测数据(WORK=$WORK 前缀=$PREFIX)=="
    return $rc
  fi
  say ""
  say "== 收尾清理 =="
  if [ -n "${PSPACE:-}" ] && [ -n "$TOKEN" ]; then
    PREFIXES="$PREFIX" BASE="$BASE" TOKEN="$TOKEN" SPACE="$PSPACE" \
      python3 "$WORK/cleanfiles.py" 2>&1 | grep -E '^(个人空间条目|已删除|清理后|cleanup_probe_files_remaining)' | sed 's/^/  /'
  fi
  if [ -n "${BIGSPACE:-}" ]; then
    $PSQL -c "DELETE FROM files WHERE space_id='$BIGSPACE' AND parent_id IS NOT NULL;" >/dev/null 2>&1
    $PSQL -c "DELETE FROM files WHERE space_id='$BIGSPACE';" >/dev/null 2>&1
    $PSQL -c "DELETE FROM spaces WHERE id='$BIGSPACE';" >/dev/null 2>&1
    left=$(q1 "SELECT count(*) FROM files WHERE space_id='$BIGSPACE';")
    spleft=$(q1 "SELECT count(*) FROM spaces WHERE id='$BIGSPACE';")
    http=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/v1/files?space=$BIGSPACE" -H "Authorization: Bearer $TOKEN")
    say "  10 万行空间:剩余行=${left:-?} 剩余空间行=${spleft:-?} 列表 HTTP=${http:-?}"
    printf '%s\n' "cleanup_big_space_rows_left=${left:-?}" \
                  "cleanup_big_space_left=${spleft:-?}" \
                  "cleanup_big_space_list_http=${http:-?}" >> "$SUMMARY"
  fi
  if [ -n "${PERSONAL:-}" ]; then
    now=$(q1 "SELECT count(*) FROM files WHERE space_id='$PERSONAL';")
    say "  个人空间行数:基线=${BASE_ROWS:-?} 清理后=${now:-?}"
    printf '%s\n' "cleanup_personal_rows_before=${BASE_ROWS:-?}" \
                  "cleanup_personal_rows_after=${now:-?}" >> "$SUMMARY"
  fi
  say "  (原始样本保留在 $WORK,可复核)"
  return $rc
}
trap cleanup EXIT

# ---------------------------------------------------------------- 登录
say "== TS-08 性能基线 $(date -Is) =="
say "base=$BASE user=$USER rows=$ROWS list_reps=$NREP feed_rounds=$NFEED sar=[$SAR] upfile=$UPFILE"
LOGIN=$(curl -s -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -d "{\"login\":\"$USER\",\"password\":\"$PASS\",\"audience\":\"desktop\"}")
TOKEN=$(printf '%s' "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("token",{}).get("access_token",""))' 2>/dev/null)
[ -n "$TOKEN" ] || die "登录失败:$(printf '%s' "$LOGIN" | head -c 200)"
USER_ID=$(printf '%s' "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["user"]["id"])')
PERSONAL=$(printf '%s' "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["personal_space"]["id"])')
PSPACE="$PERSONAL"
say "  登录成功 user=$USER_ID personal_space=$PERSONAL token_len=${#TOKEN}"

# ---------------------------------------------------------------- 机器/基线
say ""
say "== 机器与服务 =="
NCPU=$(nproc 2>/dev/null || echo '?')
MEMTOTAL=$(awk '/MemTotal/{printf "%.0f", $2/1024}' /proc/meminfo)
MEMAVAIL=$(awk '/MemAvailable/{printf "%.0f", $2/1024}' /proc/meminfo)
LOAD=$(cut -d' ' -f1-3 /proc/loadavg)
KERNEL=$(uname -sr)
DISK=$(df -h / | awk 'NR==2{print $1" "$2" used="$5}')
say "  kernel=$KERNEL cpu=${NCPU}C mem=${MEMTOTAL}MB(avail ${MEMAVAIL}MB) load=$LOAD disk=$DISK"
BASE_ROWS=$(q1 "SELECT count(*) FROM files;")
BASE_OBJS=$(q1 "SELECT count(*) FROM file_objects;")
BASE_FEED=$(q1 "SELECT count(*) FROM sync_feed;")
say "  基线: files=$BASE_ROWS file_objects=$BASE_OBJS sync_feed=$BASE_FEED"
{
  echo "machine_cpu=$NCPU"
  echo "machine_mem_total_mb=$MEMTOTAL"
  echo "machine_mem_avail_mb=$MEMAVAIL"
  echo "machine_kernel=$KERNEL"
  echo "machine_disk_root=$DISK"
  echo "baseline_files_rows=$BASE_ROWS"
  echo "baseline_file_objects=$BASE_OBJS"
  echo "baseline_sync_feed_rows=$BASE_FEED"
  echo "test_rows=$ROWS"
  echo "list_reps=$NREP"
  echo "feed_rounds=$NFEED"
  echo "sar=$SAR"
  echo "upfile_bytes=$UPFILE"
} >> "$SUMMARY"

# ---------------------------------------------------------------- 资源采样
SAMPLER="$WORK/sampler.txt"; : > "$SAMPLER"
(
  while : ; do
    C=$(awk '/^cpu /{printf "%d %d", $2+$4, $2+$3+$4+$5+$6+$7+$8}' /proc/stat)
    M=$(awk '/MemAvailable/{printf "%d", $2/1024}' /proc/meminfo)
    L=$(cut -d' ' -f1 /proc/loadavg)
    printf '%s %s %s %s\n' "$(date +%s)" "$C" "$M" "$L" >> "$SAMPLER"
    sleep 2
  done
) > /dev/null 2>&1 &
SAMPLER_PID=$!

# ============================================================================
# 一、10 万行目录列表
# ============================================================================
say ""
say "== 一、目录列表($ROWS 行)=="
BODY=$(curl -s -X POST "$BASE/api/v1/spaces" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $TOKEN" -d "{\"name\":\"TS08-Perf-$RUNID\"}")
BIGSPACE=$(printf '%s' "$BODY" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)
[ -n "$BIGSPACE" ] || die "建临时空间失败:$(printf '%s' "$BODY" | head -c 200)"
BIGROOT=$(q1 "SELECT id FROM files WHERE space_id='$BIGSPACE' AND parent_id IS NULL;")
[ -n "$BIGROOT" ] || die "临时空间根目录缺失"
say "  临时空间 id=$BIGSPACE root=$BIGROOT(测完即毁)"
say "  直接 INSERT $ROWS 行(parent=根目录;不建 file_objects、不动 used_bytes/quota)…"
T0=$(date +%s)
$PSQL -c "INSERT INTO files (space_id, parent_id, owner_id, name, is_dir, size, mime_type, version, depth)
          SELECT '$BIGSPACE', '$BIGROOT', '$USER_ID',
                 'ts08-f' || lpad(g::text, 6, '0') || '.bin',
                 false, (g % 4096)::bigint, 'application/octet-stream', 1, 1
          FROM generate_series(1, $ROWS) g;" >/dev/null 2>&1 || die "插入失败"
T1=$(date +%s)
INSERTED=$(q1 "SELECT count(*) FROM files WHERE space_id='$BIGSPACE' AND parent_id='$BIGROOT';")
TBLSIZE=$(q1 "SELECT pg_size_pretty(pg_total_relation_size('files'));")
say "  插入完成 rows=$INSERTED 用时=$((T1-T0))s files 表=$TBLSIZE"
printf '%s\n' "list_insert_seconds=$((T1-T0))" "list_inserted_rows=$INSERTED" \
              "list_files_table_size=$TBLSIZE" >> "$SUMMARY"

i=0; while [ $i -lt 3 ]; do
  curl -s -o /dev/null "$BASE/api/v1/files?space=$BIGSPACE" -H "Authorization: Bearer $TOKEN"; i=$((i+1))
done

onet() { curl -s -o "$WORK/last.json" -w '%{time_total}' "$1" -H "Authorization: Bearer $TOKEN" \
           | awk '{printf "%.1f", $1*1000}'; }

measure_list() {
  OUT="$WORK/list-$1.samples"; : > "$OUT"; i=0
  while [ $i -lt "$3" ]; do onet "$2" >> "$OUT"; echo >> "$OUT"; i=$((i+1)); done
  sed -i '/^$/d' "$OUT"
  BYTES=$(wc -c < "$WORK/last.json")
  printf 'list_%s_response_bytes=%s\n' "$1" "$BYTES" >> "$SUMMARY"
  # 注意:样本**必须写成 [a,b,c] 形式**,stats.py 靠 '[' 判断这是数组(实测漏了 [] 导致分位数整段丢失)
  printf 'list_%s_samples_ms=[%s]\n' "$1" "$(tr '\n' ',' < "$OUT" | sed 's/,$//')" >> "$SUMMARY"
  say "  $1:响应体 ${BYTES} 字节"
}

say "  默认页(limit 缺省=200)×$NREP …"
measure_list default "$BASE/api/v1/files?space=$BIGSPACE" "$NREP"
sleep 2
say "  最大页(limit=999)×$NREP …"
measure_list max999 "$BASE/api/v1/files?space=$BIGSPACE&limit=999" "$NREP"
sleep 2

say "  全量分页遍历(keyset,limit=999)…"
SOUT="$WORK/list-sweep.samples"; : > "$SOUT"
AFTER=""; PAGES=0; SEEN=0
T0=$(now_ns)
while : ; do
  URL="$BASE/api/v1/files?space=$BIGSPACE&limit=999"
  [ -n "$AFTER" ] && URL="$URL&after=$AFTER"
  onet "$URL" >> "$SOUT"; echo >> "$SOUT"
  PAGES=$((PAGES+1))
  N=$(python3 -c 'import json,sys;print(len(json.load(open(sys.argv[1])).get("entries") or []))' "$WORK/last.json")
  NA=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("next_after") or "")' "$WORK/last.json")
  SEEN=$((SEEN+N))
  [ -z "$NA" ] && break
  case "$NA" in *[!A-Za-z0-9._-]*) say "  next_after 含需转义字符,提前停止"; break ;; esac
  AFTER="$NA"
  [ "$PAGES" -gt 2000 ] && break
  sleep 0.005
done
T1=$(now_ns)
WALL=$(awk -v a="$T0" -v b="$T1" 'BEGIN{printf "%.3f", (b-a)/1000000000}')
sed -i '/^$/d' "$SOUT"
say "  遍历 pages=$PAGES entries=$SEEN wall=${WALL}s"
printf '%s\n' "list_sweep_pages=$PAGES" "list_sweep_entries=$SEEN" "list_sweep_wall_s=$WALL" \
              "list_sweep_samples_ms=[$(tr '\n' ',' < "$SOUT" | sed 's/,$//')]" >> "$SUMMARY"

# ============================================================================
# 二、远端变更感知
# ============================================================================
say ""
say "== 二、远端变更感知 =="
cat > "$WORK/feed_measure.py" <<'PYEOF'
import base64, http.client, json, os, subprocess, sys, time, uuid

BASE = os.environ.get("BASE", "http://127.0.0.1:8080")
TOKEN = os.environ["TOKEN"]; SPACE = os.environ["SPACE"]
N = int(os.environ.get("N", "30"))
TIMEOUT_MS = int(os.environ.get("POLL_TIMEOUT_MS", "5000"))
POLL_MS = float(os.environ.get("POLL_INTERVAL_MS", "5"))
PREFIX = os.environ.get("NAME_PREFIX", "ts08probe")
STAMP = os.environ.get("STAMP") or ("%d-%d" % (os.getpid(), int(time.time())))
_hp = BASE.split("//", 1)[1].rstrip("/")
HOST, _p = _hp.split("/")[0].split(":"); PORT = int(_p)

def log(m): print("[feed] %s" % m, file=sys.stderr, flush=True)
def conn(): return http.client.HTTPConnection(HOST, PORT, timeout=30)

def psql(sql):
    p = subprocess.run(["sudo", "-u", "postgres", "psql", "-d", "netdisk", "-q", "-t", "-A",
                        "-F", "|", "-v", "ON_ERROR_STOP=1", "-c", sql],
                       capture_output=True, text=True, timeout=60)
    if p.returncode != 0: raise RuntimeError("psql: %s" % p.stderr.strip()[:200])
    return p.stdout.strip()

def feed_row(fid):
    out = psql("SELECT change_seq, (extract(epoch from created_at)*1000000000)::bigint FROM sync_feed "
               "WHERE space_id='%s' AND file_id='%s' ORDER BY change_seq DESC LIMIT 1;" % (SPACE, fid))
    if not out: return None, None
    a, b = out.split("|"); return int(a), int(b)

def backoff(call, what):
    last = None
    for att in range(6):
        try: return call()
        except RuntimeError as e:
            last = e; time.sleep(0.3 * (att + 1))
    raise RuntimeError("%s 持续失败: %r" % (what, last))

def tus_write(i, size=16):
    name = "%s-%s-%d.bin" % (PREFIX, STAMP, i)
    def once():
        c = conn()
        try:
            c.putrequest("POST", "/tus")
            c.putheader("Tus-Resumable", "1.0.0"); c.putheader("Upload-Length", str(size))
            c.putheader("Upload-Metadata", "filename " + base64.b64encode(name.encode()).decode())
            c.putheader("Authorization", "Bearer " + TOKEN); c.endheaders()
            r = c.getresponse(); raw = r.read(); hdrs = dict(r.getheaders()); code = r.status
        finally: c.close()
        if code != 201: raise RuntimeError("TUS create %d %s" % (code, raw[:120]))
        up = json.loads(raw)["upload_id"]; ticket = hdrs.get("X-Upload-Token", "")
        body = bytes([(i + j) % 251 for j in range(size)])
        t0 = time.time(); c = conn()
        try:
            c.putrequest("PATCH", "/tus/" + up)
            c.putheader("Tus-Resumable", "1.0.0"); c.putheader("Upload-Offset", "0")
            c.putheader("Content-Type", "application/offset+octet-stream")
            c.putheader("X-Upload-Token", ticket); c.putheader("Authorization", "Bearer " + TOKEN)
            c.putheader("Content-Length", str(size)); c.endheaders(); c.send(body)
            rr = c.getresponse(); payload = rr.read(); h2 = dict(rr.getheaders())
            t1 = time.time(); code2 = rr.status
        finally: c.close()
        if code2 != 200 or h2.get("Upload-Complete") != "true":
            raise RuntimeError("TUS patch %d %s" % (code2, payload[:120]))
        return {"t_done": t1, "write_ms": (t1 - t0) * 1000.0, "file_id": h2.get("X-File-Id", "")}
    return backoff(once, "TUS 写")

def multipart_write(i, size=16):
    name = "%s-mp-%s-%d.bin" % (PREFIX, STAMP, i)
    b = "----ts08" + uuid.uuid4().hex
    meta = json.dumps({"space_id": SPACE, "parent_id": "", "name": name, "size": size})
    body = b"".join([
        ("--%s\r\n" % b).encode(), b'Content-Disposition: form-data; name="metadata"\r\n\r\n',
        meta.encode(), b"\r\n", ("--%s\r\n" % b).encode(),
        ('Content-Disposition: form-data; name="file"; filename="%s"\r\n' % name).encode(),
        b"Content-Type: application/octet-stream\r\n\r\n",
        bytes([(i + j) % 251 for j in range(size)]), b"\r\n", ("--%s--\r\n" % b).encode()])
    def once():
        t0 = time.time(); c = conn()
        try:
            c.putrequest("POST", "/api/v1/upload/simple")
            c.putheader("Content-Type", "multipart/form-data; boundary=" + b)
            c.putheader("Authorization", "Bearer " + TOKEN)
            c.putheader("Content-Length", str(len(body))); c.endheaders(); c.send(body)
            r = c.getresponse(); raw = r.read(); t1 = time.time(); code = r.status
        finally: c.close()
        if code != 201: raise RuntimeError("simple %d %s" % (code, raw[:120]))
        return {"t_done": t1, "write_ms": (t1 - t0) * 1000.0, "file_id": json.loads(raw)["file"]["id"]}
    return backoff(once, "multipart 写")

def poll_visible(seq, deadline):
    while time.time() < deadline:
        c = conn()
        try:
            c.request("GET", "/api/v1/changes?space=%s&since=%d&limit=1" % (SPACE, seq - 1),
                      headers={"Authorization": "Bearer " + TOKEN})
            r = c.getresponse(); raw = r.read(); code = r.status
        finally: c.close()
        if code != 200: raise RuntimeError("changes %d %s" % (code, raw[:120]))
        items = json.loads(raw).get("items") or []
        if items and int(items[0]["change_seq"]) >= seq: return time.time()
        time.sleep(POLL_MS / 1000.0)
    return None

R = {"tus_write_ms": [], "tus_changes_ms": [], "mp_write_ms": [], "mp_changes_ms": [],
     "sse_ms": [], "sse_write_ms": [], "ids": [], "errors": 0}

def run(channel, writer, n):
    for i in range(n):
        try:
            w = writer(i + (5000 if channel == "mp" else 0))
            seq, ns = feed_row(w["file_id"])
            if seq is None: raise RuntimeError("sync_feed 找不到该变更")
            t = poll_visible(seq, time.time() + TIMEOUT_MS / 1000.0)
            if t is None: raise RuntimeError("超时未见变更 seq=%d" % seq)
            ms = (t - ns / 1e9) * 1000.0
            if channel == "tus":
                R["tus_write_ms"].append(round(w["write_ms"], 3)); R["tus_changes_ms"].append(round(ms, 3))
            else:
                R["mp_write_ms"].append(round(w["write_ms"], 3)); R["mp_changes_ms"].append(round(ms, 3))
            R["ids"].append(w["file_id"])
            log("%s #%d write=%.1fms visible=%.1fms seq=%d" % (channel, i, w["write_ms"], ms, seq))
            time.sleep(0.05)
        except Exception as e:
            R["errors"] += 1; log("%s #%d 失败: %r" % (channel, i, e))

log("A) TUS 写 → /changes 可见")
run("tus", tus_write, N)
log("B) multipart 写 → /changes 可见")
run("mp", multipart_write, N)

log("C) multipart 写 → SSE 帧到达")
sse = conn()
sse.putrequest("GET", "/api/v1/events")
sse.putheader("Authorization", "Bearer " + TOKEN); sse.putheader("Accept", "text/event-stream")
sse.endheaders(); sr = sse.getresponse()
if sr.status != 200:
    R["errors"] += 1; log("SSE 建连失败 %d" % sr.status)
else:
    for i in range(N):
        try:
            w = multipart_write(10000 + i)
            seq, ns = feed_row(w["file_id"])
            got = None; deadline = time.time() + TIMEOUT_MS / 1000.0
            while time.time() < deadline:
                line = sr.readline()
                if not line: raise RuntimeError("SSE 连接被关闭")
                s = line.decode("utf-8", "replace").rstrip("\r\n")
                if not s.startswith("id: "): continue
                sp, _, sq = s[4:].partition(":")
                if sp != SPACE or not sq.isdigit(): continue
                if int(sq) >= seq: got = time.time(); break
            if got is None: raise RuntimeError("超时未收到 seq>=%d 的帧" % seq)
            ms = (got - ns / 1e9) * 1000.0
            R["sse_ms"].append(round(ms, 3)); R["sse_write_ms"].append(round(w["write_ms"], 3))
            log("sse #%d write=%.1fms frame=%.1fms seq=%d" % (i, w["write_ms"], ms, seq))
            time.sleep(0.05)
        except Exception as e:
            R["errors"] += 1; log("sse #%d 失败: %r" % (i, e)); break
    try: sse.close()
    except Exception: pass

log("D) TUS 写 → SSE(反证:该路径未推事件,预期 0 帧)")
tus_frames = -1; probe = ""
sse = conn()
sse.putrequest("GET", "/api/v1/events")
sse.putheader("Authorization", "Bearer " + TOKEN); sse.putheader("Accept", "text/event-stream")
sse.endheaders(); sr = sse.getresponse()
if sr.status != 200:
    R["errors"] += 1
else:
    try:
        w = tus_write(20000); seq, ns = feed_row(w["file_id"])
        sse.sock.settimeout(4.0); tus_frames = 0; end = time.time() + 4.0
        while time.time() < end:
            try: line = sr.readline()
            except Exception: break
            if not line: break
            if line.decode("utf-8", "replace").startswith("id: "): tus_frames += 1
        probe = "TUS 定稿 seq=%d:4s 内收到 %d 个 change 帧" % (seq, tus_frames)
    except Exception as e:
        probe = "探针异常: %r" % (e,)
    try: sse.close()
    except Exception: pass
log("D) %s" % probe)

for k in ("tus_write_ms", "tus_changes_ms", "mp_write_ms", "mp_changes_ms", "sse_ms",
          "sse_write_ms", "ids"):
    print("%s=%s" % (k, json.dumps(R[k])))
print("tus_sse_frames_in_4s=%d" % tus_frames)
print("tus_sse_probe=%s" % json.dumps(probe, ensure_ascii=False))
print("feed_errors=%d" % R["errors"])
PYEOF

BASE="$BASE" TOKEN="$TOKEN" SPACE="$PERSONAL" N="$NFEED" NAME_PREFIX="$PREFIX" \
  POLL_INTERVAL_MS="$POLL_INTERVAL_MS" POLL_TIMEOUT_MS="$POLL_TIMEOUT_MS" \
  python3 "$WORK/feed_measure.py" > "$WORK/feed.out" 2> "$WORK/feed.err" || true
sed 's/^/  /' "$WORK/feed.err"
cat "$WORK/feed.out" >> "$SUMMARY"

# ============================================================================
# 三、并发上传/下载
# ============================================================================
say ""
say "== 三、并发上传/下载($SAR 并发,文件 $UPFILE 字节)=="
SRC="$WORK/up.bin"
head -c "$UPFILE" /dev/urandom > "$SRC" || die "生成测试文件失败"
SIZE=$(wc -c < "$SRC")

up_worker() {
  i="$1"; chunk="$WORK/up/chunk-$i"; HEAD="$WORK/up/head-$i"; BODY="$WORK/up/body-$i"
  T_START=$(now_ns)
  NAME="$PREFIX-up-c$CONC-$i.bin"
  META="filename $(printf '%s' "$NAME" | base64 | tr -d '\n')"
  # create 走 upload 限速(10/s):429 必须退避重试并**单独计数**,
  # 否则 16 并发会把"限速拒绝"误报成"上传失败"(实测踩到)——
  # 限速拒绝本身是事实,要如实报;重试后的并发吞吐是另一个数。
  CODE=""; TRIES=0
  while [ "$TRIES" -lt 8 ]; do
    TRIES=$((TRIES+1))
    CODE=$(curl -s -o "$BODY" -D "$HEAD" -w '%{http_code}' --max-time 300 -X POST "$BASE/tus" \
      -H 'Tus-Resumable: 1.0.0' -H "Upload-Length: $SIZE" -H "Upload-Metadata: $META" \
      -H "Authorization: Bearer $TOKEN")
    [ "$CODE" = "429" ] || break
    echo "429create" >> "$WORK/up/retries"
    sleep 0.35
  done
  if [ "$CODE" = "429" ]; then
    echo "$i ERR create_429_(8次重试后仍被限速)" >> "$WORK/up/errs"; return 1
  fi
  if [ "$CODE" != "201" ]; then echo "$i ERR create_$CODE" >> "$WORK/up/errs"; return 1; fi
  UP=$(sed -n 's/.*"upload_id":"\([^"]*\)".*/\1/p' "$BODY")
  TICKET=$(tr -d '\r' < "$HEAD" | sed -n 's/^[Xx]-[Uu]pload-[Tt]oken: *//p' | head -1)
  OFF=0
  while [ "$OFF" -lt "$SIZE" ]; do
    N=$(( SIZE - OFF )); [ "$N" -gt 33554432 ] && N=33554432
    dd if="$SRC" of="$chunk" bs=1 skip="$OFF" count="$N" 2>/dev/null
    RESP=""; RT=0
    while [ "$RT" -lt 8 ]; do
      RT=$((RT+1))
      RESP=$(curl -s -D "$HEAD.p" -o "$BODY.p" -w '%{http_code}' --max-time 300 -X PATCH "$BASE/tus/$UP" \
        -H 'Tus-Resumable: 1.0.0' -H "Upload-Offset: $OFF" -H "X-Upload-Token: $TICKET" \
        -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/offset+octet-stream' \
        --data-binary @"$chunk")
      [ "$RESP" = "429" ] || break
      echo "429patch" >> "$WORK/up/retries"
      sleep 0.35
    done
    case "$RESP" in
      204) OFF=$(( OFF + N )) ;;
      200) OFF=$(( OFF + N )); break ;;
      *) echo "$i ERR patch_${RESP}_at_${OFF}" >> "$WORK/up/errs"; return 1 ;;
    esac
  done
  FID=$(tr -d '\r' < "$HEAD.p" | sed -n 's/^[Xx]-[Ff]ile-[Ii]d: *//p' | head -1)
  T_END=$(now_ns)
  echo "up_latency_ms=$(awk -v a="$T_START" -v b="$T_END" 'BEGIN{printf "%.1f",(b-a)/1000000}')"
  echo "$i OK $FID" >> "$WORK/up/done"
  return 0
}

run_upload() {
  CONC="$1"
  rm -rf "$WORK/up"; mkdir -p "$WORK/up"; : > "$WORK/up/errs"; : > "$WORK/up/done"
  : > "$WORK/up/retries"
  PIDS=""; T0=$(now_ns); i=1
  while [ "$i" -le "$CONC" ]; do
    ( WORK="$WORK" BASE="$BASE" TOKEN="$TOKEN" SRC="$SRC" SIZE="$SIZE" CONC="$CONC" PREFIX="$PREFIX" up_worker "$i" ) \
      > "$WORK/up/out-$i" 2>&1 &
    PIDS="$PIDS $!"; i=$((i+1))
  done
  for p in $PIDS; do wait "$p" || true; done
  T1=$(now_ns); NS=$(( T1 - T0 ))
  OK=$(wc -l < "$WORK/up/done" | tr -d ' '); ERRS=$(wc -l < "$WORK/up/errs" | tr -d ' ')
  RETRY=$(wc -l < "$WORK/up/retries" 2>/dev/null | tr -d ' ')
  MSG=$(tr '\n' ';' < "$WORK/up/errs" | head -c 200)
  LAT=$(sed -n 's/^up_latency_ms=//p' "$WORK/up/out-"* 2>/dev/null | sed '/^$/d' | tr '\n' ',' | sed 's/,$//')
  AGG=$(awk -v b="$(( SIZE * CONC ))" -v ns="$NS" 'BEGIN{ if(ns>0) printf "%.2f", (b/1048576)/(ns/1000000000); else print "0" }')
  WALL=$(awk -v ns="$NS" 'BEGIN{printf "%.3f", ns/1000000000}')
  say "  上传 ×$CONC: ${AGG} MB/s wall=${WALL}s ok=$OK err=$ERRS 限速重试=${RETRY:-0}"
  printf '%s\n' "concurrent_upload_${CONC}_mbps=$AGG" "concurrent_upload_${CONC}_wall_s=$WALL" \
    "concurrent_upload_${CONC}_ok=$OK" "concurrent_upload_${CONC}_errors=$ERRS" \
    "concurrent_upload_${CONC}_ratelimit_retries=${RETRY:-0}" \
    "concurrent_upload_${CONC}_latency_ms=[$LAT]" "concurrent_upload_${CONC}_errmsg=$MSG" >> "$SUMMARY"
}

dl_worker() {
  i="$1"
  CODE=$(curl -s -o /dev/null -w '%{http_code} %{size_download} %{time_total}' --max-time 300 \
    "$BASE/api/v1/files/$FID/content" -H "Authorization: Bearer $TOKEN")
  echo "$i $CODE" >> "$WORK/down/res-$i"
  return 0
}

run_download() {
  CONC="$1"; FID="$2"
  rm -rf "$WORK/down"; mkdir -p "$WORK/down"
  PIDS=""; T0=$(now_ns); i=1
  while [ "$i" -le "$CONC" ]; do
    ( WORK="$WORK" BASE="$BASE" TOKEN="$TOKEN" FID="$FID" dl_worker "$i" ) >/dev/null 2>&1 &    PIDS="$PIDS $!"; i=$((i+1))
  done
  for p in $PIDS; do wait "$p" || true; done
  T1=$(now_ns); NS=$(( T1 - T0 ))
  OK=0; ERRS=0; BYTES=0; LAT=""
  for f in "$WORK"/down/res-*; do
    [ -f "$f" ] || continue
    read -r idx code sz tm < "$f"
    if [ "$code" = "200" ]; then
      OK=$(( OK + 1 )); BYTES=$(( BYTES + ${sz%.*} ))
      LAT="$LAT $(awk -v t="$tm" 'BEGIN{printf "%.1f", t*1000}')"
    else ERRS=$(( ERRS + 1 )); fi
  done
  AGG=$(awk -v b="$BYTES" -v ns="$NS" 'BEGIN{ if(ns>0) printf "%.2f", (b/1048576)/(ns/1000000000); else print "0" }')
  LATC=$(echo $LAT | tr ' ' ',' | sed 's/^,//')
  WALL=$(awk -v ns="$NS" 'BEGIN{printf "%.3f", ns/1000000000}')
  say "  下载 ×$CONC: ${AGG} MB/s wall=${WALL}s ok=$OK err=$ERRS bytes=$BYTES"
  printf '%s\n' "concurrent_download_${CONC}_mbps=$AGG" "concurrent_download_${CONC}_wall_s=$WALL" \
    "concurrent_download_${CONC}_bytes=$BYTES" "concurrent_download_${CONC}_ok=$OK" \
    "concurrent_download_${CONC}_errors=$ERRS" "concurrent_download_${CONC}_latency_ms=[$LATC]" >> "$SUMMARY"
}

for c in $SAR; do run_upload "$c"; sleep 3; done

DLNAME="$PREFIX-dl.bin"
META="filename $(printf '%s' "$DLNAME" | base64 | tr -d '\n')"
DLFID=""
CODE=$(curl -s -o "$WORK/dlbody" -D "$WORK/dlhead" -w '%{http_code}' --max-time 300 -X POST "$BASE/tus" \
  -H 'Tus-Resumable: 1.0.0' -H "Upload-Length: $SIZE" -H "Upload-Metadata: $META" \
  -H "Authorization: Bearer $TOKEN")
if [ "$CODE" = "201" ]; then
  DUP=$(sed -n 's/.*"upload_id":"\([^"]*\)".*/\1/p' "$WORK/dlbody")
  DT=$(tr -d '\r' < "$WORK/dlhead" | sed -n 's/^[Xx]-[Uu]pload-[Tt]oken: *//p' | head -1)
  RC=$(curl -s -o /dev/null -D "$WORK/dlh2" -w '%{http_code}' --max-time 300 -X PATCH "$BASE/tus/$DUP" \
    -H 'Tus-Resumable: 1.0.0' -H 'Upload-Offset: 0' -H "X-Upload-Token: $DT" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/offset+octet-stream' \
    --data-binary @"$SRC")
  [ "$RC" = "200" ] && DLFID=$(tr -d '\r' < "$WORK/dlh2" | sed -n 's/^[Xx]-[Ff]ile-[Ii]d: *//p' | head -1)
fi
if [ -n "$DLFID" ]; then
  sleep 2
  for c in $SAR; do run_download "$c" "$DLFID"; sleep 3; done
else
  say "  下载测量跳过:下载用对象上传失败"
  for c in $SAR; do
    printf '%s\n' "concurrent_download_${c}_mbps=" "concurrent_download_${c}_ok=0" \
                  "concurrent_download_${c}_errors=1" >> "$SUMMARY"
  done
fi

# ============================================================================
# 四、统计(分位数/直方图/判定)
# ============================================================================
# 清理脚本(供 cleanup() 调用)
cat > "$WORK/cleanfiles.py" <<'PYEOF'
import http.client, json, os, time
BASE = os.environ.get("BASE", "http://127.0.0.1:8080")
TOKEN = os.environ["TOKEN"]; SPACE = os.environ["SPACE"]
PREFIXES = [p for p in os.environ.get("PREFIXES", "ts08").split(",") if p]
_hp = BASE.split("//", 1)[1].rstrip("/")
HOST, _p = _hp.split("/")[0].split(":"); PORT = int(_p)

def req(method, path):
    c = http.client.HTTPConnection(HOST, PORT, timeout=30)
    try:
        c.request(method, path, headers={"Authorization": "Bearer " + TOKEN})
        r = c.getresponse(); return r.status, r.read()
    finally: c.close()

def list_all():
    out, after = [], ""
    for _ in range(5000):
        path = "/api/v1/files?space=%s&limit=999" % SPACE
        if after: path += "&after=" + after
        for att in range(5):
            st, raw = req("GET", path)
            if st != 429: break
            time.sleep(0.3 * (att + 1))
        if st != 200: raise SystemExit("list %d %s" % (st, raw[:200]))
        d = json.loads(raw)
        out.extend(d.get("entries") or [])
        after = d.get("next_after") or ""
        if not after: break
    return out

entries = list_all()
targets = [e for e in entries if any(e["name"].startswith(p) for p in PREFIXES)]
print("个人空间条目=%d 需删除(前缀 %s)=%d" % (len(entries), PREFIXES, len(targets)))
done = 0; fail = 0
for e in targets:
    # 删除走 file_write 限速(实测 50/s):必须对 429 退避重试,否则清理会**静默失败**(实测踩到)
    st = 0
    for att in range(6):
        st, _ = req("DELETE", "/api/v1/files/" + e["id"])
        if st == 200: done += 1; break
        if st == 429: time.sleep(0.3 * (att + 1)); continue
        fail += 1; print("  delete %-58s -> %d" % (e["name"], st)); break
    else:
        fail += 1; print("  delete %-58s -> 429 持续" % e["name"])
    time.sleep(0.03)
print("已删除=%d 失败=%d" % (done, fail))
left = list_all()
lv = [e for e in left if any(e["name"].startswith(p) for p in PREFIXES)]
print("清理后:条目=%d 仍匹配前缀=%d" % (len(left), len(lv)))
for e in left:
    print("  keep %s (%d)" % (e["name"], e["size"]))
print("cleanup_probe_files_remaining=%d" % len(lv))
PYEOF

cat > "$WORK/stats.py" <<'PYEOF'
import math, os, re
SUM = os.environ["SUMMARY"]; WORK = os.environ["WORK"]

def pct(xs, p):
    if not xs: return float("nan")
    s = sorted(xs); k = (len(s) - 1) * p
    lo, hi = math.floor(k), math.ceil(k)
    return s[lo] if lo == hi else s[lo] * (hi - k) + s[hi] * (k - lo)

lines = [L.rstrip("\n") for L in open(SUM, encoding="utf-8")]
kv = {}
for L in lines:
    if "=" in L:
        k, v = L.split("=", 1); kv[k] = v

def samples(name):
    v = kv.get(name, "")
    if not v.startswith("["): return []
    return [float(x) for x in v[1:-1].split(",") if x.strip()]

def hist(xs, edges):
    parts = []
    for i, e in enumerate(edges):
        lo = edges[i-1] if i else 0
        c = sum(1 for x in xs if lo <= x < e)
        if c: parts.append("<%d:%d" % (e, c))
    return ";".join(parts)

out = []
def emit(k, v): out.append("%s=%s" % (k, v))

# 列表:默认页(客户端默认 = 200)是判定口径;若缺则回落到最大页
for label in ("default", "max999", "sweep"):
    xs = samples("list_%s_samples_ms" % label)
    if not xs: continue
    emit("list_%s_n" % label, len(xs))
    emit("list_%s_min_ms" % label, "%.1f" % min(xs))
    emit("list_%s_p50_ms" % label, "%.1f" % pct(xs, 0.50))
    emit("list_%s_p95_ms" % label, "%.1f" % pct(xs, 0.95))
    emit("list_%s_max_ms" % label, "%.1f" % max(xs))
    emit("list_%s_mean_ms" % label, "%.1f" % (sum(xs) / len(xs)))
    emit("list_%s_hist_ms" % label, hist(xs, [40, 60, 80, 100, 150, 200, 300, 500, 1000, 10**9]))

dd = samples("list_default_samples_ms") or samples("list_max999_samples_ms")
if dd:
    emit("list_p95_ms", "%.1f" % pct(dd, 0.95))
    emit("list_p50_ms", "%.1f" % pct(dd, 0.50))
    emit("list_max_ms", "%.1f" % max(dd))

for key, edges in (("tus_changes_ms", [10, 20, 30, 50, 80, 120, 200, 500, 1000, 3000, 10**9]),
                   ("mp_changes_ms", [10, 20, 30, 50, 80, 120, 200, 500, 1000, 3000, 10**9]),
                   ("sse_ms", [10, 20, 30, 50, 80, 120, 200, 500, 1000, 3000, 10**9])):
    xs = samples(key)
    if not xs: continue
    emit("%s_n" % key, len(xs))
    emit("%s_p50_ms" % key, "%.1f" % pct(xs, 0.50))
    emit("%s_p95_ms" % key, "%.1f" % pct(xs, 0.95))
    emit("%s_max_ms" % key, "%.1f" % max(xs))
    emit("%s_mean_ms" % key, "%.1f" % (sum(xs) / len(xs)))
    emit("%s_hist_ms" % key, hist(xs, edges))
for key in ("tus_write_ms", "mp_write_ms", "sse_write_ms"):
    xs = samples(key)
    if xs: emit("%s_p50_ms" % key, "%.1f" % pct(xs, 0.50)); emit("%s_p95_ms" % key, "%.1f" % pct(xs, 0.95))

if samples("mp_changes_ms"):
    emit("changes_p50_ms", "%.1f" % pct(samples("mp_changes_ms"), 0.50))
    emit("changes_p95_ms", "%.1f" % pct(samples("mp_changes_ms"), 0.95))
    emit("changes_max_ms", "%.1f" % max(samples("mp_changes_ms")))
if samples("sse_ms"):
    emit("sse_p50_ms", "%.1f" % pct(samples("sse_ms"), 0.50))
    emit("sse_p95_ms", "%.1f" % pct(samples("sse_ms"), 0.95))
    emit("sse_max_ms", "%.1f" % max(samples("sse_ms")))

for L in lines:
    m = re.match(r"^concurrent_(upload|download)_(\d+)_latency_ms=\[(.*)\]$", L)
    if not m: continue
    kind, conc, body = m.group(1), m.group(2), m.group(3)
    xs = [float(x) for x in body.split(",") if x.strip()]
    if not xs: continue
    emit("concurrent_%s_%s_p50_ms" % (kind, conc), "%.1f" % pct(xs, 0.50))
    emit("concurrent_%s_%s_p95_ms" % (kind, conc), "%.1f" % pct(xs, 0.95))
    emit("concurrent_%s_%s_max_ms" % (kind, conc), "%.1f" % max(xs))

rows = []
try:
    for L in open(os.path.join(WORK, "sampler.txt"), encoding="utf-8"):
        p = L.split()
        # 采样行 = " epoch  user_ticks  total_ticks  mem_avail_mb  load1 "
        # (第 2 列由 awk 一次打印两个数以支持 6 核 >100% 的占用,所以字段数是 5)
        if len(p) < 5: continue
        try: rows.append((int(p[0]), int(p[1]), int(p[2]), int(p[3]), float(p[4])))
        except ValueError: continue
except OSError:
    pass
if len(rows) >= 2:
    cpu = []
    for i in range(1, len(rows)):
        if rows[i][2] > rows[i-1][2]:
            cpu.append(100.0 * (rows[i][1] - rows[i-1][1]) / (rows[i][2] - rows[i-1][2]))
    mems = [r[3] for r in rows]; loads = [r[4] for r in rows]
    if cpu:
        emit("sampler_cpu_allcores_avg_pct", "%.1f" % (sum(cpu) / len(cpu)))
        emit("sampler_cpu_allcores_p95_pct", "%.1f" % pct(cpu, 0.95))
        emit("sampler_cpu_allcores_max_pct", "%.1f" % max(cpu))
    emit("sampler_mem_available_min_mb", min(mems))
    emit("sampler_mem_available_max_mb", max(mems))
    emit("sampler_load1_max", "%.2f" % max(loads))
    emit("sampler_samples", len(rows))

verdict = []
if dd:
    p95 = pct(dd, 0.95)
    verdict.append("verdict_list_p95_le_500ms=%s(list_default_p95=%.1fms)" % ("PASS" if p95 <= 500 else "FAIL", p95))
mc = samples("mp_changes_ms")
if mc:
    p95 = pct(mc, 0.95)
    verdict.append("verdict_changes_p95_le_3000ms=%s(changes_p95=%.1fms)" % ("PASS" if p95 <= 3000 else "FAIL", p95))
sc = samples("sse_ms")
if sc:
    p95 = pct(sc, 0.95)
    verdict.append("verdict_sse_p95_le_3000ms=%s(sse_p95=%.1fms)" % ("PASS" if p95 <= 3000 else "FAIL", p95))
for L in lines:
    m = re.match(r"^concurrent_(upload|download)_(\d+)_errors=(.*)$", L)
    if m:
        verdict.append("verdict_concurrent_%s_%s_no_errors=%s" % (
            m.group(1), m.group(2), "PASS" if m.group(3).strip() == "0" else "FAIL"))

with open(SUM, "w", encoding="utf-8") as f:
    for L in lines:
        f.write(L + "\n")
    f.write("# ---- 统计 ----\n")
    for L in out: f.write(L + "\n")
    f.write("# ---- 判定(阈值见验收点)----\n")
    for L in verdict: f.write(L + "\n")
print("\n".join(verdict))
PYEOF

kill "$SAMPLER_PID" 2>/dev/null || true
wait "$SAMPLER_PID" 2>/dev/null || true

# ---------------------------------------------------------------- 磁盘写基线
# 上传吞吐会被盘拖住;没有本机磁盘数就无法判断瓶颈在服务端还是在盘。
# 放在所有**延迟**测量之后:写 256MB 会脏页回写,可能拖慢紧随其后的列表延迟。
DSKT="$WORK/diskw"
T0=$(now_ns)
dd if=/dev/zero of="$DSKT" bs=1M count=256 conv=fsync 2>/dev/null
T1=$(now_ns)
DMBPS=$(awk -v ns="$(( T1 - T0 ))" 'BEGIN{ if(ns>0) printf "%.1f", 256/(ns/1000000000); else print "n/a" }')
rm -f "$DSKT"
say "  本机顺序写基线(256MB,fsync)=${DMBPS} MB/s"
printf 'disk_seq_write_baseline_mbps=%s\n' "$DMBPS" >> "$SUMMARY"

# ---------------------------------------------------------------- 清理探针文件
# 顺序要紧:先清探针文件,再跑 stats(否则 stats 覆盖 summary 会把清理证据丢掉;
# 10 万行临时空间在 EXIT 钩子里删,它的证据也在 summary 里)。
say ""
say "== 四、清理探针文件 =="
if [ -n "$TOKEN" ]; then
  PREFIXES="$PREFIX" BASE="$BASE" TOKEN="$TOKEN" SPACE="$PSPACE" \
    python3 "$WORK/cleanfiles.py" > "$WORK/clean1.out" 2>&1
  sed 's/^/  /' "$WORK/clean1.out"
  grep -E '^(已删除|清理后|cleanup_probe_files_remaining)' "$WORK/clean1.out" >> "$SUMMARY"
  # 顺带清掉以前失败运行留下的 ts08- 探针文件(避免污染后续基线)
  stale=$(PREFIXES="ts08-" BASE="$BASE" TOKEN="$TOKEN" SPACE="$PSPACE" \
    python3 "$WORK/cleanfiles.py" 2>&1 | sed -n 's/^cleanup_probe_files_remaining=//p')
  printf '%s\n' "cleanup_stale_ts08_remaining=${stale:-?}" >> "$SUMMARY"
  say "  历史 ts08- 残留(清理后)=${stale:-?}"
fi

say ""
say "== 五、统计与判定 =="
SUMMARY="$SUMMARY" WORK="$WORK" python3 "$WORK/stats.py" | sed 's/^/  /'
grep -E '_p50_ms=|_p95_ms=|_max_ms=|_mbps=|_errors=|_ok=|_hist_ms=' "$SUMMARY" | sed 's/^/  /'

# ============================================================================
say ""
say "===== TS-08 机器可读摘要 ====="
cat "$SUMMARY"
say "===== 摘要结束 ====="
exit 0
