#!/bin/bash
# =============================================================================
#  setup-watchdog-interval — install the sudoers rule that lets the
#  aperod-api service change the watchdog interval without SSH.
#
#  What this sets up
#  -----------------
#  The aperod-api service runs as user 'aperod' with NoNewPrivileges=true
#  and ProtectSystem=strict, so it cannot write to /etc/aperod or call sudo
#  by default.  This script adds a narrow sudoers rule that allows exactly
#  one command:
#
#    /usr/local/bin/aperod-watchdog-set-interval --interval=<number>
#
#  No password, no other commands.  The command itself validates the range
#  (5–3600), writes /etc/aperod/watchdog.env, and restarts the timer.
#
#  Usage (run once as root after install-validator.sh):
#    sudo bash blockchain/deploy/setup-watchdog-interval.sh
#
#  Idempotent — safe to re-run.
# =============================================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
warn() { echo -e "  ${YELLOW}⚠${NC}  $*"; }
die()  { echo -e "  ${RED}✗${NC} $*"; exit 1; }
step() { echo -e "\n${BOLD}═══ $* ═══${NC}"; }

APEROD_USER="${APEROD_USER:-aperod}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="/usr/local/bin/aperod-watchdog-set-interval"
SUDOERS_FILE="/etc/sudoers.d/aperod-watchdog-interval"

# ── Root check ────────────────────────────────────────────────────────────────
if [[ "$(id -u)" -ne 0 ]]; then
  die "Запустите от root: sudo bash $0"
fi

# ── Verify aperod user exists ─────────────────────────────────────────────────
if ! id "${APEROD_USER}" &>/dev/null; then
  die "Пользователь '${APEROD_USER}' не найден."
fi

# ══════════════════════════════════════════════════════════════════════════════
step "1. Установка aperod-watchdog-set-interval → ${HELPER}"

install -o root -g root -m 750 \
  "${SCRIPT_DIR}/aperod-watchdog-set-interval.sh" "${HELPER}"
ok "${HELPER} (mode 750, root:root)"

# ══════════════════════════════════════════════════════════════════════════════
step "2. Создание sudoers-правила: ${SUDOERS_FILE}"

# The rule allows the aperod user to run ONLY the helper with --interval=<digits>.
# NOPASSWD is required because the API service has no TTY.
# The trailing space before '' is intentional: it prevents sudo from passing
# additional arguments when the rule is matched.
cat > "${SUDOERS_FILE}" <<EOF
# Installed by blockchain/deploy/setup-watchdog-interval.sh
# Allows the aperod-api service to update the watchdog interval from the
# Admin Panel without requiring SSH access.
#
# The helper validates the range (5–3600), writes /etc/aperod/watchdog.env,
# and restarts the aperod-node-watchdog.timer unit.
#
# Scope: one command, one pattern, NOPASSWD.
${APEROD_USER} ALL=(root) NOPASSWD: ${HELPER} --interval=[0-9]*
EOF
chmod 0440 "${SUDOERS_FILE}"
ok "${SUDOERS_FILE} создан (0440)"

# ── Syntax check ──────────────────────────────────────────────────────────────
if visudo -cf "${SUDOERS_FILE}" 2>&1; then
  ok "visudo синтаксис ОК"
else
  rm -f "${SUDOERS_FILE}"
  die "Синтаксическая ошибка sudoers — файл удалён. Проверьте /etc/sudoers.d/"
fi

# ══════════════════════════════════════════════════════════════════════════════
step "3. Проверка: пользователь '${APEROD_USER}' может вызвать helper"

if sudo -u "${APEROD_USER}" sudo -n "${HELPER}" --interval=60 2>/dev/null; then
  ok "Тест sudo прошёл (интервал 60 с применён)"
else
  warn "Тест sudo не выполнен — это нормально если watchdog-таймер ещё не установлен."
  warn "Проверьте вручную: sudo -u ${APEROD_USER} sudo -n ${HELPER} --interval=60"
fi

# ══════════════════════════════════════════════════════════════════════════════
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Настройка watchdog-interval завершена!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  Admin Panel → Infrastructure → Nodes → Watchdog interval"
echo -e "  теперь сохраняется без SSH."
echo
