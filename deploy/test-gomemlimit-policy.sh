#!/usr/bin/env bash
# Focused unit tests for the shared host-aware GOMEMLIMIT policy.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY="${SCRIPT_DIR}/gomemlimit-policy.sh"
PASS=0; FAIL=0
pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; FAIL=$((FAIL + 1)); }

run_case() {
  local name="$1" kib="$2" expected="$3"
  local meminfo
  meminfo=$(mktemp)
  printf 'MemTotal:       %s kB\n' "$kib" > "$meminfo"
  unset GOMEMLIMIT_BYTES GOMEMLIMIT_MEMTOTAL_KB
  GOMEMLIMIT_MEMINFO="$meminfo"
  source "$POLICY"
  gomemlimit_resolve
  [[ "$GOMEMLIMIT_BYTES" == "$expected" ]] && pass "$name" || fail "$name (got $GOMEMLIMIT_BYTES, expected $expected)"
  rm -f "$meminfo"
}

run_case "low RAM uses 1.5 GiB floor" 1048576 1610612736
run_case "relay RAM uses 87.5 percent" 4194304 3758096384
run_case "primary-size host is capped canonically" 10485760 5905580032
unset GOMEMLIMIT_MEMINFO GOMEMLIMIT_MEMTOTAL_KB
GOMEMLIMIT_BYTES=2147483648
source "$POLICY"
gomemlimit_resolve
[[ "$GOMEMLIMIT_BYTES" == 2147483648 ]] && pass "explicit override wins" || fail "explicit override wins"
echo "Results: PASS=$PASS FAIL=$FAIL"
[[ "$FAIL" == 0 ]]