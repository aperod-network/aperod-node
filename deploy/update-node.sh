#!/usr/bin/env bash
# update-node.sh — Pull latest source, rebuild the Go node binary, and restart
#                  the aperod-node systemd service.
#
# Run this script instead of manually building and copying the binary after
# every code change. The two most common mistakes it prevents:
#
#   1. Building to the wrong path (e.g. /opt/aperod/data/aperod-node instead of
#      /usr/local/bin/aperod-node) — the service keeps running the OLD binary
#      silently, and the update appears to succeed.
#
#   2. Copying over a running binary — causes "Text file busy" (ETXTBSY).
#      This script always stops the service BEFORE installing the new binary.
#
# Usage:
#   sudo bash /opt/aperod/blockchain/deploy/update-node.sh
#
# The script must be run as root (or via sudo) so it can:
#   - call systemctl stop/start
#   - write to /usr/local/bin/
#   - switch to the `aperod` user for git and Go operations
#
# Optional env vars for Telegram build-failure alerts:
#   SUPPORT_BOT_TOKEN      — Telegram bot token (e.g. 123456:ABC-xxx)
#   SUPPORT_ADMIN_CHAT_ID  — Telegram chat/user ID to receive the alert
#
# ---------------------------------------------------------------------------
# HEALTH CHECK
# ---------------------------------------------------------------------------
# After starting the service the script polls localhost:8545/api/v1/status
# up to HEALTH_MAX_ATTEMPTS times (default: 15), waiting HEALTH_WAIT_SECS
# (default: 2) between each attempt.  If the endpoint never responds the
# script exits non-zero and sends a Telegram alert.
#
# Configurable env vars:
#   HEALTH_MAX_ATTEMPTS  — poll attempts before giving up  (default: 15)
#   HEALTH_WAIT_SECS     — seconds between each poll       (default: 2)
#   SKIP_HEALTH_CHECK    — set to 1 to bypass the check entirely
#
# ---------------------------------------------------------------------------
# PEER CONNECTIVITY CHECK (Step 5b)
# ---------------------------------------------------------------------------
# After the HTTP health check passes, the script polls /api/v1/network/stats
# for up to PEER_WAIT_SECS (default: 30 s).  If peer_count is still 0 at the
# end it emits a visible warning and sends a Telegram alert.  The check is
# NON-FATAL — it never aborts the deploy.
#
# This catches P2P regressions (bad TLS config, changed protocol version, etc.)
# that would pass the HTTP health check but leave the node network-isolated.
#
# Configurable env vars:
#   PEER_WAIT_SECS   — seconds to wait for at least one peer (default: 30)
#   SKIP_PEER_CHECK  — set to 1 to bypass (e.g. single-node dev setup)
#
set -euo pipefail

APEROD_DIR="/opt/aperod"
BLOCKCHAIN_DIR="${APEROD_DIR}/blockchain"
SERVICE_NAME="aperod-node"
BINARY_DST="/usr/local/bin/aperod-node"
BINARY_SRC="${BLOCKCHAIN_DIR}/build/aperod-node"
HEALTH_URL="http://localhost:8545/api/v1/status"
STATS_URL="http://localhost:8545/api/v1/network/stats"
DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Health-check tunables (override via environment)
HEALTH_MAX_ATTEMPTS="${HEALTH_MAX_ATTEMPTS:-15}"
HEALTH_WAIT_SECS="${HEALTH_WAIT_SECS:-2}"
SKIP_HEALTH_CHECK="${SKIP_HEALTH_CHECK:-0}"

# Peer-check tunables (override via environment)
# PEER_WAIT_SECS: how long to wait for at least one peer before warning (default 30 s)
# SKIP_PEER_CHECK: set to 1 to bypass the peer check (e.g. single-node dev setup)
PEER_WAIT_SECS="${PEER_WAIT_SECS:-30}"
SKIP_PEER_CHECK="${SKIP_PEER_CHECK:-0}"

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
# Step 0: Ensure the service file forwards logs to journald.
#
# Older installs generated the service without StandardOutput/StandardError,
# so `journalctl -u aperod-node` showed only lifecycle events and nothing
# from the application itself.  This step patches any existing service file
# where a directive is absent OR set to a non-journal value.
#
# Each directive is checked and patched independently:
#   - If absent:              insert it after the anchor line.
#   - If present but wrong:   replace the existing line in-place.
#   - If already correct:     leave it untouched.
#
# The anchor for insertions is the first ExecStart= line inside [Service],
# which is always present.  This guarantees a valid insertion point regardless
# of what other directives the file has.
# ---------------------------------------------------------------------------
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

# Helper: ensure a Key=Value directive exists in the service file with exactly
# the required value.  If the key is absent it is appended after the anchor
# line; if it is present with the wrong value it is replaced in-place.
# Usage: _ensure_directive KEY VALUE ANCHOR_PATTERN
_ensure_directive() {
  local key="$1" value="$2" anchor="$3"
  local required="${key}=${value}"
  if grep -q "^${key}=" "${SERVICE_FILE}"; then
    if ! grep -qF "^${required}" "${SERVICE_FILE}"; then
      # Present but wrong value — replace it.
      sed -i "s|^${key}=.*|${required}|" "${SERVICE_FILE}"
      echo "  [patch] Updated ${key} → ${value}"
      return 0
    fi
    # Already correct.
    return 1
  else
    # Absent — insert after the anchor line.
    sed -i "/^${anchor}/a ${required}" "${SERVICE_FILE}"
    echo "  [patch] Inserted ${required}"
    return 0
  fi
}

# ---------------------------------------------------------------------------
# Step 0b: One-time validator.key ownership fix.
#
# Validators installed before July 2026 may have /etc/aperod/validator.key
# owned by root instead of aperod.  The aperod-node service runs as
# User=aperod, so a root-owned key causes "permission denied" on startup.
# Newer binaries also reject any mode wider than 600 ("unsafe permissions").
#
# This block silently corrects both issues on every update so that operators
# do not have to remember a one-time manual step.
# ---------------------------------------------------------------------------
VALIDATOR_KEY="/etc/aperod/validator.key"
if [[ -f "${VALIDATOR_KEY}" ]]; then
  key_owner=$(stat -c '%U' "${VALIDATOR_KEY}" 2>/dev/null || true)
  key_mode=$(stat -c '%a'  "${VALIDATOR_KEY}" 2>/dev/null || true)
  needs_fix=false
  [[ "${key_owner}" != "aperod" ]] && needs_fix=true
  [[ "${key_mode}"  != "600"    ]] && needs_fix=true
  if [[ "${needs_fix}" == "true" ]]; then
    echo "  [fix] ${VALIDATOR_KEY}: owner=${key_owner} mode=${key_mode} → fixing to aperod:aperod 600"
    chown aperod:aperod "${VALIDATOR_KEY}"
    chmod 600           "${VALIDATOR_KEY}"
    echo "  [fix] validator.key ownership corrected (one-time fix for pre-July-2026 installs)."
  else
    echo "  [ok] ${VALIDATOR_KEY}: ownership and permissions are correct (aperod:aperod 600)."
  fi
fi

if [[ -f "${SERVICE_FILE}" ]]; then
  patched=false

  _ensure_directive "StandardOutput" "journal"  "ExecStart=" && patched=true
  _ensure_directive "StandardError"  "journal"  "ExecStart=" && patched=true
  _ensure_directive "SyslogIdentifier" "aperod-node" "ExecStart=" && patched=true

  if [[ "${patched}" == "true" ]]; then
    systemctl daemon-reload
    echo "  [patch] daemon-reload complete — new log settings take effect after restart."
  else
    echo "  [ok] Service file already has correct journal directives — no patch needed."
  fi
else
  echo "  [warn] ${SERVICE_FILE} not found — skipping journal directive check."
fi

# Ensure log directory exists with correct ownership (used if an operator
# sets up a file sink via ExecStart wrapper or logrotate).
if [[ ! -d /var/log/aperod ]]; then
  mkdir -p /var/log/aperod
  chown aperod:adm /var/log/aperod 2>/dev/null || chown aperod:aperod /var/log/aperod
  chmod 750 /var/log/aperod
  echo "  [ok] Created /var/log/aperod/ for optional persistent log sink."
fi

# ---------------------------------------------------------------------------
# Pre-flight: Refuse to run on a server where the node has never been
# installed.  If neither the target binary nor the service unit file exists
# this is almost certainly a fresh machine where install-node.sh should be
# run first.  Proceeding would stop a non-existent service, build a new
# binary with no backup to roll back to, and silently leave the service
# offline if anything goes wrong.
# ---------------------------------------------------------------------------
if [[ ! -f "${BINARY_DST}" && ! -f "${SERVICE_FILE}" ]]; then
  echo "" >&2
  echo "✗ Fresh-server detected — aborting." >&2
  echo "  No binary found at    : ${BINARY_DST}" >&2
  echo "  No service file found : ${SERVICE_FILE}" >&2
  echo "" >&2
  echo "  update-node.sh is for upgrading an already-running Aperod node." >&2
  echo "  For a first-time installation run install-node.sh instead." >&2

  send_telegram_alert "⚠️ <b>aperod-node update ABORTED</b>
Server: $(hostname)
No binary at <code>${BINARY_DST}</code> and no service file at <code>${SERVICE_FILE}</code>.
This looks like a fresh server — run <code>install-node.sh</code> instead of <code>update-node.sh</code>."

  exit 1
fi

# ---------------------------------------------------------------------------
# Step 0c: Ensure timeout.conf drop-in exists with a safe TimeoutStopSec.
#
# The node's checkSystemdTimeout() guard checks the exact filename
# /etc/systemd/system/aperod-node.service.d/timeout.conf.  If the file is
# absent (e.g. pre-task-1426 install) the check falls through to the main
# unit and may still pass — but having a dedicated drop-in makes the setting
# explicit, auditable, and overridable without touching the main unit.
#
# We only write the file when it is missing.  If the operator has already
# customised it (e.g. to a larger value) we leave it alone.
# ---------------------------------------------------------------------------
DROPIN_DIR="/etc/systemd/system/${SERVICE_NAME}.service.d"
TIMEOUT_CONF="${DROPIN_DIR}/timeout.conf"
mkdir -p "${DROPIN_DIR}"

# Safe threshold: any TimeoutStopSec below this value risks SIGKILL before
# the UTXO snapshot finishes writing (Aug 2026 outage was caused by 300 s).
TIMEOUT_MIN_SEC=900

_write_timeout_conf() {
  cat > "${TIMEOUT_CONF}" <<EOF
# Aperod node — shutdown timeout drop-in
# ─────────────────────────────────────────────
# Install path: /etc/systemd/system/aperod-node.service.d/timeout.conf
#
# TimeoutStopSec=${TIMEOUT_MIN_SEC}
#   Give the SIGTERM shutdown handler up to 15 minutes to flush the UTXO
#   snapshot to disk before systemd sends SIGKILL.  A shorter timeout
#   truncates the snapshot and forces the next restart into the multi-hour
#   800K-block scan — root cause of the August 2026 outage.
#
#   To raise this value: edit this file and run: systemctl daemon-reload

[Service]
TimeoutStopSec=${TIMEOUT_MIN_SEC}
EOF
}

if [[ ! -f "${TIMEOUT_CONF}" ]]; then
  # File absent — create it with the safe value.
  _write_timeout_conf
  echo "  [ok] timeout.conf drop-in created: ${TIMEOUT_CONF} (TimeoutStopSec=${TIMEOUT_MIN_SEC})"
  systemctl daemon-reload
  echo "  [patch] daemon-reload complete — new TimeoutStopSec takes effect after restart."
else
  # File exists — parse the current TimeoutStopSec and upgrade if unsafe.
  #
  # grep uses || true so a missing TimeoutStopSec line does not abort the
  # script under set -euo pipefail (grep exits 1 when it matches nothing).
  _current=$(grep -E '^[[:space:]]*TimeoutStopSec=' "${TIMEOUT_CONF}" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '[:space:]' || true)
  _need_upgrade=false

  if [[ -z "${_current}" ]]; then
    # File exists but has no TimeoutStopSec line — must add one.
    _need_upgrade=true

  elif [[ "${_current,,}" == "infinity" ]]; then
    # infinity is always safe — never downgrade.
    _need_upgrade=false

  else
    # Normalise systemd duration syntax to seconds so suffixed values like
    # "300s" or "5min" are compared correctly against TIMEOUT_MIN_SEC.
    # Supported suffixes (case-insensitive, single unit only):
    #   NNs | NNsec | NNsecs | NNsecond | NNseconds  → N seconds
    #   NNm | NNmin | NNmins | NNminute | NNminutes   → N * 60
    #   NNh | NNhr  | NNhrs  | NNhour   | NNhours     → N * 3600
    # Bare integers are already in seconds.
    # Any other format (compound "5min 30s", unrecognised suffix, etc.) is
    # treated as unparseable and upgraded to the safe value — better safe
    # than sorry when the UTXO snapshot could be multi-GB.
    _secs=""
    _lower="${_current,,}"

    if [[ "${_lower}" =~ ^([0-9]+)(s|sec|secs|second|seconds)$ ]]; then
      _secs="${BASH_REMATCH[1]}"
    elif [[ "${_lower}" =~ ^([0-9]+)(m|min|mins|minute|minutes)$ ]]; then
      _secs=$(( BASH_REMATCH[1] * 60 ))
    elif [[ "${_lower}" =~ ^([0-9]+)(h|hr|hrs|hour|hours)$ ]]; then
      _secs=$(( BASH_REMATCH[1] * 3600 ))
    elif [[ "${_lower}" =~ ^[0-9]+$ ]]; then
      _secs="${_current}"
    fi
    # _secs is empty → unrecognised format → upgrade.

    if [[ -z "${_secs}" ]]; then
      echo "  [patch] ${TIMEOUT_CONF}: TimeoutStopSec=${_current} is unrecognised/complex — upgrading to safe value."
      _need_upgrade=true
    elif (( _secs < TIMEOUT_MIN_SEC )); then
      _need_upgrade=true
    fi
  fi

  if [[ "${_need_upgrade}" == "true" ]]; then
    echo "  [patch] ${TIMEOUT_CONF}: TimeoutStopSec=${_current:-<missing>} is below safe threshold (${TIMEOUT_MIN_SEC}s) — upgrading."
    _write_timeout_conf
    systemctl daemon-reload
    echo "  [patch] daemon-reload complete — TimeoutStopSec upgraded to ${TIMEOUT_MIN_SEC}."
  else
    echo "  [ok] ${TIMEOUT_CONF}: TimeoutStopSec=${_current} is safe (≥ ${TIMEOUT_MIN_SEC}s) — not overwriting."
  fi
fi

# ---------------------------------------------------------------------------
# Step 1: Pull latest source
# ---------------------------------------------------------------------------
echo "==> [1/5] Pulling latest source as aperod..."
sudo -u aperod git -C "$APEROD_DIR" pull

# ---------------------------------------------------------------------------
# Step 1b: Keep /usr/local/bin/aperod_backup.sh in sync with the repo.
#
# setup-backup.sh installs the script once and never updates it again.
# When git pull brings in changes to blockchain/deploy/aperod_backup.sh the
# running installed copy would silently stay at the old version.  This step
# detects a mismatch and atomically replaces the installed copy (stage in the
# same directory, then rename(2)) so the next scheduled backup always uses the
# current code without ever exposing a partially written file.
#
# Security: this step runs as root (update-node.sh requires sudo) so it can
# legitimately write to root-owned /usr/local/bin/.  The installed copy is
# root-owned (mode 700, owner root) and is not writable by the aperod user,
# preserving the privilege boundary between the unprivileged pull user and
# the root-executed backup service.
#
# Logic lives in sync-backup-script.sh (same directory) so it can be sourced
# and tested independently.  See that file for full documentation.
# ---------------------------------------------------------------------------
# shellcheck source=sync-backup-script.sh
source "${DEPLOY_DIR}/sync-backup-script.sh"
echo "==> [1b] Syncing aperod_backup.sh..."
_sync_backup_script

# ---------------------------------------------------------------------------
# Step 1c: Install/refresh the git post-merge hook.
#
# A bare `git pull` that bypasses update-node.sh runs as `aperod` and cannot
# write to root-owned /usr/local/bin/.  The post-merge hook closes the
# visibility gap: it detects a mismatch between the repo copy and the
# installed copy and immediately alerts the operator on stderr (and via
# Telegram when credentials are available), so they know to run
# sudo update-node.sh to perform the privileged sync.
#
# We refresh the hook on every update-node.sh run so that changes to the
# hook script itself are picked up automatically.
#
# If .git/hooks does not exist (tarball install) the step is a no-op.
# ---------------------------------------------------------------------------
echo "==> [1c] Installing/refreshing git post-merge hook..."
HOOK_SRC="${DEPLOY_DIR}/post-merge"
GIT_HOOKS_DIR="${APEROD_DIR}/.git/hooks"
if [[ -d "${GIT_HOOKS_DIR}" && -f "${HOOK_SRC}" ]]; then
  cp "${HOOK_SRC}" "${GIT_HOOKS_DIR}/post-merge"
  chmod +x "${GIT_HOOKS_DIR}/post-merge"
  echo "  [hook] post-merge hook installed: ${GIT_HOOKS_DIR}/post-merge"
else
  echo "  [hook] ${GIT_HOOKS_DIR} not found — skipping post-merge hook install (tarball install)."
fi

# ---------------------------------------------------------------------------
# Step 2: Rebuild Go binary — if this fails, abort before touching the service.
# The old binary keeps running untouched.
# ---------------------------------------------------------------------------
echo "==> [2/5] Building aperod-node (Go)..."
if ! sudo -u aperod bash -c "export PATH=\$PATH:/usr/local/go/bin; cd '${BLOCKCHAIN_DIR}' && make build"; then
  echo ""
  echo "✗ Build failed — service NOT stopped. The old binary is still running." >&2

  send_telegram_alert "⚠️ <b>aperod-node build FAILED</b>
Server: $(hostname)
The service was <b>NOT stopped</b> — the old binary is still running.
Fix the Go error and re-run <code>update-node.sh</code>."

  exit 1
fi

if [[ ! -f "${BINARY_SRC}" ]]; then
  echo "✗ Build succeeded but binary not found at ${BINARY_SRC}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Step 3: Stop the service BEFORE copying.
#
# Copying over a running Go binary fails with "Text file busy" (ETXTBSY).
# Always stop first, install, then start — never the other way around.
# ---------------------------------------------------------------------------
echo "==> [3/5] Stopping ${SERVICE_NAME}..."
systemctl stop "${SERVICE_NAME}" || true   # non-fatal if already stopped
sleep 1

# ---------------------------------------------------------------------------
# Step 4: Install binary to the correct system path.
#
# The systemd service's ExecStart points to /usr/local/bin/aperod-node.
# Building to /opt/aperod/data/ or any other path silently leaves the service
# running the old code — this step ensures only the right path is updated.
#
# Rollback: we back up the old binary before overwriting it.  If the backup
# itself fails we abort immediately and restart the service so the node is
# never left offline without a recovery path.  If the `cp` or `chmod` install
# step fails after a successful backup, the old binary is restored and the
# service is restarted.  The Telegram alert reports the actual outcome of each
# step rather than just the backup flag.
# ---------------------------------------------------------------------------
echo "==> [4/5] Installing binary to ${BINARY_DST}..."

# Back up the current binary so we can restore it on install failure.
# Failure to create the backup is FATAL — abort now and restart the service
# rather than proceeding without any rollback capability.
BINARY_BACKUP="${BINARY_DST}.pre-update"
_backed_up=false
if [[ -f "${BINARY_DST}" ]]; then
  if /bin/cp "${BINARY_DST}" "${BINARY_BACKUP}" 2>/dev/null; then
    _backed_up=true
    echo "  Backed up existing binary to ${BINARY_BACKUP}"
  else
    echo ""
    echo "✗ Could not create backup at ${BINARY_BACKUP} — aborting install to protect the running service." >&2
    echo "  The service was stopped; restarting it now." >&2
    systemctl start "${SERVICE_NAME}" || true
    send_telegram_alert "⚠️ <b>aperod-node install ABORTED</b>
Server: $(hostname)
Could not create backup at <code>${BINARY_BACKUP}</code> (storage full or permissions error).
Install was aborted; the service has been restarted with the existing binary.
Fix the issue and re-run <code>update-node.sh</code>."
    exit 1
  fi
fi

# Helper: undo a partial install and restart the old binary.
# Tracks the actual outcome of each recovery step and reports it accurately.
_rollback_install() {
  local _restored=false _restarted=false _summary
  echo "✗ Binary installation failed — attempting rollback..." >&2
  if [[ "${_backed_up}" == "true" && -f "${BINARY_BACKUP}" ]]; then
    if /bin/cp "${BINARY_BACKUP}" "${BINARY_DST}" && chmod +x "${BINARY_DST}"; then
      _restored=true
      if systemctl start "${SERVICE_NAME}" 2>/dev/null; then
        _restarted=true
        echo "  Rolled back to previous binary; service restarted." >&2
        echo "  Service state: $(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || echo unknown)" >&2
      else
        echo "  Old binary restored but 'systemctl start' failed — service may be down." >&2
        echo "  Manual recovery: systemctl start ${SERVICE_NAME}" >&2
      fi
    else
      echo "  Rollback copy also failed — service remains stopped." >&2
      echo "  Manual recovery: /bin/cp ${BINARY_BACKUP} ${BINARY_DST} && chmod +x ${BINARY_DST} && systemctl start ${SERVICE_NAME}" >&2
    fi
  else
    echo "  No backup available — service remains stopped." >&2
    echo "  Manual recovery: cp <new-binary> ${BINARY_DST} && chmod +x ${BINARY_DST} && systemctl start ${SERVICE_NAME}" >&2
  fi
  if [[ "${_restored}" == "true" && "${_restarted}" == "true" ]]; then
    _summary="Old binary was restored and service restarted successfully."
  elif [[ "${_restored}" == "true" ]]; then
    _summary="Old binary was restored but service failed to start — check <code>journalctl -u ${SERVICE_NAME}</code>."
  else
    _summary="Rollback failed — service is DOWN. Manual recovery required."
  fi
  send_telegram_alert "🚨 <b>aperod-node install FAILED</b>
Server: $(hostname)
Binary copy to <code>${BINARY_DST}</code> failed.
${_summary}"
}

# Install new binary; roll back and abort if anything goes wrong.
if ! cp "${BINARY_SRC}" "${BINARY_DST}"; then
  _rollback_install
  exit 1
fi
if ! chmod +x "${BINARY_DST}"; then
  _rollback_install
  exit 1
fi

echo "  Installed: $(${BINARY_DST} --version 2>/dev/null || ls -lh "${BINARY_DST}" | awk '{print $5, $9}')"
rm -f "${BINARY_BACKUP}" 2>/dev/null || true   # clean up backup on success

# Start the service after the new binary is in place.
systemctl start "${SERVICE_NAME}"
echo "  Service started."

# ---------------------------------------------------------------------------
# Step 5: Health check — wait for the node API to respond.
#
# Polls /api/v1/status up to HEALTH_MAX_ATTEMPTS times.
# Exits non-zero and fires a Telegram alert if the node never comes up.
# Set SKIP_HEALTH_CHECK=1 to bypass (e.g. when the RPC port is not exposed).
# ---------------------------------------------------------------------------
echo "==> [5/5] Health check (polling ${HEALTH_URL})..."

if [[ "$SKIP_HEALTH_CHECK" == "1" ]]; then
  echo "  SKIP_HEALTH_CHECK=1 — skipping."
else
  attempt=0
  healthy=false
  while [[ $attempt -lt $HEALTH_MAX_ATTEMPTS ]]; do
    attempt=$(( attempt + 1 ))
    if curl -sf --connect-timeout 2 "${HEALTH_URL}" > /dev/null 2>&1; then
      healthy=true
      break
    fi
    echo "  Waiting... (attempt ${attempt}/${HEALTH_MAX_ATTEMPTS})"
    sleep "${HEALTH_WAIT_SECS}"
  done

  if [[ "${healthy}" != "true" ]]; then
    echo ""
    echo "✗ Health check FAILED — node did not respond on ${HEALTH_URL}" >&2
    echo "  Inspect logs: journalctl -u ${SERVICE_NAME} -n 100 --no-pager" >&2

    send_telegram_alert "🚨 <b>aperod-node health check FAILED</b>
Server: $(hostname)
Node did not respond on <code>${HEALTH_URL}</code> after $(( HEALTH_MAX_ATTEMPTS * HEALTH_WAIT_SECS ))s.
Inspect logs: <code>journalctl -u ${SERVICE_NAME} -n 100</code>"

    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Step 5b: Peer connectivity check — warn if the node has no P2P peers.
#
# Logic lives in peer-check.sh (same directory) so it can be unit-tested
# independently.  See that file for full documentation.
# ---------------------------------------------------------------------------
# shellcheck source=peer-check.sh
source "${DEPLOY_DIR}/peer-check.sh"
aperod_peer_check

echo ""
echo "✓ Update complete. New build is live."
echo "  Service status : systemctl status ${SERVICE_NAME}"
echo "  Live logs      : journalctl -u ${SERVICE_NAME} -f"
echo "  Chain tip      : curl -s ${HEALTH_URL} | jq ."
echo "  Peer stats     : curl -s ${STATS_URL} | jq .peer_count"
echo ""
echo "  Binary installed to : ${BINARY_DST}"
echo "  ⚠  Do NOT build to /opt/aperod/data/ — the service ignores that path."
