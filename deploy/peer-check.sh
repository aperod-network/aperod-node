#!/usr/bin/env bash
# peer-check.sh — Peer connectivity check for aperod-node deploys.
#
# Source this file; then call  aperod_peer_check.
#
# Required env vars (set by the caller before sourcing):
#   STATS_URL         — full URL of /api/v1/network/stats
#   PEER_WAIT_SECS    — seconds to poll before giving up (default: 30)
#   SKIP_PEER_CHECK   — set to 1 to skip entirely
#   SKIP_HEALTH_CHECK — inherited from update-node.sh (skip if 1)
#   SERVICE_NAME      — systemd unit name, used in warning messages
#
# Required function (must be defined by the caller before calling
# aperod_peer_check):
#   send_telegram_alert <message>
#
# Exit behaviour: always 0 — the peer check is NON-FATAL.

aperod_peer_check() {
  if [[ "${SKIP_HEALTH_CHECK:-0}" == "1" || "${SKIP_PEER_CHECK:-0}" == "1" ]]; then
    return 0
  fi

  echo "==> [5b] Peer connectivity check (waiting up to ${PEER_WAIT_SECS}s for peers)..."

  local peer_count=0
  local peer_start=$SECONDS   # bash built-in: seconds since shell started
  while true; do
    local elapsed=$(( SECONDS - peer_start ))
    local remaining=$(( PEER_WAIT_SECS - elapsed ))
    [[ $remaining -le 0 ]] && break   # budget exhausted

    # Cap per-request total time to remaining budget (min 1 s so curl doesn't
    # get a zero/negative timeout).  --max-time bounds the full HTTP response,
    # not just the TCP connect, so a stalled server cannot block beyond budget.
    local curl_max=$(( remaining < 4 ? remaining : 4 ))
    [[ $curl_max -lt 1 ]] && curl_max=1

    local raw
    raw=$(curl -sf --connect-timeout 2 --max-time "${curl_max}" \
          "${STATS_URL}" 2>/dev/null || true)
    if [[ -n "$raw" ]]; then
      # Extract peer_count from the JSON response.
      # Use python3 if available, otherwise fall back to grep+sed.
      if command -v python3 &>/dev/null; then
        peer_count=$(echo "$raw" | python3 -c \
          "import sys,json; d=json.load(sys.stdin); print(d.get('peer_count', d.get('peers',{}).get('connected',0)))" \
          2>/dev/null || echo "0")
      else
        peer_count=$(echo "$raw" | grep -o '"peer_count":[0-9]*' | grep -o '[0-9]*$' || echo "0")
      fi
      if [[ "${peer_count:-0}" -gt 0 ]]; then
        echo "  ✓ P2P connected: ${peer_count} peer(s) active."
        break
      fi
    fi

    elapsed=$(( SECONDS - peer_start ))
    remaining=$(( PEER_WAIT_SECS - elapsed ))
    [[ $remaining -le 0 ]] && break
    local sleep_secs=$(( remaining < 2 ? remaining : 2 ))
    [[ $sleep_secs -lt 1 ]] && sleep_secs=1
    echo "  Still waiting for peers... (${elapsed}s / ${PEER_WAIT_SECS}s)"
    sleep "${sleep_secs}"
  done

  if [[ "${peer_count:-0}" -eq 0 ]]; then
    echo ""
    echo "⚠  WARNING: peer_count == 0 after ${PEER_WAIT_SECS}s — node may be network-isolated." >&2
    echo "   The API is responding but the node may be network-isolated after this deploy." >&2
    echo "   Common causes: bad TLS config, changed protocol version, firewall." >&2
    echo "   Check P2P logs : journalctl -u ${SERVICE_NAME} -n 200 --no-pager | grep -i 'p2p\|peer\|dial\|connect'" >&2
    echo "   Live stats     : curl -s ${STATS_URL} | jq ." >&2

    send_telegram_alert "⚠️ <b>aperod-node: zero P2P peers after restart</b>
Server: $(hostname)
No peers connected after ${PEER_WAIT_SECS}s.
The node may be network-isolated (bad TLS config, changed protocol version, or bootnodes unreachable).
Check: <code>journalctl -u ${SERVICE_NAME} -n 200 | grep -i p2p</code>
Stats: <code>curl -s ${STATS_URL}</code>"
    echo ""
  fi
}
