#!/usr/bin/env bash
# =============================================================================
#  Static-analysis tests for deploy script stop/build/start ordering
#
#  Guards against the "Text file busy" regression: aperod-node must be stopped
#  BEFORE the Go binary is rebuilt.  If a future edit moves `systemctl stop`
#  to after `make build`, the binary is overwritten while the kernel still has
#  it mapped and Linux returns ETXTBSY ("Text file busy").
#
#  Tests for deploy/deploy-server.sh:
#    T1. `systemctl stop aperod-node` appears in the script
#    T2. `make build` (Go node build) appears in the script
#    T3. `systemctl stop aperod-node` appears on an EARLIER line than `make build`
#    T4. Regression guard: `systemctl restart aperod-node` (start) appears
#        AFTER `make build` — i.e., start is not accidentally placed before stop
#    T5. Self-check: the test itself detects a mutated (wrong-order) script
#
#  Tests for deploy/deploy.sh:
#    T6. `systemctl stop` appears in the script (loop-style stop)
#    T7. `make build` (Go node build) appears in the script
#    T8. `systemctl stop` appears on an EARLIER line than `make build`
#    T9. Regression guard: `systemctl restart` (start) appears AFTER `make build`
#   T10. Self-check: the test itself detects a mutated (wrong-order) deploy.sh
#
#  Presence tests for critical steps in BOTH deploy scripts:
#   T11. `make deps` is present in both scripts
#   T12. `pnpm install` is present in both scripts
#
#  Service-restart presence tests:
#   T13. `systemctl restart aperod-api` appears in deploy-server.sh
#   T14. deploy.sh combined restart line contains `aperod-api`
#   T15. deploy.sh combined restart line contains `aperod-node`
#
#  GOMEMLIMIT drop-in presence tests:
#   T16. a `cp ... gomemlimit.conf` command is present in deploy-server.sh
#   T17. a `cp ... gomemlimit.conf` command is present in deploy.sh
#   T18. self-check: deploy-server.sh with cp removed is correctly detected
#   T19. self-check: deploy.sh with cp removed is correctly detected
#
#  GOMEMLIMIT value tests:
#   T20. gomemlimit.conf contains GOMEMLIMIT=5905580032 (5.5 GB exactly)
#   T21. gomemlimit.conf contains TimeoutStopSec=900
#
#  Smoke-test script presence tests:
#   T22. deploy/smoke-test-gomemlimit.sh exists in the repo
#   T23. the smoke-test call appears in deploy-server.sh
#   T24. the smoke-test call appears in deploy.sh
#   T25. self-check: a deploy script missing the smoke-test call is detected
#
#  Usage:
#    bash deploy/test-deploy-order.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -euo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_SERVER="${SCRIPT_DIR}/deploy-server.sh"
DEPLOY="${SCRIPT_DIR}/deploy.sh"

[[ -f "$DEPLOY_SERVER" ]] || { echo "ERROR: deploy script not found: $DEPLOY_SERVER" >&2; exit 1; }
[[ -f "$DEPLOY" ]]        || { echo "ERROR: deploy script not found: $DEPLOY" >&2; exit 1; }

GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
PASS=0; FAIL=0

pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$((PASS+1)); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$((FAIL+1)); }

# ---------------------------------------------------------------------------
# Helper: return the first line-number in FILE that matches PATTERN,
# skipping comment lines (lines whose first non-whitespace character is #).
# Returns empty string if not found.
# ---------------------------------------------------------------------------
first_line_of() {
    local file="$1" pattern="$2"
    # The trailing `|| true` prevents pipefail from aborting the caller when
    # grep finds no matches (exit 1) or grep -v filters every matched line
    # (also exit 1). The function always returns 0; callers check for empty output.
    grep -n "$pattern" "$file" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1 || true
}

# =============================================================================
#  Tests for deploy/deploy-server.sh
# =============================================================================
echo -e "\n${CYAN}Running deploy-order tests for deploy/deploy-server.sh…${NC}\n"

# ── T1: `systemctl stop aperod-node` is present ──────────────────────────────
{
    stop_line=$(first_line_of "$DEPLOY_SERVER" 'systemctl stop aperod-node')
    if [[ -n "$stop_line" ]]; then
        pass_test "T1: 'systemctl stop aperod-node' found (line $stop_line)"
    else
        fail_test "T1: 'systemctl stop aperod-node' NOT found in deploy-server.sh"
    fi
}

# ── T2: `make build` is present ──────────────────────────────────────────────
{
    build_line=$(first_line_of "$DEPLOY_SERVER" 'make build')
    if [[ -n "$build_line" ]]; then
        pass_test "T2: 'make build' found (line $build_line)"
    else
        fail_test "T2: 'make build' NOT found in deploy-server.sh"
    fi
}

# ── T3: stop comes BEFORE build ──────────────────────────────────────────────
# This is the core regression guard.
{
    stop_line=$(first_line_of "$DEPLOY_SERVER" 'systemctl stop aperod-node')
    build_line=$(first_line_of "$DEPLOY_SERVER" 'make build')

    if [[ -z "$stop_line" || -z "$build_line" ]]; then
        fail_test "T3: cannot compare ordering — one or both patterns missing (stop=$stop_line, build=$build_line)"
    elif [[ "$stop_line" -lt "$build_line" ]]; then
        pass_test "T3: 'systemctl stop aperod-node' (line $stop_line) is BEFORE 'make build' (line $build_line) — no ETXTBSY risk"
    else
        fail_test "T3: 'systemctl stop aperod-node' (line $stop_line) is NOT before 'make build' (line $build_line) — ETXTBSY regression!"
    fi
}

# ── T4: `systemctl restart aperod-node` (start) is AFTER `make build` ────────
# Ensures the start was not accidentally placed before stop+build.
{
    restart_line=$(first_line_of "$DEPLOY_SERVER" 'systemctl restart aperod-node')
    build_line=$(first_line_of "$DEPLOY_SERVER" 'make build')

    if [[ -z "$restart_line" ]]; then
        fail_test "T4: 'systemctl restart aperod-node' NOT found in deploy-server.sh"
    elif [[ -z "$build_line" ]]; then
        fail_test "T4: 'make build' NOT found — cannot verify restart ordering"
    elif [[ "$restart_line" -gt "$build_line" ]]; then
        pass_test "T4: 'systemctl restart aperod-node' (line $restart_line) is AFTER 'make build' (line $build_line) — correct sequence"
    else
        fail_test "T4: 'systemctl restart aperod-node' (line $restart_line) is before 'make build' (line $build_line) — start appears before build!"
    fi
}

# ── T5: self-check — mutated script (wrong order) is detected ────────────────
# Writes a temporary deploy script where stop is placed AFTER build and
# confirms that the ordering check would return exit 1.
{
    tmp=$(mktemp)
    # Minimal fake deploy script with inverted order: build first, then stop
    cat > "$tmp" <<'FAKE'
#!/usr/bin/env bash
# fake deploy — wrong order (build before stop)
sudo -u aperod bash -c "cd /opt/aperod/blockchain && make deps && make build"
systemctl stop aperod-node || true
systemctl restart aperod-node
FAKE

    stop_line=$(first_line_of "$tmp" 'systemctl stop aperod-node')
    build_line=$(first_line_of "$tmp" 'make build')

    detected=false
    if [[ -n "$stop_line" && -n "$build_line" && "$stop_line" -ge "$build_line" ]]; then
        detected=true
    fi

    rm -f "$tmp"

    if $detected; then
        pass_test "T5: self-check — mutated (wrong-order) script is correctly detected as a regression"
    else
        fail_test "T5: self-check — mutated script was NOT flagged; ordering check may have a bug"
    fi
}

# =============================================================================
#  Tests for deploy/deploy.sh
# =============================================================================
echo -e "\n${CYAN}Running deploy-order tests for deploy/deploy.sh…${NC}\n"

# ── T6: `systemctl stop` is present ──────────────────────────────────────────
# deploy.sh stops services in a loop via `systemctl stop "$svc"`; match the
# literal token that appears on non-comment lines.
{
    stop_line=$(first_line_of "$DEPLOY" 'systemctl stop')
    if [[ -n "$stop_line" ]]; then
        pass_test "T6: 'systemctl stop' found in deploy.sh (line $stop_line)"
    else
        fail_test "T6: 'systemctl stop' NOT found in deploy.sh"
    fi
}

# ── T7: `make build` is present ──────────────────────────────────────────────
{
    build_line=$(first_line_of "$DEPLOY" 'make build')
    if [[ -n "$build_line" ]]; then
        pass_test "T7: 'make build' found in deploy.sh (line $build_line)"
    else
        fail_test "T7: 'make build' NOT found in deploy.sh"
    fi
}

# ── T8: stop comes BEFORE build ──────────────────────────────────────────────
{
    stop_line=$(first_line_of "$DEPLOY" 'systemctl stop')
    build_line=$(first_line_of "$DEPLOY" 'make build')

    if [[ -z "$stop_line" || -z "$build_line" ]]; then
        fail_test "T8: cannot compare ordering — one or both patterns missing (stop=$stop_line, build=$build_line)"
    elif [[ "$stop_line" -lt "$build_line" ]]; then
        pass_test "T8: 'systemctl stop' (line $stop_line) is BEFORE 'make build' (line $build_line) — no ETXTBSY risk"
    else
        fail_test "T8: 'systemctl stop' (line $stop_line) is NOT before 'make build' (line $build_line) — ETXTBSY regression!"
    fi
}

# ── T9: `systemctl restart` (start) is AFTER `make build` ────────────────────
{
    restart_line=$(first_line_of "$DEPLOY" 'systemctl restart')
    build_line=$(first_line_of "$DEPLOY" 'make build')

    if [[ -z "$restart_line" ]]; then
        fail_test "T9: 'systemctl restart' NOT found in deploy.sh"
    elif [[ -z "$build_line" ]]; then
        fail_test "T9: 'make build' NOT found — cannot verify restart ordering"
    elif [[ "$restart_line" -gt "$build_line" ]]; then
        pass_test "T9: 'systemctl restart' (line $restart_line) is AFTER 'make build' (line $build_line) — correct sequence"
    else
        fail_test "T9: 'systemctl restart' (line $restart_line) is before 'make build' (line $build_line) — start appears before build!"
    fi
}

# ── T10: self-check — mutated deploy.sh (wrong order) is detected ────────────
{
    tmp=$(mktemp)
    # Minimal fake deploy.sh with inverted order: build first, then stop
    cat > "$tmp" <<'FAKE'
#!/usr/bin/env bash
# fake deploy.sh — wrong order (build before stop)
sudo -u aperod bash -c "cd /opt/aperod/blockchain && make deps && make build"
for svc in aperod-api aperod-node; do
    systemctl stop "$svc" || true
done
systemctl restart aperod-api aperod-node
FAKE

    stop_line=$(first_line_of "$tmp" 'systemctl stop')
    build_line=$(first_line_of "$tmp" 'make build')

    detected=false
    if [[ -n "$stop_line" && -n "$build_line" && "$stop_line" -ge "$build_line" ]]; then
        detected=true
    fi

    rm -f "$tmp"

    if $detected; then
        pass_test "T10: self-check — mutated deploy.sh (wrong-order) is correctly detected as a regression"
    else
        fail_test "T10: self-check — mutated deploy.sh was NOT flagged; ordering check may have a bug"
    fi
}

# =============================================================================
#  Presence tests for critical steps in BOTH deploy scripts
# =============================================================================
echo -e "\n${CYAN}Running presence tests (T11–T12) for both deploy scripts…${NC}\n"

# ── T11: `make deps` is present in BOTH deploy scripts ───────────────────────
# A future edit could silently drop `make deps`, leaving Go module caches stale
# and causing build failures when new modules are introduced.
{
    deps_server=$(first_line_of "$DEPLOY_SERVER" 'make deps')
    deps_main=$(first_line_of "$DEPLOY" 'make deps')

    missing=()
    [[ -z "$deps_server" ]] && missing+=("deploy-server.sh")
    [[ -z "$deps_main"   ]] && missing+=("deploy.sh")

    if [[ ${#missing[@]} -eq 0 ]]; then
        pass_test "T11: 'make deps' found in both deploy scripts (deploy-server.sh line $deps_server, deploy.sh line $deps_main)"
    else
        fail_test "T11: 'make deps' NOT found in: ${missing[*]} — Go dependency step may have been dropped"
    fi
}

# ── T12: `pnpm install` is present in BOTH deploy scripts ────────────────────
# Omitting `pnpm install` causes the Node.js build steps (tsc, esbuild, vite)
# to fail when new packages are added to the lockfile.
{
    pnpm_server=$(first_line_of "$DEPLOY_SERVER" 'pnpm install')
    pnpm_main=$(first_line_of "$DEPLOY" 'pnpm install')

    missing=()
    [[ -z "$pnpm_server" ]] && missing+=("deploy-server.sh")
    [[ -z "$pnpm_main"   ]] && missing+=("deploy.sh")

    if [[ ${#missing[@]} -eq 0 ]]; then
        pass_test "T12: 'pnpm install' found in both deploy scripts (deploy-server.sh line $pnpm_server, deploy.sh line $pnpm_main)"
    else
        fail_test "T12: 'pnpm install' NOT found in: ${missing[*]} — Node.js dependency step may have been dropped"
    fi
}

# =============================================================================
#  Service-restart presence tests (T13–T15)
#
#  deploy-server.sh restarts each service on a SEPARATE line:
#    systemctl restart aperod-api
#    systemctl restart aperod-node
#
#  deploy.sh uses a COMBINED restart on ONE line:
#    systemctl restart aperod-api aperod-node
#
#  If either service name is accidentally removed, the old binary keeps running
#  after a deploy and the node (or API) goes offline silently.
# =============================================================================
echo -e "\n${CYAN}Running service-restart presence tests (T13–T15)…${NC}\n"

# ── T13: `systemctl restart aperod-api` appears in deploy-server.sh ──────────
# T4 already guards aperod-node ordering; this test adds the mirror guard for
# aperod-api so a removal of either service name is independently caught.
{
    restart_api_line=$(first_line_of "$DEPLOY_SERVER" 'systemctl restart aperod-api')
    if [[ -n "$restart_api_line" ]]; then
        pass_test "T13: 'systemctl restart aperod-api' found in deploy-server.sh (line $restart_api_line)"
    else
        fail_test "T13: 'systemctl restart aperod-api' NOT found in deploy-server.sh — aperod-api may not be restarted on deploy"
    fi
}

# ── T14: deploy.sh combined restart line contains `aperod-api` ───────────────
# deploy.sh uses `systemctl restart aperod-api aperod-node` on one line.
# Check that `aperod-api` is named explicitly so neither service can be
# silently dropped without this test failing.
{
    if grep -qE 'systemctl[[:space:]]+restart[^#]*aperod-api' "$DEPLOY"; then
        line=$(grep -n 'systemctl.*restart.*aperod-api' "$DEPLOY" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1)
        pass_test "T14: 'aperod-api' present in systemctl restart line of deploy.sh (line $line)"
    else
        fail_test "T14: 'aperod-api' NOT found in any 'systemctl restart' line of deploy.sh — service may be missing from combined restart"
    fi
}

# ── T15: deploy.sh combined restart line contains `aperod-node` ──────────────
{
    if grep -qE 'systemctl[[:space:]]+restart[^#]*aperod-node' "$DEPLOY"; then
        line=$(grep -n 'systemctl.*restart.*aperod-node' "$DEPLOY" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1)
        pass_test "T15: 'aperod-node' present in systemctl restart line of deploy.sh (line $line)"
    else
        fail_test "T15: 'aperod-node' NOT found in any 'systemctl restart' line of deploy.sh — service may be missing from combined restart"
    fi
}

# =============================================================================
#  GOMEMLIMIT drop-in presence tests (T16–T19)
#
#  Both deploy scripts must execute a `cp` command that installs gomemlimit.conf
#  into the systemd drop-in directory on every deploy (clean-server and update
#  alike).  Without the copy step the node runs without the GOMEMLIMIT=5.5 GB
#  cap and is OOM-killed on the 7.8 GB host.
#
#  The pattern `cp.*gomemlimit\.conf` is intentionally strict: it does NOT
#  match variable assignments like DROPIN_SRC=".../gomemlimit.conf", so
#  removing only the cp line is sufficient to fail T16/T17.
#
#  GOMEMLIMIT value tests (T20–T21)
#   T20. gomemlimit.conf contains GOMEMLIMIT=5905580032 (5.5 GB exactly)
#   T21. gomemlimit.conf contains TimeoutStopSec=900
# =============================================================================
echo -e "\n${CYAN}Running GOMEMLIMIT drop-in presence tests (T16–T19)…${NC}\n"

# ── T16: a cp command for gomemlimit.conf is present in deploy-server.sh ──────
# Matches `cp ... gomemlimit.conf` but NOT variable assignments such as
# DROPIN_SRC=".../gomemlimit.conf", so dropping only the cp line fails this test.
{
    dropin_cp_line=$(first_line_of "$DEPLOY_SERVER" 'cp.*gomemlimit\.conf')
    if [[ -n "$dropin_cp_line" ]]; then
        pass_test "T16: 'cp ... gomemlimit.conf' install step found in deploy-server.sh (line $dropin_cp_line)"
    else
        fail_test "T16: no 'cp ... gomemlimit.conf' install step in deploy-server.sh — GOMEMLIMIT drop-in will not be installed on update deploys, risking OOM"
    fi
}

# ── T17: a cp command for gomemlimit.conf is present in deploy.sh ─────────────
# deploy.sh uses a two-line cp (source on first line, dest on second);
# the source line already contains gomemlimit.conf so a single-line grep suffices.
{
    dropin_cp_line=$(first_line_of "$DEPLOY" 'cp.*gomemlimit\.conf')
    if [[ -n "$dropin_cp_line" ]]; then
        pass_test "T17: 'cp ... gomemlimit.conf' install step found in deploy.sh (line $dropin_cp_line)"
    else
        fail_test "T17: no 'cp ... gomemlimit.conf' install step in deploy.sh — GOMEMLIMIT drop-in will not be installed on clean-server deploys, risking OOM"
    fi
}

# ── T18: self-check — deploy-server.sh with cp removed is detected ────────────
# Creates a fake deploy-server.sh that keeps the DROPIN_SRC variable assignment
# (which contains gomemlimit.conf) but omits the actual cp command.  T16's
# pattern must NOT match this — if it does, T16 has a false-positive bug.
{
    tmp=$(mktemp)
    cat > "$tmp" <<'FAKE'
#!/usr/bin/env bash
# fake deploy-server.sh — cp step removed; only variable assignment remains
DROPIN_SRC="/opt/aperod/deploy/systemd/aperod-node.service.d/gomemlimit.conf"
DROPIN_DIR="/etc/systemd/system/aperod-node.service.d"
mkdir -p "${DROPIN_DIR}"
# cp "${DROPIN_SRC}" "${DROPIN_DIR}/gomemlimit.conf"   <-- intentionally removed
systemctl daemon-reload
FAKE

    cp_line=$(first_line_of "$tmp" 'cp.*gomemlimit\.conf')
    rm -f "$tmp"

    if [[ -z "$cp_line" ]]; then
        pass_test "T18: self-check — deploy-server.sh with cp removed is correctly flagged (variable-only reference does not pass T16)"
    else
        fail_test "T18: self-check — mutated deploy-server.sh (cp removed) was NOT flagged; T16 pattern is too broad"
    fi
}

# ── T19: self-check — deploy.sh with cp removed is detected ──────────────────
# Creates a fake deploy.sh where gomemlimit.conf appears only in a comment,
# not in a cp command.  T17's pattern must NOT match this.
{
    tmp=$(mktemp)
    cat > "$tmp" <<'FAKE'
#!/usr/bin/env bash
# fake deploy.sh — cp step removed; filename referenced only in a comment
DROPIN_DIR="/etc/systemd/system/aperod-node.service.d"
mkdir -p "${DROPIN_DIR}"
# TODO: cp gomemlimit.conf here (intentionally removed for this test)
systemctl daemon-reload
FAKE

    cp_line=$(first_line_of "$tmp" 'cp.*gomemlimit\.conf')
    rm -f "$tmp"

    if [[ -z "$cp_line" ]]; then
        pass_test "T19: self-check — deploy.sh with cp removed is correctly flagged (comment-only reference does not pass T17)"
    else
        fail_test "T19: self-check — mutated deploy.sh (cp removed) was NOT flagged; T17 pattern is too broad"
    fi
}

# =============================================================================
#  GOMEMLIMIT value tests (T20–T21)
#
#  The conf file is the source of truth for the memory limit.  Even if the cp
#  step exists, a wrong value in the source file would be silently deployed.
#
#  T20 asserts the exact byte value 5905580032 (5500 MiB = 5.5 GB).
#    - 6500 MiB caused OOM on the 7.8 GB host.
#    - 4500–5000 MiB caused GC thrash at 99% CPU.
#    - 5500 MiB is the stable value with ~500 MB GC headroom.
#
#  T21 asserts TimeoutStopSec=900 (15 minutes).
#    A shorter timeout truncates the UTXO snapshot on shutdown and forces
#    the next restart into the multi-hour 800K-block scan.
# =============================================================================
echo -e "\n${CYAN}Running GOMEMLIMIT value tests (T20–T21) for gomemlimit.conf…${NC}\n"

CONF="${SCRIPT_DIR}/systemd/aperod-node.service.d/gomemlimit.conf"
[[ -f "$CONF" ]] || { echo -e "${RED}[FAIL]${NC}  T20/T21: conf file not found: $CONF" >&2; FAIL=$((FAIL+2)); }

if [[ -f "$CONF" ]]; then

# ── T20: GOMEMLIMIT equals exactly 5905580032 ─────────────────────────────────
{
    EXPECTED_GOMEMLIMIT=5905580032
    # Extract the numeric value after GOMEMLIMIT= (handles both quoted and
    # unquoted forms: Environment="GOMEMLIMIT=5905580032" or GOMEMLIMIT=5905580032)
    actual=$(grep -E 'GOMEMLIMIT=' "$CONF" | grep -v '^[[:space:]]*#' \
             | sed -E 's/.*GOMEMLIMIT=([0-9]+).*/\1/' | head -1)
    if [[ "$actual" == "$EXPECTED_GOMEMLIMIT" ]]; then
        pass_test "T20: GOMEMLIMIT=$actual in gomemlimit.conf — correct (5.5 GB)"
    elif [[ -z "$actual" ]]; then
        fail_test "T20: GOMEMLIMIT not found in gomemlimit.conf — expected $EXPECTED_GOMEMLIMIT (5.5 GB)"
    else
        fail_test "T20: GOMEMLIMIT=$actual in gomemlimit.conf — expected $EXPECTED_GOMEMLIMIT (5.5 GB); wrong value risks OOM or GC thrash"
    fi
}

# ── T21: TimeoutStopSec=900 is present ────────────────────────────────────────
{
    EXPECTED_TIMEOUT=900
    actual=$(grep -E 'TimeoutStopSec=' "$CONF" | grep -v '^[[:space:]]*#' \
             | sed -E 's/.*TimeoutStopSec=([0-9]+).*/\1/' | head -1)
    if [[ "$actual" == "$EXPECTED_TIMEOUT" ]]; then
        pass_test "T21: TimeoutStopSec=$actual in gomemlimit.conf — correct (15 min snapshot flush window)"
    elif [[ -z "$actual" ]]; then
        fail_test "T21: TimeoutStopSec not found in gomemlimit.conf — expected $EXPECTED_TIMEOUT; shorter timeout truncates UTXO snapshot on shutdown"
    else
        fail_test "T21: TimeoutStopSec=$actual in gomemlimit.conf — expected $EXPECTED_TIMEOUT; shorter timeout truncates UTXO snapshot on shutdown"
    fi
}

fi  # end [[ -f "$CONF" ]]

# =============================================================================
#  Smoke-test script presence tests (T22–T25)
#
#  Both deploy scripts call `bash ${APP_DIR}/deploy/smoke-test-gomemlimit.sh`
#  after the services restart to confirm GOMEMLIMIT is active.  If that script
#  is accidentally deleted or renamed, or the call is removed from either
#  deploy script, the OOM guard check disappears without any warning.
#
#  T22 asserts the script file itself exists in the repo so a deletion or
#  rename is caught before a deploy is attempted.
#
#  T23/T24 use a strict regex that matches the actual bash invocation:
#    bash .*/smoke-test-gomemlimit\.sh
#  This does NOT match variable assignments (SMOKE=".../smoke-test-gomemlimit.sh")
#  or comment lines, so removing only the invocation line fails the test.
#
#  T25 is a self-check: a fake deploy script that references the script in a
#  comment but never calls it must NOT pass T23/T24's pattern.
# =============================================================================
echo -e "\n${CYAN}Running smoke-test script presence tests (T22–T25)…${NC}\n"

SMOKE_SCRIPT="${SCRIPT_DIR}/smoke-test-gomemlimit.sh"

# ── T22: smoke-test-gomemlimit.sh exists in the repo ─────────────────────────
{
    if [[ -f "$SMOKE_SCRIPT" ]]; then
        pass_test "T22: 'deploy/smoke-test-gomemlimit.sh' exists in the repo"
    else
        fail_test "T22: 'deploy/smoke-test-gomemlimit.sh' NOT found at $SMOKE_SCRIPT — OOM smoke test will silently fail on deploy"
    fi
}

# ── T23: smoke-test call is present in deploy-server.sh ──────────────────────
# Matches `bash .../smoke-test-gomemlimit.sh` on a non-comment line.
# Does NOT match variable assignments or comment-only references.
{
    smoke_line=$(first_line_of "$DEPLOY_SERVER" 'bash.*smoke-test-gomemlimit\.sh')
    if [[ -n "$smoke_line" ]]; then
        pass_test "T23: smoke-test-gomemlimit.sh is called in deploy-server.sh (line $smoke_line)"
    else
        fail_test "T23: no 'bash .../smoke-test-gomemlimit.sh' call found in deploy-server.sh — GOMEMLIMIT check will not run after update deploys"
    fi
}

# ── T24: smoke-test call is present in deploy.sh ─────────────────────────────
{
    smoke_line=$(first_line_of "$DEPLOY" 'bash.*smoke-test-gomemlimit\.sh')
    if [[ -n "$smoke_line" ]]; then
        pass_test "T24: smoke-test-gomemlimit.sh is called in deploy.sh (line $smoke_line)"
    else
        fail_test "T24: no 'bash .../smoke-test-gomemlimit.sh' call found in deploy.sh — GOMEMLIMIT check will not run after clean-server deploys"
    fi
}

# ── T25: self-check — a script missing the smoke-test call is detected ────────
# Creates a fake deploy script where smoke-test-gomemlimit.sh is only mentioned
# in a comment and in a variable assignment — never actually called.
# T23/T24's pattern must NOT match this, proving it won't produce a false pass.
{
    tmp=$(mktemp)
    cat > "$tmp" <<'FAKE'
#!/usr/bin/env bash
# fake deploy.sh — smoke-test call removed; filename only in comment + variable
# bash "${APP_DIR}/deploy/smoke-test-gomemlimit.sh"   <-- intentionally removed
SMOKE_SCRIPT="${APP_DIR}/deploy/smoke-test-gomemlimit.sh"
systemctl restart aperod-api aperod-node
FAKE

    smoke_line=$(first_line_of "$tmp" 'bash.*smoke-test-gomemlimit\.sh')
    rm -f "$tmp"

    if [[ -z "$smoke_line" ]]; then
        pass_test "T25: self-check — deploy script missing the smoke-test call is correctly flagged (comment/variable reference does not pass T23/T24)"
    else
        fail_test "T25: self-check — mutated script (smoke-test call removed) was NOT flagged; T23/T24 pattern is too broad"
    fi
}

# =============================================================================
#  Makefile atomic-mv tests (T26–T28)
#
#  The Makefile must build each binary to a <name>.new temp file first and
#  then `mv -f` it into place.  This prevents "Text file busy" (ETXTBSY) when
#  someone runs `make build` while the service is still running — Go cannot
#  truncate/overwrite a mapped executable, but `mv` replaces the directory
#  entry (not the inode) so the running process is never disturbed.
#
#  T26. The build-node rule builds to aperod-node.new (not directly to
#       aperod-node), confirming that `go build` never writes to the live path.
#  T27. A `mv -f` line follows the .new build in the Makefile, confirming the
#       atomic promotion step is present.
#  T28. Self-check: a Makefile that builds directly to aperod-node (no .new)
#       is detected as a regression.
# =============================================================================
echo -e "\n${CYAN}Running Makefile atomic-mv tests (T26–T28)…${NC}\n"

MAKEFILE="${SCRIPT_DIR}/../blockchain/Makefile"
[[ -f "$MAKEFILE" ]] || { echo -e "${RED}[FAIL]${NC}  T26/T27/T28: Makefile not found: $MAKEFILE" >&2; FAIL=$((FAIL+3)); }

if [[ -f "$MAKEFILE" ]]; then

# ── T26: build-node compiles to a .new temp path (not directly to the final binary) ─
# The Makefile uses Make variables ($(GO), $(BINARY_NODE), etc.) rather than
# literal names, so we match `-o.*\.new` which appears on the `go build -o
# <name>.new` line regardless of how the variables expand.  This confirms that
# `go build -o` never writes directly to the live binary, preventing ETXTBSY.
{
    new_build_line=$(first_line_of "$MAKEFILE" '\-o.*\.new')
    if [[ -n "$new_build_line" ]]; then
        pass_test "T26: 'go build -o ... .new' temp-build target found in Makefile (line $new_build_line) — go build never clobbers the live binary"
    else
        fail_test "T26: no 'go build -o ... .new' target found in Makefile — build-node may write directly to the live binary path, risking ETXTBSY"
    fi
}

# ── T27: `mv -f` promotes the .new file atomically ────────────────────────────
# Checks for the mv -f line that atomically swaps .new into the final binary path.
# The Makefile uses Makefile variables, so we match `mv -f.*\.new`.
{
    mv_line=$(first_line_of "$MAKEFILE" 'mv -f.*\.new')
    if [[ -n "$mv_line" ]]; then
        pass_test "T27: 'mv -f ... .new' atomic-install step found in Makefile (line $mv_line)"
    else
        fail_test "T27: no 'mv -f ... .new' step found in Makefile — .new file is built but never promoted atomically; final binary may not be updated"
    fi
}

# ── T28: self-check — Makefile that builds directly to final binary is detected ─
# A fake Makefile that omits the .new intermediate (no `go build ... .new` line)
# must fail T26's pattern check.
{
    tmp=$(mktemp)
    cat > "$tmp" <<'FAKE'
build-node:
	mkdir -p build
	CGO_ENABLED=0 go build -ldflags="-s -w" -o build/aperod-node ./cmd/node
FAKE

    bad_line=$(first_line_of "$tmp" 'go build.*\.new')
    rm -f "$tmp"

    if [[ -z "$bad_line" ]]; then
        pass_test "T28: self-check — Makefile without .new step is correctly flagged as a regression"
    else
        fail_test "T28: self-check — Makefile without .new step was NOT flagged; T26 pattern may be too broad"
    fi
}

fi  # end [[ -f "$MAKEFILE" ]]

# =============================================================================
#  Atomic binary-swap live test (T29)
#
#  T26–T28 verify the Makefile *statically* (pattern matching).  T29 goes
#  further: it actually runs `make build-node` while a process backed by
#  build/aperod-node is alive, then asserts the process survived (no crash,
#  SIGSEGV, or SIGBUS) and the resulting binary has valid ELF magic bytes.
#
#  The test lives in blockchain/deploy/atomic_swap_test.go and is driven by
#  the Go testing framework.  It is skipped automatically when `make` is not
#  in PATH (e.g. minimal CI runners) or when the host is non-Linux.
#
#  T29. Concurrent `make build-node` does not crash a running aperod-node
#       process and leaves a valid ELF binary at build/aperod-node.
# =============================================================================
echo -e "\n${CYAN}Running atomic binary-swap live test (T29)…${NC}\n"

BLOCKCHAIN_DIR="${SCRIPT_DIR}/../blockchain"

# ── T29: atomic swap survives a concurrent make build ─────────────────────────
{
    if [[ ! -d "$BLOCKCHAIN_DIR" ]]; then
        fail_test "T29: blockchain/ directory not found at $BLOCKCHAIN_DIR — cannot run atomic-swap test"
    elif ! command -v go &>/dev/null; then
        # Not a failure — just note the skip so the summary stays clean.
        echo -e "${CYAN}[SKIP]${NC}  T29: 'go' not found in PATH — skipping atomic-swap live test"
    else
        tmpout="$(mktemp)"
        if (cd "$BLOCKCHAIN_DIR" && go test ./deploy/... -run TestAtomicBinarySwap -v -timeout 180s) 2>&1 | tee "$tmpout"; then
            # go test exits 0 for both PASS and SKIP.
            # Detect a skip by looking for the canonical "--- SKIP:" marker in output.
            if grep -qE '^\s*--- SKIP:' "$tmpout" 2>/dev/null; then
                echo -e "${CYAN}[SKIP]${NC}  T29: TestAtomicBinarySwap was skipped (unsupported platform or missing tool)"
            else
                pass_test "T29: TestAtomicBinarySwap — concurrent make build-node did not crash the running process; new binary is valid ELF"
            fi
        else
            fail_test "T29: TestAtomicBinarySwap FAILED — atomic binary swap may have corrupted the running process or produced an invalid binary"
        fi
        rm -f "$tmpout"
    fi
}

# =============================================================================
#  .gitignore source-exclusion tests (T30–T32)
#
#  Regression guard for the Aug 12 2026 incident: blockchain/.gitignore had a
#  bare `explorer-indexer` pattern (meant for a build binary) that also matched
#  the source directory cmd/explorer-indexer/ — the code built locally but was
#  never committed, so every server deploy failed with "directory not found".
#
#  T30. No source file (*.go under blockchain/, *.ts/*.tsx under artifacts/*/src)
#       present on disk is ignored by git.
#  T31. Self-check: the detection logic finds a deliberately ignored source
#       file in a scratch repository.
#  T32. Binary-name ignore patterns in blockchain/.gitignore are anchored
#       (leading `/`) so they cannot match same-named source directories.
# =============================================================================
echo -e "\n${CYAN}Running .gitignore source-exclusion tests (T30–T32)…${NC}\n"

REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ── T30: no ignored source files in the working tree ─────────────────────────
{
    if ! command -v git &>/dev/null || [[ ! -e "${REPO_ROOT}/.git" ]]; then
        echo -e "${CYAN}[SKIP]${NC}  T30: git or .git not available — skipping"
    else
        ignored_sources="$(cd "$REPO_ROOT" && git ls-files --others --ignored --exclude-standard \
            -- 'blockchain/*.go' 'blockchain/**/*.go' \
               'artifacts/*/src/**/*.ts' 'artifacts/*/src/**/*.tsx' \
               ':(exclude)blockchain/data/**' ':(exclude)blockchain/build/**' \
            2>/dev/null || true)"
        if [[ -z "$ignored_sources" ]]; then
            pass_test "T30: no source files are silently excluded by .gitignore"
        else
            fail_test "T30: source files exist on disk but are IGNORED by git (they will never reach the server):"
            echo "$ignored_sources" | sed 's/^/          /'
        fi
    fi
}

# ── T31: self-check — the detection logic catches an ignored source file ─────
{
    if ! command -v git &>/dev/null; then
        echo -e "${CYAN}[SKIP]${NC}  T31: git not available — skipping self-check"
    else
        scratch="$(mktemp -d)"
        (
            cd "$scratch"
            git init -q .
            printf 'explorer-indexer\n' > .gitignore   # the buggy unanchored pattern
            mkdir -p blockchain/cmd/explorer-indexer
            printf 'package main\n' > blockchain/cmd/explorer-indexer/main.go
        )
        detected="$(cd "$scratch" && git ls-files --others --ignored --exclude-standard \
            -- 'blockchain/*.go' 'blockchain/**/*.go' 2>/dev/null || true)"
        if [[ "$detected" == *"explorer-indexer/main.go"* ]]; then
            pass_test "T31: self-check — detection logic catches a source dir ignored by an unanchored pattern"
        else
            fail_test "T31: self-check FAILED — detection logic did not flag the ignored source file"
        fi
        rm -rf "$scratch"
    fi
}

# ── T32: binary ignore patterns in blockchain/.gitignore are anchored ─────────
{
    BC_GITIGNORE="${REPO_ROOT}/blockchain/.gitignore"
    if [[ ! -f "$BC_GITIGNORE" ]]; then
        echo -e "${CYAN}[SKIP]${NC}  T32: blockchain/.gitignore not found — skipping"
    else
        # A "binary-name" pattern is a bare word with no '/', no '*', no
        # leading '!' and no file extension — exactly the shape that also
        # matches a same-named source directory anywhere in the tree.
        unanchored="$(grep -vE '^\s*(#|$)' "$BC_GITIGNORE" \
            | grep -vE '/|\*|^!' \
            | grep -vE '\.[a-z0-9]+$' || true)"
        if [[ -z "$unanchored" ]]; then
            pass_test "T32: all binary-name patterns in blockchain/.gitignore are anchored or extension-scoped"
        else
            fail_test "T32: unanchored binary-name patterns in blockchain/.gitignore (add a leading '/'):"
            echo "$unanchored" | sed 's/^/          /'
        fi
    fi
}

# ── T33: root drop-in and canonical blockchain drop-in agree on GOMEMLIMIT ────
# deploy.sh / deploy-server.sh install deploy/systemd/aperod-node.service.d/
# gomemlimit.conf, while ensure-dropin.sh / verify-dropin.sh parse the value
# from blockchain/deploy/gomemlimit.conf (canonical). If the two files ever
# disagree, the primary deploy path installs one limit while node verification
# expects another — exactly the drift these checks exist to prevent.
{
    ROOT_DROPIN="${REPO_ROOT}/deploy/systemd/aperod-node.service.d/gomemlimit.conf"
    CANON_DROPIN="${REPO_ROOT}/blockchain/deploy/gomemlimit.conf"
    if [[ ! -f "$ROOT_DROPIN" || ! -f "$CANON_DROPIN" ]]; then
        fail_test "T33: drop-in file missing (root: $([[ -f $ROOT_DROPIN ]] && echo ok || echo MISSING), canonical: $([[ -f $CANON_DROPIN ]] && echo ok || echo MISSING))"
    else
        root_val="$(grep -oE 'GOMEMLIMIT=[0-9]+' "$ROOT_DROPIN" | tail -1 | cut -d= -f2 || true)"
        canon_val="$(grep -oE 'GOMEMLIMIT=[0-9]+' "$CANON_DROPIN" | tail -1 | cut -d= -f2 || true)"
        if [[ -n "$root_val" && -n "$canon_val" && "$root_val" == "$canon_val" ]]; then
            pass_test "T33: root drop-in and canonical blockchain/deploy/gomemlimit.conf agree (GOMEMLIMIT=$root_val)"
        else
            fail_test "T33: GOMEMLIMIT drift — root drop-in has '${root_val:-unparseable}', canonical has '${canon_val:-unparseable}'; update BOTH files together"
        fi
    fi
}

# =============================================================================
#  Env-var pre-flight guard tests (T34–T39)
#
#  aperod-api-deploy.sh (Step 0) checks that PORT, DATABASE_URL, and
#  SESSION_SECRET are all present before attempting a build or restart.
#  If the guard is silently removed, the service restarts into an incomplete
#  environment and enters the 8-hour crash-loop that the guard was designed
#  to prevent.
#
#  T34–T36 are static presence tests: they verify that each required variable
#          name appears in the REQUIRED_API_VARS array declaration, so a
#          typo or deletion is caught without running the script.
#  T37     checks that the guard block actually calls `exit 1` on failure.
#  T38     checks that the env-check script itself exists in the repo.
#  T39     runs deploy/test-env-check.sh — the runtime test that exercises
#          the guard for each missing var individually and asserts exit
#          non-zero with no `systemctl restart` call.
# =============================================================================
echo -e "\n${CYAN}Running env-var pre-flight guard tests (T34–T39) for aperod-api-deploy.sh…${NC}\n"

API_DEPLOY="${SCRIPT_DIR}/aperod-api-deploy.sh"
ENV_CHECK_SCRIPT="${SCRIPT_DIR}/test-env-check.sh"

if [[ ! -f "$API_DEPLOY" ]]; then
    echo -e "${RED}[FAIL]${NC}  T34–T39: aperod-api-deploy.sh not found: $API_DEPLOY" >&2
    FAIL=$((FAIL+6))
fi

if [[ -f "$API_DEPLOY" ]]; then

# ── T34: REQUIRED_API_VARS contains PORT ─────────────────────────────────────
{
    if grep -qE 'REQUIRED_API_VARS=\([^)]*"PORT"' "$API_DEPLOY"; then
        line=$(grep -n 'REQUIRED_API_VARS=' "$API_DEPLOY" | grep -v '^[0-9]*:[[:space:]]*#' | head -1 | cut -d: -f1)
        pass_test "T34: 'PORT' found in REQUIRED_API_VARS in aperod-api-deploy.sh (line $line)"
    else
        fail_test "T34: 'PORT' NOT found in REQUIRED_API_VARS in aperod-api-deploy.sh — guard will not check PORT"
    fi
}

# ── T35: REQUIRED_API_VARS contains DATABASE_URL ──────────────────────────────
{
    if grep -qE 'REQUIRED_API_VARS=\([^)]*"DATABASE_URL"' "$API_DEPLOY"; then
        pass_test "T35: 'DATABASE_URL' found in REQUIRED_API_VARS in aperod-api-deploy.sh"
    else
        fail_test "T35: 'DATABASE_URL' NOT found in REQUIRED_API_VARS in aperod-api-deploy.sh — guard will not check DATABASE_URL"
    fi
}

# ── T36: REQUIRED_API_VARS contains SESSION_SECRET ───────────────────────────
{
    if grep -qE 'REQUIRED_API_VARS=\([^)]*"SESSION_SECRET"' "$API_DEPLOY"; then
        pass_test "T36: 'SESSION_SECRET' found in REQUIRED_API_VARS in aperod-api-deploy.sh"
    else
        fail_test "T36: 'SESSION_SECRET' NOT found in REQUIRED_API_VARS in aperod-api-deploy.sh — guard will not check SESSION_SECRET"
    fi
}

# ── T37: the guard block calls `exit 1` when vars are missing ─────────────────
# We match a non-comment `exit 1` that appears after the REQUIRED_API_VARS
# declaration, confirming the guard actually aborts the script.
{
    required_line=$(first_line_of "$API_DEPLOY" 'REQUIRED_API_VARS=')
    exit1_line=$(first_line_of "$API_DEPLOY" 'exit 1')

    if [[ -z "$required_line" ]]; then
        fail_test "T37: REQUIRED_API_VARS declaration not found — cannot verify guard exit"
    elif [[ -z "$exit1_line" ]]; then
        fail_test "T37: 'exit 1' not found in aperod-api-deploy.sh — env-var guard has no abort path"
    elif [[ "$exit1_line" -gt "$required_line" ]]; then
        pass_test "T37: 'exit 1' (line $exit1_line) appears after REQUIRED_API_VARS declaration (line $required_line) — guard has an abort path"
    else
        fail_test "T37: 'exit 1' (line $exit1_line) appears BEFORE REQUIRED_API_VARS (line $required_line) — guard block may not contain the exit"
    fi
}

fi  # end [[ -f "$API_DEPLOY" ]]

# ── T38: deploy/test-env-check.sh exists in the repo ─────────────────────────
{
    if [[ -f "$ENV_CHECK_SCRIPT" ]]; then
        pass_test "T38: 'deploy/test-env-check.sh' exists in the repo"
    else
        fail_test "T38: 'deploy/test-env-check.sh' NOT found at $ENV_CHECK_SCRIPT — env-check runtime tests are missing"
    fi
}

# ── T39: run deploy/test-env-check.sh — runtime guard verification ────────────
# Exercises the actual aperod-api-deploy.sh guard for each of the three
# required vars (PORT, DATABASE_URL, SESSION_SECRET) individually.
# Each sub-test asserts exit non-zero and no `systemctl restart` call.
{
    if [[ ! -f "$ENV_CHECK_SCRIPT" ]]; then
        fail_test "T39: deploy/test-env-check.sh not found — cannot run runtime guard tests"
    else
        tmpout="$(mktemp)"
        if bash "$ENV_CHECK_SCRIPT" > "$tmpout" 2>&1; then
            pass_test "T39: deploy/test-env-check.sh — all env-var guard runtime tests passed (PORT, DATABASE_URL, SESSION_SECRET each tested individually)"
        else
            fail_test "T39: deploy/test-env-check.sh FAILED — env-var pre-flight guard may be broken:"
            # Show the failure output indented for readability
            sed 's/^/          /' "$tmpout" >&2
        fi
        rm -f "$tmpout"
    fi
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}/${TOTAL} passed${NC}${FAIL:+, }${FAIL:+${RED}${FAIL} failed${NC}}"

if [[ "$FAIL" -eq 0 ]]; then
    echo -e "${GREEN}All deploy-order tests passed.${NC}"
    exit 0
else
    echo -e "${RED}${FAIL} test(s) failed.${NC}" >&2
    exit 1
fi
