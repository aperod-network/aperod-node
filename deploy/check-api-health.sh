#!/usr/bin/env bash
# check-api-health.sh — Verify aperod-api PM2 restart count has not spiked.
#
# Reads the restart count that was saved to a baseline file at deploy time and
# compares it against the live value from `pm2 jlist`.  Exits non-zero (and
# sends a Telegram alert) when the count has grown by more than RESTART_THRESHOLD.
#
# Usage:
#   bash check-api-health.sh [--threshold N] [--baseline-file PATH] [--wait-secs N]
#
# Options (can also be set as env vars):
#   --threshold N        Max allowed restarts since last baseline write (default: 5)
#   --baseline-file PATH Where the restart count baseline is stored
#                        (default: /tmp/aperod-api-restart-baseline)
#   --wait-secs N        Seconds to wait before sampling pm2 (default: 0)
#
# Optional env vars for Telegram alerts:
#   SUPPORT_BOT_TOKEN      — Telegram bot token
#   SUPPORT_ADMIN_CHAT_ID  — Telegram chat/user ID to receive the alert
#
# Exit codes:
#   0  — healthy (restarts within threshold)
#   1  — spike detected (restarts > threshold since baseline)
#   2  — pm2 or the aperod-api process is not available

set -euo pipefail

# ---------------------------------------------------------------------------
# Defaults (overridable via CLI or env)
# ---------------------------------------------------------------------------
RESTART_THRESHOLD="${RESTART_THRESHOLD:-5}"
BASELINE_FILE="${BASELINE_FILE:-/tmp/aperod-api-restart-baseline}"
WAIT_SECS="${WAIT_SECS:-0}"
PM2_APP="aperod-api"

# ---------------------------------------------------------------------------
# Parse CLI args
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --threshold)    RESTART_THRESHOLD="$2"; shift 2 ;;
    --baseline-file) BASELINE_FILE="$2"; shift 2 ;;
    --wait-secs)    WAIT_SECS="$2"; shift 2 ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Helper: send a Telegram message if credentials are configured.
# ---------------------------------------------------------------------------
send_telegram_alert() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      > /dev/null 2>&1 || true   # never let a failed alert abort the script
  fi
}

# ---------------------------------------------------------------------------
# Optional initial wait (e.g. give PM2 time to start the new process)
# ---------------------------------------------------------------------------
if [[ "$WAIT_SECS" -gt 0 ]]; then
  echo "  Waiting ${WAIT_SECS}s before sampling restart count..."
  sleep "$WAIT_SECS"
fi

# ---------------------------------------------------------------------------
# Read current restart count from PM2
# ---------------------------------------------------------------------------
if ! command -v pm2 &> /dev/null; then
  echo "✗ pm2 not found on PATH" >&2
  exit 2
fi

CURRENT_COUNT=$(pm2 jlist 2>/dev/null \
  | python3 -c "
import json, sys
try:
    data = json.load(sys.stdin)
    for p in data:
        if p.get('name') == '${PM2_APP}':
            print(p.get('pm2_env', {}).get('restart_time', 0))
            sys.exit(0)
    print('NOT_FOUND')
except Exception as e:
    print('ERROR:', e, file=sys.stderr)
    sys.exit(2)
" 2>&1) || {
  echo "✗ Failed to parse pm2 jlist output" >&2
  exit 2
}

if [[ "$CURRENT_COUNT" == "NOT_FOUND" ]]; then
  echo "✗ PM2 process '${PM2_APP}' not found in pm2 jlist" >&2
  exit 2
fi

echo "  Current restart count: ${CURRENT_COUNT}"

# ---------------------------------------------------------------------------
# Read baseline (set to current count if no baseline file exists yet)
# ---------------------------------------------------------------------------
if [[ -f "$BASELINE_FILE" ]]; then
  BASELINE_COUNT=$(cat "$BASELINE_FILE")
  echo "  Baseline restart count: ${BASELINE_COUNT} (from ${BASELINE_FILE})"
else
  echo "  No baseline file found at ${BASELINE_FILE} — treating current count as baseline."
  echo "$CURRENT_COUNT" > "$BASELINE_FILE"
  echo "  Baseline written: ${CURRENT_COUNT}"
  echo "✓ Health check passed (baseline initialised)."
  exit 0
fi

# ---------------------------------------------------------------------------
# Compare
# ---------------------------------------------------------------------------
DELTA=$(( CURRENT_COUNT - BASELINE_COUNT ))

if [[ "$DELTA" -lt 0 ]]; then
  # PM2 was restarted via `pm2 delete + pm2 start` — counter reset to 0.
  # Treat as healthy and update the baseline.
  echo "  Restart counter reset detected (delta=${DELTA}) — updating baseline."
  echo "$CURRENT_COUNT" > "$BASELINE_FILE"
  echo "✓ Health check passed (counter reset)."
  exit 0
fi

echo "  Restarts since last deploy: ${DELTA} (threshold: ${RESTART_THRESHOLD})"

if [[ "$DELTA" -gt "$RESTART_THRESHOLD" ]]; then
  echo ""
  echo "✗ RESTART SPIKE: ${PM2_APP} restarted ${DELTA} times since last deploy (threshold=${RESTART_THRESHOLD})." >&2
  echo "  Check logs: pm2 logs ${PM2_APP} --lines 100" >&2

  send_telegram_alert "🚨 <b>aperod-api RESTART SPIKE</b>
Server: $(hostname)
Restarts since last deploy: <b>${DELTA}</b> (threshold: ${RESTART_THRESHOLD})
Current restart count: ${CURRENT_COUNT}

Check logs: <code>pm2 logs ${PM2_APP} --lines 100</code>"

  exit 1
fi

echo "✓ Health check passed (${DELTA} restart(s) since last deploy, within threshold of ${RESTART_THRESHOLD})."
exit 0
