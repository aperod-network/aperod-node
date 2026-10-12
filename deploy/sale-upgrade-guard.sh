#!/usr/bin/env bash
# Sourced by update entrypoints. No installed root tool is replaced by this file.
# The candidate must implement the read-only maintenance mode. No opt-out.
sale_upgrade_guard() (
    set -euo pipefail
    local binary="$1" config="$2" result
    command -v python3 >/dev/null || { echo "Sale preflight: python3 missing; service NOT stopped." >&2; exit 1; }
    [[ -x "$binary" && -f "$config" ]] || {
        echo "Sale preflight: candidate/config unavailable; service NOT stopped." >&2
        exit 1
    }
    # Legacy nodes ignore unknown flags. Probe with their safe validation mode
    # before asking for the new command, so they cannot accidentally start.
    result=$("$binary" --sale-upgrade-preflight-capability --validate-config --config "$config") || exit 1
    printf '%s' "$result" | python3 -c '
import json,sys
x=json.load(sys.stdin)
assert x=={"schema":1,"command":"sale-upgrade-preflight"} and type(x["schema"]) is int
' || { echo "Sale preflight unsupported; service NOT stopped." >&2; exit 1; }
    result=$("$binary" --sale-upgrade-preflight --config "$config") || {
        echo "Sale preflight refused; service NOT stopped. Preserve the refused backup copy." >&2
        exit 1
    }
    # Unsupported flags, arbitrary exit-0 output and malformed receipts fail.
    printf '%s' "$result" | python3 -c '
import json, sys
x=json.load(sys.stdin)
assert set(x)=={"schema","status","mode","tip_height","tip_hash"}
assert type(x["schema"]) is int and x["schema"]==1 and x["status"]=="ok"
assert x["mode"] in ("inactive","activation_pending","active")
assert type(x["tip_height"]) is int and 0<=x["tip_height"]<2**64
assert isinstance(x["tip_hash"],str) and len(x["tip_hash"])==64 and x["tip_hash"]!="0"*64
assert all(c in "0123456789abcdef" for c in x["tip_hash"])
' || { echo "Sale preflight receipt invalid; service NOT stopped." >&2; exit 1; }
    echo "Sale preflight: signed configuration and canonical copy consistent."
)
