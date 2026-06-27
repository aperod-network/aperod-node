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
iNSTALL_DIR="/opt/aperod"
DATA_DIR="/var/lib/aperod"
CONFIG_DIR="/etc/aperod"
WALLET_DIR="/etc/aperod/wallet"
GO_VERSION="1.23.4"
REPO_URL="https://github.com/aperod-network/aperod-node.git"
P2P_PORT=30303
RPC_PORT=8545

echo -e "
${BOLD}╔═══════════════════════════════════════════╗
║   Aperod APR Full Node — Installer         ║
║   github.com/aperod-network/aperod-node    ║
╚═══════════════════════════════════════════╝${NC}
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
  info "Устанавливаем Go ${GO_VERSION}���"
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
export GIT_TERMINAL_PROMPT=0
if [[ -d "${INSTALL_DIR}/.git" ]]; then
  git -C "${INSTALL_DIR}" pull --ff-only
  ok "Репозиторий обновлён"
else
  git clone --depth=1 "${REPO_URL}" "${INSTALL_DIR}" || die "Не удалось клонировать ${REPO_URL}"
  ok "Репозиторий клонирован в ${INSTALL_DIR}"
fi
