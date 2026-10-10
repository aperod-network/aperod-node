#!/usr/bin/env bash
# Real upgrade entrypoint, explicit baseline approval and retained runtime state.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$DIR/test-node-service-lifecycle.sh" upgrade
