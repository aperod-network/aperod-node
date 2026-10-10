#!/usr/bin/env bash
# ============================================================
#  upgrade-node.sh — Safe, single-command Aperod node upgrade
#
#  This is the CANONICAL upgrade path for production operators.
#  Run it instead of a manual `git pull + make build + systemctl restart`
#  sequence to guarantee that memory-protection drop-ins are always
#  present and active after every binary upgrade.
#
#  What it does (in order):
#    1. Verifies the node was previously installed (abort on fresh servers).
#    2. Writes / verifies the memory-protection drop-ins via ensure-dropin.sh:
#         timeout.conf     — TimeoutStopSec=900  (prevents snapshot truncation)
#         gomemlimit.conf  — GOMEMLIMIT=5 GiB    (prevents OOM-kill)
#       This step is idempotent: files are only written when their content
#       has changed, so re-running the script is always safe.
#    3. Delegates the full upgrade sequence to update-node.sh, which:
#         • Pulls the latest source  (git pull)
#         • Rebuilds the Go binary   (make build)
#         • Stops the service        (systemctl stop)
#         • Installs the new binary  (/usr/local/bin/aperod-node)
#         • Restarts the service     (systemctl start)
#         • Polls the health endpoint until the node responds
#
#  Because ensure-dropin.sh runs BEFORE update-node.sh restarts the
#  service, the restarted process always inherits the correct
#  GOMEMLIMIT and TimeoutStopSec values — even on nodes that were
#  installed before task-1429 added those drop-ins.
#
#  Usage:
#    sudo bash /opt/aperod/blockchain/deploy/upgrade-node.sh
#
#  All env-var overrides accepted by update-node.sh are passed through:
#    SKIP_HEALTH_CHECK=1   — skip the post-restart API poll
#    SKIP_PEER_CHECK=1     — skip the post-restart P2P peer count check
#    HEALTH_MAX_ATTEMPTS   — poll attempts before health check fails (default 15)
#    HEALTH_WAIT_SECS      — seconds between health poll attempts (default 2)
#    PEER_WAIT_SECS        — seconds to wait for at least one peer (default 30)
#    SUPPORT_BOT_TOKEN     — Telegram bot token for failure alerts
#    SUPPORT_ADMIN_CHAT_ID — Telegram chat ID for failure alerts
#
#  Injectable seams for testing (mirrors ensure-dropin.sh):
#    DROPIN_DIR — drop-in directory (default: /etc/systemd/system/aperod-node.service.d)
#    SYSTEMCTL  — systemctl binary  (default: systemctl)
#
#  Idempotency:
#    Safe to run multiple times.  ensure-dropin.sh only rewrites drop-in
#    files when their content has changed, and update-node.sh aborts
#    before touching anything if the build fails.
# ============================================================
set -euo pipefail

# ── Colour helpers ─────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()    { echo -e "${CYAN}[upgrade]${NC}  $*"; }
ok()      { echo -e "${GREEN}[upgrade]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[upgrade]${NC}  $*"; }
die()     { echo -e "${RED}[upgrade]${NC}  $*" >&2; exit 1; }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Locate sibling scripts relative to this file ──────────
UPGRADE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENSURE_DROPIN_SH="${UPGRADE_DIR}/ensure-dropin.sh"
UPDATE_NODE_SH="${UPGRADE_DIR}/update-node.sh"

# ── Guard: require root ────────────────────────────────────
if [[ $(id -u) -ne 0 ]]; then
  die "Run as root: sudo bash ${BASH_SOURCE[0]}"
fi

# ── Guard: sibling scripts must exist ─────────────────────
[[ -f "${ENSURE_DROPIN_SH}" ]] \
  || die "ensure-dropin.sh not found at ${ENSURE_DROPIN_SH}"
[[ -f "${UPDATE_NODE_SH}" ]] \
  || die "update-node.sh not found at ${UPDATE_NODE_SH}"

# ── Guard: abort on fresh servers (update-node.sh checks too,
#    but an early message here is more helpful for operators) ──
BINARY_DST="/usr/local/bin/aperod-node"
SERVICE_FILE="/etc/systemd/system/aperod-node.service"
if [[ ! -f "${BINARY_DST}" && ! -f "${SERVICE_FILE}" ]]; then
  die "Node is not installed on this server.
  No binary at    : ${BINARY_DST}
  No service at   : ${SERVICE_FILE}

  upgrade-node.sh upgrades an existing installation.
  For a first-time install run install-node.sh instead."
fi

# ── Banner ─────────────────────────────────────────────────
echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod Node — Safe Upgrade                          ║
╚════════════════════════════════════════════════════════════╝${NC}
"

# ── Step 1: Guarantee memory-protection drop-ins ──────────
#
# ensure-dropin.sh writes (or verifies) two drop-in files:
#   timeout.conf     — TimeoutStopSec=900
#   gomemlimit.conf  — GOMEMLIMIT=5 GiB
# It calls `systemctl daemon-reload` at the end so the values
# are active the moment update-node.sh restarts the service.
#
# The DROPIN_DIR and SYSTEMCTL env vars are forwarded so test
# harnesses can override the defaults without modifying the script.
section "Step 1/2 — Verifying memory-protection drop-ins"
info "Running ensure-dropin.sh..."

# Pass through injectable seams if set by the caller.
env_prefix=""
[[ -n "${DROPIN_DIR:-}" ]]  && env_prefix+="DROPIN_DIR=${DROPIN_DIR} "
[[ -n "${SYSTEMCTL:-}" ]]   && env_prefix+="SYSTEMCTL=${SYSTEMCTL} "

if [[ -n "${env_prefix}" ]]; then
  env ${env_prefix} bash "${ENSURE_DROPIN_SH}"
else
  bash "${ENSURE_DROPIN_SH}"
fi

ok "Memory-protection drop-ins verified."

# ── Step 2: Pull, rebuild, and restart ───────────────────
#
# update-node.sh is the full upgrade engine.  All env-var
# overrides accepted by that script are forwarded automatically
# because this script inherits the caller's environment.
section "Step 2/2 — Upgrading binary via update-node.sh"
info "Running update-node.sh..."

bash "${UPDATE_NODE_SH}"

# ── Done ──────────────────────────────────────────────────
echo ""
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Upgrade complete — memory protections confirmed active.${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════════${NC}"
echo ""
echo -e "  Drop-in directory : /etc/systemd/system/aperod-node.service.d/"
echo -e "  Service status    : systemctl status aperod-node"
echo -e "  Live logs         : journalctl -u aperod-node -f"
echo ""
