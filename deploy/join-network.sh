#!/usr/bin/env bash
# ============================================================
#  Aperod — Join Existing Network
#  Запуск на ОСНОВНОМ узле (disturbing-blush / 89.169.53.128)
#  Подключает новый сервер к работающей сети за один шаг.
#
#  Использование:
#    sudo bash join-network.sh <IP_НОВОГО_СЕРВЕРА>
#
#  Пример:
#    sudo bash join-network.sh 77.221.153.86
#
#  Что делает скрипт:
#    1. Останавливает и отключает aperod-node на новом сервере
#    2. Rsync цепи с --delete (гарантирует чистый LevelDB)
#    3. Удаляет скопированный p2p_identity.key (новый генерируется при старте)
#    4. Устанавливает правильные права aperod:aperod
#    5. Включает и запускает aperod-node на новом сервере
#    6. Ждёт готовности API и проверяет peer_count
# ============================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[ERR]${NC}   $*"; exit 1; }

# ── Параметры ─────────────────────────────────────────────
PRIMARY_DATA_DIR="/opt/aperod/data/testnet"
SECONDARY_DATA_DIR="/var/lib/aperod"
SECONDARY_USER="aperod"
HEALTH_MAX_ATTEMPTS=30
HEALTH_WAIT_SECS=5

# ── Аргументы ─────────────────────────────────────────────
TARGET_IP="${1:-}"
[[ -z "${TARGET_IP}" ]] && die "Укажите IP нового сервера: bash join-network.sh <IP>"

# ── Проверки ──────────────────────────────────────────────
[[ -d "${PRIMARY_DATA_DIR}" ]] || die "Директория данных не найдена: ${PRIMARY_DATA_DIR}"
command -v rsync >/dev/null 2>&1 || die "rsync не установлен"
command -v ssh >/dev/null 2>&1   || die "ssh не установлен"

echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod — Join Network Script                        ║
║        Основной узел → ${TARGET_IP}
╚════════════════════════════════════════════════════════════╝${NC}
"

info "Целевой сервер: ${TARGET_IP}"
info "Источник данных: ${PRIMARY_DATA_DIR}"
info "Назначение: ${TARGET_IP}:${SECONDARY_DATA_DIR}"
echo

# ── Шаг 1: Останавливаем ноду на целевом сервере ──────────
info "Шаг 1/5: Останавливаем и отключаем aperod-node на ${TARGET_IP}…"
ssh "root@${TARGET_IP}" "systemctl disable --now aperod-node 2>/dev/null; echo 'stopped'" || \
  ssh "root@${TARGET_IP}" "systemctl stop aperod-node 2>/dev/null || true; echo 'stopped'"
ok "Нода остановлена (systemd auto-restart отключён)"

# ── Шаг 2: Rsync данных с --delete ────────────────────────
info "Шаг 2/5: Синхронизируем цепь (rsync --delete)…"
info "  Это может занять несколько минут (~1-2 ГБ)"
rsync -az --delete --progress --ignore-errors \
  "${PRIMARY_DATA_DIR}/" \
  "root@${TARGET_IP}:${SECONDARY_DATA_DIR}/"
ok "Rsync завершён"

# ── Шаг 3: Удаляем скопированный p2p identity ─────────────
info "Шаг 3/5: Удаляем скопированный p2p_identity.key…"
ssh "root@${TARGET_IP}" "rm -f ${SECONDARY_DATA_DIR}/p2p_identity.key && echo 'removed'"
ok "p2p_identity.key удалён (новый будет создан при старте)"

# ── Шаг 4: Права и запуск ─────────────────────────────────
info "Шаг 4/5: Устанавливаем права, drop-in конфиги и запускаем ноду…"
ssh "root@${TARGET_IP}" "
  chown -R ${SECONDARY_USER}:${SECONDARY_USER} ${SECONDARY_DATA_DIR}/
  mkdir -p /etc/systemd/system/aperod-node.service.d
  cat > /etc/systemd/system/aperod-node.service.d/timeout.conf << 'DROPIN'
[Service]
TimeoutStopSec=300
DROPIN
  cat > /etc/systemd/system/aperod-node.service.d/gomemlimit.conf << 'DROPIN'
[Service]
Environment=\"GOMEMLIMIT=5368709120\"
DROPIN
  systemctl daemon-reload
  systemctl enable --now aperod-node
  echo 'started'
"
ok "Нода запущена"

# ── Шаг 5: Ожидаем готовности API ─────────────────────────
info "Шаг 5/5: Ожидаем готовности API (key-image rebuild, ~5 мин)…"
ATTEMPT=0
while [[ ${ATTEMPT} -lt ${HEALTH_MAX_ATTEMPTS} ]]; do
  ATTEMPT=$((ATTEMPT + 1))

  STATS=$(ssh "root@${TARGET_IP}" \
    "curl -s --max-time 3 http://127.0.0.1:8545/api/v1/network/stats 2>/dev/null || echo ''"
  )

  if [[ -n "${STATS}" ]] && echo "${STATS}" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    HEIGHT=$(echo "${STATS}" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('height',0))")
    PEERS=$(echo "${STATS}"  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('peer_count',0))")
    echo -e "  Попытка ${ATTEMPT}/${HEALTH_MAX_ATTEMPTS}: height=${HEIGHT} peers=${PEERS}"
    if [[ "${HEIGHT}" -gt 0 ]]; then
      break
    fi
  else
    echo -e "  Попытка ${ATTEMPT}/${HEALTH_MAX_ATTEMPTS}: API ещё не готов, ожидаем…"
  fi
  sleep ${HEALTH_WAIT_SECS}
done

if [[ ${ATTEMPT} -ge ${HEALTH_MAX_ATTEMPTS} ]]; then
  warn "Таймаут ожидания API. Проверьте логи: journalctl -u aperod-node -n 50 --no-pager"
  exit 1
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════╗"
echo -e "  ✓  Узел ${TARGET_IP} подключён к сети"
echo -e "     Height: ${HEIGHT}  |  Peers: ${PEERS}"
echo -e "╚══════════════════════════════════════════════════════════════╝${NC}"
echo

if [[ "${PEERS}" -eq 0 ]]; then
  warn "Peers = 0 — нода загружается, пиры появятся после завершения key-image rebuild (~5 мин)"
  warn "Повторите проверку: ssh root@${TARGET_IP} \"curl -s http://127.0.0.1:8545/api/v1/network/stats\""
else
  ok "Peers = ${PEERS} — сеть работает!"
fi

echo
info "Следующие шаги для нового валидатора:"
echo "  1. Убедитесь что порт 30303/tcp открыт (firewall)"
echo "  2. Пополните reward_address APRO для стейкинга (мин. 100 000 APRO)"
echo "  3. Отправьте StakeTx через кошелёк для регистрации в validator set"
echo "  4. После следующего epoch (~100 блоков) нода начнёт производить блоки"
echo
info "Для синхронизации с другим ключом (не основного узла) добавьте в node.yaml:"
echo "  consensus:"
echo "    non_validator: true   # синхронизация без производства блоков"
echo "    # validator_key: ...  # раскомментировать когда ключ добавлен в validator set"
