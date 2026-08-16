#!/usr/bin/env bash
# ============================================================
# aperod-morning-report.sh — утренний отчёт о работе систем за ночь
#
# Что анализируется:
#   1. Рестарты и падения сервисов (aperod-node, aperod-api)
#   2. OOM-kill (ядро убило процесс из-за нехватки памяти)
#   3. Ошибки Go-ноды: паники, circuit breaker, потеря пиров
#   4. Ошибки API-сервера: 5xx, DB errors, node unreachable
#   5. Ночная проверка (night-check.log) — если запускалась
#   6. Бэкап-лог — успех / пропуски / ошибки
#   7. Аномалии высоты блоков (стагнация)
#   8. Дисковые события (ENOSPC, inode)
#
# Вывод: хронологическая лента событий + сводка.
# Опционально отправляет отчёт в Telegram.
#
# УСТАНОВКА:
#   sudo cp blockchain/deploy/aperod-morning-report.sh /usr/local/bin/
#   sudo chmod +x /usr/local/bin/aperod-morning-report.sh
#
# CRON (каждый день в 07:00 — читать после ночи):
#   echo "0 7 * * * root /usr/local/bin/aperod-morning-report.sh" \
#     | sudo tee /etc/cron.d/aperod-morning-report
#
# РУЧНОЙ ЗАПУСК (последние 12 часов):
#   sudo bash /usr/local/bin/aperod-morning-report.sh
#
# РУЧНОЙ ЗАПУСК за произвольный период:
#   sudo SINCE="2026-08-15 21:00" UNTIL="2026-08-16 07:00" \
#     bash /usr/local/bin/aperod-morning-report.sh
#
# ТОЛЬКО ВЫВОД В ТЕРМИНАЛ (без Telegram):
#   sudo NO_TELEGRAM=1 bash /usr/local/bin/aperod-morning-report.sh
# ============================================================
set -uo pipefail

# ── Период анализа ───────────────────────────────────────────
# По умолчанию: с 21:00 вчера до сейчас (покрывает всю ночь)
_DEFAULT_SINCE=$(date -d "yesterday 21:00" "+%Y-%m-%d %H:%M" 2>/dev/null \
  || date -v-1d -v21H -v0M -v0S "+%Y-%m-%d %H:%M" 2>/dev/null \
  || date "+%Y-%m-%d 21:00" --date="yesterday")
SINCE="${SINCE:-$_DEFAULT_SINCE}"
UNTIL="${UNTIL:-$(date "+%Y-%m-%d %H:%M")}"
HOURS_BACK="${HOURS_BACK:-10}"   # запасной вариант для journalctl --since

NIGHT_CHECK_LOG="${NIGHT_CHECK_LOG:-/var/log/aperod/night-check.log}"
BACKUP_HISTORY="${BACKUP_HISTORY:-/opt/aperod/data/backup-history.jsonl}"
NODE_API="${NODE_API:-http://localhost:8545}"
API_BASE="${API_BASE:-http://localhost:3001}"
LOG_DIR="${LOG_DIR:-/var/log/aperod}"
NO_TELEGRAM="${NO_TELEGRAM:-}"

# ── Загружаем Telegram credentials ──────────────────────────
for _ENV_FILE in /etc/aperod/api.env /etc/aperod/backup-secrets.env; do
  [ -f "$_ENV_FILE" ] || continue
  while IFS='=' read -r _K _V; do
    [[ "$_K" =~ ^# ]] && continue
    [[ -z "$_K" ]] && continue
    case "$_K" in
      TELEGRAM_BOT_TOKEN)     export TELEGRAM_BOT_TOKEN="${TELEGRAM_BOT_TOKEN:-${_V}}" ;;
      ADMIN_TELEGRAM_CHAT_ID) export ADMIN_TELEGRAM_CHAT_ID="${ADMIN_TELEGRAM_CHAT_ID:-${_V}}" ;;
    esac
  done < "$_ENV_FILE"
done

HOSTNAME_SHORT=$(hostname -s 2>/dev/null || echo "server")

# ════════════════════════════════════════════════════════════
# СБОР СОБЫТИЙ — каждое событие: "TIMESTAMP|УРОВЕНЬ|ИСТОЧНИК|СООБЩЕНИЕ"
# ════════════════════════════════════════════════════════════
declare -a EVENTS=()

# Добавить событие
_event() {
  local ts="$1" lvl="$2" src="$3" msg="$4"
  EVENTS+=("${ts}|${lvl}|${src}|${msg}")
}

# Безопасный journalctl (не падает если нет прав / юнита)
_jctl() {
  journalctl --no-pager --output=short-iso \
    --since "$SINCE" --until "$UNTIL" \
    "$@" 2>/dev/null || true
}

# ── 1. Рестарты и падения сервисов ──────────────────────────
for _SVC in aperod-node aperod-api; do
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
    if echo "$line" | grep -qiE "Started|start|активирован"; then
      _event "$_ts" "RESTART" "$_SVC" "▶ Сервис запущен"
    elif echo "$line" | grep -qiE "Stopped|stop|деактивирован|killed|SIGKILL|SIGTERM"; then
      _event "$_ts" "CRASH" "$_SVC" "⛔ Сервис остановлен/убит"
    elif echo "$line" | grep -qiE "failed|Failed|провал|ошибка активации"; then
      _event "$_ts" "CRASH" "$_SVC" "💥 Сервис упал (activation failed)"
    fi
  done < <(_jctl -u "$_SVC" | grep -iE "Started|Stopped|killed|failed|start|stop|SIGKILL|SIGTERM|деактив|активир|провал" || true)
done

# ── 2. OOM-kill (ядро убивает процесс) ──────────────────────
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  _proc=$(echo "$line" | grep -oP "(?<=killed process )[0-9]+ \(\S+\)" || \
          echo "$line" | grep -oP "(?<=oom_reaper: reaped process )[0-9]+ \(\S+\)" || \
          echo "aperod-node")
  _event "$_ts" "OOM" "kernel" "☠ OOM-kill: $_proc"
done < <(_jctl -k | grep -iE "oom.kill|oom_reaper|Out of memory" || true)

# ── 3. Паники Go-ноды ───────────────────────────────────────
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  _event "$_ts" "CRASH" "aperod-node" "🔥 Паника Go: $(echo "$line" | grep -oP 'panic:.*' | head -c 80 || echo "$line" | tail -c 80)"
done < <(_jctl -u aperod-node | grep -iE "panic:|goroutine [0-9]+ \[" || true)

# ── 4. Circuit breaker (API) ────────────────────────────────
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  if echo "$line" | grep -qiE "circuit breaker.*open|breaker open"; then
    _event "$_ts" "ERROR" "aperod-api" "🔴 Circuit breaker ОТКРЫТ — нода недоступна"
  elif echo "$line" | grep -qiE "circuit breaker.*clos|breaker clos|circuit.*recover"; then
    _event "$_ts" "RECOVERY" "aperod-api" "🟢 Circuit breaker ЗАКРЫТ — нода восстановилась"
  fi
done < <(_jctl -u aperod-api | grep -iE "circuit breaker" || true)

# ── 5. Потеря пиров / бан ───────────────────────────────────
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  if echo "$line" | grep -qiE "peer.*banned|banned peer|auto.ban"; then
    _ip=$(echo "$line" | grep -oP '\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}' | head -1 || echo "?")
    _event "$_ts" "WARN" "aperod-node" "🚫 Пир забанен: $_ip"
  elif echo "$line" | grep -qiE "peer.*disconnect|disconnect.*peer|0 peers|no peers"; then
    _event "$_ts" "WARN" "aperod-node" "📡 Разрыв соединения с пиром"
  fi
done < <(_jctl -u aperod-node | grep -iE "banned|disconnect|0 peers|no peers" || true)

# ── 6. DB-ошибки (API) ──────────────────────────────────────
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  _short=$(echo "$line" | grep -oP 'error:.*' | head -c 80 || echo "$line" | tail -c 80)
  _event "$_ts" "ERROR" "aperod-api" "🗄 DB ошибка: $_short"
done < <(_jctl -u aperod-api | grep -iE "DB error|database.*error|connection.*refused.*postgres|ECONNREFUSED.*5432" || true)

# ── 7. Disk full / inode ────────────────────────────────────
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  _event "$_ts" "CRASH" "disk" "💾 Диск переполнен / нет инодов"
done < <(_jctl | grep -iE "No space left on device|ENOSPC|no inode" || true)

# ── 8. Ночная проверка (night-check.log) ────────────────────
NIGHT_CHECK_SECTION=""
if [ -f "$NIGHT_CHECK_LOG" ]; then
  # Ищем запуски ночной проверки за период
  while IFS= read -r line; do
    if [[ "$line" =~ \[([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}) ]]; then
      _ts="${BASH_REMATCH[1]/T/ }"
      if [[ "$line" =~ "aperod-night-check.sh запущен" ]]; then
        _event "$_ts" "INFO" "night-check" "🌙 Ночная проверка запущена"
      elif [[ "$line" =~ "Ошибок: 0, предупреждений: 0" ]]; then
        _event "$_ts" "OK" "night-check" "✅ Ночная проверка: всё в порядке"
      elif [[ "$line" =~ "Ошибок:" ]]; then
        _summary=$(echo "$line" | grep -oP 'Ошибок:.*' || echo "$line")
        _event "$_ts" "WARN" "night-check" "⚠️ Ночная проверка: $_summary"
      fi
    fi
  done < <(grep -E "^\[20[0-9]{2}" "$NIGHT_CHECK_LOG" 2>/dev/null || true)

  # Последний ночной отчёт (полный блок)
  NIGHT_CHECK_SECTION=$(awk '
    /aperod-night-check.sh запущен/ { found=1; block="" }
    found { block = block "\n" $0 }
    /Отчёт отправлен|Telegram.*отключён/ { if(found) last=block; found=0 }
    END { print last }
  ' "$NIGHT_CHECK_LOG" 2>/dev/null | tail -40 || true)
fi

# ── 9. Бэкап-события ────────────────────────────────────────
if [ -f "$BACKUP_HISTORY" ]; then
  while IFS= read -r bjson; do
    [[ -z "$bjson" ]] && continue
    _bts=$(echo "$bjson" | python3 -c "
import sys,json
try:
  d=json.load(sys.stdin)
  t=d.get('timestamp','')[:16].replace('T',' ')
  print(t)
except: pass" 2>/dev/null || true)
    [[ -z "$_bts" ]] && continue
    # Фильтруем по периоду (грубо — по строке даты)
    _bdate=$(echo "$_bts" | cut -c1-10)
    _since_date=$(echo "$SINCE" | cut -c1-10)
    [[ "$_bdate" < "$_since_date" ]] && continue
    _status=$(echo "$bjson" | python3 -c "
import sys,json
try:
  d=json.load(sys.stdin)
  print(d.get('status','?'))
except: print('?')" 2>/dev/null || echo "?")
    _node=$(echo "$bjson" | python3 -c "
import sys,json
try:
  d=json.load(sys.stdin)
  print(d.get('node','?'))
except: print('?')" 2>/dev/null || echo "?")
    case "$_status" in
      success) _event "$_bts" "OK"   "backup" "💾 Бэкап [$_node]: ✅ успешно" ;;
      skipped) _event "$_bts" "INFO" "backup" "💾 Бэкап [$_node]: ⏭ пропущен" ;;
      *)       _event "$_bts" "ERROR" "backup" "💾 Бэкап [$_node]: ❌ ошибка ($_status)" ;;
    esac
  done < "$BACKUP_HISTORY"
fi

# ── 10. Стагнация высоты (проверяем journalctl на повторяющийся height) ──
_LAST_H=""
_STAG_TS=""
_STAG_COUNT=0
while IFS= read -r line; do
  _h=$(echo "$line" | grep -oP '"height":\s*\K[0-9]+' || echo "$line" | grep -oP 'height=\K[0-9]+' || true)
  [[ -z "$_h" ]] && continue
  _ts=$(echo "$line" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16)
  if [[ "$_h" == "$_LAST_H" ]]; then
    _STAG_COUNT=$(( _STAG_COUNT + 1 ))
    if [[ "$_STAG_COUNT" -eq 5 ]]; then
      _event "$_ts" "WARN" "aperod-node" "📏 Высота блоков не растёт: $_h (возможная стагнация)"
    fi
  else
    _STAG_COUNT=0
    _LAST_H="$_h"
    _STAG_TS="$_ts"
  fi
done < <(_jctl -u aperod-node | grep -oE '"height":[0-9]+|height=[0-9]+' || true)

# ════════════════════════════════════════════════════════════
# СОРТИРОВКА И ПОДСЧЁТ
# ════════════════════════════════════════════════════════════
# Сортируем по временной метке
IFS=$'\n' SORTED_EVENTS=($(printf '%s\n' "${EVENTS[@]}" | sort 2>/dev/null || true))
unset IFS

TOTAL_CRASH=0
TOTAL_OOM=0
TOTAL_ERROR=0
TOTAL_WARN=0
TOTAL_RESTART=0
TOTAL_RECOVERY=0

for ev in "${SORTED_EVENTS[@]:-}"; do
  [[ -z "$ev" ]] && continue
  _lvl=$(echo "$ev" | cut -d'|' -f2)
  case "$_lvl" in
    CRASH)    TOTAL_CRASH=$(( TOTAL_CRASH + 1 )) ;;
    OOM)      TOTAL_OOM=$(( TOTAL_OOM + 1 )) ;;
    ERROR)    TOTAL_ERROR=$(( TOTAL_ERROR + 1 )) ;;
    WARN)     TOTAL_WARN=$(( TOTAL_WARN + 1 )) ;;
    RESTART)  TOTAL_RESTART=$(( TOTAL_RESTART + 1 )) ;;
    RECOVERY) TOTAL_RECOVERY=$(( TOTAL_RECOVERY + 1 )) ;;
  esac
done

# ════════════════════════════════════════════════════════════
# ТЕКУЩЕЕ СОСТОЯНИЕ (прямо сейчас)
# ════════════════════════════════════════════════════════════
_curl() { curl -sf --max-time 6 "$@" 2>/dev/null || true; }

_NODE_STATUS=$(systemctl is-active aperod-node 2>/dev/null || echo "unknown")
_API_STATUS=$(systemctl is-active aperod-api  2>/dev/null || echo "unknown")

_node_json=$(_curl "$NODE_API/api/v1/status" || true)
_CUR_HEIGHT=$(echo "$_node_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('tip_height',d.get('height','?')))" 2>/dev/null || echo "?")
_CUR_PEERS=$(echo  "$_node_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('peers',d.get('peer_count','?')))" 2>/dev/null || echo "?")
_CUR_SYNC=$(echo   "$_node_json" | python3 -c "import sys,json; d=json.load(sys.stdin); print('синхр.' if not d.get('syncing',False) else 'синхронизируется')" 2>/dev/null || echo "?")

_api_health=$(_curl "$API_BASE/api/health" || true)
_CB=$(echo "$_api_health" | python3 -c "import sys,json; d=json.load(sys.stdin); print('закрыт' if d.get('circuit_breaker')=='closed' else d.get('circuit_breaker','?'))" 2>/dev/null || echo "?")

_DISK_INFO=$(df -h /opt/aperod 2>/dev/null | awk 'NR==2{print $5 " (" $4 " свободно)"}' || echo "?")
_RAM_FREE=$(free -m 2>/dev/null | awk '/^Mem:/{print $7 " МБ свободно"}' || echo "?")

# ════════════════════════════════════════════════════════════
# ФОРМАТИРОВАНИЕ ОТЧЁТА
# ════════════════════════════════════════════════════════════
_SEP="══════════════════════════════════════════"

# Цвета для терминала
_RED=$'\033[0;31m'
_YEL=$'\033[0;33m'
_GRN=$'\033[0;32m'
_BLU=$'\033[0;34m'
_RST=$'\033[0m'

_status_color() {
  case "$1" in
    CRASH|OOM) echo "${_RED}" ;;
    ERROR)     echo "${_RED}" ;;
    WARN)      echo "${_YEL}" ;;
    RESTART)   echo "${_BLU}" ;;
    RECOVERY|OK) echo "${_GRN}" ;;
    *)         echo "" ;;
  esac
}

# ── Вывод в терминал ────────────────────────────────────────
echo ""
echo "${_BLU}${_SEP}${_RST}"
echo "${_BLU}  🌅 УТРЕННИЙ ОТЧЁТ Aperod @ ${HOSTNAME_SHORT}${_RST}"
echo "${_BLU}${_SEP}${_RST}"
echo "  Период:  ${SINCE}  →  ${UNTIL}"
echo ""

# Сводка
echo "  ──── СВОДКА ────────────────────────────────"
if [[ "$TOTAL_CRASH" -gt 0 || "$TOTAL_OOM" -gt 0 || "$TOTAL_ERROR" -gt 0 ]]; then
  echo "  ${_RED}Падений сервисов: ${TOTAL_CRASH}${_RST}   OOM-убийств: ${_RED}${TOTAL_OOM}${_RST}   Ошибок: ${_RED}${TOTAL_ERROR}${_RST}"
else
  echo "  ${_GRN}Падений: 0   OOM: 0   Ошибок: 0${_RST}"
fi
echo "  Рестартов: ${TOTAL_RESTART}   Предупреждений: ${_YEL}${TOTAL_WARN}${_RST}   Восстановлений: ${_GRN}${TOTAL_RECOVERY}${_RST}"
echo ""

# Текущее состояние
echo "  ──── СЕЙЧАС ────────────────────────────────"
_nd_color=$( [[ "$_NODE_STATUS" == "active" ]] && echo "${_GRN}" || echo "${_RED}" )
_api_color=$( [[ "$_API_STATUS" == "active" ]] && echo "${_GRN}" || echo "${_RED}" )
echo "  Нода:    ${_nd_color}${_NODE_STATUS}${_RST}   Высота: ${_CUR_HEIGHT}   Пиры: ${_CUR_PEERS}   ${_CUR_SYNC}"
echo "  API:     ${_api_color}${_API_STATUS}${_RST}   Circuit breaker: ${_CB}"
echo "  Диск:    ${_DISK_INFO}"
echo "  RAM:     ${_RAM_FREE}"
echo ""

# Хронология
if [[ "${#SORTED_EVENTS[@]}" -eq 0 ]]; then
  echo "  ${_GRN}✅ За ночь не зафиксировано ни одного сбоя или предупреждения.${_RST}"
else
  echo "  ──── ХРОНОЛОГИЯ СОБЫТИЙ ────────────────────"
  for ev in "${SORTED_EVENTS[@]:-}"; do
    [[ -z "$ev" ]] && continue
    _evts=$(echo "$ev" | cut -d'|' -f1)
    _elvl=$(echo "$ev" | cut -d'|' -f2)
    _esrc=$(echo "$ev" | cut -d'|' -f3)
    _emsg=$(echo "$ev" | cut -d'|' -f4)
    _col=$(_status_color "$_elvl")
    printf "  %s  ${_col}%-8s${_RST}  %-12s  %s\n" "$_evts" "$_elvl" "$_esrc" "$_emsg"
  done
fi

# Ночная проверка
if [[ -n "$NIGHT_CHECK_SECTION" ]]; then
  echo ""
  echo "  ──── НОЧНАЯ ПРОВЕРКА (night-check) ─────────"
  echo "$NIGHT_CHECK_SECTION" | grep -v "^$" | sed 's/^/  /' | head -30
fi

echo ""
echo "${_BLU}${_SEP}${_RST}"
echo ""

# ════════════════════════════════════════════════════════════
# TELEGRAM — отправка (если настроен и не отключён)
# ════════════════════════════════════════════════════════════
if [[ -z "$NO_TELEGRAM" ]] \
   && [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]] \
   && [[ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]]; then

  # Статус-иконки для Telegram
  _svc_icon() { [[ "$1" == "active" ]] && echo "🟢" || echo "🔴"; }

  _TG_HEADER="🌅 <b>Утренний отчёт @ ${HOSTNAME_SHORT}</b>
📅 ${SINCE} → ${UNTIL}"

  # Сводка
  if [[ "$TOTAL_CRASH" -gt 0 || "$TOTAL_OOM" -gt 0 || "$TOTAL_ERROR" -gt 0 ]]; then
    _TG_SUMMARY="
⚠️ <b>Ночь была с инцидентами</b>
• Падений: <b>${TOTAL_CRASH}</b>
• OOM-убийств: <b>${TOTAL_OOM}</b>
• Ошибок: <b>${TOTAL_ERROR}</b>
• Предупреждений: ${TOTAL_WARN}
• Рестартов: ${TOTAL_RESTART}
• Восстановлений: ${TOTAL_RECOVERY}"
  else
    _TG_SUMMARY="
✅ <b>Ночь прошла без инцидентов</b>
• Рестартов: ${TOTAL_RESTART}
• Предупреждений: ${TOTAL_WARN}"
  fi

  # Текущее состояние
  _nd_ic=$(_svc_icon "$_NODE_STATUS")
  _api_ic=$(_svc_icon "$_API_STATUS")
  _TG_NOW="
<b>Сейчас:</b>
${_nd_ic} Нода: <code>${_NODE_STATUS}</code> — высота ${_CUR_HEIGHT}, пиры ${_CUR_PEERS}, ${_CUR_SYNC}
${_api_ic} API: <code>${_API_STATUS}</code> — circuit breaker: ${_CB}
💿 Диск: ${_DISK_INFO}
🧠 RAM: ${_RAM_FREE}"

  # Хронология (ограничиваем 20 строками для Telegram)
  if [[ "${#SORTED_EVENTS[@]}" -gt 0 ]]; then
    _TG_EVENTS=$'\n<b>Хронология (топ событий):</b>\n<pre>'
    _ev_count=0
    for ev in "${SORTED_EVENTS[@]:-}"; do
      [[ -z "$ev" ]] && continue
      _evts=$(echo "$ev" | cut -d'|' -f1)
      _elvl=$(echo "$ev" | cut -d'|' -f2)
      _esrc=$(echo "$ev" | cut -d'|' -f3)
      _emsg=$(echo "$ev" | cut -d'|' -f4 | head -c 60)
      printf -v _ev_line "%-5s %-7s %-11s %s" "${_evts:11:5}" "$_elvl" "$_esrc" "$_emsg"
      _TG_EVENTS+="${_ev_line}"$'\n'
      _ev_count=$(( _ev_count + 1 ))
      [[ "$_ev_count" -ge 20 ]] && { _TG_EVENTS+="... (ещё $(( ${#SORTED_EVENTS[@]} - 20 )) событий)"$'\n'; break; }
    done
    _TG_EVENTS+='</pre>'
  else
    _TG_EVENTS=""
  fi

  _TG_MSG="${_TG_HEADER}${_TG_SUMMARY}${_TG_NOW}${_TG_EVENTS}"

  curl -sf --max-time 15 \
    "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
    -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
    -d "parse_mode=HTML" \
    --data-urlencode "text=${_TG_MSG}" \
    > /dev/null 2>&1 || true

  echo "📨 Отчёт отправлен в Telegram (chat_id: ${ADMIN_TELEGRAM_CHAT_ID})"
else
  echo "ℹ️  Telegram не настроен или отключён (NO_TELEGRAM=1)"
fi

# Лог
mkdir -p "$LOG_DIR" 2>/dev/null || true
echo "[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] aperod-morning-report: crashes=${TOTAL_CRASH} oom=${TOTAL_OOM} errors=${TOTAL_ERROR} warns=${TOTAL_WARN} restarts=${TOTAL_RESTART}" \
  >> "$LOG_DIR/morning-report.log" 2>/dev/null || true
