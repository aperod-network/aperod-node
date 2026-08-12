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
# Helper: kill whatever holds a TCP port and WAIT until it is actually free.
#
# A bare `fuser -k` + `sleep 1` is not enough: the old listener may take a few
# seconds to release the socket (graceful shutdown, FIN_WAIT).  If pm2 starts
# the new process while the port is still held, it crashes with EADDRINUSE and
# pm2 retries — each collision burns one restart against the post-deploy
# health-check threshold (observed: 5 restarts in 10 s, exactly at the limit).
# Polling until the port is free makes the subsequent start collision-free.
# ---------------------------------------------------------------------------
wait_port_free() {
  local port="$1" timeout="${2:-10}" waited=0
  if ! fuser "${port}/tcp" >/dev/null 2>&1; then
    return 0
  fi
  echo "  Port ${port} is in use — killing orphaned process..."
  fuser -k "${port}/tcp" 2>/dev/null || true
  while fuser "${port}/tcp" >/dev/null 2>&1; do
    if (( waited >= timeout )); then
      echo "  WARNING: port ${port} still busy after ${timeout}s — sending SIGKILL..."
      fuser -k -KILL "${port}/tcp" 2>/dev/null || true
      sleep 1
      if fuser "${port}/tcp" >/dev/null 2>&1; then
        echo "  ERROR: port ${port} could not be freed" >&2
        return 1
      fi
      break
    fi
    sleep 1
    waited=$((waited + 1))
  done
  echo "  Port ${port} is free (waited ${waited}s)."
}

# ---------------------------------------------------------------------------
# Step 1: Pull latest source
# ---------------------------------------------------------------------------
echo "==> [1/4] Pulling latest source as aperod..."
sudo -u aperod git -C "$APEROD_DIR" pull

# ---------------------------------------------------------------------------
# Step 1b: Keep /usr/local/bin/aperod_backup.sh in sync with the repo.
#
# Mirrors the same step in update-node.sh (see that file for full commentary).
# Both scripts run as root on the same server; either one can perform the
# privileged atomic copy, and subsequent runs are idempotent no-ops.
# ---------------------------------------------------------------------------
BLOCKCHAIN_DIR="${APEROD_DIR}/blockchain"
DEPLOY_DIR_API="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=sync-backup-script.sh
source "${DEPLOY_DIR_API}/sync-backup-script.sh"
echo "==> [1b] Syncing aperod_backup.sh..."
_sync_backup_script

# ---------------------------------------------------------------------------
# Step 1c: Keep /usr/local/bin/aperod-deploy in sync with the repo.
#
# /usr/local/bin/aperod-deploy is a copy of deploy/aperod-api-deploy.sh at the
# monorepo root.  It was previously installed once and never refreshed, so a
# git pull that changed the deploy script left the OLD logic running (Aug 7
# 2026 incident: the script was fixed 3 times while the installed copy lagged
# each time).  Reuse the same atomic stage-then-rename sync used for
# aperod_backup.sh; non-fatal if either side is absent.
# ---------------------------------------------------------------------------
echo "==> [1c] Syncing aperod-deploy..."
_sync_backup_script /usr/local/bin/aperod-deploy "${APEROD_DIR}/deploy/aperod-api-deploy.sh"

# ---------------------------------------------------------------------------
# Step 2: Rebuild TypeScript — if this fails, abort before touching pm2.
# ---------------------------------------------------------------------------
echo "==> [2/4] Rebuilding TypeScript as aperod..."

# Self-heal the known EACCES failure mode: if the API was ever started as
# root (emergency `node dist/index.mjs`, stale root PID), dist/ becomes
# root-owned and the aperod-user build fails with
# "EACCES: permission denied, unlink dist/index.mjs".  Fix ownership before
# building instead of aborting the deploy.
API_DIST="${APEROD_DIR}/artifacts/api-server/dist"
if [[ -d "${API_DIST}" ]] && find "${API_DIST}" ! -user aperod -print -quit | grep -q .; then
  echo "  [fix] ${API_DIST} contains non-aperod-owned files — chown -R aperod:aperod"
  chown -R aperod:aperod "${API_DIST}"
fi

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
# Kill any orphaned Node.js process still holding port 3001 (can happen when a
# previous PM2 daemon was killed but its child process survived) and wait until
# the port is genuinely free. Without this, the new process crashes immediately
# with EADDRINUSE and PM2 enters a crash loop.
API_PORT="${API_PORT:-3001}"
wait_port_free "$API_PORT" "${PORT_FREE_TIMEOUT:-10}" || {
  send_telegram_alert "⚠️ <b>aperod-api deploy</b>: port ${API_PORT} could not be freed on $(hostname); pm2 NOT restarted."
  echo "✗ Aborting before pm2 restart — old process still holds port ${API_PORT}." >&2
  exit 1
}

# If the process is not yet registered in PM2 (e.g. after server reboot or
# accidental `pm2 delete`), `pm2 restart` exits non-zero with "not found".
# Fall back to a clean start from the ecosystem config in that case.
# Use `--update-env` so new env vars in .env are picked up on restart.
if pm2 restart "$PM2_APP" --update-env 2>/dev/null; then
  echo "  PM2 restart successful."
else
  echo "  Process not found in PM2 — starting fresh from ecosystem config..."
  # Delete any stale/corrupted entry before starting fresh to avoid the
  # "Cannot read properties of undefined (reading 'pm2_env')" TypeError.
  pm2 delete "$PM2_APP" 2>/dev/null || true
  # Free the port again in the fallback path — the earlier wait ran before the
  # failed pm2 restart, but a brief race or a surviving zombie could have
  # re-bound it by the time we reach pm2 start.
  wait_port_free "$API_PORT" "${PORT_FREE_TIMEOUT:-10}" || {
    send_telegram_alert "⚠️ <b>aperod-api deploy</b>: port ${API_PORT} could not be freed on $(hostname); pm2 start skipped."
    echo "✗ Aborting fresh start — port ${API_PORT} still busy." >&2
    exit 1
  }
  # pm2 start from a bare ecosystem file loses the env vars that were in the
  # previous process descriptor (DATABASE_URL, SESSION_SECRET, etc.).
  # Source /opt/aperod/.env first so the new process inherits them.
  if [[ -f "${APEROD_DIR}/.env" ]]; then
    set -a && source "${APEROD_DIR}/.env" && set +a
    echo "  Loaded env vars from ${APEROD_DIR}/.env"
  fi
  pm2 start "${DEPLOY_DIR}/ecosystem.config.cjs"
fi

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
