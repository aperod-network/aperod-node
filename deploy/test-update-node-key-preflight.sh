#!/usr/bin/env bash
# =============================================================================
#  Tests for the validator-key permission preflight in update-node.sh
#
#  Regression guard for the ~20-minute outage caused when a deploy stopped the
#  working node, installed a new binary that enforces chmod 600 on the validator
#  key, and only THEN failed the startup check — leaving the service in a
#  crash-restart loop until an operator ran chmod by hand.
#
#  The preflight must, BEFORE the service is stopped:
#    K1. resolve the key path from node.yaml (data_dir + validator_key)
#    K2. auto-fix an unsafe (644-style) permission with chmod g-rwx,o-rwx — no downtime
#    K3. auto-fix wrong ownership with chown aperod:aperod
#    K4. never loosen an already-safe (600 or 400) key — a no-op, no chmod/chown issued
#    K5. abort (return 1) when the key exists but cannot be made safe
#    K6. treat a missing key file as a non-validator node (return 0, no-op)
#    K7. fall back to the standard prod layout when node.yaml has no key info
#
#  Plus static ordering guards that the preflight is wired in update-node.sh to
#  run BEFORE the `systemctl stop` step (the whole point — zero downtime).
#
#  The two helper functions are extracted verbatim from update-node.sh and
#  exercised with stubbed chmod / chown / stat so no real key files, users, or
#  privileges are involved.
#
#  Usage:  bash deploy/test-update-node-key-preflight.sh
#  Exit:   0 all passed / 1 failures
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_NODE="${SCRIPT_DIR}/update-node.sh"
[[ -f "$UPDATE_NODE" ]] || { echo "ERROR: not found: $UPDATE_NODE" >&2; exit 1; }

GREEN='\033[0;32m'; RED='\033[0;31m'; NC='\033[0m'
PASS=0; FAIL=0
pass_test() { echo -e "${GREEN}[PASS]${NC}  $1"; PASS=$((PASS+1)); }
fail_test() { echo -e "${RED}[FAIL]${NC}  $1"; FAIL=$((FAIL+1)); }

# ── Extract the two helper functions verbatim from update-node.sh ────────────
RESOLVE_SRC="$(awk '/^_resolve_validator_key_path\(\) \{/,/^\}/' "$UPDATE_NODE")"
PREFLIGHT_SRC="$(awk '/^preflight_validator_key\(\) \{/,/^\}/' "$UPDATE_NODE")"

if [[ -z "$RESOLVE_SRC" ]]; then
    fail_test "K0a: _resolve_validator_key_path() not found in update-node.sh"
else
    pass_test "K0a: _resolve_validator_key_path() present in update-node.sh"
fi
if [[ -z "$PREFLIGHT_SRC" ]]; then
    fail_test "K0b: preflight_validator_key() not found in update-node.sh"
else
    pass_test "K0b: preflight_validator_key() present in update-node.sh"
fi

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# Runs preflight_validator_key against a fake key file with a given mode/owner.
#   $1 — a unique case tag (used to name the per-case state dir)
#   $2 — starting mode as an octal string reported by the stat stub (e.g. 644)
#   $3 — starting owner reported by the stat stub (e.g. root)
#   $4 — "ok" (chmod/chown succeed) or "fail-chmod" / "fail-chown"
# The stub stat reads the *current* mode/owner from state files, so after a
# successful chmod the re-verify stat sees 600.  Emits the function's stdout and
# writes chmod/chown invocations to a per-case ops.log the caller can inspect at
# a deterministic path ($WORKDIR/<tag>/ops.log).
run_preflight() {
    local tag="$1" start_mode="$2" start_owner="$3" mode="$4"
    local statedir="$WORKDIR/$tag"
    mkdir -p "$statedir"
    echo "$start_mode"  > "$statedir/mode"
    echo "$start_owner" > "$statedir/owner"
    : > "$statedir/ops.log"
    local keyfile="$statedir/validator.key"
    : > "$keyfile"

    STATEDIR="$statedir" FAILMODE="$mode" PREFLIGHT_SRC="$PREFLIGHT_SRC" \
        bash -s "$keyfile" <<'SCEN'
set -uo pipefail
# stat stub: -c '%a' → mode file, -c '%U' → owner file (ignore the path arg)
stat() {
    if [[ "${1:-}" == "-c" ]]; then
        case "$2" in
            '%a') cat "$STATEDIR/mode"  ;;
            '%U') cat "$STATEDIR/owner" ;;
            *)    return 1 ;;
        esac
        return 0
    fi
    return 1
}
chmod() {
    echo "chmod $*" >> "$STATEDIR/ops.log"
    [[ "$FAILMODE" == "fail-chmod" ]] && return 1
    # Reflect the tightened mode: group/other bits stripped, owner bits kept
    # (mirrors `chmod g-rwx,o-rwx` semantics — 644→600, 400→400).
    m="$(cat "$STATEDIR/mode")"
    echo "${m%??}00" > "$STATEDIR/mode"
    return 0
}
chown() {
    echo "chown $*" >> "$STATEDIR/ops.log"
    [[ "$FAILMODE" == "fail-chown" ]] && return 1
    echo "aperod" > "$STATEDIR/owner"
    return 0
}
eval "$PREFLIGHT_SRC"
preflight_validator_key "$1"
echo "__RC=$?"
SCEN
}
# Deterministic ops-log path for a given case tag.
ops_log() { echo "$WORKDIR/$1/ops.log"; }

# Extract the trailing __RC from a run's captured output.
rc_of() { grep -oE '__RC=[0-9]+' <<<"$1" | tail -1 | cut -d= -f2; }

# ── K2: unsafe 644 permissions → chmod 600 issued, returns 0 ─────────────────
{
    out="$(run_preflight k2 644 aperod ok)"; log="$(ops_log k2)"
    if [[ "$(rc_of "$out")" == "0" ]] && grep -q "chmod g-rwx,o-rwx" "$log"; then
        pass_test "K2: 644 key auto-fixed with chmod g-rwx,o-rwx (returns 0, no downtime)"
    else
        fail_test "K2: expected chmod g-rwx,o-rwx + rc 0 (rc=$(rc_of "$out"), ops=$(cat "$log"))"
    fi
}

# ── K3: wrong owner → chown aperod:aperod issued ─────────────────────────────
{
    out="$(run_preflight k3 600 root ok)"; log="$(ops_log k3)"
    if [[ "$(rc_of "$out")" == "0" ]] && grep -q "chown aperod:aperod" "$log"; then
        pass_test "K3: root-owned key auto-fixed with chown aperod:aperod (returns 0)"
    else
        fail_test "K3: expected chown aperod:aperod + rc 0 (rc=$(rc_of "$out"), ops=$(cat "$log"))"
    fi
}

# ── K4: already-safe 600/aperod → no-op, permissions never loosened ──────────
{
    out="$(run_preflight k4 600 aperod ok)"; log="$(ops_log k4)"
    if [[ "$(rc_of "$out")" == "0" ]] && [[ ! -s "$log" ]]; then
        pass_test "K4: safe 600/aperod key is a no-op — permissions never loosened"
    else
        fail_test "K4: expected no chmod/chown for a safe key (rc=$(rc_of "$out"), ops=$(cat "$log"))"
    fi
}

# ── K5: chmod fails → returns 1 so the caller aborts BEFORE stopping ─────────
{
    out="$(run_preflight k5 644 aperod fail-chmod)"
    if [[ "$(rc_of "$out")" == "1" ]]; then
        pass_test "K5: unfixable key returns 1 → deploy aborts before the node is stopped"
    else
        fail_test "K5: expected rc 1 when chmod fails (rc=$(rc_of "$out"))"
    fi
}

# ── K6: missing key file → non-validator node, returns 0 no-op ───────────────
{
    out="$(STATEDIR="$WORKDIR/none" FAILMODE=ok PREFLIGHT_SRC="$PREFLIGHT_SRC" \
        bash -s "$WORKDIR/does-not-exist.key" <<'SCEN'
set -uo pipefail
eval "$PREFLIGHT_SRC"
preflight_validator_key "$1"
echo "__RC=$?"
SCEN
)"
    if [[ "$(rc_of "$out")" == "0" ]] && grep -q "non-validator node" <<<"$out"; then
        pass_test "K6: missing key file → treated as non-validator node (returns 0)"
    else
        fail_test "K6: expected rc 0 + non-validator message (rc=$(rc_of "$out"), out=$out)"
    fi
}

# ── K1 / K7: path resolution from node.yaml and prod-layout fallback ──────────
run_resolve() {
    local node_yaml="$1" blockchain_dir="$2"
    RESOLVE_SRC="$RESOLVE_SRC" BLOCKCHAIN_DIR="$blockchain_dir" \
        bash -s "$node_yaml" <<'SCEN'
set -uo pipefail
eval "$RESOLVE_SRC"
_resolve_validator_key_path "$1"
SCEN
}

# K1: node.yaml with data_dir → resolves <data_dir>/validator.key
{
    rt="$WORKDIR/k1"; mkdir -p "$rt/data/testnet"
    : > "$rt/data/testnet/validator.key"
    cat > "$rt/node.yaml" <<YAML
data_dir: ./data/testnet
consensus:
  validator_key: ""
YAML
    got="$(run_resolve "$rt/node.yaml" "$rt")"
    if [[ "$got" == "$rt/data/testnet/validator.key" ]]; then
        pass_test "K1: key path resolved from node.yaml data_dir (<data_dir>/validator.key)"
    else
        fail_test "K1: expected $rt/data/testnet/validator.key, got '$got'"
    fi
}

# K1b: explicit consensus.validator_key path wins
{
    rt="$WORKDIR/k1b"; mkdir -p "$rt/keys"
    : > "$rt/keys/my.key"
    cat > "$rt/node.yaml" <<YAML
data_dir: ./data/testnet
consensus:
  validator_key: "$rt/keys/my.key"
YAML
    got="$(run_resolve "$rt/node.yaml" "$rt")"
    if [[ "$got" == "$rt/keys/my.key" ]]; then
        pass_test "K1b: explicit consensus.validator_key path takes precedence"
    else
        fail_test "K1b: expected $rt/keys/my.key, got '$got'"
    fi
}

# K7: no node.yaml → falls back to the standard prod layout under BLOCKCHAIN_DIR
{
    rt="$WORKDIR/k7"; mkdir -p "$rt/data/testnet"
    : > "$rt/data/testnet/validator.key"
    got="$(run_resolve "$rt/does-not-exist.yaml" "$rt")"
    if [[ "$got" == "$rt/data/testnet/validator.key" ]]; then
        pass_test "K7: falls back to prod layout (BLOCKCHAIN_DIR/data/testnet/validator.key)"
    else
        fail_test "K7: expected prod-layout fallback, got '$got'"
    fi
}

# ── K4b: read-only 400/aperod key → no-op, owner bits never loosened ─────────
{
    out="$(run_preflight k4b 400 aperod ok)"; log="$(ops_log k4b)"
    if [[ "$(rc_of "$out")" == "0" ]] && [[ ! -s "$log" ]]; then
        pass_test "K4b: 400/aperod key is a no-op — never loosened to 600"
    else
        fail_test "K4b: expected no chmod/chown for a 400 key (rc=$(rc_of "$out"), ops=$(cat "$log"))"
    fi
}

# ── S1: preflight is wired to run BEFORE the systemctl stop step ─────────────
{
    call_line="$(grep -n 'if ! preflight_validator_key "' "$UPDATE_NODE" | head -1 | cut -d: -f1 || true)"
    stop_line="$(grep -n 'systemctl stop "${SERVICE_NAME}"' "$UPDATE_NODE" | head -1 | cut -d: -f1 || true)"
    if [[ -n "$call_line" && -n "$stop_line" && "$call_line" -lt "$stop_line" ]]; then
        pass_test "S1: preflight (line $call_line) runs BEFORE systemctl stop (line $stop_line) — zero downtime"
    else
        fail_test "S1: preflight must run before systemctl stop (preflight=$call_line, stop=$stop_line)"
    fi
}

# ── S2: the auto-fix only ever tightens (chmod 600, never a looser mode) ──────
{
    # Inspect the extracted preflight body: every chmod applied to the key path
    # must be the symbolic tighten-only form `chmod g-rwx,o-rwx` (strips
    # group/other, preserves owner bits) so it can never loosen a stricter key.
    key_chmods="$(grep -E 'chmod [^ ]+ "\$\{key_path\}"' <<<"$PREFLIGHT_SRC" || true)"
    other_chmods="$(grep -E 'chmod [^ ]+ "\$\{key_path\}"' <<<"$PREFLIGHT_SRC" | grep -v 'chmod g-rwx,o-rwx ' || true)"
    if [[ -n "$key_chmods" && -z "$other_chmods" ]]; then
        pass_test "S2: preflight only uses chmod g-rwx,o-rwx on the key (never loosens permissions)"
    else
        fail_test "S2: preflight must only use chmod g-rwx,o-rwx on the key (found: '${other_chmods:-<no chmod on key>}')"
    fi
}

# ── S3: the belt-and-braces dry-run also runs before the stop step ───────────
{
    dry_line="$(grep -n -- '--validate-config --config' "$UPDATE_NODE" | head -1 | cut -d: -f1 || true)"
    stop_line="$(grep -n 'systemctl stop "${SERVICE_NAME}"' "$UPDATE_NODE" | head -1 | cut -d: -f1 || true)"
    if [[ -n "$dry_line" && -n "$stop_line" && "$dry_line" -lt "$stop_line" ]]; then
        pass_test "S3: new-binary --validate-config dry-run (line $dry_line) runs before stop (line $stop_line)"
    else
        fail_test "S3: dry-run must run before systemctl stop (dry=$dry_line, stop=$stop_line)"
    fi
}

echo ""
TOTAL=$((PASS + FAIL))
echo -e "Results: ${GREEN}${PASS}/${TOTAL} passed${NC}"
if [[ "$FAIL" -eq 0 ]]; then
    exit 0
else
    echo -e "${RED}${FAIL} test(s) failed.${NC}" >&2
    exit 1
fi
