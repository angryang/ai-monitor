#!/usr/bin/env bash
# ai-monitor · 一键本地启动脚本
#
# 用法：
#   ./scripts/run-local.sh                # 前台跑（默认；Ctrl+C 退出）
#   ./scripts/run-local.sh --bg           # 后台跑，日志写到 /tmp/ai-monitor.log
#   ./scripts/run-local.sh --clean        # 启动前清空 SQLite DB
#   ./scripts/run-local.sh --bg --clean   # 组合
#
# 必备环境变量（脚本会兜底，但推荐你显式 export）：
#   AI_MONITOR_MASTER_KEY   AES-GCM 主密钥，必须正好 32 字节原始字符串
#   AI_MONITOR_JWT_SECRET   JWT HS256 密钥，≥32 字节原始字符串
#   AI_MONITOR_ADMIN_USERNAME  默认值 admin
#   AI_MONITOR_ADMIN_PASSWORD  默认值 LocalAdmin2026!Q（≥12 字符）
#   WECOM_WEBHOOK           默认值 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test
#   AI_MONITOR_HTTP_ADDR    默认值 :9090
#
# 退出（前台）：Ctrl+C
# 退出（后台）：kill $(cat /tmp/ai-monitor.pid)
set -euo pipefail

# ---------- locate project root ----------
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
PROJ_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
cd "$PROJ_ROOT"

# ---------- flag parsing ----------
BG=0
CLEAN=0
for arg in "$@"; do
  case "$arg" in
    --bg)    BG=1 ;;
    --clean) CLEAN=1 ;;
    -h|--help)
      sed -n '2,16p' "$0"
      exit 0
      ;;
    *)
      echo "[run-local] unknown argument: $arg (use --help)" >&2
      exit 2
      ;;
  esac
done

# ---------- clean DB ----------
if (( CLEAN )); then
  rm -f ai-monitor.db ai-monitor.db-shm ai-monitor.db-wal
  echo "[run-local] cleared ai-monitor.db*"
fi

# ---------- env defaults ----------
if [[ -z "${AI_MONITOR_MASTER_KEY:-}" ]]; then
  export AI_MONITOR_MASTER_KEY="$(LC_ALL=C tr -dc 'a-zA-Z0-9' < /dev/urandom | head -c 32)"
fi
if [[ -z "${AI_MONITOR_JWT_SECRET:-}" ]]; then
  export AI_MONITOR_JWT_SECRET="$(LC_ALL=C tr -dc 'a-zA-Z0-9' < /dev/urandom | head -c 32)"
fi
export AI_MONITOR_ADMIN_USERNAME="${AI_MONITOR_ADMIN_USERNAME:-admin}"
export AI_MONITOR_ADMIN_PASSWORD="${AI_MONITOR_ADMIN_PASSWORD:-LocalAdmin2026!Q}"
export WECOM_WEBHOOK="${WECOM_WEBHOOK:-https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test}"
export AI_MONITOR_HTTP_ADDR="${AI_MONITOR_HTTP_ADDR:-:9090}"

# Quick length sanity check (catches typos like an empty export).
mk_len=${#AI_MONITOR_MASTER_KEY}
js_len=${#AI_MONITOR_JWT_SECRET}
if (( mk_len < 16 || js_len < 16 )); then
  echo "[run-local] FATAL: AI_MONITOR_MASTER_KEY=$mk_len bytes, AI_MONITOR_JWT_SECRET=$js_len bytes — both must be ≥16 raw chars" >&2
  echo "[run-local] hint: remove any quoting that may have stripped the env value" >&2
  exit 3
fi

# ---------- build binary if missing ----------
if [[ ! -x ./ai-monitor ]]; then
  if [[ -f go.mod ]]; then
    echo "[run-local] building ai-monitor from ./cmd/ai-monitor ..."
    ( cd "$PROJ_ROOT" && go build -o ai-monitor ./cmd/ai-monitor )
  else
    echo "[run-local] FATAL: no ./ai-monitor binary and no go.mod found in $PROJ_ROOT" >&2
    exit 4
  fi
fi

# ---------- run ----------
URL="http://127.0.0.1${AI_MONITOR_HTTP_ADDR}"
USER="${AI_MONITOR_ADMIN_USERNAME}"
PASS="${AI_MONITOR_ADMIN_PASSWORD}"

cat <<EOF
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ai-monitor · starting
  URL    : ${URL}
  admin  : ${USER} / ${PASS}
  (anywhere in browser if Dashboard assets are embedded;
   otherwise use the printed URL above)
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
EOF

if (( BG )); then
  nohup ./ai-monitor -config deploy/example.config.yaml \
    > /tmp/ai-monitor.log 2>&1 &
  echo $! > /tmp/ai-monitor.pid
  pid=$(cat /tmp/ai-monitor.pid)

  # Wait up to ~6s for healthz to come up; if not, kill and report.
  ok=0
  for i in 1 2 3 4 5 6 7 8 9 10 11 12; do
    sleep 0.5
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "[run-local] PID $pid exited prematurely; tail of log ↓"
      tail -n 30 /tmp/ai-monitor.log
      exit 5
    fi
    if curl -sf -m 1 "${URL}/healthz" >/dev/null 2>&1; then
      ok=1; break
    fi
  done

  echo "[run-local] background pid  : $pid"
  echo "[run-local] background log  : tail -f /tmp/ai-monitor.log"
  echo "[run-local] stop            : kill \$(cat /tmp/ai-monitor.pid)"
  if (( ok )); then
    echo "[run-local] healthz         : 200 OK"
  else
    echo "[run-local] healthz         : NOT responding after ~6s — tail of log ↓"
    tail -n 20 /tmp/ai-monitor.log
    echo "[run-local] common causes:"
    echo "  - port :9090 already bound by another process (lsof -nP -iTCP:9090 -sTCP:LISTEN)"
    echo "  - master / jwt env exported as empty string (start fresh with unset env)"
    echo "  - stale PID file (rm /tmp/ai-monitor.pid)"
    kill "$pid" 2>/dev/null || true
    exit 6
  fi
else
  exec ./ai-monitor -config deploy/example.config.yaml
fi
