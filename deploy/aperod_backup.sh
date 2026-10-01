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
if [ -n "${APEROD_BACKUP_DIR_OVERRIDE:-}" ]; then
  BACKUP_ROOT_REQUESTED="${APEROD_BACKUP_DIR_OVERRIDE}"
else
  BACKUP_ROOT_REQUESTED="/opt/aperod/backup-tmp"
fi
BACKUP_ROOT=""
BACKUP_DIR=""
BACKUP_DIR_CANONICAL=""
BACKUP_DIR_OWNED=0
NODE_DATA_DIR="${APEROD_NODE_DATA_DIR_OVERRIDE:-/opt/aperod/data}"
CHAIN_DATA_DIR="${APEROD_CHAIN_DATA_DIR_OVERRIDE:-${NODE_DATA_DIR}/testnet}"
BACKUP_SOCKET_NAME=".aperod-backup.sock"
DB_NAME="${APEROD_DB_NAME:-barboskin}"
DB_USER="${APEROD_DB_USER:-postgres}"
ENCRYPTION_PASSWORD="${APEROD_BACKUP_PASSWORD:?Задайте APEROD_BACKUP_PASSWORD в /etc/environment}"
TIMESTAMP=$(date +"%Y-%m-%d_%H-%M-%S")
# Keep exactly one current backup object in remote storage.  A stable name makes
# each successful upload replace the previous backup instead of accumulating
# timestamped archives indefinitely.
BACKUP_NAME="aperod_backup"
PREVIOUS_OBJECT="${BACKUP_NAME}_previous.tar.gpg"
TEXTFILE_DIR="${APEROD_TEXTFILE_DIR:-/var/lib/node_exporter/textfile_collector}"
TEXTFILE="${TEXTFILE_DIR}/aperod_backup.prom"
START_TS=$(date +%s)

# ── Run-history log (parsed by Admin Panel /api/admin/backup/history) ─────────
HISTORY_LOG="${APEROD_HISTORY_LOG:-/var/log/aperod_backup.log}"
_BACKUP_FINAL_STATUS="fail"
_BACKUP_DEGRADED=0
_BACKUP_FILE_BYTES=0
_BACKUP_FILE_NAME=""
_BACKUP_SKIP_REASON=""
_BACKUP_DISK_PATH=""
_BACKUP_DISK_FREE_GB=""
SNAPSHOT_DIR=""
STAGING_ROOT=""
SNAPSHOT_VALIDATED=0
AUTH_HELPER="${APEROD_ROLLOUT_CLEANUP_BIN:-/usr/local/bin/aperod-rollout-cleanup}"
AUTH_HELPER_READY=0
AUTH_ENABLED=0
AUTH_ATTEMPT_REGISTERED=0
AUTH_BEGIN_INVOKED=0
AUTH_CONTEXT_FILE=""
AUTH_ANCHORS_FILE=""
AUTH_PROOF_FILE=""
AUTH_PINNED=0

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
  local degraded_field=""
  if [ "${_BACKUP_FINAL_STATUS}" = "skipped" ] && [ -n "${_BACKUP_SKIP_REASON}" ]; then
    local safe_disk_path="${_BACKUP_DISK_PATH//\"/\\\"}"
    skip_fields=",\"skipReason\":\"${_BACKUP_SKIP_REASON}\",\"diskPath\":\"${safe_disk_path}\",\"diskFreeGB\":\"${_BACKUP_DISK_FREE_GB}\""
  fi
  if [ "${_BACKUP_DEGRADED}" -eq 1 ]; then
    degraded_field=",\"degraded\":true"
  fi
  local entry="{\"ts\":\"${ts_iso}\",\"status\":\"${_BACKUP_FINAL_STATUS}\",\"duration\":${duration},\"sizeBytes\":${_BACKUP_FILE_BYTES},\"file\":\"${safe_file}\"${skip_fields}${degraded_field}}"
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
_cleanup_validated_snapshot() {
  [ "$SNAPSHOT_VALIDATED" -eq 1 ] || return 0
  [ -n "$STAGING_ROOT" ] && [ -n "$SNAPSHOT_DIR" ] || return 1
  [ ! -L "$STAGING_ROOT" ] || {
    echo "ОШИБКА очистки: staging root стал символической ссылкой; оставляем без изменений." >&2
    return 1
  }
  [ ! -L "$SNAPSHOT_DIR" ] || {
    echo "ОШИБКА очистки: каталог checkpoint стал символической ссылкой; оставляем без изменений." >&2
    return 1
  }
  local canonical_root canonical_stage
  canonical_root=$(realpath -e -- "$STAGING_ROOT") || return 1
  canonical_stage=$(realpath -e -- "$SNAPSHOT_DIR") || return 1
  case "$canonical_stage" in
    "$canonical_root"/*) ;;
    *)
      echo "ОШИБКА очистки: checkpoint больше не находится в staging root; оставляем без изменений." >&2
      return 1
      ;;
  esac
  [ "$canonical_stage" != "$canonical_root" ] || return 1
  [ -d "$canonical_stage" ] && [ ! -L "$canonical_stage" ] || return 1
  rm -rf -- "$canonical_stage"
}

_cleanup_owned_workdir() {
  [ "$BACKUP_DIR_OWNED" -eq 1 ] || return 0
  [ -n "$BACKUP_ROOT" ] && [ -n "$BACKUP_DIR" ] && [ -n "$BACKUP_DIR_CANONICAL" ] || return 1
  [ ! -L "$BACKUP_DIR" ] || {
    echo "ОШИБКА очистки: рабочая директория стала символической ссылкой; оставляем без изменений." >&2
    return 1
  }
  local canonical_root canonical_work
  canonical_root=$(realpath -e -- "$BACKUP_ROOT") || return 1
  canonical_work=$(realpath -e -- "$BACKUP_DIR") || return 1
  [ "$canonical_root" = "$BACKUP_ROOT" ] \
    && [ "$canonical_work" = "$BACKUP_DIR_CANONICAL" ] \
    && [ "$canonical_work" = "$BACKUP_DIR" ] \
    && [ "$(dirname -- "$canonical_work")" = "$canonical_root" ] \
    && [[ "$(basename -- "$canonical_work")" =~ ^aperod_backups_[A-Za-z0-9]+$ ]] \
    && [ -d "$canonical_work" ] || {
      echo "ОШИБКА очистки: рабочая директория больше не совпадает с созданным каталогом; оставляем без изменений." >&2
      return 1
    }
  rm -rf -- "$canonical_work"
  BACKUP_DIR_OWNED=0
}

_disable_backup_authorization() {
  local reason="${1:-backup-authorization-failed}"
  AUTH_ENABLED=0
  AUTH_PINNED=0
  if [ "$AUTH_HELPER_READY" -eq 1 ]; then
    if _auth_output_is_safe "$AUTH_CONTEXT_FILE"; then
      if ! "$AUTH_HELPER" backup-abort --context "$AUTH_CONTEXT_FILE"; then
        echo "ОШИБКА: authorization abort failed (${reason}); prior authorization may remain invalidated only by backup-begin." >&2
      else
        echo "  Rollout-cleanup authorization disabled: ${reason}."
      fi
    elif [ "$AUTH_BEGIN_INVOKED" -eq 1 ]; then
      echo "  Rollout-cleanup authorization disabled: ${reason}; backup-begin invalidated prior attempts before source capture."
    else
      echo "ОШИБКА: authorization disabled (${reason}), but no registered backup context exists for backup-abort." >&2
    fi
  fi
}

_abort_backup_authorization() {
  [ "$AUTH_ATTEMPT_REGISTERED" -eq 1 ] || return 0
  AUTH_ATTEMPT_REGISTERED=0
  if [ "$AUTH_HELPER_READY" -eq 1 ] && [ -n "$AUTH_CONTEXT_FILE" ]; then
    "$AUTH_HELPER" backup-abort --context "$AUTH_CONTEXT_FILE" \
      || echo "ОШИБКА: rollout-cleanup backup-abort failed; authorization must remain disabled." >&2
  fi
}

_on_exit() {
  local original_status=$?
  trap - EXIT
  _abort_backup_authorization || true
  _write_history_log || true
  _cleanup_validated_snapshot || true
  _cleanup_owned_workdir || true
  exit "$original_status"
}
trap _on_exit EXIT

# ── Helper: write prometheus textfile ─────────────────────────────────────────
write_metrics() {
  local success=$1
  local skipped=${2:-0}
  local degraded=${3:-0}
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
# HELP aperod_backup_legacy_migration 1 if the latest successful backup migrated a legacy generation with incomplete historical verification.
# TYPE aperod_backup_legacy_migration gauge
aperod_backup_legacy_migration ${degraded}
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

if [ -z "$S3_ENDPOINT" ] || [ -z "$S3_ACCESS" ] || [ -z "$S3_SECRET" ]; then
  echo "ОШИБКА: S3 endpoint/accessKeyId/secretAccessKey пустые в ${SETTINGS_FILE}."
  echo "  Заполните раздел «S3-хранилище» в /admin-panel/integrations и нажмите Сохранить."
  exit 1
fi

echo "  Провайдер endpoint: ${S3_ENDPOINT}"
echo "  Bucket: ${S3_BUCKET}  Region: ${S3_REGION}"

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

# Backblaze B2 keeps every overwrite as a full historical file version.  The
# S3-compatible `rclone cleanup` command only removes stale multipart uploads,
# so old backup versions must be removed through the native B2 API by fileId.
# This function keeps the two newest upload versions (current + previous) and
# removes older uploads or hide markers only after the replacement was verified.
_cleanup_b2_object_versions() {
  local object_name="$1"
  local keep_count="${2:-2}"

  case "$S3_ENDPOINT" in
    *backblazeb2.com*) ;;
    *)
      echo "  Version cleanup skipped: provider is not Backblaze B2."
      return 0
      ;;
  esac

  B2_CLEANUP_ACCESS="$S3_ACCESS" \
  B2_CLEANUP_SECRET="$S3_SECRET" \
  B2_CLEANUP_BUCKET="$S3_BUCKET" \
  B2_CLEANUP_OBJECT="$object_name" \
  B2_CLEANUP_KEEP_COUNT="$keep_count" \
  python3 - <<'PY'
import base64
import json
import os
import urllib.request

access = os.environ["B2_CLEANUP_ACCESS"]
secret = os.environ["B2_CLEANUP_SECRET"]
bucket_name = os.environ["B2_CLEANUP_BUCKET"]
object_name = os.environ["B2_CLEANUP_OBJECT"]
keep_count = int(os.environ["B2_CLEANUP_KEEP_COUNT"])

credentials = base64.b64encode(f"{access}:{secret}".encode()).decode()
request = urllib.request.Request(
    "https://api.backblazeb2.com/b2api/v2/b2_authorize_account",
    headers={"Authorization": f"Basic {credentials}"},
)
with urllib.request.urlopen(request, timeout=30) as response:
    auth = json.load(response)

api_url = auth.get("apiUrl")
token = auth.get("authorizationToken")
account_id = auth.get("accountId")
allowed = auth.get("allowed") or {}
bucket_id = allowed.get("bucketId")
if not api_url or not token or not account_id:
    raise RuntimeError("Backblaze authorization response is incomplete")

def post(path, payload):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        api_url + path,
        data=data,
        headers={
            "Authorization": token,
            "Content-Type": "application/json",
        },
    )
    with urllib.request.urlopen(req, timeout=30) as response:
        return json.load(response)

if not bucket_id:
    buckets = post(
        "/b2api/v2/b2_list_buckets",
        {"accountId": account_id, "bucketName": bucket_name},
    ).get("buckets", [])
    exact_buckets = [bucket for bucket in buckets if bucket.get("bucketName") == bucket_name]
    if len(exact_buckets) != 1:
        raise RuntimeError(f"expected one Backblaze bucket named {bucket_name!r}")
    bucket_id = exact_buckets[0]["bucketId"]

def list_exact_versions():
    exact = []
    next_name = None
    next_id = None
    while True:
        payload = {
            "bucketId": bucket_id,
            "prefix": object_name,
            "maxFileCount": 1000,
        }
        if next_name:
            payload["startFileName"] = next_name
        if next_id:
            payload["startFileId"] = next_id
        page = post("/b2api/v2/b2_list_file_versions", payload)
        exact.extend(
            item for item in page.get("files", [])
            if item.get("fileName") == object_name
        )
        next_name = page.get("nextFileName")
        next_id = page.get("nextFileId")
        if not next_name:
            return exact

versions = list_exact_versions()
uploads = sorted(
    (item for item in versions if item.get("action") == "upload"),
    key=lambda item: int(item.get("uploadTimestamp", 0)),
    reverse=True,
)
if not uploads and keep_count:
    raise RuntimeError(f"no current upload version found for {object_name}")

keep = uploads[:keep_count]
keep_ids = {item.get("fileId") for item in keep}
victims = [item for item in versions if item.get("fileId") not in keep_ids]
reclaimed_bytes = 0
for item in victims:
    post(
        "/b2api/v2/b2_delete_file_version",
        {"fileName": object_name, "fileId": item["fileId"]},
    )
    if item.get("action") == "upload":
        reclaimed_bytes += int(item.get("contentLength", 0))

remaining = list_exact_versions()
remaining_ids = {item.get("fileId") for item in remaining}
if remaining_ids != keep_ids or len(remaining) != len(keep):
    raise RuntimeError(
        f"version cleanup verification failed: expected {len(keep)} versions, found {len(remaining)}"
    )

retention = "all versions removed" if keep_count == 0 else f"newest {keep_count} upload version(s) retained"
print(
    f"  Backblaze versions removed: {len(victims)}; "
    f"reclaimed: {reclaimed_bytes / (1024 ** 3):.2f} GiB; {retention}."
)
PY
}

# ── Guard: create a private, uniquely owned work directory ────────────────────
# With set -euo pipefail a failed mkdir would fire the EXIT trap with status
# "fail" — giving Prometheus a spurious failure metric and Telegram a noisy alert
# with no actionable context.  Catch it explicitly so admins see a clear "skipped"
# with reason backup_dir_unavailable instead. Never reuse or remove the caller's
# override path; it only selects the parent under which mktemp creates a new dir.
BACKUP_DIR_SETUP_ERROR=""
if ! mkdir -p "$BACKUP_ROOT_REQUESTED" 2>/dev/null; then
  BACKUP_DIR_SETUP_ERROR="$BACKUP_ROOT_REQUESTED"
else
  BACKUP_ROOT=$(realpath -e -- "$BACKUP_ROOT_REQUESTED") || BACKUP_DIR_SETUP_ERROR="$BACKUP_ROOT_REQUESTED"
  if [ -z "$BACKUP_DIR_SETUP_ERROR" ]; then
    if ! BACKUP_DIR=$(mktemp -d -- "${BACKUP_ROOT%/}/aperod_backups_XXXXXX" 2>/dev/null); then
      BACKUP_DIR_SETUP_ERROR="$BACKUP_ROOT"
    else
      BACKUP_DIR_OWNED=1
      BACKUP_DIR_CANONICAL=$(realpath -e -- "$BACKUP_DIR") || BACKUP_DIR_SETUP_ERROR="$BACKUP_DIR"
      if [ -z "$BACKUP_DIR_SETUP_ERROR" ]; then
        if [ "$(dirname -- "$BACKUP_DIR_CANONICAL")" != "$BACKUP_ROOT" ] \
          || [[ ! "$(basename -- "$BACKUP_DIR_CANONICAL")" =~ ^aperod_backups_[A-Za-z0-9]+$ ]]; then
          BACKUP_DIR_SETUP_ERROR="$BACKUP_DIR"
        else
          BACKUP_DIR="$BACKUP_DIR_CANONICAL"
        fi
      fi
    fi
  fi
fi
if [ -n "$BACKUP_DIR_SETUP_ERROR" ]; then
  echo "ОШИБКА: не удалось безопасно создать рабочую директорию для бэкапа: ${BACKUP_DIR_SETUP_ERROR}"
  echo "  Проверьте права доступа к каталогу: $(dirname -- "$BACKUP_DIR_SETUP_ERROR")"
  echo "  Бэкап пропущен (причина: backup_dir_unavailable)."
  _BACKUP_FINAL_STATUS="skipped"
  _BACKUP_SKIP_REASON="backup_dir_unavailable"
  _BACKUP_DISK_PATH="${BACKUP_DIR_SETUP_ERROR}"
  _BACKUP_DISK_FREE_GB="N/A"
  # ── Telegram backup_dir_unavailable alert ─────────────────────────────────
  if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
    _unavail_ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    _unavail_text="⚠️ <b>Бэкап Aperod пропущен — директория недоступна</b>%0A%0A"
    _unavail_text+="<b>Путь:</b> <code>${BACKUP_DIR_SETUP_ERROR}</code>%0A"
    _unavail_text+="<b>Время:</b> ${_unavail_ts}%0A%0A"
    _unavail_text+="[!] Создать директорию не удалось — проверьте права доступа к <code>$(dirname -- "$BACKUP_DIR_SETUP_ERROR")</code>.%0A%0A"
    _unavail_text+="💡 Исправьте права (chown/chmod) — бэкап возобновится на следующем расписании."
    curl -s --max-time 15 -X POST \
      "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
      -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
      -d "text=${_unavail_text}" \
      -d "parse_mode=HTML" \
      -d "disable_web_page_preview=true" \
      >/dev/null 2>&1 || true
    echo "  Telegram backup_dir_unavailable alert sent to admin chat."
  fi
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

# The node's root-only Unix socket exports a closed, logical chain.db. Never
# fall back to copying the live database: an unavailable or invalid checkpoint
# is a hard failure.
if [ "$EUID" -ne 0 ]; then
  echo "ОШИБКА: резервная копия требует root для доступа к сокету ноды."
  exit 1
fi
if [ ! -d "$CHAIN_DATA_DIR" ]; then
  echo "ОШИБКА: активный каталог данных ноды не найден: ${CHAIN_DATA_DIR}"
  exit 1
fi
CHAIN_DATA_DIR=$(realpath -e -- "$CHAIN_DATA_DIR")
BACKUP_SOCKET="${CHAIN_DATA_DIR}/${BACKUP_SOCKET_NAME}"
if [ ! -S "$BACKUP_SOCKET" ]; then
  echo "ОШИБКА: сокет логической резервной копии недоступен: ${BACKUP_SOCKET}"
  exit 1
fi

# The Go exporter copies the active LevelDB into .backup-staging on this same
# filesystem. Require room for the active DB's apparent size plus a 5 GiB
# reserve before asking it to begin; otherwise it could fill the node volume.
CHAIN_DB_DIR="${CHAIN_DATA_DIR}/chain.db"
if [ ! -d "$CHAIN_DB_DIR" ] || [ -L "$CHAIN_DB_DIR" ]; then
  echo "ОШИБКА: активная LevelDB chain.db отсутствует или является символической ссылкой: ${CHAIN_DB_DIR}"
  exit 1
fi
if find "$CHAIN_DB_DIR" -type l -print -quit 2>/dev/null | grep -q .; then
  echo "ОШИБКА: активная LevelDB chain.db содержит символическую ссылку; checkpoint не запрашивался."
  exit 1
fi
CHAIN_DB_KB=$(du -sk --apparent-size -- "$CHAIN_DB_DIR" 2>/dev/null | awk '{print $1}')
CHAIN_AVAIL_KB=$(df --output=avail "$CHAIN_DATA_DIR" 2>/dev/null | awk 'NR==2{print $1}')
if [[ ! "$CHAIN_DB_KB" =~ ^[0-9]+$ ]] || [[ ! "$CHAIN_AVAIL_KB" =~ ^[0-9]+$ ]]; then
  echo "ОШИБКА: не удалось измерить размер chain.db или свободное место до checkpoint; checkpoint не запрашивался."
  exit 1
fi
CHAIN_REQUIRED_KB=$(( CHAIN_DB_KB + MIN_AVAIL_KB ))
if [ "$CHAIN_AVAIL_KB" -lt "$CHAIN_REQUIRED_KB" ]; then
  CHAIN_FREE_GB=$(awk "BEGIN{printf \"%.1f\", ${CHAIN_AVAIL_KB}/1024/1024}")
  CHAIN_REQUIRED_GB=$(awk "BEGIN{printf \"%.1f\", ${CHAIN_REQUIRED_KB}/1024/1024}")
  echo "=== ВНИМАНИЕ: checkpoint пропущен из-за нехватки места на файловой системе ноды ==="
  echo "  chain.db занимает ${CHAIN_DB_KB} KiB; свободно ${CHAIN_FREE_GB} GiB; требуется ${CHAIN_REQUIRED_GB} GiB (DB + 5 GiB reserve)."
  _BACKUP_FINAL_STATUS="skipped"
  _BACKUP_SKIP_REASON="low_checkpoint_space"
  _BACKUP_DISK_PATH="active chain filesystem (${CHAIN_DATA_DIR})"
  _BACKUP_DISK_FREE_GB="${CHAIN_FREE_GB}"
  if [ -n "${TELEGRAM_BOT_TOKEN:-}" ] && [ -n "${ADMIN_TELEGRAM_CHAT_ID:-}" ]; then
    _checkpoint_ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    _checkpoint_text="⚠️ <b>Бэкап Aperod пропущен — недостаточно места для checkpoint</b>%0A%0A"
    _checkpoint_text+="<b>Файловая система:</b> ${CHAIN_DATA_DIR}%0A"
    _checkpoint_text+="<b>Размер chain.db:</b> ${CHAIN_DB_KB} KiB%0A"
    _checkpoint_text+="<b>Свободно:</b> ${CHAIN_FREE_GB} GiB%0A"
    _checkpoint_text+="<b>Требуется:</b> ${CHAIN_REQUIRED_GB} GiB (chain.db + 5 GiB reserve)%0A"
    _checkpoint_text+="<b>Время:</b> ${_checkpoint_ts}%0A"
    curl -s --max-time 15 -X POST \
      "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/sendMessage" \
      -d "chat_id=${ADMIN_TELEGRAM_CHAT_ID}" \
      -d "text=${_checkpoint_text}" \
      -d "parse_mode=HTML" \
      -d "disable_web_page_preview=true" \
      >/dev/null 2>&1 || true
  fi
  write_metrics 0 1
  exit 0
fi
BACKUP_VERIFY_BIN="${APEROD_BACKUP_VERIFY_BIN:-/usr/local/bin/aperod-backup-verify}"
if [ -L "$BACKUP_VERIFY_BIN" ] || [ ! -f "$BACKUP_VERIFY_BIN" ] || [ ! -x "$BACKUP_VERIFY_BIN" ]; then
  echo "ОШИБКА: обязательный verifier должен быть исполняемым обычным файлом: ${BACKUP_VERIFY_BIN}; checkpoint не запрашивался."
  exit 1
fi
BACKUP_VERIFY_BIN=$(realpath -e -- "$BACKUP_VERIFY_BIN") || {
  echo "ОШИБКА: обязательный verifier не удалось разрешить: ${BACKUP_VERIFY_BIN}; checkpoint не запрашивался."
  exit 1
}
if [ -L "$BACKUP_VERIFY_BIN" ] || [ ! -f "$BACKUP_VERIFY_BIN" ] || [ ! -x "$BACKUP_VERIFY_BIN" ]; then
  echo "ОШИБКА: verifier не является исполняемым обычным файлом: ${BACKUP_VERIFY_BIN}; checkpoint не запрашивался."
  exit 1
fi
if ! command -v pg_restore >/dev/null 2>&1; then
  echo "ОШИБКА: обязательная команда pg_restore недоступна; checkpoint не запрашивался."
  exit 1
fi

_auth_output_is_safe() {
  local path="$1" mode owner
  [ -f "$path" ] && [ ! -L "$path" ] || return 1
  owner=$(stat -c '%u' -- "$path") || return 1
  mode=$(stat -c '%a' -- "$path") || return 1
  [[ "$owner" = 0 && "$mode" =~ ^[0-7]+$ ]] || return 1
  (( (8#$mode & 077) == 0 ))
}

if [ ! -e "$AUTH_HELPER" ]; then
  echo "ПРЕДУПРЕЖДЕНИЕ: ${AUTH_HELPER} отсутствует; бэкап продолжится без rollout-cleanup authorization."
elif [ -L "$AUTH_HELPER" ] || [ ! -f "$AUTH_HELPER" ] || [ ! -x "$AUTH_HELPER" ] \
  || [ "$(stat -c '%u' -- "$AUTH_HELPER" 2>/dev/null || echo invalid)" != 0 ] \
  || (( (8#$(stat -c '%a' -- "$AUTH_HELPER" 2>/dev/null || echo 777) & 022) != 0 )); then
  echo "ПРЕДУПРЕЖДЕНИЕ: rollout-cleanup helper небезопасен или неисполняем; бэкап продолжится без authorization." >&2
else
  AUTH_HELPER=$(realpath -e -- "$AUTH_HELPER") || AUTH_HELPER_READY=0
  if [ -f "$AUTH_HELPER" ] && [ -x "$AUTH_HELPER" ] && [ ! -L "$AUTH_HELPER" ]; then
    AUTH_HELPER_READY=1
  fi
fi

if [ "$AUTH_HELPER_READY" -eq 1 ]; then
  AUTH_CONTEXT_FILE="${BACKUP_DIR}/rollout-context.json"
  AUTH_ANCHORS_FILE="${BACKUP_DIR}/chain-anchors.json"
  AUTH_PROOF_FILE="${BACKUP_DIR}/chain-proof.json"
  if [ "$(stat -c '%u' -- "$BACKUP_DIR")" != 0 ]; then
    echo "ПРЕДУПРЕЖДЕНИЕ: backup workdir не принадлежит root; rollout-cleanup authorization отключена."
    _disable_backup_authorization "backup-workdir-owner-invalid"
  else
    AUTH_BEGIN_INVOKED=1
    if "$AUTH_HELPER" backup-begin \
      --data-dir "$CHAIN_DATA_DIR" \
      --config /etc/aperod/node.yaml \
      --service aperod-node \
      --api-url http://127.0.0.1:8545 \
      --anchors-output "$AUTH_ANCHORS_FILE" \
      --context-output "$AUTH_CONTEXT_FILE"; then
      if _auth_output_is_safe "$AUTH_CONTEXT_FILE"; then
        AUTH_ATTEMPT_REGISTERED=1
      fi
      if [ "$AUTH_ATTEMPT_REGISTERED" -eq 1 ] && _auth_output_is_safe "$AUTH_ANCHORS_FILE"; then
        AUTH_ENABLED=1
        echo "  Rollout-cleanup authorization attempt registered with live chain anchors."
      else
        echo "ПРЕДУПРЕЖДЕНИЕ: backup-begin returned unsafe or missing root-owned context/anchors; authorization disabled." >&2
        _disable_backup_authorization "backup-begin-output-invalid"
      fi
    else
      if _auth_output_is_safe "$AUTH_CONTEXT_FILE"; then
        AUTH_ATTEMPT_REGISTERED=1
      fi
      echo "ПРЕДУПРЕЖДЕНИЕ: backup-begin failed; бэкап продолжится без rollout-cleanup authorization." >&2
      _disable_backup_authorization "backup-begin-failed"
    fi
  fi
fi

echo "=== [1/5] Запрашиваем закрытый логический checkpoint ноды ==="
CHECKPOINT_RESPONSE="${BACKUP_DIR}/checkpoint-response.json"
curl --fail --silent --show-error --max-time 900 \
  --unix-socket "$BACKUP_SOCKET" \
  --request POST \
  --output "$CHECKPOINT_RESPONSE" \
  "http://localhost/checkpoint"

STAGING_ROOT="${CHAIN_DATA_DIR}/.backup-staging"
SNAPSHOT_DIR=$(python3 - "$CHECKPOINT_RESPONSE" "$STAGING_ROOT" <<'PY'
import json
import os
import re
import stat
import sys

response_path, staging_root = sys.argv[1:]
if stat.S_ISLNK(os.lstat(staging_root).st_mode):
    raise SystemExit("checkpoint staging root must not be a symbolic link")
with open(response_path, "r", encoding="utf-8") as source:
    response = json.load(source)
if not isinstance(response, dict):
    raise SystemExit("checkpoint response must be a JSON object")
path = response.get("path")
height = response.get("tip_height")
tip_hash = response.get("tip_hash")
if not isinstance(path, str) or not path or any(ord(ch) < 32 for ch in path):
    raise SystemExit("checkpoint response has invalid path")
if stat.S_ISLNK(os.lstat(path).st_mode):
    raise SystemExit("checkpoint stage must not be a symbolic link")
if isinstance(height, bool) or not isinstance(height, int) or height < 0:
    raise SystemExit("checkpoint response has invalid tip_height")
if not isinstance(tip_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", tip_hash):
    raise SystemExit("checkpoint response has invalid tip_hash")

root = os.path.realpath(staging_root, strict=True)
stage = os.path.realpath(path, strict=True)
if os.path.commonpath((root, stage)) != root or stage == root:
    raise SystemExit("checkpoint stage escapes the active staging root")
if not os.path.isdir(stage):
    raise SystemExit("checkpoint stage is not a directory")

for name in ("chain.db", "manifest.json"):
    candidate = os.path.join(stage, name)
    info = os.lstat(candidate)
    if name == "chain.db":
        if not stat.S_ISDIR(info.st_mode):
            raise SystemExit("checkpoint chain.db is not a real directory")
        count = 0
        for current, directories, files in os.walk(candidate, followlinks=False):
            for child in directories + files:
                child_path = os.path.join(current, child)
                child_info = os.lstat(child_path)
                if stat.S_ISLNK(child_info.st_mode):
                    raise SystemExit("checkpoint chain.db contains a symbolic link")
                if stat.S_ISDIR(child_info.st_mode):
                    continue
                if not stat.S_ISREG(child_info.st_mode):
                    raise SystemExit("checkpoint chain.db contains a non-regular file")
                count += 1
        if count == 0:
            raise SystemExit("checkpoint chain.db is empty")
    elif (not stat.S_ISREG(info.st_mode) or info.st_size <= 0
            or info.st_nlink != 1):
        raise SystemExit("checkpoint stage has invalid manifest.json")

with open(os.path.join(stage, "manifest.json"), "r", encoding="utf-8") as source:
    manifest = json.load(source)
manifest_height = manifest.get("tip_height") if isinstance(manifest, dict) else None
if (not isinstance(manifest, dict) or manifest.get("success") is not True
        or isinstance(manifest_height, bool) or not isinstance(manifest_height, int)
        or manifest_height != height or manifest.get("tip_hash") != tip_hash):
    raise SystemExit("checkpoint manifest does not agree with socket response")
print(stage)
PY
)
rm -f -- "$CHECKPOINT_RESPONSE"
STAGING_ROOT=$(realpath -e -- "$STAGING_ROOT")
SNAPSHOT_DIR=$(realpath -e -- "$SNAPSHOT_DIR")
SNAPSHOT_VALIDATED=1
SNAPSHOT_DB="${SNAPSHOT_DIR}/chain.db"
SNAPSHOT_MANIFEST="${SNAPSHOT_DIR}/manifest.json"
mapfile -t CHECKPOINT_TIP < <(python3 - "$SNAPSHOT_MANIFEST" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    manifest = json.load(source)
print(manifest["tip_height"])
print(manifest["tip_hash"])
PY
)
if [ "${#CHECKPOINT_TIP[@]}" -ne 2 ]; then
  echo "ОШИБКА: невозможно прочитать tip из checkpoint manifest."
  exit 1
fi
CHECKPOINT_TIP_HEIGHT="${CHECKPOINT_TIP[0]}"
CHECKPOINT_TIP_HASH="${CHECKPOINT_TIP[1]}"

# Both source and destination filesystems get a fixed minimum-space check.
_disk_preflight "$BACKUP_DIR" "BACKUP_DIR (/opt/aperod/backup-tmp)"
_disk_preflight "$SNAPSHOT_DB" "closed checkpoint database (${SNAPSHOT_DB})"

echo "=== [2/5] Дамп PostgreSQL и проверка места ==="
echo "  Дамп PostgreSQL: ${DB_NAME} (user=${DB_USER}) ..."
sudo -u "$DB_USER" pg_dump -F c -b "$DB_NAME" > "${BACKUP_DIR}/explorer_db.dump"

# Conservatively require enough room for the uncompressed archive (therefore
# its encrypted output), which includes the immutable snapshot DB, manifest,
# and PostgreSQL dump, plus filesystem/crypto overhead.
_disk_preflight_scaled() {
  local db_kb manifest_kb dump_kb output_kb buffer_kb required_kb avail_kb
  db_kb=$(du -k -- "$SNAPSHOT_DB" | awk '{print $1}')
  manifest_kb=$(du -k -- "$SNAPSHOT_MANIFEST" | awk '{print $1}')
  dump_kb=$(du -k -- "${BACKUP_DIR}/explorer_db.dump" | awk '{print $1}')
  output_kb=$(( db_kb + manifest_kb + dump_kb ))
  buffer_kb=$(( 1 * 1024 * 1024 ))
  required_kb=$(( output_kb + buffer_kb ))
  avail_kb=$(df --output=avail "$BACKUP_DIR" 2>/dev/null | awk 'NR==2{print $1}')
  [ -z "$avail_kb" ] && avail_kb=0
  if [ "$avail_kb" -lt "$required_kb" ]; then
    local free_gb need_gb
    free_gb=$(awk "BEGIN{printf \"%.1f\", ${avail_kb}/1024/1024}")
    need_gb=$(awk "BEGIN{printf \"%.1f\", ${required_kb}/1024/1024}")
    echo "=== ВНИМАНИЕ: мало свободного места на диске ==="
    echo "  BACKUP_DIR: ${free_gb} ГБ свободно; требуется ~${need_gb} ГБ (checkpoint + архив + 1 ГБ)"
    _BACKUP_FINAL_STATUS="skipped"
    _BACKUP_SKIP_REASON="low_disk"
    _BACKUP_DISK_PATH="BACKUP_DIR (${BACKUP_DIR})"
    _BACKUP_DISK_FREE_GB="${free_gb}"
    write_metrics 0 1
    exit 0
  fi
  echo "  Checkpoint DB: ${db_kb} KiB; требуемое место для архива: ${required_kb} KiB — OK"
}
_disk_preflight_scaled

echo "=== [3/5] Архивирование + шифрование AES-256 (потоковый режим) ==="
# Only immutable, closed snapshot files and the database dump are archived.
# Tar errors on any source change; there is deliberately no ignore-failed-read.
tar -czf - \
  -C "$SNAPSHOT_DIR" chain.db manifest.json \
  -C "$BACKUP_DIR" explorer_db.dump \
  | gpg --batch --yes --passphrase-fd 3 \
      --symmetric --cipher-algo AES256 \
      -o "${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg" - \
    3< <(printf '%s' "$ENCRYPTION_PASSWORD")
echo "  Зашифровано: $(du -sh "${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg" | cut -f1)"

# ── Upload uniquely, download and fully verify before promotion ───────────────
echo "=== [4/5] Загрузка уникального кандидата и проверка удаленной копии ==="
ARCHIVE_FILE="${BACKUP_DIR}/${BACKUP_NAME}.tar.gpg"
ARCHIVE_SIZE=$(stat -c '%s' -- "$ARCHIVE_FILE")
ARCHIVE_SHA256=$(sha256sum -- "$ARCHIVE_FILE" | awk '{print $1}')
FIXED_OBJECT="${BACKUP_NAME}.tar.gpg"
FIXED_REMOTE="${RCLONE_REMOTE}/${FIXED_OBJECT}"
LEGACY_OBJECT_PREFIX="${BACKUP_NAME}_legacy_"
PRESERVED_LEGACY_OBJECTS=""
CANDIDATE_OBJECT="${BACKUP_NAME}_candidate_${TIMESTAMP}_$$_$(date +%s%N).tar.gpg"
CANDIDATE_REMOTE="${RCLONE_REMOTE}/${CANDIDATE_OBJECT}"
DOWNLOAD_FILE="${BACKUP_DIR}/remote-candidate.tar.gpg"

_require_download_space() {
  local bytes="$1" avail_kb required_kb
  [[ "$bytes" =~ ^[0-9]+$ ]] || {
    echo "ОШИБКА: неверный размер удаленного архива для локальной проверки."
    return 1
  }
  avail_kb=$(df --output=avail "$BACKUP_DIR" 2>/dev/null | awk 'NR==2{print $1}')
  [ -z "$avail_kb" ] && avail_kb=0
  required_kb=$(( (bytes + 1023) / 1024 + 65536 ))
  if [ "$avail_kb" -lt "$required_kb" ]; then
    echo "ОШИБКА: недостаточно локального места для скачивания и проверки удаленного архива."
    return 1
  fi
}

_copy_remote_fresh() {
  local remote="$1"
  local destination="$2"
  rm -f -- "$destination" || return 1
  rclone copyto "$remote" "$destination" --ignore-times --s3-no-check-bucket
}

_cleanup_archive_verify_dir() {
  local path="$1" canonical
  [ -n "$path" ] && [ ! -L "$path" ] || return 1
  canonical=$(realpath -e -- "$path") || return 1
  [ "$canonical" = "$path" ] \
    && [ "$(dirname -- "$canonical")" = "$BACKUP_DIR" ] \
    && [[ "$(basename -- "$canonical")" =~ ^archive_verify_[A-Za-z0-9]+$ ]] \
    && [ -d "$canonical" ] || return 1
  rm -rf -- "$canonical"
}

_verify_archive_payload() (
  set -euo pipefail
  local encrypted="$1" expected_height="${2:-}" expected_hash="${3:-}"
  local payload_mode="${4:-new}"
  local proof_anchors="${5:-}" proof_output="${6:-}"
  local verify_root verify_stage tar_file extractor encrypted_bytes encrypted_kb unpacked_bytes avail_kb need_kb verifier_output
  verify_root=$(mktemp -d -- "${BACKUP_DIR}/archive_verify_XXXXXX") || return 1
  trap '_cleanup_archive_verify_dir "$verify_root" || true' EXIT
  verify_stage="${verify_root}/stage"
  tar_file="${verify_root}/archive.tar.gz"
  extractor="${verify_root}/safe-extract.py"
  mkdir -m 700 -- "$verify_stage" || return 1
  encrypted_bytes=$(stat -c '%s' -- "$encrypted") || return 1
  encrypted_kb=$(( (encrypted_bytes + 1023) / 1024 ))
  avail_kb=$(df --output=avail "$BACKUP_DIR" 2>/dev/null | awk 'NR==2{print $1}')
  [[ "$avail_kb" =~ ^[0-9]+$ ]] || return 1
  if [ "$avail_kb" -lt "$((encrypted_kb + 65536))" ]; then
    echo "ОШИБКА: недостаточно локального места для расшифровки remote archive." >&2
    return 1
  fi
  gpg --batch --yes --passphrase-fd 3 --decrypt --output "$tar_file" "$encrypted" \
    3< <(printf '%s' "$ENCRYPTION_PASSWORD") || return 1
  cat > "$extractor" <<'PY' || return 1
import os
import shutil
import sys
import tarfile

mode, archive_path = sys.argv[1:3]
destination = sys.argv[3] if len(sys.argv) > 3 else None
legacy_mode = mode.endswith("-legacy")

def validated_members(archive, legacy):
    members = archive.getmembers()
    names = set()
    required = ({"testnet": False, "testnet/chain.db": False, "explorer_db.dump": False}
                if legacy else
                {"chain.db": False, "manifest.json": False, "explorer_db.dump": False})
    chain_files = 0
    total_bytes = 0
    for member in members:
        name = member.name
        if not isinstance(name, str) or not name or "\x00" in name or any(ord(c) < 32 for c in name):
            raise ValueError("archive member has an invalid path")
        while name.startswith("./"):
            name = name[2:]
        normalized = name[:-1] if name.endswith("/") else name
        if not normalized:
            if not member.isdir():
                raise ValueError("archive root entry is not a directory")
            continue
        parts = normalized.split("/")
        if normalized.startswith("/") or any(part in ("", ".", "..") for part in parts):
            raise ValueError("archive member path is unsafe")
        if normalized in names:
            raise ValueError("archive contains duplicate members")
        names.add(normalized)
        if not (member.isdir() or member.isreg()) or getattr(member, "sparse", None) is not None:
            raise ValueError("archive contains a link or special file")
        if legacy:
            if normalized == "testnet":
                if not member.isdir():
                    raise ValueError("legacy testnet root is not a directory")
                required["testnet"] = True
            elif normalized == "testnet/chain.db":
                if not member.isdir():
                    raise ValueError("legacy chain.db root is not a directory")
                required["testnet/chain.db"] = True
            elif normalized == "explorer_db.dump":
                if not member.isreg():
                    raise ValueError("legacy explorer_db.dump must be a regular file")
                required["explorer_db.dump"] = True
                total_bytes += member.size
            elif normalized.startswith("testnet/chain.db/"):
                if member.isfile():
                    chain_files += 1
                total_bytes += member.size if member.isfile() else 0
            else:
                # Historical node-data archives can contain unrelated regular files.
                if not (member.isdir() or member.isreg()):
                    raise ValueError("legacy archive contains a non-regular extra member")
                total_bytes += member.size if member.isfile() else 0
        elif normalized == "chain.db":
            if not member.isdir():
                raise ValueError("chain.db archive root is not a directory")
            required["chain.db"] = True
        elif normalized in ("manifest.json", "explorer_db.dump"):
            if not member.isreg():
                raise ValueError(f"{normalized} must be a regular file")
            required[normalized] = True
            total_bytes += member.size
        elif parts[0] == "chain.db" and len(parts) > 1:
            if not (member.isdir() or member.isreg()) or getattr(member, "sparse", None) is not None:
                raise ValueError("chain.db contains a non-regular archive entry")
            if member.isreg():
                chain_files += 1
                total_bytes += member.size
        else:
            raise ValueError("archive contains an unexpected member")
    if not all(required.values()) or chain_files == 0:
        if legacy:
            raise ValueError("legacy archive is missing testnet/chain.db or explorer_db.dump")
        raise ValueError("archive is missing chain.db, manifest, or pg_dump")
    return members, total_bytes

with tarfile.open(archive_path, mode="r:gz") as archive:
    members, total = validated_members(archive, legacy_mode)
    if mode in ("--inspect", "--inspect-legacy"):
        print(total)
    elif mode in ("--extract", "--extract-legacy") and destination:
        for member in members:
            name = member.name
            while name.startswith("./"):
                name = name[2:]
            normalized = name[:-1] if name.endswith("/") else name
            if not normalized:
                continue
            target = os.path.join(destination, *normalized.split("/"))
            if member.isdir():
                os.makedirs(target, mode=0o700, exist_ok=True)
                continue
            os.makedirs(os.path.dirname(target), mode=0o700, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                raise ValueError("archive regular file has no contents")
            written = 0
            with source, open(target, "xb") as output:
                while True:
                    block = source.read(1024 * 1024)
                    if not block:
                        break
                    output.write(block)
                    written += len(block)
            if written != member.size:
                raise ValueError("archive member size changed during extraction")
    else:
        raise ValueError("invalid safe-extractor mode")
PY
  if [ "$payload_mode" = "legacy" ]; then
    inspect_mode="--inspect-legacy"
    extract_mode="--extract-legacy"
  else
    inspect_mode="--inspect"
    extract_mode="--extract"
  fi
  unpacked_bytes=$(python3 "$extractor" "$inspect_mode" "$tar_file") || return 1
  [[ "$unpacked_bytes" =~ ^[0-9]+$ ]] || return 1
  avail_kb=$(df --output=avail "$BACKUP_DIR" 2>/dev/null | awk 'NR==2{print $1}')
  [[ "$avail_kb" =~ ^[0-9]+$ ]] || return 1
  need_kb=$(( (unpacked_bytes + 1023) / 1024 + 65536 ))
  if [ "$avail_kb" -lt "$need_kb" ]; then
    echo "ОШИБКА: недостаточно локального места для безопасной распаковки и проверки remote archive." >&2
    return 1
  fi
  python3 "$extractor" "$extract_mode" "$tar_file" "$verify_stage" || return 1
  if [ "$payload_mode" = "legacy" ]; then
    verifier_output=$("$BACKUP_VERIFY_BIN" --legacy-stage "$verify_stage") || return 1
    python3 - "$verifier_output" <<'PY' || return 1
import json, re, sys
try:
    result = json.loads(sys.argv[1])
except json.JSONDecodeError as error:
    raise SystemExit(f"legacy backup verifier returned invalid JSON: {error}")
height = result.get("tip_height") if isinstance(result, dict) else None
tip_hash = result.get("tip_hash") if isinstance(result, dict) else None
if (not isinstance(result, dict) or result.get("success") is not True
        or result.get("legacy") is not True
        or isinstance(height, bool) or not isinstance(height, int) or height < 0
        or not isinstance(tip_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", tip_hash)):
    raise SystemExit("legacy backup verifier result is invalid")
PY
  else
    python3 - "$verify_stage/manifest.json" <<'PY' || return 1
import json, re, sys
with open(sys.argv[1], encoding="utf-8") as source:
    manifest = json.load(source)
if not isinstance(manifest, dict) or manifest.get("success") is not True:
    raise SystemExit("extracted manifest is invalid")
height = manifest.get("tip_height")
tip_hash = manifest.get("tip_hash")
if isinstance(height, bool) or not isinstance(height, int) or height < 0:
    raise SystemExit("extracted manifest tip_height is invalid")
if not isinstance(tip_hash, str) or not re.fullmatch(r"[0-9a-f]{64}", tip_hash):
    raise SystemExit("extracted manifest tip_hash is invalid")
PY
    verifier_output=$("$BACKUP_VERIFY_BIN" --stage "$verify_stage") || return 1
    python3 - "$verifier_output" "$verify_stage/manifest.json" "$expected_height" "$expected_hash" <<'PY' || return 1
import json, sys
with open(sys.argv[2], encoding="utf-8") as source:
    manifest = json.load(source)
try:
    result = json.loads(sys.argv[1])
except json.JSONDecodeError as error:
    raise SystemExit(f"backup verifier returned invalid JSON: {error}")
if (not isinstance(result, dict) or result.get("success") is not True
        or result.get("legacy") is True
        or result.get("tip_hash") != manifest.get("tip_hash")
        or result.get("tip_height") != manifest.get("tip_height")):
    raise SystemExit("backup verifier result does not match extracted manifest tip")
if sys.argv[3] and (str(manifest.get("tip_height")) != sys.argv[3]
                    or manifest.get("tip_hash") != sys.argv[4]):
    raise SystemExit("verified archive tip differs from checkpoint tip")
PY
  fi
  pg_restore --file /dev/null "$verify_stage/explorer_db.dump" || return 1
  if [ -n "$proof_anchors" ] || [ -n "$proof_output" ]; then
    if [ "$payload_mode" != "new" ] || [ -z "$proof_anchors" ] || [ -z "$proof_output" ]; then
      echo "ОШИБКА: chain proof разрешен только для new-format archive с обоими capture путями." >&2
      _disable_backup_authorization "backup-proof-mode-invalid" || true
    elif ! "$BACKUP_VERIFY_BIN" --stage "$verify_stage" \
      --anchors-file "$proof_anchors" --proof-output "$proof_output" >/dev/null; then
      echo "ПРЕДУПРЕЖДЕНИЕ: genesis/anchor proof не подтвержден; auto-cleanup authorization отключена." >&2
      rm -f -- "$proof_output"
      _disable_backup_authorization "backup-proof-failed" || true
    fi
  fi
)

_verify_remote_archive() {
  local remote="$1"
  local download="$2"
  local expected_size="${3:-$ARCHIVE_SIZE}"
  local expected_sha="${4:-$ARCHIVE_SHA256}"
  local expected_height="${5:-}" expected_hash="${6:-}"
  local payload_mode="${7:-new}"
  local proof_anchors="${8:-}" proof_output="${9:-}"
  local remote_size remote_sha
  _require_download_space "$expected_size" || return 1
  _copy_remote_fresh "$remote" "$download" || return 1
  remote_size=$(rclone size "$remote" --s3-no-check-bucket --json \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); n=d.get("bytes"); assert isinstance(n,int) and n >= 0; print(n)') || return 1
  [ "$remote_size" = "$expected_size" ] || {
    echo "ОШИБКА: размер загруженного объекта не совпадает с локальным архивом."
    return 1
  }
  [ "$(stat -c '%s' -- "$download")" = "$expected_size" ] || {
    echo "ОШИБКА: размер скачанного архива не совпадает с локальным."
    return 1
  }
  remote_sha=$(sha256sum -- "$download" | awk '{print $1}') || return 1
  [ "$remote_sha" = "$expected_sha" ] || {
    echo "ОШИБКА: SHA256 удаленного архива не совпадает."
    return 1
  }
  _verify_archive_payload "$download" "$expected_height" "$expected_hash" "$payload_mode" \
    "$proof_anchors" "$proof_output"
}

# A successful bucket listing is required to distinguish an absent fixed object
# from a credentials/network error. Never overwrite a possibly-good object if
# its existing state cannot be determined.
FIXED_LISTING="${BACKUP_DIR}/remote-root-listing.json"
rclone lsjson "$RCLONE_REMOTE" --max-depth 1 --s3-no-check-bucket > "$FIXED_LISTING"
REMOTE_USED_BYTES=$(python3 - "$FIXED_LISTING" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
if not isinstance(entries, list):
    raise SystemExit("remote listing is not an array")
total = 0
for item in entries:
    if not isinstance(item, dict):
        raise SystemExit("remote listing contains an invalid entry")
    if item.get("IsDir"):
        continue
    size = item.get("Size")
    if isinstance(size, bool) or not isinstance(size, int) or size < 0:
        raise SystemExit("remote listing contains an invalid object size")
    total += size
print(total)
PY
)
FIXED_EXISTS=$(python3 - "$FIXED_LISTING" "$FIXED_OBJECT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
print("yes" if any(item.get("Name") == sys.argv[2] and not item.get("IsDir")
                  for item in entries) else "no")
PY
)
PREVIOUS_EXISTS=$(python3 - "$FIXED_LISTING" "$PREVIOUS_OBJECT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
print("yes" if any(item.get("Name") == sys.argv[2] and not item.get("IsDir")
                  for item in entries) else "no")
PY
)
FIXED_REMOTE_SIZE=$(python3 - "$FIXED_LISTING" "$FIXED_OBJECT" "$FIXED_EXISTS" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
found = [item.get("Size") for item in entries if item.get("Name") == sys.argv[2] and not item.get("IsDir")]
if sys.argv[3] == "no":
    print(0)
    raise SystemExit(0)
if len(found) != 1 or isinstance(found[0], bool) or not isinstance(found[0], int) or found[0] < 0:
    raise SystemExit("fixed backup listing has no unique valid size")
print(found[0])
PY
)
PREVIOUS_REMOTE_SIZE=$(python3 - "$FIXED_LISTING" "$PREVIOUS_OBJECT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
found = [item.get("Size") for item in entries if item.get("Name") == sys.argv[2] and not item.get("IsDir")]
if not found:
    print(0)
elif len(found) != 1 or isinstance(found[0], bool) or not isinstance(found[0], int) or found[0] < 0:
    raise SystemExit("previous backup listing has an invalid size")
else:
    print(found[0])
PY
)
LEGACY_OBJECTS=$(python3 - "$FIXED_LISTING" "$FIXED_OBJECT" "$PREVIOUS_OBJECT" "$LEGACY_OBJECT_PREFIX" <<'PY'
import json, re, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
fixed, previous, preserved_prefix = sys.argv[2:]
legacy = []
for item in entries:
    if not isinstance(item, dict) or item.get("IsDir"):
        continue
    name = item.get("Name")
    if name in (fixed, previous) or not isinstance(name, str):
        continue
    if name.startswith(preserved_prefix):
        continue
    if name.startswith("aperod_backup_") and name.endswith(".tar.gpg"):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
            raise SystemExit("legacy backup object has an unsafe name")
        legacy.append(name)
print("\n".join(sorted(set(legacy))))
PY
)
PRESERVED_LEGACY_OBJECTS=$(python3 - "$FIXED_LISTING" "$LEGACY_OBJECT_PREFIX" <<'PY'
import json, re, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
prefix = sys.argv[2]
names = []
for item in entries:
    if not isinstance(item, dict) or item.get("IsDir"):
        continue
    name = item.get("Name")
    if isinstance(name, str) and name.startswith(prefix) and name.endswith(".tar.gpg"):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+", name):
            raise SystemExit("preserved legacy object has an unsafe name")
        names.append(name)
print("\n".join(sorted(set(names))))
PY
)

case "$S3_ENDPOINT" in
  *backblazeb2.com*) IS_B2=1 ;;
  *) IS_B2=0 ;;
esac
if [ "$AUTH_ENABLED" -eq 1 ] && [ "$IS_B2" -ne 1 ]; then
  echo "ПРЕДУПРЕЖДЕНИЕ: rollout-cleanup authorization поддерживает только immutable native B2 fileId; S3 backup будет продолжен без auto-cleanup." >&2
  _disable_backup_authorization "backup-provider-unsupported"
fi

PREVIOUS_FIXED="${BACKUP_DIR}/previous-fixed.tar.gpg"
PREVIOUS_REMOTE="${RCLONE_REMOTE}/${PREVIOUS_OBJECT}"
PREVIOUS_OLD_LOCAL="${BACKUP_DIR}/previous-generation-old.tar.gpg"
PREVIOUS_FIXED_SIZE=0
PREVIOUS_FIXED_SHA=""
PREVIOUS_OLD_SIZE=0
PREVIOUS_OLD_SHA=""
FIXED_FORMAT="absent"
if [ "$FIXED_EXISTS" = "yes" ]; then
  _require_download_space "$FIXED_REMOTE_SIZE"
  _copy_remote_fresh "$FIXED_REMOTE" "$PREVIOUS_FIXED"
  [ -s "$PREVIOUS_FIXED" ] || {
    echo "ОШИБКА: существующая фиксированная копия не сохранена локально."
    exit 1
  }
  [ "$(stat -c '%s' -- "$PREVIOUS_FIXED")" = "$FIXED_REMOTE_SIZE" ] || {
    echo "ОШИБКА: размер скачанной предыдущей копии отличается от remote listing."
    exit 1
  }
  PREVIOUS_FIXED_SIZE=$(stat -c '%s' -- "$PREVIOUS_FIXED")
  PREVIOUS_FIXED_SHA=$(sha256sum -- "$PREVIOUS_FIXED" | awk '{print $1}')
  # This fixed object has already been independently force-downloaded above,
  # and its listed size plus local SHA256 were checked. Verify both supported
  # archive layouts from that same byte-checked local ciphertext so a legacy
  # B2 object incurs only one old-fixed download against the account bandwidth cap.
  if _verify_archive_payload "$PREVIOUS_FIXED" "" "" new; then
    FIXED_FORMAT="new"
  elif _verify_archive_payload "$PREVIOUS_FIXED" "" "" legacy; then
    FIXED_FORMAT="legacy"
    echo "ВНИМАНИЕ: текущая копия legacy; read-only DB проверка пройдена, полнота истории не подтверждена."
  else
    echo "ОШИБКА: существующий fixed archive не прошел ни новую, ни legacy read-only проверку; он сохранен."
    exit 1
  fi
fi

# S3-compatible APIs do not expose a portable quota query. Now that the fixed
# payload mode is known, use the peak relevant to the rotation. B2 retains the
# current fixed upload as a version, so peak is existing visible/hidden bytes +
# candidate + replacement; a legacy migration additionally writes one exact
# legacy copy. Other S3 providers keep the more conservative four-archive peak.
REMOTE_CAP_BYTES="${APEROD_BACKUP_REMOTE_CAP_BYTES:-}"
REMOTE_HIDDEN_VERSION_BYTES="${APEROD_BACKUP_REMOTE_HIDDEN_VERSION_BYTES:-0}"
if [[ ! "$REMOTE_HIDDEN_VERSION_BYTES" =~ ^[0-9]+$ ]]; then
  echo "ОШИБКА: APEROD_BACKUP_REMOTE_HIDDEN_VERSION_BYTES должен быть целым числом байт."
  exit 1
fi
REMOTE_HIDDEN_VERSION_BYTES=$((10#$REMOTE_HIDDEN_VERSION_BYTES))
if [ "$IS_B2" -eq 1 ]; then
  REMOTE_PEAK_BYTES=$(( REMOTE_USED_BYTES + REMOTE_HIDDEN_VERSION_BYTES + ARCHIVE_SIZE * 2 ))
  if [ "$FIXED_FORMAT" = "legacy" ]; then
    REMOTE_PEAK_BYTES=$(( REMOTE_PEAK_BYTES + FIXED_REMOTE_SIZE ))
  fi
else
  REMOTE_PEAK_BYTES=$(( REMOTE_USED_BYTES + REMOTE_HIDDEN_VERSION_BYTES + ARCHIVE_SIZE * 4 + FIXED_REMOTE_SIZE ))
fi
if [ -n "$REMOTE_CAP_BYTES" ]; then
  if [[ ! "$REMOTE_CAP_BYTES" =~ ^[0-9]+$ ]]; then
    echo "ОШИБКА: APEROD_BACKUP_REMOTE_CAP_BYTES должен быть целым числом байт."
    exit 1
  fi
  REMOTE_CAP_BYTES=$((10#$REMOTE_CAP_BYTES))
  if [ "$IS_B2" -eq 1 ] && [ -z "${APEROD_BACKUP_REMOTE_HIDDEN_VERSION_BYTES:-}" ]; then
    echo "ОШИБКА: для проверки лимита B2 задайте APEROD_BACKUP_REMOTE_HIDDEN_VERSION_BYTES; S3 listing не показывает старые версии."
    exit 1
  fi
  if [ "$REMOTE_PEAK_BYTES" -gt "$REMOTE_CAP_BYTES" ]; then
    echo "ОШИБКА: удаленного лимита недостаточно для безопасной ротации (требуется до ${REMOTE_PEAK_BYTES} байт)."
    exit 1
  fi
else
  if [ "$IS_B2" -eq 1 ]; then
    echo "ПРЕДУПРЕЖДЕНИЕ: лимит удаленного хранилища неизвестен; B2 ротация может временно потребовать два новых архива плюс копию legacy fixed сверх скрытых версий."
  else
    echo "ПРЕДУПРЕЖДЕНИЕ: лимит удаленного хранилища неизвестен; безопасная ротация может временно потребовать до четырех новых архивов плюс копию текущего fixed сверх скрытых версий."
  fi
fi
if [ "$IS_B2" -eq 1 ]; then
  echo "ПРЕДУПРЕЖДЕНИЕ: S3 listing не учитывает старые версии B2; оценка использует APEROD_BACKUP_REMOTE_HIDDEN_VERSION_BYTES, если задан."
fi

if [ "$FIXED_FORMAT" = "legacy" ]; then
  LEGACY_OBJECT="${LEGACY_OBJECT_PREFIX}${PREVIOUS_FIXED_SHA}_${TIMESTAMP}_$$_$(date +%s%N).tar.gpg"
  LEGACY_REMOTE="${RCLONE_REMOTE}/${LEGACY_OBJECT}"
  LEGACY_COPY_LOCAL="${BACKUP_DIR}/preserved-legacy-copy.tar.gpg"
  if python3 - "$FIXED_LISTING" "$LEGACY_OBJECT" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    entries = json.load(source)
raise SystemExit(0 if any(item.get("Name") == sys.argv[2] and not item.get("IsDir")
                          for item in entries) else 1)
PY
  then
    echo "ОШИБКА: уникальное имя preserved legacy object неожиданно занято; старый fixed не перезаписан."
    exit 1
  fi
  echo "Сохраняем точные legacy ciphertext bytes в ${LEGACY_OBJECT} до замены fixed."
  rclone copyto "$PREVIOUS_FIXED" "$LEGACY_REMOTE" --s3-no-check-bucket
  _require_download_space "$PREVIOUS_FIXED_SIZE"
  _copy_remote_fresh "$LEGACY_REMOTE" "$LEGACY_COPY_LOCAL"
  if [ "$(stat -c '%s' -- "$LEGACY_COPY_LOCAL")" != "$PREVIOUS_FIXED_SIZE" ] \
    || [ "$(sha256sum -- "$LEGACY_COPY_LOCAL" | awk '{print $1}')" != "$PREVIOUS_FIXED_SHA" ]; then
    echo "ОШИБКА: preserved legacy object не совпал байт-в-байт; fixed оставлен без изменений."
    exit 1
  fi
  echo "Legacy ciphertext независимо скачан и SHA256 проверен; эта копия остается до двух new-format поколений."
fi
if [ "$IS_B2" -eq 0 ] && [ "$PREVIOUS_EXISTS" = "yes" ]; then
  _require_download_space "$PREVIOUS_REMOTE_SIZE"
  _copy_remote_fresh "$PREVIOUS_REMOTE" "$PREVIOUS_OLD_LOCAL"
  [ -s "$PREVIOUS_OLD_LOCAL" ] || {
    echo "ОШИБКА: предыдущая проверенная копия не сохранена для безопасной ротации."
    exit 1
  }
  [ "$(stat -c '%s' -- "$PREVIOUS_OLD_LOCAL")" = "$PREVIOUS_REMOTE_SIZE" ] || {
    echo "ОШИБКА: размер скачанной previous-копии отличается от remote listing."
    exit 1
  }
  PREVIOUS_OLD_SIZE=$(stat -c '%s' -- "$PREVIOUS_OLD_LOCAL")
  PREVIOUS_OLD_SHA=$(sha256sum -- "$PREVIOUS_OLD_LOCAL" | awk '{print $1}')
  _verify_remote_archive "$PREVIOUS_REMOTE" "$PREVIOUS_OLD_LOCAL" \
    "$PREVIOUS_OLD_SIZE" "$PREVIOUS_OLD_SHA"
fi

rclone copyto "$ARCHIVE_FILE" "$CANDIDATE_REMOTE" --s3-no-check-bucket
if ! _verify_remote_archive "$CANDIDATE_REMOTE" "$DOWNLOAD_FILE" \
  "$ARCHIVE_SIZE" "$ARCHIVE_SHA256" "$CHECKPOINT_TIP_HEIGHT" "$CHECKPOINT_TIP_HASH"; then
  exit 1
fi
echo "  Уникальный удаленный объект проверен: размер, SHA256 и содержимое архива совпадают."

# Promote only a candidate that passed a full download/decrypt/list validation.
# If replacement verification fails, restore the preexisting fixed object (or
# remove the new name if there was no predecessor) before returning failure.
_restore_previous_fixed() {
  if [ "$FIXED_EXISTS" = "yes" ]; then
    echo "ОШИБКА: восстанавливаем предыдущую фиксированную копию."
    if ! rclone copyto "$PREVIOUS_FIXED" "$FIXED_REMOTE" --s3-no-check-bucket; then
      echo "КРИТИЧЕСКАЯ ОШИБКА: не удалось восстановить предыдущий фиксированный объект."
      return 1
    fi
    if ! _require_download_space "$PREVIOUS_FIXED_SIZE" \
      || ! _copy_remote_fresh "$FIXED_REMOTE" "$DOWNLOAD_FILE" \
      || [ "$(sha256sum -- "$DOWNLOAD_FILE" | awk '{print $1}')" != "$PREVIOUS_FIXED_SHA" ]; then
      echo "КРИТИЧЕСКАЯ ОШИБКА: восстановленный фиксированный объект не прошел SHA256."
      return 1
    fi
  else
    rclone deletefile "$FIXED_REMOTE" --s3-no-check-bucket || {
      echo "КРИТИЧЕСКАЯ ОШИБКА: не удалось удалить непроверенный фиксированный объект."
      return 1
    }
  fi
}

echo "=== [5/5] Продвижение проверенного архива и ротация ==="
if ! rclone copyto "$ARCHIVE_FILE" "$FIXED_REMOTE" --s3-no-check-bucket; then
  _restore_previous_fixed || true
  exit 1
fi
AUTH_CAPTURE_ARGS=()
if [ "$AUTH_ENABLED" -eq 1 ]; then
  rm -f -- "$AUTH_PROOF_FILE"
  if [ "$IS_B2" -eq 1 ] && "$AUTH_HELPER" backup-pin \
    --context "$AUTH_CONTEXT_FILE" \
    --settings "$SETTINGS_FILE" \
    --remote-object "$FIXED_OBJECT"; then
    AUTH_PINNED=1
    AUTH_CAPTURE_ARGS=("$AUTH_ANCHORS_FILE" "$AUTH_PROOF_FILE")
  else
    echo "ПРЕДУПРЕЖДЕНИЕ: backup-pin не подтвердил immutable B2 object; verified backup продолжится без authorization." >&2
    _disable_backup_authorization "backup-pin-failed"
  fi
fi
if ! _verify_remote_archive "$FIXED_REMOTE" "$DOWNLOAD_FILE" \
  "$ARCHIVE_SIZE" "$ARCHIVE_SHA256" "$CHECKPOINT_TIP_HEIGHT" "$CHECKPOINT_TIP_HASH" \
  new "${AUTH_CAPTURE_ARGS[@]}"; then
  _restore_previous_fixed || true
  exit 1
fi
if [ "$AUTH_PINNED" -eq 1 ] && ! _auth_output_is_safe "$AUTH_PROOF_FILE"; then
  echo "ПРЕДУПРЕЖДЕНИЕ: fixed archive verified, но atomically generated proof отсутствует или небезопасен; authorization отключена." >&2
  _disable_backup_authorization "backup-proof-output-invalid"
fi

# Keep the prior fixed backup under an immutable distinct object on providers
# without accessible object-version retention. Never replace that object until
# the new fixed object has passed full remote verification.
HAVE_PREVIOUS_VERIFIED=0
if [ "$FIXED_FORMAT" = "legacy" ]; then
  if [ "$IS_B2" -eq 1 ]; then
    _cleanup_b2_object_versions "$FIXED_OBJECT"
  fi
  # On B2, fixed and preserved legacy ciphertexts are now independently
  # verified. The candidate duplicates the freshly verified fixed upload, so
  # remove only that exact object by native fileId even before a second
  # new-format generation exists. A cleanup failure leaves fixed + legacy.
  if [ "$IS_B2" -eq 1 ]; then
    _cleanup_b2_object_versions "$CANDIDATE_OBJECT" 0
  fi
  echo "Degraded migration run: preserved legacy predecessor remains but is not a verified new-format previous generation."
elif [ "$IS_B2" -eq 1 ]; then
  _cleanup_b2_object_versions "$FIXED_OBJECT"
  [ "$FIXED_EXISTS" = "yes" ] && HAVE_PREVIOUS_VERIFIED=1
else
  if [ "$FIXED_EXISTS" = "yes" ]; then
    _restore_previous_generation() {
      if [ "$PREVIOUS_EXISTS" = "yes" ]; then
        rclone copyto "$PREVIOUS_OLD_LOCAL" "$PREVIOUS_REMOTE" --s3-no-check-bucket || return 1
        _require_download_space "$PREVIOUS_OLD_SIZE" || return 1
        _copy_remote_fresh "$PREVIOUS_REMOTE" "$DOWNLOAD_FILE" || return 1
        [ "$(sha256sum -- "$DOWNLOAD_FILE" | awk '{print $1}')" = "$PREVIOUS_OLD_SHA" ] || return 1
      else
        rclone deletefile "$PREVIOUS_REMOTE" --s3-no-check-bucket || return 1
      fi
    }
    if ! rclone copyto "$PREVIOUS_FIXED" "$PREVIOUS_REMOTE" --s3-no-check-bucket \
      || ! _verify_remote_archive "$PREVIOUS_REMOTE" "$DOWNLOAD_FILE" \
        "$PREVIOUS_FIXED_SIZE" "$PREVIOUS_FIXED_SHA"; then
      echo "ОШИБКА: предыдущую проверенную генерацию не удалось сохранить; откатываем текущий объект."
      _restore_previous_generation || echo "КРИТИЧЕСКАЯ ОШИБКА: не удалось восстановить прежний объект previous."
      _restore_previous_fixed || echo "КРИТИЧЕСКАЯ ОШИБКА: не удалось восстановить прежний fixed."
      exit 1
    fi
    HAVE_PREVIOUS_VERIFIED=1
  elif [ "$PREVIOUS_EXISTS" = "yes" ]; then
    HAVE_PREVIOUS_VERIFIED=1
  fi
fi

# Prune timestamped generations only after a distinct, verified previous
# generation exists (or B2's two retained upload fileIds are confirmed).
if [ "$HAVE_PREVIOUS_VERIFIED" -eq 1 ]; then
  if [ "$IS_B2" -eq 1 ]; then
    _cleanup_b2_object_versions "$CANDIDATE_OBJECT" 0
  else
    rclone deletefile "$CANDIDATE_REMOTE" --s3-no-check-bucket
  fi
  while IFS= read -r legacy_object; do
    [ -n "$legacy_object" ] || continue
    if [ "$IS_B2" -eq 1 ]; then
      _cleanup_b2_object_versions "$legacy_object" 0
    else
      rclone deletefile "${RCLONE_REMOTE}/${legacy_object}" --s3-no-check-bucket
    fi
  done <<< "$LEGACY_OBJECTS"
  while IFS= read -r preserved_legacy_object; do
    [ -n "$preserved_legacy_object" ] || continue
    if [ "$IS_B2" -eq 1 ]; then
      _cleanup_b2_object_versions "$preserved_legacy_object" 0
    else
      rclone deletefile "${RCLONE_REMOTE}/${preserved_legacy_object}" --s3-no-check-bucket
    fi
  done <<< "$PRESERVED_LEGACY_OBJECTS"
else
  # Keep the candidate until a previous new-format generation exists. The
  # B2-specific first-migration exception prunes only its redundant candidate;
  # its preserved exact-byte legacy object remains until the second generation.
  echo "Предыдущей new-format генерации пока нет; старые копии и verified candidate сохранены."
fi

if [ "$AUTH_ENABLED" -eq 1 ] && [ "$AUTH_PINNED" -eq 1 ]; then
  if _auth_output_is_safe "$AUTH_PROOF_FILE" && "$AUTH_HELPER" backup-publish \
    --context "$AUTH_CONTEXT_FILE" \
    --proof "$AUTH_PROOF_FILE" \
    --settings "$SETTINGS_FILE" \
    --remote-object "$FIXED_OBJECT" \
    --archive-sha256 "$ARCHIVE_SHA256" \
    --archive-size "$ARCHIVE_SIZE" \
    --tip-height "$CHECKPOINT_TIP_HEIGHT" \
    --tip-hash "$CHECKPOINT_TIP_HASH"; then
    echo "  Rollout-cleanup backup authorization published after verified B2 retention."
    AUTH_ENABLED=0
    AUTH_PINNED=0
  else
    echo "ПРЕДУПРЕЖДЕНИЕ: backup-publish failed after normal backup verification; cleanup authorization is disabled." >&2
    _disable_backup_authorization "backup-publish-failed"
  fi
fi

_BACKUP_FILE_NAME="$FIXED_OBJECT"
_BACKUP_FILE_BYTES="$ARCHIVE_SIZE"
echo "=== [5/5] Удаленная фиксированная копия проверена; ротация завершена ==="

# ── Write success metrics ──────────────────────────────────────────────────────
_BACKUP_FINAL_STATUS="ok"
if [ "$FIXED_FORMAT" = "legacy" ]; then
  _BACKUP_DEGRADED=1
fi
if [ "$_BACKUP_DEGRADED" -eq 1 ]; then
  write_metrics 1 0 1
  echo "=== БЭКАП ЗАВЕРШЁН В DEGRADED LEGACY-MIGRATION MODE: историческая полнота не подтверждена ==="
else
  write_metrics 1
  echo "=== БЭКАП ЗАВЕРШЁН: ${BACKUP_NAME}.tar.gpg → ${RCLONE_REMOTE} ==="
fi
