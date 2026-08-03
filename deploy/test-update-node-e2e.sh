#!/usr/bin/env bash
# =============================================================================
#  test-update-node-e2e.sh — End-to-end smoke test for update-node.sh
#
#  Strategy
#  ────────
#  Spin up a disposable Ubuntu 22.04 Docker container that starts from a state
#  simulating a previously installed Aperod node, then runs update-node.sh and
#  asserts the service is still healthy afterwards.
#
#  Pre-seeded "already installed" state (created inside the container before
#  update-node.sh runs):
#    • aperod system user
#    • /usr/local/bin/aperod-node           — "old" stub binary
#    • /etc/systemd/system/aperod-node.service
#    • /etc/aperod/node.yaml
#    • /opt/aperod/blockchain/              — fake source tree with a git repo
#    • /opt/aperod/blockchain/deploy/       — real deploy/ scripts (incl. peer-check.sh)
#    • old service "running" via fake systemctl PID tracking
#
#  Stub commands in /stubs/ (prepended to PATH):
#    sudo      – strips "-u USER" and runs the rest as root (no sudo config needed)
#    git       – no-op (simulates successful git pull)
#    make      – copies the new stub binary from /stubs-extra/ into the build dir
#    curl      – passes through to real curl for localhost health calls
#    systemctl – handles stop/start/is-active/daemon-reload via PID files
#    ufw       – no-op
#
#  Pre-built stub binaries in /stubs-extra/:
#    aperod-node  – Python3 HTTP server on 127.0.0.1:8545 answering:
#                     GET /api/v1/status → 200  (update-node.sh health URL)
#                     GET /health        → 200  (assertion convenience)
#
#  Env vars set for update-node.sh:
#    SKIP_PEER_CHECK=1          — bypass the 30-second peer-wait (no peers in test)
#    HEALTH_MAX_ATTEMPTS=10     — don't wait too long in CI
#    HEALTH_WAIT_SECS=1
#
#  Assertions checked after update-node.sh exits:
#    U1.  update-node.sh exits 0.
#    U2.  systemctl is-active --quiet aperod-node  returns 0.
#    U3.  curl http://127.0.0.1:8545/api/v1/status returns HTTP 200.
#    U4.  /usr/local/bin/aperod-node               is executable.
#    U5.  /etc/systemd/system/aperod-node.service  still exists.
#
#  Skip condition:
#    Docker is not found in PATH → test prints SKIP and exits 0.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-update-node-e2e.sh
#
#  Exit codes:
#    0 — all assertions passed (or Docker unavailable → skipped)
#    1 — one or more assertions failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
BOLD='\033[1m'; NC='\033[0m'

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Check Docker availability ─────────────────────────────────────────────────
if ! command -v docker &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker not found in PATH — skipping e2e test."
  exit 0
fi

if ! docker info &>/dev/null 2>&1; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker daemon not reachable — skipping e2e test."
  exit 0
fi

IMAGE_TAG="aperod-update-e2e-test:latest"

# ── Build context in a temp directory ────────────────────────────────────────
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"; docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true' EXIT

# Copy the whole deploy directory so update-node.sh finds peer-check.sh and
# other sibling files it sources via DEPLOY_DIR.
cp -r "$SCRIPT_DIR" "$CTX/deploy"

# ── Stub commands (written into CTX/stubs/) ───────────────────────────────────
mkdir -p "$CTX/stubs"

# sudo — strip "-u USER" / "-u" flag so commands run as root inside the
#        container without needing sudoers configuration.
cat > "$CTX/stubs/sudo" << 'STUB'
#!/usr/bin/env bash
# Rebuild args array, dropping -u <user> pairs.
args=()
skip_next=false
for arg in "$@"; do
  if $skip_next; then
    skip_next=false
    continue
  fi
  if [[ "$arg" == "-u" ]]; then
    skip_next=true
    continue
  fi
  args+=("$arg")
done
exec "${args[@]}"
STUB

# git — no-op (simulates `git -C /opt/aperod pull` succeeding)
cat > "$CTX/stubs/git" << 'STUB'
#!/usr/bin/env bash
# Accept any git subcommand and succeed silently.
exit 0
STUB

# make — simulate `make build` by copying the new stub binary into the build dir
cat > "$CTX/stubs/make" << 'STUB'
#!/usr/bin/env bash
# Handle "make build" (and "make deps") issued by update-node.sh.
case "${*}" in
  *build*|*deps*)
    mkdir -p /opt/aperod/blockchain/build
    cp /stubs-extra/aperod-node /opt/aperod/blockchain/build/aperod-node
    chmod +x /opt/aperod/blockchain/build/aperod-node
    echo "[stub-make] built /opt/aperod/blockchain/build/aperod-node"
    ;;
esac
exit 0
STUB

# ufw — no-op
cat > "$CTX/stubs/ufw" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# curl — pass through to real binary for localhost calls (health check assertions)
cat > "$CTX/stubs/curl" << 'STUB'
#!/usr/bin/env bash
exec /usr/bin/curl "$@"
STUB

# systemctl — manages fake process lifecycle using PID files.
#
# Calls handled:
#   stop aperod-node                     → kill existing PID (if any), exit 0
#   start aperod-node                    → parse unit file, fork stub binary
#   is-active [--quiet] aperod-node      → check PID liveness
#   daemon-reload                        → exit 0
#   (anything else)                      → exit 0
cat > "$CTX/stubs/systemctl" << 'STUB'
#!/usr/bin/env bash
STATE_DIR="/run/fake-systemd"
PID_FILE="$STATE_DIR/aperod-node.pid"
LOG_FILE="/tmp/fake-systemctl.log"

mkdir -p "$STATE_DIR"
echo "[fake-systemctl] $*" >> "$LOG_FILE"

case "$*" in
  "stop aperod-node")
    if [[ -f "$PID_FILE" ]]; then
      OLD_PID=$(cat "$PID_FILE" 2>/dev/null || true)
      if [[ -n "$OLD_PID" ]] && kill -0 "$OLD_PID" 2>/dev/null; then
        kill "$OLD_PID" 2>/dev/null || true
        sleep 0.3
        echo "[fake-systemctl] stopped aperod-node (was PID=$OLD_PID)" >> "$LOG_FILE"
      fi
      rm -f "$PID_FILE"
    fi
    exit 0
    ;;

  "start aperod-node")
    UNIT_FILE="/etc/systemd/system/aperod-node.service"
    if [[ ! -f "$UNIT_FILE" ]]; then
      echo "[fake-systemctl] ERROR: unit file not found: $UNIT_FILE" >> "$LOG_FILE"
      exit 1
    fi

    # Extract ExecStart= from the [Service] section
    EXEC_START=$(awk '/^\[Service\]/{f=1} f && /^ExecStart=/{sub(/^ExecStart=/,""); print; exit}' "$UNIT_FILE")
    # Extract User= from the [Service] section
    SERVICE_USER=$(awk '/^\[Service\]/{f=1} f && /^User=/{sub(/^User=/,""); print; exit}' "$UNIT_FILE")

    echo "[fake-systemctl] ExecStart='$EXEC_START'" >> "$LOG_FILE"
    echo "[fake-systemctl] User='$SERVICE_USER'"     >> "$LOG_FILE"

    if [[ -z "$EXEC_START" ]]; then
      echo "[fake-systemctl] ERROR: ExecStart not found in $UNIT_FILE" >> "$LOG_FILE"
      exit 1
    fi

    # If the unit specifies a User=, verify the account exists before forking.
    if [[ -n "$SERVICE_USER" ]]; then
      if ! id "$SERVICE_USER" &>/dev/null; then
        echo "[fake-systemctl] ERROR: User='$SERVICE_USER' does not exist" >> "$LOG_FILE"
        exit 1
      fi
      echo "[fake-systemctl] user '$SERVICE_USER' verified OK" >> "$LOG_FILE"
      runuser -u "$SERVICE_USER" -- bash -c "exec $EXEC_START" \
        >> /tmp/aperod-node-stub.log 2>&1 &
    else
      bash -c "exec $EXEC_START" >> /tmp/aperod-node-stub.log 2>&1 &
    fi

    echo $! > "$PID_FILE"
    echo "[fake-systemctl] started aperod-node (PID=$(cat "$PID_FILE"))" >> "$LOG_FILE"
    exit 0
    ;;

  "is-active --quiet aperod-node"|"is-active aperod-node")
    if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE" 2>/dev/null)" 2>/dev/null; then
      exit 0
    fi
    exit 3
    ;;

  *)
    exit 0
    ;;
esac
STUB

chmod +x "$CTX/stubs/"*

# ── Pre-built stub binaries (written into CTX/stubs-extra/) ──────────────────
mkdir -p "$CTX/stubs-extra"

# aperod-node: handles --version (used by update-node.sh Step 4 to log the
# installed binary version) and then acts as a Python3 HTTP server responding
# to the URLs update-node.sh uses:
#   GET /api/v1/status → 200  (HEALTH_URL polled by health-check step)
#   GET /health        → 200  (used in assertion U3 for convenience)
#
# IMPORTANT: --version must print a line and exit 0 immediately.
# update-node.sh Step 4 runs:
#   echo "  Installed: $(${BINARY_DST} --version 2>/dev/null || ls -lh ...)"
# If the binary never exits the command substitution hangs forever, blocking
# the service start and all subsequent assertions.
cat > "$CTX/stubs-extra/aperod-node" << 'STUB'
#!/usr/bin/env python3
"""Stub aperod-node: --version exit + health endpoints for smoke test."""
import sys

# Handle --version before starting the server so `aperod-node --version`
# prints a line and exits immediately (required by update-node.sh Step 4).
if len(sys.argv) > 1 and sys.argv[1] == "--version":
    print("aperod-node v0.0.0-stub")
    sys.exit(0)

import http.server, socketserver

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true,"syncing":false,"height":1}')
    def log_message(self, *a):
        pass  # suppress access log noise

try:
    with socketserver.TCPServer(("127.0.0.1", 8545), Handler) as srv:
        srv.serve_forever()
except Exception as e:
    print(f"aperod-node stub error: {e}", file=sys.stderr)
    sys.exit(1)
STUB

chmod +x "$CTX/stubs-extra/"*

# ── Preseed script (sets up the "already installed" state inside the container)
cat > "$CTX/preseed.sh" << 'PRESEED'
#!/usr/bin/env bash
# Create the system state that represents a previously installed Aperod node.
set -euo pipefail

# 1. Create the aperod system user (same as install-node.sh would have done).
useradd --system --no-create-home --shell /usr/sbin/nologin aperod 2>/dev/null || true

# 2. Directory structure
mkdir -p /opt/aperod/blockchain/build
mkdir -p /opt/aperod/blockchain/deploy
mkdir -p /etc/aperod
mkdir -p /etc/systemd/system/aperod-node.service.d
mkdir -p /var/log/aperod
mkdir -p /run/fake-systemd

chown aperod:aperod /opt/aperod 2>/dev/null || true

# 3. Initialize a minimal git repo in /opt/aperod so `git -C /opt/aperod pull`
#    doesn't complain about a missing repo (the stub git is a no-op anyway, but
#    the directory must exist before update-node.sh cds into it).
git init /opt/aperod/blockchain >/dev/null 2>&1 || true
git -C /opt/aperod/blockchain config user.email "test@test.com" || true
git -C /opt/aperod/blockchain config user.name  "Test"          || true

# 4. Copy the real deploy scripts so DEPLOY_DIR/peer-check.sh is found
cp /deploy/* /opt/aperod/blockchain/deploy/ 2>/dev/null || true

# 5. "Old" binary — a simple no-op placeholder (the update will replace it)
cp /stubs-extra/aperod-node /usr/local/bin/aperod-node
chmod +x /usr/local/bin/aperod-node

# 6. Service file (mirrors what install-node.sh would have written)
cat > /etc/systemd/system/aperod-node.service << 'UNIT'
[Unit]
Description=Aperod Node
After=network.target

[Service]
User=aperod
ExecStart=/usr/local/bin/aperod-node
Restart=always
StandardOutput=journal
StandardError=journal
SyslogIdentifier=aperod-node

[Install]
WantedBy=multi-user.target
UNIT

# 7. GOMEMLIMIT drop-in (would have been written by install-node.sh)
cat > /etc/systemd/system/aperod-node.service.d/gomemlimit.conf << 'DROPIN'
[Service]
Environment="GOMEMLIMIT=5368709120"
DROPIN

# 8. Minimal node config
cat > /etc/aperod/node.yaml << 'YAML'
network: testnet
p2p:
  listen_addr: "0.0.0.0:7777"
rpc:
  listen_addr: "0.0.0.0:8545"
YAML

# 9. Start the "old" service so there is a running PID to stop
UNIT_FILE="/etc/systemd/system/aperod-node.service"
STATE_DIR="/run/fake-systemd"
PID_FILE="$STATE_DIR/aperod-node.pid"
runuser -u aperod -- bash -c "exec /usr/local/bin/aperod-node" \
  >> /tmp/aperod-node-stub.log 2>&1 &
echo $! > "$PID_FILE"
echo "[preseed] old aperod-node started (PID=$(cat "$PID_FILE"))"

echo "[preseed] Pre-seeded state complete."
PRESEED
chmod +x "$CTX/preseed.sh"

# ── Test harness script (runs inside the container) ───────────────────────────
cat > "$CTX/test-harness.sh" << 'HARNESS'
#!/usr/bin/env bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail_assert() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }

# Stubs take priority over system binaries
export PATH="/stubs:$PATH"

# ── Step 1: Pre-seed the "previously installed" state ────────────────────────
echo "══════════════════════════════════════════════════"
echo "  Pre-seeding installed-node state"
echo "══════════════════════════════════════════════════"
bash /preseed.sh

# ── Step 2: Run update-node.sh ───────────────────────────────────────────────
echo ""
echo "══════════════════════════════════════════════════"
echo "  Running update-node.sh (non-interactive)"
echo "══════════════════════════════════════════════════"

# update-node.sh is designed for /opt/aperod layout; run it from there.
# SKIP_PEER_CHECK=1  → bypass the 30-second peer-wait (no peers in a test env)
# HEALTH_MAX_ATTEMPTS / HEALTH_WAIT_SECS → keep the test fast
SKIP_PEER_CHECK=1 \
HEALTH_MAX_ATTEMPTS=10 \
HEALTH_WAIT_SECS=1 \
  bash /deploy/update-node.sh
UPDATE_EXIT=$?

echo ""
echo "══════════════════════════════════════════════════"
echo "  Assertions"
echo "══════════════════════════════════════════════════"

# U1: update-node.sh must exit 0
if [[ $UPDATE_EXIT -eq 0 ]]; then
  pass_assert "U1: update-node.sh exited 0"
else
  fail_assert "U1: update-node.sh exited $UPDATE_EXIT (expected 0)"
fi

# U2: service must be active after the upgrade
if systemctl is-active --quiet aperod-node; then
  pass_assert "U2: systemctl is-active aperod-node returned 0"
else
  fail_assert "U2: systemctl is-active aperod-node returned non-zero"
  echo "     fake-systemctl log:" >&2
  cat /tmp/fake-systemctl.log >&2 || true
  echo "     aperod-node stub log:" >&2
  cat /tmp/aperod-node-stub.log >&2 || true
fi

# U3: health endpoint must respond HTTP 200 after the upgrade
HTTP_CODE=$(/usr/bin/curl -s -o /dev/null -w "%{http_code}" \
  --max-time 5 http://127.0.0.1:8545/api/v1/status 2>/dev/null || echo "000")
if [[ "$HTTP_CODE" == "200" ]]; then
  pass_assert "U3: GET /api/v1/status returned HTTP $HTTP_CODE"
else
  fail_assert "U3: GET /api/v1/status returned HTTP $HTTP_CODE (expected 200)"
  echo "     aperod-node stub log:" >&2
  cat /tmp/aperod-node-stub.log >&2 || true
fi

# U4: the binary must be executable at the expected path
if [[ -x /usr/local/bin/aperod-node ]]; then
  pass_assert "U4: /usr/local/bin/aperod-node is executable"
else
  fail_assert "U4: /usr/local/bin/aperod-node not found or not executable"
fi

# U5: service file must still exist (the upgrade must not remove it)
if [[ -f /etc/systemd/system/aperod-node.service ]]; then
  pass_assert "U5: /etc/systemd/system/aperod-node.service still exists"
else
  fail_assert "U5: /etc/systemd/system/aperod-node.service was removed"
fi

echo ""
echo "──────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL assertions FAILED.${NC}"
  exit 1
fi
HARNESS
chmod +x "$CTX/test-harness.sh"

# ── Dockerfile ────────────────────────────────────────────────────────────────
cat > "$CTX/Dockerfile" << 'DOCKER'
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Install only what the test harness and update-node.sh genuinely need:
#   python3      — aperod-node stub (HTTP health server)
#   curl         — health-check assertions
#   bash         — update-node.sh is a bash script
#   passwd       — provides useradd
#   util-linux   — provides runuser
#   git          — git stub uses /stubs/git; real git not called, but the
#                  binary must resolve somewhere for PATH lookup
#   make         — our stub /stubs/make delegates build; system make not called
#   ca-certificates — curl HTTPS (Telegram alert path, never reached in test)
# apt-get calls from update-node.sh itself are not present (update does no
# package installation), so no apt-get stub is needed.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      python3 curl bash ca-certificates passwd util-linux git make \
 && rm -rf /var/lib/apt/lists/*

# Stub commands — prepended to PATH inside the test harness
COPY stubs/       /stubs/
# Pre-built stub binaries (the "new" aperod-node that make installs)
COPY stubs-extra/ /stubs-extra/
# Real deploy directory — update-node.sh + peer-check.sh + all siblings
COPY deploy/      /deploy/
# Pre-seed and test harness scripts
COPY preseed.sh        /preseed.sh
COPY test-harness.sh   /test-harness.sh

# Pre-create standard system directories
RUN mkdir -p /etc/systemd/system /run/fake-systemd /etc/aperod /var/log/aperod

CMD ["bash", "/test-harness.sh"]
DOCKER

# ── Build the test image ──────────────────────────────────────────────────────
echo -e "\n${BOLD}Building Docker test image…${NC}"
if ! docker build --quiet -t "$IMAGE_TAG" "$CTX" 2>&1; then
  # Retry with verbose output on failure so the error is visible
  docker build -t "$IMAGE_TAG" "$CTX"
  echo -e "${RED}[ERR]${NC}  Docker build failed" >&2
  exit 1
fi
echo -e "${GREEN}[OK]${NC}   Image built: $IMAGE_TAG"

# ── Run the test container ────────────────────────────────────────────────────
echo -e "\n${BOLD}Running update-node.sh e2e test inside container…${NC}"
echo "────────────────────────────────────────────────────"

if docker run --rm "$IMAGE_TAG" bash /test-harness.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}update-node.sh e2e smoke test PASSED.${NC}"
  exit 0
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}update-node.sh e2e smoke test FAILED.${NC}"
  exit 1
fi
