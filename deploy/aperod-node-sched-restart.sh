#!/usr/bin/env bash
# =============================================================================
#  aperod-node-sched-restart.sh — graceful planned restart of aperod-node
#
#  Invoked by aperod-node-sched-restart.timer (default: every 3 hours).
#  Purpose: prevent RAM exhaustion from the Go heap leak (~1.3 GB/h).
#
#  Sequence:
#    1. Send "about to restart" Telegram notification
#    2. Pause aperod-node-watchdog.timer (prevents competing health-probe restart)
#    3. systemctl restart aperod-node  (SIGTERM → snapshot saved → start)
#    4. Wait up to 90 s for the API to return HTTP 200
#    5. Send "back online" or "did not recover" Telegram notification
#    6. Resume watchdog timer (always, via EXIT trap)
#
#  The EXIT trap guarantees the watchdog is re-enabled no matter how the script
#  exits — including early exits triggered by set -e on unexpected errors.
#
#  Optional env vars (set in /etc/aperod/sched-restart.env or in the
#  .service unit):
#    SCHED_RESTART_INTERVAL_SECS  — interval passed through for the message
#    SUPPORT_BOT_TOKEN            — Telegram bot token
#    SUPPORT_ADMIN_CHAT_ID        — Telegram chat ID
#    NODE_API_URL                 — base URL of the Go node API
#    STATE_DIR                    — override state directory (for tests)
# =============================================================================
set -euo pipefail

NODE_API_URL="${NODE_API_URL:-http://127.0.0.1:8545}"
STATE_DIR="${STATE_DIR:-/var/lib/aperod}"
SCHED_RESTART_COUNT_FILE="${STATE_DIR}/sched-restart-count"
SCHED_RESTART_LAST_FILE="${STATE_DIR}/sched-restart-last"

INTERVAL_SECS="${SCHED_RESTART_INTERVAL_SECS:-10800}"
INTERVAL_H=$(( INTERVAL_SECS / 3600 ))
HOSTNAME_LABEL="$(hostname 2>/dev/null || echo unknown)"

# Scheduled restarts are opt-in. A missing config file or a deploy that restores
# unit files must never resurrect an operator-disabled restart.
if [[ "${SCHED_RESTART_ENABLED:-false}" != "true" ]]; then
  echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [sched-restart] disabled — no restart or notification"
  exit 0
fi

# Track whether we paused the watchdog so the EXIT trap knows what to restore.
WATCHDOG_WAS_ACTIVE=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [sched-restart] $*"; }

send_telegram() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    # --connect-timeout and --max-time bound every Telegram call so a stalled
    # network path can never block the restart or disable the watchdog.
    curl -s --connect-timeout 5 --max-time 10 -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      >/dev/null 2>&1 || true
  fi
}

increment_restart_count() {
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  local count=0
  if [[ -f "${SCHED_RESTART_COUNT_FILE}" ]]; then
    count=$(cat "${SCHED_RESTART_COUNT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
  fi
  echo $(( count + 1 )) > "${SCHED_RESTART_COUNT_FILE}" || true
}

# ---------------------------------------------------------------------------
# EXIT trap — always re-enable the watchdog if we paused it
# This fires on every exit path: normal, set -e abort, signal, etc.
# ---------------------------------------------------------------------------
_cleanup() {
  if [[ "${WATCHDOG_WAS_ACTIVE}" == "1" ]]; then
    systemctl start aperod-node-watchdog.timer 2>/dev/null || true
    log "Watchdog таймер возобновлён (EXIT trap)"
  fi
}
trap '_cleanup' EXIT

# ---------------------------------------------------------------------------
# Step 1 — Pre-restart notification
# ---------------------------------------------------------------------------
log "Плановый перезапуск ноды (интервал: ${INTERVAL_H} ч) — отправляем уведомление"

send_telegram "⏱ <b>Плановый перезапуск ноды</b>
Сервер: <code>${HOSTNAME_LABEL}</code>
Причина: профилактика роста RAM (каждые ${INTERVAL_H} ч)
Действие: <code>systemctl restart aperod-node</code>
Нода вернётся через ~30 с — это плановое обслуживание."

# ---------------------------------------------------------------------------
# Step 2 — Pause the watchdog
# ---------------------------------------------------------------------------
if systemctl is-active --quiet aperod-node-watchdog.timer 2>/dev/null; then
  systemctl stop aperod-node-watchdog.timer 2>/dev/null || true
  WATCHDOG_WAS_ACTIVE=1
  log "Watchdog таймер временно остановлен"
fi

# ---------------------------------------------------------------------------
# Step 3 — Graceful restart
# systemd sends SIGTERM; aperod-node saves snapshot; TimeoutStopSec=900 guard.
# If restart fails, send an alert and exit (EXIT trap re-enables watchdog).
# ---------------------------------------------------------------------------
log "Запускаем systemctl restart aperod-node…"
if ! systemctl restart aperod-node 2>/dev/null; then
  log "ERROR: systemctl restart aperod-node вернул ошибку"
  send_telegram "❌ <b>Ошибка планового перезапуска ноды</b>
Сервер: <code>${HOSTNAME_LABEL}</code>
<code>systemctl restart aperod-node</code> завершился с ошибкой.
Проверьте: <code>journalctl -u aperod-node -n 30</code>"
  exit 1
fi
log "systemctl restart вернул управление"

# Record state
mkdir -p "${STATE_DIR}" 2>/dev/null || true
date -u '+%Y-%m-%dT%H:%M:%SZ' > "${SCHED_RESTART_LAST_FILE}" || true
increment_restart_count

# ---------------------------------------------------------------------------
# Step 4 — Wait for node to come back (up to 90 s, polling every 3 s)
# ---------------------------------------------------------------------------
log "Ожидаем запуска API ноды (до 90 с)…"
STATUS_URL="${NODE_API_URL}/api/v1/status"
RECOVERED=0
for i in $(seq 1 30); do
  sleep 3
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time 3 "${STATUS_URL}" 2>/dev/null || echo "000")
  if [[ "$HTTP_CODE" == "200" ]]; then
    log "Нода онлайн (HTTP 200) — попытка ${i} из 30 ($((i * 3)) с)"
    RECOVERED=1
    break
  fi
done

# ---------------------------------------------------------------------------
# Step 5 — Post-restart notification
# (Watchdog is re-enabled by the EXIT trap after this function returns.)
# ---------------------------------------------------------------------------
if [[ "${RECOVERED}" == "1" ]]; then
  send_telegram "✅ <b>Нода перезапущена</b>
Сервер: <code>${HOSTNAME_LABEL}</code>
Статус: онлайн · API отвечает
RAM сброшена — следующий перезапуск через ${INTERVAL_H} ч"
  log "Нода успешно восстановлена"
else
  log "WARN: Нода не ответила HTTP 200 в течение 90 с после перезапуска"
  send_telegram "⚠️ <b>Нода не ответила после планового перезапуска</b>
Сервер: <code>${HOSTNAME_LABEL}</code>
API не вернул HTTP 200 в течение 90 с.
Проверьте: <code>journalctl -u aperod-node -n 30</code>"
fi

exit 0
