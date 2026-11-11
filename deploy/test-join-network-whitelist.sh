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

# Read each production heredoc rather than testing a copied implementation.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
extract_whitelist_python() {
  python3 - "$1" "$2" <<'PY'
import sys
lines = open(sys.argv[1]).read().splitlines()
starts = [i for i, line in enumerate(lines)
          if "python3 - " in line and sys.argv[2] in line and "<<'PY'" in line]
assert len(starts) == 1, f"Expected one production whitelist writer: {sys.argv[2]}"
start = starts[0] + 1
end = lines.index("PY", start)
body = "\n".join(lines[start:end])
assert "peer_whitelist" in body, "Production writer does not update peer_whitelist"
print(body)
PY
}

# ── Helper: create a minimal node.yaml ────────────────────────────────────────
make_yaml() {
  local path="$1"
  cat > "${path}" <<YAML
network: testnet
data_dir: ${path}.data
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
run_writer() {
  python3 - "$1" "$2" "${DIR}/data" <<< "${ADD_WHITELIST_PY}"
}

echo ""
echo -e "${BOLD}join-network.sh / aperod-join.sh — peer_whitelist tests${NC}"
echo "──────────────────────────────────────────────────────────"

for MODE in push bootstrap api; do
  case "${MODE}" in
    push) ADD_WHITELIST_PY=$(extract_whitelist_python "${SCRIPT_DIR}/join-network.sh" '"${PRIMARY_IP}"') ;;
    bootstrap) ADD_WHITELIST_PY=$(extract_whitelist_python "${SCRIPT_DIR}/join-network.sh" '"${VALIDATOR_IP}"') ;;
    api) ADD_WHITELIST_PY=$(extract_whitelist_python "${SCRIPT_DIR}/aperod-join.sh" '"${PRIMARY_HOST}"') ;;
  esac
  info "Production writer: ${MODE}"

# ── Test 1: Adds validator IP when whitelist is empty ──────────────────────────
info "Test 1: adds validator IP to empty whitelist"
YAML1="${DIR}/test1.yaml"
make_yaml "${YAML1}"
run_writer "${YAML1}" "89.169.53.128"
WL=$(get_whitelist "${YAML1}")
if [[ "${WL}" == "89.169.53.128" ]]; then
  ok "Test 1: validator IP present in peer_whitelist"
else
  fail "Test 1: expected '89.169.53.128' but got '${WL}'"
fi

# ── Test 2: Idempotent — running twice doesn't duplicate ───────────────────────
info "Test 2: idempotent — running twice does not duplicate entry"
BEFORE_REPEAT=$(cat "${YAML1}")
run_writer "${YAML1}" "89.169.53.128"
WL=$(get_whitelist "${YAML1}")
COUNT=$(echo "${WL}" | tr ' ' '\n' | grep -c "89.169.53.128" || true)
if [[ "${COUNT}" -eq 1 && "$(cat "${YAML1}")" == "${BEFORE_REPEAT}" ]]; then
  ok "Test 2: exactly one entry and unchanged config after second run"
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
run_writer "${YAML3}" "89.169.53.128"
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
run_writer "${YAML5}" "77.221.153.86"
WL=$(get_whitelist "${YAML5}")
if [[ "${WL}" == "77.221.153.86" ]]; then
  ok "Test 5: bare IP stored without port suffix"
else
  fail "Test 5: expected '77.221.153.86', got '${WL}'"
fi

info "Test 6: existing sidecars, custom paths, disable flag, corruption"
# Run the production writer with a real sidecar in every configuration mode.
python3 - "${DIR}" "${ADD_WHITELIST_PY}" <<'PY'
import json, os, stat, subprocess, sys, yaml
root, writer = sys.argv[1:]
for variant in ("default", "custom", "relative", "disabled", "corrupt"):
    data = os.path.join(root, variant)
    os.makedirs(data, exist_ok=True)
    sidecar = os.path.join(data, "operator-peers.json" if variant in ("custom", "relative") else "p2p_whitelist.json")
    cfg = {"data_dir": data, "p2p": {"peer_whitelist": ["203.0.113.7"]}}
    if variant == "custom":
        cfg["p2p"]["whitelist_file"] = sidecar
    elif variant == "relative":
        cfg["p2p"]["whitelist_file"] = "operator-peers.json"
    elif variant == "disabled":
        cfg["p2p"]["whitelist_file"] = "-"
    with open(sidecar, "w") as f:
        f.write("broken" if variant == "corrupt" else '["203.0.113.20"]')
    os.chmod(sidecar, 0o640)
    config = os.path.join(data, "node.yaml")
    with open(config, "w") as f:
        yaml.safe_dump(cfg, f)
    before_cfg = open(config, "rb").read()
    before_sidecar = open(sidecar, "rb").read()
    env = os.environ.copy()
    if variant == "relative":
        bin_dir = os.path.join(root, "bin")
        os.makedirs(bin_dir, exist_ok=True)
        systemctl = os.path.join(bin_dir, "systemctl")
        with open(systemctl, "w") as f:
            f.write('#!/bin/sh\nprintf "%s\\n" "$JOIN_TEST_WORKING_DIR"\n')
        os.chmod(systemctl, 0o700)
        env["PATH"] = bin_dir + os.pathsep + env["PATH"]
        env["JOIN_TEST_WORKING_DIR"] = data
    proc = subprocess.run(["python3", "-", config, "198.51.100.8", data],
                          input=writer, text=True, capture_output=True, env=env)
    if variant == "corrupt":
        assert proc.returncode != 0, proc.stdout
        assert open(config, "rb").read() == before_cfg
        assert open(sidecar, "rb").read() == before_sidecar
        continue
    assert proc.returncode == 0, proc.stderr
    assert yaml.safe_load(open(config))["p2p"]["peer_whitelist"] == ["203.0.113.7", "198.51.100.8"]
    expected = ["203.0.113.20"] if variant == "disabled" else ["203.0.113.20", "198.51.100.8"]
    assert json.load(open(sidecar)) == expected
    assert stat.S_IMODE(os.stat(sidecar).st_mode) == 0o640
    again = subprocess.run(["python3", "-", config, "198.51.100.8", data],
                           input=writer, text=True, capture_output=True, env=env)
    assert again.returncode == 0, again.stderr
    assert json.load(open(sidecar)) == expected
PY
ok "Test 6: effective sidecars retain entries and permissions; disabled/corrupt policies respected"
done

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo "──────────────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}${NC} passed, ${RED}${FAIL}${NC} failed out of ${TOTAL} tests"
echo ""
if [[ "${FAIL}" -gt 0 ]]; then
  exit 1
fi
