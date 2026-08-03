#!/bin/bash
# ==========================================================
# Aperod Automatic Backup, Encryption & S3 Upload
# Deploy: sudo bash blockchain/deploy/setup-backup.sh
#         (installs this script, systemd units, cron, and secrets file)
#
# Schedule: cron triggers `systemctl start aperod-backup.service` every 12 h.
#           The service loads APEROD_BACKUP_PASSWORD from /etc/aperod/backup-secrets.env
#           (root:root 0600) — never from a world-readable file or the command line.
#
# S3 credentials are read AUTOMATICALLY from:
#   ${DATA_DIR}/integration-settings.json  (set via /admin-panel/integrations)
#   No manual rclone config needed!
#
# Required env vars (loaded by aperod-backup.service EnvironmentFile):
#   APEROD_BACKUP_PASSWORD  — AES-256 encryption passphrase
#   DATA_DIR                — where integration-settings.json lives (default: /opt/aperod/data)
#
# Prometheus metrics: /var/lib/node_exporter/textfile_collector/aperod_backup.prom
# ==========================================================

set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
DATA_DIR="${DATA_DIR:-/opt/aperod/data}"
SETTINGS_FILE="${DATA_DIR}/integration-settings.json"
BACKUP_DIR="/tmp/aperod_backups_$$"
NODE_DATA_DIR="/opt/aperod/data"
DB_NAME="${APEROD_DB_NAME:-barboskin}"
DB_USER="${APEROD_DB_USER:-postgres}"
ENCRYPTION_PASSWORD="${APEROD_BACKUP_PASSWORD:?Задайте APEROD_BACKUP_PASSWORD в /etc/environment}"
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
BACKUP_NAME="aperod_backup_${TIMESTAMP}"
TEXTFILE_DIR="/var/lib/node_exporter/textfile_collector"
TEXTFILE="${TEXTFILE_DIR}/aperod_backup.prom"
START_TS=$(date +%s)

# ── Run-history log (parsed by Admin Panel /api/admin/backup/history) ─────────
HISTORY_LOG="/var/log/aperod_backup.log"
_BACKUP_FINAL_STATUS="fail"
_BACKUP_FILE_BYTES=0
_BACKUP_FILE_NAME=""

# Write one JSON line to the history log on every exit (success or failure).
# Also sends a Telegram failure alert if TELEGRAM_BOT_TOKEN + ADMIN_TELEGRAM_CHAT_ID
# are available in the environment (loaded from /etc/aperod/api.env by the service).
_write_history_log() {
  local end_ts
  end_ts=$(date +%s)
  local duration=$(( end_ts - START_TS ))
  local ts_iso
  ts_iso=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  # Escape double-quotes in filename (should never happen, but be safe)
  local safe_file="${_BACKUP_FILE_NAME//\"/\\\"}"
  local entry="{\"ts\":\"${ts_iso}\",\"status\":\"${_BACKUP_FINAL_STATUS}\",\"duration\":${duration},\"sizeBytes\":${_BACKUP_FILE_BYTES},\"file\":\"${safe_file}\"}"
  # Append to log; create file + dir if they don't exist yet
  mkdir -p "$(dirname "$HISTORY_LOG")" 2>/dev/null || true
  echo "$entry" >> "$HISTORY_LOG" 2>/dev/null || true
  # Keep only the last 100 lines so the log never grows unbounded
  if [ -f "$HISTORY_LOG" ]; then
    local tmp
    tmp=$(tail -n 100 "$HISTORY_LOG") && echo "$tmp" > "$HISTORY_LOG" || true
  fi

  # ── Telegram failure alert ───────────────────────────────────────────────────
  # Fires only on failure; success is tracked by the API server's backup monitor.
  # Requires TELEGRAM_BOT_TOKEN and ADMIN_TELEGRAM_CHAT_ID in the environment.
  if [ "${_BACKUP_FINAL_STATUS}" = "fail" ] \
     && [ -n "${TELEGRAM_BOT_TOKEN:-}" ] \
     && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
    local tg_text
    tg_text="❌ <b>Бэкап Aperod завершился с ошибкой</b>%0A%0A"
    tg_text+="<b>Время:</b> ${ts_iso}%0A"
    tg_text+="<b>Длительность:</b> ${duration} сек%0A%0A"
    tg_text+="⚠️ Данные могут быть непригодны для восстановления.%0A%0A"
    tg_text+="📋 <b>Диагностика:</b>%0A"
    tg_text+="<code>journalctl -u aperod-backup.service -n 50</code>"
    curl -s --max-time 15 -X POST \
      "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
      -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
      -d "text=${tg_text}" \
      -d "parse_mode=HTML" \
      -d "disable_web_page_preview=true" \
      >/dev/null 2>&1 || true
    echo "  Telegram failure alert sent to admin chat."
  fi
}
trap '_write_history_log; rm -rf "$BACKUP_DIR"' EXIT

# ── Helper: write prometheus textfile ─────────────────────────────────────────
write_metrics() {
  local success=$1
  local end_ts
  end_ts=$(date +%s)
  local duration=$((end_ts - START_TS))
  mkdir -p "$TEXTFILE_DIR"
  cat > "${TEXTFILE}.tmp" <<PROM
# HELP aperod_backup_last_success 1 if the last backup completed successfully.
# TYPE aperod_backup_last_success gauge
aperod_backup_last_success ${success}
# HELP aperod_backup_last_success_timestamp_seconds Unix timestamp of the last backup run.
# TYPE aperod_backup_last_success_timestamp_seconds gauge
aperod_backup_last_success_timestamp_seconds ${START_TS}
# HELP aperod_backup_duration_seconds Duration of the last backup in seconds.
# TYPE aperod_backup_duration_seconds gauge
aperod_backup_duration_seconds ${duration}
PROM
  mv "${TEXTFILE}.tmp" "$TEXTFILE"
}

# Mark as "failed/in-progress" at start — will be overwritten to 1 on success
write_metrics 0

# ── Read S3 credentials from integration-settings.json ────────────────────────
echo "=== Читаем настройки S3 из ${SETTINGS_FILE} ==="
if [ ! -f "$SETTINGS_FILE" ]; then
  echo "ОШИБКА: ${SETTINGS_FILE} не найден."
  echo "  Сохраните ключи через /admin-panel/integrations, затем убедитесь что"
  echo "  DATA_DIR=${DATA_DIR} совпадает с настройкой API-сервера."
  exit 1
fi

_py() { python3 -c "import json,sys; d=json.load(open('${SETTINGS_FILE}')); s=d.get('s3backup',{}); print($1)"; }

S3_ENDPOINT=$(_py "s.get('endpoint','')")
S3_ACCESS=$(_py "s.get('accessKeyId','')")
S3_SECRET=$(_py "s.get('secretAccessKey','')")
S3_BUCKET=$(_py "s.get('bucket','aperod-vault')")
S3_REGION=$(_py "s.get('region','us-west-004')")
S3_RETENTION_DAYS=$(_py "s.get('retentionDays',14)")

if [ -z "$S3_ENDPOINT" ] || [ -z "$S3_ACCESS" ] || [ -z "$S3_SECRET" ]; then
  echo "ОШИБКА: S3 endpoint/accessKeyId/secretAccessKey пустые в ${SETTINGS_FILE}."
  echo "  Заполните раздел «S3-хранилище» в /admin-panel/integrations и нажмите Сохранить."
  exit 1
fi

echo "  Провайдер endpoint: ${S3_ENDPOINT}"
echo "  Bucket: ${S3_BUCKET}  Region: ${S3_REGION}  Retention: ${S3_RETENTION_DAYS} дней"

# ── Configure rclone via env vars (no rclone.conf needed!) ────────────────────
# rclone reads RCLONE_CONFIG_<REMOTE>_<KEY> from environment
export RCLONE_CONFIG_S3BACKUP_TYPE="s3"
export RCLONE_CONFIG_S3BACKUP_PROVIDER="Other"
export RCLONE_CONFIG_S3BACKUP_ENDPOINT="$S3_ENDPOINT"
export RCLONE_CONFIG_S3BACKUP_ACCESS_KEY_ID="$S3_ACCESS"
export RCLONE_CONFIG_S3BACKUP_SECRET_ACCESS_KEY="$S3_SECRET"
export RCLONE_CONFIG_S3BACKUP_REGION="$S3_REGION"
export RCLONE_CONFIG_S3BACKUP_ACL="private"

RCLONE_REMOTE="s3backup:${S3_BUCKET}"
PRUNE_AGE="${S3_RETENTION_DAYS}d"

mkdir -p "$BACKUP_DIR"
echo "=== [1/4] Бэкап начат: ${TIMESTAMP} ==="

# ── 1. PostgreSQL dump ─────────────────────────────────────────────────────────
echo "  Дамп PostgreSQL: ${DB_NAME} (user=${DB_USER}) ..."
# sudo -u postgres — файл пишет root через shell redirection (не pg_dump),
# обходит и peer-auth (не нужен пароль) и permission denied в /tmp.
sudo -u "$DB_USER" pg_dump -F c -b "$DB_NAME" > "${BACKUP_DIR}/explorer_db.dump"
echo "  Дамп БД: $(du -sh "${BACKUP_DIR}/explorer_db.dump" | cut -f1)"

# ── 2. Blockchain node data ────────────────────────────────────────────────────
if [ -d "$NODE_DATA_DIR" ]; then
  echo "  Архивируем данные ноды (${NODE_DATA_DIR})..."
  # --ignore-failed-read prevents tar from dying when LevelDB compaction
  # deletes an SST file mid-archive (this is safe: the DB remains consistent).
  tar --ignore-failed-read --warning=no-file-removed \
    -czf "${BACKUP_DIR}/node_chain_data.tar.gz" -C "$NODE_DATA_DIR" . || true
  echo "  Архив ноды: $(du -sh "${BACKUP_DIR}/node_chain_data.tar.gz" | cut -f1)"
else
  echo "  ПРЕДУПРЕЖДЕНИЕ: NODE_DATA_DIR=${NODE_DATA_DIR} не найден, пропускаем."
fi

# ── 3. Pack + encrypt (AES-256 via GnuPG) ─────────────────────────────────────
echo "=== [2/4] Шифрование AES-256 ==="
cd "$BACKUP_DIR"
tar -cf "${BACKUP_NAME}.tar" *.dump *.tar.gz 2>/dev/null || tar -cf "${BACKUP_NAME}.tar" *.dump

echo "$ENCRYPTION_PASSWORD" | gpg --batch --yes --passphrase-fd 0 \
  --symmetric --cipher-algo AES256 \
  -o "${BACKUP_NAME}.tar.gpg" "${BACKUP_NAME}.tar"
echo "  Зашифровано: $(du -sh "${BACKUP_NAME}.tar.gpg" | cut -f1)"

# ── 4. Upload to S3 via rclone ─────────────────────────────────────────────────
echo "=== [3/4] Загрузка в ${RCLONE_REMOTE} ==="
rclone copy "${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg" "$RCLONE_REMOTE" --s3-no-check-bucket
echo "  Загружено: ${BACKUP_NAME}.tar.gpg"

# Prune old backups
echo "  Удаляем резервные копии старше ${S3_RETENTION_DAYS} дней..."
rclone delete --min-age "$PRUNE_AGE" "$RCLONE_REMOTE" 2>/dev/null || true

# ── 5. Cleanup ─────────────────────────────────────────────────────────────────
echo "=== [4/4] Очистка временных файлов ==="
rm -rf "$BACKUP_DIR"

# ── Capture final file size (before cleanup removed the local copy) ───────────
# The file was already uploaded; get its size from the remote listing (quick).
_BACKUP_FILE_NAME="${BACKUP_NAME}.tar.gpg"
_BACKUP_FILE_BYTES=$(rclone size "${RCLONE_REMOTE}/${BACKUP_NAME}.tar.gpg" --s3-no-check-bucket --json 2>/dev/null \
  | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('bytes',0))" 2>/dev/null || echo 0)

# ── Write success metrics ──────────────────────────────────────────────────────
_BACKUP_FINAL_STATUS="ok"
write_metrics 1
echo "=== БЭКАП ЗАВЕРШЁН: ${BACKUP_NAME}.tar.gpg → ${RCLONE_REMOTE} ==="
