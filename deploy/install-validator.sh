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
P2P_PORT=30303
RPC_PORT=8545

echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod Validator Node — Installer                   ║
║        aperod.com  |  t.me/aperod_bot                      ║
╚════════════════════════════════════════════════════════════╝${NC}
"

# ── Проверка root ─────────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Запустите скрипт от root: sudo bash install-validator.sh"
fi

# ── Проверка ОС ───────────────────────────────────────────
if ! grep -qE "(Ubuntu|Debian)" /etc/os-release 2>/dev/null; then
  warn "Скрипт тестировался на Ubuntu 22.04/24.04 и Debian 12. Продолжить? [y/N]"
  read -rp "" CONFIRM </dev/tty
  [[ "${CONFIRM,,}" == "y" ]] || die "Установка отменена"
fi

# ── Шаг 0: Получите кошелёк ДО установки ─────────────────
echo -e "${YELLOW}${BOLD}══════════════════════════════════════════════════════════════"
echo -e "  ВАЖНО: перед установкой ноды нужен APRO-адрес кошелька"
echo -e "══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  Все вознаграждения за блоки будут поступать на ваш кошелёк"
echo -e "  в Telegram. Если кошелька ещё нет — создайте его:"
echo
echo -e "  ${BOLD}1. Откройте бот: https://t.me/aperod_bot${NC}"
echo -e "  ${BOLD}2. Нажмите «Создать кошелёк»${NC}"
echo -e "  ${BOLD}3. Скопируйте ваш APRO-адрес${NC}"
echo

# Определяем доступность интерактивного ввода.
# curl|bash делает stdin пайпом, но /dev/tty доступен если есть реальный терминал.
IS_TTY=false
if { : </dev/tty; } 2>/dev/null; then
  IS_TTY=true
fi

REWARD_ADDRESS=""

if [[ "${IS_TTY:-false}" == "true" ]]; then
  while true; do
    echo -e "${BOLD}Введите ваш APRO-адрес из Telegram-кошелька:${NC}"
    read -rp "  > " REWARD_ADDRESS </dev/tty
    REWARD_ADDRESS="${REWARD_ADDRESS// /}"  # убираем пробелы
    if [[ ${#REWARD_ADDRESS} -ge 80 ]]; then
      ok "Адрес принят: ${REWARD_ADDRESS:0:20}…${REWARD_ADDRESS: -8}"
      break
    else
      warn "Адрес слишком короткий (${#REWARD_ADDRESS} символов). APRO-адрес содержит ~95 символов. Попробуйте ещё раз."
    fi
  done
else
  # Неинтерактивный режим — адрес через переменную окружения
  if [[ -n "${APEROD_REWARD_ADDRESS:-}" ]]; then
    REWARD_ADDRESS="${APEROD_REWARD_ADDRESS}"
    ok "Адрес из переменной окружения: ${REWARD_ADDRESS:0:20}…"
  else
    die "Неинтерактивный режим: передайте адрес кошелька через:
    APEROD_REWARD_ADDRESS=<ваш-apro-адрес> bash install-validator.sh"
  fi
fi

echo

# ── 1. Зависимости ────────────────────────────────────────
info "Обновляем пакеты и устанавливаем зависимости…"
apt-get update -q
apt-get install -y -q git curl wget build-essential ufw jq xxd
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
TARBALL_URL="https://github.com/aperod-network/aperod-node/archive/refs/heads/main.tar.gz"
rm -rf "${INSTALL_DIR}"
mkdir -p "${INSTALL_DIR}"
info "Скачиваем архив исходного кода…"
wget -q "${TARBALL_URL}" -O /tmp/aperod-src.tar.gz \
  || die "Не удалось скачать ${TARBALL_URL}"
tar -xzf /tmp/aperod-src.tar.gz -C "${INSTALL_DIR}" --strip-components=1
rm -f /tmp/aperod-src.tar.gz
ok "Исходный код получен в ${INSTALL_DIR}"

# ── 5. Сборка бинарников ──────────────────────────────────
info "Компилируем aperod-node (может занять 1–3 минуты)…"
cd "${INSTALL_DIR}"
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

# ── 7. Ключ консенсуса валидатора ─────────────────────────
echo
echo -e "${BOLD}═══════════════════════════════════════════════"
echo -e "  Настройка ключа консенсуса валидатора"
echo -e "═══════════════════════════════════════════════${NC}"
echo -e "  (Этот ключ используется только для подписи блоков."
echo -e "   Средства хранятся в вашем Telegram-кошельке.)"
echo

KEY_CHOICE="new"
if [[ -f "${CONFIG_DIR}/validator.key" ]]; then
  warn "Файл ключа уже существует: ${CONFIG_DIR}/validator.key"
  if [[ "${IS_TTY:-false}" == "true" ]]; then
    read -rp "Перезаписать? [y/N]: " OVERWRITE_KEY
  else
    OVERWRITE_KEY="n"
    info "Неинтерактивный режим — используем существующий ключ"
  fi
  if [[ "${OVERWRITE_KEY,,}" != "y" ]]; then
    info "Используем существующий ключ"
    PRIVKEY_HEX=$(xxd -p -c 256 "${CONFIG_DIR}/validator.key" | tr -d '\n' || cat "${CONFIG_DIR}/validator.key")
    PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "")
    KEY_CHOICE="keep"
  fi
fi

if [[ "${KEY_CHOICE}" != "keep" ]]; then
  if [[ "${IS_TTY:-false}" == "true" ]]; then
    echo
    echo "  1) Сгенерировать новый ключ (рекомендуется)"
    echo "  2) Ввести существующий приватный ключ (hex, 64 символа)"
    echo
    read -rp "Выбор [1/2]: " KEY_CHOICE
  else
    KEY_CHOICE="1"
    info "Неинтерактивный режим — генерируем новый ключ"
  fi

  if [[ "${KEY_CHOICE}" == "2" ]]; then
    if [[ "${IS_TTY:-false}" != "true" ]]; then
      die "Режим ввода существующего ключа требует интерактивного терминала."
    fi
    read -rsp "  Введите приватный ключ (64 hex-символа, ввод скрыт): " PRIVKEY_HEX
    echo
    if [[ ! "${PRIVKEY_HEX}" =~ ^[0-9a-fA-F]{64}$ ]]; then
      die "Неверный формат ключа. Ожидается 64 hex-символа (32 байта)."
    fi
    mkdir -p "${CONFIG_DIR}"
    echo -n "${PRIVKEY_HEX}" | xxd -r -p > "${CONFIG_DIR}/validator.key"
    PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "")
    [[ -z "${PUBKEY_HEX}" ]] && PUBKEY_HEX="(запустите: aperod wallet pubkey ${PRIVKEY_HEX})"
    ok "Ключ сохранён"
  else
    info "Генерируем новый ключ Ed25519…"
    KEYGEN_OUT=$(aperod wallet keygen 2>&1)
    PRIVKEY_HEX=$(echo "${KEYGEN_OUT}" | grep -oP "Private:\s+\K[0-9a-f]+" | head -1 || true)
    PUBKEY_HEX=$(echo "${KEYGEN_OUT}"  | grep -oP "Public:\s+\K[0-9a-f]+"  | head -1 || true)
    if [[ -z "${PRIVKEY_HEX}" ]]; then
      die "Не удалось сгенерировать ключ. Вывод aperod: ${KEYGEN_OUT}"
    fi
    mkdir -p "${CONFIG_DIR}"
    echo -n "${PRIVKEY_HEX}" | xxd -r -p > "${CONFIG_DIR}/validator.key"
    ok "Новый ключ консенсуса сгенерирован и сохранён"
  fi
fi

# The aperod-node service runs as User=aperod and the binary enforces strict
# permissions (chmod 600).  Wrong owner (root) → EPERM; wrong mode (640) →
# "unsafe permissions" error on startup.  Both are set correctly here.
chmod 600 "${CONFIG_DIR}/validator.key"
chown "${APEROD_USER}:${APEROD_USER}" "${CONFIG_DIR}/validator.key"

echo
echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════╗"
echo -e "  ✓  Публичный ключ валидатора (для регистрации ноды):"
echo -e "     ${PUBKEY_HEX}"
echo -e "╚══════════════════════════════════════════════════════════════╝${NC}"
echo

# ── 8. Определяем внешний IP ─────────────────────────────
MY_IP=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null \
     || curl -s --connect-timeout 5 icanhazip.com 2>/dev/null \
     || curl -s --connect-timeout 5 api.ipify.org 2>/dev/null \
     || echo "0.0.0.0")

if [[ "${MY_IP}" == "0.0.0.0" ]]; then
  warn "Не удалось определить внешний IP автоматически."
  if [[ "${IS_TTY:-false}" == "true" ]]; then
    read -rp "Введите публичный IP этого сервера: " MY_IP
  else
    MY_IP="0.0.0.0"
    warn "Укажите IP вручную в ${CONFIG_DIR}/node.yaml после установки"
  fi
fi
info "Внешний IP: ${MY_IP}"

# ── 9. Копируем genesis конфиг ────────────────────────────
mkdir -p "${CONFIG_DIR}"
if [[ -f "${INSTALL_DIR}/config/genesis-testnet.yaml" ]]; then
  cp "${INSTALL_DIR}/config/genesis-testnet.yaml" "${CONFIG_DIR}/genesis-testnet.yaml"
  ok "Genesis конфиг скопирован: ${CONFIG_DIR}/genesis-testnet.yaml"
else
  warn "Файл genesis не найден. Нода не запустится без него."
fi

# ── 10. Конфигурация ноды ─────────────────────────────────
cat > "${CONFIG_DIR}/node.yaml" <<EOF
# Aperod Node Configuration
# Автоматически создан install-validator.sh $(date -u +"%Y-%m-%d %H:%M UTC")

network: testnet
data_dir: ${DATA_DIR}
log_level: info

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/${P2P_PORT}
  bootnodes:
    - /ip4/77.221.153.86/tcp/30303
  max_peers: 50

consensus:
  validator_key: ${CONFIG_DIR}/validator.key
  reward_address: ${REWARD_ADDRESS}

api:
  enabled: true
  listen_addr: 127.0.0.1:${RPC_PORT}

genesis:
  file: ${CONFIG_DIR}/genesis-testnet.yaml
EOF

ok "Конфигурация сохранена: ${CONFIG_DIR}/node.yaml"

# ── 11. Firewall ──────────────────────────────────────────
info "Настраиваем ufw…"
ufw --force reset >/dev/null 2>&1
ufw default deny incoming  >/dev/null 2>&1
ufw default allow outgoing >/dev/null 2>&1
ufw allow 22/tcp    comment "SSH"              >/dev/null 2>&1
ufw allow ${P2P_PORT}/tcp comment "Aperod P2P"           >/dev/null 2>&1
ufw allow ${P2P_PORT}/udp comment "Aperod P2P discovery" >/dev/null 2>&1
ufw --force enable          >/dev/null 2>&1
ok "Firewall настроен (открыты: 22, ${P2P_PORT}/tcp, ${P2P_PORT}/udp)"

# ── 12. systemd сервис ────────────────────────────────────
cat > /etc/systemd/system/aperod-node.service <<EOF
[Unit]
Description=Aperod Validator Node (${MY_IP})
Documentation=https://aperod.com/docs
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
# Must be ≥ 900 s so the shutdown snapshot goroutine can flush to disk before
# systemd sends SIGKILL.  A shorter timeout causes OOM loops on the next
# restart because the node always falls back to the multi-hour block scan.
# The Aug 2026 outage was caused by a 300 s value triggering SIGKILL
# mid-write on a 5.7 GB RAM node — 900 s gives 15 minutes of headroom.
TimeoutStopSec=900

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

# ── Watchdog — автоматический перезапуск при зависании API ────────────────────
# After the node is started, install the watchdog timer that polls
# GET /api/v1/status every 60 s and calls `systemctl restart aperod-node`
# if the probe does not return HTTP 200 within 5 s.
info "Устанавливаем watchdog для aperod-node…"
cp "${INSTALL_DIR}/deploy/aperod-node-watchdog.sh" /usr/local/bin/aperod-node-watchdog.sh
chmod +x /usr/local/bin/aperod-node-watchdog.sh
cp "${INSTALL_DIR}/deploy/aperod-node-watchdog.service" /etc/systemd/system/
cp "${INSTALL_DIR}/deploy/aperod-node-watchdog.timer"   /etc/systemd/system/
# Create optional env file for Telegram alerts (operator fills in credentials)
mkdir -p /etc/aperod
if [[ ! -f /etc/aperod/watchdog.env ]]; then
  cat > /etc/aperod/watchdog.env <<WENV
# Watchdog alert credentials — fill in to receive Telegram notifications on node restarts
# SUPPORT_BOT_TOKEN=
# SUPPORT_ADMIN_CHAT_ID=
WENV
  chmod 600 /etc/aperod/watchdog.env
fi
systemctl daemon-reload
systemctl enable --now aperod-node-watchdog.timer
ok "Watchdog установлен (aperod-node-watchdog.timer запущен)"

sleep 3
if systemctl is-active --quiet aperod-node; then
  ok "Нода работает нормально"
else
  warn "Сервис запущен, но возможны ошибки. Проверьте: journalctl -u aperod-node -n 50"
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Установка завершена успешно!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  ${BOLD}Статус ноды:${NC}      systemctl status aperod-node"
echo -e "  ${BOLD}Логи:${NC}             journalctl -u aperod-node -f"
echo -e "  ${BOLD}Конфиг:${NC}           ${CONFIG_DIR}/node.yaml"
echo -e "  ${BOLD}Данные:${NC}           ${DATA_DIR}"
echo -e "  ${BOLD}P2P эндпоинт:${NC}    /ip4/${MY_IP}/tcp/${P2P_PORT}"
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  Адрес для получения вознаграждений:${NC}"
echo -e "  ${BOLD}${REWARD_ADDRESS}${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  Все начисления за блоки будут поступать на этот адрес"
echo -e "  в ваш Telegram-кошелёк (${CYAN}https://t.me/aperod_bot${NC})"
echo -e "  и вы получите уведомление в Telegram при каждом начислении."
echo

echo -e "${YELLOW}${BOLD}  Следующий шаг — зарегистрируйте ноду и сделайте стейк:${NC}"
echo
echo -e "  1) Убедитесь, что нода работает:"
echo -e "     ${CYAN}journalctl -u aperod-node -f${NC}"
echo
echo -e "  2) Зарегистрируйте ноду (выполните команду ниже):"
echo
APPLY_CMD="curl -s -X POST https://aperod.com/api/validators/apply \\
  -H 'Content-Type: application/json' \\
  -d '{
    \"pubKey\":   \"${PUBKEY_HEX}\",
    \"alias\":    \"my-validator\",
    \"endpoint\": \"/ip4/${MY_IP}/tcp/${P2P_PORT}\",
    \"address\":  \"${REWARD_ADDRESS}\"
  }'"
echo -e "  ${BOLD}${GREEN}${APPLY_CMD}${NC}"
echo
echo -e "  3) Переведите минимум ${BOLD}100 000 APRO${NC} на адрес вашего кошелька:"
echo -e "     ${BOLD}${REWARD_ADDRESS}${NC}"
echo -e "     Нода войдёт в активный набор валидаторов автоматически."
echo
echo -e "  ${BOLD}Параметры сети:${NC}"
echo -e "    Мин. стейк          : 100 000 APRO"
echo -e "    Макс. валидаторов   : 21"
echo -e "    Эпоха               : каждые 100 блоков (~1.7 мин)"
echo -e "    Анбондинг           : 7 200 блоков (~2 часа)"
echo -e "    Штраф за двойную подпись: 10% стейка"
echo
echo -e "  Ваш публичный ключ  : ${BOLD}${PUBKEY_HEX}${NC}"
echo -e "  P2P-эндпоинт        : /ip4/${MY_IP}/tcp/${P2P_PORT}"
echo -e "  Документация        : ${CYAN}https://aperod.com/docs${NC}"
echo
