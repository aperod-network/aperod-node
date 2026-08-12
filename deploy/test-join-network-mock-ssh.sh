#!/usr/bin/env bash
# =============================================================================
#  test-join-network-mock-ssh.sh — Mock-SSH end-to-end test for join-network.sh
#
#  Verifies that running the full join-network.sh script against a stubbed SSH
#  target correctly:
#    1. Writes the primary bootnode into p2p.bootnodes in the secondary node.yaml
#    2. Exits 0 and reports peer_count >= 1 in the stats output
#    3. Works via the Python fallback path (when node-config.sh is absent)
#    4. Works via the node-config.sh preferred path (when it is present)
#    5. Preserves existing p2p.bootnodes entries when adding a new one
#    6. Leaves no stale .tmp file behind after the injection
#
#  All SSH, rsync, systemctl, and sleep calls are stubbed.  The SSH stub
#  intercepts the step-5/7 bootnode-injection heredoc (identified by the remote
#  command being exactly "bash") and executes it locally against a real temp
#  node.yaml — confirming that the quoting, variable expansion, and Python/
#  node-config.sh logic in the heredoc actually work end-to-end.
#
#  SECONDARY_NODE_YAML is overridden via env var so the injection targets our
#  temp file instead of /etc/aperod/node.yaml (requires no root access).
#
#  Run from anywhere:
#    bash blockchain/deploy/test-join-network-mock-ssh.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOIN_SH="${SCRIPT_DIR}/join-network.sh"
NODE_CONFIG_SH="${SCRIPT_DIR}/node-config.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'

PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Pre-flight ────────────────────────────────────────────────────────────────
if [[ ! -f "${JOIN_SH}" ]]; then
  echo -e "${RED}[ERR]${NC}  join-network.sh not found at: ${JOIN_SH}" >&2
  exit 1
fi

if ! command -v python3 &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  python3 not found in PATH — skipping." >&2
  exit 0
fi

if ! python3 -c "import yaml" 2>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  python3 pyyaml not available — skipping." >&2
  exit 0
fi

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

TARGET_IP="192.0.2.55"   # TEST-NET-1 — never routes
PRIMARY_IP="10.0.0.1"
BOOTNODE="/ip4/${PRIMARY_IP}/tcp/30303"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

make_stub() {
  local dir="$1" cmd="$2" body="$3"
  mkdir -p "$dir"
  printf '#!/usr/bin/env bash\n%s\n' "$body" >"${dir}/${cmd}"
  chmod +x "${dir}/${cmd}"
}

# Return 0 if the file is parseable YAML.
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

# Print p2p.bootnodes entries, one per line.
get_p2p_bootnodes() {
  python3 - "$1" <<'PY' 2>/dev/null
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
nodes = (cfg.get("p2p") or {}).get("bootnodes") or []
for n in nodes:
    print(n)
PY
}

# Print root-level bootnodes entries (legacy key), one per line.
get_toplevel_bootnodes() {
  python3 - "$1" <<'PY' 2>/dev/null
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
nodes = cfg.get("bootnodes") or []
for n in nodes:
    print(n)
PY
}

count_lines() { wc -l | tr -d ' '; }

# Write a canonical nested-schema node.yaml (install-validator.sh output).
make_nested_config() {
  local path="$1"
  cat >"$path" <<'YAML'
network: testnet
data_dir: /var/lib/aperod
log_level: info

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  bootnodes: []
  max_peers: 50

consensus:
  validator_key: /etc/aperod/validator.key
  reward_address: aproecTest

api:
  enabled: true
  listen_addr: 127.0.0.1:8545

genesis:
  file: /etc/aperod/genesis-testnet.yaml
YAML
}

# Write a node.yaml with a pre-existing bootnode already in p2p.bootnodes.
make_config_with_existing_bootnode() {
  local path="$1"
  cat >"$path" <<'YAML'
network: testnet
data_dir: /var/lib/aperod

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  bootnodes:
    - /ip4/1.2.3.4/tcp/30303
  max_peers: 50

consensus:
  validator_key: /etc/aperod/validator.key
  reward_address: aproecTest

api:
  enabled: true
  listen_addr: 127.0.0.1:8545

genesis:
  file: /etc/aperod/genesis-testnet.yaml
YAML
}

# Write a node.yaml that uses the older root-level bootnodes key (install-node.sh).
make_toplevel_config() {
  local path="$1"
  cat >"$path" <<'YAML'
network: testnet
data_dir: /var/lib/aperod

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  max_peers: 30

bootnodes: []

api:
  enabled: true
  listen_addr: 127.0.0.1:8545

genesis:
  file: /etc/aperod/genesis-testnet.yaml
YAML
}

# ---------------------------------------------------------------------------
# run_join TARGET_IP BIN_DIR DATA_DIR NODE_YAML [NODE_CONFIG_SH_OVERRIDE]
#
# Runs join-network.sh with stubs prepended to PATH and the key env vars
# overridden.  The SECONDARY_NODE_YAML env var redirects the bootnode-injection
# step to our temp node.yaml; SECONDARY_NODE_CONFIG_SH controls which path the
# heredoc tries first.
#
# Sets LAST_EXIT and LAST_OUTPUT.
# ---------------------------------------------------------------------------
LAST_OUTPUT=""
LAST_EXIT=0

run_join() {
  local tip="$1" bdir="$2" ddir="$3" node_yaml="$4"
  local node_config_sh="${5:-/nonexistent/node-config.sh}"

  LAST_EXIT=0
  LAST_OUTPUT=$(
    PATH="${bdir}:${PATH}" \
    PRIMARY_IP="${PRIMARY_IP}" \
    PRIMARY_DATA_DIR="${ddir}" \
    SECONDARY_NODE_YAML="${node_yaml}" \
    SECONDARY_NODE_CONFIG_SH="${node_config_sh}" \
    bash "${JOIN_SH}" "${tip}" 2>&1
  ) || LAST_EXIT=$?
}

# ---------------------------------------------------------------------------
# build_common_stubs BIN_DIR NODE_YAML PEER_COUNT HEIGHT
#
# Creates systemctl, rsync, sleep stubs common to most tests.
# The ssh stub is built separately per test to allow per-test customisation.
# ---------------------------------------------------------------------------
build_systemctl_stub() {
  local dir="$1"
  make_stub "${dir}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)   exit 0 ;;
  *"is-active"*)          exit 1 ;;   # node is NOT active → wait loop exits
  *"start aperod-node"*)  exit 0 ;;
  *"enable"*)             exit 0 ;;
  *"disable"*)            exit 0 ;;
  *)                      exit 0 ;;
esac
'
}

# Build an ssh stub that:
#   • "bash" (no other args) → executes stdin heredoc locally (bootnode step).
#   • network/stats query   → returns JSON with given height and peer_count.
#   • everything else       → drains stdin, prints neutral success messages.
#
# Usage: build_ssh_stub BIN_DIR HEIGHT PEER_COUNT
build_ssh_stub() {
  local dir="$1" height="$2" peer_count="$3"
  make_stub "${dir}" "ssh" "
shift   # drop 'root@IP'
CMD=\"\$*\"
# Step 5/7: bootnode injection heredoc — the remote command is just 'bash'.
if [[ \"\${CMD}\" == \"bash\" ]]; then
  bash   # execute the heredoc from stdin locally
elif echo \"\${CMD}\" | grep -q 'network/stats'; then
  echo '{\"height\":${height},\"peer_count\":${peer_count},\"syncing\":false}'
elif echo \"\${CMD}\" | grep -q 'systemctl show'; then
  # verify-dropin.sh checks these two settings over ssh
  echo 'Environment=GOMEMLIMIT=5368709120'
  echo 'TimeoutStopUSec=15min'
elif echo \"\${CMD}\" | grep -q 'curl'; then
  echo '{\"ok\":true}'
else
  cat >/dev/null
  printf 'stopped\nremoved\nstarted\n'
fi
exit 0
"
}

# =============================================================================
# ── TEST M1: Python fallback path — bootnode written to p2p.bootnodes ─────────
# =============================================================================
section "M1: Full script run — Python fallback writes bootnode into p2p.bootnodes"
#
# node-config.sh is pointed at a non-existent path so step 5/7 falls through to
# the Python fallback inside the heredoc.  We verify that after join-network.sh
# exits 0 the real temp node.yaml contains the primary bootnode in p2p.bootnodes.

M1_DATA="${TMPDIR_TEST}/m1-data"
mkdir -p "${M1_DATA}/chain.db" && touch "${M1_DATA}/chain.db/CURRENT"

M1_YAML="${TMPDIR_TEST}/m1-node.yaml"
make_nested_config "${M1_YAML}"

M1_BIN="${TMPDIR_TEST}/m1-bin"
build_systemctl_stub "${M1_BIN}"
build_ssh_stub "${M1_BIN}" 42000 1
make_stub "${M1_BIN}" "rsync" 'exit 0'
make_stub "${M1_BIN}" "sleep"  'exit 0'

run_join "${TARGET_IP}" "${M1_BIN}" "${M1_DATA}" "${M1_YAML}" "/nonexistent/node-config.sh"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "M1a: join-network.sh exited 0 (Python fallback path)"
else
  fail "M1a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

if is_valid_yaml "${M1_YAML}"; then
  pass "M1b: node.yaml is valid YAML after run"
else
  fail "M1b: node.yaml is invalid YAML after run"
fi

M1_COUNT=$(get_p2p_bootnodes "${M1_YAML}" | count_lines)
if [[ "${M1_COUNT}" -ge 1 ]]; then
  pass "M1c: p2p.bootnodes has ${M1_COUNT} entry (>= 1)"
else
  fail "M1c: p2p.bootnodes is empty — bootnode injection did not run"
fi

if get_p2p_bootnodes "${M1_YAML}" | grep -qF "${BOOTNODE}"; then
  pass "M1d: p2p.bootnodes contains ${BOOTNODE}"
else
  fail "M1d: ${BOOTNODE} not found in p2p.bootnodes. Got: $(get_p2p_bootnodes "${M1_YAML}")"
fi

TL_M1=$(get_toplevel_bootnodes "${M1_YAML}" | count_lines)
if [[ "${TL_M1}" -eq 0 ]]; then
  pass "M1e: no spurious root-level bootnodes key written"
else
  fail "M1e: unexpected root-level bootnodes key found (count=${TL_M1})"
fi

if echo "${LAST_OUTPUT}" | grep -q "peers=1"; then
  pass "M1f: output reports peers=1 (peer_count >= 1)"
else
  fail "M1f: expected 'peers=1' in output. Got:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -q "height=42000"; then
  pass "M1g: output reports height=42000"
else
  fail "M1g: expected 'height=42000' in output. Got:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -q "подключён к сети"; then
  pass "M1h: success banner present in output"
else
  fail "M1h: success banner 'подключён к сети' missing from output"
fi

if [[ ! -f "${M1_YAML}.tmp" ]]; then
  pass "M1i: no stale .tmp file left behind"
else
  fail "M1i: stale .tmp file found after run"
fi

# =============================================================================
# ── TEST M2: node-config.sh preferred path ────────────────────────────────────
# =============================================================================
section "M2: Full script run — node-config.sh preferred path writes bootnode"
#
# SECONDARY_NODE_CONFIG_SH is pointed at the real node-config.sh in the deploy
# directory.  The heredoc in step 5/7 will call it (if it is executable) rather
# than falling through to the Python fallback.

if [[ ! -f "${NODE_CONFIG_SH}" ]]; then
  echo -e "${YELLOW}  SKIP${NC}  node-config.sh not found at ${NODE_CONFIG_SH} — skipping M2"
else
  M2_DATA="${TMPDIR_TEST}/m2-data"
  mkdir -p "${M2_DATA}/chain.db" && touch "${M2_DATA}/chain.db/CURRENT"

  M2_YAML="${TMPDIR_TEST}/m2-node.yaml"
  make_nested_config "${M2_YAML}"

  M2_BIN="${TMPDIR_TEST}/m2-bin"
  build_systemctl_stub "${M2_BIN}"
  build_ssh_stub "${M2_BIN}" 77777 3
  make_stub "${M2_BIN}" "rsync" 'exit 0'
  make_stub "${M2_BIN}" "sleep"  'exit 0'

  run_join "${TARGET_IP}" "${M2_BIN}" "${M2_DATA}" "${M2_YAML}" "${NODE_CONFIG_SH}"

  if [[ ${LAST_EXIT} -eq 0 ]]; then
    pass "M2a: join-network.sh exited 0 (node-config.sh path)"
  else
    fail "M2a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
  fi

  if is_valid_yaml "${M2_YAML}"; then
    pass "M2b: node.yaml is valid YAML after node-config.sh injection"
  else
    fail "M2b: node.yaml is invalid YAML after node-config.sh injection"
  fi

  M2_COUNT=$(get_p2p_bootnodes "${M2_YAML}" | count_lines)
  if [[ "${M2_COUNT}" -ge 1 ]]; then
    pass "M2c: p2p.bootnodes has ${M2_COUNT} entry (>= 1) via node-config.sh"
  else
    fail "M2c: p2p.bootnodes is empty — node-config.sh injection did not run"
  fi

  if get_p2p_bootnodes "${M2_YAML}" | grep -qF "${BOOTNODE}"; then
    pass "M2d: p2p.bootnodes contains ${BOOTNODE} after node-config.sh run"
  else
    fail "M2d: ${BOOTNODE} not found in p2p.bootnodes after node-config.sh run. Got: $(get_p2p_bootnodes "${M2_YAML}")"
  fi

  if echo "${LAST_OUTPUT}" | grep -q "peers=3"; then
    pass "M2e: output reports peers=3 (peer_count >= 1)"
  else
    fail "M2e: expected 'peers=3' in output. Got:\n${LAST_OUTPUT}"
  fi

  # ── M2 idempotency: second run must not add a duplicate entry ──────────────
  # join-network.sh step 5/7 calls node-config.sh add-bootnode again with the
  # same multiaddr.  node-config.sh must detect the entry is already present
  # and leave p2p.bootnodes with exactly 1 entry.
  M2_SECOND_EXIT=0
  M2_SECOND_OUTPUT=$(
    PATH="${M2_BIN}:${PATH}" \
    PRIMARY_IP="${PRIMARY_IP}" \
    PRIMARY_DATA_DIR="${M2_DATA}" \
    SECONDARY_NODE_YAML="${M2_YAML}" \
    SECONDARY_NODE_CONFIG_SH="${NODE_CONFIG_SH}" \
    bash "${JOIN_SH}" "${TARGET_IP}" 2>&1
  ) || M2_SECOND_EXIT=$?

  if [[ ${M2_SECOND_EXIT} -eq 0 ]]; then
    pass "M2f: second run exited 0 (node-config.sh idempotency)"
  else
    fail "M2f: second run expected exit 0, got ${M2_SECOND_EXIT}. Output:\n${M2_SECOND_OUTPUT}"
  fi

  M2_COUNT2=$(get_p2p_bootnodes "${M2_YAML}" | count_lines)
  if [[ "${M2_COUNT2}" -eq 1 ]]; then
    pass "M2g: p2p.bootnodes has exactly 1 entry after second run (no duplicate via node-config.sh)"
  else
    fail "M2g: expected exactly 1 bootnode after second run, got ${M2_COUNT2} — node-config.sh added a duplicate"
  fi

  if is_valid_yaml "${M2_YAML}"; then
    pass "M2h: node.yaml is valid YAML after second node-config.sh run"
  else
    fail "M2h: node.yaml is invalid YAML after second node-config.sh run"
  fi
fi

# =============================================================================
# ── TEST M3: Existing p2p.bootnodes entry preserved ───────────────────────────
# =============================================================================
section "M3: Pre-existing p2p.bootnodes entry is preserved when new bootnode is added"

M3_DATA="${TMPDIR_TEST}/m3-data"
mkdir -p "${M3_DATA}/chain.db" && touch "${M3_DATA}/chain.db/CURRENT"

M3_YAML="${TMPDIR_TEST}/m3-node.yaml"
make_config_with_existing_bootnode "${M3_YAML}"

M3_BIN="${TMPDIR_TEST}/m3-bin"
build_systemctl_stub "${M3_BIN}"
build_ssh_stub "${M3_BIN}" 99999 2
make_stub "${M3_BIN}" "rsync" 'exit 0'
make_stub "${M3_BIN}" "sleep"  'exit 0'

run_join "${TARGET_IP}" "${M3_BIN}" "${M3_DATA}" "${M3_YAML}" "/nonexistent/node-config.sh"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "M3a: join-network.sh exited 0 (pre-existing bootnode config)"
else
  fail "M3a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

M3_COUNT=$(get_p2p_bootnodes "${M3_YAML}" | count_lines)
if [[ "${M3_COUNT}" -eq 2 ]]; then
  pass "M3b: p2p.bootnodes has exactly 2 entries (pre-existing + new)"
else
  fail "M3b: expected 2 p2p.bootnodes, got ${M3_COUNT}. Entries: $(get_p2p_bootnodes "${M3_YAML}")"
fi

if get_p2p_bootnodes "${M3_YAML}" | grep -qF "${BOOTNODE}"; then
  pass "M3c: new bootnode ${BOOTNODE} present"
else
  fail "M3c: new bootnode not found in p2p.bootnodes"
fi

if get_p2p_bootnodes "${M3_YAML}" | grep -qF "/ip4/1.2.3.4/tcp/30303"; then
  pass "M3d: pre-existing bootnode /ip4/1.2.3.4/tcp/30303 preserved"
else
  fail "M3d: pre-existing bootnode was lost during injection"
fi

# =============================================================================
# ── TEST M4: Legacy root-level bootnodes key migrated ─────────────────────────
# =============================================================================
section "M4: Legacy root-level bootnodes key is migrated into p2p.bootnodes"

M4_DATA="${TMPDIR_TEST}/m4-data"
mkdir -p "${M4_DATA}/chain.db" && touch "${M4_DATA}/chain.db/CURRENT"

M4_YAML="${TMPDIR_TEST}/m4-node.yaml"
make_toplevel_config "${M4_YAML}"

M4_BIN="${TMPDIR_TEST}/m4-bin"
build_systemctl_stub "${M4_BIN}"
build_ssh_stub "${M4_BIN}" 55555 1
make_stub "${M4_BIN}" "rsync" 'exit 0'
make_stub "${M4_BIN}" "sleep"  'exit 0'

run_join "${TARGET_IP}" "${M4_BIN}" "${M4_DATA}" "${M4_YAML}" "/nonexistent/node-config.sh"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "M4a: join-network.sh exited 0 (legacy schema)"
else
  fail "M4a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

if is_valid_yaml "${M4_YAML}"; then
  pass "M4b: node.yaml is valid YAML after migration"
else
  fail "M4b: node.yaml is invalid YAML after migration"
fi

# Root-level bootnodes must be gone (Go ignores it; preserving it misleads operators).
TL_M4=$(get_toplevel_bootnodes "${M4_YAML}" | count_lines)
if [[ "${TL_M4}" -eq 0 ]]; then
  pass "M4c: root-level bootnodes key removed after migration"
else
  fail "M4c: root-level bootnodes key still present (${TL_M4} entries) — Go ignores this key"
fi

NB_M4=$(get_p2p_bootnodes "${M4_YAML}" | count_lines)
if [[ "${NB_M4}" -ge 1 ]]; then
  pass "M4d: p2p.bootnodes has ${NB_M4} entry after legacy migration"
else
  fail "M4d: p2p.bootnodes is empty after legacy migration"
fi

if get_p2p_bootnodes "${M4_YAML}" | grep -qF "${BOOTNODE}"; then
  pass "M4e: ${BOOTNODE} present in p2p.bootnodes after migration"
else
  fail "M4e: ${BOOTNODE} missing from p2p.bootnodes after migration"
fi

# =============================================================================
# ── TEST M5: Idempotency — running twice does not double-add ──────────────────
# =============================================================================
section "M5: Running join-network.sh twice does not add a duplicate bootnode entry"

M5_DATA="${TMPDIR_TEST}/m5-data"
mkdir -p "${M5_DATA}/chain.db" && touch "${M5_DATA}/chain.db/CURRENT"

M5_YAML="${TMPDIR_TEST}/m5-node.yaml"
make_nested_config "${M5_YAML}"

M5_BIN="${TMPDIR_TEST}/m5-bin"
build_systemctl_stub "${M5_BIN}"
build_ssh_stub "${M5_BIN}" 11111 1
make_stub "${M5_BIN}" "rsync" 'exit 0'
make_stub "${M5_BIN}" "sleep"  'exit 0'

# First run.
run_join "${TARGET_IP}" "${M5_BIN}" "${M5_DATA}" "${M5_YAML}" "/nonexistent/node-config.sh"
FIRST_EXIT=${LAST_EXIT}

# Second run — should be idempotent.
run_join "${TARGET_IP}" "${M5_BIN}" "${M5_DATA}" "${M5_YAML}" "/nonexistent/node-config.sh"

if [[ ${FIRST_EXIT} -eq 0 ]] && [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "M5a: both runs exited 0"
else
  fail "M5a: first exit=${FIRST_EXIT}, second exit=${LAST_EXIT} (expected both 0)"
fi

M5_COUNT=$(get_p2p_bootnodes "${M5_YAML}" | count_lines)
if [[ "${M5_COUNT}" -eq 1 ]]; then
  pass "M5b: p2p.bootnodes has exactly 1 entry after two runs (idempotent)"
else
  fail "M5b: expected 1 bootnode after two runs, got ${M5_COUNT} — duplicate was added"
fi

# =============================================================================
# ── TEST M6: Missing node.yaml — exits non-zero with "не найден" message ──────
# =============================================================================
section "M6: Missing node.yaml on secondary — script exits non-zero with install guidance"
#
# When SECONDARY_NODE_YAML points at a non-existent path the guard inside the
# step-5/7 heredoc must reject the run immediately (exit 1), propagating a
# non-zero exit code from join-network.sh.  The output must contain:
#   • "не найден" — the Russian-language error phrase
#   • "install-validator.sh" or "install-node.sh" — the install guidance
# And the absent file must NOT be created (no silent config synthesis).

M6_DATA="${TMPDIR_TEST}/m6-data"
mkdir -p "${M6_DATA}/chain.db" && touch "${M6_DATA}/chain.db/CURRENT"

# Deliberately do NOT create this file — it is the absent node.yaml.
M6_YAML="${TMPDIR_TEST}/m6-node.yaml"

M6_BIN="${TMPDIR_TEST}/m6-bin"
build_systemctl_stub "${M6_BIN}"
build_ssh_stub "${M6_BIN}" 99999 0
make_stub "${M6_BIN}" "rsync" 'exit 0'
make_stub "${M6_BIN}" "sleep"  'exit 0'

run_join "${TARGET_IP}" "${M6_BIN}" "${M6_DATA}" "${M6_YAML}" "/nonexistent/node-config.sh"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "M6a: join-network.sh exited non-zero (${LAST_EXIT}) when node.yaml is absent"
else
  fail "M6a: expected non-zero exit when node.yaml absent, got 0. Output:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -q "не найден"; then
  pass "M6b: output contains 'не найден' error message"
else
  fail "M6b: 'не найден' not found in output. Got:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -qE "install-validator\.sh|install-node\.sh"; then
  pass "M6c: output contains install guidance (install-validator.sh / install-node.sh)"
else
  fail "M6c: install guidance missing from output. Got:\n${LAST_OUTPUT}"
fi

if [[ ! -f "${M6_YAML}" ]]; then
  pass "M6d: no node.yaml was synthesised at the absent path"
else
  fail "M6d: a node.yaml was wrongly created at ${M6_YAML} — silent config synthesis detected"
fi

# =============================================================================
# ── Summary ───────────────────────────────────────────────────────────────────
# =============================================================================
echo
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo

if [[ ${FAIL} -gt 0 ]]; then
  exit 1
fi
exit 0
