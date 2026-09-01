#!/usr/bin/env bash
# =============================================================================
#  test-ensure-dropin.sh — Tests for ensure-dropin.sh
#
#  Strategy: run the real ensure-dropin.sh with injectable DROPIN_DIR and
#  SYSTEMCTL seams so the tests need no root access and no live systemd.
#  Tests verify:
#    1. Both drop-in files are created on first run.
#    2. timeout.conf has [Service] + TimeoutStopSec=900.
#    3. gomemlimit.conf has [Service] + Environment="GOMEMLIMIT=<canonical>",
#       where the expected value is parsed from a temp canonical drop-in file
#       (CANONICAL_DROPIN seam) — decoupled from any hard-coded literal.
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

# ── Canonical drop-in (source of truth) injected via CANONICAL_DROPIN seam ────
# ensure-dropin.sh parses the expected GOMEMLIMIT from this file instead of a
# hard-coded literal.  Point it at a temp file with a known value so the tests
# stay decoupled from whatever the repo's production number happens to be.
EXPECTED_GOMEMLIMIT="5905580032"
CANONICAL_DROPIN_FILE="$TMPDIR_TEST/canonical-gomemlimit.conf"
printf '[Service]\nEnvironment="GOMEMLIMIT=%s"\n' "$EXPECTED_GOMEMLIMIT" \
  > "$CANONICAL_DROPIN_FILE"

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
  CANONICAL_DROPIN="$CANONICAL_DROPIN_FILE" \
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
section "Test 3: gomemlimit.conf has [Service] and Environment=\"GOMEMLIMIT=<canonical>\" (parsed from CANONICAL_DROPIN)"

if grep -q "^\[Service\]$" "$T1_DIR/gomemlimit.conf" 2>/dev/null; then
  pass "[Service] header found in gomemlimit.conf"
else
  fail "[Service] header NOT found in gomemlimit.conf (content: $(cat "$T1_DIR/gomemlimit.conf" 2>/dev/null || echo '<missing>'))"
fi

# The written value must equal the canonical value ensure-dropin.sh parsed from
# CANONICAL_DROPIN_FILE (not a hard-coded literal).  If ensure-dropin.sh ever
# reverts to hard-coding a different number, this assertion fails.
if grep -q "^Environment=\"GOMEMLIMIT=${EXPECTED_GOMEMLIMIT}\"$" "$T1_DIR/gomemlimit.conf" 2>/dev/null; then
  pass "Environment=\"GOMEMLIMIT=${EXPECTED_GOMEMLIMIT}\" (canonical value) found in gomemlimit.conf"
else
  fail "Environment=\"GOMEMLIMIT=${EXPECTED_GOMEMLIMIT}\" NOT found (content: $(cat "$T1_DIR/gomemlimit.conf" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 3b: the written GOMEMLIMIT tracks the canonical file (proves the value
#          is parsed from CANONICAL_DROPIN, not hard-coded in ensure-dropin.sh)
# =============================================================================
section "Test 3b: written GOMEMLIMIT tracks the canonical drop-in value"

T3B_DIR=$(mktemp -d "$TMPDIR_TEST/t3b-XXXXXXXX")
T3B_SC_LOG="$TMPDIR_TEST/t3b-sc.log"
T3B_CANON="$TMPDIR_TEST/t3b-canonical.conf"
T3B_VALUE="4294967296"   # arbitrary distinct value (4 GiB)
printf '[Service]\nEnvironment="GOMEMLIMIT=%s"\n' "$T3B_VALUE" > "$T3B_CANON"

T3B_FAKE_SC=$(make_fake_bin "systemctl" "$T3B_SC_LOG")
if DROPIN_DIR="$T3B_DIR" \
   SYSTEMCTL="$T3B_FAKE_SC/systemctl" \
   CANONICAL_DROPIN="$T3B_CANON" \
   bash "$ENSURE_SH" >/dev/null 2>&1; then
  if grep -q "^Environment=\"GOMEMLIMIT=${T3B_VALUE}\"$" "$T3B_DIR/gomemlimit.conf" 2>/dev/null; then
    pass "written value follows the canonical file (GOMEMLIMIT=${T3B_VALUE})"
  else
    fail "written value did NOT follow canonical file (content: $(cat "$T3B_DIR/gomemlimit.conf" 2>/dev/null || echo '<missing>'))"
  fi
else
  fail "ensure-dropin.sh exited non-zero with a valid canonical drop-in"
fi

# =============================================================================
# Test 3c: ensure-dropin.sh dies when the canonical drop-in is missing
# =============================================================================
section "Test 3c: ensure-dropin.sh fails fast when CANONICAL_DROPIN is missing"

T3C_DIR=$(mktemp -d "$TMPDIR_TEST/t3c-XXXXXXXX")
T3C_SC_LOG="$TMPDIR_TEST/t3c-sc.log"
T3C_FAKE_SC=$(make_fake_bin "systemctl" "$T3C_SC_LOG")
T3C_MISSING="$TMPDIR_TEST/t3c-does-not-exist.conf"   # never created

T3C_EXIT=0
DROPIN_DIR="$T3C_DIR" \
SYSTEMCTL="$T3C_FAKE_SC/systemctl" \
CANONICAL_DROPIN="$T3C_MISSING" \
bash "$ENSURE_SH" >/dev/null 2>&1 || T3C_EXIT=$?

if [[ "$T3C_EXIT" -ne 0 ]]; then
  pass "ensure-dropin.sh exited non-zero ($T3C_EXIT) when canonical drop-in absent"
else
  fail "ensure-dropin.sh should have failed with a missing canonical drop-in but exited 0"
fi

if [[ ! -f "$T3C_DIR/gomemlimit.conf" ]]; then
  pass "no gomemlimit.conf written when canonical value could not be resolved"
else
  fail "gomemlimit.conf was written despite the canonical drop-in being absent"
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
# Test 10: second run does not change file mtime (write_if_changed is truly
#          a no-op when content is already correct)
# =============================================================================
section "Test 10: second run does not change file mtime (no spurious rewrite)"

T10_DIR=$(mktemp -d "$TMPDIR_TEST/t10-XXXXXXXX")
T10_SC_LOG="$TMPDIR_TEST/t10-sc.log"

# First run — create the files
run_ensure_dropin "$T10_DIR" "$T10_SC_LOG" >/dev/null 2>&1

# Sleep >1 s so any rewrite on the second run would produce a visibly different
# mtime (filesystem timestamps have 1-second resolution on most kernels).
sleep 1.1

# Capture mtime before second run
if stat --version >/dev/null 2>&1; then
  # GNU stat (Linux)
  TIMEOUT_MTIME_BEFORE=$(stat -c '%Y' "$T10_DIR/timeout.conf")
  GOMEMLIMIT_MTIME_BEFORE=$(stat -c '%Y' "$T10_DIR/gomemlimit.conf")
else
  # BSD stat (macOS)
  TIMEOUT_MTIME_BEFORE=$(stat -f '%m' "$T10_DIR/timeout.conf")
  GOMEMLIMIT_MTIME_BEFORE=$(stat -f '%m' "$T10_DIR/gomemlimit.conf")
fi

# Second run — should be a complete no-op for file writes
run_ensure_dropin "$T10_DIR" "$T10_SC_LOG" >/dev/null 2>&1

# Capture mtime after second run
if stat --version >/dev/null 2>&1; then
  TIMEOUT_MTIME_AFTER=$(stat -c '%Y' "$T10_DIR/timeout.conf")
  GOMEMLIMIT_MTIME_AFTER=$(stat -c '%Y' "$T10_DIR/gomemlimit.conf")
else
  TIMEOUT_MTIME_AFTER=$(stat -f '%m' "$T10_DIR/timeout.conf")
  GOMEMLIMIT_MTIME_AFTER=$(stat -f '%m' "$T10_DIR/gomemlimit.conf")
fi

if [[ "$TIMEOUT_MTIME_BEFORE" == "$TIMEOUT_MTIME_AFTER" ]]; then
  pass "timeout.conf mtime unchanged after second run (mtime: $TIMEOUT_MTIME_BEFORE)"
else
  fail "timeout.conf was rewritten on second run (mtime: $TIMEOUT_MTIME_BEFORE → $TIMEOUT_MTIME_AFTER)"
fi

if [[ "$GOMEMLIMIT_MTIME_BEFORE" == "$GOMEMLIMIT_MTIME_AFTER" ]]; then
  pass "gomemlimit.conf mtime unchanged after second run (mtime: $GOMEMLIMIT_MTIME_BEFORE)"
else
  fail "gomemlimit.conf was rewritten on second run (mtime: $GOMEMLIMIT_MTIME_BEFORE → $GOMEMLIMIT_MTIME_AFTER)"
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
