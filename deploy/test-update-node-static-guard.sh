#!/usr/bin/env bash
# =============================================================================
#  test-update-node-static-guard.sh
#
#  Verifies that the static-link guard (Step 2b) in update-node.sh:
#    G1. Fires (exits non-zero) when BINARY_SRC is a dynamically-linked binary
#        (e.g. /bin/ls), and emits the expected error strings on stderr.
#    G2. Does NOT fire (exits 0 from the guard section) when ldd reports
#        "not a dynamic executable" — the happy path must pass through.
#    G3. The guard section is syntactically present in update-node.sh so that
#        a future rename/refactor cannot silently remove it.
#    G4. With ldd hidden, readelf detects PT_INTERP in a dynamic ELF and aborts.
#    G5. With ldd hidden, readelf output without INTERP passes the guard.
#
#  Design
#  ------
#  The test extracts the Step 2b guard block verbatim from update-node.sh via
#  awk (same approach used by test-update-node-key-preflight.sh) and exercises
#  it in an isolated subshell with:
#    - BINARY_SRC overridden to a known dynamic/static binary
#    - send_telegram_alert() stubbed to a no-op (no real HTTP call)
#    - For G2, ldd injected via PATH to emit "not a dynamic executable"
#    - For G4/G5, a minimal PATH exposes readelf but deliberately omits ldd
#
#  Skip conditions
#  ---------------
#    • systemctl not found in PATH — test environment cannot run the guard
#      meaningfully (the guard itself calls ldd/readelf, but update-node.sh
#      calls systemctl in surrounding steps; skipping here keeps CI green on
#      developer machines and containers without systemd).
#    • Both ldd and readelf absent — the guard would emit only a warning and
#      never abort; the test would trivially pass for the wrong reason.
#    • No known dynamic binary found — cannot exercise the negative case.
#
#  Usage (from anywhere in the monorepo):
#    bash blockchain/deploy/test-update-node-static-guard.sh
#
#  Exit codes:
#    0 — all assertions passed (or skip condition met)
#    1 — one or more assertions failed
# =============================================================================
set -uo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_NODE="${DEPLOY_DIR}/update-node.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0

pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$(( PASS + 1 )); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$(( FAIL + 1 )); }
skip_all()  { echo -e "${YELLOW}[SKIP]${NC}  $1"; exit 0; }

echo ""
echo "========================================"
echo " Static-link guard tests — update-node.sh Step 2b"
echo "========================================"

# ── Prerequisite: update-node.sh must exist ──────────────────────────────────
if [[ ! -f "${UPDATE_NODE}" ]]; then
  echo "ERROR: ${UPDATE_NODE} not found — cannot run tests." >&2
  exit 1
fi

# ── Skip condition: neither ldd nor readelf available ────────────────────────
# The guard emits only a "warn" message and never sets _binary_is_dynamic=true
# when both tools are absent — testing the guard output would be meaningless.
_has_ldd=false
_has_readelf=false
command -v ldd     > /dev/null 2>&1 && _has_ldd=true
command -v readelf > /dev/null 2>&1 && _has_readelf=true

if [[ "${_has_ldd}" == "false" && "${_has_readelf}" == "false" ]]; then
  skip_all "Neither ldd nor readelf found — guard would only warn, not abort; skipping"
fi

# ── Locate a known dynamically-linked system binary ──────────────────────────
_DYN_BIN=""
for _candidate in /bin/ls /usr/bin/ls /bin/cat /usr/bin/cat /bin/bash /usr/bin/bash; do
  if [[ -f "${_candidate}" ]]; then
    if [[ "${_has_ldd}" == "true" ]]; then
      _ldd_probe=$(ldd "${_candidate}" 2>&1 || true)
      if ! echo "${_ldd_probe}" | grep -q "not a dynamic executable"; then
        _DYN_BIN="${_candidate}"
        break
      fi
    elif [[ "${_has_readelf}" == "true" ]]; then
      if readelf -l "${_candidate}" 2>/dev/null | grep -q 'INTERP'; then
        _DYN_BIN="${_candidate}"
        break
      fi
    fi
  fi
done

if [[ -z "${_DYN_BIN}" ]]; then
  skip_all "Could not find a known dynamically-linked system binary — skipping"
fi
echo "  Using dynamic binary: ${_DYN_BIN}"

if [[ "${_has_readelf}" != "true" ]]; then
  echo "ERROR: readelf is required to test the musl/Alpine fallback." >&2
  exit 1
fi

# ── Extract the Step 2b guard block from update-node.sh ─────────────────────
#
# The block starts at the "# Step 2b:" comment and ends just before the
# "# Step 2c:" comment.  awk prints the range inclusive of both anchors but
# the trailing "# Step 2c:" line itself is excluded so it does not interfere
# with the subsequent preflight code.  If either anchor is missing the
# extraction produces an empty string (caught by test G3 below).
GUARD_SRC="$(awk '/# Step 2b:/,/# Step 2c:/ { if (/# Step 2c:/) exit; print }' "${UPDATE_NODE}")"

# ── G3: presence check — guard section must be found ─────────────────────────
echo ""
echo "── G3: guard section present in update-node.sh ──────────────────────────"

if [[ -n "${GUARD_SRC}" ]] && echo "${GUARD_SRC}" | grep -q '_binary_is_dynamic'; then
  pass_test "G3a: Step 2b guard block is present in update-node.sh"
else
  fail_test "G3a: Step 2b guard block NOT found in update-node.sh — was it removed or renamed?"
fi

if echo "${GUARD_SRC}" | grep -q 'Static-link check FAILED'; then
  pass_test "G3b: guard block contains the expected error message string"
else
  fail_test "G3b: 'Static-link check FAILED' not found in guard block — message may have changed"
fi

if echo "${GUARD_SRC}" | grep -q 'exit 1'; then
  pass_test "G3c: guard block contains 'exit 1'"
else
  fail_test "G3c: 'exit 1' not found in guard block — guard may not abort correctly"
fi

# ── G1: dynamic binary → guard must fire (exit non-zero + error on stderr) ───
echo ""
echo "── G1: dynamic binary triggers the guard ────────────────────────────────"

_G1_OUT="$(mktemp)"
_G1_ERR="$(mktemp)"
_G1_RC=0

bash > "${_G1_OUT}" 2>"${_G1_ERR}" <<SUBSHELL || _G1_RC=$?
set -uo pipefail

# Stub send_telegram_alert — no real HTTP call in tests.
send_telegram_alert() { :; }

# Point the guard at the known dynamic binary.
BINARY_SRC="${_DYN_BIN}"

# Run the extracted guard section verbatim.
${GUARD_SRC}
SUBSHELL

if [[ "${_G1_RC}" -ne 0 ]]; then
  pass_test "G1a: guard exited non-zero (rc=${_G1_RC}) for dynamic binary ${_DYN_BIN}"
else
  fail_test "G1a: guard exited 0 for dynamic binary — guard did not fire"
fi

if grep -q "Static-link check FAILED" "${_G1_ERR}"; then
  pass_test "G1b: stderr contains 'Static-link check FAILED'"
else
  fail_test "G1b: expected 'Static-link check FAILED' on stderr — got: $(head -5 "${_G1_ERR}")"
fi

if grep -q "NOT stopped\|not stopped\|still running" "${_G1_ERR}"; then
  pass_test "G1c: stderr confirms the service was NOT stopped"
else
  fail_test "G1c: expected 'NOT stopped' / 'still running' on stderr — got: $(head -5 "${_G1_ERR}")"
fi

rm -f "${_G1_OUT}" "${_G1_ERR}"

# ── G2: mock-static binary → guard must NOT fire (exit 0) ────────────────────
#
# Inject a fake ldd (via a temp bin/ dir prepended to PATH) that always outputs
# "not a dynamic executable", simulating a correctly built static binary.
# The guard must set _binary_is_dynamic=false and not call exit 1.
echo ""
echo "── G2: mock-static binary passes the guard ──────────────────────────────"

_MOCK_BIN="$(mktemp -d)"
_G2_OUT="$(mktemp)"
_G2_ERR="$(mktemp)"
_G2_RC=0

# Mock ldd: always reports the binary is static.
cat > "${_MOCK_BIN}/ldd" <<'MOCKLDD'
#!/usr/bin/env bash
echo "	not a dynamic executable"
exit 0
MOCKLDD
chmod +x "${_MOCK_BIN}/ldd"

# Mock readelf: should not be reached, but just in case — report no INTERP.
cat > "${_MOCK_BIN}/readelf" <<'MOCKELF'
#!/usr/bin/env bash
echo "no interp header"
exit 0
MOCKELF
chmod +x "${_MOCK_BIN}/readelf"

bash > "${_G2_OUT}" 2>"${_G2_ERR}" <<SUBSHELL || _G2_RC=$?
set -uo pipefail

# Prepend the mock bin so our fake ldd is found first.
export PATH="${_MOCK_BIN}:\${PATH}"

# Stub send_telegram_alert.
send_telegram_alert() { :; }

# Any existing file works — the mock ldd ignores the argument.
BINARY_SRC="${_DYN_BIN}"

# Run the extracted guard section verbatim.
${GUARD_SRC}
SUBSHELL

if [[ "${_G2_RC}" -eq 0 ]]; then
  pass_test "G2a: guard exited 0 for mock-static binary (happy path passed through)"
else
  fail_test "G2a: guard exited ${_G2_RC} for mock-static binary — happy path incorrectly aborted"
fi

if grep -q "statically linked" "${_G2_OUT}"; then
  pass_test "G2b: stdout confirms 'statically linked'"
else
  fail_test "G2b: expected 'statically linked' on stdout — got: $(head -5 "${_G2_OUT}")"
fi

if ! grep -q "Static-link check FAILED" "${_G2_ERR}"; then
  pass_test "G2c: no failure message on stderr (guard did not fire)"
else
  fail_test "G2c: 'Static-link check FAILED' unexpectedly on stderr for mock-static binary"
fi

rm -f "${_G2_OUT}" "${_G2_ERR}"
rm -rf "${_MOCK_BIN}"

# ── G4/G5: hide ldd and exercise the readelf fallback ────────────────────────
#
# A private PATH exposes only the helper tools the extracted guard requires.
# In particular, ldd is deliberately absent even if the host has it installed.
echo ""
echo "── G4/G5: readelf fallback when ldd is absent ───────────────────────────"

_MOCK_NO_LDD_BIN="$(mktemp -d)"
_READ_ELF_BIN="$(command -v readelf)"
for _tool in bash env grep touch; do
  _tool_path="$(command -v "${_tool}")"
  if [[ -z "${_tool_path}" ]] || ! ln -s "${_tool_path}" "${_MOCK_NO_LDD_BIN}/${_tool}"; then
    echo "ERROR: cannot prepare isolated PATH for readelf fallback (${_tool})." >&2
    rm -rf "${_MOCK_NO_LDD_BIN}"
    exit 1
  fi
done
if ! ln -s "${_READ_ELF_BIN}" "${_MOCK_NO_LDD_BIN}/readelf"; then
  echo "ERROR: cannot expose readelf in the isolated PATH." >&2
  rm -rf "${_MOCK_NO_LDD_BIN}"
  exit 1
fi

# G4: use the actual readelf tool on the known dynamic ELF. It must find
# PT_INTERP and reject the candidate before the update script stops the service.
_G4_OUT="$(mktemp)"
_G4_ERR="$(mktemp)"
_G4_RC=0
bash > "${_G4_OUT}" 2>"${_G4_ERR}" <<SUBSHELL || _G4_RC=$?
set -uo pipefail
export PATH="${_MOCK_NO_LDD_BIN}"
if command -v ldd > /dev/null 2>&1; then
  echo "ldd unexpectedly present in fallback PATH" >&2
  exit 99
fi
send_telegram_alert() { :; }
BINARY_SRC="${_DYN_BIN}"
${GUARD_SRC}
SUBSHELL

if [[ "${_G4_RC}" -eq 1 ]]; then
  pass_test "G4a: readelf fallback exited 1 with ldd absent"
else
  fail_test "G4a: expected exit 1 for dynamic ELF, got rc=${_G4_RC}"
fi
if grep -q "readelf: PT_INTERP segment found" "${_G4_ERR}" \
    && grep -q "Static-link check FAILED" "${_G4_ERR}"; then
  pass_test "G4b: stderr confirms readelf found PT_INTERP and the guard failed"
else
  fail_test "G4b: expected PT_INTERP and static-link failure — got: $(head -5 "${_G4_ERR}")"
fi
if grep -q "NOT stopped\|not stopped\|still running" "${_G4_ERR}"; then
  pass_test "G4c: stderr confirms the service was NOT stopped"
else
  fail_test "G4c: expected service-not-stopped message — got: $(head -5 "${_G4_ERR}")"
fi
rm -f "${_G4_OUT}" "${_G4_ERR}"

# G5: mock readelf with output that contains no INTERP marker. The marker file
# proves the fallback executable (not ldd) actually ran in this isolated PATH.
_G5_MARKER="${_MOCK_NO_LDD_BIN}/readelf-was-called"
rm -f "${_MOCK_NO_LDD_BIN}/readelf"
cat > "${_MOCK_NO_LDD_BIN}/readelf" <<'MOCKREADELF'
#!/usr/bin/env bash
echo "no program interpreter header"
touch "${G5_MARKER}"
MOCKREADELF
chmod +x "${_MOCK_NO_LDD_BIN}/readelf"
_G5_OUT="$(mktemp)"
_G5_ERR="$(mktemp)"
_G5_RC=0
bash > "${_G5_OUT}" 2>"${_G5_ERR}" <<SUBSHELL || _G5_RC=$?
set -uo pipefail
export PATH="${_MOCK_NO_LDD_BIN}"
export G5_MARKER="${_G5_MARKER}"
if command -v ldd > /dev/null 2>&1; then
  echo "ldd unexpectedly present in fallback PATH" >&2
  exit 99
fi
send_telegram_alert() { :; }
BINARY_SRC="${_DYN_BIN}"
${GUARD_SRC}
SUBSHELL

if [[ "${_G5_RC}" -eq 0 ]]; then
  pass_test "G5a: no-INTERP fallback exited 0 with ldd absent"
else
  fail_test "G5a: readelf fallback exited ${_G5_RC} for output without INTERP"
fi
if [[ -f "${_G5_MARKER}" ]] && grep -q "no PT_INTERP segment" "${_G5_OUT}"; then
  pass_test "G5b: mock readelf was called and the guard accepted no INTERP"
else
  fail_test "G5b: mock readelf did not run or guard did not report a static binary"
fi
if ! grep -q "Static-link check FAILED" "${_G5_ERR}"; then
  pass_test "G5c: stderr contains no static-link failure"
else
  fail_test "G5c: static-link failure was unexpected on the no-INTERP path"
fi
rm -f "${_G5_OUT}" "${_G5_ERR}"
rm -rf "${_MOCK_NO_LDD_BIN}"

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "========================================"
TOTAL=$(( PASS + FAIL ))
echo "Results: ${PASS}/${TOTAL} passed"
if [[ "${FAIL}" -gt 0 ]]; then
  echo -e "${RED}${FAIL} test(s) FAILED${NC}"
  exit 1
fi
echo -e "${GREEN}All tests passed.${NC}"
exit 0
