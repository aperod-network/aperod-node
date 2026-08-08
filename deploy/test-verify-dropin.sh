#!/usr/bin/env bash
# test-verify-dropin.sh — Automated tests for verify-dropin.sh
#
# Mocks the `ssh` binary via a PATH-prepended shim so no real SSH connection
# is needed.  Each case exercises a distinct failure mode of verify-dropin.sh.
#
# Cases
# ─────
#   1. Correct values              → exit 0
#   2. GOMEMLIMIT missing          → exit 1
#   3. GOMEMLIMIT wrong value      → exit 1
#   4. TimeoutStopUSec wrong value → exit 1
#   5. Drop-in files missing       → exit 1
#
# Exit codes
# ──────────
#   0  all cases passed
#   1  one or more cases failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERIFY_SCRIPT="$SCRIPT_DIR/verify-dropin.sh"

PASS=0
FAIL=0

# ── Helpers ───────────────────────────────────────────────────────────────────

# run_case NAME EXPECTED_EXIT SYSTEMCTL_OUTPUT DROPIN_EXISTS
#   SYSTEMCTL_OUTPUT  — text returned when remote cmd contains "systemctl show"
#   DROPIN_EXISTS     — "yes" or "no" returned for every drop-in file test
run_case() {
    local name="$1"
    local expected_exit="$2"
    local systemctl_output="$3"
    local dropin_exists="${4:-yes}"

    local tmpdir
    tmpdir="$(mktemp -d)"

    # Store mocked values in files (avoids quoting issues inside heredocs)
    printf '%s\n' "$systemctl_output" > "$tmpdir/systemctl_output.txt"
    printf '%s\n' "$dropin_exists"    > "$tmpdir/dropin_exists.txt"

    # Write the ssh shim.  It dispatches on what the remote command contains.
    cat > "$tmpdir/ssh" << 'SHIM_EOF'
#!/usr/bin/env bash
# Fake ssh: ignore host arg, dispatch on remote command text.
MYDIR="$(cd "$(dirname "$0")" && pwd)"
shift             # drop "root@<IP>"
REMOTE_CMD="$*"

if echo "$REMOTE_CMD" | grep -q "systemctl show"; then
    cat "$MYDIR/systemctl_output.txt"
elif echo "$REMOTE_CMD" | grep -q "test -f"; then
    cat "$MYDIR/dropin_exists.txt"
else
    # Unknown command — return empty (safe default)
    echo ""
fi
exit 0
SHIM_EOF
    chmod +x "$tmpdir/ssh"

    local actual_exit=0
    PATH="$tmpdir:$PATH" bash "$VERIFY_SCRIPT" "127.0.0.1" > /dev/null 2>&1 \
        || actual_exit=$?

    rm -rf "$tmpdir"

    echo "--- $name"
    if [[ "$actual_exit" -eq "$expected_exit" ]]; then
        echo "PASS: $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL: $name — expected exit $expected_exit, got $actual_exit"
        FAIL=$((FAIL + 1))
    fi
    echo ""
}

# ── Shared systemctl outputs ──────────────────────────────────────────────────

# Correct: GOMEMLIMIT=5368709120, TimeoutStopUSec=15min (=900s)
CORRECT_OUTPUT="Environment=GOMEMLIMIT=5368709120
TimeoutStopUSec=15min"

# ── Test cases ────────────────────────────────────────────────────────────────

echo "=== verify-dropin.sh tests ==="
echo ""

# Case 1: everything correct — both drop-ins present and values match
run_case "CorrectValues_ExitsZero" 0 "$CORRECT_OUTPUT" "yes"

# Case 2: GOMEMLIMIT line absent — drop-in was never applied
MISSING_GOMEMLIMIT="TimeoutStopUSec=15min"
run_case "MissingGOMEMLIMIT_ExitsOne" 1 "$MISSING_GOMEMLIMIT" "yes"

# Case 3: GOMEMLIMIT present but has wrong byte value
WRONG_GOMEMLIMIT="Environment=GOMEMLIMIT=1073741824
TimeoutStopUSec=15min"
run_case "WrongGOMEMLIMIT_ExitsOne" 1 "$WRONG_GOMEMLIMIT" "yes"

# Case 4: TimeoutStopUSec present but encodes a different duration (5 min = 300s)
WRONG_TIMEOUT="Environment=GOMEMLIMIT=5368709120
TimeoutStopUSec=5min"
run_case "WrongTimeoutStopSec_ExitsOne" 1 "$WRONG_TIMEOUT" "yes"

# Case 5: drop-in files are missing on the remote host (even if env is correct)
run_case "MissingDropinFiles_ExitsOne" 1 "$CORRECT_OUTPUT" "no"

# ── Summary ───────────────────────────────────────────────────────────────────

echo "=== Results: PASS=$PASS  FAIL=$FAIL ==="
echo ""

if [[ "$FAIL" -gt 0 ]]; then
    echo "FAIL — $FAIL test(s) failed"
    exit 1
fi

echo "OK"
exit 0
