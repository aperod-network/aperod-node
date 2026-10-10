#!/usr/bin/env bash
# =============================================================================
#  node-resync.sh — Safely resync a relay or validator node's chain data from
#                   a running source validator.
#
#  The script stops the SOURCE, rsyncs chain.db + snapshots to TARGET,
#  fixes permissions, restarts TARGET, then restarts SOURCE.
#
#  Run on the SOURCE server (the one that has the authoritative chain data):
#
#    sudo bash node-resync.sh \
#        --source /opt/aperod/data/testnet \
#        --target root@77.221.153.86:/var/lib/aperod
#
#  Or run from any host that has SSH access to both:
#    SOURCE_HOST=root@source-ip
#    TARGET_HOST=root@target-ip
#    (use --source-host / --target-host flags)
#
#  What it does:
#    1. Stops aperod-node on SOURCE (or SOURCE_HOST via SSH)
#    2. rsyncs chain.db + snapshot-v2-*.json.gz to TARGET
#    3. On TARGET: removes root-owned p2p_whitelist.json / p2p_bans.json
#       (stale sidecar files would block P2P startup)
#    4. Restarts aperod-node on TARGET
#    5. Restarts aperod-node on SOURCE (unless --keep-source-down)
#    6. Sends Telegram alerts on success and failure
#
#  Requirements:
#    - SSH key-based access (no password prompts) when using --source-host /
#      --target-host
#    - rsync and ssh available on the machine running this script
#    - aperod-node.service installed and named identically on both hosts
#
#  Optional env vars:
#    SOURCE_SERVICE       service name on source (default: aperod-node)
#    TARGET_SERVICE       service name on target (default: aperod-node)
#    RSYNC_BW_LIMIT       bandwidth limit in KB/s (default: 0 = unlimited)
#    SUPPORT_BOT_TOKEN    Telegram bot token for alerts
#    SUPPORT_ADMIN_CHAT_ID Telegram chat ID for alerts
#    SSH_KEY              path to SSH private key (default: ~/.ssh/id_ed25519)
# =============================================================================
set -euo pipefail

SOURCE_DIR=""
TARGET=""
SOURCE_HOST=""       # empty = local
TARGET_HOST=""       # empty = target is already included in TARGET path
KEEP_SOURCE_DOWN=0
DRY_RUN=0
SOURCE_SERVICE="${SOURCE_SERVICE:-aperod-node}"
TARGET_SERVICE="${TARGET_SERVICE:-aperod-node}"
RSYNC_BW_LIMIT="${RSYNC_BW_LIMIT:-0}"
SSH_KEY="${SSH_KEY:-${HOME}/.ssh/id_ed25519}"

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
  case "$1" in
    --source)           SOURCE_DIR="$2";   shift 2 ;;
    --target)           TARGET="$2";       shift 2 ;;
    --source-host)      SOURCE_HOST="$2";  shift 2 ;;
    --keep-source-down) KEEP_SOURCE_DOWN=1; shift ;;
    --dry-run)          DRY_RUN=1;         shift ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "${SOURCE_DIR}" || -z "${TARGET}" ]]; then
  echo "Usage: $0 --source <data-dir> --target <[user@host:]path>" >&2
  echo "  e.g. $0 --source /opt/aperod/data/testnet --target root@77.221.153.86:/var/lib/aperod" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [resync] $*"; }

send_telegram() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      >/dev/null 2>&1 || true
  fi
}

ssh_run() {
  local host="$1"; shift
  if [[ -f "${SSH_KEY}" ]]; then
    ssh -i "${SSH_KEY}" -o StrictHostKeyChecking=yes \
        -o UserKnownHostsFile="${KNOWN_HOSTS_FILE:-/etc/aperod/validator_known_hosts}" \
        "${host}" "$@"
  else
    ssh "${host}" "$@"
  fi
}

service_ctl() {
  local host="$1" action="$2" service="$3"
  if [[ -z "${host}" ]]; then
    systemctl "${action}" "${service}"
  else
    ssh_run "${host}" systemctl "${action}" "${service}"
  fi
}

START_TIME=$(date +%s)

log "=== Aperod node-resync starting ==="
log "  source dir : ${SOURCE_DIR}"
log "  source host: ${SOURCE_HOST:-local}"
log "  target     : ${TARGET}"

# ---------------------------------------------------------------------------
# 1. Stop aperod-node on SOURCE
# ---------------------------------------------------------------------------
log "Step 1/5 — stopping ${SOURCE_SERVICE} on source..."
if [[ "${DRY_RUN}" -eq 1 ]]; then
  log "[dry-run] would run: systemctl stop ${SOURCE_SERVICE}"
else
  service_ctl "${SOURCE_HOST}" stop "${SOURCE_SERVICE}"
  log "${SOURCE_SERVICE} stopped on source"
fi

RESYNC_OK=0
# Use a subshell so we can trap errors and still restart the source.
{
  # -------------------------------------------------------------------------
  # 2. rsync chain.db + snapshots to TARGET
  # -------------------------------------------------------------------------
  log "Step 2/5 — rsyncing chain.db and snapshots to ${TARGET}..."

  RSYNC_OPTS=(
    -az
    --progress
    --delete-after
  )
  if [[ "${RSYNC_BW_LIMIT}" -gt 0 ]]; then
    RSYNC_OPTS+=("--bwlimit=${RSYNC_BW_LIMIT}")
  fi
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    RSYNC_OPTS+=(--dry-run)
  fi
  if [[ -n "${SOURCE_HOST}" ]]; then
    RSYNC_SRC="${SOURCE_HOST}:${SOURCE_DIR}"
  else
    RSYNC_SRC="${SOURCE_DIR}"
  fi

  # chain.db (LevelDB directory)
  log "  rsyncing chain.db..."
  rsync "${RSYNC_OPTS[@]}" \
    --include="chain.db/" \
    --include="chain.db/**" \
    --exclude="*" \
    "${RSYNC_SRC}/" \
    "${TARGET}/"

  # snapshots (glob-style via find + --files-from)
  log "  rsyncing snapshots..."
  if [[ -n "${SOURCE_HOST}" ]]; then
    SNAP_LIST=$(ssh_run "${SOURCE_HOST}" \
      "find ${SOURCE_DIR} -maxdepth 1 -name 'snapshot-v2-*.json.gz' -printf '%f\n' 2>/dev/null || true")
  else
    SNAP_LIST=$(find "${SOURCE_DIR}" -maxdepth 1 -name "snapshot-v2-*.json.gz" \
                -printf '%f\n' 2>/dev/null || true)
  fi

  if [[ -n "${SNAP_LIST}" ]]; then
    TMP_FILES=$(mktemp)
    echo "${SNAP_LIST}" > "${TMP_FILES}"
    rsync "${RSYNC_OPTS[@]}" \
      --files-from="${TMP_FILES}" \
      "${RSYNC_SRC}/" \
      "${TARGET}/"
    rm -f "${TMP_FILES}"
    log "  snapshots synced: $(echo "${SNAP_LIST}" | wc -l | tr -d ' ') file(s)"
  else
    log "  no snapshot files found — skipping (node will do a full UTXO scan on startup)"
  fi

  log "rsync complete"

  # -------------------------------------------------------------------------
  # 3. Fix permissions on TARGET
  # -------------------------------------------------------------------------
  log "Step 3/5 — fixing permissions on target..."
  # Extract target host and path from TARGET (format: [user@host:]path)
  if [[ "${TARGET}" == *:* ]]; then
    TGT_HOST="${TARGET%%:*}"
    TGT_PATH="${TARGET##*:}"
  else
    TGT_HOST=""
    TGT_PATH="${TARGET}"
  fi

  FIX_CMD="
    rm -f '${TGT_PATH}/p2p_whitelist.json' '${TGT_PATH}/p2p_bans.json' 2>/dev/null || true
    echo 'Stale p2p sidecar files removed (if any)'
  "
  if [[ -n "${TGT_HOST}" ]]; then
    ssh_run "${TGT_HOST}" bash -c "${FIX_CMD}"
  else
    eval "${FIX_CMD}"
  fi
  log "permissions fixed"

  # -------------------------------------------------------------------------
  # 4. Restart TARGET
  # -------------------------------------------------------------------------
  log "Step 4/5 — starting ${TARGET_SERVICE} on target..."
  if [[ "${DRY_RUN}" -eq 1 ]]; then
    log "[dry-run] would run: systemctl start ${TARGET_SERVICE}"
  else
    if [[ -n "${TGT_HOST}" ]]; then
      ssh_run "${TGT_HOST}" systemctl restart "${TARGET_SERVICE}"
    else
      systemctl restart "${TARGET_SERVICE}"
    fi
    log "${TARGET_SERVICE} restarted on target"
  fi

  RESYNC_OK=1
}

# -------------------------------------------------------------------------
# 5. Restart SOURCE (always — even if resync failed)
# -------------------------------------------------------------------------
log "Step 5/5 — restarting ${SOURCE_SERVICE} on source..."
if [[ "${KEEP_SOURCE_DOWN}" -eq 1 ]]; then
  log "  --keep-source-down set — skipping source restart"
elif [[ "${DRY_RUN}" -eq 1 ]]; then
  log "  [dry-run] would run: systemctl start ${SOURCE_SERVICE}"
else
  service_ctl "${SOURCE_HOST}" start "${SOURCE_SERVICE}" || true
  log "${SOURCE_SERVICE} restarted on source"
fi

END_TIME=$(date +%s)
DURATION=$(( END_TIME - START_TIME ))

if [[ "${RESYNC_OK}" -eq 1 ]]; then
  log "=== Resync complete in ${DURATION}s ==="
  send_telegram "✅ <b>aperod node-resync complete</b>
Source: ${SOURCE_HOST:-local}:${SOURCE_DIR}
Target: ${TARGET}
Duration: ${DURATION}s"
else
  log "=== Resync FAILED after ${DURATION}s — source has been restarted ==="
  send_telegram "❌ <b>aperod node-resync FAILED</b>
Source: ${SOURCE_HOST:-local}:${SOURCE_DIR}
Target: ${TARGET}
Duration: ${DURATION}s

Source node was restarted regardless. Check target manually."
  exit 1
fi
