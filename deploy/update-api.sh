#!/usr/bin/env bash
# update-api.sh — Pull latest source, rebuild TypeScript, and restart the API.
#
# Run this script instead of bare `pm2 restart aperod-api` after every git pull.
# Bare restart replays the OLD compiled binary; new source changes are silently
# ignored until a rebuild is done.
#
# ONE-TIME PREREQUISITE (run once as root if dist/ was ever built by root):
#   chown -R aperod:aperod /opt/aperod/artifacts/api-server/dist/
#
# Usage:
#   sudo bash /opt/aperod/blockchain/deploy/update-api.sh
#
# The script must be run as root (or via sudo) so it can switch to the
# `aperod` user for git and pnpm operations, then call pm2 as root.
#
set -euo pipefail

APEROD_DIR="/opt/aperod"
API_FILTER="@workspace/api-server"
PM2_APP="aperod-api"

echo "==> [1/3] Pulling latest source as aperod..."
sudo -u aperod git -C "$APEROD_DIR" pull

echo "==> [2/3] Rebuilding TypeScript as aperod..."
sudo -u aperod bash -c "cd '$APEROD_DIR' && pnpm --filter '$API_FILTER' run build"

echo "==> [3/3] Restarting PM2 process '$PM2_APP'..."
pm2 restart "$PM2_APP"

echo ""
echo "✓ Update complete. New build is live."
echo "  Check logs: pm2 logs $PM2_APP --lines 50"
