#!/usr/bin/env bash
set -euo pipefail
IFS=$'\n\t'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/mainnet-payout-release-gate.sh"
FIXTURE="$(mktemp -d)"
trap 'rm -rf "$FIXTURE"' EXIT

fail() {
  echo "[FAIL] $*" >&2
  exit 1
}
pass() { echo "[PASS] $*"; }

printf 'network: mainnet\n' > "$FIXTURE/mainnet.yaml"
printf 'network: "testnet" # isolated rehearsal\n' > "$FIXTURE/testnet.yaml"
printf 'consensus:\n  activation_height: 10\n' > "$FIXTURE/unknown.yaml"
missing_config="${FIXTURE}/missing.yaml"

unset APEROD_MAINNET_PAYOUT_PREFLIGHT_OK
if mainnet_payout_release_gate "$FIXTURE/mainnet.yaml" "" >/dev/null 2>&1; then
  fail "Mainnet update passed without preflight approval"
fi
pass "Mainnet update is blocked without the preflight assertion"

APEROD_MAINNET_PAYOUT_PREFLIGHT_OK=1
mainnet_payout_release_gate "$FIXTURE/mainnet.yaml" "" ||
  fail "Mainnet update was blocked after a passing preflight assertion"
pass "Mainnet update is allowed after a passing preflight assertion"

unset APEROD_MAINNET_PAYOUT_PREFLIGHT_OK
mainnet_payout_release_gate "$FIXTURE/testnet.yaml" "" ||
  fail "non-Mainnet update was blocked"
pass "non-Mainnet update does not require the Mainnet preflight"

if mainnet_payout_release_gate "$FIXTURE/unknown.yaml" "" >/dev/null 2>&1; then
  fail "unknown network configuration was accepted"
fi
pass "unknown network configuration fails closed"

if mainnet_payout_release_gate "$missing_config" "" >/dev/null 2>&1; then
  fail "missing network configuration was accepted"
fi
pass "missing network configuration fails closed"

echo "All node release-gate tests passed."
