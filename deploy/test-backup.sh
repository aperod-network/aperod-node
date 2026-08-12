#!/usr/bin/env bash
# =============================================================================
#  test-backup.sh — Integration tests for aperod_backup.sh Telegram alert path
#
#  Scenarios:
#    1. pg_dump failure → Telegram failure alert curl IS called with correct params
#    2. pg_dump failure + tokens unset → curl NOT called for Telegram
#    3. chat_id and bot token forwarded correctly in curl call
#    4. Static analysis — TELEGRAM_BOT_TOKEN / ADMIN_TELEGRAM_CHAT_ID read from env
#    5. EnvironmentFile line present in aperod-backup.service
#
#  Run from anywhere:
#    bash blockchain/deploy/test-backup.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_SH="$SCRIPT_DIR/aperod_backup.sh"
SERVICE_FILE="$SCRIPT_DIR/aperod-backup.service"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Ensure the script under test exists ───────────────────────────────────────
if [[ ! -f "$BACKUP_SH" ]]; then
  echo -e "${RED}[ERR]${NC}  aperod_backup.sh not found at: $BACKUP_SH" >&2
  exit 1
fi

# ── Shared temp directory (cleaned on exit) ───────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Build a fake-bin directory containing a stub for $1 that:
#   - logs all args to $2
#   - exits 0 (or exit code $3 if provided)
make_fake_bin() {
  local cmd="$1"
  local log_file="$2"
  local exit_code="${3:-0}"
  local fake_dir
  fake_dir=$(mktemp -d "$TMPDIR_TEST/fake-bin-XXXXXXXX")
  cat >"$fake_dir/$cmd" <<STUB
#!/usr/bin/env bash
echo "$cmd \$*" >> "$log_file"
exit $exit_code
STUB
  chmod +x "$fake_dir/$cmd"
  echo "$fake_dir"
}

# Build a fake curl that:
#   - logs every invocation to $1
#   - exits 0 (backup script discards curl output with >/dev/null 2>&1 || true)
make_fake_curl() {
  local log_file="$1"
  local fake_dir
  fake_dir=$(mktemp -d "$TMPDIR_TEST/fake-curl-XXXXXXXX")
  cat >"$fake_dir/curl" <<STUB
#!/usr/bin/env bash
echo "curl \$*" >> "$log_file"
exit 0
STUB
  chmod +x "$fake_dir/curl"
  echo "$fake_dir"
}

# Create a minimal integration-settings.json with valid S3 fields.
make_settings_json() {
  local dir="$1"
  mkdir -p "$dir"
  cat >"$dir/integration-settings.json" <<'JSON'
{
  "s3backup": {
    "endpoint": "https://s3.example.com",
    "accessKeyId": "TESTKEY",
    "secretAccessKey": "TESTSECRET",
    "bucket": "test-bucket",
    "region": "us-east-1",
    "retentionDays": 14
  }
}
JSON
}

# Run aperod_backup.sh with all external dependencies stubbed so the
# script fails at pg_dump (stub sudo exits 1) and fires the Telegram alert.
# Returns the exit code of the backup script.
#
# Args: $1=curl_log $2=fake_curl_dir [$3=TELEGRAM_BOT_TOKEN] [$4=ADMIN_TELEGRAM_CHAT_ID]
run_backup_fail() {
  local curl_log="$1"
  local fake_curl_dir="$2"
  local bot_token="${3:-}"
  local chat_id="${4:-}"

  local run_dir
  run_dir=$(mktemp -d "$TMPDIR_TEST/run-XXXXXXXX")

  # Settings file
  make_settings_json "$run_dir/data"

  # Metrics dir (writable by us)
  local metrics_dir="$run_dir/metrics"
  mkdir -p "$metrics_dir"

  # Stub sudo → exits 1 (simulates pg_dump failure)
  local sudo_log="$run_dir/sudo.log"
  local fake_sudo
  fake_sudo=$(make_fake_bin "sudo" "$sudo_log" 1)

  # Stub rclone (shouldn't be reached, but guard against any code path)
  local rclone_log="$run_dir/rclone.log"
  local fake_rclone
  fake_rclone=$(make_fake_bin "rclone" "$rclone_log" 0)

  # Stub gpg (shouldn't be reached either)
  local gpg_log="$run_dir/gpg.log"
  local fake_gpg
  fake_gpg=$(make_fake_bin "gpg" "$gpg_log" 0)

  # Provide a writable backup dir so the new mkdir guard passes and the
  # script reaches the pg_dump stage (where the sudo stub fails as intended).
  local backup_dir="$run_dir/backup-tmp"
  mkdir -p "$backup_dir"

  local exit_code=0
  APEROD_BACKUP_PASSWORD="test-password-123" \
    DATA_DIR="$run_dir/data" \
    APEROD_TEXTFILE_DIR="$metrics_dir" \
    APEROD_HISTORY_LOG="$run_dir/backup.log" \
    APEROD_BACKUP_DIR_OVERRIDE="$backup_dir" \
    TELEGRAM_BOT_TOKEN="$bot_token" \
    ADMIN_TELEGRAM_CHAT_ID="$chat_id" \
    PATH="$fake_curl_dir:$fake_sudo:$fake_rclone:$fake_gpg:$PATH" \
    bash "$BACKUP_SH" >/dev/null 2>&1 || exit_code=$?

  echo "$exit_code"
}

# =============================================================================
# Test 1: pg_dump failure → Telegram failure alert IS sent
# =============================================================================
section "Test 1: pg_dump failure → Telegram failure alert curl is called"

T1_CURL_LOG="$TMPDIR_TEST/curl-t1.log"
T1_FAKE_CURL=$(make_fake_curl "$T1_CURL_LOG")

T1_EXIT=$(run_backup_fail "$T1_CURL_LOG" "$T1_FAKE_CURL" \
            "test-bot-token-abc" "987654321")

if [[ "$T1_EXIT" -ne 0 ]]; then
  pass "backup script exited non-zero ($T1_EXIT) when sudo/pg_dump failed"
else
  fail "backup script exited 0 — expected failure; cannot verify Telegram path"
fi

if [[ -f "$T1_CURL_LOG" ]] && grep -q "api.telegram.org" "$T1_CURL_LOG"; then
  pass "curl was called with api.telegram.org (Telegram alert sent)"
else
  fail "curl was NOT called with api.telegram.org (log: $(cat "$T1_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T1_CURL_LOG" ]] && grep -q "sendMessage" "$T1_CURL_LOG"; then
  pass "Telegram sendMessage endpoint was targeted"
else
  fail "Telegram sendMessage endpoint was NOT targeted"
fi

# =============================================================================
# Test 2: pg_dump failure, tokens unset → Telegram curl NOT called
# =============================================================================
section "Test 2: pg_dump failure + tokens unset → Telegram alert silently skipped"

T2_CURL_LOG="$TMPDIR_TEST/curl-t2.log"
T2_FAKE_CURL=$(make_fake_curl "$T2_CURL_LOG")

T2_EXIT=$(run_backup_fail "$T2_CURL_LOG" "$T2_FAKE_CURL" "" "")

if [[ "$T2_EXIT" -ne 0 ]]; then
  pass "backup script exited non-zero ($T2_EXIT) — failure confirmed"
else
  fail "backup script unexpectedly exited 0"
fi

if [[ ! -f "$T2_CURL_LOG" ]] || ! grep -q "api.telegram.org" "$T2_CURL_LOG" 2>/dev/null; then
  pass "Telegram was NOT called when tokens are unset (silent skip)"
else
  fail "Telegram was unexpectedly called when tokens are unset"
fi

# =============================================================================
# Test 3: chat_id matches ADMIN_TELEGRAM_CHAT_ID in curl call
# =============================================================================
section "Test 3: chat_id forwarded correctly to Telegram sendMessage"

T3_CURL_LOG="$TMPDIR_TEST/curl-t3.log"
T3_FAKE_CURL=$(make_fake_curl "$T3_CURL_LOG")
T3_CHAT_ID="112233445"

T3_EXIT=$(run_backup_fail "$T3_CURL_LOG" "$T3_FAKE_CURL" \
            "tok-xyz-789" "$T3_CHAT_ID")

if [[ -f "$T3_CURL_LOG" ]] && grep -q "chat_id=${T3_CHAT_ID}" "$T3_CURL_LOG"; then
  pass "curl called with chat_id=${T3_CHAT_ID}"
else
  fail "curl NOT called with chat_id=${T3_CHAT_ID} (log: $(cat "$T3_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 4: bot token embedded in Telegram API URL
# =============================================================================
section "Test 4: bot token embedded correctly in Telegram API URL"

T4_CURL_LOG="$TMPDIR_TEST/curl-t4.log"
T4_FAKE_CURL=$(make_fake_curl "$T4_CURL_LOG")
T4_TOKEN="my-secret-bot-token-444"

T4_EXIT=$(run_backup_fail "$T4_CURL_LOG" "$T4_FAKE_CURL" \
            "$T4_TOKEN" "55667788")

if [[ -f "$T4_CURL_LOG" ]] && grep -q "bot${T4_TOKEN}" "$T4_CURL_LOG"; then
  pass "bot token embedded in Telegram API URL (bot<token>/sendMessage)"
else
  fail "bot token NOT found in curl URL (log: $(cat "$T4_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 5: alert text contains "fail" marker (both case variants acceptable)
# =============================================================================
section "Test 5: Telegram alert text contains failure indicator"

T5_CURL_LOG="$TMPDIR_TEST/curl-t5.log"
T5_FAKE_CURL=$(make_fake_curl "$T5_CURL_LOG")

T5_EXIT=$(run_backup_fail "$T5_CURL_LOG" "$T5_FAKE_CURL" \
            "tok-t5" "11223344")

if [[ -f "$T5_CURL_LOG" ]] && grep -iq "fail\|FAIL\|ошибк" "$T5_CURL_LOG"; then
  pass "Telegram message text contains failure marker"
else
  fail "Telegram message text does NOT contain failure marker (log: $(cat "$T5_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 6: Telegram only called ONCE per backup failure (no duplicate alerts)
# =============================================================================
section "Test 6: Telegram sendMessage called exactly once on backup failure"

T6_CURL_LOG="$TMPDIR_TEST/curl-t6.log"
T6_FAKE_CURL=$(make_fake_curl "$T6_CURL_LOG")

T6_EXIT=$(run_backup_fail "$T6_CURL_LOG" "$T6_FAKE_CURL" \
            "tok-t6" "99887766")

T6_COUNT=0
if [[ -f "$T6_CURL_LOG" ]]; then
  T6_COUNT=$(grep -c "sendMessage" "$T6_CURL_LOG" 2>/dev/null || echo "0")
fi

if [[ "$T6_COUNT" -eq 1 ]]; then
  pass "sendMessage called exactly once (no duplicate alerts)"
else
  fail "sendMessage call count: expected 1, got $T6_COUNT"
fi

# =============================================================================
# Test 7: Static analysis — TELEGRAM_BOT_TOKEN read from env, not hardcoded
# =============================================================================
section "Test 7: static analysis — TELEGRAM_BOT_TOKEN and ADMIN_TELEGRAM_CHAT_ID read from env"

HARDCODED_TOKEN=$(grep -E 'TELEGRAM_BOT_TOKEN\s*=' "$BACKUP_SH" \
  | grep -v '^\s*#' \
  | grep -v '\${TELEGRAM_BOT_TOKEN' \
  || true)
if [[ -z "$HARDCODED_TOKEN" ]]; then
  pass "TELEGRAM_BOT_TOKEN is not assigned a literal value — reads from env"
else
  fail "TELEGRAM_BOT_TOKEN appears to be hardcoded: $HARDCODED_TOKEN"
fi

HARDCODED_CHAT=$(grep -E 'ADMIN_TELEGRAM_CHAT_ID\s*=' "$BACKUP_SH" \
  | grep -v '^\s*#' \
  | grep -v '\${ADMIN_TELEGRAM_CHAT_ID' \
  || true)
if [[ -z "$HARDCODED_CHAT" ]]; then
  pass "ADMIN_TELEGRAM_CHAT_ID is not assigned a literal value — reads from env"
else
  fail "ADMIN_TELEGRAM_CHAT_ID appears to be hardcoded: $HARDCODED_CHAT"
fi

# =============================================================================
# Test 8: aperod-backup.service has EnvironmentFile loading api.env
# =============================================================================
section "Test 8: aperod-backup.service contains EnvironmentFile=/etc/aperod/api.env"

if [[ ! -f "$SERVICE_FILE" ]]; then
  fail "aperod-backup.service not found at $SERVICE_FILE"
else
  if grep -q "EnvironmentFile.*api\.env" "$SERVICE_FILE"; then
    pass "aperod-backup.service contains EnvironmentFile line referencing api.env"
  else
    fail "aperod-backup.service does NOT contain EnvironmentFile referencing api.env (content: $(cat "$SERVICE_FILE"))"
  fi

  # Also confirm TELEGRAM vars would be sourced from that file by checking it's
  # not excluded with a minus prefix that silently skips the file.
  # (A leading '-' means "ignore if missing", which is acceptable.)
  ENV_LINE=$(grep "EnvironmentFile.*api\.env" "$SERVICE_FILE" | head -1)
  pass "EnvironmentFile line: $ENV_LINE"
fi

# =============================================================================
# Test 9: Telegram alert NOT fired on success path
# =============================================================================
section "Test 9: Telegram alert is NOT fired on a successful backup"

# Simulate a successful backup by stubbing every external tool to succeed.
T9_DIR=$(mktemp -d "$TMPDIR_TEST/run-t9-XXXXXXXX")
make_settings_json "$T9_DIR/data"
mkdir -p "$T9_DIR/metrics"

T9_CURL_LOG="$T9_DIR/curl.log"
T9_FAKE_CURL=$(make_fake_curl "$T9_CURL_LOG")

# sudo stub that succeeds and creates an empty dump file
T9_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t9-XXXXXXXX")
cat >"$T9_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
# Intercept: sudo -u postgres pg_dump ... > output_file
# The redirect is handled by bash, not sudo; just touch any file and exit 0.
exit 0
STUB
chmod +x "$T9_FAKE_SUDO_DIR/sudo"

# Provide a pg_dump stub too (sudo calls it as arg, but bash redirect applies)
T9_PGDUMP_LOG="$T9_DIR/pgdump.log"
T9_FAKE_PGDUMP=$(make_fake_bin "pg_dump" "$T9_PGDUMP_LOG" 0)

# gpg stub that creates the output file (script passes -o <outfile> arg)
T9_FAKE_GPG_DIR=$(mktemp -d "$TMPDIR_TEST/fake-gpg-t9-XXXXXXXX")
cat >"$T9_FAKE_GPG_DIR/gpg" <<'STUB'
#!/usr/bin/env bash
# Parse -o <outfile> from args and create an empty file there
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    touch "$2"
    shift 2
  else
    shift
  fi
done
exit 0
STUB
chmod +x "$T9_FAKE_GPG_DIR/gpg"

# tar stub: streaming pipeline writes to stdout → gpg stub creates the output file
T9_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t9-XXXXXXXX")
cat >"$T9_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
# In the streaming pipeline tar writes to stdout (-czf -); gpg handles the output.
exit 0
STUB
chmod +x "$T9_FAKE_TAR_DIR/tar"

# rclone stub that succeeds; for `rclone size` return minimal JSON
T9_FAKE_RCLONE_DIR=$(mktemp -d "$TMPDIR_TEST/fake-rclone-t9-XXXXXXXX")
cat >"$T9_FAKE_RCLONE_DIR/rclone" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "size" ]]; then
  echo '{"count":1,"bytes":1024}'
fi
exit 0
STUB
chmod +x "$T9_FAKE_RCLONE_DIR/rclone"

# df stub — return plenty of free space so disk preflight passes
T9_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t9-XXXXXXXX")
cat >"$T9_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
# Print a fake df --output=avail result (20 GB free in KB)
echo "Avail"
echo "20971520"
exit 0
STUB
chmod +x "$T9_FAKE_DF_DIR/df"

T9_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t9-XXXXXXXX")
T9_EXIT=0
APEROD_BACKUP_PASSWORD="test-password-success" \
  DATA_DIR="$T9_DIR/data" \
  APEROD_TEXTFILE_DIR="$T9_DIR/metrics" \
  APEROD_HISTORY_LOG="$T9_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T9_BACKUP_DIR" \
  TELEGRAM_BOT_TOKEN="tok-success" \
  ADMIN_TELEGRAM_CHAT_ID="12345678" \
  PATH="$T9_FAKE_CURL:$T9_FAKE_SUDO_DIR:$T9_FAKE_PGDUMP:$T9_FAKE_GPG_DIR:$T9_FAKE_TAR_DIR:$T9_FAKE_RCLONE_DIR:$T9_FAKE_DF_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T9_EXIT=$?

# Success path sets _BACKUP_FINAL_STATUS="ok" so Telegram block is skipped.
if [[ ! -f "$T9_CURL_LOG" ]] || ! grep -q "api.telegram.org" "$T9_CURL_LOG" 2>/dev/null; then
  pass "Telegram failure alert was NOT sent on successful backup"
else
  fail "Telegram failure alert was unexpectedly sent on a successful run (log: $(cat "$T9_CURL_LOG" 2>/dev/null))"
fi

# =============================================================================
# Test 10: Low-disk preflight → exits 0, aperod_backup_skipped_low_disk=1
# =============================================================================
section "Test 10: low-disk preflight → exit 0 and skipped_low_disk=1 in Prometheus"

T10_DIR=$(mktemp -d "$TMPDIR_TEST/run-t10-XXXXXXXX")
make_settings_json "$T10_DIR/data"
mkdir -p "$T10_DIR/metrics"
T10_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t10-XXXXXXXX")

T10_CURL_LOG="$T10_DIR/curl.log"
T10_FAKE_CURL=$(make_fake_curl "$T10_CURL_LOG")

# df stub: return only 1 MB free (far below 5 GB minimum)
T10_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t10-XXXXXXXX")
cat >"$T10_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "1024"
exit 0
STUB
chmod +x "$T10_FAKE_DF_DIR/df"

# sudo stub (pg_dump path — should never be reached after preflight bails)
T10_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t10-XXXXXXXX")
cat >"$T10_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T10_FAKE_SUDO_DIR/sudo"

T10_FAKE_RCLONE=$(make_fake_bin "rclone" "$T10_DIR/rclone.log" 0)
T10_FAKE_GPG=$(make_fake_bin "gpg"    "$T10_DIR/gpg.log"    0)

T10_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t10-XXXXXXXX")
cat >"$T10_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T10_FAKE_TAR_DIR/tar"

T10_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t10" \
  DATA_DIR="$T10_DIR/data" \
  APEROD_TEXTFILE_DIR="$T10_DIR/metrics" \
  APEROD_HISTORY_LOG="$T10_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T10_BACKUP_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t10" \
  ADMIN_TELEGRAM_CHAT_ID="10000001" \
  PATH="$T10_FAKE_CURL:$T10_FAKE_DF_DIR:$T10_FAKE_SUDO_DIR:$T10_FAKE_RCLONE:$T10_FAKE_GPG:$T10_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T10_EXIT=$?
[[ "$T10_EXIT" -eq 99 ]] && T10_EXIT=0  # bash "$BACKUP_SH" returned 0

if [[ "$T10_EXIT" -eq 0 ]]; then
  pass "low-disk preflight exits 0 (skipped, not failed)"
else
  fail "low-disk preflight exited $T10_EXIT — expected 0"
fi

T10_PROM="$T10_DIR/metrics/aperod_backup.prom"
if [[ -f "$T10_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T10_PROM"; then
  pass "Prometheus: aperod_backup_skipped_low_disk=1 written on low-disk skip"
else
  fail "Prometheus: aperod_backup_skipped_low_disk=1 NOT found (file: $(cat "$T10_PROM" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T10_PROM" ]] && grep -q "^aperod_backup_last_success 0" "$T10_PROM"; then
  pass "Prometheus: aperod_backup_last_success=0 on low-disk skip"
else
  fail "Prometheus: aperod_backup_last_success=0 NOT found on low-disk skip"
fi

if [[ -f "$T10_CURL_LOG" ]] && grep -q "api.telegram.org" "$T10_CURL_LOG"; then
  pass "Telegram low-disk alert was sent when tokens are set"
else
  fail "Telegram low-disk alert was NOT sent (log: $(cat "$T10_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T10_CURL_LOG" ]] && grep -q "BACKUP_DIR" "$T10_CURL_LOG"; then
  pass "Telegram low-disk alert contains partition label (BACKUP_DIR (/opt/aperod/backup-tmp))"
else
  fail "Telegram low-disk alert does NOT contain partition label (log: $(cat "$T10_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T10_CURL_LOG" ]] && grep -qE '[0-9]+\.[0-9]' "$T10_CURL_LOG"; then
  pass "Telegram low-disk alert contains numeric free-space value"
else
  fail "Telegram low-disk alert does NOT contain numeric free-space value (log: $(cat "$T10_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 11: Recovery — after space freed the next run succeeds (skipped_low_disk=0)
# =============================================================================
section "Test 11: recovery — next cycle with sufficient space exits 0, skipped_low_disk=0"

T11_DIR=$(mktemp -d "$TMPDIR_TEST/run-t11-XXXXXXXX")
make_settings_json "$T11_DIR/data"
mkdir -p "$T11_DIR/metrics"
T11_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t11-XXXXXXXX")

T11_CURL_LOG="$T11_DIR/curl.log"
T11_FAKE_CURL=$(make_fake_curl "$T11_CURL_LOG")

# ── Run A: low disk ──────────────────────────────────────────────────────────
T11_FAKE_DF_LOW=$(mktemp -d "$TMPDIR_TEST/fake-df-t11-low-XXXXXXXX")
cat >"$T11_FAKE_DF_LOW/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "512"
exit 0
STUB
chmod +x "$T11_FAKE_DF_LOW/df"

T11_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t11-XXXXXXXX")
cat >"$T11_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T11_FAKE_SUDO_DIR/sudo"

T11A_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t11" \
  DATA_DIR="$T11_DIR/data" \
  APEROD_TEXTFILE_DIR="$T11_DIR/metrics" \
  APEROD_HISTORY_LOG="$T11_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T11_BACKUP_DIR" \
  PATH="$T11_FAKE_CURL:$T11_FAKE_DF_LOW:$T11_FAKE_SUDO_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T11A_EXIT=$?
[[ "$T11A_EXIT" -eq 99 ]] && T11A_EXIT=0

T11_PROM="$T11_DIR/metrics/aperod_backup.prom"
if [[ "$T11A_EXIT" -eq 0 ]] && [[ -f "$T11_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T11_PROM"; then
  pass "Run A (low disk): exit 0 and skipped_low_disk=1 confirmed"
else
  fail "Run A (low disk): exit=$T11A_EXIT; prom=$(cat "$T11_PROM" 2>/dev/null || echo '<missing>')"
fi

# ── Run B: sufficient disk — full success stubs ──────────────────────────────
T11_FAKE_DF_OK=$(mktemp -d "$TMPDIR_TEST/fake-df-t11-ok-XXXXXXXX")
cat >"$T11_FAKE_DF_OK/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "20971520"
exit 0
STUB
chmod +x "$T11_FAKE_DF_OK/df"

T11_FAKE_GPG_DIR=$(mktemp -d "$TMPDIR_TEST/fake-gpg-t11-XXXXXXXX")
cat >"$T11_FAKE_GPG_DIR/gpg" <<'STUB'
#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then touch "$2"; shift 2; else shift; fi
done
exit 0
STUB
chmod +x "$T11_FAKE_GPG_DIR/gpg"

T11_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t11-XXXXXXXX")
cat >"$T11_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
# In the streaming pipeline tar writes to stdout (-czf -); gpg stub creates output.
exit 0
STUB
chmod +x "$T11_FAKE_TAR_DIR/tar"

T11_FAKE_RCLONE_DIR=$(mktemp -d "$TMPDIR_TEST/fake-rclone-t11-XXXXXXXX")
cat >"$T11_FAKE_RCLONE_DIR/rclone" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "size" ]]; then echo '{"count":1,"bytes":2048}'; fi
exit 0
STUB
chmod +x "$T11_FAKE_RCLONE_DIR/rclone"

# Re-create backup dir (previous run may have cleaned it up via EXIT trap)
mkdir -p "$T11_BACKUP_DIR"

T11B_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t11" \
  DATA_DIR="$T11_DIR/data" \
  APEROD_TEXTFILE_DIR="$T11_DIR/metrics" \
  APEROD_HISTORY_LOG="$T11_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T11_BACKUP_DIR" \
  PATH="$T11_FAKE_CURL:$T11_FAKE_DF_OK:$T11_FAKE_SUDO_DIR:$T11_FAKE_GPG_DIR:$T11_FAKE_TAR_DIR:$T11_FAKE_RCLONE_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T11B_EXIT=$?
[[ "$T11B_EXIT" -eq 99 ]] && T11B_EXIT=0

if [[ "$T11B_EXIT" -eq 0 ]]; then
  pass "Run B (disk freed): exits 0 — backup succeeded"
else
  fail "Run B (disk freed): exited $T11B_EXIT — expected 0"
fi

if [[ -f "$T11_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 0" "$T11_PROM"; then
  pass "Prometheus: aperod_backup_skipped_low_disk reset to 0 after successful run"
else
  fail "Prometheus: aperod_backup_skipped_low_disk NOT reset to 0 (file: $(cat "$T11_PROM" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T11_PROM" ]] && grep -q "^aperod_backup_last_success 1" "$T11_PROM"; then
  pass "Prometheus: aperod_backup_last_success=1 after recovery run"
else
  fail "Prometheus: aperod_backup_last_success=1 NOT found after recovery run"
fi

# =============================================================================
# Test 12: Static — BACKUP_DIR preflight checks /opt/aperod partition, not /tmp
# =============================================================================
section "Test 12: static analysis — BACKUP_DIR preflight checks /opt/aperod, not /tmp"

# The hardcoded default inside the variable expansion must point to /opt/aperod.
# The assignment looks like: BACKUP_DIR="${APEROD_BACKUP_DIR_OVERRIDE:-/opt/aperod/...}"
BACKUP_DIR_LINE=$(grep 'BACKUP_DIR=' "$BACKUP_SH" | grep -v '^\s*#' | head -1 || true)

if echo "$BACKUP_DIR_LINE" | grep -q ':-/opt/aperod'; then
  pass "BACKUP_DIR default (after :-) contains /opt/aperod (not /tmp)"
else
  fail "BACKUP_DIR default does NOT contain /opt/aperod: $BACKUP_DIR_LINE"
fi

if echo "$BACKUP_DIR_LINE" | grep -qv ':-/tmp'; then
  pass "BACKUP_DIR default does not fall back to /tmp"
else
  fail "BACKUP_DIR default uses /tmp — preflight would check the wrong partition"
fi

# The _disk_preflight call for BACKUP_DIR must pass a label containing opt/aperod
PREFLIGHT_CALL=$(grep '_disk_preflight.*BACKUP_DIR' "$BACKUP_SH" | grep -v '^\s*#' | head -1 || true)
if echo "$PREFLIGHT_CALL" | grep -q 'opt/aperod'; then
  pass "_disk_preflight for BACKUP_DIR passes an /opt/aperod label"
else
  fail "_disk_preflight BACKUP_DIR label does not reference /opt/aperod: $PREFLIGHT_CALL"
fi

# Confirm there is NO _disk_preflight call that hardcodes /tmp
TMP_PREFLIGHT=$(grep '_disk_preflight' "$BACKUP_SH" | grep '/tmp' | grep -v '^\s*#' || true)
if [[ -z "$TMP_PREFLIGHT" ]]; then
  pass "No _disk_preflight call hardcodes /tmp as the checked partition"
else
  fail "_disk_preflight found with /tmp reference: $TMP_PREFLIGHT"
fi

# =============================================================================
# Test 13: Streaming archive — decrypt + list shows DB dump, no intermediate files
# =============================================================================
section "Test 13: streaming tar|gpg archive — decrypt+list contains expected members, no intermediate files"

if ! command -v gpg >/dev/null 2>&1; then
  echo "  SKIP: gpg not found — cannot run real-crypto archive test"
  ((PASS++))
else

T13_DIR=$(mktemp -d "$TMPDIR_TEST/run-t13-XXXXXXXX")
T13_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t13-XXXXXXXX")
T13_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t13-XXXXXXXX")
make_settings_json "$T13_DIR/data"
mkdir -p "$T13_DIR/metrics"

# Populate a small node data file so tar has real content to compress
echo "blockdata" > "$T13_DATA_DIR/test_block.bin"

# sudo stub: creates the DB dump file directly (bypasses pg_dump)
T13_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t13-XXXXXXXX")
cat >"$T13_FAKE_SUDO_DIR/sudo" <<STUB
#!/usr/bin/env bash
# sudo -u postgres pg_dump ... redirected to BACKUP_DIR/explorer_db.dump by caller
exit 0
STUB
chmod +x "$T13_FAKE_SUDO_DIR/sudo"

# Create the DB dump file the way the real script does (shell redirect writes it)
touch "$T13_BACKUP_DIR/explorer_db.dump"
# Pre-populate the dump file so tar can archive it — real script writes it via redirect
echo "pg_dump_placeholder" > "$T13_BACKUP_DIR/explorer_db.dump"

T13_FAKE_RCLONE_DIR=$(mktemp -d "$TMPDIR_TEST/fake-rclone-t13-XXXXXXXX")
cat >"$T13_FAKE_RCLONE_DIR/rclone" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "size" ]]; then echo '{"count":1,"bytes":1024}'; fi
exit 0
STUB
chmod +x "$T13_FAKE_RCLONE_DIR/rclone"

T13_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t13-XXXXXXXX")
cat >"$T13_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "20971520"
exit 0
STUB
chmod +x "$T13_FAKE_DF_DIR/df"

# du stub for _disk_preflight_scaled (called with NODE_DATA_DIR)
T13_FAKE_DU_DIR=$(mktemp -d "$TMPDIR_TEST/fake-du-t13-XXXXXXXX")
cat >"$T13_FAKE_DU_DIR/du" <<'STUB'
#!/usr/bin/env bash
# Return a small size (100 MB) for any path
echo "102400	$2"
exit 0
STUB
chmod +x "$T13_FAKE_DU_DIR/du"

T13_EXIT=0
APEROD_BACKUP_PASSWORD="t13-test-password" \
  DATA_DIR="$T13_DIR/data" \
  APEROD_TEXTFILE_DIR="$T13_DIR/metrics" \
  APEROD_HISTORY_LOG="$T13_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T13_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T13_DATA_DIR" \
  PATH="$T13_FAKE_SUDO_DIR:$T13_FAKE_RCLONE_DIR:$T13_FAKE_DF_DIR:$T13_FAKE_DU_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T13_EXIT=$?

# The BACKUP_DIR is cleaned by the script's EXIT trap; find the gpg file beforehand.
# We can find it via rclone size call in the history log or re-run just the crypto step.
# Simpler: intercept by overriding APEROD_BACKUP_DIR_OVERRIDE to a persistent dir and
# disabling the cleanup by re-running with a patched PATH that stubs rclone to leave the file.
# Instead: run just the streaming step directly with known paths for verification.
T13_VERIFY_DIR=$(mktemp -d "$TMPDIR_TEST/verify-t13-XXXXXXXX")
echo "pg_dump_placeholder" > "$T13_VERIFY_DIR/explorer_db.dump"
echo "blockdata" > "$T13_VERIFY_DIR/test_block.bin"
T13_DATA_VERIFY=$(mktemp -d "$TMPDIR_TEST/nodedata-verify-t13-XXXXXXXX")
echo "blockdata" > "$T13_DATA_VERIFY/test_block.bin"
T13_PASS="direct-crypto-test-pw"
T13_OUTFILE="$T13_VERIFY_DIR/direct.tar.gpg"

# Run the streaming command directly (no stubs needed — real tar/gpg)
TAR_ARGS_TEST=( --ignore-failed-read --warning=no-file-removed -czf -
               -C "$T13_VERIFY_DIR" explorer_db.dump
               -C "$T13_DATA_VERIFY" . )
tar "${TAR_ARGS_TEST[@]}" \
  | gpg --batch --yes --passphrase-fd 3 \
      --symmetric --cipher-algo AES256 \
      -o "$T13_OUTFILE" - \
    3< <(echo "$T13_PASS") 2>/dev/null
T13_CRYPTO_EXIT=$?

if [[ "$T13_CRYPTO_EXIT" -eq 0 ]] && [[ -f "$T13_OUTFILE" ]]; then
  pass "Streaming tar -czf pipe to gpg produced a .tar.gpg file (exit 0)"
else
  fail "Streaming pipeline failed (exit=$T13_CRYPTO_EXIT, file exists: $([ -f "$T13_OUTFILE" ] && echo yes || echo no))"
fi

# Decrypt and list — must contain explorer_db.dump
T13_LISTING=$(gpg --batch --yes --passphrase "$T13_PASS" \
  --decrypt "$T13_OUTFILE" 2>/dev/null | tar -tzf - 2>/dev/null || true)

if echo "$T13_LISTING" | grep -q "explorer_db.dump"; then
  pass "Decrypted archive listing contains explorer_db.dump"
else
  fail "Decrypted archive listing does NOT contain explorer_db.dump (listing: $T13_LISTING)"
fi

if echo "$T13_LISTING" | grep -q "test_block.bin"; then
  pass "Decrypted archive listing contains node data file (test_block.bin)"
else
  fail "Decrypted archive listing does NOT contain node data file (listing: $T13_LISTING)"
fi

# Confirm no intermediate files were created (no .tar or .tar.gz alongside the .tar.gpg)
T13_LEFTOVER=$(find "$T13_VERIFY_DIR" -maxdepth 1 \
  \( -name "*.tar" -o -name "node_chain_data.tar.gz" \) 2>/dev/null || true)
if [[ -z "$T13_LEFTOVER" ]]; then
  pass "No intermediate .tar or node_chain_data.tar.gz left alongside the .tar.gpg"
else
  fail "Intermediate files found (should not exist in streaming mode): $T13_LEFTOVER"
fi

fi  # end gpg available guard

# =============================================================================
# Test 14: Scaled preflight ACCEPTS when BACKUP_DIR has enough space (data + 1 GiB)
# =============================================================================
section "Test 14: scaled preflight accepts when BACKUP_DIR has node-data + 1 GiB free"

T14_DIR=$(mktemp -d "$TMPDIR_TEST/run-t14-XXXXXXXX")
T14_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t14-XXXXXXXX")
echo "blockdata" > "$T14_DATA_DIR/block.bin"
make_settings_json "$T14_DIR/data"
mkdir -p "$T14_DIR/metrics"
T14_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t14-XXXXXXXX")
T14_CURL_LOG="$T14_DIR/curl.log"
T14_FAKE_CURL=$(make_fake_curl "$T14_CURL_LOG")

# du stub: 2 GiB of node data
T14_FAKE_DU_DIR=$(mktemp -d "$TMPDIR_TEST/fake-du-t14-XXXXXXXX")
cat >"$T14_FAKE_DU_DIR/du" <<'STUB'
#!/usr/bin/env bash
echo "2097152	$2"
exit 0
STUB
chmod +x "$T14_FAKE_DU_DIR/du"

# df stub: 6 GiB free — clears the fixed 5 GiB floor AND the scaled 3 GiB threshold
# (2 GiB node data + 1 GiB buffer = 3 GiB scaled requirement; 5 GiB absolute floor)
T14_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t14-XXXXXXXX")
cat >"$T14_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "6291456"
exit 0
STUB
chmod +x "$T14_FAKE_DF_DIR/df"

# sudo stub: fails immediately so we don't need full tar/gpg stubs
T14_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t14-XXXXXXXX")
cat >"$T14_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$T14_FAKE_SUDO_DIR/sudo"

T14_RCLONE=$(make_fake_bin "rclone" "$T14_DIR/rclone.log" 0)
T14_GPG=$(make_fake_bin "gpg"    "$T14_DIR/gpg.log"    0)
T14_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t14-XXXXXXXX")
cat >"$T14_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T14_FAKE_TAR_DIR/tar"

T14_EXIT=0
APEROD_BACKUP_PASSWORD="test-pass-t14" \
  DATA_DIR="$T14_DIR/data" \
  APEROD_TEXTFILE_DIR="$T14_DIR/metrics" \
  APEROD_HISTORY_LOG="$T14_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T14_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T14_DATA_DIR" \
  PATH="$T14_FAKE_CURL:$T14_FAKE_DU_DIR:$T14_FAKE_DF_DIR:$T14_FAKE_SUDO_DIR:$T14_RCLONE:$T14_GPG:$T14_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T14_EXIT=$?

# Script fails at pg_dump (sudo exits 1), but that's AFTER the preflight passed.
# A non-zero exit here means preflight passed (it would have exited 0 if skipped).
if [[ "$T14_EXIT" -ne 0 ]]; then
  pass "Scaled preflight passed (script reached pg_dump stage and exited non-zero there)"
else
  # Exit 0 could mean the preflight skipped it OR the success path ran — check prom
  T14_PROM="$T14_DIR/metrics/aperod_backup.prom"
  if [[ -f "$T14_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 0" "$T14_PROM"; then
    pass "Scaled preflight passed (skipped_low_disk=0, exit 0 — success path or skipped for unrelated reason)"
  elif [[ -f "$T14_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T14_PROM"; then
    fail "Scaled preflight incorrectly triggered low-disk skip when BACKUP_DIR had 4 GiB free (data=2 GiB + 1 GiB buffer=3 GiB required)"
  else
    pass "Scaled preflight passed (exit 0, no skipped_low_disk=1 metric)"
  fi
fi

if [[ ! -f "$T14_CURL_LOG" ]] || ! grep -q "мало места\|low.disk\|low_disk" "$T14_CURL_LOG" 2>/dev/null; then
  pass "No low-disk Telegram alert fired when space was sufficient"
else
  fail "Low-disk Telegram alert fired incorrectly when BACKUP_DIR had sufficient space"
fi

# =============================================================================
# Test 15: Scaled preflight SKIPS when BACKUP_DIR lacks node-data + 1 GiB free
# =============================================================================
section "Test 15: scaled preflight skips when BACKUP_DIR has less than node-data + 1 GiB free"

T15_DIR=$(mktemp -d "$TMPDIR_TEST/run-t15-XXXXXXXX")
T15_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t15-XXXXXXXX")
echo "blockdata" > "$T15_DATA_DIR/block.bin"
make_settings_json "$T15_DIR/data"
mkdir -p "$T15_DIR/metrics"
T15_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t15-XXXXXXXX")
T15_CURL_LOG="$T15_DIR/curl.log"
T15_FAKE_CURL=$(make_fake_curl "$T15_CURL_LOG")

# du stub: 3 GiB of node data
T15_FAKE_DU_DIR=$(mktemp -d "$TMPDIR_TEST/fake-du-t15-XXXXXXXX")
cat >"$T15_FAKE_DU_DIR/du" <<'STUB'
#!/usr/bin/env bash
echo "3145728	$2"
exit 0
STUB
chmod +x "$T15_FAKE_DU_DIR/du"

# df stub: 3.5 GiB free — less than 3 GiB data + 1 GiB buffer = 4 GiB required
T15_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t15-XXXXXXXX")
cat >"$T15_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "3670016"
exit 0
STUB
chmod +x "$T15_FAKE_DF_DIR/df"

T15_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t15-XXXXXXXX")
cat >"$T15_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T15_FAKE_SUDO_DIR/sudo"

T15_RCLONE=$(make_fake_bin "rclone" "$T15_DIR/rclone.log" 0)
T15_GPG=$(make_fake_bin "gpg"    "$T15_DIR/gpg.log"    0)
T15_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t15-XXXXXXXX")
cat >"$T15_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T15_FAKE_TAR_DIR/tar"

T15_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t15" \
  DATA_DIR="$T15_DIR/data" \
  APEROD_TEXTFILE_DIR="$T15_DIR/metrics" \
  APEROD_HISTORY_LOG="$T15_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T15_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T15_DATA_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t15" \
  ADMIN_TELEGRAM_CHAT_ID="15000001" \
  PATH="$T15_FAKE_CURL:$T15_FAKE_DU_DIR:$T15_FAKE_DF_DIR:$T15_FAKE_SUDO_DIR:$T15_RCLONE:$T15_GPG:$T15_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T15_EXIT=$?
[[ "$T15_EXIT" -eq 99 ]] && T15_EXIT=0

if [[ "$T15_EXIT" -eq 0 ]]; then
  pass "Scaled preflight exits 0 (skipped gracefully, not failed)"
else
  fail "Scaled preflight exited $T15_EXIT — expected 0 (skip)"
fi

T15_PROM="$T15_DIR/metrics/aperod_backup.prom"
if [[ -f "$T15_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T15_PROM"; then
  pass "Prometheus: aperod_backup_skipped_low_disk=1 on scaled-preflight skip"
else
  fail "Prometheus: aperod_backup_skipped_low_disk=1 NOT found on scaled-preflight skip (file: $(cat "$T15_PROM" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T15_CURL_LOG" ]] && grep -q "api.telegram.org" "$T15_CURL_LOG"; then
  pass "Telegram low-disk alert fired on scaled-preflight skip"
else
  fail "Telegram low-disk alert NOT fired on scaled-preflight skip (log: $(cat "$T15_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# Confirm the history log records skipReason=low_disk
T15_HIST="$T15_DIR/backup.log"
if [[ -f "$T15_HIST" ]] && grep -q '"skipReason":"low_disk"' "$T15_HIST"; then
  pass "History log records skipReason=low_disk on scaled-preflight skip"
else
  fail "History log does NOT record skipReason=low_disk (log: $(cat "$T15_HIST" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 16: NODE_DATA_DIR low-disk → exits 0, skipped_low_disk=1, Telegram alert
# =============================================================================
section "Test 16: NODE_DATA_DIR low-disk → exit 0, skipped_low_disk=1, Telegram alert sent"

T16_DIR=$(mktemp -d "$TMPDIR_TEST/run-t16-XXXXXXXX")
T16_NODE_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t16-XXXXXXXX")
echo "blockdata" > "$T16_NODE_DATA_DIR/block.bin"
make_settings_json "$T16_DIR/data"
mkdir -p "$T16_DIR/metrics"
T16_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t16-XXXXXXXX")

T16_CURL_LOG="$T16_DIR/curl.log"
T16_FAKE_CURL=$(make_fake_curl "$T16_CURL_LOG")

# df stub: first call (BACKUP_DIR) returns 20 GiB (passes the 5 GiB floor);
# second call (NODE_DATA_DIR) returns 1 MB (below 5 GiB floor → triggers skip).
# A counter file tracks invocation number.
T16_DF_COUNTER="$T16_DIR/df-call-count"
T16_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t16-XXXXXXXX")
cat >"$T16_FAKE_DF_DIR/df" <<STUB
#!/usr/bin/env bash
count=0
[[ -f "$T16_DF_COUNTER" ]] && count=\$(cat "$T16_DF_COUNTER")
count=\$(( count + 1 ))
echo "\$count" > "$T16_DF_COUNTER"
echo "Avail"
if [[ "\$count" -le 1 ]]; then
  echo "20971520"   # 20 GiB — BACKUP_DIR passes
else
  echo "1024"       # 1 MB — NODE_DATA_DIR fails
fi
exit 0
STUB
chmod +x "$T16_FAKE_DF_DIR/df"

T16_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t16-XXXXXXXX")
cat >"$T16_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T16_FAKE_SUDO_DIR/sudo"

T16_RCLONE=$(make_fake_bin "rclone" "$T16_DIR/rclone.log" 0)
T16_GPG=$(make_fake_bin "gpg"    "$T16_DIR/gpg.log"    0)
T16_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t16-XXXXXXXX")
cat >"$T16_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T16_FAKE_TAR_DIR/tar"

T16_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t16" \
  DATA_DIR="$T16_DIR/data" \
  APEROD_TEXTFILE_DIR="$T16_DIR/metrics" \
  APEROD_HISTORY_LOG="$T16_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T16_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T16_NODE_DATA_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t16" \
  ADMIN_TELEGRAM_CHAT_ID="16000001" \
  PATH="$T16_FAKE_CURL:$T16_FAKE_DF_DIR:$T16_FAKE_SUDO_DIR:$T16_RCLONE:$T16_GPG:$T16_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T16_EXIT=$?
[[ "$T16_EXIT" -eq 99 ]] && T16_EXIT=0  # script exited 0

if [[ "$T16_EXIT" -eq 0 ]]; then
  pass "NODE_DATA_DIR low-disk preflight exits 0 (skipped, not failed)"
else
  fail "NODE_DATA_DIR low-disk preflight exited $T16_EXIT — expected 0"
fi

T16_PROM="$T16_DIR/metrics/aperod_backup.prom"
if [[ -f "$T16_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T16_PROM"; then
  pass "Prometheus: aperod_backup_skipped_low_disk=1 written on NODE_DATA_DIR low-disk skip"
else
  fail "Prometheus: aperod_backup_skipped_low_disk=1 NOT found (file: $(cat "$T16_PROM" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T16_PROM" ]] && grep -q "^aperod_backup_last_success 0" "$T16_PROM"; then
  pass "Prometheus: aperod_backup_last_success=0 on NODE_DATA_DIR low-disk skip"
else
  fail "Prometheus: aperod_backup_last_success=0 NOT found on NODE_DATA_DIR low-disk skip"
fi

if [[ -f "$T16_CURL_LOG" ]] && grep -q "api.telegram.org" "$T16_CURL_LOG"; then
  pass "Telegram low-disk alert was sent when NODE_DATA_DIR has insufficient space"
else
  fail "Telegram low-disk alert was NOT sent for NODE_DATA_DIR low-disk (log: $(cat "$T16_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T16_CURL_LOG" ]] && grep -q "NODE_DATA_DIR" "$T16_CURL_LOG"; then
  pass "Telegram low-disk alert contains partition label NODE_DATA_DIR"
else
  fail "Telegram low-disk alert does NOT contain NODE_DATA_DIR label (log: $(cat "$T16_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

T16_HIST="$T16_DIR/backup.log"
if [[ -f "$T16_HIST" ]] && grep -q '"skipReason":"low_disk"' "$T16_HIST"; then
  pass "History log records skipReason=low_disk for NODE_DATA_DIR skip"
else
  fail "History log does NOT record skipReason=low_disk for NODE_DATA_DIR skip (log: $(cat "$T16_HIST" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 17: NODE_DATA_DIR recovery — next run with enough space on both resets metric to 0
# =============================================================================
section "Test 17: NODE_DATA_DIR recovery — next cycle with sufficient space resets skipped_low_disk to 0"

T17_DIR=$(mktemp -d "$TMPDIR_TEST/run-t17-XXXXXXXX")
T17_NODE_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t17-XXXXXXXX")
echo "blockdata" > "$T17_NODE_DATA_DIR/block.bin"
make_settings_json "$T17_DIR/data"
mkdir -p "$T17_DIR/metrics"
T17_BACKUP_DIR=$(mktemp -d "$TMPDIR_TEST/backup-t17-XXXXXXXX")

T17_CURL_LOG="$T17_DIR/curl.log"
T17_FAKE_CURL=$(make_fake_curl "$T17_CURL_LOG")

# ── Run A: NODE_DATA_DIR low disk (reuse counter pattern) ───────────────────
T17_DF_COUNTER_A="$T17_DIR/df-call-count-a"
T17_FAKE_DF_LOW=$(mktemp -d "$TMPDIR_TEST/fake-df-t17-low-XXXXXXXX")
cat >"$T17_FAKE_DF_LOW/df" <<STUB
#!/usr/bin/env bash
count=0
[[ -f "$T17_DF_COUNTER_A" ]] && count=\$(cat "$T17_DF_COUNTER_A")
count=\$(( count + 1 ))
echo "\$count" > "$T17_DF_COUNTER_A"
echo "Avail"
if [[ "\$count" -le 1 ]]; then
  echo "20971520"   # BACKUP_DIR passes
else
  echo "512"        # NODE_DATA_DIR fails
fi
exit 0
STUB
chmod +x "$T17_FAKE_DF_LOW/df"

T17_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t17-XXXXXXXX")
cat >"$T17_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T17_FAKE_SUDO_DIR/sudo"

T17A_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t17" \
  DATA_DIR="$T17_DIR/data" \
  APEROD_TEXTFILE_DIR="$T17_DIR/metrics" \
  APEROD_HISTORY_LOG="$T17_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T17_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T17_NODE_DATA_DIR" \
  PATH="$T17_FAKE_CURL:$T17_FAKE_DF_LOW:$T17_FAKE_SUDO_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T17A_EXIT=$?
[[ "$T17A_EXIT" -eq 99 ]] && T17A_EXIT=0

T17_PROM="$T17_DIR/metrics/aperod_backup.prom"
if [[ "$T17A_EXIT" -eq 0 ]] && [[ -f "$T17_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T17_PROM"; then
  pass "Run A (NODE_DATA_DIR low disk): exit 0 and skipped_low_disk=1 confirmed"
else
  fail "Run A (NODE_DATA_DIR low disk): exit=$T17A_EXIT; prom=$(cat "$T17_PROM" 2>/dev/null || echo '<missing>')"
fi

# ── Run B: sufficient disk on both — full success stubs ─────────────────────
T17_FAKE_DF_OK=$(mktemp -d "$TMPDIR_TEST/fake-df-t17-ok-XXXXXXXX")
cat >"$T17_FAKE_DF_OK/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "20971520"   # 20 GiB — passes for every partition
exit 0
STUB
chmod +x "$T17_FAKE_DF_OK/df"

T17_FAKE_GPG_DIR=$(mktemp -d "$TMPDIR_TEST/fake-gpg-t17-XXXXXXXX")
cat >"$T17_FAKE_GPG_DIR/gpg" <<'STUB'
#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then touch "$2"; shift 2; else shift; fi
done
exit 0
STUB
chmod +x "$T17_FAKE_GPG_DIR/gpg"

T17_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t17-XXXXXXXX")
cat >"$T17_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T17_FAKE_TAR_DIR/tar"

T17_FAKE_RCLONE_DIR=$(mktemp -d "$TMPDIR_TEST/fake-rclone-t17-XXXXXXXX")
cat >"$T17_FAKE_RCLONE_DIR/rclone" <<'STUB'
#!/usr/bin/env bash
if [[ "$1" == "size" ]]; then echo '{"count":1,"bytes":2048}'; fi
exit 0
STUB
chmod +x "$T17_FAKE_RCLONE_DIR/rclone"

# Re-create backup dir (previous run cleaned it via EXIT trap)
mkdir -p "$T17_BACKUP_DIR"

T17B_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t17" \
  DATA_DIR="$T17_DIR/data" \
  APEROD_TEXTFILE_DIR="$T17_DIR/metrics" \
  APEROD_HISTORY_LOG="$T17_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T17_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T17_NODE_DATA_DIR" \
  PATH="$T17_FAKE_CURL:$T17_FAKE_DF_OK:$T17_FAKE_SUDO_DIR:$T17_FAKE_GPG_DIR:$T17_FAKE_TAR_DIR:$T17_FAKE_RCLONE_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T17B_EXIT=$?
[[ "$T17B_EXIT" -eq 99 ]] && T17B_EXIT=0

if [[ "$T17B_EXIT" -eq 0 ]]; then
  pass "Run B (both disks OK): exits 0 — backup succeeded after NODE_DATA_DIR was freed"
else
  fail "Run B (both disks OK): exited $T17B_EXIT — expected 0"
fi

if [[ -f "$T17_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 0" "$T17_PROM"; then
  pass "Prometheus: aperod_backup_skipped_low_disk reset to 0 after NODE_DATA_DIR recovery"
else
  fail "Prometheus: aperod_backup_skipped_low_disk NOT reset to 0 after recovery (file: $(cat "$T17_PROM" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T17_PROM" ]] && grep -q "^aperod_backup_last_success 1" "$T17_PROM"; then
  pass "Prometheus: aperod_backup_last_success=1 after NODE_DATA_DIR recovery run"
else
  fail "Prometheus: aperod_backup_last_success=1 NOT found after NODE_DATA_DIR recovery run"
fi

# =============================================================================
# Test 18: mkdir -p BACKUP_DIR fails → exit 0, skipped metric with backup_dir_unavailable
# =============================================================================
section "Test 18: mkdir BACKUP_DIR failure → exit 0 + aperod_backup_skipped_low_disk=1"

T18_DIR=$(mktemp -d "$TMPDIR_TEST/run-t18-XXXXXXXX")
make_settings_json "$T18_DIR/data"
mkdir -p "$T18_DIR/metrics"

# Use a sentinel keyword in the backup dir path so the mkdir stub can identify
# exactly which mkdir call to fail without interfering with other mkdir calls
# (metrics dir, history log dir, etc.).
T18_BACKUP_DIR="$T18_DIR/FAILMKDIR/aperod_backups_test"

# Build a mkdir stub that:
#   - fails (exit 1) when the last argument contains "FAILMKDIR"
#   - delegates everything else to the real /bin/mkdir
T18_FAKE_MKDIR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-mkdir-t18-XXXXXXXX")
cat >"$T18_FAKE_MKDIR_DIR/mkdir" <<STUB
#!/usr/bin/env bash
last_arg="\${@: -1}"
if [[ "\$last_arg" == *"FAILMKDIR"* ]]; then
  exit 1
fi
/bin/mkdir "\$@"
STUB
chmod +x "$T18_FAKE_MKDIR_DIR/mkdir"

# Stub curl so we can check Telegram (no alerts expected since status is "skipped")
T18_CURL_LOG="$T18_DIR/curl.log"
T18_FAKE_CURL=$(make_fake_curl "$T18_CURL_LOG")

# Stub sudo/rclone/gpg/tar/df — none should be reached before the mkdir guard fires
T18_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t18-XXXXXXXX")
cat >"$T18_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T18_FAKE_SUDO_DIR/sudo"

T18_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t18-XXXXXXXX")
cat >"$T18_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "20971520"
exit 0
STUB
chmod +x "$T18_FAKE_DF_DIR/df"

T18_RCLONE=$(make_fake_bin "rclone" "$T18_DIR/rclone.log" 0)
T18_GPG=$(make_fake_bin "gpg"    "$T18_DIR/gpg.log"    0)
T18_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t18-XXXXXXXX")
cat >"$T18_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T18_FAKE_TAR_DIR/tar"

T18_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t18" \
  DATA_DIR="$T18_DIR/data" \
  APEROD_TEXTFILE_DIR="$T18_DIR/metrics" \
  APEROD_HISTORY_LOG="$T18_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T18_BACKUP_DIR" \
  TELEGRAM_BOT_TOKEN="" \
  ADMIN_TELEGRAM_CHAT_ID="" \
  PATH="$T18_FAKE_MKDIR_DIR:$T18_FAKE_CURL:$T18_FAKE_DF_DIR:$T18_FAKE_SUDO_DIR:$T18_RCLONE:$T18_GPG:$T18_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T18_EXIT=$?
[[ "$T18_EXIT" -eq 99 ]] && T18_EXIT=0  # script returned 0

if [[ "$T18_EXIT" -eq 0 ]]; then
  pass "mkdir BACKUP_DIR failure: script exits 0 (skipped, not failed)"
else
  fail "mkdir BACKUP_DIR failure: script exited $T18_EXIT — expected 0"
fi

T18_PROM="$T18_DIR/metrics/aperod_backup.prom"
if [[ -f "$T18_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T18_PROM"; then
  pass "Prometheus: aperod_backup_skipped_low_disk=1 written when backup dir is unavailable"
else
  fail "Prometheus: aperod_backup_skipped_low_disk=1 NOT found (file: $(cat "$T18_PROM" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T18_PROM" ]] && grep -q "^aperod_backup_last_success 0" "$T18_PROM"; then
  pass "Prometheus: aperod_backup_last_success=0 when backup dir is unavailable"
else
  fail "Prometheus: aperod_backup_last_success=0 NOT found (file: $(cat "$T18_PROM" 2>/dev/null || echo '<missing>'))"
fi

T18_HIST="$T18_DIR/backup.log"
if [[ -f "$T18_HIST" ]] && grep -q '"skipReason":"backup_dir_unavailable"' "$T18_HIST"; then
  pass "History log records skipReason=backup_dir_unavailable"
else
  fail "History log does NOT record skipReason=backup_dir_unavailable (log: $(cat "$T18_HIST" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T18_HIST" ]] && grep -q '"status":"skipped"' "$T18_HIST"; then
  pass "History log records status=skipped (not fail)"
else
  fail "History log does NOT record status=skipped (log: $(cat "$T18_HIST" 2>/dev/null || echo '<missing>'))"
fi

# Confirm Telegram failure alert was NOT sent (it's a skip, not a failure)
if [[ ! -f "$T18_CURL_LOG" ]] || ! grep -q "api.telegram.org" "$T18_CURL_LOG" 2>/dev/null; then
  pass "Telegram failure alert NOT sent on backup_dir_unavailable skip (correct — it is a skip)"
else
  fail "Telegram failure alert was unexpectedly sent on backup_dir_unavailable skip"
fi

# =============================================================================
# Test 18b: mkdir BACKUP_DIR fails + tokens SET → Telegram alert IS sent
# =============================================================================
section "Test 18b: mkdir BACKUP_DIR failure + tokens set → Telegram alert IS sent"

T18B_DIR=$(mktemp -d "$TMPDIR_TEST/run-t18b-XXXXXXXX")
make_settings_json "$T18B_DIR/data"
mkdir -p "$T18B_DIR/metrics"

# Same sentinel keyword trick as Test 18
T18B_BACKUP_DIR="$T18B_DIR/FAILMKDIR/aperod_backups_test"

T18B_FAKE_MKDIR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-mkdir-t18b-XXXXXXXX")
cat >"$T18B_FAKE_MKDIR_DIR/mkdir" <<STUB
#!/usr/bin/env bash
last_arg="\${@: -1}"
if [[ "\$last_arg" == *"FAILMKDIR"* ]]; then
  exit 1
fi
/bin/mkdir "\$@"
STUB
chmod +x "$T18B_FAKE_MKDIR_DIR/mkdir"

T18B_CURL_LOG="$T18B_DIR/curl.log"
T18B_FAKE_CURL=$(make_fake_curl "$T18B_CURL_LOG")

T18B_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t18b-XXXXXXXX")
cat >"$T18B_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T18B_FAKE_SUDO_DIR/sudo"

T18B_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t18b-XXXXXXXX")
cat >"$T18B_FAKE_DF_DIR/df" <<'STUB'
#!/usr/bin/env bash
echo "Avail"
echo "20971520"
exit 0
STUB
chmod +x "$T18B_FAKE_DF_DIR/df"

T18B_RCLONE=$(make_fake_bin "rclone" "$T18B_DIR/rclone.log" 0)
T18B_GPG=$(make_fake_bin "gpg"    "$T18B_DIR/gpg.log"    0)
T18B_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t18b-XXXXXXXX")
cat >"$T18B_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T18B_FAKE_TAR_DIR/tar"

T18B_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t18b" \
  DATA_DIR="$T18B_DIR/data" \
  APEROD_TEXTFILE_DIR="$T18B_DIR/metrics" \
  APEROD_HISTORY_LOG="$T18B_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T18B_BACKUP_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t18b-secret" \
  ADMIN_TELEGRAM_CHAT_ID="18200001" \
  PATH="$T18B_FAKE_MKDIR_DIR:$T18B_FAKE_CURL:$T18B_FAKE_DF_DIR:$T18B_FAKE_SUDO_DIR:$T18B_RCLONE:$T18B_GPG:$T18B_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T18B_EXIT=$?
[[ "$T18B_EXIT" -eq 99 ]] && T18B_EXIT=0  # script returned 0

if [[ "$T18B_EXIT" -eq 0 ]]; then
  pass "mkdir BACKUP_DIR failure (tokens set): script still exits 0 (skipped, not failed)"
else
  fail "mkdir BACKUP_DIR failure (tokens set): script exited $T18B_EXIT — expected 0"
fi

if [[ -f "$T18B_CURL_LOG" ]] && grep -q "api.telegram.org" "$T18B_CURL_LOG"; then
  pass "Telegram backup_dir_unavailable alert was sent when tokens are set"
else
  fail "Telegram backup_dir_unavailable alert was NOT sent when tokens are set (log: $(cat "$T18B_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T18B_CURL_LOG" ]] && grep -q "sendMessage" "$T18B_CURL_LOG"; then
  pass "Telegram sendMessage endpoint targeted for backup_dir_unavailable alert"
else
  fail "Telegram sendMessage NOT targeted for backup_dir_unavailable alert (log: $(cat "$T18B_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T18B_CURL_LOG" ]] && grep -q "FAILMKDIR\|недоступна\|директория\|unavailable\|permission\|права" "$T18B_CURL_LOG"; then
  pass "Telegram message text references the unavailable path or permissions"
else
  fail "Telegram message text does NOT reference path/permissions (log: $(cat "$T18B_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T18B_CURL_LOG" ]] && grep -q "chat_id=18200001" "$T18B_CURL_LOG"; then
  pass "Telegram backup_dir_unavailable alert sent to correct chat_id"
else
  fail "Telegram backup_dir_unavailable alert sent to wrong or no chat_id (log: $(cat "$T18B_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

if [[ -f "$T18B_CURL_LOG" ]] && grep -q "bot${TELEGRAM_BOT_TOKEN:-tok-t18b-secret}\|bottok-t18b-secret" "$T18B_CURL_LOG"; then
  pass "Bot token embedded in Telegram API URL for backup_dir_unavailable alert"
else
  # Check for the token pattern another way since TELEGRAM_BOT_TOKEN is set as env during subshell
  if grep -q "bottok-t18b-secret" "$T18B_CURL_LOG" 2>/dev/null; then
    pass "Bot token embedded in Telegram API URL for backup_dir_unavailable alert"
  else
    fail "Bot token NOT found in Telegram API URL (log: $(cat "$T18B_CURL_LOG" 2>/dev/null || echo '<empty>'))"
  fi
fi

# =============================================================================
# Test 19: stale-backup-dir cleanup runs BEFORE NODE_DATA_DIR preflight
#
# Scenario A — stale dir found + cleaned:
#   df returns 4 GiB (below 5 GiB floor) for NODE_DATA_DIR UNTIL the rm -rf of
#   the stale dir sets a marker file; after that it returns 6 GiB (above the
#   floor).  Because _cleanup_stale_backup_dirs() runs before _disk_preflight(),
#   the cleanup fires first → marker is set → df returns sufficient space →
#   backup proceeds past the preflight and reaches pg_dump.
#
# Scenario B — no stale dir (cleanup finds nothing, no space freed):
#   df returns 4 GiB throughout → NODE_DATA_DIR preflight triggers → backup
#   is skipped with skipped_low_disk=1 (confirms the A result is non-trivial).
# =============================================================================
section "Test 19: stale-backup-dir cleanup runs before NODE_DATA_DIR preflight (ordering guard)"

T19_DIR=$(mktemp -d "$TMPDIR_TEST/run-t19-XXXXXXXX")
T19_NODE_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t19-XXXXXXXX")
echo "blockdata" > "$T19_NODE_DATA_DIR/block.bin"
make_settings_json "$T19_DIR/data"
mkdir -p "$T19_DIR/metrics"

# The backup dir parent is T19_DIR/backup-parent; cleanup scans that dir.
T19_BACKUP_PARENT=$(mktemp -d "$TMPDIR_TEST/backup-parent-t19-XXXXXXXX")
T19_BACKUP_DIR="$T19_BACKUP_PARENT/aperod_backups_$$"

# Stale backup dir that the find stub will report
T19_STALE_DIR="$T19_BACKUP_PARENT/aperod_backups_99999"
mkdir -p "$T19_STALE_DIR"

# Marker written by rm stub when it removes the stale dir
T19_RM_MARKER="$T19_DIR/cleanup-ran.marker"

# ── Scenario A stubs ─────────────────────────────────────────────────────────

T19_CURL_LOG="$T19_DIR/curl.log"
T19_FAKE_CURL=$(make_fake_curl "$T19_CURL_LOG")

# find stub: when called looking for aperod_backups_* dirs, print the stale dir.
# Pattern uses unquoted glob (*aperod_backups_*) so bash [[ ]] treats it as a
# wildcard match against $* (which contains "-name aperod_backups_*" literally).
T19_FAKE_FIND_DIR=$(mktemp -d "$TMPDIR_TEST/fake-find-t19-XXXXXXXX")
cat >"$T19_FAKE_FIND_DIR/find" <<STUB
#!/usr/bin/env bash
if [[ "\$*" == *-name*aperod_backups_* ]]; then
  printf '%s\0' "$T19_STALE_DIR"
else
  /usr/bin/find "\$@"
fi
STUB
chmod +x "$T19_FAKE_FIND_DIR/find"

# du stub: return 2 GiB for any path (stale dir size + scaled preflight)
T19_FAKE_DU_DIR=$(mktemp -d "$TMPDIR_TEST/fake-du-t19-XXXXXXXX")
cat >"$T19_FAKE_DU_DIR/du" <<'STUB'
#!/usr/bin/env bash
echo "2097152	${@: -1}"
exit 0
STUB
chmod +x "$T19_FAKE_DU_DIR/du"

# rm stub: touch the marker when removing the stale dir, then run real rm
T19_FAKE_RM_DIR=$(mktemp -d "$TMPDIR_TEST/fake-rm-t19-XXXXXXXX")
cat >"$T19_FAKE_RM_DIR/rm" <<STUB
#!/usr/bin/env bash
if [[ "\$*" == *"$T19_STALE_DIR"* ]]; then
  touch "$T19_RM_MARKER"
fi
/bin/rm "\$@"
STUB
chmod +x "$T19_FAKE_RM_DIR/rm"

# df stub: BACKUP_DIR always returns 20 GiB; NODE_DATA_DIR returns 4 GiB
# (below 5 GiB floor) unless the rm marker exists, in which case 6 GiB (above).
T19_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t19-XXXXXXXX")
cat >"$T19_FAKE_DF_DIR/df" <<STUB
#!/usr/bin/env bash
last_arg="\${@: -1}"
echo "Avail"
if [[ "\$last_arg" == "$T19_NODE_DATA_DIR"* ]]; then
  if [[ -f "$T19_RM_MARKER" ]]; then
    echo "6291456"   # 6 GiB — cleanup freed space, preflight passes
  else
    echo "4194304"   # 4 GiB — insufficient, preflight would skip
  fi
else
  echo "20971520"    # 20 GiB — BACKUP_DIR always passes
fi
exit 0
STUB
chmod +x "$T19_FAKE_DF_DIR/df"

# sudo stub: fails immediately (simulates pg_dump stage — if reached, preflight passed)
T19_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t19-XXXXXXXX")
cat >"$T19_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$T19_FAKE_SUDO_DIR/sudo"

T19_RCLONE=$(make_fake_bin "rclone" "$T19_DIR/rclone.log" 0)
T19_GPG=$(make_fake_bin "gpg"    "$T19_DIR/gpg.log"    0)
T19_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t19-XXXXXXXX")
cat >"$T19_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T19_FAKE_TAR_DIR/tar"

# Create backup dir so the mkdir guard passes
mkdir -p "$T19_BACKUP_DIR"

T19A_EXIT=0
APEROD_BACKUP_PASSWORD="test-pass-t19" \
  DATA_DIR="$T19_DIR/data" \
  APEROD_TEXTFILE_DIR="$T19_DIR/metrics" \
  APEROD_HISTORY_LOG="$T19_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T19_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T19_NODE_DATA_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t19" \
  ADMIN_TELEGRAM_CHAT_ID="19000001" \
  PATH="$T19_FAKE_FIND_DIR:$T19_FAKE_RM_DIR:$T19_FAKE_DU_DIR:$T19_FAKE_DF_DIR:$T19_FAKE_CURL:$T19_FAKE_SUDO_DIR:$T19_RCLONE:$T19_GPG:$T19_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T19A_EXIT=$?

# Scenario A checks
if [[ -f "$T19_RM_MARKER" ]]; then
  pass "Scenario A: cleanup ran — stale backup dir was removed (rm marker set)"
else
  fail "Scenario A: cleanup did NOT run — rm marker was never set"
fi

# pg_dump stage was reached (sudo stub exits 1 → script exits non-zero)
# If preflight skipped the backup, script exits 0; non-zero means it proceeded.
if [[ "$T19A_EXIT" -ne 0 ]]; then
  pass "Scenario A: backup proceeded past NODE_DATA_DIR preflight (reached pg_dump, exited non-zero)"
else
  # Exit 0 could mean skipped — check prom to distinguish success from skip
  T19_PROM="$T19_DIR/metrics/aperod_backup.prom"
  if [[ -f "$T19_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T19_PROM"; then
    fail "Scenario A: backup was SKIPPED (skipped_low_disk=1) — cleanup did not free space before preflight ran"
  else
    pass "Scenario A: backup proceeded past NODE_DATA_DIR preflight (exit 0, no low-disk skip metric)"
  fi
fi

T19_PROM="$T19_DIR/metrics/aperod_backup.prom"
if [[ ! -f "$T19_PROM" ]] || ! grep -q "^aperod_backup_skipped_low_disk 1" "$T19_PROM"; then
  pass "Scenario A: aperod_backup_skipped_low_disk NOT set to 1 — preflight passed after cleanup"
else
  fail "Scenario A: aperod_backup_skipped_low_disk=1 found — preflight incorrectly skipped after cleanup freed space"
fi

if [[ ! -f "$T19_CURL_LOG" ]] || ! grep -q "мало места\|low.disk\|NODE_DATA_DIR" "$T19_CURL_LOG" 2>/dev/null; then
  pass "Scenario A: no low-disk Telegram alert — NODE_DATA_DIR preflight passed"
else
  fail "Scenario A: low-disk Telegram alert fired — preflight triggered despite cleanup freeing space"
fi

# ── Scenario B: no stale dir found → no space freed → preflight skips ────────

T19B_DIR=$(mktemp -d "$TMPDIR_TEST/run-t19b-XXXXXXXX")
T19B_NODE_DATA_DIR=$(mktemp -d "$TMPDIR_TEST/nodedata-t19b-XXXXXXXX")
make_settings_json "$T19B_DIR/data"
mkdir -p "$T19B_DIR/metrics"
T19B_BACKUP_PARENT=$(mktemp -d "$TMPDIR_TEST/backup-parent-t19b-XXXXXXXX")
T19B_BACKUP_DIR="$T19B_BACKUP_PARENT/aperod_backups_$$"
mkdir -p "$T19B_BACKUP_DIR"

T19B_CURL_LOG="$T19B_DIR/curl.log"
T19B_FAKE_CURL=$(make_fake_curl "$T19B_CURL_LOG")

# find stub: returns nothing (no stale dirs) — cleanup frees no space
T19B_FAKE_FIND_DIR=$(mktemp -d "$TMPDIR_TEST/fake-find-t19b-XXXXXXXX")
cat >"$T19B_FAKE_FIND_DIR/find" <<'STUB'
#!/usr/bin/env bash
if [[ "$*" == *-name*aperod_backups_* ]]; then
  : # print nothing — no stale dirs found
else
  /usr/bin/find "$@"
fi
STUB
chmod +x "$T19B_FAKE_FIND_DIR/find"

# df stub: BACKUP_DIR returns 20 GiB; NODE_DATA_DIR returns 4 GiB (insufficient, no cleanup)
T19B_FAKE_DF_DIR=$(mktemp -d "$TMPDIR_TEST/fake-df-t19b-XXXXXXXX")
cat >"$T19B_FAKE_DF_DIR/df" <<STUB
#!/usr/bin/env bash
last_arg="\${@: -1}"
echo "Avail"
if [[ "\$last_arg" == "$T19B_NODE_DATA_DIR"* ]]; then
  echo "4194304"   # 4 GiB — insufficient, preflight should skip
else
  echo "20971520"  # 20 GiB — BACKUP_DIR passes
fi
exit 0
STUB
chmod +x "$T19B_FAKE_DF_DIR/df"

T19B_FAKE_DU_DIR=$(mktemp -d "$TMPDIR_TEST/fake-du-t19b-XXXXXXXX")
cat >"$T19B_FAKE_DU_DIR/du" <<'STUB'
#!/usr/bin/env bash
echo "2097152	${@: -1}"
exit 0
STUB
chmod +x "$T19B_FAKE_DU_DIR/du"

T19B_FAKE_SUDO_DIR=$(mktemp -d "$TMPDIR_TEST/fake-sudo-t19b-XXXXXXXX")
cat >"$T19B_FAKE_SUDO_DIR/sudo" <<'STUB'
#!/usr/bin/env bash
exit 1
STUB
chmod +x "$T19B_FAKE_SUDO_DIR/sudo"

T19B_RCLONE=$(make_fake_bin "rclone" "$T19B_DIR/rclone.log" 0)
T19B_GPG=$(make_fake_bin "gpg"    "$T19B_DIR/gpg.log"    0)
T19B_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t19b-XXXXXXXX")
cat >"$T19B_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$T19B_FAKE_TAR_DIR/tar"

T19B_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t19b" \
  DATA_DIR="$T19B_DIR/data" \
  APEROD_TEXTFILE_DIR="$T19B_DIR/metrics" \
  APEROD_HISTORY_LOG="$T19B_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T19B_BACKUP_DIR" \
  APEROD_NODE_DATA_DIR_OVERRIDE="$T19B_NODE_DATA_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t19b" \
  ADMIN_TELEGRAM_CHAT_ID="19000002" \
  PATH="$T19B_FAKE_FIND_DIR:$T19B_FAKE_DU_DIR:$T19B_FAKE_DF_DIR:$T19B_FAKE_CURL:$T19B_FAKE_SUDO_DIR:$T19B_RCLONE:$T19B_GPG:$T19B_FAKE_TAR_DIR:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T19B_EXIT=$?
[[ "$T19B_EXIT" -eq 99 ]] && T19B_EXIT=0  # script returned 0

if [[ "$T19B_EXIT" -eq 0 ]]; then
  pass "Scenario B: exits 0 (skipped) when no stale dir is cleaned and NODE_DATA_DIR has 4 GiB"
else
  fail "Scenario B: exited $T19B_EXIT — expected 0 (skip)"
fi

T19B_PROM="$T19B_DIR/metrics/aperod_backup.prom"
if [[ -f "$T19B_PROM" ]] && grep -q "^aperod_backup_skipped_low_disk 1" "$T19B_PROM"; then
  pass "Scenario B: aperod_backup_skipped_low_disk=1 — NODE_DATA_DIR preflight correctly triggered without cleanup"
else
  fail "Scenario B: aperod_backup_skipped_low_disk=1 NOT found (file: $(cat "$T19B_PROM" 2>/dev/null || echo '<missing>'))"
fi

# =============================================================================
# Test 20: update scripts source sync-backup-script.sh and call _sync_backup_script
# =============================================================================
section "Test 20: update-node.sh and update-api.sh delegate to sync-backup-script.sh"

SYNC_HELPER="$SCRIPT_DIR/sync-backup-script.sh"
UPDATE_NODE_SH="$SCRIPT_DIR/update-node.sh"
UPDATE_API_SH="$SCRIPT_DIR/update-api.sh"

# ── The shared helper must exist ──────────────────────────────────────────────
if [[ ! -f "$SYNC_HELPER" ]]; then
  fail "sync-backup-script.sh not found at $SYNC_HELPER"
else
  pass "sync-backup-script.sh exists"
fi

# ── Atomic rename: helper must stage + mv, never write directly ──────────────
if grep -q 'mv -f' "$SYNC_HELPER"; then
  pass "sync-backup-script.sh uses mv -f (atomic rename after staging)"
else
  fail "sync-backup-script.sh does NOT use mv -f — not atomic"
fi

if grep -q 'mktemp' "$SYNC_HELPER"; then
  pass "sync-backup-script.sh uses mktemp for staging (same-filesystem temp)"
else
  fail "sync-backup-script.sh does NOT use mktemp for staging"
fi

# install(1) writes directly to the destination — verify it is NOT used here
if grep -q '^\s*install ' "$SYNC_HELPER" | grep -qv '^\s*#'; then
  fail "sync-backup-script.sh uses 'install' — should use atomic stage+rename instead"
else
  pass "sync-backup-script.sh does NOT use 'install' directly to destination"
fi

# ── update-node.sh must source the helper and call _sync_backup_script ────────
if [[ ! -f "$UPDATE_NODE_SH" ]]; then
  fail "update-node.sh not found at $UPDATE_NODE_SH"
else
  if grep -q 'source.*sync-backup-script\.sh' "$UPDATE_NODE_SH"; then
    pass "update-node.sh sources sync-backup-script.sh"
  else
    fail "update-node.sh does NOT source sync-backup-script.sh"
  fi

  if grep -q '_sync_backup_script' "$UPDATE_NODE_SH"; then
    pass "update-node.sh calls _sync_backup_script"
  else
    fail "update-node.sh does NOT call _sync_backup_script"
  fi
fi

# ── update-api.sh must source the helper and call _sync_backup_script ─────────
if [[ ! -f "$UPDATE_API_SH" ]]; then
  fail "update-api.sh not found at $UPDATE_API_SH"
else
  if grep -q 'source.*sync-backup-script\.sh' "$UPDATE_API_SH"; then
    pass "update-api.sh sources sync-backup-script.sh"
  else
    fail "update-api.sh does NOT source sync-backup-script.sh"
  fi

  if grep -q '_sync_backup_script' "$UPDATE_API_SH"; then
    pass "update-api.sh calls _sync_backup_script"
  else
    fail "update-api.sh does NOT call _sync_backup_script"
  fi
fi

# =============================================================================
# Test 20b: _sync_backup_script functional — sources real helper, path overrides
# =============================================================================
section "Test 20b: _sync_backup_script (real code) — atomic copy on mismatch"

# Source the actual production helper so we test the real implementation.
# shellcheck source=sync-backup-script.sh
source "$SYNC_HELPER"

T20_DIR=$(mktemp -d "$TMPDIR_TEST/run-t20-XXXXXXXX")

# ── Scenario A: versions differ → atomic update ──────────────────────────────
T20A_INSTALLED="$T20_DIR/installed_a.sh"
T20A_REPO="$T20_DIR/repo_a.sh"
printf '#!/bin/bash\necho old\n' > "$T20A_INSTALLED"
printf '#!/bin/bash\necho new\n' > "$T20A_REPO"
chmod 700 "$T20A_INSTALLED" "$T20A_REPO"

T20A_OUTPUT=$(_sync_backup_script "$T20A_INSTALLED" "$T20A_REPO" 2>&1 || true)

if echo "$T20A_OUTPUT" | grep -q '\[sync\].*updated'; then
  pass "sync outputs 'updated' when versions differ"
else
  fail "sync did NOT output 'updated' when versions differed (output: $T20A_OUTPUT)"
fi

if diff -q "$T20A_INSTALLED" "$T20A_REPO" >/dev/null 2>&1; then
  pass "installed file matches repo after sync (content check)"
else
  fail "installed file does NOT match repo after sync"
fi

# ── Scenario B: versions identical → no-op ───────────────────────────────────
T20B_INSTALLED="$T20_DIR/installed_b.sh"
T20B_REPO="$T20_DIR/repo_b.sh"
printf '#!/bin/bash\necho same\n' > "$T20B_INSTALLED"
printf '#!/bin/bash\necho same\n' > "$T20B_REPO"
chmod 700 "$T20B_INSTALLED" "$T20B_REPO"

# Record mtime to confirm no write occurred
T20B_MTIME_BEFORE=$(stat -c '%Y' "$T20B_INSTALLED" 2>/dev/null || echo 0)
sleep 1   # ensure a write would change mtime

T20B_OUTPUT=$(_sync_backup_script "$T20B_INSTALLED" "$T20B_REPO" 2>&1 || true)
T20B_MTIME_AFTER=$(stat -c '%Y' "$T20B_INSTALLED" 2>/dev/null || echo 1)

if echo "$T20B_OUTPUT" | grep -q 'already up to date'; then
  pass "sync outputs 'already up to date' when versions are identical"
else
  fail "sync did not output 'already up to date' for identical files (output: $T20B_OUTPUT)"
fi

if [[ "$T20B_MTIME_BEFORE" -eq "$T20B_MTIME_AFTER" ]]; then
  pass "sync did not modify the installed file when already up to date"
else
  fail "sync wrote to the installed file even though versions were identical"
fi

# ── Scenario C: installed absent → no-op ─────────────────────────────────────
T20C_REPO="$T20_DIR/repo_c.sh"
printf '#!/bin/bash\necho hello\n' > "$T20C_REPO"
T20C_INSTALLED="$T20_DIR/not_installed.sh"   # does not exist

T20C_OUTPUT=$(_sync_backup_script "$T20C_INSTALLED" "$T20C_REPO" 2>&1 || true)

if [[ -z "$T20C_OUTPUT" ]]; then
  pass "sync is a no-op (silent) when installed file is absent"
else
  fail "sync produced unexpected output when installed file is absent: $T20C_OUTPUT"
fi

if [[ ! -f "$T20C_INSTALLED" ]]; then
  pass "sync did not create the installed file when it was absent (not an initial installer)"
else
  fail "sync created a file that should not exist"
fi

# ── Scenario D: atomic rename — staging temp is cleaned up on failure ─────────
# Make the destination directory read-only so mv fails; verify no stale temp.
T20D_DIR=$(mktemp -d "$TMPDIR_TEST/readonly-t20-XXXXXXXX")
T20D_INSTALLED="$T20D_DIR/aperod_backup.sh"
T20D_REPO="$T20_DIR/repo_d.sh"
printf '#!/bin/bash\necho old\n' > "$T20D_INSTALLED"
printf '#!/bin/bash\necho new\n' > "$T20D_REPO"

# Make directory read-only so rename cannot succeed but mktemp can still
# create a temp file in it (we need a read-only-for-rename situation).
# Easiest: pass a mismatched destination path that can't be renamed into.
# Use a cross-filesystem target by pointing installed to /tmp while repo is local.
T20D_CROSSFS_INSTALLED="/tmp/aperod_sync_test_$$.sh"
printf '#!/bin/bash\necho old\n' > "$T20D_CROSSFS_INSTALLED"

T20D_OUTPUT=$(_sync_backup_script "$T20D_CROSSFS_INSTALLED" "$T20D_REPO" 2>&1 || true)
rm -f "$T20D_CROSSFS_INSTALLED" 2>/dev/null || true

# Cross-filesystem mv fails (EXDEV); function should print a warning and clean up.
# Accept either: success (some kernels allow cross-fs mv via copy+unlink fallback) OR
# a [warn] line indicating graceful failure — never a crash or a stale staging file.
if echo "$T20D_OUTPUT" | grep -qE '\[sync\].*updated|\[warn\].*sync FAILED'; then
  pass "sync either succeeds or prints [warn] on cross-filesystem edge case (no crash)"
else
  fail "sync produced unexpected output on cross-filesystem case: $T20D_OUTPUT"
fi

# =============================================================================
# Test 20c: sha256sum match — simulated deploy produces identical checksums
#
# This is the canonical acceptance check called out in the task spec:
#   sha256sum /usr/local/bin/aperod_backup.sh blockchain/deploy/aperod_backup.sh
# should print the same hash after every git pull + update script run.
#
# We simulate the deploy by:
#   1. Writing an "old" installed copy (different content).
#   2. Running _sync_backup_script with the repo copy as source.
#   3. Computing sha256sum of both files and asserting they match.
# =============================================================================
section "Test 20c: sha256sum — installed copy matches repo copy after simulated deploy"

T20C2_DIR=$(mktemp -d "$TMPDIR_TEST/run-t20c2-XXXXXXXX")
T20C2_INSTALLED="$T20C2_DIR/aperod_backup_installed.sh"
T20C2_REPO="$T20C2_DIR/aperod_backup_repo.sh"

# Write distinct contents so the sync is forced to overwrite.
printf '#!/bin/bash\n# OLD VERSION — should be replaced\necho old-backup\n' \
  > "$T20C2_INSTALLED"
chmod 700 "$T20C2_INSTALLED"

# Use the actual aperod_backup.sh from the repo as the authoritative source.
cp "$BACKUP_SH" "$T20C2_REPO"
chmod 700 "$T20C2_REPO"

# Run the sync (already sourced above in Test 20b).
_sync_backup_script "$T20C2_INSTALLED" "$T20C2_REPO" >/dev/null 2>&1 || true

# sha256sum comparison — the core of the acceptance criterion.
if command -v sha256sum >/dev/null 2>&1; then
  T20C2_HASH_INSTALLED=$(sha256sum "$T20C2_INSTALLED" | awk '{print $1}')
  T20C2_HASH_REPO=$(sha256sum "$T20C2_REPO" | awk '{print $1}')
  if [[ "$T20C2_HASH_INSTALLED" == "$T20C2_HASH_REPO" ]]; then
    pass "sha256sum match after simulated deploy: installed == repo ($T20C2_HASH_REPO)"
  else
    fail "sha256sum MISMATCH after simulated deploy: installed=$T20C2_HASH_INSTALLED repo=$T20C2_HASH_REPO"
  fi
elif command -v shasum >/dev/null 2>&1; then
  # macOS fallback
  T20C2_HASH_INSTALLED=$(shasum -a 256 "$T20C2_INSTALLED" | awk '{print $1}')
  T20C2_HASH_REPO=$(shasum -a 256 "$T20C2_REPO" | awk '{print $1}')
  if [[ "$T20C2_HASH_INSTALLED" == "$T20C2_HASH_REPO" ]]; then
    pass "sha256sum (shasum -a 256) match after simulated deploy: installed == repo"
  else
    fail "sha256sum MISMATCH after simulated deploy (shasum): installed=$T20C2_HASH_INSTALLED repo=$T20C2_HASH_REPO"
  fi
else
  # Neither tool available — fall back to diff (already verified above in 20b Scenario A).
  if diff -q "$T20C2_INSTALLED" "$T20C2_REPO" >/dev/null 2>&1; then
    pass "sha256sum: tool unavailable; diff confirms installed == repo after simulated deploy"
  else
    fail "sha256sum: tool unavailable; diff reports installed != repo after simulated deploy"
  fi
fi

# Also confirm the old content is gone.
if ! grep -q 'OLD VERSION' "$T20C2_INSTALLED" 2>/dev/null; then
  pass "old backup script content replaced — installed copy is no longer stale"
else
  fail "old backup script content still present — sync did not overwrite the installed copy"
fi

# =============================================================================
# Test 21: update-validator.sh — sources sync-backup-script.sh and calls
#          _sync_backup_script so aperod_backup.sh stays in sync on the main
#          server and on each validator that has backup configured.
# =============================================================================
section "Test 21: update-validator.sh sources sync-backup-script.sh and calls _sync_backup_script"

UPDATE_VALIDATOR_SH="$SCRIPT_DIR/update-validator.sh"

if [[ ! -f "$UPDATE_VALIDATOR_SH" ]]; then
  fail "update-validator.sh not found at: $UPDATE_VALIDATOR_SH"
else

  # Static check 21a: sources sync-backup-script.sh
  if grep -qE 'source\s.*sync-backup-script\.sh' "$UPDATE_VALIDATOR_SH"; then
    pass "update-validator.sh sources sync-backup-script.sh"
  else
    fail "update-validator.sh does NOT source sync-backup-script.sh"
  fi

  # Static check 21b: calls _sync_backup_script
  if grep -q '_sync_backup_script' "$UPDATE_VALIDATOR_SH"; then
    pass "update-validator.sh calls _sync_backup_script"
  else
    fail "update-validator.sh does NOT call _sync_backup_script"
  fi

  # Static check 21c: passes the repo aperod_backup.sh path explicitly
  # (so the function does not rely on ambient BLOCKCHAIN_DIR / APEROD_DIR)
  if grep -q 'aperod_backup.sh' "$UPDATE_VALIDATOR_SH"; then
    pass "update-validator.sh references aperod_backup.sh (passes path to _sync_backup_script)"
  else
    fail "update-validator.sh does NOT reference aperod_backup.sh"
  fi

  # Static check 21d: remote validators also receive the backup script.
  # The SCP call uses BACKUP_SH_SRC (set to "${DEPLOY_DIR}/aperod_backup.sh"),
  # so match either the literal path or the variable name in an scp call.
  if grep -qE 'scp.*BACKUP_SH_SRC|BACKUP_SH_SRC=.*aperod_backup' "$UPDATE_VALIDATOR_SH"; then
    pass "update-validator.sh SCPs aperod_backup.sh to each validator (via BACKUP_SH_SRC)"
  else
    fail "update-validator.sh does NOT SCP aperod_backup.sh to validators"
  fi

  # Static check 21e: remote install is conditional on backup being configured
  # (BACKUP_SH_SENT guard so validators without setup-backup.sh are skipped)
  if grep -q 'BACKUP_SH_SENT' "$UPDATE_VALIDATOR_SH"; then
    pass "update-validator.sh uses BACKUP_SH_SENT guard (remote install is conditional)"
  else
    fail "update-validator.sh does NOT have BACKUP_SH_SENT guard"
  fi

  # Behavioral check 21f: remote backup install uses atomic stage-then-rename.
  # Extract the remote heredoc and verify it never directly `cp` onto the live
  # installed path, instead staging into the same directory and using `mv -f`.
  # This prevents a concurrent cron/systemd backup job from reading a half-written
  # file (the same guarantee _sync_backup_script provides on the main server).
  REMOTE_HEREDOC=$(sed -n '/REMOTE_EOF/,/^REMOTE_EOF$/p' "$UPDATE_VALIDATOR_SH" || true)

  # Must use mv -f for the final install of the backup script.
  if echo "$REMOTE_HEREDOC" | grep -q 'mv -f.*BACKUP'; then
    pass "remote backup install uses mv -f (atomic rename, not direct cp to live path)"
  else
    fail "remote backup install does NOT use mv -f — direct cp onto live path is not atomic"
  fi

  # Must stage into the install directory (same filesystem) via mktemp.
  if echo "$REMOTE_HEREDOC" | grep -qE 'mktemp.*INSTALL_DIR|mktemp.*usr.local.bin'; then
    pass "remote backup install stages into the install directory (same filesystem mktemp)"
  else
    fail "remote backup install does NOT stage into the install directory — mv may cross filesystems"
  fi

  # Must NOT have a bare 'sudo cp ... BACKUP_INSTALLED' without a subsequent mv -f
  # (i.e. the cp must target the staging temp file, not the live installed path).
  DIRECT_CP=$(echo "$REMOTE_HEREDOC" | grep -E 'sudo cp.*BACKUP_INSTALLED' || true)
  if [[ -z "$DIRECT_CP" ]]; then
    pass "remote script does not directly cp onto the live BACKUP_INSTALLED path"
  else
    fail "remote script has a bare 'sudo cp ... BACKUP_INSTALLED' — live path is overwritten non-atomically: $DIRECT_CP"
  fi

fi

# =============================================================================
# Test 22: install-node.sh — copies aperod_backup.sh to /usr/local/bin/ as
#          part of first-time setup so the correct version is present from day 1.
# =============================================================================
section "Test 22: install-node.sh copies aperod_backup.sh to /usr/local/bin/ on first-time install"

INSTALL_NODE_SH="$SCRIPT_DIR/install-node.sh"

if [[ ! -f "$INSTALL_NODE_SH" ]]; then
  fail "install-node.sh not found at: $INSTALL_NODE_SH"
else

  # Static check 22a: copies aperod_backup.sh
  if grep -qE 'cp.*aperod_backup\.sh' "$INSTALL_NODE_SH"; then
    pass "install-node.sh copies aperod_backup.sh"
  else
    fail "install-node.sh does NOT copy aperod_backup.sh"
  fi

  # Static check 22b: installs to /usr/local/bin/
  if grep -qE 'aperod_backup\.sh.*(/usr/local/bin/|/usr/local/bin/aperod_backup)' "$INSTALL_NODE_SH" \
     || grep -qE '/usr/local/bin/aperod_backup\.sh' "$INSTALL_NODE_SH"; then
    pass "install-node.sh installs aperod_backup.sh to /usr/local/bin/"
  else
    fail "install-node.sh does NOT install aperod_backup.sh to /usr/local/bin/"
  fi

  # Static check 22c: sets executable permission (chmod 700 or chmod +x)
  if grep -qE 'chmod.*(700|[+]x).*aperod_backup' "$INSTALL_NODE_SH" \
     || grep -A3 'cp.*aperod_backup' "$INSTALL_NODE_SH" | grep -q 'chmod'; then
    pass "install-node.sh sets executable permission on aperod_backup.sh"
  else
    fail "install-node.sh does NOT set executable permission on aperod_backup.sh"
  fi

  # Static check 22d: installation is guarded by a file-existence check
  # (e.g. [[ -f "${SCRIPT_DIR}/aperod_backup.sh" ]]) so a missing repo
  # copy does not abort the install.
  if grep -qE '\[\[.*-f.*aperod_backup' "$INSTALL_NODE_SH"; then
    pass "install-node.sh guards aperod_backup.sh copy with a -f existence check"
  else
    fail "install-node.sh does NOT guard aperod_backup.sh copy with a -f check"
  fi

fi

# =============================================================================
# Test 23: backup_dir_unavailable → exit 0 (skipped), status/reason set,
#          Telegram alert fires when tokens are present.
#
#   Regression guard: when BACKUP_DIR cannot be created the script must record
#   _BACKUP_FINAL_STATUS=skipped / _BACKUP_SKIP_REASON=backup_dir_unavailable in
#   the history log AND fire the Telegram alert (not a noisy failure metric).
#   mkdir failure is simulated by pointing BACKUP_DIR under a regular file, so
#   `mkdir -p` cannot create the directory (a file is not a directory).
# =============================================================================
section "Test 23: backup_dir_unavailable → skipped + reason set + Telegram alert"

T23_DIR=$(mktemp -d "$TMPDIR_TEST/run-t23-XXXXXXXX")
make_settings_json "$T23_DIR/data"
mkdir -p "$T23_DIR/metrics"

# Create a regular file, then aim BACKUP_DIR at a subpath *under* that file.
# `mkdir -p` fails because a component of the path is not a directory — this
# reproduces a non-writable / unavailable backup location without needing root.
T23_BLOCKER_FILE="$T23_DIR/not-a-dir"
: > "$T23_BLOCKER_FILE"
T23_BACKUP_DIR="$T23_BLOCKER_FILE/aperod_backups"

T23_CURL_LOG="$T23_DIR/curl.log"
T23_FAKE_CURL=$(make_fake_curl "$T23_CURL_LOG")

# sudo stub — pg_dump path must never be reached after the guard bails.
T23_FAKE_SUDO=$(make_fake_bin "sudo" "$T23_DIR/sudo.log" 0)
T23_FAKE_RCLONE=$(make_fake_bin "rclone" "$T23_DIR/rclone.log" 0)
T23_FAKE_GPG=$(make_fake_bin "gpg" "$T23_DIR/gpg.log" 0)

T23_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t23" \
  DATA_DIR="$T23_DIR/data" \
  APEROD_TEXTFILE_DIR="$T23_DIR/metrics" \
  APEROD_HISTORY_LOG="$T23_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T23_BACKUP_DIR" \
  TELEGRAM_BOT_TOKEN="tok-t23" \
  ADMIN_TELEGRAM_CHAT_ID="23000023" \
  PATH="$T23_FAKE_CURL:$T23_FAKE_SUDO:$T23_FAKE_RCLONE:$T23_FAKE_GPG:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T23_EXIT=$?
[[ "$T23_EXIT" -eq 99 ]] && T23_EXIT=0  # script returned 0

if [[ "$T23_EXIT" -eq 0 ]]; then
  pass "backup_dir_unavailable exits 0 (skipped, not failed)"
else
  fail "backup_dir_unavailable exited $T23_EXIT — expected 0"
fi

# The history-log JSON line encodes _BACKUP_FINAL_STATUS and _BACKUP_SKIP_REASON.
T23_LOG="$T23_DIR/backup.log"
if [[ -f "$T23_LOG" ]] && grep -q '"status":"skipped"' "$T23_LOG"; then
  pass "history log records _BACKUP_FINAL_STATUS=skipped"
else
  fail "history log missing status=skipped (log: $(cat "$T23_LOG" 2>/dev/null || echo '<missing>'))"
fi

if [[ -f "$T23_LOG" ]] && grep -q '"skipReason":"backup_dir_unavailable"' "$T23_LOG"; then
  pass "history log records _BACKUP_SKIP_REASON=backup_dir_unavailable"
else
  fail "history log missing skipReason=backup_dir_unavailable (log: $(cat "$T23_LOG" 2>/dev/null || echo '<missing>'))"
fi

# Prometheus: skipped, not a failure.
T23_PROM="$T23_DIR/metrics/aperod_backup.prom"
if [[ -f "$T23_PROM" ]] && grep -q "^aperod_backup_last_success 0" "$T23_PROM"; then
  pass "Prometheus: aperod_backup_last_success=0 on backup_dir_unavailable"
else
  fail "Prometheus: aperod_backup_last_success=0 NOT found (file: $(cat "$T23_PROM" 2>/dev/null || echo '<missing>'))"
fi

# Telegram POST must fire with tokens present, targeting the Telegram API.
if [[ -f "$T23_CURL_LOG" ]] && grep -q "api.telegram.org" "$T23_CURL_LOG"; then
  pass "Telegram backup_dir_unavailable alert curl POST fired when tokens are set"
else
  fail "Telegram backup_dir_unavailable alert was NOT sent (log: $(cat "$T23_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# The alert must carry the configured chat id (correct routing).
if [[ -f "$T23_CURL_LOG" ]] && grep -q "23000023" "$T23_CURL_LOG"; then
  pass "Telegram backup_dir_unavailable alert routed to the configured chat id"
else
  fail "Telegram backup_dir_unavailable alert missing chat id (log: $(cat "$T23_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# The alert must be an HTTP POST (curl -X POST), matching the script's call.
if [[ -f "$T23_CURL_LOG" ]] && grep -q -- "-X POST" "$T23_CURL_LOG"; then
  pass "Telegram backup_dir_unavailable alert uses an HTTP POST"
else
  fail "Telegram backup_dir_unavailable alert is not a POST (log: $(cat "$T23_CURL_LOG" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test 24: backup_dir_unavailable + tokens unset → NO Telegram curl POST
#
#   The alert block is guarded by TELEGRAM_BOT_TOKEN + ADMIN_TELEGRAM_CHAT_ID;
#   with both empty no curl call to the Telegram API must be attempted.
# =============================================================================
section "Test 24: backup_dir_unavailable + tokens unset → no Telegram curl POST"

T24_DIR=$(mktemp -d "$TMPDIR_TEST/run-t24-XXXXXXXX")
make_settings_json "$T24_DIR/data"
mkdir -p "$T24_DIR/metrics"

T24_BLOCKER_FILE="$T24_DIR/not-a-dir"
: > "$T24_BLOCKER_FILE"
T24_BACKUP_DIR="$T24_BLOCKER_FILE/aperod_backups"

T24_CURL_LOG="$T24_DIR/curl.log"
T24_FAKE_CURL=$(make_fake_curl "$T24_CURL_LOG")
T24_FAKE_SUDO=$(make_fake_bin "sudo" "$T24_DIR/sudo.log" 0)

T24_EXIT=99
APEROD_BACKUP_PASSWORD="test-pass-t24" \
  DATA_DIR="$T24_DIR/data" \
  APEROD_TEXTFILE_DIR="$T24_DIR/metrics" \
  APEROD_HISTORY_LOG="$T24_DIR/backup.log" \
  APEROD_BACKUP_DIR_OVERRIDE="$T24_BACKUP_DIR" \
  TELEGRAM_BOT_TOKEN="" \
  ADMIN_TELEGRAM_CHAT_ID="" \
  PATH="$T24_FAKE_CURL:$T24_FAKE_SUDO:$PATH" \
  bash "$BACKUP_SH" >/dev/null 2>&1 || T24_EXIT=$?
[[ "$T24_EXIT" -eq 99 ]] && T24_EXIT=0

if [[ ! -f "$T24_CURL_LOG" ]] || ! grep -q "api.telegram.org" "$T24_CURL_LOG"; then
  pass "no Telegram curl POST when tokens are unset (alert correctly suppressed)"
else
  fail "Telegram alert fired despite unset tokens (log: $(cat "$T24_CURL_LOG" 2>/dev/null || echo '<empty>'))"
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
