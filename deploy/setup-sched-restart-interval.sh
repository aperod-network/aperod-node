#!/bin/bash
# =============================================================================
#  setup-sched-restart-interval — install the sudoers rule that lets the
#  aperod-api service change the scheduled-restart interval without SSH.
#
#  What this sets up
#  -----------------
#  The aperod-api service runs as user 'aperod' with NoNewPrivileges=true
#  and ProtectSystem=strict, so it cannot write to /etc/aperod or call sudo
#  by default.  This script adds a narrow sudoers rule that allows exactly
#  one command:
#
#    /usr/local/bin/aperod-sched-restart-set-interval --interval=<number>
#
#  No password, no other commands.  The command itself validates the range
#  (3600–86400), writes /etc/aperod/sched-restart.env, and restarts the timer.
#
#  Usage (run once as root after install-validator.sh):
#    sudo bash blockchain/deploy/setup-sched-restart-interval.sh
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
HELPER="/usr/local/bin/aperod-sched-restart-set-interval"
SUDOERS_FILE="/etc/sudoers.d/aperod-sched-restart-interval"

# ── Root check ────────────────────────────────────────────────────────────────
if [[ "$(id -u)" -ne 0 ]]; then
  die "Запустите от root: sudo bash $0"
fi

# ── Verify aperod user exists ─────────────────────────────────────────────────
if ! id "${APEROD_USER}" &>/dev/null; then
  die "Пользователь '${APEROD_USER}' не найден."
fi

# ══════════════════════════════════════════════════════════════════════════════
step "1. Установка aperod-sched-restart-set-interval → ${HELPER}"

install -o root -g root -m 750 \
  "${SCRIPT_DIR}/aperod-sched-restart-set-interval.sh" "${HELPER}"
ok "${HELPER} (mode 750, root:root)"

# ══════════════════════════════════════════════════════════════════════════════
step "2. Создание sudoers-правила: ${SUDOERS_FILE}"

# The rule allows the aperod user to run ONLY the helper with --interval=<digits>.
# NOPASSWD is required because the API service has no TTY.
mkdir -p "$(dirname "${SUDOERS_FILE}")"
cat > "${SUDOERS_FILE}" <<EOF
# Installed by blockchain/deploy/setup-sched-restart-interval.sh
# Allows the aperod-api service to update the scheduled-restart interval from
# the Admin Panel without requiring SSH access.
#
# The helper validates the range (3600–86400), writes /etc/aperod/sched-restart.env,
# and restarts the aperod-node-sched-restart.timer unit.
#
# Scope: one command, one pattern, NOPASSWD.
${APEROD_USER} ALL=(root) NOPASSWD: ${HELPER} --interval=[0-9]*
EOF
chmod 0440 "${SUDOERS_FILE}"
ok "${SUDOERS_FILE} создан (0440)"

# ── Syntax check (skipped when visudo is not available, e.g. in CI) ───────────
if command -v visudo >/dev/null 2>&1; then
  if visudo -cf "${SUDOERS_FILE}" 2>&1; then
    ok "visudo синтаксис ОК"
  else
    rm -f "${SUDOERS_FILE}"
    die "Синтаксическая ошибка sudoers — файл удалён. Проверьте /etc/sudoers.d/"
  fi
else
  warn "visudo не найден — пропускаем синтаксическую проверку (CI-окружение)"
fi

# ══════════════════════════════════════════════════════════════════════════════
step "3. Проверка: пользователь '${APEROD_USER}' может вызвать helper"

if sudo -u "${APEROD_USER}" sudo -n "${HELPER}" --interval=3600 2>/dev/null; then
  ok "Тест sudo прошёл (интервал 3600 с применён)"
else
  warn "Тест sudo не выполнен — это нормально если sched-restart-таймер ещё не установлен."
  warn "Проверьте вручную: sudo -u ${APEROD_USER} sudo -n ${HELPER} --interval=3600"
fi

# ══════════════════════════════════════════════════════════════════════════════
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Настройка sched-restart-interval завершена!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  Admin Panel → Infrastructure → Nodes → Scheduled Restart interval"
echo -e "  теперь сохраняется без SSH."
echo
