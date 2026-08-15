#!/usr/bin/env bash
# =============================================================================
#  test-join-network-identity.sh — Confirm P2P fingerprint isolation after
#  join-network.sh runs.
#
#  Verifies that join-network.sh guarantees the target node never inherits the
#  source node's p2p_identity.key, by testing two complementary defences:
#
#   Defence A — rsync --exclude='p2p_identity.key'
#     The source key is never transferred to the target during rsync.
#
#   Defence B — SSH rm -f <target>/p2p_identity.key (Step 4)
#     Any key already on the target (from a previous install) is deleted so
#     the node generates a fresh identity on first start.
#
#  Test matrix
#  ───────────
#   I1  Normal path — rsync excludes key AND rm -f deletes old target key.
#       Assertions:
#         I1a  join-network.sh exits 0
#         I1b  rsync was called with --exclude=p2p_identity.key
#         I1c  target p2p_identity.key deleted (rm -f ran)
#         I1d  source p2p_identity.key unchanged
#         I1e  target key absent → next node start produces a fresh fingerprint
#
#   I2  Safety-net path — even if rsync copies the key (--exclude removed),
#       the SSH rm -f step in Step 4 still deletes it.
#       Assertions:
#         I2a  join-network.sh exits 0
#         I2b  target p2p_identity.key deleted by the rm -f safety-net
#
#   I3  Fresh-key assertion — stub node generates a new key on startup that
#       differs from the source key.
#       Assertions:
#         I3a  join-network.sh exits 0
#         I3b  source p2p_identity.key intact after run
#         I3c  target key (freshly written by stub node) differs from source key
#
#  All SSH, rsync, systemctl, and sleep calls are stubbed.  No root access,
#  real SSH target, or Docker is required.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-join-network-identity.sh
#
#  Exit codes:
#    0 — all assertions passed
#    1 — one or more assertions failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOIN_SH="${SCRIPT_DIR}/join-network.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BOLD='\033[1m'; NC='\033[0m'

PASS=0
FAIL=0

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Pre-flight ─────────────────────────────────────────────────────────────────
if [[ ! -f "${JOIN_SH}" ]]; then
  echo -e "${RED}[ERR]${NC}  join-network.sh not found at: ${JOIN_SH}" >&2
  exit 1
fi

if ! command -v python3 &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  python3 not found in PATH — skipping." >&2
  exit 0
fi

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

TARGET_IP="192.0.2.55"   # TEST-NET-1 — never routes
PRIMARY_IP="10.0.0.1"

# ── Helpers ────────────────────────────────────────────────────────────────────

make_stub() {
  local dir="$1" cmd="$2" body="$3"
  mkdir -p "$dir"
  printf '#!/usr/bin/env bash\n%s\n' "$body" >"${dir}/${cmd}"
  chmod +x "${dir}/${cmd}"
}

build_systemctl_stub() {
  local dir="$1"
  make_stub "${dir}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)          exit 0 ;;
  *"is-active"*)                 exit 1 ;;   # not active → wait loop exits
  *"start aperod-node"*)         exit 0 ;;
  *"enable"*)                    exit 0 ;;
  *"disable"*)                   exit 0 ;;
  *)                             exit 0 ;;
esac
'
}

# build_ssh_stub BIN_DIR TARGET_DATA_DIR HEIGHT PEER_COUNT [ON_ENABLE_HOOK]
#
# Creates an ssh stub that:
#   • Strip leading "root@IP" argument, then dispatch on the remote command.
#   • "bash" (no extra args) → drain the bootnode-injection heredoc without
#     running it.  The identity test does not test bootnode injection logic
#     (covered by test-join-network-bootnode.sh); draining removes any pyyaml
#     dependency from this test.
#   • Command containing p2p_identity.key → execute rm -f locally against the
#     overridden SECONDARY_DATA_DIR so the test can assert the file is gone.
#   • network/stats curl → return synthetic JSON.
#   • "enable --now aperod-node" → optionally run ON_ENABLE_HOOK (e.g. write
#     a fresh key file to simulate node startup).
#   • Everything else → drain stdin, print success tokens.
build_ssh_stub() {
  local dir="$1" tgt_data="$2" height="$3" peer_count="$4"
  local on_enable="${5:-}"

  make_stub "${dir}" "ssh" "
shift   # drop 'root@IP'
CMD=\"\$*\"

# Step 5: bootnode injection heredoc — drain without running (no pyyaml needed).
if [[ \"\${CMD}\" == 'bash' ]]; then
  cat >/dev/null
  echo '[OK]   bootnode stub'
  exit 0
fi

# Step 4: delete the identity key.  The command contains the target data-dir
# path (now overridden via SECONDARY_DATA_DIR env var to our temp dir).
if echo \"\${CMD}\" | grep -q 'p2p_identity.key'; then
  # The path in the command is the overridden SECONDARY_DATA_DIR value, so we
  # can evaluate it directly as a local shell command (no path translation needed).
  eval \"\${CMD}\" 2>/dev/null || true
  exit 0
fi

# Step 7: health / stats query
if echo \"\${CMD}\" | grep -q 'network/stats'; then
  echo '{\"height\":${height},\"peer_count\":${peer_count},\"syncing\":false}'
  exit 0
fi

# Step 6: enable --now — optionally run the on-enable hook (e.g. write fresh key)
if echo \"\${CMD}\" | grep -q 'enable --now aperod-node'; then
  ${on_enable}
  printf 'started\n'
  exit 0
fi

# verify-dropin.sh: 'systemctl show aperod-node' — return fake output with
# the expected GOMEMLIMIT and TimeoutStopUSec values so the verify step passes.
if echo \"\${CMD}\" | grep -q 'systemctl show aperod-node'; then
  printf 'Environment=GOMEMLIMIT=5905580032\nTimeoutStopUSec=15min\n'
  exit 0
fi

# verify-dropin.sh: 'test -f /etc/systemd/.../gomemlimit.conf && echo yes || echo no'
# and same for timeout.conf — return 'yes' so the drop-in presence check passes.
if echo \"\${CMD}\" | grep -q 'gomemlimit\.conf\|timeout\.conf'; then
  echo 'yes'
  exit 0
fi

# All other SSH commands (disable, chown, ensure-dropin, etc.) — succeed silently
cat >/dev/null 2>&1 || true
printf 'stopped\nremoved\nstarted\n'
exit 0
"
}

# build_rsync_stub BIN_DIR SRC_DIR TGT_DIR LOG_FILE COPY_IDENTITY_KEY
#
# Creates an rsync stub that:
#   • Logs the exact argument list to LOG_FILE.
#   • Copies files from SRC_DIR to TGT_DIR using find+cp (local, no SSH).
#   • When COPY_IDENTITY_KEY=false (normal case): skips p2p_identity.key,
#     mirroring --exclude behaviour.
#   • When COPY_IDENTITY_KEY=true (regression scenario): copies everything
#     including the identity key, simulating a broken rsync without --exclude.
build_rsync_stub() {
  local dir="$1" src="$2" tgt="$3" log="$4" copy_key="${5:-false}"

  local copy_key_code
  if [[ "${copy_key}" == "true" ]]; then
    # Simulate rsync without --exclude: copy everything
    copy_key_code="cp -r '${src}/.' '${tgt}/' 2>/dev/null || true"
  else
    # Simulate rsync with --exclude=p2p_identity.key: skip the key
    copy_key_code="find '${src}/' -maxdepth 1 -mindepth 1 ! -name 'p2p_identity.key' -exec cp -r {} '${tgt}/' \\; 2>/dev/null || true"
  fi

  make_stub "${dir}" "rsync" "
echo \"\$*\" >> '${log}'
mkdir -p '${tgt}'
${copy_key_code}
exit 0
"
}

# Minimal node.yaml accepted by the bootnode-injection heredoc in join-network.sh
write_node_yaml() {
  local path="$1"
  cat >"${path}" <<'YAML'
network: testnet
data_dir: /var/lib/aperod
p2p:
  listen_addr: /ip4/0.0.0.0/tcp/30303
  bootnodes: []
  max_peers: 50
api:
  enabled: true
  listen_addr: 127.0.0.1:8545
genesis:
  file: /etc/aperod/genesis-testnet.yaml
YAML
}

# run_join TARGET_IP BIN_DIR SRC_DATA_DIR TGT_DATA_DIR NODE_YAML
# Sets LAST_EXIT and LAST_OUTPUT.
LAST_OUTPUT=""
LAST_EXIT=0

run_join() {
  local tip="$1" bdir="$2" src="$3" tgt="$4" node_yaml="$5"
  LAST_EXIT=0
  LAST_OUTPUT=$(
    PATH="${bdir}:${PATH}" \
    PRIMARY_IP="${PRIMARY_IP}" \
    PRIMARY_DATA_DIR="${src}" \
    SECONDARY_DATA_DIR="${tgt}" \
    SECONDARY_NODE_YAML="${node_yaml}" \
    SECONDARY_NODE_CONFIG_SH="/nonexistent/node-config.sh" \
    bash "${JOIN_SH}" "${tip}" 2>&1
  ) || LAST_EXIT=$?
}

# =============================================================================
# ── TEST I0: Timeout path — is-active never returns 1 → abort before rsync
# =============================================================================
# systemctl stop exits 0 (pretends to work) but is-active also always exits 0
# (service never actually stops).  The 15-second wait loop exhausts and the
# script must abort with a non-zero exit, leaving rsync uncalled and the source
# p2p_identity.key completely untouched.
section "I0: 15-s timeout — source refuses to stop → abort before rsync; source key intact"

I0_SRC="${TMPDIR_TEST}/i0-src"
I0_TGT="${TMPDIR_TEST}/i0-tgt"
mkdir -p "${I0_SRC}" "${I0_TGT}"

# Seed a known fingerprint in the source — must survive untouched.
echo "source-fingerprint-i0-deadbeef4242" > "${I0_SRC}/p2p_identity.key"
# Add a chain.db so the data-dir check in join-network.sh passes.
mkdir -p "${I0_SRC}/chain.db" && touch "${I0_SRC}/chain.db/CURRENT"

I0_YAML="${TMPDIR_TEST}/i0-node.yaml"
write_node_yaml "${I0_YAML}"

# systemctl stub: stop exits 0 (command accepted) but is-active ALWAYS exits 0
# (service is still running) → the wait loop never exits early → timeout fires.
# The stub is also a SPY: every invocation is appended to I0_SYSCTL_LOG so the
# test can assert that "start aperod-node" fired on the source after the abort
# (the recovery restart + EXIT-trap safety net in join-network.sh).
I0_BIN="${TMPDIR_TEST}/i0-bin"
I0_SYSCTL_LOG="${TMPDIR_TEST}/i0-systemctl.log"
make_stub "${I0_BIN}" "systemctl" "
echo \"\$*\" >> '${I0_SYSCTL_LOG}'
case \"\$*\" in
  *\"stop aperod-node\"*)          exit 0 ;;   # pretend stop command succeeded …
  *\"is-active\"*)                 exit 0 ;;   # … but service is STILL active
  *) exit 0 ;;
esac
"

# sleep stub: no-op so the 15-iteration wait loop finishes instantly.
make_stub "${I0_BIN}" "sleep" 'exit 0'

# rsync stub: writes a sentinel file so we can detect whether it was called.
I0_RSYNC_SENTINEL="${TMPDIR_TEST}/i0-rsync-called"
make_stub "${I0_BIN}" "rsync" "
touch '${I0_RSYNC_SENTINEL}'
exit 0
"

# ssh stub: SSH is only used after the stop/wait phase, but we provide a
# minimal stub in case any early step reaches it (it should not).
make_stub "${I0_BIN}" "ssh" '
shift
echo "stopped"
exit 0
'

run_join "${TARGET_IP}" "${I0_BIN}" "${I0_SRC}" "${I0_TGT}" "${I0_YAML}"

# I0a: script must exit non-zero (abort due to timeout)
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "I0a: join-network.sh exited non-zero (${LAST_EXIT}) after 15-s timeout"
else
  fail "I0a: expected non-zero exit after timeout but got 0. Output:\n${LAST_OUTPUT}"
fi

# I0b: rsync must NOT have been called (LevelDB integrity preserved)
if [[ ! -f "${I0_RSYNC_SENTINEL}" ]]; then
  pass "I0b: rsync was not called — LevelDB not touched after timeout abort"
else
  fail "I0b: rsync was called despite source node never stopping — LevelDB would be corrupted"
fi

# I0c: source p2p_identity.key must be intact and unchanged
if [[ -f "${I0_SRC}/p2p_identity.key" ]]; then
  I0_ACTUAL=$(cat "${I0_SRC}/p2p_identity.key")
  if [[ "${I0_ACTUAL}" == "source-fingerprint-i0-deadbeef4242" ]]; then
    pass "I0c: source p2p_identity.key unchanged after abort"
  else
    fail "I0c: source p2p_identity.key content changed unexpectedly: ${I0_ACTUAL}"
  fi
else
  fail "I0c: source p2p_identity.key is missing after abort — script damaged the source"
fi

# I0d: the source node must be restarted after the timeout abort.
# join-network.sh has two defences: the inline recovery `systemctl start` right
# before the timeout `die`, and the unconditional EXIT-trap safety net.  Either
# way, the spy stub must have recorded at least one "start aperod-node" call
# AFTER the stop/is-active sequence — otherwise the source is left down.
if grep -q 'start aperod-node' "${I0_SYSCTL_LOG}" 2>/dev/null; then
  # Ensure the start happened after the stop (recovery, not a stray pre-start).
  I0_STOP_LINE=$(grep -n 'stop aperod-node' "${I0_SYSCTL_LOG}" | head -1 | cut -d: -f1)
  I0_START_LINE=$(grep -n 'start aperod-node' "${I0_SYSCTL_LOG}" | grep -v 'stop' | tail -1 | cut -d: -f1)
  if [[ -n "${I0_STOP_LINE}" && -n "${I0_START_LINE}" && ${I0_START_LINE} -gt ${I0_STOP_LINE} ]]; then
    pass "I0d: source aperod-node restarted after timeout abort (start recorded after stop)"
  else
    fail "I0d: 'start aperod-node' recorded but not after the stop (stop line=${I0_STOP_LINE:-none}, start line=${I0_START_LINE:-none})"
  fi
else
  fail "I0d: 'start aperod-node' never invoked on source after timeout abort — source node left stopped. systemctl log: $(cat "${I0_SYSCTL_LOG}" 2>/dev/null || echo '(empty)')"
fi

# =============================================================================
# ── TEST I1: Normal path — rsync excludes key AND rm -f deletes old target key
# =============================================================================
section "I1: Normal path — --exclude keeps source key local; SSH rm -f wipes target key"

I1_SRC="${TMPDIR_TEST}/i1-src"
I1_TGT="${TMPDIR_TEST}/i1-tgt"
mkdir -p "${I1_SRC}" "${I1_TGT}"

# Seed a known key in source (primary node's identity)
echo "source-fingerprint-aabbccdd1122" > "${I1_SRC}/p2p_identity.key"
# Seed a different key in target (pre-existing from a prior install)
echo "old-target-fingerprint-deadbeef9999" > "${I1_TGT}/p2p_identity.key"
# Add a chain-db file so rsync has non-key content to copy
mkdir -p "${I1_SRC}/chain.db" && touch "${I1_SRC}/chain.db/CURRENT"

I1_YAML="${TMPDIR_TEST}/i1-node.yaml"
write_node_yaml "${I1_YAML}"

I1_LOG="${TMPDIR_TEST}/i1-rsync.log"
I1_BIN="${TMPDIR_TEST}/i1-bin"
build_systemctl_stub "${I1_BIN}"
build_ssh_stub       "${I1_BIN}" "${I1_TGT}" 42000 1
build_rsync_stub     "${I1_BIN}" "${I1_SRC}" "${I1_TGT}" "${I1_LOG}" false
make_stub "${I1_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${I1_BIN}" "${I1_SRC}" "${I1_TGT}" "${I1_YAML}"

# I1a: script must exit 0
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "I1a: join-network.sh exited 0"
else
  fail "I1a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# I1b: rsync must have been called with --exclude=p2p_identity.key
if grep -q -- '--exclude=p2p_identity.key' "${I1_LOG}" 2>/dev/null; then
  pass "I1b: rsync called with --exclude=p2p_identity.key"
else
  fail "I1b: rsync NOT called with --exclude=p2p_identity.key. rsync args: $(cat "${I1_LOG}" 2>/dev/null || echo '(log empty)')"
fi

# I1c: target key must be gone (Step 4 SSH rm -f ran)
if [[ ! -f "${I1_TGT}/p2p_identity.key" ]]; then
  pass "I1c: target p2p_identity.key deleted by SSH rm -f (Step 4)"
else
  fail "I1c: target p2p_identity.key still present — SSH rm -f did not fire"
fi

# I1d: source key must be intact and unchanged
if [[ -f "${I1_SRC}/p2p_identity.key" ]]; then
  SRC_ACTUAL=$(cat "${I1_SRC}/p2p_identity.key")
  if [[ "${SRC_ACTUAL}" == "source-fingerprint-aabbccdd1122" ]]; then
    pass "I1d: source p2p_identity.key unchanged (rsync --exclude preserved it)"
  else
    fail "I1d: source p2p_identity.key content changed unexpectedly: ${SRC_ACTUAL}"
  fi
else
  fail "I1d: source p2p_identity.key was deleted — rsync damaged the source"
fi

# I1e: target key absent → node generates fresh identity on first start
if [[ ! -f "${I1_TGT}/p2p_identity.key" ]]; then
  pass "I1e: target has no key → node will generate a fresh fingerprint (cannot match source)"
else
  fail "I1e: target key still exists — fingerprint collision risk remains"
fi

# =============================================================================
# ── TEST I2: Safety-net — SSH rm -f still fires even if rsync copied the key
# =============================================================================
section "I2: Safety-net — SSH rm -f removes key even if rsync would have copied it"
#
# This test simulates a regression where --exclude is accidentally removed from
# the rsync command: the key IS copied to the target by the rsync stub.
# We then verify that Step 4's SSH rm -f deletes it anyway.

I2_SRC="${TMPDIR_TEST}/i2-src"
I2_TGT="${TMPDIR_TEST}/i2-tgt"
mkdir -p "${I2_SRC}" "${I2_TGT}"

echo "source-key-i2-cafebabe" > "${I2_SRC}/p2p_identity.key"
mkdir -p "${I2_SRC}/chain.db" && touch "${I2_SRC}/chain.db/CURRENT"

I2_YAML="${TMPDIR_TEST}/i2-node.yaml"
write_node_yaml "${I2_YAML}"

I2_LOG="${TMPDIR_TEST}/i2-rsync.log"
I2_BIN="${TMPDIR_TEST}/i2-bin"
build_systemctl_stub "${I2_BIN}"
build_ssh_stub       "${I2_BIN}" "${I2_TGT}" 99999 2
# "Bad" rsync stub: copies p2p_identity.key (simulates missing --exclude)
build_rsync_stub     "${I2_BIN}" "${I2_SRC}" "${I2_TGT}" "${I2_LOG}" true
make_stub "${I2_BIN}" "sleep" 'exit 0'

run_join "${TARGET_IP}" "${I2_BIN}" "${I2_SRC}" "${I2_TGT}" "${I2_YAML}"

# I2a: script exits 0 even with naive rsync
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "I2a: join-network.sh exited 0 with naive rsync (no --exclude)"
else
  fail "I2a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# I2b: SSH rm -f must still remove the key that rsync copied
if [[ ! -f "${I2_TGT}/p2p_identity.key" ]]; then
  pass "I2b: target p2p_identity.key removed by SSH rm -f safety-net (Step 4)"
else
  fail "I2b: target p2p_identity.key persists — SSH rm -f safety-net did not fire; shared fingerprint risk!"
fi

# =============================================================================
# ── TEST I3: Fresh-key assertion — stub node generates a distinct key
# =============================================================================
section "I3: Freshly-generated target key differs from source key"
#
# The SSH stub's "enable --now aperod-node" handler writes a new random key to
# the target data dir, simulating what a real node does on first start when
# no p2p_identity.key is present.  We then assert the two key files differ.

I3_SRC="${TMPDIR_TEST}/i3-src"
I3_TGT="${TMPDIR_TEST}/i3-tgt"
mkdir -p "${I3_SRC}" "${I3_TGT}"

echo "source-key-fingerprint-cafebabe0011" > "${I3_SRC}/p2p_identity.key"
echo "old-target-key-to-be-deleted"        > "${I3_TGT}/p2p_identity.key"
mkdir -p "${I3_SRC}/chain.db" && touch "${I3_SRC}/chain.db/CURRENT"

I3_YAML="${TMPDIR_TEST}/i3-node.yaml"
write_node_yaml "${I3_YAML}"

I3_LOG="${TMPDIR_TEST}/i3-rsync.log"
I3_BIN="${TMPDIR_TEST}/i3-bin"
build_systemctl_stub "${I3_BIN}"
build_rsync_stub     "${I3_BIN}" "${I3_SRC}" "${I3_TGT}" "${I3_LOG}" false
make_stub "${I3_BIN}" "sleep" 'exit 0'

# SSH stub for I3: the "enable --now" handler writes a freshly-generated key
# (random hex suffix) to the target dir, just like a real node would.
FRESH_KEY_FILE="${I3_TGT}/p2p_identity.key"
make_stub "${I3_BIN}" "ssh" "
shift   # drop 'root@IP'
CMD=\"\$*\"

if [[ \"\${CMD}\" == 'bash' ]]; then
  cat >/dev/null; echo '[OK]   bootnode stub'; exit 0
fi

if echo \"\${CMD}\" | grep -q 'p2p_identity.key'; then
  eval \"\${CMD}\" 2>/dev/null || true
  exit 0
fi

if echo \"\${CMD}\" | grep -q 'network/stats'; then
  echo '{\"height\":55000,\"peer_count\":1,\"syncing\":false}'
  exit 0
fi

# verify-dropin.sh: systemctl show + drop-in file checks
if echo \"\${CMD}\" | grep -q 'systemctl show aperod-node'; then
  printf 'Environment=GOMEMLIMIT=5905580032\nTimeoutStopUSec=15min\n'
  exit 0
fi
if echo \"\${CMD}\" | grep -q 'gomemlimit\.conf\|timeout\.conf'; then
  echo 'yes'
  exit 0
fi

# Simulate node generating a fresh random identity on first start
if echo \"\${CMD}\" | grep -q 'enable --now aperod-node'; then
  RAND=\$(od -An -N8 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n' || echo '99887766aabbccdd')
  echo \"fresh-target-fingerprint-\${RAND}\" > '${FRESH_KEY_FILE}'
  printf 'started\n'
  exit 0
fi

cat >/dev/null 2>&1 || true
printf 'stopped\nremoved\nstarted\n'
exit 0
"

run_join "${TARGET_IP}" "${I3_BIN}" "${I3_SRC}" "${I3_TGT}" "${I3_YAML}"

# I3a: script exits 0
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "I3a: join-network.sh exited 0"
else
  fail "I3a: expected exit 0, got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# I3b: source key intact
if [[ -f "${I3_SRC}/p2p_identity.key" ]]; then
  pass "I3b: source p2p_identity.key still present"
else
  fail "I3b: source p2p_identity.key missing after run"
fi

# I3c: fresh target key must differ from source key
if [[ -f "${I3_TGT}/p2p_identity.key" ]]; then
  SRC_CONTENT=$(cat "${I3_SRC}/p2p_identity.key")
  TGT_CONTENT=$(cat "${I3_TGT}/p2p_identity.key")
  if [[ "${SRC_CONTENT}" != "${TGT_CONTENT}" ]]; then
    pass "I3c: fresh target key differs from source key — no shared P2P fingerprint"
  else
    fail "I3c: target key equals source key — P2P fingerprint collision!"
  fi
else
  # Key absent is also acceptable: node will generate a fresh one on actual start.
  pass "I3c: target has no key — node will generate a distinct fingerprint on first start"
fi

# =============================================================================
# ── Summary ────────────────────────────────────────────────────────────────────
# =============================================================================
echo ""
echo "──────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ ${FAIL} -eq 0 ]]; then
  echo -e "${GREEN}All ${TOTAL} assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}${FAIL} of ${TOTAL} assertions FAILED.${NC}"
  exit 1
fi
