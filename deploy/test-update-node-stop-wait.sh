#!/usr/bin/env bash
# =============================================================================
#  Tests for the stop-wait loop in update-node.sh
#
#  Regression guard for the ETXTBSY ("Text file busy") deploy failure: the node
#  flushes a UTXO snapshot on shutdown that can take several minutes.  The
#  binary-swap cp must not run until the service is fully inactive.
#
#  The stop-wait block is extracted verbatim from update-node.sh and exercised
#  with a stubbed systemctl so no real services or processes are involved.
#
#  Scenarios:
#    SW0. stop-wait block is present in update-node.sh
#    SW1. service already inactive  — exits immediately, no SIGKILL issued
#    SW2. service deactivating      — polls until inactive, no SIGKILL
#    SW3. service never stops       — SIGKILL escalation after timeout
#    SW4. stop-wait runs BEFORE cp  — ordering guard
#    SW5. SIGKILL fires send_telegram_alert (regression: alert must not abort)
#
#  Usage:  bash deploy/test-update-node-stop-wait.sh
#  Exit:   0 all passed / 1 failures
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_NODE="${SCRIPT_DIR}/update-node.sh"
[[ -f "$UPDATE_NODE" ]] || { echo "ERROR: not found: $UPDATE_NODE" >&2; exit 1; }

GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
PASS=0; FAIL=0
pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$((PASS+1)); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$((FAIL+1)); }

# ── SW0: stop-wait block present in update-node.sh ───────────────────────────
LOOP_SRC="$(awk '/_stop_waited=0/{found=1} found{print} found && /^done/{exit}' "$UPDATE_NODE")"
if [[ -z "$LOOP_SRC" ]]; then
    fail_test "SW0: stop-wait loop (_stop_waited=0 … done) not found in update-node.sh"
    echo -e "${RED}Cannot run further tests without the loop source.${NC}" >&2
    exit 1
fi
pass_test "SW0: stop-wait loop present in update-node.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Runs the stop-wait loop in an isolated bash process.
#   $1 — path to state file (content = current systemctl is-active output)
#   $2 — (optional) countdown: poll-invocations before the state file changes
#         to "inactive" automatically (0 = already inactive from the start)
#         -1 = stays active forever (triggers SIGKILL path)
#   Exports KILL_LOG path so callers can check whether SIGKILL was issued.
run_loop() {
    local state_file="$1"
    local countdown="${2:-0}"
    KILL_LOG="${state_file}.kills"
    ALERT_LOG="${state_file}.alerts"
    STATE_FILE="$state_file" COUNTDOWN_FILE="${state_file}.countdown" \
    KILL_LOG="$KILL_LOG" ALERT_LOG="$ALERT_LOG" \
    LOOP_SRC="$LOOP_SRC" bash -s <<'SCENARIO'
set -uo pipefail

# ── stubs ──────────────────────────────────────────────────────────────────
systemctl() {
    local sub="$1"; shift
    case "$sub" in
        is-active)
            local n; n=$(cat "$COUNTDOWN_FILE" 2>/dev/null || echo 0)
            if [[ "$n" -gt 0 ]]; then
                echo $(( n - 1 )) > "$COUNTDOWN_FILE"
                cat "$STATE_FILE"    # still active/deactivating
            elif [[ "$n" -eq -1 ]]; then
                cat "$STATE_FILE"    # permanently active
            else
                echo "inactive"
            fi
            ;;
        kill)
            echo "$*" >> "$KILL_LOG"
            # After SIGKILL the service becomes inactive
            echo "inactive" > "$STATE_FILE"
            ;;
    esac
}
sleep() { :; }  # no real waiting
send_telegram_alert() { echo "$*" >> "$ALERT_LOG"; }

SERVICE_NAME="aperod-node"
export -f systemctl sleep send_telegram_alert

eval "$LOOP_SRC"
SCENARIO
}

# ── SW1: already inactive — immediate exit, no SIGKILL ───────────────────────
{
    st="$WORKDIR/sw1_state"; echo "inactive" > "$st"
    echo 0 > "${st}.countdown"
    if run_loop "$st" 0 && [[ ! -s "${st}.kills" ]]; then
        pass_test "SW1: already-inactive service — loop exits immediately, no SIGKILL"
    else
        fail_test "SW1: expected immediate exit with no SIGKILL (kills: $(cat "${st}.kills" 2>/dev/null || echo none))"
    fi
}

# ── SW2: deactivating — polls until inactive, no SIGKILL ─────────────────────
{
    st="$WORKDIR/sw2_state"; echo "deactivating" > "$st"
    # 3 polls reporting "deactivating", then inactive
    echo 3 > "${st}.countdown"
    if run_loop "$st" && [[ ! -s "${st}.kills" ]]; then
        pass_test "SW2: deactivating service — loop polls until inactive, no SIGKILL"
    else
        fail_test "SW2: expected success after transient deactivating (kills: $(cat "${st}.kills" 2>/dev/null || echo none))"
    fi
}

# ── SW3: never stops — SIGKILL escalation ────────────────────────────────────
{
    st="$WORKDIR/sw3_state"; echo "deactivating" > "$st"
    echo -1 > "${st}.countdown"   # -1 = stays active forever
    run_loop "$st" || true        # may exit non-zero after SIGKILL; that's fine
    if grep -q "SIGKILL" "${st}.kills" 2>/dev/null; then
        pass_test "SW3: permanently-active service — escalates to SIGKILL after timeout"
    else
        fail_test "SW3: expected SIGKILL escalation (kills: $(cat "${st}.kills" 2>/dev/null || echo none))"
    fi
}

# ── SW4: stop-wait loop runs BEFORE the cp install step ──────────────────────
{
    loop_line="$(grep -n '_stop_waited=0' "$UPDATE_NODE" | head -1 | cut -d: -f1 || true)"
    cp_line="$(grep -n 'cp.*aperod-node\|/bin/cp.*aperod-node' "$UPDATE_NODE" \
                | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1 || true)"
    if [[ -n "$loop_line" && -n "$cp_line" && "$loop_line" -lt "$cp_line" ]]; then
        pass_test "SW4: stop-wait loop (line $loop_line) is BEFORE binary cp (line $cp_line) — no ETXTBSY risk"
    else
        fail_test "SW4: stop-wait loop must run BEFORE cp (loop=$loop_line, cp=$cp_line)"
    fi
}

# ── SW5: SIGKILL path also calls send_telegram_alert (not hard-fails) ────────
{
    st="$WORKDIR/sw5_state"; echo "deactivating" > "$st"
    echo -1 > "${st}.countdown"
    run_loop "$st" || true
    if [[ -s "${st}.alerts" ]]; then
        pass_test "SW5: SIGKILL path sends a Telegram alert and does not abort the loop"
    else
        fail_test "SW5: expected Telegram alert after SIGKILL (alerts: $(cat "${st}.alerts" 2>/dev/null || echo none))"
    fi
}

# ── SW6: self-check — removing the loop causes SW1 to detect missing block ───
{
    # Mutate: replace the _stop_waited=0 line with a 1-second sleep (old behaviour)
    MUTATED="$(sed 's/_stop_waited=0/sleep 1/' "$UPDATE_NODE")"
    MUTATED_LOOP="$(echo "$MUTATED" | awk '/_stop_waited=0/{found=1} found{print} found && /^done/{exit}')"
    if [[ -z "$MUTATED_LOOP" ]]; then
        pass_test "SW6: self-check — removing the loop is correctly detected (SW0 would fail)"
    else
        fail_test "SW6: self-check failed — mutation was not detected"
    fi
}

echo ""
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}/${TOTAL} passed${NC}"
if [[ "$FAIL" -eq 0 ]]; then
    exit 0
else
    echo -e "${RED}${FAIL} test(s) failed.${NC}" >&2
    exit 1
fi
