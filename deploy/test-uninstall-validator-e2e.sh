#!/usr/bin/env bash
# =============================================================================
#  test-uninstall-validator-e2e.sh — End-to-end smoke test for uninstall-validator.sh
#
#  Strategy
#  ────────
#  Spin up a disposable Ubuntu 22.04 Docker container pre-seeded with the
#  expected post-install file layout:
#
#    • /etc/systemd/system/aperod-node.service  — fake unit file
#    • /usr/local/bin/aperod-node               — stub binary
#    • /usr/local/bin/aperod                    — stub binary
#    • /etc/aperod/node.yaml                    — minimal config
#    • /var/lib/aperod/testnet/                 — empty data dir
#    • /opt/aperod/                             — empty install dir
#    • system user "aperod"                     — created before the run
#
#  A fake systemctl stub simulates a running service (PID file in
#  /run/fake-systemd/aperod-node.pid) so uninstall-validator.sh sees the
#  service as active and issues a real "stop" (which kills the stub).
#
#  The uninstaller is run non-interactively via APEROD_UNINSTALL_CONFIRM=YES.
#
#  Assertions checked after the uninstaller exits:
#    A1.  uninstall-validator.sh exits 0.
#    A2.  systemctl is-active aperod-node returns non-zero (service gone).
#    A3.  /etc/systemd/system/aperod-node.service is absent.
#    A4.  /usr/local/bin/aperod-node is absent.
#    A5.  /usr/local/bin/aperod is absent.
#    A6.  /etc/aperod/ is absent.
#    A7.  /var/lib/aperod/ is absent.
#    A8.  /opt/aperod/ is absent.
#    A9.  system user "aperod" is absent.
#
#  Skip condition:
#    Docker not found in PATH → test prints SKIP and exits 0.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-uninstall-validator-e2e.sh
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

IMAGE_TAG="aperod-uninstall-e2e-test:latest"

# ── Build context in a temp directory ────────────────────────────────────────
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"; docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true' EXIT

# Copy the whole deploy directory so the uninstaller finds sibling files
cp -r "$SCRIPT_DIR" "$CTX/deploy"

# ── Stub commands (written into CTX/stubs/) ───────────────────────────────────
mkdir -p "$CTX/stubs"

# ufw — no-op
cat > "$CTX/stubs/ufw" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# systemctl — manages fake process lifecycle via PID file.
#
# Calls handled:
#   is-active [--quiet] aperod-node  → check PID liveness (exits 0 if alive)
#   stop aperod-node                 → kill the process and remove PID file
#   disable aperod-node              → exit 0
#   daemon-reload                    → exit 0
#   (anything else)                  → exit 0
cat > "$CTX/stubs/systemctl" << 'STUB'
#!/usr/bin/env bash
STATE_DIR="/run/fake-systemd"
PID_FILE="$STATE_DIR/aperod-node.pid"
LOG_FILE="/tmp/fake-systemctl.log"

mkdir -p "$STATE_DIR"
echo "[fake-systemctl] $*" >> "$LOG_FILE"

# Strip --quiet from argument matching
ARGS="${*//--quiet/}"
ARGS="${ARGS//  / }"
ARGS="${ARGS# }"
ARGS="${ARGS% }"

case "$ARGS" in
  "is-active aperod-node"|"is-active  aperod-node")
    if [[ -f "$PID_FILE" ]] && kill -0 "$(cat "$PID_FILE" 2>/dev/null)" 2>/dev/null; then
      [[ "$*" != *"--quiet"* ]] && echo "active"
      exit 0
    fi
    [[ "$*" != *"--quiet"* ]] && echo "inactive"
    exit 3
    ;;
  "is-enabled aperod-node"|"is-enabled  aperod-node")
    # Pretend enabled so the uninstaller runs the disable step
    exit 0
    ;;
  "stop aperod-node")
    if [[ -f "$PID_FILE" ]]; then
      PID=$(cat "$PID_FILE" 2>/dev/null || true)
      if [[ -n "$PID" ]]; then
        kill "$PID" 2>/dev/null || true
        sleep 0.1
        kill -9 "$PID" 2>/dev/null || true
      fi
      rm -f "$PID_FILE"
      echo "[fake-systemctl] stopped aperod-node (PID=$PID)" >> "$LOG_FILE"
    fi
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
STUB

chmod +x "$CTX/stubs/"*

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

# ── Pre-seed the post-install file layout ────────────────────────────────────
echo "══════════════════════════════════════════════════"
echo "  Pre-seeding post-install layout"
echo "══════════════════════════════════════════════════"

# Create system user
useradd --system --no-create-home aperod 2>/dev/null || true
echo "  [seed] user 'aperod' created"

# Stub binary: a long-running sleep that the fake systemctl can track
cat > /usr/local/bin/aperod-node << 'BINSTUB'
#!/usr/bin/env bash
sleep 3600
BINSTUB
chmod +x /usr/local/bin/aperod-node
echo "  [seed] /usr/local/bin/aperod-node"

# Wallet CLI stub
cat > /usr/local/bin/aperod << 'BINSTUB'
#!/usr/bin/env bash
exit 0
BINSTUB
chmod +x /usr/local/bin/aperod
echo "  [seed] /usr/local/bin/aperod"

# Systemd service unit
mkdir -p /etc/systemd/system
cat > /etc/systemd/system/aperod-node.service << 'UNIT'
[Unit]
Description=Aperod Node
After=network.target

[Service]
User=aperod
ExecStart=/usr/local/bin/aperod-node
Restart=always

[Install]
WantedBy=multi-user.target
UNIT
echo "  [seed] /etc/systemd/system/aperod-node.service"

# GOMEMLIMIT drop-in (optional; uninstaller removes the unit file, not this dir)
mkdir -p /etc/systemd/system/aperod-node.service.d

# Config directory
mkdir -p /etc/aperod
echo "network: testnet" > /etc/aperod/node.yaml
echo "  [seed] /etc/aperod/node.yaml"

# Data directory
mkdir -p /var/lib/aperod/testnet
echo "  [seed] /var/lib/aperod/testnet"

# Install directory
mkdir -p /opt/aperod
echo "  [seed] /opt/aperod"

# Start the fake service: fork the stub and write PID
mkdir -p /run/fake-systemd
/usr/local/bin/aperod-node &
echo $! > /run/fake-systemd/aperod-node.pid
echo "  [seed] fake aperod-node started (PID=$(cat /run/fake-systemd/aperod-node.pid))"

# Verify it looks active before running the uninstaller
if systemctl is-active --quiet aperod-node; then
  echo "  [seed] service appears active — ready to uninstall"
else
  echo "  [seed] WARNING: service did NOT appear active before uninstall"
fi

echo ""
echo "══════════════════════════════════════════════════"
echo "  Running uninstall-validator.sh (non-interactive)"
echo "══════════════════════════════════════════════════"

APEROD_UNINSTALL_CONFIRM=YES bash /deploy/uninstall-validator.sh
UNINSTALL_EXIT=$?

echo ""
echo "══════════════════════════════════════════════════"
echo "  Assertions"
echo "══════════════════════════════════════════════════"

# A1: uninstaller must exit 0
if [[ $UNINSTALL_EXIT -eq 0 ]]; then
  pass_assert "A1: uninstall-validator.sh exited 0"
else
  fail_assert "A1: uninstall-validator.sh exited $UNINSTALL_EXIT (expected 0)"
fi

# A2: service must be inactive (fake systemctl checks PID liveness)
if systemctl is-active --quiet aperod-node 2>/dev/null; then
  fail_assert "A2: systemctl is-active aperod-node returned 0 — service still running"
  echo "     fake-systemctl log:" >&2
  cat /tmp/fake-systemctl.log >&2 || true
else
  pass_assert "A2: systemctl is-active aperod-node returned non-zero (service gone)"
fi

# A3: service unit file must be absent
if [[ ! -f /etc/systemd/system/aperod-node.service ]]; then
  pass_assert "A3: /etc/systemd/system/aperod-node.service is absent"
else
  fail_assert "A3: /etc/systemd/system/aperod-node.service still exists"
fi

# A4: aperod-node binary must be absent
if [[ ! -f /usr/local/bin/aperod-node ]]; then
  pass_assert "A4: /usr/local/bin/aperod-node is absent"
else
  fail_assert "A4: /usr/local/bin/aperod-node still exists"
fi

# A5: aperod binary must be absent
if [[ ! -f /usr/local/bin/aperod ]]; then
  pass_assert "A5: /usr/local/bin/aperod is absent"
else
  fail_assert "A5: /usr/local/bin/aperod still exists"
fi

# A6: config directory must be absent
if [[ ! -d /etc/aperod ]]; then
  pass_assert "A6: /etc/aperod/ is absent"
else
  fail_assert "A6: /etc/aperod/ still exists"
fi

# A7: data directory must be absent
if [[ ! -d /var/lib/aperod ]]; then
  pass_assert "A7: /var/lib/aperod/ is absent"
else
  fail_assert "A7: /var/lib/aperod/ still exists"
fi

# A8: install directory must be absent
if [[ ! -d /opt/aperod ]]; then
  pass_assert "A8: /opt/aperod/ is absent"
else
  fail_assert "A8: /opt/aperod/ still exists"
fi

# A9: system user must be absent
if ! id aperod &>/dev/null 2>&1; then
  pass_assert "A9: system user 'aperod' is absent"
else
  fail_assert "A9: system user 'aperod' still exists"
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

# Install what the test harness needs:
#   bash      — uninstaller is a bash script
#   passwd    — provides useradd/userdel
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
       bash passwd \
 && rm -rf /var/lib/apt/lists/*

# Stub commands — prepended to PATH in the test harness
COPY stubs/       /stubs/
# Real deploy directory — uninstall-validator.sh + sibling files
COPY deploy/      /deploy/
# Test harness
COPY test-harness.sh /test-harness.sh

# Pre-create directories the uninstaller or harness expects
RUN mkdir -p /etc/systemd/system /run/fake-systemd

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
echo -e "\n${BOLD}Running uninstall-validator.sh e2e test inside container…${NC}"
echo "────────────────────────────────────────────────────"

if docker run --rm "$IMAGE_TAG" bash /test-harness.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}uninstall-validator.sh e2e smoke test PASSED.${NC}"
  exit 0
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}uninstall-validator.sh e2e smoke test FAILED.${NC}"
  exit 1
fi
