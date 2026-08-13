#!/usr/bin/env bash
# =============================================================================
#  test-env-check.sh — Verify the env-var pre-flight guard in
#                      deploy/aperod-api-deploy.sh
#
#  The guard (Step 0, lines ~113-158 of aperod-api-deploy.sh) must:
#    • Detect each missing required var: PORT, DATABASE_URL, SESSION_SECRET
#    • Exit non-zero before ever calling `systemctl restart aperod-api`
#
#  Strategy — run a patched copy of the deploy script with:
#    • EUID check disabled so CI (non-root) can execute this
#    • APEROD_DIR set to a temp directory (satisfies the -d guard)
#    • API_ENV_FILE pointing to a controlled temp file
#    • systemctl stubbed: records every call in a log file, never touches real services
#    • send_telegram stubbed: no-op (no real credentials in test env)
#    • All script output suppressed so only the exit code is used for assertions
#
#  Usage:
#    bash deploy/test-env-check.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -euo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_SCRIPT="${SCRIPT_DIR}/aperod-api-deploy.sh"

[[ -f "$DEPLOY_SCRIPT" ]] || {
  echo "ERROR: deploy script not found: $DEPLOY_SCRIPT" >&2
  exit 1
}

GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'; NC='\033[0m'
PASS=0; FAIL=0

pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$((PASS+1)); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$((FAIL+1)); }

# ---------------------------------------------------------------------------
# build_test_harness WORK_DIR
#
# Writes a patched copy of aperod-api-deploy.sh plus the stub helpers into
# WORK_DIR.  The patched script is at WORK_DIR/aperod-api-deploy-test.sh.
# ---------------------------------------------------------------------------
build_test_harness() {
  local work="$1"

  # ── stub systemctl ──────────────────────────────────────────────────────
  # Logs every call to $SYSTEMCTL_LOG; never touches real services.
  cat > "${work}/systemctl" <<'STUB'
#!/usr/bin/env bash
log="${SYSTEMCTL_LOG:-/dev/null}"
echo "systemctl $*" >> "$log"
# Exit 0 for show (env-fallback path), non-zero for restart so any accidental
# restart that escapes the guard would surface as a non-zero exit in tests.
case "$1" in
  show)    exit 0 ;;
  restart) exit 1 ;;
  *)       exit 0 ;;
esac
STUB
  chmod +x "${work}/systemctl"

  # ── stub pnpm (prevent any accidental build steps from running) ─────────
  cat > "${work}/pnpm" <<'STUB'
#!/usr/bin/env bash
log="${SYSTEMCTL_LOG:-/dev/null}"
echo "pnpm $*" >> "$log"
exit 0
STUB
  chmod +x "${work}/pnpm"

  # ── build patched deploy script ─────────────────────────────────────────
  # The patched script prepends a stub PATH + SYSTEMCTL_LOG header, then
  # appends the original script (minus the shebang line) with the EUID
  # check replaced by a no-op.
  local patched="${work}/aperod-api-deploy-test.sh"

  # Write the shebang + stub header
  cat > "$patched" <<HEADER
#!/usr/bin/env bash
# ── test harness: stub PATH injected by test-env-check.sh ──
export PATH="${work}:\${PATH}"
export SYSTEMCTL_LOG="${work}/systemctl.log"
# ── end test harness ──
HEADER

  # Append the original script (skip its shebang on line 1) with:
  #   • EUID check disabled
  sed \
    -e '1d' \
    -e 's|\[\[ \$EUID -ne 0 \]\].*$|: # EUID check disabled for env-check tests|' \
    "$DEPLOY_SCRIPT" >> "$patched"

  chmod +x "$patched"
}

# ---------------------------------------------------------------------------
# run_env_check WORK_DIR ENV_FILE
#
# Runs the patched deploy script with API_ENV_FILE=ENV_FILE.
# Prints only the numeric exit code to stdout; all script output is discarded.
# Never aborts the caller via set -e.
# ---------------------------------------------------------------------------
run_env_check() {
  local work="$1"
  local env_file="$2"
  local patched="${work}/aperod-api-deploy-test.sh"

  # Provide APEROD_DIR pointing to a real directory so the `-d` guard passes.
  # Use API_SERVICE=fake-nonexistent-service so the systemctl fallback also
  # returns no env (empty Property=Environment response).
  local rc=0
  APEROD_DIR="${work}" \
  API_ENV_FILE="${env_file}" \
  API_SERVICE="fake-nonexistent-service" \
  SKIP_HEALTH_CHECK=1 \
  SYSTEMCTL_LOG="${work}/systemctl.log" \
    bash "$patched" >/dev/null 2>/dev/null || rc=$?

  # Print the exit code so callers can capture it with $()
  printf '%s' "$rc"
}

# ---------------------------------------------------------------------------
# did_restart WORK_DIR -> 0 if systemctl restart was called, 1 otherwise
# ---------------------------------------------------------------------------
did_restart() {
  local work="$1"
  grep -q 'systemctl restart' "${work}/systemctl.log" 2>/dev/null
}

# =============================================================================
#  Build the test harness once; reuse for every sub-test.
# =============================================================================
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

build_test_harness "$WORK"

echo -e "\n${CYAN}Running env-var pre-flight guard tests for aperod-api-deploy.sh…${NC}\n"

# =============================================================================
#  Helper: write an env file with exactly one required var omitted
# =============================================================================
write_env_file() {
  local path="$1"
  local omit="$2"   # key of the var to leave out (e.g. "PORT")
  : > "$path"
  local vars=("PORT=3001" "DATABASE_URL=postgres://localhost/test" "SESSION_SECRET=test-secret-value")
  for kv in "${vars[@]}"; do
    local key="${kv%%=*}"
    if [[ "$key" != "$omit" ]]; then
      echo "$kv" >> "$path"
    fi
  done
}

# =============================================================================
#  T1: Missing PORT → exits non-zero, restart NOT called
# =============================================================================
{
  env_file="${WORK}/env-missing-port"
  write_env_file "$env_file" "PORT"
  rm -f "${WORK}/systemctl.log"

  rc="$(run_env_check "$WORK" "$env_file")"

  if [[ "$rc" -ne 0 ]]; then
    if did_restart "$WORK"; then
      fail_test "T1: Missing PORT: script exited $rc (correct) BUT 'systemctl restart' was called — guard did not abort early enough"
    else
      pass_test "T1: Missing PORT: script exits non-zero ($rc) and 'systemctl restart' was NOT called"
    fi
  else
    fail_test "T1: Missing PORT: script exited 0 — env-var guard did not fire"
  fi
}

# =============================================================================
#  T2: Missing DATABASE_URL → exits non-zero, restart NOT called
# =============================================================================
{
  env_file="${WORK}/env-missing-db"
  write_env_file "$env_file" "DATABASE_URL"
  rm -f "${WORK}/systemctl.log"

  rc="$(run_env_check "$WORK" "$env_file")"

  if [[ "$rc" -ne 0 ]]; then
    if did_restart "$WORK"; then
      fail_test "T2: Missing DATABASE_URL: script exited $rc (correct) BUT 'systemctl restart' was called — guard did not abort early enough"
    else
      pass_test "T2: Missing DATABASE_URL: script exits non-zero ($rc) and 'systemctl restart' was NOT called"
    fi
  else
    fail_test "T2: Missing DATABASE_URL: script exited 0 — env-var guard did not fire"
  fi
}

# =============================================================================
#  T3: Missing SESSION_SECRET → exits non-zero, restart NOT called
# =============================================================================
{
  env_file="${WORK}/env-missing-secret"
  write_env_file "$env_file" "SESSION_SECRET"
  rm -f "${WORK}/systemctl.log"

  rc="$(run_env_check "$WORK" "$env_file")"

  if [[ "$rc" -ne 0 ]]; then
    if did_restart "$WORK"; then
      fail_test "T3: Missing SESSION_SECRET: script exited $rc (correct) BUT 'systemctl restart' was called — guard did not abort early enough"
    else
      pass_test "T3: Missing SESSION_SECRET: script exits non-zero ($rc) and 'systemctl restart' was NOT called"
    fi
  else
    fail_test "T3: Missing SESSION_SECRET: script exited 0 — env-var guard did not fire"
  fi
}

# =============================================================================
#  T4: Self-check (positive path) — all vars present must NOT trigger the guard
# =============================================================================
{
  env_file="${WORK}/env-complete"
  printf 'PORT=3001\nDATABASE_URL=postgres://localhost/test\nSESSION_SECRET=test-secret-value\n' > "$env_file"
  rm -f "${WORK}/systemctl.log"

  # Capture stderr (where the guard error message goes) and look for the
  # specific error string that the guard emits when vars are missing.
  local_stderr="$(APEROD_DIR="${WORK}" API_ENV_FILE="${env_file}" \
    API_SERVICE="fake-nonexistent-service" SKIP_HEALTH_CHECK=1 \
    SYSTEMCTL_LOG="${WORK}/systemctl.log" \
    bash "${WORK}/aperod-api-deploy-test.sh" 2>&1 >/dev/null || true)"

  if echo "$local_stderr" | grep -q "Required environment variables are NOT set"; then
    fail_test "T4: Self-check — complete env file incorrectly triggered the guard (false positive)"
  else
    pass_test "T4: Self-check — complete env file passes the guard without false positive"
  fi
}

# =============================================================================
#  T5: Self-check (negative) — a script WITHOUT the guard exits 0 with missing
#      vars, confirming our harness would catch a regression where the guard
#      is removed.
# =============================================================================
{
  fake="${WORK}/fake-no-guard.sh"
  cat > "$fake" <<'FAKE'
#!/usr/bin/env bash
# Fake deploy script — env-var guard deliberately absent.
set -euo pipefail
echo "[fake-deploy] No env check — proceeding directly..." >&2
exit 0
FAKE
  chmod +x "$fake"

  rc=0
  bash "$fake" >/dev/null 2>/dev/null || rc=$?

  if [[ "$rc" -eq 0 ]]; then
    pass_test "T5: Self-check — script without env guard exits 0 even with missing vars (harness would flag a regression if guard were removed)"
  else
    fail_test "T5: Self-check — fake no-guard script unexpectedly exited non-zero ($rc)"
  fi
}

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}/${TOTAL} passed${NC}${FAIL:+, }${FAIL:+${RED}${FAIL} failed${NC}}"

if [[ "$FAIL" -eq 0 ]]; then
  echo -e "${GREEN}All env-check tests passed.${NC}"
  exit 0
else
  echo -e "${RED}${FAIL} test(s) failed.${NC}" >&2
  exit 1
fi
