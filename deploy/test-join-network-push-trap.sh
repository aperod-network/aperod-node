#!/usr/bin/env bash
# =============================================================================
#  test-join-network-push-trap.sh — Tests for the EXIT/ERR trap in the
#  push-mode (join-network.sh <TARGET_IP>) of join-network.sh.
#
#  Focused on three properties of the _push_cleanup trap:
#
#  1. Interrupted before source was stopped (Step 1 done, Step 2 not yet)
#     When the script fails after stopping the target node (Step 1,
#     _TARGET_STOPPED=1) but before stopping the source node (Step 2,
#     _SOURCE_STOPPED still 0), the _push_cleanup trap must:
#       a. Print the [TRAP] banner.
#       b. Call `ssh root@TARGET_IP systemctl start aperod-node` to restart
#          the target node.
#       c. NOT call `systemctl start aperod-node` locally for the source
#          (source was never stopped → no restart needed).
#
#  2. Rsync interrupted mid-transfer (_PUSH_RSYNC_STARTED=1)
#     When rsync exits non-zero (partial / interrupted copy), the trap must:
#       a. Print the [TRAP] banner.
#       b. Call `systemctl start aperod-node` to restart the source node
#          (which was stopped before rsync).
#       c. NOT call `ssh root@TARGET_IP systemctl start aperod-node` for the
#          target — data may be partially overwritten; the sentinel must remain.
#       d. Print the sentinel / partial-data warning in the output.
#
#  3. Successful run → trap cleared, NOT fired on clean exit
#     After a clean push completes, `trap - EXIT ERR` is called before the
#     process exits.  The ssh target-restart command must NOT be called a
#     second time by the trap.
#
#  External tools required: bash, python3
#
#  Run from anywhere:
#    bash blockchain/deploy/test-join-network-push-trap.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOIN_SH="${SCRIPT_DIR}/join-network.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'

PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Pre-flight ────────────────────────────────────────────────────────────────
if [[ ! -f "${JOIN_SH}" ]]; then
  echo -e "${RED}[ERR]${NC}  join-network.sh not found at: ${JOIN_SH}" >&2
  exit 1
fi

if ! command -v python3 &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  python3 not found in PATH — skipping." >&2
  exit 0
fi

TARGET_IP="192.0.2.50"   # TEST-NET-1 — never routes
PRIMARY_IP="192.0.2.1"

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# make_stub BIN_DIR CMD BODY
#   Creates a stub executable at BIN_DIR/CMD that runs BODY as bash.
# ---------------------------------------------------------------------------
make_stub() {
  local dir="$1" cmd="$2" body="$3"
  mkdir -p "$dir"
  printf '#!/usr/bin/env bash\n%s\n' "$body" >"${dir}/${cmd}"
  chmod +x "${dir}/${cmd}"
}

# ---------------------------------------------------------------------------
# run_push BIN_DIR PRIMARY_DATA [EXTRA_ENV...]
#   Runs join-network.sh TARGET_IP with the given stubs in push-mode.
#   Sets LAST_OUTPUT and LAST_EXIT.
# ---------------------------------------------------------------------------
LAST_OUTPUT=""
LAST_EXIT=0

run_push() {
  local bdir="$1" pdata="$2"
  shift 2
  LAST_EXIT=0
  LAST_OUTPUT=$(
    env \
      PATH="${bdir}:${PATH}" \
      PRIMARY_DATA_DIR="${pdata}" \
      PRIMARY_IP="${PRIMARY_IP}" \
      SECONDARY_DATA_DIR="/var/lib/aperod-test" \
      SECONDARY_NODE_YAML="/etc/aperod-test/node.yaml" \
      SECONDARY_NODE_CONFIG_SH="/opt/aperod-test/node-config.sh" \
      HEALTH_MAX_ATTEMPTS=3 \
      HEALTH_WAIT_SECS=0 \
      "$@" \
    bash "${JOIN_SH}" "${TARGET_IP}" 2>&1
  ) || LAST_EXIT=$?
}

# =============================================================================
# ── SUITE 1: Interrupted after Step 1 (target stopped) but before Step 2
#    (source never stopped) → trap restarts target, NOT source ────────────────
# =============================================================================
section "Interrupted after target stop (Step 1) before source stop (Step 2) — trap restarts target only"

S1_DIR="${TMPDIR_TEST}/s1"
S1_DATA="${S1_DIR}/data"
S1_BIN="${S1_DIR}/bin"

mkdir -p "${S1_DATA}/chain.db"
touch "${S1_DATA}/chain.db/CURRENT"

# Sentinels
S1_TARGET_RESTART="${S1_DIR}/target-start-called"   # ssh … systemctl start aperod-node
S1_SOURCE_RESTART="${S1_DIR}/source-start-called"   # local systemctl start aperod-node

# systemctl stub:
#   stop aperod-node  → exit 1 (simulates failure, triggers die at step 2)
#   is-active         → exit 1 (not running, not needed but safe)
#   start aperod-node → touch sentinel (trap would call this for source if buggy)
make_stub "${S1_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 1 ;;
  *'is-active'*)
    exit 1 ;;
  *'start aperod-node'*)
    touch '${S1_SOURCE_RESTART}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub:
#   disable --now / stop  → echo 'stopped', exit 0 (step 1: target stops OK)
#   systemctl start       → touch sentinel, exit 0 (trap restarts target)
#   anything else         → exit 0
make_stub "${S1_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -qE 'disable|stop aperod-node'; then
  echo 'stopped'
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${S1_TARGET_RESTART}'
  echo 'started'
  exit 0
else
  cat >/dev/null 2>/dev/null || true
  echo 'ok'
  exit 0
fi
"

make_stub "${S1_BIN}" "rsync" 'exit 0'
make_stub "${S1_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S1_BIN}" "chown" 'exit 0'
make_stub "${S1_BIN}" "sleep" 'exit 0'
make_stub "${S1_BIN}" "hostname" "echo '${PRIMARY_IP}'"
make_stub "${S1_BIN}" "verify-dropin.sh" 'exit 0'

run_push "${S1_BIN}" "${S1_DATA}"

# ── T1: script exits non-zero ────────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T1: script exited non-zero (${LAST_EXIT}) when source-stop failed at step 2"
else
  fail "T1: expected non-zero exit after source-stop failure but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── T2: [TRAP] banner present ─────────────────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T2: [TRAP] banner printed in output"
else
  fail "T2: expected [TRAP] banner in output. Got:\n${LAST_OUTPUT}"
fi

# ── T3: ssh start called for target (target was stopped at step 1) ────────────
if [[ -f "${S1_TARGET_RESTART}" ]]; then
  pass "T3: trap called ssh systemctl start aperod-node to restart target node"
else
  fail "T3: trap did not restart target node via ssh. Output:\n${LAST_OUTPUT}"
fi

# ── T4: local start NOT called for source (source was never stopped) ──────────
if [[ ! -f "${S1_SOURCE_RESTART}" ]]; then
  pass "T4: trap did NOT call local systemctl start aperod-node (source was never stopped)"
else
  fail "T4: trap erroneously called local systemctl start aperod-node when source was never stopped. Output:\n${LAST_OUTPUT}"
fi

# ── T5: output does NOT mention source-node restart ───────────────────────────
# The source restart message contains "ИСТОЧНИКЕ" (meaning "source" in Russian).
if ! echo "${LAST_OUTPUT}" | grep -q '\[TRAP\].*ИСТОЧНИКЕ\|\[TRAP\].*источнике'; then
  pass "T5: [TRAP] output does not reference source-node restart (source was never stopped)"
else
  fail "T5: [TRAP] output incorrectly references source-node restart. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SUITE 2: Rsync interrupted mid-transfer (_PUSH_RSYNC_STARTED=1) ──────────
#    Source was stopped at step 2 → trap restarts source.
#    Target data may be partial → trap must NOT restart target.
# =============================================================================
section "Rsync interrupted mid-transfer — trap restarts source, does NOT restart target"

S2_DIR="${TMPDIR_TEST}/s2"
S2_DATA="${S2_DIR}/data"
S2_BIN="${S2_DIR}/bin"

mkdir -p "${S2_DATA}/chain.db"
touch "${S2_DATA}/chain.db/CURRENT"

# Sentinels
S2_TARGET_RESTART="${S2_DIR}/target-start-called"   # ssh … systemctl start aperod-node
S2_SOURCE_RESTART="${S2_DIR}/source-start-called"   # local systemctl start aperod-node
S2_RSYNC_CALLED="${S2_DIR}/rsync-called"

# systemctl stub:
#   stop aperod-node  → exit 0 (step 2: source stops OK → _SOURCE_STOPPED=1)
#   is-active         → exit 1 (not running → wait loop exits immediately)
#   start aperod-node → touch sentinel (trap restarts source); exit 0
make_stub "${S2_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'start aperod-node'*)
    touch '${S2_SOURCE_RESTART}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub:
#   disable --now / stop → 'stopped', exit 0 (step 1: target stops)
#   sentinel write       → 'sentinel written', exit 0 (step 2b)
#   systemctl start      → touch sentinel, exit 0 (trap would restart target if buggy)
#   anything else        → exit 0
make_stub "${S2_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -qE 'disable|stop aperod-node'; then
  echo 'stopped'
  exit 0
elif echo \"\$CMD\" | grep -q 'rsync-in-progress'; then
  echo 'sentinel written'
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${S2_TARGET_RESTART}'
  echo 'started'
  exit 0
else
  cat >/dev/null 2>/dev/null || true
  echo 'ok'
  exit 0
fi
"

# rsync stub: exit 23 (simulates partial / interrupted transfer)
make_stub "${S2_BIN}" "rsync" "
touch '${S2_RSYNC_CALLED}'
exit 23
"

make_stub "${S2_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S2_BIN}" "chown" 'exit 0'
make_stub "${S2_BIN}" "sleep" 'exit 0'
make_stub "${S2_BIN}" "hostname" "echo '${PRIMARY_IP}'"
make_stub "${S2_BIN}" "verify-dropin.sh" 'exit 0'

run_push "${S2_BIN}" "${S2_DATA}"

# ── T6: script exits non-zero after rsync failure ────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T6: script exited non-zero (${LAST_EXIT}) when rsync returned code 23"
else
  fail "T6: expected non-zero exit after rsync failure but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── T7: rsync stub was actually invoked ──────────────────────────────────────
if [[ -f "${S2_RSYNC_CALLED}" ]]; then
  pass "T7: rsync stub was called (confirming the failure step was exercised)"
else
  fail "T7: rsync stub was never called — test did not reach the rsync step. Output:\n${LAST_OUTPUT}"
fi

# ── T8: [TRAP] banner present ─────────────────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T8: [TRAP] banner printed in output after rsync failure"
else
  fail "T8: expected [TRAP] banner in output. Got:\n${LAST_OUTPUT}"
fi

# ── T9: source node restarted by trap ────────────────────────────────────────
if [[ -f "${S2_SOURCE_RESTART}" ]]; then
  pass "T9: trap called local systemctl start aperod-node to restart source node"
else
  fail "T9: trap did not restart source node. Output:\n${LAST_OUTPUT}"
fi

# ── T10: target NOT restarted (data may be partial) ──────────────────────────
if [[ ! -f "${S2_TARGET_RESTART}" ]]; then
  pass "T10: trap correctly did NOT restart target node when rsync was interrupted (partial data)"
else
  fail "T10: trap erroneously restarted target node while rsync was in progress (partial data). Output:\n${LAST_OUTPUT}"
fi

# ── T11: output warns about partial state / sentinel ─────────────────────────
if echo "${LAST_OUTPUT}" | grep -qi 'sentinel\|partial\|частичн\|rsync-in-progress\|rsync был прерван'; then
  pass "T11: output warns about partial rsync state and sentinel"
else
  fail "T11: expected output warning about partial data / sentinel. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SUITE 3: Interrupted after Step 1 only via SSH disconnect
#    (ssh for target disable exits non-zero; _TARGET_STOPPED was already set 1)
#    Source is still running → trap restarts target, NOT source ───────────────
# =============================================================================
section "SSH disconnect during target-stop (step 1) — trap restarts target, NOT source"

S3_DIR="${TMPDIR_TEST}/s3"
S3_DATA="${S3_DIR}/data"
S3_BIN="${S3_DIR}/bin"

mkdir -p "${S3_DATA}/chain.db"
touch "${S3_DATA}/chain.db/CURRENT"

# Sentinels
S3_TARGET_RESTART="${S3_DIR}/target-start-called"
S3_SOURCE_RESTART="${S3_DIR}/source-start-called"

# systemctl stub:
#   stop aperod-node  → never reached (script exits before step 2)
#   start aperod-node → touch sentinel (trap would restart source if buggy)
make_stub "${S3_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'start aperod-node'*)
    touch '${S3_SOURCE_RESTART}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub:
#   disable --now → exit 1 (simulates SSH disconnect / remote failure)
#     Note: _TARGET_STOPPED=1 is set BEFORE this ssh call, so the trap fires
#     with _TARGET_STOPPED=1 and _SOURCE_STOPPED=0 (step 2 was never reached).
#   OR second attempt stop → exit 1 as well
#   systemctl start → touch sentinel, exit 0 (trap restart of target)
#   anything else   → exit 0
make_stub "${S3_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -qE 'disable|stop aperod-node'; then
  cat >/dev/null 2>/dev/null || true
  exit 1
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${S3_TARGET_RESTART}'
  echo 'started'
  exit 0
else
  cat >/dev/null 2>/dev/null || true
  echo 'ok'
  exit 0
fi
"

make_stub "${S3_BIN}" "rsync" 'exit 0'
make_stub "${S3_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S3_BIN}" "chown" 'exit 0'
make_stub "${S3_BIN}" "sleep" 'exit 0'
make_stub "${S3_BIN}" "hostname" "echo '${PRIMARY_IP}'"
make_stub "${S3_BIN}" "verify-dropin.sh" 'exit 0'

run_push "${S3_BIN}" "${S3_DATA}"

# ── T12: script exits non-zero when SSH fails during target-stop ──────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T12: script exited non-zero (${LAST_EXIT}) when SSH disconnected during target-stop"
else
  fail "T12: expected non-zero exit after SSH disconnect but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── T13: [TRAP] banner present ───────────────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T13: [TRAP] banner printed in output after SSH disconnect"
else
  fail "T13: expected [TRAP] banner in output. Got:\n${LAST_OUTPUT}"
fi

# ── T14: trap restarted target via ssh ───────────────────────────────────────
# _TARGET_STOPPED=1 was set BEFORE the failing ssh call → trap must attempt restart.
if [[ -f "${S3_TARGET_RESTART}" ]]; then
  pass "T14: trap called ssh systemctl start aperod-node to restart target (set before failing ssh)"
else
  fail "T14: trap did not restart target node. Output:\n${LAST_OUTPUT}"
fi

# ── T15: source NOT restarted (source was never stopped) ─────────────────────
if [[ ! -f "${S3_SOURCE_RESTART}" ]]; then
  pass "T15: trap did NOT call local systemctl start aperod-node (source was never stopped)"
else
  fail "T15: trap erroneously restarted source when it was never stopped. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SUITE 4: Successful run → trap cleared, NOT fired on clean exit ───────────
# =============================================================================
section "Successful push — trap cleared before exit, ssh target-restart not called by trap"

S4_DIR="${TMPDIR_TEST}/s4"
S4_DATA="${S4_DIR}/data"
S4_BIN="${S4_DIR}/bin"

mkdir -p "${S4_DATA}/chain.db"
touch "${S4_DATA}/chain.db/CURRENT"

# Count ssh target-start calls.  On a clean success: exactly 1 call from step 6
# (`systemctl enable --now aperod-node` inside the remote SSH command).
# The trap must NOT add a second call.
S4_TARGET_START_COUNT="${S4_DIR}/target-start-count"

# systemctl stub:
#   stop aperod-node         → 0 (step 2: source stops → _SOURCE_STOPPED=1)
#   is-active                → 1 (not running → wait loop exits)
#   start aperod-node        → 0 (step after rsync: source restarted)
#   anything else            → 0
make_stub "${S4_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'start aperod-node'*)
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub: all steps succeed cleanly.
#   disable --now / stop → 'stopped', exit 0 (step 1 target stop)
#   rsync-in-progress    → 'sentinel ok', exit 0 (steps 2b, 3b)
#   rm -f p2p_identity   → 'removed', exit 0 (step 4)
#   network/stats curl   → JSON with height > 0 (step 7 health check)
#   enable --now         → touch count, echo 'started', exit 0 (step 6)
#   systemctl show       → valid drop-in env output (verify-dropin.sh check)
#   test -f drop-in      → 'yes' (verify-dropin.sh file existence check)
#   systemctl start      → count if called by trap (should not happen)
#   anything else        → exit 0
make_stub "${S4_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -qE 'disable|stop aperod-node'; then
  echo 'stopped'
  exit 0
elif echo \"\$CMD\" | grep -q 'rsync-in-progress'; then
  echo 'sentinel ok'
  exit 0
elif echo \"\$CMD\" | grep -q 'p2p_identity'; then
  echo 'removed'
  exit 0
elif echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"height\":9001,\"peer_count\":3}'
  exit 0
elif echo \"\$CMD\" | grep -q 'enable --now'; then
  COUNT=0
  [[ -f '${S4_TARGET_START_COUNT}' ]] && COUNT=\$(cat '${S4_TARGET_START_COUNT}')
  COUNT=\$((COUNT + 1))
  printf '%s' \"\$COUNT\" >'${S4_TARGET_START_COUNT}'
  echo 'started'
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl show aperod-node'; then
  # Return valid drop-in output so verify-dropin.sh passes its checks.
  printf 'Environment=GOMEMLIMIT=5905580032\nTimeoutStopUSec=15min\n'
  exit 0
elif echo \"\$CMD\" | grep -q 'test -f'; then
  # verify-dropin.sh checks whether drop-in conf files exist.
  echo 'yes'
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  # Called by trap — should NOT happen on clean exit.
  COUNT=0
  [[ -f '${S4_TARGET_START_COUNT}' ]] && COUNT=\$(cat '${S4_TARGET_START_COUNT}')
  COUNT=\$((COUNT + 1))
  printf '%s' \"\$COUNT\" >'${S4_TARGET_START_COUNT}'
  echo 'started'
  exit 0
else
  cat >/dev/null 2>/dev/null || true
  echo 'ok'
  exit 0
fi
"

# rsync succeeds.
make_stub "${S4_BIN}" "rsync" 'exit 0'

make_stub "${S4_BIN}" "curl"  "echo '{\"height\":9001,\"peer_count\":3}'; exit 0"
make_stub "${S4_BIN}" "chown" 'exit 0'
make_stub "${S4_BIN}" "sleep" 'exit 0'
make_stub "${S4_BIN}" "hostname" "echo '${PRIMARY_IP}'"

# verify-dropin.sh stub: success (dropin gate passes)
make_stub "${S4_BIN}" "verify-dropin.sh" 'exit 0'
# Also stub the full path variant used by join-network.sh (bash "${_SCRIPT_DIR}/verify-dropin.sh")
# join-network.sh calls: bash "${_SCRIPT_DIR}/verify-dropin.sh" "${TARGET_IP}"
# Since bash is not overridable via PATH easily, we write a verify-dropin.sh stub
# in the same SCRIPT_DIR — but we cannot override that.  Instead we confirm the
# script reaches the health-wait step (height > 0 → exit 0).

run_push "${S4_BIN}" "${S4_DATA}"

# ── T16: script exits 0 on clean push ────────────────────────────────────────
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "T16: script exited 0 on successful push"
else
  fail "T16: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# ── T17: [TRAP] banner must NOT appear in output of a clean run ───────────────
if ! echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T17: [TRAP] banner absent from output (trap was cleared before exit)"
else
  fail "T17: [TRAP] found in output of a successful run — trap fired when it should not have. Output:\n${LAST_OUTPUT}"
fi

# ── T18: ssh target start/enable called exactly once (from step 6 only) ───────
S4_COUNT=0
[[ -f "${S4_TARGET_START_COUNT}" ]] && S4_COUNT=$(cat "${S4_TARGET_START_COUNT}")
if [[ "${S4_COUNT}" -eq 1 ]]; then
  pass "T18: ssh enable/start aperod-node called exactly once on target (step 6, not by trap)"
elif [[ "${S4_COUNT}" -eq 0 ]]; then
  fail "T18: ssh enable/start was never called — step 6 (enable --now) missing. Output:\n${LAST_OUTPUT}"
else
  fail "T18: ssh enable/start called ${S4_COUNT} times — trap fired extra restart on clean exit. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── Summary ───────────────────────────────────────────────────────────────────
# =============================================================================
echo
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo

if [[ ${FAIL} -gt 0 ]]; then
  exit 1
fi
exit 0
