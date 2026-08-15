#!/usr/bin/env bash
# =============================================================================
#  test-install-node-primary-ip.sh — Focused test for install-node.sh --primary-ip
#
#  What it tests
#  ─────────────
#  Run 1 — with --primary-ip 10.0.0.2:
#    A1. node-config.sh add-bootnode was called exactly once.
#    A2. The multiaddr passed was /ip4/10.0.0.2/tcp/30303 (correct port).
#    A3. node.yaml contains the bootnode entry /ip4/10.0.0.2/tcp/30303.
#    A4. The node service was enabled/started (systemctl enable --now).
#
#  Run 2 — without --primary-ip:
#    A5. node-config.sh add-bootnode was NOT called.
#    A6. The warning block header "ВНИМАНИЕ: bootnode не настроен" appears in stdout.
#    A7. node.yaml p2p.bootnodes list is empty (no phantom entries).
#    A8. The node service was NOT started (safety hold).
#
#  Implementation notes
#  ────────────────────
#  A spy wrapper is installed at /deploy/node-config.sh inside the container
#  (install-node.sh calls it as bash "${SCRIPT_DIR}/node-config.sh", using the
#  absolute SCRIPT_DIR path — a PATH stub would not intercept it).  The spy:
#    1. Appends the subcommand + arguments to /tmp/node-config-spy.log
#    2. Delegates to the real script at /deploy/node-config.sh.real
#
#  The full installer infrastructure (apt-get, go, git, make, ufw, curl,
#  systemctl stubs) is identical to test-install-node-e2e.sh so the real
#  install-node.sh runs end-to-end, not a stripped excerpt.
#
#  Skip condition: Docker not found or daemon unreachable → exit 0 (skip).
#
#  Run from anywhere:
#    bash blockchain/deploy/test-install-node-primary-ip.sh
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

# ── Docker availability check ─────────────────────────────────────────────────
if ! command -v docker &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker not found in PATH — skipping primary-ip test."
  exit 0
fi
if ! docker info &>/dev/null 2>&1; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker daemon not reachable — skipping primary-ip test."
  exit 0
fi

IMAGE_TAG="aperod-primary-ip-test:latest"

# ── Build context ─────────────────────────────────────────────────────────────
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"; docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true' EXIT

cp -r "$SCRIPT_DIR" "$CTX/deploy"

# ── Stubs ─────────────────────────────────────────────────────────────────────
mkdir -p "$CTX/stubs"

# apt-get — no-op
cat > "$CTX/stubs/apt-get" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# go — report 1.23.4 so the installer skips Go download
cat > "$CTX/stubs/go" << 'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  version) echo "go version go1.23.4 linux/amd64" ;;
  *) exit 0 ;;
esac
exit 0
STUB

# git — create a minimal repo with a Makefile that copies stub binaries
cat > "$CTX/stubs/git" << 'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == "clone" ]]; then
  DEST="${!#}"
  mkdir -p "$DEST/.git"
  python3 - "$DEST/Makefile" << 'PYEOF'
import sys
makefile = (
    b".PHONY: deps build\n"
    b"deps:\n"
    b"\t@true\n"
    b"build:\n"
    b"\tmkdir -p build"
    b" && cp /stubs-extra/aperod-node build/aperod-node"
    b" && cp /stubs-extra/aperod build/aperod\n"
)
with open(sys.argv[1], "wb") as f:
    f.write(makefile)
PYEOF
  exit 0
fi
exit 0
STUB

# make — uses the real make to run the injected Makefile
cat > "$CTX/stubs/make" << 'STUB'
#!/usr/bin/env bash
/usr/bin/make "$@"
STUB

# wget — no-op (git stub handles the clone; no tarball download needed)
cat > "$CTX/stubs/wget" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# sudo — strip -u <user> and run as current user (container runs as root)
cat > "$CTX/stubs/sudo" << 'STUB'
#!/usr/bin/env bash
args=()
skip_next=false
for arg in "$@"; do
  if $skip_next; then skip_next=false; continue; fi
  if [[ "$arg" == "-u" || "$arg" == "--user" ]]; then skip_next=true; continue; fi
  if [[ "$arg" =~ ^-u.+ ]]; then continue; fi
  args+=("$arg")
done
exec "${args[@]}"
STUB

# ufw — no-op
cat > "$CTX/stubs/ufw" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# curl — return fixed IP for external lookups; pass through for localhost
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

# systemctl — fake lifecycle management with PID file + call log
cat > "$CTX/stubs/systemctl" << 'STUB'
#!/usr/bin/env bash
STATE_DIR="/run/fake-systemd"
PID_FILE="$STATE_DIR/aperod-node.pid"
CALLS_LOG="/tmp/systemctl-calls.log"
mkdir -p "$STATE_DIR"
echo "$*" >> "$CALLS_LOG"

case "$*" in
  "start aperod-node"|"enable --now aperod-node")
    UNIT_FILE="/etc/systemd/system/aperod-node.service"
    [[ -f "$UNIT_FILE" ]] || exit 1
    EXEC_START=$(awk '/^\[Service\]/{f=1} f && /^ExecStart=/{sub(/^ExecStart=/,""); print; exit}' "$UNIT_FILE")
    SERVICE_USER=$(awk '/^\[Service\]/{f=1} f && /^User=/{sub(/^User=/,""); print; exit}' "$UNIT_FILE")
    [[ -n "$EXEC_START" ]] || exit 1
    if [[ -n "$SERVICE_USER" ]]; then
      id "$SERVICE_USER" &>/dev/null || exit 1
      runuser -u "$SERVICE_USER" -- bash -c "exec $EXEC_START" \
        >> /tmp/aperod-node-stub.log 2>&1 &
    else
      bash -c "exec $EXEC_START" >> /tmp/aperod-node-stub.log 2>&1 &
    fi
    echo $! > "$PID_FILE"
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

# ── Stub binaries ─────────────────────────────────────────────────────────────
mkdir -p "$CTX/stubs-extra"

# aperod-node: minimal HTTP health server
cat > "$CTX/stubs-extra/aperod-node" << 'STUB'
#!/usr/bin/env python3
"""Stub aperod-node: health endpoint."""
import http.server, socketserver, sys

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"status":"ok","syncing":false,"height":0}')
    def log_message(self, *a):
        pass

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
    for a in "$@"; do [[ "$prev" == "--out" ]] && out="$a"; prev="$a"; done
    if [[ -n "$out" ]]; then
      mkdir -p "$(dirname "$out")"
      printf '{"address":"apr1test00000000000000000000000000000000000","version":1}\n' > "$out"
    fi
    ;;
  "wallet address") echo "apr1test00000000000000000000000000000000000" ;;
  "wallet import")
    out=""
    prev=""
    for a in "$@"; do [[ "$prev" == "--out" ]] && out="$a"; prev="$a"; done
    if [[ -n "$out" ]]; then
      mkdir -p "$(dirname "$out")"
      printf '{"address":"apr1test00000000000000000000000000000000000","version":1}\n' > "$out"
    fi
    ;;
esac
exit 0
STUB

chmod +x "$CTX/stubs-extra/"*

# ── Test harness (runs inside the container) ──────────────────────────────────
cat > "$CTX/test-harness.sh" << 'HARNESS'
#!/usr/bin/env bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail_assert() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }

export PATH="/stubs:$PATH"

# ── Install the node-config.sh spy ───────────────────────────────────────────
# install-node.sh calls: bash "${SCRIPT_DIR}/node-config.sh" add-bootnode <addr>
# where SCRIPT_DIR resolves to /deploy.  A spy at /deploy/node-config.sh
# intercepts this call, records it, then delegates to the real script.
mv /deploy/node-config.sh /deploy/node-config.sh.real
cat > /deploy/node-config.sh << 'SPY'
#!/usr/bin/env bash
# Spy: record subcommand + args, then delegate to the real script.
echo "$@" >> /tmp/node-config-spy.log
exec bash /deploy/node-config.sh.real "$@"
SPY
chmod +x /deploy/node-config.sh

# ─────────────────────────────────────────────────────────────────────────────
# Run 1: --primary-ip 10.0.0.2
# Expected: node-config.sh add-bootnode called with /ip4/10.0.0.2/tcp/30303;
#           node.yaml contains that bootnode; service started.
# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════"
echo "  Run 1 — --primary-ip 10.0.0.2"
echo "════════════════════════════════════════════════"

# Reset spy log for this run
rm -f /tmp/node-config-spy.log /tmp/systemctl-calls.log

PRIMARY_IP="10.0.0.2"
INSTALL_OUT=$(printf '1\n' | bash /deploy/install-node.sh --primary-ip "$PRIMARY_IP" 2>&1) || true
INSTALL_EXIT=$?

echo "$INSTALL_OUT"

echo ""
echo "  Assertions (Run 1):"

# A1: node-config.sh add-bootnode was called at least once
if grep -q "^add-bootnode " /tmp/node-config-spy.log 2>/dev/null; then
  pass_assert "A1: node-config.sh add-bootnode was called"
else
  fail_assert "A1: node-config.sh add-bootnode was NOT called (spy log empty or missing)"
  echo "     spy log:" >&2
  cat /tmp/node-config-spy.log >&2 2>/dev/null || echo "     (no spy log found)" >&2
fi

# A2: the multiaddr passed matches /ip4/<IP>/tcp/30303
EXPECTED_MULTIADDR="/ip4/${PRIMARY_IP}/tcp/30303"
if grep -qF "add-bootnode ${EXPECTED_MULTIADDR}" /tmp/node-config-spy.log 2>/dev/null; then
  pass_assert "A2: node-config.sh received correct multiaddr: ${EXPECTED_MULTIADDR}"
else
  fail_assert "A2: correct multiaddr not found in spy log (expected: add-bootnode ${EXPECTED_MULTIADDR})"
  echo "     spy log contents:" >&2
  cat /tmp/node-config-spy.log >&2 2>/dev/null || echo "     (no spy log found)" >&2
fi

# A3: node.yaml contains the bootnode entry
if grep -q "/ip4/${PRIMARY_IP}/tcp/" /etc/aperod/node.yaml 2>/dev/null; then
  pass_assert "A3: node.yaml contains bootnode entry for ${PRIMARY_IP}"
else
  fail_assert "A3: node.yaml missing bootnode /ip4/${PRIMARY_IP}/tcp/"
  echo "     p2p section of node.yaml:" >&2
  grep -A10 "^p2p:" /etc/aperod/node.yaml >&2 2>/dev/null || cat /etc/aperod/node.yaml >&2 2>/dev/null
fi

# A4: systemctl enable --now (or start) was called for aperod-node
if grep -qE "^(enable --now|start) aperod-node$" /tmp/systemctl-calls.log 2>/dev/null; then
  pass_assert "A4: systemctl enable --now aperod-node was called (service started)"
else
  fail_assert "A4: systemctl enable/start aperod-node was NOT called"
  echo "     systemctl calls log:" >&2
  cat /tmp/systemctl-calls.log >&2 2>/dev/null || echo "     (no systemctl log found)" >&2
fi

# ─────────────────────────────────────────────────────────────────────────────
# Run 2: no --primary-ip
# Expected: no node-config.sh call; warning block in stdout; empty bootnodes;
#           service NOT started.
# ─────────────────────────────────────────────────────────────────────────────
echo ""
echo "════════════════════════════════════════════════"
echo "  Run 2 — no --primary-ip"
echo "════════════════════════════════════════════════"

# Reset everything for a clean run
rm -f /tmp/node-config-spy.log /tmp/systemctl-calls.log
rm -f /etc/aperod/node.yaml
rm -rf /run/fake-systemd

INSTALL_OUT2=$(printf '1\n' | bash /deploy/install-node.sh 2>&1) || true

echo "$INSTALL_OUT2"

echo ""
echo "  Assertions (Run 2):"

# A5: node-config.sh add-bootnode must NOT have been called
if ! grep -q "^add-bootnode " /tmp/node-config-spy.log 2>/dev/null; then
  pass_assert "A5: node-config.sh add-bootnode was NOT called (correct: no --primary-ip)"
else
  fail_assert "A5: node-config.sh add-bootnode was called without --primary-ip — unexpected"
  echo "     spy log:" >&2
  cat /tmp/node-config-spy.log >&2
fi

# A6: the warning block header must appear in the installer output
# The header line is locale-agnostic enough that we check the key phrase.
if echo "$INSTALL_OUT2" | grep -q "ВНИМАНИЕ"; then
  pass_assert "A6: warning block 'ВНИМАНИЕ' appeared in installer output"
elif echo "$INSTALL_OUT2" | grep -q "bootnode не настроен"; then
  pass_assert "A6: warning block 'bootnode не настроен' appeared in installer output"
else
  fail_assert "A6: warning block NOT found in installer output — users will not be warned about missing bootnode"
  echo "     (last 20 lines of installer output):" >&2
  echo "$INSTALL_OUT2" | tail -20 >&2
fi

# A7: node.yaml p2p.bootnodes must be empty
YAML_BOOTNODE_COUNT=$(python3 -c "
import sys, yaml
try:
    with open('/etc/aperod/node.yaml') as f:
        cfg = yaml.safe_load(f)
    p2p = cfg.get('p2p', {}) or {}
    entries = p2p.get('bootnodes', []) or []
    print(len(entries))
except Exception as e:
    print('error', file=sys.stderr)
    sys.exit(1)
" 2>/dev/null || echo "error")

if [[ "$YAML_BOOTNODE_COUNT" == "0" ]]; then
  pass_assert "A7: p2p.bootnodes is empty in node.yaml (correct for no --primary-ip)"
elif [[ "$YAML_BOOTNODE_COUNT" == "error" ]]; then
  fail_assert "A7: could not parse node.yaml — file missing or invalid YAML"
  cat /etc/aperod/node.yaml >&2 2>/dev/null || echo "  (file not found)" >&2
else
  fail_assert "A7: p2p.bootnodes has $YAML_BOOTNODE_COUNT entries (expected 0 for no --primary-ip)"
  grep -A10 "bootnodes" /etc/aperod/node.yaml >&2 || true
fi

# A8: systemctl enable --now must NOT have been called (safety hold)
if ! grep -qE "^(enable --now|start) aperod-node$" /tmp/systemctl-calls.log 2>/dev/null; then
  pass_assert "A8: service NOT started on no-primary-ip install (safety hold correct)"
else
  fail_assert "A8: service was started without --primary-ip — node may form an isolated chain"
  echo "     systemctl calls log:" >&2
  cat /tmp/systemctl-calls.log >&2 2>/dev/null || echo "     (no log found)" >&2
fi

# ─────────────────────────────────────────────────────────────────────────────
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

# Packages needed by the installer and test harness (same as e2e test):
#   jq           — wallet address extraction in install-node.sh
#   python3      — aperod-node stub + YAML parsing in assertions
#   python3-yaml — PyYAML required by node-config.sh
#   curl         — health checks + installer IP lookup (smart stub)
#   make         — delegates to injected Makefile
#   bash         — installer is a bash script
#   passwd       — provides useradd; installer creates aperod account
#   util-linux   — provides runuser; fake systemctl starts service as User=aperod
DOCKER

cat >> "$CTX/Dockerfile" << 'DOCKER'
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      jq python3 python3-yaml curl make bash ca-certificates passwd util-linux \
 && rm -rf /var/lib/apt/lists/*

COPY stubs/       /stubs/
COPY stubs-extra/ /stubs-extra/
COPY deploy/      /deploy/
COPY test-harness.sh /test-harness.sh

RUN grep -q Ubuntu /etc/os-release
RUN mkdir -p /etc/systemd/system /run/fake-systemd /etc/aperod

CMD ["bash", "/test-harness.sh"]
DOCKER

# ── Build the image ───────────────────────────────────────────────────────────
echo -e "\n${BOLD}Building Docker test image…${NC}"
if ! docker build --quiet -t "$IMAGE_TAG" "$CTX" 2>&1; then
  docker build -t "$IMAGE_TAG" "$CTX"
  echo -e "${RED}[ERR]${NC}  Docker build failed" >&2
  exit 1
fi
echo -e "${GREEN}[OK]${NC}   Image built: $IMAGE_TAG"

# ── Run the test container ────────────────────────────────────────────────────
echo -e "\n${BOLD}Running install-node.sh --primary-ip tests…${NC}"
echo "────────────────────────────────────────────────────"

if docker run --rm "$IMAGE_TAG" bash /test-harness.sh; then
  echo ""
  echo -e "${GREEN}${BOLD}install-node.sh --primary-ip test PASSED.${NC}"
  exit 0
else
  echo ""
  echo -e "${RED}${BOLD}install-node.sh --primary-ip test FAILED.${NC}"
  exit 1
fi
