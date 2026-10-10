#!/usr/bin/env bash
# Independent old/new histories, real build/service, retention and health rollback.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$DIR/test-node-service-lifecycle.sh" update
