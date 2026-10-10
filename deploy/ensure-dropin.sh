#!/usr/bin/env bash
# ============================================================
#  ensure-dropin.sh — Idempotently install aperod-node systemd
#  memory-protection drop-ins.
#
#  Writes (or verifies) two drop-in files under
#  /etc/systemd/system/aperod-node.service.d/:
#
#    timeout.conf     — TimeoutStopSec=900 (15 min shutdown window
#                       so the UTXO snapshot can flush to disk)
#    gomemlimit.conf  — GOMEMLIMIT=<canonical> (Go heap cap to
#                       prevent OOM-kill and corrupt snapshots).
#                       The value is read from the canonical
#                       drop-in file (gomemlimit.conf) that ships
#                       alongside this script — the single source
#                       of truth for the production limit.
#
#  Safe to call multiple times; only rewrites a file when its
#  content has changed.  Calls `systemctl daemon-reload` at the
#  end regardless, so the caller does not need to.
#
#  Usage:
#    sudo bash ensure-dropin.sh
#    # or from another script:
#    bash "${INSTALL_DIR}/deploy/ensure-dropin.sh"
#
#  Injectable seams for testing (override via environment):
#    DROPIN_DIR       — directory that holds the installed .conf files
#                       (default: /etc/systemd/system/aperod-node.service.d)
#    SYSTEMCTL        — systemctl binary/stub
#                       (default: systemctl)
#    CANONICAL_DROPIN — repo drop-in file holding the source-of-truth
#                       GOMEMLIMIT value
#                       (default: <script dir>/gomemlimit.conf)
# ============================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; NC='\033[0m'
info() { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }

# ── Injectable seams (overridable for tests) ───────────────
DROPIN_DIR="${DROPIN_DIR:-/etc/systemd/system/aperod-node.service.d}"
SYSTEMCTL="${SYSTEMCTL:-systemctl}"

# ── Host-aware GOMEMLIMIT policy ───────────────────────────
# The shipped gomemlimit.conf documents the primary's canonical cap. Relays
# must instead use their own RAM so a later join/update never promotes a small
# host to the primary's 5.5 GiB setting.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/gomemlimit-policy.sh"
gomemlimit_resolve
GOMEMLIMIT_VALUE="${GOMEMLIMIT_BYTES}"

# ── Helper: write file only when content differs ──────────
write_if_changed() {
  local path="$1"
  local content="$2"
  if [[ -f "${path}" ]] && [[ "$(cat "${path}")" == "${content}" ]]; then
    ok "Drop-in already up to date: ${path}"
  else
    printf '%s\n' "${content}" > "${path}"
    ok "Wrote drop-in: ${path}"
  fi
}

# ── Create drop-in directory ───────────────────────────────
mkdir -p "${DROPIN_DIR}"

# ── timeout.conf ──────────────────────────────────────────
# TimeoutStopSec=900 gives the UTXO snapshot up to 15 minutes
# to flush before systemd sends SIGKILL.  The Aug 2026 outage
# was caused by a 300 s value triggering SIGKILL mid-write on
# a 5.7 GB RAM node.
TIMEOUT_CONTENT="# Aperod node — shutdown timeout drop-in
# Install path: ${DROPIN_DIR}/timeout.conf
#
# TimeoutStopSec=900 gives the UTXO snapshot up to 15 minutes to flush
# before systemd sends SIGKILL.  The Aug 2026 outage was caused by a
# 300 s value triggering SIGKILL mid-write on a 5.7 GB RAM node.
[Service]
TimeoutStopSec=900"

write_if_changed "${DROPIN_DIR}/timeout.conf" "${TIMEOUT_CONTENT}"

# ── gomemlimit.conf ───────────────────────────────────────
# GOMEMLIMIT caps the Go heap so the runtime triggers GC before the kernel
# OOM-killer fires.  Without this cap the node's RSS can grow past available
# RAM, triggering SIGKILL and corrupting the LevelDB snapshot.  The value is
# taken from the canonical drop-in file (single source of truth).
GOMEMLIMIT_CONTENT="[Service]
Environment=\"GOMEMLIMIT=${GOMEMLIMIT_VALUE}\""

write_if_changed "${DROPIN_DIR}/gomemlimit.conf" "${GOMEMLIMIT_CONTENT}"

# ── Reload systemd so drop-ins take effect ────────────────
info "Вызываем ${SYSTEMCTL} daemon-reload…"
"${SYSTEMCTL}" daemon-reload
ok "daemon-reload завершён — drop-ins активны"
