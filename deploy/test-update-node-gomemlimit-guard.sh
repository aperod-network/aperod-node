#!/usr/bin/env bash
# =============================================================================
# test-update-node-gomemlimit-guard.sh — fail-closed update preflight tests.
#
# Extract and execute the real Step 0d block rather than duplicating it.  The
# shared GOMEMLIMIT policy is exercised with hostile/missing meminfo, a
# low-memory relay, and an explicit operator override. The restart-policy
# preflight is also executed verbatim against a systemctl stub to prove that
# refusal leaves a simulated running process and its data untouched.
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
# T3: a 2 GiB relay must receive 87.5% of RAM, never the primary cap.
# =============================================================================
section "T3: low-memory relay receives host-aware floor"
T3_MEMINFO="${TMPDIR_TEST}/relay-meminfo"
printf 'MemTotal:       2097152 kB\n' > "${T3_MEMINFO}"
T3_DROPIN=$(mktemp -d "${TMPDIR_TEST}/t3-XXXXXXXX")
T3_LOG="${TMPDIR_TEST}/t3-systemctl.log"; T3_RC=0
run_step0d_block "${T3_MEMINFO}" "${T3_DROPIN}" "${T3_LOG}" >"${TMPDIR_TEST}/t3.out" 2>"${TMPDIR_TEST}/t3.err" || T3_RC=$?
assert_dropin_and_reload "T3" "${T3_RC}" "${T3_DROPIN}" "${T3_LOG}" 1879048192

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
printf '[Service]\nEnvironment="GOMEMLIMIT=1879048192"' > "${T5_DROPIN}/gomemlimit.conf"
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
  pass "legacy and current GOMEMLIMIT policy scenarios remain green"
else
  echo -e "${RED}${FAIL} of ${TOTAL} tests FAILED.${NC}"
  exit 1
fi

# =============================================================================
# T9-T13: exercise the real restart-policy helper with a harmless systemctl
# implementation. No host service, unit file, or production data is touched.
# =============================================================================
section "T9-T13: restart-policy protection for legacy units"
RESTART_POLICY_BLOCK=$(awk '
  /^# BEGIN restart-policy preflight$/ { found=1; next }
  /^# END restart-policy preflight$/ { exit }
  found { print }
' "${UPDATE_SH}")
if [[ -z "${RESTART_POLICY_BLOCK}" ]]; then
  fail "T9: restart-policy helper block is missing"
else
  pass "T9: restart-policy helper is present"
fi

make_restart_policy_systemctl() {
  local fake_dir="$1"
  cat > "${fake_dir}/systemctl" <<'STUB'
#!/usr/bin/env bash
set -u
echo "systemctl $*" >> "${FAKE_SYSTEMCTL_LOG}"
case "$1" in
  daemon-reload)
    [[ "${FAKE_RELOAD_FAIL:-0}" != "1" ]]
    exit $?
    ;;
  show)
    [[ "${FAKE_SHOW_FAIL:-0}" != "1" ]] || exit 1
    case " $* " in
      *" -p RestartPreventExitStatus "*)
        if [[ "${FAKE_INITIAL_PREVENT:-}" == "78" ]] ||
           { [[ "${FAKE_IGNORE_DROPIN:-0}" != "1" ]] &&
             [[ -f "${DROPIN_DIR}/restart-policy.conf" ]] &&
             grep -q '^RestartPreventExitStatus=78$' "${DROPIN_DIR}/restart-policy.conf"; }; then
          echo 78
        else
          echo
        fi
        ;;
      *" -p Restart "*)
        echo "${FAKE_RESTART_MODE:-always}"
        ;;
      *)
        echo "unexpected systemctl show arguments: $*" >&2
        exit 2
        ;;
    esac
    ;;
  *)
    exit 0
    ;;
esac
STUB
  chmod +x "${fake_dir}/systemctl"
}

run_restart_policy_helper() {
  local dropin="$1" fake_bin="$2" log="$3"
  local initial_prevent="${4:-}" show_fail="${5:-0}" ignore_dropin="${6:-0}"
  DROPIN_DIR="${dropin}" \
  SERVICE_NAME="aperod-node" \
  SYSTEMCTL="${fake_bin}/systemctl" \
  FAKE_SYSTEMCTL_LOG="${log}" \
  FAKE_INITIAL_PREVENT="${initial_prevent}" \
  FAKE_SHOW_FAIL="${show_fail}" \
  FAKE_IGNORE_DROPIN="${ignore_dropin}" \
  bash -s <<RUNNER
set -uo pipefail
${RESTART_POLICY_BLOCK}
ensure_restart_policy
RUNNER
}

# T9: an old unit with an ordinary Restart policy gains an isolated, verified
# drop-in before any service stop. This is a simulated future update, not a
# change to the current production unit.
T9_DROPIN="${TMPDIR_TEST}/t9-dropin"; mkdir -p "${T9_DROPIN}"
T9_BIN="${TMPDIR_TEST}/t9-bin"; mkdir -p "${T9_BIN}"
T9_LOG="${TMPDIR_TEST}/t9-systemctl.log"; : > "${T9_LOG}"
make_restart_policy_systemctl "${T9_BIN}"
if run_restart_policy_helper "${T9_DROPIN}" "${T9_BIN}" "${T9_LOG}" \
  >"${TMPDIR_TEST}/t9.out" 2>"${TMPDIR_TEST}/t9.err"; then
  if [[ "$(cat "${T9_DROPIN}/restart-policy.conf" 2>/dev/null)" == $'[Service]\nRestartPreventExitStatus=78' ]]; then
    pass "T9: legacy unit receives the exact EX_CONFIG drop-in"
  else
    fail "T9: generated restart-policy drop-in is incorrect"
  fi
  if grep -q 'Installed and verified' "${TMPDIR_TEST}/t9.out" &&
     grep -q '^systemctl daemon-reload$' "${T9_LOG}"; then
    pass "T9: daemon-reload and effective policy verification succeeded"
  else
    fail "T9: policy was not reported and reloaded as verified"
  fi
  if ! grep -q 'systemctl stop' "${T9_LOG}"; then
    pass "T9: preflight did not stop or signal the service"
  else
    fail "T9: preflight unexpectedly stopped the service"
  fi
else
  fail "T9: a normal legacy unit could not be upgraded safely"
fi

# T10: an unreadable effective policy refuses preflight while preserving a
# live stand-in process and its data; no drop-in or service operation occurs.
section "T10: unreadable policy refusal preserves process and data"
T10_DROPIN="${TMPDIR_TEST}/t10-dropin"; mkdir -p "${T10_DROPIN}"
T10_BIN="${TMPDIR_TEST}/t10-bin"; mkdir -p "${T10_BIN}"
T10_LOG="${TMPDIR_TEST}/t10-systemctl.log"; : > "${T10_LOG}"
T10_DATA="${TMPDIR_TEST}/t10-chain-data"; printf 'canonical-chain-fixture\n' > "${T10_DATA}"
T10_HASH=$(sha256sum "${T10_DATA}" | cut -d' ' -f1)
make_restart_policy_systemctl "${T10_BIN}"
sleep 30 & T10_PID=$!
T10_RC=0
run_restart_policy_helper "${T10_DROPIN}" "${T10_BIN}" "${T10_LOG}" "" 1 \
  >"${TMPDIR_TEST}/t10.out" 2>"${TMPDIR_TEST}/t10.err" || T10_RC=$?
if [[ "${T10_RC}" -ne 0 ]] && kill -0 "${T10_PID}" 2>/dev/null; then
  pass "T10: policy-read failure aborts and the process stays alive"
else
  fail "T10: refusal did not preserve the simulated running process"
fi
if [[ "$(sha256sum "${T10_DATA}" | cut -d' ' -f1)" == "${T10_HASH}" &&
      ! -e "${T10_DROPIN}/restart-policy.conf" ]]; then
  pass "T10: chain-data fixture and unit files remain unchanged"
else
  fail "T10: refusal changed the data fixture or created a drop-in"
fi
if ! grep -q 'systemctl stop' "${T10_LOG}" &&
   grep -q 'NOT stopped' "${TMPDIR_TEST}/t10.err"; then
  pass "T10: refusal is explicit and no stop command was issued"
else
  fail "T10: refusal lacks a no-stop guarantee"
fi
kill "${T10_PID}" 2>/dev/null || true
wait "${T10_PID}" 2>/dev/null || true

# T11: a pre-existing operator-owned file is never overwritten when its
# effective policy is incomplete.
section "T11: conflicting operator drop-in is preserved"
T11_DROPIN="${TMPDIR_TEST}/t11-dropin"; mkdir -p "${T11_DROPIN}"
T11_BIN="${TMPDIR_TEST}/t11-bin"; mkdir -p "${T11_BIN}"
T11_LOG="${TMPDIR_TEST}/t11-systemctl.log"; : > "${T11_LOG}"
printf '[Service]\nRestart=on-failure\n# operator-owned\n' > "${T11_DROPIN}/restart-policy.conf"
cp "${T11_DROPIN}/restart-policy.conf" "${TMPDIR_TEST}/t11-original.conf"
make_restart_policy_systemctl "${T11_BIN}"
T11_RC=0
run_restart_policy_helper "${T11_DROPIN}" "${T11_BIN}" "${T11_LOG}" \
  >"${TMPDIR_TEST}/t11.out" 2>"${TMPDIR_TEST}/t11.err" || T11_RC=$?
if [[ "${T11_RC}" -ne 0 ]] && cmp -s "${T11_DROPIN}/restart-policy.conf" "${TMPDIR_TEST}/t11-original.conf"; then
  pass "T11: incomplete operator drop-in causes a safe refusal without overwrite"
else
  fail "T11: existing operator drop-in was not preserved on refusal"
fi

# T12: a systemd reload that fails to expose the requested effective value
# rolls back only the newly created file, leaving the node running.
section "T12: failed effective verification cleans up its own change"
T12_DROPIN="${TMPDIR_TEST}/t12-dropin"; mkdir -p "${T12_DROPIN}"
T12_BIN="${TMPDIR_TEST}/t12-bin"; mkdir -p "${T12_BIN}"
T12_LOG="${TMPDIR_TEST}/t12-systemctl.log"; : > "${T12_LOG}"
make_restart_policy_systemctl "${T12_BIN}"
T12_RC=0
run_restart_policy_helper "${T12_DROPIN}" "${T12_BIN}" "${T12_LOG}" "" 0 1 \
  >"${TMPDIR_TEST}/t12.out" 2>"${TMPDIR_TEST}/t12.err" || T12_RC=$?
if [[ "${T12_RC}" -ne 0 && ! -e "${T12_DROPIN}/restart-policy.conf" ]] &&
   [[ "$(grep -c '^systemctl daemon-reload$' "${T12_LOG}")" -eq 2 ]]; then
  pass "T12: failed verification removes its file and reloads the original policy"
else
  fail "T12: failed policy verification did not safely roll back"
fi

# T13: already-safe units remain unchanged and do not reload systemd.
section "T13: an already-protected unit is idempotent"
T13_DROPIN="${TMPDIR_TEST}/t13-dropin"; mkdir -p "${T13_DROPIN}"
T13_BIN="${TMPDIR_TEST}/t13-bin"; mkdir -p "${T13_BIN}"
T13_LOG="${TMPDIR_TEST}/t13-systemctl.log"; : > "${T13_LOG}"
make_restart_policy_systemctl "${T13_BIN}"
if run_restart_policy_helper "${T13_DROPIN}" "${T13_BIN}" "${T13_LOG}" 78 \
  >"${TMPDIR_TEST}/t13.out" 2>"${TMPDIR_TEST}/t13.err" &&
   ! grep -q '^systemctl daemon-reload$' "${T13_LOG}" &&
   ! grep -q 'systemctl stop' "${T13_LOG}" &&
   [[ ! -e "${T13_DROPIN}/restart-policy.conf" ]]; then
  pass "T13: existing exit-78 protection is verified without rewriting"
else
  fail "T13: already-protected unit was changed or failed verification"
fi

# T14: both entry points perform the policy check before any disruptive or
# unrelated upgrade step.
section "T14: update and upgrade entry points preflight first"
UPDATE_POLICY_CALL=$(grep -n '^ensure_restart_policy || exit 1$' "${UPDATE_SH}" | head -1 | cut -d: -f1)
SOURCE_PREPARE_LINE=$(grep -n '^node_source_prepare || exit 1$' "${UPDATE_SH}" | head -1 | cut -d: -f1)
UPDATE_STOP_LINE=$(grep -n '^[^#]*systemctl stop' "${UPDATE_SH}" | head -1 | cut -d: -f1)
UPGRADE_POLICY_CALL=$(grep -n -- '--check-restart-policy' "${SCRIPT_DIR}/upgrade-node.sh" | tail -1 | cut -d: -f1)
ENSURE_DROPIN_CALL=$(grep -n 'bash "${ENSURE_DROPIN_SH}"' "${SCRIPT_DIR}/upgrade-node.sh" | head -1 | cut -d: -f1)
if [[ -n "${UPDATE_POLICY_CALL}" && -n "${SOURCE_PREPARE_LINE}" &&
      "${UPDATE_POLICY_CALL}" -lt "${SOURCE_PREPARE_LINE}" &&
      "${UPDATE_POLICY_CALL}" -lt "${UPDATE_STOP_LINE}" ]]; then
  pass "T14: update-node verifies the restart policy before source preparation and stop"
else
  fail "T14: update-node restart-policy preflight is missing or too late"
fi
if [[ -n "${UPGRADE_POLICY_CALL}" && -n "${ENSURE_DROPIN_CALL}" &&
      "${UPGRADE_POLICY_CALL}" -lt "${ENSURE_DROPIN_CALL}" ]]; then
  pass "T14: upgrade-node preflights before memory-drop-in changes"
else
  fail "T14: upgrade-node restart-policy preflight is missing or too late"
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