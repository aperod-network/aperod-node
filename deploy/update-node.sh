#!/usr/bin/env bash
# update-node.sh — Pull latest source, rebuild the Go node binary, and restart
#                  the aperod-node systemd service.
#
# Run this script instead of manually building and copying the binary after
# every code change. The two most common mistakes it prevents:
#
#   1. Building to the wrong path (e.g. /opt/aperod/data/aperod-node instead of
#      /usr/local/bin/aperod-node) — the service keeps running the OLD binary
#      silently, and the update appears to succeed.
#
#   2. Copying over a running binary — causes "Text file busy" (ETXTBSY).
#      This script always stops the service BEFORE installing the new binary.
#
# Usage:
#   sudo bash /opt/aperod/blockchain/deploy/update-node.sh
#
# The script must be run as root (or via sudo) so it can:
#   - call systemctl stop/start
#   - write to /usr/local/bin/
#   - switch to the `aperod` user for git and Go operations
#
# Optional env vars for Telegram build-failure alerts:
#   SUPPORT_BOT_TOKEN      — Telegram bot token (e.g. 123456:ABC-xxx)
#   SUPPORT_ADMIN_CHAT_ID  — Telegram chat/user ID to receive the alert
#
# ---------------------------------------------------------------------------
# HEALTH CHECK
# ---------------------------------------------------------------------------
# After starting the service the script polls localhost:8545/api/v1/status
# up to HEALTH_MAX_ATTEMPTS times (default: 15), waiting HEALTH_WAIT_SECS
# (default: 2) between each attempt.  If the endpoint never responds the
# script exits non-zero and sends a Telegram alert.
#
# Configurable env vars:
#   HEALTH_MAX_ATTEMPTS  — poll attempts before giving up  (default: 15)
#   HEALTH_WAIT_SECS     — seconds between each poll       (default: 2)
#   SKIP_HEALTH_CHECK    — set to 1 to bypass the check entirely
#
set -euo pipefail

APEROD_DIR="/opt/aperod"
BLOCKCHAIN_DIR="${APEROD_DIR}/blockchain"
SERVICE_NAME="aperod-node"
BINARY_DST="/usr/local/bin/aperod-node"
BINARY_SRC="${BLOCKCHAIN_DIR}/build/aperod-node"
HEALTH_URL="http://localhost:8545/api/v1/status"
DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Health-check tunables (override via environment)
HEALTH_MAX_ATTEMPTS="${HEALTH_MAX_ATTEMPTS:-15}"
HEALTH_WAIT_SECS="${HEALTH_WAIT_SECS:-2}"
SKIP_HEALTH_CHECK="${SKIP_HEALTH_CHECK:-0}"

# ---------------------------------------------------------------------------
# Helper: send a Telegram message if credentials are configured.
# ---------------------------------------------------------------------------
send_telegram_alert() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      > /dev/null 2>&1 || true   # never let a failed alert abort the script
  fi
}

# ---------------------------------------------------------------------------
# Step 1: Pull latest source
# ---------------------------------------------------------------------------
echo "==> [1/5] Pulling latest source as aperod..."
sudo -u aperod git -C "$APEROD_DIR" pull

# ---------------------------------------------------------------------------
# Step 2: Rebuild Go binary — if this fails, abort before touching the service.
# The old binary keeps running untouched.
# ---------------------------------------------------------------------------
echo "==> [2/5] Building aperod-node (Go)..."
if ! sudo -u aperod bash -c "export PATH=\$PATH:/usr/local/go/bin; cd '${BLOCKCHAIN_DIR}' && make build"; then
  echo ""
  echo "✗ Build failed — service NOT stopped. The old binary is still running." >&2

  send_telegram_alert "⚠️ <b>aperod-node build FAILED</b>
Server: $(hostname)
The service was <b>NOT stopped</b> — the old binary is still running.
Fix the Go error and re-run <code>update-node.sh</code>."

  exit 1
fi

if [[ ! -f "${BINARY_SRC}" ]]; then
  echo "✗ Build succeeded but binary not found at ${BINARY_SRC}" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Step 3: Stop the service BEFORE copying.
#
# Copying over a running Go binary fails with "Text file busy" (ETXTBSY).
# Always stop first, install, then start — never the other way around.
# ---------------------------------------------------------------------------
echo "==> [3/5] Stopping ${SERVICE_NAME}..."
systemctl stop "${SERVICE_NAME}" || true   # non-fatal if already stopped
sleep 1

# ---------------------------------------------------------------------------
# Step 4: Install binary to the correct system path.
#
# The systemd service's ExecStart points to /usr/local/bin/aperod-node.
# Building to /opt/aperod/data/ or any other path silently leaves the service
# running the old code — this step ensures only the right path is updated.
# ---------------------------------------------------------------------------
echo "==> [4/5] Installing binary to ${BINARY_DST}..."
cp "${BINARY_SRC}" "${BINARY_DST}"
chmod +x "${BINARY_DST}"
echo "  Installed: $(${BINARY_DST} --version 2>/dev/null || ls -lh "${BINARY_DST}" | awk '{print $5, $9}')"

# Start the service after the new binary is in place.
systemctl start "${SERVICE_NAME}"
echo "  Service started."

# ---------------------------------------------------------------------------
# Step 5: Health check — wait for the node API to respond.
#
# Polls /api/v1/status up to HEALTH_MAX_ATTEMPTS times.
# Exits non-zero and fires a Telegram alert if the node never comes up.
# Set SKIP_HEALTH_CHECK=1 to bypass (e.g. when the RPC port is not exposed).
# ---------------------------------------------------------------------------
echo "==> [5/5] Health check (polling ${HEALTH_URL})..."

if [[ "$SKIP_HEALTH_CHECK" == "1" ]]; then
  echo "  SKIP_HEALTH_CHECK=1 — skipping."
else
  attempt=0
  healthy=false
  while [[ $attempt -lt $HEALTH_MAX_ATTEMPTS ]]; do
    attempt=$(( attempt + 1 ))
    if curl -sf --connect-timeout 2 "${HEALTH_URL}" > /dev/null 2>&1; then
      healthy=true
      break
    fi
    echo "  Waiting... (attempt ${attempt}/${HEALTH_MAX_ATTEMPTS})"
    sleep "${HEALTH_WAIT_SECS}"
  done

  if [[ "${healthy}" != "true" ]]; then
    echo ""
    echo "✗ Health check FAILED — node did not respond on ${HEALTH_URL}" >&2
    echo "  Inspect logs: journalctl -u ${SERVICE_NAME} -n 100 --no-pager" >&2

    send_telegram_alert "🚨 <b>aperod-node health check FAILED</b>
Server: $(hostname)
Node did not respond on <code>${HEALTH_URL}</code> after $(( HEALTH_MAX_ATTEMPTS * HEALTH_WAIT_SECS ))s.
Inspect logs: <code>journalctl -u ${SERVICE_NAME} -n 100</code>"

    exit 1
  fi
fi

# ── P2P peer count check (non-fatal warning) ────────────────────────────────
# After the HTTP API is up, wait for at least one P2P peer to reconnect.
# A deploy that breaks TLS config or the protocol version passes the API check
# while leaving the node completely isolated — this check catches that case.
#
# Configurable env vars:
#   PEER_WAIT_SECS  — total seconds to wait for a peer (default: 30)
#   SKIP_PEER_CHECK — set to 1 to bypass (e.g. single-node devnet)

PEER_STATS_URL="http://localhost:8545/api/v1/network/stats"
PEER_WAIT_SECS="${PEER_WAIT_SECS:-30}"
SKIP_PEER_CHECK="${SKIP_PEER_CHECK:-0}"

if [[ "$SKIP_HEALTH_CHECK" == "1" || "$SKIP_PEER_CHECK" == "1" ]]; then
  echo "  Skipping P2P peer check."
else
  echo ""
  echo "  Checking P2P connectivity (waiting up to ${PEER_WAIT_SECS}s for ≥1 peer)..."
  peer_ok=false
  peer_elapsed=0
  while [[ $peer_elapsed -lt $PEER_WAIT_SECS ]]; do
    peer_count=$(curl -sf --connect-timeout 2 "${PEER_STATS_URL}" 2>/dev/null \
      | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('peer_count',0))" 2>/dev/null || echo "0")
    if [[ "${peer_count:-0}" -gt 0 ]]; then
      echo "  ✓ P2P connected: ${peer_count} peer(s) active."
      peer_ok=true
      break
    fi
    sleep 5
    peer_elapsed=$(( peer_elapsed + 5 ))
    echo "    Still waiting... peer_count=${peer_count:-0} (${peer_elapsed}s / ${PEER_WAIT_SECS}s)"
  done

  if [[ "${peer_ok}" != "true" ]]; then
    echo ""
    echo "  ⚠  WARNING: node has 0 P2P peers after ${PEER_WAIT_SECS}s." >&2
    echo "     The API is responding but the node may be network-isolated." >&2
    echo "     Common causes: bad TLS config, changed protocol version, firewall." >&2
    echo "     Check: journalctl -u ${SERVICE_NAME} -n 50 --no-pager" >&2

    send_telegram_alert "⚠️ <b>aperod-node P2P isolation warning</b>
Server: $(hostname)
The node restarted successfully (API responds) but has <b>0 P2P peers</b> after ${PEER_WAIT_SECS}s.
The node may be network-isolated after this deploy.
Check: <code>journalctl -u ${SERVICE_NAME} -n 50</code>"
  fi
fi

echo ""
echo "✓ Update complete. New build is live."
echo "  Service status : systemctl status ${SERVICE_NAME}"
echo "  Live logs      : journalctl -u ${SERVICE_NAME} -f"
echo "  Chain tip      : curl -s ${HEALTH_URL} | jq ."
echo ""
echo "  Binary installed to : ${BINARY_DST}"
echo "  ⚠  Do NOT build to /opt/aperod/data/ — the service ignores that path."
