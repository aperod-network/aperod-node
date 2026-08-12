#!/usr/bin/env bash
# =============================================================================
#  test-update-validator.sh — End-to-end tests for the aperod_backup.sh sync
#                              step in update-validator.sh.
#
#  All external calls (ssh, scp, systemctl, curl) are stubbed so that no real
#  network connections are made and no root privileges are required.
#
#  Scenarios:
#    UV-1  BACKUP_SH_SENT=1 is injected into the remote script when
#          aperod_backup.sh exists in the repo and the SCP stub succeeds.
#    UV-2  Remote install step replaces the installed copy when the validator
#          has backup configured (/usr/local/bin/aperod_backup.sh present).
#    UV-3  Remote install step is skipped when /usr/local/bin/aperod_backup.sh
#          is absent on the validator.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-update-validator.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UPDATE_VALIDATOR_SH="${SCRIPT_DIR}/update-validator.sh"
SYNC_BACKUP_SH="${SCRIPT_DIR}/sync-backup-script.sh"
BACKUP_SH_REPO="${SCRIPT_DIR}/aperod_backup.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; BOLD='\033[1m'; NC='\033[0m'
PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Guard: scripts must exist ─────────────────────────────────────────────────
for f in "$UPDATE_VALIDATOR_SH" "$SYNC_BACKUP_SH"; do
  if [[ ! -f "$f" ]]; then
    echo -e "${RED}[ERR]${NC}  required file not found: $f" >&2
    exit 1
  fi
done

# ── Shared temp directory (cleaned on exit) ────────────────────────────────────
TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# =============================================================================
# Helpers
# =============================================================================

# make_fake_binary name dir [exit_code]
#   Creates a stub executable in dir/ that logs all args and exits with
#   exit_code (default 0).
make_fake_binary() {
  local name="$1" dir="$2" ec="${3:-0}"
  local log="${dir}/${name}.log"
  cat >"${dir}/${name}" <<STUB
#!/usr/bin/env bash
echo "${name} \$*" >> "${log}"
exit ${ec}
STUB
  chmod +x "${dir}/${name}"
}

# =============================================================================
# Test UV-1:
#   BACKUP_SH_SENT=1 is injected into the remote script when
#   aperod_backup.sh exists in the repo and the SCP stub succeeds.
# =============================================================================
section "UV-1: BACKUP_SH_SENT=1 injected when backup.sh exists and SCP succeeds"

UV1=$(mktemp -d "$TMPDIR_TEST/uv1-XXXXXXXX")
mkdir -p "${UV1}/fake-bins"

# Fake infrastructure
touch "${UV1}/known_hosts"
touch "${UV1}/id_fake"
echo "fake-binary-content" > "${UV1}/binary"
# Writable BINARY_DST on the fake remote
UV1_BINARY_DST="${UV1}/remote-bin/aperod-node"
mkdir -p "$(dirname "${UV1_BINARY_DST}")"
echo "aperod@192.0.2.1" > "${UV1}/validators.conf"

# ── Stub: scp — log calls and always succeed ──────────────────────────────
UV1_SCP_LOG="${UV1}/scp.log"
cat >"${UV1}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV1_SCP_LOG}"
exit 0
SCPSTUB
chmod +x "${UV1}/fake-bins/scp"

# ── Stub: ssh — capture the remote script, then succeed ───────────────────
UV1_SCRIPT_LOG="${UV1}/remote_script.txt"
cat >"${UV1}/fake-bins/ssh" <<SSHSTUB
#!/usr/bin/env bash
# Read the remote script piped via stdin (bash -s) and save for inspection
cat > "${UV1_SCRIPT_LOG}"
exit 0
SSHSTUB
chmod +x "${UV1}/fake-bins/ssh"

# Stub curl (Telegram — must never fire in this test)
make_fake_binary "curl" "${UV1}/fake-bins"

# ── Run update-validator.sh ───────────────────────────────────────────────
(
  export SSH_KEY="${UV1}/id_fake"
  export KNOWN_HOSTS_FILE="${UV1}/known_hosts"
  export BINARY_SRC="${UV1}/binary"
  export BINARY_DST="${UV1_BINARY_DST}"
  export VALIDATORS_CONF="${UV1}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV1}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) || true   # exit code is not the focus of UV-1

# Assertions
if [[ -f "${UV1_SCRIPT_LOG}" ]]; then
  pass "Remote script was captured by the SSH stub"
else
  fail "Remote script NOT captured — SSH stub was never called"
fi

if [[ -f "${UV1_SCRIPT_LOG}" ]] && grep -q 'BACKUP_SH_SENT="1"' "${UV1_SCRIPT_LOG}"; then
  pass 'BACKUP_SH_SENT="1" is present in the injected remote script'
else
  fail 'BACKUP_SH_SENT="1" NOT found in remote script (grep: '"$(grep 'BACKUP_SH_SENT' "${UV1_SCRIPT_LOG}" 2>/dev/null || echo '<not found>')"')'
fi

if [[ -f "${UV1_SCP_LOG}" ]] && grep -q "aperod_backup" "${UV1_SCP_LOG}"; then
  pass "SCP stub was called with aperod_backup.sh as a source"
else
  fail "SCP stub was NOT called with aperod_backup.sh (log: $(cat "${UV1_SCP_LOG}" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test UV-2:
#   Remote install step replaces the installed backup.sh when the validator
#   already has /usr/local/bin/aperod_backup.sh (backup is configured).
# =============================================================================
section "UV-2: remote install replaces aperod_backup.sh when backup is configured on validator"

UV2=$(mktemp -d "$TMPDIR_TEST/uv2-XXXXXXXX")
mkdir -p "${UV2}/fake-bins"
touch "${UV2}/known_hosts"
touch "${UV2}/id_fake"
echo "fake-binary-content" > "${UV2}/binary"
echo "aperod@192.0.2.2" > "${UV2}/validators.conf"

# Writable BINARY_DST for the remote install step (avoids /usr/local/bin)
UV2_BINARY_DST="${UV2}/remote-fs/usr/local/bin/aperod-node"
mkdir -p "$(dirname "${UV2_BINARY_DST}")"
touch "${UV2_BINARY_DST}" && chmod +x "${UV2_BINARY_DST}"

# Fake installed aperod_backup.sh on the "validator" — backup IS configured
UV2_INSTALLED="${UV2}/remote-fs/usr/local/bin/aperod_backup.sh"
echo "# OLD BACKUP SCRIPT VERSION" > "${UV2_INSTALLED}"
chmod 700 "${UV2_INSTALLED}"

# Directory where scp places the staged backup file on the "remote" machine
UV2_REMOTE_TMP="${UV2}/remote-tmp"
mkdir -p "${UV2_REMOTE_TMP}"

# ── Stub: scp — log calls; for backup.sh, place a copy in the remote tmp ──
UV2_SCP_LOG="${UV2}/scp.log"
cat >"${UV2}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV2_SCP_LOG}"
# When scp-ing aperod_backup.sh (source path, no colon), copy to fake remote tmp
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    cp "\$arg" "${UV2_REMOTE_TMP}/aperod_backup_new.sh" 2>/dev/null || true
    break
  fi
done
exit 0
SCPSTUB
chmod +x "${UV2}/fake-bins/scp"

# ── Stub: ssh — captures the remote script, rewrites paths, runs locally ──
UV2_SCRIPT_LOG="${UV2}/remote_script.txt"
UV2_SSH_RUN_LOG="${UV2}/ssh_run.log"

# sudo stub: pass-through (no elevation needed in a test)
cat >"${UV2}/fake-bins/sudo" <<'SUDOSTUB'
#!/usr/bin/env bash
exec "$@"
SUDOSTUB
chmod +x "${UV2}/fake-bins/sudo"

# systemctl stub: always succeeds
cat >"${UV2}/fake-bins/systemctl" <<'SVCSTUB'
#!/usr/bin/env bash
exit 0
SVCSTUB
chmod +x "${UV2}/fake-bins/systemctl"

make_fake_binary "curl" "${UV2}/fake-bins"

# The remote script has two hardcoded paths that point to the real filesystem:
#   /usr/local/bin/aperod_backup.sh  — BACKUP_INSTALLED variable
#   /tmp/aperod_backup_new.sh        — BACKUP_TMP variable
# BINARY_DST and /tmp/aperod-node-new are controlled via env/substitution below.
#
# We also need a fake source binary at /tmp/aperod-node-new on the "remote".
UV2_FAKE_NODE_NEW="${UV2_REMOTE_TMP}/aperod-node-new"
cp "${UV2}/binary" "${UV2_FAKE_NODE_NEW}"

# Write the SSH stub using printf to avoid heredoc quoting issues with paths
printf '%s\n' \
  '#!/usr/bin/env bash' \
  'SCRIPT=$(cat)' \
  "echo \"\$SCRIPT\" > '${UV2_SCRIPT_LOG}'" \
  "MODIFIED=\"\${SCRIPT//\/usr\/local\/bin\/aperod_backup.sh/${UV2_INSTALLED//\//\\/}}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod_backup_new.sh/${UV2_REMOTE_TMP//\//\\/}\/aperod_backup_new.sh}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod-node-new/${UV2_FAKE_NODE_NEW//\//\\/}}\"" \
  "PATH='${UV2}/fake-bins':\$PATH bash -s <<< \"\$MODIFIED\" >> '${UV2_SSH_RUN_LOG}' 2>&1" \
  > "${UV2}/fake-bins/ssh"
chmod +x "${UV2}/fake-bins/ssh"

# ── Run update-validator.sh ───────────────────────────────────────────────
(
  export SSH_KEY="${UV2}/id_fake"
  export KNOWN_HOSTS_FILE="${UV2}/known_hosts"
  export BINARY_SRC="${UV2}/binary"
  export BINARY_DST="${UV2_BINARY_DST}"
  export VALIDATORS_CONF="${UV2}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV2}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) || true

# Assertions
INSTALLED_CONTENT=$(cat "${UV2_INSTALLED}" 2>/dev/null || echo "<missing>")
if [[ "${INSTALLED_CONTENT}" != "# OLD BACKUP SCRIPT VERSION" ]]; then
  pass "Remote install replaced aperod_backup.sh (content changed from old version)"
else
  # Show the run log to aid debugging
  fail "Remote install did NOT replace aperod_backup.sh (content: ${INSTALLED_CONTENT}; run log: $(cat "${UV2_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

if [[ -f "${UV2_SSH_RUN_LOG}" ]] && grep -q "\[3b\]" "${UV2_SSH_RUN_LOG}"; then
  pass "[3b] aperod_backup.sh install log line present in remote script output"
else
  fail "[3b] log line NOT found in remote script output (log: $(cat "${UV2_SSH_RUN_LOG}" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test UV-3:
#   Remote install step is SKIPPED when /usr/local/bin/aperod_backup.sh is
#   absent on the validator (backup was never configured there).
# =============================================================================
section "UV-3: remote install skipped when aperod_backup.sh absent on validator"

UV3=$(mktemp -d "$TMPDIR_TEST/uv3-XXXXXXXX")
mkdir -p "${UV3}/fake-bins"
touch "${UV3}/known_hosts"
touch "${UV3}/id_fake"
echo "fake-binary-content" > "${UV3}/binary"
echo "aperod@192.0.2.3" > "${UV3}/validators.conf"

# Writable BINARY_DST (no /usr/local/bin)
UV3_BINARY_DST="${UV3}/remote-fs/usr/local/bin/aperod-node"
mkdir -p "$(dirname "${UV3_BINARY_DST}")"
touch "${UV3_BINARY_DST}" && chmod +x "${UV3_BINARY_DST}"

# NOTE: aperod_backup.sh is intentionally NOT created on the "validator"
UV3_INSTALLED="${UV3}/remote-fs/usr/local/bin/aperod_backup.sh"

UV3_REMOTE_TMP="${UV3}/remote-tmp"
mkdir -p "${UV3_REMOTE_TMP}"
UV3_FAKE_NODE_NEW="${UV3_REMOTE_TMP}/aperod-node-new"
cp "${UV3}/binary" "${UV3_FAKE_NODE_NEW}"

# ── Stub: scp ────────────────────────────────────────────────────────────
UV3_SCP_LOG="${UV3}/scp.log"
cat >"${UV3}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV3_SCP_LOG}"
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    cp "\$arg" "${UV3_REMOTE_TMP}/aperod_backup_new.sh" 2>/dev/null || true
    break
  fi
done
exit 0
SCPSTUB
chmod +x "${UV3}/fake-bins/scp"

# ── Stubs: sudo, systemctl, curl ─────────────────────────────────────────
cat >"${UV3}/fake-bins/sudo" <<'SUDOSTUB'
#!/usr/bin/env bash
exec "$@"
SUDOSTUB
chmod +x "${UV3}/fake-bins/sudo"

cat >"${UV3}/fake-bins/systemctl" <<'SVCSTUB'
#!/usr/bin/env bash
exit 0
SVCSTUB
chmod +x "${UV3}/fake-bins/systemctl"

make_fake_binary "curl" "${UV3}/fake-bins"

# ── Stub: ssh ────────────────────────────────────────────────────────────
UV3_SCRIPT_LOG="${UV3}/remote_script.txt"
UV3_SSH_RUN_LOG="${UV3}/ssh_run.log"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'SCRIPT=$(cat)' \
  "echo \"\$SCRIPT\" > '${UV3_SCRIPT_LOG}'" \
  "MODIFIED=\"\${SCRIPT//\/usr\/local\/bin\/aperod_backup.sh/${UV3_INSTALLED//\//\\/}}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod_backup_new.sh/${UV3_REMOTE_TMP//\//\\/}\/aperod_backup_new.sh}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod-node-new/${UV3_FAKE_NODE_NEW//\//\\/}}\"" \
  "PATH='${UV3}/fake-bins':\$PATH bash -s <<< \"\$MODIFIED\" >> '${UV3_SSH_RUN_LOG}' 2>&1" \
  > "${UV3}/fake-bins/ssh"
chmod +x "${UV3}/fake-bins/ssh"

# ── Run update-validator.sh ───────────────────────────────────────────────
(
  export SSH_KEY="${UV3}/id_fake"
  export KNOWN_HOSTS_FILE="${UV3}/known_hosts"
  export BINARY_SRC="${UV3}/binary"
  export BINARY_DST="${UV3_BINARY_DST}"
  export VALIDATORS_CONF="${UV3}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV3}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) || true

# Assertions
if [[ ! -f "${UV3_INSTALLED}" ]]; then
  pass "aperod_backup.sh was NOT created on the validator (install step correctly skipped)"
else
  fail "aperod_backup.sh was unexpectedly created on validator when backup was not configured"
fi

UV3_TMP_STAGED="${UV3_REMOTE_TMP}/aperod_backup_new.sh"
if [[ ! -f "${UV3_TMP_STAGED}" ]]; then
  pass "Staged backup tmp file was cleaned up after skipping the install"
else
  fail "Staged backup tmp file was NOT cleaned up (run log: $(cat "${UV3_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

if [[ -f "${UV3_SSH_RUN_LOG}" ]] && grep -q "\[3b\]" "${UV3_SSH_RUN_LOG}"; then
  fail "[3b] install log line appeared even though backup was not configured on validator"
else
  pass "[3b] install log line correctly absent when backup is not configured"
fi

# =============================================================================
# Test UV-4:
#   BACKUP_SH_SENT=1 is injected when a validator is supplied as a positional
#   CLI argument (i.e. without relying on validators.conf).
# =============================================================================
section "UV-4: BACKUP_SH_SENT=1 injected when validator host is a CLI argument"

UV4=$(mktemp -d "$TMPDIR_TEST/uv4-XXXXXXXX")
mkdir -p "${UV4}/fake-bins"

touch "${UV4}/known_hosts"
touch "${UV4}/id_fake"
echo "fake-binary-content" > "${UV4}/binary"
# No validators.conf created — the CLI argument path must not need it.

UV4_BINARY_DST="${UV4}/remote-bin/aperod-node"
mkdir -p "$(dirname "${UV4_BINARY_DST}")"

# ── Stub: scp — log calls and always succeed ──────────────────────────────
UV4_SCP_LOG="${UV4}/scp.log"
cat >"${UV4}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV4_SCP_LOG}"
exit 0
SCPSTUB
chmod +x "${UV4}/fake-bins/scp"

# ── Stub: ssh — capture the remote script piped via stdin ─────────────────
UV4_SCRIPT_LOG="${UV4}/remote_script.txt"
cat >"${UV4}/fake-bins/ssh" <<SSHSTUB
#!/usr/bin/env bash
cat > "${UV4_SCRIPT_LOG}"
exit 0
SSHSTUB
chmod +x "${UV4}/fake-bins/ssh"

# Stub curl (Telegram — must never fire in this test)
make_fake_binary "curl" "${UV4}/fake-bins"

# ── Run update-validator.sh with a positional argument (no conf file) ─────
(
  export SSH_KEY="${UV4}/id_fake"
  export KNOWN_HOSTS_FILE="${UV4}/known_hosts"
  export BINARY_SRC="${UV4}/binary"
  export BINARY_DST="${UV4_BINARY_DST}"
  # Deliberately point VALIDATORS_CONF at a non-existent path to confirm
  # the script uses the CLI argument instead of falling back to conf.
  export VALIDATORS_CONF="${UV4}/nonexistent-validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV4}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" "aperod@192.0.2.4" >/dev/null 2>&1
) || true   # exit code is not the focus of UV-4

# Assertions
if [[ -f "${UV4_SCRIPT_LOG}" ]]; then
  pass "Remote script was captured by the SSH stub (CLI-arg path)"
else
  fail "Remote script NOT captured — SSH stub was never called (CLI-arg path)"
fi

if [[ -f "${UV4_SCRIPT_LOG}" ]] && grep -q 'BACKUP_SH_SENT="1"' "${UV4_SCRIPT_LOG}"; then
  pass 'BACKUP_SH_SENT="1" is present in the injected remote script (CLI-arg path)'
else
  fail 'BACKUP_SH_SENT="1" NOT found in remote script on CLI-arg path (grep: '"$(grep 'BACKUP_SH_SENT' "${UV4_SCRIPT_LOG}" 2>/dev/null || echo '<not found>')"')'
fi

if [[ -f "${UV4_SCP_LOG}" ]] && grep -q "aperod_backup" "${UV4_SCP_LOG}"; then
  pass "SCP stub was called with aperod_backup.sh when validator supplied as CLI arg"
else
  fail "SCP stub was NOT called with aperod_backup.sh on CLI-arg path (log: $(cat "${UV4_SCP_LOG}" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test UV-5:
#   Mid-transfer crash: mktemp succeeds but mv fails.
#   The cleanup branch (sudo rm -f BACKUP_STAGE) must fire so the staging
#   file is not left on disk, the old installed copy is preserved, and the
#   script continues to the next validator without aborting.
# =============================================================================
section "UV-5: stale staging file cleaned up and old copy preserved when mv fails"

UV5=$(mktemp -d "$TMPDIR_TEST/uv5-XXXXXXXX")
mkdir -p "${UV5}/fake-bins"
touch "${UV5}/known_hosts"
touch "${UV5}/id_fake"
echo "fake-binary-content" > "${UV5}/binary"

# Two validators so we can confirm the script continues past the first failure.
printf 'aperod@192.0.2.5\naperod@192.0.2.6\n' > "${UV5}/validators.conf"

# Writable BINARY_DST (no /usr/local/bin)
UV5_BINARY_DST="${UV5}/remote-fs/usr/local/bin/aperod-node"
mkdir -p "$(dirname "${UV5_BINARY_DST}")"
touch "${UV5_BINARY_DST}" && chmod +x "${UV5_BINARY_DST}"

# Fake installed aperod_backup.sh — this must remain unchanged after the run.
UV5_INSTALLED="${UV5}/remote-fs/usr/local/bin/aperod_backup.sh"
echo "# OLD BACKUP SCRIPT VERSION" > "${UV5_INSTALLED}"
chmod 700 "${UV5_INSTALLED}"

UV5_REMOTE_TMP="${UV5}/remote-tmp"
mkdir -p "${UV5_REMOTE_TMP}"

# The staging file that mktemp will "create" — we track this specific path.
UV5_STAGE_FILE="${UV5}/remote-tmp/.aperod_backup_sync.TESTXXXX"

# ── Stub: scp — log calls; copy backup.sh to fake remote tmp ─────────────
UV5_SCP_LOG="${UV5}/scp.log"
cat >"${UV5}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV5_SCP_LOG}"
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    cp "\$arg" "${UV5_REMOTE_TMP}/aperod_backup_new.sh" 2>/dev/null || true
    break
  fi
done
exit 0
SCPSTUB
chmod +x "${UV5}/fake-bins/scp"

# ── Stub: mktemp — creates a file at a known path and echoes it ───────────
# Intercepts `sudo mktemp <template>` so we know exactly where the staging
# file lives without parsing the remote script's output.
cat >"${UV5}/fake-bins/mktemp" <<MKTEMPSTUB
#!/usr/bin/env bash
touch "${UV5_STAGE_FILE}"
echo "${UV5_STAGE_FILE}"
MKTEMPSTUB
chmod +x "${UV5}/fake-bins/mktemp"

# ── Stub: mv — always fails to simulate a permissions / rename error ──────
cat >"${UV5}/fake-bins/mv" <<'MVSTUB'
#!/usr/bin/env bash
exit 1
MVSTUB
chmod +x "${UV5}/fake-bins/mv"

# ── Stubs: sudo (pass-through), systemctl, curl ───────────────────────────
cat >"${UV5}/fake-bins/sudo" <<'SUDOSTUB'
#!/usr/bin/env bash
exec "$@"
SUDOSTUB
chmod +x "${UV5}/fake-bins/sudo"

cat >"${UV5}/fake-bins/systemctl" <<'SVCSTUB'
#!/usr/bin/env bash
exit 0
SVCSTUB
chmod +x "${UV5}/fake-bins/systemctl"

make_fake_binary "curl" "${UV5}/fake-bins"

# ── Stub: ssh — captures and rewrites the remote script, then runs locally ─
UV5_SCRIPT_LOG="${UV5}/remote_script.txt"
UV5_SSH_RUN_LOG="${UV5}/ssh_run.log"
UV5_FAKE_NODE_NEW="${UV5_REMOTE_TMP}/aperod-node-new"
cp "${UV5}/binary" "${UV5_FAKE_NODE_NEW}"

# Count how many times SSH is called (one per validator) to confirm continuation.
UV5_SSH_CALL_COUNT="${UV5}/ssh_call_count"
echo "0" > "${UV5_SSH_CALL_COUNT}"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'SCRIPT=$(cat)' \
  "echo \"\$SCRIPT\" > '${UV5_SCRIPT_LOG}'" \
  "COUNT=\$(cat '${UV5_SSH_CALL_COUNT}'); echo \$(( COUNT + 1 )) > '${UV5_SSH_CALL_COUNT}'" \
  "MODIFIED=\"\${SCRIPT//\/usr\/local\/bin\/aperod_backup.sh/${UV5_INSTALLED//\//\\/}}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod_backup_new.sh/${UV5_REMOTE_TMP//\//\\/}\/aperod_backup_new.sh}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod-node-new/${UV5_FAKE_NODE_NEW//\//\\/}}\"" \
  "PATH='${UV5}/fake-bins':\$PATH bash -s <<< \"\$MODIFIED\" >> '${UV5_SSH_RUN_LOG}' 2>&1" \
  > "${UV5}/fake-bins/ssh"
chmod +x "${UV5}/fake-bins/ssh"

# ── Run update-validator.sh ───────────────────────────────────────────────
UV5_EXIT=0
(
  export SSH_KEY="${UV5}/id_fake"
  export KNOWN_HOSTS_FILE="${UV5}/known_hosts"
  export BINARY_SRC="${UV5}/binary"
  export BINARY_DST="${UV5_BINARY_DST}"
  export VALIDATORS_CONF="${UV5}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV5}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) && UV5_EXIT=0 || UV5_EXIT=$?

# Assertion 1: staging file must be gone (cleanup branch fired)
if [[ ! -f "${UV5_STAGE_FILE}" ]]; then
  pass "Staging file was cleaned up after mv failure (no stale file left on disk)"
else
  fail "Staging file was NOT cleaned up — stale file remains: ${UV5_STAGE_FILE}"
fi

# Assertion 2: old installed copy must be unchanged
INSTALLED_CONTENT=$(cat "${UV5_INSTALLED}" 2>/dev/null || echo "<missing>")
if [[ "${INSTALLED_CONTENT}" == "# OLD BACKUP SCRIPT VERSION" ]]; then
  pass "Old installed aperod_backup.sh preserved after mv failure"
else
  fail "Installed aperod_backup.sh was unexpectedly modified (content: ${INSTALLED_CONTENT})"
fi

# Assertion 3: warn line present in remote script output
if [[ -f "${UV5_SSH_RUN_LOG}" ]] && grep -q "atomic rename failed" "${UV5_SSH_RUN_LOG}"; then
  pass "Warn line 'atomic rename failed' emitted in remote script output"
else
  fail "Warn line NOT found in remote script output (log: $(cat "${UV5_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

# Assertion 4: script must have called SSH twice (both validators processed)
SSH_CALLS=$(cat "${UV5_SSH_CALL_COUNT}" 2>/dev/null || echo "0")
if [[ "${SSH_CALLS}" -ge 2 ]]; then
  pass "update-validator.sh continued to the second validator after mv failure (SSH called ${SSH_CALLS}×)"
else
  fail "update-validator.sh did NOT continue past the first validator (SSH called ${SSH_CALLS}× — expected ≥2)"
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
