#!/usr/bin/env bash
# ============================================================
# aperod-night-check.sh — ночная проверка всех систем Aperod
#
# Что проверяется:
#   1. Systemd-сервисы (aperod-node, aperod-api)
#   2. Go-нода: высота, синхронизация, кол-во пиров, RSS
#   3. API-сервер: health-endpoint, circuit breaker
#   4. Бэкап: время последнего успешного бэкапа
#   5. Диск: свободное место на /opt/aperod
#   6. Оперативная память / swap
#   7. Подключённые пиры: есть ли с HIGH LAG (>10 000 блоков)
#   8. Скрипт бэкапа: SHA совпадает с /usr/local/bin/
#
# Результат отправляется в Telegram и пишется в лог.
#
# УСТАНОВКА:
#   sudo cp blockchain/deploy/aperod-night-check.sh /usr/local/bin/
#   sudo chmod +x /usr/local/bin/aperod-night-check.sh
#
# CRON (ежедневно в 02:00):
#   echo "0 2 * * * root /usr/local/bin/aperod-night-check.sh" \
#     | sudo tee /etc/cron.d/aperod-night-check
#
# TELEGRAM:
#   Скрипт читает TELEGRAM_BOT_TOKEN и ADMIN_TELEGRAM_CHAT_ID из:
#     1. /etc/aperod/api.env
#     2. /etc/aperod/backup-secrets.env
#     3. переменных окружения (если запущен с ними вручную)
#
# РУЧНОЙ ЗАПУСК:
#   sudo bash /usr/local/bin/aperod-night-check.sh
# ============================================================
set -euo pipefail

# ── Конфиг ──────────────────────────────────────────────────
NODE_API="${NODE_API:-http://localhost:8545}"
API_BASE="${API_BASE:-http://localhost:3001}"
LOG_FILE="${LOG_FILE:-/var/log/aperod/night-check.log}"
NODE_USER="${NODE_USER:-aperod}"
BACKUP_HISTORY="${BACKUP_HISTORY:-/opt/aperod/data/backup-history.jsonl}"
LAG_WARN_BLOCKS="${LAG_WARN_BLOCKS:-10000}"   # блоков отставания = WARNING
DISK_WARN_PCT="${DISK_WARN_PCT:-85}"           # % заполненности диска = WARNING
DISK_CRIT_PCT="${DISK_CRIT_PCT:-95}"           # % заполненности диска = CRITICAL
RAM_WARN_MB="${RAM_WARN_MB:-500}"              # свободная RAM < X МБ = WARNING
BACKUP_STALE_H="${BACKUP_STALE_H:-26}"        # бэкап не запускался > X часов = WARNING

# ── Загружаем Telegram credentials ──────────────────────────
for _ENV_FILE in /etc/aperod/api.env /etc/aperod/backup-secrets.env; do
  [ -f "$_ENV_FILE" ] || continue
  while IFS='=' read -r _K _V; do
    [[ "$_K" =~ ^# ]] && continue
    [[ -z "$_K" ]] && continue
    case "$_K" in
      TELEGRAM_BOT_TOKEN)    export TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-${_V}}" ;;
      ADMIN_TELEGRAM_CHAT_ID) export ADMIN_TELEGRAM_CHAT_ID="${ADMIN_TELEGRAM_CHAT_ID:-${_V}}" ;;
    esac
  done < "$_ENV_FILE"
done

# ── Переменные состояния ─────────────────────────────────────
CHECKS=()          # массив строк "EMOJI Описание: Значение"
ERRORS=0
WARNINGS=0
TS_ISO=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
HOSTNAME_SHORT=$(hostname -s 2>/dev/null || echo "server")

# ── Утилиты ──────────────────────────────────────────────────
_ok()   { CHECKS+=("✅ $*"); }
_warn() { CHECKS+=("⚠️ $*"); (( WARNINGS++ )) || true; }
_fail() { CHECKS+=("❌ $*"); (( ERRORS++ )) || true; }
_info() { CHECKS+=("ℹ️ $*"); }

_curl() {
  # Тихий curl с таймаутом; возвращает тело или пустую строку
  curl -sf --max-time 8 "$@" 2>/dev/null || true
}

# Убеждаемся, что директория лога существует
mkdir -p "$(dirname "$LOG_FILE")" 2>/dev/null || true

echo "[$TS_ISO] aperod-night-check.sh запущен" >> "$LOG_FILE" 2>/dev/null || true

# ════════════════════════════════════════════════════════════
# 1. Systemd-сервисы
# ════════════════════════════════════════════════════════════
for _SVC in aperod-node aperod-api; do
  _STATE=$(systemctl is-active "$_SVC" 2>/dev/null || echo "unknown")
  case "$_STATE" in
    active)   _ok  "Сервис $_SVC: active (running)" ;;
    inactive) _fail "Сервис $_SVC: inactive — не запущен" ;;
    failed)   _fail "Сервис $_SVC: FAILED — $(systemctl status "$_SVC" --no-pager -l 2>/dev/null | tail -3 | tr '\n' ' ')" ;;
    *)        _warn "Сервис $_SVC: $_STATE" ;;
  esac
done

# ════════════════════════════════════════════════════════════
# 2. Go-нода
# ════════════════════════════════════════════════════════════
_NODE_STATUS=$(_curl "${NODE_API}/api/v1/status" || true)

if [ -z "$_NODE_STATUS" ]; then
  _fail "Go-нода: не отвечает на ${NODE_API}/api/v1/status"
else
  _HEIGHT=$(echo "$_NODE_STATUS" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('tip_height', d.get('height', '?')))" 2>/dev/null || echo "?")
  _SYNCING=$(echo "$_NODE_STATUS" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('syncing', False))" 2>/dev/null || echo "?")
  _PEERS=$(echo "$_NODE_STATUS" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('peer_count', '?'))" 2>/dev/null || echo "?")
  _RSS_MB=$(echo "$_NODE_STATUS" | python3 -c "
import json, sys
d = json.load(sys.stdin)
m = d.get('memory', {})
rss = m.get('rss_bytes', m.get('rss', 0))
print(int(rss) // 1048576 if str(rss).isdigit() else '?')
" 2>/dev/null || echo "?")

  if [ "$_SYNCING" = "True" ] || [ "$_SYNCING" = "true" ]; then
    _SYNC_HEIGHT=$(echo "$_NODE_STATUS" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('syncing_height','?'))" 2>/dev/null || echo "?")
    _warn "Go-нода: СИНХРОНИЗИРУЕТСЯ (наш tip=${_HEIGHT}, цепь=${_SYNC_HEIGHT}, пиры=${_PEERS})"
  else
    _ok "Go-нода: высота ${_HEIGHT}, пиры ${_PEERS}, RSS ${_RSS_MB} МБ"
  fi

  # Пиры = 0 — критично
  if [ "$_PEERS" = "0" ] || [ "$_PEERS" = "?" ]; then
    _fail "Go-нода: 0 подключённых пиров — нода изолирована от сети"
  fi

  # Память > 7 ГБ — предупреждение
  if [[ "$_RSS_MB" =~ ^[0-9]+$ ]] && [ "$_RSS_MB" -gt 7168 ]; then
    _warn "Go-нода: RSS ${_RSS_MB} МБ (>7 ГБ) — возможна утечка памяти"
  fi
fi

# ════════════════════════════════════════════════════════════
# 3. API-сервер
# ════════════════════════════════════════════════════════════
_API_HEALTH=$(_curl "${API_BASE}/api/health" || true)
if [ -z "$_API_HEALTH" ]; then
  _fail "API-сервер: не отвечает на ${API_BASE}/api/health"
else
  _API_STATUS=$(echo "$_API_HEALTH" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('status','?'))" 2>/dev/null || echo "ok")
  _CB_OPEN=$(echo "$_API_HEALTH" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('circuit_breaker_open', False))" 2>/dev/null || echo "false")
  if [ "$_CB_OPEN" = "True" ] || [ "$_CB_OPEN" = "true" ]; then
    _warn "API-сервер: circuit breaker OPEN — Go-нода считается недоступной"
  else
    _ok "API-сервер: отвечает (status=${_API_STATUS}, CB=closed)"
  fi
fi

# ════════════════════════════════════════════════════════════
# 4. Бэкап — время последнего успешного бэкапа
# ════════════════════════════════════════════════════════════
_BACKUP_OK=0
if [ -f "$BACKUP_HISTORY" ]; then
  _LAST_OK=$(grep '"status":"ok"' "$BACKUP_HISTORY" 2>/dev/null | tail -1 || true)
  if [ -n "$_LAST_OK" ]; then
    _LAST_TS=$(echo "$_LAST_OK" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('ts','?'))" 2>/dev/null || echo "?")
    _LAST_EPOCH=$(date -d "$_LAST_TS" +%s 2>/dev/null || echo "0")
    _NOW_EPOCH=$(date +%s)
    _AGE_H=$(( (_NOW_EPOCH - _LAST_EPOCH) / 3600 ))
    _LAST_MB=$(echo "$_LAST_OK" | python3 -c "import json,sys; d=json.load(sys.stdin); print(round(d.get('sizeBytes',0)/1048576,1))" 2>/dev/null || echo "?")
    if [ "$_AGE_H" -gt "$BACKUP_STALE_H" ]; then
      _warn "Бэкап: последний успешный ${_AGE_H}ч назад (${_LAST_TS}, ${_LAST_MB} МБ) — ожидается каждые 12ч"
    else
      _ok "Бэкап: успешен ${_AGE_H}ч назад (${_LAST_TS}, ${_LAST_MB} МБ)"
      _BACKUP_OK=1
    fi
  else
    _fail "Бэкап: нет ни одного успешного бэкапа в истории (${BACKUP_HISTORY})"
  fi
else
  _warn "Бэкап: история не найдена (${BACKUP_HISTORY}) — бэкап ещё не запускался?"
fi

# Дополнительно — проверить лог systemd для aperod-backup.service
if [ "$_BACKUP_OK" = "0" ]; then
  _LAST_SVC_FAIL=$(journalctl -u aperod-backup.service --since "24 hours ago" --no-pager -q 2>/dev/null \
    | grep -i "error\|fail\|exception" | tail -1 || true)
  [ -n "$_LAST_SVC_FAIL" ] && _info "Бэкап (journalctl): ${_LAST_SVC_FAIL:0:120}"
fi

# ════════════════════════════════════════════════════════════
# 5. Диск
# ════════════════════════════════════════════════════════════
_DISK_CHECK_PATH="${DISK_PATH:-/opt/aperod}"
[ -d "$_DISK_CHECK_PATH" ] || _DISK_CHECK_PATH="/"

_DISK_INFO=$(df -h "$_DISK_CHECK_PATH" 2>/dev/null | tail -1 || true)
if [ -n "$_DISK_INFO" ]; then
  _DISK_PCT=$(echo "$_DISK_INFO" | awk '{print $5}' | tr -d '%')
  _DISK_USED=$(echo "$_DISK_INFO" | awk '{print $3}')
  _DISK_AVAIL=$(echo "$_DISK_INFO" | awk '{print $4}')
  if [ "$_DISK_PCT" -ge "$DISK_CRIT_PCT" ]; then
    _fail "Диск (${_DISK_CHECK_PATH}): ${_DISK_PCT}% занято, доступно ${_DISK_AVAIL} — КРИТИЧНО"
  elif [ "$_DISK_PCT" -ge "$DISK_WARN_PCT" ]; then
    _warn "Диск (${_DISK_CHECK_PATH}): ${_DISK_PCT}% занято, доступно ${_DISK_AVAIL}"
  else
    _ok "Диск (${_DISK_CHECK_PATH}): ${_DISK_PCT}% занято, доступно ${_DISK_AVAIL}"
  fi
fi

# ════════════════════════════════════════════════════════════
# 6. Оперативная память / swap
# ════════════════════════════════════════════════════════════
_MEM_INFO=$(free -m 2>/dev/null | grep '^Mem:' || true)
if [ -n "$_MEM_INFO" ]; then
  _MEM_TOTAL=$(echo "$_MEM_INFO" | awk '{print $2}')
  _MEM_AVAIL=$(echo "$_MEM_INFO" | awk '{print $7}')
  _MEM_USED=$(( _MEM_TOTAL - _MEM_AVAIL ))
  _MEM_PCT=$(( _MEM_USED * 100 / _MEM_TOTAL ))
  if [ "$_MEM_AVAIL" -lt "$RAM_WARN_MB" ]; then
    _warn "RAM: свободно ${_MEM_AVAIL} МБ из ${_MEM_TOTAL} МБ (${_MEM_PCT}% занято) — мало"
  else
    _ok "RAM: свободно ${_MEM_AVAIL} МБ из ${_MEM_TOTAL} МБ (${_MEM_PCT}% занято)"
  fi
fi

_SWAP_INFO=$(free -m 2>/dev/null | grep '^Swap:' || true)
if [ -n "$_SWAP_INFO" ]; then
  _SWAP_TOTAL=$(echo "$_SWAP_INFO" | awk '{print $2}')
  _SWAP_USED=$(echo "$_SWAP_INFO" | awk '{print $3}')
  if [ "$_SWAP_TOTAL" -gt 0 ]; then
    _SWAP_PCT=$(( _SWAP_USED * 100 / _SWAP_TOTAL ))
    if [ "$_SWAP_PCT" -gt 50 ]; then
      _warn "Swap: ${_SWAP_USED}/${_SWAP_TOTAL} МБ (${_SWAP_PCT}%) — активно используется"
    else
      _ok "Swap: ${_SWAP_USED}/${_SWAP_TOTAL} МБ (${_SWAP_PCT}%)"
    fi
  fi
fi

# ════════════════════════════════════════════════════════════
# 7. Подключённые пиры: HIGH LAG
# ════════════════════════════════════════════════════════════
_PEERS_RESP=$(_curl "${NODE_API}/api/v1/network/peers" || true)
if [ -n "$_PEERS_RESP" ] && [ -n "$_NODE_STATUS" ]; then
  _OUR_H=$(echo "$_NODE_STATUS" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('tip_height', d.get('height', 0)))" 2>/dev/null || echo "0")
  _LAG_RESULT=$(python3 - <<PYEOF
import json, sys
peers_raw = '''${_PEERS_RESP}'''
our_h_raw = '''${_OUR_H}'''
try:
    peers = json.loads(peers_raw)
    our_h = int(our_h_raw)
    lag_threshold = ${LAG_WARN_BLOCKS}
    high_lag = []
    for p in (peers if isinstance(peers, list) else peers.get('peers', [])):
        h = p.get('height', 0)
        lag = our_h - h
        if lag > lag_threshold:
            high_lag.append(f"{p.get('addr','?')} lag={lag}")
    print(f"TOTAL:{len(peers) if isinstance(peers, list) else len(peers.get('peers',[]))}")
    if high_lag:
        print("LAG:" + " | ".join(high_lag))
except Exception as e:
    print(f"ERR:{e}")
PYEOF
  )
  _PEER_TOTAL=$(echo "$_LAG_RESULT" | grep '^TOTAL:' | cut -d: -f2)
  _LAG_LIST=$(echo "$_LAG_RESULT" | grep '^LAG:' | sed 's/^LAG://' || true)
  if [ -n "$_LAG_LIST" ]; then
    _warn "Пиры с HIGH LAG (>${LAG_WARN_BLOCKS} блоков): ${_LAG_LIST}"
    _info "Исправление: добавить IP пира в p2p.peer_whitelist (Admin Panel → Peer Health)"
  else
    [ -n "$_PEER_TOTAL" ] && _ok "Пиры: ${_PEER_TOTAL} подключено, HIGH LAG нет"
  fi
fi

# ════════════════════════════════════════════════════════════
# 8. SHA скрипта бэкапа совпадает с /usr/local/bin/
# ════════════════════════════════════════════════════════════
_INSTALLED_BACKUP="/usr/local/bin/aperod_backup.sh"
_REPO_BACKUP="$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/aperod_backup.sh"
if [ -f "$_INSTALLED_BACKUP" ] && [ -f "$_REPO_BACKUP" ]; then
  _SUM_INST=$(sha256sum "$_INSTALLED_BACKUP" | awk '{print $1}')
  _SUM_REPO=$(sha256sum "$_REPO_BACKUP"      | awk '{print $1}')
  if [ "$_SUM_INST" = "$_SUM_REPO" ]; then
    _ok "Скрипт бэкапа: совпадает с репозиторием (${_SUM_INST:0:12}…)"
  else
    _warn "Скрипт бэкапа расходится с репозиторием — нужно запустить update-node.sh"
  fi
elif [ ! -f "$_INSTALLED_BACKUP" ]; then
  _warn "Скрипт бэкапа не установлен: ${_INSTALLED_BACKUP}"
fi

# ════════════════════════════════════════════════════════════
# Формируем и отправляем Telegram-отчёт
# ════════════════════════════════════════════════════════════
_STATUS_EMOJI="✅"
[ "$WARNINGS" -gt 0 ] && _STATUS_EMOJI="⚠️"
[ "$ERRORS"   -gt 0 ] && _STATUS_EMOJI="🚨"

_TG_TEXT="${_STATUS_EMOJI} <b>Aperod ночная проверка — ${HOSTNAME_SHORT}</b>
<i>${TS_ISO}</i>
"

for _LINE in "${CHECKS[@]}"; do
  _TG_TEXT+="
${_LINE}"
done

_TG_TEXT+="

<b>Итог:</b> ошибок: ${ERRORS}, предупреждений: ${WARNINGS}"

# Лог
echo "[$TS_ISO] errors=${ERRORS} warnings=${WARNINGS}" >> "$LOG_FILE" 2>/dev/null || true
for _LINE in "${CHECKS[@]}"; do
  echo "  $_LINE" >> "$LOG_FILE" 2>/dev/null || true
done

# Вывод в консоль (если запущен вручную)
echo ""
echo "══════════════════════════════════════════════"
echo " Aperod ночная проверка  ${TS_ISO}"
echo "══════════════════════════════════════════════"
for _LINE in "${CHECKS[@]}"; do
  echo "  $_LINE"
done
echo ""
echo "  Итог: ошибок=${ERRORS}, предупреждений=${WARNINGS}"
echo "══════════════════════════════════════════════"
echo ""

# Telegram
if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
  _ENCODED=$(python3 -c "
import sys, urllib.parse
print(urllib.parse.quote('''${_TG_TEXT}''', safe=''))
" 2>/dev/null || echo "")

  if [ -n "$_ENCODED" ]; then
    curl -s --max-time 20 -X POST \
      "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
      -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
      -d "text=${_ENCODED}" \
      -d "parse_mode=HTML" \
      -d "disable_web_page_preview=true" \
      >/dev/null 2>&1 && \
      echo "  ✓ Telegram-отчёт отправлен" || \
      echo "  ✗ Не удалось отправить Telegram-отчёт"
  fi
else
  echo "  ℹ Telegram не настроен (TELEGRAM_BOT_TOKEN / ADMIN_TELEGRAM_CHAT_ID не заданы)"
  echo "    → задайте в /etc/aperod/api.env или /etc/aperod/backup-secrets.env"
fi

exit 0
