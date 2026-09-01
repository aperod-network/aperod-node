#!/usr/bin/env bash
# enable-pruning.sh — adds light-pruning config to /etc/aperod/node.yaml
# and restarts the node so it takes effect immediately.
#
# Usage:
#   sudo bash /opt/aperod/blockchain/deploy/enable-pruning.sh [--keep-blocks N]
#
# Default keep_blocks = 200000 (~6.5 days at 3 s/block).

set -euo pipefail

CONFIG_FILE="${APEROD_CONFIG:-/etc/aperod/node.yaml}"
NODE_SERVICE="aperod-node"
KEEP_BLOCKS=200000

for arg in "$@"; do
  case "$arg" in
    --keep-blocks=*) KEEP_BLOCKS="${arg#*=}" ;;
  esac
done

if ! [[ "$KEEP_BLOCKS" =~ ^[0-9]+$ ]]; then
  echo "error: --keep-blocks must be a positive integer" >&2
  exit 1
fi

echo "enable-pruning: config = $CONFIG_FILE"
echo "enable-pruning: keep_blocks = $KEEP_BLOCKS"

# ── Check if pruning is already configured ────────────────────────────────────
if grep -q '^\s*pruning:' "$CONFIG_FILE" 2>/dev/null; then
  # Update existing mode line if it says archive.
  if grep -qE '^\s*mode:\s*archive' "$CONFIG_FILE"; then
    sed -i 's/^\(\s*mode:\s*\)archive/\1light/' "$CONFIG_FILE"
    echo "enable-pruning: switched mode: archive → light"
  else
    echo "enable-pruning: pruning already present in config — no changes needed."
  fi
  # Update keep_blocks if present.
  if grep -q '^\s*keep_blocks:' "$CONFIG_FILE"; then
    sed -i "s/^\(\s*keep_blocks:\s*\)[0-9]*/\1${KEEP_BLOCKS}/" "$CONFIG_FILE"
    echo "enable-pruning: updated keep_blocks = $KEEP_BLOCKS"
  fi
else
  # Append pruning block to end of file.
  cat >> "$CONFIG_FILE" <<YAML

pruning:
  mode: light
  keep_blocks: ${KEEP_BLOCKS}
YAML
  echo "enable-pruning: appended pruning block to $CONFIG_FILE"
fi

echo "enable-pruning: restarting $NODE_SERVICE ..."
systemctl restart "$NODE_SERVICE"
sleep 3
journalctl -u "$NODE_SERVICE" -n 10 --no-pager | grep -i "prun\|start\|error" || true
echo "enable-pruning: done."
