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

# Verify the installed file is non-empty, executable, and syntactically valid.
# A truncated write (e.g. disk-full during install) or wrong permissions would
# cause a silent failure at backup time — catch it now instead.
[[ -s /usr/local/bin/aperod_backup.sh ]] \
  || die "aperod_backup.sh установлен, но файл пустой — возможна неполная запись"
[[ -x /usr/local/bin/aperod_backup.sh ]] \
  || die "aperod_backup.sh не исполняемый после install — проверьте файловую систему"
bash -n /usr/local/bin/aperod_backup.sh \
  || die "aperod_backup.sh не прошёл синтаксическую проверку (bash -n) — файл мог быть усечён при записи"
ok "aperod_backup.sh прошёл синтаксическую проверку (bash -n)"

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
step "8. Пароль шифрования в /etc/aperod/backup-secrets.env (root-only 0600)"

SECRETS_DIR="/etc/aperod"
SECRETS_FILE="${SECRETS_DIR}/backup-secrets.env"

mkdir -p "${SECRETS_DIR}"
chmod 700 "${SECRETS_DIR}"

if [ -f "${SECRETS_FILE}" ] && grep -q 'APEROD_BACKUP_PASSWORD' "${SECRETS_FILE}" 2>/dev/null; then
  ok "APEROD_BACKUP_PASSWORD уже задан в ${SECRETS_FILE}"
else
  # Migrate from /etc/environment if the variable exists there
  LEGACY_PASS=$(grep -o 'APEROD_BACKUP_PASSWORD=[^ ]*' /etc/environment 2>/dev/null | head -1 | cut -d= -f2-)
  if [ -n "${LEGACY_PASS}" ]; then
    (umask 077 && printf 'APEROD_BACKUP_PASSWORD=%s\n' "${LEGACY_PASS}" > "${SECRETS_FILE}")
    chown root:root "${SECRETS_FILE}"
    chmod 600 "${SECRETS_FILE}"
    # Remove from world-readable /etc/environment now that we have the secure file
    sed -i '/^APEROD_BACKUP_PASSWORD=/d' /etc/environment
    ok "APEROD_BACKUP_PASSWORD перенесён из /etc/environment → ${SECRETS_FILE} (root:root 0600)"
    ok "Удалён из /etc/environment"
  else
    NEW_PASS=$(openssl rand -hex 32)
    # Write with O_CREAT | secure permissions — never expose to world-readable files
    (umask 077 && printf 'APEROD_BACKUP_PASSWORD=%s\n' "${NEW_PASS}" > "${SECRETS_FILE}")
    chown root:root "${SECRETS_FILE}"
    chmod 600 "${SECRETS_FILE}"
    ok "APEROD_BACKUP_PASSWORD сгенерирован → ${SECRETS_FILE} (root:root 0600)"
    echo ""
    echo -e "  ${YELLOW}⚠  ВАЖНО: Сохраните пароль в надёжное место — без него бэкапы нельзя расшифровать!${NC}"
    echo -e "  ${CYAN}Просмотр: sudo grep APEROD_BACKUP_PASSWORD ${SECRETS_FILE}${NC}"
    echo ""
  fi
fi

# Restart API so it picks up the new EnvironmentFile path if needed
if systemctl is-active --quiet "${API_UNIT}" 2>/dev/null; then
  systemctl restart "${API_UNIT}"
  ok "${API_UNIT} перезапущен"
fi

# ═══════════════════════════════════════════════════════════════════════════════
step "8b. Создание /etc/aperod/api.env для Telegram-уведомлений бэкапа"
# The backup service (running as root) needs TELEGRAM_BOT_TOKEN and
# ADMIN_TELEGRAM_CHAT_ID to send failure alerts directly from the bash exit trap.
# We copy these from the API service's environment (aperod-api.service) if
# they are already configured there; otherwise we leave the file empty so the
# backup script silently skips the notification.

API_ENV_FILE="${SECRETS_DIR}/api.env"

# Read Telegram credentials from /etc/environment (the standard place for
# systemd services on this server — aperod-api.service loads it via EnvironmentFile).
# We do NOT call systemctl show-environment because that only returns the systemd
# manager's DefaultEnvironment, not per-service overrides.
TG_BOT_TOKEN=$(grep -oP '(?<=^TELEGRAM_BOT_TOKEN=)\S+' /etc/environment 2>/dev/null | head -1 || true)
TG_ADMIN_CHAT=$(grep -oP '(?<=^ADMIN_TELEGRAM_CHAT_ID=)\S+' /etc/environment 2>/dev/null | head -1 || true)

# Also check the existing api.env (idempotent re-run preserves values)
if [ -f "${API_ENV_FILE}" ]; then
  [ -z "${TG_BOT_TOKEN}" ] && TG_BOT_TOKEN=$(grep -oP '(?<=^TELEGRAM_BOT_TOKEN=)\S+' "${API_ENV_FILE}" 2>/dev/null | head -1 || true)
  [ -z "${TG_ADMIN_CHAT}" ] && TG_ADMIN_CHAT=$(grep -oP '(?<=^ADMIN_TELEGRAM_CHAT_ID=)\S+' "${API_ENV_FILE}" 2>/dev/null | head -1 || true)
fi

(umask 077 && cat > "${API_ENV_FILE}" <<APIENV
# Telegram credentials for aperod-backup failure notifications.
# Created by blockchain/deploy/setup-backup.sh — do not edit manually.
# Re-run setup-backup.sh after rotating the bot token.
TELEGRAM_BOT_TOKEN=${TG_BOT_TOKEN}
ADMIN_TELEGRAM_CHAT_ID=${TG_ADMIN_CHAT}
APIENV
)
chown root:root "${API_ENV_FILE}"
chmod 600 "${API_ENV_FILE}"

if [ -n "${TG_BOT_TOKEN}" ] && [ -n "${TG_ADMIN_CHAT}" ]; then
  ok "Telegram credentials written to ${API_ENV_FILE} — backup failures will alert admin chat"
else
  warn "Telegram credentials not found — backup failure Telegram alerts will be silent."
  warn "  Set TELEGRAM_BOT_TOKEN and ADMIN_TELEGRAM_CHAT_ID, then re-run this script."
fi

# ═══════════════════════════════════════════════════════════════════════════════
step "9. Cron-задание бэкапа (каждые 12 часов)"

CRON_FILE="/etc/cron.d/aperod-backup"

# Cron triggers the systemd service, NOT the script directly.
# The service already has:  EnvironmentFile=-/etc/aperod/backup-secrets.env
# This is the only safe way — cron does not load /etc/environment.
cat > "${CRON_FILE}" <<'EOF'
# Aperod automatic backup — every 12 hours
# Triggers the systemd service which loads APEROD_BACKUP_PASSWORD
# from /etc/aperod/backup-secrets.env (root:root 0600).
# Installed by blockchain/deploy/setup-backup.sh
0 */12 * * * root systemctl start aperod-backup.service
EOF
chmod 644 "${CRON_FILE}"
ok "Cron задание создано: ${CRON_FILE}"
cat "${CRON_FILE}" | grep -v '^#' | sed 's/^/     /'

# ═══════════════════════════════════════════════════════════════════════════════
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Система бэкапа установлена и готова к работе!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo
echo -e "  ${BOLD}Следующие шаги:${NC}"
echo
echo -e "  1. Убедитесь что S3-ключи настроены:"
echo -e "     Admin Panel → Integrations → S3-хранилище"
echo
echo -e "  2. Запустите первый бэкап из Admin Panel → Integrations → «Запустить сейчас»"
echo -e "     или вручную:"
echo -e "     ${CYAN}sudo bash /usr/local/bin/aperod_backup.sh${NC}"
echo
echo -e "  3. Проверьте статус (cron + пароль + S3):"
echo -e "     ${CYAN}sudo bash blockchain/deploy/check-system.sh${NC}"
echo
