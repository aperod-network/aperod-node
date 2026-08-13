#!/usr/bin/env bash
# =============================================================================
#  test-update-node-backup-syntax.sh
#
#  Verifies that the self-check guard added to _sync_backup_script() in
#  sync-backup-script.sh fires and exits non-zero when:
#    T1 — the repo copy of aperod_backup.sh is truncated (empty file)
#    T2 — the repo copy has a bash syntax error (truncated mid-function)
#    T3 — the repo copy is valid → _sync_backup_script succeeds (exit 0)
#
#  The test sources the real sync-backup-script.sh so any future change to
#  the guard logic is immediately exercised here.
#
#  No Docker required — all file operations happen in a temp directory that
#  is cleaned up on exit.
#
#  Run from anywhere in the monorepo:
#    bash blockchain/deploy/test-update-node-backup-syntax.sh
#
#  Exit codes:
#    0 — all assertions passed
#    1 — one or more assertions failed
# =============================================================================
set -uo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYNC_SH="${DEPLOY_DIR}/sync-backup-script.sh"
REAL_BACKUP_SH="${DEPLOY_DIR}/aperod_backup.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "  ${GREEN}PASS${NC}  $1"; (( PASS++ )) || true; }
fail_assert() { echo -e "  ${RED}FAIL${NC}  $1"; (( FAIL++ )) || true; }

# Source the real function under test.
if [[ ! -f "$SYNC_SH" ]]; then
  echo "ERROR: $SYNC_SH not found" >&2
  exit 1
fi
# shellcheck source=sync-backup-script.sh
source "$SYNC_SH"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Create a temp workspace: two subdirs, one acting as the "installed" location
# and one acting as the "repo" location.
make_workspace() {
  local ws
  ws="$(mktemp -d)"
  mkdir -p "${ws}/installed" "${ws}/repo"
  echo "$ws"
}

# Place a dummy "stale" installed copy so cmp -s detects a difference and
# _sync_backup_script proceeds to the cp+mv path.
seed_installed() {
  local dir="$1"
  printf '#!/usr/bin/env bash\n# stale placeholder\nexit 0\n' \
    > "${dir}/installed/aperod_backup.sh"
  chmod 700 "${dir}/installed/aperod_backup.sh"
}

# ---------------------------------------------------------------------------
# T1: truncated (empty) repo copy → self-check must catch empty file, exit ≠ 0
# ---------------------------------------------------------------------------
echo ""
echo "── T1: truncated (empty) repo copy ──"

WS=$(make_workspace)
trap 'rm -rf "$WS"' EXIT

seed_installed "$WS"
# Empty file simulates a completely truncated download or disk-full write.
: > "${WS}/repo/aperod_backup.sh"

T1_RC=0
T1_ERR=$(
  _sync_backup_script "${WS}/installed/aperod_backup.sh" "${WS}/repo/aperod_backup.sh" 2>&1 >/dev/null
) || T1_RC=$?

if [[ "$T1_RC" -ne 0 ]]; then
  pass_assert "T1: _sync_backup_script exited non-zero ($T1_RC) for empty repo copy"
else
  fail_assert "T1: _sync_backup_script returned 0 for an empty repo copy — self-check did not fire"
fi

if echo "$T1_ERR" | grep -q "\[ERROR\].*empty"; then
  pass_assert "T1: stderr contains [ERROR] message about empty file"
else
  fail_assert "T1: expected [ERROR] about empty file in stderr — got: $(echo "$T1_ERR" | head -3)"
fi

rm -rf "$WS"
trap - EXIT

# ---------------------------------------------------------------------------
# T2: repo copy has a bash syntax error (truncated mid-function) → bash -n fails
# ---------------------------------------------------------------------------
echo ""
echo "── T2: repo copy truncated mid-function (bash -n should fail) ──"

WS=$(make_workspace)
trap 'rm -rf "$WS"' EXIT

seed_installed "$WS"
# Write a script that is syntactically invalid — open function body, no closing brace.
cat > "${WS}/repo/aperod_backup.sh" << 'EOF'
#!/usr/bin/env bash
# aperod_backup.sh — truncated mid-function (simulates partial write)
_do_backup() {
  local src="$1"
  # file was truncated here — no closing brace, no exit
EOF
chmod 755 "${WS}/repo/aperod_backup.sh"

T2_RC=0
T2_ERR=$(
  _sync_backup_script "${WS}/installed/aperod_backup.sh" "${WS}/repo/aperod_backup.sh" 2>&1 >/dev/null
) || T2_RC=$?

if [[ "$T2_RC" -ne 0 ]]; then
  pass_assert "T2: _sync_backup_script exited non-zero ($T2_RC) for syntax-broken repo copy"
else
  fail_assert "T2: _sync_backup_script returned 0 for a syntax-broken repo copy — bash -n guard did not fire"
fi

if echo "$T2_ERR" | grep -q "\[ERROR\].*syntax\|bash -n"; then
  pass_assert "T2: stderr contains [ERROR] message about bash -n / syntax"
else
  fail_assert "T2: expected [ERROR] about bash -n in stderr — got: $(echo "$T2_ERR" | head -3)"
fi

rm -rf "$WS"
trap - EXIT

# ---------------------------------------------------------------------------
# T3: valid repo copy → _sync_backup_script must succeed (exit 0)
#     and the self-check "passed" confirmation must be printed.
# ---------------------------------------------------------------------------
echo ""
echo "── T3: valid repo copy → success path ──"

WS=$(make_workspace)
trap 'rm -rf "$WS"' EXIT

seed_installed "$WS"

# Use the real aperod_backup.sh from the deploy directory if present,
# otherwise synthesise a minimal valid script.
if [[ -f "$REAL_BACKUP_SH" ]]; then
  cp "$REAL_BACKUP_SH" "${WS}/repo/aperod_backup.sh"
else
  cat > "${WS}/repo/aperod_backup.sh" << 'EOF'
#!/usr/bin/env bash
# Minimal valid aperod_backup.sh stub used by the syntax-guard test.
set -euo pipefail
main() {
  echo "backup OK"
}
main "$@"
EOF
fi
chmod 755 "${WS}/repo/aperod_backup.sh"

T3_RC=0
T3_OUT=$(
  _sync_backup_script "${WS}/installed/aperod_backup.sh" "${WS}/repo/aperod_backup.sh" 2>&1
) || T3_RC=$?

if [[ "$T3_RC" -eq 0 ]]; then
  pass_assert "T3: _sync_backup_script exited 0 for a valid repo copy"
else
  fail_assert "T3: _sync_backup_script exited $T3_RC for a valid repo copy (unexpected failure)"
fi

if echo "$T3_OUT" | grep -q "self-check passed"; then
  pass_assert "T3: self-check confirmation message printed"
else
  fail_assert "T3: expected 'self-check passed' in output — got: $(echo "$T3_OUT" | head -5)"
fi

rm -rf "$WS"
trap - EXIT

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "========================================"
TOTAL=$(( PASS + FAIL ))
echo "Results: ${PASS}/${TOTAL} passed"
if [[ "$FAIL" -gt 0 ]]; then
  echo -e "${RED}${FAIL} test(s) FAILED${NC}"
  exit 1
fi
echo -e "${GREEN}All tests passed.${NC}"
exit 0
