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

# Systemd runs services with LANG=C by default; without UTF-8 bash cannot
# parse multi-byte characters (emoji, Cyrillic) in string literals.
export LC_ALL=en_US.UTF-8
export LANG=en_US.UTF-8

# ── Config ────────────────────────────────────────────────────────────────────
DATA_DIR="${DATA_DIR:-/opt/aperod/data}"
SETTINGS_FILE="${DATA_DIR}/integration-settings.json"
BACKUP_DIR="${APEROD_BACKUP_DIR_OVERRIDE:-/opt/aperod/backup-tmp/aperod_backups_$$}"
NODE_DATA_DIR="${APEROD_NODE_DATA_DIR_OVERRIDE:-/opt/aperod/data}"
DB_NAME="${APEROD_DB_NAME:-barboskin}"
DB_USER="${APEROD_DB_USER:-postgres}"
ENCRYPTION_PASSWORD="${APEROD_BACKUP_PASSWORD:?Задайте APEROD_BACKUP_PASSWORD в /etc/environment}"
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
BACKUP_NAME="aperod_backup_${TIMESTAMP}"
TEXTFILE_DIR="${APEROD_TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
TEXTFILE="${TEXTFILE_DIR}/aperod_backup.prom"
START_TS=$(date +%s)

# ── Run-history log (parsed by Admin Panel /api/admin/backup/history) ─────────
HISTORY_LOG="${APEROD_HISTORY_LOG:-/var/log/aperod_backup.log}"
_BACKUP_FINAL_STATUS="fail"
_BACKUP_FILE_BYTES=0
_BACKUP_FILE_NAME=""
_BACKUP_SKIP_REASON=""
_BACKUP_DISK_PATH=""
_BACKUP_DISK_FREE_GB=""

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
  local skip_fields=""
  if [ "${_BACKUP_FINAL_STATUS}" = "skipped" ] && [ -n "${_BACKUP_SKIP_REASON}" ]; then
    local safe_disk_path="${_BACKUP_DISK_PATH//\"/\\\"}"
    skip_fields=",\"skipReason\":\"${_BACKUP_SKIP_REASON}\",\"diskPath\":\"${safe_disk_path}\",\"diskFreeGB\":\"${_BACKUP_DISK_FREE_GB}\""
  fi
  local entry="{\"ts\":\"${ts_iso}\",\"status\":\"${_BACKUP_FINAL_STATUS}\",\"duration\":${duration},\"sizeBytes\":${_BACKUP_FILE_BYTES},\"file\":\"${safe_file}\"${skip_fields}}"
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
    tg_text="[FAIL] <b>Бэкап Aperod завершился с ошибкой</b>%0A%0A"
    tg_text+="<b>Время:</b> ${ts_iso}%0A"
    tg_text+="<b>Длительность:</b> ${duration} сек%0A%0A"
    tg_text+="[!] Данные могут быть непригодны для восстановления.%0A%0A"
    tg_text+="[i] <b>Диагностика:</b>%0A"
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
  local skipped=${2:-0}
  local end_ts
  end_ts=$(date +%s)
  local duration=$((end_ts - START_TS))
  mkdir -p "$TEXTFILE_DIR"
  cat > "${TEXTFILE}.tmp" <<PROM
# HELP aperod_backup_last_success 1 if the last backup completed successfully.
# TYPE aperod_backup_last_success gauge
aperod_backup_last_success ${success}
# HELP aperod_backup_skipped_low_disk 1 if the most recent backup attempt was skipped due to low disk space.
# TYPE aperod_backup_skipped_low_disk gauge
aperod_backup_skipped_low_disk ${skipped}
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

# ── Clean up stale backup-tmp dirs from previous crashed runs ──────────────────
# If the server was hard-killed (OOM, power loss), the EXIT trap never ran and
# the temp dir remains, potentially consuming hundreds of MBs or more — which
# can cause the disk-space preflight to skip the next backup (false low-disk).
# Glob all aperod_backups_* dirs in the parent directory; remove any that are
# older than 6 hours (safely past any legitimate running backup).
_cleanup_stale_backup_dirs() {
  local base_dir
  base_dir=$(dirname "$BACKUP_DIR")
  [ -d "$base_dir" ] || return 0

  local stale_dirs=()
  while IFS= read -r -d '' dir; do
    stale_dirs+=("$dir")
  done < <(find "$base_dir" -maxdepth 1 -type d -name 'aperod_backups_*' -mmin +360 -print0 2>/dev/null)

  [ "${#stale_dirs[@]}" -eq 0 ] && return 0

  echo "=== ПРЕДУПРЕЖДЕНИЕ: обнаружены устаревшие временные директории бэкапа ==="
  echo "  Вероятно, предыдущий запуск завершился аварийно (OOM, аварийное отключение)."

  local total_kb=0
  for dir in "${stale_dirs[@]}"; do
    local sz_kb
    sz_kb=$(du -sk "$dir" 2>/dev/null | awk '{print $1}')
    [ -z "$sz_kb" ] && sz_kb=0
    local sz_human
    sz_human=$(du -sh "$dir" 2>/dev/null | awk '{print $1}')
    echo "  Удаляем устаревшую директорию: ${dir}  (${sz_human})"
    rm -rf "$dir"
    total_kb=$(( total_kb + sz_kb ))
  done

  local total_gb
  total_gb=$(awk "BEGIN{printf \"%.2f\", ${total_kb}/1024/1024}")
  echo "  Итого освобождено: ${total_gb} ГБ из ${#stale_dirs[@]} директорий."

  # ── Telegram alert if stale dirs exceeded 1 GB ───────────────────────────────
  local threshold_kb=$(( 1 * 1024 * 1024 ))  # 1 GiB in kibibytes
  if [ "$total_kb" -gt "$threshold_kb" ] \
     && [ -n "${TELEGRAM_BOT_TOKEN:-}" ] \
     && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
    local ts_iso
    ts_iso=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    local tg_text
    tg_text="⚠️ <b>Aperod Backup: устаревшие временные файлы удалены</b>%0A%0A"
    tg_text+="<b>Директорий удалено:</b> ${#stale_dirs[@]}%0A"
    tg_text+="<b>Освобождено:</b> ${total_gb} ГБ%0A"
    tg_text+="<b>Время:</b> ${ts_iso}%0A%0A"
    tg_text+="[!] Предыдущий запуск скрипта бэкапа завершился аварийно (OOM или%0A"
    tg_text+="принудительное завершение процесса) — временные файлы остались на диске.%0A%0A"
    tg_text+="[i] <b>Диагностика:</b>%0A"
    tg_text+="<code>journalctl -u aperod-backup.service -n 50</code>"
    curl -s --max-time 15 -X POST \
      "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
      -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
      -d "text=${tg_text}" \
      -d "parse_mode=HTML" \
      -d "disable_web_page_preview=true" \
      >/dev/null 2>&1 || true
    echo "  Telegram stale-dir alert sent to admin chat."
  fi
}
_cleanup_stale_backup_dirs

# ── Guard: BACKUP_DIR must be creatable ───────────────────────────────────────
# With set -euo pipefail a failed mkdir would fire the EXIT trap with status
# "fail" — giving Prometheus a spurious failure metric and Telegram a noisy alert
# with no actionable context.  Catch it explicitly so admins see a clear "skipped"
# with reason backup_dir_unavailable instead.
if ! mkdir -p "$BACKUP_DIR" 2>/dev/null; then
  echo "ОШИБКА: не удалось создать директорию для бэкапа: ${BACKUP_DIR}"
  echo "  Возможные причины: неверные права доступа к $(dirname "$BACKUP_DIR")"
  echo "  или отсутствует родительская директория."
  echo "  Бэкап пропущен (причина: backup_dir_unavailable)."
  _BACKUP_FINAL_STATUS="skipped"
  _BACKUP_SKIP_REASON="backup_dir_unavailable"
  _BACKUP_DISK_PATH="${BACKUP_DIR}"
  _BACKUP_DISK_FREE_GB="N/A"
  write_metrics 0 1
  exit 0
fi

# ── Pre-flight: disk space check (5 GB minimum on each relevant filesystem) ──
# If either BACKUP_DIR or NODE_DATA_DIR has less than 5 GB free, send a
# Telegram alert and exit 0 ("skipped") so Prometheus records skipped_low_disk=1
# rather than a failure, giving admins time to free space before data is lost.
MIN_AVAIL_KB=$(( 5 * 1024 * 1024 ))   # 5 GiB in kibibytes

_disk_preflight() {
  local path="$1"
  local label="$2"
  local avail_kb
  avail_kb=$(df --output=avail "$path" 2>/dev/null | awk 'NR==2{print $1}')
  [ -z "$avail_kb" ] && avail_kb=0
  if [ "$avail_kb" -lt "$MIN_AVAIL_KB" ]; then
    local free_gb
    free_gb=$(awk "BEGIN{printf \"%.1f\", ${avail_kb}/1024/1024}")
    echo "=== ВНИМАНИЕ: мало свободного места на диске ==="
    echo "  Раздел : ${label}"
    echo "  Свободно: ${free_gb} ГБ — требуется минимум 5 ГБ"
    echo "  Бэкап пропущен."
    # ── Telegram low-disk alert ──────────────────────────────────────────────
    if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
      local ts_iso
      ts_iso=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
      local tg_text
      tg_text="⚠️ <b>Бэкап Aperod пропущен — мало места на диске</b>%0A%0A"
      tg_text+="<b>Раздел:</b> ${label}%0A"
      tg_text+="<b>Свободно:</b> ${free_gb} ГБ%0A"
      tg_text+="<b>Требуется:</b> минимум 5 ГБ%0A"
      tg_text+="<b>Время:</b> ${ts_iso}%0A%0A"
      tg_text+="💡 Освободите место — бэкап возобновится на следующем расписании."
      curl -s --max-time 15 -X POST \
        "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
        -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
        -d "text=${tg_text}" \
        -d "parse_mode=HTML" \
        -d "disable_web_page_preview=true" \
        >/dev/null 2>&1 || true
      echo "  Telegram low-disk alert sent to admin chat."
    fi
    # Record skipped status (EXIT trap will call _write_history_log)
    _BACKUP_FINAL_STATUS="skipped"
    _BACKUP_SKIP_REASON="low_disk"
    _BACKUP_DISK_PATH="${label}"
    _BACKUP_DISK_FREE_GB="${free_gb}"
    write_metrics 0 1
    exit 0
  fi
}

_disk_preflight "$BACKUP_DIR" "BACKUP_DIR (/opt/aperod/backup-tmp)"
[ -d "$NODE_DATA_DIR" ] && _disk_preflight "$NODE_DATA_DIR" "NODE_DATA_DIR (${NODE_DATA_DIR})"

# ── Scaled preflight: BACKUP_DIR must fit one full encrypted copy of node data ──
# With streaming (tar|gpg), only the GPG output file exists on disk at a time,
# so the required headroom equals the raw node data size (pre-compression worst
# case) plus 1 GiB of buffer for the DB dump and filesystem overhead.
_disk_preflight_scaled() {
  [ -d "$NODE_DATA_DIR" ] || return 0
  local data_kb
  data_kb=$(du -sk "$NODE_DATA_DIR" 2>/dev/null | awk '{print $1}')
  [ -z "$data_kb" ] && data_kb=0
  local buffer_kb=$(( 1 * 1024 * 1024 ))   # 1 GiB buffer
  local required_kb=$(( data_kb + buffer_kb ))
  local avail_kb
  avail_kb=$(df --output=avail "$BACKUP_DIR" 2>/dev/null | awk 'NR==2{print $1}')
  [ -z "$avail_kb" ] && avail_kb=0
  if [ "$avail_kb" -lt "$required_kb" ]; then
    local free_gb need_gb
    free_gb=$(awk "BEGIN{printf \"%.1f\", ${avail_kb}/1024/1024}")
    need_gb=$(awk "BEGIN{printf \"%.1f\", ${required_kb}/1024/1024}")
    echo "=== ВНИМАНИЕ: мало свободного места на диске ==="
    echo "  Раздел : BACKUP_DIR (/opt/aperod/backup-tmp)"
    echo "  Свободно: ${free_gb} ГБ — требуется ~${need_gb} ГБ (размер ноды + 1 ГБ буфер)"
    echo "  Бэкап пропущен."
    if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
      local ts_iso
      ts_iso=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
      local tg_text
      tg_text="⚠️ <b>Бэкап Aperod пропущен — мало места на диске</b>%0A%0A"
      tg_text+="<b>Раздел:</b> BACKUP_DIR (/opt/aperod/backup-tmp)%0A"
      tg_text+="<b>Свободно:</b> ${free_gb} ГБ%0A"
      tg_text+="<b>Требуется:</b> ~${need_gb} ГБ (размер данных ноды + 1 ГБ)%0A"
      tg_text+="<b>Время:</b> ${ts_iso}%0A%0A"
      tg_text+="💡 Освободите место — бэкап возобновится на следующем расписании."
      curl -s --max-time 15 -X POST \
        "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
        -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
        -d "text=${tg_text}" \
        -d "parse_mode=HTML" \
        -d "disable_web_page_preview=true" \
        >/dev/null 2>&1 || true
      echo "  Telegram low-disk alert sent to admin chat."
    fi
    _BACKUP_FINAL_STATUS="skipped"
    _BACKUP_SKIP_REASON="low_disk"
    _BACKUP_DISK_PATH="BACKUP_DIR (/opt/aperod/backup-tmp)"
    _BACKUP_DISK_FREE_GB="${free_gb}"
    write_metrics 0 1
    exit 0
  fi
  local data_gb
  data_gb=$(awk "BEGIN{printf \"%.1f\", ${data_kb}/1024/1024}")
  local need_gb
  need_gb=$(awk "BEGIN{printf \"%.1f\", ${required_kb}/1024/1024}")
  echo "  Данные ноды: ${data_gb} ГБ → требуется ~${need_gb} ГБ на BACKUP_DIR — OK"
}
_disk_preflight_scaled

echo "=== [1/4] Бэкап начат: ${TIMESTAMP} ==="

# ── 1. PostgreSQL dump ─────────────────────────────────────────────────────────
echo "  Дамп PostgreSQL: ${DB_NAME} (user=${DB_USER}) ..."
# sudo -u postgres — файл пишет root через shell redirection (не pg_dump),
# обходит и peer-auth (не нужен пароль) и permission denied в /tmp.
sudo -u "$DB_USER" pg_dump -F c -b "$DB_NAME" > "${BACKUP_DIR}/explorer_db.dump"
echo "  Дамп БД: $(du -sh "${BACKUP_DIR}/explorer_db.dump" | cut -f1)"

# ── 2+3. Stream all sources directly into GPG (no intermediate tar/tar.gz) ──────
# Previous approach: node_chain_data.tar.gz  +  backup.tar  +  backup.tar.gpg
#   existed simultaneously → up to 3× chain size of temp disk usage.
# New approach: tar stdout piped to gpg stdin → only one output file ever written.
#   Peak temp usage = GPG archive (~= compressed chain size) + small DB dump.
#
# --ignore-failed-read prevents tar from dying when LevelDB compaction removes
# an SST file mid-archive (safe: the DB remains consistent).
echo "=== [2/4] Архивирование + шифрование AES-256 (потоковый режим) ==="
TAR_ARGS=( --ignore-failed-read --warning=no-file-removed -czf -
           -C "$BACKUP_DIR" explorer_db.dump )
if [ -d "$NODE_DATA_DIR" ]; then
  echo "  Включаем данные ноды (${NODE_DATA_DIR}) в поток архива..."
  TAR_ARGS+=( -C "$NODE_DATA_DIR" . )
else
  echo "  ПРЕДУПРЕЖДЕНИЕ: NODE_DATA_DIR=${NODE_DATA_DIR} не найден, пропускаем."
fi
# Decrypt+extract: gpg -d <file>.tar.gpg | tar -xzf -
tar "${TAR_ARGS[@]}" \
  | gpg --batch --yes --passphrase-fd 3 \
      --symmetric --cipher-algo AES256 \
      -o "${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg" - \
    3< <(echo "$ENCRYPTION_PASSWORD")
echo "  Зашифровано: $(du -sh "${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg" | cut -f1)"

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
