#!/usr/bin/env bash
# =============================================================================
#  test-update-node-gomemlimit-guard.sh — Tests for the Step 0d preflight
#  guard in update-node.sh.
#
#  Strategy: extract and run the *real* Step 0d block from update-node.sh
#  (via awk + sed seam injection) rather than hand-copying logic, so any
#  future refactor that silently removes or breaks the preflight will also
#  break this test.
#
#  What is tested:
#    T1. Absent canonical gomemlimit.conf → guard exits non-zero,
#        prints "NOT stopped" to stderr, does NOT invoke systemctl.
#    T2. Corrupt canonical file (no GOMEMLIMIT=<digits> sequence) → guard
#        exits non-zero, prints "could not parse GOMEMLIMIT" and "NOT stopped"
#        to stderr, does NOT invoke systemctl.
#    T3. Valid canonical file → guard exits 0, writes correct
#        [Service] / Environment="GOMEMLIMIT=<N>" drop-in, and calls
#        systemctl daemon-reload exactly once.
#    T4. Static analysis: the Step 0d block still exists in update-node.sh
#        and contains the expected sentinel strings (GOMEMLIMIT_CANONICAL,
#        "NOT stopped", "could not parse GOMEMLIMIT", exit 1).
#
#  Seams injected by this test (all overrideable via environment or sed):
#    GOMEMLIMIT_CANONICAL  — path to the canonical gomemlimit.conf source
#    GOMEMLIMIT_CONF       — path where the drop-in is written
#    SYSTEMCTL             — command used for daemon-reload
#
#  Run from anywhere:
#    bash blockchain/deploy/test-update-node-gomemlimit-guard.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_SH="$SCRIPT_DIR/update-node.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0; FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; PASS=$((PASS+1)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; FAIL=$((FAIL+1)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Ensure update-node.sh exists ──────────────────────────────────────────────
if [[ ! -f "$UPDATE_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  update-node.sh not found at: $UPDATE_SH" >&2
  exit 1
fi

# ── Shared temp directory (cleaned on exit) ───────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# make_fake_bin CMD LOG_FILE
#   Creates a stub executable for CMD in a fresh temp dir; every invocation
#   appends "CMD <args>" to LOG_FILE.  Prints the bin-dir path to stdout.
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
# run_step0d_block CANONICAL_PATH DROPIN_DIR SC_LOG
#
#   Extracts the real Step 0d block from update-node.sh using awk (from the
#   GOMEMLIMIT_CANONICAL= line up to, but not including, "# Step 1:"), then
#   injects three seams via sed:
#     • GOMEMLIMIT_CANONICAL  → CANONICAL_PATH (may or may not exist)
#     • GOMEMLIMIT_CONF       → DROPIN_DIR/gomemlimit.conf
#     • SYSTEMCTL             → path to a fake systemctl stub
#   A send_telegram_alert stub is prepended so no real HTTP call is made.
#
#   The modified block is run in a bash subprocess whose stdin is a heredoc.
#   stdout and stderr are not redirected here — callers redirect them.
#   Return code is the block's exit code.
# ---------------------------------------------------------------------------
run_step0d_block() {
  local canonical_path="$1"
  local dropin_dir="$2"
  local sc_log="$3"

  local fake_sc_dir
  fake_sc_dir=$(make_fake_bin "systemctl" "$sc_log")

  # Extract step 0d: from GOMEMLIMIT_CANONICAL= up to (not including) Step 1.
  local block
  block=$(awk '
    /^GOMEMLIMIT_CANONICAL=/ { f=1 }
    f && /^# Step 1:/        { exit }
    f                        { print }
  ' "$UPDATE_SH")

  if [[ -z "$block" ]]; then
    echo "[ERR] Could not extract step 0d block from $UPDATE_SH" >&2
    return 1
  fi

  # Inject seams: replace the three assignment lines with controlled values.
  block=$(echo "$block" \
    | sed "s|GOMEMLIMIT_CANONICAL=.*|GOMEMLIMIT_CANONICAL=\"${canonical_path}\"|" \
    | sed "s|GOMEMLIMIT_CONF=.*|GOMEMLIMIT_CONF=\"${dropin_dir}/gomemlimit.conf\"|" \
    | sed "s|SYSTEMCTL=.*|SYSTEMCTL=\"${fake_sc_dir}/systemctl\"|")

  # Run the modified block.  The outer bash expands ${block} once into the
  # heredoc; the inner bash then executes the resulting code, so variables
  # set INSIDE the block (e.g. GOMEMLIMIT_CANONICAL) are available to later
  # lines in the same block — exactly as they are in the real update-node.sh.
  PATH="$fake_sc_dir:$PATH" \
  bash -s <<RUNNER
set -uo pipefail
send_telegram_alert() { :; }
${block}
RUNNER
}

# =============================================================================
# T1: absent canonical gomemlimit.conf → exit non-zero, service NOT stopped
# =============================================================================
section "T1: absent canonical gomemlimit.conf → guard aborts before stopping the service"

T1_DROPIN_DIR=$(mktemp -d "$TMPDIR_TEST/t1-XXXXXXXX")
T1_SC_LOG="$TMPDIR_TEST/t1-sc.log"
T1_ERR="$TMPDIR_TEST/t1.err"
T1_RC=0

# The canonical file does NOT exist.
T1_CANONICAL="$TMPDIR_TEST/nonexistent-gomemlimit.conf"

run_step0d_block "$T1_CANONICAL" "$T1_DROPIN_DIR" "$T1_SC_LOG" \
  >"$TMPDIR_TEST/t1.out" 2>"$T1_ERR" || T1_RC=$?

if [[ $T1_RC -ne 0 ]]; then
  pass "T1: guard exits non-zero when canonical file is absent (rc=$T1_RC)"
else
  fail "T1: guard exited 0 — expected non-zero (absent-file path should abort)"
fi

if grep -q "NOT stopped" "$T1_ERR" 2>/dev/null; then
  pass "T1: stderr contains 'NOT stopped' — service was not touched"
else
  fail "T1: 'NOT stopped' not found on stderr (stderr: $(head -5 "$T1_ERR" 2>/dev/null || echo '<empty>'))"
fi

# systemctl must NOT have been invoked (service was never stopped)
if [[ ! -s "$T1_SC_LOG" ]]; then
  pass "T1: systemctl was not called — service was not stopped"
else
  fail "T1: systemctl was called unexpectedly (log: $(cat "$T1_SC_LOG"))"
fi

# Drop-in must NOT have been written (guard aborted before write)
if [[ ! -f "$T1_DROPIN_DIR/gomemlimit.conf" ]]; then
  pass "T1: drop-in was not written — guard aborted cleanly before write"
else
  fail "T1: drop-in was written unexpectedly — guard should have aborted before reaching the write step"
fi

# =============================================================================
# T2: corrupt canonical file (no GOMEMLIMIT=<digits>) → exit non-zero, parse error
# =============================================================================
section "T2: corrupt canonical file (no GOMEMLIMIT=<digits>) → guard aborts with parse error"

T2_DROPIN_DIR=$(mktemp -d "$TMPDIR_TEST/t2-XXXXXXXX")
T2_SC_LOG="$TMPDIR_TEST/t2-sc.log"
T2_ERR="$TMPDIR_TEST/t2.err"
T2_RC=0

# Create a canonical file that exists but contains no GOMEMLIMIT=<digits> line.
T2_CANONICAL="$TMPDIR_TEST/corrupt-gomemlimit.conf"
cat > "$T2_CANONICAL" <<'EOF'
[Service]
# GOMEMLIMIT intentionally omitted or non-numeric below
Environment="GOMEMLIMIT=not-a-number"
EOF

run_step0d_block "$T2_CANONICAL" "$T2_DROPIN_DIR" "$T2_SC_LOG" \
  >"$TMPDIR_TEST/t2.out" 2>"$T2_ERR" || T2_RC=$?

if [[ $T2_RC -ne 0 ]]; then
  pass "T2: guard exits non-zero when GOMEMLIMIT value cannot be parsed (rc=$T2_RC)"
else
  fail "T2: guard exited 0 — expected non-zero (corrupt file should abort)"
fi

if grep -q "could not parse GOMEMLIMIT" "$T2_ERR" 2>/dev/null; then
  pass "T2: stderr contains 'could not parse GOMEMLIMIT'"
else
  fail "T2: 'could not parse GOMEMLIMIT' not found on stderr (stderr: $(head -5 "$T2_ERR" 2>/dev/null || echo '<empty>'))"
fi

if grep -q "NOT stopped" "$T2_ERR" 2>/dev/null; then
  pass "T2: stderr confirms service was NOT stopped (corrupt-file path)"
else
  fail "T2: 'NOT stopped' not found on stderr for corrupt-file case"
fi

if [[ ! -s "$T2_SC_LOG" ]]; then
  pass "T2: systemctl was not called — service was not stopped"
else
  fail "T2: systemctl was called unexpectedly on corrupt-file path (log: $(cat "$T2_SC_LOG"))"
fi

# =============================================================================
# T3: valid canonical file → exit 0, correct drop-in written, daemon-reload called
# =============================================================================
section "T3: valid canonical file → guard exits 0, drop-in written, daemon-reload called"

T3_DROPIN_DIR=$(mktemp -d "$TMPDIR_TEST/t3-XXXXXXXX")
T3_SC_LOG="$TMPDIR_TEST/t3-sc.log"
T3_ERR="$TMPDIR_TEST/t3.err"
T3_RC=0

# Valid canonical file using the production GOMEMLIMIT value.
T3_CANONICAL="$TMPDIR_TEST/valid-gomemlimit.conf"
EXPECTED_VALUE=5905580032
printf '[Service]\nEnvironment="GOMEMLIMIT=%s"\n' "$EXPECTED_VALUE" > "$T3_CANONICAL"

run_step0d_block "$T3_CANONICAL" "$T3_DROPIN_DIR" "$T3_SC_LOG" \
  >"$TMPDIR_TEST/t3.out" 2>"$T3_ERR" || T3_RC=$?

if [[ $T3_RC -eq 0 ]]; then
  pass "T3: guard exits 0 with a valid canonical file"
else
  fail "T3: guard exited non-zero (rc=$T3_RC); stderr: $(head -5 "$T3_ERR" 2>/dev/null || echo '<empty>')"
fi

T3_DROPIN_FILE="$T3_DROPIN_DIR/gomemlimit.conf"
if [[ -f "$T3_DROPIN_FILE" ]]; then
  pass "T3: drop-in gomemlimit.conf was created"
else
  fail "T3: drop-in gomemlimit.conf was NOT created"
fi

if grep -q "^\[Service\]$" "$T3_DROPIN_FILE" 2>/dev/null; then
  pass "T3: [Service] section header present in drop-in"
else
  fail "T3: [Service] header missing from drop-in (content: $(cat "$T3_DROPIN_FILE" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^Environment=\"GOMEMLIMIT=${EXPECTED_VALUE}\"$" "$T3_DROPIN_FILE" 2>/dev/null; then
  pass "T3: Environment=\"GOMEMLIMIT=${EXPECTED_VALUE}\" written correctly to drop-in"
else
  fail "T3: Environment line missing or wrong (content: $(cat "$T3_DROPIN_FILE" 2>/dev/null || echo '<missing>'))"
fi

RELOAD_COUNT=$(grep -c "daemon-reload" "$T3_SC_LOG" 2>/dev/null || echo 0)
if [[ "$RELOAD_COUNT" -eq 1 ]]; then
  pass "T3: systemctl daemon-reload was called exactly once after writing the drop-in"
else
  fail "T3: expected exactly 1 daemon-reload call, got ${RELOAD_COUNT} (sc_log: $(cat "$T3_SC_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# T4: static analysis — Step 0d block is still present and complete
# =============================================================================

section "T4: static analysis — Step 0d guard block still present in update-node.sh"

EXTRACTED=$(awk '
  /^GOMEMLIMIT_CANONICAL=/ { f=1 }
  f && /^# Step 1:/        { exit }
  f                        { print }
' "$UPDATE_SH")

if [[ -n "$EXTRACTED" ]]; then
  pass "T4: Step 0d block is extractable from update-node.sh"
else
  fail "T4: Step 0d block NOT extractable — it may have been renamed or removed (check GOMEMLIMIT_CANONICAL= / Step 1: anchors)"
fi

if echo "$EXTRACTED" | grep -q 'GOMEMLIMIT_CANONICAL'; then
  pass "T4: GOMEMLIMIT_CANONICAL variable is referenced in the extracted block"
else
  fail "T4: GOMEMLIMIT_CANONICAL NOT found in extracted block"
fi

if echo "$EXTRACTED" | grep -q 'NOT stopped'; then
  pass "T4: 'NOT stopped' safety message present in Step 0d block"
else
  fail "T4: 'NOT stopped' NOT found in extracted Step 0d block"
fi

if echo "$EXTRACTED" | grep -q 'could not parse GOMEMLIMIT'; then
  pass "T4: 'could not parse GOMEMLIMIT' parse-error message present"
else
  fail "T4: 'could not parse GOMEMLIMIT' NOT found in extracted block"
fi

if echo "$EXTRACTED" | grep -q 'exit 1'; then
  pass "T4: 'exit 1' abort path present in Step 0d block"
else
  fail "T4: 'exit 1' NOT found in extracted block — guard has no abort path"
fi

# =============================================================================
# T5: static ordering — Step 0d preflight precedes 'systemctl stop' in update-node.sh
# =============================================================================
section "T5: static ordering — Step 0d preflight precedes 'systemctl stop' in update-node.sh"

# If Step 0d were ever moved below the stop command a corrupt or absent
# gomemlimit.conf would only be discovered after the node is already dead.
STEP0D_LINE=$(grep -n "^GOMEMLIMIT_CANONICAL=" "$UPDATE_SH" | head -1 | cut -d: -f1)
# Match only non-comment lines (exclude lines starting with #) so script-header
# comments like "# call systemctl stop" do not give a false early line number.
STOP_LINE=$(grep -n "^[^#]*systemctl stop" "$UPDATE_SH" | head -1 | cut -d: -f1)

if [[ -n "$STEP0D_LINE" && -n "$STOP_LINE" ]]; then
  if [[ "$STEP0D_LINE" -lt "$STOP_LINE" ]]; then
    pass "T5: Step 0d preflight (line $STEP0D_LINE) precedes 'systemctl stop' (line $STOP_LINE) — guard fires before the service is ever stopped"
  else
    fail "T5: 'systemctl stop' (line $STOP_LINE) appears BEFORE Step 0d preflight (line $STEP0D_LINE) — a corrupt gomemlimit.conf would be discovered only after the node is already dead"
  fi
else
  fail "T5: could not locate ordering anchors — GOMEMLIMIT_CANONICAL= found on line '${STEP0D_LINE:-<missing>}', systemctl stop on '${STOP_LINE:-<missing>}'"
fi

# =============================================================================
# T6: self-check — detecting a script where 'systemctl stop' precedes Step 0d
# =============================================================================
section "T6: self-check — detecting a script where 'systemctl stop' precedes Step 0d"

FAKE_UPDATE="$TMPDIR_TEST/fake-update-node.sh"
cat > "$FAKE_UPDATE" <<'FAKE'
#!/usr/bin/env bash
# fake update-node.sh: systemctl stop is placed BEFORE the Step 0d guard (wrong order)
systemctl stop aperod-node || true

GOMEMLIMIT_CANONICAL="/opt/aperod/deploy/gomemlimit.conf"

# Step 1: Pull latest source
FAKE

FAKE_STEP0D=$(grep -n "^GOMEMLIMIT_CANONICAL=" "$FAKE_UPDATE" | head -1 | cut -d: -f1)
FAKE_STOP=$(grep -n "systemctl stop" "$FAKE_UPDATE" | head -1 | cut -d: -f1)

WRONG_ORDER_DETECTED=false
if [[ -n "$FAKE_STEP0D" && -n "$FAKE_STOP" && "$FAKE_STEP0D" -gt "$FAKE_STOP" ]]; then
  WRONG_ORDER_DETECTED=true
fi

if $WRONG_ORDER_DETECTED; then
  pass "T6: self-check — wrong-order script (stop before guard) is correctly identified as a regression"
else
  fail "T6: self-check — wrong-order script was NOT detected; the T5 ordering check may have a bug"
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
