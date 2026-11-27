#!/usr/bin/env bash
# ============================================================
#  Aperod — managed replica recovery (operator only).
#  Не используйте для подключения нового валидатора к mainnet:
#  / Do not use for public mainnet onboarding: see deploy/MAINNET.md.
#  Run on the recovery destination, not on the source server.
#
#  Использование:
#    sudo bash aperod-join.sh <SOURCE_HOST>:<API_PORT> [OPTIONS]
#
#  Примеры:
#    # Remote sync requires an SSH tunnel with a pinned host key:
#    sudo bash aperod-join.sh source.example:8545 --tunnel operator@source.example
#
#    # Нестандартная директория данных:
#    sudo bash aperod-join.sh source.example:8545 \
#      --tunnel operator@source.example --data-dir /srv/aperod-replica
#
#  Опции:
#    --api-key    <key>      X-API-Key (предпочтительно задать через APEROD_API_KEY)
#    --tunnel     <user@host> SSH-туннель; обязателен для удалённой загрузки
#    --data-dir   <path>     Директория данных (по умолчанию /var/lib/aperod)
#    --user       <name>     Пользователь-владелец данных (по умолчанию aperod)
#    --p2p-port   <port>     P2P-порт основного узла (по умолчанию 30303)
#    --skip-start            Не запускать aperod-node после загрузки
#    --no-chaindb            Пропустить загрузку chain.db (только snapshot)
#
#  Безопасность передачи:
#    Remote HTTP sync is rejected. Use --tunnel with the primary server's
#    SSH host key already pinned in root's known_hosts. Loopback HTTP is
#    allowed for local test fixtures only. Never accept a first-use host key.
#
#  Что делает скрипт:
#    1. Проверяет, что локальная нода остановлена и chain.db ещё не существует
#    2. При --tunnel: поднимает SSH-туннель с заранее закреплённым host key
#    3. Загружает chain.db и snapshot только во временную staging-директорию
#    4. Проверяет tar-пути, полную высоту chain.db и точное совпадение snapshot
#       с tip chain.db; до этого этапа существующие данные не изменяются
#    5. Переносит проверенную пару в data_dir и создаёт новый P2P identity
#    6. Прописывает PRIMARY_IP как bootnode в /etc/aperod/node.yaml
#    7. Применяет drop-in конфиги systemd и при необходимости запускает ноду
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
API_KEY="${APEROD_API_KEY:-}"
TUNNEL_HOST=""
SKIP_START=false
NO_CHAINDB=false
PRIMARY=""
PRIMARY_P2P_PORT=30303           # P2P port of the primary node (--p2p-port)
TUNNEL_LOCAL_PORT=19545          # ephemeral local port for SSH tunnel
HEALTH_MAX_ATTEMPTS=60           # 5 мин при 5-секундном интервале
HEALTH_WAIT_SECS=5
TUNNEL_PID=""
JOIN_STAGE=""
# NODE_YAML / NODE_CONFIG_SH: overridable via env vars so that tests can
# redirect paths to temp files without root access or a real install.
NODE_YAML="${APEROD_NODE_YAML:-/etc/aperod/node.yaml}"
NODE_CONFIG_SH="${APEROD_NODE_CONFIG_SH:-/opt/aperod/blockchain/deploy/node-config.sh}"

# ── Парсинг аргументов ────────────────────────────────────
if [[ $# -eq 0 ]]; then
  die "Укажите адрес основного узла: bash aperod-join.sh <IP>:<PORT>"
fi

PRIMARY="${1}"
shift

while [[ $# -gt 0 ]]; do
  case "$1" in
    --api-key)    API_KEY="${2:-}";           shift 2 ;;
    --tunnel)     TUNNEL_HOST="${2:-}";       shift 2 ;;
    --data-dir)   DATA_DIR="${2:-}";          shift 2 ;;
    --user)       DATA_USER="${2:-}";         shift 2 ;;
    --p2p-port)   PRIMARY_P2P_PORT="${2:-}";  shift 2 ;;
    --skip-start) SKIP_START=true;            shift   ;;
    --no-chaindb) NO_CHAINDB=true;            shift   ;;
    *) die "Неизвестный аргумент: $1" ;;
  esac
done

[[ -z "${PRIMARY}" ]] && die "Укажите адрес основного узла: <IP>:<PORT>"

# Разбираем PRIMARY на хост и порт для SSH-туннеля
PRIMARY_HOST="${PRIMARY%%:*}"
PRIMARY_PORT="${PRIMARY##*:}"
[[ "${PRIMARY_PORT}" =~ ^[0-9]+$ ]] || PRIMARY_PORT="8545"

# Chain DB and snapshots are consensus-critical bootstrap data. Do not accept
# them over remote plaintext HTTP, even when no API key is configured.
if [[ -z "${TUNNEL_HOST}" && "${PRIMARY_HOST}" != "127.0.0.1" ]]; then
  die "Удалённая загрузка по HTTP запрещена. Используйте --tunnel с заранее закреплённым SSH host key."
fi

# Joining without a config cannot install the validator's whitelist protection.
# Fail before stopping a service or replacing any existing chain data.
[[ -f "${NODE_YAML}" ]] || die "${NODE_YAML} не найден. Сначала установите ноду через install-node.sh; без node.yaml нельзя настроить peer_whitelist."

# ── Ранняя проверка: --no-chaindb требует существующую цепь ────────────────────
# install-node.sh keeps consensus.non_validator: true until canonical mainnet
# identity and validator activation are independently verified. Never remove
# that guard here: a matching local snapshot proves consistency, not mainnet.
if [[ "${NO_CHAINDB}" == "true" && -f "${NODE_YAML}" ]]; then
  NV_FLAG=$(python3 - "${NODE_YAML}" <<'PY' 2>/dev/null || echo "false"
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
print(str((cfg.get('consensus') or {}).get('non_validator', False)).lower())
PY
)
  if [[ "${NV_FLAG}" == "true" && ! -d "${DATA_DIR}/chain.db" ]]; then
    echo
    echo -e "${RED}${BOLD}╔══════════════════════════════════════════════════════════════════╗"
    echo -e "║  ✗  --no-chaindb требует существующую chain.db                   ║"
    echo -e "║     --no-chaindb requires an existing chain.db                    ║"
    echo -e "╠══════════════════════════════════════════════════════════════════╣"
    echo -e "║  Новый узел установлен в безопасном режиме и имеет               ║"
    echo -e "║  The new node is in safe mode with                                ║"
    echo -e "║  consensus.non_validator: true в node.yaml.                      ║"
    echo -e "║  consensus.non_validator: true in node.yaml.                     ║"
    echo -e "║  Без существующей цепи нельзя создавать собственный genesis.     ║"
    echo -e "║  Without an existing chain, do not create a separate genesis.    ║"
    echo -e "║                                                                  ║"
    echo -e "║  Дождитесь проверенного bootstrap mainnet-релиза.                ║"
    echo -e "║  Wait for a verified mainnet bootstrap release.                  ║"
    echo -e "╚══════════════════════════════════════════════════════════════════╝${NC}"
    echo
    die "--no-chaindb требует существующую chain.db; этот скрипт не выбирает mainnet genesis."
  fi
fi

# ── Автоопределение GOMEMLIMIT ────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/gomemlimit-policy.sh"
gomemlimit_resolve

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

# ── Вспомогательная функция: cleanup ─────────────────────
cleanup() {
  if [[ -n "${TUNNEL_PID}" ]]; then
    kill "${TUNNEL_PID}" 2>/dev/null || true
    info "SSH-туннель закрыт (PID ${TUNNEL_PID})"
  fi
  if [[ -n "${JOIN_STAGE}" && -d "${JOIN_STAGE}" ]]; then
    rm -rf -- "${JOIN_STAGE}"
    info "Временная staging-директория удалена"
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
  ssh -o StrictHostKeyChecking=yes \
      -o UserKnownHostsFile=/root/.ssh/known_hosts \
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

# ── Шаг 2: Не трогаем запущенную ноду и существующую цепь ─
info "Шаг 2/7: Проверяем, что локальная нода и её данные не будут перезаписаны…"
if systemctl is-active --quiet aperod-node 2>/dev/null; then
  die "aperod-node сейчас запущен. Присоединение остановлено без изменения службы или данных; сначала проверьте этот сервер."
else
  ok "aperod-node не запущен"
fi

if [[ "${NO_CHAINDB}" == "false" ]]; then
  [[ ! -e "${DATA_DIR}/chain.db" ]] || die \
    "В ${DATA_DIR} уже есть chain.db. Скрипт отказывается заменять существующую цепь."
  if compgen -G "${DATA_DIR}/snapshot-v2-*.json.gz" >/dev/null; then
    die "В ${DATA_DIR} уже есть startup snapshot. Скрипт отказывается менять существующие данные."
  fi
fi
echo

# ── Шаг 3: Подготовка директории данных ──────────────────
info "Шаг 3/7: Подготавливаем директорию данных…"
mkdir -p "${DATA_DIR}"
ok "Директория данных готова: ${DATA_DIR}"
if [[ "${NO_CHAINDB}" == "false" ]]; then
  JOIN_STAGE=$(mktemp -d "${DATA_DIR}/.join-staging.XXXXXX")
  chmod 0700 "${JOIN_STAGE}"
  ok "Создана закрытая staging-директория"
else
  warn "--no-chaindb: существующие chain.db и snapshot не удаляются и не заменяются"
fi
echo

# ── Шаг 4: Скачиваем chain.db ─────────────────────────────
if [[ "${NO_CHAINDB}" == "false" ]]; then
  info "Шаг 4/7: Скачиваем chain.db (~1-2 ГБ, может занять несколько минут)…"
  CHAINDB_TMP="${JOIN_STAGE}/chaindb.tar.gz"

  if api_curl "${BASE_URL}/api/v1/chaindb/export" \
      --progress-bar \
      --max-time 1800 \
      -o "${CHAINDB_TMP}"; then
    gzip -t "${CHAINDB_TMP}" || die "Архив chain.db повреждён (gzip integrity check failed)."
    CHAINDB_UNPACKED_BYTES=$(python3 - "${CHAINDB_TMP}" <<'PY'
import sys, tarfile
from pathlib import PurePosixPath

total = 0
with tarfile.open(sys.argv[1], "r:gz") as archive:
    members = archive.getmembers()
    if not members:
        raise SystemExit("empty chain.db archive")
    for member in members:
        path = PurePosixPath(member.name)
        if path.is_absolute() or ".." in path.parts or not path.parts or path.parts[0] != "chain.db":
            raise SystemExit("unsafe archive path: " + member.name)
        if not (member.isdir() or member.isfile()):
            raise SystemExit("unsupported archive member type: " + member.name)
        total += member.size
        if total > (1 << 63) - 1:
            raise SystemExit("archive size overflows local disk accounting")
print(total)
PY
    ) || die "Архив chain.db содержит недопустимые пути или типы файлов."
    FREE_BYTES=$(df -PB1 "${DATA_DIR}" | awk 'NR == 2 {print $4}')
    [[ "${FREE_BYTES}" =~ ^[0-9]+$ ]] || die "Не удалось проверить свободное место для распаковки."
    REQUIRED_BYTES=$((CHAINDB_UNPACKED_BYTES + 5368709120))
    (( FREE_BYTES > REQUIRED_BYTES )) || die \
      "Недостаточно места для chain.db и резерва 5 ГиБ: свободно ${FREE_BYTES}, требуется больше ${REQUIRED_BYTES} байт."

    info "  Распаковываем проверенный архив в staging…"
    tar --no-same-permissions --no-same-owner -xzf "${CHAINDB_TMP}" -C "${JOIN_STAGE}"
    [[ -d "${JOIN_STAGE}/chain.db" ]] || die "В архиве нет каталога chain.db."
    rm -f "${CHAINDB_TMP}"

    NODE_BIN="${APEROD_NODE_BIN:-/usr/local/bin/aperod-node}"
    [[ -x "${NODE_BIN}" ]] || die "Проверяющий бинарный файл не найден: ${NODE_BIN}"
    info "  Проверяем непрерывность всех сохранённых высот…"
    "${NODE_BIN}" --check-store --data-dir="${JOIN_STAGE}" --max-missing=0
    ok "chain.db прошёл проверку высот и tip"
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
  warn "  Текущие chain.db и snapshot остаются без изменений"
fi
echo

# ── Шаг 5: Скачиваем snapshot ─────────────────────────────
if [[ "${NO_CHAINDB}" == "false" ]]; then
  info "Шаг 5/7: Скачиваем snapshot и проверяем точное совпадение с chain.db…"
  SNAP_HEADER_FILE="${JOIN_STAGE}/.snap-headers.tmp"
  SNAP_TMP="${JOIN_STAGE}/.snapshot-download.tmp"

  if api_curl "${BASE_URL}/api/v1/snapshot/export" \
      --progress-bar \
      --max-time 300 \
      --max-filesize 1073741824 \
      -D "${SNAP_HEADER_FILE}" \
      -o "${SNAP_TMP}"; then
    RAW_FILENAME=$(grep -i "X-Snapshot-Filename:" "${SNAP_HEADER_FILE}" 2>/dev/null \
      | tr -d '\r' | awk '{print $2}' | tr -d ' ')
    if [[ "${RAW_FILENAME}" =~ ^snapshot-v2-[0-9]+\.json\.gz$ ]]; then
      SNAP_FILENAME="${RAW_FILENAME}"
    else
      die "Основной узел вернул некорректное имя snapshot; данные не установлены."
    fi
    gzip -t "${SNAP_TMP}" || die "Startup snapshot повреждён."
    mv "${SNAP_TMP}" "${JOIN_STAGE}/${SNAP_FILENAME}"
    rm -f "${SNAP_HEADER_FILE}"

    NODE_BIN="${APEROD_NODE_BIN:-/usr/local/bin/aperod-node}"
    info "  Проверяем checksum, содержимое и точное совпадение snapshot с tip…"
    "${NODE_BIN}" --check-snapshot --data-dir="${JOIN_STAGE}"
    SNAP_HEIGHT=$(echo "${SNAP_FILENAME}" | grep -oP '(?<=snapshot-v2-)\d+')
    ok "Snapshot совпадает с chain.db: height=${SNAP_HEIGHT}"
  else
    rm -f "${SNAP_TMP}" "${SNAP_HEADER_FILE}"
    die "Не удалось загрузить обязательный snapshot. Staging удалится; существующие данные не затронуты."
  fi
else
  warn "Шаг 5/7: Snapshot не скачивается без chain.db, чтобы не смешать разные состояния"
fi
echo

# ── Шаг 6: Публикуем только предварительно проверенную пару ─
if [[ "${NO_CHAINDB}" == "false" ]]; then
  info "Шаг 6/7: Устанавливаем проверенную пару chain.db + snapshot…"
  SNAP_FILENAME=$(find "${JOIN_STAGE}" -maxdepth 1 -type f -name 'snapshot-v2-*.json.gz' -printf '%f\n' -quit)
  [[ -n "${SNAP_FILENAME}" ]] || die "Проверенный snapshot исчез из staging."
  mv -nT "${JOIN_STAGE}/${SNAP_FILENAME}" "${DATA_DIR}/${SNAP_FILENAME}"
  [[ ! -e "${JOIN_STAGE}/${SNAP_FILENAME}" ]] || die \
    "Snapshot не установлен: целевой файл уже существует. Существующие данные сохранены."
  mv -nT "${JOIN_STAGE}/chain.db" "${DATA_DIR}/chain.db"
  if [[ -e "${JOIN_STAGE}/chain.db" ]]; then
    rm -f "${DATA_DIR}/${SNAP_FILENAME}"
    die "Не удалось установить chain.db; snapshot откатан, staging будет удалён."
  fi

  # The joining node must create its own P2P identity, never copy the source's.
  rm -f "${DATA_DIR}/p2p_identity.key"
  ok "Проверенная пара установлена; исходные данные не перезаписывались"
else
  info "Шаг 6/7: Существующие chain.db, snapshot и P2P identity сохранены"
fi

if id "${DATA_USER}" &>/dev/null; then
  if [[ "${NO_CHAINDB}" == "false" ]]; then
    chown -R "${DATA_USER}:${DATA_USER}" "${DATA_DIR}/chain.db" "${DATA_DIR}/${SNAP_FILENAME}"
  fi
  ok "Права установлены: ${DATA_USER}:${DATA_USER}"
else
  warn "Пользователь '${DATA_USER}' не существует — права не изменены"
  warn "Создайте: useradd --system --no-create-home --shell /usr/sbin/nologin ${DATA_USER}"
fi
echo

# ── Шаг 7: Прописываем bootnode в node.yaml ──────────────
# Без этого шага у нового узла нет записей в p2p.bootnodes.
# Оба узла (основной и новый) ждут входящего подключения и
# никогда не устанавливают соединение — peer_count=0 навсегда.
BOOTNODE_ADDR="/ip4/${PRIMARY_HOST}/tcp/${PRIMARY_P2P_PORT}"
info "Шаг 7/8: Прописываем bootnode ${BOOTNODE_ADDR} в ${NODE_YAML}…"

if [[ ! -f "${NODE_YAML}" ]]; then
  die "${NODE_YAML} не найден — bootnode и peer_whitelist не настроены."
else
  # Preferred path: node-config.sh (YAML-safe, idempotent, handles legacy migration).
  if [[ -x "${NODE_CONFIG_SH}" ]]; then
    APEROD_CONFIG="${NODE_YAML}" bash "${NODE_CONFIG_SH}" add-bootnode "${BOOTNODE_ADDR}"
  else
    # Fallback: inline Python — same logic as node-config.sh and join-network.sh.
    python3 - "${NODE_YAML}" "${BOOTNODE_ADDR}" <<'PY'
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes.
# The Go runtime only reads cfg.P2P.Bootnodes (yaml:"bootnodes" under p2p:);
# a root-level key is silently ignored and the node stays isolated.
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
  ok "Bootnode ${BOOTNODE_ADDR} прописан в ${NODE_YAML}"

  # ── Прописываем IP основного узла в peer_whitelist ───────────────────────
  # Предотвращает накопление bad-block страйков (→ 24-ч бан) пока новый узел
  # догоняет цепь и не может принять gossip-блоки основного узла.
  # peer_whitelist исключает PRIMARY_HOST из счётчика страйков.
  info "Шаг 7.5/8: Прописываем ${PRIMARY_HOST} в p2p.peer_whitelist…"
  python3 - "${NODE_YAML}" "${PRIMARY_HOST}" "${DATA_DIR}" <<'PY'
import sys, yaml, os, json, ipaddress, stat, subprocess, tempfile
cfg_path     = sys.argv[1]
primary_ip   = sys.argv[2]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}
p2p = cfg.setdefault("p2p", {})
validator_ip = primary_ip
ipaddress.IPv4Address(validator_ip)
# A nonempty runtime sidecar overrides YAML. Merge the joining validator into
# the target's policy too; do not delete existing operator entries or import
# the source host's policy. "-" explicitly disables sidecar persistence.
sidecar = p2p.get("whitelist_file") or os.path.join(cfg.get("data_dir") or sys.argv[3], "p2p_whitelist.json")
if sidecar != "-":
    if not os.path.isabs(sidecar):
        cwd = subprocess.check_output(["systemctl", "show", "aperod-node", "--property=WorkingDirectory", "--value"], text=True).strip() or "/"
        if not os.path.isabs(cwd):
            raise ValueError("aperod-node WorkingDirectory must be absolute")
        sidecar = os.path.join(cwd, sidecar)
    if os.path.lexists(sidecar):
        with open(sidecar) as f:
            entries = json.load(f)
        if not isinstance(entries, list) or any(not isinstance(e, str) for e in entries):
            raise ValueError("Invalid whitelist sidecar: expected IP/CIDR array")
        for entry in entries:
            ipaddress.ip_network(entry, strict=False)
        if validator_ip not in entries:
            entries.append(validator_ip)
            meta = os.stat(sidecar)
            fd, tmp_sidecar = tempfile.mkstemp(prefix=".join-whitelist-", dir=os.path.dirname(sidecar))
            try:
                with os.fdopen(fd, "w") as f:
                    os.fchown(f.fileno(), meta.st_uid, meta.st_gid)
                    os.fchmod(f.fileno(), stat.S_IMODE(meta.st_mode))
                    json.dump(entries, f)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(tmp_sidecar, sidecar)
            finally:
                if os.path.exists(tmp_sidecar):
                    os.unlink(tmp_sidecar)
wl  = list(p2p.get("peer_whitelist") or [])
if primary_ip not in wl:
    wl.append(primary_ip)
    p2p["peer_whitelist"] = wl
    tmp = cfg_path + ".tmp"
    with open(tmp, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    os.replace(tmp, cfg_path)
    print(f"[OK]   p2p.peer_whitelist updated: {wl}")
else:
    print(f"[OK]   p2p.peer_whitelist already contains {primary_ip} — skipping")
PY
  ok "IP основного узла ${PRIMARY_HOST} добавлен в peer_whitelist"

  # A verified archive and matching local snapshot establish consistency, not
  # canonical chain identity or validator eligibility. Keep the safe-mode guard.
  info "consensus.non_validator оставлен без изменений до подтверждения genesis и допуска валидатора"
  info "consensus.non_validator stays enabled until genesis and validator eligibility are verified"
fi
echo

# ── Шаг 8: systemd drop-in конфиги ───────────────────────
info "Шаг 8/8: Применяем systemd drop-in конфиги…"
# APEROD_DROPIN_DIR lets tests redirect to a writable temp directory so the
# full script can run end-to-end without root or a real systemd installation.
DROPIN_DIR="${APEROD_DROPIN_DIR:-/etc/systemd/system/aperod-node.service.d}"
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

# ── Активируем watchdog и таймер перезапуска ─────────────────────────────────
# install-node.sh копирует файлы таймеров, но не запускает их когда --primary-ip
# не был передан — чтобы не допустить запуска ноды до получения корректного
# chain.db.  После успешного join нода работает в сети, поэтому безопасно
# выполнить enable + start.  Для нод, установленных с --primary-ip, таймеры уже
# активны — enable --now идемпотентен и просто убедится в этом.
#
# Когда вызван --skip-start (нода не запускалась), таймеры тоже НЕ активируем:
# watchdog сразу сделает `systemctl restart aperod-node` после первого
# неудачного health-probe, что означает запуск ноды вопреки --skip-start.
if [[ "${SKIP_START}" == "false" ]]; then
  for _timer in aperod-node-watchdog.timer aperod-node-sched-restart.timer; do
    if systemctl list-unit-files --quiet "${_timer}" &>/dev/null; then
      systemctl enable --now "${_timer}" && ok "${_timer} включён и запущен"
    fi
  done
  unset _timer
else
  info "Таймеры watchdog/sched-restart не активированы (--skip-start; запустите вручную после старта ноды)"
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════════╗"
echo -e "  ✓  Локальная проверка и настройка подключения завершены"
echo -e "     Local checks and connection configuration are complete"
if [[ "${SKIP_START}" == "false" ]]; then
  echo -e "     Height: ${HEIGHT}  |  Peers: ${PEERS}"
fi
echo -e "     Genesis mainnet и регистрация валидатора этим скриптом не подтверждаются."
echo -e "     This script does not verify the mainnet genesis or validator registration."
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
