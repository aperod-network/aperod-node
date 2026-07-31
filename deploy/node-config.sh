#!/usr/bin/env bash
# node-config.sh — Safe node.yaml management tool
#
# Edits node.yaml using Python (not sed) to avoid silent YAML corruption.
# Validates the result after every write.
#
# Subcommands:
#   list-bootnodes            — Print all current bootnodes
#   add-bootnode    <addr>    — Append a bootnode entry (e.g. /ip4/1.2.3.4/tcp/30303)
#   remove-bootnode <addr>    — Remove a bootnode entry by exact match
#
# Usage (as root or with sudo):
#   sudo bash /opt/aperod/blockchain/deploy/node-config.sh list-bootnodes
#   sudo bash /opt/aperod/blockchain/deploy/node-config.sh add-bootnode /ip4/1.2.3.4/tcp/30303
#   sudo bash /opt/aperod/blockchain/deploy/node-config.sh remove-bootnode /ip4/1.2.3.4/tcp/30303
#
set -euo pipefail

CONFIG_FILE="${APEROD_CONFIG:-/etc/aperod/node.yaml}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
die()  { echo -e "${RED}[ERR]${NC}  $*" >&2; exit 1; }
ok()   { echo -e "${GREEN}[OK]${NC}   $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }

# ── Python helper (requires python3 + pyyaml) ─────────────────────────────────
# pyyaml is almost always available; installs it silently if not.
ensure_pyyaml() {
  python3 -c "import yaml" 2>/dev/null && return
  warn "PyYAML not found — installing..."
  pip3 install -q pyyaml || apt-get install -y -q python3-yaml 2>/dev/null || \
    die "Cannot install pyyaml. Run: pip3 install pyyaml"
}

# ── Validate the YAML file after writing ─────────────────────────────────────
# First tries `aperod-node --validate-config` (full config parse + semantic
# validation via cfg.Validate()).  Falls back to python yaml.safe_load() when
# the binary is not on PATH or when a tmp file is being validated (the binary
# needs a real config path, which may not match the temp path).
validate_yaml() {
  local file="$1"

  # Attempt binary validation when the file matches the real config location.
  if [[ "$file" == "$CONFIG_FILE" ]] && command -v aperod-node &>/dev/null; then
    if aperod-node --config "$file" --validate-config 2>&1; then
      return 0
    else
      echo "[warn] aperod-node --validate-config failed; falling back to PyYAML check." >&2
    fi
  fi

  python3 - <<EOF
import sys, yaml
try:
    with open("${file}") as f:
        yaml.safe_load(f)
    sys.exit(0)
except Exception as e:
    print(f"YAML validation failed: {e}", file=sys.stderr)
    sys.exit(1)
EOF
}

# ── list-bootnodes ────────────────────────────────────────────────────────────
cmd_list() {
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"
  ensure_pyyaml
  python3 - "$CONFIG_FILE" <<'EOF'
import yaml, sys
with open(sys.argv[1]) as f:
    cfg = yaml.safe_load(f) or {}
bootnodes = (cfg.get("p2p") or {}).get("bootnodes") or []
if not bootnodes:
    print("(no bootnodes configured)")
else:
    for i, b in enumerate(bootnodes):
        print(f"  [{i}] {b}")
EOF
}

# ── add-bootnode ──────────────────────────────────────────────────────────────
cmd_add() {
  local new_addr="$1"
  [[ -n "$new_addr" ]] || die "Usage: $0 add-bootnode <multiaddr>"
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"
  ensure_pyyaml

  # Create the temp file in the same directory as CONFIG_FILE so that the final
  # rename (mv) is an atomic same-filesystem operation.  cp is NOT used because
  # it truncates the destination before writing — a crash or ENOSPC mid-copy
  # would leave node.yaml empty/corrupt.  mv on the same filesystem is a single
  # rename(2) syscall: it either succeeds atomically or leaves the original untouched.
  local tmp cfg_dir lockfile
  cfg_dir="$(dirname "$CONFIG_FILE")"
  lockfile="${cfg_dir}/.node-config.lock"
  tmp=$(mktemp "${cfg_dir}/.node-config-XXXXXX")

  # Acquire an exclusive advisory lock on the lock file for the duration of the
  # read→edit→validate→rename cycle.  Two concurrent invocations will serialise
  # here so that the second one reads the file AFTER the first has already
  # committed its rename(2).  The file descriptor (and thus the lock) is held
  # until this process exits — no explicit close needed; bash releases all fds
  # on exit automatically.
  exec 9>"$lockfile"
  flock -x 9

  # Use ${tmp:-} so the EXIT trap does not trip set -u after the function returns.
  trap 'rm -f "${tmp:-}"' EXIT

  python3 - "$CONFIG_FILE" "$new_addr" "$tmp" <<'EOF'
import yaml, sys

cfg_path, new_addr, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

if "p2p" not in cfg or cfg["p2p"] is None:
    cfg["p2p"] = {}
if "bootnodes" not in cfg["p2p"] or cfg["p2p"]["bootnodes"] is None:
    cfg["p2p"]["bootnodes"] = []

existing = cfg["p2p"]["bootnodes"]
if new_addr in existing:
    print(f"Already present: {new_addr}")
    # Still write the unchanged config so the temp file is non-empty and
    # the validate_yaml + cp cycle is a safe no-op rather than overwriting
    # the original with an empty file.
    with open(out_path, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    sys.exit(0)

existing.append(new_addr)
cfg["p2p"]["bootnodes"] = existing

with open(out_path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
print(f"Added: {new_addr}")
EOF

  # Validate before replacing
  validate_yaml "$tmp" || die "Validation failed — config not written."

  echo ""
  echo "  Diff:"
  diff "$CONFIG_FILE" "$tmp" || true
  echo ""

  # Preserve original file permissions on the temp file before replacing.
  chmod --reference="$CONFIG_FILE" "$tmp" 2>/dev/null || \
    chmod "$(stat -f '%Mp%Lp' "$CONFIG_FILE" 2>/dev/null || echo 644)" "$tmp" 2>/dev/null || true

  # Atomic rename: rename(2) on the same filesystem never truncates the
  # destination mid-write — the old inode remains intact until the syscall
  # succeeds, so a crash or ENOSPC here leaves node.yaml untouched.
  mv "$tmp" "$CONFIG_FILE"
  ok "node.yaml updated successfully."
  echo ""
  echo "  Current bootnodes:"
  cmd_list
  echo ""
  warn "Restart aperod-node to apply: systemctl restart aperod-node"
}

# ── remove-bootnode ───────────────────────────────────────────────────────────
cmd_remove() {
  local rm_addr="$1"
  [[ -n "$rm_addr" ]] || die "Usage: $0 remove-bootnode <multiaddr>"
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"
  ensure_pyyaml

  local tmp cfg_dir lockfile
  cfg_dir="$(dirname "$CONFIG_FILE")"
  lockfile="${cfg_dir}/.node-config.lock"
  tmp=$(mktemp "${cfg_dir}/.node-config-XXXXXX")

  # Acquire an exclusive advisory lock — same serialisation guarantee as cmd_add.
  exec 9>"$lockfile"
  flock -x 9

  trap 'rm -f "${tmp:-}"' EXIT

  python3 - "$CONFIG_FILE" "$rm_addr" "$tmp" <<'EOF'
import yaml, sys

cfg_path, rm_addr, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

existing = (cfg.get("p2p") or {}).get("bootnodes") or []
if rm_addr not in existing:
    print(f"Not found: {rm_addr}", file=sys.stderr)
    sys.exit(1)

existing.remove(rm_addr)
cfg["p2p"]["bootnodes"] = existing

with open(out_path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
print(f"Removed: {rm_addr}")
EOF

  validate_yaml "$tmp" || die "Validation failed — config not written."

  echo ""
  echo "  Diff:"
  diff "$CONFIG_FILE" "$tmp" || true
  echo ""

  chmod --reference="$CONFIG_FILE" "$tmp" 2>/dev/null || \
    chmod "$(stat -f '%Mp%Lp' "$CONFIG_FILE" 2>/dev/null || echo 644)" "$tmp" 2>/dev/null || true

  mv "$tmp" "$CONFIG_FILE"
  ok "node.yaml updated successfully."
  echo ""
  echo "  Remaining bootnodes:"
  cmd_list
  echo ""
  warn "Restart aperod-node to apply: systemctl restart aperod-node"
}

# ── dispatch ──────────────────────────────────────────────────────────────────
SUBCMD="${1:-help}"
shift || true

case "$SUBCMD" in
  list-bootnodes)    cmd_list ;;
  add-bootnode)      cmd_add "${1:-}" ;;
  remove-bootnode)   cmd_remove "${1:-}" ;;
  help|--help|-h)
    echo "Usage: $0 <subcommand> [args]"
    echo ""
    echo "Subcommands:"
    echo "  list-bootnodes            — List current bootnodes in node.yaml"
    echo "  add-bootnode    <addr>    — Safely append a bootnode"
    echo "  remove-bootnode <addr>    — Remove a bootnode by exact match"
    echo ""
    echo "Config file: ${CONFIG_FILE}"
    echo "Override:    APEROD_CONFIG=/path/to/node.yaml $0 ..."
    ;;
  *)
    die "Unknown subcommand: $SUBCMD. Run '$0 help' for usage."
    ;;
esac
