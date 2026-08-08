#!/usr/bin/env bash
# =============================================================================
#  test-ensure-dropin.sh — Tests for ensure-dropin.sh
#
#  Strategy: run the real ensure-dropin.sh with injectable DROPIN_DIR and
#  SYSTEMCTL seams so the tests need no root access and no live systemd.
#  Tests verify:
#    1. Both drop-in files are created on first run.
#    2. timeout.conf has [Service] + TimeoutStopSec=900.
#    3. gomemlimit.conf has [Service] + Environment="GOMEMLIMIT=5368709120".
#    4. A second run is idempotent: file content is unchanged (mtime stable).
#    5. daemon-reload is called on every run, even when nothing changed.
#    6. Static analysis: ensure-dropin.sh calls ${SYSTEMCTL} daemon-reload.
#    7. Static analysis: both drop-in conf names referenced in ensure-dropin.sh.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-ensure-dropin.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENSURE_SH="$SCRIPT_DIR/ensure-dropin.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Ensure ensure-dropin.sh exists ───────────────────────────────────────────
if [[ ! -f "$ENSURE_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  ensure-dropin.sh not found at: $ENSURE_SH" >&2
  exit 1
fi

# ── Shared temp directory (cleaned on exit) ───────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# make_fake_bin CMD LOG_FILE
#   Creates a stub binary for CMD in a fresh temp dir.  Every invocation
#   appends "CMD <args>" to LOG_FILE and exits 0.  Prints the bin dir path.
# ---------------------------------------------------------------------------
make_fake_bin() {
  local cmd="$1" log_file="$2"
  local fake_dir
  fake_dir=$(mktemp -d "$TMPDIR_TEST/fake-bin-XXXXXXXX")
  cat >"$fake_dir/$cmd" <<STUB
#!/usr/bin/env bash
echo "$cmd \$*" >> "$log_file"
exit 0
STUB
  chmod +x "$fake_dir/$cmd"
  echo "$fake_dir"
}

# ---------------------------------------------------------------------------
# run_ensure_dropin DROPIN_DIR SC_LOG
#   Runs ensure-dropin.sh with DROPIN_DIR and SYSTEMCTL injected.
#   Appends systemctl invocations to SC_LOG.
# ---------------------------------------------------------------------------
run_ensure_dropin() {
  local dropin_dir="$1"
  local sc_log="$2"
  local fake_sc_dir
  fake_sc_dir=$(make_fake_bin "systemctl" "$sc_log")

  DROPIN_DIR="$dropin_dir" \
  SYSTEMCTL="$fake_sc_dir/systemctl" \
  bash "$ENSURE_SH"
}

# =============================================================================
# Test 1: both drop-in files created on first run
# =============================================================================
section "Test 1: both drop-in files are created on first run"

T1_DIR=$(mktemp -d "$TMPDIR_TEST/t1-XXXXXXXX")
T1_SC_LOG="$TMPDIR_TEST/t1-sc.log"

if run_ensure_dropin "$T1_DIR" "$T1_SC_LOG" >/dev/null 2>&1; then
  pass "ensure-dropin.sh exited 0 on first run"
else
  fail "ensure-dropin.sh exited non-zero on first run"
fi

if [[ -f "$T1_DIR/timeout.conf" ]]; then
  pass "timeout.conf was created"
else
  fail "timeout.conf was NOT created"
fi

if [[ -f "$T1_DIR/gomemlimit.conf" ]]; then
  pass "gomemlimit.conf was created"
else
  fail "gomemlimit.conf was NOT created"
fi

# =============================================================================
# Test 2: timeout.conf has correct content
# =============================================================================
section "Test 2: timeout.conf has [Service] and TimeoutStopSec=900"

if grep -q "^\[Service\]$" "$T1_DIR/timeout.conf" 2>/dev/null; then
  pass "[Service] header found in timeout.conf"
else
  fail "[Service] header NOT found in timeout.conf (content: $(cat "$T1_DIR/timeout.conf" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^TimeoutStopSec=900$" "$T1_DIR/timeout.conf" 2>/dev/null; then
  pass "TimeoutStopSec=900 found in timeout.conf"
else
  fail "TimeoutStopSec=900 NOT found in timeout.conf (content: $(cat "$T1_DIR/timeout.conf" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 3: gomemlimit.conf has correct content
# =============================================================================
section "Test 3: gomemlimit.conf has [Service] and Environment=\"GOMEMLIMIT=5368709120\""

if grep -q "^\[Service\]$" "$T1_DIR/gomemlimit.conf" 2>/dev/null; then
  pass "[Service] header found in gomemlimit.conf"
else
  fail "[Service] header NOT found in gomemlimit.conf (content: $(cat "$T1_DIR/gomemlimit.conf" 2>/dev/null || echo '<missing>'))"
fi

if grep -q '^Environment="GOMEMLIMIT=5368709120"$' "$T1_DIR/gomemlimit.conf" 2>/dev/null; then
  pass "Environment=\"GOMEMLIMIT=5368709120\" found in gomemlimit.conf"
else
  fail "Environment=\"GOMEMLIMIT=5368709120\" NOT found (content: $(cat "$T1_DIR/gomemlimit.conf" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 4: second run is idempotent — file content unchanged
# =============================================================================
section "Test 4: second run is idempotent (file content identical)"

T4_DIR=$(mktemp -d "$TMPDIR_TEST/t4-XXXXXXXX")
T4_SC_LOG="$TMPDIR_TEST/t4-sc.log"

# First run
run_ensure_dropin "$T4_DIR" "$T4_SC_LOG" >/dev/null 2>&1

TIMEOUT_AFTER_RUN1=$(cat "$T4_DIR/timeout.conf")
GOMEMLIMIT_AFTER_RUN1=$(cat "$T4_DIR/gomemlimit.conf")

# Second run
run_ensure_dropin "$T4_DIR" "$T4_SC_LOG" >/dev/null 2>&1

TIMEOUT_AFTER_RUN2=$(cat "$T4_DIR/timeout.conf")
GOMEMLIMIT_AFTER_RUN2=$(cat "$T4_DIR/gomemlimit.conf")

if [[ "$TIMEOUT_AFTER_RUN1" == "$TIMEOUT_AFTER_RUN2" ]]; then
  pass "timeout.conf content unchanged after second run"
else
  fail "timeout.conf content changed on second run"
fi

if [[ "$GOMEMLIMIT_AFTER_RUN1" == "$GOMEMLIMIT_AFTER_RUN2" ]]; then
  pass "gomemlimit.conf content unchanged after second run"
else
  fail "gomemlimit.conf content changed on second run"
fi

# =============================================================================
# Test 5: daemon-reload called on every run, even when nothing changed
# =============================================================================
section "Test 5: daemon-reload is called on every run (even idempotent runs)"

T5_DIR=$(mktemp -d "$TMPDIR_TEST/t5-XXXXXXXX")
T5_SC_LOG="$TMPDIR_TEST/t5-sc.log"

# First run
run_ensure_dropin "$T5_DIR" "$T5_SC_LOG" >/dev/null 2>&1

RUN1_RELOAD_COUNT=$(grep -c "systemctl daemon-reload" "$T5_SC_LOG" 2>/dev/null || echo "0")
if [[ "$RUN1_RELOAD_COUNT" -ge 1 ]]; then
  pass "daemon-reload called on first run (count: $RUN1_RELOAD_COUNT)"
else
  fail "daemon-reload NOT called on first run (log: $(cat "$T5_SC_LOG" 2>/dev/null || echo '<empty>'))"
fi

# Second run (nothing to write)
run_ensure_dropin "$T5_DIR" "$T5_SC_LOG" >/dev/null 2>&1

RUN2_RELOAD_COUNT=$(grep -c "systemctl daemon-reload" "$T5_SC_LOG" 2>/dev/null || echo "0")
if [[ "$RUN2_RELOAD_COUNT" -ge 2 ]]; then
  pass "daemon-reload called again on second run (total count: $RUN2_RELOAD_COUNT)"
else
  fail "daemon-reload NOT called on second run (total count: $RUN2_RELOAD_COUNT)"
fi

# =============================================================================
# Test 6: static analysis — ensure-dropin.sh references SYSTEMCTL daemon-reload
# =============================================================================
section "Test 6: static analysis — ensure-dropin.sh calls \${SYSTEMCTL} daemon-reload"

if grep -q 'daemon-reload' "$ENSURE_SH"; then
  pass "daemon-reload referenced in ensure-dropin.sh"
else
  fail "daemon-reload NOT referenced in ensure-dropin.sh"
fi

if grep -q 'SYSTEMCTL' "$ENSURE_SH"; then
  pass "SYSTEMCTL variable referenced in ensure-dropin.sh (injectable seam present)"
else
  fail "SYSTEMCTL variable NOT referenced — daemon-reload seam is missing"
fi

# =============================================================================
# Test 7: static analysis — both conf filenames referenced in ensure-dropin.sh
# =============================================================================
section "Test 7: static analysis — both conf filenames referenced in ensure-dropin.sh"

if grep -q 'timeout\.conf' "$ENSURE_SH"; then
  pass "timeout.conf referenced in ensure-dropin.sh"
else
  fail "timeout.conf NOT referenced in ensure-dropin.sh"
fi

if grep -q 'gomemlimit\.conf' "$ENSURE_SH"; then
  pass "gomemlimit.conf referenced in ensure-dropin.sh"
else
  fail "gomemlimit.conf NOT referenced in ensure-dropin.sh"
fi

# =============================================================================
# Test 8: static analysis — install-validator.sh calls ensure-dropin.sh
# =============================================================================
section "Test 8: static analysis — install-validator.sh delegates to ensure-dropin.sh"

INSTALL_VAL_SH="$SCRIPT_DIR/install-validator.sh"
if [[ -f "$INSTALL_VAL_SH" ]]; then
  if grep -q 'ensure-dropin\.sh' "$INSTALL_VAL_SH"; then
    pass "install-validator.sh calls ensure-dropin.sh"
  else
    fail "install-validator.sh does NOT call ensure-dropin.sh"
  fi
else
  fail "install-validator.sh not found at $INSTALL_VAL_SH"
fi

# =============================================================================
# Test 9: static analysis — join-network.sh calls ensure-dropin.sh at the
# correct deployed path on the secondary server
# =============================================================================
section "Test 9: static analysis — join-network.sh uses correct deployed path for ensure-dropin.sh"

JOIN_SH="$SCRIPT_DIR/join-network.sh"
EXPECTED_REMOTE_PATH="/opt/aperod/blockchain/deploy/ensure-dropin.sh"
if [[ -f "$JOIN_SH" ]]; then
  if grep -q "${EXPECTED_REMOTE_PATH}" "$JOIN_SH"; then
    pass "join-network.sh references the correct remote path: ${EXPECTED_REMOTE_PATH}"
  else
    fail "join-network.sh does NOT reference '${EXPECTED_REMOTE_PATH}' — wrong deployed path"
    # Show what it does reference for diagnosis
    ACTUAL=$(grep 'ensure-dropin' "$JOIN_SH" || echo '<not found>')
    echo "       Actual: $ACTUAL"
  fi
else
  fail "join-network.sh not found at $JOIN_SH"
fi

# =============================================================================
# Summary
# =============================================================================
echo ""
echo "─────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL tests passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL tests FAILED.${NC}"
  exit 1
fi
