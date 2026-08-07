#!/usr/bin/env bash
# =============================================================================
#  test-install-node-gomemlimit.sh — Tests for the GOMEMLIMIT drop-in block
#  inside install-node.sh.
#
#  Strategy: the tests extract and execute the *real* sections of
#  install-node.sh (via sed/awk + substitution of file-system seams) rather
#  than hand-copying logic, so any future refactor of the installer that
#  breaks drop-in generation will also break this test.
#
#  Sections exercised from install-node.sh:
#    • GOMEMLIMIT computation block  (lines ~27–38, TOTAL_RAM_KB … GOMEMLIMIT_BYTES=)
#    • Drop-in creation block        (section "10b. GOMEMLIMIT drop-in")
#
#  What is tested:
#    1.  Drop-in file is created at the expected path when the installer block
#        runs against a controlled temp directory.
#    2.  The file contains a [Service] section header.
#    3.  Environment="GOMEMLIMIT=<N>" with a non-zero integer is present.
#    4.  TimeoutStopSec=900 is present.
#    5.  Custom GOMEMLIMIT_BYTES env override is respected (auto path skipped).
#    6.  Auto-computed value is clamped to MIN_GOMEMLIMIT (1.5 GiB) when total
#        RAM is tiny — exercised via a fake /proc/meminfo seam.
#    7.  Auto-computed value is clamped to MAX_GOMEMLIMIT (6500 MiB) when
#        total RAM is huge — same seam.
#    8.  Static analysis: the drop-in creation block still exists in
#        install-node.sh (guards against silent removal in a refactor).
#    9.  Install-path comment in the generated file is correct.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-install-node-gomemlimit.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_SH="$SCRIPT_DIR/install-node.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Ensure install-node.sh exists ─────────────────────────────────────────────
if [[ ! -f "$INSTALL_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  install-node.sh not found at: $INSTALL_SH" >&2
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
# run_real_dropin_block DROPIN_DIR GOMEMLIMIT_BYTES
#
#   Extracts the drop-in creation block directly from install-node.sh using
#   awk (matching the "10b." section comment to the closing ok() call), then:
#     • replaces the hardcoded DROPIN_DIR path with the supplied temp dir, and
#     • injects stub definitions for ok()/info() so the block can run outside
#       the full installer context.
#   systemctl is replaced by a no-op stub via PATH prepending.
#
#   The resulting script fragment is piped to bash -s, ensuring that a future
#   change to the real block (different path, renamed variable, added line)
#   will propagate into this test automatically.
# ---------------------------------------------------------------------------
run_real_dropin_block() {
  local dropin_dir="$1"
  local gomemlimit_bytes="$2"

  # Stub for systemctl (daemon-reload / enable / start called after block)
  local fake_sc_log="$TMPDIR_TEST/sc-dropin.log"
  local fake_sc_dir
  fake_sc_dir=$(make_fake_bin "systemctl" "$fake_sc_log")

  # Extract the real drop-in block from install-node.sh.
  # awk: start capturing at the "10b." comment, stop after the ok() call that
  # follows the EOF heredoc close.
  local block
  block=$(awk '/# ── 10b\. GOMEMLIMIT drop-in/{f=1} f{print} /^ok.*GOMEMLIMIT drop-in/{f=0}' "$INSTALL_SH")

  if [[ -z "$block" ]]; then
    echo "[ERR] Could not extract drop-in block from $INSTALL_SH" >&2
    return 1
  fi

  # Replace the hardcoded DROPIN_DIR assignment with our temp dir so the
  # heredoc is written under a path we can inspect.
  block=$(echo "$block" | sed "s|DROPIN_DIR=.*|DROPIN_DIR=\"${dropin_dir}\"|")

  # Run the block in a subprocess.  We prepend stub ok()/info() functions and
  # export GOMEMLIMIT_BYTES so the heredoc expansion uses the supplied value.
  GOMEMLIMIT_BYTES="$gomemlimit_bytes" \
  PATH="$fake_sc_dir:$PATH" \
  bash -s <<RUNNER
set -euo pipefail
ok()   { echo "[OK]   \$*"; }
info() { echo "[INFO] \$*"; }
GOMEMLIMIT_BYTES="${gomemlimit_bytes}"
${block}
RUNNER
}

# ---------------------------------------------------------------------------
# run_real_clamping_computation TOTAL_RAM_KB
#
#   Extracts the GOMEMLIMIT computation block from install-node.sh and runs
#   it against a synthetic /proc/meminfo-like file, exercising the real
#   MIN/MAX clamping arithmetic from the installer without duplicating it.
#   Prints the resulting GOMEMLIMIT_BYTES value to stdout.
# ---------------------------------------------------------------------------
run_real_clamping_computation() {
  local total_ram_kb="$1"

  # Fake meminfo file
  local fake_meminfo="$TMPDIR_TEST/meminfo-${total_ram_kb}"
  echo "MemTotal:       ${total_ram_kb} kB" > "$fake_meminfo"

  # Extract the computation block: from TOTAL_RAM_KB= to GOMEMLIMIT_BYTES= (inclusive)
  local comp_block
  comp_block=$(awk '/^TOTAL_RAM_KB=/{f=1} f{print} /^GOMEMLIMIT_BYTES=/{f=0; print; exit}' "$INSTALL_SH")

  if [[ -z "$comp_block" ]]; then
    echo "[ERR] Could not extract GOMEMLIMIT computation block from $INSTALL_SH" >&2
    return 1
  fi

  # Redirect /proc/meminfo references to our synthetic file
  comp_block=$(echo "$comp_block" | sed "s|/proc/meminfo|${fake_meminfo}|g")

  # Run the computation and print GOMEMLIMIT_BYTES
  bash -c "${comp_block}; echo \"\$GOMEMLIMIT_BYTES\""
}

# =============================================================================
# Test 1: drop-in file is created at the expected path
# =============================================================================
section "Test 1: drop-in file is created when the real installer block runs"

T1_DIR=$(mktemp -d "$TMPDIR_TEST/t1-XXXXXXXX")
T1_DROPIN="$T1_DIR/gomemlimit.conf"

if run_real_dropin_block "$T1_DIR" "5368709120" >/dev/null 2>&1; then
  :
else
  fail "run_real_dropin_block exited non-zero; aborting remaining tests"
  echo ""
  echo "─────────────────────────────────────────────"
  echo -e "${RED}1 of 1 tests FAILED.${NC}"
  exit 1
fi

if [[ -f "$T1_DROPIN" ]]; then
  pass "gomemlimit.conf was created at $T1_DROPIN"
else
  fail "gomemlimit.conf was NOT created at $T1_DROPIN"
fi

# =============================================================================
# Test 2: [Service] section header is present
# =============================================================================
section "Test 2: [Service] section header is present in the generated drop-in"

if grep -q "^\[Service\]$" "$T1_DROPIN" 2>/dev/null; then
  pass "[Service] section header found"
else
  fail "[Service] section header NOT found (content: $(cat "$T1_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 3: Environment="GOMEMLIMIT=<non-zero integer>" is present
# =============================================================================
section "Test 3: Environment=\"GOMEMLIMIT=<non-zero integer>\" is present"

GOMEMLIMIT_LINE=$(grep -E '^Environment="GOMEMLIMIT=[0-9]+"$' "$T1_DROPIN" 2>/dev/null || true)
if [[ -n "$GOMEMLIMIT_LINE" ]]; then
  pass "GOMEMLIMIT environment line found: $GOMEMLIMIT_LINE"
else
  fail "GOMEMLIMIT environment line NOT found (content: $(cat "$T1_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

GOMEMLIMIT_VALUE=$(echo "$GOMEMLIMIT_LINE" | grep -oE '[0-9]+' || true)
if [[ -n "$GOMEMLIMIT_VALUE" && "$GOMEMLIMIT_VALUE" -gt 0 ]]; then
  pass "GOMEMLIMIT value is a positive integer: $GOMEMLIMIT_VALUE"
else
  fail "GOMEMLIMIT value is zero or missing (got: '$GOMEMLIMIT_VALUE')"
fi

# =============================================================================
# Test 4: TimeoutStopSec is NOT in gomemlimit.conf (moved to timeout.conf)
# =============================================================================
section "Test 4: TimeoutStopSec is NOT in gomemlimit.conf (lives in timeout.conf now)"

if grep -q "^TimeoutStopSec=" "$T1_DROPIN" 2>/dev/null; then
  fail "TimeoutStopSec found in gomemlimit.conf — it should live in timeout.conf only"
else
  pass "TimeoutStopSec is absent from gomemlimit.conf (correct — belongs in timeout.conf)"
fi

# =============================================================================
# Test 5: custom GOMEMLIMIT_BYTES override is respected
# =============================================================================
section "Test 5: custom GOMEMLIMIT_BYTES override is written verbatim to the drop-in"

CUSTOM_LIMIT=3221225472   # 3 GiB
T5_DIR=$(mktemp -d "$TMPDIR_TEST/t5-XXXXXXXX")
T5_DROPIN="$T5_DIR/gomemlimit.conf"
run_real_dropin_block "$T5_DIR" "$CUSTOM_LIMIT" >/dev/null 2>&1

if grep -q "^Environment=\"GOMEMLIMIT=${CUSTOM_LIMIT}\"$" "$T5_DROPIN" 2>/dev/null; then
  pass "custom GOMEMLIMIT_BYTES=${CUSTOM_LIMIT} is written to the drop-in"
else
  fail "custom GOMEMLIMIT_BYTES=${CUSTOM_LIMIT} NOT found in drop-in (content: $(cat "$T5_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 6: real computation block clamps to MIN (1.5 GiB) for tiny RAM
# =============================================================================
section "Test 6: real installer computation clamps to MIN_GOMEMLIMIT (1.5 GiB) for tiny RAM"

TINY_RAM_KB=$(( 1024 * 1024 ))   # 1 GiB in KB → 75% = 805 MiB < 1.5 GiB floor
MIN_EXPECTED=$(( 1536 * 1024 * 1024 ))

CLAMPED=$(run_real_clamping_computation "$TINY_RAM_KB" 2>/dev/null)

if [[ "$CLAMPED" -eq "$MIN_EXPECTED" ]]; then
  pass "installer computation clamped to MIN=${MIN_EXPECTED} for 1 GiB host (got: $CLAMPED)"
else
  fail "expected MIN clamp=${MIN_EXPECTED}, got '$CLAMPED'"
fi

T6_DIR=$(mktemp -d "$TMPDIR_TEST/t6-XXXXXXXX")
T6_DROPIN="$T6_DIR/gomemlimit.conf"
run_real_dropin_block "$T6_DIR" "$CLAMPED" >/dev/null 2>&1

if grep -q "^Environment=\"GOMEMLIMIT=${MIN_EXPECTED}\"$" "$T6_DROPIN" 2>/dev/null; then
  pass "MIN-clamped value ${MIN_EXPECTED} written to drop-in"
else
  fail "MIN-clamped value ${MIN_EXPECTED} NOT found in drop-in"
fi

# =============================================================================
# Test 7: real computation block clamps to MAX (6500 MiB) for large RAM
# =============================================================================
section "Test 7: real installer computation clamps to MAX_GOMEMLIMIT (6500 MiB) for large RAM"

LARGE_RAM_KB=$(( 64 * 1024 * 1024 ))   # 64 GiB → 75% = 48 GiB >> 6500 MiB
MAX_EXPECTED=6979321856

CLAMPED_MAX=$(run_real_clamping_computation "$LARGE_RAM_KB" 2>/dev/null)

if [[ "$CLAMPED_MAX" -eq "$MAX_EXPECTED" ]]; then
  pass "installer computation clamped to MAX=${MAX_EXPECTED} for 64 GiB host (got: $CLAMPED_MAX)"  # 6500 MiB
else
  fail "expected MAX clamp=${MAX_EXPECTED}, got '$CLAMPED_MAX'"
fi

T7_DIR=$(mktemp -d "$TMPDIR_TEST/t7-XXXXXXXX")
T7_DROPIN="$T7_DIR/gomemlimit.conf"
run_real_dropin_block "$T7_DIR" "$CLAMPED_MAX" >/dev/null 2>&1

if grep -q "^Environment=\"GOMEMLIMIT=${MAX_EXPECTED}\"$" "$T7_DROPIN" 2>/dev/null; then
  pass "MAX-clamped value ${MAX_EXPECTED} written to drop-in"  # 6500 MiB
else
  fail "MAX-clamped value ${MAX_EXPECTED} NOT found in drop-in"
fi

# =============================================================================
# Test 8: static analysis — drop-in creation block still exists in install-node.sh
# =============================================================================
section "Test 8: static analysis — drop-in blocks still present in install-node.sh"

if awk '/# ── 10b\. GOMEMLIMIT drop-in/{f=1} f{print} /^ok.*GOMEMLIMIT drop-in/{f=0}' "$INSTALL_SH" | \
    grep -q 'GOMEMLIMIT'; then
  pass "GOMEMLIMIT drop-in block extractable from install-node.sh"
else
  fail "GOMEMLIMIT drop-in block NOT extractable — it may have been renamed or removed"
fi

if grep -q 'Environment="GOMEMLIMIT=' "$INSTALL_SH"; then
  pass "Environment=\"GOMEMLIMIT=...\" line found in install-node.sh"
else
  fail "Environment=\"GOMEMLIMIT=...\" line NOT found in install-node.sh"
fi

if grep -q 'TimeoutStopSec=900' "$INSTALL_SH"; then
  pass "TimeoutStopSec=900 found in install-node.sh"
else
  fail "TimeoutStopSec=900 NOT found in install-node.sh"
fi

if grep -q 'aperod-node.service.d' "$INSTALL_SH"; then
  pass "aperod-node.service.d directory referenced in install-node.sh"
else
  fail "aperod-node.service.d NOT referenced in install-node.sh"
fi

# Static check: 10c block exists
if awk '/# ── 10c\. TimeoutStopSec drop-in/{f=1} f{print} /^ok.*[Tt]imeout/{f=0}' "$INSTALL_SH" | \
    grep -q 'timeout.conf'; then
  pass "timeout.conf drop-in block (10c) extractable from install-node.sh"
else
  fail "timeout.conf drop-in block (10c) NOT extractable — it may have been renamed or removed"
fi

# =============================================================================
# Test 9: install path comment in generated file matches real service drop-in path
# =============================================================================
section "Test 9: install-path comment in generated drop-in is correct"

if grep -q "/etc/systemd/system/aperod-node.service.d/gomemlimit.conf" "$T1_DROPIN" 2>/dev/null; then
  pass "gomemlimit.conf drop-in contains the correct install-path comment"
else
  fail "install-path comment NOT found in gomemlimit.conf drop-in (content: $(cat "$T1_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 10: timeout.conf drop-in is created with correct content
# =============================================================================
section "Test 10: timeout.conf drop-in is created by installer 10c block"

# ---------------------------------------------------------------------------
# run_real_timeout_block DROPIN_DIR
#
#   Extracts the timeout.conf creation block (10c) from install-node.sh and
#   runs it against a controlled temp dir.
# ---------------------------------------------------------------------------
run_real_timeout_block() {
  local dropin_dir="$1"

  local fake_sc_log="$TMPDIR_TEST/sc-timeout.log"
  local fake_sc_dir
  fake_sc_dir=$(make_fake_bin "systemctl" "$fake_sc_log")

  local block
  block=$(awk '/# ── 10c\. TimeoutStopSec drop-in/{f=1} f{print} /^ok.*[Tt]imeout/{f=0}' "$INSTALL_SH")

  if [[ -z "$block" ]]; then
    echo "[ERR] Could not extract timeout.conf block from $INSTALL_SH" >&2
    return 1
  fi

  # The 10c block uses DROPIN_DIR set by the 10b block; inject it directly
  # since we're running the block in isolation.
  PATH="$fake_sc_dir:$PATH" \
  DROPIN_DIR="$dropin_dir" \
  bash -s <<RUNNER
set -euo pipefail
ok()   { echo "[OK]   \$*"; }
info() { echo "[INFO] \$*"; }
DROPIN_DIR="${dropin_dir}"
${block}
RUNNER
}

T10_DIR=$(mktemp -d "$TMPDIR_TEST/t10-XXXXXXXX")
T10_DROPIN="$T10_DIR/timeout.conf"

if run_real_timeout_block "$T10_DIR" >/dev/null 2>&1; then
  :
else
  fail "run_real_timeout_block exited non-zero"
fi

if [[ -f "$T10_DROPIN" ]]; then
  pass "timeout.conf was created at $T10_DROPIN"
else
  fail "timeout.conf was NOT created at $T10_DROPIN"
fi

if grep -q "^\[Service\]$" "$T10_DROPIN" 2>/dev/null; then
  pass "[Service] section header found in timeout.conf"
else
  fail "[Service] section header NOT found in timeout.conf (content: $(cat "$T10_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^TimeoutStopSec=900$" "$T10_DROPIN" 2>/dev/null; then
  pass "TimeoutStopSec=900 found in timeout.conf"
else
  fail "TimeoutStopSec=900 NOT found in timeout.conf (content: $(cat "$T10_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "/etc/systemd/system/aperod-node.service.d/timeout.conf" "$T10_DROPIN" 2>/dev/null; then
  pass "timeout.conf contains the correct install-path comment"
else
  fail "install-path comment NOT found in timeout.conf (content: $(cat "$T10_DROPIN" 2>/dev/null || echo '<missing>'))"
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
