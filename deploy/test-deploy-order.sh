#!/usr/bin/env bash
# test-deploy-order.sh — CI entry-point for deployment-order and atomic-swap tests.
#
# Runs from the blockchain/ root (the deploy-order-tests workflow uses
# `bash deploy/test-deploy-order.sh` with blockchain/ as the working dir).
#
# Tests included
# ──────────────
# TestAtomicBinarySwap   — verifies that running `make build-node` while a
#   process backed by build/aperod-node is alive neither crashes the running
#   process nor produces a corrupted binary (ELF magic check).
#
# TestNodeBinaryIsStatic — verifies that `make build-node` produces a fully
#   static binary (no PT_INTERP ELF segment, ldd reports "not a dynamic
#   executable") so it runs on Debian 11 / Ubuntu 20.04 (GLIBC 2.31) without
#   "GLIBC_X.YY not found" errors.
#
# Exit codes
# ──────────
#   0  all tests passed with no skips
#   1  one or more tests failed
#  77  one or more tests were skipped

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
SKIPPED=()

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
            SKIPPED+=("$name")
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

# ── TestNodeBinaryIsStatic ────────────────────────────────────────────────────
# Builds aperod-node via `make build-node` and verifies the resulting ELF has
# no PT_INTERP segment (fully static) and that ldd reports "not a dynamic
# executable".  Guards against regressions where CGO_ENABLED=0 is accidentally
# removed from the Makefile, which would break Debian 11 / Ubuntu 20.04 nodes.
run_test "TestNodeBinaryIsStatic" "TestNodeBinaryIsStatic"

# ── TestAtomicBinarySwap ──────────────────────────────────────────────────────
# Starts a stub process from build/aperod-node, runs make build-node
# concurrently, and asserts the process survived and the new binary is valid ELF.
run_test "TestAtomicBinarySwap" "TestAtomicBinarySwap"

# ── update-api.sh backup-script sync (shell e2e) ──────────────────────────────
# Verifies that the _sync_backup_script helper sourced by update-api.sh (Step 1b)
# correctly replaces a stale installed copy and that sha256sum matches the repo
# copy afterwards.  No Docker required — exercises the shell function directly.
echo "--- UpdateApiBackupScriptSync"
if bash "$SCRIPT_DIR/test-update-api-e2e.sh"; then
    echo "PASS: UpdateApiBackupScriptSync"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: UpdateApiBackupScriptSync (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── join-network.sh push-mode trap (shell e2e) ───────────────────────────────
# Verifies the _push_cleanup trap in push-mode (join-network.sh <TARGET_IP>):
#  1. Interrupted between Step 1 (target stopped) and Step 2 (source not yet
#     stopped) → trap restarts target, does NOT restart source.
#  2. Rsync interrupted mid-transfer → trap restarts source, does NOT restart
#     target (data may be partial; sentinel remains).
#  3. SSH disconnect during target-stop → trap restarts target, NOT source.
#  4. Successful run → trap cleared before exit, ssh target-restart not called.
echo "--- JoinNetworkPushTrapTests"
if bash "$SCRIPT_DIR/test-join-network-push-trap.sh"; then
    echo "PASS: JoinNetworkPushTrapTests"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: JoinNetworkPushTrapTests (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── join-network.sh identity isolation (shell e2e) ───────────────────────────
# Verifies that join-network.sh never copies the source node's p2p_identity.key
# to the target (rsync --exclude) and deletes any pre-existing key via SSH rm -f
# so the target generates a fresh P2P fingerprint on first start.
echo "--- JoinNetworkIdentityIsolation"
if bash "$SCRIPT_DIR/test-join-network-identity.sh"; then
    echo "PASS: JoinNetworkIdentityIsolation"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: JoinNetworkIdentityIsolation (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── verify-dropin.sh self-tests (shell e2e) ───────────────────────────────────
# Mocks ssh via a PATH-prepended shim and exercises four failure modes:
# missing GOMEMLIMIT, wrong GOMEMLIMIT, wrong TimeoutStopSec, missing drop-in.
echo "--- VerifyDropinTests"
if bash "$SCRIPT_DIR/test-verify-dropin.sh"; then
    echo "PASS: VerifyDropinTests"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: VerifyDropinTests (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── join-network.sh dropin gate integration (shell e2e) ───────────────────────
# Stubs ssh, rsync, and systemctl so the full push-mode flow of join-network.sh
# runs without real SSH targets.  Confirms that a verify-dropin.sh failure
# (wrong/missing GOMEMLIMIT or absent drop-in files) causes join-network.sh to
# abort with a non-zero exit code, and that passing values let the join succeed.
echo "--- JoinNetworkDropinGateTests"
if bash "$SCRIPT_DIR/test-join-network-dropin.sh"; then
    echo "PASS: JoinNetworkDropinGateTests"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: JoinNetworkDropinGateTests (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── update-node.sh stop-wait loop tests (shell e2e) ──────────────────────────
# Guards against regressions to the stop-wait-before-cp block introduced to
# prevent ETXTBSY ("Text file busy") when the node's snapshot flush is still
# running at deploy time.  Exercises: immediate exit, polling, SIGKILL
# escalation, ordering (loop before cp), Telegram alert on forced kill.
echo "--- NodeStopWaitTests"
if bash "$SCRIPT_DIR/test-update-node-stop-wait.sh"; then
    echo "PASS: NodeStopWaitTests"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: NodeStopWaitTests (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── aperod_backup.sh self-check guard (shell unit test) ───────────────────────
# Verifies that _sync_backup_script() (sourced by update-node.sh at Step 1b)
# runs three integrity checks after installing a new version of aperod_backup.sh
# and exits non-zero when the repo copy is truncated or syntactically broken.
# No Docker required — sources the real sync-backup-script.sh directly.
echo "--- UpdateNodeBackupSyntaxGuard"
if bash "$SCRIPT_DIR/test-update-node-backup-syntax.sh"; then
    echo "PASS: UpdateNodeBackupSyntaxGuard"
    PASS=$((PASS + 1))
else
    EXIT=$?
    echo "FAIL: UpdateNodeBackupSyntaxGuard (exit $EXIT)"
    FAIL=$((FAIL + 1))
fi
echo ""

# ── Summary ──────────────────────────────────────────────────────────────────
echo "=== Results: PASS=$PASS  FAIL=$FAIL  SKIP=${#SKIPPED[@]} ==="

if [ "$FAIL" -gt 0 ]; then
    echo "FAIL — $FAIL test(s) failed"
    exit 1
fi

if [ "${#SKIPPED[@]}" -gt 0 ]; then
    echo "SKIPPED SCENARIOS:"
    printf '  - %s\n' "${SKIPPED[@]}"
    echo "SKIP_SUMMARY count=${#SKIPPED[@]}"
    exit 77
fi

echo "OK"
exit 0
