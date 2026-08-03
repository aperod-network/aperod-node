#!/usr/bin/env bash
# =============================================================================
#  test-install-node-e2e.sh — End-to-end smoke test for install-node.sh
#
#  Strategy
#  ────────
#  Spin up a disposable Ubuntu 22.04 Docker container pre-wired with:
#
#    • Stub commands in /stubs/ (prepended to PATH):
#        apt-get   – no-op (jq / python3 / curl pre-installed in image)
#        go        – reports go1.23.4 so install-node.sh skips Go download
#        wget      – creates a minimal tarball for the source-code step;
#                    no-op for any other URL
#        make      – copies the pre-built stub binaries into build/
#        ufw       – no-op
#        curl      – passes through to real curl for localhost calls;
#                    returns "10.0.0.1" for external IP-lookup URLs so the
#                    installer never falls through to the read() prompt
#        systemctl – on "start aperod-node" forks the stub binary and records
#                    the PID; on "is-active" checks the PID is alive
#
#    • Stub binaries in /stubs-extra/:
#        aperod-node  – Python3 HTTP server on 127.0.0.1:8545 → GET /health 200
#        aperod       – wallet stub that creates a minimal wallet JSON on
#                       "wallet create --out FILE"
#
#  The real deploy/ directory (install-node.sh + all sibling watchdog files) is
#  copied into the image at /deploy so the installer can find them via
#  BASH_SOURCE / SCRIPT_DIR.
#
#  Non-interactive answers piped via stdin:
#    "1\n"   →  wallet choice: 1 (create new wallet)
#  The curl stub always returns a valid IP so the external-IP read() prompt is
#  never reached.
#
#  Assertions checked after the installer exits:
#    A1.  Installer exits 0.
#    A2.  systemctl is-active --quiet aperod-node  returns 0.
#    A3.  curl http://127.0.0.1:8545/health        returns HTTP 200.
#    A4.  /etc/systemd/system/aperod-node.service  exists.
#    A5.  GOMEMLIMIT drop-in                       exists.
#    A6.  /etc/aperod/node.yaml                    exists.
#
#  Skip condition:
#    Docker is not found in PATH → test prints SKIP and exits 0.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-install-node-e2e.sh
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

IMAGE_TAG="aperod-install-e2e-test:latest"

# ── Build context in a temp directory ────────────────────────────────────────
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"; docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true' EXIT

# Copy the whole deploy directory so the installer finds sibling watchdog files
cp -r "$SCRIPT_DIR" "$CTX/deploy"

# ── Stub commands (written into CTX/stubs/) ───────────────────────────────────
mkdir -p "$CTX/stubs"

# apt-get — no-op (real packages installed at image build time)
cat > "$CTX/stubs/apt-get" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# go — pretend 1.23.4 is installed so install-node.sh skips the Go download
cat > "$CTX/stubs/go" << 'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  version) echo "go version go1.23.4 linux/amd64" ;;
  *) exit 0 ;;
esac
exit 0
STUB

# wget — create a minimal valid tarball for the aperod source download;
#        no-op for any other URL
cat > "$CTX/stubs/wget" << 'STUB'
#!/usr/bin/env bash
OUTPUT=""
prev=""
for arg in "$@"; do
  [[ "$prev" == "-O" ]] && OUTPUT="$arg"
  prev="$arg"
done

for arg in "$@"; do
  case "$arg" in
    *aperod-node*|*aperod-src*)
      if [[ -n "$OUTPUT" ]]; then
        # Create a minimal tarball: top-level dir + Makefile.
        # --strip-components=1 removes the top-level dir leaving just Makefile.
        # The Makefile copies pre-built stubs from /stubs-extra/ into build/.
        python3 - "$OUTPUT" << 'PYEOF'
import sys, tarfile, io

out_path = sys.argv[1]

# Makefile content — tabs required by make (embedded as \t in the byte string)
makefile = (
    b".PHONY: deps build\n"
    b"deps:\n"
    b"\t@true\n"
    b"build:\n"
    b"\tmkdir -p build"
    b" && cp /stubs-extra/aperod-node build/aperod-node"
    b" && cp /stubs-extra/aperod build/aperod\n"
)

buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w:gz") as tf:
    ti = tarfile.TarInfo(name="aperod-node-main/Makefile")
    ti.size = len(makefile)
    ti.mode = 0o644
    tf.addfile(ti, io.BytesIO(makefile))
buf.seek(0)
with open(out_path, "wb") as f:
    f.write(buf.read())
PYEOF
      fi
      exit 0
      ;;
  esac
done

# No-op for any other URL (Go toolchain etc. — not reached because go stub
# makes install-node.sh believe Go is already installed)
exit 0
STUB

# make — delegates to the Makefile injected by the wget stub
cat > "$CTX/stubs/make" << 'STUB'
#!/usr/bin/env bash
/usr/bin/make "$@"
STUB

# ufw — no-op
cat > "$CTX/stubs/ufw" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# curl — smart wrapper: returns "10.0.0.1" for external IP-lookup URLs so the
#        installer never falls through to the read() prompt; passes all other
#        calls (including the health-check assertion) to the real binary
cat > "$CTX/stubs/curl" << 'STUB'
#!/usr/bin/env bash
for arg in "$@"; do
  case "$arg" in
    *ifconfig.me*|*icanhazip.com*)
      echo "10.0.0.1"
      exit 0
      ;;
  esac
done
exec /usr/bin/curl "$@"
STUB

# systemctl — manages fake process lifecycle using PID files.
#
# "start aperod-node" reads the GENERATED unit file at
#   /etc/systemd/system/aperod-node.service
# to extract ExecStart= and User=, validates the user exists, then forks the
# command as that user via `runuser`.  This means a missing useradd step in
# install-node.sh (User=aperod but no aperod account) causes the stub to exit
# non-zero, failing assertion A2.
#
# Calls handled:
#   daemon-reload                        → exit 0
#   enable aperod-node                   → exit 0
#   start aperod-node                    → parse unit, verify user, fork
#   is-active [--quiet] aperod-node      → check PID liveness
#   enable --now aperod-node-watchdog…   → exit 0
#   restart aperod-node-watchdog.timer   → exit 0
#   list-timers …                        → exit 0
#   (anything else)                      → exit 0
cat > "$CTX/stubs/systemctl" << 'STUB'
#!/usr/bin/env bash
STATE_DIR="/run/fake-systemd"
PID_FILE="$STATE_DIR/aperod-node.pid"
LOG_FILE="/tmp/fake-systemctl.log"

mkdir -p "$STATE_DIR"
echo "[fake-systemctl] $*" >> "$LOG_FILE"

case "$*" in
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
    # A missing account here mirrors what real systemd would do (fail to start).
    if [[ -n "$SERVICE_USER" ]]; then
      if ! id "$SERVICE_USER" &>/dev/null; then
        echo "[fake-systemctl] ERROR: User='$SERVICE_USER' does not exist — installer did not create the account" >> "$LOG_FILE"
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

# aperod-node: minimal Python3 HTTP server responding to GET /health → 200
cat > "$CTX/stubs-extra/aperod-node" << 'STUB'
#!/usr/bin/env python3
"""Stub aperod-node: health endpoint for installer smoke test."""
import http.server, socketserver, sys

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"ok","syncing":false,"height":0}')
    def log_message(self, *a):
        pass  # suppress access log noise

try:
    with socketserver.TCPServer(("127.0.0.1", 8545), Handler) as srv:
        srv.serve_forever()
except Exception as e:
    print(f"aperod-node stub error: {e}", file=sys.stderr)
    sys.exit(1)
STUB

# aperod: wallet CLI stub
cat > "$CTX/stubs-extra/aperod" << 'STUB'
#!/usr/bin/env bash
case "${1:-} ${2:-}" in
  "wallet create")
    out=""
    prev=""
    for a in "$@"; do
      [[ "$prev" == "--out" ]] && out="$a"
      prev="$a"
    done
    if [[ -n "$out" ]]; then
      mkdir -p "$(dirname "$out")"
      printf '{"address":"apr1test00000000000000000000000000000000000","version":1}\n' > "$out"
    fi
    ;;
  "wallet address")
    echo "apr1test00000000000000000000000000000000000"
    ;;
  "wallet import")
    out=""
    prev=""
    for a in "$@"; do
      [[ "$prev" == "--out" ]] && out="$a"
      prev="$a"
    done
    if [[ -n "$out" ]]; then
      mkdir -p "$(dirname "$out")"
      printf '{"address":"apr1test00000000000000000000000000000000000","version":1}\n' > "$out"
    fi
    ;;
esac
exit 0
STUB

chmod +x "$CTX/stubs-extra/"*

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

echo "══════════════════════════════════════════════════"
echo "  Running install-node.sh (non-interactive)"
echo "══════════════════════════════════════════════════"

# Pipe "1\n" for the wallet-choice prompt (choice 1 = create new wallet).
# The curl stub returns a valid IP for external lookups so the external-IP
# read() prompt is never reached.
printf '1\n' | bash /deploy/install-node.sh
INSTALL_EXIT=$?

echo ""
echo "══════════════════════════════════════════════════"
echo "  Assertions"
echo "══════════════════════════════════════════════════"

# A1: installer must exit 0
if [[ $INSTALL_EXIT -eq 0 ]]; then
  pass_assert "A1: install-node.sh exited 0"
else
  fail_assert "A1: install-node.sh exited $INSTALL_EXIT (expected 0)"
fi

# A2: service must be active (fake systemctl checks PID liveness)
if systemctl is-active --quiet aperod-node; then
  pass_assert "A2: systemctl is-active aperod-node returned 0"
else
  fail_assert "A2: systemctl is-active aperod-node returned non-zero"
  echo "     fake-systemctl log:" >&2
  cat /tmp/fake-systemctl.log >&2 || true
  echo "     aperod-node stub log:" >&2
  cat /tmp/aperod-node-stub.log >&2 || true
fi

# A3: health endpoint must respond HTTP 200
HTTP_CODE=$(/usr/bin/curl -s -o /dev/null -w "%{http_code}" \
  --max-time 5 http://127.0.0.1:8545/health 2>/dev/null || echo "000")
if [[ "$HTTP_CODE" == "200" ]]; then
  pass_assert "A3: GET /health returned HTTP $HTTP_CODE"
else
  fail_assert "A3: GET /health returned HTTP $HTTP_CODE (expected 200)"
  echo "     aperod-node stub log:" >&2
  cat /tmp/aperod-node-stub.log >&2 || true
fi

# A4: systemd service file must be present
if [[ -f /etc/systemd/system/aperod-node.service ]]; then
  pass_assert "A4: /etc/systemd/system/aperod-node.service exists"
else
  fail_assert "A4: /etc/systemd/system/aperod-node.service NOT found"
fi

# A5: GOMEMLIMIT drop-in must be present
if [[ -f /etc/systemd/system/aperod-node.service.d/gomemlimit.conf ]]; then
  pass_assert "A5: GOMEMLIMIT drop-in exists"
else
  fail_assert "A5: GOMEMLIMIT drop-in NOT found"
fi

# A6: node config must be present
if [[ -f /etc/aperod/node.yaml ]]; then
  pass_assert "A6: /etc/aperod/node.yaml exists"
else
  fail_assert "A6: /etc/aperod/node.yaml NOT found"
fi

# A7: aperod system user must have been created by the installer
# (required because the service unit declares User=aperod; real systemd fails
# to start if the account is absent — and so does the fake systemctl stub)
if id aperod &>/dev/null; then
  pass_assert "A7: system user 'aperod' was created by the installer"
else
  fail_assert "A7: system user 'aperod' NOT created — real systemd would reject the service start"
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

# Install only what the installer and test harness genuinely need:
#   jq           — wallet address extraction in install-node.sh
#   python3      — aperod-node stub (HTTP health server) + wget stub (tarfile)
#   curl         — health-check assertion + installer IP lookup (smart stub)
#   make         — delegates to Makefile created by wget stub
#   bash         — installer is a bash script
#   passwd       — provides useradd; install-node.sh creates the aperod account
#   util-linux   — provides runuser; fake systemctl runs ExecStart as User=aperod
# apt-get calls from install-node.sh itself are intercepted by the apt-get stub
# so no network I/O happens during the installer run.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      jq python3 curl make bash ca-certificates passwd util-linux \
 && rm -rf /var/lib/apt/lists/*

# Stub commands — prepended to PATH in the test harness
COPY stubs/       /stubs/
# Pre-built stub binaries — referenced by the Makefile in the fake tarball
COPY stubs-extra/ /stubs-extra/
# Real deploy directory — install-node.sh + all sibling watchdog files
COPY deploy/      /deploy/
# Test harness
COPY test-harness.sh /test-harness.sh

# Ensure /etc/os-release identifies this as Ubuntu so the OS check passes
# (Ubuntu base image already contains this, but be explicit)
RUN grep -q Ubuntu /etc/os-release

# Pre-create /etc/systemd/system so the installer can write service files
# without needing systemd itself to be running
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
echo -e "\n${BOLD}Running install-node.sh e2e test inside container…${NC}"
echo "────────────────────────────────────────────────────"

if docker run --rm "$IMAGE_TAG" bash /test-harness.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}install-node.sh e2e smoke test PASSED.${NC}"
  exit 0
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}install-node.sh e2e smoke test FAILED.${NC}"
  exit 1
fi
