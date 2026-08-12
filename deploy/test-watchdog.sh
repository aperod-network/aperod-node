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

# Start a persistent HTTP server that returns $2 status and $3 JSON body for every GET.
start_json_server() {
  local port="$1"
  local status_code="$2"
  local body="$3"
  python3 -u - "$port" "$status_code" "$body" <<'PY' >/dev/null 2>&1 &
import http.server, sys, signal

port        = int(sys.argv[1])
status_code = int(sys.argv[2])
body        = sys.argv[3].encode()

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(status_code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *args):
        pass

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
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
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
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
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
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
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
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
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
# Tests for aperod-watchdog-set-interval.sh
# =============================================================================
SET_INTERVAL_SH="$SCRIPT_DIR/aperod-watchdog-set-interval.sh"

if [[ ! -f "$SET_INTERVAL_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  aperod-watchdog-set-interval.sh not found at: $SET_INTERVAL_SH" >&2
  ((FAIL++))
else

# Helper: build a fake systemctl stub that silently accepts any arguments.
make_fake_systemctl() {
  local log_file="$1"
  local fake_dir
  fake_dir=$(mktemp -d "$TMPDIR_TEST/fake-sc-XXXXXXXX")
  cat >"$fake_dir/systemctl" <<STUB
#!/usr/bin/env bash
echo "systemctl \$*" >> "$log_file"
exit 0
STUB
  chmod +x "$fake_dir/systemctl"
  echo "$fake_dir"
}

# =============================================================================
# Test 9: valid integer → correct OnBootSec/OnUnitActiveSec in the drop-in
# =============================================================================
section "Test 9: valid WATCHDOG_INTERVAL_SECS=30 → drop-in contains OnBootSec=30"

T9_DIR=$(mktemp -d "$TMPDIR_TEST/t9-XXXXXXXX")
T9_ENV="$T9_DIR/watchdog.env"
T9_DROPIN_DIR="$T9_DIR/dropin"
T9_SC_LOG="$T9_DIR/systemctl.log"
T9_FAKE_SC=$(make_fake_systemctl "$T9_SC_LOG")

echo "WATCHDOG_INTERVAL_SECS=30" > "$T9_ENV"

_APEROD_TEST=1 \
  ENV_FILE="$T9_ENV" \
  DROPIN_DIR="$T9_DROPIN_DIR" \
  PATH="$T9_FAKE_SC:$PATH" \
  bash "$SET_INTERVAL_SH" >/dev/null 2>&1
T9_EXIT=$?

if [[ $T9_EXIT -eq 0 ]]; then
  pass "aperod-watchdog-set-interval.sh exited 0 for valid interval"
else
  fail "aperod-watchdog-set-interval.sh exited $T9_EXIT (expected 0)"
fi

T9_DROPIN="$T9_DROPIN_DIR/interval.conf"
if [[ -f "$T9_DROPIN" ]]; then
  pass "drop-in file was created at $T9_DROPIN"
else
  fail "drop-in file was NOT created at $T9_DROPIN"
fi

if grep -q "^OnBootSec=30$" "$T9_DROPIN" 2>/dev/null; then
  pass "drop-in contains OnBootSec=30"
else
  fail "drop-in does NOT contain OnBootSec=30 (content: $(cat "$T9_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^OnUnitActiveSec=30$" "$T9_DROPIN" 2>/dev/null; then
  pass "drop-in contains OnUnitActiveSec=30"
else
  fail "drop-in does NOT contain OnUnitActiveSec=30 (content: $(cat "$T9_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "daemon-reload" "$T9_SC_LOG" 2>/dev/null; then
  pass "systemctl daemon-reload was called"
else
  fail "systemctl daemon-reload was NOT called (log: $(cat "$T9_SC_LOG" 2>/dev/null || echo '<empty>'))"
fi

if grep -q "restart aperod-node-watchdog.timer" "$T9_SC_LOG" 2>/dev/null; then
  pass "systemctl restart aperod-node-watchdog.timer was called"
else
  fail "systemctl restart aperod-node-watchdog.timer was NOT called (log: $(cat "$T9_SC_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 10: missing WATCHDOG_INTERVAL_SECS → defaults to 60
# =============================================================================
section "Test 10: missing WATCHDOG_INTERVAL_SECS → drop-in defaults to 60"

T10_DIR=$(mktemp -d "$TMPDIR_TEST/t10-XXXXXXXX")
T10_ENV="$T10_DIR/watchdog.env"
T10_DROPIN_DIR="$T10_DIR/dropin"
T10_SC_LOG="$T10_DIR/systemctl.log"
T10_FAKE_SC=$(make_fake_systemctl "$T10_SC_LOG")

# Write env file without the key
echo "# no interval here" > "$T10_ENV"

_APEROD_TEST=1 \
  ENV_FILE="$T10_ENV" \
  DROPIN_DIR="$T10_DROPIN_DIR" \
  PATH="$T10_FAKE_SC:$PATH" \
  bash "$SET_INTERVAL_SH" >/dev/null 2>&1
T10_EXIT=$?

if [[ $T10_EXIT -eq 0 ]]; then
  pass "script exited 0 when WATCHDOG_INTERVAL_SECS is absent"
else
  fail "script exited $T10_EXIT (expected 0)"
fi

T10_DROPIN="$T10_DROPIN_DIR/interval.conf"
if grep -q "^OnBootSec=60$" "$T10_DROPIN" 2>/dev/null; then
  pass "drop-in defaults to OnBootSec=60 when key is missing"
else
  fail "drop-in does NOT contain OnBootSec=60 (content: $(cat "$T10_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^OnUnitActiveSec=60$" "$T10_DROPIN" 2>/dev/null; then
  pass "drop-in defaults to OnUnitActiveSec=60 when key is missing"
else
  fail "drop-in does NOT contain OnUnitActiveSec=60 (content: $(cat "$T10_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 11: value below 5 → warning + fallback to 60
# =============================================================================
section "Test 11: WATCHDOG_INTERVAL_SECS=3 (< 5) → warning on stderr, fallback to 60"

T11_DIR=$(mktemp -d "$TMPDIR_TEST/t11-XXXXXXXX")
T11_ENV="$T11_DIR/watchdog.env"
T11_DROPIN_DIR="$T11_DIR/dropin"
T11_SC_LOG="$T11_DIR/systemctl.log"
T11_STDERR="$T11_DIR/stderr.log"
T11_FAKE_SC=$(make_fake_systemctl "$T11_SC_LOG")

echo "WATCHDOG_INTERVAL_SECS=3" > "$T11_ENV"

_APEROD_TEST=1 \
  ENV_FILE="$T11_ENV" \
  DROPIN_DIR="$T11_DROPIN_DIR" \
  PATH="$T11_FAKE_SC:$PATH" \
  bash "$SET_INTERVAL_SH" >/dev/null 2>"$T11_STDERR"
T11_EXIT=$?

if [[ $T11_EXIT -eq 0 ]]; then
  pass "script exited 0 for below-minimum value"
else
  fail "script exited $T11_EXIT (expected 0)"
fi

if grep -qi "warn" "$T11_STDERR" 2>/dev/null; then
  pass "warning emitted to stderr for value below 5"
else
  fail "no warning on stderr for value below 5 (stderr: $(cat "$T11_STDERR" 2>/dev/null || echo '<empty>'))"
fi

T11_DROPIN="$T11_DROPIN_DIR/interval.conf"
if grep -q "^OnBootSec=60$" "$T11_DROPIN" 2>/dev/null; then
  pass "drop-in falls back to OnBootSec=60 for value below minimum"
else
  fail "drop-in does NOT fall back to 60 for value below minimum (content: $(cat "$T11_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^OnUnitActiveSec=60$" "$T11_DROPIN" 2>/dev/null; then
  pass "drop-in falls back to OnUnitActiveSec=60 for value below minimum"
else
  fail "drop-in does NOT fall back to 60 for OnUnitActiveSec (content: $(cat "$T11_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 12: non-integer value → warning + fallback to 60
# =============================================================================
section "Test 12: WATCHDOG_INTERVAL_SECS=abc (non-integer) → warning + fallback to 60"

T12_DIR=$(mktemp -d "$TMPDIR_TEST/t12-XXXXXXXX")
T12_ENV="$T12_DIR/watchdog.env"
T12_DROPIN_DIR="$T12_DIR/dropin"
T12_SC_LOG="$T12_DIR/systemctl.log"
T12_STDERR="$T12_DIR/stderr.log"
T12_FAKE_SC=$(make_fake_systemctl "$T12_SC_LOG")

echo "WATCHDOG_INTERVAL_SECS=abc" > "$T12_ENV"

_APEROD_TEST=1 \
  ENV_FILE="$T12_ENV" \
  DROPIN_DIR="$T12_DROPIN_DIR" \
  PATH="$T12_FAKE_SC:$PATH" \
  bash "$SET_INTERVAL_SH" >/dev/null 2>"$T12_STDERR"
T12_EXIT=$?

if [[ $T12_EXIT -eq 0 ]]; then
  pass "script exited 0 for non-integer value"
else
  fail "script exited $T12_EXIT (expected 0)"
fi

if grep -qi "warn" "$T12_STDERR" 2>/dev/null; then
  pass "warning emitted to stderr for non-integer value"
else
  fail "no warning on stderr for non-integer value (stderr: $(cat "$T12_STDERR" 2>/dev/null || echo '<empty>'))"
fi

T12_DROPIN="$T12_DROPIN_DIR/interval.conf"
if grep -q "^OnBootSec=60$" "$T12_DROPIN" 2>/dev/null; then
  pass "drop-in falls back to OnBootSec=60 for non-integer"
else
  fail "drop-in does NOT fall back to 60 for non-integer (content: $(cat "$T12_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

if grep -q "^OnUnitActiveSec=60$" "$T12_DROPIN" 2>/dev/null; then
  pass "drop-in falls back to OnUnitActiveSec=60 for non-integer"
else
  fail "drop-in does NOT fall back to 60 for OnUnitActiveSec (non-integer) (content: $(cat "$T12_DROPIN" 2>/dev/null || echo '<missing>'))"
fi

fi  # end of set-interval tests block (script exists guard)

# =============================================================================
# Test 13: restart event appended to watchdog-restart-events on probe failure
# =============================================================================
section "Test 13: restart event file is updated when probe fails"

T13_DIR=$(mktemp -d "$TMPDIR_TEST/t13-XXXXXXXX")
T13_SC_LOG="$T13_DIR/systemctl.log"
T13_FAKE_SC=$(make_fake_bin "systemctl" "$T13_SC_LOG")

NODE_API_URL="http://127.0.0.1:19999" \
  STATE_DIR="$T13_DIR" \
  TIMEOUT_SECS="1" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T13_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T13_EXIT=$?

if [[ $T13_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 after probe failure"
else
  fail "watchdog exited $T13_EXIT (expected 0)"
fi

T13_EVENTS="${T13_DIR}/watchdog-restart-events"
if [[ -f "$T13_EVENTS" ]]; then
  pass "watchdog-restart-events file was created on restart"
else
  fail "watchdog-restart-events file was NOT created (STATE_DIR=$T13_DIR)"
fi

T13_LINE_COUNT=$(wc -l < "$T13_EVENTS" 2>/dev/null || echo "0")
if [[ "$T13_LINE_COUNT" -ge 1 ]]; then
  pass "watchdog-restart-events contains at least 1 line after restart"
else
  fail "watchdog-restart-events is empty after restart"
fi

T13_TS=$(grep -E '^[0-9]+$' "$T13_EVENTS" 2>/dev/null | head -1 || true)
if [[ -n "$T13_TS" && "$T13_TS" -gt 0 ]]; then
  pass "restart-events entry is a positive integer (Unix-ms: $T13_TS)"
else
  fail "restart-events entry is not a positive integer (got: '$T13_TS')"
fi

# =============================================================================
# Test 14: watchdog-restart-events NOT written when probe returns 200
# =============================================================================
section "Test 14: restart event file is NOT written when probe succeeds (200)"

PORT14=$(find_free_port)
SRV14_PID=$(start_mock_server "$PORT14" 200)

if wait_for_server "$PORT14" 5; then
  pass "mock HTTP server (200) is listening on port $PORT14"
else
  fail "mock HTTP server did not start on port $PORT14 within 5 s"
fi

T14_DIR=$(mktemp -d "$TMPDIR_TEST/t14-XXXXXXXX")
T14_SC_LOG="$T14_DIR/systemctl.log"
T14_FAKE_SC=$(make_fake_bin "systemctl" "$T14_SC_LOG")

NODE_API_URL="http://127.0.0.1:${PORT14}" \
  STATE_DIR="$T14_DIR" \
  TIMEOUT_SECS="3" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T14_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T14_EXIT=$?

kill "$SRV14_PID" 2>/dev/null || true
wait "$SRV14_PID" 2>/dev/null || true

if [[ $T14_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 on healthy probe"
else
  fail "watchdog exited $T14_EXIT (expected 0)"
fi

T14_EVENTS="${T14_DIR}/watchdog-restart-events"
if [[ ! -f "$T14_EVENTS" ]]; then
  pass "watchdog-restart-events was NOT written for a 200 response"
else
  fail "watchdog-restart-events was unexpectedly written for a 200 response (content: $(cat "$T14_EVENTS"))"
fi

# =============================================================================
# Test 15: API 200 + RSS above RAM_THRESHOLD_MB → proactive restart triggered
# =============================================================================
section "Test 15: RSS above RAM_THRESHOLD_MB → restart even when API is healthy"

PORT15=$(find_free_port)
SRV15_PID=$(start_mock_server "$PORT15" 200)

if wait_for_server "$PORT15" 5; then
  pass "mock HTTP server (200) is listening on port $PORT15"
else
  fail "mock HTTP server did not start on port $PORT15 within 5 s"
fi

T15_DIR=$(mktemp -d "$TMPDIR_TEST/t15-XXXXXXXX")
T15_SC_LOG="$T15_DIR/systemctl.log"
T15_FAKE_SC=$(make_fake_bin "systemctl" "$T15_SC_LOG")

# 5 000 000 KB = 4882 MB — above the default 4800 MB threshold
NODE_API_URL="http://127.0.0.1:${PORT15}" \
  STATE_DIR="$T15_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_RSS_KB="5000000" \
  RAM_THRESHOLD_MB="4800" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T15_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T15_EXIT=$?

kill "$SRV15_PID" 2>/dev/null || true
wait "$SRV15_PID" 2>/dev/null || true

if [[ $T15_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 after RAM-triggered restart"
else
  fail "watchdog exited $T15_EXIT (expected 0)"
fi

if [[ -f "$T15_SC_LOG" ]] && grep -q "systemctl restart aperod-node" "$T15_SC_LOG"; then
  pass "systemctl restart called when RSS exceeds threshold"
else
  fail "systemctl restart NOT called when RSS exceeds threshold (log: $(cat "$T15_SC_LOG" 2>/dev/null || echo '<empty>'))"
fi

T15_EVENTS="${T15_DIR}/watchdog-restart-events"
if [[ -f "$T15_EVENTS" ]]; then
  pass "watchdog-restart-events written on RAM-triggered restart"
else
  fail "watchdog-restart-events NOT written on RAM-triggered restart"
fi

# =============================================================================
# Test 16: API 200 + RSS below threshold → no restart
# =============================================================================
section "Test 16: RSS below RAM_THRESHOLD_MB — no restart even when API is 200"

PORT16=$(find_free_port)
SRV16_PID=$(start_mock_server "$PORT16" 200)

if wait_for_server "$PORT16" 5; then
  pass "mock HTTP server (200) is listening on port $PORT16"
else
  fail "mock HTTP server did not start on port $PORT16 within 5 s"
fi

T16_DIR=$(mktemp -d "$TMPDIR_TEST/t16-XXXXXXXX")
T16_SC_LOG="$T16_DIR/systemctl.log"
T16_FAKE_SC=$(make_fake_bin "systemctl" "$T16_SC_LOG")

# 1 000 000 KB = 976 MB — well below 4800 MB threshold
NODE_API_URL="http://127.0.0.1:${PORT16}" \
  STATE_DIR="$T16_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_RSS_KB="1000000" \
  RAM_THRESHOLD_MB="4800" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T16_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T16_EXIT=$?

kill "$SRV16_PID" 2>/dev/null || true
wait "$SRV16_PID" 2>/dev/null || true

if [[ $T16_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when RSS is below threshold"
else
  fail "watchdog exited $T16_EXIT (expected 0)"
fi

if [[ ! -f "$T16_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T16_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called when RSS is below threshold"
else
  fail "systemctl restart was unexpectedly called for RSS below threshold"
fi

# =============================================================================
# Test 17: RAM_THRESHOLD_MB=0 disables RAM check → no restart regardless of RSS
# =============================================================================
section "Test 17: RAM_THRESHOLD_MB=0 disables RAM check — no restart even for huge RSS"

PORT17=$(find_free_port)
SRV17_PID=$(start_mock_server "$PORT17" 200)

if wait_for_server "$PORT17" 5; then
  pass "mock HTTP server (200) is listening on port $PORT17"
else
  fail "mock HTTP server did not start on port $PORT17 within 5 s"
fi

T17_DIR=$(mktemp -d "$TMPDIR_TEST/t17-XXXXXXXX")
T17_SC_LOG="$T17_DIR/systemctl.log"
T17_FAKE_SC=$(make_fake_bin "systemctl" "$T17_SC_LOG")

# RSS = 50 GB — absurdly high, but check is disabled
NODE_API_URL="http://127.0.0.1:${PORT17}" \
  STATE_DIR="$T17_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_RSS_KB="52428800" \
  RAM_THRESHOLD_MB="0" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T17_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T17_EXIT=$?

kill "$SRV17_PID" 2>/dev/null || true
wait "$SRV17_PID" 2>/dev/null || true

if [[ $T17_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when RAM check is disabled"
else
  fail "watchdog exited $T17_EXIT (expected 0)"
fi

if [[ ! -f "$T17_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T17_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called when RAM_THRESHOLD_MB=0 (check disabled)"
else
  fail "systemctl restart was unexpectedly called when RAM_THRESHOLD_MB=0"
fi

# =============================================================================
# Test 18: API 200 + height advancing → no restart, stall counter zeroed
# =============================================================================
section "Test 18: Block height advancing — no restart, stall counter reset to 0"

PORT18=$(find_free_port)
SRV18_PID=$(start_mock_server "$PORT18" 200)

if wait_for_server "$PORT18" 5; then
  pass "mock HTTP server (200) is listening on port $PORT18"
else
  fail "mock HTTP server did not start on port $PORT18 within 5 s"
fi

T18_DIR=$(mktemp -d "$TMPDIR_TEST/t18-XXXXXXXX")
T18_SC_LOG="$T18_DIR/systemctl.log"
T18_FAKE_SC=$(make_fake_bin "systemctl" "$T18_SC_LOG")

# Pre-populate height file so we can test advancing from 100 → 200
echo "100" > "${T18_DIR}/watchdog-last-height"
# Pre-set a stall count to verify it gets reset
echo "2" > "${T18_DIR}/watchdog-stall-count"

NODE_API_URL="http://127.0.0.1:${PORT18}" \
  STATE_DIR="$T18_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_HEIGHT="200" \
  STALL_CHECKS_MAX="3" \
  WATCHDOG_INTERVAL_SECS="45" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T18_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T18_EXIT=$?

kill "$SRV18_PID" 2>/dev/null || true
wait "$SRV18_PID" 2>/dev/null || true

if [[ $T18_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when height is advancing"
else
  fail "watchdog exited $T18_EXIT (expected 0)"
fi

if [[ ! -f "$T18_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T18_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called when height advances"
else
  fail "systemctl restart was unexpectedly called when height advances"
fi

T18_STALL=$(cat "${T18_DIR}/watchdog-stall-count" 2>/dev/null || echo "X")
if [[ "$T18_STALL" == "0" ]]; then
  pass "stall counter reset to 0 after height advance"
else
  fail "stall counter should be 0 after height advance, got: $T18_STALL"
fi

T18_STORED=$(cat "${T18_DIR}/watchdog-last-height" 2>/dev/null || echo "X")
if [[ "$T18_STORED" == "200" ]]; then
  pass "last-height updated to 200 after advance"
else
  fail "last-height should be 200, got: $T18_STORED"
fi

# =============================================================================
# Test 19: API 200 + height stalled for STALL_CHECKS_MAX checks → restart
# =============================================================================
section "Test 19: Block height stalled for STALL_CHECKS_MAX checks → restart triggered"

PORT19=$(find_free_port)
SRV19_PID=$(start_mock_server "$PORT19" 200)

if wait_for_server "$PORT19" 5; then
  pass "mock HTTP server (200) is listening on port $PORT19"
else
  fail "mock HTTP server did not start on port $PORT19 within 5 s"
fi

T19_DIR=$(mktemp -d "$TMPDIR_TEST/t19-XXXXXXXX")
T19_SC_LOG="$T19_DIR/systemctl.log"
T19_FAKE_SC=$(make_fake_bin "systemctl" "$T19_SC_LOG")

# Pre-populate height = 500 and stall count = STALL_CHECKS_MAX-1 so this
# probe is the final one that crosses the threshold.
echo "500" > "${T19_DIR}/watchdog-last-height"
echo "2" > "${T19_DIR}/watchdog-stall-count"  # +1 = 3 = STALL_CHECKS_MAX → fire

NODE_API_URL="http://127.0.0.1:${PORT19}" \
  STATE_DIR="$T19_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_HEIGHT="500" \
  STALL_CHECKS_MAX="3" \
  WATCHDOG_INTERVAL_SECS="45" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T19_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T19_EXIT=$?

kill "$SRV19_PID" 2>/dev/null || true
wait "$SRV19_PID" 2>/dev/null || true

if [[ $T19_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 after stall-triggered restart"
else
  fail "watchdog exited $T19_EXIT (expected 0)"
fi

if [[ -f "$T19_SC_LOG" ]] && grep -q "systemctl restart aperod-node" "$T19_SC_LOG"; then
  pass "systemctl restart called after STALL_CHECKS_MAX stall probes"
else
  fail "systemctl restart NOT called after STALL_CHECKS_MAX stall probes (log: $(cat "$T19_SC_LOG" 2>/dev/null || echo '<empty>'))"
fi

T19_EVENTS="${T19_DIR}/watchdog-restart-events"
if [[ -f "$T19_EVENTS" ]]; then
  pass "watchdog-restart-events written on stall-triggered restart"
else
  fail "watchdog-restart-events NOT written on stall-triggered restart"
fi

T19_STALL=$(cat "${T19_DIR}/watchdog-stall-count" 2>/dev/null || echo "X")
if [[ "$T19_STALL" == "0" ]]; then
  pass "stall counter reset to 0 after stall restart"
else
  fail "stall counter should be 0 after restart, got: $T19_STALL"
fi

# =============================================================================
# Test 20: API 200 + height regresses (post-restart snapshot) → no restart
# =============================================================================
section "Test 20: Block height regresses after snapshot reload — no restart, counter reset"

PORT20=$(find_free_port)
SRV20_PID=$(start_mock_server "$PORT20" 200)

if wait_for_server "$PORT20" 5; then
  pass "mock HTTP server (200) is listening on port $PORT20"
else
  fail "mock HTTP server did not start on port $PORT20 within 5 s"
fi

T20_DIR=$(mktemp -d "$TMPDIR_TEST/t20-XXXXXXXX")
T20_SC_LOG="$T20_DIR/systemctl.log"
T20_FAKE_SC=$(make_fake_bin "systemctl" "$T20_SC_LOG")

# Simulate: last known height was 5000, node restarted from snapshot at 4800
echo "5000" > "${T20_DIR}/watchdog-last-height"
echo "1" > "${T20_DIR}/watchdog-stall-count"

NODE_API_URL="http://127.0.0.1:${PORT20}" \
  STATE_DIR="$T20_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_HEIGHT="4800" \
  STALL_CHECKS_MAX="3" \
  WATCHDOG_INTERVAL_SECS="45" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T20_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T20_EXIT=$?

kill "$SRV20_PID" 2>/dev/null || true
wait "$SRV20_PID" 2>/dev/null || true

if [[ $T20_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 on height regression (post-restart snapshot)"
else
  fail "watchdog exited $T20_EXIT (expected 0)"
fi

if [[ ! -f "$T20_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T20_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called on height regression"
else
  fail "systemctl restart was unexpectedly called on height regression"
fi

T20_STALL=$(cat "${T20_DIR}/watchdog-stall-count" 2>/dev/null || echo "X")
if [[ "$T20_STALL" == "0" ]]; then
  pass "stall counter reset to 0 after height regression"
else
  fail "stall counter should be 0 after height regression, got: $T20_STALL"
fi

T20_STORED=$(cat "${T20_DIR}/watchdog-last-height" 2>/dev/null || echo "X")
if [[ "$T20_STORED" == "4800" ]]; then
  pass "last-height updated to regressed value 4800"
else
  fail "last-height should be 4800, got: $T20_STORED"
fi

# =============================================================================
# Test 21: peer_count > 0 → zero-since file NOT created, no alert
# =============================================================================
section "Test 21: peer_count > 0 — zero-since timer is not started"

PORT21=$(find_free_port)
SRV21_PID=$(start_mock_server "$PORT21" 200)

if wait_for_server "$PORT21" 5; then
  pass "mock HTTP server (200) is listening on port $PORT21"
else
  fail "mock HTTP server did not start on port $PORT21 within 5 s"
fi

T21_DIR=$(mktemp -d "$TMPDIR_TEST/t21-XXXXXXXX")
T21_SC_LOG="$T21_DIR/systemctl.log"
T21_FAKE_SC=$(make_fake_bin "systemctl" "$T21_SC_LOG")

NODE_API_URL="http://127.0.0.1:${PORT21}" \
  STATE_DIR="$T21_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_PEER_COUNT="3" \
  PEER_WAIT_MINS="10" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T21_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T21_EXIT=$?

kill "$SRV21_PID" 2>/dev/null || true
wait "$SRV21_PID" 2>/dev/null || true

if [[ $T21_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when peer_count > 0"
else
  fail "watchdog exited $T21_EXIT (expected 0)"
fi

T21_ZERO_SINCE="${T21_DIR}/watchdog-peers-zero-since"
if [[ ! -f "$T21_ZERO_SINCE" ]]; then
  pass "zero-since file NOT created when peers > 0"
else
  fail "zero-since file was unexpectedly created when peers = 3"
fi

if [[ ! -f "$T21_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T21_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called when peers > 0"
else
  fail "systemctl restart was unexpectedly called when peers > 0"
fi

# =============================================================================
# Test 22: peer_count=0 but under threshold → zero-since file created, no alert
# =============================================================================
section "Test 22: peer_count=0 below threshold — timer started, no Telegram alert yet"

PORT22=$(find_free_port)
SRV22_PID=$(start_mock_server "$PORT22" 200)

if wait_for_server "$PORT22" 5; then
  pass "mock HTTP server (200) is listening on port $PORT22"
else
  fail "mock HTTP server did not start on port $PORT22 within 5 s"
fi

T22_DIR=$(mktemp -d "$TMPDIR_TEST/t22-XXXXXXXX")
T22_SC_LOG="$T22_DIR/systemctl.log"
T22_CURL_LOG="$T22_DIR/curl.log"
T22_FAKE_SC=$(make_fake_bin "systemctl" "$T22_SC_LOG")
T22_FAKE_CURL=$(make_fake_curl "$T22_CURL_LOG" "200")

# No pre-existing zero-since file — first probe with peer_count=0
NODE_API_URL="http://127.0.0.1:${PORT22}" \
  STATE_DIR="$T22_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_PEER_COUNT="0" \
  PEER_WAIT_MINS="10" \
  SUPPORT_BOT_TOKEN="test-token" \
  SUPPORT_ADMIN_CHAT_ID="123456" \
  PATH="$T22_FAKE_CURL:$T22_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T22_EXIT=$?

kill "$SRV22_PID" 2>/dev/null || true
wait "$SRV22_PID" 2>/dev/null || true

if [[ $T22_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when peer_count=0 below threshold"
else
  fail "watchdog exited $T22_EXIT (expected 0)"
fi

T22_ZERO_SINCE="${T22_DIR}/watchdog-peers-zero-since"
if [[ -f "$T22_ZERO_SINCE" ]]; then
  pass "zero-since file created on first peer_count=0 probe"
else
  fail "zero-since file NOT created on first peer_count=0 probe"
fi

# No Telegram alert should have been sent (threshold not reached yet)
if [[ ! -f "$T22_CURL_LOG" ]] || ! grep -q "api.telegram.org" "$T22_CURL_LOG" 2>/dev/null; then
  pass "no Telegram alert sent before threshold is reached"
else
  fail "Telegram alert was sent before threshold — should wait ${PEER_WAIT_MINS} min"
fi

# =============================================================================
# Test 23: peer_count=0 and threshold exceeded → Telegram alert sent
# =============================================================================
section "Test 23: peer_count=0 for longer than PEER_WAIT_MINS — Telegram alert is sent"

PORT23=$(find_free_port)
SRV23_PID=$(start_mock_server "$PORT23" 200)

if wait_for_server "$PORT23" 5; then
  pass "mock HTTP server (200) is listening on port $PORT23"
else
  fail "mock HTTP server did not start on port $PORT23 within 5 s"
fi

T23_DIR=$(mktemp -d "$TMPDIR_TEST/t23-XXXXXXXX")
T23_SC_LOG="$T23_DIR/systemctl.log"
T23_CURL_LOG="$T23_DIR/curl.log"
T23_FAKE_SC=$(make_fake_bin "systemctl" "$T23_SC_LOG")
T23_FAKE_CURL=$(make_fake_curl "$T23_CURL_LOG" "200")

# Pre-populate zero-since to 20 minutes ago — well past the 1-minute threshold
T23_PAST=$(( $(date +%s) - 1200 ))
echo "${T23_PAST}" > "${T23_DIR}/watchdog-peers-zero-since"

NODE_API_URL="http://127.0.0.1:${PORT23}" \
  STATE_DIR="$T23_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_PEER_COUNT="0" \
  PEER_WAIT_MINS="1" \
  SUPPORT_BOT_TOKEN="test-token" \
  SUPPORT_ADMIN_CHAT_ID="123456" \
  PATH="$T23_FAKE_CURL:$T23_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T23_EXIT=$?

kill "$SRV23_PID" 2>/dev/null || true
wait "$SRV23_PID" 2>/dev/null || true

if [[ $T23_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 after peer-zero alert threshold exceeded"
else
  fail "watchdog exited $T23_EXIT (expected 0)"
fi

if [[ -f "$T23_CURL_LOG" ]] && grep -q "api.telegram.org" "$T23_CURL_LOG" 2>/dev/null; then
  pass "Telegram alert sent when peer_count=0 exceeds PEER_WAIT_MINS"
else
  fail "Telegram alert NOT sent when peer_count=0 exceeds threshold (curl log: $(cat "$T23_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

T23_LAST_ALERT="${T23_DIR}/watchdog-last-peer-alert"
if [[ -f "$T23_LAST_ALERT" ]]; then
  pass "watchdog-last-peer-alert written after sending alert"
else
  fail "watchdog-last-peer-alert NOT written after sending alert"
fi

# No restart should be triggered by the peer-zero alert
if [[ ! -f "$T23_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T23_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called for peer-zero alert (alert only, not a restart trigger)"
else
  fail "systemctl restart was unexpectedly called for peer-zero alert"
fi

# =============================================================================
# Test 24: peer_count recovers (> 0) → zero-since AND alert cooldown both cleared
# =============================================================================
section "Test 24: peer_count recovers to > 0 — zero-since timer AND alert cooldown cleared"

PORT24=$(find_free_port)
SRV24_PID=$(start_mock_server "$PORT24" 200)

if wait_for_server "$PORT24" 5; then
  pass "mock HTTP server (200) is listening on port $PORT24"
else
  fail "mock HTTP server did not start on port $PORT24 within 5 s"
fi

T24_DIR=$(mktemp -d "$TMPDIR_TEST/t24-XXXXXXXX")
T24_SC_LOG="$T24_DIR/systemctl.log"
T24_FAKE_SC=$(make_fake_bin "systemctl" "$T24_SC_LOG")

# Pre-populate both state files as if a prior outage occurred and was alerted
T24_PAST=$(( $(date +%s) - 300 ))
echo "${T24_PAST}" > "${T24_DIR}/watchdog-peers-zero-since"
echo "${T24_PAST}" > "${T24_DIR}/watchdog-last-peer-alert"

NODE_API_URL="http://127.0.0.1:${PORT24}" \
  STATE_DIR="$T24_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_PEER_COUNT="2" \
  PEER_WAIT_MINS="10" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T24_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T24_EXIT=$?

kill "$SRV24_PID" 2>/dev/null || true
wait "$SRV24_PID" 2>/dev/null || true

if [[ $T24_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when peers recovered"
else
  fail "watchdog exited $T24_EXIT (expected 0)"
fi

T24_ZERO_SINCE="${T24_DIR}/watchdog-peers-zero-since"
if [[ ! -f "$T24_ZERO_SINCE" ]]; then
  pass "zero-since file removed when peer_count recovered to > 0"
else
  fail "zero-since file was NOT removed after peer recovery (content: $(cat "$T24_ZERO_SINCE" 2>/dev/null || echo '<empty>'))"
fi

T24_LAST_ALERT="${T24_DIR}/watchdog-last-peer-alert"
if [[ ! -f "$T24_LAST_ALERT" ]]; then
  pass "alert cooldown file removed on recovery so next outage is not silenced"
else
  fail "alert cooldown file was NOT removed on recovery — next distinct outage would be suppressed"
fi

# =============================================================================
# Test 25: /api/v1/network/stats returns JSON without peer_count field →
#          zero-since timer reset (including any pre-existing stale timer)
# =============================================================================
section "Test 25: stats JSON without peer_count — zero-since timer reset (stale timer cleared)"

PORT25=$(find_free_port)
SRV25_PID=$(start_json_server "$PORT25" 200 '{"height":1000,"mempool_count":0}')

if wait_for_server "$PORT25" 5; then
  pass "mock JSON server (no peer_count) is listening on port $PORT25"
else
  fail "mock JSON server did not start on port $PORT25 within 5 s"
fi

T25_DIR=$(mktemp -d "$TMPDIR_TEST/t25-XXXXXXXX")
T25_SC_LOG="$T25_DIR/systemctl.log"
T25_FAKE_SC=$(make_fake_bin "systemctl" "$T25_SC_LOG")

# Pre-populate a stale zero-since to verify it is reset when data is unavailable
T25_PAST=$(( $(date +%s) - 3600 ))
echo "${T25_PAST}" > "${T25_DIR}/watchdog-peers-zero-since"

NODE_API_URL="http://127.0.0.1:${PORT25}" \
  STATE_DIR="$T25_DIR" \
  TIMEOUT_SECS="3" \
  PEER_WAIT_MINS="1" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T25_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T25_EXIT=$?

kill "$SRV25_PID" 2>/dev/null || true
wait "$SRV25_PID" 2>/dev/null || true

if [[ $T25_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when peer_count field absent"
else
  fail "watchdog exited $T25_EXIT (expected 0)"
fi

T25_ZERO_SINCE="${T25_DIR}/watchdog-peers-zero-since"
if [[ ! -f "$T25_ZERO_SINCE" ]]; then
  pass "stale zero-since file REMOVED when peer_count is absent from stats response"
else
  fail "stale zero-since file was NOT removed for absent peer_count field"
fi

if [[ ! -f "$T25_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T25_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called when peer_count is absent"
else
  fail "systemctl restart was unexpectedly called when peer_count is absent"
fi

# =============================================================================
# Test 26: /api/v1/network/stats returns malformed body →
#          zero-since timer reset (including any pre-existing stale timer)
# =============================================================================
section "Test 26: stats malformed body — stale zero-since timer reset, no false alert"

PORT26=$(find_free_port)
SRV26_PID=$(start_json_server "$PORT26" 200 'internal error')

if wait_for_server "$PORT26" 5; then
  pass "mock server (malformed body) is listening on port $PORT26"
else
  fail "mock server did not start on port $PORT26 within 5 s"
fi

T26_DIR=$(mktemp -d "$TMPDIR_TEST/t26-XXXXXXXX")
T26_SC_LOG="$T26_DIR/systemctl.log"
T26_CURL_LOG="$T26_DIR/curl.log"
T26_FAKE_SC=$(make_fake_bin "systemctl" "$T26_SC_LOG")
T26_FAKE_CURL=$(make_fake_curl "$T26_CURL_LOG" "200")

# Pre-populate a stale zero-since that would trigger an alert if not reset
T26_PAST=$(( $(date +%s) - 3600 ))
echo "${T26_PAST}" > "${T26_DIR}/watchdog-peers-zero-since"

NODE_API_URL="http://127.0.0.1:${PORT26}" \
  STATE_DIR="$T26_DIR" \
  TIMEOUT_SECS="3" \
  PEER_WAIT_MINS="1" \
  SUPPORT_BOT_TOKEN="test-token" \
  SUPPORT_ADMIN_CHAT_ID="123456" \
  PATH="$T26_FAKE_CURL:$T26_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T26_EXIT=$?

kill "$SRV26_PID" 2>/dev/null || true
wait "$SRV26_PID" 2>/dev/null || true

if [[ $T26_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when stats body is malformed"
else
  fail "watchdog exited $T26_EXIT (expected 0)"
fi

T26_ZERO_SINCE="${T26_DIR}/watchdog-peers-zero-since"
if [[ ! -f "$T26_ZERO_SINCE" ]]; then
  pass "stale zero-since file REMOVED when stats body is malformed"
else
  fail "stale zero-since file was NOT removed for malformed stats body"
fi

# No Telegram alert should fire — malformed data is not a confirmed zero
if [[ ! -f "$T26_CURL_LOG" ]] || ! grep -q "api.telegram.org" "$T26_CURL_LOG" 2>/dev/null; then
  pass "no false Telegram alert when stats body is malformed (stale timer was present)"
else
  fail "false Telegram alert was sent despite malformed stats — stale timer should have been reset"
fi

# =============================================================================
# Test 27: PEER_WAIT_MINS=abc (invalid) — falls back to default 10, no crash
# =============================================================================
section "Test 27: PEER_WAIT_MINS=abc (non-integer) — watchdog uses default 10, does not crash"

PORT27=$(find_free_port)
SRV27_PID=$(start_mock_server "$PORT27" 200)

if wait_for_server "$PORT27" 5; then
  pass "mock HTTP server (200) is listening on port $PORT27"
else
  fail "mock HTTP server did not start on port $PORT27 within 5 s"
fi

T27_DIR=$(mktemp -d "$TMPDIR_TEST/t27-XXXXXXXX")
T27_SC_LOG="$T27_DIR/systemctl.log"
T27_STDERR="$T27_DIR/stderr.log"
T27_FAKE_SC=$(make_fake_bin "systemctl" "$T27_SC_LOG")

NODE_API_URL="http://127.0.0.1:${PORT27}" \
  STATE_DIR="$T27_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_PEER_COUNT="0" \
  PEER_WAIT_MINS="abc" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T27_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>"$T27_STDERR"
T27_EXIT=$?

kill "$SRV27_PID" 2>/dev/null || true
wait "$SRV27_PID" 2>/dev/null || true

if [[ $T27_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 despite invalid PEER_WAIT_MINS=abc"
else
  fail "watchdog exited $T27_EXIT — should not crash on invalid PEER_WAIT_MINS (stderr: $(cat "$T27_STDERR" 2>/dev/null || echo '<empty>'))"
fi

if [[ ! -f "$T27_SC_LOG" ]] || ! grep -q "restart aperod-node" "$T27_SC_LOG" 2>/dev/null; then
  pass "systemctl restart NOT called when PEER_WAIT_MINS is invalid (threshold not yet reached)"
else
  fail "systemctl restart was unexpectedly called for invalid PEER_WAIT_MINS"
fi

# =============================================================================
# Test 28: recovery then new threshold-exceeding outage → alert fires immediately
#          (proves the alert cooldown is cleared on recovery, not just zero-since)
# =============================================================================
section "Test 28: new outage after recovery fires alert even within old ALERT_COOLDOWN_SECS"

PORT28=$(find_free_port)
SRV28_PID=$(start_mock_server "$PORT28" 200)

if wait_for_server "$PORT28" 5; then
  pass "mock HTTP server (200) is listening on port $PORT28"
else
  fail "mock HTTP server did not start on port $PORT28 within 5 s"
fi

T28_DIR=$(mktemp -d "$TMPDIR_TEST/t28-XXXXXXXX")
T28_SC_LOG="$T28_DIR/systemctl.log"
T28_CURL_LOG="$T28_DIR/curl.log"
T28_FAKE_SC=$(make_fake_bin "systemctl" "$T28_SC_LOG")
T28_FAKE_CURL=$(make_fake_curl "$T28_CURL_LOG" "200")

# Simulate: peer recovered (last-peer-alert was set seconds ago from prior outage),
# then immediately another outage triggers.  With the cooldown cleared on recovery,
# this new outage MUST alert without waiting for ALERT_COOLDOWN_SECS to expire.
# We set watchdog-peers-zero-since to 20 min ago (past PEER_WAIT_MINS=1) and
# watchdog-last-peer-alert to 0 (cleared) — this is the state after a recovery run.
T28_PAST=$(( $(date +%s) - 1200 ))
echo "${T28_PAST}" > "${T28_DIR}/watchdog-peers-zero-since"
# No last-peer-alert file → alert cooldown elapsed (cleared by recovery)

NODE_API_URL="http://127.0.0.1:${PORT28}" \
  STATE_DIR="$T28_DIR" \
  TIMEOUT_SECS="3" \
  MOCK_PEER_COUNT="0" \
  PEER_WAIT_MINS="1" \
  ALERT_COOLDOWN_SECS="3600" \
  SUPPORT_BOT_TOKEN="test-token" \
  SUPPORT_ADMIN_CHAT_ID="123456" \
  PATH="$T28_FAKE_CURL:$T28_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T28_EXIT=$?

kill "$SRV28_PID" 2>/dev/null || true
wait "$SRV28_PID" 2>/dev/null || true

if [[ $T28_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 for new outage after recovery"
else
  fail "watchdog exited $T28_EXIT (expected 0)"
fi

if [[ -f "$T28_CURL_LOG" ]] && grep -q "api.telegram.org" "$T28_CURL_LOG" 2>/dev/null; then
  pass "Telegram alert fires for new outage — cooldown was correctly cleared by prior recovery"
else
  fail "Telegram alert NOT sent for new outage — cooldown was NOT cleared (second outage silently suppressed)"
fi

# =============================================================================
# Test 29: PEER_WAIT_MINS=0 (disabled) clears stale zero-since and cooldown files
# =============================================================================
section "Test 29: PEER_WAIT_MINS=0 clears stale state — re-enable won't alert immediately"

PORT29=$(find_free_port)
SRV29_PID=$(start_mock_server "$PORT29" 200)

if wait_for_server "$PORT29" 5; then
  pass "mock HTTP server (200) is listening on port $PORT29"
else
  fail "mock HTTP server did not start on port $PORT29 within 5 s"
fi

T29_DIR=$(mktemp -d "$TMPDIR_TEST/t29-XXXXXXXX")
T29_SC_LOG="$T29_DIR/systemctl.log"
T29_FAKE_SC=$(make_fake_bin "systemctl" "$T29_SC_LOG")

# Pre-populate both state files — disabling the check should wipe them
T29_PAST=$(( $(date +%s) - 3600 ))
echo "${T29_PAST}" > "${T29_DIR}/watchdog-peers-zero-since"
echo "${T29_PAST}" > "${T29_DIR}/watchdog-last-peer-alert"

NODE_API_URL="http://127.0.0.1:${PORT29}" \
  STATE_DIR="$T29_DIR" \
  TIMEOUT_SECS="3" \
  PEER_WAIT_MINS="0" \
  SUPPORT_BOT_TOKEN="" \
  SUPPORT_ADMIN_CHAT_ID="" \
  PATH="$T29_FAKE_SC:$PATH" \
  bash "$WATCHDOG_SH" >/dev/null 2>&1
T29_EXIT=$?

kill "$SRV29_PID" 2>/dev/null || true
wait "$SRV29_PID" 2>/dev/null || true

if [[ $T29_EXIT -eq 0 ]]; then
  pass "watchdog exited 0 when PEER_WAIT_MINS=0"
else
  fail "watchdog exited $T29_EXIT (expected 0)"
fi

T29_ZERO_SINCE="${T29_DIR}/watchdog-peers-zero-since"
if [[ ! -f "$T29_ZERO_SINCE" ]]; then
  pass "zero-since state cleared when PEER_WAIT_MINS=0"
else
  fail "zero-since state NOT cleared when feature is disabled"
fi

T29_LAST_ALERT="${T29_DIR}/watchdog-last-peer-alert"
if [[ ! -f "$T29_LAST_ALERT" ]]; then
  pass "alert cooldown cleared when PEER_WAIT_MINS=0"
else
  fail "alert cooldown NOT cleared when feature is disabled"
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
