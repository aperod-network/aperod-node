#!/bin/bash
# ==========================================================
# Aperod Automatic Backup, Encryption & S3 Upload
# Deploy: sudo cp blockchain/deploy/aperod_backup.sh /usr/local/bin/aperod_backup.sh
#         sudo chmod 700 /usr/local/bin/aperod_backup.sh
# Cron:   0 */12 * * * root /usr/local/bin/aperod_backup.sh >> /var/log/aperod_backup.log 2>&1
#
# S3 credentials are read AUTOMATICALLY from:
#   ${DATA_DIR}/integration-settings.json  (set via /admin-panel/integrations)
#   No manual rclone config needed!
#
# Optional env vars (override integration-settings.json):
#   APEROD_BACKUP_PASSWORD  — AES-256 encryption passphrase (required)
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
echo "  Дамп PostgreSQL: ${DB_NAME} ..."
pg_dump -U "$DB_USER" -F c -b -f "${BACKUP_DIR}/explorer_db.dump" "$DB_NAME"
echo "  Дамп БД: $(du -sh "${BACKUP_DIR}/explorer_db.dump" | cut -f1)"

# ── 2. Blockchain node data ────────────────────────────────────────────────────
if [ -d "$NODE_DATA_DIR" ]; then
  echo "  Архивируем данные ноды (${NODE_DATA_DIR})..."
  tar -czf "${BACKUP_DIR}/node_chain_data.tar.gz" -C "$NODE_DATA_DIR" .
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

# ── Write success metrics ──────────────────────────────────────────────────────
write_metrics 1
echo "=== БЭКАП ЗАВЕРШЁН: ${BACKUP_NAME}.tar.gpg → ${RCLONE_REMOTE} ==="
