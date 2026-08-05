#!/usr/bin/env bash
# =============================================================================
#  aperod-node-sched-restart.sh — graceful planned restart of aperod-node
#
#  Invoked by aperod-node-sched-restart.timer (default: every 3 hours).
#  Purpose: prevent RAM exhaustion from the Go heap leak (~1.3 GB/h).
#
#  Sequence:
#    1. Send "about to restart" Telegram notification
#    2. systemctl restart aperod-node  (SIGTERM → snapshot saved → start)
#    3. Wait up to 90 s for the API to return HTTP 200
#    4. Send "back online" or "did not recover" Telegram notification
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

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [sched-restart] $*"; }

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

increment_restart_count() {
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  local count=0
  if [[ -f "${SCHED_RESTART_COUNT_FILE}" ]]; then
    count=$(cat "${SCHED_RESTART_COUNT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
  fi
  echo $(( count + 1 )) > "${SCHED_RESTART_COUNT_FILE}" || true
}

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
# Step 2 — Graceful restart
# systemd sends SIGTERM; aperod-node saves snapshot; TimeoutStopSec=900 guard.
# ---------------------------------------------------------------------------
log "Запускаем systemctl restart aperod-node…"
systemctl restart aperod-node
log "systemctl restart вернул управление"

# Record state
mkdir -p "${STATE_DIR}" 2>/dev/null || true
date -u '+%Y-%m-%dT%H:%M:%SZ' > "${SCHED_RESTART_LAST_FILE}" || true
increment_restart_count

# ---------------------------------------------------------------------------
# Step 3 — Wait for node to come back (up to 90 s, polling every 3 s)
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
# Step 4 — Post-restart notification
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
