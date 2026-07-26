#!/bin/bash
# ==========================================================
# Aperod Automatic Backup, Encryption & S3 Upload
# Deploy: sudo cp blockchain/deploy/aperod_backup.sh /usr/local/bin/aperod_backup.sh
#         sudo chmod 700 /usr/local/bin/aperod_backup.sh
# Cron:   0 */12 * * * /usr/local/bin/aperod_backup.sh >> /var/log/aperod_backup.log 2>&1
#
# Required env vars (set in /etc/environment or systemd override):
#   APEROD_BACKUP_PASSWORD  — AES-256 encryption passphrase
#   RCLONE_REMOTE           — rclone remote:bucket (default: s3-backup:aperod-vault)
# ==========================================================

set -euo pipefail

# ── Config ───────────────────────────────────────────────────────────────────
BACKUP_DIR="/tmp/aperod_backups_$$"
NODE_DATA_DIR="/opt/aperod/data"
DB_NAME="${APEROD_DB_NAME:-barboskin}"
DB_USER="${APEROD_DB_USER:-postgres}"
ENCRYPTION_PASSWORD="${APEROD_BACKUP_PASSWORD:?Set APEROD_BACKUP_PASSWORD env var}"
RCLONE_REMOTE="${RCLONE_REMOTE:-s3-backup:aperod-vault}"
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
BACKUP_NAME="aperod_backup_${TIMESTAMP}"

mkdir -p "$BACKUP_DIR"
echo "=== [1/4] Backup started: ${TIMESTAMP} ==="

# ── 1. PostgreSQL dump (Explorer indexes & metadata) ─────────────────────────
echo "  Dumping PostgreSQL: ${DB_NAME} ..."
pg_dump -U "$DB_USER" -F c -b -f "${BACKUP_DIR}/explorer_db.dump" "$DB_NAME"
echo "  DB dump: $(du -sh "${BACKUP_DIR}/explorer_db.dump" | cut -f1)"

# ── 2. Blockchain node state (LevelDB/RocksDB) ───────────────────────────────
if [ -d "$NODE_DATA_DIR" ]; then
  echo "  Archiving node chain data (${NODE_DATA_DIR})..."
  tar -czf "${BACKUP_DIR}/node_chain_data.tar.gz" -C "$NODE_DATA_DIR" .
  echo "  Chain archive: $(du -sh "${BACKUP_DIR}/node_chain_data.tar.gz" | cut -f1)"
else
  echo "  WARNING: NODE_DATA_DIR=${NODE_DATA_DIR} not found, skipping."
fi

# ── 3. Pack + encrypt (AES-256 via GnuPG) ────────────────────────────────────
echo "=== [2/4] Encrypting archive (AES-256) ==="
cd "$BACKUP_DIR"
tar -cf "${BACKUP_NAME}.tar" *.dump *.tar.gz 2>/dev/null || tar -cf "${BACKUP_NAME}.tar" *.dump

echo "$ENCRYPTION_PASSWORD" | gpg --batch --yes --passphrase-fd 0 \
  --symmetric --cipher-algo AES256 \
  -o "${BACKUP_NAME}.tar.gpg" "${BACKUP_NAME}.tar"
echo "  Encrypted: $(du -sh "${BACKUP_NAME}.tar.gpg" | cut -f1)"

# ── 4. Upload to S3-compatible storage ───────────────────────────────────────
echo "=== [3/4] Uploading to ${RCLONE_REMOTE} ==="
rclone copy "${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg" "$RCLONE_REMOTE"
echo "  Uploaded: ${BACKUP_NAME}.tar.gpg"

# Prune backups older than 14 days
echo "  Pruning backups older than 14 days..."
rclone delete --min-age 336h "$RCLONE_REMOTE" 2>/dev/null || true

# ── 5. Cleanup temp files ────────────────────────────────────────────────────
echo "=== [4/4] Cleanup ==="
rm -rf "$BACKUP_DIR"

echo "=== BACKUP COMPLETE: ${BACKUP_NAME}.tar.gpg → ${RCLONE_REMOTE} ==="
