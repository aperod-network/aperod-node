#!/usr/bin/env bash
# ============================================================
#  Aperod Validator Node — Automatic Installer
#  Поддерживается: Ubuntu 22.04 / 24.04 / Debian 12
#  Использование:  sudo bash install-validator.sh
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
APEROD_USER="aperod"
INSTALL_DIR="/opt/aperod"
DATA_DIR="/var/lib/aperod"
CONFIG_DIR="/etc/aperod"
GO_VERSION="1.23.4"
REPO_URL="https://github.com/aperod/aperod.git"
P2P_PORT=30303
RPC_PORT=8545

echo -e "
${BOLD}╔════════════════════════════════════════╗
║   Aperod Validator Node — Installer    ║
║   github.com/aperod/aperod             ║
╚════════════════════════════════════════╝${NC}
"

# ── Проверка root ─────────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Запустите скрипт от root: sudo bash install-validator.sh"
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

# ── 3. Пользователь aperod ────────────────────────────────
if ! id "${APEROD_USER}" &>/dev/null; then
  useradd -r -m -s /bin/false "${APEROD_USER}"
  ok "Создан системный пользователь ${APEROD_USER}"
else
  ok "Пользователь ${APEROD_USER} уже существует"
fi

# ── 4. Клонирование репозитория ───────────────────────────
info "Получаем исходный код Aperod…"
if [[ -d "${INSTALL_DIR}/.git" ]]; then
  git -C "${INSTALL_DIR}" pull --ff-only
  ok "Репозиторий обновлён"
else
  git clone --depth=1 "${REPO_URL}" "${INSTALL_DIR}"
  ok "Репозиторий клонирован в ${INSTALL_DIR}"
fi

# ── 5. Сборка бинарников ──────────────────────────────────
info "Компилируем aperod-node (может занять 1–3 минуты)…"
cd "${INSTALL_DIR}/blockchain"
export GOPATH="/root/go"
export PATH="$PATH:/usr/local/go/bin"

make deps 2>&1 | tail -5
make build 2>&1 | tail -10

if [[ ! -f "build/aperod-node" ]]; then
  die "Сборка не удалась — файл build/aperod-node не найден. Проверьте вывод выше."
fi

cp build/aperod-node /usr/local/bin/aperod-node
cp build/aperod      /usr/local/bin/aperod
chmod +x /usr/local/bin/aperod-node /usr/local/bin/aperod
ok "Бинарники установлены: /usr/local/bin/aperod-node, /usr/local/bin/aperod"

# ── 6. Директории ─────────────────────────────────────────
mkdir -p "${CONFIG_DIR}" "${DATA_DIR}"

# ── 7. Ключи валидатора ───────────────────────────────────
echo
echo -e "${BOLD}═══════════════════════════════════════════════"
echo -e "  Настройка ключа валидатора"
echo -e "═══════════════════════════════════════════════${NC}"

if [[ -f "${CONFIG_DIR}/validator.key" ]]; then
  warn "Файл ключа уже существует: ${CONFIG_DIR}/validator.key"
  read -rp "Перезаписать? [y/N]: " OVERWRITE_KEY
  if [[ "${OVERWRITE_KEY,,}" != "y" ]]; then
    info "Используем существующий ключ"
    PUBKEY_HEX=$(aperod wallet pubkey "$(cat "${CONFIG_DIR}/validator.key")" 2>/dev/null || echo "")
    KEY_CHOICE="keep"
  fi
fi

if [[ "${KEY_CHOICE:-}" != "keep" ]]; then
  echo
  echo "  1) Сгенерировать новый ключ валидатора"
  echo "  2) Ввести существующий приватный ключ (hex, 64 символа)"
  echo
  read -rp "Выбор [1/2]: " KEY_CHOICE

  if [[ "${KEY_CHOICE}" == "2" ]]; then
    echo
    read -rsp "  Введите приватный ключ (64 hex-символа, ввод скрыт): " PRIVKEY_HEX
    echo
    if [[ ! "${PRIVKEY_HEX}" =~ ^[0-9a-fA-Fa-f]{64}$ ]]; then
      die "Неверный формат ключа. Ожидается 64 hex-символа (32 байта)."
    fi
    echo -n "${PRIVKEY_HEX}" > "${CONFIG_DIR}/validator.key"
    PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "[ошибка получения pubkey]")
    ok "Ключ сохранён"
  else
    info "Генерируем новый ключ Ed25519…"
    KEY_OUTPUT=$(aperod wallet create --validator 2>&1)
    PRIVKEY_HEX=$(echo "${KEY_OUTPUT}" | grep -oP "Private Key:\s+\K[0-9a-f]{64}" || true)
    PUBKEY_HEX=$(echo "${KEY_OUTPUT}"  | grep -oP "Public Key:\s+\K[0-9a-f]{64}"  || true)

    if [[ -z "${PRIVKEY_HEX}" ]]; then
      # Fallback: использовать openssl для генерации ключа
      warn "Не удалось распарсить вывод 'aperod wallet create'. Генерируем через openssl…"
      PRIVKEY_HEX=$(openssl rand -hex 32)
      PUBKEY_HEX="[вычислите через: aperod wallet pubkey ${PRIVKEY_HEX}]"
    fi

    echo -n "${PRIVKEY_HEX}" > "${CONFIG_DIR}/validator.key"
    ok "Новый ключ сгенерирован и сохранён"
  fi
fi

chmod 600 "${CONFIG_DIR}/validator.key"
chown root:root "${CONFIG_DIR}/validator.key"

echo
echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════╗"
echo -e "  ✓  Публичный ключ вашего валидатора:"
echo -e "     ${PUBKEY_HEX}"
echo -e "  Сохраните его — потребуется для отправки стейка"
echo -e "╚══════════════════════════════════════════════════════════╝${NC}"
echo

# ── 8. Определяем внешний IP ─────────────────────────────
MY_IP=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null \
     || curl -s --connect-timeout 5 icanhazip.com 2>/dev/null \
     || curl -s --connect-timeout 5 api.ipify.org 2>/dev/null \
     || echo "0.0.0.0")

if [[ "${MY_IP}" == "0.0.0.0" ]]; then
  warn "Не удалось определить внешний IP автоматически."
  read -rp "Введите публичный IP этого сервера: " MY_IP
fi
info "Внешний IP: ${MY_IP}"

# ── 9. Конфигурация ноды ──────────────────────────────────
cat > "${CONFIG_DIR}/node.yaml" <<EOF
# Aperod Node Configuration
# Автоматически создан install-validator.sh $(date -u +"%Y-%m-%d %H:%M UTC")

network: testnet
data_dir: ${DATA_DIR}
log_level: info

p2p:
  listen: /ip4/0.0.0.0/tcp/${P2P_PORT}
  external: /ip4/${MY_IP}/tcp/${P2P_PORT}
  max_peers: 50

rpc:
  listen: 127.0.0.1:${RPC_PORT}
  cors_origins: []

validator:
  enabled: true
  key_file: ${CONFIG_DIR}/validator.key

bootnodes:
  - /ip4/172.28.0.11/tcp/30303
  - /ip4/172.28.0.12/tcp/30303
  - /ip4/172.28.0.13/tcp/30303

metrics:
  enabled: true
  listen: 127.0.0.1:9090
EOF

ok "Конфигурация сохранена: ${CONFIG_DIR}/node.yaml"

# ── 10. Firewall ──────────────────────────────────────────
info "Настраиваем ufw…"
ufw --force reset >/dev/null 2>&1
ufw default deny incoming  >/dev/null 2>&1
ufw default allow outgoing >/dev/null 2>&1
ufw allow 22/tcp    comment "SSH"             >/dev/null 2>&1
ufw allow ${P2P_PORT}/tcp comment "Aperod P2P"          >/dev/null 2>&1
ufw allow ${P2P_PORT}/udp comment "Aperod P2P discovery" >/dev/null 2>&1
ufw --force enable          >/dev/null 2>&1
ok "Firewall настроен (открыты: 22, ${P2P_PORT}/tcp, ${P2P_PORT}/udp)"

# ── 11. systemd сервис ────────────────────────────────────
cat > /etc/systemd/system/aperod-node.service <<EOF
[Unit]
Description=Aperod Validator Node (${MY_IP})
Documentation=https://github.com/aperod/aperod
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APEROD_USER}
Group=${APEROD_USER}
ExecStart=/usr/local/bin/aperod-node --config ${CONFIG_DIR}/node.yaml
Restart=always
RestartSec=5
StartLimitBurst=5
StartLimitIntervalSec=60
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=aperod-node

# Hardening
ProtectSystem=full
PrivateTmp=true
NoNewPrivileges=true
ReadWritePaths=${DATA_DIR}
ReadOnlyPaths=${CONFIG_DIR}

[Install]
WantedBy=multi-user.target
EOF

chown -R "${APEROD_USER}:${APEROD_USER}" "${DATA_DIR}"

systemctl daemon-reload
systemctl enable aperod-node
systemctl start  aperod-node
ok "Сервис aperod-node запущен"

# Ждём 3 секунды и проверяем
sleep 3
if systemctl is-active --quiet aperod-node; then
  ok "Нода работает нормально"
else
  warn "Сервис запущен, но возможны ошибки. Проверьте: journalctl -u aperod-node -n 50"
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Установка завершена успешно!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo
echo -e "  ${BOLD}Статус ноды:${NC}     systemctl status aperod-node"
echo -e "  ${BOLD}Логи:${NC}            journalctl -u aperod-node -f"
echo -e "  ${BOLD}Конфиг:${NC}          ${CONFIG_DIR}/node.yaml"
echo -e "  ${BOLD}Данные:${NC}          ${DATA_DIR}"
echo -e "  ${BOLD}P2P эндпоинт:${NC}   /ip4/${MY_IP}/tcp/${P2P_PORT}"
echo
echo -e "${YELLOW}${BOLD}  Следующий шаг — отправьте стейк-транзакцию (мин. 100 000 APR):${NC}"
echo -e "  ┌──────────────────────────────────────────────────────────"
echo -e "  │  Никакого одобрения администратора не требуется."
echo -e "  │  Aperod — безразрешительная система: топ-21 нод по стейку"
echo -e "  │  активируются автоматически на следующей эпохе (100 блоков)."
echo -e "  │"
echo -e "  │  1) Пополните адрес ноды — минимум 100 000 APR"
echo -e "  │"
echo -e "  │  2) Отправьте депозит:"
echo -e "  │     aperod validator stake \\"
echo -e "  │       --key ${CONFIG_DIR}/validator.key \\"
echo -e "  │       --amount 100000 \\"
echo -e "  │       --node http://127.0.0.1:${RPC_PORT}"
echo -e "  │"
echo -e "  │  3) Проверьте статус:"
echo -e "  │     aperod validator status \\"
echo -e "  │       --pubkey ${PUBKEY_HEX} \\"
echo -e "  │       --node http://127.0.0.1:${RPC_PORT}"
echo -e "  │"
echo -e "  │  Ваш публичный ключ: ${PUBKEY_HEX}"
echo -e "  │  P2P-эндпоинт:       /ip4/${MY_IP}/tcp/${P2P_PORT}"
echo -e "  └──────────────────────────────────────────────────────────"
echo
