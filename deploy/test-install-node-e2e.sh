#!/usr/bin/env bash
# Complete pinned-source installation in a disposable real-systemd CI guest.
# No fake git/go/make/provenance/systemctl. Docker absence is incomplete coverage.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec bash "$DIR/test-node-service-lifecycle.sh" install
