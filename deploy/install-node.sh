#!/usr/bin/env bash
# ============================================================
#  Aperod APRO Full Node — Automatic Installer (Non-Validator)
#  Поддерживается: Ubuntu 22.04 / 24.04 / Debian 12
#  Использование:  sudo bash install-node.sh
# ============================================================
set -euo pipefail

# ── Цвета вывода ──────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m';  BOLD='\033[1m';  NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[ERR]${NC}   $*"; exit 1; }

# ── Параметры ─────────────────────────────────────────────
INSTALL_DIR="/opt/aperod"
DATA_DIR="/var/lib/aperod"
CONFIG_DIR="/etc/aperod"
WALLET_DIR="/etc/aperod/wallet"
GO_VERSION="1.23.4"
REPO_URL="https://github.com/aperod-network/aperod-node.git"
P2P_PORT=30303
RPC_PORT=8545

# Resolve script directory early — referenced in step 8b (bootnode) and later
# steps (watchdog, backup, etc.).  Must be set before any ${SCRIPT_DIR} use.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ── Аргументы командной строки ────────────────────────────
# --primary-ip <IP>   Публичный IP основного (primary) узла.
#                     Если указан, сразу прописывается как bootnode в node.yaml.
#                     Если не указан — нода стартует без bootnode; запустите
#                     aperod-join.sh отдельно до первого старта ноды.
PRIMARY_NODE_IP=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --primary-ip)
      PRIMARY_NODE_IP="${2:-}"
      [[ -n "${PRIMARY_NODE_IP}" ]] || die "--primary-ip требует значение (IP-адрес)"
      # Validate: must be a bare IPv4 address (4 octets, each 0-255, no leading zeros)
      if ! [[ "${PRIMARY_NODE_IP}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; then
        die "--primary-ip '${PRIMARY_NODE_IP}' не является корректным IPv4-адресом (ожидается формат A.B.C.D)"
      fi
      IFS='.' read -r _o1 _o2 _o3 _o4 <<< "${PRIMARY_NODE_IP}"
      for _oct in "${_o1}" "${_o2}" "${_o3}" "${_o4}"; do
        # Reject leading zeros (e.g. "08") — they are ambiguous and not standard
        if [[ "${_oct}" =~ ^0[0-9] ]]; then
          die "--primary-ip '${PRIMARY_NODE_IP}': октет '${_oct}' содержит ведущий ноль (используйте ${_oct#0} вместо ${_oct})"
        fi
        # Force base-10 to avoid octal interpretation in arithmetic context
        if (( 10#${_oct} > 255 )); then
          die "--primary-ip '${PRIMARY_NODE_IP}': октет ${_oct} вне диапазона 0-255"
        fi
      done
      unset _o1 _o2 _o3 _o4 _oct
      shift 2
      ;;
    *)
      die "Неизвестный аргумент: $1. Использование: sudo bash install-node.sh [--primary-ip <IP>]"
      ;;
  esac
done

# ── Лимит памяти Go-рантайма (GOMEMLIMIT) ─────────────────
# По умолчанию — 75 % от общей RAM хоста, но не меньше 1.5 ГБ и не больше
# 5.5 ГБ (значение, проверенное на продакшн-ноде с 7.8 ГБ RAM).
# Переопределить: GOMEMLIMIT_BYTES=3221225472 sudo bash install-node.sh
TOTAL_RAM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
TOTAL_RAM_BYTES=$(( TOTAL_RAM_KB * 1024 ))
AUTO_GOMEMLIMIT=$(( TOTAL_RAM_BYTES * 3 / 4 ))
MIN_GOMEMLIMIT=$(( 1536 * 1024 * 1024 ))   # 1.5 GiB floor
MAX_GOMEMLIMIT=$(( 6979321856 ))            # 6500 MiB ceiling — bumped Aug 2026 after UTXO set at 980k+ blocks hit 4.7 GB startup RSS; 5500 MiB caused GC thrash at 265% CPU
if (( AUTO_GOMEMLIMIT < MIN_GOMEMLIMIT )); then AUTO_GOMEMLIMIT=${MIN_GOMEMLIMIT}; fi
if (( AUTO_GOMEMLIMIT > MAX_GOMEMLIMIT )); then AUTO_GOMEMLIMIT=${MAX_GOMEMLIMIT}; fi
GOMEMLIMIT_BYTES="${GOMEMLIMIT_BYTES:-${AUTO_GOMEMLIMIT}}"

echo -e "
${BOLD}╔════════════════════════════════════════════╗
║   Aperod APRO Full Node — Installer        ║
║   github.com/aperod-network/aperod-node    ║
╚════════════════════════════════════════════╝${NC}
"

# ── Проверка root ─────────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Запустите скрипт от root: sudo bash install-node.sh"
fi

# ── Проверка ОС ───────────────────────────────────────────
if ! grep -qE "(Ubuntu|Debian)" /etc/os-release 2>/dev/null; then
  warn "Скрипт тестировался на Ubuntu 22.04/24.04 и Debian 12. Продолжить? [y/N]"
  read -rp "" CONFIRM
  [[ "${CONFIRM,,}" == "y" ]] || die "Установка отменена"
fi

# ── 1. Зависимости ────────────────────────────────────────
info "Обновляем пакеты и устанавливаем зависимости…"
apt-get update -q
apt-get install -y -q git curl wget build-essential ufw jq
ok "Зависимости установлены"

# ── 2. Go ─────────────────────────────────────────────────
need_go=false
if command -v go &>/dev/null; then
  GO_INSTALLED=$(go version 2>/dev/null | awk '{print $3}' | tr -d 'go')
  if [[ "${GO_INSTALLED}" < "1.22" ]]; then
    warn "Установлен Go ${GO_INSTALLED} — требуется 1.22+. Обновляем…"
    need_go=true
  else
    ok "Go ${GO_INSTALLED} уже установлен"
    export PATH="$PATH:/usr/local/go/bin"
  fi
else
  need_go=true
fi

if ${need_go}; then
  info "Устанавливаем Go ${GO_VERSION}…"
  ARCH=$( [[ $(uname -m) == "aarch64" ]] && echo "arm64" || echo "amd64" )
  wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -O /tmp/go.tar.gz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tar.gz
  rm -f /tmp/go.tar.gz
  echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
  export PATH="$PATH:/usr/local/go/bin"
  ok "Go ${GO_VERSION} установлен"
fi

# ── 2b. Системный пользователь aperod ────────────────────
# Created here — before the git clone — because the clone runs as `aperod`
# (so the working tree is owned by the pull user from the start) and
# `sudo -u aperod` requires the account to exist.
if ! id aperod &>/dev/null; then
  useradd --system \
          --no-create-home \
          --shell /usr/sbin/nologin \
          --home-dir "${DATA_DIR}" \
          aperod
  ok "Системный пользователь aperod создан"
else
  ok "Пользователь aperod уже существует — пропускаем"
fi

# ── 3. Клонирование репозитория ───────────────────────────
# We use `git clone` (not a tarball) so the install directory is a proper
# git checkout.  This is required for two reasons:
#
#   a) `update-node.sh` later runs `git pull` to receive updates.
#   b) The post-merge hook (step 12b) is installed into `.git/hooks/`.
#      Without a git repo the hook directory does not exist and the hook
#      cannot fire after bare `git pull` calls — which is the exact failure
#      mode this task exists to prevent.
#
# Three cases handled explicitly:
#   1. .git exists → pull (idempotent re-run or update).
#   2. Directory is non-empty but has no .git → prior tarball install detected;
#      print clear migration instructions and abort so the operator decides.
#   3. Directory is absent or empty → fresh clone.
info "Получаем исходный код Aperod (git clone)…"
REPO_URL_GIT="https://github.com/aperod-network/aperod-node.git"
mkdir -p "${INSTALL_DIR}"

if [[ -d "${INSTALL_DIR}/.git" ]]; then
  # ── Case 1: existing git repo — pull latest ──────────────────────────────
  info "  Репозиторий уже существует — обновляем через git pull…"
  chown -R aperod:aperod "${INSTALL_DIR}"
  sudo -u aperod git -C "${INSTALL_DIR}" pull --ff-only \
    || die "git pull не удался. Проверьте подключение к GitHub."
  ok "Репозиторий обновлён в ${INSTALL_DIR}"

elif [[ -n "$(ls -A "${INSTALL_DIR}" 2>/dev/null)" ]]; then
  # ── Case 2: non-empty directory without .git → prior tarball install ─────
  # git clone would fail on a non-empty target.  Require the operator to
  # convert explicitly so we never silently overwrite production data.
  echo ""
  echo -e "${RED}═══════════════════════════════════════════════════${NC}"
  echo -e "${RED}  Обнаружена существующая установка (не git-репо)  ${NC}"
  echo -e "${RED}═══════════════════════════════════════════════════${NC}"
  echo ""
  echo "  ${INSTALL_DIR} содержит файлы, но не является git-репозиторием."
  echo "  Вероятно, это старая установка через тарбол."
  echo ""
  echo "  ── Как конвертировать в git-репозиторий ──────────────"
  echo "  cd ${INSTALL_DIR}"
  echo "  git init"
  echo "  git remote add origin ${REPO_URL_GIT}"
  echo "  git fetch --depth=1 origin main"
  echo "  git reset --hard origin/main"
  echo "  sudo bash deploy/install-node.sh   # повторный запуск"
  echo "  ──────────────────────────────────────────────────────"
  echo ""
  die "Прервано. Выполните конвертацию выше, затем повторите запуск."

else
  # ── Case 3: fresh install — clone as aperod ──────────────────────────────
  # Clone as the aperod user so the working tree and .git directory are owned
  # by the pull user from the start.  The aperod account was created at step
  # 2b; `sudo -u aperod` is now safe to use.
  chown aperod:aperod "${INSTALL_DIR}"
  sudo -u aperod git clone --depth=1 "${REPO_URL_GIT}" "${INSTALL_DIR}" \
    || die "git clone не удался. Проверьте подключение к GitHub."
  ok "Репозиторий клонирован в ${INSTALL_DIR}"
fi

# ── 4. Сборка бинарников ──────────────────────────────────
info "Компилируем aperod-node и aperod CLI (1–3 минуты)…"
cd "${INSTALL_DIR}"
export GOPATH="/root/go"

make deps 2>&1 | tail -3
make build 2>&1 | tail -8

if [[ ! -f "build/aperod-node" || ! -f "build/aperod" ]]; then
  die "Сборка не удалась — проверьте вывод выше"
fi

# ── 4b. Проверка статической линковки ────────────────────
# Если CGO_ENABLED=0 случайно убрали из Makefile, скомпилированный бинарник
# будет динамически слинкован с glibc хоста сборки. На старых production-хостах
# (Debian 11, Ubuntu 20.04) с glibc 2.31 он немедленно упадёт при старте.
#
# Проверяем ДО установки и запуска сервиса, чтобы ошибка была очевидна,
# а не проявлялась как crash-loop уже после установки.
#
# Основная проверка: ldd (есть на всех glibc-дистрибутивах).
#   Статический бинарник → "not a dynamic executable"
#   Динамический бинарник → список зависимостей (проверка срабатывает)
#
# Запасная проверка (при отсутствии ldd, например musl Alpine): readelf -l
# ищет заголовок программы PT_INTERP напрямую в ELF.
info "Проверяем статическую линковку бинарника aperod-node…"

_binary_is_dynamic=false

if command -v ldd > /dev/null 2>&1; then
  _ldd_out=$(ldd "build/aperod-node" 2>&1 || true)
  if echo "${_ldd_out}" | grep -q "not a dynamic executable"; then
    ok "ldd: бинарник статически слинкован ✓"
  else
    _binary_is_dynamic=true
    echo "  Вывод ldd:" >&2
    echo "${_ldd_out}" | sed 's/^/    /' >&2
  fi
elif command -v readelf > /dev/null 2>&1; then
  if readelf -l "build/aperod-node" 2>/dev/null | grep -q 'INTERP'; then
    _binary_is_dynamic=true
    echo "  readelf: обнаружен сегмент PT_INTERP — бинарник имеет путь к динамическому линкеру" >&2
  else
    ok "readelf: сегмент PT_INTERP отсутствует — бинарник статически слинкован ✓"
  fi
else
  warn "Ни ldd, ни readelf не найдены — пропускаем проверку статической линковки."
  warn "Убедитесь, что в Makefile в цели build-node установлен CGO_ENABLED=0."
fi

if [[ "${_binary_is_dynamic}" == "true" ]]; then
  echo "" >&2
  echo -e "${RED}═══════════════════════════════════════════════════════════${NC}" >&2
  echo -e "${RED}  Проверка статической линковки ПРОВАЛИЛАСЬ                ${NC}" >&2
  echo -e "${RED}═══════════════════════════════════════════════════════════${NC}" >&2
  echo "" >&2
  echo "  build/aperod-node динамически слинкован." >&2
  echo "  Установка бинарника ПРЕРВАНА — сервис НЕ будет запущен." >&2
  echo "" >&2
  echo "  Исправление: убедитесь, что в Makefile в цели build-node" >&2
  echo "  установлен CGO_ENABLED=0, затем повторите запуск install-node.sh." >&2
  echo "" >&2
  die "Динамически слинкованный бинарник не может быть установлен"
fi

cp build/aperod-node /usr/local/bin/aperod-node
cp build/aperod      /usr/local/bin/aperod
chmod +x /usr/local/bin/aperod-node /usr/local/bin/aperod
ok "Бинарники установлены: /usr/local/bin/aperod-node, /usr/local/bin/aperod"

# ── 5. Создаём директории ─────────────────────────────────
mkdir -p "${CONFIG_DIR}" "${DATA_DIR}" "${WALLET_DIR}"

# ── 5b. Права директории данных ──────────────────────────
# The aperod system account was already created at step 2b.
# Assign ownership of the data directory to aperod so the service can write
# snapshots and the block chain (ReadWritePaths in the unit file).
chown -R aperod:aperod "${DATA_DIR}"
chmod 750 "${DATA_DIR}"

# ── 6. Кошелёк ────────────────────────────────────────────
echo
echo -e "${BOLD}═══════════════════════════════════════════════"
echo -e "  Настройка кошелька"
echo -e "═══════════════════════════════════════════════${NC}"

WALLET_FILE="${WALLET_DIR}/default.json"
WALLET_ADDR=""

if [[ -f "${WALLET_FILE}" ]]; then
  warn "Файл кошелька уже существует: ${WALLET_FILE}"
  read -rp "Использовать существующий? [Y/n]: " USE_EXISTING
  if [[ "${USE_EXISTING,,}" != "n" ]]; then
    WALLET_ADDR=$(jq -r '.address // empty' "${WALLET_FILE}" 2>/dev/null || "")
    ok "Используем существующий кошелёк"
  fi
fi

if [[ -z "${WALLET_ADDR}" ]]; then
  echo
  echo "  1) Создать новый кошелёк (рекомендуется)"
  echo "  2) Импортировать существующий приватный ключ (hex, 64 символа)"
  echo
  read -rp "Выбор [1/2]: " W_CHOICE

  if [[ "${W_CHOICE}" == "2" ]]; then
    echo
    read -rsp "  Введите приватный ключ (64 hex-символа, ввод скрыт): " PRIVKEY
    echo
    if [[ ! "${PRIVKEY}" =~ ^[0-9a-fA-F]{64}$ ]]; then
      die "Неверный формат: ожидается 64 hex-символа"
    fi
    aperod wallet import --key "${PRIVKEY}" --out "${WALLET_FILE}" 2>/dev/null || \
      echo -n "${PRIVKEY}" > "${WALLET_FILE}"
    ok "Приватный ключ импортирован"
  else
    info "Генерируем новый кошелёк…"
    aperod wallet create --out "${WALLET_FILE}" 2>/dev/null || true

    # Если команда создала файл — читаем адрес из него
    if [[ -f "${WALLET_FILE}" ]]; then
      WALLET_ADDR=$(jq -r '.address // empty' "${WALLET_FILE}" 2>/dev/null || "")
    fi
    ok "Кошелёк создан"
  fi

  chmod 600 "${WALLET_FILE}"
  chown root:root "${WALLET_FILE}"

  # Получить адрес
  if [[ -z "${WALLET_ADDR}" ]]; then
    WALLET_ADDR=$(aperod wallet address 2>/dev/null || "см. файл ${WALLET_FILE}")
  fi
fi

# ── 7. Определяем внешний IP ──────────────────────────────
MY_IP=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null \
     || curl -s --connect-timeout 5 icanhazip.com 2>/dev/null \
     || echo "0.0.0.0")

if [[ "${MY_IP}" == "0.0.0.0" ]]; then
  warn "Не удалось определить внешний IP автоматически."
  read -rp "Введите публичный IP этого сервера (или Enter чтобы пропустить): " MY_IP
  MY_IP="${MY_IP:-0.0.0.0}"
fi
info "Внешний IP: ${MY_IP}"

# ── 8. Конфигурация ноды ──────────────────────────────────
cat > "${CONFIG_DIR}/node.yaml" <<EOF
# Aperod Node Configuration
# Автоматически создан install-node.sh $(date -u +"%Y-%m-%d %H:%M UTC")

network: testnet
data_dir: ${DATA_DIR}
log_level: info

p2p:
  listen: /ip4/0.0.0.0/tcp/${P2P_PORT}
  external: /ip4/${MY_IP}/tcp/${P2P_PORT}
  max_peers: 30
  # bootnodes: populated by aperod-join.sh or --primary-ip flag.
  # Do NOT start the node until aperod-join.sh has been run.
  bootnodes: []

rpc:
  # ВАЖНО: только localhost! Никогда не меняйте на 0.0.0.0
  listen: 127.0.0.1:${RPC_PORT}
  cors_origins: []

wallet:
  key_file: ${WALLET_FILE}

# ── Обрезка старых блоков (pruning) ──────────────────────
# Хранить только последние N блоков, старые удалять автоматически.
# Рекомендуется для нод с ограниченным диском.
#
#   100000 блоков ≈ 11–50 дней истории, ~2–5 ГБ — для обычных пользователей
#   500000 блоков ≈ 2–8 месяцев,       ~10–25 ГБ — для платёжных шлюзов
#   0      = pruning отключён, хранить всю историю (архивная нода)
#
pruning:
  enabled: true
  keep_blocks: 100000   # изменить при необходимости

metrics:
  enabled: true
  listen: 127.0.0.1:9090
EOF

ok "Конфигурация сохранена: ${CONFIG_DIR}/node.yaml"

# ── 8b. Bootnode из --primary-ip ──────────────────────────
# Если оператор передал --primary-ip, немедленно прописываем основной узел
# как bootnode через node-config.sh, чтобы нода не стартовала без пиров.
# Если --primary-ip не передан — выводим заметное предупреждение.
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  BOOTNODE_ADDR="/ip4/${PRIMARY_NODE_IP}/tcp/${P2P_PORT}"
  info "Прописываем bootnode из --primary-ip: ${BOOTNODE_ADDR}"
  bash "${SCRIPT_DIR}/node-config.sh" add-bootnode "${BOOTNODE_ADDR}" \
    || die "Не удалось добавить bootnode ${BOOTNODE_ADDR} — проверьте ${CONFIG_DIR}/node.yaml"
  ok "Bootnode добавлен: ${BOOTNODE_ADDR}"
else
  echo
  echo -e "${YELLOW}${BOLD}╔══════════════════════════════════════════════════════════╗"
  echo -e "║  ⚠  ВНИМАНИЕ: bootnode не настроен                        ║"
  echo -e "╠══════════════════════════════════════════════════════════╣"
  echo -e "║  Нода стартует без пиров и может сформировать отдельную  ║"
  echo -e "║  цепь с несовместимым genesis-блоком.                    ║"
  echo -e "║                                                          ║"
  echo -e "║  Запустите ноду ТОЛЬКО ПОСЛЕ выполнения aperod-join.sh:  ║"
  echo -e "║                                                          ║"
  echo -e "║    sudo bash /opt/aperod/deploy/aperod-join.sh \\         ║"
  echo -e "║      <PRIMARY_IP>:8545                                   ║"
  echo -e "║                                                          ║"
  echo -e "║  Или переустановите с флагом --primary-ip:               ║"
  echo -e "║                                                          ║"
  echo -e "║    sudo bash install-node.sh --primary-ip <PRIMARY_IP>   ║"
  echo -e "╚══════════════════════════════════════════════════════════╝${NC}"
  echo

  # ── Safe-mode guard ──────────────────────────────────────────────────────────
  # Set consensus.non_validator: true so the node cannot produce blocks (and
  # therefore cannot mine an incompatible genesis) even if the operator ignores
  # the warning above and manually enables the systemd unit before running
  # aperod-join.sh.  aperod-join.sh unsets this field after the correct
  # chain.db is in place and the bootnode has been configured.
  # The field must be nested under the `consensus:` key to match the Go struct
  # (config.ConsensusConfig.NonValidator yaml:"non_validator").
  info "Устанавливаем consensus.non_validator: true (режим реле до выполнения aperod-join.sh)…"
  if [[ -x "${SCRIPT_DIR}/node-config.sh" ]]; then
    APEROD_CONFIG="${CONFIG_DIR}/node.yaml" bash "${SCRIPT_DIR}/node-config.sh" \
      set-field consensus.non_validator true \
      || die "Не удалось установить consensus.non_validator: true в ${CONFIG_DIR}/node.yaml"
  else
    # Fallback: inline Python — mirrors node-config.sh set-field with dotted path.
    python3 - "${CONFIG_DIR}/node.yaml" <<'PY'
import sys, yaml, os
cfg_path = sys.argv[1]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}
if cfg.get('consensus') is None:
    cfg['consensus'] = {}
cfg['consensus']['non_validator'] = True
tmp = cfg_path + '.tmp'
with open(tmp, 'w') as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
print('[OK]   consensus.non_validator: true written to', cfg_path)
PY
  fi
  ok "consensus.non_validator: true — нода не будет производить блоки до aperod-join.sh"
fi

# ── 9. Firewall ────────────────────────────────────────────
info "Настраиваем ufw…"
ufw --force reset       >/dev/null 2>&1
ufw default deny incoming  >/dev/null 2>&1
ufw default allow outgoing >/dev/null 2>&1
ufw allow 22/tcp    comment "SSH"               >/dev/null 2>&1
ufw allow ${P2P_PORT}/tcp comment "Aperod P2P"  >/dev/null 2>&1
ufw allow ${P2P_PORT}/udp comment "Aperod P2P"  >/dev/null 2>&1
# 8545 остаётся закрытым для внешних соединений
ufw --force enable      >/dev/null 2>&1
ok "Firewall: открыты порты 22 и ${P2P_PORT}. Порт ${RPC_PORT} (RPC) — только localhost."

# ── 10. systemd-сервис ────────────────────────────────────
cat > /etc/systemd/system/aperod-node.service <<EOF
[Unit]
Description=Aperod APR Full Node
Documentation=https://github.com/aperod-network/aperod-node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=aperod
ExecStart=/usr/local/bin/aperod-node --config ${CONFIG_DIR}/node.yaml
Restart=always
RestartSec=5
StartLimitBurst=5
StartLimitIntervalSec=60
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=aperod-node
# Must be ≥ 900 s so the shutdown snapshot goroutine can flush to disk before
# systemd sends SIGKILL.  A shorter timeout truncates the snapshot and forces
# the next restart into the multi-hour 800K-block scan — root cause of the
# August 2026 outage.
TimeoutStopSec=900

# ── Systemd Sandbox (prevents RCE escalation to full server access) ──────────
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=true
LockPersonality=true
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
ReadWritePaths=${DATA_DIR}
ReadWritePaths=${CONFIG_DIR}

[Install]
WantedBy=multi-user.target
EOF

# ── 10b. GOMEMLIMIT drop-in ───────────────────────────────
# Creates /etc/systemd/system/aperod-node.service.d/gomemlimit.conf so
# the Go runtime stays within the host's available memory and the
# shutdown snapshot has enough time to flush before SIGKILL.
DROPIN_DIR="/etc/systemd/system/aperod-node.service.d"
mkdir -p "${DROPIN_DIR}"
cat > "${DROPIN_DIR}/gomemlimit.conf" <<EOF
# Aperod node — memory limit drop-in
# ─────────────────────────────────────────────
# Install path: /etc/systemd/system/aperod-node.service.d/gomemlimit.conf
#
# GOMEMLIMIT=${GOMEMLIMIT_BYTES} bytes (~$(( GOMEMLIMIT_BYTES / 1024 / 1024 )) MiB)
#   Go soft memory limit.  Set to 75 % of host RAM at install time.
#   To change: edit this file then run:
#     systemctl daemon-reload && systemctl restart aperod-node

[Service]
Environment="GOMEMLIMIT=${GOMEMLIMIT_BYTES}"
EOF
ok "GOMEMLIMIT drop-in создан: ${DROPIN_DIR}/gomemlimit.conf (лимит: $(( GOMEMLIMIT_BYTES / 1024 / 1024 )) МиБ)"

# ── 10c. TimeoutStopSec drop-in ───────────────────────────
# Creates /etc/systemd/system/aperod-node.service.d/timeout.conf
# (the exact filename that checkSystemdTimeout() checks inside the node).
# A dedicated drop-in takes precedence over the main unit file and makes
# the shutdown timeout visible and auditable without opening the full unit.
#
# 900 s = 15 minutes — enough for the UTXO snapshot to flush to disk
# before systemd sends SIGKILL on a 5.7 GB RAM node.  A shorter value
# (e.g. the 300 s default before August 2026) truncates the snapshot and
# forces the next boot into the multi-hour 800 K-block rescan.
cat > "${DROPIN_DIR}/timeout.conf" <<EOF
# Aperod node — shutdown timeout drop-in
# ─────────────────────────────────────────────
# Install path: /etc/systemd/system/aperod-node.service.d/timeout.conf
#
# TimeoutStopSec=900
#   Give the SIGTERM shutdown handler up to 15 minutes to flush the UTXO
#   snapshot to disk before systemd sends SIGKILL.  A shorter timeout
#   truncates the snapshot and forces the next restart into the multi-hour
#   800K-block scan — root cause of the August 2026 outage.
#
#   To change without reinstalling:
#     nano /etc/systemd/system/aperod-node.service.d/timeout.conf
#     systemctl daemon-reload

[Service]
TimeoutStopSec=900
EOF
ok "Timeout drop-in создан: ${DROPIN_DIR}/timeout.conf (900 s)"

systemctl daemon-reload
# Only enable (and start) aperod-node when a bootnode has been configured.
# Without a bootnode the node could mine a block with an incompatible genesis key;
# the operator must run aperod-join.sh first.  aperod-join.sh runs
# `systemctl enable --now aperod-node` after populating chain data and bootnode.
# Do NOT run `systemctl enable` here on the no-primary path: even without a
# `systemctl start`, an enabled unit would auto-start on the next reboot.
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  systemctl enable --now aperod-node
else
  info "Сервис aperod-node НЕ включён в автозапуск и НЕ запущен."
  info "aperod-join.sh выполнит enable+start после загрузки корректного chain.db."
fi

# ── 11. Watchdog — автоматический перезапуск при зависании API ────────────────
info "Устанавливаем watchdog для aperod-node…"
cp "${SCRIPT_DIR}/aperod-node-watchdog.sh" /usr/local/bin/aperod-node-watchdog.sh
chmod +x /usr/local/bin/aperod-node-watchdog.sh
cp "${SCRIPT_DIR}/aperod-node-watchdog.service" /etc/systemd/system/
cp "${SCRIPT_DIR}/aperod-node-watchdog.timer"   /etc/systemd/system/
# Create optional env file for Telegram alerts (operator fills in bot token / chat ID)
mkdir -p /etc/aperod
if [[ ! -f /etc/aperod/watchdog.env ]]; then
  cat > /etc/aperod/watchdog.env <<WENV
# Aperod node watchdog configuration
# ─────────────────────────────────────────────────────────────────
# Probe interval (seconds).  Default: 60.
# To change without a redeploy:
#   1. Edit this value.
#   2. Run: sudo aperod-watchdog-set-interval
# Minimum accepted value: 5
WATCHDOG_INTERVAL_SECS=60

# Telegram alert credentials — fill in to receive notifications on node restarts.
# SUPPORT_BOT_TOKEN=
# SUPPORT_ADMIN_CHAT_ID=
WENV
  chmod 600 /etc/aperod/watchdog.env
fi

# Install the interval-apply helper so operators can change the timer on the fly
cp "${SCRIPT_DIR}/aperod-watchdog-set-interval.sh" /usr/local/bin/aperod-watchdog-set-interval
chmod +x /usr/local/bin/aperod-watchdog-set-interval

# Apply WATCHDOG_INTERVAL_SECS from watchdog.env to the timer drop-in.
# On the no-primary path use APEROD_DROPIN_ONLY=1: write the drop-in file and
# run daemon-reload but skip `systemctl restart`, so a pre-existing enabled
# timer is not accidentally activated before aperod-join.sh runs.
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  /usr/local/bin/aperod-watchdog-set-interval
else
  APEROD_DROPIN_ONLY=1 /usr/local/bin/aperod-watchdog-set-interval
fi

# Only enable/start the watchdog when the node itself is enabled (--primary-ip path).
# On the no-primary path `aperod-node` is not enabled either; enabling the watchdog
# would allow it to start the node indirectly (`systemctl restart aperod-node`).
# aperod-join.sh enables and starts this timer after a successful chain sync.
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  systemctl enable --now aperod-node-watchdog.timer
  ok "Watchdog установлен и запущен (aperod-node-watchdog.timer)"
else
  ok "Watchdog установлен (файлы скопированы; enable+start выполнит aperod-join.sh)"
fi

# ── 11b. Scheduled RAM-prevention restart timer ───────────────────────────────
# The Go node leaks ~1.3 GB/h.  A restart every 3 h keeps RAM well below the
# GOMEMLIMIT ceiling and prevents GC thrash / watchdog crash-loops.
# The interval is configurable via /etc/aperod/sched-restart.env without
# editing any system files (run `sudo aperod-sched-restart-set-interval`).
info "Устанавливаем таймер планового перезапуска (каждые 3 ч, защита от роста RAM)…"
cp "${SCRIPT_DIR}/aperod-node-sched-restart.sh"      /usr/local/bin/aperod-node-sched-restart.sh
chmod +x /usr/local/bin/aperod-node-sched-restart.sh
cp "${SCRIPT_DIR}/aperod-node-sched-restart.service" /etc/systemd/system/
cp "${SCRIPT_DIR}/aperod-node-sched-restart.timer"   /etc/systemd/system/

# Create optional env file for interval and Telegram credentials
mkdir -p /etc/aperod
if [[ ! -f /etc/aperod/sched-restart.env ]]; then
  cat > /etc/aperod/sched-restart.env <<SRENV
# Aperod node scheduled-restart configuration
# ─────────────────────────────────────────────────────────────────
# Restart interval in seconds.  Default: 10800 (3 hours).
# Valid range: 3600 (1 h) – 86400 (24 h).
# To change without a redeploy:
#   1. Edit this value.
#   2. Run: sudo aperod-sched-restart-set-interval
SCHED_RESTART_INTERVAL_SECS=10800

# Telegram credentials — if blank, no notifications are sent.
# (Shared with watchdog.env; fill in once and copy to both files.)
# SUPPORT_BOT_TOKEN=
# SUPPORT_ADMIN_CHAT_ID=
SRENV
  chmod 600 /etc/aperod/sched-restart.env
fi

# Install the interval-apply helper
cp "${SCRIPT_DIR}/aperod-sched-restart-set-interval.sh" \
   /usr/local/bin/aperod-sched-restart-set-interval
chmod +x /usr/local/bin/aperod-sched-restart-set-interval

# Apply the interval from sched-restart.env to the timer drop-in.
# Same APEROD_DROPIN_ONLY guard as the watchdog helper above.
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  /usr/local/bin/aperod-sched-restart-set-interval
else
  APEROD_DROPIN_ONLY=1 /usr/local/bin/aperod-sched-restart-set-interval
fi

# Same guard as the watchdog: do not enable the restart timer until the node
# is safely connected to the network.  An enabled timer would fire after its
# interval and run `systemctl restart aperod-node`, starting an isolated node.
# aperod-join.sh enables and starts this timer after a successful chain sync.
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  systemctl enable --now aperod-node-sched-restart.timer
  ok "Таймер планового перезапуска установлен и запущен (aperod-node-sched-restart.timer, каждые 3 ч)"
else
  ok "Таймер планового перезапуска установлен (файлы скопированы; enable+start выполнит aperod-join.sh)"
fi

# Install sudoers rule so the Admin Panel (aperod-api) can change the interval
# via PATCH /api/admin/system/sched-restart-interval without SSH access.
# Mirrors the watchdog-interval sudoers setup done earlier in this script.
info "Устанавливаем sudoers-правило для Admin Panel (sched-restart-interval)…"
bash "${SCRIPT_DIR}/setup-sched-restart-interval.sh"
ok "Sudoers-правило для sched-restart-interval установлено"

# ── 12. aperod_backup.sh ──────────────────────────────────────────────────
# Install the backup script from the repo so the correct version is present
# from day one.  setup-backup.sh (run separately) configures the schedule and
# credentials; this step just ensures /usr/local/bin/aperod_backup.sh is never
# stale if setup-backup.sh was run before a fresh install-node.sh deployment.
info "Устанавливаем aperod_backup.sh…"
if [[ -f "${SCRIPT_DIR}/aperod_backup.sh" ]]; then
  cp "${SCRIPT_DIR}/aperod_backup.sh" /usr/local/bin/aperod_backup.sh
  chmod 700 /usr/local/bin/aperod_backup.sh
  # Verify the installed file is non-empty, executable, and syntactically valid.
  # A truncated write (e.g. disk-full during install) or wrong permissions would
  # cause a silent failure at backup time — catch it now instead.
  [[ -s /usr/local/bin/aperod_backup.sh ]] \
    || die "aperod_backup.sh установлен, но файл пустой — возможна неполная запись"
  [[ -x /usr/local/bin/aperod_backup.sh ]] \
    || die "aperod_backup.sh не исполняемый после chmod 700 — проверьте файловую систему"
  bash -n /usr/local/bin/aperod_backup.sh \
    || die "aperod_backup.sh не прошёл синтаксическую проверку (bash -n) — файл мог быть усечён при записи"
  ok "aperod_backup.sh установлен и прошёл синтаксическую проверку: /usr/local/bin/aperod_backup.sh"
  info "  Для настройки резервного копирования запустите: sudo bash ${SCRIPT_DIR}/setup-backup.sh"
else
  warn "aperod_backup.sh не найден в ${SCRIPT_DIR} — пропускаем установку скрипта резервного копирования"
fi

# ── 12b. Install git post-merge hook ─────────────────────────────────────────
# When operators run bare `git pull` (bypassing update-node.sh), the installed
# /usr/local/bin/aperod_backup.sh can silently drift from the repo copy.
# The hook detects this mismatch immediately and warns on stderr so the
# operator knows to run sudo update-node.sh to perform the privileged sync.
#
# Security: the hook runs as the aperod user and performs NO writes to
# root-owned paths.  It only reads and compares files, then prints warnings.
# The actual privileged copy remains an explicit operator action (update-node.sh).
#
# Tarball installs have no .git directory — the step is a silent no-op.
info "Устанавливаем git post-merge hook (оповещение о расхождении aperod_backup.sh)…"
HOOK_SRC="${SCRIPT_DIR}/post-merge"
GIT_HOOKS_DIR="${INSTALL_DIR}/.git/hooks"
if [[ -d "${GIT_HOOKS_DIR}" && -f "${HOOK_SRC}" ]]; then
  cp "${HOOK_SRC}" "${GIT_HOOKS_DIR}/post-merge"
  chmod +x "${GIT_HOOKS_DIR}/post-merge"
  ok "post-merge hook установлен: ${GIT_HOOKS_DIR}/post-merge"
else
  info "  .git/hooks не найден — пропускаем (установка через тарбол, без git)."
fi

# Проверяем что стартовал (только если --primary-ip был передан)
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
  sleep 3
  if systemctl is-active --quiet aperod-node; then
    ok "Сервис aperod-node запущен и добавлен в автозапуск"
  else
    warn "Сервис запущен, но, возможно, есть ошибки. Проверьте: journalctl -u aperod-node -n 30"
  fi
else
  ok "Сервис aperod-node НЕ включён в автозапуск — выполните aperod-join.sh для первого запуска"
fi

# ── Итоговый вывод ────────────────────────────────────────
echo
if [[ -n "${PRIMARY_NODE_IP}" ]]; then
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Aperod APR нода установлена и запущена!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
else
echo -e "${YELLOW}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo -e "${YELLOW}${BOLD}  ✓  Aperod APR нода установлена (сервис не запущен)${NC}"
echo -e "${YELLOW}${BOLD}══════════════════════════════════════════════════════════${NC}"
fi
echo
echo -e "  ${BOLD}Ваш APR-адрес:${NC}"
echo -e "  ${WALLET_ADDR:-[адрес в файле ${WALLET_FILE}]}"
echo
echo -e "  ${BOLD}Полезные команды:${NC}"
echo -e "  systemctl status aperod-node         — статус ноды"
echo -e "  journalctl -u aperod-node -f          — логи в реальном времени"
echo -e "  aperod wallet balance                 — баланс кошелька"
echo -e "  aperod wallet address                 — ваш APR-адрес"
echo -e "  aperod wallet new-address --label X   — создать адрес для платежа"
echo -e "  aperod wallet send --to <addr> --amount <n> — отправить APR"
echo
echo -e "${YELLOW}${BOLD}  ⚠  Не забудьте сделать резервную копию кошелька:${NC}"
echo -e "  gpg --symmetric --cipher-algo AES256 ${WALLET_FILE}"
echo -e "  Сохраните .gpg файл на USB или в менеджере паролей."
echo
if [[ -z "${PRIMARY_NODE_IP}" ]]; then
  echo -e "${YELLOW}${BOLD}  ⚠  ВАЖНО: нода НЕ запущена и НЕ добавлена в автозапуск.${NC}"
  echo -e "  Сервис aperod-node будет запущен автоматически после aperod-join.sh."
  echo -e "  До этого нода не будет стартовать — ни вручную, ни при перезагрузке."
  echo -e ""
  echo -e "  Для подключения к сети выполните одно из следующих действий:"
  echo -e ""
  echo -e "  Вариант 1 — рекомендуется (синхронизирует chain.db и запускает ноду):"
  echo -e "    sudo bash /opt/aperod/deploy/aperod-join.sh <PRIMARY_IP>:8545"
  echo -e ""
  echo -e "  Вариант 2 — переустановка с флагом (если chain.db ещё не получен):"
  echo -e "    sudo bash install-node.sh --primary-ip <PRIMARY_IP>"
  echo
fi
