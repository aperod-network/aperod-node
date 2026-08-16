#!/usr/bin/env bash
# ============================================================
# aperod-morning-report.sh — утренний отчёт о работе систем за ночь
#
# УСТАНОВКА:
#   sudo curl -fsSL \
#     "https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/aperod-morning-report.sh" \
#     -o /usr/local/bin/aperod-morning-report.sh \
#     && sudo chmod +x /usr/local/bin/aperod-morning-report.sh
#
# CRON (07:00 ежедневно):
#   echo "0 7 * * * root /usr/local/bin/aperod-morning-report.sh" \
#     | sudo tee /etc/cron.d/aperod-morning-report
#
# РУЧНОЙ ЗАПУСК:
#   sudo bash /usr/local/bin/aperod-morning-report.sh
#
# ЗА ПРОИЗВОЛЬНЫЙ ПЕРИОД:
#   sudo SINCE="2026-08-15 21:00" UNTIL="2026-08-16 07:00" \
#     bash /usr/local/bin/aperod-morning-report.sh
#
# БЕЗ TELEGRAM:
#   sudo NO_TELEGRAM=1 bash /usr/local/bin/aperod-morning-report.sh
# ============================================================

# Не используем pipefail — journalctl иногда возвращает ненулевой код
set -uo pipefail

# ── Период анализа ───────────────────────────────────────────
_SINCE_DEFAULT=$(date -d "yesterday 21:00" "+%Y-%m-%d %H:%M" 2>/dev/null \
  || date "+%Y-%m-%d %H:%M" -d "10 hours ago" 2>/dev/null \
  || date -v-10H "+%Y-%m-%d %H:%M" 2>/dev/null \
  || echo "$(date '+%Y-%m-%d') 21:00")
SINCE="${SINCE:-$_SINCE_DEFAULT}"
UNTIL="${UNTIL:-$(date "+%Y-%m-%d %H:%M")}"

NIGHT_CHECK_LOG="${NIGHT_CHECK_LOG:-/var/log/aperod/night-check.log}"
BACKUP_HISTORY="${BACKUP_HISTORY:-/opt/aperod/data/backup-history.jsonl}"
NODE_API="${NODE_API:-http://localhost:8545}"
API_BASE="${API_BASE:-http://localhost:3001}"
LOG_DIR="${LOG_DIR:-/var/log/aperod}"
NO_TELEGRAM="${NO_TELEGRAM:-}"
JCTL_TIMEOUT="${JCTL_TIMEOUT:-20}"   # секунд на каждый запрос journalctl

# ── Telegram credentials ─────────────────────────────────────
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

# ── Утилиты ──────────────────────────────────────────────────
declare -a EVENTS=()
_event() { EVENTS+=("${1}|${2}|${3}|${4}"); }

# journalctl с таймаутом и ограничением строк
_jctl() {
  timeout "$JCTL_TIMEOUT" journalctl --no-pager --output=short-iso \
    --since "$SINCE" --until "$UNTIL" \
    "$@" 2>/dev/null || true
}

# Извлечь временну́ю метку (первое поле ISO, обрезать до HH:MM)
_ts_of() { echo "$1" | awk '{print $1}' | sed 's/T/ /' | cut -c1-16; }

_curl() { curl -sf --max-time 6 "$@" 2>/dev/null || true; }

# ════════════════════════════════════════════════════════════
# 1. Рестарты / падения aperod-node и aperod-api
# ════════════════════════════════════════════════════════════
for _SVC in aperod-node aperod-api; do
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    _ts=$(_ts_of "$line")
    if echo "$line" | grep -qiE "Started|start"; then
      _event "$_ts" "RESTART" "$_SVC" "▶ Сервис запущен"
    elif echo "$line" | grep -qiE "Stopped|stop|SIGKILL|SIGTERM|killed"; then
      _event "$_ts" "CRASH"   "$_SVC" "⛔ Сервис остановлен/убит"
    elif echo "$line" | grep -qiE "failed"; then
      _event "$_ts" "CRASH"   "$_SVC" "💥 Сервис упал (failed)"
    fi
  done < <(_jctl -u "$_SVC" | grep -iE "Started|Stopped|killed|failed|SIGKILL|SIGTERM" 2>/dev/null || true)
done

# ════════════════════════════════════════════════════════════
# 2. OOM-kill (ядро убивает процесс)
# ════════════════════════════════════════════════════════════
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(_ts_of "$line")
  _proc=$(echo "$line" | grep -oP '(?<=killed process )[0-9]+ \(\S+\)' || echo "процесс")
  _event "$_ts" "OOM" "kernel" "☠ OOM-kill: $_proc"
done < <(timeout "$JCTL_TIMEOUT" journalctl --no-pager --output=short-iso -k \
           --since "$SINCE" --until "$UNTIL" 2>/dev/null \
         | grep -iE "oom.kill|oom_reaper|Out of memory" || true)

# ════════════════════════════════════════════════════════════
# 3. Паники Go-ноды
# ════════════════════════════════════════════════════════════
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(_ts_of "$line")
  _txt=$(echo "$line" | grep -oP 'panic:.*' | head -c 70 || echo "$line" | tail -c 70)
  _event "$_ts" "CRASH" "aperod-node" "🔥 Паника: $_txt"
done < <(_jctl -u aperod-node | grep -iE "panic:" 2>/dev/null | head -20 || true)

# ════════════════════════════════════════════════════════════
# 4. Circuit breaker (API-сервер ↔ нода)
# ════════════════════════════════════════════════════════════
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(_ts_of "$line")
  if echo "$line" | grep -qiE "circuit breaker.*open|breaker open"; then
    _event "$_ts" "ERROR"    "aperod-api" "🔴 Circuit breaker ОТКРЫТ — нода недоступна"
  elif echo "$line" | grep -qiE "circuit breaker.*clos|breaker clos"; then
    _event "$_ts" "RECOVERY" "aperod-api" "🟢 Circuit breaker ЗАКРЫТ — восстановлено"
  fi
done < <(_jctl -u aperod-api | grep -iE "circuit breaker" 2>/dev/null | head -40 || true)

# ════════════════════════════════════════════════════════════
# 5. Баны пиров и разрывы соединений
# ════════════════════════════════════════════════════════════
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(_ts_of "$line")
  _ip=$(echo "$line" | grep -oP '\d{1,3}(\.\d{1,3}){3}' | head -1 || echo "?")
  if echo "$line" | grep -qiE "banned|auto.ban"; then
    _event "$_ts" "WARN" "aperod-node" "🚫 Пир забанен: $_ip"
  else
    _event "$_ts" "WARN" "aperod-node" "📡 Разрыв соединения с пиром ($_ip)"
  fi
done < <(_jctl -u aperod-node | grep -iE "banned|peer.*disconnect" 2>/dev/null | head -30 || true)

# ════════════════════════════════════════════════════════════
# 6. Ошибки PostgreSQL (API-сервер)
# ════════════════════════════════════════════════════════════
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(_ts_of "$line")
  _short=$(echo "$line" | grep -oP 'error:.*' | head -c 70 || echo "$line" | tail -c 70)
  _event "$_ts" "ERROR" "aperod-api" "🗄 DB: $_short"
done < <(_jctl -u aperod-api | grep -iE "ECONNREFUSED.*5432|database.*error|DB error" 2>/dev/null | head -20 || true)

# ════════════════════════════════════════════════════════════
# 7. Диск переполнен
# ════════════════════════════════════════════════════════════
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  _ts=$(_ts_of "$line")
  _event "$_ts" "CRASH" "disk" "💾 Нет места на диске (ENOSPC)"
done < <(timeout "$JCTL_TIMEOUT" journalctl --no-pager --output=short-iso \
           --since "$SINCE" --until "$UNTIL" 2>/dev/null \
         | grep -iE "No space left on device|ENOSPC" | head -5 || true)

# ════════════════════════════════════════════════════════════
# 8. Ночная проверка (night-check.log)
# ════════════════════════════════════════════════════════════
NIGHT_CHECK_LINES=""
if [ -f "$NIGHT_CHECK_LOG" ]; then
  while IFS= read -r line; do
    [[ -z "$line" ]] && continue
    if [[ "$line" =~ \[([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}) ]]; then
      _ts="${BASH_REMATCH[1]/T/ }"
      if [[ "$line" == *"запущен"* ]]; then
        _event "$_ts" "INFO" "night-check" "🌙 Ночная проверка запущена"
      elif [[ "$line" == *"Ошибок: 0"* && "$line" == *"предупреждений: 0"* ]]; then
        _event "$_ts" "OK"   "night-check" "✅ Ночная проверка: всё ОК"
      elif [[ "$line" == *"Ошибок:"* ]]; then
        _s=$(echo "$line" | grep -oP 'Ошибок:.*' || echo "$line")
        _event "$_ts" "WARN" "night-check" "⚠️ Ночная проверка: $_s"
      fi
    fi
  done < <(grep "^\[20" "$NIGHT_CHECK_LOG" 2>/dev/null | tail -100 || true)

  # Последний блок ночной проверки для Telegram
  NIGHT_CHECK_LINES=$(tail -60 "$NIGHT_CHECK_LOG" 2>/dev/null \
    | grep -A50 "night-check.sh запущен" | tail -40 || true)
fi

# ════════════════════════════════════════════════════════════
# 9. Бэкап-история
# ════════════════════════════════════════════════════════════
if [ -f "$BACKUP_HISTORY" ]; then
  _since_prefix=$(echo "$SINCE" | cut -c1-10)
  while IFS= read -r bjson; do
    [[ -z "$bjson" ]] && continue
    # Парсим без python3 — awk достаточно для простого JSON
    _bts=$(echo "$bjson" | grep -oP '"timestamp":\s*"\K[^"]+' | cut -c1-16 | tr 'T' ' ' || true)
    [[ -z "$_bts" ]] && continue
    _bdate=$(echo "$_bts" | cut -c1-10)
    [[ "$_bdate" < "$_since_prefix" ]] && continue
    _bstatus=$(echo "$bjson" | grep -oP '"status":\s*"\K[^"]+' || echo "?")
    _bnode=$(echo   "$bjson" | grep -oP '"node":\s*"\K[^"]+' || echo "?")
    case "$_bstatus" in
      success) _event "$_bts" "OK"    "backup" "💾 Бэкап [$_bnode]: ✅ успешно" ;;
      skipped) _event "$_bts" "INFO"  "backup" "💾 Бэкап [$_bnode]: ⏭ пропущен" ;;
      *)       _event "$_bts" "ERROR" "backup" "💾 Бэкап [$_bnode]: ❌ ошибка ($_bstatus)" ;;
    esac
  done < <(tail -200 "$BACKUP_HISTORY" 2>/dev/null || true)
fi

# ════════════════════════════════════════════════════════════
# СОРТИРОВКА И ПОДСЧЁТ
# ════════════════════════════════════════════════════════════
IFS=$'\n' SORTED_EVENTS=($(printf '%s\n' "${EVENTS[@]:-}" | sort 2>/dev/null || true))
unset IFS

TOTAL_CRASH=0 TOTAL_OOM=0 TOTAL_ERROR=0 TOTAL_WARN=0
TOTAL_RESTART=0 TOTAL_RECOVERY=0 TOTAL_OK=0

for ev in "${SORTED_EVENTS[@]:-}"; do
  [[ -z "$ev" ]] && continue
  case "$(echo "$ev" | cut -d'|' -f2)" in
    CRASH)    TOTAL_CRASH=$(( TOTAL_CRASH + 1 ))       ;;
    OOM)      TOTAL_OOM=$(( TOTAL_OOM + 1 ))           ;;
    ERROR)    TOTAL_ERROR=$(( TOTAL_ERROR + 1 ))       ;;
    WARN)     TOTAL_WARN=$(( TOTAL_WARN + 1 ))         ;;
    RESTART)  TOTAL_RESTART=$(( TOTAL_RESTART + 1 ))   ;;
    RECOVERY) TOTAL_RECOVERY=$(( TOTAL_RECOVERY + 1 )) ;;
    OK)       TOTAL_OK=$(( TOTAL_OK + 1 ))             ;;
  esac
done

# ════════════════════════════════════════════════════════════
# ТЕКУЩЕЕ СОСТОЯНИЕ ПРЯМО СЕЙЧАС
# ════════════════════════════════════════════════════════════
_NODE_STATUS=$(systemctl is-active aperod-node 2>/dev/null || echo "unknown")
_API_STATUS=$(systemctl is-active aperod-api   2>/dev/null || echo "unknown")

_node_json=$(_curl "$NODE_API/api/v1/status")
_CUR_HEIGHT=$(echo "$_node_json" | grep -oP '"tip_height":\s*\K[0-9]+' \
             || echo "$_node_json" | grep -oP '"height":\s*\K[0-9]+' || echo "?")
_CUR_PEERS=$(echo  "$_node_json" | grep -oP '"peers":\s*\K[0-9]+' \
            || echo "$_node_json" | grep -oP '"peer_count":\s*\K[0-9]+' || echo "?")
_SYNCING=$(echo    "$_node_json" | grep -oP '"syncing":\s*\K(true|false)' || echo "")
[[ "$_SYNCING" == "false" ]] && _CUR_SYNC="синхронизирован" \
  || [[ "$_SYNCING" == "true" ]] && _CUR_SYNC="синхронизируется" || _CUR_SYNC="?"

_api_health=$(_curl "$API_BASE/api/health")
_CB=$(echo "$_api_health" | grep -oP '"circuit_breaker":\s*"\K[^"]+' || echo "?")

_DISK=$(df -h /opt/aperod 2>/dev/null | awk 'NR==2{print $5 " (" $4 " своб.)"}' || echo "?")
_RAM=$(free -m 2>/dev/null | awk '/^Mem:/{printf "%d МБ своб. / %d МБ всего", $7, $2}' || echo "?")

# ════════════════════════════════════════════════════════════
# ВЫВОД В ТЕРМИНАЛ
# ════════════════════════════════════════════════════════════
R=$'\033[0;31m' Y=$'\033[0;33m' G=$'\033[0;32m' B=$'\033[0;34m' N=$'\033[0m'
SEP="══════════════════════════════════════════"

echo ""
echo "${B}${SEP}${N}"
printf "${B}  🌅 УТРЕННИЙ ОТЧЁТ Aperod @ %s${N}\n" "$HOSTNAME_SHORT"
echo "${B}${SEP}${N}"
printf "  Период: %s  →  %s\n" "$SINCE" "$UNTIL"
echo ""

# Сводка
echo "  ─── СВОДКА ─────────────────────────────────"
if [[ $((TOTAL_CRASH + TOTAL_OOM + TOTAL_ERROR)) -gt 0 ]]; then
  printf "  ${R}Падений: %d${N}   OOM: ${R}%d${N}   Ошибок: ${R}%d${N}\n" \
    "$TOTAL_CRASH" "$TOTAL_OOM" "$TOTAL_ERROR"
else
  printf "  ${G}Падений: 0   OOM: 0   Ошибок: 0${N}\n"
fi
printf "  Рестартов: %d   Предупреждений: ${Y}%d${N}   Восстановлений: ${G}%d${N}\n" \
  "$TOTAL_RESTART" "$TOTAL_WARN" "$TOTAL_RECOVERY"
echo ""

# Текущее состояние
echo "  ─── СЕЙЧАС ─────────────────────────────────"
_nc=$([[ "$_NODE_STATUS" == "active" ]] && echo "$G" || echo "$R")
_ac=$([[ "$_API_STATUS"  == "active" ]] && echo "$G" || echo "$R")
printf "  Нода:  ${_nc}%s${N}  │  Высота: %s  │  Пиры: %s  │  %s\n" \
  "$_NODE_STATUS" "$_CUR_HEIGHT" "$_CUR_PEERS" "$_CUR_SYNC"
printf "  API:   ${_ac}%s${N}  │  Circuit breaker: %s\n" "$_API_STATUS" "$_CB"
printf "  Диск:  %s\n" "$_DISK"
printf "  RAM:   %s\n" "$_RAM"
echo ""

# Хронология
if [[ ${#SORTED_EVENTS[@]} -eq 0 ]]; then
  echo "  ${G}✅ За ночь не зафиксировано ни одного сбоя.${N}"
else
  echo "  ─── ХРОНОЛОГИЯ СОБЫТИЙ ─────────────────────"
  for ev in "${SORTED_EVENTS[@]:-}"; do
    [[ -z "$ev" ]] && continue
    _evts=$(echo "$ev" | cut -d'|' -f1)
    _elvl=$(echo "$ev" | cut -d'|' -f2)
    _esrc=$(echo "$ev" | cut -d'|' -f3)
    _emsg=$(echo "$ev" | cut -d'|' -f4)
    case "$_elvl" in
      CRASH|OOM) _c="$R" ;; ERROR) _c="$R" ;;
      WARN)      _c="$Y" ;; RESTART) _c="$B" ;;
      RECOVERY|OK) _c="$G" ;; *) _c="" ;;
    esac
    printf "  %s  ${_c}%-8s${N}  %-12s  %s\n" "$_evts" "$_elvl" "$_esrc" "$_emsg"
  done
fi
echo ""
echo "${B}${SEP}${N}"
echo ""

# ════════════════════════════════════════════════════════════
# TELEGRAM
# ════════════════════════════════════════════════════════════
if [[ -z "$NO_TELEGRAM" ]] \
   && [[ -n "${TELEGRAM_BOT_TOKEN:-}" ]] \
   && [[ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]]; then

  _svc_ic() { [[ "$1" == "active" ]] && echo "🟢" || echo "🔴"; }

  if [[ $((TOTAL_CRASH + TOTAL_OOM + TOTAL_ERROR)) -gt 0 ]]; then
    _mood="⚠️ <b>Ночь с инцидентами</b>"
  elif [[ $TOTAL_WARN -gt 0 ]]; then
    _mood="💛 <b>Ночь с предупреждениями</b>"
  else
    _mood="✅ <b>Ночь прошла штатно</b>"
  fi

  _TG="🌅 <b>Утренний отчёт @ ${HOSTNAME_SHORT}</b>
📅 ${SINCE} → ${UNTIL}

${_mood}
• Падений: <b>${TOTAL_CRASH}</b>  OOM: <b>${TOTAL_OOM}</b>  Ошибок: <b>${TOTAL_ERROR}</b>
• Рестартов: ${TOTAL_RESTART}  Предупреждений: ${TOTAL_WARN}  Восстановлений: ${TOTAL_RECOVERY}

<b>Сейчас:</b>
$(_svc_ic "$_NODE_STATUS") Нода: <code>${_NODE_STATUS}</code> — высота ${_CUR_HEIGHT}, пиры ${_CUR_PEERS}, ${_CUR_SYNC}
$(_svc_ic "$_API_STATUS") API: <code>${_API_STATUS}</code> — circuit breaker: ${_CB}
💿 Диск: ${_DISK}
🧠 RAM: ${_RAM}"

  if [[ ${#SORTED_EVENTS[@]} -gt 0 ]]; then
    _TG+=$'\n\n<b>Хронология:</b>\n<pre>'
    _n=0
    for ev in "${SORTED_EVENTS[@]:-}"; do
      [[ -z "$ev" ]] && continue
      _evts=$(echo "$ev" | cut -d'|' -f1 | cut -c12-16)   # только HH:MM
      _elvl=$(echo "$ev" | cut -d'|' -f2)
      _esrc=$(echo "$ev" | cut -d'|' -f3)
      _emsg=$(echo "$ev" | cut -d'|' -f4 | cut -c1-55)
      _TG+=$(printf "%s %-7s %-11s %s\n" "$_evts" "$_elvl" "$_esrc" "$_emsg")
      _n=$(( _n + 1 ))
      if [[ $_n -ge 18 ]]; then
        _TG+="... ещё $(( ${#SORTED_EVENTS[@]} - 18 )) событий"$'\n'
        break
      fi
    done
    _TG+='</pre>'
  fi

  curl -sf --max-time 15 \
    "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
    -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
    -d "parse_mode=HTML" \
    --data-urlencode "text=${_TG}" \
    > /dev/null 2>&1 && echo "📨 Отчёт отправлен в Telegram" || echo "⚠️  Telegram: ошибка отправки"
else
  echo "ℹ️  Telegram отключён (NO_TELEGRAM=1 или нет токена)"
fi

# Лог
mkdir -p "$LOG_DIR" 2>/dev/null || true
echo "[$(date -u +"%Y-%m-%dT%H:%M:%SZ")] morning-report: crashes=${TOTAL_CRASH} oom=${TOTAL_OOM} errors=${TOTAL_ERROR} warns=${TOTAL_WARN} restarts=${TOTAL_RESTART}" \
  >> "$LOG_DIR/morning-report.log" 2>/dev/null || true
