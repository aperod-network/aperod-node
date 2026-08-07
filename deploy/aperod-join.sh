#!/usr/bin/env bash
# ============================================================
#  Aperod — One-Command Node Join
#  Запустите НА НОВОМ СЕРВЕРЕ (не на основном).
#
#  Использование:
#    sudo bash aperod-join.sh <PRIMARY_IP>:<API_PORT> [OPTIONS]
#
#  Примеры:
#    # Простой вариант (dev/тест, без API-ключа):
#    sudo bash aperod-join.sh 89.169.53.128:8545
#
#    # С API-ключом (передаётся в открытом виде по HTTP — используйте --tunnel!):
#    sudo bash aperod-join.sh 89.169.53.128:8545 --api-key mySecret --tunnel user@primary
#
#    # Через SSH-туннель (рекомендуется для prod):
#    sudo bash aperod-join.sh 89.169.53.128:8545 --tunnel aperod@89.169.53.128
#
#    # Нестандартная директория данных:
#    sudo bash aperod-join.sh 89.169.53.128:8545 --data-dir /opt/aperod/data/testnet
#
#  Опции:
#    --api-key    <key>      X-API-Key для аутентификации на основном узле
#    --tunnel     <user@host> Поднять SSH-туннель (рекомендуется при --api-key)
#    --data-dir   <path>     Директория данных (по умолчанию /var/lib/aperod)
#    --user       <name>     Пользователь-владелец данных (по умолчанию aperod)
#    --skip-start            Не запускать aperod-node после загрузки
#    --no-chaindb            Пропустить загрузку chain.db (только snapshot)
#
#  ⚠️  ВАЖНО — безопасность передачи данных:
#    Этот скрипт использует plain HTTP для загрузки chain.db и snapshot.
#    Если задан --api-key, ключ передаётся в открытом виде.
#    В производственной среде настоятельно рекомендуется один из вариантов:
#      1. --tunnel user@primary-host  (SSH-туннель, автоматически)
#      2. Настроить HTTPS на основном узле (nginx + TLS)
#      3. Использовать VPN (WireGuard/OpenVPN)
#    Без одного из этих вариантов API-ключ и данные цепочки могут быть
#    перехвачены на промежуточных узлах сети.
#
#  Что делает скрипт:
#    1. Останавливает aperod-node на ТЕКУЩЕМ сервере
#    2. При --tunnel: поднимает SSH-туннель к основному узлу
#    3. Скачивает chain.db через HTTP с основного узла
#    4. Скачивает snapshot (UTXO-состояние) через HTTP с основного узла
#    5. Распаковывает файлы в data_dir с проверкой имён файлов
#    6. Удаляет p2p_identity.key (нода генерирует новый при старте)
#    7. Применяет drop-in конфиги systemd (timeout, GOMEMLIMIT)
#    8. Запускает aperod-node и ждёт готовности API
# ============================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[ERR]${NC}   $*" >&2; exit 1; }

# ── Параметры по умолчанию ────────────────────────────────
DATA_DIR="/var/lib/aperod"
DATA_USER="aperod"
API_KEY=""
TUNNEL_HOST=""
SKIP_START=false
NO_CHAINDB=false
PRIMARY=""
TUNNEL_LOCAL_PORT=19545          # ephemeral local port for SSH tunnel
HEALTH_MAX_ATTEMPTS=60           # 5 мин при 5-секундном интервале
HEALTH_WAIT_SECS=5
TUNNEL_PID=""

# ── Парсинг аргументов ────────────────────────────────────
if [[ $# -eq 0 ]]; then
  die "Укажите адрес основного узла: bash aperod-join.sh <IP>:<PORT>"
fi

PRIMARY="${1}"
shift

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-key)    API_KEY="${2:-}";     shift 2 ;;
    --tunnel)     TUNNEL_HOST="${2:-}"; shift 2 ;;
    --data-dir)   DATA_DIR="${2:-}";    shift 2 ;;
    --user)       DATA_USER="${2:-}";   shift 2 ;;
    --skip-start) SKIP_START=true;      shift   ;;
    --no-chaindb) NO_CHAINDB=true;      shift   ;;
    *) die "Неизвестный аргумент: $1" ;;
  esac
done

[[ -z "${PRIMARY}" ]] && die "Укажите адрес основного узла: <IP>:<PORT>"

# Разбираем PRIMARY на хост и порт для SSH-туннеля
PRIMARY_HOST="${PRIMARY%%:*}"
PRIMARY_PORT="${PRIMARY##*:}"
[[ "${PRIMARY_PORT}" =~ ^[0-9]+$ ]] || PRIMARY_PORT="8545"

# ── Автоопределение GOMEMLIMIT ────────────────────────────
TOTAL_RAM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
TOTAL_RAM_BYTES=$(( TOTAL_RAM_KB * 1024 ))
AUTO_GOMEMLIMIT=$(( TOTAL_RAM_BYTES * 3 / 4 ))
MIN_GOMEMLIMIT=$(( 1536 * 1024 * 1024 ))   # 1.5 GiB
MAX_GOMEMLIMIT=$(( 5905580032 ))            # 5500 MiB
if (( AUTO_GOMEMLIMIT < MIN_GOMEMLIMIT )); then AUTO_GOMEMLIMIT=${MIN_GOMEMLIMIT}; fi
if (( AUTO_GOMEMLIMIT > MAX_GOMEMLIMIT )); then AUTO_GOMEMLIMIT=${MAX_GOMEMLIMIT}; fi
GOMEMLIMIT_BYTES="${GOMEMLIMIT_BYTES:-${AUTO_GOMEMLIMIT}}"

# ── Баннер ────────────────────────────────────────────────
echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod — One-Command Node Join                      ║
╚════════════════════════════════════════════════════════════╝${NC}
"
info "Основной узел:   ${PRIMARY}"
info "Локальный dir:   ${DATA_DIR}"
info "Пользователь:    ${DATA_USER}"
info "GOMEMLIMIT:      $(( GOMEMLIMIT_BYTES / 1024 / 1024 )) МБ"
if [[ -n "${TUNNEL_HOST}" ]]; then
  info "SSH-туннель:     ${TUNNEL_HOST} → localhost:${TUNNEL_LOCAL_PORT}"
fi
echo

# ── Проверка root ─────────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Запустите от root: sudo bash aperod-join.sh ..."
fi

# ── Проверка зависимостей ─────────────────────────────────
for cmd in curl tar; do
  command -v "${cmd}" >/dev/null 2>&1 || die "Требуется '${cmd}', но он не установлен"
done
if [[ -n "${TUNNEL_HOST}" ]]; then
  command -v ssh >/dev/null 2>&1 || die "Требуется 'ssh' для --tunnel, но он не установлен"
fi

# ── Предупреждение: HTTP + API-ключ ──────────────────────
if [[ -n "${API_KEY}" && -z "${TUNNEL_HOST}" ]]; then
  echo -e "${YELLOW}${BOLD}⚠️  ВНИМАНИЕ: API-ключ будет передан по нешифрованному HTTP.${NC}"
  echo -e "${YELLOW}   Рекомендуем использовать SSH-туннель:${NC}"
  echo -e "${YELLOW}   ${BOLD}bash aperod-join.sh ${PRIMARY} --api-key ... --tunnel ${DATA_USER}@${PRIMARY_HOST}${NC}"
  echo
  read -rp "Продолжить без туннеля? [y/N] " CONFIRM
  [[ "${CONFIRM,,}" == "y" ]] || die "Прервано. Запустите с --tunnel для безопасной передачи."
fi

# ── Вспомогательная функция: cleanup ─────────────────────
cleanup() {
  if [[ -n "${TUNNEL_PID}" ]]; then
    kill "${TUNNEL_PID}" 2>/dev/null || true
    info "SSH-туннель закрыт (PID ${TUNNEL_PID})"
  fi
}
trap cleanup EXIT

# ── Вспомогательная функция — curl с API-ключом ───────────
# Используется только с BASE_URL, установленным ниже (после возможного туннеля)
api_curl() {
  local url="$1"; shift
  if [[ -n "${API_KEY}" ]]; then
    curl --fail --show-error -H "X-API-Key: ${API_KEY}" "$@" "${url}"
  else
    curl --fail --show-error "$@" "${url}"
  fi
}

# ── Шаг 1: Проверяем доступность основного узла ──────────
info "Шаг 1/7: Проверяем доступность основного узла…"

if [[ -n "${TUNNEL_HOST}" ]]; then
  info "  Поднимаем SSH-туннель: ${TUNNEL_HOST}:${PRIMARY_PORT} → localhost:${TUNNEL_LOCAL_PORT}…"
  ssh -o StrictHostKeyChecking=accept-new \
      -o ExitOnForwardFailure=yes \
      -o ServerAliveInterval=30 \
      -fN \
      -L "${TUNNEL_LOCAL_PORT}:127.0.0.1:${PRIMARY_PORT}" \
      "${TUNNEL_HOST}"
  # Capture the background ssh PID
  TUNNEL_PID=$(pgrep -n -f "ssh.*${TUNNEL_LOCAL_PORT}:127.0.0.1:${PRIMARY_PORT}" 2>/dev/null || true)
  BASE_URL="http://127.0.0.1:${TUNNEL_LOCAL_PORT}"
  sleep 1
else
  BASE_URL="http://${PRIMARY}"
fi

if ! api_curl "${BASE_URL}/api/v1/status" -s --max-time 10 -o /dev/null; then
  die "Основной узел недоступен: ${BASE_URL}/api/v1/status
  Убедитесь что:
  - IP и порт корректны
  - Порт ${PRIMARY_PORT} открыт в firewall основного узла
  - Основной узел запущен: systemctl status aperod-node (на основном)"
fi
ok "Основной узел доступен"
echo

# ── Шаг 2: Останавливаем ноду на ТЕКУЩЕМ сервере ─────────
info "Шаг 2/7: Останавливаем aperod-node на текущем сервере…"
if systemctl is-active --quiet aperod-node 2>/dev/null; then
  systemctl disable --now aperod-node
  ok "aperod-node остановлен и отключён"
else
  warn "aperod-node не запущен — продолжаем"
fi
echo

# ── Шаг 3: Подготовка директории данных ──────────────────
info "Шаг 3/7: Подготавливаем директорию данных…"
if [[ -d "${DATA_DIR}" ]]; then
  warn "Очищаем старые данные: chain.db и snapshot файлы…"
  rm -rf "${DATA_DIR}/chain.db" "${DATA_DIR}"/snapshot-v2-*.json.gz
fi
mkdir -p "${DATA_DIR}"
ok "Директория данных готова: ${DATA_DIR}"
echo

# ── Шаг 4: Скачиваем chain.db ─────────────────────────────
if [[ "${NO_CHAINDB}" == "false" ]]; then
  info "Шаг 4/7: Скачиваем chain.db (~1-2 ГБ, может занять несколько минут)…"
  CHAINDB_TMP="${DATA_DIR}/.chaindb-download.tar.gz.tmp"
  rm -f "${CHAINDB_TMP}"

  if api_curl "${BASE_URL}/api/v1/chaindb/export" \
      --progress-bar \
      --max-time 1800 \
      -o "${CHAINDB_TMP}"; then
    info "  Распаковываем chain.db…"
    # --no-same-permissions / --no-same-owner: don't trust archive metadata for permissions.
    # This prevents a tampered archive from installing suid files or changing ownership.
    tar --no-same-permissions --no-same-owner -xzf "${CHAINDB_TMP}" -C "${DATA_DIR}"
    rm -f "${CHAINDB_TMP}"
    ok "chain.db загружен и распакован"
  else
    rm -f "${CHAINDB_TMP}"
    die "Не удалось скачать chain.db с основного узла.
  Возможные причины:
  - Нужен API-ключ: добавьте --api-key
  - Нужен SSH-туннель: добавьте --tunnel user@host
  - Основной узел не поддерживает экспорт (обновите до версии с aperod-join)"
  fi
else
  warn "Шаг 4/7: Загрузка chain.db пропущена (--no-chaindb)"
  warn "  Нода синхронизируется от peers — это займёт несколько часов"
fi
echo

# ── Шаг 5: Скачиваем snapshot ─────────────────────────────
info "Шаг 5/7: Скачиваем UTXO-snapshot с основного узла…"
SNAP_HEADER_FILE="${DATA_DIR}/.snap-headers.tmp"
SNAP_TMP="${DATA_DIR}/.snapshot-download.tmp"
rm -f "${SNAP_HEADER_FILE}" "${SNAP_TMP}"

if api_curl "${BASE_URL}/api/v1/snapshot/export" \
    --progress-bar \
    --max-time 300 \
    -D "${SNAP_HEADER_FILE}" \
    -o "${SNAP_TMP}"; then

  # Read server-supplied filename from response headers.
  RAW_FILENAME=$(grep -i "X-Snapshot-Filename:" "${SNAP_HEADER_FILE}" 2>/dev/null \
    | tr -d '\r' | awk '{print $2}' | tr -d ' ')
  rm -f "${SNAP_HEADER_FILE}"

  # ── Path traversal guard ─────────────────────────────────────
  # Accept only filenames that match the exact canonical pattern.
  # Any other value (including paths with / or ..) is rejected.
  if [[ "${RAW_FILENAME}" =~ ^snapshot-v2-[0-9]+\.json\.gz$ ]]; then
    SNAP_FILENAME="${RAW_FILENAME}"
  else
    warn "Unexpected X-Snapshot-Filename: '${RAW_FILENAME}' — using safe default"
    SNAP_FILENAME="snapshot-v2-import.json.gz"
  fi

  mv "${SNAP_TMP}" "${DATA_DIR}/${SNAP_FILENAME}"
  SNAP_HEIGHT=$(echo "${SNAP_FILENAME}" | grep -oP '(?<=snapshot-v2-)\d+' || echo "unknown")
  ok "Snapshot загружен: ${SNAP_FILENAME} (height=${SNAP_HEIGHT})"
else
  rm -f "${SNAP_TMP}" "${SNAP_HEADER_FILE}"
  warn "Snapshot недоступен — нода запустится без fast-path (чуть медленнее)"
  warn "Это нормально если snapshot ещё не создан на основном узле"
fi
echo

# ── Шаг 6: Права и p2p identity ──────────────────────────
info "Шаг 6/7: Устанавливаем права и очищаем p2p identity…"

# Удаляем p2p_identity.key — нода сгенерирует новый уникальный ключ при старте.
# Если скопировать ключ с основного узла, оба сервера видят друг друга как
# self-connection и peer_count остаётся 0 навсегда.
rm -f "${DATA_DIR}/p2p_identity.key"

if id "${DATA_USER}" &>/dev/null; then
  chown -R "${DATA_USER}:${DATA_USER}" "${DATA_DIR}"
  ok "Права установлены: ${DATA_USER}:${DATA_USER}"
else
  warn "Пользователь '${DATA_USER}' не существует — права не изменены"
  warn "Создайте: useradd --system --no-create-home --shell /usr/sbin/nologin ${DATA_USER}"
fi
echo

# ── Шаг 7: systemd drop-in конфиги ───────────────────────
info "Шаг 7/7: Применяем systemd drop-in конфиги…"
DROPIN_DIR="/etc/systemd/system/aperod-node.service.d"
mkdir -p "${DROPIN_DIR}"

cat > "${DROPIN_DIR}/timeout.conf" << 'DROPIN'
# Aperod node — shutdown timeout drop-in
# Install path: /etc/systemd/system/aperod-node.service.d/timeout.conf
#
# TimeoutStopSec=900 gives the UTXO snapshot up to 15 minutes to flush
# before systemd sends SIGKILL.  The Aug 2026 outage was caused by a
# 300 s value triggering SIGKILL mid-write on a 5.7 GB RAM node.
[Service]
TimeoutStopSec=900
DROPIN

cat > "${DROPIN_DIR}/gomemlimit.conf" << DROPIN
[Service]
Environment="GOMEMLIMIT=${GOMEMLIMIT_BYTES}"
DROPIN

systemctl daemon-reload
ok "Drop-in конфиги применены (TimeoutStopSec=900, GOMEMLIMIT=${GOMEMLIMIT_BYTES})"
echo

# ── Закрываем туннель после загрузки (нода сама подключится по внешнему IP) ──
if [[ -n "${TUNNEL_PID}" ]]; then
  kill "${TUNNEL_PID}" 2>/dev/null || true
  TUNNEL_PID=""
  info "SSH-туннель закрыт (данные загружены)"
fi

# ── Запуск ноды ───────────────────────────────────────────
if [[ "${SKIP_START}" == "true" ]]; then
  echo -e "${YELLOW}Запуск пропущен (--skip-start). Запустите вручную:${NC}"
  echo "  systemctl enable --now aperod-node"
  echo
else
  info "Запускаем aperod-node…"
  systemctl enable --now aperod-node
  ok "aperod-node запущен"
  echo

  # ── Ожидаем готовности API ───────────────────────────────
  info "Ожидаем готовности API (~5 мин для key-image rebuild)…"
  ATTEMPT=0
  HEIGHT=0
  PEERS=0
  while [[ ${ATTEMPT} -lt ${HEALTH_MAX_ATTEMPTS} ]]; do
    ATTEMPT=$(( ATTEMPT + 1 ))

    STATS=$(curl -s --max-time 5 "http://127.0.0.1:8545/api/v1/network/stats" 2>/dev/null || echo "")

    if [[ -n "${STATS}" ]] && python3 -c "import sys,json; json.load(sys.stdin)" <<< "${STATS}" 2>/dev/null; then
      HEIGHT=$(python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('height',0))" <<< "${STATS}" 2>/dev/null || echo "0")
      PEERS=$(python3  -c "import sys,json; d=json.load(sys.stdin); print(d.get('peer_count',0))" <<< "${STATS}" 2>/dev/null || echo "0")
      printf "  [%2d/%d] height=%-8s peers=%s\n" "${ATTEMPT}" "${HEALTH_MAX_ATTEMPTS}" "${HEIGHT}" "${PEERS}"
      if [[ "${HEIGHT}" -gt 0 ]]; then
        break
      fi
    else
      printf "  [%2d/%d] API ещё не готов…\n" "${ATTEMPT}" "${HEALTH_MAX_ATTEMPTS}"
    fi
    sleep ${HEALTH_WAIT_SECS}
  done

  if [[ "${HEIGHT}" -eq 0 ]]; then
    echo
    warn "API не ответил в ожидаемое время."
    warn "Проверьте логи: journalctl -u aperod-node -n 50 --no-pager"
    exit 1
  fi
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════════╗"
echo -e "  ✓  Нода успешно подключена к сети Aperod"
if [[ "${SKIP_START}" == "false" ]]; then
  echo -e "     Height: ${HEIGHT}  |  Peers: ${PEERS}"
fi
echo -e "╚══════════════════════════════════════════════════════════════════╝${NC}"
echo

if [[ "${SKIP_START}" == "false" && "${PEERS}" -eq 0 ]]; then
  warn "Peers = 0 — нода загружается, пиры появятся после key-image rebuild"
  warn "Повторите через 5 мин:"
  warn "  curl -s http://127.0.0.1:8545/api/v1/network/stats | python3 -m json.tool"
  echo
fi

info "Следующие шаги:"
echo "  1. Откройте порт: ufw allow 30303/tcp"
echo "  2. Для валидатора: пополните reward_address (мин. 100 000 APRO)"
echo "     и отправьте StakeTx через Telegram Wallet → Staking"
echo
