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
#    3. Исключает p2p_identity.key из rsync и удаляет остаток (новый генерируется при старте)
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
PRIMARY_P2P_PORT=30303
SECONDARY_NODE_YAML="/etc/aperod/node.yaml"
SECONDARY_NODE_CONFIG_SH="/opt/aperod/blockchain/deploy/node-config.sh"

# ── Аргументы ─────────────────────────────────────────────
TARGET_IP="${1:-}"
[[ -z "${TARGET_IP}" ]] && die "Укажите IP нового сервера: bash join-network.sh <IP>"

# PRIMARY_IP — IP-адрес ЭТОГО сервера (откуда запускается скрипт).
# Переопределите переменной окружения PRIMARY_IP если auto-detect даёт
# неверный адрес (например, внутренний 10.x вместо внешнего IP).
if [[ -z "${PRIMARY_IP:-}" ]]; then
  # Explicitly pick the first IPv4 address (skip IPv6 addresses).
  # hostname -I can return IPv6 first on dual-stack hosts, but /ip4/.../tcp/...
  # multiaddrs require a dotted-quad IPv4 address.
  PRIMARY_IP=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
  if [[ -z "${PRIMARY_IP}" ]]; then
    die "Не удалось определить IPv4-адрес этого сервера.
  Укажите вручную: PRIMARY_IP=<IPv4> bash join-network.sh <TARGET_IP>"
  fi
fi
# Validate that PRIMARY_IP is a dotted-quad to catch accidental IPv6 overrides.
if ! echo "${PRIMARY_IP}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
  die "PRIMARY_IP='${PRIMARY_IP}' не является IPv4-адресом.
  /ip4/.../tcp/... multiaddr требует dotted-quad IPv4. Укажите IPv4: PRIMARY_IP=<IPv4> bash join-network.sh <TARGET_IP>"
fi
PRIMARY_BOOTNODE="/ip4/${PRIMARY_IP}/tcp/${PRIMARY_P2P_PORT}"

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

info "Целевой сервер:   ${TARGET_IP}"
info "Источник данных:  ${PRIMARY_DATA_DIR}"
info "Назначение:       ${TARGET_IP}:${SECONDARY_DATA_DIR}"
info "Bootnode:         ${PRIMARY_BOOTNODE}"
echo

# ── Шаг 1: Останавливаем ноду на целевом сервере ──────────
info "Шаг 1/7: Останавливаем и отключаем aperod-node на ${TARGET_IP}…"
ssh "root@${TARGET_IP}" "systemctl disable --now aperod-node 2>/dev/null; echo 'stopped'" || \
  ssh "root@${TARGET_IP}" "systemctl stop aperod-node 2>/dev/null || true; echo 'stopped'"
ok "Нода остановлена (systemd auto-restart отключён)"

# ── Шаг 2: Останавливаем ноду на ИСТОЧНИКЕ перед rsync ───
# LevelDB небезопасно копировать на ходу: WAL-записи и
# компакция .ldb создают внутренне несогласованную копию,
# которая восстанавливается на меньшей высоте и расходится
# с основной цепью. Остановка занимает ~60 секунд.
info "Шаг 2/7: Останавливаем aperod-node на ИСТОЧНИКЕ (этот сервер)…"
info "  ⚠ Кратковременный простой ~60 с — LevelDB нельзя копировать на ходу"
if ! systemctl stop aperod-node 2>/dev/null; then
  die "Не удалось остановить aperod-node на источнике. Прерываем — \
rsync живой LevelDB приведёт к повреждению данных на целевом сервере."
fi
# Убеждаемся что сервис действительно остановлен
for _i in $(seq 1 15); do
  systemctl is-active --quiet aperod-node || break
  sleep 1
done
if systemctl is-active --quiet aperod-node; then
  systemctl start aperod-node 2>/dev/null || true
  die "aperod-node на источнике не остановился за 15 с. Прерываем."
fi
ok "aperod-node на источнике остановлен"

# ── Шаг 3: Rsync данных с --delete ────────────────────────
info "Шаг 3/7: Синхронизируем цепь (rsync --delete)…"
info "  Это может занять несколько минут (~1-2 ГБ)"
rsync -az --delete --progress --ignore-errors \
  --exclude='p2p_identity.key' \
  "${PRIMARY_DATA_DIR}/" \
  "root@${TARGET_IP}:${SECONDARY_DATA_DIR}/"
ok "Rsync завершён"

# ── Перезапускаем ноду на ИСТОЧНИКЕ ───────────────────────
info "Перезапускаем aperod-node на источнике…"
systemctl start aperod-node 2>/dev/null || warn "Не удалось запустить aperod-node на источнике — проверьте вручную"
ok "aperod-node на источнике запущен"

# ── Шаг 4: Удаляем скопированный p2p identity ─────────────
info "Шаг 4/7: Удаляем скопированный p2p_identity.key…"
ssh "root@${TARGET_IP}" "rm -f ${SECONDARY_DATA_DIR}/p2p_identity.key && echo 'removed'"
ok "p2p_identity.key удалён (новый будет создан при старте)"

# ── Шаг 5: Прописываем bootnode в node.yaml нового узла ───
# Без этого шага secondary имеет bootnodes: [] и не инициирует
# P2P-подключение к primary — оба узла ждут входящего dial и
# никогда не видят друг друга (peer_count=0 навсегда).
info "Шаг 5/7: Прописываем bootnode ${PRIMARY_BOOTNODE} в ${SECONDARY_NODE_YAML}…"
ssh "root@${TARGET_IP}" bash <<ENDSSH
set -euo pipefail
NODE_YAML="${SECONDARY_NODE_YAML}"
BOOTNODE="${PRIMARY_BOOTNODE}"
NODE_CONFIG_SH="${SECONDARY_NODE_CONFIG_SH}"

# node.yaml must already exist from install-validator.sh / install-node.sh.
# Creating a stripped file here would silently discard network, data_dir,
# consensus, API and genesis settings and cause the service to fail to start.
if [[ ! -f "\${NODE_YAML}" ]]; then
  echo "[ERR]  \${NODE_YAML} не найден на целевом сервере." >&2
  echo "       Установите ноду через install-validator.sh (или install-node.sh)" >&2
  echo "       перед запуском join-network.sh." >&2
  exit 1
fi

# Always write the bootnode to p2p.bootnodes — the only field read by
# config.go P2PConfig (yaml:"bootnodes" under p2p:).
# Both node-config.sh and the Python fallback migrate any legacy root-level
# 'bootnodes' key (produced by older install-node.sh) into p2p.bootnodes
# and remove the ignored root key so the node actually dials the listed peers.

# Preferred path: node-config.sh (YAML-safe, idempotent, handles migration).
if [[ -x "\${NODE_CONFIG_SH}" ]]; then
  APEROD_CONFIG="\${NODE_YAML}" bash "\${NODE_CONFIG_SH}" add-bootnode "\${BOOTNODE}"
else
  # Fallback: python3 directly — same migration logic as node-config.sh.
  python3 - "\${NODE_YAML}" "\${BOOTNODE}" <<'PY'
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes.
legacy = cfg.pop("bootnodes", None)

p2p = cfg.setdefault("p2p", {})
nodes = list(p2p.get("bootnodes") or [])

if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in nodes:
            nodes.append(entry)

if bootnode not in nodes:
    nodes.append(bootnode)
p2p["bootnodes"] = nodes

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
print(f"[OK]   p2p.bootnodes updated: {nodes}")
PY
fi
ENDSSH
ok "Bootnode ${PRIMARY_BOOTNODE} прописан в ${SECONDARY_NODE_YAML}"

# ── Шаг 6: Права и запуск ─────────────────────────────────
info "Шаг 6/7: Устанавливаем права, drop-in конфиги и запускаем ноду…"
ssh "root@${TARGET_IP}" "
  chown -R ${SECONDARY_USER}:${SECONDARY_USER} ${SECONDARY_DATA_DIR}/
  mkdir -p /etc/systemd/system/aperod-node.service.d
  cat > /etc/systemd/system/aperod-node.service.d/timeout.conf << 'DROPIN'
# Aperod node — shutdown timeout drop-in
# Install path: /etc/systemd/system/aperod-node.service.d/timeout.conf
#
# TimeoutStopSec=900 gives the UTXO snapshot up to 15 minutes to flush
# before systemd sends SIGKILL.  The Aug 2026 outage was caused by a
# 300 s value triggering SIGKILL mid-write on a 5.7 GB RAM node.
[Service]
TimeoutStopSec=900
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

# ── Шаг 7: Ожидаем готовности API ─────────────────────────
info "Шаг 7/7: Ожидаем готовности API (key-image rebuild, ~5 мин)…"
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
