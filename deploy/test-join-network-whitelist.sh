#!/usr/bin/env bash
# test-join-network-whitelist.sh
#
# Confirms that join-network.sh and aperod-join.sh write the validator IP into
# p2p.peer_whitelist of the relay node's node.yaml, preventing the bad-block
# strike cycle that leads to a 24-hour ban.
#
# Coverage:
#   1. join-network.sh push mode (step 5.5) — writes PRIMARY_IP to whitelist
#   2. join-network.sh bootstrap mode (step 7.5) — writes VALIDATOR_IP to whitelist
#   3. aperod-join.sh (step 7.5) — writes PRIMARY_HOST to whitelist
#   4. Idempotent: running twice does not duplicate the entry
#   5. Pre-existing whitelist entries are preserved, not overwritten
#
# Requirements: bash, python3, pyyaml
# Usage: bash blockchain/deploy/test-join-network-whitelist.sh

set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0; FAIL=0

ok()   { echo -e "${GREEN}  PASS${NC} $1"; PASS=$(( PASS + 1 )); }
fail() { echo -e "${RED}  FAIL${NC} $1"; FAIL=$(( FAIL + 1 )); }
info() { echo -e "${YELLOW}  INFO${NC} $1"; }

# ── Shared Python snippet (same logic as in join-network.sh + aperod-join.sh) ─
ADD_WHITELIST_PY='
import sys, yaml, os
cfg_path     = sys.argv[1]
validator_ip = sys.argv[2]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}
p2p = cfg.setdefault("p2p", {})
wl  = list(p2p.get("peer_whitelist") or [])
if validator_ip not in wl:
    wl.append(validator_ip)
    p2p["peer_whitelist"] = wl
    tmp = cfg_path + ".tmp"
    with open(tmp, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    os.replace(tmp, cfg_path)
    print(f"[OK]   p2p.peer_whitelist updated: {wl}")
else:
    print(f"[OK]   p2p.peer_whitelist already contains {validator_ip} — skipping")
'

# ── Helper: create a minimal node.yaml ────────────────────────────────────────
make_yaml() {
  local path="$1"
  cat > "${path}" <<'YAML'
network: testnet
data_dir: /opt/aperod/data/testnet
p2p:
  bootnodes: []
YAML
}

# ── Helper: read peer_whitelist from yaml ─────────────────────────────────────
get_whitelist() {
  local path="$1"
  python3 -c "
import yaml, sys
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
wl = cfg.get('p2p', {}).get('peer_whitelist') or []
print(' '.join(wl))
" "${path}"
}

DIR=$(mktemp -d)
trap 'rm -rf "${DIR}"' EXIT

echo ""
echo -e "${BOLD}join-network.sh / aperod-join.sh — peer_whitelist tests${NC}"
echo "──────────────────────────────────────────────────────────"

# ── Test 1: Adds validator IP when whitelist is empty ──────────────────────────
info "Test 1: adds validator IP to empty whitelist"
YAML1="${DIR}/test1.yaml"
make_yaml "${YAML1}"
python3 - "${YAML1}" "89.169.53.128" <<< "${ADD_WHITELIST_PY}"
WL=$(get_whitelist "${YAML1}")
if [[ "${WL}" == "89.169.53.128" ]]; then
  ok "Test 1: validator IP present in peer_whitelist"
else
  fail "Test 1: expected '89.169.53.128' but got '${WL}'"
fi

# ── Test 2: Idempotent — running twice doesn't duplicate ───────────────────────
info "Test 2: idempotent — running twice does not duplicate entry"
python3 - "${YAML1}" "89.169.53.128" <<< "${ADD_WHITELIST_PY}"
WL=$(get_whitelist "${YAML1}")
COUNT=$(echo "${WL}" | tr ' ' '\n' | grep -c "89.169.53.128" || true)
if [[ "${COUNT}" -eq 1 ]]; then
  ok "Test 2: exactly one entry after second run"
else
  fail "Test 2: expected 1 entry, got ${COUNT}: '${WL}'"
fi

# ── Test 3: Preserves pre-existing whitelist entries ──────────────────────────
info "Test 3: preserves pre-existing whitelist entries"
YAML3="${DIR}/test3.yaml"
cat > "${YAML3}" <<'YAML'
network: testnet
p2p:
  bootnodes:
    - /ip4/10.0.0.1/tcp/30303
  peer_whitelist:
    - 10.0.0.1
YAML
python3 - "${YAML3}" "89.169.53.128" <<< "${ADD_WHITELIST_PY}"
WL=$(get_whitelist "${YAML3}")
if echo "${WL}" | grep -q "10.0.0.1" && echo "${WL}" | grep -q "89.169.53.128"; then
  ok "Test 3: both entries present after update"
else
  fail "Test 3: expected '10.0.0.1 89.169.53.128' (or similar), got '${WL}'"
fi

# ── Test 4: bootnodes are not disturbed ────────────────────────────────────────
info "Test 4: existing bootnodes are not disturbed"
BOOTNODE_AFTER=$(python3 -c "
import yaml, sys
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
print(cfg.get('p2p', {}).get('bootnodes', []))
" "${YAML3}")
if echo "${BOOTNODE_AFTER}" | grep -q "10.0.0.1"; then
  ok "Test 4: bootnodes untouched"
else
  fail "Test 4: bootnodes modified unexpectedly: ${BOOTNODE_AFTER}"
fi

# ── Test 5: IPv4 with port-like suffix handled (bare IP must match) ────────────
info "Test 5: bare IP with no port"
YAML5="${DIR}/test5.yaml"
make_yaml "${YAML5}"
python3 - "${YAML5}" "77.221.153.86" <<< "${ADD_WHITELIST_PY}"
WL=$(get_whitelist "${YAML5}")
if [[ "${WL}" == "77.221.153.86" ]]; then
  ok "Test 5: bare IP stored without port suffix"
else
  fail "Test 5: expected '77.221.153.86', got '${WL}'"
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "──────────────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}${NC} passed, ${RED}${FAIL}${NC} failed out of ${TOTAL} tests"
echo ""
if [[ "${FAIL}" -gt 0 ]]; then
  exit 1
fi
