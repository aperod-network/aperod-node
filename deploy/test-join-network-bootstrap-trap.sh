#!/usr/bin/env bash
# =============================================================================
#  test-join-network-bootstrap-trap.sh — Tests for the EXIT/ERR trap in the
#  --bootstrap-from (bootstrap) mode of join-network.sh.
#
#  Focused on two properties of the cleanup trap:
#
#  1. Rsync failure → trap fires
#     When rsync exits non-zero (simulating an interrupted or partial copy),
#     the _bootstrap_cleanup trap must:
#       a. Call `systemctl start aperod-node` to restart the local node.
#       b. Call `ssh root@VALIDATOR_IP systemctl start aperod-node` to restart
#          the remote validator that was stopped before the rsync.
#       c. Print the [TRAP] banner in the output.
#
#  2. Successful run → trap is cleared, NOT fired on exit 0
#     After a clean bootstrap completes, `trap - EXIT ERR` is called before
#     the process exits.  The local `systemctl start aperod-node` must NOT
#     be called a second time by the trap (only the normal `systemctl
#     enable --now aperod-node` at step 8 counts as the first call).
#     The [TRAP] banner must be absent from the output.
#
#  External tools required: bash, python3
#
#  Run from anywhere:
#    bash blockchain/deploy/test-join-network-bootstrap-trap.sh
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

VALIDATOR_IP="192.0.2.2"   # TEST-NET-1 — never routes

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
# run_bootstrap BIN_DIR LOCAL_DATA LOCAL_YAML LOCAL_CONFIG [EXTRA_ENV...]
#   Runs join-network.sh --bootstrap-from=VALIDATOR_IP with the given stubs.
#   Sets LAST_OUTPUT and LAST_EXIT.
# ---------------------------------------------------------------------------
LAST_OUTPUT=""
LAST_EXIT=0

run_bootstrap() {
  local bdir="$1" ldata="$2" lyaml="$3" lconfig="$4"
  shift 4
  LAST_EXIT=0
  LAST_OUTPUT=$(
    env \
      PATH="${bdir}:${PATH}" \
      SECONDARY_DATA_DIR="${ldata}" \
      LOCAL_DATA_DIR="${ldata}" \
      LOCAL_NODE_YAML="${lyaml}" \
      LOCAL_NODE_CONFIG_SH="${lconfig}" \
      PRIMARY_DATA_DIR="${ldata}" \
      VALIDATOR_DATA_DIR="${ldata}" \
      HEALTH_MAX_ATTEMPTS=5 \
      HEALTH_WAIT_SECS=0 \
      "$@" \
    bash "${JOIN_SH}" "--bootstrap-from=${VALIDATOR_IP}" 2>&1
  ) || LAST_EXIT=$?
}

# =============================================================================
# ── SUITE 1: rsync fails → trap fires and restarts BOTH nodes ─────────────────
# =============================================================================
section "Rsync failure — trap fires, calls systemctl start aperod-node (local) and ssh validator restart"

S1_DIR="${TMPDIR_TEST}/s1"
S1_DATA="${S1_DIR}/data"
S1_BIN="${S1_DIR}/bin"
S1_YAML="${S1_DIR}/node.yaml"
S1_CONFIG="${S1_DIR}/node-config.sh"

mkdir -p "${S1_DATA}/chain.db"
touch "${S1_DATA}/chain.db/CURRENT"
printf 'network: testnet\n' >"${S1_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${S1_CONFIG}"; chmod +x "${S1_CONFIG}"

# Sentinels — each stub touches its file when the relevant command is called.
S1_LOCAL_RESTART="${S1_DIR}/local-start-called"   # systemctl start aperod-node
S1_VAL_RESTART="${S1_DIR}/val-start-called"        # ssh … systemctl start aperod-node
S1_RSYNC_CALLED="${S1_DIR}/rsync-called"

# systemctl stub:
#   stop aperod-node  → 0  (stop succeeds → _BS_LOCAL_STOPPED set to 1)
#   is-active         → 1  (service reports it is not active → loop exits)
#   start aperod-node → 0  (trap restart succeeds); touch sentinel on start
make_stub "${S1_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'start aperod-node'*)
    touch '${S1_LOCAL_RESTART}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub:
#   network/stats              → validator tip_height JSON (step 1)
#   bash (heredoc remote stop) → "stopped" (step 3 remote stop succeeds → _BS_VALIDATOR_STOPPED=1)
#   systemctl start            → "started" (trap restart); touch sentinel
make_stub "${S1_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":1000,\"height\":1000,\"peer_count\":2}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${S1_VAL_RESTART}'
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'   # remote stop succeeds
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

# rsync stub: exits 1 on first invocation → triggers set -e → EXIT trap fires.
make_stub "${S1_BIN}" "rsync" "
touch '${S1_RSYNC_CALLED}'
exit 1
"

make_stub "${S1_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S1_BIN}" "chown" 'exit 0'
make_stub "${S1_BIN}" "sleep" 'exit 0'

run_bootstrap "${S1_BIN}" "${S1_DATA}" "${S1_YAML}" "${S1_CONFIG}"

# ── T1: script exits non-zero after rsync failure ────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T1: script exited non-zero (${LAST_EXIT}) when rsync failed"
else
  fail "T1: expected non-zero exit after rsync failure but got 0"
fi

# ── T2: rsync stub was actually invoked ──────────────────────────────────────
if [[ -f "${S1_RSYNC_CALLED}" ]]; then
  pass "T2: rsync was called (confirming the failure step was exercised)"
else
  fail "T2: rsync stub was never called — test did not reach the rsync step"
fi

# ── T3: [TRAP] banner present in output ──────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T3: [TRAP] banner printed in output after rsync failure"
else
  fail "T3: expected [TRAP] banner in output. Got:\n${LAST_OUTPUT}"
fi

# ── T4: systemctl start aperod-node called by trap (local node restart) ───────
if [[ -f "${S1_LOCAL_RESTART}" ]]; then
  pass "T4: systemctl start aperod-node called by trap to restart local node"
else
  fail "T4: trap did not call systemctl start aperod-node for local node. Output:\n${LAST_OUTPUT}"
fi

# ── T5: ssh validator restart called by trap ──────────────────────────────────
if [[ -f "${S1_VAL_RESTART}" ]]; then
  pass "T5: ssh systemctl start aperod-node called by trap to restart validator"
else
  fail "T5: trap did not call ssh restart for validator. Output:\n${LAST_OUTPUT}"
fi

# ── T6: trap message mentions the validator (by IP or 'валидатор') ────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\].*валидатор\|\[TRAP\].*validator\|\[TRAP\].*Перезапуск'; then
  pass "T6: [TRAP] output mentions validator restart attempt"
else
  # Allow broader match — just check TRAP + validator IP present somewhere
  if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]' && \
     echo "${LAST_OUTPUT}" | grep -qi 'валидатор\|validator\|192\.0\.2\.2'; then
    pass "T6: [TRAP] present and validator referenced in output"
  else
    fail "T6: expected [TRAP] output referencing validator. Got:\n${LAST_OUTPUT}"
  fi
fi

# =============================================================================
# ── SUITE 2: successful run → trap cleared, NOT fired on clean exit ────────────
# =============================================================================
section "Successful run — trap cleared before exit, systemctl start not called by trap"

S2_DIR="${TMPDIR_TEST}/s2"
S2_DATA="${S2_DIR}/data"
S2_BIN="${S2_DIR}/bin"
S2_YAML="${S2_DIR}/node.yaml"
S2_CONFIG="${S2_DIR}/node-config.sh"

mkdir -p "${S2_DATA}/chain.db"
touch "${S2_DATA}/chain.db/CURRENT"
# Provide a fake snapshot file so SNAP_HEIGHT detection works.
touch "${S2_DATA}/snapshot-v2-5000.json.gz"
printf 'network: testnet\n' >"${S2_YAML}"

# node-config.sh stub: no-op (records bootnode + tolerance calls but does not
# need PyYAML).
printf '#!/usr/bin/env bash\nexit 0\n' >"${S2_CONFIG}"; chmod +x "${S2_CONFIG}"

# Count how many times systemctl start aperod-node is called.
# On a clean success: exactly 1 call from `systemctl enable --now aperod-node`
# (step 8).  The trap must NOT add a second call.
S2_LOCAL_START_COUNT="${S2_DIR}/local-start-count"

make_stub "${S2_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'enable --now aperod-node'*|*'start aperod-node'*)
    COUNT=0
    [[ -f '${S2_LOCAL_START_COUNT}' ]] && COUNT=\$(cat '${S2_LOCAL_START_COUNT}')
    COUNT=\$((COUNT + 1))
    printf '%s' \"\$COUNT\" >'${S2_LOCAL_START_COUNT}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub: clean success for all steps.
make_stub "${S2_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":6000,\"height\":6000,\"peer_count\":3}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

# rsync succeeds.
make_stub "${S2_BIN}" "rsync" 'exit 0'

# curl: local API reports height > 0 immediately.
make_stub "${S2_BIN}" "curl" "
echo '{\"height\":5001,\"peer_count\":2,\"syncing\":false}'
exit 0
"

make_stub "${S2_BIN}" "chown" 'exit 0'
make_stub "${S2_BIN}" "sleep" 'exit 0'

run_bootstrap "${S2_BIN}" "${S2_DATA}" "${S2_YAML}" "${S2_CONFIG}"

# ── T7: script exits 0 on clean run ──────────────────────────────────────────
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "T7: script exited 0 on successful bootstrap"
else
  fail "T7: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# ── T8: [TRAP] banner must NOT appear in output of a clean run ────────────────
if ! echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T8: [TRAP] banner absent from output (trap was cleared before exit)"
else
  fail "T8: [TRAP] found in output of a successful run — trap fired when it should not have. Output:\n${LAST_OUTPUT}"
fi

# ── T9: systemctl start/enable called exactly once (from step 8 only) ─────────
# The trap must not add a second invocation.
S2_COUNT=0
[[ -f "${S2_LOCAL_START_COUNT}" ]] && S2_COUNT=$(cat "${S2_LOCAL_START_COUNT}")
if [[ "${S2_COUNT}" -eq 1 ]]; then
  pass "T9: systemctl start/enable aperod-node called exactly once (step 8, not by trap)"
elif [[ "${S2_COUNT}" -eq 0 ]]; then
  fail "T9: systemctl start/enable was never called — step 8 (enable --now) missing. Output:\n${LAST_OUTPUT}"
else
  fail "T9: systemctl start/enable called ${S2_COUNT} times — trap fired extra restart on clean exit. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SUITE 3: chain.db rsync fails, snapshot rsync succeeds
#    (first rsync command exits non-zero → same trap behavior) ─────────────────
# =============================================================================
section "Chain.db rsync exits non-zero → trap fires, validator and local node restarted"

S3_DIR="${TMPDIR_TEST}/s3"
S3_DATA="${S3_DIR}/data"
S3_BIN="${S3_DIR}/bin"
S3_YAML="${S3_DIR}/node.yaml"
S3_CONFIG="${S3_DIR}/node-config.sh"

mkdir -p "${S3_DATA}/chain.db"
touch "${S3_DATA}/chain.db/CURRENT"
printf 'network: testnet\n' >"${S3_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${S3_CONFIG}"; chmod +x "${S3_CONFIG}"

S3_LOCAL_RESTART="${S3_DIR}/local-start-called"
S3_VAL_RESTART="${S3_DIR}/val-start-called"

make_stub "${S3_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*)
    touch '${S3_LOCAL_RESTART}'
    exit 0 ;;
  *)                     exit 0 ;;
esac
"

make_stub "${S3_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":2000,\"height\":2000,\"peer_count\":1}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${S3_VAL_RESTART}'
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

# rsync: fail with code 11 (simulates partial / interrupted transfer)
make_stub "${S3_BIN}" "rsync" 'exit 11'
make_stub "${S3_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S3_BIN}" "chown" 'exit 0'
make_stub "${S3_BIN}" "sleep" 'exit 0'

run_bootstrap "${S3_BIN}" "${S3_DATA}" "${S3_YAML}" "${S3_CONFIG}"

# ── T10: script exits non-zero ───────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T10: script exited non-zero (${LAST_EXIT}) when chain.db rsync returned code 11"
else
  fail "T10: expected non-zero exit when rsync returned code 11 but got 0"
fi

# ── T11: systemctl start aperod-node called by trap ──────────────────────────
if [[ -f "${S3_LOCAL_RESTART}" ]]; then
  pass "T11: trap called systemctl start aperod-node after chain.db rsync exit 11"
else
  fail "T11: trap did not call systemctl start aperod-node. Output:\n${LAST_OUTPUT}"
fi

# ── T12: ssh validator restart called by trap ─────────────────────────────────
if [[ -f "${S3_VAL_RESTART}" ]]; then
  pass "T12: trap called ssh systemctl start aperod-node for validator after rsync exit 11"
else
  fail "T12: trap did not call ssh validator restart. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SUITE 4: SSH disconnects during remote-stop heredoc → trap still fires ────
#
#  The critical property under test:
#    _BS_VALIDATOR_STOPPED is set to 1 BEFORE the `ssh … bash` heredoc call
#    (join-network.sh step 3).  If the SSH connection drops mid-stop (ssh
#    exits non-zero), the ERR/EXIT trap must still:
#      a. Print the [TRAP] banner.
#      b. Call `ssh root@VALIDATOR_IP systemctl start aperod-node`
#         (validator restart — _BS_VALIDATOR_STOPPED=1).
#      c. Call `systemctl start aperod-node` for the local node
#         (_BS_LOCAL_STOPPED=1 and _BS_RSYNC_STARTED=0 → data untouched).
# =============================================================================
section "SSH disconnect during remote-stop heredoc — trap fires, restarts both nodes"

S4_DIR="${TMPDIR_TEST}/s4"
S4_DATA="${S4_DIR}/data"
S4_BIN="${S4_DIR}/bin"
S4_YAML="${S4_DIR}/node.yaml"
S4_CONFIG="${S4_DIR}/node-config.sh"

mkdir -p "${S4_DATA}/chain.db"
touch "${S4_DATA}/chain.db/CURRENT"
printf 'network: testnet\n' >"${S4_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${S4_CONFIG}"; chmod +x "${S4_CONFIG}"

# Sentinels
S4_LOCAL_RESTART="${S4_DIR}/local-start-called"
S4_VAL_RESTART="${S4_DIR}/val-start-called"

# systemctl stub:
#   stop aperod-node  → 0  (step 2: local node stops → _BS_LOCAL_STOPPED=1)
#   is-active         → 1  (not active → wait loop exits immediately)
#   start aperod-node → 0  (trap restart); touch sentinel
make_stub "${S4_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'start aperod-node'*)
    touch '${S4_LOCAL_RESTART}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub:
#   network/stats              → JSON (step 1 tip_height read)
#   [ -d … ]                   → exit 0 (steps 1b dir checks)
#   bash (heredoc remote-stop) → exit 1 (simulates SSH disconnect mid-stop)
#   systemctl start            → touch sentinel + exit 0 (trap restart of validator)
#   anything else              → exit 0
make_stub "${S4_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":3000,\"height\":3000,\"peer_count\":2}'
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${S4_VAL_RESTART}'
  echo 'started'
  exit 0
elif [[ \"\$CMD\" == 'bash' ]]; then
  # Consume the heredoc stdin so the pipe doesn't block, then simulate
  # an SSH connection drop by returning a non-zero exit code.
  cat >/dev/null
  exit 1
else
  cat >/dev/null
  exit 0
fi
"

# rsync stub: should never be reached (SSH fails at step 3 before rsync).
make_stub "${S4_BIN}" "rsync" 'exit 0'
make_stub "${S4_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S4_BIN}" "chown" 'exit 0'
make_stub "${S4_BIN}" "sleep" 'exit 0'

run_bootstrap "${S4_BIN}" "${S4_DATA}" "${S4_YAML}" "${S4_CONFIG}"

# ── T13: script exits non-zero when SSH drops during remote-stop ──────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T13: script exited non-zero (${LAST_EXIT}) when SSH disconnected during remote-stop"
else
  fail "T13: expected non-zero exit after SSH disconnect but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── T14: [TRAP] banner present in output ─────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T14: [TRAP] banner printed in output after SSH disconnect"
else
  fail "T14: expected [TRAP] banner in output. Got:\n${LAST_OUTPUT}"
fi

# ── T15: systemctl start aperod-node called by trap (local node restart) ──────
# _BS_LOCAL_STOPPED=1 and _BS_RSYNC_STARTED=0 → data is intact → safe to restart.
if [[ -f "${S4_LOCAL_RESTART}" ]]; then
  pass "T15: trap called systemctl start aperod-node to restart local node"
else
  fail "T15: trap did not call systemctl start aperod-node for local node. Output:\n${LAST_OUTPUT}"
fi

# ── T16: ssh validator restart called by trap ─────────────────────────────────
# _BS_VALIDATOR_STOPPED=1 was set BEFORE the failing ssh bash call → trap must
# attempt validator restart even though the stop heredoc itself failed.
if [[ -f "${S4_VAL_RESTART}" ]]; then
  pass "T16: trap called ssh systemctl start aperod-node to restart validator"
else
  fail "T16: trap did not call ssh restart for validator. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SUITE 5: `systemctl enable --now aperod-node` fails at step 8 ─────────────
#
#  Critical property:
#    _BS_LOCAL_STOPPED is set to 0 and `trap - EXIT ERR` is called ONLY after
#    `systemctl enable --now aperod-node` succeeds.  If the command fails the
#    script exits immediately (set -euo pipefail) while the trap is still
#    active and _BS_LOCAL_STOPPED is still 1.  The cleanup trap must therefore:
#      a. Print the [TRAP] banner.
#      b. Call `systemctl start aperod-node` to restart the local relay that
#         was stopped in step 2.
#    The validator is already back up from step 5, so no ssh restart is needed.
# =============================================================================
section "systemctl enable --now aperod-node fails at step 8 — trap fires, local node restarted"

S5_DIR="${TMPDIR_TEST}/s5"
S5_DATA="${S5_DIR}/data"
S5_BIN="${S5_DIR}/bin"
S5_YAML="${S5_DIR}/node.yaml"
S5_CONFIG="${S5_DIR}/node-config.sh"

mkdir -p "${S5_DATA}/chain.db"
touch "${S5_DATA}/chain.db/CURRENT"
# Provide a fake snapshot so SNAP_HEIGHT detection (after rsync) works.
touch "${S5_DATA}/snapshot-v2-7000.json.gz"
printf 'network: testnet\n' >"${S5_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${S5_CONFIG}"; chmod +x "${S5_CONFIG}"

# Sentinels
S5_LOCAL_RESTART="${S5_DIR}/local-start-called"
S5_ENABLE_CALLED="${S5_DIR}/enable-called"

# systemctl stub:
#   stop aperod-node         → 0   (step 2: local stops → _BS_LOCAL_STOPPED=1)
#   is-active                → 1   (not running → wait loop exits)
#   enable --now aperod-node → 1   (step 8: unit file missing / error)
#   start aperod-node        → 0   (trap restart); touch sentinel
make_stub "${S5_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    exit 0 ;;
  *'is-active'*)
    exit 1 ;;
  *'enable --now aperod-node'*)
    touch '${S5_ENABLE_CALLED}'
    exit 1 ;;
  *'start aperod-node'*)
    touch '${S5_LOCAL_RESTART}'
    exit 0 ;;
  *)
    exit 0 ;;
esac
"

# ssh stub: everything succeeds (validator starts OK at step 5; step 8 is local-only).
make_stub "${S5_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":8000,\"height\":8000,\"peer_count\":3}'
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  echo 'started'
  exit 0
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'
  exit 0
else
  cat >/dev/null
  exit 0
fi
"

# rsync succeeds — the failure happens later at step 8.
make_stub "${S5_BIN}" "rsync" 'exit 0'

make_stub "${S5_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${S5_BIN}" "chown" 'exit 0'
make_stub "${S5_BIN}" "sleep" 'exit 0'

run_bootstrap "${S5_BIN}" "${S5_DATA}" "${S5_YAML}" "${S5_CONFIG}"

# ── T17: script exits non-zero when enable --now fails ───────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T17: script exited non-zero (${LAST_EXIT}) when systemctl enable --now aperod-node failed"
else
  fail "T17: expected non-zero exit after enable --now failure but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── T18: enable --now was actually called ─────────────────────────────────────
if [[ -f "${S5_ENABLE_CALLED}" ]]; then
  pass "T18: systemctl enable --now aperod-node was called (failure step exercised)"
else
  fail "T18: systemctl enable --now was never called — test did not reach step 8. Output:\n${LAST_OUTPUT}"
fi

# ── T19: [TRAP] banner present in output ─────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q '\[TRAP\]'; then
  pass "T19: [TRAP] banner printed after enable --now failure"
else
  fail "T19: expected [TRAP] banner in output. Got:\n${LAST_OUTPUT}"
fi

# ── T20: trap called systemctl start aperod-node to restart local node ────────
# _BS_LOCAL_STOPPED=1 (set at step 2) and _BS_RSYNC_STARTED=0 (rsync completed
# and sentinel was removed at step 4c) → chain.db is consistent → safe to restart.
if [[ -f "${S5_LOCAL_RESTART}" ]]; then
  pass "T20: trap called systemctl start aperod-node to restart local relay after enable --now failure"
else
  fail "T20: trap did not call systemctl start aperod-node. Output:\n${LAST_OUTPUT}"
fi

# ── T21: error output contains a helpful diagnostic message ──────────────────
if echo "${LAST_OUTPUT}" | grep -qi 'enable --now\|unit\|install-node\|unit-файл\|завершился'; then
  pass "T21: output contains a diagnostic hint about the enable --now failure"
else
  fail "T21: expected a diagnostic message about the enable --now failure. Got:\n${LAST_OUTPUT}"
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
