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
set -euo pipefail

APEROD_DIR="/opt/aperod"
API_FILTER="@workspace/api-server"
PM2_APP="aperod-api"

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
echo "==> [1/3] Pulling latest source as aperod..."
sudo -u aperod git -C "$APEROD_DIR" pull

# ---------------------------------------------------------------------------
# Step 2: Rebuild TypeScript — if this fails, abort before touching pm2.
# ---------------------------------------------------------------------------
echo "==> [2/3] Rebuilding TypeScript as aperod..."
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
# Step 3: Restart PM2 only after a successful build
# ---------------------------------------------------------------------------
echo "==> [3/3] Restarting PM2 process '$PM2_APP'..."
pm2 restart "$PM2_APP"

echo ""
echo "✓ Update complete. New build is live."
echo "  Check logs: pm2 logs $PM2_APP --lines 50"
