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
#   BACKUP_SH_SENT=1 is injected for EVERY validator when two hosts are
#   supplied as CLI arguments (exercises the $@ loop, not the conf-file loop).
#   Both must trigger the SSH stub and both remote scripts must contain the
#   BACKUP_SH_SENT="1" marker so that the remote install logic can fire.
# =============================================================================
section "UV-5: BACKUP_SH_SENT=1 injected for both validators when two CLI arguments are passed"

UV5=$(mktemp -d "$TMPDIR_TEST/uv5-XXXXXXXX")
mkdir -p "${UV5}/fake-bins"

touch "${UV5}/known_hosts"
touch "${UV5}/id_fake"
echo "fake-binary-content" > "${UV5}/binary"
# No validators.conf — both hosts come from the CLI argument path.

UV5_BINARY_DST="${UV5}/remote-bin/aperod-node"
mkdir -p "$(dirname "${UV5_BINARY_DST}")"

# ── Stub: scp — log the full argument list so we can verify each host ─────
# The target arg looks like  aperod@192.0.2.51:/tmp/aperod_backup_new.sh
# Logging $* captures it so we can grep for each specific host:port.
UV5_SCP_LOG="${UV5}/scp.log"
cat >"${UV5}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV5_SCP_LOG}"
exit 0
SCPSTUB
chmod +x "${UV5}/fake-bins/scp"

# ── Stub: ssh — log the target argument and APPEND the piped script ────────
# ssh is called as:  ssh [opts] aperod@192.0.2.5x "bash -s" <<< SCRIPT
# The last non-option argument before "bash" is the target user@host.
# We log it separately so we can assert each specific host was contacted
# exactly once, not just that ssh was called twice with the same host.
UV5_SSH_ARG_LOG="${UV5}/ssh_args.log"   # one line per call: the target host
UV5_SCRIPT_LOG="${UV5}/remote_scripts.txt"  # appended scripts (one per call)

cat >"${UV5}/fake-bins/ssh" <<'SSHSTUB_EOF'
#!/usr/bin/env bash
# Walk args to find the user@host (first arg that contains @ and no leading -)
for a in "$@"; do
  case "$a" in
    -*) continue ;;
    *@*) TARGET_HOST="$a"; break ;;
  esac
done
echo "${TARGET_HOST:-unknown}" >> "UV5_SSH_ARG_LOG_PLACEHOLDER"
cat >> "UV5_SCRIPT_LOG_PLACEHOLDER"
exit 0
SSHSTUB_EOF
# Substitute the actual paths (heredoc quoting would lose the variables)
sed -i \
  -e "s|UV5_SSH_ARG_LOG_PLACEHOLDER|${UV5_SSH_ARG_LOG}|g" \
  -e "s|UV5_SCRIPT_LOG_PLACEHOLDER|${UV5_SCRIPT_LOG}|g" \
  "${UV5}/fake-bins/ssh"
chmod +x "${UV5}/fake-bins/ssh"

# Stub curl (Telegram — must never fire in this test)
make_fake_binary "curl" "${UV5}/fake-bins"

# ── Run update-validator.sh with two positional arguments (no conf file) ──
(
  export SSH_KEY="${UV5}/id_fake"
  export KNOWN_HOSTS_FILE="${UV5}/known_hosts"
  export BINARY_SRC="${UV5}/binary"
  export BINARY_DST="${UV5_BINARY_DST}"
  # Point VALIDATORS_CONF at a non-existent path to confirm the script does
  # NOT fall back to it when CLI arguments are present.
  export VALIDATORS_CONF="${UV5}/nonexistent-validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV5}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" "aperod@192.0.2.51" "aperod@192.0.2.52" >/dev/null 2>&1
) || true   # exit code is not the focus of UV-5

# Assertion 1: SSH must have been called exactly twice (one per distinct host)
UV5_CALLS=$(wc -l < "${UV5_SSH_ARG_LOG}" 2>/dev/null || echo "0")
UV5_CALLS="${UV5_CALLS// /}"   # trim whitespace from wc output
if [[ "${UV5_CALLS}" -eq 2 ]]; then
  pass "SSH stub was called exactly twice — one per CLI-argument validator"
else
  fail "SSH stub was called ${UV5_CALLS}× — expected exactly 2 (arg log: $(cat "${UV5_SSH_ARG_LOG}" 2>/dev/null || echo '<empty>'))"
fi

# Assertion 2: each distinct host must appear exactly once in the SSH arg log
for UV5_HOST in "aperod@192.0.2.51" "aperod@192.0.2.52"; do
  if grep -qxF "${UV5_HOST}" "${UV5_SSH_ARG_LOG}" 2>/dev/null; then
    pass "SSH stub contacted ${UV5_HOST} (host-identity check)"
  else
    fail "SSH stub did NOT contact ${UV5_HOST} (arg log: $(cat "${UV5_SSH_ARG_LOG}" 2>/dev/null || echo '<empty>'))"
  fi
done

# Assertion 3: BACKUP_SH_SENT="1" must appear exactly twice (once per script)
UV5_BACKUP_COUNT=$(grep -c 'BACKUP_SH_SENT="1"' "${UV5_SCRIPT_LOG}" 2>/dev/null || echo "0")
if [[ "${UV5_BACKUP_COUNT}" -eq 2 ]]; then
  pass "BACKUP_SH_SENT=\"1\" found exactly twice in remote scripts (one per validator)"
else
  fail "BACKUP_SH_SENT=\"1\" found ${UV5_BACKUP_COUNT}× — expected exactly 2"
fi

# Assertion 4: aperod_backup.sh SCP must have been sent to each specific host
for UV5_HOST in "aperod@192.0.2.51" "aperod@192.0.2.52"; do
  if grep -q "aperod_backup.*${UV5_HOST}" "${UV5_SCP_LOG}" 2>/dev/null \
      || grep -q "${UV5_HOST}.*aperod_backup" "${UV5_SCP_LOG}" 2>/dev/null; then
    pass "SCP delivered aperod_backup.sh to ${UV5_HOST}"
  else
    fail "SCP did NOT deliver aperod_backup.sh to ${UV5_HOST} (log: $(cat "${UV5_SCP_LOG}" 2>/dev/null || echo '<empty>'))"
  fi
done

# =============================================================================
# Test UV-6:
#   Mid-transfer crash: mktemp succeeds but mv fails.
#   The cleanup branch (sudo rm -f BACKUP_STAGE) must fire so the staging
#   file is not left on disk, the old installed copy is preserved, and the
#   script continues to the next validator without aborting.
# =============================================================================
section "UV-6: stale staging file cleaned up and old copy preserved when mv fails"

UV6=$(mktemp -d "$TMPDIR_TEST/uv6-XXXXXXXX")
mkdir -p "${UV6}/fake-bins"
touch "${UV6}/known_hosts"
touch "${UV6}/id_fake"
echo "fake-binary-content" > "${UV6}/binary"

# Two validators so we can confirm the script continues past the first failure.
printf 'aperod@192.0.2.61\naperod@192.0.2.62\n' > "${UV6}/validators.conf"

# Writable BINARY_DST (no /usr/local/bin)
UV6_BINARY_DST="${UV6}/remote-fs/usr/local/bin/aperod-node"
mkdir -p "$(dirname "${UV6_BINARY_DST}")"
touch "${UV6_BINARY_DST}" && chmod +x "${UV6_BINARY_DST}"

# Fake installed aperod_backup.sh — this must remain unchanged after the run.
UV6_INSTALLED="${UV6}/remote-fs/usr/local/bin/aperod_backup.sh"
echo "# OLD BACKUP SCRIPT VERSION" > "${UV6_INSTALLED}"
chmod 700 "${UV6_INSTALLED}"

UV6_REMOTE_TMP="${UV6}/remote-tmp"
mkdir -p "${UV6_REMOTE_TMP}"

# The staging file that mktemp will "create" — we track this specific path.
UV6_STAGE_FILE="${UV6}/remote-tmp/.aperod_backup_sync.TESTXXXX"

# ── Stub: scp — log calls; copy backup.sh to fake remote tmp ─────────────
UV6_SCP_LOG="${UV6}/scp.log"
cat >"${UV6}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV6_SCP_LOG}"
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    cp "\$arg" "${UV6_REMOTE_TMP}/aperod_backup_new.sh" 2>/dev/null || true
    break
  fi
done
exit 0
SCPSTUB
chmod +x "${UV6}/fake-bins/scp"

# ── Stub: mktemp — creates a file at a known path and echoes it ───────────
# Intercepts `sudo mktemp <template>` so we know exactly where the staging
# file lives without parsing the remote script's output.
cat >"${UV6}/fake-bins/mktemp" <<MKTEMPSTUB
#!/usr/bin/env bash
touch "${UV6_STAGE_FILE}"
echo "${UV6_STAGE_FILE}"
MKTEMPSTUB
chmod +x "${UV6}/fake-bins/mktemp"

# ── Stub: mv — always fails to simulate a permissions / rename error ──────
cat >"${UV6}/fake-bins/mv" <<'MVSTUB'
#!/usr/bin/env bash
exit 1
MVSTUB
chmod +x "${UV6}/fake-bins/mv"

# ── Stubs: sudo (pass-through), systemctl, curl ───────────────────────────
cat >"${UV6}/fake-bins/sudo" <<'SUDOSTUB'
#!/usr/bin/env bash
exec "$@"
SUDOSTUB
chmod +x "${UV6}/fake-bins/sudo"

cat >"${UV6}/fake-bins/systemctl" <<'SVCSTUB'
#!/usr/bin/env bash
exit 0
SVCSTUB
chmod +x "${UV6}/fake-bins/systemctl"

make_fake_binary "curl" "${UV6}/fake-bins"

# ── Stub: ssh — captures and rewrites the remote script, then runs locally ─
UV6_SCRIPT_LOG="${UV6}/remote_script.txt"
UV6_SSH_RUN_LOG="${UV6}/ssh_run.log"
UV6_FAKE_NODE_NEW="${UV6_REMOTE_TMP}/aperod-node-new"
cp "${UV6}/binary" "${UV6_FAKE_NODE_NEW}"

# Count how many times SSH is called (one per validator) to confirm continuation.
UV6_SSH_CALL_COUNT="${UV6}/ssh_call_count"
echo "0" > "${UV6_SSH_CALL_COUNT}"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'SCRIPT=$(cat)' \
  "echo \"\$SCRIPT\" > '${UV6_SCRIPT_LOG}'" \
  "COUNT=\$(cat '${UV6_SSH_CALL_COUNT}'); echo \$(( COUNT + 1 )) > '${UV6_SSH_CALL_COUNT}'" \
  "MODIFIED=\"\${SCRIPT//\/usr\/local\/bin\/aperod_backup.sh/${UV6_INSTALLED//\//\\/}}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod_backup_new.sh/${UV6_REMOTE_TMP//\//\\/}\/aperod_backup_new.sh}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod-node-new/${UV6_FAKE_NODE_NEW//\//\\/}}\"" \
  "PATH='${UV6}/fake-bins':\$PATH bash -s <<< \"\$MODIFIED\" >> '${UV6_SSH_RUN_LOG}' 2>&1" \
  > "${UV6}/fake-bins/ssh"
chmod +x "${UV6}/fake-bins/ssh"

# ── Run update-validator.sh ───────────────────────────────────────────────
UV6_EXIT=0
(
  export SSH_KEY="${UV6}/id_fake"
  export KNOWN_HOSTS_FILE="${UV6}/known_hosts"
  export BINARY_SRC="${UV6}/binary"
  export BINARY_DST="${UV6_BINARY_DST}"
  export VALIDATORS_CONF="${UV6}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV6}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) && UV6_EXIT=0 || UV6_EXIT=$?

# Assertion 1: staging file must be gone (cleanup branch fired)
if [[ ! -f "${UV6_STAGE_FILE}" ]]; then
  pass "Staging file was cleaned up after mv failure (no stale file left on disk)"
else
  fail "Staging file was NOT cleaned up — stale file remains: ${UV6_STAGE_FILE}"
fi

# Assertion 2: old installed copy must be unchanged
INSTALLED_CONTENT=$(cat "${UV6_INSTALLED}" 2>/dev/null || echo "<missing>")
if [[ "${INSTALLED_CONTENT}" == "# OLD BACKUP SCRIPT VERSION" ]]; then
  pass "Old installed aperod_backup.sh preserved after mv failure"
else
  fail "Installed aperod_backup.sh was unexpectedly modified (content: ${INSTALLED_CONTENT})"
fi

# Assertion 3: warn line present in remote script output
if [[ -f "${UV6_SSH_RUN_LOG}" ]] && grep -q "atomic rename failed" "${UV6_SSH_RUN_LOG}"; then
  pass "Warn line 'atomic rename failed' emitted in remote script output"
else
  fail "Warn line NOT found in remote script output (log: $(cat "${UV6_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

# Assertion 4: script must have called SSH twice (both validators processed)
SSH_CALLS=$(cat "${UV6_SSH_CALL_COUNT}" 2>/dev/null || echo "0")
if [[ "${SSH_CALLS}" -ge 2 ]]; then
  pass "update-validator.sh continued to the second validator after mv failure (SSH called ${SSH_CALLS}×)"
else
  fail "update-validator.sh did NOT continue past the first validator (SSH called ${SSH_CALLS}× — expected ≥2)"
fi

# =============================================================================
# Test UV-7:
#   SCP of aperod_backup.sh fails (exit 1) on the CLI-arg path.
#   The script must:
#     • Log the warn message (non-fatal)
#     • Inject BACKUP_SH_SENT="0" into the remote script (not "1")
#     • Still SCP the main binary and call SSH to run the remote update
# =============================================================================
section "UV-7: SCP of aperod_backup.sh fails on CLI-arg path → BACKUP_SH_SENT=0, binary SCP still runs"

UV7=$(mktemp -d "$TMPDIR_TEST/uv7-XXXXXXXX")
mkdir -p "${UV7}/fake-bins"

touch "${UV7}/known_hosts"
touch "${UV7}/id_fake"
echo "fake-binary-content" > "${UV7}/binary"
# No validators.conf — single host supplied as CLI argument.

UV7_BINARY_DST="${UV7}/remote-bin/aperod-node"
mkdir -p "$(dirname "${UV7_BINARY_DST}")"

# ── Stub: scp — succeeds for the main binary, fails for aperod_backup.sh ──
# The binary SCP looks like:  scp ... /path/to/binary  aperod@host:/tmp/aperod-node-new
# The backup SCP looks like:  scp ... /path/to/aperod_backup.sh  aperod@host:/tmp/aperod_backup_new.sh
UV7_SCP_LOG="${UV7}/scp.log"
cat >"${UV7}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV7_SCP_LOG}"
# Fail only for the backup.sh transfer; succeed for everything else.
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    exit 1
  fi
done
exit 0
SCPSTUB
chmod +x "${UV7}/fake-bins/scp"

# ── Stub: ssh — capture the remote script piped via stdin ─────────────────
UV7_SCRIPT_LOG="${UV7}/remote_script.txt"
cat >"${UV7}/fake-bins/ssh" <<SSHSTUB
#!/usr/bin/env bash
cat > "${UV7_SCRIPT_LOG}"
exit 0
SSHSTUB
chmod +x "${UV7}/fake-bins/ssh"

# Stub curl (Telegram — must never fire for a non-fatal warn)
make_fake_binary "curl" "${UV7}/fake-bins"

# ── Run update-validator.sh with a positional CLI argument ─────────────────
UV7_OUTPUT_LOG="${UV7}/output.log"
UV7_EXIT=0
(
  export SSH_KEY="${UV7}/id_fake"
  export KNOWN_HOSTS_FILE="${UV7}/known_hosts"
  export BINARY_SRC="${UV7}/binary"
  export BINARY_DST="${UV7_BINARY_DST}"
  # Point VALIDATORS_CONF at a non-existent path — CLI arg must take priority.
  export VALIDATORS_CONF="${UV7}/nonexistent-validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV7}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" "aperod@192.0.2.7" >"${UV7_OUTPUT_LOG}" 2>&1
) && UV7_EXIT=0 || UV7_EXIT=$?

# Assertion 0: script must exit 0 — backup.sh SCP failure is non-fatal
if [[ "${UV7_EXIT}" -eq 0 ]]; then
  pass "update-validator.sh exited 0 (backup.sh SCP failure is non-fatal)"
else
  fail "update-validator.sh exited ${UV7_EXIT} — expected 0; backup.sh SCP failure must not abort the script"
fi

# Assertion 0b: operator-visible warn message must appear in captured output
if grep -q "aperod_backup.sh SCP failed" "${UV7_OUTPUT_LOG}" 2>/dev/null; then
  pass "Warn 'aperod_backup.sh SCP failed' is present in script output"
else
  fail "Warn message NOT found in script output (log: $(cat "${UV7_OUTPUT_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

# Assertion 1: SSH stub must have been called — the remote update must proceed
if [[ -f "${UV7_SCRIPT_LOG}" ]]; then
  pass "Remote script was captured by SSH stub despite backup.sh SCP failure"
else
  fail "Remote script NOT captured — SSH stub was never called after backup.sh SCP failure"
fi

# Assertion 2: BACKUP_SH_SENT must be "0" (not "1") in the injected script
if [[ -f "${UV7_SCRIPT_LOG}" ]] && grep -q 'BACKUP_SH_SENT="0"' "${UV7_SCRIPT_LOG}"; then
  pass 'BACKUP_SH_SENT="0" correctly injected when backup.sh SCP failed'
else
  fail 'BACKUP_SH_SENT="0" NOT found — got: '"$(grep 'BACKUP_SH_SENT' "${UV7_SCRIPT_LOG}" 2>/dev/null || echo '<not found>')"
fi

# Assertion 3: "1" must NOT appear for BACKUP_SH_SENT
if [[ -f "${UV7_SCRIPT_LOG}" ]] && grep -q 'BACKUP_SH_SENT="1"' "${UV7_SCRIPT_LOG}"; then
  fail 'BACKUP_SH_SENT="1" found in remote script even though backup.sh SCP failed'
else
  pass 'BACKUP_SH_SENT="1" correctly absent from remote script'
fi

# Assertion 4: main binary SCP must still have run (aperod-node-new target)
if [[ -f "${UV7_SCP_LOG}" ]] && grep -q "aperod-node-new" "${UV7_SCP_LOG}"; then
  pass "Main binary SCP still ran after backup.sh SCP failure"
else
  fail "Main binary SCP did NOT run (log: $(cat "${UV7_SCP_LOG}" 2>/dev/null || echo '<empty>'))"
fi

# Assertion 5: backup.sh SCP attempt was made (the failure is real, not skipped)
if [[ -f "${UV7_SCP_LOG}" ]] && grep -q "aperod_backup" "${UV7_SCP_LOG}"; then
  pass "SCP was attempted for aperod_backup.sh on CLI-arg path (failure correctly triggered)"
else
  fail "SCP was NOT attempted for aperod_backup.sh on CLI-arg path (log: $(cat "${UV7_SCP_LOG}" 2>/dev/null || echo '<empty>'))"
fi

# =============================================================================
# Test UV-8:
#   cp of BACKUP_TMP into the staging file fails (exit 1).
#   The || cleanup handler must:
#     • Remove the staging file that mktemp already created
#     • Leave the installed copy unchanged
#     • Emit the "atomic rename failed" warn line
#   This closes the gap left by UV-6, which only exercised the mv failure
#   branch; here the failure occurs one step earlier in the cp→chmod→mv chain.
# =============================================================================
section "UV-8: staging file cleaned up and old copy preserved when cp (staging) fails"

UV8=$(mktemp -d "$TMPDIR_TEST/uv8-XXXXXXXX")
mkdir -p "${UV8}/fake-bins"
touch "${UV8}/known_hosts"
touch "${UV8}/id_fake"
echo "fake-binary-content" > "${UV8}/binary"
echo "aperod@192.0.2.81" > "${UV8}/validators.conf"

# Writable BINARY_DST (avoids /usr/local/bin)
UV8_BINARY_DST="${UV8}/remote-fs/usr/local/bin/aperod-node"
mkdir -p "$(dirname "${UV8_BINARY_DST}")"
touch "${UV8_BINARY_DST}" && chmod +x "${UV8_BINARY_DST}"

# Fake installed aperod_backup.sh — must remain unchanged after the run.
UV8_INSTALLED="${UV8}/remote-fs/usr/local/bin/aperod_backup.sh"
echo "# OLD BACKUP SCRIPT VERSION" > "${UV8_INSTALLED}"
chmod 700 "${UV8_INSTALLED}"

UV8_REMOTE_TMP="${UV8}/remote-tmp"
mkdir -p "${UV8_REMOTE_TMP}"

# The staging file that our mktemp stub will "create".
UV8_STAGE_FILE="${UV8}/remote-tmp/.aperod_backup_sync.TESTXXXX"

# ── Stub: scp — log calls; copy backup.sh to fake remote tmp ─────────────
UV8_SCP_LOG="${UV8}/scp.log"
cat >"${UV8}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV8_SCP_LOG}"
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    cp "\$arg" "${UV8_REMOTE_TMP}/aperod_backup_new.sh" 2>/dev/null || true
    break
  fi
done
exit 0
SCPSTUB
chmod +x "${UV8}/fake-bins/scp"

# ── Stub: mktemp — creates a file at a known path and echoes it ───────────
cat >"${UV8}/fake-bins/mktemp" <<MKTEMPSTUB
#!/usr/bin/env bash
touch "${UV8_STAGE_FILE}"
echo "${UV8_STAGE_FILE}"
MKTEMPSTUB
chmod +x "${UV8}/fake-bins/mktemp"

# ── Stub: cp — fails only for the staging copy (aperod_backup_new source) ─
# The binary install uses:  cp <aperod-node-new>  <BINARY_DST>
# The staging copy uses:    cp <aperod_backup_new.sh>  <BACKUP_STAGE>
# We check only the first positional (source) argument so that copies whose
# DESTINATION happens to contain "aperod_backup_new" (e.g. the scp stub
# populating the remote-tmp file) are NOT intercepted.
cat >"${UV8}/fake-bins/cp" <<'CPSTUB'
#!/usr/bin/env bash
# Walk past any leading flags (-a, -r, -f, etc.) to find the source argument.
for arg in "$@"; do
  case "$arg" in
    -*) continue ;;   # skip flags
    *)
      if [[ "$arg" == *"aperod_backup_new"* ]]; then
        exit 1   # simulate cp failure for the staging copy step
      fi
      break   # source found and it is not the backup-new file — fall through
      ;;
  esac
done
exec /bin/cp "$@"
CPSTUB
chmod +x "${UV8}/fake-bins/cp"

# ── Stubs: sudo (pass-through), systemctl, curl ───────────────────────────
cat >"${UV8}/fake-bins/sudo" <<'SUDOSTUB'
#!/usr/bin/env bash
exec "$@"
SUDOSTUB
chmod +x "${UV8}/fake-bins/sudo"

cat >"${UV8}/fake-bins/systemctl" <<'SVCSTUB'
#!/usr/bin/env bash
exit 0
SVCSTUB
chmod +x "${UV8}/fake-bins/systemctl"

make_fake_binary "curl" "${UV8}/fake-bins"

# ── Stub: ssh — captures and rewrites the remote script, then runs locally ─
UV8_SCRIPT_LOG="${UV8}/remote_script.txt"
UV8_SSH_RUN_LOG="${UV8}/ssh_run.log"
UV8_FAKE_NODE_NEW="${UV8_REMOTE_TMP}/aperod-node-new"
cp "${UV8}/binary" "${UV8_FAKE_NODE_NEW}"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'SCRIPT=$(cat)' \
  "echo \"\$SCRIPT\" > '${UV8_SCRIPT_LOG}'" \
  "MODIFIED=\"\${SCRIPT//\/usr\/local\/bin\/aperod_backup.sh/${UV8_INSTALLED//\//\\/}}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod_backup_new.sh/${UV8_REMOTE_TMP//\//\\/}\/aperod_backup_new.sh}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod-node-new/${UV8_FAKE_NODE_NEW//\//\\/}}\"" \
  "PATH='${UV8}/fake-bins':\$PATH bash -s <<< \"\$MODIFIED\" >> '${UV8_SSH_RUN_LOG}' 2>&1" \
  > "${UV8}/fake-bins/ssh"
chmod +x "${UV8}/fake-bins/ssh"

# ── Run update-validator.sh ───────────────────────────────────────────────
(
  export SSH_KEY="${UV8}/id_fake"
  export KNOWN_HOSTS_FILE="${UV8}/known_hosts"
  export BINARY_SRC="${UV8}/binary"
  export BINARY_DST="${UV8_BINARY_DST}"
  export VALIDATORS_CONF="${UV8}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV8}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) || true

# Assertion 1: staging file must be gone (cleanup branch fired after cp failure)
if [[ ! -f "${UV8_STAGE_FILE}" ]]; then
  pass "Staging file was cleaned up after cp failure (no stale file left on disk)"
else
  fail "Staging file was NOT cleaned up — stale file remains: ${UV8_STAGE_FILE}"
fi

# Assertion 2: old installed copy must be unchanged
INSTALLED_CONTENT=$(cat "${UV8_INSTALLED}" 2>/dev/null || echo "<missing>")
if [[ "${INSTALLED_CONTENT}" == "# OLD BACKUP SCRIPT VERSION" ]]; then
  pass "Old installed aperod_backup.sh preserved after cp failure"
else
  fail "Installed aperod_backup.sh was unexpectedly modified (content: ${INSTALLED_CONTENT})"
fi

# Assertion 3: warn line must appear in remote script output
if [[ -f "${UV8_SSH_RUN_LOG}" ]] && grep -q "atomic rename failed" "${UV8_SSH_RUN_LOG}"; then
  pass "Warn line 'atomic rename failed' emitted in remote script output after cp failure"
else
  fail "Warn line NOT found in remote script output (log: $(cat "${UV8_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

# =============================================================================
# Test UV-9:
#   mktemp itself fails (exits 1, prints nothing) when the install directory
#   is not writable.  BACKUP_STAGE is set to the empty string and the inner
#   install block is skipped.  The outer rm -f "${BACKUP_TMP}" must still run
#   so no /tmp artefact is left behind, and the warn line about "cannot create
#   staging file" must be emitted.
# =============================================================================
section "UV-9: /tmp backup temp file cleaned up and warn emitted when mktemp fails"

UV9=$(mktemp -d "$TMPDIR_TEST/uv9-XXXXXXXX")
mkdir -p "${UV9}/fake-bins"
touch "${UV9}/known_hosts"
touch "${UV9}/id_fake"
echo "fake-binary-content" > "${UV9}/binary"
echo "aperod@192.0.2.91" > "${UV9}/validators.conf"

# Writable BINARY_DST (avoids /usr/local/bin)
UV9_BINARY_DST="${UV9}/remote-fs/usr/local/bin/aperod-node"
mkdir -p "$(dirname "${UV9_BINARY_DST}")"
touch "${UV9_BINARY_DST}" && chmod +x "${UV9_BINARY_DST}"

# Fake installed aperod_backup.sh — backup IS configured on this validator.
UV9_INSTALLED="${UV9}/remote-fs/usr/local/bin/aperod_backup.sh"
echo "# OLD BACKUP SCRIPT VERSION" > "${UV9_INSTALLED}"
chmod 700 "${UV9_INSTALLED}"

UV9_REMOTE_TMP="${UV9}/remote-tmp"
mkdir -p "${UV9_REMOTE_TMP}"

# ── Stub: scp — log calls; copy backup.sh to fake remote tmp ─────────────
UV9_SCP_LOG="${UV9}/scp.log"
cat >"${UV9}/fake-bins/scp" <<SCPSTUB
#!/usr/bin/env bash
echo "scp \$*" >> "${UV9_SCP_LOG}"
for arg in "\$@"; do
  if [[ "\$arg" == *"aperod_backup"* && "\$arg" != *":"* ]]; then
    cp "\$arg" "${UV9_REMOTE_TMP}/aperod_backup_new.sh" 2>/dev/null || true
    break
  fi
done
exit 0
SCPSTUB
chmod +x "${UV9}/fake-bins/scp"

# ── Stub: mktemp — always exits 1 and prints nothing ─────────────────────
# Simulates the case where the install directory is not writable by the
# aperod user (e.g. /usr/local/bin owned by root, sudo not available).
cat >"${UV9}/fake-bins/mktemp" <<'MKTEMPSTUB'
#!/usr/bin/env bash
exit 1
MKTEMPSTUB
chmod +x "${UV9}/fake-bins/mktemp"

# ── Stubs: sudo (pass-through), systemctl, curl ───────────────────────────
cat >"${UV9}/fake-bins/sudo" <<'SUDOSTUB'
#!/usr/bin/env bash
exec "$@"
SUDOSTUB
chmod +x "${UV9}/fake-bins/sudo"

cat >"${UV9}/fake-bins/systemctl" <<'SVCSTUB'
#!/usr/bin/env bash
exit 0
SVCSTUB
chmod +x "${UV9}/fake-bins/systemctl"

make_fake_binary "curl" "${UV9}/fake-bins"

# ── Stub: ssh — captures and rewrites the remote script, then runs locally ─
UV9_SCRIPT_LOG="${UV9}/remote_script.txt"
UV9_SSH_RUN_LOG="${UV9}/ssh_run.log"
UV9_FAKE_NODE_NEW="${UV9_REMOTE_TMP}/aperod-node-new"
cp "${UV9}/binary" "${UV9_FAKE_NODE_NEW}"

printf '%s\n' \
  '#!/usr/bin/env bash' \
  'SCRIPT=$(cat)' \
  "echo \"\$SCRIPT\" > '${UV9_SCRIPT_LOG}'" \
  "MODIFIED=\"\${SCRIPT//\/usr\/local\/bin\/aperod_backup.sh/${UV9_INSTALLED//\//\\/}}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod_backup_new.sh/${UV9_REMOTE_TMP//\//\\/}\/aperod_backup_new.sh}\"" \
  "MODIFIED=\"\${MODIFIED//\/tmp\/aperod-node-new/${UV9_FAKE_NODE_NEW//\//\\/}}\"" \
  "PATH='${UV9}/fake-bins':\$PATH bash -s <<< \"\$MODIFIED\" >> '${UV9_SSH_RUN_LOG}' 2>&1" \
  > "${UV9}/fake-bins/ssh"
chmod +x "${UV9}/fake-bins/ssh"

# ── Run update-validator.sh ───────────────────────────────────────────────
(
  export SSH_KEY="${UV9}/id_fake"
  export KNOWN_HOSTS_FILE="${UV9}/known_hosts"
  export BINARY_SRC="${UV9}/binary"
  export BINARY_DST="${UV9_BINARY_DST}"
  export VALIDATORS_CONF="${UV9}/validators.conf"
  export SKIP_HEALTH_CHECK=1
  export HEALTH_MAX_ATTEMPTS=1
  export HEALTH_WAIT_SECS=0
  export SUPPORT_BOT_TOKEN=
  export SUPPORT_ADMIN_CHAT_ID=
  export BLOCKCHAIN_DIR="${SCRIPT_DIR}"

  PATH="${UV9}/fake-bins:$PATH" \
    bash "${UPDATE_VALIDATOR_SH}" >/dev/null 2>&1
) || true

# Assertion 1: /tmp/aperod_backup_new.sh must be gone (rm -f always runs)
UV9_TMP_STAGED="${UV9_REMOTE_TMP}/aperod_backup_new.sh"
if [[ ! -f "${UV9_TMP_STAGED}" ]]; then
  pass "Backup tmp file cleaned up even though mktemp failed (no /tmp artefact left)"
else
  fail "Backup tmp file was NOT cleaned up after mktemp failure (run log: $(cat "${UV9_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

# Assertion 2: warn line about "cannot create staging file" must be emitted
if [[ -f "${UV9_SSH_RUN_LOG}" ]] && grep -q "cannot create staging file" "${UV9_SSH_RUN_LOG}"; then
  pass "Warn 'cannot create staging file' emitted when mktemp fails"
else
  fail "Warn 'cannot create staging file' NOT found in remote script output (log: $(cat "${UV9_SSH_RUN_LOG}" 2>/dev/null | head -20 || echo '<empty>'))"
fi

# Assertion 3: old installed copy must be untouched (install block was skipped)
INSTALLED_CONTENT=$(cat "${UV9_INSTALLED}" 2>/dev/null || echo "<missing>")
if [[ "${INSTALLED_CONTENT}" == "# OLD BACKUP SCRIPT VERSION" ]]; then
  pass "Old installed aperod_backup.sh preserved when mktemp fails (install skipped)"
else
  fail "Installed aperod_backup.sh unexpectedly modified (content: ${INSTALLED_CONTENT})"
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
