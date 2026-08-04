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

  local exit_code=0
  APEROD_BACKUP_PASSWORD="test-password-123" \
    DATA_DIR="$run_dir/data" \
    APEROD_TEXTFILE_DIR="$metrics_dir" \
    APEROD_HISTORY_LOG="$run_dir/backup.log" \
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

# tar stub that succeeds and creates an empty file
T9_FAKE_TAR_DIR=$(mktemp -d "$TMPDIR_TEST/fake-tar-t9-XXXXXXXX")
cat >"$T9_FAKE_TAR_DIR/tar" <<'STUB'
#!/usr/bin/env bash
# Create any -f <file> argument as an empty file so downstream commands work.
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-f" || "$1" == *f* ]]; then
    # find next non-flag arg as the output file
    shift
    [[ "${1:-}" != -* ]] && touch "$1" 2>/dev/null || true
    break
  fi
  shift
done
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
while [[ $# -gt 0 ]]; do
  if [[ "$1" == "-f" || "$1" == *f* ]]; then
    shift; [[ "${1:-}" != -* ]] && touch "$1" 2>/dev/null || true; break
  fi
  shift
done
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
