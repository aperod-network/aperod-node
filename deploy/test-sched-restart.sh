#!/usr/bin/env bash
# =============================================================================
#  test-sched-restart.sh — Integration tests for aperod-node-sched-restart.sh
#
#  Scenarios:
#    S1. --connect-timeout and --max-time flags present in Telegram curl calls
#        and restart still executes when Telegram curl fails immediately
#    S2. Telegram curl unavailable (exits 1) → restart proceeds; exit 0
#    S3. Node recovers after restart → success notification sent, watchdog resumed
#    S4. Node does not recover → warning notification sent, watchdog still resumed
#    S5. Watchdog timer absent (is-active returns 1) → no failure, restart runs
#
#  Run from anywhere:
#    bash blockchain/deploy/test-sched-restart.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCHED_SH="$SCRIPT_DIR/aperod-node-sched-restart.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0; FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; (( PASS++ )); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; (( FAIL++ )); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

if ! command -v python3 >/dev/null 2>&1; then
  echo -e "${RED}[ERR]${NC}  python3 required." >&2; exit 1
fi
if [[ ! -f "$SCHED_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  aperod-node-sched-restart.sh not found at: $SCHED_SH" >&2; exit 1
fi

REAL_CURL="$(command -v curl 2>/dev/null || echo "")"
if [[ -z "$REAL_CURL" ]]; then
  echo -e "${RED}[ERR]${NC}  curl not found in PATH; required for API health mock." >&2; exit 1
fi

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

find_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket(); s.bind(('127.0.0.1', 0)); print(s.getsockname()[1]); s.close()
PY
}

# Start a persistent mock HTTP server returning $2 for every GET on port $1.
start_mock_server() {
  local port="$1" code="$2"
  python3 -u - "$port" "$code" <<'PY' >/dev/null 2>&1 &
import http.server, sys, signal
port, code = int(sys.argv[1]), int(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(code); self.end_headers()
    def log_message(self, *a): pass
signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PY
  echo $!
}

wait_for_server() {
  local port="$1" timeout="${2:-5}" deadline
  deadline=$(( $(date +%s) + timeout ))
  while (( $(date +%s) < deadline )); do
    python3 -c "
import urllib.request
try: urllib.request.urlopen('http://127.0.0.1:$port/', timeout=0.5)
except: pass
" 2>/dev/null && return 0
    sleep 0.2
  done
  return 1
}

# Write a smart mock curl:
#   - Telegram calls (URL contains "api.telegram.org"): record + exit $TELEGRAM_EXIT
#   - All other calls (API health check, etc.): delegate to real curl
# $1 = bindir, $2 = Telegram exit code (default 0)
# Prints the path to the calls log file.
write_mock_curl() {
  local bindir="$1" tg_exit="${2:-0}"
  local calls_file="$TMPDIR_TEST/curl_calls_${bindir##*/}"
  cat > "$bindir/curl" <<CURL_MOCK
#!/usr/bin/env bash
echo "\$*" >> "$calls_file"
for arg in "\$@"; do
  if [[ "\$arg" == *"api.telegram.org"* ]]; then
    exit $tg_exit
  fi
done
# Not a Telegram call — delegate to real curl
exec "$REAL_CURL" "\$@"
CURL_MOCK
  chmod +x "$bindir/curl"
  echo "$calls_file"
}

# Write a mock systemctl that records calls.
# $1 = bindir, $2 = "true"/"false" for is-active result
# Prints the path to the calls log file.
write_mock_systemctl() {
  local bindir="$1" active="${2:-true}"
  local calls_file="$TMPDIR_TEST/systemctl_calls_${bindir##*/}"
  cat > "$bindir/systemctl" <<SCH
#!/usr/bin/env bash
echo "\$*" >> "$calls_file"
[[ "\$1" == "is-active" ]] && { [[ "$active" == "true" ]] && exit 0 || exit 1; }
exit 0
SCH
  chmod +x "$bindir/systemctl"
  echo "$calls_file"
}

make_bindir() {
  local name="$1"
  local d="$TMPDIR_TEST/bin_$name"
  mkdir -p "$d"
  echo "$d"
}

run_script() {
  local bindir="$1"; shift
  env PATH="$bindir:$PATH" \
      _APEROD_TEST=1 \
      STATE_DIR="$TMPDIR_TEST/state_$$_$RANDOM" \
      "$@" \
      bash "$SCHED_SH"
}

# ---------------------------------------------------------------------------
# S1. Flags present + restart executes even when Telegram curl fails
# ---------------------------------------------------------------------------
section "S1: --connect-timeout/--max-time flags present; restart executes when Telegram fails"

PORT_S1=$(find_free_port)
SRV_S1=$(start_mock_server "$PORT_S1" 200)
wait_for_server "$PORT_S1" 5 || fail "mock API server S1 did not start"

BIN_S1=$(make_bindir s1)
SC_S1=$(write_mock_systemctl "$BIN_S1" true)
CL_S1=$(write_mock_curl "$BIN_S1" 1)   # Telegram exits 1 immediately

run_script "$BIN_S1" \
  NODE_API_URL="http://127.0.0.1:$PORT_S1" \
  SUPPORT_BOT_TOKEN="tok" SUPPORT_ADMIN_CHAT_ID="123" \
  SCHED_RESTART_INTERVAL_SECS=10800 >/dev/null 2>&1
kill "$SRV_S1" 2>/dev/null || true

# Verify --connect-timeout and --max-time flags appear in a Telegram call
if grep -q "api.telegram.org" "$CL_S1" 2>/dev/null; then
  TG_LINE=$(grep "api.telegram.org" "$CL_S1" | head -1)
  if echo "$TG_LINE" | grep -q "\-\-connect-timeout"; then
    pass "--connect-timeout flag present in Telegram curl call"
  else
    fail "--connect-timeout flag MISSING from Telegram curl call"
  fi
  if echo "$TG_LINE" | grep -q "\-\-max-time"; then
    pass "--max-time flag present in Telegram curl call"
  else
    fail "--max-time flag MISSING from Telegram curl call"
  fi
else
  fail "No Telegram curl call recorded (expected at least one)"
fi

if grep -q "restart aperod-node" "$SC_S1" 2>/dev/null; then
  pass "systemctl restart aperod-node called despite Telegram curl failure"
else
  fail "systemctl restart aperod-node NOT called when Telegram curl fails"
fi

# ---------------------------------------------------------------------------
# S2. Telegram curl exits 1 → restart proceeds, script exits 0
# ---------------------------------------------------------------------------
section "S2: Telegram unavailable → restart proceeds, exit 0"

PORT_S2=$(find_free_port)
SRV_S2=$(start_mock_server "$PORT_S2" 200)
wait_for_server "$PORT_S2" 5 || fail "mock API server S2 did not start"

BIN_S2=$(make_bindir s2)
SC_S2=$(write_mock_systemctl "$BIN_S2" false)
CL_S2=$(write_mock_curl "$BIN_S2" 1)

run_script "$BIN_S2" \
  NODE_API_URL="http://127.0.0.1:$PORT_S2" \
  SUPPORT_BOT_TOKEN="tok" SUPPORT_ADMIN_CHAT_ID="123" \
  SCHED_RESTART_INTERVAL_SECS=10800 >/dev/null 2>&1
RC_S2=$?
kill "$SRV_S2" 2>/dev/null || true

if grep -q "restart aperod-node" "$SC_S2" 2>/dev/null; then
  pass "systemctl restart aperod-node called when Telegram is down"
else
  fail "systemctl restart aperod-node NOT called when Telegram is down"
fi
if [[ "$RC_S2" -eq 0 ]]; then
  pass "Script exits 0 when Telegram is unavailable"
else
  fail "Script exited $RC_S2 (expected 0) when Telegram is unavailable"
fi

# ---------------------------------------------------------------------------
# S3. Node recovers → success notification + watchdog re-enabled
# ---------------------------------------------------------------------------
section "S3: Node recovers → success notification + watchdog re-enabled"

PORT_S3=$(find_free_port)
SRV_S3=$(start_mock_server "$PORT_S3" 200)
wait_for_server "$PORT_S3" 5 || fail "mock API server S3 did not start"

BIN_S3=$(make_bindir s3)
SC_S3=$(write_mock_systemctl "$BIN_S3" true)   # watchdog active
CL_S3=$(write_mock_curl "$BIN_S3" 0)            # Telegram succeeds

run_script "$BIN_S3" \
  NODE_API_URL="http://127.0.0.1:$PORT_S3" \
  SUPPORT_BOT_TOKEN="tok" SUPPORT_ADMIN_CHAT_ID="123" \
  SCHED_RESTART_INTERVAL_SECS=10800 >/dev/null 2>&1
kill "$SRV_S3" 2>/dev/null || true

TG_COUNT_S3=$(grep -c "api.telegram.org" "$CL_S3" 2>/dev/null || echo 0)
if (( TG_COUNT_S3 >= 2 )); then
  pass "≥2 Telegram notifications sent on successful recovery (pre + post)"
else
  fail "Expected ≥2 Telegram calls on success, got ${TG_COUNT_S3}"
fi

if grep -q "stop aperod-node-watchdog.timer" "$SC_S3" 2>/dev/null; then
  pass "Watchdog timer paused before restart"
else
  fail "Watchdog timer NOT paused before restart"
fi

if grep -q "start aperod-node-watchdog.timer" "$SC_S3" 2>/dev/null; then
  pass "Watchdog timer re-enabled after successful restart"
else
  fail "Watchdog timer NOT re-enabled after restart"
fi

# ---------------------------------------------------------------------------
# S4. Node does not recover → warning notification + watchdog still re-enabled
# ---------------------------------------------------------------------------
section "S4: Node does not recover → warning notification + watchdog re-enabled"

# Port with no server → real curl gets connection refused → HTTP "000"
PORT_S4=$(find_free_port)   # intentionally no server started

BIN_S4=$(make_bindir s4)
SC_S4=$(write_mock_systemctl "$BIN_S4" true)   # watchdog active
CL_S4=$(write_mock_curl "$BIN_S4" 0)            # Telegram succeeds; API check uses real curl

run_script "$BIN_S4" \
  NODE_API_URL="http://127.0.0.1:$PORT_S4" \
  SUPPORT_BOT_TOKEN="tok" SUPPORT_ADMIN_CHAT_ID="123" \
  SCHED_RESTART_INTERVAL_SECS=10800 >/dev/null 2>&1 || true

TG_COUNT_S4=$(grep -c "api.telegram.org" "$CL_S4" 2>/dev/null || echo 0)
if (( TG_COUNT_S4 >= 2 )); then
  pass "≥2 Telegram notifications sent on failed recovery (pre + warning)"
else
  fail "Expected ≥2 Telegram calls on failed recovery, got ${TG_COUNT_S4}"
fi

if grep -q "start aperod-node-watchdog.timer" "$SC_S4" 2>/dev/null; then
  pass "Watchdog timer re-enabled even after failed recovery"
else
  fail "Watchdog timer NOT re-enabled after failed recovery"
fi

# ---------------------------------------------------------------------------
# S5. Watchdog timer absent → no failure, restart runs normally
# ---------------------------------------------------------------------------
section "S5: Watchdog timer absent → no failure, restart runs"

PORT_S5=$(find_free_port)
SRV_S5=$(start_mock_server "$PORT_S5" 200)
wait_for_server "$PORT_S5" 5 || fail "mock API server S5 did not start"

BIN_S5=$(make_bindir s5)
SC_S5=$(write_mock_systemctl "$BIN_S5" false)   # is-active → 1 (absent)
CL_S5=$(write_mock_curl "$BIN_S5" 0)

run_script "$BIN_S5" \
  NODE_API_URL="http://127.0.0.1:$PORT_S5" \
  SCHED_RESTART_INTERVAL_SECS=10800 >/dev/null 2>&1
RC_S5=$?
kill "$SRV_S5" 2>/dev/null || true

if [[ "$RC_S5" -eq 0 ]]; then
  pass "Script exits 0 when watchdog timer is absent"
else
  fail "Script exited $RC_S5 when watchdog timer is absent (expected 0)"
fi

if grep -q "restart aperod-node" "$SC_S5" 2>/dev/null; then
  pass "systemctl restart aperod-node called when watchdog is absent"
else
  fail "systemctl restart aperod-node NOT called when watchdog is absent"
fi

if ! grep -q "stop aperod-node-watchdog" "$SC_S5" 2>/dev/null; then
  pass "Watchdog stop NOT called when timer was inactive"
else
  fail "Watchdog stop called unexpectedly when timer was inactive"
fi

# ---------------------------------------------------------------------------
# S6. systemctl restart aperod-node returns non-zero →
#     watchdog re-enabled + failure notification sent
# ---------------------------------------------------------------------------
section "S6: systemctl restart fails → watchdog re-enabled + failure notification"

PORT_S6=$(find_free_port)
SRV_S6=$(start_mock_server "$PORT_S6" 200)
wait_for_server "$PORT_S6" 5 || fail "mock API server S6 did not start"

BIN_S6=$(make_bindir s6)

# Special systemctl: is-active → 0 (watchdog active), restart → 1 (fails), rest → 0
SC_S6="$TMPDIR_TEST/systemctl_calls_s6"
cat > "$BIN_S6/systemctl" <<'SCH6'
#!/usr/bin/env bash
echo "$*" >> "SC6_CALLS_PLACEHOLDER"
case "$1" in
  is-active)  exit 0 ;;
  restart)    exit 1 ;;
  *)          exit 0 ;;
esac
SCH6
sed -i "s|SC6_CALLS_PLACEHOLDER|$SC_S6|g" "$BIN_S6/systemctl"
chmod +x "$BIN_S6/systemctl"

CL_S6=$(write_mock_curl "$BIN_S6" 0)   # Telegram succeeds

run_script "$BIN_S6" \
  NODE_API_URL="http://127.0.0.1:$PORT_S6" \
  SUPPORT_BOT_TOKEN="tok" SUPPORT_ADMIN_CHAT_ID="123" \
  SCHED_RESTART_INTERVAL_SECS=10800 >/dev/null 2>&1 || true
kill "$SRV_S6" 2>/dev/null || true

# Watchdog must be re-enabled via EXIT trap even though restart failed
if grep -q "start aperod-node-watchdog.timer" "$SC_S6" 2>/dev/null; then
  pass "Watchdog re-enabled via EXIT trap after failed systemctl restart"
else
  fail "Watchdog NOT re-enabled after failed systemctl restart"
fi

# Failure notification must be sent
TG_S6=$(grep -c "api.telegram.org" "$CL_S6" 2>/dev/null || echo 0)
if (( TG_S6 >= 2 )); then
  pass "≥2 Telegram notifications sent (pre-restart + error alert)"
else
  fail "Expected ≥2 Telegram calls after restart failure, got ${TG_S6}"
fi

# ---------------------------------------------------------------------------
# Results
# ---------------------------------------------------------------------------
echo
echo -e "──────────────────────────────────────────────────"
echo -e "All assertions: PASS=${PASS}  FAIL=${FAIL}"
echo -e "──────────────────────────────────────────────────"
if (( FAIL > 0 )); then
  echo -e "${RED}${BOLD}FAIL: SchedRestartSh${NC}"
  exit 1
fi
echo -e "${GREEN}${BOLD}PASS: SchedRestartSh${NC}"
exit 0
