#!/usr/bin/env bash
# =============================================================================
#  aperod-node-watchdog.sh — Restart aperod-node when its API stops responding
#
#  Invoked every 60 s by aperod-node-watchdog.timer.
#  Sends GET /api/v1/status to 127.0.0.1:8545 with a 5 s timeout.
#  If the response is not HTTP 200, triggers `systemctl restart aperod-node`.
#
#  Optional env vars (set in the .service unit's Environment= lines or in
#  /etc/aperod/watchdog.env):
#    NODE_API_URL   — base URL of the Go node API (default: http://127.0.0.1:8545)
#    TIMEOUT_SECS   — curl timeout in seconds (default: 5)
#    SUPPORT_BOT_TOKEN      — Telegram bot token for alerts (optional)
#    SUPPORT_ADMIN_CHAT_ID  — Telegram chat ID for alerts (optional)
# =============================================================================
set -euo pipefail

NODE_API_URL="${NODE_API_URL:-http://127.0.0.1:8545}"
TIMEOUT_SECS="${TIMEOUT_SECS:-5}"
STATUS_URL="${NODE_API_URL}/api/v1/status"

# State files written every run so the Admin Panel can show watchdog status
STATE_DIR="/var/lib/aperod"
LAST_CHECK_FILE="${STATE_DIR}/watchdog-last-check"
LAST_RESTART_FILE="${STATE_DIR}/watchdog-last-restart"
RESTART_COUNT_FILE="${STATE_DIR}/watchdog-restarts"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [watchdog] $*"; }

send_telegram() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      >/dev/null 2>&1 || true
  fi
}

# Record the current UTC timestamp to a file (creates state dir if needed)
write_timestamp() {
  local file="$1"
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  date -u '+%Y-%m-%dT%H:%M:%SZ' > "${file}" || true
}

# Atomically increment the restart counter file
increment_restart_count() {
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  local count=0
  if [[ -f "${RESTART_COUNT_FILE}" ]]; then
    count=$(cat "${RESTART_COUNT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
  fi
  echo $(( count + 1 )) > "${RESTART_COUNT_FILE}" || true
}

# ---------------------------------------------------------------------------
# Record this check run (always — so Admin Panel can detect liveness)
# ---------------------------------------------------------------------------
write_timestamp "${LAST_CHECK_FILE}"

# ---------------------------------------------------------------------------
# Health check
# ---------------------------------------------------------------------------
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
  --max-time "${TIMEOUT_SECS}" \
  "${STATUS_URL}" 2>/dev/null || echo "000")

if [[ "$HTTP_CODE" == "200" ]]; then
  log "OK (HTTP ${HTTP_CODE})"
  exit 0
fi

# ---------------------------------------------------------------------------
# Probe failed — restart the node
# ---------------------------------------------------------------------------
log "FAIL: ${STATUS_URL} returned HTTP ${HTTP_CODE} (timeout=${TIMEOUT_SECS}s) — restarting aperod-node"

send_telegram "🔄 <b>aperod-node watchdog</b>
Server: $(hostname)
Probe: <code>${STATUS_URL}</code>
Result: HTTP ${HTTP_CODE}
Action: <code>systemctl restart aperod-node</code>"

systemctl restart aperod-node

# Record the restart event
write_timestamp "${LAST_RESTART_FILE}"
increment_restart_count

log "aperod-node restarted"
exit 0
