#!/usr/bin/env bash
# ============================================================
#  Runtime test: verify-dropin.sh failure stops join-network.sh
#  before Step 7 ("Шаг 7/7: Ожидаем готовности API").
#
#  All external dependencies (ssh, systemctl, rsync) are mocked
#  via a temporary bin/ directory prepended to PATH.  The real
#  verify-dropin.sh is replaced with a stub (exits 1 or 0) inside
#  a temporary copy of the script directory so that
#  ${BASH_SOURCE[0]} resolves correctly.
#
#  Test cases
#  ----------
#    TA1. join-network.sh exits non-zero when verify-dropin.sh exits 1
#    TA2. join-network.sh output does NOT contain "Шаг 7/7" when
#         verify-dropin.sh exits 1 (script stopped before Step 7)
#    TA3. join-network.sh reaches Step 7 when verify-dropin.sh exits 0
#         (happy path continues normally)
#    TA4. Self-check: a patched join-network.sh with the guard removed
#         DOES reach Step 7 even when verify-dropin.sh exits 1,
#         proving our TA2 detection is sound
#
#  Usage:
#    bash blockchain/deploy/test-verify-dropin-guard.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# ============================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
PASS=0; FAIL=0

pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$((PASS+1)); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$((FAIL+1)); }

echo -e "\n${CYAN}Running verify-dropin guard tests for join-network.sh…${NC}\n"

# ── Scratch directory ──────────────────────────────────────────────────────────
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# ── Mock bin directory ─────────────────────────────────────────────────────────
MOCK_BIN="${WORK}/bin"
mkdir -p "${MOCK_BIN}"

# mock: rsync — succeeds silently
cat > "${MOCK_BIN}/rsync" <<'SH'
#!/usr/bin/env bash
exit 0
SH

# mock: systemctl
#   is-active → exit 1 (node not active after stop — while-loop exits immediately)
#   all others → exit 0
cat > "${MOCK_BIN}/systemctl" <<'SH'
#!/usr/bin/env bash
for arg in "$@"; do
  [[ "$arg" == "is-active" ]] && exit 1
done
exit 0
SH

# mock: ssh
#   Step 7 health query (contains "network/stats") → return valid JSON so the
#   health loop exits on the first attempt.
#   All other calls (stop node, heredoc bash config, chown) → succeed silently.
#
#   NOTE: we intentionally do NOT drain stdin here.  Reading stdin unconditionally
#   blocks when ssh is called without a heredoc (e.g. the health-check call), and
#   bash handles the heredoc delivery without requiring the child to read it all.
cat > "${MOCK_BIN}/ssh" <<'SH'
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "$arg" == *"network/stats"* ]]; then
    echo '{"height":12345,"peer_count":2}'
    exit 0
  fi
done
echo "ok"
exit 0
SH

# mock: hostname — join-network.sh calls `hostname -I` to detect PRIMARY_IP
# (We also set PRIMARY_IP in the env, making this a belt-and-suspenders guard.)
cat > "${MOCK_BIN}/hostname" <<'SH'
#!/usr/bin/env bash
echo "192.0.2.1"
SH

# mock: fuser — exit 1 means no process currently has chain.db open.
cat > "${MOCK_BIN}/fuser" <<'SH'
#!/usr/bin/env bash
exit 1
SH

chmod +x "${MOCK_BIN}/rsync" \
         "${MOCK_BIN}/systemctl" \
         "${MOCK_BIN}/ssh" \
         "${MOCK_BIN}/hostname" \
         "${MOCK_BIN}/fuser"

# ── Fake data directories ──────────────────────────────────────────────────────
PRIMARY_DIR="${WORK}/primary"
SECONDARY_DIR="${WORK}/secondary"
mkdir -p "${PRIMARY_DIR}/chain.db" "${SECONDARY_DIR}"

# Minimal node.yaml so the bootnode-config step has a file to edit
NODE_YAML="${WORK}/node.yaml"
cat > "${NODE_YAML}" <<'YAML'
p2p:
  bootnodes: []
YAML

# ── Helper: build a fresh deploy-dir for one test run ─────────────────────────
# $1 = directory to create
# $2 = verify-dropin.sh exit code (0 or 1)
# $3 = (optional) remove-guard flag: pass "noguard" to strip the exit-on-failure
#      block so the script does NOT abort when verify-dropin.sh fails
make_deploy_dir() {
  local dir="$1" dropin_exit="$2" mode="${3:-}"
  mkdir -p "$dir"

  # Copy join-network.sh, patching HEALTH_MAX_ATTEMPTS and HEALTH_WAIT_SECS so
  # the health-wait loop is fast during tests (1 attempt, 0 second sleep).
  sed \
    -e 's/^HEALTH_MAX_ATTEMPTS=.*/HEALTH_MAX_ATTEMPTS=1/' \
    -e 's/^HEALTH_WAIT_SECS=.*/HEALTH_WAIT_SECS=0/' \
    "${SCRIPT_DIR}/join-network.sh" > "${dir}/join-network.sh"

  # If requested, neutralise the verify-dropin guard so the script continues
  # even when verify-dropin.sh exits 1.  We use python3 for a reliable
  # multi-line substitution instead of fragile multi-expression sed.
  if [[ "${mode}" == "noguard" ]]; then
    python3 - "${dir}/join-network.sh" <<'PY'
import sys, re, os
path = sys.argv[1]
content = open(path).read()
# Replace the guard block:
#   if ! bash "${_SCRIPT_DIR}/verify-dropin.sh" "${TARGET_IP}"; then
#     warn "..."
#     warn "..."
#     exit 1
#   fi
# with a call that ignores the exit code.
patched = re.sub(
    r'if ! bash "\$\{_SCRIPT_DIR\}/verify-dropin\.sh" "\$\{TARGET_IP\}";.*?fi\n',
    'bash "${_SCRIPT_DIR}/verify-dropin.sh" "${TARGET_IP}" || true\n',
    content,
    flags=re.DOTALL,
)
if patched == content:
    print("WARNING: guard pattern not found — patch may be ineffective", file=sys.stderr)
open(path, 'w').write(patched)
PY
  fi

  # Stub verify-dropin.sh with the requested exit code
  cat > "${dir}/verify-dropin.sh" <<STUB
#!/usr/bin/env bash
# test stub: exits ${dropin_exit}
exit ${dropin_exit}
STUB
  chmod +x "${dir}/join-network.sh" "${dir}/verify-dropin.sh"
}

# ── Shared environment for all test runs ───────────────────────────────────────
COMMON_ENV=(
  env
  PATH="${MOCK_BIN}:/usr/bin:/bin"
  PRIMARY_IP="192.0.2.1"
  PRIMARY_DATA_DIR="${PRIMARY_DIR}"
  SECONDARY_DATA_DIR="${SECONDARY_DIR}"
  SECONDARY_NODE_YAML="${NODE_YAML}"
  SECONDARY_NODE_CONFIG_SH="/nonexistent/node-config.sh"
)

TARGET_IP="1.2.3.4"

# ==============================================================================
#  TA1 & TA2 — verify-dropin.sh exits 1
#              join-network.sh must exit non-zero AND not reach "Шаг 7/7"
# ==============================================================================

DIR_FAIL="${WORK}/run_fail"
make_deploy_dir "${DIR_FAIL}" 1

OUT_FAIL="${WORK}/out_fail.txt"
EXIT_FAIL=0
"${COMMON_ENV[@]}" bash "${DIR_FAIL}/join-network.sh" "${TARGET_IP}" \
  > "${OUT_FAIL}" 2>&1 || EXIT_FAIL=$?

# ── TA1: exits non-zero ───────────────────────────────────────────────────────
if [[ "${EXIT_FAIL}" -ne 0 ]]; then
  pass_test "TA1: join-network.sh exits non-zero (${EXIT_FAIL}) when verify-dropin.sh exits 1"
else
  fail_test "TA1: join-network.sh exited 0 even though verify-dropin.sh failed — guard is missing or broken"
fi

# ── TA2: Step 7 was NOT reached ───────────────────────────────────────────────
if grep -qF "Шаг 7/7" "${OUT_FAIL}" 2>/dev/null; then
  fail_test "TA2: output contains 'Шаг 7/7' — script reached Step 7 despite verify-dropin.sh failing"
else
  pass_test "TA2: output does NOT contain 'Шаг 7/7' — script correctly stopped before Step 7"
fi

# ==============================================================================
#  TA3 — verify-dropin.sh exits 0
#         join-network.sh must reach Step 7 (happy path continues normally)
# ==============================================================================

DIR_PASS="${WORK}/run_pass"
make_deploy_dir "${DIR_PASS}" 0

OUT_PASS="${WORK}/out_pass.txt"
EXIT_PASS=0
"${COMMON_ENV[@]}" bash "${DIR_PASS}/join-network.sh" "${TARGET_IP}" \
  > "${OUT_PASS}" 2>&1 || EXIT_PASS=$?

if grep -qF "Шаг 7/7" "${OUT_PASS}" 2>/dev/null; then
  pass_test "TA3: join-network.sh reached Step 7 when verify-dropin.sh exits 0 (happy path continues)"
else
  fail_test "TA3: join-network.sh did NOT reach Step 7 even when verify-dropin.sh exits 0 — unexpected early exit (code ${EXIT_PASS})"
  echo "  Last 10 lines of output:" >&2
  tail -10 "${OUT_PASS}" | sed 's/^/    /' >&2
fi

# ==============================================================================
#  TA4 — Self-check: patched join-network.sh with guard removed DOES reach
#         Step 7 when verify-dropin.sh exits 1.
#         This proves the TA2 detection logic is sound: our check can reliably
#         tell the difference between "stopped before Step 7" and "reached Step 7".
# ==============================================================================

DIR_NOGUARD="${WORK}/run_noguard"
make_deploy_dir "${DIR_NOGUARD}" 1 "noguard"

OUT_NOGUARD="${WORK}/out_noguard.txt"
EXIT_NOGUARD=0
"${COMMON_ENV[@]}" bash "${DIR_NOGUARD}/join-network.sh" "${TARGET_IP}" \
  > "${OUT_NOGUARD}" 2>&1 || EXIT_NOGUARD=$?

if grep -qF "Шаг 7/7" "${OUT_NOGUARD}" 2>/dev/null; then
  pass_test "TA4: self-check — patched join-network.sh (guard removed) reaches Step 7 even when verify-dropin.sh exits 1, confirming TA2 detection is sound"
else
  fail_test "TA4: self-check — patched join-network.sh (guard removed) did NOT reach Step 7; the test environment may be broken (exit ${EXIT_NOGUARD})"
  echo "  Last 10 lines of output:" >&2
  tail -10 "${OUT_NOGUARD}" | sed 's/^/    /' >&2
fi

# ==============================================================================
#  Summary
# ==============================================================================
echo ""
echo "────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}/${TOTAL} passed${NC}${FAIL:+, }${FAIL:+${RED}${FAIL} failed${NC}}"

if [[ "${FAIL}" -eq 0 ]]; then
  echo -e "${GREEN}All verify-dropin guard tests passed.${NC}"
  exit 0
else
  echo -e "${RED}${FAIL} test(s) failed.${NC}" >&2
  exit 1
fi
