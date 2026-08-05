#!/usr/bin/env bash
# ============================================================
#  Aperod — One-Command Node Join
#  Запустите НА НОВОМ СЕРВЕРЕ (не на основном).
#
#  Использование:
#    sudo bash aperod-join.sh <PRIMARY_IP>:<API_PORT> [OPTIONS]
#
#  Примеры:
#    sudo bash aperod-join.sh 89.169.53.128:8545
#    sudo bash aperod-join.sh 89.169.53.128:8545 --api-key mySecret
#    sudo bash aperod-join.sh 89.169.53.128:8545 --data-dir /opt/aperod/data/testnet
#
#  Опции:
#    --api-key    <key>   X-API-Key для аутентификации на основном узле
#    --data-dir   <path>  Директория данных на новом сервере (по умолчанию /var/lib/aperod)
#    --user       <name>  Пользователь-владелец данных          (по умолчанию aperod)
#    --skip-start         Не запускать aperod-node после загрузки
#    --no-chaindb         Пропустить загрузку chain.db (только snapshot)
#
#  Что делает скрипт:
#    1. Останавливает aperod-node на ТЕКУЩЕМ сервере
#    2. Скачивает chain.db (полная история блоков) через HTTP с основного узла
#    3. Скачивает snapshot (состояние UTXO) через HTTP с основного узла
#    4. Распаковывает файлы в data_dir с правильными правами
#    5. Генерирует свежий p2p_identity.key (никогда не копирует с основного)
#    6. Применяет drop-in конфиги systemd (timeout, GOMEMLIMIT)
#    7. Запускает aperod-node и ждёт готовности API
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
SKIP_START=false
NO_CHAINDB=false
PRIMARY=""
HEALTH_MAX_ATTEMPTS=60   # 5 мин при 5-секундном интервале
HEALTH_WAIT_SECS=5

# ── Парсинг аргументов ────────────────────────────────────
if [[ $# -eq 0 ]]; then
  die "Укажите адрес основного узла: bash aperod-join.sh <IP>:<PORT>"
fi

PRIMARY="${1}"
shift

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-key)    API_KEY="${2:-}"; shift 2 ;;
    --data-dir)   DATA_DIR="${2:-}"; shift 2 ;;
    --user)       DATA_USER="${2:-}"; shift 2 ;;
    --skip-start) SKIP_START=true; shift ;;
    --no-chaindb) NO_CHAINDB=true; shift ;;
    *) die "Неизвестный аргумент: $1" ;;
  esac
done

[[ -z "${PRIMARY}" ]] && die "Укажите адрес основного узла: <IP>:<PORT>"

PRIMARY_URL="http://${PRIMARY}"

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
info "Основной узел:   ${PRIMARY_URL}"
info "Локальный dir:   ${DATA_DIR}"
info "Пользователь:    ${DATA_USER}"
info "GOMEMLIMIT:      $(( GOMEMLIMIT_BYTES / 1024 / 1024 )) МБ"
echo

# ── Проверка root ─────────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Запустите от root: sudo bash aperod-join.sh ..."
fi

# ── Проверка зависимостей ─────────────────────────────────
for cmd in curl tar; do
  command -v "${cmd}" >/dev/null 2>&1 || die "Требуется '${cmd}', но он не установлен"
done

# ── Вспомогательная функция — curl с API-ключом ───────────
api_curl() {
  local url="$1"; shift
  if [[ -n "${API_KEY}" ]]; then
    curl --fail --show-error -H "X-API-Key: ${API_KEY}" "$@" "${url}"
  else
    curl --fail --show-error "$@" "${url}"
  fi
}

# ── Шаг 1: Проверяем доступность основного узла ──────────
info "Шаг 1/7: Проверяем доступность ${PRIMARY_URL}…"
if ! api_curl "${PRIMARY_URL}/api/v1/status" -s --max-time 10 -o /dev/null; then
  die "Основной узел недоступен: ${PRIMARY_URL}/api/v1/status
  Убедитесь что:
  - IP и порт корректны
  - Порт ${PRIMARY} открыт в firewall основного узла
  - Основной узел запущен (systemctl status aperod-node на основном)"
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
# Очищаем данные (chain.db, snapshot) чтобы не было конфликтов LevelDB
if [[ -d "${DATA_DIR}" ]]; then
  warn "Очищаем старые данные в ${DATA_DIR}/ …"
  rm -rf "${DATA_DIR}/chain.db" "${DATA_DIR}"/snapshot-*.json.gz
fi
mkdir -p "${DATA_DIR}"
ok "Директория данных готова: ${DATA_DIR}"
echo

# ── Шаг 4: Скачиваем chain.db ─────────────────────────────
if [[ "${NO_CHAINDB}" == "false" ]]; then
  info "Шаг 4/7: Скачиваем chain.db с основного узла…"
  info "  Это может занять несколько минут (~1-2 ГБ)"
  CHAINDB_TMP="${DATA_DIR}/.chaindb-download.tar.gz.tmp"
  rm -f "${CHAINDB_TMP}"

  if api_curl "${PRIMARY_URL}/api/v1/chaindb/export" \
      --progress-bar \
      --max-time 1800 \
      -o "${CHAINDB_TMP}"; then
    info "  Распаковываем chain.db…"
    tar -xzf "${CHAINDB_TMP}" -C "${DATA_DIR}"
    rm -f "${CHAINDB_TMP}"
    ok "chain.db загружен и распакован"
  else
    rm -f "${CHAINDB_TMP}"
    die "Не удалось скачать chain.db с основного узла.
  Возможные причины:
  - Основной узел не настроен с api.key — добавьте --api-key или проверьте node.yaml
  - Эндпоинт недоступен — убедитесь, что используется версия с поддержкой экспорта
  - Сетевые проблемы или timeout — попробуйте ещё раз или используйте --no-chaindb"
  fi
else
  warn "Шаг 4/7: Загрузка chain.db пропущена (--no-chaindb)"
  warn "  Нода синхронизируется от peers — это займёт несколько часов"
fi
echo

# ── Шаг 5: Скачиваем snapshot ─────────────────────────────
info "Шаг 5/7: Скачиваем UTXO-snapshot с основного узла…"
SNAP_HEADER_FILE="${DATA_DIR}/.snap-headers.tmp"
rm -f "${SNAP_HEADER_FILE}"

if api_curl "${PRIMARY_URL}/api/v1/snapshot/export" \
    --progress-bar \
    --max-time 300 \
    -D "${SNAP_HEADER_FILE}" \
    -o "${DATA_DIR}/.snapshot-download.tmp"; then

  # Читаем имя файла из заголовка ответа
  SNAP_FILENAME=$(grep -i "X-Snapshot-Filename:" "${SNAP_HEADER_FILE}" 2>/dev/null \
    | tr -d '\r' | awk '{print $2}')
  rm -f "${SNAP_HEADER_FILE}"

  if [[ -z "${SNAP_FILENAME}" ]]; then
    SNAP_FILENAME="snapshot-v2-import.json.gz"
  fi

  mv "${DATA_DIR}/.snapshot-download.tmp" "${DATA_DIR}/${SNAP_FILENAME}"
  SNAP_HEIGHT=$(grep -oP '(?<=snapshot-v2-)\d+' <<< "${SNAP_FILENAME}" || echo "unknown")
  ok "Snapshot загружен: ${SNAP_FILENAME} (height=${SNAP_HEIGHT})"
else
  rm -f "${DATA_DIR}/.snapshot-download.tmp" "${SNAP_HEADER_FILE}"
  warn "Snapshot недоступен — нода запустится без fast-path (медленнее)"
  warn "Это нормально если snapshot ещё не создан на основном узле"
fi
echo

# ── Шаг 6: Права и p2p identity ──────────────────────────
info "Шаг 6/7: Устанавливаем права и очищаем p2p identity…"

# Удаляем p2p_identity.key — нода сгенерирует новый уникальный ключ при старте
# Использование ключа основного узла вызывает self-connection и peer_count=0 навсегда
rm -f "${DATA_DIR}/p2p_identity.key"

# Права
if id "${DATA_USER}" &>/dev/null; then
  chown -R "${DATA_USER}:${DATA_USER}" "${DATA_DIR}"
  ok "Права установлены: ${DATA_USER}:${DATA_USER}"
else
  warn "Пользователь '${DATA_USER}' не существует — права не изменены"
  warn "Создайте пользователя: useradd --system --no-create-home --shell /usr/sbin/nologin ${DATA_USER}"
fi
echo

# ── Шаг 7: systemd drop-in конфиги ───────────────────────
info "Шаг 7/7: Применяем systemd drop-in конфиги…"
DROPIN_DIR="/etc/systemd/system/aperod-node.service.d"
mkdir -p "${DROPIN_DIR}"

cat > "${DROPIN_DIR}/timeout.conf" << 'DROPIN'
[Service]
TimeoutStopSec=300
DROPIN

cat > "${DROPIN_DIR}/gomemlimit.conf" << DROPIN
[Service]
Environment="GOMEMLIMIT=${GOMEMLIMIT_BYTES}"
DROPIN

systemctl daemon-reload
ok "Drop-in конфиги применены (TimeoutStopSec=300, GOMEMLIMIT=${GOMEMLIMIT_BYTES})"
echo

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
  info "Ожидаем готовности API (может занять ~5 мин для key-image rebuild)…"
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
    warn "Ищите: 'API server ready' или 'p2p started'"
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
  warn "Peers = 0 — нода загружается, пиры появятся после завершения key-image rebuild"
  warn "Повторите проверку через 5 минут:"
  warn "  curl -s http://127.0.0.1:8545/api/v1/network/stats | python3 -m json.tool"
  echo
fi

info "Следующие шаги:"
echo "  1. Убедитесь что порт 30303/tcp открыт: ufw allow 30303/tcp"
echo "  2. Для регистрации как валидатор:"
echo "     - Пополните reward_address (мин. 100 000 APRO)"
echo "     - Отправьте StakeTx через Telegram Wallet → Staking"
echo "     - Уберите 'non_validator: true' из node.yaml (если есть)"
echo "     - systemctl restart aperod-node"
echo
