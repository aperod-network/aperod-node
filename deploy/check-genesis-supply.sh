#!/usr/bin/env bash
# check-genesis-supply.sh — Full genesis supply audit.
#
# Checks genesis allocation totals; display-only team policy comes from the
# same distribution JSON as the website. Not a proof of circulating UTXOs.
#
# Usage:
#   ./check-genesis-supply.sh                              # node.yaml only
#   ./check-genesis-supply.sh --config /etc/aperod/node.yaml
#   ./check-genesis-supply.sh --api-url http://localhost:3001   # pull mints/burned from API
#   ./check-genesis-supply.sh --distribution /path/to/genesis-distribution.json
#   ./check-genesis-supply.sh --at 2026-08-14                 # offline UTC snapshot
# Installed copy: set APEROD_DIR to the source checkout, or pass --distribution.
# Current policy cancels team entitlements; retired annual schedules belong in
# explicit historical JSON fixtures only. This report never modifies genesis.
#
# Exit codes:
#   0  — genesis sums match
#   1  — mismatch or parse error

set -euo pipefail

CONFIG="/etc/aperod/node.yaml"
API_URL=""
DISTRIBUTION=""
AS_OF=""
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

while (($#)); do
  case "$1" in
    --config=*)  CONFIG="${1#--config=}" ;;
    --config)    shift; CONFIG="${1:-}" ;;
    --api-url=*) API_URL="${1#--api-url=}" ;;
    --api-url)   shift; API_URL="${1:-}" ;;
    --distribution=*) DISTRIBUTION="${1#--distribution=}"; : "${DISTRIBUTION:?Missing --distribution path}" ;;
    --distribution) shift; DISTRIBUTION="${1:?Missing --distribution path}" ;;
    --at=*) AS_OF="${1#--at=}"; : "${AS_OF:?Missing --at date}" ;;
    --at) shift; AS_OF="${1:?Missing --at date}" ;;
    *) echo "ERROR: unknown option: $1" >&2; exit 1 ;;
  esac
  shift
done

if [[ -z "$DISTRIBUTION" ]]; then
  # Both checkout execution and the installed /usr/local/bin copy are supported.
  if [[ -f "${SCRIPT_DIR}/../../lib/db/genesis-distribution.json" ]]; then
    DISTRIBUTION="${SCRIPT_DIR}/../../lib/db/genesis-distribution.json"
  else
    DISTRIBUTION="${APEROD_DIR:-/opt/aperod}/lib/db/genesis-distribution.json"
  fi
fi
if [[ ! -f "$DISTRIBUTION" ]]; then
  echo "ERROR: distribution JSON not found; use --distribution: $DISTRIBUTION" >&2
  exit 1
fi
if [[ -n "$AS_OF" && -n "$API_URL" ]]; then
  echo "ERROR: --at is offline-only; live API counters cannot prove a historical snapshot" >&2
  exit 1
fi

if [[ ! -f "$CONFIG" ]]; then
  echo "ERROR: config file not found: $CONFIG" >&2
  exit 1
fi

python3 - "$CONFIG" "$API_URL" "$DISTRIBUTION" "$AS_OF" <<'PYEOF'
import sys, re, os, json, urllib.request, urllib.error
from datetime import datetime, timezone
from decimal import Decimal

config_path = sys.argv[1]
api_url     = sys.argv[2] if len(sys.argv) > 2 else ""
distribution_path = sys.argv[3]
as_of = sys.argv[4]

def fail(message):
    print("ERROR: " + message, file=sys.stderr)
    sys.exit(1)

def utc_day(value):
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}", value):
        raise ValueError("expected YYYY-MM-DD")
    return datetime.strptime(value, "%Y-%m-%d").replace(tzinfo=timezone.utc)

try:
    snapshot = utc_day(as_of) if as_of else datetime.now(timezone.utc)
    with open(distribution_path) as source:
        distribution = json.load(source)
    if not isinstance(distribution, list) or not distribution:
        raise ValueError("expected a non-empty allocation array")
    for entry in distribution:
        if not isinstance(entry, dict):
            raise ValueError("invalid allocation entry")
        amount = entry.get("amount_apro")
        if type(amount) is not int or amount < 0:
            raise ValueError("amount_apro must be a non-negative integer")
        if "is_schedule_entry" in entry and type(entry["is_schedule_entry"]) is not bool:
            raise ValueError("is_schedule_entry must be boolean")
        if entry.get("unlock_date_utc"):
            utc_day(entry["unlock_date_utc"])
    schedules = [e for e in distribution if e.get("is_schedule_entry") is True or e.get("unlock_date_utc")]
    primary = [e for e in distribution if not e.get("is_schedule_entry") and not e.get("unlock_date_utc")]
    if sum(e["amount_apro"] for e in primary) != 10_000_000_000:
        raise ValueError("primary allocation total must be exactly 10B; schedules are suballocations")
except (ValueError, OSError, TypeError):
    fail("invalid distribution JSON or UTC snapshot; refusing a continuous-vesting fallback")

# ── Load node.yaml ────────────────────────────────────────────────────────────
with open(config_path) as f:
    node_text = f.read()

def parse_int(s):
    return int(s.replace("_", ""))

# ── Resolve genesis file ──────────────────────────────────────────────────────
# node.yaml has:  genesis:\n  file: "config/genesis-testnet.yaml"
# Path is relative to node.yaml's directory.
m_gf = re.search(r'^\s*file\s*:\s*"?([^"#\n]+)"?', node_text, re.MULTILINE)
if not m_gf:
    print("ERROR: genesis.file not found in config — check node.yaml genesis: section", file=sys.stderr)
    sys.exit(1)
genesis_rel = m_gf.group(1).strip().strip('"')
config_dir  = os.path.dirname(os.path.abspath(config_path))
genesis_path = os.path.join(config_dir, genesis_rel)
if not os.path.isfile(genesis_path):
    # Try relative to cwd as fallback
    genesis_path2 = os.path.join(os.getcwd(), genesis_rel)
    if os.path.isfile(genesis_path2):
        genesis_path = genesis_path2
    else:
        print(f"ERROR: genesis file not found: {genesis_path}", file=sys.stderr)
        sys.exit(1)

with open(genesis_path) as f:
    text = f.read()

# ── Parse initial_supply from genesis file ────────────────────────────────────
m_is = re.search(r'^\s*initial_supply\s*:\s*([0-9_]+)', text, re.MULTILINE)
if m_is:
    initial_supply_apro = parse_int(m_is.group(1))
    initial_supply_napr = initial_supply_apro * 100_000_000
else:
    # Fallback: 10B sealed at genesis
    initial_supply_apro = 10_000_000_000
    initial_supply_napr = initial_supply_apro * 100_000_000

# ── Parse genesis timestamp ───────────────────────────────────────────────────
m_ts = re.search(r'^\s*timestamp\s*:\s*([0-9_]+)', text, re.MULTILINE)
config_genesis_ts = parse_int(m_ts.group(1)) if m_ts else 0

# ── Parse allocations from genesis file ──────────────────────────────────────
# Supports two vesting formats:
#   flat:   type: immediate   cliff_seconds: N   vest_seconds: N
#   nested: vesting:\n        type: immediate\n  cliff_seconds: N
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
    # New allocation entry starts at "  - address:" or "  - label:"
    if re.match(r'\s+-\s+(address|label)\s*:', line):
        if cur:
            allocations.append(cur)
        cur = {"amount": 0, "label": "", "address": "", "category": "", "vesting_type": "immediate",
               "cliff_seconds": 0, "vest_seconds": 0}
    m = re.match(r'\s+amount\s*:\s*([0-9_]+)', line)
    if m: cur["amount"] = parse_int(m.group(1))
    m = re.match(r'\s+label\s*:\s*"?([^"#\n]+)"?', line)
    if m: cur["label"] = m.group(1).strip().strip('"')
    m = re.match(r'\s+(?:-\s+)?address\s*:\s*"?([^"#\n]*)', line)
    if m: cur["address"] = m.group(1).strip().strip('"')
    m = re.match(r'\s+category\s*:\s*"?([^"#\n]+)', line)
    if m: cur["category"] = m.group(1).strip().strip('"')
    # type: may appear directly or nested under vesting:
    m = re.match(r'\s+type\s*:\s*(\w+)', line)
    if m: cur["vesting_type"] = m.group(1)
    m = re.match(r'\s+cliff_seconds\s*:\s*([0-9_]+)', line)
    if m: cur["cliff_seconds"] = parse_int(m.group(1))
    m = re.match(r'\s+vest_seconds\s*:\s*([0-9_]+)', line)
    if m: cur["vest_seconds"] = parse_int(m.group(1))
if cur:
    allocations.append(cur)

if not allocations:
    print(f"ERROR: no allocations found in genesis file: {genesis_path}", file=sys.stderr)
    sys.exit(1)

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
now_sec = int(snapshot.timestamp())
genesis_known = genesis_ts > 0

total_locked_napr   = 0
total_unlocked_napr = 0

rows = []
policy_notes = []
for a in allocations:
    amt    = a["amount"]
    vtype  = a["vesting_type"]
    label  = a["label"] or "(unnamed)"
    cliff  = a["cliff_seconds"]
    vest   = a["vest_seconds"]

    # Recognize legacy team allocations, but never rewrite genesis or claim
    # that changing nominal policy removes a node-enforced reservation.
    team = a["category"] == "team" or bool(re.search(r"\b(dev(?:elopment)?\s+fund|team)\b", label, re.I))
    if team:
        candidates = [e for e in primary if e.get("category") == "team"
                      and e["amount_apro"] * 100_000_000 == amt]
        if not candidates:
            # Cancelled fund: the current JSON reclassifies the same nominal
            # allocation into immediate Community & Ecosystem availability.
            candidates = [e for e in primary if e.get("category") == "community"
                          and e.get("vesting_type") == "immediate"
                          and e["amount_apro"] * 100_000_000 == amt]
        if a["address"]:
            candidates = [e for e in candidates if e.get("address") == a["address"]]
        if len(candidates) != 1:
            fail("legacy team allocation cannot be uniquely reconciled with distribution JSON")
        policy = candidates[0]
        policy_label = policy.get("label", label)
        if policy.get("category") != "team":
            unlocked = amt
            vtype = "policy-immediate"
            policy_notes.append(f"YAML '{label}' -> '{policy_label}': team entitlement cancelled; "
                                "nominal policy only, not proof of canonical release.")
            label = policy_label
        else:
            matching = [e for e in schedules if e.get("category") == "team"]
            if not matching or any(not e.get("unlock_date_utc") for e in matching):
                fail("team tranche dates are missing; refusing continuous interpolation")
            total_scheduled = sum(e["amount_apro"] for e in matching) * 100_000_000
            if total_scheduled > amt:
                fail("team tranches exceed the primary allocation")
            policy_cliff = policy.get("cliff_seconds", 0)
            if type(policy_cliff) is not int or policy_cliff < 0:
                fail("invalid team cliff_seconds")
            if not genesis_known:
                unlocked = None
            elif now_sec < genesis_ts + policy_cliff:
                unlocked = 0
            else:
                unlocked = amt - total_scheduled + sum(
                    e["amount_apro"] * 100_000_000 for e in matching
                    if now_sec >= int(utc_day(e["unlock_date_utc"]).timestamp())
                )
            vtype = "dated-tranches"
            policy_notes.append(f"YAML '{label}': using dated JSON tranches, not its legacy continuous schedule.")
    elif vtype == "immediate":
        unlocked = amt
    elif not genesis_known:
        unlocked = None
    else:
        elapsed = max(0, now_sec - genesis_ts)
        if vtype == "linear":
            unlocked = amt * min(elapsed, vest) // vest if vest > 0 else amt
        elif vtype == "cliff_linear":
            if elapsed < cliff:
                unlocked = 0
            else:
                unlocked = amt * min(elapsed - cliff, vest) // vest if vest > 0 else amt
        else:
            fail("unknown configured vesting type; refusing to assume unlocked")

    if unlocked is None:
        pct = None
        locked   = None
    else:
        pct = unlocked * 100 / amt if amt else 100
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
    apro = Decimal(napr) / 100_000_000
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
print(f"{BOLD}  APEROD — Genesis Allocation Audit{RESET}")
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
print(f"  Snapshot     : {snapshot.strftime('%Y-%m-%d %H:%M UTC')}")
print(f"  Policy JSON  : {distribution_path}")
print("  Basis        : nominal allocation / calendar estimates, NOT verified circulating or spendable UTXOs")
for note in policy_notes:
    print(f"  Policy note  : {note}")
print("  Genesis YAML is unchanged; node-enforced locks/reserves require independent canonical reconciliation.")
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
all_vesting_known = all(row["unlocked"] is not None for row in rows)
print(f"  {'GENESIS TOTAL':<{col1}}  {fmt(total_napr)}  "
      f"{fmt(total_unlocked_napr if all_vesting_known else None)}  "
      f"{fmt(total_locked_napr if all_vesting_known else None)}")
print()

# Supply identity
print(f"{BOLD}── Nominal accounting (not a circulating-supply proof) ─────────{RESET}")
print(f"  Genesis total      : {fmt(total_napr)} APRO")
if minted_napr is not None:
    print(f"  Admin minted       :{CYAN}{fmt(minted_napr)}{RESET} APRO  (reported by API, not independently verified)")
if burned_napr is not None:
    print(f"  Burned (EIP-1559)  :{RED}{fmt(-burned_napr)}{RESET} APRO")

if minted_napr is not None and burned_napr is not None:
    circulating = total_unlocked_napr + minted_napr - burned_napr if all_vesting_known else None
    print()
    print(f"  ┌──────────────────────────────────────────────────────┐")
    if circulating is not None:
        print(f"  │  Nominal estimate = unlocked + minted − burned       │")
        print(f"  │  = {fmt(total_unlocked_napr)} + {fmt(minted_napr)} − {fmt(burned_napr)} │")
        print(f"  │  = {BOLD}{GREEN}{fmt(circulating)} APRO{RESET}                        │")
    print(f"  │  This estimate grants no minting authority.          │")
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
