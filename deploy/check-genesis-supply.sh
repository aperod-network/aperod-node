#!/usr/bin/env bash
# check-genesis-supply.sh — Verify that genesis allocation amounts sum exactly
# to the declared initial_supply.
#
# Usage:
#   ./check-genesis-supply.sh                         # uses /etc/aperod/node.yaml
#   ./check-genesis-supply.sh --config /path/node.yaml
#
# Exit codes:
#   0  — sums match
#   1  — mismatch or parse error

set -euo pipefail

CONFIG="/etc/aperod/node.yaml"

for arg in "$@"; do
  case "$arg" in
    --config=*) CONFIG="${arg#--config=}" ;;
    --config)   shift; CONFIG="${1:-}" ;;
  esac
done

if [[ ! -f "$CONFIG" ]]; then
  echo "ERROR: config file not found: $CONFIG" >&2
  exit 1
fi

python3 - "$CONFIG" <<'PYEOF'
import sys, re

config_path = sys.argv[1]
with open(config_path, "r") as f:
    text = f.read()

# ── Parse initial_supply (APRO, integer) ─────────────────────────────────────
m = re.search(r'^\s*initial_supply\s*:\s*([0-9_]+)', text, re.MULTILINE)
if not m:
    print("ERROR: initial_supply not found in config", file=sys.stderr)
    sys.exit(1)
initial_supply_apro = int(m.group(1).replace("_", ""))
initial_supply_napr = initial_supply_apro * 100_000_000  # 1 APRO = 10^8 nAPRO

# ── Parse allocations block ───────────────────────────────────────────────────
# Find all  amount: <digits>  lines inside the allocations: block.
# We locate the block by finding "^allocations:" then collecting indented lines.
in_alloc = False
alloc_lines = []
for line in text.splitlines():
    if re.match(r'^allocations\s*:', line):
        in_alloc = True
        continue
    if in_alloc:
        # A top-level key (no leading spaces) ends the block
        if line and not line[0].isspace() and not line.startswith('#'):
            break
        alloc_lines.append(line)

amounts = []
for line in alloc_lines:
    m = re.match(r'\s+amount\s*:\s*([0-9_]+)', line)
    if m:
        amounts.append(int(m.group(1).replace("_", "")))

if not amounts:
    print("ERROR: no allocation amounts found in config", file=sys.stderr)
    sys.exit(1)

total_napr = sum(amounts)
total_apro = total_napr / 100_000_000

# ── Report ────────────────────────────────────────────────────────────────────
print(f"Config          : {config_path}")
print(f"initial_supply  : {initial_supply_apro:>20,.0f} APRO")
print(f"Allocations ({len(amounts):>2}): {total_apro:>20,.8f} APRO")
print(f"Difference      : {(total_apro - initial_supply_apro):>+20,.8f} APRO")
print()

if total_napr == initial_supply_napr:
    print("✓  PASS — genesis allocations match initial_supply exactly.")
    sys.exit(0)
else:
    diff_napr = total_napr - initial_supply_napr
    direction = "OVER" if diff_napr > 0 else "SHORT"
    print(f"✗  FAIL — {direction} by {abs(diff_napr)} nAPRO ({abs(diff_napr)/1e8:.8f} APRO).", file=sys.stderr)
    sys.exit(1)
PYEOF
