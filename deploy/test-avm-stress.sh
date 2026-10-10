#!/usr/bin/env bash
set -euo pipefail

if [[ "${APEROD_RUN_AVM_STRESS:-0}" != "1" ]]; then
  echo "[SKIP] isolated AVM stress test is opt-in; set APEROD_RUN_AVM_STRESS=1"
  echo "SKIP_SUMMARY: AVM stress test requires explicit opt-in"
  exit 77
fi

if ! command -v go >/dev/null 2>&1; then
  echo "[SKIP] Go toolchain is unavailable"
  echo "SKIP_SUMMARY: Go toolchain unavailable"
  exit 77
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}/.."
duration="${APEROD_AVM_STRESS_DURATION:-30s}"
workers="${APEROD_AVM_STRESS_WORKERS:-8}"
APEROD_RUN_AVM_STRESS=1 \
APEROD_AVM_STRESS_DURATION="${duration}" \
APEROD_AVM_STRESS_WORKERS="${workers}" \
GOSUMDB=sum.golang.org \
GOTOOLCHAIN=go1.25.13+auto \
go test ./consensus -run '^TestAVMSpamDoesNotDelayBlockProduction$' -count=1 -v -timeout=7m