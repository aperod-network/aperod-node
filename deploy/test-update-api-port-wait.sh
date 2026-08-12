#!/usr/bin/env bash
# =============================================================================
#  Tests for the wait_port_free helper in update-api.sh
#
#  Regression guard for the post-deploy restart spike: pm2 must not be started
#  while the old listener still holds the API port.  wait_port_free() must:
#    P1. return 0 immediately when the port is already free (no kill issued)
#    P2. kill the holder and poll until the port is released (transient hold)
#    P3. escalate to SIGKILL after the timeout and return 0 once freed
#    P4. return 1 when the port can never be freed (caller aborts the deploy)
#
#  The helper is extracted verbatim from update-api.sh and exercised with a
#  stubbed `fuser` so no real sockets or processes are involved.
#
#  Usage:  bash deploy/test-update-api-port-wait.sh
#  Exit:   0 all passed / 1 failures
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_API="${SCRIPT_DIR}/update-api.sh"
[[ -f "$UPDATE_API" ]] || { echo "ERROR: not found: $UPDATE_API" >&2; exit 1; }

GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
PASS=0; FAIL=0
pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$((PASS+1)); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$((FAIL+1)); }

# ── Extract the helper function verbatim from update-api.sh ──────────────────
FUNC_SRC="$(awk '/^wait_port_free\(\) \{/,/^\}/' "$UPDATE_API")"
if [[ -z "$FUNC_SRC" ]]; then
    fail_test "P0: wait_port_free() not found in update-api.sh"
    echo -e "${RED}1 test(s) failed.${NC}" >&2
    exit 1
fi
pass_test "P0: wait_port_free() present in update-api.sh"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Runs one scenario in an isolated bash process.
#   $1 — name of a state file consumed by the fuser stub
#   $2 — timeout passed to wait_port_free
# The fuser stub semantics: the state file holds an integer N; each *probe*
# (fuser without -k) decrements it and reports "busy" while N > 0.  A kill
# (-k without -KILL) is a no-op; -KILL sets N=0 unless N is "sticky" (-1 = the
# port can never be freed).
run_scenario() {
    local state_file="$1" timeout="$2"
    STATE_FILE="$state_file" FUNC_SRC="$FUNC_SRC" bash -s "$timeout" <<'SCENARIO'
set -uo pipefail
kill_log="${STATE_FILE}.kills"
fuser() {
    local n; n="$(cat "$STATE_FILE")"
    if [[ "$1" == "-k" ]]; then
        echo "$*" >> "$kill_log"
        if [[ "${2:-}" == "-KILL" || "$*" == *"-KILL"* ]]; then
            [[ "$n" != "-1" ]] && echo 0 > "$STATE_FILE"
        fi
        return 0
    fi
    # probe
    if [[ "$n" == "-1" ]]; then return 0; fi          # sticky busy
    if (( n > 0 )); then echo $((n - 1)) > "$STATE_FILE"; return 0; fi
    return 1                                          # port free
}
sleep() { :; }   # no real waiting in tests
eval "$FUNC_SRC"
wait_port_free 3001 "$1"
SCENARIO
}

# ── P1: port already free — immediate success, no kill ───────────────────────
{
    st="$WORKDIR/p1"; echo 0 > "$st"
    if run_scenario "$st" 10 >/dev/null && [[ ! -s "${st}.kills" ]]; then
        pass_test "P1: free port — returns 0 without issuing a kill"
    else
        fail_test "P1: expected immediate success with no kill (kills: $(cat "${st}.kills" 2>/dev/null))"
    fi
}

# ── P2: transient hold — released after a few polls ──────────────────────────
{
    st="$WORKDIR/p2"; echo 3 > "$st"
    if out="$(run_scenario "$st" 10)" && grep -q "is free" <<<"$out"; then
        pass_test "P2: transiently-held port — helper polls until released"
    else
        fail_test "P2: expected success after transient hold (output: $out)"
    fi
}

# ── P3: hold exceeds timeout — SIGKILL escalation frees the port ─────────────
{
    st="$WORKDIR/p3"; echo 99 > "$st"
    if out="$(run_scenario "$st" 2)" && grep -q -- "-KILL" "${st}.kills"; then
        pass_test "P3: timeout — escalates to SIGKILL and succeeds once freed"
    else
        fail_test "P3: expected SIGKILL escalation (kills: $(cat "${st}.kills" 2>/dev/null))"
    fi
}

# ── P4: port can never be freed — returns non-zero ───────────────────────────
{
    st="$WORKDIR/p4"; echo -1 > "$st"
    if run_scenario "$st" 2 >/dev/null 2>&1; then
        fail_test "P4: expected failure when port can never be freed"
    else
        pass_test "P4: unfreeable port — returns non-zero so the deploy aborts"
    fi
}

# ── P5: update-api.sh actually calls the helper before pm2 restart ───────────
{
    call_line="$(grep -n 'wait_port_free "\$API_PORT"' "$UPDATE_API" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1 || true)"
    restart_line="$(grep -n 'pm2 restart' "$UPDATE_API" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1 || true)"
    if [[ -n "$call_line" && -n "$restart_line" && "$call_line" -lt "$restart_line" ]]; then
        pass_test "P5: wait_port_free is invoked (line $call_line) BEFORE pm2 restart (line $restart_line)"
    else
        fail_test "P5: wait_port_free must run before pm2 restart (call=$call_line, restart=$restart_line)"
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
