#!/usr/bin/env bash
# check-genesis-supply.sh — Full genesis supply audit.
#
# Shows per-allocation locked/unlocked status based on vesting schedules,
# plus circulating supply breakdown (unlocked + admin minted − burned).
#
# Usage:
#   ./check-genesis-supply.sh                              # node.yaml only
#   ./check-genesis-supply.sh --config /etc/aperod/node.yaml
#   ./check-genesis-supply.sh --api-url http://localhost:3001   # pull mints/burned from API
#
# Exit codes:
#   0  — genesis sums match
#   1  — mismatch or parse error

set -euo pipefail

CONFIG="/etc/aperod/node.yaml"
API_URL=""

for arg in "$@"; do
  case "$arg" in
    --config=*) CONFIG="${arg#--config=}" ;;
    --config)   shift; CONFIG="${1:-}" ;;
    --api-url=*) API_URL="${arg#--api-url=}" ;;
    --api-url)   shift; API_URL="${1:-}" ;;
  esac
done

if [[ ! -f "$CONFIG" ]]; then
  echo "ERROR: config file not found: $CONFIG" >&2
  exit 1
fi

python3 - "$CONFIG" "$API_URL" <<'PYEOF'
import sys, re, json, urllib.request, urllib.error
from datetime import datetime, timezone

config_path = sys.argv[1]
api_url     = sys.argv[2] if len(sys.argv) > 2 else ""

# ── Load config ───────────────────────────────────────────────────────────────
with open(config_path) as f:
    text = f.read()

def parse_int(s):
    return int(s.replace("_", ""))

# initial_supply (APRO)
m = re.search(r'^\s*initial_supply\s*:\s*([0-9_]+)', text, re.MULTILINE)
if not m:
    print("ERROR: initial_supply not found", file=sys.stderr); sys.exit(1)
initial_supply_apro = parse_int(m.group(1))
initial_supply_napr = initial_supply_apro * 100_000_000

# genesis timestamp from config (0 = set at first start, unknown without API)
m_ts = re.search(r'^\s*timestamp\s*:\s*([0-9_]+)', text, re.MULTILINE)
config_genesis_ts = parse_int(m_ts.group(1)) if m_ts else 0

# ── Parse allocations ─────────────────────────────────────────────────────────
# Very lightweight YAML parser for the known structure.
in_alloc = False
alloc_lines = []
for line in text.splitlines():
    if re.match(r'^allocations\s*:', line):
        in_alloc = True; continue
    if in_alloc:
        if line and not line[0].isspace() and not line.startswith('#'):
            break
        alloc_lines.append(line)

allocations = []
cur = {}
for line in alloc_lines:
    if re.match(r'\s+-\s+address', line):
        if cur:
            allocations.append(cur)
        cur = {"amount": 0, "label": "", "vesting_type": "immediate",
               "cliff_seconds": 0, "vest_seconds": 0}
    m = re.match(r'\s+amount\s*:\s*([0-9_]+)', line)
    if m: cur["amount"] = parse_int(m.group(1))
    m = re.match(r'\s+label\s*:\s*"?([^"#\n]+)"?', line)
    if m: cur["label"] = m.group(1).strip().strip('"')
    m = re.match(r'\s+type\s*:\s*(\w+)', line)
    if m: cur["vesting_type"] = m.group(1)
    m = re.match(r'\s+cliff_seconds\s*:\s*([0-9_]+)', line)
    if m: cur["cliff_seconds"] = parse_int(m.group(1))
    m = re.match(r'\s+vest_seconds\s*:\s*([0-9_]+)', line)
    if m: cur["vest_seconds"] = parse_int(m.group(1))
if cur:
    allocations.append(cur)

total_napr = sum(a["amount"] for a in allocations)

# ── Fetch API data (proof-of-reserves) ───────────────────────────────────────
genesis_ts   = config_genesis_ts  # may be overridden by API
minted_napr  = None
burned_napr  = None
api_error    = None

if api_url:
    try:
        url = api_url.rstrip("/") + "/api/v1/proof-of-reserves"
        with urllib.request.urlopen(url, timeout=10) as r:
            por = json.load(r)
        genesis_ts   = int(por.get("genesis_ts_seconds") or 0) or genesis_ts
        minted_napr  = int(por.get("total_minted_napro", 0))
        burned_napr  = int(por.get("total_burned_napro", 0))
    except Exception as e:
        api_error = str(e)

# ── Compute vesting per allocation ────────────────────────────────────────────
now_sec = int(datetime.now(timezone.utc).timestamp())
genesis_known = genesis_ts > 0

total_locked_napr   = 0
total_unlocked_napr = 0

rows = []
for a in allocations:
    amt    = a["amount"]
    vtype  = a["vesting_type"]
    label  = a["label"] or "(unnamed)"
    cliff  = a["cliff_seconds"]
    vest   = a["vest_seconds"]

    if vtype == "immediate":
        pct = 100.0
    elif not genesis_known:
        pct = None  # unknown
    else:
        elapsed = now_sec - genesis_ts
        if vtype == "linear":
            pct = min(100.0, max(0.0, elapsed / vest * 100)) if vest > 0 else 100.0
        elif vtype == "cliff_linear":
            if elapsed < cliff:
                pct = 0.0
            else:
                pct = min(100.0, max(0.0, (elapsed - cliff) / vest * 100)) if vest > 0 else 100.0
        else:
            pct = 100.0  # unknown type → assume unlocked

    if pct is None:
        unlocked = None
        locked   = None
    else:
        unlocked = int(amt * pct / 100)
        locked   = amt - unlocked
        total_locked_napr   += locked
        total_unlocked_napr += unlocked

    rows.append({
        "label": label, "amount": amt, "vtype": vtype,
        "pct": pct, "unlocked": unlocked, "locked": locked,
    })

# ── Helpers ───────────────────────────────────────────────────────────────────
def fmt(napr):
    if napr is None: return "   unknown"
    apro = napr / 1e8
    return f"{apro:>18,.4f}"

def bar(pct, width=20):
    if pct is None: return "?" * width
    filled = round(pct / 100 * width)
    return "█" * filled + "░" * (width - filled)

# ── Print ─────────────────────────────────────────────────────────────────────
RESET = "\033[0m"
BOLD  = "\033[1m"
GREEN = "\033[32m"
YELLOW = "\033[33m"
RED   = "\033[31m"
CYAN  = "\033[36m"

print()
print(f"{BOLD}══════════════════════════════════════════════════════════════{RESET}")
print(f"{BOLD}  APEROD — Genesis Supply Audit{RESET}")
print(f"{BOLD}══════════════════════════════════════════════════════════════{RESET}")
print(f"  Config       : {config_path}")
if genesis_ts > 0:
    gt = datetime.fromtimestamp(genesis_ts, tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    print(f"  Genesis time : {gt}  (unix {genesis_ts})")
else:
    print(f"  Genesis time : {YELLOW}unknown — pass --api-url to resolve{RESET}")
if api_url:
    if api_error:
        print(f"  API          : {RED}unreachable ({api_error}){RESET}")
    else:
        print(f"  API          : {api_url}  (live data)")
print(f"  Snapshot     : {datetime.now(timezone.utc).strftime('%Y-%m-%d %H:%M UTC')}")
print()

# Per-allocation table
col1 = max(len(r["label"]) for r in rows) + 2
header = (
    f"  {'Allocation':<{col1}}  {'Total (APRO)':>18}  {'Unlocked':>18}  {'Locked':>18}  {'Vesting':>14}  {'%':>6}  Progress"
)
sep = "  " + "─" * (len(header) - 2)
print(header)
print(sep)

for r in rows:
    pct_str  = f"{r['pct']:>5.1f}%" if r["pct"] is not None else " ???%"
    progress = bar(r["pct"])
    color = GREEN if r["pct"] == 100 else (YELLOW if r["pct"] and r["pct"] > 0 else RED if r["pct"] == 0 else RESET)
    print(
        f"  {r['label']:<{col1}}  {fmt(r['amount'])}  "
        f"{fmt(r['unlocked'])}  {fmt(r['locked'])}  "
        f"  {r['vtype']:>14}  {color}{pct_str}{RESET}  {color}{progress}{RESET}"
    )

print(sep)
# Totals from genesis
print(f"  {'GENESIS TOTAL':<{col1}}  {fmt(total_napr)}  {fmt(total_unlocked_napr)}  {fmt(total_locked_napr)}")
print()

# Supply identity
print(f"{BOLD}── Supply Identity ──────────────────────────────────────────{RESET}")
print(f"  Genesis total      : {fmt(total_napr)} APRO")
if minted_napr is not None:
    print(f"  Admin minted       :{CYAN}{fmt(minted_napr)}{RESET} APRO  (confirmed admin mints)")
if burned_napr is not None:
    print(f"  Burned (EIP-1559)  :{RED}{fmt(-burned_napr)}{RESET} APRO")

if minted_napr is not None and burned_napr is not None:
    circulating = total_unlocked_napr + minted_napr - burned_napr if genesis_known else None
    print()
    print(f"  ┌──────────────────────────────────────────────────────┐")
    if circulating is not None:
        print(f"  │  Circulating = unlocked + minted − burned            │")
        print(f"  │  = {fmt(total_unlocked_napr)} + {fmt(minted_napr)} − {fmt(burned_napr)} │")
        print(f"  │  = {BOLD}{GREEN}{fmt(circulating)} APRO{RESET}                        │")
    print(f"  │  Remaining mintable cap : {fmt(initial_supply_napr - (total_napr + (minted_napr or 0)))} APRO  │")
    print(f"  └──────────────────────────────────────────────────────┘")

print()

# Genesis integrity check
ok_color = GREEN if total_napr == initial_supply_napr else RED
status = "✓  PASS" if total_napr == initial_supply_napr else "✗  FAIL"
print(f"  {ok_color}{BOLD}{status}{RESET} — genesis allocations "
      f"{'match' if total_napr == initial_supply_napr else 'DO NOT match'} initial_supply "
      f"({initial_supply_apro:,} APRO)")
if total_napr != initial_supply_napr:
    diff = total_napr - initial_supply_napr
    print(f"         Difference: {diff:+,} nAPRO ({diff/1e8:+.8f} APRO)", file=sys.stderr)

print()
sys.exit(0 if total_napr == initial_supply_napr else 1)
PYEOF
