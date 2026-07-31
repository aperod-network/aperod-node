#!/usr/bin/env bash
# test-node-config.sh — End-to-end tests for node-config.sh
#
# Verifies that node.yaml cannot be corrupted even if Python crashes mid-write
# or if the final atomic rename fails (e.g. ENOSPC, permission error).
#
# Run from anywhere:
#   bash blockchain/deploy/test-node-config.sh
#
# Exit codes:
#   0 — all tests passed
#   1 — one or more tests failed
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODE_CONFIG_SH="$SCRIPT_DIR/node-config.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'

PASS=0
FAIL=0

pass() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Require python3 + pyyaml ──────────────────────────────────────────────────
if ! python3 -c "import yaml" 2>/dev/null; then
  echo -e "${RED}[ERR]${NC}  python3 + pyyaml required. Run: pip3 install pyyaml" >&2
  exit 1
fi

# ── Setup: temp working directory ─────────────────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

ADDR1="/ip4/1.2.3.4/tcp/30303"
ADDR2="/ip4/5.6.7.8/tcp/30303"
ADDR_ABSENT="/ip4/9.9.9.9/tcp/30303"

# Write a minimal valid node.yaml.
make_config() {
  local path="$1"
  cat >"$path" <<'YAML'
node:
  name: test-node
  data_dir: /tmp/aperod-test

p2p:
  listen: "0.0.0.0:30303"
  bootnodes: []

consensus:
  type: poa
YAML
}

# Return 0 if file is parseable YAML.
is_valid_yaml() {
  python3 - "$1" <<'PY' 2>/dev/null
import sys, yaml
try:
    with open(sys.argv[1]) as f:
        yaml.safe_load(f)
    sys.exit(0)
except Exception:
    sys.exit(1)
PY
}

# Print bootnode count from YAML file.
bootnode_count() {
  python3 - "$1" <<'PY' 2>/dev/null
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
nodes = (cfg.get("p2p") or {}).get("bootnodes") or []
print(len(nodes))
PY
}

# ─────────────────────────────────────────────────────────────────────────────
# Test 1: add-bootnode writes valid YAML
# ─────────────────────────────────────────────────────────────────────────────
section "Test 1: add-bootnode produces valid YAML"
CFG1="$TMPDIR_TEST/node-t1.yaml"
make_config "$CFG1"

APEROD_CONFIG="$CFG1" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR1" >/dev/null 2>&1
EXIT=$?

if [[ $EXIT -eq 0 ]]; then
  pass "add-bootnode exited 0"
else
  fail "add-bootnode exited $EXIT (expected 0)"
fi

if is_valid_yaml "$CFG1"; then
  pass "node.yaml is valid YAML after add-bootnode"
else
  fail "node.yaml is invalid YAML after add-bootnode"
fi

COUNT=$(bootnode_count "$CFG1")
if [[ "$COUNT" -eq 1 ]]; then
  pass "bootnode list contains exactly 1 entry"
else
  fail "expected 1 bootnode, got '$COUNT'"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 2: duplicate add-bootnode is a no-op
# ─────────────────────────────────────────────────────────────────────────────
section "Test 2: duplicate add-bootnode does not double-add"
# Run add again with the same address (reuse CFG1 which already has ADDR1)
APEROD_CONFIG="$CFG1" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR1" >/dev/null 2>&1

COUNT=$(bootnode_count "$CFG1")
if [[ "$COUNT" -eq 1 ]]; then
  pass "bootnode count is still 1 after duplicate add"
else
  fail "expected 1 bootnode after duplicate add, got '$COUNT'"
fi

if is_valid_yaml "$CFG1"; then
  pass "YAML is still valid after duplicate add"
else
  fail "YAML became invalid after duplicate add"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 3: remove-bootnode leaves valid YAML with correct entries
# ─────────────────────────────────────────────────────────────────────────────
section "Test 3: remove-bootnode produces valid YAML"
CFG3="$TMPDIR_TEST/node-t3.yaml"
make_config "$CFG3"

# Add two bootnodes
APEROD_CONFIG="$CFG3" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR1" >/dev/null 2>&1
APEROD_CONFIG="$CFG3" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR2" >/dev/null 2>&1

BEFORE=$(bootnode_count "$CFG3")

APEROD_CONFIG="$CFG3" bash "$NODE_CONFIG_SH" remove-bootnode "$ADDR1" >/dev/null 2>&1
RMEXIT=$?

if [[ $RMEXIT -eq 0 ]]; then
  pass "remove-bootnode exited 0"
else
  fail "remove-bootnode exited $RMEXIT (expected 0)"
fi

if is_valid_yaml "$CFG3"; then
  pass "YAML is valid after remove-bootnode"
else
  fail "YAML is invalid after remove-bootnode"
fi

AFTER=$(bootnode_count "$CFG3")
EXPECTED=$((BEFORE - 1))
if [[ "$AFTER" -eq $EXPECTED ]]; then
  pass "bootnode count decreased from $BEFORE to $AFTER"
else
  fail "expected $EXPECTED bootnodes after remove, got '$AFTER'"
fi

CONTENT=$(<"$CFG3")
if echo "$CONTENT" | grep -qF "$ADDR1"; then
  fail "removed address $ADDR1 still present in config"
else
  pass "removed address $ADDR1 is absent from config"
fi

if echo "$CONTENT" | grep -qF "$ADDR2"; then
  pass "remaining address $ADDR2 is still in config"
else
  fail "remaining address $ADDR2 is missing from config"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 4: corrupt node.yaml input → non-zero exit, original file unchanged
# ─────────────────────────────────────────────────────────────────────────────
section "Test 4: corrupt node.yaml input → script exits non-zero, file unchanged"
CFG4="$TMPDIR_TEST/node-t4.yaml"

# Write deliberately corrupt YAML as the starting config.
printf 'p2p:\n  bootnodes: [unclosed bracket\nnode: {\n' >"$CFG4"

# Hash the corrupt file before calling node-config.sh.
BEFORE_HASH=$(sha256sum "$CFG4" | awk '{print $1}')

# add-bootnode must exit non-zero when it cannot parse the input YAML.
APEROD_CONFIG="$CFG4" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR1" >/dev/null 2>&1
BAD_EXIT=$?

if [[ $BAD_EXIT -ne 0 ]]; then
  pass "add-bootnode exited non-zero ($BAD_EXIT) on corrupt input YAML"
else
  fail "add-bootnode exited 0 on corrupt input YAML (expected failure)"
fi

# The file must still contain the same corrupt content — nothing was written.
AFTER_HASH=$(sha256sum "$CFG4" | awk '{print $1}')
if [[ "$AFTER_HASH" == "$BEFORE_HASH" ]]; then
  pass "corrupt file is byte-for-byte unchanged after failed add-bootnode"
else
  fail "file was modified despite add-bootnode failure — corruption bug!"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 5: fault injection — atomic rename fails → original untouched
#
# This is the primary regression guard for the corruption bug: we inject a
# failing mv into PATH so the final "put new file in place" step fails AFTER
# Python has written and validated the temp file.  The original node.yaml must
# be byte-for-byte identical to what it was before the call.
# ─────────────────────────────────────────────────────────────────────────────
section "Test 5: original untouched when the final atomic rename fails"
CFG5="$TMPDIR_TEST/node-t5.yaml"
make_config "$CFG5"
ORIGINAL5=$(cat "$CFG5")
HASH5_BEFORE=$(sha256sum "$CFG5" | awk '{print $1}')

# Create a fake bin directory with a mv that always fails.
FAKE_BIN5="$TMPDIR_TEST/fake-bin5"
mkdir -p "$FAKE_BIN5"
cat >"$FAKE_BIN5/mv" <<'MVSH'
#!/usr/bin/env bash
# Simulates ENOSPC / rename failure: always exit 1 without touching any file.
echo "mv: simulated atomic rename failure (ENOSPC)" >&2
exit 1
MVSH
chmod +x "$FAKE_BIN5/mv"

# Run add-bootnode with the failing mv at the front of PATH.
PATH="$FAKE_BIN5:$PATH" APEROD_CONFIG="$CFG5" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR1" >/dev/null 2>&1
MV_EXIT=$?

if [[ $MV_EXIT -ne 0 ]]; then
  pass "script exited non-zero ($MV_EXIT) when atomic rename failed"
else
  fail "script exited 0 despite mv failure (should have propagated the error)"
fi

# The original config must be byte-for-byte identical.
HASH5_AFTER=$(sha256sum "$CFG5" | awk '{print $1}')
if [[ "$HASH5_AFTER" == "$HASH5_BEFORE" ]]; then
  pass "node.yaml is byte-for-byte unchanged after failed atomic rename"
else
  fail "node.yaml was modified despite mv failure — CORRUPTION BUG (cp would do this)"
fi

if is_valid_yaml "$CFG5"; then
  pass "node.yaml is still valid YAML after failed atomic rename"
else
  fail "node.yaml became invalid YAML after failed atomic rename"
fi

# Confirm the config content is exactly what we started with (not a partial write).
CURRENT5=$(cat "$CFG5")
if [[ "$CURRENT5" == "$ORIGINAL5" ]]; then
  pass "node.yaml content is identical to original (no partial write)"
else
  fail "node.yaml content differs from original — partial write detected"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 6: structural guard — validate_yaml is called BEFORE mv
# ─────────────────────────────────────────────────────────────────────────────
# Verify at the source level that the script always validates the temp file
# before replacing the real config.  Any future edit that introduces a direct
# write to $CONFIG_FILE or moves the mv before the validate call will fail here.
section "Test 6: validate_yaml is called before mv in both cmd_add and cmd_remove"

SCRIPT="$NODE_CONFIG_SH"

check_order() {
  local func="$1"
  local body
  body=$(awk "/^${func}\(\)/{f=1} f{print} /^}/{if(f){f=0}}" "$SCRIPT")

  local validate_line mv_line
  validate_line=$(echo "$body" | grep -n "validate_yaml" | head -1 | cut -d: -f1)
  mv_line=$(echo "$body" | grep -n '^\s*mv ' | head -1 | cut -d: -f1)

  if [[ -z "$validate_line" ]]; then
    fail "$func: validate_yaml call not found in function body"
    return
  fi
  if [[ -z "$mv_line" ]]; then
    fail "$func: atomic mv not found in function body"
    return
  fi

  if [[ "$validate_line" -lt "$mv_line" ]]; then
    pass "$func: validate_yaml (line $validate_line) is before mv (line $mv_line)"
  else
    fail "$func: mv (line $mv_line) appears BEFORE validate_yaml (line $validate_line) — regression!"
  fi
}

check_order "cmd_add"
check_order "cmd_remove"

# Assert no direct truncating writes to CONFIG_FILE exist (no cp, no > redirect).
DIRECT_WRITES=$(grep -n \
  '>"$CONFIG_FILE"\|>>"$CONFIG_FILE"\|tee.*"$CONFIG_FILE"\|cp .*"$CONFIG_FILE"' \
  "$SCRIPT" || true)
if [[ -z "$DIRECT_WRITES" ]]; then
  pass "no direct/truncating writes to \$CONFIG_FILE (only atomic mv from validated tmp)"
else
  fail "direct/truncating write to \$CONFIG_FILE detected — bypasses atomicity:\n$DIRECT_WRITES"
fi

# Assert mv is used (not cp) for the final replacement.
MV_LINES=$(grep -c '^\s*mv .*"\$CONFIG_FILE"' "$SCRIPT" || true)
CP_LINES=$(grep -c '^\s*cp .*"\$CONFIG_FILE"' "$SCRIPT" || true)
if [[ "$MV_LINES" -ge 2 ]]; then
  pass "mv used for final replacement in both cmd_add and cmd_remove ($MV_LINES occurrences)"
else
  fail "expected ≥2 mv-to-CONFIG_FILE lines, found $MV_LINES"
fi
if [[ "$CP_LINES" -eq 0 ]]; then
  pass "no cp-to-CONFIG_FILE found (atomic mv only)"
else
  fail "cp-to-CONFIG_FILE still present ($CP_LINES lines) — not atomic"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 7: remove-bootnode on absent address exits non-zero, file unchanged
# ─────────────────────────────────────────────────────────────────────────────
section "Test 7: remove-bootnode on absent address exits non-zero"
CFG7="$TMPDIR_TEST/node-t7.yaml"
make_config "$CFG7"

BEFORE7_HASH=$(sha256sum "$CFG7" | awk '{print $1}')

APEROD_CONFIG="$CFG7" bash "$NODE_CONFIG_SH" remove-bootnode "$ADDR_ABSENT" >/dev/null 2>&1
RM7_EXIT=$?

if [[ $RM7_EXIT -ne 0 ]]; then
  pass "remove-bootnode on absent address exited non-zero ($RM7_EXIT)"
else
  fail "remove-bootnode on absent address exited 0 (expected failure)"
fi

AFTER7_HASH=$(sha256sum "$CFG7" | awk '{print $1}')
if [[ "$AFTER7_HASH" == "$BEFORE7_HASH" ]]; then
  pass "config file is unchanged after failed remove-bootnode"
else
  fail "config file was modified despite failed remove-bootnode"
fi

if is_valid_yaml "$CFG7"; then
  pass "YAML remains valid after failed remove-bootnode"
else
  fail "YAML became invalid after failed remove-bootnode"
fi

# ─────────────────────────────────────────────────────────────────────────────
# Test 8: concurrent add-bootnode calls — both addresses must survive
#
# Two admins run add-bootnode simultaneously.  Without flock they race: both
# read the same original, write separate temp files, and the last mv wins —
# silently discarding the other's change.  With flock they serialise, so both
# addresses end up in the final config.
# ─────────────────────────────────────────────────────────────────────────────
section "Test 8: concurrent add-bootnode calls — both addresses survive"
CFG8="$TMPDIR_TEST/node-t8.yaml"
make_config "$CFG8"

# Launch two concurrent add-bootnode invocations in the background.
APEROD_CONFIG="$CFG8" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR1" >/dev/null 2>&1 &
PID1=$!
APEROD_CONFIG="$CFG8" bash "$NODE_CONFIG_SH" add-bootnode "$ADDR2" >/dev/null 2>&1 &
PID2=$!

wait $PID1; EXIT1=$?
wait $PID2; EXIT2=$?

if [[ $EXIT1 -eq 0 && $EXIT2 -eq 0 ]]; then
  pass "both concurrent add-bootnode calls exited 0"
else
  fail "concurrent add-bootnode: exit codes were $EXIT1 and $EXIT2 (expected both 0)"
fi

if is_valid_yaml "$CFG8"; then
  pass "node.yaml is valid YAML after concurrent adds"
else
  fail "node.yaml is invalid YAML after concurrent adds"
fi

COUNT8=$(bootnode_count "$CFG8")
if [[ "$COUNT8" -eq 2 ]]; then
  pass "both bootnodes present after concurrent adds (count=$COUNT8)"
else
  fail "expected 2 bootnodes after concurrent adds, got '$COUNT8'"
fi

CONTENT8=$(<"$CFG8")
ADDR1_FOUND=0; ADDR2_FOUND=0
echo "$CONTENT8" | grep -qF "$ADDR1" && ADDR1_FOUND=1
echo "$CONTENT8" | grep -qF "$ADDR2" && ADDR2_FOUND=1

if [[ $ADDR1_FOUND -eq 1 && $ADDR2_FOUND -eq 1 ]]; then
  pass "both ADDR1 and ADDR2 are present in the final config"
else
  fail "one or both addresses missing from config after concurrent adds (ADDR1=$ADDR1_FOUND ADDR2=$ADDR2_FOUND)"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
TOTAL=$((PASS + FAIL))
echo ""
echo "─────────────────────────────────────"
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}${BOLD}All $TOTAL tests passed.${NC}"
  exit 0
else
  echo -e "${RED}${BOLD}$FAIL / $TOTAL tests FAILED.${NC}"
  exit 1
fi
