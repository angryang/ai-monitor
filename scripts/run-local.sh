#!/usr/bin/env bash
# ai-monitor · 一键本地启动脚本
#
# 用法：
#   ./scripts/run-local.sh                # 前台跑（默认；Ctrl+C 退出）
#   ./scripts/run-local.sh --bg           # 后台跑，日志写到 /tmp/ai-monitor.log
#   ./scripts/run-local.sh --clean        # 启动前清空 SQLite DB
#   ./scripts/run-local.sh --rebuild      # 强制重新编译二进制后再启动
#   ./scripts/run-local.sh --bg --clean   # 组合
#
# 环境变量（脚本会兜底，但推荐你显式 export）：
#   AI_MONITOR_MASTER_KEY   AES-GCM 主密钥，必须正好 32 字节原始字符串
#   AI_MONITOR_JWT_SECRET   JWT HS256 密钥，≥32 字节原始字符串
#   AI_MONITOR_ADMIN_USERNAME  默认值 admin
#   AI_MONITOR_ADMIN_PASSWORD  默认值 LocalAdmin2026!Q（≥12 字符）
#   WECOM_WEBHOOK           默认值 https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test
#   AI_MONITOR_HTTP_ADDR    默认值 :9090
#
# 主密钥持久化：未显式 export AI_MONITOR_MASTER_KEY 时，脚本会把随机生成的
# 密钥写入仓库根的 .local-secrets（已 gitignore，权限 600），后续启动复用同一
# 份密钥。否则每次启动都是新密钥，DB 里已加密的 api_key 将无法解密。
#
# 退出（前台）：Ctrl+C
# 退出（后台）：kill $(cat /tmp/ai-monitor.pid)
set -euo pipefail

# ---------- locate project root ----------
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
PROJ_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
cd "$PROJ_ROOT"

SECRETS_FILE="$PROJ_ROOT/.local-secrets"

# ---------- flag parsing ----------
BG=0
CLEAN=0
REBUILD=0
for arg in "$@"; do
  case "$arg" in
    --bg)    BG=1 ;;
    --clean) CLEAN=1 ;;
    --rebuild) REBUILD=1 ;;
    -h|--help)
      sed -n '2,24p' "$0"
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

# ---------- local secret store ----------
# Keeps the generated master key / JWT secret stable across restarts. Without
# this, every start would mint a fresh key and the api_keys already sealed in
# the SQLite DB would fail to decrypt ("cipher: message authentication
# failed"), taking the whole service down at startup.
load_secret() {
  local key="$1"
  [[ -f "$SECRETS_FILE" ]] || return 0
  local line
  line=$(grep -E "^${key}=" "$SECRETS_FILE" 2>/dev/null | tail -n1) || return 0
  [[ -n "$line" ]] || return 0
  printf '%s' "${line#*=}"
}

save_secret() {
  local key="$1" val="$2"
  local tmp="$SECRETS_FILE.tmp.$$"
  if [[ -f "$SECRETS_FILE" ]]; then
    grep -vE "^${key}=" "$SECRETS_FILE" > "$tmp" 2>/dev/null || true
  else
    : > "$tmp"
  fi
  printf '%s=%s\n' "$key" "$val" >> "$tmp"
  chmod 600 "$tmp"
  mv "$tmp" "$SECRETS_FILE"
  chmod 600 "$SECRETS_FILE"
}

gen_secret() {
  # DO NOT use `tr -dc 'a-zA-Z0-9' < /dev/urandom | head -c 32` here.
  # Two independent failure modes, both observed on macOS:
  #   1. hangs — head exits after 32 bytes, but tr keeps reading the
  #      unbounded /dev/urandom stream and may never observe SIGPIPE, so
  #      the pipeline wedges forever and the script looks "stuck at startup";
  #   2. returns 141 — under `set -o pipefail` tr's SIGPIPE death propagates
  #      out of the command substitution, and `set -e` then kills the script
  #      silently.
  # openssl is bounded and side-effect free. Fallback keeps every stage of
  # the pipeline fed by a *finite* stream.
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex 16 # 32 hex chars == 32 bytes
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n' | cut -c1-32
  fi
}

# ---------- env defaults ----------
NEW_MASTER_KEY=0
if [[ -z "${AI_MONITOR_MASTER_KEY:-}" ]]; then
  AI_MONITOR_MASTER_KEY="$(load_secret AI_MONITOR_MASTER_KEY)"
  if [[ -z "$AI_MONITOR_MASTER_KEY" ]]; then
    AI_MONITOR_MASTER_KEY="$(gen_secret)"
    save_secret AI_MONITOR_MASTER_KEY "$AI_MONITOR_MASTER_KEY"
    NEW_MASTER_KEY=1
    echo "[run-local] generated a new master key -> $SECRETS_FILE (gitignored, mode 600)"
  fi
  export AI_MONITOR_MASTER_KEY
fi
if [[ -z "${AI_MONITOR_JWT_SECRET:-}" ]]; then
  AI_MONITOR_JWT_SECRET="$(load_secret AI_MONITOR_JWT_SECRET)"
  if [[ -z "$AI_MONITOR_JWT_SECRET" ]]; then
    AI_MONITOR_JWT_SECRET="$(gen_secret)"
    save_secret AI_MONITOR_JWT_SECRET "$AI_MONITOR_JWT_SECRET"
  fi
  export AI_MONITOR_JWT_SECRET
fi
export AI_MONITOR_ADMIN_USERNAME="${AI_MONITOR_ADMIN_USERNAME:-admin}"
export AI_MONITOR_ADMIN_PASSWORD="${AI_MONITOR_ADMIN_PASSWORD:-LocalAdmin2026!Q}"
export WECOM_WEBHOOK="${WECOM_WEBHOOK:-https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=test}"
export AI_MONITOR_HTTP_ADDR="${AI_MONITOR_HTTP_ADDR:-:9090}"

# Length sanity check, mirroring what the binary enforces so a bad export
# fails here instead of inside Go with a less obvious message:
#   config.NewAESCipher requires exactly 32 bytes,
#   auth.MinSecretBytes requires >= 32.
mk_len=${#AI_MONITOR_MASTER_KEY}
js_len=${#AI_MONITOR_JWT_SECRET}
if (( mk_len != 32 )); then
  echo "[run-local] FATAL: AI_MONITOR_MASTER_KEY is $mk_len bytes, must be exactly 32" >&2
  echo "[run-local] hint: export AI_MONITOR_MASTER_KEY=\$(openssl rand -hex 16)" >&2
  echo "[run-local] hint: remove any quoting that may have stripped the env value" >&2
  exit 3
fi
if (( js_len < 32 )); then
  echo "[run-local] FATAL: AI_MONITOR_JWT_SECRET is $js_len bytes, must be >= 32" >&2
  echo "[run-local] hint: export AI_MONITOR_JWT_SECRET=\$(openssl rand -hex 32)" >&2
  exit 3
fi

# ---------- build binary if missing or stale ----------
# The old logic only built when ./ai-monitor was absent, so any source change
# was silently ignored: you kept running a stale binary that predated the
# edit. Rebuild whenever any source file is newer than the binary.
NEED_BUILD=0
BUILD_REASON=""
if [[ ! -x ./ai-monitor ]]; then
  NEED_BUILD=1
  BUILD_REASON="binary missing"
elif (( REBUILD )); then
  NEED_BUILD=1
  BUILD_REASON="--rebuild requested"
else
  STALE=$(find cmd internal -type f \( -name '*.go' -o -name '*.html' -o -name '*.css' -o -name '*.js' \) \
    -newer ./ai-monitor -print -quit 2>/dev/null || true)
  if [[ -n "$STALE" ]]; then
    NEED_BUILD=1
    BUILD_REASON="sources newer than binary ($STALE)"
  fi
fi
if (( NEED_BUILD )); then
  if ! command -v go >/dev/null 2>&1; then
    echo "[run-local] FATAL: '$BUILD_REASON' but 'go' is not on PATH" >&2
    exit 4
  fi
  echo "[run-local] building ai-monitor from ./cmd/ai-monitor ($BUILD_REASON) ..."
  go build -o ai-monitor ./cmd/ai-monitor
  echo "[run-local] build ok"
fi

# ---------- run ----------
URL="http://127.0.0.1${AI_MONITOR_HTTP_ADDR}"
USER="${AI_MONITOR_ADMIN_USERNAME}"
PASS="${AI_MONITOR_ADMIN_PASSWORD}"

# A brand-new master key cannot decrypt api_keys sealed by an older key. Warn
# before the binary dies with a bare "cipher: message authentication failed".
if (( NEW_MASTER_KEY )) && [[ -s ai-monitor.db ]]; then
  echo "[run-local] WARN: ai-monitor.db already exists but a NEW master key was just generated." >&2
  echo "[run-local]       Existing encrypted api_keys cannot be decrypted with it." >&2
  echo "[run-local]       Fix: (a) restore the previous .local-secrets, or" >&2
  echo "[run-local]             (b) run './scripts/run-local.sh --clean' to start over." >&2
fi

cat <<EOF
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ai-monitor · starting
  URL    : ${URL}
  admin  : ${USER} / ${PASS}
  master : ${AI_MONITOR_MASTER_KEY:0:6}… (${SECRETS_FILE#$PROJ_ROOT/})
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
