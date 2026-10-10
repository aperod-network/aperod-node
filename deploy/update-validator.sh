#!/usr/bin/env bash
# update-validator.sh — Push a freshly built aperod-node binary to every
#                       validator server listed in validators.conf (or passed
#                       as command-line arguments), then restart the service
#                       and verify the node comes back up.
#
# ---------------------------------------------------------------------------
# BACKGROUND
# ---------------------------------------------------------------------------
# The main node and all validator nodes MUST run the same binary version.
# When the main node gains a new feature (e.g. TLS P2P encryption), validators
# running an older binary fail to complete the handshake and stay at peer_count=0
# for hours without any obvious error.
#
# This script automates the distribution step so it is never skipped.
#
# ---------------------------------------------------------------------------
# TYPICAL WORKFLOW (run on the main server as root)
# ---------------------------------------------------------------------------
#   # 1. Update the main node first (builds the binary, runs health checks):
#   sudo bash /opt/aperod/blockchain/deploy/update-node.sh
#
#   # 2. Push the same binary to all validators:
#   sudo bash /opt/aperod/blockchain/deploy/update-validator.sh
#
#   # Or push to a single validator by passing its SSH target:
#   sudo bash /opt/aperod/blockchain/deploy/update-validator.sh aperod@203.0.113.10
#
# ---------------------------------------------------------------------------
# HOW IT WORKS
# ---------------------------------------------------------------------------
# For each validator the script:
#   1. SCPs the pre-built binary from the main server to the validator.
#   2. Stops the aperod-node service on the validator (via ssh + systemctl).
#   3. Installs the binary to /usr/local/bin/aperod-node.
#   4. Starts the service.
#   5. Polls /api/v1/status (on 127.0.0.1:8545, through the SSH tunnel) until
#      the node responds — or fires a Telegram alert on failure.
#
# The binary is NOT rebuilt by this script.  Run update-node.sh first to
# ensure /usr/local/bin/aperod-node on the main server is up-to-date, then
# run this script to push it out.
#
# ---------------------------------------------------------------------------
# REQUIREMENTS
# ---------------------------------------------------------------------------
#   - SSH key-based access to every validator (no password prompts).
#     Default key: ~/.ssh/id_ed25519  (override with SSH_KEY env var).
#   - The aperod user on each validator has sudo rights for systemctl.
#   - Port 22 reachable from the main server to every validator.
#   - Every validator host key MUST be in the known-hosts file BEFORE
#     running this script (see "First-time host-key verification" below).
#
# ---------------------------------------------------------------------------
# FIRST-TIME HOST-KEY VERIFICATION (run once per new validator)
# ---------------------------------------------------------------------------
# This script uses StrictHostKeyChecking=yes — it will REFUSE to connect to
# any host whose key is not already recorded.  This prevents MITM attacks
# where a bad actor intercepts the update and receives the binary instead.
#
# Add a validator's host key to the deploy known-hosts file exactly once,
# AFTER you have independently verified the key fingerprint (e.g. via your
# cloud provider's console or an out-of-band call):
#
#   # 1. Fetch and display the host key fingerprint:
#   ssh-keyscan -H <validator-ip> 2>/dev/null | ssh-keygen -lf - -E sha256
#
#   # 2. Verify the fingerprint matches what your provider shows, then add it:
#   ssh-keyscan -H <validator-ip> >> /etc/aperod/validator_known_hosts
#   chmod 600 /etc/aperod/validator_known_hosts
#
# The default known-hosts file is /etc/aperod/validator_known_hosts.
# Override with the KNOWN_HOSTS_FILE env var.
#
# If a validator's host key changes (e.g. server was reprovisioned), the
# script will abort with a clear error.  Remove the stale entry and add the
# new fingerprint after out-of-band verification:
#   ssh-keygen -R <validator-ip> -f /etc/aperod/validator_known_hosts
#   ssh-keyscan -H <validator-ip> >> /etc/aperod/validator_known_hosts
#
# ---------------------------------------------------------------------------
# OPTIONAL ENV VARS
# ---------------------------------------------------------------------------
#   VALIDATORS_CONF      — path to validators.conf         (default: same dir as script)
#   SSH_KEY              — path to SSH private key          (default: ~/.ssh/id_ed25519)
#   KNOWN_HOSTS_FILE     — path to deploy known-hosts file  (default: /etc/aperod/validator_known_hosts)
#   SSH_USER             — SSH login user                   (default: aperod; overridden
#                          per-host if the conf entry contains user@host)
#   BINARY_SRC           — local binary to push             (default: /usr/local/bin/aperod-node)
#   BINARY_DST           — remote destination path          (default: /usr/local/bin/aperod-node)
#   SERVICE_NAME         — systemd service name             (default: aperod-node)
#   HEALTH_MAX_ATTEMPTS  — health-poll attempts             (default: 15)
#   HEALTH_WAIT_SECS     — seconds between polls            (default: 2)
#   SKIP_HEALTH_CHECK    — set to 1 to skip health check
#   SUPPORT_BOT_TOKEN    — Telegram bot token for alerts
#   SUPPORT_ADMIN_CHAT_ID— Telegram chat ID for alerts
#
# ---------------------------------------------------------------------------
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ---------------------------------------------------------------------------
# Keep /usr/local/bin/aperod_backup.sh in sync with the repo on this server.
# Logic is identical to the step in update-node.sh — see sync-backup-script.sh.
# ---------------------------------------------------------------------------
# shellcheck source=sync-backup-script.sh
source "${DEPLOY_DIR}/sync-backup-script.sh"

VALIDATORS_CONF="${VALIDATORS_CONF:-${DEPLOY_DIR}/validators.conf}"
SSH_KEY="${SSH_KEY:-${HOME}/.ssh/id_ed25519}"
KNOWN_HOSTS_FILE="${KNOWN_HOSTS_FILE:-/etc/aperod/validator_known_hosts}"
SSH_USER="${SSH_USER:-aperod}"
BINARY_SRC="${BINARY_SRC:-/usr/local/bin/aperod-node}"
BINARY_DST="${BINARY_DST:-/usr/local/bin/aperod-node}"
SERVICE_NAME="${SERVICE_NAME:-aperod-node}"

HEALTH_MAX_ATTEMPTS="${HEALTH_MAX_ATTEMPTS:-15}"
HEALTH_WAIT_SECS="${HEALTH_WAIT_SECS:-2}"
SKIP_HEALTH_CHECK="${SKIP_HEALTH_CHECK:-0}"

# ---------------------------------------------------------------------------
# Helper: send a Telegram message if credentials are configured.
# ---------------------------------------------------------------------------
send_telegram_alert() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      > /dev/null 2>&1 || true   # never let a failed alert abort the script
  fi
}

# ---------------------------------------------------------------------------
# Verify the known-hosts file exists before attempting any connections.
#
# StrictHostKeyChecking=yes means SSH will refuse any host whose fingerprint
# is not already recorded — this prevents MITM attacks where an attacker
# intercepts the update.  The operator must add each validator's key once
# (see the "First-time host-key verification" instructions at the top of this
# file) before running the script.
# ---------------------------------------------------------------------------
if [[ ! -f "${KNOWN_HOSTS_FILE}" ]]; then
  echo "✗ Known-hosts file not found: ${KNOWN_HOSTS_FILE}" >&2
  echo "" >&2
  echo "  You must add each validator's SSH host key before the first run." >&2
  echo "  For each validator, after verifying the fingerprint out-of-band:" >&2
  echo "    sudo mkdir -p /etc/aperod" >&2
  echo "    ssh-keyscan -H <validator-ip> >> ${KNOWN_HOSTS_FILE}" >&2
  echo "    sudo chmod 600 ${KNOWN_HOSTS_FILE}" >&2
  echo "" >&2
  echo "  Override the file path with: KNOWN_HOSTS_FILE=<path> $0 ..." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Shared SSH options.
#
# StrictHostKeyChecking=yes + UserKnownHostsFile: only connect to hosts whose
# fingerprint has been pre-verified and recorded in KNOWN_HOSTS_FILE.  If a
# host key has changed (reprovisioned server), SSH aborts with a clear error
# so the operator can investigate before proceeding.
#
# BatchMode=yes: fail immediately instead of hanging on a password prompt.
# ---------------------------------------------------------------------------
SSH_OPTS=(
  -i "${SSH_KEY}"
  -o StrictHostKeyChecking=yes
  -o UserKnownHostsFile="${KNOWN_HOSTS_FILE}"
  -o BatchMode=yes
  -o ConnectTimeout=10
)

# ---------------------------------------------------------------------------
# Collect the list of validators to update.
#
# Priority order:
#   1. Command-line arguments (e.g. aperod@1.2.3.4  or  1.2.3.4)
#   2. VALIDATORS_CONF file (one user@host per line, # = comment)
# ---------------------------------------------------------------------------
VALIDATORS=()

if [[ $# -gt 0 ]]; then
  for arg in "$@"; do
    VALIDATORS+=("$arg")
  done
  echo "==> Using validators from command-line arguments: ${VALIDATORS[*]}"
else
  if [[ ! -f "${VALIDATORS_CONF}" ]]; then
    echo "✗ No validators specified and conf file not found: ${VALIDATORS_CONF}" >&2
    echo "  Usage:" >&2
    echo "    $0 aperod@<ip1> aperod@<ip2>" >&2
    echo "    OR populate ${VALIDATORS_CONF} with one user@host per line." >&2
    exit 1
  fi

  while IFS= read -r line; do
    # Strip leading/trailing whitespace
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    # Skip blank lines and comments
    [[ -z "$line" || "$line" == \#* ]] && continue
    VALIDATORS+=("$line")
  done < "${VALIDATORS_CONF}"

  if [[ ${#VALIDATORS[@]} -eq 0 ]]; then
    echo "✗ No validators found in ${VALIDATORS_CONF}." >&2
    echo "  Add entries in the format  aperod@<ip>  (one per line)." >&2
    exit 1
  fi

  echo "==> Loaded ${#VALIDATORS[@]} validator(s) from ${VALIDATORS_CONF}"
fi

# ---------------------------------------------------------------------------
# Verify that the binary to push exists on this (main) server.
# ---------------------------------------------------------------------------
if [[ ! -f "${BINARY_SRC}" ]]; then
  echo "✗ Binary not found at ${BINARY_SRC}" >&2
  echo "  Run update-node.sh first to build and install the binary on this server." >&2
  exit 1
fi

echo "==> Binary to push: ${BINARY_SRC}"
echo "    $(ls -lh "${BINARY_SRC}" | awk '{print $5, $9}')"
echo ""

# ---------------------------------------------------------------------------
# Sync aperod_backup.sh on this (main) server.
#
# setup-backup.sh installs /usr/local/bin/aperod_backup.sh once and never
# updates it again.  When git pull brings in changes to the backup script the
# running installed copy would silently stay stale.  This step atomically
# replaces it (stage + rename(2)) so the next scheduled backup always runs
# the current code.  Non-fatal: skipped when backup is not configured.
# ---------------------------------------------------------------------------
echo "==> Syncing aperod_backup.sh on this server..."
_sync_backup_script /usr/local/bin/aperod_backup.sh "${DEPLOY_DIR}/aperod_backup.sh"
echo ""

# ---------------------------------------------------------------------------
# Process each validator.
# ---------------------------------------------------------------------------
FAILED=()

for TARGET in "${VALIDATORS[@]}"; do
  # Normalise: if no '@' in target, prepend the default SSH user.
  if [[ "${TARGET}" != *@* ]]; then
    TARGET="${SSH_USER}@${TARGET}"
  fi

  HOST="${TARGET#*@}"   # everything after the last @
  USER="${TARGET%%@*}"  # everything before the @

  echo "========================================"
  echo "==> Updating validator: ${TARGET}"
  echo "========================================"

  # ── Step 1: SCP the binary to a temp path on the validator ──────────────
  # We use a temporary path so we never replace a running binary in-place;
  # the remote install step (Step 3) does the atomic swap.
  REMOTE_TMP="/tmp/aperod-node-new"

  echo "  [1/5] Copying binary to ${TARGET}:${REMOTE_TMP}..."
  if ! scp "${SSH_OPTS[@]}" "${BINARY_SRC}" "${TARGET}:${REMOTE_TMP}"; then
    echo "  ✗ SCP failed for ${TARGET}" >&2
    send_telegram_alert "❌ <b>update-validator failed — SCP error</b>
Validator: <code>${TARGET}</code>
Could not copy binary. Check SSH key and network connectivity."
    FAILED+=("${TARGET}")
    continue
  fi

  # ── Step 1b: Push aperod_backup.sh to the validator (non-fatal) ──────────
  # Only copies if the file already exists on the remote (i.e. setup-backup.sh
  # was previously run on that validator).  A missing installed copy or a
  # missing repo file is silently skipped so this step never aborts the update.
  BACKUP_SH_SRC="${DEPLOY_DIR}/aperod_backup.sh"
  REMOTE_BACKUP_TMP="/tmp/aperod_backup_new.sh"
  BACKUP_SH_SENT=0
  if [[ -f "${BACKUP_SH_SRC}" ]]; then
    if scp "${SSH_OPTS[@]}" "${BACKUP_SH_SRC}" "${TARGET}:${REMOTE_BACKUP_TMP}" 2>/dev/null; then
      BACKUP_SH_SENT=1
      echo "  [1b] aperod_backup.sh staged on ${TARGET}."
    else
      echo "  [warn] aperod_backup.sh SCP failed for ${TARGET} — skipping backup sync." >&2
    fi
  fi

  # ── Steps 2–5: Remote: stop → install → start → health check ────────────
  REMOTE_SCRIPT=$(cat <<'REMOTE_EOF'
set -euo pipefail

BINARY_SRC="/tmp/aperod-node-new"
BINARY_DST="__BINARY_DST__"
SERVICE_NAME="__SERVICE_NAME__"
HEALTH_MAX_ATTEMPTS="__HEALTH_MAX_ATTEMPTS__"
HEALTH_WAIT_SECS="__HEALTH_WAIT_SECS__"
SKIP_HEALTH_CHECK="__SKIP_HEALTH_CHECK__"
BACKUP_SH_SENT="__BACKUP_SH_SENT__"
HEALTH_URL="http://127.0.0.1:8545/api/v1/status"
BACKUP_INSTALLED="/usr/local/bin/aperod_backup.sh"
BACKUP_TMP="/tmp/aperod_backup_new.sh"

echo "  [2/5] Stopping ${SERVICE_NAME}..."
sudo systemctl stop "${SERVICE_NAME}" || true
sleep 1

echo "  [3/5] Installing binary to ${BINARY_DST}..."
sudo cp "${BINARY_SRC}" "${BINARY_DST}"
sudo chmod +x "${BINARY_DST}"
echo "    Installed: $(${BINARY_DST} --version 2>/dev/null || ls -lh "${BINARY_DST}" | awk '{print $5, $9}')"
rm -f "${BINARY_SRC}"

# ── Step 3b: Install aperod_backup.sh if backup is configured ────────────
# Only replaces the installed copy when the script was sent (BACKUP_SH_SENT=1)
# AND backup is already configured on this validator (/usr/local/bin/aperod_backup.sh
# exists).  This mirrors the _sync_backup_script non-fatal contract.
#
# Atomicity: stage into the same directory as the installed copy (/usr/local/bin),
# set permissions, then rename(2) via mv -f.  A cron/systemd backup job that starts
# concurrently will see either the complete old copy or the complete new copy, never
# a partially written file — exactly the same guarantee as _sync_backup_script on
# the main server.
if [[ "${BACKUP_SH_SENT}" == "1" ]] && [[ -f "${BACKUP_INSTALLED}" ]]; then
  if [[ -f "${BACKUP_TMP}" ]]; then
    INSTALL_DIR="$(dirname "${BACKUP_INSTALLED}")"
    BACKUP_STAGE="$(sudo mktemp "${INSTALL_DIR}/.aperod_backup_sync.XXXXXXXX" 2>/dev/null)" || BACKUP_STAGE=""
    if [[ -n "${BACKUP_STAGE}" ]]; then
      sudo cp "${BACKUP_TMP}" "${BACKUP_STAGE}" \
        && sudo chmod 700 "${BACKUP_STAGE}" \
        && sudo mv -f "${BACKUP_STAGE}" "${BACKUP_INSTALLED}" \
        && echo "  [3b] aperod_backup.sh updated on this validator (atomic rename)." \
        || { sudo rm -f "${BACKUP_STAGE}" 2>/dev/null || true
             echo "  [warn] aperod_backup.sh atomic rename failed — keeping old copy." >&2; }
    else
      echo "  [warn] aperod_backup.sh sync skipped — cannot create staging file in ${INSTALL_DIR}" >&2
    fi
    rm -f "${BACKUP_TMP}"
  fi
elif [[ -f "${BACKUP_TMP}" ]]; then
  rm -f "${BACKUP_TMP}"   # clean up temp file when backup is not configured
fi

echo "  [4/5] Starting ${SERVICE_NAME}..."
sudo systemctl start "${SERVICE_NAME}"

echo "  [5/5] Health check (polling ${HEALTH_URL})..."
if [[ "${SKIP_HEALTH_CHECK}" == "1" ]]; then
  echo "    SKIP_HEALTH_CHECK=1 — skipping."
  exit 0
fi

attempt=0
healthy=false
while [[ $attempt -lt $HEALTH_MAX_ATTEMPTS ]]; do
  attempt=$(( attempt + 1 ))
  if curl -sf --connect-timeout 2 "${HEALTH_URL}" > /dev/null 2>&1; then
    healthy=true
    break
  fi
  echo "    Waiting... (attempt ${attempt}/${HEALTH_MAX_ATTEMPTS})"
  sleep "${HEALTH_WAIT_SECS}"
done

if [[ "${healthy}" == "true" ]]; then
  echo "    ✓ Node API is responding."
else
  echo "    ✗ Health check failed — node did not come up." >&2
  echo "      Inspect logs: journalctl -u ${SERVICE_NAME} -n 100 --no-pager" >&2
  exit 1
fi
REMOTE_EOF
  )

  # Substitute the configuration values into the remote script.
  REMOTE_SCRIPT="${REMOTE_SCRIPT//__BINARY_DST__/${BINARY_DST}}"
  REMOTE_SCRIPT="${REMOTE_SCRIPT//__SERVICE_NAME__/${SERVICE_NAME}}"
  REMOTE_SCRIPT="${REMOTE_SCRIPT//__HEALTH_MAX_ATTEMPTS__/${HEALTH_MAX_ATTEMPTS}}"
  REMOTE_SCRIPT="${REMOTE_SCRIPT//__HEALTH_WAIT_SECS__/${HEALTH_WAIT_SECS}}"
  REMOTE_SCRIPT="${REMOTE_SCRIPT//__SKIP_HEALTH_CHECK__/${SKIP_HEALTH_CHECK}}"
  REMOTE_SCRIPT="${REMOTE_SCRIPT//__BACKUP_SH_SENT__/${BACKUP_SH_SENT}}"

  if ! ssh "${SSH_OPTS[@]}" "${TARGET}" "bash -s" <<< "${REMOTE_SCRIPT}"; then
    echo "  ✗ Remote update failed on ${TARGET}" >&2
    send_telegram_alert "❌ <b>update-validator failed — remote error</b>
Validator: <code>${TARGET}</code>
Binary was copied but service did not restart or health check failed.
Check: <code>ssh ${TARGET} journalctl -u ${SERVICE_NAME} -n 100</code>"
    FAILED+=("${TARGET}")
    continue
  fi

  echo "  ✓ ${TARGET} updated successfully."
  echo ""
done

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
TOTAL=${#VALIDATORS[@]}
FAIL_COUNT=${#FAILED[@]}
OK_COUNT=$(( TOTAL - FAIL_COUNT ))

echo "========================================"
echo "==> Done: ${OK_COUNT}/${TOTAL} validators updated."
if [[ ${FAIL_COUNT} -gt 0 ]]; then
  echo ""
  echo "  The following validators failed:"
  for f in "${FAILED[@]}"; do
    echo "    ✗ ${f}"
  done
  echo ""
  echo "  For each failed validator, investigate manually:"
  echo "    ssh -i ${SSH_KEY} -o UserKnownHostsFile=${KNOWN_HOSTS_FILE} <validator>  'journalctl -u ${SERVICE_NAME} -n 100 --no-pager'"
  echo ""

  send_telegram_alert "⚠️ <b>update-validator: ${FAIL_COUNT} validator(s) FAILED</b>
Failed: $(IFS=', '; echo "${FAILED[*]}")
Check journalctl on each server."

  exit 1
fi
echo ""
echo "✓ All validators are running the new binary."
echo ""
echo "  To verify a validator:"
echo "    ssh <validator>  'curl -s http://127.0.0.1:8545/api/v1/status | python3 -m json.tool'"
echo "    ssh <validator>  'journalctl -u ${SERVICE_NAME} -n 50 --no-pager'"
