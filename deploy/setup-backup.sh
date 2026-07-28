#!/bin/bash
# ==========================================================
# Aperod Backup — One-Time Setup Script
#
# Installs and activates the privileged backup pipeline:
#   1. aperod_backup.sh          → /usr/local/bin/
#   2. aperod-backup.service     → /etc/systemd/system/
#   3. aperod-backup.path        → /etc/systemd/system/
#   4. aperod-backup-trigger.conf → /etc/tmpfiles.d/
#   5. Creates /run/aperod (owned by aperod user) via tmpfiles
#   6. Patches aperod-api.service with ReadWritePaths=/run/aperod
#   7. Enables and starts aperod-backup.path
#   8. Verifies the API service can create the trigger file
#
# Usage:
#   sudo bash blockchain/deploy/setup-backup.sh
#
# Idempotent — safe to re-run.
# ==========================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
ok()   { echo -e "  ${GREEN}✓${NC} $*"; }
warn() { echo -e "  ${YELLOW}⚠${NC}  $*"; }
die()  { echo -e "  ${RED}✗${NC} $*"; exit 1; }
step() { echo -e "\n${BOLD}═══ $* ═══${NC}"; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APEROD_USER="${APEROD_USER:-aperod}"
TRIGGER_DIR="/run/aperod"
TRIGGER_FILE="${TRIGGER_DIR}/backup-trigger"
API_UNIT="aperod-api.service"
DROPIN_DIR="/etc/systemd/system/${API_UNIT}.d"
DROPIN_FILE="${DROPIN_DIR}/backup-trigger.conf"

# ── Root check ────────────────────────────────────────────────────────────────
if [[ "$(id -u)" -ne 0 ]]; then
  die "Запустите от root: sudo bash $0"
fi

# ── Verify aperod user exists ─────────────────────────────────────────────────
if ! id "${APEROD_USER}" &>/dev/null; then
  die "Пользователь '${APEROD_USER}' не найден. Создайте его или задайте APEROD_USER=другой_пользователь"
fi

# ═══════════════════════════════════════════════════════════════════════════════
step "1. Установка скрипта бэкапа"

install -o root -g root -m 700 "${SCRIPT_DIR}/aperod_backup.sh" /usr/local/bin/aperod_backup.sh
ok "aperod_backup.sh → /usr/local/bin/aperod_backup.sh (mode 700)"

# ═══════════════════════════════════════════════════════════════════════════════
step "2. Установка systemd unit-файлов"

install -o root -g root -m 644 "${SCRIPT_DIR}/aperod-backup.service" /etc/systemd/system/aperod-backup.service
ok "aperod-backup.service → /etc/systemd/system/"

install -o root -g root -m 644 "${SCRIPT_DIR}/aperod-backup.path" /etc/systemd/system/aperod-backup.path
ok "aperod-backup.path → /etc/systemd/system/"

# ═══════════════════════════════════════════════════════════════════════════════
step "3. Установка tmpfiles.d (создаёт /run/aperod, владелец: ${APEROD_USER})"

# Render the conf file with the actual username
cat > /etc/tmpfiles.d/aperod-backup-trigger.conf <<EOF
# Creates /run/aperod owned by '${APEROD_USER}' so that the API service
# (running as ${APEROD_USER}, NoNewPrivileges=true) can write the backup trigger.
d /run/aperod 0775 ${APEROD_USER} ${APEROD_USER} -
EOF
ok "/etc/tmpfiles.d/aperod-backup-trigger.conf создан"

# Apply immediately (creates /run/aperod now; the rule also runs at every boot)
systemd-tmpfiles --create /etc/tmpfiles.d/aperod-backup-trigger.conf
ok "/run/aperod создан: $(stat -c '%U:%G %a' ${TRIGGER_DIR})"

# ═══════════════════════════════════════════════════════════════════════════════
step "4. Добавление ReadWritePaths=/run/aperod в ${API_UNIT}"
# Use a drop-in override so the original unit is never modified directly.

mkdir -p "${DROPIN_DIR}"
cat > "${DROPIN_FILE}" <<EOF
# Drop-in created by blockchain/deploy/setup-backup.sh
# Grants the aperod-api service write access to the backup trigger directory.
[Service]
ReadWritePaths=/run/aperod
EOF
ok "Drop-in создан: ${DROPIN_FILE}"

# ═══════════════════════════════════════════════════════════════════════════════
step "5. daemon-reload + запуск aperod-backup.path"

systemctl daemon-reload
ok "systemctl daemon-reload"

# Restart API so it picks up the new ReadWritePaths
if systemctl is-active --quiet "${API_UNIT}" 2>/dev/null; then
  systemctl restart "${API_UNIT}"
  ok "${API_UNIT} перезапущен (принял ReadWritePaths)"
else
  warn "${API_UNIT} не запущен — пропускаем перезапуск"
fi

systemctl enable --now aperod-backup.path
ok "aperod-backup.path включён и запущен"

# ═══════════════════════════════════════════════════════════════════════════════
step "6. Проверка: API-сервис может создать trigger-файл"

# Simulate what the API does: write the file as the aperod user
if sudo -u "${APEROD_USER}" touch "${TRIGGER_FILE}" 2>/dev/null; then
  ok "Пользователь '${APEROD_USER}' успешно создал ${TRIGGER_FILE}"
  rm -f "${TRIGGER_FILE}"
else
  warn "Не удалось создать ${TRIGGER_FILE} от имени '${APEROD_USER}'. Проверьте права: ls -la /run/aperod"
fi

# ═══════════════════════════════════════════════════════════════════════════════
step "7. Проверка: aperod-backup.path активен"

if systemctl is-active --quiet aperod-backup.path; then
  ok "aperod-backup.path: $(systemctl is-active aperod-backup.path)"
else
  warn "aperod-backup.path не активен. Проверьте: systemctl status aperod-backup.path"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Система бэкапа установлена и готова к работе!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  ${BOLD}Следующие шаги:${NC}"
echo
echo -e "  1. Задайте пароль шифрования (${BOLD}один раз${NC}, запишите его!):"
echo -e "     ${CYAN}sudo bash -c 'echo \"APEROD_BACKUP_PASSWORD=\$(openssl rand -hex 32)\" >> /etc/environment'${NC}"
echo -e "     ${CYAN}sudo systemctl restart ${API_UNIT}${NC}"
echo
echo -e "  2. Запустите первый бэкап из Admin Panel → Integrations → S3-хранилище"
echo -e "     или вручную:"
echo -e "     ${CYAN}sudo bash /usr/local/bin/aperod_backup.sh${NC}"
echo
echo -e "  3. Настройте cron (бэкап каждые 12 часов):"
echo -e "     ${CYAN}echo \"0 */12 * * * root /usr/local/bin/aperod_backup.sh >> /var/log/aperod_backup.log 2>&1\" \\"
echo -e "       | sudo tee /etc/cron.d/aperod-backup${NC}"
echo
echo -e "  4. Проверьте статус:"
echo -e "     ${CYAN}sudo bash blockchain/deploy/check-system.sh${NC}"
echo
