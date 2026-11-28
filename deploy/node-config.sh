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

# ── Backup current config before overwriting ──────────────────────────────────
# Creates $CONFIG_FILE.bak-YYYYMMDD-HHMMSS-XXXXXX (unique even within the same
# second) in the same directory.  Called just before every atomic rename so the
# pre-write state is always recoverable with `restore-backup`.
#
# FAILURE POLICY: if the backup cannot be written, this function calls die()
# so the caller exits before the atomic rename takes place.  The EXIT trap in
# cmd_add / cmd_remove will clean up the pending tmp file.  Never silently
# skipping the backup and continuing with the write would violate the guarantee
# that every config change has a recoverable pre-write snapshot.
backup_config() {
  local ts bak
  ts=$(date -u +%Y%m%d-%H%M%S)
  # mktemp guarantees uniqueness even when two writes happen in the same second;
  # the human-readable timestamp prefix keeps backups sortable by name.
  # --tmpdir is not used so the file lands beside the config (same filesystem).
  bak=$(mktemp "${CONFIG_FILE}.bak-${ts}-XXXXXX") || \
    die "Cannot create backup temp file beside $CONFIG_FILE — aborting write."
  # Preserve permissions only (not timestamps) so the backup's mtime reflects
  # when it was created — this is what `ls -1t` relies on in restore-backup.
  cp --preserve=mode "$CONFIG_FILE" "$bak" 2>/dev/null || \
    cp "$CONFIG_FILE" "$bak" || \
    { rm -f "$bak"; die "Cannot write backup $bak — aborting write to protect existing config."; }
  ok "Backup saved: $bak"
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

# Migrate legacy root-level 'bootnodes' key produced by older install-node.sh.
# The Go runtime only reads cfg.P2P.Bootnodes (yaml:"bootnodes" under p2p:);
# a root-level key is silently ignored and the node stays isolated.  Move any
# existing entries into p2p.bootnodes and remove the stale root-level key.
legacy = cfg.pop("bootnodes", None)

if "p2p" not in cfg or cfg["p2p"] is None:
    cfg["p2p"] = {}
if "bootnodes" not in cfg["p2p"] or cfg["p2p"]["bootnodes"] is None:
    cfg["p2p"]["bootnodes"] = []

existing = cfg["p2p"]["bootnodes"]

# Preserve any valid entries from the migrated root-level key.
if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in existing:
            existing.append(entry)

if new_addr in existing:
    print(f"Already present: {new_addr}")
    # Still write the (possibly migrated) config so the temp file is non-empty
    # and the validate_yaml + mv cycle is a safe no-op.
    with open(out_path, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    sys.exit(0)

existing.append(new_addr)
cfg["p2p"]["bootnodes"] = existing

with open(out_path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
if legacy:
    print(f"Migrated root-level bootnodes to p2p.bootnodes; added: {new_addr}")
else:
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

  # Snapshot the current config BEFORE the atomic rename so it is always
  # recoverable via `restore-backup` if the new values turn out to be wrong.
  backup_config

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

  # Snapshot before the atomic rename (same policy as cmd_add).
  backup_config

  mv "$tmp" "$CONFIG_FILE"
  ok "node.yaml updated successfully."
  echo ""
  echo "  Remaining bootnodes:"
  cmd_list
  echo ""
  warn "Restart aperod-node to apply: systemctl restart aperod-node"
}

# ── set-field ────────────────────────────────────────────────────────────────
# Sets a top-level boolean/string/integer field in node.yaml.
# Usage: set-field <key> <value>
# Example: set-field non_validator true
cmd_set_field() {
  local key="${1:-}" value="${2:-}"
  [[ -n "$key" ]]   || die "Usage: $0 set-field <key> <value>"
  [[ -n "$value" ]] || die "Usage: $0 set-field <key> <value>"
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"
  ensure_pyyaml

  local tmp cfg_dir lockfile
  cfg_dir="$(dirname "$CONFIG_FILE")"
  lockfile="${cfg_dir}/.node-config.lock"
  tmp=$(mktemp "${cfg_dir}/.node-config-XXXXXX")

  exec 9>"$lockfile"
  flock -x 9

  trap 'rm -f "${tmp:-}"' EXIT

  python3 - "$CONFIG_FILE" "$key" "$value" "$tmp" <<'EOF'
import yaml, sys

cfg_path, key, value_s, out_path = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Convert string value to appropriate Python type.
if value_s.lower() == 'true':
    value = True
elif value_s.lower() == 'false':
    value = False
else:
    try:
        value = int(value_s)
    except ValueError:
        value = value_s

# Support dotted paths (e.g. "consensus.non_validator") for nested fields.
parts = key.split('.')
node = cfg
for part in parts[:-1]:
    if part not in node or node[part] is None:
        node[part] = {}
    node = node[part]
node[parts[-1]] = value

with open(out_path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
print(f"Set {key}: {value}")
EOF

  validate_yaml "$tmp" || die "Validation failed — config not written."
  chmod --reference="$CONFIG_FILE" "$tmp" 2>/dev/null || true
  backup_config
  mv "$tmp" "$CONFIG_FILE"
  ok "node.yaml updated: ${key} = ${value}"
}

# ── unset-field ───────────────────────────────────────────────────────────────
# Removes a top-level field from node.yaml.  No-op if the key is not present.
# Usage: unset-field <key>
# Example: unset-field non_validator
cmd_unset_field() {
  local key="${1:-}"
  [[ -n "$key" ]] || die "Usage: $0 unset-field <key>"
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"
  ensure_pyyaml

  local tmp cfg_dir lockfile
  cfg_dir="$(dirname "$CONFIG_FILE")"
  lockfile="${cfg_dir}/.node-config.lock"
  tmp=$(mktemp "${cfg_dir}/.node-config-XXXXXX")

  exec 9>"$lockfile"
  flock -x 9

  trap 'rm -f "${tmp:-}"' EXIT

  python3 - "$CONFIG_FILE" "$key" "$tmp" <<'EOF'
import yaml, sys

cfg_path, key, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Support dotted paths (e.g. "consensus.non_validator") for nested fields.
parts = key.split('.')
node = cfg
for part in parts[:-1]:
    if not isinstance(node.get(part), dict):
        print(f"{key} not present — nothing to do")
        with open(out_path, "w") as f:
            yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
        sys.exit(0)
    node = node[part]

leaf = parts[-1]
if leaf not in node:
    print(f"{key} not present — nothing to do")
    with open(out_path, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    sys.exit(0)

del node[leaf]

with open(out_path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
print(f"Removed: {key}")
EOF

  validate_yaml "$tmp" || die "Validation failed — config not written."
  chmod --reference="$CONFIG_FILE" "$tmp" 2>/dev/null || true
  backup_config
  mv "$tmp" "$CONFIG_FILE"
  ok "node.yaml updated: removed ${key}"
}

# ── set-snapshot-tolerance ───────────────────────────────────────────────────
# Raises snapshot.utxo_count_tolerance_pct to the given value (never lowers it).
# Used by join-network.sh --bootstrap-from so rsync-bootstrapped relay nodes
# can load their snapshot even when the stored UTXO count drifts slightly from
# the on-disk DB count.
cmd_set_snapshot_tolerance() {
  local min_pct="$1"
  [[ -n "$min_pct" ]] || die "Usage: $0 set-snapshot-tolerance <pct>"
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"
  ensure_pyyaml

  local tmp cfg_dir lockfile
  cfg_dir="$(dirname "$CONFIG_FILE")"
  lockfile="${cfg_dir}/.node-config.lock"
  tmp=$(mktemp "${cfg_dir}/.node-config-XXXXXX")

  exec 9>"$lockfile"
  flock -x 9

  trap 'rm -f "${tmp:-}"' EXIT

  python3 - "$CONFIG_FILE" "$min_pct" "$tmp" <<'EOF'
import yaml, sys

cfg_path, min_pct_s, out_path = sys.argv[1], sys.argv[2], sys.argv[3]
min_pct = float(min_pct_s)

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

snap = cfg.setdefault("snapshot", {})
current = snap.get("utxo_count_tolerance_pct", 0)
if float(current) >= min_pct:
    print(f"utxo_count_tolerance_pct already {current} (>= {min_pct_s}), kept")
    sys.exit(0)

snap["utxo_count_tolerance_pct"] = min_pct

with open(out_path, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
print(f"Set utxo_count_tolerance_pct: {current} -> {min_pct}")
EOF

  if [[ $? -ne 0 ]]; then
    rm -f "$tmp"; exit 0  # Python printed "already N, kept" and exited 0; or real error
  fi
  [[ -s "$tmp" ]] || { rm -f "$tmp"; exit 0; }

  validate_yaml "$tmp" || die "Validation failed — config not written."

  chmod --reference="$CONFIG_FILE" "$tmp" 2>/dev/null || true
  backup_config
  mv "$tmp" "$CONFIG_FILE"
  ok "snapshot.utxo_count_tolerance_pct updated."
  warn "Restart aperod-node to apply: systemctl restart aperod-node"
}

# ── restore-backup ────────────────────────────────────────────────────────────
# Finds the newest timestamped backup beside CONFIG_FILE and mv's it back into
# place atomically.  The superseded (current) config is kept as a backup itself
# so the restore is reversible.
cmd_restore_backup() {
  [[ -f "$CONFIG_FILE" ]] || die "Config not found: $CONFIG_FILE"

  local cfg_dir newest_bak ts lockfile
  cfg_dir="$(dirname "$CONFIG_FILE")"
  local base
  base="$(basename "$CONFIG_FILE")"
  lockfile="${cfg_dir}/.node-config.lock"

  # Acquire the same exclusive advisory lock used by cmd_add / cmd_remove so
  # that a restore cannot race against a concurrent mutation.  Without the lock
  # two concurrent operations could each read the config, produce temp files,
  # and then have the later mv silently overwrite the earlier commit.
  exec 9>"$lockfile"
  flock -x 9

  # Find the most-recently-modified .bak-* file for this config.
  # `|| true` prevents `set -eo pipefail` from triggering an early exit when
  # `ls` finds no matching files (exit 1) before we reach the explicit die.
  newest_bak=$(ls -1t "${cfg_dir}/${base}.bak-"* 2>/dev/null | head -1 || true)
  [[ -n "$newest_bak" ]] || die "No backup found for $CONFIG_FILE (looked for ${cfg_dir}/${base}.bak-*)"

  echo "  Newest backup : $newest_bak"
  echo "  Diff (backup vs current):"
  diff "$newest_bak" "$CONFIG_FILE" || true
  echo ""

  # Validate the backup before we do anything destructive.
  ensure_pyyaml
  python3 - <<EOF
import sys, yaml
try:
    with open("${newest_bak}") as f:
        yaml.safe_load(f)
    sys.exit(0)
except Exception as e:
    print(f"Backup YAML validation failed: {e}", file=sys.stderr)
    sys.exit(1)
EOF

  # Atomically swap: first save the current config so the restore is reversible,
  # then put the backup in place with a single rename(2).
  # Use a distinct prefix (.before-restore-) so this snapshot is never
  # mistaken for a regular pre-write backup (.bak-) and does not collide
  # with the file we are about to restore even if clocks show the same second.
  # mktemp ensures uniqueness even for rapid successive restores.
  ts=$(date -u +%Y%m%d-%H%M%S)
  local bak_of_current
  bak_of_current=$(mktemp "${CONFIG_FILE}.before-restore-${ts}-XXXXXX") || \
    die "Cannot create pre-restore snapshot temp file — aborting restore."
  cp --preserve=mode "$CONFIG_FILE" "$bak_of_current" 2>/dev/null || \
    cp "$CONFIG_FILE" "$bak_of_current" || \
    { rm -f "$bak_of_current"; die "Cannot write pre-restore snapshot — aborting restore."; }
  ok "Current config saved as: $bak_of_current"

  mv "$newest_bak" "$CONFIG_FILE"
  ok "Restored: $newest_bak → $CONFIG_FILE"
  echo ""
  warn "Restart aperod-node to apply: systemctl restart aperod-node"
}

# ── dispatch ──────────────────────────────────────────────────────────────────
SUBCMD="${1:-help}"
shift || true

case "$SUBCMD" in
  list-bootnodes)         cmd_list ;;
  add-bootnode)           cmd_add "${1:-}" ;;
  remove-bootnode)        cmd_remove "${1:-}" ;;
  set-field)              cmd_set_field "${1:-}" "${2:-}" ;;
  unset-field)            cmd_unset_field "${1:-}" ;;
  set-snapshot-tolerance) cmd_set_snapshot_tolerance "${1:-}" ;;
  restore-backup)         cmd_restore_backup ;;
  help|--help|-h)
    echo "Usage: $0 <subcommand> [args]"
    echo ""
    echo "Subcommands:"
    echo "  list-bootnodes                — List current bootnodes in node.yaml"
    echo "  add-bootnode    <addr>        — Safely append a bootnode"
    echo "  remove-bootnode <addr>        — Remove a bootnode by exact match"
    echo "  set-field       <key> <value> — Set a top-level field (bool/int/string)"
    echo "  unset-field     <key>         — Remove a top-level field (no-op if absent)"
    echo "  set-snapshot-tolerance <pct> — Raise snapshot.utxo_count_tolerance_pct (never lowers)"
    echo "  restore-backup                — Roll back to the most-recent pre-write backup"
    echo ""
    echo "Config file: ${CONFIG_FILE}"
    echo "Override:    APEROD_CONFIG=/path/to/node.yaml $0 ..."
    ;;
  *)
    die "Unknown subcommand: $SUBCMD. Run '$0 help' for usage."
    ;;
esac
