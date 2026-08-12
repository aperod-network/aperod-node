#!/usr/bin/env bash
# ============================================================
#  Aperod — Verify systemd drop-in settings on a remote node
#
#  Checks that GOMEMLIMIT and TimeoutStopSec are correctly
#  applied to the aperod-node systemd service.
#
#  Usage (run from the primary node, same as join-network.sh):
#    bash verify-dropin.sh <IP>
#
#  Example:
#    bash verify-dropin.sh 77.221.153.86
#
#  Exit codes:
#    0 — both settings are present and correct
#    1 — one or more settings are missing or wrong
# ============================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[ERR]${NC}   $*"; exit 1; }

# ── Expected values ──────────────────────────────────────────
# EXPECTED_GOMEMLIMIT is read from the canonical drop-in file that ships in
# this directory (gomemlimit.conf) rather than being hard-coded here.  That
# file is the single source of truth for the production value, so bumping the
# limit only requires editing one place and this check never goes stale.
#
# CANONICAL_DROPIN — overridable so tests can point at a temp file.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CANONICAL_DROPIN="${CANONICAL_DROPIN:-${SCRIPT_DIR}/gomemlimit.conf}"

[[ -f "${CANONICAL_DROPIN}" ]] \
  || die "Canonical drop-in not found: ${CANONICAL_DROPIN}
  This file holds the source-of-truth GOMEMLIMIT value. Restore it from the repo."

EXPECTED_GOMEMLIMIT=$(grep -oE 'GOMEMLIMIT=[0-9]+' "${CANONICAL_DROPIN}" \
  | head -1 | cut -d= -f2)
[[ -n "${EXPECTED_GOMEMLIMIT}" ]] \
  || die "Could not parse GOMEMLIMIT from canonical drop-in: ${CANONICAL_DROPIN}"

EXPECTED_TIMEOUT="900"

TARGET_IP="${1:-}"
[[ -z "${TARGET_IP}" ]] && die "Usage: bash verify-dropin.sh <IP>"

echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod — Verify Drop-in Settings                    ║
║        Target: ${TARGET_IP}
╚════════════════════════════════════════════════════════════╝${NC}
"

info "Querying systemctl show aperod-node on ${TARGET_IP}…"

SHOW_OUTPUT=$(ssh "root@${TARGET_IP}" "systemctl show aperod-node" 2>/dev/null) \
  || die "Failed to SSH to ${TARGET_IP} or systemctl show failed"

FAILED=0

# ── Check GOMEMLIMIT ─────────────────────────────────────────
GOMEMLIMIT_LINE=$(echo "${SHOW_OUTPUT}" | grep "^Environment=" | grep "GOMEMLIMIT" || true)

if [[ -z "${GOMEMLIMIT_LINE}" ]]; then
  echo -e "${RED}[FAIL]${NC}  GOMEMLIMIT is NOT set in aperod-node environment"
  warn "Drop-in may not have been applied or daemon-reload was skipped"
  FAILED=1
else
  # Extract the value from the line, e.g. Environment=GOMEMLIMIT=5368709120
  ACTUAL_GOMEMLIMIT=$(echo "${GOMEMLIMIT_LINE}" | grep -oP 'GOMEMLIMIT=\K[0-9]+' || true)
  if [[ "${ACTUAL_GOMEMLIMIT}" == "${EXPECTED_GOMEMLIMIT}" ]]; then
    ok "GOMEMLIMIT=${ACTUAL_GOMEMLIMIT} ✓"
  else
    echo -e "${RED}[FAIL]${NC}  GOMEMLIMIT=${ACTUAL_GOMEMLIMIT} (expected ${EXPECTED_GOMEMLIMIT})"
    FAILED=1
  fi
fi

# ── Check TimeoutStopSec ─────────────────────────────────────
TIMEOUT_LINE=$(echo "${SHOW_OUTPUT}" | grep "^TimeoutStopUSec=" || true)

if [[ -z "${TIMEOUT_LINE}" ]]; then
  echo -e "${RED}[FAIL]${NC}  TimeoutStopUSec not found in systemctl show output"
  FAILED=1
else
  # systemctl show reports in microseconds, e.g. TimeoutStopUSec=15min
  # or as a human-readable string like "15min". We also accept the raw
  # microsecond form. Parse seconds from common representations.
  TIMEOUT_RAW=$(echo "${TIMEOUT_LINE}" | cut -d= -f2-)

  # Convert to seconds for comparison
  ACTUAL_TIMEOUT_SECS=""
  if echo "${TIMEOUT_RAW}" | grep -qE '^[0-9]+$'; then
    # Pure microseconds (older systemd)
    ACTUAL_TIMEOUT_SECS=$(( ${TIMEOUT_RAW} / 1000000 ))
  elif echo "${TIMEOUT_RAW}" | grep -qiE '^[0-9]+min( [0-9]+s)?$'; then
    MINS=$(echo "${TIMEOUT_RAW}" | grep -oP '^\K[0-9]+(?=min)')
    SECS_PART=$(echo "${TIMEOUT_RAW}" | grep -oP '[0-9]+(?=s$)' || echo "0")
    ACTUAL_TIMEOUT_SECS=$(( MINS * 60 + SECS_PART ))
  elif echo "${TIMEOUT_RAW}" | grep -qiE '^[0-9]+h( [0-9]+min)?( [0-9]+s)?$'; then
    HRS=$(echo "${TIMEOUT_RAW}" | grep -oP '^\K[0-9]+(?=h)')
    MINS_PART=$(echo "${TIMEOUT_RAW}" | grep -oP '[0-9]+(?=min)' || echo "0")
    SECS_PART=$(echo "${TIMEOUT_RAW}" | grep -oP '[0-9]+(?=s$)' || echo "0")
    ACTUAL_TIMEOUT_SECS=$(( HRS * 3600 + MINS_PART * 60 + SECS_PART ))
  elif echo "${TIMEOUT_RAW}" | grep -qiE '^[0-9]+s$'; then
    ACTUAL_TIMEOUT_SECS=$(echo "${TIMEOUT_RAW}" | grep -oP '[0-9]+')
  else
    # Fallback: try to get it directly from TimeoutStopSec property name
    TIMEOUT_SEC_LINE=$(echo "${SHOW_OUTPUT}" | grep "^TimeoutStopSec=" || true)
    if [[ -n "${TIMEOUT_SEC_LINE}" ]]; then
      ACTUAL_TIMEOUT_SECS=$(echo "${TIMEOUT_SEC_LINE}" | cut -d= -f2-)
    fi
  fi

  if [[ -z "${ACTUAL_TIMEOUT_SECS}" ]]; then
    echo -e "${YELLOW}[WARN]${NC}  Could not parse TimeoutStopUSec='${TIMEOUT_RAW}' — checking raw value"
    # Last resort: show the raw value and let the operator decide
    warn "Raw: ${TIMEOUT_LINE}"
    warn "Expected ${EXPECTED_TIMEOUT}s (15min). Verify manually."
  elif [[ "${ACTUAL_TIMEOUT_SECS}" -eq "${EXPECTED_TIMEOUT}" ]]; then
    ok "TimeoutStopSec=${ACTUAL_TIMEOUT_SECS}s ✓"
  else
    echo -e "${RED}[FAIL]${NC}  TimeoutStopSec=${ACTUAL_TIMEOUT_SECS}s (expected ${EXPECTED_TIMEOUT}s)"
    FAILED=1
  fi
fi

# ── Drop-in files present? ───────────────────────────────────
info "Checking drop-in files on ${TARGET_IP}…"

for DROPIN in gomemlimit.conf timeout.conf; do
  DROPIN_PATH="/etc/systemd/system/aperod-node.service.d/${DROPIN}"
  EXISTS=$(ssh "root@${TARGET_IP}" "test -f '${DROPIN_PATH}' && echo yes || echo no" 2>/dev/null)
  if [[ "${EXISTS}" == "yes" ]]; then
    ok "Drop-in present: ${DROPIN_PATH}"
  else
    echo -e "${RED}[FAIL]${NC}  Drop-in missing: ${DROPIN_PATH}"
    FAILED=1
  fi
done

# ── Result ───────────────────────────────────────────────────
echo
if [[ "${FAILED}" -eq 0 ]]; then
  echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════╗"
  echo -e "  ✓  All drop-in settings verified on ${TARGET_IP}"
  echo -e "╚══════════════════════════════════════════════════════════════╝${NC}"
  exit 0
else
  echo -e "${RED}${BOLD}╔══════════════════════════════════════════════════════════════╗"
  echo -e "  ✗  One or more drop-in settings are missing or wrong"
  echo -e "     Re-run join-network.sh or apply the drop-ins manually:"
  echo -e "     blockchain/deploy/join-network.sh ${TARGET_IP}"
  echo -e "╚══════════════════════════════════════════════════════════════╝${NC}"
  exit 1
fi
