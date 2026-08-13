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
  [[ "${key_mode: -2}" != "00"  ]] && needs_fix=true
  if [[ "${needs_fix}" == "true" ]]; then
    echo "  [fix] ${VALIDATOR_KEY}: owner=${key_owner} mode=${key_mode} → chown aperod:aperod + strip group/other bits"
    chown aperod:aperod "${VALIDATOR_KEY}"
    chmod g-rwx,o-rwx   "${VALIDATOR_KEY}"
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

  send_telegram_alert "⚠️ <b>aperod-node: обновление ПРЕРВАНО</b>
Сервер: $(hostname)
Бинарник не найден: <code>${BINARY_DST}</code>, файл сервиса: <code>${SERVICE_FILE}</code>.
Похоже, это новый сервер — запустите <code>install-node.sh</code> вместо <code>update-node.sh</code>."

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
# Step 1a: Warn if node.yaml still carries dangerous ban-threshold defaults.
#
# Production nodes manually fixed to bad_block_ban_threshold=5 /
# bad_block_height_lead=1000 in August 2026.  The install template now ships
# those safe values, but operators who cloned before the fix may still have
# the old permissive values (999 / 9999999) in their live node.yaml.
# Those values effectively disable rogue-peer protection.
#
# The check is non-fatal — it never blocks an upgrade — but it prints a
# prominent warning and sends a Telegram alert so the operator is not left
# unknowingly unprotected.
# ---------------------------------------------------------------------------
NODE_YAML="${APEROD_DIR}/blockchain/node.yaml"
if [[ -f "${NODE_YAML}" ]]; then
  _ban_warn=false
  _ban_threshold=$(grep -E '^[[:space:]]*bad_block_ban_threshold:' "${NODE_YAML}" 2>/dev/null | head -1 | awk -F: '{print $2}' | tr -d '[:space:]' || true)
  _height_lead=$(grep -E '^[[:space:]]*bad_block_height_lead:' "${NODE_YAML}" 2>/dev/null | head -1 | awk -F: '{print $2}' | tr -d '[:space:]' || true)

  # Dangerous values that were shipped before the August 2026 fix.
  if [[ "${_ban_threshold}" == "999" || "${_height_lead}" == "9999999" ]]; then
    _ban_warn=true
  fi

  if [[ "${_ban_warn}" == "true" ]]; then
    echo ""
    echo "⚠️  WARNING: ${NODE_YAML} has dangerous rogue-peer ban values:" >&2
    [[ "${_ban_threshold}" == "999"     ]] && echo "     bad_block_ban_threshold: 999  (safe default: 5)"       >&2
    [[ "${_height_lead}"   == "9999999" ]] && echo "     bad_block_height_lead:   9999999  (safe default: 1000)" >&2
    echo "   These values effectively disable wrong-fork peer protection." >&2
    echo "   Edit ${NODE_YAML} and set:" >&2
    echo "     bad_block_ban_threshold: 5" >&2
    echo "     bad_block_height_lead: 1000" >&2
    echo "   Then restart the node for the change to take effect." >&2
    echo ""
    send_telegram_alert "⚠️ <b>aperod-node: опасные значения бан-порогов</b>
Сервер: $(hostname)
В <code>${NODE_YAML}</code> ещё стоят разрешающие значения до августа 2026:
bad_block_ban_threshold=${_ban_threshold:-&lt;не задано&gt;}  (безопасно: 5)
bad_block_height_lead=${_height_lead:-&lt;не задано&gt;}  (безопасно: 1000)
Защита от мошеннических пиров отключена. Исправьте node.yaml и перезапустите ноду."
  else
    echo "  [ok] node.yaml ban thresholds look safe (bad_block_ban_threshold=${_ban_threshold:-default}, bad_block_height_lead=${_height_lead:-default})."
  fi
fi

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

# Also keep /usr/local/bin/aperod-deploy (copy of deploy/aperod-api-deploy.sh
# at the monorepo root) fresh — it was previously installed once and never
# updated by git pull.  Non-fatal if either side is absent on this host.
echo "==> [1b] Syncing aperod-deploy..."
_sync_backup_script /usr/local/bin/aperod-deploy "${APEROD_DIR}/deploy/aperod-api-deploy.sh"

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

  send_telegram_alert "⚠️ <b>aperod-node: сборка ПРОВАЛИЛАСЬ</b>
Сервер: $(hostname)
Сервис <b>НЕ остановлен</b> — старый бинарник всё ещё работает.
Исправьте ошибку Go и запустите <code>update-node.sh</code> повторно."

  exit 1
fi

if [[ ! -f "${BINARY_SRC}" ]]; then
  echo "✗ Build succeeded but binary not found at ${BINARY_SRC}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Step 2b: Guard — abort if the fresh binary is dynamically linked.
#
# If CGO_ENABLED=0 is accidentally removed from the Makefile, the compiled
# binary links against the build host's GLIBC.  Older production hosts
# (Debian 11, Ubuntu 20.04) ship GLIBC 2.31 and refuse to start a binary
# that requires GLIBC_2.32+, causing an immediate crash-loop on every
# upgraded machine after the service restarts.
#
# This check runs BEFORE the service is stopped — if the binary is dynamic,
# the old version keeps running untouched and the operator gets a clear error
# message instead of a silent regression that only surfaces after restart.
#
# Primary check: ldd (present on all glibc-based Linux distros).
#   Static binary  → "not a dynamic executable"
#   Dynamic binary → lists shared-library dependencies (guard fires)
#
# Fallback check (when ldd is absent, e.g. musl-based Alpine): readelf -l
# looks for a PT_INTERP program header directly in the ELF.
# ---------------------------------------------------------------------------
echo "==> [2b] Verifying binary is statically linked..."

_binary_is_dynamic=false

if command -v ldd > /dev/null 2>&1; then
  _ldd_out=$(ldd "${BINARY_SRC}" 2>&1 || true)
  if echo "${_ldd_out}" | grep -q "not a dynamic executable"; then
    echo "  ldd: binary is statically linked ✓"
  else
    _binary_is_dynamic=true
    echo "  ldd output:" >&2
    echo "${_ldd_out}" | sed 's/^/    /' >&2
  fi
elif command -v readelf > /dev/null 2>&1; then
  if readelf -l "${BINARY_SRC}" 2>/dev/null | grep -q 'INTERP'; then
    _binary_is_dynamic=true
    echo "  readelf: PT_INTERP segment found — binary has a dynamic linker path" >&2
  else
    echo "  readelf: no PT_INTERP segment — binary is statically linked ✓"
  fi
else
  echo "  [warn] Neither ldd nor readelf found — skipping static-link check."
  echo "         Ensure CGO_ENABLED=0 is set in the Makefile build-node target."
fi

if [[ "${_binary_is_dynamic}" == "true" ]]; then
  echo ""
  echo "✗ Static-link check FAILED — ${BINARY_SRC} is dynamically linked." >&2
  echo "  The service was NOT stopped. The old binary is still running." >&2
  echo "  Fix: ensure CGO_ENABLED=0 is set in the Makefile build-node target," >&2
  echo "  then re-run update-node.sh." >&2

  send_telegram_alert "⚠️ <b>aperod-node: проверка статической линковки ПРОВАЛИЛАСЬ</b>
Сервер: $(hostname)
Новый бинарник <code>${BINARY_SRC}</code> динамически слинкован.
Сервис <b>НЕ остановлен</b> — старый бинарник всё ещё работает.
Исправление: убедитесь, что в Makefile цели <code>build-node</code> установлен <code>CGO_ENABLED=0</code>, затем запустите <code>update-node.sh</code> повторно."

  exit 1
fi

# ---------------------------------------------------------------------------
# Step 2c: Validator-key permission preflight — runs BEFORE the service stop.
#
# The freshly deployed binary refuses to boot when the validator key file has
# any group/other permission bit set (mode & 0o077 != 0).  A production node
# whose key still carries historical 644 permissions would therefore be
# stopped, have the new binary installed, and only THEN fail the startup check —
# leaving the service in a crash-restart loop until an operator runs chmod by
# hand.  That is exactly the ~20-minute outage this preflight prevents.
#
# Strategy:
#   1. Resolve the key path from node.yaml (data_dir + consensus.validator_key);
#      fall back to the standard on-disk layout, then to a glob.
#   2. If the file is group/other-accessible OR not owned by aperod, AUTO-FIX it
#      with `chmod g-rwx,o-rwx` + `chown aperod:aperod`.  Stripping group/other is the safest
#      possible mode — this NEVER loosens permissions, only tightens them.
#   3. As a belt-and-braces check, run the new binary's `--validate-config`
#      dry-run (which enforces the same key-permission rule plus config parsing)
#      BEFORE stopping the node.  If it still reports a fatal problem, abort now
#      with a clear message while the old binary keeps serving.
#
# All of this happens while the OLD (working) node is still running, so a
# failure here costs zero downtime.
# ---------------------------------------------------------------------------

# _resolve_validator_key_path — print the validator key path the node will use.
# Reads data_dir and consensus.validator_key from node.yaml when present; falls
# back to the standard testnet data layout and finally a glob.  Prints nothing
# if no candidate file exists.
_resolve_validator_key_path() {
  local node_yaml="$1"
  local candidate=""

  if [[ -f "${node_yaml}" ]]; then
    # An explicit consensus.validator_key wins if it is set to a non-empty path.
    local explicit_key
    explicit_key=$(grep -E '^[[:space:]]*validator_key:' "${node_yaml}" 2>/dev/null \
      | head -1 | sed -E 's/^[[:space:]]*validator_key:[[:space:]]*//' \
      | sed -E 's/[[:space:]]*(#.*)?$//' | tr -d '"'"'"'' || true)
    if [[ -n "${explicit_key}" ]]; then
      candidate="${explicit_key}"
    else
      # Otherwise the node derives <data_dir>/validator.key.
      local data_dir
      data_dir=$(grep -E '^[[:space:]]*data_dir:' "${node_yaml}" 2>/dev/null \
        | head -1 | sed -E 's/^[[:space:]]*data_dir:[[:space:]]*//' \
        | sed -E 's/[[:space:]]*(#.*)?$//' | tr -d '"'"'"'' || true)
      if [[ -n "${data_dir}" ]]; then
        # Resolve a relative data_dir against the node.yaml directory.
        if [[ "${data_dir}" != /* ]]; then
          data_dir="${data_dir#./}"   # drop a leading ./ so the path stays clean
          data_dir="$(cd "$(dirname "${node_yaml}")" && pwd)/${data_dir}"
        fi
        candidate="${data_dir%/}/validator.key"
      fi
    fi
  fi

  if [[ -n "${candidate}" && -f "${candidate}" ]]; then
    printf '%s\n' "${candidate}"
    return 0
  fi

  # Fallback 1: the standard production on-disk layout.
  local prod_key="${BLOCKCHAIN_DIR}/data/testnet/validator.key"
  if [[ -f "${prod_key}" ]]; then
    printf '%s\n' "${prod_key}"
    return 0
  fi

  # Fallback 2: glob under the blockchain data tree (first match wins).
  local globbed
  globbed=$(find "${BLOCKCHAIN_DIR}" -maxdepth 4 -name validator.key -type f 2>/dev/null | head -1 || true)
  if [[ -n "${globbed}" ]]; then
    printf '%s\n' "${globbed}"
    return 0
  fi

  return 0   # no key file found — nothing to preflight (non-validator node)
}

# preflight_validator_key — tighten permissions/ownership on the validator key
# so the new binary's startup check cannot fail.  chmod g-rwx,o-rwx (owner bits
# preserved, e.g. 0400 stays 0400) + chown aperod:aperod only ever tightens access.  Returns 0 on success (fixed or already safe),
# 1 only if the file exists but could not be made safe (caller must abort).
preflight_validator_key() {
  local key_path="$1"

  if [[ -z "${key_path}" || ! -f "${key_path}" ]]; then
    echo "  [ok] No validator key file found — permission preflight skipped (non-validator node)."
    return 0
  fi

  local mode owner
  mode=$(stat -c '%a' "${key_path}" 2>/dev/null || echo "")
  owner=$(stat -c '%U' "${key_path}" 2>/dev/null || echo "")

  # mode & 0o077 != 0  ⇒  some group/other bit is set (what the binary rejects).
  local needs_chmod=false
  if [[ -n "${mode}" ]]; then
    # Right-pad short modes (e.g. "60" is not expected, but be defensive).
    local go_bits="${mode: -2}"          # last two octal digits (group+other)
    [[ "${go_bits}" != "00" ]] && needs_chmod=true
  else
    needs_chmod=true                     # stat failed — force a tighten
  fi

  local needs_chown=false
  [[ "${owner}" != "aperod" ]] && needs_chown=true

  if [[ "${needs_chmod}" == "true" ]]; then
    echo "  [fix] ${key_path}: mode ${mode:-?} is group/other-accessible → chmod g-rwx,o-rwx (tighten only, owner bits preserved)"
    if ! chmod g-rwx,o-rwx "${key_path}"; then
      echo "✗ Could not chmod g-rwx,o-rwx ${key_path}" >&2
      return 1
    fi
  fi

  if [[ "${needs_chown}" == "true" ]]; then
    echo "  [fix] ${key_path}: owner ${owner:-?} → aperod:aperod"
    if ! chown aperod:aperod "${key_path}"; then
      echo "✗ Could not chown aperod:aperod ${key_path}" >&2
      return 1
    fi
  fi

  # Re-verify the mode is now safe (defensive; should always pass after chmod).
  local final_mode
  final_mode=$(stat -c '%a' "${key_path}" 2>/dev/null || echo "")
  if [[ -n "${final_mode}" && "${final_mode: -2}" != "00" ]]; then
    echo "✗ ${key_path} still has unsafe permissions (${final_mode}) after fix" >&2
    return 1
  fi

  if [[ "${needs_chmod}" == "false" && "${needs_chown}" == "false" ]]; then
    echo "  [ok] ${key_path}: permissions and ownership already safe (no group/other bits, aperod-owned)."
  else
    echo "  [ok] ${key_path}: validator key permissions/ownership corrected before stopping the node."
  fi
  return 0
}

echo "==> [2c] Validator-key permission preflight (before stopping the node)..."
_vkey_path="$(_resolve_validator_key_path "${NODE_YAML}")"
if ! preflight_validator_key "${_vkey_path}"; then
  echo "" >&2
  echo "✗ Validator-key preflight FAILED — the service was NOT stopped." >&2
  echo "  The old binary is still running. Fix the key file, then re-run update-node.sh." >&2
  echo "  Manual recovery: chmod go-rwx ${_vkey_path:-<validator.key>} && chown aperod:aperod ${_vkey_path:-<validator.key>}" >&2
  send_telegram_alert "⚠️ <b>aperod-node: preflight ключа валидатора ПРОВАЛИЛСЯ</b>
Сервер: $(hostname)
Не удалось обезопасить ключ <code>${_vkey_path:-&lt;неизвестно&gt;}</code> (chmod go-rwx / chown aperod:aperod).
Сервис <b>НЕ остановлен</b> — старый бинарник всё ещё работает.
Исправьте права на файл ключа и запустите <code>update-node.sh</code> повторно."
  exit 1
fi

# Belt-and-braces: run the new binary's dry-run validation (config parse + the
# same fatal validator-key permission check) BEFORE stopping the live node.
# --validate-config exits 0 on success; any non-zero means the new binary would
# refuse to boot, so we abort now while the old binary is still serving.
# Skippable via SKIP_CONFIG_DRYRUN=1 for environments where the RPC config is
# not present on the deploy host.
SKIP_CONFIG_DRYRUN="${SKIP_CONFIG_DRYRUN:-0}"
if [[ "${SKIP_CONFIG_DRYRUN}" != "1" && -f "${NODE_YAML}" ]]; then
  if "${BINARY_SRC}" --validate-config --config "${NODE_YAML}" >/tmp/aperod-validate.$$  2>&1; then
    echo "  [ok] New binary dry-run passed ($(head -1 /tmp/aperod-validate.$$ 2>/dev/null))."
    rm -f /tmp/aperod-validate.$$ 2>/dev/null || true
  else
    _dryrun_out="$(cat /tmp/aperod-validate.$$ 2>/dev/null || true)"
    rm -f /tmp/aperod-validate.$$ 2>/dev/null || true
    echo "" >&2
    echo "✗ New binary dry-run FAILED — the service was NOT stopped." >&2
    echo "  The old binary is still running. Fix the reported issue and re-run update-node.sh." >&2
    echo "  Details: ${_dryrun_out}" >&2
    send_telegram_alert "⚠️ <b>aperod-node: проверка конфига ПРОВАЛИЛАСЬ</b>
Сервер: $(hostname)
Новый бинарник отверг рабочий конфиг при <code>--validate-config</code>:
<code>${_dryrun_out}</code>
Сервис <b>НЕ остановлен</b> — старый бинарник всё ещё работает."
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Step 3: Stop the service BEFORE copying.
#
# Copying over a running Go binary fails with "Text file busy" (ETXTBSY).
# Always stop first, install, then start — never the other way around.
# ---------------------------------------------------------------------------
echo "==> [3/5] Stopping ${SERVICE_NAME}..."
systemctl stop "${SERVICE_NAME}" || true   # non-fatal if already stopped
# Wait up to 120 s for the service to fully stop before we try to replace the
# binary.  A single "sleep 1" is not enough: on shutdown the node flushes a
# UTXO snapshot that can take several minutes.  Copying over a binary that is
# still mapped into a running process fails with ETXTBSY ("Text file busy").
_stop_waited=0
while true; do
    _st=$(systemctl is-active "${SERVICE_NAME}" 2>/dev/null || true)
    # Break when stopped: "inactive", "failed", or empty string (fake systemctl
    # stubs exit non-zero with no output when the service is not running).
    [ "$_st" = "inactive" ] || [ "$_st" = "failed" ] || [ -z "$_st" ] && break
    if [ "$_stop_waited" -ge 120 ]; then
        echo "  [warn] ${SERVICE_NAME} still '${_st}' after 120 s — sending SIGKILL to unblock binary swap" >&2
        systemctl kill --signal=SIGKILL "${SERVICE_NAME}" 2>/dev/null || true
        sleep 2
        send_telegram_alert "⚠️ <b>Деплой SIGKILL: ${SERVICE_NAME}</b>
Сервис всё ещё в состоянии '<code>${_st}</code>' спустя ${_stop_waited}с — отправлен SIGKILL для замены бинарника.
Проверьте зависание сброса снимка: <code>journalctl -u ${SERVICE_NAME} -n 100 --no-pager</code>"
        break
    fi
    sleep 5
    _stop_waited=$(( _stop_waited + 5 ))
done

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
    send_telegram_alert "⚠️ <b>aperod-node: установка ПРЕРВАНА</b>
Сервер: $(hostname)
Не удалось создать резервную копию <code>${BINARY_BACKUP}</code> (диск заполнен или ошибка прав).
Установка прервана; сервис перезапущен со старым бинарником.
Устраните проблему и запустите <code>update-node.sh</code> повторно."
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
    _summary="Старый бинарник восстановлен и сервис перезапущен."
  elif [[ "${_restored}" == "true" ]]; then
    _summary="Старый бинарник восстановлен, но сервис не запустился — проверьте <code>journalctl -u ${SERVICE_NAME}</code>."
  else
    _summary="Откат не удался — сервис УПАЛ. Требуется ручное восстановление."
  fi
  send_telegram_alert "🚨 <b>aperod-node: установка ПРОВАЛИЛАСЬ</b>
Сервер: $(hostname)
Копирование бинарника в <code>${BINARY_DST}</code> не удалось.
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

    send_telegram_alert "🚨 <b>aperod-node: health check ПРОВАЛИЛСЯ</b>
Сервер: $(hostname)
Нода не ответила на <code>${HEALTH_URL}</code> за $(( HEALTH_MAX_ATTEMPTS * HEALTH_WAIT_SECS ))с.
Проверьте логи: <code>journalctl -u ${SERVICE_NAME} -n 100</code>"

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
