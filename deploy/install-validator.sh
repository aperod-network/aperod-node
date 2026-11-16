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
GO_VERSION="1.26.11"
P2P_PORT=30303
RPC_PORT=8545
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/node-source-release.sh"
if [[ "${APEROD_NETWORK:-}" != "testnet" ]]; then
  die "Этот установщик поддерживает только testnet. Для mainnet нужны проверенные genesis и данные существующей цепи; пустой узел не запускается / This installer supports testnet only. Mainnet requires a verified genesis and existing chain state; a blank node will not be started"
fi
node_source_pin_guard || die "Нужен явно выбранный публичный коммит / An explicitly selected public commit is required"
[[ ! -e /usr/local/bin/aperod-node && ! -e "${CONFIG_DIR}/node.yaml" ]] ||
  die "Узел уже установлен: используйте update-node.sh с проверенным baseline / Node already installed: use update-node.sh with an approved baseline"
node_source_fresh_state_guard "$DATA_DIR" "$CONFIG_DIR" ||
  die "Существующие данные/конфигурацию нельзя переустанавливать как новый узел / Existing data or configuration cannot be reinstalled as a new node"
# A bootstrap package is independent of INSTALL_DIR and must be complete before
# installing binaries or creating a validator identity/configuration.
for bootstrap_file in ensure-dropin.sh gomemlimit-policy.sh aperod-node-watchdog.sh \
  aperod-node-watchdog.service aperod-node-watchdog.timer; do
  [[ -f "${SCRIPT_DIR}/${bootstrap_file}" && ! -L "${SCRIPT_DIR}/${bootstrap_file}" ]] ||
    die "Неполный bootstrap package: отсутствует ${bootstrap_file} / Incomplete bootstrap package: missing ${bootstrap_file}"
done

printf '\n%s\n%s\n%s\n\n' \
  '============================================================' \
  'Aperod Validator Node — Installer / Установщик ноды валидатора' \
  'aperod.com | t.me/aperod_bot'

# ── Проверка root ─────────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Запустите скрипт от root: sudo bash install-validator.sh / Run as root: sudo bash install-validator.sh"
fi

# ── Проверка ОС ───────────────────────────────────────────
if ! grep -qE "(Ubuntu|Debian)" /etc/os-release 2>/dev/null; then
  warn "Проверено на Ubuntu 22.04/24.04 и Debian 12. Продолжить? / Tested on Ubuntu 22.04/24.04 and Debian 12. Continue? [y/N]"
  read -rp "" CONFIRM </dev/tty
  [[ "${CONFIRM,,}" == "y" ]] || die "Установка отменена / Installation cancelled"
fi

# ── Шаг 0: Получите кошелёк ДО установки ─────────────────
echo -e "${YELLOW}${BOLD}============================================================"
echo -e "  ВАЖНО / IMPORTANT: перед установкой нужен APRO-адрес / an APRO wallet address is required"
echo -e "============================================================${NC}"
echo
echo -e "  Вознаграждения поступают в Telegram-кошелёк. Если его нет — создайте:"
echo -e "  Block rewards go to your Telegram wallet. Create one if needed:"
echo
echo -e "  ${BOLD}1. Откройте / Open: https://t.me/aperod_bot${NC}"
echo -e "  ${BOLD}2. Нажмите «Создать кошелёк» / Tap “Create wallet”${NC}"
echo -e "  ${BOLD}3. Скопируйте APRO-адрес / Copy your APRO address${NC}"
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
    echo -e "${BOLD}Введите APRO-адрес из Telegram-кошелька / Enter the APRO address from your Telegram wallet:${NC}"
    read -rp "  > " REWARD_ADDRESS </dev/tty
    REWARD_ADDRESS="${REWARD_ADDRESS// /}"  # убираем пробелы
    if [[ ${#REWARD_ADDRESS} -ge 80 ]]; then
      ok "Адрес принят / Address accepted: ${REWARD_ADDRESS:0:20}…${REWARD_ADDRESS: -8}"
      break
    else
      warn "Адрес короткий (${#REWARD_ADDRESS} символов); нужно около 95. Повторите ввод. / Address is too short (${#REWARD_ADDRESS} characters); APRO addresses are about 95 characters. Try again."
    fi
  done
else
  # Неинтерактивный режим — адрес через переменную окружения
  if [[ -n "${APEROD_REWARD_ADDRESS:-}" ]]; then
    REWARD_ADDRESS="${APEROD_REWARD_ADDRESS}"
    ok "Адрес задан через переменную окружения / Address supplied through environment: ${REWARD_ADDRESS:0:20}…"
  else
    die "Неинтерактивный режим: задайте адрес кошелька / Non-interactive mode: provide the wallet address with:
    APEROD_REWARD_ADDRESS=<ваш-apro-адрес> bash install-validator.sh"
  fi
fi

echo

# ── 1. Зависимости ────────────────────────────────────────
info "Обновляем пакеты и устанавливаем зависимости / Updating packages and installing dependencies…"
apt-get update -q
apt-get install -y -q git curl wget build-essential ufw jq xxd
ok "Зависимости установлены / Dependencies installed"

# ── 2. Go ─────────────────────────────────────────────────
need_go=false
if command -v go &>/dev/null; then
  GO_INSTALLED=$(go version 2>/dev/null | awk '{print $3}' | tr -d 'go')
  if [[ "${GO_INSTALLED}" < "1.22" ]]; then
    warn "Установлен Go ${GO_INSTALLED}; требуется 1.22+. Обновляем / Go ${GO_INSTALLED} is installed; 1.22+ is required. Updating…"
    need_go=true
  else
    ok "Go ${GO_INSTALLED} уже установлен / Go ${GO_INSTALLED} is already installed"
  fi
else
  need_go=true
fi

if ${need_go}; then
  info "Устанавливаем Go ${GO_VERSION} / Installing Go ${GO_VERSION}…"
  ARCH=$( [[ $(uname -m) == "aarch64" ]] && echo "arm64" || echo "amd64" )
  case "$ARCH" in
    amd64) GO_SHA256="9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec" ;;
    arm64) GO_SHA256="6c2f29b458b5add0835f18b776f69c8881a6d88428f1484a6da55755e620757e" ;;
    *) die "Неподдерживаемая архитектура Go / Unsupported Go architecture: ${ARCH}" ;;
  esac
  GO_ARCHIVE=$(mktemp /tmp/aperod-go.XXXXXX.tar.gz) || die "Не удалось создать временный файл Go / Could not create a temporary Go archive"
  if ! wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -O "$GO_ARCHIVE"; then
    rm -f "$GO_ARCHIVE"
    die "Не удалось скачать архив Go ${GO_VERSION} / Could not download the Go ${GO_VERSION} archive"
  fi
  if ! printf '%s  %s\n' "$GO_SHA256" "$GO_ARCHIVE" | sha256sum --check --status; then
    rm -f "$GO_ARCHIVE"
    die "Контрольная сумма Go ${GO_VERSION} не совпала / Go ${GO_VERSION} checksum mismatch"
  fi
  rm -rf /usr/local/go
  if ! tar -C /usr/local -xzf "$GO_ARCHIVE"; then
    rm -f "$GO_ARCHIVE"
    die "Не удалось распаковать проверенный архив Go ${GO_VERSION} / Could not extract the verified Go ${GO_VERSION} archive"
  fi
  rm -f "$GO_ARCHIVE"
  echo 'export PATH=$PATH:/usr/local/go/bin' > /etc/profile.d/go.sh
  export PATH="$PATH:/usr/local/go/bin"
  ok "Go ${GO_VERSION} установлен / Go ${GO_VERSION} installed"
fi

# ── 3. Пользователь aperod ────────────────────────────────
if ! id "${APEROD_USER}" &>/dev/null; then
  useradd -r -m -s /bin/false "${APEROD_USER}"
  ok "Создан системный пользователь / Created system user: ${APEROD_USER}"
else
  ok "Пользователь уже существует / User already exists: ${APEROD_USER}"
fi

# ── 4. Клонирование репозитория ───────────────────────────
info "Получаем исходный код Aperod / Fetching Aperod source code…"
mkdir -p "${INSTALL_DIR}"
node_source_prepare || die "Не удалось получить и проверить выбранный публичный коммит / Could not fetch and verify the selected public commit"
# Build as root only in the private root-owned job, never an account-writable
# checkout. Runtime directories are assigned to the service account separately.
GENESIS_SOURCE="${NODE_SOURCE_DIR}/config/genesis-testnet.yaml"
[[ -f "$GENESIS_SOURCE" && ! -L "$GENESIS_SOURCE" ]] ||
  die "В выбранном коммите нет genesis-конфига; установка остановлена до записи ключей и бинарников / Genesis config is missing from the selected commit; stopped before writing keys or binaries"

# ── 5. Сборка бинарников ──────────────────────────────────
info "Компилируем aperod-node (1–3 минуты) / Building aperod-node (1–3 minutes)…"
cd "${NODE_SOURCE_DIR}"
node_source_build_context "${NODE_SOURCE_DIR}" || die "Не удалось изолировать Git-контекст сборки / Could not isolate the Git build context"
export GOPATH="/root/go"
export PATH="$PATH:/usr/local/go/bin"

env CGO_ENABLED=0 GOWORK=off GOFLAGS= GOENV=off make deps 2>&1 | tail -5
env CGO_ENABLED=0 GOWORK=off GOFLAGS= GOENV=off GOAMD64=v1 make CGO_ENABLED=0 build 2>&1 | tail -10
node_source_candidate_guard "$NODE_SOURCE_DIR" "$NODE_SOURCE_DIR/build/aperod-node" ||
  die "Бинарник не соответствует выбранному чистому коммиту / Binary does not match the selected clean commit or is not portable"

if [[ ! -f "build/aperod-node" ]]; then
  die "Сборка не удалась: build/aperod-node не найден. См. вывод выше. / Build failed: build/aperod-node was not found. See output above."
fi

cp build/aperod-node /usr/local/bin/aperod-node
cp build/aperod      /usr/local/bin/aperod
chmod +x /usr/local/bin/aperod-node /usr/local/bin/aperod
ok "Бинарники установлены / Binaries installed: /usr/local/bin/aperod-node, /usr/local/bin/aperod"

# ── 6. Директории ─────────────────────────────────────────
mkdir -p "${CONFIG_DIR}" "${DATA_DIR}"

# ── 7. Ключ консенсуса валидатора ─────────────────────────
echo
echo -e "${BOLD}============================================================"
echo -e "  Настройка ключа консенсуса / Validator consensus key setup"
echo -e "============================================================${NC}"
echo -e "  Ключ подписывает блоки; средства остаются в Telegram-кошельке."
echo -e "  This key signs blocks; funds remain in your Telegram wallet."
echo

KEY_CHOICE="new"
if [[ -f "${CONFIG_DIR}/validator.key" ]]; then
  warn "Файл ключа уже существует / Key file already exists: ${CONFIG_DIR}/validator.key"
  if [[ "${IS_TTY:-false}" == "true" ]]; then
    read -rp "Перезаписать / Overwrite? [y/N]: " OVERWRITE_KEY
  else
    OVERWRITE_KEY="n"
    info "Неинтерактивный режим — используем существующий ключ / Non-interactive mode — keeping the existing key"
  fi
  if [[ "${OVERWRITE_KEY,,}" != "y" ]]; then
    info "Используем существующий ключ / Keeping the existing key"
    PRIVKEY_HEX=$(xxd -p -c 256 "${CONFIG_DIR}/validator.key" | tr -d '\n' || cat "${CONFIG_DIR}/validator.key")
    PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "")
    KEY_CHOICE="keep"
  fi
fi

if [[ "${KEY_CHOICE}" != "keep" ]]; then
  if [[ "${IS_TTY:-false}" == "true" ]]; then
    echo
    echo "  1) Сгенерировать новый ключ (рекомендуется) / Generate a new key (recommended)"
    echo "  2) Ввести приватный ключ (64 hex-символа) / Enter an existing private key (64 hex characters)"
    echo
    read -rp "Выбор / Choice [1/2]: " KEY_CHOICE
  else
    KEY_CHOICE="1"
    info "Неинтерактивный режим — генерируем новый ключ / Non-interactive mode — generating a new key"
  fi

  if [[ "${KEY_CHOICE}" == "2" ]]; then
    if [[ "${IS_TTY:-false}" != "true" ]]; then
      die "Ввод существующего ключа требует терминала / Entering an existing key requires an interactive terminal"
    fi
    read -rsp "  Введите приватный ключ / Enter private key (64 hex characters; hidden input): " PRIVKEY_HEX
    echo
    if [[ ! "${PRIVKEY_HEX}" =~ ^[0-9a-fA-F]{64}$ ]]; then
      die "Неверный формат ключа: нужны 64 hex-символа (32 байта) / Invalid key format: expected 64 hex characters (32 bytes)"
    fi
    mkdir -p "${CONFIG_DIR}"
    echo -n "${PRIVKEY_HEX}" | xxd -r -p > "${CONFIG_DIR}/validator.key"
    PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "")
    [[ -z "${PUBKEY_HEX}" ]] && PUBKEY_HEX="(запустите / run: aperod wallet pubkey <private-key>)"
    ok "Ключ сохранён / Key saved"
  else
    info "Генерируем новый ключ Ed25519 / Generating a new Ed25519 key…"
    KEYGEN_OUT=$(aperod wallet keygen 2>&1)
    PRIVKEY_HEX=$(echo "${KEYGEN_OUT}" | grep -oP "Private:\s+\K[0-9a-f]+" | head -1 || true)
    PUBKEY_HEX=$(echo "${KEYGEN_OUT}"  | grep -oP "Public:\s+\K[0-9a-f]+"  | head -1 || true)
    if [[ -z "${PRIVKEY_HEX}" ]]; then
      die "Не удалось сгенерировать ключ; вывод скрыт, чтобы не раскрыть секреты / Could not generate key; command output is hidden to avoid exposing secrets"
    fi
    mkdir -p "${CONFIG_DIR}"
    echo -n "${PRIVKEY_HEX}" | xxd -r -p > "${CONFIG_DIR}/validator.key"
    ok "Новый ключ консенсуса создан и сохранён / New consensus key generated and saved"
  fi
fi

# The aperod-node service runs as User=aperod and the binary enforces strict
# permissions (chmod 600).  Wrong owner (root) → EPERM; wrong mode (640) →
# "unsafe permissions" error on startup.  Both are set correctly here.
chmod 600 "${CONFIG_DIR}/validator.key"
chown "${APEROD_USER}:${APEROD_USER}" "${CONFIG_DIR}/validator.key"

echo
echo -e "${GREEN}${BOLD}============================================================"
echo -e "  Публичный ключ валидатора / Validator public key:"
echo -e "     ${PUBKEY_HEX}"
echo -e "============================================================${NC}"
echo

# ── 8. Определяем внешний IP ─────────────────────────────
MY_IP=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null \
     || curl -s --connect-timeout 5 icanhazip.com 2>/dev/null \
     || curl -s --connect-timeout 5 api.ipify.org 2>/dev/null \
     || echo "0.0.0.0")

if [[ "${MY_IP}" == "0.0.0.0" ]]; then
  warn "Не удалось определить внешний IP / Could not determine the public IP automatically"
  if [[ "${IS_TTY:-false}" == "true" ]]; then
    read -rp "Введите публичный IP / Enter this server's public IP: " MY_IP
  else
		die "Нужен публичный IP; небезопасная конфигурация запрещена / A public IP is required; refusing an unsafe 0.0.0.0 configuration"
  fi
fi
[[ -n "${MY_IP}" && "${MY_IP}" != "0.0.0.0" ]] \
	|| die "Нужен публичный IP; установка остановлена / A public IP is required; installation stopped"
info "Внешний IP / Public IP: ${MY_IP}"

# ── 9. Копируем genesis конфиг ────────────────────────────
mkdir -p "${CONFIG_DIR}"
cp "$GENESIS_SOURCE" "${CONFIG_DIR}/genesis-testnet.yaml"
ok "Genesis-конфиг скопирован из проверенного коммита / Genesis config copied from verified commit: ${CONFIG_DIR}/genesis-testnet.yaml"

# ── 10. Конфигурация ноды ─────────────────────────────────
command -v openssl >/dev/null 2>&1 || die "Для API-ключа нужен openssl / openssl is required to generate the API key"
umask 077
API_KEY="$(openssl rand -hex 32)" || die "Не удалось создать API-ключ / Could not generate API key"
[[ ${#API_KEY} -eq 64 ]] || die "Генератор API-ключа вернул неверный результат / API key generator returned an invalid result"
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
  key: ${API_KEY}

genesis:
  file: ${CONFIG_DIR}/genesis-testnet.yaml
EOF
chmod 600 "${CONFIG_DIR}/node.yaml"
chown "${APEROD_USER}:${APEROD_USER}" "${CONFIG_DIR}/node.yaml"
unset API_KEY

ok "Конфигурация сохранена / Configuration saved: ${CONFIG_DIR}/node.yaml"

# ── 11. Firewall ──────────────────────────────────────────
info "Настраиваем ufw / Configuring ufw…"
ufw --force reset >/dev/null 2>&1
ufw default deny incoming  >/dev/null 2>&1
ufw default allow outgoing >/dev/null 2>&1
ufw allow 22/tcp    comment "SSH"              >/dev/null 2>&1
ufw allow ${P2P_PORT}/tcp comment "Aperod P2P"           >/dev/null 2>&1
ufw allow ${P2P_PORT}/udp comment "Aperod P2P discovery" >/dev/null 2>&1
ufw --force enable          >/dev/null 2>&1
ok "Firewall настроен (открыты порты / ports: 22, ${P2P_PORT}/tcp, ${P2P_PORT}/udp)"

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
LimitCORE=0
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

# Write / verify memory-protection drop-ins (timeout.conf + gomemlimit.conf)
# and call daemon-reload so the drop-ins are active before the service starts.
bash "${SCRIPT_DIR}/ensure-dropin.sh"
systemctl enable aperod-node
systemctl start  aperod-node
ok "Сервис aperod-node запущен / aperod-node service started"

# ── Watchdog — автоматический перезапуск при зависании API ────────────────────
# After the node is started, install the watchdog timer that polls
# GET /api/v1/status every 60 s and calls `systemctl restart aperod-node`
# if the probe does not return HTTP 200 within 5 s.
info "Устанавливаем watchdog для aperod-node / Installing aperod-node watchdog…"
cp "${SCRIPT_DIR}/aperod-node-watchdog.sh" /usr/local/bin/aperod-node-watchdog.sh
chmod +x /usr/local/bin/aperod-node-watchdog.sh
cp "${SCRIPT_DIR}/aperod-node-watchdog.service" /etc/systemd/system/
cp "${SCRIPT_DIR}/aperod-node-watchdog.timer"   /etc/systemd/system/
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
ok "Watchdog установлен / Watchdog installed (aperod-node-watchdog.timer is running)"

sleep 3
if systemctl is-active --quiet aperod-node; then
  ok "Нода работает / Node is running"
else
  warn "Сервис запущен; проверьте журнал / Service started; check logs: journalctl -u aperod-node -n 50"
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}============================================================"
echo -e "${GREEN}${BOLD}  Установка завершена успешно / Installation completed successfully${NC}"
echo -e "${GREEN}${BOLD}============================================================${NC}"
echo
echo -e "  Статус ноды / Node status:       systemctl status aperod-node"
echo -e "  Логи / Logs:                     journalctl -u aperod-node -f"
echo -e "  Конфиг / Config:                 ${CONFIG_DIR}/node.yaml"
echo -e "  Данные / Data:                   ${DATA_DIR}"
echo -e "  P2P-адрес / P2P endpoint:        /ip4/${MY_IP}/tcp/${P2P_PORT}"
echo
echo -e "${GREEN}${BOLD}============================================================"
echo -e "${GREEN}${BOLD}  Адрес вознаграждений / Reward address:${NC}"
echo -e "  ${BOLD}${REWARD_ADDRESS}${NC}"
echo -e "${GREEN}${BOLD}============================================================${NC}"
echo
echo -e "  Вознаграждения поступают на этот адрес в Telegram-кошелёк."
echo -e "  Block rewards will be sent to this address in your Telegram wallet."
echo -e "  ${CYAN}https://t.me/aperod_bot${NC}"
echo

echo -e "${YELLOW}${BOLD}  Следующий шаг / Next: зарегистрируйте ноду и сделайте стейк / register the node and stake:${NC}"
echo
echo -e "  1) Проверьте ноду / Check the node:"
echo -e "     ${CYAN}journalctl -u aperod-node -f${NC}"
echo
echo -e "  2) Зарегистрируйте ноду / Register the node (run this command):"
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
echo -e "  3) Переведите минимум / Transfer at least ${BOLD}100,000 APRO${NC} на адрес кошелька / to your wallet address:"
echo -e "     ${BOLD}${REWARD_ADDRESS}${NC}"
echo -e "     Валидатор активируется автоматически / The node joins the active validator set automatically."
echo
echo -e "  ${BOLD}Параметры сети / Network parameters:${NC}"
echo -e "    Мин. стейк / Minimum stake        : 100,000 APRO"
echo -e "    Макс. валидаторов / Max validators: 21"
echo -e "    Эпоха / Epoch                     : 100 блоков / blocks (~1.7 мин / min)"
echo -e "    Анбондинг / Unbonding              : 7,200 блоков / blocks (~2 часа / hours)"
echo -e "    Штраф за двойную подпись / Double-sign penalty: 10% стейка / of stake"
echo
echo -e "  Публичный ключ / Public key: ${BOLD}${PUBKEY_HEX}${NC}"
echo -e "  P2P-адрес / P2P endpoint:   /ip4/${MY_IP}/tcp/${P2P_PORT}"
echo -e "  Документация / Docs:        ${CYAN}https://aperod.com/docs${NC}"
echo
