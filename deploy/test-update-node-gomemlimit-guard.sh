#!/usr/bin/env bash
# =============================================================================
# test-update-node-gomemlimit-guard.sh — fail-closed tests for Step 0d.
#
# Extract and execute the real Step 0d block rather than duplicating it.  The
# shared policy is deliberately exercised with hostile/missing meminfo, a
# low-memory relay, and an explicit operator override.  A policy failure must
# occur before a drop-in write or systemctl invocation (and therefore before
# update-node.sh can later stop the service).
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_SH="${SCRIPT_DIR}/update-node.sh"
POLICY_SH="${SCRIPT_DIR}/gomemlimit-policy.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0; FAIL=0
pass() { echo -e "${GREEN}  PASS${NC}  $*"; PASS=$((PASS + 1)); }
fail() { echo -e "${RED}  FAIL${NC}  $*"; FAIL=$((FAIL + 1)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

if [[ ! -f "${UPDATE_SH}" || ! -f "${POLICY_SH}" ]]; then
  echo -e "${RED}[ERR]${NC} update-node.sh or gomemlimit-policy.sh is missing" >&2
  exit 1
fi

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "${TMPDIR_TEST}"' EXIT

# Creates a harmless systemctl replacement that records every invocation.
make_fake_bin() {
  local log_file="$1" fake_dir
  fake_dir=$(mktemp -d "${TMPDIR_TEST}/fake-bin-XXXXXXXX")
  cat > "${fake_dir}/systemctl" <<STUB
#!/usr/bin/env bash
echo "systemctl \$*" >> "${log_file}"
STUB
  chmod +x "${fake_dir}/systemctl"
  echo "${fake_dir}"
}

# run_step0d_block MEMINFO DROPIN_DIR SYSTEMCTL_LOG [OVERRIDE]
# Extracts Step 0d from the production script and runs it in a child shell.
# DEPLOY_DIR and DROPIN_DIR are injected as supported test seams; no production
# paths, services, or network calls are used.
run_step0d_block() {
  local meminfo="$1" dropin_dir="$2" sc_log="$3" override="${4:-}"
  local fake_sc_dir block
  fake_sc_dir=$(make_fake_bin "${sc_log}")
  block=$(awk '
    /^GOMEMLIMIT_CONF=/ { found=1 }
    found && /^# Step 1:/ { exit }
    found { print }
  ' "${UPDATE_SH}")
  if [[ -z "${block}" ]]; then
    echo "[ERR] could not extract Step 0d from ${UPDATE_SH}" >&2
    return 1
  fi

  DEPLOY_DIR="${SCRIPT_DIR}" \
  DROPIN_DIR="${dropin_dir}" \
  SYSTEMCTL="${fake_sc_dir}/systemctl" \
  GOMEMLIMIT_MEMINFO="${meminfo}" \
  GOMEMLIMIT_BYTES="${override}" \
  PATH="${fake_sc_dir}:${PATH}" \
  bash -s <<RUNNER
set -uo pipefail
send_telegram_alert() { :; }
${block}
RUNNER
}

assert_failed_closed() {
  local test_name="$1" rc="$2" err="$3" sc_log="$4" dropin="$5"
  if [[ "${rc}" -ne 0 ]]; then
    pass "${test_name}: policy failure exits non-zero"
  else
    fail "${test_name}: policy failure exited 0"
  fi
  if grep -q "NOT stopped" "${err}" 2>/dev/null; then
    pass "${test_name}: stderr confirms service was NOT stopped"
  else
    fail "${test_name}: missing NOT-stopped safety message"
  fi
  if [[ ! -s "${sc_log}" ]]; then
    pass "${test_name}: systemctl was not called"
  else
    fail "${test_name}: systemctl was called unexpectedly ($(cat "${sc_log}"))"
  fi
  if [[ ! -f "${dropin}/gomemlimit.conf" ]]; then
    pass "${test_name}: no drop-in was written"
  else
    fail "${test_name}: drop-in was written despite failed policy"
  fi
}

# =============================================================================
# T1/T2: preserve the old absent/corrupt-source fail-closed scenarios, adapted
# to the host-memory source now used by gomemlimit-policy.sh.
# =============================================================================
section "T1: absent meminfo aborts before service handling"
T1_DROPIN=$(mktemp -d "${TMPDIR_TEST}/t1-XXXXXXXX")
T1_LOG="${TMPDIR_TEST}/t1-systemctl.log"; T1_ERR="${TMPDIR_TEST}/t1.err"; T1_RC=0
run_step0d_block "${TMPDIR_TEST}/missing-meminfo" "${T1_DROPIN}" "${T1_LOG}" \
  >"${TMPDIR_TEST}/t1.out" 2>"${T1_ERR}" || T1_RC=$?
assert_failed_closed "T1" "${T1_RC}" "${T1_ERR}" "${T1_LOG}" "${T1_DROPIN}"

section "T2: malformed MemTotal aborts before service handling"
T2_MEMINFO="${TMPDIR_TEST}/malformed-meminfo"
printf 'MemTotal: not-a-number kB\n' > "${T2_MEMINFO}"
T2_DROPIN=$(mktemp -d "${TMPDIR_TEST}/t2-XXXXXXXX")
T2_LOG="${TMPDIR_TEST}/t2-systemctl.log"; T2_ERR="${TMPDIR_TEST}/t2.err"; T2_RC=0
run_step0d_block "${T2_MEMINFO}" "${T2_DROPIN}" "${T2_LOG}" \
  >"${TMPDIR_TEST}/t2.out" 2>"${T2_ERR}" || T2_RC=$?
assert_failed_closed "T2" "${T2_RC}" "${T2_ERR}" "${T2_LOG}" "${T2_DROPIN}"

assert_dropin_and_reload() {
  local test_name="$1" rc="$2" dropin="$3" log="$4" expected="$5"
  local file="${dropin}/gomemlimit.conf" reloads
  if [[ "${rc}" -eq 0 ]]; then pass "${test_name}: guard exits 0"; else fail "${test_name}: guard exits ${rc}"; fi
  if [[ "$(cat "${file}" 2>/dev/null)" == "$(printf '[Service]\nEnvironment="GOMEMLIMIT=%s"' "${expected}")" ]]; then
    pass "${test_name}: writes GOMEMLIMIT=${expected} drop-in"
  else
    fail "${test_name}: drop-in content is wrong or absent"
  fi
  reloads=$(grep -c '^systemctl daemon-reload$' "${log}" 2>/dev/null || true)
  if [[ "${reloads}" -eq 1 ]]; then
    pass "${test_name}: daemon-reload called exactly once"
  else
    fail "${test_name}: expected one daemon-reload, got ${reloads}"
  fi
}

# =============================================================================
# T3: a 2 GiB relay must receive the 1.5 GiB floor, never the primary cap.
# =============================================================================
section "T3: low-memory relay receives host-aware floor"
T3_MEMINFO="${TMPDIR_TEST}/relay-meminfo"
printf 'MemTotal:       2097152 kB\n' > "${T3_MEMINFO}"
T3_DROPIN=$(mktemp -d "${TMPDIR_TEST}/t3-XXXXXXXX")
T3_LOG="${TMPDIR_TEST}/t3-systemctl.log"; T3_RC=0
run_step0d_block "${T3_MEMINFO}" "${T3_DROPIN}" "${T3_LOG}" >"${TMPDIR_TEST}/t3.out" 2>"${TMPDIR_TEST}/t3.err" || T3_RC=$?
assert_dropin_and_reload "T3" "${T3_RC}" "${T3_DROPIN}" "${T3_LOG}" 1610612736

# =============================================================================
# T4: an explicit valid override wins even on a low-memory host.
# =============================================================================
section "T4: explicit operator override is preserved"
T4_DROPIN=$(mktemp -d "${TMPDIR_TEST}/t4-XXXXXXXX")
T4_LOG="${TMPDIR_TEST}/t4-systemctl.log"; T4_RC=0; T4_OVERRIDE=2147483648
run_step0d_block "${T3_MEMINFO}" "${T4_DROPIN}" "${T4_LOG}" "${T4_OVERRIDE}" \
  >"${TMPDIR_TEST}/t4.out" 2>"${TMPDIR_TEST}/t4.err" || T4_RC=$?
assert_dropin_and_reload "T4" "${T4_RC}" "${T4_DROPIN}" "${T4_LOG}" "${T4_OVERRIDE}"

# =============================================================================
# T5: an unchanged drop-in remains idempotent and does not daemon-reload.
# =============================================================================
section "T5: matching drop-in is idempotent"
T5_DROPIN=$(mktemp -d "${TMPDIR_TEST}/t5-XXXXXXXX")
printf '[Service]\nEnvironment="GOMEMLIMIT=1610612736"' > "${T5_DROPIN}/gomemlimit.conf"
T5_LOG="${TMPDIR_TEST}/t5-systemctl.log"; T5_RC=0
run_step0d_block "${T3_MEMINFO}" "${T5_DROPIN}" "${T5_LOG}" >"${TMPDIR_TEST}/t5.out" 2>"${TMPDIR_TEST}/t5.err" || T5_RC=$?
if [[ "${T5_RC}" -eq 0 && ! -s "${T5_LOG}" ]]; then
  pass "T5: matching host-aware drop-in skips daemon-reload"
else
  fail "T5: matching drop-in unexpectedly failed or reloaded"
fi

# =============================================================================
# T6/T7: static completeness and pre-stop ordering guards.
# =============================================================================
section "T6: Step 0d still uses the shared fail-closed policy"
EXTRACTED=$(awk '/^GOMEMLIMIT_CONF=/ { found=1 } found && /^# Step 1:/ { exit } found { print }' "${UPDATE_SH}")
for sentinel in 'source "${DEPLOY_DIR}/gomemlimit-policy.sh"' 'gomemlimit_resolve' 'could not resolve host-aware GOMEMLIMIT policy' 'NOT stopped' 'exit 1'; do
  if grep -qF "${sentinel}" <<< "${EXTRACTED}"; then
    pass "T6: ${sentinel} is present"
  else
    fail "T6: ${sentinel} is missing"
  fi
done
if grep -q 'GOMEMLIMIT_CANONICAL' <<< "${EXTRACTED}"; then
  fail "T6: obsolete canonical-primary copy remains in Step 0d"
else
  pass "T6: Step 0d does not copy the canonical primary limit"
fi

section "T7: policy preflight precedes systemctl stop"
STEP0D_LINE=$(grep -n '^GOMEMLIMIT_CONF=' "${UPDATE_SH}" | head -1 | cut -d: -f1)
STOP_LINE=$(grep -n '^[^#]*systemctl stop' "${UPDATE_SH}" | head -1 | cut -d: -f1)
if [[ -n "${STEP0D_LINE}" && -n "${STOP_LINE}" && "${STEP0D_LINE}" -lt "${STOP_LINE}" ]]; then
  pass "T7: Step 0d precedes systemctl stop"
else
  fail "T7: could not prove Step 0d runs before systemctl stop"
fi

# Retain a negative control for the ordering assertion: a script with a stop
# before the policy block must be recognized as unsafe.
section "T8: ordering check detects a stop-before-policy regression"
FAKE_UPDATE="${TMPDIR_TEST}/fake-update-node.sh"
cat > "${FAKE_UPDATE}" <<'FAKE'
#!/usr/bin/env bash
systemctl stop aperod-node
GOMEMLIMIT_CONF="/etc/systemd/system/aperod-node.service.d/gomemlimit.conf"
# Step 1: Pull latest source
FAKE
FAKE_STEP0D_LINE=$(grep -n '^GOMEMLIMIT_CONF=' "${FAKE_UPDATE}" | head -1 | cut -d: -f1)
FAKE_STOP_LINE=$(grep -n '^[^#]*systemctl stop' "${FAKE_UPDATE}" | head -1 | cut -d: -f1)
if [[ -n "${FAKE_STEP0D_LINE}" && -n "${FAKE_STOP_LINE}" && "${FAKE_STEP0D_LINE}" -gt "${FAKE_STOP_LINE}" ]]; then
  pass "T8: stop-before-policy regression is detected"
else
  fail "T8: stop-before-policy regression was not detected"
fi

echo ""
echo "─────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ "${FAIL}" -eq 0 ]]; then
  echo -e "${GREEN}All ${TOTAL} tests passed.${NC}"
  exit 0
fi
echo -e "${RED}${FAIL} of ${TOTAL} tests FAILED.${NC}"
exit 1