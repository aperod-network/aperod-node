#!/usr/bin/env bash
# =============================================================================
#  test-watchdog-set-interval.sh — unit tests for aperod-watchdog-set-interval.sh
#
#  Run directly:  bash test-watchdog-set-interval.sh
#  Or via Go:     go test ./deploy/... (TestWatchdogSetInterval)
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELPER="${SCRIPT_DIR}/aperod-watchdog-set-interval.sh"

PASS=0; FAIL=0

pass() { PASS=$((PASS+1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; }

# ── Helpers ───────────────────────────────────────────────────────────────────

# run_helper <env-file-content>
#   Runs the helper in test mode inside a temp directory.
#   Prints the path to the generated drop-in file (or empty string if missing).
run_helper() {
  local env_content="$1"
  local tmpdir
  tmpdir=$(mktemp -d)
  # Ensure cleanup even on error
  # shellcheck disable=SC2064
  trap "rm -rf '${tmpdir}'" RETURN

  local env_file="${tmpdir}/watchdog.env"
  local dropin_dir="${tmpdir}/timer.d"
  local dropin_file="${dropin_dir}/interval.conf"

  printf '%s\n' "${env_content}" > "${env_file}"

  # Provide a stub systemctl so the script doesn't need real systemd.
  local fake_bin="${tmpdir}/bin"
  mkdir -p "${fake_bin}"
  printf '#!/bin/bash\nexit 0\n' > "${fake_bin}/systemctl"
  chmod +x "${fake_bin}/systemctl"

  # Run helper in test mode (skips root check).
  _APEROD_TEST=1 \
  ENV_FILE="${env_file}" \
  DROPIN_DIR="${dropin_dir}" \
  PATH="${fake_bin}:${PATH}" \
    bash "${HELPER}" >/dev/null 2>&1 || true

  # Return drop-in contents (or empty if file missing).
  if [[ -f "${dropin_file}" ]]; then
    cat "${dropin_file}"
  fi
}

# extract_value <key> <content>  — return the LAST occurrence of key=value
extract_value() {
  local key="$1" content="$2"
  echo "${content}" | grep -E "^${key}=" | tail -1 | sed "s/^${key}=//"
}

# ── Tests ─────────────────────────────────────────────────────────────────────

echo "=== aperod-watchdog-set-interval tests ==="

# 1. Valid integer — written correctly into the drop-in
echo "--- Test 1: valid interval (30 s)"
out=$(run_helper "WATCHDOG_INTERVAL_SECS=30")
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "30" && "${active}" == "30" ]]; then
  pass "valid integer 30 → OnBootSec=30, OnUnitActiveSec=30"
else
  fail "valid integer 30 → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 2. Inline comment is stripped (primary documented example)
echo "--- Test 2: inline comment stripped (15   # faster detection for HA setups)"
out=$(run_helper "WATCHDOG_INTERVAL_SECS=15   # faster detection for HA setups")
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "15" && "${active}" == "15" ]]; then
  pass "inline comment stripped → 15"
else
  fail "inline comment stripped → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 3. Missing variable → default 60
echo "--- Test 3: missing WATCHDOG_INTERVAL_SECS → default 60"
out=$(run_helper "# no interval set here")
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "60" && "${active}" == "60" ]]; then
  pass "missing var → default 60"
else
  fail "missing var → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 4. Empty env file → default 60
echo "--- Test 4: empty env file → default 60"
out=$(run_helper "")
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "60" && "${active}" == "60" ]]; then
  pass "empty env file → default 60"
else
  fail "empty env file → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 5. Value below minimum (< 5) → falls back to 60
echo "--- Test 5: value below minimum (3) → fallback 60"
out=$(run_helper "WATCHDOG_INTERVAL_SECS=3")
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "60" && "${active}" == "60" ]]; then
  pass "value below minimum → fallback 60"
else
  fail "value below minimum → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 6. Non-integer value → falls back to 60
echo "--- Test 6: non-integer value ('fast') → fallback 60"
out=$(run_helper "WATCHDOG_INTERVAL_SECS=fast")
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "60" && "${active}" == "60" ]]; then
  pass "non-integer → fallback 60"
else
  fail "non-integer → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 7. Quoted value → parsed correctly
echo "--- Test 7: quoted value (\"45\") → 45"
out=$(run_helper 'WATCHDOG_INTERVAL_SECS="45"')
boot=$(extract_value "OnBootSec" "${out}")
active=$(extract_value "OnUnitActiveSec" "${out}")
if [[ "${boot}" == "45" && "${active}" == "45" ]]; then
  pass "quoted value \"45\" → 45"
else
  fail "quoted value → got OnBootSec='${boot}', OnUnitActiveSec='${active}'"
fi

# 8. Drop-in includes the reset lines (OnBootSec= / OnUnitActiveSec=) before new values
echo "--- Test 8: drop-in contains reset lines before new values"
out=$(run_helper "WATCHDOG_INTERVAL_SECS=20")
reset_boot=$(echo "${out}" | grep -c "^OnBootSec=$" || true)
reset_active=$(echo "${out}" | grep -c "^OnUnitActiveSec=$" || true)
if [[ "${reset_boot}" -ge 1 && "${reset_active}" -ge 1 ]]; then
  pass "drop-in contains reset lines (OnBootSec=, OnUnitActiveSec=)"
else
  fail "drop-in missing reset lines: reset_boot=${reset_boot}, reset_active=${reset_active}"
fi

# 9. systemctl called with daemon-reload and restart (via stub)
echo "--- Test 9: systemctl daemon-reload and restart are invoked"
tmpdir=$(mktemp -d)
trap "rm -rf '${tmpdir}'" EXIT
env_file="${tmpdir}/watchdog.env"
dropin_dir="${tmpdir}/timer.d"
log_file="${tmpdir}/systemctl.log"
fake_bin="${tmpdir}/bin"
mkdir -p "${fake_bin}"
cat > "${fake_bin}/systemctl" <<'STUB'
#!/bin/bash
echo "$*" >> "${SYSTEMCTL_LOG}"
exit 0
STUB
chmod +x "${fake_bin}/systemctl"
echo "WATCHDOG_INTERVAL_SECS=10" > "${env_file}"

SYSTEMCTL_LOG="${log_file}" \
_APEROD_TEST=1 \
ENV_FILE="${env_file}" \
DROPIN_DIR="${dropin_dir}" \
PATH="${fake_bin}:${PATH}" \
  bash "${HELPER}" >/dev/null 2>&1 || true

daemon_reload=$(grep -c "daemon-reload" "${log_file}" 2>/dev/null || echo 0)
timer_restart=$(grep -c "restart aperod-node-watchdog.timer" "${log_file}" 2>/dev/null || echo 0)
if [[ "${daemon_reload}" -ge 1 && "${timer_restart}" -ge 1 ]]; then
  pass "systemctl daemon-reload + restart both called"
else
  fail "systemctl calls missing: daemon-reload=${daemon_reload}, restart=${timer_restart}"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo "Results: ${PASS} passed, ${FAIL} failed"
if [[ ${FAIL} -ne 0 ]]; then
  exit 1
fi
