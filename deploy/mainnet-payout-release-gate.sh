#!/usr/bin/env bash
# Mainnet binary releases require the private two-source payout preflight.
# The release orchestrator supplies the success marker only after that check.

mainnet_payout_release_gate() {
  local configured_config="${1:-/etc/aperod/node.yaml}"
  local mainnet_config="${2-/var/lib/aperod/mainnet/node.yaml}"
  local config network mainnet_found=false config_found=false

  for config in "$configured_config" "$mainnet_config"; do
    [[ -f "$config" ]] || continue
    config_found=true
    if ! network=$(python3 - "$config" <<'PY'
import re
import sys
from pathlib import Path

try:
    text = Path(sys.argv[1]).read_text(encoding="utf-8")
except OSError:
    raise SystemExit(2)
for line in text.splitlines():
    if line.startswith("#") or line[:1].isspace():
        continue
    match = re.match(r"^network\s*:\s*(.*?)\s*(?:#.*)?$", line)
    if match:
        print(match.group(1).strip().strip("\"'").lower())
        break
else:
    raise SystemExit(3)
PY
    ); then
      echo "RELEASE BLOCKED: a node network configuration could not be verified." >&2
      return 1
    fi
    case "$network" in
      mainnet) mainnet_found=true ;;
      testnet|devnet|localnet) ;;
      *)
        echo "RELEASE BLOCKED: a node network configuration has an unknown network." >&2
        return 1
        ;;
    esac
  done

  if [[ "$config_found" != "true" ]]; then
    echo "RELEASE BLOCKED: no node network configuration could be verified." >&2
    return 1
  fi

  if [[ "$mainnet_found" == "true" && "${APEROD_MAINNET_PAYOUT_PREFLIGHT_OK:-}" != "1" ]]; then
    echo "RELEASE BLOCKED: Mainnet node updates require the approved payout preflight." >&2
    return 1
  fi
}
