#!/usr/bin/env bash
  # ============================================================
  #  Aperod Validator Node — Automatic Installer
  #  Поддерживается: Ubuntu 22.04 / 24.04 / Debian 12
  #  Использование:  sudo bash install-validator.sh
  # ============================================================
  set -euo pipefail

  RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
  CYAN='\033[0;36m';  BOLD='\033[1m';  NC='\033[0m'
  info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
  ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
  warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
  die()   { echo -e "${RED}[ERR]${NC}   $*"; exit 1; }

  APEROD_USER="aperod"
  INSTALL_DIR="/opt/aperod"
  DATA_DIR="/var/lib/aperod"
  CONFIG_DIR="/etc/aperod"
  GO_VERSION="1.23.4"
  REPO_URL="https://github.com/aperod-network/aperod-node.git"
  P2P_PORT=30303
  RPC_PORT=8545

  echo -e "
  ${BOLD}╔════════════════════════════════════════════════════════════╗
  ║        Aperod Validator Node — Installer                   ║
  ║        aperod.com  |  t.me/aperod_bot                      ║
  ╚════════════════════════════════════════════════════════════╝${NC}
  "

  if [[ $(id -u) -ne 0 ]]; then
    die "Запустите скрипт от root: sudo bash install-validator.sh"
  fi

  if ! grep -qE "(Ubuntu|Debian)" /etc/os-release 2>/dev/null; then
    warn "Скрипт тестировался на Ubuntu 22.04/24.04 и Debian 12. Продолжить? [y/N]"
    read -rp "" CONFIRM
    [[ "${CONFIRM,,}" == "y" ]] || die "Установка отменена"
  fi

  IS_TTY=false
  if [[ -t 0 ]]; then IS_TTY=true; fi

  # ── Шаг 0: APR-адрес из Telegram-кошелька ────────────────
  echo -e "${YELLOW}${BOLD}══════════════════════════════════════════════════════════════"
  echo -e "  ВАЖНО: перед установкой нужен APR-адрес из Telegram-кошелька"
  echo -e "══════════════════════════════════════════════════════════════${NC}"
  echo
  echo -e "  Все вознаграждения за блоки поступают на ваш Telegram-кошелёк."
  echo -e "  Если кошелька ещё нет — создайте его:"
  echo
  echo -e "  ${BOLD}1. Откройте бот: https://t.me/aperod_bot${NC}"
  echo -e "  ${BOLD}2. Нажмите «Создать кошелёк»${NC}"
  echo -e "  ${BOLD}3. Скопируйте ваш APR-адрес (~95 символов)${NC}"
  echo

  REWARD_ADDRESS=""

  if [[ "${IS_TTY}" == "true" ]]; then
    while true; do
      echo -e "${BOLD}Введите ваш APR-адрес из Telegram-кошелька:${NC}"
      read -rp "  > " REWARD_ADDRESS
      REWARD_ADDRESS="${REWARD_ADDRESS// /}"
      if [[ ${#REWARD_ADDRESS} -ge 80 ]]; then
        ok "Адрес принят: ${REWARD_ADDRESS:0:20}…${REWARD_ADDRESS: -8}"
        break
      else
        warn "Адрес слишком короткий (${#REWARD_ADDRESS} символов). APR-адрес ~95 символов. Попробуйте ещё раз."
      fi
    done
  else
    if [[ -n "${APEROD_REWARD_ADDRESS:-}" ]]; then
      REWARD_ADDRESS="${APEROD_REWARD_ADDRESS}"
      ok "Адрес из переменной окружения: ${REWARD_ADDRESS:0:20}…"
    else
      die "Неинтерактивный режим: передайте адрес через:
      APEROD_REWARD_ADDRESS=<ваш-apr-адрес> bash install-validator.sh"
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
  info "Клонируем репозиторий aperod-node…"
  if [[ -d "${INSTALL_DIR}/.git" ]]; then
    git -C "${INSTALL_DIR}" pull --quiet
    ok "Репозиторий обновлён"
  else
    rm -rf "${INSTALL_DIR}"
    git clone --depth 1 --quiet "${REPO_URL}" "${INSTALL_DIR}"
    ok "Репозиторий клонирован в ${INSTALL_DIR}"
  fi

  # ── 5. Сборка ─────────────────────────────────────────────
  info "Компилируем aperod-node (1–3 мин)…"
  cd "${INSTALL_DIR}"
  export GOPATH="/root/go"
  export PATH="$PATH:/usr/local/go/bin"
  make deps 2>&1 | tail -5
  make build 2>&1 | tail -10

  if [[ ! -f "build/aperod-node" ]]; then
    die "Сборка не удалась. Проверьте вывод выше."
  fi

  cp build/aperod-node /usr/local/bin/aperod-node
  cp build/aperod      /usr/local/bin/aperod
  chmod +x /usr/local/bin/aperod-node /usr/local/bin/aperod
  ok "Бинарники установлены"

  # ── 6. Директории ─────────────────────────────────────────
  mkdir -p "${CONFIG_DIR}" "${DATA_DIR}"

  # ── 7. Ключ консенсуса ────────────────────────────────────
  echo
  echo -e "${BOLD}═══════════════════════════════════════════════"
  echo -e "  Настройка ключа консенсуса валидатора"
  echo -e "═══════════════════════════════════════════════${NC}"
  echo -e "  (Этот ключ только для подписи блоков."
  echo -e "   Средства хранятся в Telegram-кошельке.)"
  echo

  KEY_CHOICE="new"
  if [[ -f "${CONFIG_DIR}/validator.key" ]]; then
    warn "Ключ уже существует: ${CONFIG_DIR}/validator.key"
    if [[ "${IS_TTY}" == "true" ]]; then
      read -rp "Перезаписать? [y/N]: " OVERWRITE_KEY
    else
      OVERWRITE_KEY="n"
    fi
    if [[ "${OVERWRITE_KEY,,}" != "y" ]]; then
      info "Используем существующий ключ"
      PRIVKEY_HEX=$(xxd -p -c 256 "${CONFIG_DIR}/validator.key" | tr -d '\n' || cat "${CONFIG_DIR}/validator.key")
      PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "")
      KEY_CHOICE="keep"
    fi
  fi

  if [[ "${KEY_CHOICE}" != "keep" ]]; then
    if [[ "${IS_TTY}" == "true" ]]; then
      echo
      echo "  1) Сгенерировать новый ключ (рекомендуется)"
      echo "  2) Ввести существующий приватный ключ (hex, 64 символа)"
      echo
      read -rp "Выбор [1/2]: " KEY_CHOICE
    else
      KEY_CHOICE="1"
    fi

    if [[ "${KEY_CHOICE}" == "2" ]]; then
      read -rsp "  Приватный ключ (64 hex, ввод скрыт): " PRIVKEY_HEX
      echo
      if [[ ! "${PRIVKEY_HEX}" =~ ^[0-9a-fA-F]{64}$ ]]; then
        die "Неверный формат ключа."
      fi
      echo -n "${PRIVKEY_HEX}" | xxd -r -p > "${CONFIG_DIR}/validator.key"
      PUBKEY_HEX=$(aperod wallet pubkey "${PRIVKEY_HEX}" 2>/dev/null || echo "")
      ok "Ключ сохранён"
    else
      info "Генерируем новый ключ Ed25519…"
      KEYGEN_OUT=$(aperod wallet keygen 2>&1)
      PRIVKEY_HEX=$(echo "${KEYGEN_OUT}" | grep -oP "Private:\s+\K[0-9a-f]+" | head -1 || true)
      PUBKEY_HEX=$(echo "${KEYGEN_OUT}"  | grep -oP "Public:\s+\K[0-9a-f]+"  | head -1 || true)
      if [[ -z "${PRIVKEY_HEX}" ]]; then
        die "Не удалось сгенерировать ключ."
      fi
      echo -n "${PRIVKEY_HEX}" | xxd -r -p > "${CONFIG_DIR}/validator.key"
      ok "Ключ консенсуса сгенерирован"
    fi
  fi

  chmod 640 "${CONFIG_DIR}/validator.key"
  chown root:"${APEROD_USER}" "${CONFIG_DIR}/validator.key"

  echo
  echo -e "${GREEN}${BOLD}  ✓  Публичный ключ валидатора (для регистрации ноды):"
  echo -e "     ${PUBKEY_HEX}${NC}"
  echo

  # ── 8. Внешний IP ─────────────────────────────────────────
  MY_IP=$(curl -s --connect-timeout 5 ifconfig.me 2>/dev/null || curl -s --connect-timeout 5 api.ipify.org 2>/dev/null || echo "0.0.0.0")

  # ── 9. Genesis конфиг ────────────────────────────────────
  if [[ -f "${INSTALL_DIR}/config/genesis-testnet.yaml" ]]; then
    cp "${INSTALL_DIR}/config/genesis-testnet.yaml" "${CONFIG_DIR}/genesis-testnet.yaml"
    ok "Genesis скопирован"
  fi

  # ── 10. Конфигурация ноды ────────────────────────────────
  cat > "${CONFIG_DIR}/node.yaml" <<EOF
  # Aperod Node Configuration — auto-created by install-validator.sh

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

  # ── 11. Firewall ─────────────────────────────────────────
  info "Настраиваем ufw…"
  ufw --force reset >/dev/null 2>&1
  ufw default deny incoming  >/dev/null 2>&1
  ufw default allow outgoing >/dev/null 2>&1
  ufw allow 22/tcp    >/dev/null 2>&1
  ufw allow ${P2P_PORT}/tcp >/dev/null 2>&1
  ufw allow ${P2P_PORT}/udp >/dev/null 2>&1
  ufw --force enable  >/dev/null 2>&1
  ok "Firewall настроен"

  # ── 12. systemd сервис ───────────────────────────────────
  cat > /etc/systemd/system/aperod-node.service <<EOF
  [Unit]
  Description=Aperod Validator Node
  After=network-online.target
  Wants=network-online.target

  [Service]
  Type=simple
  User=${APEROD_USER}
  Group=${APEROD_USER}
  ExecStart=/usr/local/bin/aperod-node --config ${CONFIG_DIR}/node.yaml
  Restart=always
  RestartSec=5
  LimitNOFILE=65536
  StandardOutput=journal
  StandardError=journal
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

  sleep 3
  systemctl is-active --quiet aperod-node && ok "Нода работает" || warn "Проверьте: journalctl -u aperod-node -n 50"

  # ── Итог ─────────────────────────────────────────────────
  echo
  echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
  echo -e "${GREEN}${BOLD}  ✓  Установка завершена!${NC}"
  echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
  echo
  echo -e "  ${BOLD}Адрес вознаграждений:${NC}  ${REWARD_ADDRESS}"
  echo -e "  ${BOLD}Статус:${NC}                systemctl status aperod-node"
  echo -e "  ${BOLD}Логи:${NC}                  journalctl -u aperod-node -f"
  echo -e "  ${BOLD}Конфиг:${NC}                ${CONFIG_DIR}/node.yaml"
  echo
  echo -e "${YELLOW}${BOLD}  Следующие шаги:${NC}"
  echo
  echo -e "  1) Зарегистрируйте ноду:"
  echo
  echo -e "  ${GREEN}curl -s -X POST https://aperod.com/api/validators/apply \\"
  echo -e "    -H 'Content-Type: application/json' \\"
  echo -e "    -d '{"
  echo -e "      "pubKey":   "${PUBKEY_HEX}","
  echo -e "      "alias":    "my-validator","
  echo -e "      "endpoint": "/ip4/${MY_IP}/tcp/${P2P_PORT}","
  echo -e "      "address":  "${REWARD_ADDRESS}""
  echo -e "    }'${NC}"
  echo
  echo -e "  2) Переведите минимум ${BOLD}100 000 APR${NC} на адрес ${BOLD}${REWARD_ADDRESS}${NC}"
  echo -e "     Нода активируется автоматически в следующей эпохе."
  echo
  echo -e "  Telegram-кошелёк: ${CYAN}https://t.me/aperod_bot${NC}"
  echo
  