#!/usr/bin/env bash
# ============================================================
#  Aperod Validator Node — Uninstaller
#  Полностью удаляет ноду с сервера
#  Использование:  sudo bash uninstall-validator.sh
# ============================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info() { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }

APEROD_USER="aperod"
INSTALL_DIR="/opt/aperod"
DATA_DIR="/var/lib/aperod"
CONFIG_DIR="/etc/aperod"

echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod Validator Node — Uninstaller                 ║
╚════════════════════════════════════════════════════════════╝${NC}
"

if [[ $(id -u) -ne 0 ]]; then
  echo -e "${RED}Запустите от root: sudo bash uninstall-validator.sh${NC}"
  exit 1
fi

echo -e "${YELLOW}${BOLD}Это действие необратимо. Будут удалены:${NC}"
echo -e "  • Сервис  aperod-node (systemd)"
echo -e "  • Бинарники  /usr/local/bin/aperod-node, /usr/local/bin/aperod"
echo -e "  • Конфиг  ${CONFIG_DIR}/"
echo -e "  • Данные  ${DATA_DIR}/  (блокчейн-данные, ключи!)"
echo -e "  • Исходники  ${INSTALL_DIR}/"
echo -e "  • Системный пользователь  ${APEROD_USER}"
echo -e "  • Правила ufw для портов 30303"
echo

# Подтверждение
if [[ -t 0 ]]; then
  read -rp "Введите YES для подтверждения: " CONFIRM
  if [[ "${CONFIRM^^}" != "YES" ]]; then
    echo "Отменено."
    exit 0
  fi
else
  warn "Неинтерактивный режим — передайте APEROD_UNINSTALL_CONFIRM=YES"
  if [[ "${APEROD_UNINSTALL_CONFIRM:-}" != "YES" ]]; then
    echo "Отменено."
    exit 1
  fi
fi

echo

# 1. Остановить и отключить сервис
if systemctl is-active --quiet aperod-node 2>/dev/null; then
  info "Останавливаем aperod-node…"
  systemctl stop aperod-node
  ok "Сервис остановлен"
fi

if systemctl is-enabled --quiet aperod-node 2>/dev/null; then
  systemctl disable aperod-node
  ok "Сервис отключён из автозапуска"
fi

if [[ -f /etc/systemd/system/aperod-node.service ]]; then
  rm -f /etc/systemd/system/aperod-node.service
  systemctl daemon-reload
  ok "Unit-файл удалён"
fi

# 2. Бинарники
for bin in /usr/local/bin/aperod-node /usr/local/bin/aperod; do
  if [[ -f "${bin}" ]]; then
    rm -f "${bin}"
    ok "Удалён: ${bin}"
  fi
done

# 3. Конфиг (включая ключи!)
if [[ -d "${CONFIG_DIR}" ]]; then
  rm -rf "${CONFIG_DIR}"
  ok "Удалён конфиг: ${CONFIG_DIR}"
fi

# 4. Данные блокчейна
if [[ -d "${DATA_DIR}" ]]; then
  rm -rf "${DATA_DIR}"
  ok "Удалены данные: ${DATA_DIR}"
fi

# 5. Исходники
if [[ -d "${INSTALL_DIR}" ]]; then
  rm -rf "${INSTALL_DIR}"
  ok "Удалены исходники: ${INSTALL_DIR}"
fi

# 6. Системный пользователь
if id "${APEROD_USER}" &>/dev/null; then
  userdel "${APEROD_USER}" 2>/dev/null || true
  ok "Удалён пользователь: ${APEROD_USER}"
fi

# 7. Go (опционально)
if [[ -t 0 ]]; then
  echo
  read -rp "Удалить Go (/usr/local/go)? [y/N]: " DEL_GO
  if [[ "${DEL_GO,,}" == "y" ]]; then
    rm -rf /usr/local/go
    rm -f /etc/profile.d/go.sh
    ok "Go удалён"
  else
    info "Go оставлен"
  fi
fi

# 8. Правила ufw
if command -v ufw &>/dev/null; then
  ufw delete allow 30303/tcp >/dev/null 2>&1 || true
  ufw delete allow 30303/udp >/dev/null 2>&1 || true
  ok "Правила ufw для порта 30303 удалены"
fi

echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Нода Aperod полностью удалена с сервера.${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  Ваш Telegram-кошелёк (${CYAN}t.me/aperod_bot${NC}) не затронут."
echo -e "  APR-адрес и средства в кошельке сохранены."
echo
