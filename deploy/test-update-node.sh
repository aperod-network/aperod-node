#!/usr/bin/env bash
# test-update-node.sh — Integration tests for the peer-check logic in update-node.sh.
#
# Sources the REAL blockchain/deploy/peer-check.sh (the same file update-node.sh
# uses in production) so any change to that file is immediately exercised here.
#
# Tests exercised
# ---------------
#   1. peer_count == 0  → warning on stderr, Telegram alert fired, exit 0 (non-fatal)
#   2. peer_count  > 0  → "P2P connected" on stdout, no warning, exit 0
#   3. SKIP_PEER_CHECK=1 → aperod_peer_check() returns before any curl call;
#                          verified by a PATH-injected mock curl that logs every
#                          invocation — the log must stay empty.
#
# Requirements: bash 4+, python3 (present on every supported server).
#
# Usage:
#   bash blockchain/deploy/test-update-node.sh
#   echo $?   # 0 = all tests passed, 1 = at least one failure

set -uo pipefail   # -e intentionally omitted so failures don't stop the runner

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PEER_CHECK_SH="${DEPLOY_DIR}/peer-check.sh"

if [[ ! -f "${PEER_CHECK_SH}" ]]; then
  echo "ERROR: ${PEER_CHECK_SH} not found — cannot run tests." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Colour helpers / counters
# ---------------------------------------------------------------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0

pass() { echo -e "${GREEN}[PASS]${NC} $1"; (( PASS++ )) || true; }
fail() { echo -e "${RED}[FAIL]${NC} $1"; (( FAIL++ )) || true; }
info() { echo -e "${YELLOW}[INFO]${NC} $1"; }

# ---------------------------------------------------------------------------
# start_mock_server <port> <peer_count>
#
# Starts a Python HTTP server in the background that returns:
#   GET /api/v1/status        → {"ok":true}
#   GET /api/v1/network/stats → {"peer_count":<peer_count>}
#
# Sets MOCK_PID to the background process ID.
# Call stop_mock_server when done.
# ---------------------------------------------------------------------------
MOCK_PID=""

start_mock_server() {
  local port="$1"
  local peer_count="$2"

  python3 - "${port}" "${peer_count}" &
  MOCK_PID=$!

  # Wait up to 2 s for the server to bind
  local deadline=$(( SECONDS + 2 ))
  while [[ $SECONDS -lt $deadline ]]; do
    if curl -sf --connect-timeout 1 "http://127.0.0.1:${port}/api/v1/status" \
        > /dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done

  echo "ERROR: mock server on port ${port} did not start" >&2
  return 1
} <<'PYSERVER'
import sys, json, http.server

port       = int(sys.argv[1])
peer_count = int(sys.argv[2])

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass   # suppress access log noise

    def _send_json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/api/v1/status":
            self._send_json(200, {"ok": True})
        elif self.path == "/api/v1/network/stats":
            self._send_json(200, {"peer_count": peer_count})
        else:
            self._send_json(404, {"error": "not found"})

http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
PYSERVER

stop_mock_server() {
  if [[ -n "${MOCK_PID}" ]]; then
    kill "${MOCK_PID}" 2>/dev/null || true
    wait "${MOCK_PID}" 2>/dev/null || true
    MOCK_PID=""
  fi
}

trap stop_mock_server EXIT

# ---------------------------------------------------------------------------
# find_free_port
# ---------------------------------------------------------------------------
find_free_port() {
  python3 -c "import socket; s=socket.socket(); s.bind(('',0)); print(s.getsockname()[1]); s.close()"
}

# ---------------------------------------------------------------------------
# run_peer_check <stats_url> <peer_wait_secs> <skip_peer> <tg_log> [curl_mock_bin]
#
# Runs aperod_peer_check() from the REAL peer-check.sh inside a sub-shell.
# send_telegram_alert() is overridden to append to <tg_log> (no network call).
# If <curl_mock_bin> is given it is prepended to PATH so mock curl is used.
# Returns the exit code of the sub-shell.
# ---------------------------------------------------------------------------
run_peer_check() {
  local stats_url="$1"
  local peer_wait_secs="$2"
  local skip_peer="$3"
  local tg_log="$4"
  local curl_mock_bin="${5:-}"

  # Build optional PATH prefix; expand before the heredoc is written.
  local path_prefix=""
  if [[ -n "${curl_mock_bin}" ]]; then
    path_prefix="export PATH=\"${curl_mock_bin}:\${PATH}\"; "
  fi

  bash <<SUBSHELL
set -uo pipefail

# Optional mock curl must appear in PATH first
${path_prefix}

# Source the real production peer-check.sh
# shellcheck source=peer-check.sh
source "${PEER_CHECK_SH}"

# Variables read by aperod_peer_check()
STATS_URL="${stats_url}"
PEER_WAIT_SECS="${peer_wait_secs}"
SKIP_PEER_CHECK="${skip_peer}"
SKIP_HEALTH_CHECK="0"
SERVICE_NAME="aperod-node"

# Override Telegram so no real HTTP call is made.
# Appends the alert message to the log file for assertion.
send_telegram_alert() {
  printf '%s\n' "\$1" >> "${tg_log}"
}

aperod_peer_check
SUBSHELL
}

# ---------------------------------------------------------------------------
# TEST 1 — peer_count == 0
#
# Expectations:
#   a) stderr contains the "WARNING: peer_count == 0" line
#   b) send_telegram_alert was called with "zero P2P peers" text
#   c) exit code is 0  (non-fatal)
# ---------------------------------------------------------------------------
echo ""
info "Test 1: peer_count == 0 — warning and Telegram alert expected (exit 0)"

PORT=$(find_free_port)
start_mock_server "${PORT}" 0

TG_LOG=$(mktemp); OUT=$(mktemp); ERR=$(mktemp)

run_peer_check \
  "http://127.0.0.1:${PORT}/api/v1/network/stats" \
  3 0 "${TG_LOG}" \
  >"${OUT}" 2>"${ERR}"
RC=$?

stop_mock_server

[[ ${RC} -eq 0 ]] \
  && pass "Test 1a: exit code is 0 (non-fatal)" \
  || fail "Test 1a: expected exit 0, got ${RC}"

grep -q "WARNING: peer_count == 0" "${ERR}" \
  && pass "Test 1b: warning line present on stderr" \
  || fail "Test 1b: expected WARNING line on stderr — got: $(cat "${ERR}")"

[[ -s "${TG_LOG}" ]] \
  && pass "Test 1c: Telegram alert was fired" \
  || fail "Test 1c: Telegram alert was NOT fired"

grep -q "zero P2P peers" "${TG_LOG}" \
  && pass "Test 1d: Telegram message mentions 'zero P2P peers'" \
  || fail "Test 1d: Telegram message body wrong — got: $(cat "${TG_LOG}")"

rm -f "${OUT}" "${ERR}" "${TG_LOG}"

# ---------------------------------------------------------------------------
# TEST 2 — peer_count > 0
#
# Expectations:
#   a) stdout contains "P2P connected: 3 peer(s) active"
#   b) NO warning on stderr
#   c) Telegram alert NOT fired
#   d) exit code is 0
# ---------------------------------------------------------------------------
echo ""
info "Test 2: peer_count == 3 — connected message expected, no warning (exit 0)"

PORT=$(find_free_port)
start_mock_server "${PORT}" 3

TG_LOG=$(mktemp); OUT=$(mktemp); ERR=$(mktemp)

run_peer_check \
  "http://127.0.0.1:${PORT}/api/v1/network/stats" \
  5 0 "${TG_LOG}" \
  >"${OUT}" 2>"${ERR}"
RC=$?

stop_mock_server

[[ ${RC} -eq 0 ]] \
  && pass "Test 2a: exit code is 0" \
  || fail "Test 2a: expected exit 0, got ${RC}"

grep -q "P2P connected" "${OUT}" \
  && pass "Test 2b: 'P2P connected' line on stdout" \
  || fail "Test 2b: expected 'P2P connected' on stdout — got: $(cat "${OUT}")"

! grep -q "WARNING" "${ERR}" \
  && pass "Test 2c: no warning on stderr" \
  || fail "Test 2c: unexpected WARNING — got: $(cat "${ERR}")"

[[ ! -s "${TG_LOG}" ]] \
  && pass "Test 2d: Telegram alert not fired (correct)" \
  || fail "Test 2d: Telegram alert was unexpectedly fired — got: $(cat "${TG_LOG}")"

rm -f "${OUT}" "${ERR}" "${TG_LOG}"

# ---------------------------------------------------------------------------
# TEST 3 — SKIP_PEER_CHECK=1
#
# Expectations:
#   a) exit code is 0
#   b) The step-5b header is never printed (function returned early)
#   c) The mock curl is never invoked for the stats endpoint
#      (verified via a PATH-injected curl stub that logs every call)
# ---------------------------------------------------------------------------
echo ""
info "Test 3: SKIP_PEER_CHECK=1 — peer check skipped; no curl to stats endpoint"

# Build a mock curl binary that logs its arguments.
MOCK_BIN=$(mktemp -d)
CURL_LOG=$(mktemp)

cat > "${MOCK_BIN}/curl" <<MOCKCURL
#!/usr/bin/env bash
printf '%s\n' "\$*" >> "${CURL_LOG}"
exit 1
MOCKCURL
chmod +x "${MOCK_BIN}/curl"

PORT=$(find_free_port)   # server intentionally NOT started
TG_LOG=$(mktemp); OUT=$(mktemp); ERR=$(mktemp)

run_peer_check \
  "http://127.0.0.1:${PORT}/api/v1/network/stats" \
  5 1 "${TG_LOG}" "${MOCK_BIN}" \
  >"${OUT}" 2>"${ERR}"
RC=$?

[[ ${RC} -eq 0 ]] \
  && pass "Test 3a: exit code is 0" \
  || fail "Test 3a: expected exit 0, got ${RC}"

! grep -q "\[5b\]" "${OUT}" \
  && pass "Test 3b: peer-check step header not printed (correctly skipped)" \
  || fail "Test 3b: peer-check step started — was not skipped"

# The mock curl log must be empty (or contain no reference to the stats URL)
if [[ -s "${CURL_LOG}" ]] && grep -q "network/stats" "${CURL_LOG}"; then
  fail "Test 3c: curl was called with the stats URL — should have been skipped: $(cat "${CURL_LOG}")"
else
  pass "Test 3c: curl never called for stats endpoint (correct)"
fi

! grep -q "WARNING" "${ERR}" \
  && pass "Test 3d: no warning on stderr" \
  || fail "Test 3d: unexpected WARNING on stderr"

[[ ! -s "${TG_LOG}" ]] \
  && pass "Test 3e: Telegram alert not fired (correct)" \
  || fail "Test 3e: Telegram alert was unexpectedly fired"

rm -f "${OUT}" "${ERR}" "${TG_LOG}" "${CURL_LOG}"
rm -rf "${MOCK_BIN}"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo ""
echo "========================================"
TOTAL=$(( PASS + FAIL ))
echo "Results: ${PASS}/${TOTAL} passed"
if [[ ${FAIL} -gt 0 ]]; then
  echo -e "${RED}${FAIL} test(s) FAILED${NC}"
  exit 1
else
  echo -e "${GREEN}All tests passed.${NC}"
  exit 0
fi
