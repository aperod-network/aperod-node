#!/usr/bin/env bash
set -euo pipefail

if [[ "${APEROD_RUN_AVM_VERTICAL_SLICE:-0}" != "1" ]]; then
  echo "[SKIP] AVM two-validator vertical slice is opt-in; set APEROD_RUN_AVM_VERTICAL_SLICE=1"
  exit 77
fi

if ! command -v go >/dev/null 2>&1; then
  echo "[SKIP] Go toolchain is unavailable"
  exit 77
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}/.."
GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.25.13+auto go test ./consensus -run '^TestAVMTwoValidatorEngineIntegration$' -count=1 -v