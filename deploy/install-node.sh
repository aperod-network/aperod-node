#!/usr/bin/env bash
# ============================================================
#  Aperod APR Full Node — Automatic Installer (Non-Validator)
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

echo -e "
${BOLD}╔══════════════════════════════════════════╗
║   Aperod APR Full Node — Installer       ║
║   github.com/aperod/aperod               ║
╚══════════════════════════════════════════╝${NC}
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
if [[ -d "${INSTALL_DIR}/.git" ]]; then
  git -C "${INSTALL_DIR}" pull --ff-only
  ok "Репозиторий обновлён"
else
  git clone --depth=1 "${REPO_URL}" "${INSTALL_DIR}"
  ok "Репозиторий клонирован в ${INSTALL_DIR}"
fi

# ── 4. Сборка бинарников ──────────────────────────────────
info "Компилируем aperod-node и aperod CLI (1–3 минуты)…"
cd "${INSTALL_DIR}/blockchain"
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
Documentation=https://github.com/aperod/aperod
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/aperod-node --config ${CONFIG_DIR}/node.yaml
Restart=always
RestartSec=5
StartLimitBurst=5
StartLimitIntervalSec=60
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=aperod-node

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable aperod-node
systemctl start  aperod-node

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
