#!/usr/bin/env bash
# =============================================================================
#  test-update-api-e2e.sh — End-to-end smoke test for the backup-script sync
#  step that runs inside update-api.sh (Step 1b).
#
#  Strategy
#  ────────
#  The test does NOT require Docker.  It exercises the _sync_backup_script
#  helper from sync-backup-script.sh directly, using temp paths, which is
#  exactly what the production code does when update-api.sh sources that
#  helper and calls it.
#
#  Scenarios covered
#  ─────────────────
#  A1. Installed copy differs from repo → sync replaces it; sha256sum matches.
#  A2. Installed copy already matches repo → sync is a no-op; sha256sum still
#      matches (idempotent).
#  A3. Installed file absent → sync skips silently (returns 0); no spurious
#      file is created.
#  A4. Repo file absent → sync skips silently (returns 0); installed file is
#      left untouched.
#  A5. After sync the installed copy has mode 700 (executable, not world-readable).
#
#  Exit codes
#  ──────────
#    0 — all assertions passed
#    1 — one or more assertions failed
#
#  Run from anywhere:
#    bash blockchain/deploy/test-update-api-e2e.sh
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SYNC_SH="${SCRIPT_DIR}/sync-backup-script.sh"
REPO_BACKUP="${SCRIPT_DIR}/aperod_backup.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0; FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Pre-flight ────────────────────────────────────────────────────────────────
if [[ ! -f "$SYNC_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  sync-backup-script.sh not found at: $SYNC_SH" >&2
  exit 1
fi
if [[ ! -f "$REPO_BACKUP" ]]; then
  echo -e "${RED}[ERR]${NC}  aperod_backup.sh not found at: $REPO_BACKUP" >&2
  exit 1
fi

# ── Shared temp directory (cleaned on exit) ───────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# Source the helper so _sync_backup_script is available for all tests.
# shellcheck source=sync-backup-script.sh
source "$SYNC_SH"

# =============================================================================
# A1: Installed copy differs → sync updates it; sha256sum matches repo
# =============================================================================
section "A1: installed copy differs → sync replaces it; sha256sum matches repo"

A1_INST_DIR=$(mktemp -d "$TMPDIR_TEST/inst-a1-XXXXXXXX")
A1_INSTALLED="${A1_INST_DIR}/aperod_backup.sh"

# Write a deliberately different "old" installed copy.
echo "# OLD STALE VERSION — should be replaced by sync" > "$A1_INSTALLED"
chmod 700 "$A1_INSTALLED"

# Run the sync with explicit paths (no root needed; chown is best-effort).
_sync_backup_script "$A1_INSTALLED" "$REPO_BACKUP" >/dev/null 2>&1

# Assert the installed copy now matches the repo copy by sha256sum.
REPO_SUM=$(sha256sum "$REPO_BACKUP"  | awk '{print $1}')
INST_SUM=$(sha256sum "$A1_INSTALLED" | awk '{print $1}')

if [[ "$INST_SUM" == "$REPO_SUM" ]]; then
  pass "sha256sum of installed copy matches repo copy after sync ($INST_SUM)"
else
  fail "sha256sum mismatch after sync — installed=${INST_SUM} repo=${REPO_SUM}"
fi

# The file must still exist and be a regular file.
if [[ -f "$A1_INSTALLED" ]]; then
  pass "installed file still exists after sync"
else
  fail "installed file was removed by sync (expected it to be replaced)"
fi

# =============================================================================
# A2: Already up to date → no-op; sha256sum still matches AND inode unchanged
# =============================================================================
section "A2: installed copy already matches repo → no-op; sha256sum + inode unchanged"

A2_INST_DIR=$(mktemp -d "$TMPDIR_TEST/inst-a2-XXXXXXXX")
A2_INSTALLED="${A2_INST_DIR}/aperod_backup.sh"

# Seed with an exact copy of the repo file (already in sync).
cp "$REPO_BACKUP" "$A2_INSTALLED"
chmod 700 "$A2_INSTALLED"

A2_INODE_BEFORE=$(stat -c '%i' "$A2_INSTALLED" 2>/dev/null \
               || stat -f '%i' "$A2_INSTALLED" 2>/dev/null \
               || echo "unknown")

_sync_backup_script "$A2_INSTALLED" "$REPO_BACKUP" >/dev/null 2>&1

A2_INODE_AFTER=$(stat -c '%i' "$A2_INSTALLED" 2>/dev/null \
              || stat -f '%i' "$A2_INSTALLED" 2>/dev/null \
              || echo "unknown")

# A genuine no-op must NOT replace the file: the inode must remain the same.
# (An atomic mv -f would create a new inode even if content is identical.)
if [[ "$A2_INODE_AFTER" == "$A2_INODE_BEFORE" && "$A2_INODE_BEFORE" != "unknown" ]]; then
  pass "inode unchanged — file was not replaced on no-op sync (inode=$A2_INODE_AFTER)"
else
  fail "inode changed — file was unnecessarily replaced: before=$A2_INODE_BEFORE after=$A2_INODE_AFTER"
fi

REPO_SUM_A2=$(sha256sum "$REPO_BACKUP" | awk '{print $1}')
A2_INST_SUM=$(sha256sum "$A2_INSTALLED" | awk '{print $1}')
if [[ "$A2_INST_SUM" == "$REPO_SUM_A2" ]]; then
  pass "sha256sum still matches repo copy after no-op ($A2_INST_SUM)"
else
  fail "sha256sum does not match repo after no-op — installed=${A2_INST_SUM} repo=${REPO_SUM_A2}"
fi

# =============================================================================
# A3: Installed file absent → sync skips; no spurious file created; no output
# =============================================================================
section "A3: installed file absent → sync skips silently; no file created; no output"

A3_INST_DIR=$(mktemp -d "$TMPDIR_TEST/inst-a3-XXXXXXXX")
A3_INSTALLED="${A3_INST_DIR}/aperod_backup.sh"
# Deliberately do NOT create the installed file.

A3_OUTPUT=$( { _sync_backup_script "$A3_INSTALLED" "$REPO_BACKUP"; } 2>&1 )
A3_EXIT=$?

if [[ "$A3_EXIT" -eq 0 ]]; then
  pass "sync returned 0 when installed file is absent (non-fatal skip)"
else
  fail "sync returned $A3_EXIT — expected 0 (should skip silently)"
fi

if [[ ! -f "$A3_INSTALLED" ]]; then
  pass "no spurious installed file created when installed was absent"
else
  fail "sync unexpectedly created ${A3_INSTALLED} when it was absent"
fi

# When the installed file is absent the function returns early with no output.
if [[ -z "$A3_OUTPUT" ]]; then
  pass "sync produced no output when installed file was absent (truly silent)"
else
  fail "sync produced unexpected output when installed file was absent: '$A3_OUTPUT'"
fi

# =============================================================================
# A4: Repo file absent → sync skips; installed file untouched; no output
# =============================================================================
section "A4: repo file absent → sync skips silently; installed copy untouched; no output"

A4_INST_DIR=$(mktemp -d "$TMPDIR_TEST/inst-a4-XXXXXXXX")
A4_INSTALLED="${A4_INST_DIR}/aperod_backup.sh"
echo "# INSTALLED COPY — must not be removed" > "$A4_INSTALLED"
chmod 700 "$A4_INSTALLED"
A4_CONTENT_BEFORE=$(cat "$A4_INSTALLED")

A4_MISSING_REPO="${TMPDIR_TEST}/nonexistent_backup.sh"

A4_OUTPUT=$( { _sync_backup_script "$A4_INSTALLED" "$A4_MISSING_REPO"; } 2>&1 )
A4_EXIT=$?

if [[ "$A4_EXIT" -eq 0 ]]; then
  pass "sync returned 0 when repo file is absent (non-fatal skip)"
else
  fail "sync returned $A4_EXIT — expected 0 (should skip silently)"
fi

A4_CONTENT_AFTER=$(cat "$A4_INSTALLED")
if [[ "$A4_CONTENT_AFTER" == "$A4_CONTENT_BEFORE" ]]; then
  pass "installed file content untouched when repo file was absent"
else
  fail "installed file was modified despite repo file being absent"
fi

# When the repo file is absent the function returns early with no output.
if [[ -z "$A4_OUTPUT" ]]; then
  pass "sync produced no output when repo file was absent (truly silent)"
else
  fail "sync produced unexpected output when repo file was absent: '$A4_OUTPUT'"
fi

# =============================================================================
# A5: After sync the installed copy has mode 700
# =============================================================================
section "A5: installed copy has mode 700 after sync"

A5_INST_DIR=$(mktemp -d "$TMPDIR_TEST/inst-a5-XXXXXXXX")
A5_INSTALLED="${A5_INST_DIR}/aperod_backup.sh"

# Start with a wrong mode (644) to confirm chmod is applied.
echo "# STALE" > "$A5_INSTALLED"
chmod 644 "$A5_INSTALLED"

_sync_backup_script "$A5_INSTALLED" "$REPO_BACKUP" >/dev/null 2>&1

A5_MODE=$(stat -c '%a' "$A5_INSTALLED" 2>/dev/null \
        || stat -f '%OLp' "$A5_INSTALLED" 2>/dev/null \
        || echo "unknown")

if [[ "$A5_MODE" == "700" ]]; then
  pass "installed copy has mode 700 after sync"
else
  fail "installed copy has mode $A5_MODE after sync — expected 700"
fi

# =============================================================================
# Summary
# =============================================================================
echo ""
echo "──────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ "$FAIL" -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL assertions FAILED.${NC}"
  exit 1
fi
