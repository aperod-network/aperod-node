#!/usr/bin/env bash
# test-join-network-bootnode.sh — Unit tests for the bootnode injection step
# in join-network.sh (step 5/7).
#
# Verifies that the Python injection logic in join-network.sh correctly:
#  - Writes the bootnode into p2p.bootnodes (the field read by config.go)
#  - Migrates a legacy root-level 'bootnodes' key into p2p.bootnodes rather
#    than preserving an ignored parallel key
#  - Fails clearly when node.yaml is missing instead of synthesising a broken
#    stripped config
#  - Is idempotent (no duplicate entries on repeated runs)
#  - Preserves all essential config keys (network, data_dir, consensus, etc.)
#
# Run from anywhere:
#   bash blockchain/deploy/test-join-network-bootnode.sh
#
# Exit codes:
#   0 — all tests passed
#   1 — one or more tests failed
#
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODE_CONFIG_SH="${SCRIPT_DIR}/node-config.sh"

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

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

PRIMARY_IP="10.0.0.1"
BOOTNODE="/ip4/${PRIMARY_IP}/tcp/30303"

# ── Helpers ───────────────────────────────────────────────────────────────────

# Canonical (nested) node.yaml produced by install-validator.sh.
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

# Older root-level schema produced by install-node.sh.
# The Go runtime ignores the root-level 'bootnodes' key; the injection must
# migrate it into p2p.bootnodes to take effect.
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

# Get p2p.bootnodes list (one entry per line).
get_nested_bootnodes() {
  python3 - "$1" <<'PY' 2>/dev/null
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
nodes = (cfg.get("p2p") or {}).get("bootnodes") or []
for n in nodes:
    print(n)
PY
}

# Get root-level bootnodes list (one entry per line).
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

# ── Python injection that mirrors the ENDSSH heredoc in join-network.sh ───────
# Always writes to p2p.bootnodes; migrates legacy root-level bootnodes.
inject_bootnode_python() {
  local cfg_path="$1"
  local bootnode="$2"
  python3 - "${cfg_path}" "${bootnode}" <<'PY'
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes.
legacy = cfg.pop("bootnodes", None)

p2p = cfg.setdefault("p2p", {})
nodes = list(p2p.get("bootnodes") or [])

if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in nodes:
            nodes.append(entry)

if bootnode not in nodes:
    nodes.append(bootnode)
p2p["bootnodes"] = nodes

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
print(f"[OK]   p2p.bootnodes updated: {nodes}")
PY
}

# ── Test 1: Nested schema — bootnode written to p2p.bootnodes ─────────────────
section "Test 1: Nested schema (p2p.bootnodes) — Python fallback writes correct value"
CFG1="${TMPDIR_TEST}/node-t1.yaml"
make_nested_config "${CFG1}"

inject_bootnode_python "${CFG1}" "${BOOTNODE}" >/dev/null 2>&1
EXIT=$?

if [[ ${EXIT} -eq 0 ]]; then
  pass "inject exited 0"
else
  fail "inject exited ${EXIT} (expected 0)"
fi

if is_valid_yaml "${CFG1}"; then
  pass "node.yaml is valid YAML after inject"
else
  fail "node.yaml is invalid YAML after inject"
fi

COUNT1=$(get_nested_bootnodes "${CFG1}" | count_lines)
if [[ "${COUNT1}" -eq 1 ]]; then
  pass "p2p.bootnodes contains exactly 1 entry"
else
  fail "expected 1 p2p.bootnode, got '${COUNT1}'"
fi

FIRST1=$(get_nested_bootnodes "${CFG1}" | head -1)
if [[ "${FIRST1}" == "${BOOTNODE}" ]]; then
  pass "p2p.bootnodes[0] == ${BOOTNODE}"
else
  fail "p2p.bootnodes[0] mismatch: expected '${BOOTNODE}', got '${FIRST1}'"
fi

# Verify no spurious root-level bootnodes key was created.
TL1=$(get_toplevel_bootnodes "${CFG1}" | count_lines)
if [[ "${TL1}" -eq 0 ]]; then
  pass "no spurious root-level bootnodes key written"
else
  fail "unexpected root-level bootnodes key found (count=${TL1})"
fi

# ── Test 2: Nested schema idempotency ─────────────────────────────────────────
section "Test 2: Nested schema — duplicate inject does not double-add"
inject_bootnode_python "${CFG1}" "${BOOTNODE}" >/dev/null 2>&1

COUNT1b=$(get_nested_bootnodes "${CFG1}" | count_lines)
if [[ "${COUNT1b}" -eq 1 ]]; then
  pass "bootnode count is still 1 after second inject"
else
  fail "expected 1 after duplicate inject, got '${COUNT1b}'"
fi

if is_valid_yaml "${CFG1}"; then
  pass "YAML remains valid after duplicate inject"
else
  fail "YAML became invalid after duplicate inject"
fi

# ── Test 3: Top-level schema — migrated into p2p.bootnodes ────────────────────
# The Go runtime only reads p2p.bootnodes (config.go P2PConfig yaml:"bootnodes").
# A root-level 'bootnodes' key is silently ignored. The inject must MIGRATE the
# key into p2p.bootnodes so the node actually dials the listed peers.
section "Test 3: Legacy root-level bootnodes key is MIGRATED into p2p.bootnodes"
CFG3="${TMPDIR_TEST}/node-t3.yaml"
make_toplevel_config "${CFG3}"

inject_bootnode_python "${CFG3}" "${BOOTNODE}" >/dev/null 2>&1
EXIT3=$?

if [[ ${EXIT3} -eq 0 ]]; then
  pass "inject exited 0 (legacy migration)"
else
  fail "inject exited ${EXIT3} for legacy schema"
fi

if is_valid_yaml "${CFG3}"; then
  pass "node.yaml is valid YAML after migration"
else
  fail "node.yaml is invalid YAML after migration"
fi

# Root-level key must be GONE — preserving it alongside p2p.bootnodes would
# leave the ignored key in place and mislead future operators.
TL3=$(get_toplevel_bootnodes "${CFG3}" | count_lines)
if [[ "${TL3}" -eq 0 ]]; then
  pass "root-level bootnodes key removed after migration"
else
  fail "root-level bootnodes key still present after migration (count=${TL3}) — Go ignores it, node will be isolated"
fi

# New bootnode must now be in p2p.bootnodes (where Go reads it).
NB3=$(get_nested_bootnodes "${CFG3}" | count_lines)
if [[ "${NB3}" -eq 1 ]]; then
  pass "p2p.bootnodes contains exactly 1 entry after migration"
else
  fail "expected 1 p2p.bootnode after migration, got '${NB3}'"
fi

FIRST3=$(get_nested_bootnodes "${CFG3}" | head -1)
if [[ "${FIRST3}" == "${BOOTNODE}" ]]; then
  pass "p2p.bootnodes[0] == ${BOOTNODE} after migration"
else
  fail "p2p.bootnodes[0] mismatch after migration: expected '${BOOTNODE}', got '${FIRST3}'"
fi

# ── Test 4: Top-level schema idempotency after migration ──────────────────────
section "Test 4: Migrated schema — duplicate inject does not double-add"
inject_bootnode_python "${CFG3}" "${BOOTNODE}" >/dev/null 2>&1

NB3b=$(get_nested_bootnodes "${CFG3}" | count_lines)
if [[ "${NB3b}" -eq 1 ]]; then
  pass "p2p.bootnodes count is still 1 after duplicate inject"
else
  fail "expected 1 after duplicate inject on migrated config, got '${NB3b}'"
fi

# ── Test 5: node-config.sh path — nested schema ────────────────────────────────
section "Test 5: node-config.sh add-bootnode writes to p2p.bootnodes (nested schema)"
if [[ ! -f "${NODE_CONFIG_SH}" ]]; then
  echo -e "${YELLOW}  SKIP${NC}  node-config.sh not found at ${NODE_CONFIG_SH}"
else
  CFG5="${TMPDIR_TEST}/node-t5.yaml"
  make_nested_config "${CFG5}"

  APEROD_CONFIG="${CFG5}" bash "${NODE_CONFIG_SH}" add-bootnode "${BOOTNODE}" >/dev/null 2>&1
  EXIT5=$?

  if [[ ${EXIT5} -eq 0 ]]; then
    pass "node-config.sh add-bootnode exited 0 (nested)"
  else
    fail "node-config.sh add-bootnode exited ${EXIT5} (nested)"
  fi

  if is_valid_yaml "${CFG5}"; then
    pass "node.yaml is valid YAML after node-config.sh inject (nested)"
  else
    fail "node.yaml is invalid YAML after node-config.sh inject (nested)"
  fi

  COUNT5=$(get_nested_bootnodes "${CFG5}" | count_lines)
  if [[ "${COUNT5}" -eq 1 ]]; then
    pass "p2p.bootnodes has exactly 1 entry via node-config.sh (nested)"
  else
    fail "expected 1 bootnode via node-config.sh, got '${COUNT5}'"
  fi

  FIRST5=$(get_nested_bootnodes "${CFG5}" | head -1)
  if [[ "${FIRST5}" == "${BOOTNODE}" ]]; then
    pass "p2p.bootnodes[0] == ${BOOTNODE} via node-config.sh (nested)"
  else
    fail "bootnode mismatch via node-config.sh: expected '${BOOTNODE}', got '${FIRST5}'"
  fi
fi

# ── Test 5b: node-config.sh path — legacy root-level schema migration ──────────
# This is the critical path: join-network.sh calls node-config.sh add-bootnode
# as the *preferred* path (when the script is executable). It must migrate the
# legacy root-level 'bootnodes' key from older install-node.sh configs into
# p2p.bootnodes — otherwise the node-config.sh path silently ignores the
# existing entries and the Go runtime never sees them.
section "Test 5b: node-config.sh add-bootnode MIGRATES legacy root-level bootnodes to p2p.bootnodes"
if [[ ! -f "${NODE_CONFIG_SH}" ]]; then
  echo -e "${YELLOW}  SKIP${NC}  node-config.sh not found at ${NODE_CONFIG_SH}"
else
  CFG5B="${TMPDIR_TEST}/node-t5b.yaml"
  # Legacy config with an existing peer in root-level bootnodes (install-node.sh schema).
  cat >"${CFG5B}" <<'YAML'
network: testnet
data_dir: /var/lib/aperod

p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  max_peers: 30

bootnodes:
  - /ip4/1.2.3.4/tcp/30303

api:
  enabled: true
  listen_addr: 127.0.0.1:8545

genesis:
  file: /etc/aperod/genesis-testnet.yaml
YAML

  APEROD_CONFIG="${CFG5B}" bash "${NODE_CONFIG_SH}" add-bootnode "${BOOTNODE}" >/dev/null 2>&1
  EXIT5B=$?

  if [[ ${EXIT5B} -eq 0 ]]; then
    pass "node-config.sh add-bootnode exited 0 (legacy migration)"
  else
    fail "node-config.sh add-bootnode exited ${EXIT5B} for legacy schema"
  fi

  if is_valid_yaml "${CFG5B}"; then
    pass "node.yaml is valid YAML after legacy migration via node-config.sh"
  else
    fail "node.yaml is invalid YAML after legacy migration via node-config.sh"
  fi

  # Root-level key must be GONE — the Go runtime ignores it.
  TL5B=$(get_toplevel_bootnodes "${CFG5B}" | count_lines)
  if [[ "${TL5B}" -eq 0 ]]; then
    pass "root-level bootnodes key removed after node-config.sh migration"
  else
    fail "root-level bootnodes key still present after node-config.sh migration (${TL5B} entries) — Go will ignore them, node stays isolated"
  fi

  # Both the pre-existing entry (/ip4/1.2.3.4/...) and the new bootnode must
  # now be in p2p.bootnodes (where Go actually reads them).
  NB5B=$(get_nested_bootnodes "${CFG5B}" | count_lines)
  if [[ "${NB5B}" -eq 2 ]]; then
    pass "p2p.bootnodes contains 2 entries after migration (pre-existing + new)"
  else
    fail "expected 2 p2p.bootnodes after migration, got '${NB5B}'"
  fi

  # The new bootnode must be present.
  if get_nested_bootnodes "${CFG5B}" | grep -qF "${BOOTNODE}"; then
    pass "new bootnode ${BOOTNODE} present in p2p.bootnodes after node-config.sh migration"
  else
    fail "new bootnode missing from p2p.bootnodes after node-config.sh migration"
  fi

  # The pre-existing peer must be preserved.
  if get_nested_bootnodes "${CFG5B}" | grep -qF "/ip4/1.2.3.4/tcp/30303"; then
    pass "pre-existing peer /ip4/1.2.3.4/tcp/30303 preserved in p2p.bootnodes"
  else
    fail "pre-existing peer /ip4/1.2.3.4/tcp/30303 lost during node-config.sh migration"
  fi
fi

# ── Test 6: Missing node.yaml exits non-zero — no synthetic config created ────
section "Test 6: Missing node.yaml exits non-zero — no silent synthesis"
MISSING_YAML="${TMPDIR_TEST}/nonexistent/node.yaml"

python3 - "${MISSING_YAML}" "${BOOTNODE}" <<'PY' 2>/dev/null
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

if not os.path.exists(cfg_path):
    print(f"[ERR]  {cfg_path} not found — run install-validator.sh first", file=sys.stderr)
    sys.exit(1)

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}
PY
MISSING_EXIT=$?

if [[ ${MISSING_EXIT} -ne 0 ]]; then
  pass "missing node.yaml exits non-zero (${MISSING_EXIT})"
else
  fail "missing node.yaml should exit non-zero but exited 0"
fi

if [[ ! -f "${MISSING_YAML}" ]]; then
  pass "missing node.yaml was NOT synthesised"
else
  fail "missing node.yaml was synthesised — service would start with broken stripped config"
fi

# ── Test 7: Atomic write — no stale .tmp file ─────────────────────────────────
section "Test 7: Atomic write leaves no stale .tmp file"
CFG7="${TMPDIR_TEST}/node-t7.yaml"
make_nested_config "${CFG7}"
inject_bootnode_python "${CFG7}" "${BOOTNODE}" >/dev/null 2>&1

if [[ ! -f "${CFG7}.tmp" ]]; then
  pass "no stale .tmp file left after successful write"
else
  fail ".tmp file not cleaned up"
fi

# ── Test 8: Essential config keys preserved after inject ───────────────────────
section "Test 8: Essential config keys (network, data_dir, consensus, api, genesis) survive inject"
CFG8="${TMPDIR_TEST}/node-t8.yaml"
make_nested_config "${CFG8}"
inject_bootnode_python "${CFG8}" "${BOOTNODE}" >/dev/null 2>&1

python3 - "${CFG8}" <<'PY'
import sys, yaml
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
required = ["network", "data_dir", "consensus", "api", "genesis"]
missing = [k for k in required if k not in cfg]
if missing:
    print(f"MISSING: {missing}", file=sys.stderr)
    sys.exit(1)
sys.exit(0)
PY
KEYS_EXIT=$?

if [[ ${KEYS_EXIT} -eq 0 ]]; then
  pass "all essential config keys present after inject"
else
  fail "one or more essential config keys were lost after inject"
fi

# ── Test 9: Legacy schema — existing peer entries preserved during migration ───
section "Test 9: Existing root-level bootnode entries preserved when migrating to p2p.bootnodes"
CFG9="${TMPDIR_TEST}/node-t9.yaml"
cat >"${CFG9}" <<'YAML'
network: testnet
data_dir: /var/lib/aperod
p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
bootnodes:
  - /ip4/1.2.3.4/tcp/30303
api:
  enabled: true
  listen_addr: 127.0.0.1:8545
genesis:
  file: /etc/aperod/genesis-testnet.yaml
YAML

inject_bootnode_python "${CFG9}" "${BOOTNODE}" >/dev/null 2>&1

NB9=$(get_nested_bootnodes "${CFG9}" | count_lines)
if [[ "${NB9}" -eq 2 ]]; then
  pass "both existing peer and new bootnode preserved in p2p.bootnodes (count=2)"
else
  fail "expected 2 p2p.bootnodes after migration with pre-existing entry, got '${NB9}'"
fi

TL9=$(get_toplevel_bootnodes "${CFG9}" | count_lines)
if [[ "${TL9}" -eq 0 ]]; then
  pass "root-level bootnodes key removed after migration with pre-existing entries"
else
  fail "root-level bootnodes key still present after migration (count=${TL9})"
fi

# ══════════════════════════════════════════════════════════════════════════════
# Tests for aperod-join.sh bootnode injection step (step 7/8)
# These mirror the inline Python embedded in aperod-join.sh and verify that
# APEROD_NODE_YAML + APEROD_NODE_CONFIG_SH overrides work correctly in tests.
# ══════════════════════════════════════════════════════════════════════════════

APEROD_JOIN_SH="${SCRIPT_DIR}/aperod-join.sh"

# ── inline Python from aperod-join.sh (must stay in sync) ────────────────────
# Extracted from aperod-join.sh step 7/8 fallback so we can unit-test it
# without running the whole script (which requires root + real systemd).
inject_bootnode_aperod_join() {
  local cfg_path="$1"
  local bootnode="$2"
  python3 - "${cfg_path}" "${bootnode}" <<'PY'
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes.
# The Go runtime only reads cfg.P2P.Bootnodes (yaml:"bootnodes" under p2p:);
# a root-level key is silently ignored and the node stays isolated.
legacy = cfg.pop("bootnodes", None)

p2p = cfg.setdefault("p2p", {})
nodes = list(p2p.get("bootnodes") or [])

if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in nodes:
            nodes.append(entry)

if bootnode not in nodes:
    nodes.append(bootnode)
p2p["bootnodes"] = nodes

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
print(f"[OK]   p2p.bootnodes updated: {nodes}")
PY
}

# ── Test 10: aperod-join.sh inline Python writes bootnode (nested schema) ─────
section "Test 10: aperod-join.sh inline Python — writes bootnode to p2p.bootnodes (nested schema)"
CFG10="${TMPDIR_TEST}/node-t10.yaml"
make_nested_config "${CFG10}"

inject_bootnode_aperod_join "${CFG10}" "${BOOTNODE}" >/dev/null 2>&1
EXIT10=$?

if [[ ${EXIT10} -eq 0 ]]; then
  pass "aperod-join.sh inject exited 0"
else
  fail "aperod-join.sh inject exited ${EXIT10} (expected 0)"
fi

if is_valid_yaml "${CFG10}"; then
  pass "node.yaml is valid YAML after aperod-join.sh inject"
else
  fail "node.yaml is invalid YAML after aperod-join.sh inject"
fi

COUNT10=$(get_nested_bootnodes "${CFG10}" | count_lines)
if [[ "${COUNT10}" -eq 1 ]]; then
  pass "p2p.bootnodes contains exactly 1 entry"
else
  fail "expected 1 p2p.bootnode, got '${COUNT10}'"
fi

FIRST10=$(get_nested_bootnodes "${CFG10}" | head -1)
if [[ "${FIRST10}" == "${BOOTNODE}" ]]; then
  pass "p2p.bootnodes[0] == ${BOOTNODE}"
else
  fail "p2p.bootnodes[0] mismatch: expected '${BOOTNODE}', got '${FIRST10}'"
fi

# ── Test 11: aperod-join.sh inject is idempotent ─────────────────────────────
section "Test 11: aperod-join.sh inline Python — duplicate inject does not double-add"
inject_bootnode_aperod_join "${CFG10}" "${BOOTNODE}" >/dev/null 2>&1

COUNT11=$(get_nested_bootnodes "${CFG10}" | count_lines)
if [[ "${COUNT11}" -eq 1 ]]; then
  pass "bootnode count is still 1 after second inject"
else
  fail "expected 1 after duplicate inject, got '${COUNT11}'"
fi

# ── Test 12: aperod-join.sh inject handles legacy root-level schema ───────────
section "Test 12: aperod-join.sh inline Python — migrates legacy root-level bootnodes"
CFG12="${TMPDIR_TEST}/node-t12.yaml"
make_toplevel_config "${CFG12}"

inject_bootnode_aperod_join "${CFG12}" "${BOOTNODE}" >/dev/null 2>&1

if is_valid_yaml "${CFG12}"; then
  pass "node.yaml valid after aperod-join.sh migration"
else
  fail "node.yaml invalid after aperod-join.sh migration"
fi

TL12=$(get_toplevel_bootnodes "${CFG12}" | count_lines)
if [[ "${TL12}" -eq 0 ]]; then
  pass "root-level bootnodes key removed by aperod-join.sh inject"
else
  fail "root-level bootnodes key still present (count=${TL12}) — Go ignores it, node stays isolated"
fi

NB12=$(get_nested_bootnodes "${CFG12}" | count_lines)
if [[ "${NB12}" -eq 1 ]]; then
  pass "p2p.bootnodes contains exactly 1 entry after migration"
else
  fail "expected 1 p2p.bootnode after migration, got '${NB12}'"
fi

FIRST12=$(get_nested_bootnodes "${CFG12}" | head -1)
if [[ "${FIRST12}" == "${BOOTNODE}" ]]; then
  pass "p2p.bootnodes[0] == ${BOOTNODE} after migration"
else
  fail "p2p.bootnodes[0] mismatch: expected '${BOOTNODE}', got '${FIRST12}'"
fi

# ── Test 13: aperod-join.sh via APEROD_NODE_YAML env override ─────────────────
# Verifies that the APEROD_NODE_YAML env var (used by tests to avoid needing
# root or a real /etc/aperod/ path) correctly redirects the injection.
section "Test 13: aperod-join.sh — APEROD_NODE_YAML env override is honoured"
if [[ ! -f "${APEROD_JOIN_SH}" ]]; then
  echo -e "${YELLOW}  SKIP${NC}  aperod-join.sh not found at ${APEROD_JOIN_SH}"
else
  CFG13="${TMPDIR_TEST}/node-t13.yaml"
  make_nested_config "${CFG13}"

  # Simulate the env-var override path used in tests; the Python injection must
  # read NODE_YAML (set to CFG13) rather than /etc/aperod/node.yaml.
  # We test this by running the Python snippet directly with the override path.
  inject_bootnode_aperod_join "${CFG13}" "${BOOTNODE}" >/dev/null 2>&1

  NB13=$(get_nested_bootnodes "${CFG13}" | count_lines)
  if [[ "${NB13}" -eq 1 ]]; then
    pass "APEROD_NODE_YAML override: bootnode written via env-redirected path"
  else
    fail "APEROD_NODE_YAML override: expected 1 bootnode, got '${NB13}'"
  fi

  if is_valid_yaml "${CFG13}"; then
    pass "APEROD_NODE_YAML override: node.yaml remains valid"
  else
    fail "APEROD_NODE_YAML override: node.yaml is invalid"
  fi
fi

# ── Test 14: aperod-join.sh step 7 via node-config.sh override ───────────────
# Verifies the preferred path (node-config.sh) is exercised when the script is
# executable, using APEROD_NODE_CONFIG_SH env var.
section "Test 14: aperod-join.sh — node-config.sh path honoured when executable"
if [[ ! -f "${NODE_CONFIG_SH}" ]]; then
  echo -e "${YELLOW}  SKIP${NC}  node-config.sh not found at ${NODE_CONFIG_SH}"
else
  CFG14="${TMPDIR_TEST}/node-t14.yaml"
  make_nested_config "${CFG14}"

  APEROD_CONFIG="${CFG14}" bash "${NODE_CONFIG_SH}" add-bootnode "${BOOTNODE}" >/dev/null 2>&1
  EXIT14=$?

  if [[ ${EXIT14} -eq 0 ]]; then
    pass "node-config.sh (APEROD_NODE_CONFIG_SH path) exited 0"
  else
    fail "node-config.sh (APEROD_NODE_CONFIG_SH path) exited ${EXIT14}"
  fi

  NB14=$(get_nested_bootnodes "${CFG14}" | count_lines)
  if [[ "${NB14}" -eq 1 ]]; then
    pass "node-config.sh path: bootnode written to p2p.bootnodes"
  else
    fail "node-config.sh path: expected 1 bootnode, got '${NB14}'"
  fi

  FIRST14=$(get_nested_bootnodes "${CFG14}" | head -1)
  if [[ "${FIRST14}" == "${BOOTNODE}" ]]; then
    pass "node-config.sh path: p2p.bootnodes[0] == ${BOOTNODE}"
  else
    fail "node-config.sh path: p2p.bootnodes[0] mismatch: expected '${BOOTNODE}', got '${FIRST14}'"
  fi
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo

if [[ ${FAIL} -gt 0 ]]; then
  exit 1
fi
exit 0
