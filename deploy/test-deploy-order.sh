#!/usr/bin/env bash
# test-deploy-order.sh — CI entry-point for deployment-order and atomic-swap tests.
#
# Runs from the blockchain/ root (the deploy-order-tests workflow uses
# `bash deploy/test-deploy-order.sh` with blockchain/ as the working dir).
#
# Tests included
# ──────────────
# TestAtomicBinarySwap  — verifies that running `make build-node` while a
#   process backed by build/aperod-node is alive neither crashes the running
#   process nor produces a corrupted binary (ELF magic check).
#
# Exit codes
# ──────────
#   0  all tests passed (skipped tests are not failures)
#   1  one or more tests failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BLOCKCHAIN_DIR="$(dirname "$SCRIPT_DIR")"

cd "$BLOCKCHAIN_DIR"

echo "=== deploy-order tests ($(date -u '+%Y-%m-%dT%H:%M:%SZ')) ==="
echo "  working dir : $BLOCKCHAIN_DIR"
echo "  Go version  : $(go version 2>/dev/null || echo 'go not found')"
echo ""

PASS=0
FAIL=0

run_test() {
    local name="$1"
    local run_flag="$2"
    local tmpout
    tmpout="$(mktemp)"

    echo "--- $name"
    if go test ./deploy/... -run "$run_flag" -v -timeout 180s 2>&1 | tee "$tmpout"; then
        # go test exits 0 for both PASS and SKIP.
        # Detect a skip by looking for the canonical "--- SKIP:" marker in output.
        if grep -qE '^--- SKIP:' "$tmpout" 2>/dev/null; then
            echo "SKIP: $name"
            # A skipped test is not a failure; do not increment FAIL.
        else
            echo "PASS: $name"
            PASS=$((PASS + 1))
        fi
    else
        EXIT=$?
        echo "FAIL: $name (exit $EXIT)"
        FAIL=$((FAIL + 1))
    fi
    rm -f "$tmpout"
    echo ""
}

# ── TestAtomicBinarySwap ──────────────────────────────────────────────────────
# Starts a stub process from build/aperod-node, runs make build-node
# concurrently, and asserts the process survived and the new binary is valid ELF.
run_test "TestAtomicBinarySwap" "TestAtomicBinarySwap"

# ── Summary ──────────────────────────────────────────────────────────────────
echo "=== Results: PASS=$PASS  FAIL=$FAIL ==="

if [ "$FAIL" -gt 0 ]; then
    echo "FAIL — $FAIL test(s) failed"
    exit 1
fi

echo "OK"
exit 0
