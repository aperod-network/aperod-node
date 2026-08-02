#!/usr/bin/env bash
# =============================================================================
#  test-watchdog.sh — Integration tests for aperod-node-watchdog.sh
#
#  Scenarios:
#    1. Mock API returns non-200 → watchdog calls `systemctl restart aperod-node`
#    2. Mock API returns 200     → watchdog does NOT call `systemctl restart`
#    3. Mock API times out (no listener) → watchdog calls `systemctl restart`
#    4. Static analysis — NODE_API_URL is env-driven, not hardcoded
#    5. HTTP 404 (another non-200 code) → restart triggered
#
#  Run from anywhere:
#    bash blockchain/deploy/test-watchdog.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WATCHDOG_SH="$SCRIPT_DIR/aperod-node-watchdog.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Require python3 ────────────────────────────────────────────────────────────
if ! command -v python3 >/dev/null 2>&1; then
  echo -e "${RED}[ERR]${NC}  python3 required to run the mock HTTP server." >&2
  exit 1
fi

# ── Ensure the script under test exists ───────────────────────────────────────
if [[ ! -f "$WATCHDOG_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  aperod-node-watchdog.sh not found at: $WATCHDOG_SH" >&2
  exit 1
fi

# ── Shared temp directory (cleaned on exit) ───────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Print a free TCP port on 127.0.0.1.
find_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(('127.0.0.1', 0))
print(s.getsockname()[1])
s.close()
PY
}

# Start a persistent HTTP server that always returns $2 for every GET request
# on port $1.  The server runs until explicitly killed.  Prints the server PID.
start_mock_server() {
  local port="$1"
  local status_code="$2"
  python3 -u - "$port" "$status_code" <<'PY' >/dev/null 2>&1 &
import http.server, sys, signal

port        = int(sys.argv[1])
status_code = int(sys.argv[2])

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(status_code)
        self.end_headers()
    def log_message(self, *args):
        pass  # suppress access log noise

signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
server = http.server.HTTPServer(('127.0.0.1', port), Handler)
server.serve_forever()
PY
  echo $!
}

# Wait until the mock server is reachable via HTTP (up to $2 seconds).
# We fire a real HTTP GET and check for any response; this is safe against
# consuming single-use slots because serve_forever handles all requests.
wait_for_server() {
  local port="$1"
  local timeout="${2:-5}"
  local deadline=$(( $(date +%s) + timeout ))
  while (( $(date +%s) < deadline )); do
    if python3 -c "
import urllib.request, sys
try:
    urllib.request.urlopen('http://127.0.0.1:$port/', timeout=0.5)
except Exception:
    pass  # non-200 raises but the server IS up
sys.exit(0)
" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  return 1
}

# Build a fake-bin directory containing a stub for $1 that appends its
# arguments to $2 and exits 0.  Prints the directory path.
make_fake_bin() {
  local cmd="$1"
  local log_file="$2"
  local fake_dir
  fake_dir=$(mktemp -d "$TMPDIR_TEST/fake-bin-XXXXXXXX")
  # Use single-quoted heredoc body so the file content is literal bash;
  # we substitute $log_file via sed after writing.
  cat >"$fake_dir/$cmd" <<STUB
#!/usr/bin/env bash
echo "$cmd \$*" >> "$log_file"
exit 0
STUB
  chmod +x "$fake_dir/$cmd"
  echo "$fake_dir"
}

# =============================================================================
# Test 1: non-200 response → restart triggered
# =============================================================================
section "Test 1: non-200 HTTP response triggers systemctl restart aperod-node"

PORT1=$(find_free_port)
SRV1_PID=$(start_mock_server "$PORT1" 503)

if wait_for_server "$PORT1" 5; then
  pass "mock HTTP server (503) is listening on port $PORT1"
else
  fail "mock HTTP server did not start on port $PORT1 within 5 s"
fi

LOG1="$TMPDIR_TEST/systemctl-t1.log"
FAKE1=$(make_fake_bin "systemctl" "$LOG1")

NODE_API_URL="http://127.0.0.1:${PORT1}" \
  TIMEOUT_SECS="3" \
  PATH="$FAKE1:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT1=$?

kill "$SRV1_PID" 2>/dev/null || true
wait "$SRV1_PID" 2>/dev/null || true

if [[ $WDEXIT1 -eq 0 ]]; then
  pass "watchdog exited 0 after detecting non-200 response"
else
  fail "watchdog exited $WDEXIT1 (expected 0)"
fi

if [[ -f "$LOG1" ]] && grep -q "systemctl restart aperod-node" "$LOG1"; then
  pass "systemctl was called with 'restart aperod-node'"
else
  fail "systemctl was NOT called with 'restart aperod-node' (log: $(cat "$LOG1" 2>/dev/null || echo '<empty>'))"
fi

RESTART_COUNT1=$(grep -c "restart aperod-node" "$LOG1" 2>/dev/null || echo "0")
if [[ "$RESTART_COUNT1" -eq 1 ]]; then
  pass "systemctl restart called exactly once"
else
  fail "expected 1 restart call, got $RESTART_COUNT1"
fi

# =============================================================================
# Test 2: 200 OK response → no restart triggered
# =============================================================================
section "Test 2: HTTP 200 response — watchdog exits without calling systemctl restart"

PORT2=$(find_free_port)
SRV2_PID=$(start_mock_server "$PORT2" 200)

if wait_for_server "$PORT2" 5; then
  pass "mock HTTP server (200) is listening on port $PORT2"
else
  fail "mock HTTP server did not start on port $PORT2 within 5 s"
fi

LOG2="$TMPDIR_TEST/systemctl-t2.log"
FAKE2=$(make_fake_bin "systemctl" "$LOG2")

NODE_API_URL="http://127.0.0.1:${PORT2}" \
  TIMEOUT_SECS="3" \
  PATH="$FAKE2:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT2=$?

kill "$SRV2_PID" 2>/dev/null || true
wait "$SRV2_PID" 2>/dev/null || true

if [[ $WDEXIT2 -eq 0 ]]; then
  pass "watchdog exited 0 on healthy node"
else
  fail "watchdog exited $WDEXIT2 (expected 0)"
fi

if [[ ! -f "$LOG2" ]] || ! grep -q "restart aperod-node" "$LOG2" 2>/dev/null; then
  pass "systemctl restart was NOT called for a healthy 200 response"
else
  fail "systemctl restart was unexpectedly called for a 200 response"
fi

# =============================================================================
# Test 3: connection refused (no listener) → curl returns 000 → restart
# =============================================================================
section "Test 3: connection refused (no listener) → watchdog calls systemctl restart"

# Use a port with no listener
PORT3=$(find_free_port)

LOG3="$TMPDIR_TEST/systemctl-t3.log"
FAKE3=$(make_fake_bin "systemctl" "$LOG3")

NODE_API_URL="http://127.0.0.1:${PORT3}" \
  TIMEOUT_SECS="2" \
  PATH="$FAKE3:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT3=$?

if [[ $WDEXIT3 -eq 0 ]]; then
  pass "watchdog exited 0 after connection refused"
else
  fail "watchdog exited $WDEXIT3 (expected 0)"
fi

if [[ -f "$LOG3" ]] && grep -q "systemctl restart aperod-node" "$LOG3"; then
  pass "systemctl was called with 'restart aperod-node' on connection refused"
else
  fail "systemctl was NOT called on connection refused (log: $(cat "$LOG3" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 4: static analysis — NODE_API_URL is env-driven, not hardcoded
# =============================================================================
section "Test 4: static analysis — NODE_API_URL is env-driven, not hardcoded"

HARDCODED=$(grep -E '(curl|wget).*127\.0\.0\.1:[0-9]+' "$WATCHDOG_SH" || true)
if [[ -z "$HARDCODED" ]]; then
  pass "no hardcoded 127.0.0.1:PORT in curl call (uses \$NODE_API_URL variable)"
else
  fail "hardcoded host:port found in curl call — should use \$NODE_API_URL:\n$HARDCODED"
fi

RESTART_CMD=$(grep "systemctl restart aperod-node" "$WATCHDOG_SH" || true)
if [[ -n "$RESTART_CMD" ]]; then
  pass "restart command is 'systemctl restart aperod-node'"
else
  fail "could not find 'systemctl restart aperod-node' in watchdog script"
fi

# =============================================================================
# Test 5: HTTP 404 response also triggers restart
# =============================================================================
section "Test 5: HTTP 404 response also triggers restart"

PORT5=$(find_free_port)
SRV5_PID=$(start_mock_server "$PORT5" 404)

if wait_for_server "$PORT5" 5; then
  pass "mock HTTP server (404) is listening on port $PORT5"
else
  fail "mock HTTP server did not start on port $PORT5 within 5 s"
fi

LOG5="$TMPDIR_TEST/systemctl-t5.log"
FAKE5=$(make_fake_bin "systemctl" "$LOG5")

NODE_API_URL="http://127.0.0.1:${PORT5}" \
  TIMEOUT_SECS="3" \
  PATH="$FAKE5:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT5=$?

kill "$SRV5_PID" 2>/dev/null || true
wait "$SRV5_PID" 2>/dev/null || true

if [[ $WDEXIT5 -eq 0 ]]; then
  pass "watchdog exited 0 after 404 response"
else
  fail "watchdog exited $WDEXIT5 (expected 0)"
fi

if [[ -f "$LOG5" ]] && grep -q "systemctl restart aperod-node" "$LOG5"; then
  pass "systemctl restart called for 404 response"
else
  fail "systemctl restart was NOT called for 404 response"
fi

# ---------------------------------------------------------------------------
# make_fake_curl  — builds a stub `curl` that:
#   * logs every invocation to $2
#   * if the URL contains "api.telegram.org" → just exits 0 (Telegram path)
#   * otherwise → prints $3 to stdout (health-probe path needs HTTP code)
# ---------------------------------------------------------------------------
make_fake_curl() {
  local log_file="$1"
  local health_code="$2"   # e.g. "503", "200"
  local fake_dir
  fake_dir=$(mktemp -d "$TMPDIR_TEST/fake-curl-XXXXXXXX")
  cat >"$fake_dir/curl" <<STUB
#!/usr/bin/env bash
# Log all arguments so tests can grep them
echo "curl \$*" >> "$log_file"
# Telegram calls: output suppressed by watchdog; just exit 0
if echo "\$*" | grep -q "api.telegram.org"; then
  exit 0
fi
# Health-probe call: watchdog captures stdout as the HTTP code
echo "$health_code"
exit 0
STUB
  chmod +x "$fake_dir/curl"
  echo "$fake_dir"
}

# =============================================================================
# Test 6: Telegram sendMessage IS called when probe fails and tokens are set
# =============================================================================
section "Test 6: Telegram sendMessage is called on probe failure (tokens set)"

LOG6_CURL="$TMPDIR_TEST/curl-t6.log"
LOG6_SC="$TMPDIR_TEST/systemctl-t6.log"
FAKE6_CURL=$(make_fake_curl "$LOG6_CURL" "503")
FAKE6_SC=$(make_fake_bin "systemctl" "$LOG6_SC")

NODE_API_URL="http://127.0.0.1:19999" \
  TIMEOUT_SECS="3" \
  SUPPORT_BOT_TOKEN="test-bot-token" \
  SUPPORT_ADMIN_CHAT_ID="123456789" \
  PATH="$FAKE6_CURL:$FAKE6_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT6=$?

if [[ $WDEXIT6 -eq 0 ]]; then
  pass "watchdog exited 0 when probe failed (tokens set)"
else
  fail "watchdog exited $WDEXIT6 (expected 0)"
fi

if [[ -f "$LOG6_CURL" ]] && grep -q "api.telegram.org" "$LOG6_CURL"; then
  pass "curl was called with api.telegram.org (Telegram alert sent)"
else
  fail "curl was NOT called with api.telegram.org (log: $(cat "$LOG6_CURL" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$LOG6_CURL" ]] && grep -q "sendMessage" "$LOG6_CURL"; then
  pass "Telegram sendMessage endpoint was called"
else
  fail "Telegram sendMessage endpoint was NOT called (log: $(cat "$LOG6_CURL" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$LOG6_SC" ]] && grep -q "systemctl restart aperod-node" "$LOG6_SC"; then
  pass "systemctl restart also called alongside Telegram alert"
else
  fail "systemctl restart was NOT called (log: $(cat "$LOG6_SC" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 7: Telegram NOT called when probe returns 200
# =============================================================================
section "Test 7: Telegram is NOT called when probe returns 200"

LOG7_CURL="$TMPDIR_TEST/curl-t7.log"
LOG7_SC="$TMPDIR_TEST/systemctl-t7.log"
FAKE7_CURL=$(make_fake_curl "$LOG7_CURL" "200")
FAKE7_SC=$(make_fake_bin "systemctl" "$LOG7_SC")

NODE_API_URL="http://127.0.0.1:19999" \
  TIMEOUT_SECS="3" \
  SUPPORT_BOT_TOKEN="test-bot-token" \
  SUPPORT_ADMIN_CHAT_ID="123456789" \
  PATH="$FAKE7_CURL:$FAKE7_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT7=$?

if [[ $WDEXIT7 -eq 0 ]]; then
  pass "watchdog exited 0 on healthy node (200)"
else
  fail "watchdog exited $WDEXIT7 (expected 0)"
fi

if [[ ! -f "$LOG7_CURL" ]] || ! grep -q "api.telegram.org" "$LOG7_CURL" 2>/dev/null; then
  pass "Telegram was NOT called for a healthy 200 response"
else
  fail "Telegram was unexpectedly called for a 200 response"
fi

if [[ ! -f "$LOG7_SC" ]] || ! grep -q "restart aperod-node" "$LOG7_SC" 2>/dev/null; then
  pass "systemctl restart was NOT called for a healthy 200 response"
else
  fail "systemctl restart was unexpectedly called for a 200 response"
fi

# =============================================================================
# Test 8: Telegram silently skipped when tokens are unset
# =============================================================================
section "Test 8: Telegram silently skipped when SUPPORT_BOT_TOKEN / SUPPORT_ADMIN_CHAT_ID are unset"

LOG8_CURL="$TMPDIR_TEST/curl-t8.log"
LOG8_SC="$TMPDIR_TEST/systemctl-t8.log"
FAKE8_CURL=$(make_fake_curl "$LOG8_CURL" "503")
FAKE8_SC=$(make_fake_bin "systemctl" "$LOG8_SC")

# Explicitly unset the Telegram env vars
NODE_API_URL="http://127.0.0.1:19999" \
  TIMEOUT_SECS="3" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$FAKE8_CURL:$FAKE8_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
WDEXIT8=$?

if [[ $WDEXIT8 -eq 0 ]]; then
  pass "watchdog exited 0 even without Telegram tokens (probe failed)"
else
  fail "watchdog exited $WDEXIT8 (expected 0)"
fi

if [[ ! -f "$LOG8_CURL" ]] || ! grep -q "api.telegram.org" "$LOG8_CURL" 2>/dev/null; then
  pass "Telegram was NOT called when tokens are unset (silent skip)"
else
  fail "Telegram was unexpectedly called when tokens are unset"
fi

if [[ -f "$LOG8_SC" ]] && grep -q "systemctl restart aperod-node" "$LOG8_SC"; then
  pass "systemctl restart still called even without Telegram tokens"
else
  fail "systemctl restart was NOT called when tokens are unset (log: $(cat "$LOG8_SC" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Summary
# =============================================================================
echo ""
echo "─────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL tests passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL tests FAILED.${NC}"
  exit 1
fi
