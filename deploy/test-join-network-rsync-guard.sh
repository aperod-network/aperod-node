#!/usr/bin/env bash
# =============================================================================
#  test-join-network-rsync-guard.sh — Tests for the rsync-safety guard in
#  join-network.sh (step 2/7).
#
#  Two test suites:
#
#  Negative path
#  ─────────────
#  A stubbed systemctl returns non-zero for "stop aperod-node".  The script
#  must abort with a non-zero exit code and print the expected Russian-language
#  error message before any rsync is attempted.
#
#  Positive path
#  ─────────────
#  All external calls (systemctl, ssh, rsync) are replaced with stubs that
#  simulate a successful run.  The stub ssh returns valid JSON for the
#  network/stats poll so that the script detects height > 0 and peer_count > 0
#  and exits 0.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-join-network-rsync-guard.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail
# Detach stdin: ssh/systemctl stubs drain stdin (cat >/dev/null); if this test
# inherits a never-closing pipe (CI runners), those stubs would block forever.
exec </dev/null

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

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

TARGET_IP="192.0.2.1"   # TEST-NET-1 — never routes
PRIMARY_IP="127.0.0.1"

# ---------------------------------------------------------------------------
# make_stub BIN_DIR CMD BODY
#   Creates a stub executable $BIN_DIR/$CMD that runs BODY as bash.
# ---------------------------------------------------------------------------
make_stub() {
  local dir="$1" cmd="$2" body="$3"
  mkdir -p "$dir"
  printf '#!/usr/bin/env bash\n%s\n' "$body" >"${dir}/${cmd}"
  chmod +x "${dir}/${cmd}"
}

# ---------------------------------------------------------------------------
# run_join TARGET_IP BIN_DIR DATA_DIR
#   Runs join-network.sh with the given stubs prepended to PATH.
#   Captures combined stdout+stderr into LAST_OUTPUT.
#   Sets LAST_EXIT to the exit status.
# ---------------------------------------------------------------------------
LAST_OUTPUT=""
LAST_EXIT=0

run_join() {
  local tip="$1" bdir="$2" ddir="$3"
  # Reset exit tracker; capture combined output; use || to get the exit code
  # when the script fails without adding set -e to the outer shell (which
  # would cause ((PASS++)) to abort the test runner when PASS is 0).
  # PRIMARY_DATA_DIR env var overrides the hardcoded default in join-network.sh
  # (see the ${PRIMARY_DATA_DIR:-/opt/aperod/data/testnet} line added for
  # testability — mirrors how PRIMARY_IP is already handled).
  LAST_EXIT=0
  LAST_OUTPUT=$(
    PATH="${bdir}:${PATH}" \
    PRIMARY_IP="${PRIMARY_IP}" \
    PRIMARY_DATA_DIR="${ddir}" \
    bash "${JOIN_SH}" "${tip}" 2>&1 </dev/null
  ) || LAST_EXIT=$?
}

# =============================================================================
# ── NEGATIVE PATH ─────────────────────────────────────────────────────────────
# =============================================================================
section "Negative path — source node refuses to stop → script must abort"

NEG_DATA="${TMPDIR_TEST}/neg-data"
mkdir -p "${NEG_DATA}/chain.db"

NEG_BIN="${TMPDIR_TEST}/neg-bin"

# systemctl: "stop aperod-node" exits 1; everything else exits 0.
make_stub "${NEG_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*) exit 1 ;;
  *) exit 0 ;;
esac
'

# ssh: always succeeds (step 1 — disable target node — must not block the test).
make_stub "${NEG_BIN}" "ssh" '
# Drop the "root@IP" argument, treat the rest as a no-op command.
shift
echo "stopped"
exit 0
'

# rsync: must NOT be called — if it is, we count that as a failure.
# We make it exit 0 but write a sentinel file so we can detect the call.
RSYNC_SENTINEL="${TMPDIR_TEST}/neg-rsync-called"
make_stub "${NEG_BIN}" "rsync" "
touch '${RSYNC_SENTINEL}'
exit 0
"

run_join "${TARGET_IP}" "${NEG_BIN}" "${NEG_DATA}"

# ── Assertion N1: script exits non-zero ───────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "N1: script exited non-zero (${LAST_EXIT}) when source node refused to stop"
else
  fail "N1: script should exit non-zero but exited 0"
fi

# ── Assertion N2: expected error message is printed ───────────────────────────
EXPECTED_ERR="rsync"
if echo "${LAST_OUTPUT}" | grep -qi "${EXPECTED_ERR}"; then
  pass "N2: output contains rsync-safety error message"
else
  fail "N2: expected error message not found in output. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion N3: rsync was NOT invoked before the abort ─────────────────────
if [[ ! -f "${RSYNC_SENTINEL}" ]]; then
  pass "N3: rsync was not called before the abort"
else
  fail "N3: rsync was called despite source node failing to stop — LevelDB would be corrupted"
fi

# ── Assertion N4: output explicitly mentions the source-node stop failure ─────
if echo "${LAST_OUTPUT}" | grep -q "Не удалось остановить"; then
  pass "N4: output says 'Не удалось остановить' (source-stop failure message)"
else
  fail "N4: expected 'Не удалось остановить' in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── NEGATIVE PATH — 15-second timeout ────────────────────────────────────────
# =============================================================================
section "Negative path (timeout variant) — node stays active for 15 s → abort"

# systemctl stop exits 0 (pretends to work), but is-active also exits 0
# (pretends the service is still running) → the 15-s loop exhausts → abort.
TMO_DATA="${TMPDIR_TEST}/tmo-data"
mkdir -p "${TMO_DATA}/chain.db"

TMO_BIN="${TMPDIR_TEST}/tmo-bin"

make_stub "${TMO_BIN}" "systemctl" '
case "$*" in
  "stop aperod-node") exit 0 ;;          # pretend stop command succeeded …
  "is-active --quiet aperod-node") exit 0 ;;  # … but service is STILL active
  *) exit 0 ;;
esac
'

make_stub "${TMO_BIN}" "ssh" '
shift
echo "stopped"
exit 0
'

RSYNC_SENTINEL2="${TMPDIR_TEST}/tmo-rsync-called"
make_stub "${TMO_BIN}" "rsync" "
touch '${RSYNC_SENTINEL2}'
exit 0
"

# Override the sleep stub so the 15-iteration loop finishes instantly.
make_stub "${TMO_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${TMO_BIN}" "${TMO_DATA}"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "T1: script exited non-zero when node stayed active for 15 s"
else
  fail "T1: script should exit non-zero after 15-s timeout but exited 0"
fi

if [[ ! -f "${RSYNC_SENTINEL2}" ]]; then
  pass "T2: rsync was not called after timeout abort"
else
  fail "T2: rsync was called despite node still being active"
fi

if echo "${LAST_OUTPUT}" | grep -q "не остановился"; then
  pass "T3: output contains 'не остановился' (15-s timeout message)"
else
  fail "T3: expected 'не остановился' in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── POSITIVE PATH ─────────────────────────────────────────────────────────────
# =============================================================================
section "Positive path — clean rsync produces a target that starts at height > 0"

POS_DATA="${TMPDIR_TEST}/pos-data"
# Create a minimal chain.db directory so the data-dir check passes and rsync
# has something to copy (the actual content does not matter for the stub).
mkdir -p "${POS_DATA}/chain.db"
touch "${POS_DATA}/chain.db/CURRENT"

POS_BIN="${TMPDIR_TEST}/pos-bin"

# systemctl:
#   stop aperod-node  → 0 (success)
#   is-active         → 1 (service is NOT active → loop ends immediately)
#   start / enable    → 0
make_stub "${POS_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'

# ssh stub: handles every remote call.
#   • "curl.*network/stats" → valid JSON with height=54321, peer_count=2
#   • "systemctl show aperod-node" → valid GOMEMLIMIT + TimeoutStopUSec output
#                                     (satisfies verify-dropin.sh)
#   • "test -f" drop-in file checks  → "yes"
#   • anything else                  → print "stopped" / "removed" / "started"
# The heredoc for step 5/7 (bootnode injection) arrives via stdin when the
# remote command is just "bash"; the stub reads and discards stdin then exits 0.
make_stub "${POS_BIN}" "ssh" '
shift   # drop "root@IP"
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '\''{"height":54321,"peer_count":2,"syncing":false}'\''
elif echo "$CMD" | grep -q "curl"; then
  echo '\''{"ok":true}'\''
elif echo "$CMD" | grep -q "systemctl show aperod-node"; then
  echo "Environment=GOMEMLIMIT=5905580032"
  echo "TimeoutStopUSec=15min"
elif echo "$CMD" | grep -q "test -f"; then
  echo "yes"
else
  # Heredoc or other remote commands: drain stdin, print neutral success.
  cat >/dev/null
  printf "stopped\nremoved\nstarted\n"
fi
exit 0
'

# rsync: succeed silently and record invocation.
RSYNC_CALLED="${TMPDIR_TEST}/pos-rsync-called"
make_stub "${POS_BIN}" "rsync" "
touch '${RSYNC_CALLED}'
exit 0
"

# sleep: no-op so the health-wait loop exits quickly.
make_stub "${POS_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${POS_BIN}" "${POS_DATA}"

# ── Assertion P1: script exits 0 ─────────────────────────────────────────────
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "P1: script exited 0 after successful rsync"
else
  fail "P1: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# ── Assertion P2: rsync was actually called (real sync happened) ──────────────
if [[ -f "${RSYNC_CALLED}" ]]; then
  pass "P2: rsync was called during the positive-path run"
else
  fail "P2: rsync was not called — no data would have been transferred"
fi

# ── Assertion P3: height > 0 reported in output ───────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q "height=54321"; then
  pass "P3: output reports height=54321 (target started at correct height)"
else
  fail "P3: expected 'height=54321' in output. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion P4: peer_count > 0 reported in output ──────────────────────────
if echo "${LAST_OUTPUT}" | grep -q "peers=2"; then
  pass "P4: output reports peers=2 (peer_count > 0)"
else
  fail "P4: expected 'peers=2' in output. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion P5: success banner present ─────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -q "подключён к сети"; then
  pass "P5: success banner 'подключён к сети' present"
else
  fail "P5: success banner not found in output. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion P6: source node was restarted after rsync ──────────────────────
# "Перезапускаем aperod-node на источнике" must appear and the start call must
# happen — we verify the message in the output.
if echo "${LAST_OUTPUT}" | grep -q "источнике"; then
  pass "P6: output confirms source node was restarted after rsync"
else
  fail "P6: no source-restart confirmation found in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── POSITIVE PATH — API not ready on first polls, ready later ─────────────────
# =============================================================================
section "Positive path (delayed API) — height=0 on first polls, height>0 + peers>0 on later poll"
# The health-wait loop in step 7/7 polls until height > 0; this test verifies
# that when the first two polls return height=0 (API still starting up) the
# script keeps retrying and eventually succeeds.

DELAYED_DATA="${TMPDIR_TEST}/delayed-data"
mkdir -p "${DELAYED_DATA}/chain.db"
touch "${DELAYED_DATA}/chain.db/CURRENT"

DELAYED_BIN="${TMPDIR_TEST}/delayed-bin"

make_stub "${DELAYED_BIN}" "systemctl" '
case "$*" in
  *"is-active"*) exit 1 ;;
  *) exit 0 ;;
esac
'

# ssh stub: first 2 network/stats calls return height=0 (API not ready yet);
# 3rd call onwards returns height=77, peer_count=1.
# Also satisfies verify-dropin.sh: systemctl show → GOMEMLIMIT+TimeoutStopUSec,
# drop-in test -f checks → "yes".
DELAYED_STATS_CALL="${TMPDIR_TEST}/delayed-stats-calls"
make_stub "${DELAYED_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  CALLS=0
  [[ -f '${DELAYED_STATS_CALL}' ]] && CALLS=\$(cat '${DELAYED_STATS_CALL}')
  CALLS=\$((CALLS + 1))
  printf '%s' \"\${CALLS}\" >'${DELAYED_STATS_CALL}'
  if [[ \${CALLS} -le 2 ]]; then
    echo '{\"height\":0,\"peer_count\":0,\"syncing\":true}'
  else
    echo '{\"height\":77,\"peer_count\":1,\"syncing\":false}'
  fi
elif echo \"\$CMD\" | grep -q 'systemctl show aperod-node'; then
  echo 'Environment=GOMEMLIMIT=5905580032'
  echo 'TimeoutStopUSec=15min'
elif echo \"\$CMD\" | grep -q 'test -f'; then
  echo 'yes'
elif echo \"\$CMD\" | grep -q 'curl'; then
  echo '{\"ok\":true}'
else
  cat >/dev/null
  printf 'stopped\nremoved\nstarted\n'
fi
exit 0
"

make_stub "${DELAYED_BIN}" "rsync" 'exit 0'
make_stub "${DELAYED_BIN}" "sleep"  'exit 0'

run_join "${TARGET_IP}" "${DELAYED_BIN}" "${DELAYED_DATA}"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "D1: script exits 0 after waiting for API to become ready"
else
  fail "D1: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# The loop must have polled multiple times before seeing height > 0.
POLL_COUNT=0
[[ -f "${DELAYED_STATS_CALL}" ]] && POLL_COUNT=$(cat "${DELAYED_STATS_CALL}")
if [[ "${POLL_COUNT}" -ge 3 ]]; then
  pass "D2: health-wait loop polled ${POLL_COUNT} times before accepting height>0"
else
  fail "D2: expected ≥3 polls (2 not-ready + 1 success) but got ${POLL_COUNT}"
fi

if echo "${LAST_OUTPUT}" | grep -q "height=77"; then
  pass "D3: output reports final height=77 after delayed startup"
else
  fail "D3: expected 'height=77' in output. Got:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -q "peers=1"; then
  pass "D4: output reports peers=1 after API becomes ready"
else
  fail "D4: expected 'peers=1' in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── HEALTH-WAIT TIMEOUT — API never comes up ──────────────────────────────────
# =============================================================================
section "Health-wait timeout — API never responds → exit 1 + Таймаут warning"
# This exercises the step-7 health-wait loop in push-mode join-network.sh when
# the target API never becomes ready.  Every ssh call that would fetch
# network/stats returns an empty string so the loop exhausts all
# HEALTH_MAX_ATTEMPTS and exits 1 with the "Таймаут" warning.

HT_DATA="${TMPDIR_TEST}/ht-data"
mkdir -p "${HT_DATA}/chain.db"
touch "${HT_DATA}/chain.db/CURRENT"

HT_BIN="${TMPDIR_TEST}/ht-bin"

# systemctl: source stop succeeds, is-active returns 1 (not running), start/enable OK.
make_stub "${HT_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'

# ssh stub:
#   • network/stats polls → empty string (API never ready)
#   • systemctl show aperod-node → valid output satisfying verify-dropin.sh
#   • drop-in file existence checks → "yes"
#   • all other remote commands → neutral success
make_stub "${HT_BIN}" "ssh" '
shift   # drop "root@IP"
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  # Return empty string — simulates API never coming up.
  echo ""
elif echo "$CMD" | grep -q "systemctl show aperod-node"; then
  # Satisfy verify-dropin.sh GOMEMLIMIT and TimeoutStopUSec checks.
  echo "Environment=GOMEMLIMIT=5905580032"
  echo "TimeoutStopUSec=15min"
elif echo "$CMD" | grep -q "test -f"; then
  # Satisfy verify-dropin.sh drop-in file existence checks.
  echo "yes"
else
  # Heredoc and other remote commands: drain stdin, return success strings.
  cat >/dev/null
  printf "stopped\nremoved\nstarted\n"
fi
exit 0
'

# rsync: succeed silently.
make_stub "${HT_BIN}" "rsync" 'exit 0'

# sleep: no-op so 30 loop iterations complete instantly.
make_stub "${HT_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${HT_BIN}" "${HT_DATA}"

# ── Assertion HT1: script exits non-zero (health-wait timeout) ───────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "HT1: script exited non-zero (${LAST_EXIT}) after health-wait timeout"
else
  fail "HT1: script should exit non-zero after health-wait timeout but exited 0"
fi

# ── Assertion HT2: output contains the Russian "Таймаут" warning ──────────────
if echo "${LAST_OUTPUT}" | grep -q "Таймаут"; then
  pass "HT2: output contains 'Таймаут' warning"
else
  fail "HT2: expected 'Таймаут' in output. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion HT3: all HEALTH_MAX_ATTEMPTS were exhausted ────────────────────
# The loop prints "API ещё не готов" on every failed poll (when STATS is empty).
HT_POLL_COUNT=$(echo "${LAST_OUTPUT}" | grep -c "ещё не готов" || true)
if [[ "${HT_POLL_COUNT}" -ge 30 ]]; then
  pass "HT3: health-wait loop ran ${HT_POLL_COUNT} iterations (all attempts exhausted)"
else
  fail "HT3: expected ≥30 'ещё не готов' lines but counted ${HT_POLL_COUNT}"
fi

# =============================================================================
# ── RSYNC INTERRUPTED MID-COPY → EXIT trap must restart source node ───────────
# =============================================================================
section "Rsync interrupted mid-copy — EXIT trap restarts source node unconditionally"
# If the script is killed or rsync exits non-zero (e.g. SSH disconnect, Ctrl-C,
# partial LevelDB copy), the EXIT trap installed after step 2 must fire and
# print the source-restart attempt regardless.  The source node must NOT be left
# stopped silently.

RI_DATA="${TMPDIR_TEST}/ri-data"
mkdir -p "${RI_DATA}/chain.db"
touch "${RI_DATA}/chain.db/CURRENT"

RI_BIN="${TMPDIR_TEST}/ri-bin"

# systemctl: stop succeeds, is-active returns 1 (stopped immediately),
# start returns 0 so the trap can restart the source.
make_stub "${RI_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'

# ssh: target disable/stop returns success so step 1 passes; anything else
# drains stdin and returns neutral success strings.
make_stub "${RI_BIN}" "ssh" '
shift   # drop "root@IP"
cat >/dev/null
printf "stopped\nremoved\nstarted\n"
exit 0
'

# rsync: exits 1 to simulate an interrupted / partial mid-copy transfer.
RI_RSYNC_CALLED="${TMPDIR_TEST}/ri-rsync-called"
make_stub "${RI_BIN}" "rsync" "
touch '${RI_RSYNC_CALLED}'
exit 1
"

# sleep: no-op so any retry loops finish instantly.
make_stub "${RI_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${RI_BIN}" "${RI_DATA}"

# ── Assertion RI1: script exits non-zero after rsync failure ──────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "RI1: script exited non-zero (${LAST_EXIT}) when rsync failed mid-copy"
else
  fail "RI1: script should exit non-zero after rsync failure but exited 0"
fi

# ── Assertion RI2: rsync stub was actually invoked ───────────────────────────
if [[ -f "${RI_RSYNC_CALLED}" ]]; then
  pass "RI2: rsync was called (confirming the failure path was exercised)"
else
  fail "RI2: rsync stub was never called — test did not reach the rsync step"
fi

# ── Assertion RI3: EXIT trap printed the [TRAP] banner ───────────────────────
if echo "${LAST_OUTPUT}" | grep -q "\[TRAP\]"; then
  pass "RI3: EXIT trap printed [TRAP] source-restart banner after rsync failure"
else
  fail "RI3: expected [TRAP] message in output after rsync failure. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion RI4: trap confirmed source-node restart attempt ─────────────────
# The trap message contains "источнике" (Russian for "on the source").
if echo "${LAST_OUTPUT}" | grep -q "источнике"; then
  pass "RI4: trap confirmed source-node restart attempt ('источнике' present)"
else
  fail "RI4: expected 'источнике' in trap output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── TARGET NODE NOT INSTALLED — node.yaml absent on target ───────────────────
# =============================================================================
section "Target node not installed — step-5 heredoc guard detects absent node.yaml → abort + install instruction"
# Step 5 of join-network.sh sends a heredoc to `ssh root@TARGET bash`.
# The heredoc contains a [[ ! -f "${NODE_YAML}" ]] guard that prints an error
# and exits 1 when the file is absent.
#
# This test lets the guard execute FOR REAL by:
#   1. Pointing SECONDARY_NODE_YAML at a temp path that is never created.
#   2. Making the ssh stub run its stdin (the heredoc payload) via `exec bash`
#      instead of emitting canned output — so the actual guard code runs.
#
# If the guard is removed or the error message changes, NI2/NI3 will fail.

NI_DATA="${TMPDIR_TEST}/ni-data"
mkdir -p "${NI_DATA}/chain.db"
touch "${NI_DATA}/chain.db/CURRENT"

# Deliberately absent: do NOT create this file.
NI_NODE_YAML="${TMPDIR_TEST}/ni-node.yaml"

NI_BIN="${TMPDIR_TEST}/ni-bin"

# systemctl: source stop/is-active/start behave correctly so steps 2+trap work.
make_stub "${NI_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'

# ssh stub:
#   • systemctl disable/stop on target (step 1) → "stopped", exit 0
#   • rm -f identity key (step 4)               → "removed", exit 0
#   • bare "bash" (step-5 heredoc)              → execute the stdin payload
#                                                  via `exec bash` so the real
#                                                  [[ ! -f ]] guard runs
#   • anything else                             → neutral success
make_stub "${NI_BIN}" "ssh" '
shift   # drop "root@IP"
CMD="$*"
if echo "$CMD" | grep -qE "systemctl (disable|stop)"; then
  cat >/dev/null
  echo "stopped"
  exit 0
elif echo "$CMD" | grep -q "rm -f"; then
  echo "removed"
  exit 0
elif [[ "$CMD" == "bash" ]]; then
  # Run the heredoc payload as a real bash script.
  # join-network.sh expands ${SECONDARY_NODE_YAML} into the heredoc before
  # piping it here, so NODE_YAML will contain the temp path that does not
  # exist — the [[ ! -f "${NODE_YAML}" ]] guard will fire for real.
  exec bash
else
  cat >/dev/null
  printf "stopped\nremoved\nstarted\n"
  exit 0
fi
'

# rsync: succeed silently (it runs before step 5).
make_stub "${NI_BIN}" "rsync" 'exit 0'

# sleep: no-op.
make_stub "${NI_BIN}" "sleep" 'exit 0'

# Run join-network.sh with SECONDARY_NODE_YAML pointing to the absent file.
NI_EXIT=0
NI_OUTPUT=$(
  PATH="${NI_BIN}:${PATH}" \
  PRIMARY_IP="${PRIMARY_IP}" \
  PRIMARY_DATA_DIR="${NI_DATA}" \
  SECONDARY_NODE_YAML="${NI_NODE_YAML}" \
  bash "${JOIN_SH}" "${TARGET_IP}" 2>&1
) || NI_EXIT=$?

# ── Assertion NI1: script exits non-zero ──────────────────────────────────────
if [[ ${NI_EXIT} -ne 0 ]]; then
  pass "NI1: script exited non-zero (${NI_EXIT}) when node.yaml is absent on target"
else
  fail "NI1: script should exit non-zero but exited 0. Output:\n${NI_OUTPUT}"
fi

# ── Assertion NI2: real guard message 'не найден' appears in output ───────────
# This text is printed by the [[ ! -f "${NODE_YAML}" ]] block inside the
# heredoc in join-network.sh step 5.  If that block is removed, this fails.
if echo "${NI_OUTPUT}" | grep -q "не найден"; then
  pass "NI2: output contains 'не найден' (real step-5 guard message)"
else
  fail "NI2: expected 'не найден' in output. Got:\n${NI_OUTPUT}"
fi

# ── Assertion NI3: output contains the install instruction ───────────────────
# The guard also prints the install instruction (install-validator.sh /
# install-node.sh).  If that line is removed from the heredoc, this fails.
if echo "${NI_OUTPUT}" | grep -q "install"; then
  pass "NI3: output contains install instruction (install-validator.sh or install-node.sh)"
else
  fail "NI3: expected install instruction in output. Got:\n${NI_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP HEALTH-WAIT TIMEOUT — local API never comes up → exit 1 + Таймаут
# =============================================================================
section "Bootstrap health-wait timeout — local API never responds → exit 1 + Таймаут warning"
# This exercises the step-9 health-wait loop in --bootstrap-from mode of
# join-network.sh.  After chain.db + snapshot are rsynced and the local node is
# (re)started, the script polls the LOCAL API via curl.  When curl always
# returns an empty string the loop exhausts all HEALTH_MAX_ATTEMPTS and the
# script must exit 1 with the "Таймаут" warning.  Previously untested — the
# timeout branch (exit 1 + "Таймаут") had no coverage.

BT_DIR="${TMPDIR_TEST}/bt"
BT_DATA="${BT_DIR}/data"
BT_BIN="${BT_DIR}/bin"
BT_YAML="${BT_DIR}/node.yaml"
BT_CONFIG="${BT_DIR}/node-config.sh"

mkdir -p "${BT_DATA}/chain.db"
touch "${BT_DATA}/chain.db/CURRENT"
# Fake snapshot so the height-detection glob in step 4b finds something.
touch "${BT_DATA}/snapshot-v2-100.json.gz"
printf 'network: testnet\n' > "${BT_YAML}"
# node-config.sh stub: accept every subcommand as a no-op success.
printf '#!/usr/bin/env bash\nexit 0\n' > "${BT_CONFIG}"
chmod +x "${BT_CONFIG}"

# systemctl: local stop succeeds, is-active returns 1 (stopped), enable/start OK.
make_stub "${BT_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

# ssh stub (remote validator side):
#   • network/stats (step 1 validator tip read) → valid JSON
#   • data-dir / chain.db existence checks       → exit 0 (present)
#   • systemctl start (validator restart)        → "started"
#   • bare "bash" heredoc (remote stop)          → drain stdin, "stopped"
#   • anything else                              → neutral success
make_stub "${BT_BIN}" "ssh" '
shift   # drop root@IP
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":200,\"height\":200,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null
  echo "stopped"
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# rsync / chown: succeed silently.
make_stub "${BT_BIN}" "rsync" 'exit 0'
make_stub "${BT_BIN}" "chown" 'exit 0'

# sleep: no-op so the health-wait loop finishes instantly.
make_stub "${BT_BIN}" "sleep" 'exit 0'

# curl: ALWAYS returns an empty string → local API never becomes ready.
make_stub "${BT_BIN}" "curl" 'echo ""; exit 0'

BT_EXIT=0
BT_OUTPUT=$(
  env \
    PATH="${BT_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BT_DATA}" \
    LOCAL_DATA_DIR="${BT_DATA}" \
    LOCAL_NODE_YAML="${BT_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BT_CONFIG}" \
    PRIMARY_DATA_DIR="${BT_DATA}" \
    VALIDATOR_DATA_DIR="${BT_DATA}" \
    HEALTH_MAX_ATTEMPTS=4 \
    HEALTH_WAIT_SECS=0 \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.2" 2>&1
) || BT_EXIT=$?

# ── Assertion BT1: script exits 1 after the bootstrap health-wait timeout ─────
if [[ ${BT_EXIT} -eq 1 ]]; then
  pass "BT1: bootstrap mode exited 1 after local API health-wait timeout"
else
  fail "BT1: expected exit 1 after bootstrap health-wait timeout but got ${BT_EXIT}. Output:\n${BT_OUTPUT}"
fi

# ── Assertion BT2: output contains the Russian "Таймаут" warning ──────────────
if echo "${BT_OUTPUT}" | grep -q "Таймаут"; then
  pass "BT2: output contains 'Таймаут' warning"
else
  fail "BT2: expected 'Таймаут' in output. Got:\n${BT_OUTPUT}"
fi

# ── Assertion BT3: all HEALTH_MAX_ATTEMPTS were exhausted ─────────────────────
# The loop prints "API ещё не готов" on every failed poll (STATS empty).
BT_POLL_COUNT=$(echo "${BT_OUTPUT}" | grep -c "ещё не готов" || true)
if [[ "${BT_POLL_COUNT}" -ge 4 ]]; then
  pass "BT3: bootstrap health-wait loop ran ${BT_POLL_COUNT} iterations (all attempts exhausted)"
else
  fail "BT3: expected ≥4 'ещё не готов' lines but counted ${BT_POLL_COUNT}. Output:\n${BT_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP RSYNC INTERRUPTED — _bootstrap_cleanup trap must restart BOTH nodes
# =============================================================================
section "Bootstrap rsync interrupted — _bootstrap_cleanup trap restarts validator AND local relay"
# Task 1777: bootstrap-mode has its own _bootstrap_cleanup trap (lines 136–160
# of join-network.sh).  When rsync fails mid-copy both _BS_VALIDATOR_STOPPED
# and _BS_LOCAL_STOPPED are already 1 (set during steps 2 and 3), so the trap
# must attempt to restart BOTH the remote validator and the local relay node.
# Assertions:
#   BR1 — script exits non-zero
#   BR2 — rsync stub was actually called (test reached the right step)
#   BR3 — [TRAP] banner appears in output
#   BR4 — validator-restart message appears ("[TRAP] Перезапускаем aperod-node на валидаторе")
#   BR5 — local-restart message appears   ("[TRAP] Перезапускаем local aperod-node")

BR_DIR="${TMPDIR_TEST}/br"
BR_DATA="${BR_DIR}/data"
BR_BIN="${BR_DIR}/bin"
BR_YAML="${BR_DIR}/node.yaml"
BR_CONFIG="${BR_DIR}/node-config.sh"

mkdir -p "${BR_DATA}/chain.db"
touch "${BR_DATA}/chain.db/CURRENT"
# Fake snapshot so the height-detection glob in step 4b doesn't error out.
touch "${BR_DATA}/snapshot-v2-100.json.gz"
printf 'network: testnet\n' > "${BR_YAML}"
# node-config.sh stub: accept every subcommand as a no-op success.
printf '#!/usr/bin/env bash\nexit 0\n' > "${BR_CONFIG}"
chmod +x "${BR_CONFIG}"

# systemctl: local stop succeeds immediately, is-active reports stopped,
# start returns 0 so the trap's local-restart attempt succeeds.
make_stub "${BR_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

# ssh stub (remote validator side):
#   • network/stats (step 1 tip read)           → valid JSON
#   • [ -d ... ] existence checks (step 1b)     → exit 0 (present)
#   • bare "bash" heredoc (step 3 remote stop)  → drain stdin, "stopped"
#   • "systemctl start" (trap validator restart) → "started"
#   • anything else                             → neutral success
make_stub "${BR_BIN}" "ssh" '
shift   # drop root@IP
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":500,\"height\":500,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null
  echo "stopped"
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# rsync: exits 1 to simulate an interrupted mid-copy transfer.
BR_RSYNC_CALLED="${TMPDIR_TEST}/br-rsync-called"
make_stub "${BR_BIN}" "rsync" "
touch '${BR_RSYNC_CALLED}'
exit 1
"

# chown / sleep: succeed silently so they don't interfere.
make_stub "${BR_BIN}" "chown" 'exit 0'
make_stub "${BR_BIN}" "sleep"  'exit 0'

BR_EXIT=0
BR_OUTPUT=$(
  env \
    PATH="${BR_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BR_DATA}" \
    LOCAL_DATA_DIR="${BR_DATA}" \
    LOCAL_NODE_YAML="${BR_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BR_CONFIG}" \
    PRIMARY_DATA_DIR="${BR_DATA}" \
    VALIDATOR_DATA_DIR="${BR_DATA}" \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.3" 2>&1
) || BR_EXIT=$?

# ── Assertion BR1: script exits non-zero after rsync failure ──────────────────
if [[ ${BR_EXIT} -ne 0 ]]; then
  pass "BR1: bootstrap script exited non-zero (${BR_EXIT}) after rsync interruption"
else
  fail "BR1: expected non-zero exit after rsync interruption but got 0. Output:\n${BR_OUTPUT}"
fi

# ── Assertion BR2: rsync stub was actually called ─────────────────────────────
if [[ -f "${BR_RSYNC_CALLED}" ]]; then
  pass "BR2: rsync was called (confirming the failure path was exercised)"
else
  fail "BR2: rsync stub was never called — test did not reach the rsync step. Output:\n${BR_OUTPUT}"
fi

# ── Assertion BR3: [TRAP] banner appears in output ────────────────────────────
if echo "${BR_OUTPUT}" | grep -q "\[TRAP\]"; then
  pass "BR3: [TRAP] banner appeared in output after bootstrap rsync interruption"
else
  fail "BR3: expected [TRAP] banner in output. Got:\n${BR_OUTPUT}"
fi

# ── Assertion BR4: validator-restart message appears ─────────────────────────
# The trap prints "Перезапускаем aperod-node на валидаторе" when
# _BS_VALIDATOR_STOPPED=1.  This flag is set in step 3 (before rsync), so the
# trap must attempt the remote restart.
if echo "${BR_OUTPUT}" | grep -q "валидаторе"; then
  pass "BR4: trap printed validator-restart message ('валидаторе' present)"
else
  fail "BR4: expected validator-restart message ('валидаторе') in trap output. Got:\n${BR_OUTPUT}"
fi

# ── Assertion BR5: local relay is NOT restarted — blocked-node warning present ─
# After an interrupted rsync the local chain.db is in an unknown partial state.
# The trap must NOT attempt to start the local relay against it.  Instead it
# must warn the operator that the node is blocked by the sentinel and give
# recovery instructions.  "НЕ запускается" or "rsync-in-progress" in the
# output confirms the correct (non-restart) behaviour.
if echo "${BR_OUTPUT}" | grep -q "НЕ запускается\|rsync-in-progress"; then
  pass "BR5: trap warns that local relay is blocked (NOT restarted) after partial rsync"
else
  fail "BR5: expected blocked-node warning ('НЕ запускается' or 'rsync-in-progress') in trap output. Got:\n${BR_OUTPUT}"
fi

# ── Assertion BR6: sentinel still present in LOCAL_DATA_DIR ──────────────────
# The cleanup trap must leave .rsync-in-progress in place so aperod-node
# (e.g. a watchdog-triggered restart) cannot open the partial LevelDB.
BR_SENTINEL="${BR_DATA}/.rsync-in-progress"
if [[ -f "${BR_SENTINEL}" ]]; then
  pass "BR6: .rsync-in-progress is still present in LOCAL_DATA_DIR after bootstrap rsync interruption (node stays blocked)"
else
  fail "BR6: .rsync-in-progress was removed by the trap — local node could start against a half-written LevelDB"
fi

# =============================================================================
# ── RSYNC SENTINEL — written before rsync, removed after (push mode) ──────────
# =============================================================================
section "Rsync sentinel — written to target before rsync, removed after success"
# join-network.sh must:
#   SR1 — write .rsync-in-progress to SECONDARY_DATA_DIR on the target BEFORE rsync
#   SR2 — rsync is called after the sentinel is written
#   SR3 — .rsync-in-progress is removed on the target after a successful rsync
#   SR4 — the final success banner is present (overall script exits 0)

SR_DATA="${TMPDIR_TEST}/sr-data"
mkdir -p "${SR_DATA}/chain.db"
touch "${SR_DATA}/chain.db/CURRENT"

SR_BIN="${TMPDIR_TEST}/sr-bin"

# systemctl: normal stop/is-active/start behaviour.
make_stub "${SR_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'

# Shared state files for the ssh stub.
SR_SENTINEL_WRITTEN="${TMPDIR_TEST}/sr-sentinel-written"
SR_SENTINEL_REMOVED="${TMPDIR_TEST}/sr-sentinel-removed"
SR_RSYNC_BEFORE_SENTINEL="${TMPDIR_TEST}/sr-rsync-before-sentinel"

# ssh stub: intercepts the sentinel-write and sentinel-remove commands so we
# can assert on the ordering, and returns valid JSON for the health-wait poll.
make_stub "${SR_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'rsync-in-progress' && echo \"\$CMD\" | grep -q 'touch'; then
  touch '${SR_SENTINEL_WRITTEN}'
  echo 'sentinel written'
elif echo \"\$CMD\" | grep -q 'rsync-in-progress' && echo \"\$CMD\" | grep -q 'rm'; then
  touch '${SR_SENTINEL_REMOVED}'
  echo 'sentinel removed'
elif echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"height\":12345,\"peer_count\":1,\"syncing\":false}'
elif echo \"\$CMD\" | grep -q 'systemctl show aperod-node'; then
  echo 'Environment=GOMEMLIMIT=5905580032'
  echo 'TimeoutStopUSec=15min'
elif echo \"\$CMD\" | grep -q 'test -f'; then
  echo 'yes'
else
  cat >/dev/null
  printf 'stopped\nremoved\nstarted\n'
fi
exit 0
"

# rsync stub: records whether the sentinel was already written before rsync ran.
SR_RSYNC_CALLED="${TMPDIR_TEST}/sr-rsync-called"
make_stub "${SR_BIN}" "rsync" "
touch '${SR_RSYNC_CALLED}'
# Record whether sentinel had already been written before rsync started.
[[ ! -f '${SR_SENTINEL_WRITTEN}' ]] && touch '${SR_RSYNC_BEFORE_SENTINEL}'
exit 0
"

make_stub "${SR_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${SR_BIN}" "${SR_DATA}"

# SR1: sentinel was written before rsync
if [[ -f "${SR_SENTINEL_WRITTEN}" ]]; then
  pass "SR1: ssh stub received sentinel-write command (touch .rsync-in-progress)"
else
  fail "SR1: sentinel-write command was never sent to target. Output:\n${LAST_OUTPUT}"
fi

# SR2: rsync was called AND the sentinel was already written before rsync ran
if [[ -f "${SR_RSYNC_CALLED}" ]]; then
  if [[ ! -f "${SR_RSYNC_BEFORE_SENTINEL}" ]]; then
    pass "SR2: rsync was called AFTER sentinel was written (correct ordering)"
  else
    fail "SR2: rsync was called BEFORE sentinel was written — ordering violation"
  fi
else
  fail "SR2: rsync was never called"
fi

# SR3: sentinel was removed after rsync succeeded
if [[ -f "${SR_SENTINEL_REMOVED}" ]]; then
  pass "SR3: ssh stub received sentinel-remove command (rm .rsync-in-progress)"
else
  fail "SR3: sentinel-remove command was never sent to target. Output:\n${LAST_OUTPUT}"
fi

# SR4: overall script exited 0 and success banner present
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "SR4: script exited 0 (sentinel cycle completed, node started)"
else
  fail "SR4: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── RSYNC SENTINEL — cleanup trap removes sentinel on rsync failure (push mode)
# =============================================================================
section "Rsync sentinel — cleanup trap removes sentinel when rsync fails"
# When rsync exits non-zero the EXIT trap must remove .rsync-in-progress from
# the target so the operator is not permanently locked out.
#   ST1 — script exits non-zero
#   ST2 — sentinel-remove command is sent via ssh even though rsync failed

ST_DATA="${TMPDIR_TEST}/st-data"
mkdir -p "${ST_DATA}/chain.db"
touch "${ST_DATA}/chain.db/CURRENT"

ST_BIN="${TMPDIR_TEST}/st-bin"

make_stub "${ST_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'

ST_SENTINEL_REMOVED="${TMPDIR_TEST}/st-sentinel-removed"
make_stub "${ST_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'rsync-in-progress' && echo \"\$CMD\" | grep -q 'rm'; then
  touch '${ST_SENTINEL_REMOVED}'
  echo 'sentinel removed'
elif echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"height\":0,\"peer_count\":0}'
else
  cat >/dev/null
  printf 'stopped\nremoved\nstarted\n'
fi
exit 0
"

# rsync: fail to simulate an interrupted transfer.
make_stub "${ST_BIN}" "rsync" 'exit 1'
make_stub "${ST_BIN}" "sleep"  'exit 0'

run_join "${TARGET_IP}" "${ST_BIN}" "${ST_DATA}"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "ST1: script exited non-zero after rsync failure (as expected)"
else
  fail "ST1: expected non-zero exit after rsync failure but got 0"
fi

# ST2: the EXIT trap must NOT remove the sentinel on the target after an
# interrupted rsync.  The target data dir may be partially overwritten;
# removing the sentinel would allow a watchdog to restart the node against
# a half-written LevelDB.  The sentinel must remain so aperod-node refuses
# to start until an operator verifies the data dir and removes it manually.
if [[ ! -f "${ST_SENTINEL_REMOVED}" ]]; then
  pass "ST2: EXIT trap did NOT remove sentinel — target node remains blocked after partial rsync (correct)"
else
  fail "ST2: EXIT trap removed the sentinel after rsync failure — target node could start against a half-written LevelDB"
fi

# ST3: the trap should warn operators about the blocked node and recovery steps.
if echo "${LAST_OUTPUT}" | grep -q "НЕ запускается\|rsync-in-progress"; then
  pass "ST3: output warns that target node is blocked and sentinel remains"
else
  fail "ST3: expected blocked-node warning in trap output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP SENTINEL — written before rsync, removed after (bootstrap mode) ─
# =============================================================================
section "Bootstrap sentinel — written to local data dir before rsync, removed after"
# In --bootstrap-from mode the sentinel is written to LOCAL_DATA_DIR (this
# machine) before rsync begins, and removed after both chain.db and snapshot
# rsync commands complete successfully.
#   BS1 — script exits 0 (successful bootstrap)
#   BS2 — .rsync-in-progress is NOT present in LOCAL_DATA_DIR after success
#   BS3 — success banner contains snapshot height

BSL_DIR="${TMPDIR_TEST}/bsl"
BSL_DATA="${BSL_DIR}/data"
BSL_BIN="${BSL_DIR}/bin"
BSL_YAML="${BSL_DIR}/node.yaml"
BSL_CONFIG="${BSL_DIR}/node-config.sh"

mkdir -p "${BSL_DATA}/chain.db"
touch "${BSL_DATA}/chain.db/CURRENT"
touch "${BSL_DATA}/snapshot-v2-999.json.gz"
printf 'network: testnet\n' > "${BSL_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BSL_CONFIG}"
chmod +x "${BSL_CONFIG}"

make_stub "${BSL_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

make_stub "${BSL_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":999,\"height\":999,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null
  echo "stopped"
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

make_stub "${BSL_BIN}" "rsync"  'exit 0'
make_stub "${BSL_BIN}" "chown"  'exit 0'
make_stub "${BSL_BIN}" "sleep"  'exit 0'
# curl: return valid height so health-wait exits immediately.
make_stub "${BSL_BIN}" "curl"   'echo "{\"height\":999,\"peer_count\":1}"; exit 0'

BSL_EXIT=0
BSL_OUTPUT=$(
  env \
    PATH="${BSL_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BSL_DATA}" \
    LOCAL_DATA_DIR="${BSL_DATA}" \
    LOCAL_NODE_YAML="${BSL_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BSL_CONFIG}" \
    PRIMARY_DATA_DIR="${BSL_DATA}" \
    VALIDATOR_DATA_DIR="${BSL_DATA}" \
    HEALTH_MAX_ATTEMPTS=5 \
    HEALTH_WAIT_SECS=0 \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.10" 2>&1
) || BSL_EXIT=$?

if [[ ${BSL_EXIT} -eq 0 ]]; then
  pass "BS1: bootstrap script exited 0 (successful run with sentinel lifecycle)"
else
  fail "BS1: expected exit 0 but got ${BSL_EXIT}. Output:\n${BSL_OUTPUT}"
fi

SENTINEL_FILE="${BSL_DATA}/.rsync-in-progress"
if [[ ! -f "${SENTINEL_FILE}" ]]; then
  pass "BS2: .rsync-in-progress is absent after successful bootstrap (sentinel cleaned up)"
else
  fail "BS2: .rsync-in-progress still present after successful bootstrap — node would be permanently blocked"
fi

if echo "${BSL_OUTPUT}" | grep -q "999"; then
  pass "BS3: success banner contains snapshot height 999"
else
  fail "BS3: expected height '999' in success output. Got:\n${BSL_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP SENTINEL — sentinel retained on rsync failure (local node blocked)
# =============================================================================
section "Bootstrap sentinel — sentinel retained after rsync failure (local node stays blocked)"
# When rsync fails in --bootstrap-from mode the _bootstrap_cleanup trap must
# NOT remove .rsync-in-progress from LOCAL_DATA_DIR.  The local chain.db is in
# an unknown partial state; removing the sentinel would allow a watchdog to
# start the node against it.  The sentinel must stay until an operator manually
# verifies the data dir and re-runs bootstrap (or restores from backup).
#   BF1 — script exits non-zero
#   BF2 — .rsync-in-progress IS still present in LOCAL_DATA_DIR after the trap fires

BF_DIR="${TMPDIR_TEST}/bf"
BF_DATA="${BF_DIR}/data"
BF_BIN="${BF_DIR}/bin"
BF_YAML="${BF_DIR}/node.yaml"
BF_CONFIG="${BF_DIR}/node-config.sh"

mkdir -p "${BF_DATA}/chain.db"
touch "${BF_DATA}/chain.db/CURRENT"
touch "${BF_DATA}/snapshot-v2-100.json.gz"
printf 'network: testnet\n' > "${BF_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BF_CONFIG}"
chmod +x "${BF_CONFIG}"

make_stub "${BF_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

make_stub "${BF_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":100,\"height\":100,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null
  echo "stopped"
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# rsync: fail to trigger the cleanup trap.
make_stub "${BF_BIN}" "rsync"  'exit 1'
make_stub "${BF_BIN}" "chown"  'exit 0'
make_stub "${BF_BIN}" "sleep"  'exit 0'

BF_EXIT=0
BF_OUTPUT=$(
  env \
    PATH="${BF_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BF_DATA}" \
    LOCAL_DATA_DIR="${BF_DATA}" \
    LOCAL_NODE_YAML="${BF_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BF_CONFIG}" \
    PRIMARY_DATA_DIR="${BF_DATA}" \
    VALIDATOR_DATA_DIR="${BF_DATA}" \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.11" 2>&1
) || BF_EXIT=$?

if [[ ${BF_EXIT} -ne 0 ]]; then
  pass "BF1: bootstrap script exited non-zero after rsync failure"
else
  fail "BF1: expected non-zero exit after rsync failure but got 0. Output:\n${BF_OUTPUT}"
fi

BF_SENTINEL="${BF_DATA}/.rsync-in-progress"
if [[ -f "${BF_SENTINEL}" ]]; then
  pass "BF2: .rsync-in-progress is still present in LOCAL_DATA_DIR after rsync failure — local node remains blocked (correct)"
else
  fail "BF2: .rsync-in-progress was removed by the cleanup trap — local node could start against a half-written LevelDB"
fi

# BF3: trap warns operator about blocked node and recovery steps.
if echo "${BF_OUTPUT}" | grep -q "НЕ запускается\|rsync-in-progress"; then
  pass "BF3: trap output warns that local relay is blocked and explains recovery"
else
  fail "BF3: expected blocked-node warning in trap output. Got:\n${BF_OUTPUT}"
fi

# =============================================================================
# ── PUSH PRE-RSYNC FAILURE — sentinel write fails → target restarted ──────────
# =============================================================================
section "Push pre-rsync failure — sentinel write SSH fails → target restarted (data untouched)"
# When the sentinel-write SSH command at step 2b fails (e.g. SSH connection
# error), the script aborts before rsync runs.  _PUSH_RSYNC_STARTED=0 at this
# point so the cleanup trap must restart the target (its data is untouched) and
# must NOT attempt rsync or leave the target blocked.
#   PF1 — script exits non-zero
#   PF2 — rsync was NOT called (no data copied before abort)
#   PF3 — ssh sent a "systemctl start" to TARGET (target was restarted by trap)
#   PF4 — source node was also restarted by the trap

PF_DATA="${TMPDIR_TEST}/pf-data"
mkdir -p "${PF_DATA}/chain.db"
touch "${PF_DATA}/chain.db/CURRENT"

PF_BIN="${TMPDIR_TEST}/pf-bin"

# systemctl: source stop/is-active succeeds so step 2 completes.
make_stub "${PF_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *)                      exit 0 ;;
esac
'

# ssh stub:
#   • sentinel-write (touch .rsync-in-progress) → FAIL (exit 1)
#   • trap restart of target (systemctl start)  → succeed + record
#   • step 1 disable/stop                       → "stopped"
#   • everything else                            → neutral success
PF_TARGET_RESTARTED="${TMPDIR_TEST}/pf-target-restarted"
make_stub "${PF_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'rsync-in-progress' && echo \"\$CMD\" | grep -q 'touch'; then
  exit 1   # sentinel write fails
elif echo \"\$CMD\" | grep -q 'systemctl start aperod-node'; then
  touch '${PF_TARGET_RESTARTED}'
  echo 'started'
  exit 0
elif echo \"\$CMD\" | grep -qE 'systemctl (disable|stop|enable)'; then
  cat >/dev/null
  echo 'stopped'
  exit 0
else
  cat >/dev/null
  printf 'stopped\nremoved\nstarted\n'
  exit 0
fi
"

PF_RSYNC_CALLED="${TMPDIR_TEST}/pf-rsync-called"
make_stub "${PF_BIN}" "rsync" "touch '${PF_RSYNC_CALLED}'; exit 0"
make_stub "${PF_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${PF_BIN}" "${PF_DATA}"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "PF1: script exited non-zero after sentinel-write SSH failure"
else
  fail "PF1: expected non-zero exit but got 0. Output:\n${LAST_OUTPUT}"
fi

if [[ ! -f "${PF_RSYNC_CALLED}" ]]; then
  pass "PF2: rsync was NOT called — no partial data written to target"
else
  fail "PF2: rsync was called despite sentinel-write failure — unexpected"
fi

if [[ -f "${PF_TARGET_RESTARTED}" ]]; then
  pass "PF3: trap sent 'systemctl start aperod-node' to TARGET (target restarted, data was untouched)"
else
  fail "PF3: target was NOT restarted by trap — node left stopped unnecessarily. Output:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -q "источнике"; then
  pass "PF4: source node restart attempt confirmed in trap output"
else
  fail "PF4: expected source-restart message ('источнике') in trap output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── PUSH POST-TRANSFER FAILURE — sentinel removed, later step fails → target restarted
# =============================================================================
section "Push post-transfer failure — rsync done, sentinel removed, config fails → target restarted"
# After a successful rsync and sentinel removal (_PUSH_RSYNC_STARTED=0), if a
# later step fails (e.g. bootnode injection, chown, start), the target data is
# consistent and the trap MUST restart the target node.
#   PT1 — script exits non-zero
#   PT2 — rsync was called (transfer happened)
#   PT3 — target was restarted by the trap (data is consistent)

PT_DATA="${TMPDIR_TEST}/pt-data"
mkdir -p "${PT_DATA}/chain.db"
touch "${PT_DATA}/chain.db/CURRENT"

PT_BIN="${TMPDIR_TEST}/pt-bin"

make_stub "${PT_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *"start aperod-node"*)  exit 0 ;;
  *)                      exit 0 ;;
esac
'

# ssh stub:
#   • sentinel write (touch)           → success
#   • sentinel remove (rm)             → success
#   • rm -f p2p_identity               → success  (step 4)
#   • bare "bash" heredoc (step 5)     → FAIL (exit 1) — simulates bootnode injection failure
#   • systemctl start (trap restart)   → success + record
#   • disable/stop                     → "stopped"
PT_TARGET_RESTARTED="${TMPDIR_TEST}/pt-target-restarted"
make_stub "${PT_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'rsync-in-progress' && echo \"\$CMD\" | grep -q 'touch'; then
  echo 'sentinel written'
elif echo \"\$CMD\" | grep -q 'rsync-in-progress' && echo \"\$CMD\" | grep -q 'rm'; then
  echo 'sentinel removed'
elif echo \"\$CMD\" | grep -q 'p2p_identity'; then
  echo 'removed'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  exit 1   # bootnode injection fails
elif echo \"\$CMD\" | grep -q 'systemctl start aperod-node'; then
  touch '${PT_TARGET_RESTARTED}'
  echo 'started'
elif echo \"\$CMD\" | grep -qE 'systemctl (disable|stop|enable)'; then
  cat >/dev/null
  echo 'stopped'
else
  cat >/dev/null
  printf 'stopped\nremoved\nstarted\n'
fi
exit 0
"

PT_RSYNC_CALLED="${TMPDIR_TEST}/pt-rsync-called"
make_stub "${PT_BIN}" "rsync" "touch '${PT_RSYNC_CALLED}'; exit 0"
make_stub "${PT_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${PT_BIN}" "${PT_DATA}"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "PT1: script exited non-zero after post-transfer failure"
else
  fail "PT1: expected non-zero exit but got 0. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${PT_RSYNC_CALLED}" ]]; then
  pass "PT2: rsync was called (transfer completed before failure)"
else
  fail "PT2: rsync was not called — test did not reach the transfer step. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${PT_TARGET_RESTARTED}" ]]; then
  pass "PT3: trap restarted target after post-transfer failure (data is consistent)"
else
  fail "PT3: target was NOT restarted by trap after post-transfer failure — node left stopped unnecessarily. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP PRE-RSYNC FAILURE — sentinel write fails → local relay restarted
# =============================================================================
section "Bootstrap pre-rsync failure — sentinel write fails → local relay restarted (data untouched)"
# When touch .rsync-in-progress fails in bootstrap mode, _BS_RSYNC_STARTED=0
# (never set) so the cleanup trap must restart the local relay and the validator.
#   BPR1 — script exits non-zero
#   BPR2 — systemctl start aperod-node was called locally (local relay restarted)
#   BPR3 — validator was also restarted via ssh

BPRD_DIR="${TMPDIR_TEST}/bprd"
BPRD_DATA="${BPRD_DIR}/data"
BPRD_BIN="${BPRD_DIR}/bin"
BPRD_YAML="${BPRD_DIR}/node.yaml"
BPRD_CONFIG="${BPRD_DIR}/node-config.sh"

mkdir -p "${BPRD_DATA}/chain.db"
touch "${BPRD_DATA}/chain.db/CURRENT"
touch "${BPRD_DATA}/snapshot-v2-100.json.gz"
printf 'network: testnet\n' > "${BPRD_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BPRD_CONFIG}"
chmod +x "${BPRD_CONFIG}"

BPRD_LOCAL_RESTARTED="${TMPDIR_TEST}/bprd-local-restarted"
# systemctl: local stop succeeds, is-active returns 1 (stopped).
# Local START is recorded so we can verify relay was restarted by trap.
make_stub "${BPRD_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*) touch '${BPRD_LOCAL_RESTARTED}'; exit 0 ;;
  *)                     exit 0 ;;
esac
"

BPRD_VALIDATOR_RESTARTED="${TMPDIR_TEST}/bprd-validator-restarted"
# ssh stub: sentinel write fails; validator restart tracked.
make_stub "${BPRD_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":100,\"height\":100,\"peer_count\":1}'
elif echo \"\$CMD\" | grep -qE '\[ -d '; then
  exit 0
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'   # validator stop succeeds
elif echo \"\$CMD\" | grep -q 'systemctl start aperod-node'; then
  touch '${BPRD_VALIDATOR_RESTARTED}'
  echo 'started'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

# Override 'touch' stub so that when join-network.sh calls `touch ${_BS_SENTINEL}`
# it fails. We selectively fail on the .rsync-in-progress path only.
make_stub "${BPRD_BIN}" "touch" "
case \"\$*\" in
  *rsync-in-progress*) exit 1 ;;   # sentinel write fails
  *)                   /usr/bin/touch \"\$@\" ;;
esac
"

make_stub "${BPRD_BIN}" "rsync"  'exit 0'
make_stub "${BPRD_BIN}" "chown"  'exit 0'
make_stub "${BPRD_BIN}" "sleep"  'exit 0'

BPRD_EXIT=0
BPRD_OUTPUT=$(
  env \
    PATH="${BPRD_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BPRD_DATA}" \
    LOCAL_DATA_DIR="${BPRD_DATA}" \
    LOCAL_NODE_YAML="${BPRD_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BPRD_CONFIG}" \
    PRIMARY_DATA_DIR="${BPRD_DATA}" \
    VALIDATOR_DATA_DIR="${BPRD_DATA}" \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.20" 2>&1
) || BPRD_EXIT=$?

if [[ ${BPRD_EXIT} -ne 0 ]]; then
  pass "BPR1: bootstrap exited non-zero after sentinel-write failure"
else
  fail "BPR1: expected non-zero exit but got 0. Output:\n${BPRD_OUTPUT}"
fi

if [[ -f "${BPRD_LOCAL_RESTARTED}" ]]; then
  pass "BPR2: trap restarted local relay (data was untouched — rsync never ran)"
else
  fail "BPR2: local relay was NOT restarted by trap — node left stopped unnecessarily. Output:\n${BPRD_OUTPUT}"
fi

if [[ -f "${BPRD_VALIDATOR_RESTARTED}" ]]; then
  pass "BPR3: trap restarted validator (source node was stopped for rsync)"
else
  fail "BPR3: validator was NOT restarted by trap. Output:\n${BPRD_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP POST-TRANSFER FAILURE — sentinel removed, later step fails → local restarted
# =============================================================================
section "Bootstrap post-transfer failure — rsync done, sentinel removed, later step fails → local restarted"
# After rsync succeeds and the sentinel is removed (_BS_RSYNC_STARTED=0), if
# a later step fails (e.g. validator restart, config update), the local data is
# consistent and the trap must restart the local relay.
#   BPT1 — script exits non-zero
#   BPT2 — local relay was restarted by the trap (data consistent)
#   BPT3 — sentinel is NOT present in LOCAL_DATA_DIR (was removed on success)

BPTF_DIR="${TMPDIR_TEST}/bptf"
BPTF_DATA="${BPTF_DIR}/data"
BPTF_BIN="${BPTF_DIR}/bin"
BPTF_YAML="${BPTF_DIR}/node.yaml"
BPTF_CONFIG="${BPTF_DIR}/node-config.sh"

mkdir -p "${BPTF_DATA}/chain.db"
touch "${BPTF_DATA}/chain.db/CURRENT"
touch "${BPTF_DATA}/snapshot-v2-500.json.gz"
printf 'network: testnet\n' > "${BPTF_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BPTF_CONFIG}"
chmod +x "${BPTF_CONFIG}"

BPTF_LOCAL_RESTARTED="${TMPDIR_TEST}/bptf-local-restarted"
make_stub "${BPTF_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*) touch '${BPTF_LOCAL_RESTARTED}'; exit 0 ;;
  *)                     exit 0 ;;
esac
"

# ssh stub: all rsync steps succeed; validator restart (step 5) FAILS.
BPTF_RSYNC_CALLED="${TMPDIR_TEST}/bptf-rsync-called"
make_stub "${BPTF_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":500,\"height\":500,\"peer_count\":1}'
elif echo \"\$CMD\" | grep -qE '\[ -d '; then
  exit 0
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'   # validator stop succeeds
elif echo \"\$CMD\" | grep -q 'systemctl start aperod-node'; then
  exit 1   # validator restart FAILS (triggers trap)
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

make_stub "${BPTF_BIN}" "rsync"  "touch '${BPTF_RSYNC_CALLED}'; exit 0"
make_stub "${BPTF_BIN}" "chown"  'exit 0'
make_stub "${BPTF_BIN}" "sleep"  'exit 0'

BPTF_EXIT=0
BPTF_OUTPUT=$(
  env \
    PATH="${BPTF_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BPTF_DATA}" \
    LOCAL_DATA_DIR="${BPTF_DATA}" \
    LOCAL_NODE_YAML="${BPTF_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BPTF_CONFIG}" \
    PRIMARY_DATA_DIR="${BPTF_DATA}" \
    VALIDATOR_DATA_DIR="${BPTF_DATA}" \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.21" 2>&1
) || BPTF_EXIT=$?

if [[ ${BPTF_EXIT} -ne 0 ]]; then
  pass "BPT1: bootstrap exited non-zero after post-transfer validator-restart failure"
else
  fail "BPT1: expected non-zero exit but got 0. Output:\n${BPTF_OUTPUT}"
fi

if [[ -f "${BPTF_LOCAL_RESTARTED}" ]]; then
  pass "BPT2: trap restarted local relay after post-transfer failure (data is consistent)"
else
  fail "BPT2: local relay was NOT restarted — node left stopped unnecessarily. Output:\n${BPTF_OUTPUT}"
fi

BPTF_SENTINEL="${BPTF_DATA}/.rsync-in-progress"
if [[ ! -f "${BPTF_SENTINEL}" ]]; then
  pass "BPT3: .rsync-in-progress is absent (removed after successful rsync)"
else
  fail "BPT3: .rsync-in-progress still present despite successful rsync — node would be permanently blocked"
fi

# =============================================================================
# ── FUSER GUARD — chain.db held open by a second (non-systemd) process ────────
# =============================================================================
section "Fuser guard — chain.db open by a second process → abort BEFORE rsync"
# Task 1907: systemctl is-active only covers the systemd service.  A manually
# launched node or stuck Go test may still hold LevelDB open.  join-network.sh
# must check fuser (or lsof) after the is-active wait loop and abort before
# rsync when the DB files are busy.
#   FB1 — script exits non-zero
#   FB2 — rsync was NOT called (corrupt copy prevented)
#   FB3 — output explains the DB is held open by another process
#   FB4 — trap restarted the source node (systemd stop had already succeeded)

FB_DATA="${TMPDIR_TEST}/fb-data"
mkdir -p "${FB_DATA}/chain.db"
touch "${FB_DATA}/chain.db/CURRENT"

FB_BIN="${TMPDIR_TEST}/fb-bin"

FB_SOURCE_RESTARTED="${TMPDIR_TEST}/fb-source-restarted"
make_stub "${FB_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)   exit 0 ;;
  *'is-active'*)          exit 1 ;;
  *'start aperod-node'*)  touch '${FB_SOURCE_RESTARTED}'; exit 0 ;;
  *)                      exit 0 ;;
esac
"

# ssh: target stop (step 1) succeeds; everything else neutral success.
make_stub "${FB_BIN}" "ssh" '
shift
cat >/dev/null
printf "stopped\nremoved\nstarted\n"
exit 0
'

# fuser: ALWAYS reports the files as busy (exit 0 = at least one process
# has the file open).  Simulates a manually launched node / stuck Go test.
make_stub "${FB_BIN}" "fuser" 'exit 0'

# rsync: must NOT be called — record invocation in a sentinel file.
FB_RSYNC_CALLED="${TMPDIR_TEST}/fb-rsync-called"
make_stub "${FB_BIN}" "rsync" "
touch '${FB_RSYNC_CALLED}'
exit 0
"

make_stub "${FB_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${FB_BIN}" "${FB_DATA}"

# ── Assertion FB1: script exits non-zero ─────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "FB1: script exited non-zero (${LAST_EXIT}) when chain.db is held open by a second process"
else
  fail "FB1: script should exit non-zero when fuser reports chain.db busy but exited 0"
fi

# ── Assertion FB2: rsync was NOT invoked ─────────────────────────────────────
if [[ ! -f "${FB_RSYNC_CALLED}" ]]; then
  pass "FB2: rsync was not called while chain.db was open — corrupt copy prevented"
else
  fail "FB2: rsync was called despite chain.db being open by another process"
fi

# ── Assertion FB3: output explains the busy-DB condition ─────────────────────
if echo "${LAST_OUTPUT}" | grep -q "другим процессом"; then
  pass "FB3: output contains 'другим процессом' (busy-DB explanation)"
else
  fail "FB3: expected 'другим процессом' in output. Got:\n${LAST_OUTPUT}"
fi

# ── Assertion FB4: trap restarted the source node ────────────────────────────
if [[ -f "${FB_SOURCE_RESTARTED}" ]]; then
  pass "FB4: trap restarted the source node after the fuser abort"
else
  fail "FB4: source node was not restarted after the fuser abort. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FUSER GUARD — DB free → rsync proceeds (fuser reports not busy) ───────────
# =============================================================================
section "Fuser guard — fuser reports chain.db free → rsync proceeds normally"
# Complement to the busy case: when the stubbed fuser exits 1 (no process has
# the files open) the guard must let the script continue and call rsync.

FF_DATA="${TMPDIR_TEST}/ff-data"
mkdir -p "${FF_DATA}/chain.db"
touch "${FF_DATA}/chain.db/CURRENT"

FF_BIN="${TMPDIR_TEST}/ff-bin"

make_stub "${FF_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;
  *)                      exit 0 ;;
esac
'

make_stub "${FF_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '\''{"height":321,"peer_count":1,"syncing":false}'\''
elif echo "$CMD" | grep -q "systemctl show aperod-node"; then
  echo "Environment=GOMEMLIMIT=5905580032"
  echo "TimeoutStopUSec=15min"
elif echo "$CMD" | grep -q "test -f"; then
  echo "yes"
else
  cat >/dev/null
  printf "stopped\nremoved\nstarted\n"
fi
exit 0
'

# fuser: exit 1 = no process holds the files open.
make_stub "${FF_BIN}" "fuser" 'exit 1'

FF_RSYNC_CALLED="${TMPDIR_TEST}/ff-rsync-called"
make_stub "${FF_BIN}" "rsync" "
touch '${FF_RSYNC_CALLED}'
exit 0
"

make_stub "${FF_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${FF_BIN}" "${FF_DATA}"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "FF1: script exited 0 when fuser reports chain.db free"
else
  fail "FF1: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${FF_RSYNC_CALLED}" ]]; then
  pass "FF2: rsync was called after fuser confirmed chain.db is free"
else
  fail "FF2: rsync was not called despite fuser reporting chain.db free"
fi

if echo "${LAST_OUTPUT}" | grep -q "rsync безопасен"; then
  pass "FF3: output confirms 'rsync безопасен' (guard passed message)"
else
  fail "FF3: expected 'rsync безопасен' in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP FUSER GUARD — validator chain.db busy (custom VALIDATOR_DATA_DIR)
# =============================================================================
section "Bootstrap fuser guard — validator chain.db open by a second process → abort BEFORE rsync"
# Task 1907 (bootstrap mode): the remote-stop heredoc receives the locally
# resolved VALIDATOR_DATA_DIR via a printf prologue and must probe THAT exact
# directory — the one rsync will copy — with fuser/lsof.  This test:
#   • uses a NON-default VALIDATOR_DATA_DIR (custom temp path)
#   • makes the ssh stub execute the heredoc payload for real (exec bash)
#   • stubs fuser as "busy" and records the paths it was asked about
# Assertions:
#   BFB1 — script exits non-zero
#   BFB2 — rsync was NOT called (corrupt copy prevented)
#   BFB3 — busy-DB error message appears in output
#   BFB4 — fuser probed the CUSTOM data dir, not the hardcoded default

BFB_DIR="${TMPDIR_TEST}/bfb"
BFB_DATA="${BFB_DIR}/local-data"
BFB_VDATA="${BFB_DIR}/custom-validator-data"   # non-default validator dir
BFB_BIN="${BFB_DIR}/bin"
BFB_YAML="${BFB_DIR}/node.yaml"
BFB_CONFIG="${BFB_DIR}/node-config.sh"

mkdir -p "${BFB_DATA}/chain.db" "${BFB_VDATA}/chain.db"
touch "${BFB_DATA}/chain.db/CURRENT" "${BFB_VDATA}/chain.db/CURRENT"
printf 'network: testnet\n' > "${BFB_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BFB_CONFIG}"
chmod +x "${BFB_CONFIG}"

make_stub "${BFB_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

# ssh stub: bare "bash" → execute the heredoc payload for real so the remote
# fuser guard actually runs (against the stubbed fuser in PATH).
make_stub "${BFB_BIN}" "ssh" '
shift   # drop root@IP
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":700,\"height\":700,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  exec bash   # run the prologue + heredoc payload for real
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# fuser: reports BUSY and records the arguments it was probed with.
BFB_FUSER_ARGS="${TMPDIR_TEST}/bfb-fuser-args"
make_stub "${BFB_BIN}" "fuser" "
echo \"\$@\" >> '${BFB_FUSER_ARGS}'
exit 0
"

BFB_RSYNC_CALLED="${TMPDIR_TEST}/bfb-rsync-called"
make_stub "${BFB_BIN}" "rsync" "touch '${BFB_RSYNC_CALLED}'; exit 0"
make_stub "${BFB_BIN}" "chown" 'exit 0'
make_stub "${BFB_BIN}" "sleep" 'exit 0'
make_stub "${BFB_BIN}" "curl"  'echo ""; exit 0'

BFB_EXIT=0
BFB_OUTPUT=$(
  env \
    PATH="${BFB_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BFB_DATA}" \
    LOCAL_DATA_DIR="${BFB_DATA}" \
    LOCAL_NODE_YAML="${BFB_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BFB_CONFIG}" \
    PRIMARY_DATA_DIR="${BFB_DATA}" \
    VALIDATOR_DATA_DIR="${BFB_VDATA}" \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.30" 2>&1
) || BFB_EXIT=$?

if [[ ${BFB_EXIT} -ne 0 ]]; then
  pass "BFB1: bootstrap exited non-zero (${BFB_EXIT}) when validator chain.db is held open"
else
  fail "BFB1: expected non-zero exit when validator chain.db is busy but got 0. Output:\n${BFB_OUTPUT}"
fi

if [[ ! -f "${BFB_RSYNC_CALLED}" ]]; then
  pass "BFB2: rsync was not called while validator chain.db was open — corrupt copy prevented"
else
  fail "BFB2: rsync was called despite validator chain.db being open by another process"
fi

if echo "${BFB_OUTPUT}" | grep -q "другим процессом"; then
  pass "BFB3: output contains 'другим процессом' (busy-DB explanation)"
else
  fail "BFB3: expected 'другим процессом' in output. Got:\n${BFB_OUTPUT}"
fi

# BFB4: fuser must have been asked about files inside the CUSTOM data dir —
# proving the locally resolved VALIDATOR_DATA_DIR reached the remote shell.
if [[ -f "${BFB_FUSER_ARGS}" ]] && grep -q "${BFB_VDATA}/chain.db" "${BFB_FUSER_ARGS}"; then
  pass "BFB4: fuser probed the custom VALIDATOR_DATA_DIR (${BFB_VDATA}/chain.db)"
else
  fail "BFB4: fuser did not probe the custom data dir — guard checked the wrong directory. Probed: $(cat "${BFB_FUSER_ARGS}" 2>/dev/null || echo '<nothing>')"
fi

# =============================================================================
# ── BOOTSTRAP FUSER GUARD — validator chain.db free → rsync proceeds ──────────
# =============================================================================
section "Bootstrap fuser guard — validator chain.db free → rsync proceeds"
# Complement: with the same custom VALIDATOR_DATA_DIR and a real heredoc run,
# a fuser reporting "not busy" (exit 1) must let bootstrap continue to rsync.
#   BFF1 — script exits 0
#   BFF2 — rsync WAS called after the guard passed

BFF_DIR="${TMPDIR_TEST}/bff"
BFF_DATA="${BFF_DIR}/local-data"
BFF_VDATA="${BFF_DIR}/custom-validator-data"
BFF_BIN="${BFF_DIR}/bin"
BFF_YAML="${BFF_DIR}/node.yaml"
BFF_CONFIG="${BFF_DIR}/node-config.sh"

mkdir -p "${BFF_DATA}/chain.db" "${BFF_VDATA}/chain.db"
touch "${BFF_DATA}/chain.db/CURRENT" "${BFF_VDATA}/chain.db/CURRENT"
touch "${BFF_DATA}/snapshot-v2-800.json.gz"
printf 'network: testnet\n' > "${BFF_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BFF_CONFIG}"
chmod +x "${BFF_CONFIG}"

make_stub "${BFF_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

make_stub "${BFF_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":800,\"height\":800,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  exec bash   # run the prologue + heredoc payload for real
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# fuser: exit 1 = no process holds the files open.
make_stub "${BFF_BIN}" "fuser" 'exit 1'

BFF_RSYNC_CALLED="${TMPDIR_TEST}/bff-rsync-called"
make_stub "${BFF_BIN}" "rsync" "touch '${BFF_RSYNC_CALLED}'; exit 0"
make_stub "${BFF_BIN}" "chown" 'exit 0'
make_stub "${BFF_BIN}" "sleep" 'exit 0'
make_stub "${BFF_BIN}" "curl"  'echo "{\"height\":800,\"peer_count\":1}"; exit 0'

BFF_EXIT=0
BFF_OUTPUT=$(
  env \
    PATH="${BFF_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BFF_DATA}" \
    LOCAL_DATA_DIR="${BFF_DATA}" \
    LOCAL_NODE_YAML="${BFF_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BFF_CONFIG}" \
    PRIMARY_DATA_DIR="${BFF_DATA}" \
    VALIDATOR_DATA_DIR="${BFF_VDATA}" \
    HEALTH_MAX_ATTEMPTS=5 \
    HEALTH_WAIT_SECS=0 \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.31" 2>&1
) || BFF_EXIT=$?

if [[ ${BFF_EXIT} -eq 0 ]]; then
  pass "BFF1: bootstrap exited 0 when fuser reports validator chain.db free"
else
  fail "BFF1: expected exit 0 but got ${BFF_EXIT}. Output:\n${BFF_OUTPUT}"
fi

if [[ -f "${BFF_RSYNC_CALLED}" ]]; then
  pass "BFF2: rsync was called after the validator fuser guard passed"
else
  fail "BFF2: rsync was not called despite fuser reporting validator chain.db free. Output:\n${BFF_OUTPUT}"
fi

# =============================================================================
# ── FUSER GUARD — inspection tool failure → fail closed (push mode) ───────────
# =============================================================================
section "Fuser guard — fuser returns unexpected code → fail closed, no rsync"
# When the inspection command itself fails (exit code other than the documented
# 0=busy / 1=free), the result cannot be trusted and the script must abort
# BEFORE rsync (fail closed) — not treat the DB as free.
#   FE1 — script exits non-zero
#   FE2 — rsync was NOT called
#   FE3 — output contains the fail-closed explanation
#   FE4 — trap restarted the source node

FE_DATA="${TMPDIR_TEST}/fe-data"
mkdir -p "${FE_DATA}/chain.db"
touch "${FE_DATA}/chain.db/CURRENT"

FE_BIN="${TMPDIR_TEST}/fe-bin"

FE_SOURCE_RESTARTED="${TMPDIR_TEST}/fe-source-restarted"
make_stub "${FE_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)   exit 0 ;;
  *'is-active'*)          exit 1 ;;
  *'start aperod-node'*)  touch '${FE_SOURCE_RESTARTED}'; exit 0 ;;
  *)                      exit 0 ;;
esac
"

make_stub "${FE_BIN}" "ssh" '
shift
cat >/dev/null
printf "stopped\nremoved\nstarted\n"
exit 0
'

# fuser: exits 4 — neither the documented busy (0) nor free (1) result.
make_stub "${FE_BIN}" "fuser" 'exit 4'

FE_RSYNC_CALLED="${TMPDIR_TEST}/fe-rsync-called"
make_stub "${FE_BIN}" "rsync" "touch '${FE_RSYNC_CALLED}'; exit 0"
make_stub "${FE_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${FE_BIN}" "${FE_DATA}"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "FE1: script exited non-zero (${LAST_EXIT}) when fuser returned an unexpected code"
else
  fail "FE1: script should exit non-zero on untrusted inspection result but exited 0"
fi

if [[ ! -f "${FE_RSYNC_CALLED}" ]]; then
  pass "FE2: rsync was not called after inspection-command failure (fail closed)"
else
  fail "FE2: rsync was called despite the inspection command failing"
fi

if echo "${LAST_OUTPUT}" | grep -q "нельзя доверять"; then
  pass "FE3: output contains 'нельзя доверять' (untrusted-result explanation)"
else
  fail "FE3: expected 'нельзя доверять' in output. Got:\n${LAST_OUTPUT}"
fi

if [[ -f "${FE_SOURCE_RESTARTED}" ]]; then
  pass "FE4: trap restarted the source node after the fail-closed abort"
else
  fail "FE4: source node was not restarted after the fail-closed abort. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FUSER GUARD — neither fuser nor lsof installed → fail closed (push mode) ──
# =============================================================================
section "Fuser guard — neither fuser nor lsof available → fail closed, no rsync"
# The guard must not fail open when no inspection tool is installed: without
# fuser/lsof the script cannot prove the DB is free and must abort BEFORE
# rsync with installation guidance.  A restricted PATH (symlink farm of the
# real system tools MINUS fuser/lsof, plus the usual stubs) simulates the
# missing-tool environment.
#   MT1 — script exits non-zero
#   MT2 — rsync was NOT called
#   MT3 — output names the missing tools and gives install guidance
#   MT4 — trap restarted the source node

MT_DATA="${TMPDIR_TEST}/mt-data"
mkdir -p "${MT_DATA}/chain.db"
touch "${MT_DATA}/chain.db/CURRENT"

MT_BIN="${TMPDIR_TEST}/mt-bin"
MT_FARM="${TMPDIR_TEST}/mt-farm"
mkdir -p "${MT_FARM}"

# Symlink every real tool join-network.sh needs — except fuser and lsof.
for _c in bash sh grep sed seq cat printf echo python3 curl ls rm mkdir touch \
          env awk head tail sort mktemp dirname basename hostname tr chmod chown; do
  _p="$(command -v "${_c}" 2>/dev/null || true)"
  [[ -n "${_p}" ]] && ln -sf "${_p}" "${MT_FARM}/${_c}"
done

MT_SOURCE_RESTARTED="${TMPDIR_TEST}/mt-source-restarted"
make_stub "${MT_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)   exit 0 ;;
  *'is-active'*)          exit 1 ;;
  *'start aperod-node'*)  touch '${MT_SOURCE_RESTARTED}'; exit 0 ;;
  *)                      exit 0 ;;
esac
"

make_stub "${MT_BIN}" "ssh" '
shift
cat >/dev/null
printf "stopped\nremoved\nstarted\n"
exit 0
'

MT_RSYNC_CALLED="${TMPDIR_TEST}/mt-rsync-called"
make_stub "${MT_BIN}" "rsync" "touch '${MT_RSYNC_CALLED}'; exit 0"
make_stub "${MT_BIN}" "sleep" 'exit 0'

MT_EXIT=0
MT_OUTPUT=$(
  env \
    PATH="${MT_BIN}:${MT_FARM}" \
    PRIMARY_IP="${PRIMARY_IP}" \
    PRIMARY_DATA_DIR="${MT_DATA}" \
    bash "${JOIN_SH}" "${TARGET_IP}" 2>&1
) || MT_EXIT=$?

if [[ ${MT_EXIT} -ne 0 ]]; then
  pass "MT1: script exited non-zero (${MT_EXIT}) when neither fuser nor lsof is available"
else
  fail "MT1: script should fail closed without an inspection tool but exited 0. Output:\n${MT_OUTPUT}"
fi

if [[ ! -f "${MT_RSYNC_CALLED}" ]]; then
  pass "MT2: rsync was not called without an inspection tool (fail closed)"
else
  fail "MT2: rsync was called despite no inspection tool being available"
fi

if echo "${MT_OUTPUT}" | grep -q "Ни fuser, ни lsof" && echo "${MT_OUTPUT}" | grep -q "psmisc"; then
  pass "MT3: output names the missing tools and gives install guidance (psmisc)"
else
  fail "MT3: expected 'Ни fuser, ни lsof' + install guidance in output. Got:\n${MT_OUTPUT}"
fi

if [[ -f "${MT_SOURCE_RESTARTED}" ]]; then
  pass "MT4: trap restarted the source node after the fail-closed abort"
else
  fail "MT4: source node was not restarted after the fail-closed abort. Output:\n${MT_OUTPUT}"
fi

# =============================================================================
# ── BOOTSTRAP FUSER GUARD — inspection failure on validator → fail closed ─────
# =============================================================================
section "Bootstrap fuser guard — fuser fails on validator → fail closed, no rsync"
# Same fail-closed policy inside the bootstrap remote-stop heredoc: an
# unexpected fuser exit code on the validator must abort before rsync.
#   BFE1 — script exits non-zero
#   BFE2 — rsync was NOT called

BFE_DIR="${TMPDIR_TEST}/bfe"
BFE_DATA="${BFE_DIR}/local-data"
BFE_VDATA="${BFE_DIR}/validator-data"
BFE_BIN="${BFE_DIR}/bin"
BFE_YAML="${BFE_DIR}/node.yaml"
BFE_CONFIG="${BFE_DIR}/node-config.sh"

mkdir -p "${BFE_DATA}/chain.db" "${BFE_VDATA}/chain.db"
touch "${BFE_DATA}/chain.db/CURRENT" "${BFE_VDATA}/chain.db/CURRENT"
printf 'network: testnet\n' > "${BFE_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' > "${BFE_CONFIG}"
chmod +x "${BFE_CONFIG}"

make_stub "${BFE_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'

make_stub "${BFE_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo "{\"tip_height\":900,\"height\":900,\"peer_count\":1}"
elif echo "$CMD" | grep -qE "\[ -d "; then
  exit 0
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  exec bash   # run the prologue + heredoc payload for real
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# fuser: unexpected exit code 4 → untrusted result → fail closed.
make_stub "${BFE_BIN}" "fuser" 'exit 4'

BFE_RSYNC_CALLED="${TMPDIR_TEST}/bfe-rsync-called"
make_stub "${BFE_BIN}" "rsync" "touch '${BFE_RSYNC_CALLED}'; exit 0"
make_stub "${BFE_BIN}" "chown" 'exit 0'
make_stub "${BFE_BIN}" "sleep" 'exit 0'
make_stub "${BFE_BIN}" "curl"  'echo ""; exit 0'

BFE_EXIT=0
BFE_OUTPUT=$(
  env \
    PATH="${BFE_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${BFE_DATA}" \
    LOCAL_DATA_DIR="${BFE_DATA}" \
    LOCAL_NODE_YAML="${BFE_YAML}" \
    LOCAL_NODE_CONFIG_SH="${BFE_CONFIG}" \
    PRIMARY_DATA_DIR="${BFE_DATA}" \
    VALIDATOR_DATA_DIR="${BFE_VDATA}" \
    bash "${JOIN_SH}" "--bootstrap-from=192.0.2.32" 2>&1
) || BFE_EXIT=$?

if [[ ${BFE_EXIT} -ne 0 ]]; then
  pass "BFE1: bootstrap exited non-zero (${BFE_EXIT}) when validator fuser returned an unexpected code"
else
  fail "BFE1: expected non-zero exit on untrusted validator inspection but got 0. Output:\n${BFE_OUTPUT}"
fi

if [[ ! -f "${BFE_RSYNC_CALLED}" ]]; then
  pass "BFE2: rsync was not called after the validator inspection failure (fail closed)"
else
  fail "BFE2: rsync was called despite the validator inspection command failing"
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
