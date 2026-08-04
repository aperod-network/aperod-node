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

# make — simulate `make build` by copying the new stub binary into the build dir.
# IMPORTANT: use /bin/cp (absolute path) so this stub works correctly even when
# a failing cp stub is prepended to PATH for the Scenario 2 rollback test.
cat > "$CTX/stubs/make" << 'STUB'
#!/usr/bin/env bash
# Handle "make build" (and "make deps") issued by update-node.sh.
case "${*}" in
  *build*|*deps*)
    mkdir -p /opt/aperod/blockchain/build
    /bin/cp /stubs-extra/aperod-node /opt/aperod/blockchain/build/aperod-node
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

# ── Scenario 2: rollback test ─────────────────────────────────────────────────
# A stub `cp` (placed in /stubs-rollback/) exits 1 whenever it is called.
# update-node.sh uses /bin/cp explicitly for backup and restore, so those
# succeed via the absolute path; only the plain `cp <src> <dst>` install call
# goes through PATH and hits the stub, causing it to fail.
# Expected outcome: update-node.sh exits non-zero, old binary is restored at
# /usr/local/bin/aperod-node, and the service is back up (rollback restarted it).

mkdir -p "$CTX/stubs-rollback"

# cp stub — always fails (simulates disk-full / permission-denied mid-install)
cat > "$CTX/stubs-rollback/cp" << 'STUB'
#!/usr/bin/env bash
# Simulate a failed binary copy (e.g. ETXTBSY, disk full, permission denied).
echo "[stub-cp] cp $* → injected failure" >&2
exit 1
STUB
chmod +x "$CTX/stubs-rollback/cp"

cat > "$CTX/test-harness-rollback.sh" << 'HARNESS'
#!/usr/bin/env bash
# Scenario 2 — broken install: cp fails mid-install after service is stopped.
#
# The installed "old" binary is seeded with a DISTINCT version string
# ("v1.0.0-OLD") different from the "new" build stub ("v0.0.0-stub"), so
# assertions can prove the specific old binary was restored by rollback, not
# just that any executable remains.
#
# Expected outcomes:
#   R1. update-node.sh exits NON-ZERO (install failure detected).
#   R2. /usr/local/bin/aperod-node --version reports "v1.0.0-OLD" (old binary
#       content restored, not just any executable at that path).
#   R3. systemctl is-active aperod-node returns 0 (service restarted by rollback).
#   R4. /etc/systemd/system/aperod-node.service still exists.
#   R5. fake-systemctl log records "stop aperod-node" (service was stopped).
#   R6. fake-systemctl log records "start aperod-node" AFTER the stop
#       (rollback code path restarted the service, not the normal install path).
#   R7. Backup file /usr/local/bin/aperod-node.pre-update exists on disk
#       (Step 4 was reached and the backup step ran before the install failed).
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail_assert() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }

# ── Step 1: Pre-seed the base "previously installed" state ────────────────────
echo "══════════════════════════════════════════════════"
echo "  [Scenario 2] Pre-seeding installed-node state"
echo "══════════════════════════════════════════════════"
# Run preseed without any stub interference — it uses plain cp internally.
bash /preseed.sh

# ── Step 2: Replace the installed binary with a DISTINCTLY MARKED old version ─
# This lets the version-string assertion (R2) distinguish "old binary restored"
# from "some binary happens to be executable at that path".
# The new-build stub (/stubs-extra/aperod-node) reports "v0.0.0-stub"; this
# one reports "v1.0.0-OLD".  Both serve HTTP on :8545 so health checks pass.
# Overwrite the installed binary with a distinctly marked old version.
# The preseed-started process is a Python interpreter that already read and
# compiled the script file; overwriting the file on Linux leaves the running
# process unaffected (it holds its own file descriptors / in-memory bytecode).
# Step 3 of update-node.sh will stop that process normally.  After rollback,
# a new process is forked from the v1.0.0-OLD file content.
#
# We write to a temp file first, then /bin/cp (atomic replace on most
# filesystems) so the running process never reads a partial write.
cat > /tmp/aperod-node-old-stub.py << 'OLDSTUB'
#!/usr/bin/env python3
"""OLD stub aperod-node: --version exit + health endpoints for rollback test."""
import sys
if len(sys.argv) > 1 and sys.argv[1] == "--version":
    print("aperod-node v1.0.0-OLD")
    sys.exit(0)
import http.server, socketserver
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"ok":true,"syncing":false,"height":1,"version":"v1.0.0-OLD"}')
    def log_message(self, *a): pass
try:
    with socketserver.TCPServer(("127.0.0.1", 8545), Handler) as srv:
        srv.serve_forever()
except Exception as e:
    print(f"old stub error: {e}", file=sys.stderr)
    sys.exit(1)
OLDSTUB
/bin/cp /tmp/aperod-node-old-stub.py /usr/local/bin/aperod-node
chmod +x /usr/local/bin/aperod-node
echo "[scenario2-setup] /usr/local/bin/aperod-node replaced with v1.0.0-OLD marker."

# ── Step 3: Inject the failing cp stub and run update-node.sh ─────────────────
# /bin/cp is always the real binary (absolute path bypasses PATH lookup), so
# backup and restore succeed.  Only the PATH-resolved `cp` in Step 4 fails.
export PATH="/stubs-rollback:/stubs:$PATH"

echo ""
echo "══════════════════════════════════════════════════"
echo "  [Scenario 2] Running update-node.sh with cp stub"
echo "══════════════════════════════════════════════════"

SKIP_PEER_CHECK=1 \
HEALTH_MAX_ATTEMPTS=5 \
HEALTH_WAIT_SECS=1 \
  bash /deploy/update-node.sh
UPDATE_EXIT=$?

echo ""
echo "══════════════════════════════════════════════════"
echo "  [Scenario 2] Assertions"
echo "══════════════════════════════════════════════════"

# R1: update-node.sh must exit NON-ZERO (install failure)
if [[ $UPDATE_EXIT -ne 0 ]]; then
  pass_assert "R1: update-node.sh exited $UPDATE_EXIT (non-zero — install failure detected)"
else
  fail_assert "R1: update-node.sh exited 0 (expected non-zero — broken install was not caught)"
fi

# R2: the restored binary must report the OLD version string, not the new-build
#     stub version — proving the specific pre-update binary was restored.
RESTORED_VERSION=$(/usr/local/bin/aperod-node --version 2>/dev/null || echo "ERROR")
if [[ "$RESTORED_VERSION" == *"v1.0.0-OLD"* ]]; then
  pass_assert "R2: aperod-node --version reports '$RESTORED_VERSION' (old binary content restored)"
else
  fail_assert "R2: aperod-node --version reports '$RESTORED_VERSION' (expected 'v1.0.0-OLD' — wrong binary at destination)"
fi

# R3: service must be active (rollback restarted it with the old binary)
if systemctl is-active --quiet aperod-node; then
  pass_assert "R3: systemctl is-active aperod-node returned 0 (service restarted by rollback)"
else
  fail_assert "R3: systemctl is-active aperod-node returned non-zero (service left down)"
  echo "     fake-systemctl log:" >&2
  cat /tmp/fake-systemctl.log >&2 || true
  echo "     aperod-node stub log:" >&2
  cat /tmp/aperod-node-stub.log >&2 || true
fi

# R4: service file must still exist (the script must not remove it on failure)
if [[ -f /etc/systemd/system/aperod-node.service ]]; then
  pass_assert "R4: /etc/systemd/system/aperod-node.service still exists"
else
  fail_assert "R4: /etc/systemd/system/aperod-node.service was removed"
fi

# R5: fake-systemctl log must record a "stop aperod-node" call, proving the
#     service was actually stopped before the install attempt — not left running
#     in its pre-seeded state throughout.
SYSCTL_LOG=/tmp/fake-systemctl.log
if grep -q "stop aperod-node" "$SYSCTL_LOG" 2>/dev/null; then
  pass_assert "R5: fake-systemctl log records 'stop aperod-node' (service was stopped)"
else
  fail_assert "R5: 'stop aperod-node' not found in fake-systemctl log — service may never have been stopped"
  echo "     fake-systemctl log contents:" >&2
  cat "$SYSCTL_LOG" >&2 || echo "     (log file missing)" >&2
fi

# R6: fake-systemctl log must record a "start aperod-node" call AFTER the stop,
#     proving _rollback_install restarted the service rather than the normal
#     install-success path.  We verify line ordering in the log file.
if [[ -f "$SYSCTL_LOG" ]]; then
  STOP_LINE=$(grep -n "stop aperod-node" "$SYSCTL_LOG" 2>/dev/null | tail -1 | cut -d: -f1)
  START_LINE=$(grep -n "start aperod-node" "$SYSCTL_LOG" 2>/dev/null | tail -1 | cut -d: -f1)
  if [[ -n "$STOP_LINE" && -n "$START_LINE" && "$START_LINE" -gt "$STOP_LINE" ]]; then
    pass_assert "R6: fake-systemctl log records 'start aperod-node' after 'stop' (rollback restarted service)"
  else
    fail_assert "R6: 'start aperod-node' not found after 'stop' in fake-systemctl log — rollback may not have restarted the service"
    echo "     fake-systemctl log contents:" >&2
    cat "$SYSCTL_LOG" >&2 || echo "     (log file missing)" >&2
  fi
else
  fail_assert "R6: fake-systemctl log file missing"
fi

# R7: backup file must exist on disk, confirming Step 4 was reached and the
#     backup step ran before the install copy failed.
BINARY_BACKUP="/usr/local/bin/aperod-node.pre-update"
if [[ -f "$BINARY_BACKUP" ]]; then
  pass_assert "R7: backup file $BINARY_BACKUP exists (Step 4 was reached; backup was created)"
else
  fail_assert "R7: backup file $BINARY_BACKUP not found — Step 4 may not have been reached"
fi

echo ""
echo "──────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL rollback assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL rollback assertions FAILED.${NC}"
  exit 1
fi
HARNESS
chmod +x "$CTX/test-harness-rollback.sh"

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
COPY stubs/          /stubs/
# Rollback-scenario stubs: cp that always exits 1
COPY stubs-rollback/ /stubs-rollback/
# Pre-built stub binaries (the "new" aperod-node that make installs)
COPY stubs-extra/    /stubs-extra/
# Real deploy directory — update-node.sh + peer-check.sh + all siblings
COPY deploy/         /deploy/
# Pre-seed and test harness scripts
COPY preseed.sh               /preseed.sh
COPY test-harness.sh          /test-harness.sh
COPY test-harness-rollback.sh /test-harness-rollback.sh

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

# ── Run scenario 1: normal upgrade ───────────────────────────────────────────
echo -e "\n${BOLD}[Scenario 1] Running normal upgrade test…${NC}"
echo "────────────────────────────────────────────────────"

S1_PASS=false
if docker run --rm "$IMAGE_TAG" bash /test-harness.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}Scenario 1 (normal upgrade) PASSED.${NC}"
  S1_PASS=true
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}Scenario 1 (normal upgrade) FAILED.${NC}"
fi

# ── Run scenario 2: broken install → rollback ─────────────────────────────────
echo -e "\n${BOLD}[Scenario 2] Running broken-install rollback test…${NC}"
echo "────────────────────────────────────────────────────"

S2_PASS=false
if docker run --rm "$IMAGE_TAG" bash /test-harness-rollback.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}Scenario 2 (broken install → rollback) PASSED.${NC}"
  S2_PASS=true
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}Scenario 2 (broken install → rollback) FAILED.${NC}"
fi

# ── Overall result ────────────────────────────────────────────────────────────
echo ""
if [[ "$S1_PASS" == "true" && "$S2_PASS" == "true" ]]; then
  echo -e "${GREEN}${BOLD}All update-node.sh e2e scenarios PASSED.${NC}"
  exit 0
else
  echo -e "${RED}${BOLD}One or more update-node.sh e2e scenarios FAILED.${NC}"
  exit 1
fi
