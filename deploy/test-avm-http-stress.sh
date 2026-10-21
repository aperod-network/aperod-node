#!/usr/bin/env bash
set -euo pipefail

if [[ "${APEROD_RUN_AVM_STRESS:-0}" != "1" ]]; then
  echo "[SKIP] isolated AVM HTTP stress requires APEROD_RUN_AVM_STRESS=1"
  echo "SKIP_SUMMARY: AVM HTTP stress requires explicit opt-in"
  exit 77
fi
if [[ "$(uname -s)" != "Linux" ]]; then
  echo "[SKIP] HTTP stress resource accounting requires Linux /proc and getrusage"
  echo "SKIP_SUMMARY: Linux resource accounting unavailable"
  exit 77
fi
if ! command -v go >/dev/null 2>&1; then
  echo "[SKIP] Go toolchain is unavailable"
  echo "SKIP_SUMMARY: Go toolchain unavailable"
  exit 77
fi

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}/.."
scratch="$(mktemp -d "${TMPDIR:-/tmp}/aperod-avm-http-stress.XXXXXX")"
trap 'rm -rf -- "${scratch}"' EXIT
APEROD_RUN_AVM_STRESS=1 \
APEROD_AVM_STRESS_DURATION="${APEROD_AVM_STRESS_DURATION:-2m}" \
APEROD_AVM_STRESS_WORKERS="${APEROD_AVM_STRESS_WORKERS:-12}" \
GOMAXPROCS="${GOMAXPROCS:-2}" \
GOMEMLIMIT="${GOMEMLIMIT:-512MiB}" \
GOCACHE="${GOCACHE:-${scratch}/go-cache}" \
TMPDIR="${scratch}" \
GOSUMDB=sum.golang.org \
GOTOOLCHAIN=go1.26.10+auto \
go test -p 1 ./consensus -run '^TestAVMMixedHTTPStress$' -count=1 -v -timeout=8m
