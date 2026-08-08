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
    bash "${JOIN_SH}" "${tip}" 2>&1
  ) || LAST_EXIT=$?
}

# =============================================================================
# ── NEGATIVE PATH ─────────────────────────────────────────────────────────────
# =============================================================================
section "Negative path — source node refuses to stop → script must abort"

NEG_DATA="${TMPDIR_TEST}/neg-data"
mkdir -p "${NEG_DATA}"

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
mkdir -p "${TMO_DATA}"

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
#   • anything else         → print "stopped" / "removed" / "started" and exit 0
# The heredoc for step 5/7 (bootnode injection) arrives via stdin when the
# remote command is just "bash"; the stub reads and discards stdin then exits 0.
make_stub "${POS_BIN}" "ssh" '
shift   # drop "root@IP"
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '\''{"height":54321,"peer_count":2,"syncing":false}'\''
elif echo "$CMD" | grep -q "curl"; then
  echo '\''{"ok":true}'\''
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
