#!/usr/bin/env bash
# compact-db.sh — safely compact the Aperod LevelDB to reclaim disk space.
#
# Run AFTER enabling light pruning and letting the node prune for at least one
# epoch (~100 blocks, a few minutes).  Pruning removes TxData entries logically;
# compaction rewrites the SST files and frees the physical disk space.
#
# Usage:
#   sudo bash /opt/aperod/blockchain/deploy/compact-db.sh
#
# What it does:
#   1. Stops aperod-node (saves a UTXO snapshot first — ~10 s).
#   2. Runs  aperod-node --compact-db --data-dir=<data_dir>
#   3. Restarts aperod-node.
#
# Safe to run while the system is live; it simply pauses block production
# for the duration of compaction (a few minutes on a ~10 GB chain.db).

set -euo pipefail

NODE_SERVICE="aperod-node"
CONFIG_FILE="${APEROD_CONFIG:-/etc/aperod/node.yaml}"

# ── Resolve data_dir from node.yaml ──────────────────────────────────────────
DATA_DIR=$(python3 - "$CONFIG_FILE" <<'PYEOF'
import sys, re
cfg = open(sys.argv[1]).read()
m = re.search(r'^\s*data_dir\s*:\s*(.+)', cfg, re.MULTILINE)
print(m.group(1).strip().strip('"\'')) if m else sys.exit("data_dir not found in config")
PYEOF
)
echo "compact-db: data_dir = $DATA_DIR"

BINARY="${APEROD_BINARY:-/usr/local/bin/aperod-node}"

# ── 1. Stop the node (it saves a snapshot on SIGTERM) ────────────────────────
echo "compact-db: stopping $NODE_SERVICE ..."
systemctl stop "$NODE_SERVICE"
echo "compact-db: node stopped."

# ── 2. Compact ────────────────────────────────────────────────────────────────
echo "compact-db: starting compaction ..."
"$BINARY" --compact-db --data-dir="$DATA_DIR"

# ── 3. Restart ────────────────────────────────────────────────────────────────
echo "compact-db: restarting $NODE_SERVICE ..."
systemctl start "$NODE_SERVICE"
echo "compact-db: done. Node is back online."
