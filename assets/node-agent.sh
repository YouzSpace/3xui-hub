#!/usr/bin/env bash
# ============================================================
# ControlHub xray 节点 agent
# 由 controlhub-node.service 守护；管理 xray 内核子进程。
#
# 职责（全部出连，节点零入站端口；GitHub 零依赖）：
#   1. 首跑生成 Reality 密钥对并注册（上报公钥/私钥/SNI/内核版本）
#   2. 按周期拉全量 config（ETag 304），xray -test 校验后原子替换、重启内核
#   3. 每 5s 读内核统计（statsquery）推流量绝对值（有变化才推）
#   4. 每 30s 心跳（资源 + 本地 config 版本）
#   5. execute 长轮询取 adu/rmu 指令，本地执行后回执
#   6. 看护 xray 进程（挂了拉起）；失败绝不半写配置
#
# 依赖：curl / python3 / 已安装的 xray（安装脚本已就位）
# ============================================================
set -uo pipefail

NODE_DIR="${NODE_DIR:-/opt/controlhub-node}"
STATE="$NODE_DIR/state.json"
CONFIG="$NODE_DIR/config.json"
ETAG_FILE="$NODE_DIR/etag.txt"
VERSION_FILE="$NODE_DIR/applied_version.txt"
INTERVAL_FILE="$NODE_DIR/pull_interval.txt"
LAST_PUSH="$NODE_DIR/last_push.txt"
PIDFILE="$NODE_DIR/xray.pid"
XRAY_LOG="$NODE_DIR/xray.log"
TMPD="$NODE_DIR/tmp"

API_PORT="${XRAY_API_PORT:-10085}"
DEFAULT_PULL_INTERVAL="${PULL_INTERVAL:-30}"

# JSON 处理解释器（Debian 自带 python3；个别环境只有 python）
PY=""
for _c in python3 python; do
  if command -v "$_c" >/dev/null 2>&1 && "$_c" -c 'import sys' >/dev/null 2>&1; then
    PY="$(command -v "$_c")"
    break
  fi
done

mkdir -p "$TMPD" 2>/dev/null || true

# 运行期注入（main 读取 state.json 后赋值）
HUB=""
SECRET=""

log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*"; }

# ── state.json 读写（含 Reality 密钥，600 权限） ──────────────

state_get() {
  "$PY" - "$STATE" "$1" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    d = {}
v = d.get(sys.argv[2], "")
print(v if v is not None else "")
PY
}

state_set() {
  "$PY" - "$STATE" "$1" "$2" <<'PY'
import json, sys
path, key, val = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    d = json.load(open(path))
except Exception:
    d = {}
d[key] = val
open(path, "w").write(json.dumps(d, ensure_ascii=False))
PY
  chmod 600 "$STATE" 2>/dev/null || true
}

# ── HTTP 工具：响应体 + 换行 + HTTP 码 ────────────────────────

split_resp() {
  CODE="${1##*$'\n'}"
  BODY="${1%$'\n'*}"
}

api_post() { # api_post <path> <json> [max_time]
  curl -sS --connect-timeout 10 --max-time "${3:-20}" -w '\n%{http_code}' \
    -H "X-Node-Secret: $SECRET" -H 'Content-Type: application/json' \
    -X POST -d "$2" "$HUB$1"
}

api_get() { # api_get <path> [if_none_match]
  local extra=()
  [ -n "${2:-}" ] && extra=(-H "If-None-Match: $2")
  curl -sS --connect-timeout 10 --max-time 20 -w '\n%{http_code}' \
    -D "$TMPD/hdr.txt" "${extra[@]}" \
    -H "X-Node-Secret: $SECRET" "$HUB$1"
}

# ── Reality 材料（首跑一次性生成，写回 state.json） ───────────

ensure_reality() {
  local pk pub out sid sni dest port
  pk="$(state_get reality_private_key)"
  pub="$(state_get reality_public_key)"
  if [ -n "$pk" ] && [ -n "$pub" ]; then
    return 0
  fi

  log "生成 Reality 密钥对（xray x25519）"
  out="$("$NODE_DIR/xray" x25519 2>/dev/null)" || { log "x25519 失败"; return 1; }
  pk="$(printf '%s\n' "$out" | grep -m1 'PrivateKey' | sed 's/.*: *//')"
  pub="$(printf '%s\n' "$out" | grep -m1 'PublicKey' | sed 's/.*: *//')"
  if [ -z "$pk" ] || [ -z "$pub" ]; then
    log "x25519 输出解析失败：$(printf '%s' "$out" | head -c 200)"
    return 1
  fi

  sid="$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')"   # 8 位十六进制 shortId
  sni="${REALITY_SNI:-www.cloudflare.com}"
  dest="${REALITY_DEST:-www.cloudflare.com:443}"
  port="${LISTEN_PORT:-443}"

  state_set reality_private_key "$pk"
  state_set reality_public_key "$pub"
  state_set reality_short_id "$sid"
  state_set reality_sni "$sni"
  state_set reality_dest "$dest"
  state_set listen_port "$port"
  log "Reality 材料就绪（sni=$sni port=$port）"
  return 0
}

# ── 注册 ─────────────────────────────────────────────────────

register() {
  local pk pub sid sni dest port xver body resp code0 pi
  pk="$(state_get reality_private_key)"
  pub="$(state_get reality_public_key)"
  sid="$(state_get reality_short_id)"
  sni="$(state_get reality_sni)"
  dest="$(state_get reality_dest)"
  port="$(state_get listen_port)"
  xver="$(state_get xray_version)"

  body="$("$PY" - "$pk" "$pub" "$sid" "$sni" "$dest" "$port" "$xver" <<'PY'
import json, sys
pk, pub, sid, sni, dest, port, xver = sys.argv[1:8]
print(json.dumps({
    "reality": {
        "private_key": pk,
        "public_key": pub,
        "sni": sni,
        "dest": dest,
        "short_ids": [sid],
        "listen_port": int(port or 443),
    },
    "xray_version": xver,
}, separators=(",", ":"), ensure_ascii=False))
PY
)"

  resp="$(api_post "/api/node-api/register" "$body" 20)" || return 1
  split_resp "$resp"
  code0="$(BODY="$BODY" "$PY" -c 'import os,json
try: print(json.loads(os.environ.get("BODY") or "{}").get("code",-1))
except Exception: print(-1)' 2>/dev/null || echo -1)"

  if [ "${CODE:-}" != "200" ] || [ "$code0" != "0" ]; then
    log "register 失败（HTTP ${CODE:-?}）：$(printf '%s' "$BODY" | head -c 200)"
    return 1
  fi

  pi="$(BODY="$BODY" "$PY" -c 'import os,json
try: print(json.loads(os.environ.get("BODY") or "{}").get("data",{}).get("pull_interval",30))
except Exception: print(30)' 2>/dev/null || echo 30)"
  printf '%s' "$pi" > "$INTERVAL_FILE"
  log "注册成功（拉取周期 ${pi}s）"
  return 0
}

# ── xray 进程管理 ────────────────────────────────────────────

xray_pid() {
  local pid=""
  [ -f "$PIDFILE" ] && pid="$(cat "$PIDFILE" 2>/dev/null || true)"
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    local cmd
    cmd="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
    if [ -z "$cmd" ] || printf '%s' "$cmd" | grep -q 'xray'; then
      printf '%s' "$pid"
      return 0
    fi
  fi
  return 1
}

start_xray() {
  xray_pid >/dev/null && return 0
  [ -f "$CONFIG" ] || { log "无 config，暂不能启动内核"; return 1; }
  log "启动 xray 内核"
  nohup "$NODE_DIR/xray" run -c "$CONFIG" >> "$XRAY_LOG" 2>&1 &
  echo $! > "$PIDFILE"
  sleep 1
  xray_pid >/dev/null
}

stop_xray() {
  local pid
  if pid="$(xray_pid)"; then
    kill "$pid" 2>/dev/null || true
    local i
    for i in $(seq 1 16); do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.5
    done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$PIDFILE"
}

restart_xray() {
  stop_xray
  start_xray
}

# ── 全量配置拉取（ETag 304 → 跳过；失败绝不替换） ─────────────

pull_config() {
  local etag="" resp new_etag
  [ -f "$ETAG_FILE" ] && etag="$(cat "$ETAG_FILE" 2>/dev/null || true)"

  resp="$(api_get "/api/node-api/config" "$etag")" || { log "config 拉取网络失败"; return 1; }
  split_resp "$resp"

  if [ "${CODE:-}" = "304" ]; then
    return 0
  fi
  if [ "${CODE:-}" != "200" ]; then
    log "config 拉取失败（HTTP ${CODE:-?}）：$(printf '%s' "$BODY" | head -c 200)"
    return 1
  fi

  if ! BODY="$BODY" "$PY" - "$NODE_DIR" <<'PY'
import json, os, sys
try:
    d = json.loads(os.environ.get("BODY") or "{}")
except Exception:
    sys.exit(1)
data = d.get("data") or {}
cfg = data.get("config")
if cfg is None:
    sys.exit(1)
base = sys.argv[1]
open(base + "/config.new.json", "w").write(json.dumps(cfg, ensure_ascii=False))
open(base + "/version.next", "w").write(str(data.get("config_version") or "0"))
open(base + "/pull_interval.txt", "w").write(str(data.get("pull_interval") or "30"))
PY
  then
    log "config 解析失败（响应异常，保留旧配置）"
    rm -f "$NODE_DIR/config.new.json" "$NODE_DIR/version.next"
    return 1
  fi

  if ! "$NODE_DIR/xray" -test -c "$NODE_DIR/config.new.json" >/dev/null 2>&1; then
    log "config 校验失败（保留旧配置，坏文件另存 config.bad.json 供排查）"
    mv -f "$NODE_DIR/config.new.json" "$NODE_DIR/config.bad.json" 2>/dev/null || true
    rm -f "$NODE_DIR/version.next"
    return 1
  fi

  [ -f "$CONFIG" ] && cp -f "$CONFIG" "$CONFIG.bak"
  mv -f "$NODE_DIR/config.new.json" "$CONFIG"
  [ -f "$NODE_DIR/version.next" ] && mv -f "$NODE_DIR/version.next" "$VERSION_FILE"

  new_etag="$(grep -i '^etag:' "$TMPD/hdr.txt" 2>/dev/null | tail -1 | sed 's/^[Ee][Tt][Aa][Gg]:[[:space:]]*//' | tr -d '\r')"
  [ -n "$new_etag" ] && printf '%s' "$new_etag" > "$ETAG_FILE"

  log "全量配置已更新（version $(cat "$VERSION_FILE" 2>/dev/null || echo ?)），重启内核"
  restart_xray
  return 0
}

# ── 流量：statsquery 聚合（user>>>email>>>traffic>>>uplink|downlink） ──

collect_stats() {
  local raw
  raw="$("$NODE_DIR/xray" api statsquery --server="127.0.0.1:$API_PORT" -pattern "user" 2>/dev/null)" || return 1
  [ -n "$raw" ] || return 1

  RAW="$raw" "$PY" - <<'PY'
import json, os, sys
try:
    d = json.loads(os.environ.get("RAW") or "{}")
except Exception:
    print('{"stats":{}}')
    sys.exit(0)
stats = {}
items = d.get("stat") or d.get("stats") or []
for s in items:
    if not isinstance(s, dict):
        continue
    parts = str(s.get("name") or "").split(">>>")
    if len(parts) >= 4 and parts[0] == "user" and parts[2] == "traffic":
        try:
            v = int(float(s.get("value") or 0))
        except Exception:
            v = 0
        row = stats.setdefault(parts[1], {"up": 0, "down": 0})
        if parts[3] == "uplink":
            row["up"] = v
        elif parts[3] == "downlink":
            row["down"] = v
print(json.dumps({"stats": stats}, separators=(",", ":"), ensure_ascii=False))
PY
}

push_loop() {
  local last="" stats sum resp
  [ -f "$LAST_PUSH" ] && last="$(cat "$LAST_PUSH" 2>/dev/null || true)"

  while true; do
    stats="$(collect_stats 2>/dev/null || true)"
    if [ -n "$stats" ] && [ "$stats" != '{"stats":{}}' ]; then
      sum="$(printf '%s' "$stats" | md5sum | awk '{print $1}')"
      if [ "$sum" != "$last" ]; then
        resp="$(api_post "/api/node-api/push" "$stats" 20 || true)"
        split_resp "$resp"
        if [ "${CODE:-}" = "200" ]; then
          last="$sum"
          printf '%s' "$sum" > "$LAST_PUSH"
        else
          log "流量推送失败（HTTP ${CODE:-?}），下轮重试"
          sleep 5
        fi
      fi
    fi
    sleep 5
  done
}

# ── 心跳 ────────────────────────────────────────────────────

alive_loop() {
  while true; do
    local xver cpu mem uptime ver body resp
    xver="$(state_get xray_version)"
    cpu="$(awk '/^cpu /{u=$2+$3; t=u+$4+$5; if (t>0) printf "%.1f", u*100/t; else print 0}' /proc/stat 2>/dev/null || echo 0)"
    mem="$(awk '/MemTotal/{t=$2} /MemAvailable/{a=$2} END{if (t>0) printf "%.1f", (t-a)*100/t; else print 0}' /proc/meminfo 2>/dev/null || echo 0)"
    uptime="$(cut -d. -f1 /proc/uptime 2>/dev/null || echo 0)"
    ver="$(cat "$VERSION_FILE" 2>/dev/null || echo 0)"

    body="$("$PY" - "$xver" "$cpu" "$mem" "$uptime" "$ver" <<'PY'
import json, sys
xver, cpu, mem, up, ver = sys.argv[1:6]
def num(v):
    try: return float(v)
    except Exception: return None
def i(v):
    try: return int(float(v))
    except Exception: return 0
print(json.dumps({
    "xray_version": xver or None,
    "cpu": num(cpu),
    "mem": num(mem),
    "uptime": i(up),
    "config_version": i(ver),
}, separators=(",", ":"), ensure_ascii=False))
PY
)"

    resp="$(api_post "/api/node-api/alive" "$body" 15 || true)"
    split_resp "$resp"
    if [ "${CODE:-}" != "200" ]; then
      log "心跳失败（HTTP ${CODE:-?}）"
    fi
    sleep 30
  done
}

# ── 指令：长轮询 → 本地执行 → 回执 ────────────────────────────

run_one_cycle() {
  local resp cmds list results n resp2 ack_body
  resp="$(api_post "/api/node-api/execute" '{}' 35 || true)"
  split_resp "$resp"
  if [ "${CODE:-}" != "200" ]; then
    log "execute 失败（HTTP ${CODE:-?}）"
    sleep 5
    return 0
  fi

  cmds="$(BODY="$BODY" "$PY" -c '
import os, json
try:
    d = json.loads(os.environ.get("BODY") or "{}")
except Exception:
    d = {}
print(json.dumps(d.get("data", {}).get("commands", []), separators=(",", ":")))' 2>/dev/null || echo '[]')"

  [ "$cmds" = "[]" ] && return 0
  [ "$cmds" = "" ] && return 0

  mkdir -p "$TMPD"
  list="$TMPD/cmdlist.tsv"
  results="$TMPD/results.tsv"
  : > "$results"

  # 展开为 tab 分隔行：id \t action \t <adu:文件路径 | rmu:tag \t email...>
  CMDS="$cmds" "$PY" - "$TMPD" > "$list" <<'PY'
import json, os, sys
tmpd = sys.argv[1]
try:
    commands = json.loads(os.environ.get("CMDS") or "[]")
except Exception:
    commands = []
for c in commands:
    cid = c.get("id")
    action = str(c.get("action") or "")
    payload = c.get("payload") or {}
    if action == "adu":
        path = os.path.join(tmpd, "adu_%s.json" % cid)
        open(path, "w").write(json.dumps(payload, ensure_ascii=False))
        print("%s\tadu\t%s" % (cid, path))
    elif action == "rmu":
        emails = payload.get("emails") or []
        tag = str(payload.get("tag") or "vless-reality")
        print("%s\trmu\t%s\t%s" % (cid, tag, "\t".join(str(e) for e in emails)))
PY

  n=0
  local cid action rest ok out arr tag
  while IFS=$'\t' read -r cid action rest; do
    [ -n "$cid" ] || continue
    ok=0
    out=""
    if [ "$action" = "adu" ]; then
      out="$("$NODE_DIR/xray" api adu --server="127.0.0.1:$API_PORT" "$rest" 2>&1)" && ok=1 || ok=0
    elif [ "$action" = "rmu" ]; then
      IFS=$'\t' read -r -a arr <<< "$rest"
      tag="${arr[0]:-vless-reality}"
      if [ "${#arr[@]}" -gt 1 ]; then
        out="$("$NODE_DIR/xray" api rmu --server="127.0.0.1:$API_PORT" -tag="$tag" "${arr[@]:1}" 2>&1)" && ok=1 || ok=0
      else
        ok=1
        out="no emails"
      fi
    else
      ok=0
      out="unknown action: $action"
    fi

    printf '%s\t%s\t%s\n' "$cid" "$ok" "$(printf '%s' "$out" | tr '\n' ' ' | head -c 1500)" >> "$results"
    if [ "$ok" = "1" ]; then
      log "指令 #$cid 执行成功（$action）"
    else
      log "指令 #$cid 执行失败（$action）：$(printf '%s' "$out" | head -c 300)"
    fi
    n=$((n + 1))
  done < "$list"

  [ "$n" -gt 0 ] || return 0

  ack_body="$(RESULTS_FILE="$results" "$PY" - <<'PY'
import json, os
rows = []
try:
    lines = open(os.environ["RESULTS_FILE"], encoding="utf-8", errors="replace").read().splitlines()
except Exception:
    lines = []
for line in lines:
    parts = line.split("\t", 2)
    if len(parts) < 3:
        continue
    cid, ok, out = parts
    try:
        cid = int(cid)
    except Exception:
        continue
    rows.append({"id": cid, "success": ok == "1", "output": out})
print(json.dumps({"results": rows}, ensure_ascii=False))
PY
)"

  resp2="$(api_post "/api/node-api/ack" "$ack_body" 20 || true)"
  split_resp "$resp2"
  if [ "${CODE:-}" != "200" ]; then
    log "指令回执失败（HTTP ${CODE:-?}）"
  fi

  find "$TMPD" -name 'adu_*.json' -mmin +10 -delete 2>/dev/null || true
}

execute_loop() {
  while true; do
    run_one_cycle || true
  done
}

# ── 配置刷新循环 ────────────────────────────────────────────

config_loop() {
  while true; do
    local pi="$DEFAULT_PULL_INTERVAL"
    if [ -f "$INTERVAL_FILE" ]; then
      pi="$(cat "$INTERVAL_FILE" 2>/dev/null || echo "$DEFAULT_PULL_INTERVAL")"
    fi
    case "$pi" in ''|*[!0-9]*) pi="$DEFAULT_PULL_INTERVAL" ;; esac
    [ "$pi" -ge 5 ] || pi=5

    pull_config || true
    sleep "$pi"
  done
}

# ── xray 看护循环 ────────────────────────────────────────────

supervise_loop() {
  while true; do
    if ! xray_pid >/dev/null; then
      log "检测到 xray 未在运行，重新拉起"
      start_xray || true
    fi

    if [ -f "$XRAY_LOG" ] && [ "$(stat -c%s "$XRAY_LOG" 2>/dev/null || echo 0)" -gt 5242880 ]; then
      tail -c 1048576 "$XRAY_LOG" > "$XRAY_LOG.tmp" 2>/dev/null && mv -f "$XRAY_LOG.tmp" "$XRAY_LOG"
    fi

    sleep 10
  done
}

# ── 主流程 ───────────────────────────────────────────────────

main() {
  [ -f "$STATE" ] || { log "缺少 $STATE（请用面板生成的安装命令重装）"; exit 1; }
  [ -x "$NODE_DIR/xray" ] || { log "缺少 $NODE_DIR/xray 内核"; exit 1; }
  [ -n "$PY" ] || { log "缺少 python3/python（JSON 处理依赖）"; exit 1; }

  mkdir -p "$TMPD"
  HUB="$(state_get hub_url)"
  HUB="${HUB%/}"
  SECRET="$(state_get node_secret)"
  if [ -z "$HUB" ] || [ -z "$SECRET" ]; then
    log "state.json 缺 hub_url / node_secret"
    exit 1
  fi

  log "启动 agent（$NODE_DIR → $HUB）"

  ensure_reality || { log "Reality 材料生成失败"; exit 1; }

  until register; do
    log "注册未成功，60s 后重试（检查节点密钥/面板可达性）"
    sleep 60
  done

  until pull_config; do
    log "首次拉取配置未成功，15s 后重试"
    sleep 15
  done

  start_xray || log "xray 启动失败（看护循环会持续重试）"

  log "进入常驻循环：execute / push / alive / config / supervise"
  execute_loop & push_loop & alive_loop & config_loop & supervise_loop &
  wait -n
  log "子循环退出，agent 退出（systemd 将重启）"
  exit 1
}

main "$@"
