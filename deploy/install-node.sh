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

# ── Лимит памяти Go-рантайма (GOMEMLIMIT) ─────────────────
# По умолчанию — 75 % от общей RAM хоста, но не меньше 1.5 ГБ и не больше
# 5.5 ГБ (значение, проверенное на продакшн-ноде с 7.8 ГБ RAM).
# Переопределить: GOMEMLIMIT_BYTES=3221225472 sudo bash install-node.sh
TOTAL_RAM_KB=$(grep MemTotal /proc/meminfo | awk '{print $2}')
TOTAL_RAM_BYTES=$(( TOTAL_RAM_KB * 1024 ))
AUTO_GOMEMLIMIT=$(( TOTAL_RAM_BYTES * 3 / 4 ))
MIN_GOMEMLIMIT=$(( 1536 * 1024 * 1024 ))   # 1.5 GiB floor
MAX_GOMEMLIMIT=$(( 5905580032 ))            # 5500 MiB ceiling (prod-tested value)
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

# ── 3. Клонирование репозитория ───────────────────────────
info "Получаем исходный код Aperod…"
TARBALL_URL="https://github.com/aperod-network/aperod-node/archive/refs/heads/main.tar.gz"
mkdir -p "${INSTALL_DIR}"
info "Скачиваем архив исходного кода…"
wget -q "${TARBALL_URL}" -O /tmp/aperod-src.tar.gz \
  || die "Не удалось скачать ${TARBALL_URL}"
tar -xzf /tmp/aperod-src.tar.gz -C "${INSTALL_DIR}" --strip-components=1
rm -f /tmp/aperod-src.tar.gz
ok "Исходный код получен в ${INSTALL_DIR}"

# ── 4. Сборка бинарников ──────────────────────────────────
info "Компилируем aperod-node и aperod CLI (1–3 минуты)…"
cd "${INSTALL_DIR}"
export GOPATH="/root/go"

make deps 2>&1 | tail -3
make build 2>&1 | tail -8

if [[ ! -f "build/aperod-node" || ! -f "build/aperod" ]]; then
  die "Сборка не удалась — проверьте вывод выше"
fi

cp build/aperod-node /usr/local/bin/aperod-node
cp build/aperod      /usr/local/bin/aperod
chmod +x /usr/local/bin/aperod-node /usr/local/bin/aperod
ok "Бинарники установлены: /usr/local/bin/aperod-node, /usr/local/bin/aperod"

# ── 5. Создаём директории ─────────────────────────────────
mkdir -p "${CONFIG_DIR}" "${DATA_DIR}" "${WALLET_DIR}"

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

rpc:
  # ВАЖНО: только localhost! Никогда не меняйте на 0.0.0.0
  listen: 127.0.0.1:${RPC_PORT}
  cors_origins: []

wallet:
  key_file: ${WALLET_FILE}

bootnodes:
  - /ip4/172.28.0.11/tcp/30303
  - /ip4/172.28.0.12/tcp/30303
  - /ip4/172.28.0.13/tcp/30303

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
# Aperod node — memory limit and shutdown tuning
# ─────────────────────────────────────────────
# Install path: /etc/systemd/system/aperod-node.service.d/gomemlimit.conf
#
# GOMEMLIMIT=${GOMEMLIMIT_BYTES} bytes (~$(( GOMEMLIMIT_BYTES / 1024 / 1024 )) MiB)
#   Go soft memory limit.  Set to 75 % of host RAM at install time.
#   To change: edit this file then run:
#     systemctl daemon-reload && systemctl restart aperod-node
#
# TimeoutStopSec=900
#   Give the SIGTERM shutdown handler up to 15 minutes to flush the UTXO
#   snapshot to disk before systemd sends SIGKILL.  A shorter timeout
#   truncates the snapshot and forces the next restart into the multi-hour
#   800K-block scan — root cause of the August 2026 outage.

[Service]
Environment="GOMEMLIMIT=${GOMEMLIMIT_BYTES}"
TimeoutStopSec=900
EOF
ok "GOMEMLIMIT drop-in создан: ${DROPIN_DIR}/gomemlimit.conf (лимит: $(( GOMEMLIMIT_BYTES / 1024 / 1024 )) МиБ)"

systemctl daemon-reload
systemctl enable aperod-node
systemctl start  aperod-node

# ── 11. Watchdog — автоматический перезапуск при зависании API ────────────────
info "Устанавливаем watchdog для aperod-node…"
# Resolve the script directory so we can reference sibling deploy files
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
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

# Apply WATCHDOG_INTERVAL_SECS from watchdog.env to the timer drop-in
/usr/local/bin/aperod-watchdog-set-interval

systemctl enable --now aperod-node-watchdog.timer
ok "Watchdog установлен (aperod-node-watchdog.timer запущен)"

# Проверяем что стартовал
sleep 3
if systemctl is-active --quiet aperod-node; then
  ok "Сервис aperod-node запущен и добавлен в автозапуск"
else
  warn "Сервис запущен, но, возможно, есть ошибки. Проверьте: journalctl -u aperod-node -n 30"
fi

# ── Итоговый вывод ────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Aperod APR нода установлена и запущена!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
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
