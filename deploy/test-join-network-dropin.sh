#!/usr/bin/env bash
# test-join-network-dropin.sh — E2E test: verify-dropin.sh failure aborts join-network.sh
#
# Stubs ssh, rsync, and systemctl so the full push-mode flow of join-network.sh
# can run without real SSH targets, root access, or a live aperod installation.
# The ssh shim returns a failing systemctl show output when verify-dropin.sh
# queries the remote node, asserting that join-network.sh exits non-zero.
#
# Cases
# ─────
#   1. GOMEMLIMIT missing from systemctl show  → join-network.sh exits non-zero
#   2. GOMEMLIMIT value wrong                  → join-network.sh exits non-zero
#   3. Drop-in files absent on remote host     → join-network.sh exits non-zero
#   4. All values correct, files present       → join-network.sh exits 0
#      (health-wait resolves immediately via the ssh stub returning valid JSON)
#
# Exit codes
# ──────────
#   0  all cases passed
#   1  one or more cases failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOIN_SCRIPT="$SCRIPT_DIR/join-network.sh"

PASS=0
FAIL=0

# ── Helper ────────────────────────────────────────────────────────────────────
#
# _make_env TMPDIR CANON_GOMEMLIMIT SSH_SHOW_OUTPUT DROPIN_EXISTS
#   Populates TMPDIR with stub binaries and config files.
#
_make_env() {
    local tmpdir="$1"
    local canon_gomemlimit="$2"
    local ssh_show_output="$3"
    local dropin_exists="$4"

    # Primary data dir (required by join-network.sh pre-flight check)
    mkdir -p "$tmpdir/primary_data/chain.db"

    # Canonical drop-in file: verify-dropin.sh reads GOMEMLIMIT from here.
    printf '[Service]\nEnvironment="GOMEMLIMIT=%s"\n' "$canon_gomemlimit" \
        > "$tmpdir/gomemlimit.conf"

    # Store mocked responses in files (avoids quoting issues in heredocs).
    printf '%s\n' "$ssh_show_output" > "$tmpdir/systemctl_show_output.txt"
    printf '%s\n' "$dropin_exists"   > "$tmpdir/dropin_exists.txt"

    # ── ssh shim ──────────────────────────────────────────────────────────────
    # join-network.sh push-mode SSH calls (in order):
    #   Step 1:  "systemctl disable --now aperod-node ..."
    #   Step 4:  "rm -f ..."
    #   Step 5:  bash <<HEREDOC  (bootnode YAML edit — stdin flow)
    #   Step 6:  "chown ... && bash ensure-dropin.sh && systemctl enable ..."
    #   Step 7:  "curl -s .../api/v1/network/stats ..."
    # verify-dropin.sh SSH calls:
    #   "systemctl show aperod-node"
    #   "test -f '...' && echo yes || echo no"
    cat > "$tmpdir/ssh" << 'SHIM_EOF'
#!/usr/bin/env bash
# Fake ssh: ignore the host argument, dispatch on the remote command content.
MYDIR="$(cd "$(dirname "$0")" && pwd)"
shift  # drop "root@<IP>"

# Drain any heredoc piped to stdin so the caller is not blocked.
if [ ! -t 0 ]; then
    cat > /dev/null
fi

REMOTE_CMD="$*"

if echo "$REMOTE_CMD" | grep -q "systemctl show"; then
    # verify-dropin.sh: query drop-in values.
    cat "$MYDIR/systemctl_show_output.txt"
elif echo "$REMOTE_CMD" | grep -q "test -f"; then
    # verify-dropin.sh: check whether drop-in files exist.
    cat "$MYDIR/dropin_exists.txt"
elif echo "$REMOTE_CMD" | grep -q "curl"; then
    # Step 7 health-wait: return valid stats so the loop exits immediately.
    printf '{"height":1000,"peer_count":2}\n'
elif echo "$REMOTE_CMD" | grep -qE "systemctl (disable|stop|enable|start)"; then
    echo "stopped"
elif echo "$REMOTE_CMD" | grep -q "rm -f"; then
    echo "removed"
elif echo "$REMOTE_CMD" | grep -qE "chown|ensure-dropin"; then
    echo "started"
else
    # Unknown command (e.g. bash heredoc for step 5) — safe no-op.
    echo ""
fi
exit 0
SHIM_EOF
    chmod +x "$tmpdir/ssh"

    # ── rsync shim ────────────────────────────────────────────────────────────
    cat > "$tmpdir/rsync" << 'RSYNC_EOF'
#!/usr/bin/env bash
exit 0
RSYNC_EOF
    chmod +x "$tmpdir/rsync"

    # ── systemctl shim (local calls only) ────────────────────────────────────
    # join-network.sh uses systemctl locally to stop the source node before
    # rsync and to start it again afterwards.
    cat > "$tmpdir/systemctl" << 'SYSCTL_EOF'
#!/usr/bin/env bash
if echo "$*" | grep -q "is-active"; then
    # Not active → stop-wait polling loop exits on the first iteration.
    exit 1
fi
# stop, start, enable, disable → success.
exit 0
SYSCTL_EOF
    chmod +x "$tmpdir/systemctl"
}

# ── run_case ──────────────────────────────────────────────────────────────────
#
# run_case NAME EXPECTED_EXIT SSH_SHOW_OUTPUT DROPIN_EXISTS CANON_GOMEMLIMIT
#   EXPECTED_EXIT — integer exit code, "nonzero", or "zero"
run_case() {
    local name="$1"
    local expected_exit="$2"
    local ssh_show_output="$3"
    local dropin_exists="${4:-yes}"
    local canon_gomemlimit="${5:-5905580032}"

    local tmpdir
    tmpdir="$(mktemp -d)"

    _make_env "$tmpdir" "$canon_gomemlimit" "$ssh_show_output" "$dropin_exists"

    local actual_exit=0
    PATH="$tmpdir:$PATH" \
    PRIMARY_DATA_DIR="$tmpdir/primary_data" \
    PRIMARY_IP="10.0.0.1" \
    SECONDARY_DATA_DIR="$tmpdir/secondary_data" \
    SECONDARY_NODE_YAML="$tmpdir/node.yaml" \
    SECONDARY_NODE_CONFIG_SH="" \
    CANONICAL_DROPIN="$tmpdir/gomemlimit.conf" \
        bash "$JOIN_SCRIPT" "192.168.1.100" > /dev/null 2>&1 \
        || actual_exit=$?

    rm -rf "$tmpdir"

    echo "--- $name"
    local ok=0
    case "$expected_exit" in
        nonzero) [[ "$actual_exit" -ne 0 ]] && ok=1 ;;
        zero)    [[ "$actual_exit" -eq 0 ]] && ok=1 ;;
        *)       [[ "$actual_exit" -eq "$expected_exit" ]] && ok=1 ;;
    esac

    if [[ "$ok" -eq 1 ]]; then
        echo "PASS: $name"
        PASS=$((PASS + 1))
    else
        echo "FAIL: $name — expected exit '$expected_exit', got $actual_exit"
        FAIL=$((FAIL + 1))
    fi
    echo ""
}

# ── Test cases ────────────────────────────────────────────────────────────────

echo "=== join-network.sh dropin-gate tests ==="
echo ""

CANON=5905580032

# Case 1: GOMEMLIMIT completely absent from systemctl show output.
# verify-dropin.sh exits 1 → join-network.sh must abort (exit non-zero).
run_case "DropinGate_MissingGOMEMLIMIT_ExitsNonZero" "nonzero" \
    "TimeoutStopUSec=15min" \
    "yes" "$CANON"

# Case 2: GOMEMLIMIT present but the installed value differs from the canonical
# file (drop-in was never updated after the limit was bumped).
# verify-dropin.sh exits 1 → join-network.sh must abort (exit non-zero).
run_case "DropinGate_WrongGOMEMLIMIT_ExitsNonZero" "nonzero" \
    "Environment=GOMEMLIMIT=1073741824
TimeoutStopUSec=15min" \
    "yes" "$CANON"

# Case 3: Drop-in files are missing on the remote host.
# verify-dropin.sh exits 1 → join-network.sh must abort (exit non-zero).
run_case "DropinGate_MissingDropinFiles_ExitsNonZero" "nonzero" \
    "Environment=GOMEMLIMIT=${CANON}
TimeoutStopUSec=15min" \
    "no" "$CANON"

# Case 4: All values correct and drop-in files present.
# verify-dropin.sh exits 0 → join-network.sh proceeds past the gate and
# reaches the health-wait step (step 7).  The ssh stub returns valid JSON
# for the curl command so the loop exits on the first iteration with exit 0.
run_case "DropinGate_CorrectValues_JoinSucceeds" "zero" \
    "Environment=GOMEMLIMIT=${CANON}
TimeoutStopUSec=15min" \
    "yes" "$CANON"

# ── Summary ───────────────────────────────────────────────────────────────────

echo "=== Results: PASS=$PASS  FAIL=$FAIL ==="
echo ""

if [[ "$FAIL" -gt 0 ]]; then
    echo "FAIL — $FAIL test(s) failed"
    exit 1
fi

echo "OK"
exit 0
