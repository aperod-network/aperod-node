#!/usr/bin/env bash
# update-api.sh — Pull latest source, rebuild TypeScript, and restart the API.
#
# Run this script instead of bare `pm2 restart aperod-api` after every git pull.
# Bare restart replays the OLD compiled binary; new source changes are silently
# ignored until a rebuild is done.
#
# ONE-TIME PREREQUISITE (run once as root if dist/ was ever built by root):
#   chown -R aperod:aperod /opt/aperod/artifacts/api-server/dist/
#
# Usage:
#   sudo bash /opt/aperod/blockchain/deploy/update-api.sh
#
# The script must be run as root (or via sudo) so it can switch to the
# `aperod` user for git and pnpm operations, then call pm2 as root.
#
# Optional env vars for Telegram build-failure alerts:
#   SUPPORT_BOT_TOKEN      — Telegram bot token (e.g. 123456:ABC-xxx)
#   SUPPORT_ADMIN_CHAT_ID  — Telegram chat/user ID to receive the alert
#
# ---------------------------------------------------------------------------
# RESTART-SPIKE HEALTH CHECK
# ---------------------------------------------------------------------------
# After restarting PM2 the script runs check-api-health.sh (Step 4).
# That script reads the PM2 restart counter 10 seconds after the restart and
# compares it to the baseline saved during this deploy.  If the process has
# already crashed more than RESTART_THRESHOLD times (default: 5) it exits
# non-zero, prints an error, and sends a Telegram alert.
#
# Configurable env vars passed through to check-api-health.sh:
#   RESTART_THRESHOLD   — max allowed restarts since baseline (default: 5)
#   RESTART_BASELINE_FILE — path to the baseline file
#                           (default: /tmp/aperod-api-restart-baseline)
#   HEALTH_CHECK_WAIT   — seconds to wait before sampling pm2 (default: 10)
#
# To disable the health check entirely set SKIP_HEALTH_CHECK=1.
#
set -euo pipefail

APEROD_DIR="/opt/aperod"
API_FILTER="@workspace/api-server"
PM2_APP="aperod-api"
DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Health-check tunables (override via environment)
RESTART_THRESHOLD="${RESTART_THRESHOLD:-5}"
RESTART_BASELINE_FILE="${RESTART_BASELINE_FILE:-/tmp/aperod-api-restart-baseline}"
HEALTH_CHECK_WAIT="${HEALTH_CHECK_WAIT:-10}"
SKIP_HEALTH_CHECK="${SKIP_HEALTH_CHECK:-0}"

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
# Step 1: Pull latest source
# ---------------------------------------------------------------------------
echo "==> [1/4] Pulling latest source as aperod..."
sudo -u aperod git -C "$APEROD_DIR" pull

# ---------------------------------------------------------------------------
# Step 2: Rebuild TypeScript — if this fails, abort before touching pm2.
# ---------------------------------------------------------------------------
echo "==> [2/4] Rebuilding TypeScript as aperod..."
if ! sudo -u aperod bash -c "cd '$APEROD_DIR' && pnpm --filter '$API_FILTER' run build"; then
  echo ""
  echo "✗ Build failed — pm2 NOT restarted. Fix the error and re-run update-api.sh" >&2

  send_telegram_alert "⚠️ <b>aperod-api build FAILED</b>
Server: $(hostname)
pm2 was <b>NOT</b> restarted — the old binary is still running.
Fix the TypeScript error and re-run <code>update-api.sh</code>."

  exit 1
fi

# ---------------------------------------------------------------------------
# Step 3: Restart PM2 only after a successful build, then save a fresh
#         baseline restart count so the health check (Step 4) can detect
#         any crash-loop that starts immediately after this deploy.
# ---------------------------------------------------------------------------
echo "==> [3/4] Restarting PM2 process '$PM2_APP'..."
pm2 restart "$PM2_APP"

# Give PM2 a moment to initialise the new process before sampling the counter.
sleep 2

# Capture the post-restart restart count as the new baseline.
# check-api-health.sh will compare future counts against this value.
NEW_BASELINE=$(pm2 jlist 2>/dev/null \
  | python3 -c "
import json, sys
data = json.load(sys.stdin)
for p in data:
    if p.get('name') == '${PM2_APP}':
        print(p.get('pm2_env', {}).get('restart_time', 0))
        sys.exit(0)
print(0)
" 2>/dev/null || echo "0")

echo "$NEW_BASELINE" > "$RESTART_BASELINE_FILE"
echo "  Restart baseline saved: ${NEW_BASELINE} (file: ${RESTART_BASELINE_FILE})"

# ---------------------------------------------------------------------------
# Step 4: Health check — confirm the process isn't crash-looping.
#
# Waits HEALTH_CHECK_WAIT seconds (default: 10) then checks whether
# the restart counter has grown by more than RESTART_THRESHOLD (default: 5).
# Exits non-zero and fires a Telegram alert if a spike is detected.
# Set SKIP_HEALTH_CHECK=1 to bypass (e.g. during initial first-time setup).
# ---------------------------------------------------------------------------
echo "==> [4/4] Running post-deploy health check (waiting ${HEALTH_CHECK_WAIT}s)..."

if [[ "$SKIP_HEALTH_CHECK" == "1" ]]; then
  echo "  SKIP_HEALTH_CHECK=1 — skipping."
else
  RESTART_THRESHOLD="$RESTART_THRESHOLD" \
  BASELINE_FILE="$RESTART_BASELINE_FILE" \
  WAIT_SECS="$HEALTH_CHECK_WAIT" \
  SUPPORT_BOT_TOKEN="${SUPPORT_BOT_TOKEN:-}" \
  SUPPORT_ADMIN_CHAT_ID="${SUPPORT_ADMIN_CHAT_ID:-}" \
    bash "${DEPLOY_DIR}/check-api-health.sh" || {
      echo ""
      echo "✗ Health check FAILED — inspect pm2 logs immediately:" >&2
      echo "    pm2 logs ${PM2_APP} --lines 100" >&2
      exit 1
    }
fi

echo ""
echo "✓ Update complete. New build is live."
echo "  Check logs: pm2 logs $PM2_APP --lines 50"
echo ""
echo "  To check health at any time:"
echo "    bash ${DEPLOY_DIR}/check-api-health.sh --baseline-file ${RESTART_BASELINE_FILE}"
