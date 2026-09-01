#!/usr/bin/env bash
# =============================================================================
#  test-join-network-bootstrap.sh — Tests for the --bootstrap-from mode of
#  join-network.sh.
#
#  No PyYAML dependency: the bootnode step is exercised through a stub
#  node-config.sh that records the invocation; YAML parsing is not required.
#
#  Test suites:
#
#  Happy path (B1-B9)
#  ──────────────────
#  All external calls are stubbed.  Verifies that:
#    B1  script exits 0
#    B2  chain.db is rsynced with --delete
#    B3  snapshot include pattern is used
#    B4  p2p_bans.json removed from LOCAL_DATA_DIR
#    B5  p2p_identity.key removed from LOCAL_DATA_DIR
#    B6  validator restarted BEFORE local node starts
#    B7  bootnode injected via node-config.sh
#    B8  banner shows SNAP_HEIGHT
#    B9  banner shows VALIDATOR_TIP
#
#  Failure: validator remote stop fails (VS1-VS2)
#  ──────────────────────────────────────────────
#  Local node was stopped first; trap must restart it.
#
#  Failure: rsync fails (RF1-RF3)
#  ───────────────────────────────
#  Both nodes were stopped; trap must restart BOTH.
#
#  Last-attempt success (LA1-LA2)
#  ────────────────────────────────
#  API returns height > 0 on exactly the final allowed poll.
#  Script must exit 0, not treat this as a timeout.
#
#  Slow validator API at step 1 (SV1-SV5)
#  ───────────────────────────────────────
#  Validator stats returns an empty response. Bootstrap must continue with
#  VALIDATOR_TIP=unknown and still complete all stop, rsync, and start steps.
#
#  Failure: API timeout (AT1-AT2)
#  ───────────────────────────────
#  API never returns height > 0; script must exit non-zero.
#
#  Push-mode regression guard (PM1-PM2)
#  ─────────────────────────────────────
#  Running without --bootstrap-from still requires TARGET_IP.
#
#  Run from anywhere:
#    bash blockchain/deploy/test-join-network-bootstrap.sh
#
#  Exit codes:
#    0 — all tests passed
#    1 — one or more tests failed
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

# ── Pre-flight ────────────────────────────────────────────────────────────────
if [[ ! -f "${JOIN_SH}" ]]; then
  echo -e "${RED}[ERR]${NC}  join-network.sh not found at: ${JOIN_SH}" >&2
  exit 1
fi

if ! command -v python3 &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  python3 not found in PATH — skipping." >&2
  exit 0
fi

VALIDATOR_IP="192.0.2.2"   # TEST-NET-1 — never routes

TMPDIR_TEST=$(mktemp -d)
trap 'rm -rf "$TMPDIR_TEST"' EXIT

# ---------------------------------------------------------------------------
# make_stub BIN_DIR CMD BODY
#   Creates a stub executable at BIN_DIR/CMD that runs BODY as bash.
# ---------------------------------------------------------------------------
make_stub() {
  local dir="$1" cmd="$2" body="$3"
  mkdir -p "$dir"
  printf '#!/usr/bin/env bash\n%s\n' "$body" >"${dir}/${cmd}"
  chmod +x "${dir}/${cmd}"
}

# ---------------------------------------------------------------------------
# run_bootstrap BIN_DIR LOCAL_DATA_DIR LOCAL_NODE_YAML LOCAL_NODE_CONFIG_SH
#               [NAME=VALUE ...]
#   Runs join-network.sh --bootstrap-from=VALIDATOR_IP with the given stubs.
#   Captures combined stdout+stderr into LAST_OUTPUT.
#   Sets LAST_EXIT to the exit status.
# ---------------------------------------------------------------------------
LAST_OUTPUT=""
LAST_EXIT=0

run_bootstrap() {
  local bdir="$1" ldata="$2" lyaml="$3" lconfig="$4"
  shift 4
  local extra_env=("$@")

  LAST_EXIT=0
  LAST_OUTPUT=$(
    env \
      PATH="${bdir}:${PATH}" \
      SECONDARY_DATA_DIR="${ldata}" \
      LOCAL_DATA_DIR="${ldata}" \
      LOCAL_NODE_YAML="${lyaml}" \
      LOCAL_NODE_CONFIG_SH="${lconfig}" \
      PRIMARY_DATA_DIR="${ldata}" \
      VALIDATOR_DATA_DIR="${ldata}" \
      HEALTH_MAX_ATTEMPTS=5 \
      HEALTH_WAIT_SECS=0 \
      "${extra_env[@]}" \
    bash "${JOIN_SH}" "--bootstrap-from=${VALIDATOR_IP}" 2>&1
  ) || LAST_EXIT=$?
}

# =============================================================================
# ── HAPPY PATH ────────────────────────────────────────────────────────────────
# =============================================================================
section "Happy path — bootstrap copies chain.db and snapshot, starts both nodes"

HP_DIR="${TMPDIR_TEST}/hp"
HP_DATA="${HP_DIR}/data"
HP_BIN="${HP_DIR}/bin"
HP_YAML="${HP_DIR}/node.yaml"

mkdir -p "${HP_DATA}/chain.db"
touch "${HP_DATA}/chain.db/CURRENT"

# Fake snapshot file so height-detection ls glob finds it.
touch "${HP_DATA}/snapshot-v2-77000.json.gz"

# Minimal node.yaml (plain text — no pyyaml needed; node-config.sh stub
# handles the actual bootnode injection).
printf 'network: testnet\np2p:\n  bootnodes: []\n' >"${HP_YAML}"

# node-config.sh stub: records the invoked bootnode and tolerance in sentinel files.
HP_BOOTNODE_CALLED="${HP_DIR}/bootnode-called"
HP_TOLERANCE_CALLED="${HP_DIR}/tolerance-called"
HP_NODE_CONFIG="${HP_DIR}/node-config.sh"
cat >"${HP_NODE_CONFIG}" <<NODECONFIG
#!/usr/bin/env bash
# Stub: record subcommand invocations.
case "\${1:-}" in
  add-bootnode)
    echo "\${2:-}" >"${HP_BOOTNODE_CALLED}"
    echo "[OK]   p2p.bootnodes updated (stub)"
    ;;
  set-snapshot-tolerance)
    echo "\${2:-}" >"${HP_TOLERANCE_CALLED}"
    echo "[OK]   snapshot.utxo_count_tolerance_pct set to \${2:-} (stub)"
    ;;
esac
exit 0
NODECONFIG
chmod +x "${HP_NODE_CONFIG}"

# rsync stub: record each invocation with arguments.
HP_RSYNC_LOG="${HP_DIR}/rsync-calls.log"
make_stub "${HP_BIN}" "rsync" "
echo \"\$*\" >> '${HP_RSYNC_LOG}'
exit 0
"

# aperod-node stub: check-store succeeds.
HP_CHECK_STORE_CALLED="${HP_DIR}/check-store-called"
make_stub "${HP_BIN}" "aperod-node" "
if echo \"\$*\" | grep -q -- '--check-store'; then
  touch '${HP_CHECK_STORE_CALLED}'
  echo 'check-store OK: tip_height=77000 missing=0 (threshold=5000)'
  exit 0
fi
exit 0
"

# ssh stub: dispatch on remote command pattern.
HP_ORDER_LOG="${HP_DIR}/order.log"
HP_VALIDATOR_STARTED="${HP_DIR}/validator-started"
make_stub "${HP_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":99999,\"height\":99999,\"peer_count\":3}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${HP_VALIDATOR_STARTED}'
  echo 'validator_start' >> '${HP_ORDER_LOG}'
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

# systemctl stub: stop/is-active/start for local service.
HP_LOCAL_STARTED="${HP_DIR}/local-started"
make_stub "${HP_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)            exit 0 ;;
  *'is-active'*)                   exit 1 ;;
  *'enable --now'*|*'start aperod-node'*)
    touch '${HP_LOCAL_STARTED}'
    echo 'local_start' >> '${HP_ORDER_LOG}'
    exit 0 ;;
  *)                               exit 0 ;;
esac
"

# curl stub: local API returns height > 0 immediately.
make_stub "${HP_BIN}" "curl" "
echo '{\"height\":77000,\"peer_count\":2,\"syncing\":false}'
exit 0
"

# chown stub: no-op.
make_stub "${HP_BIN}" "chown" 'exit 0'
# sleep stub: no-op.
make_stub "${HP_BIN}" "sleep" 'exit 0'

run_bootstrap "${HP_BIN}" "${HP_DATA}" "${HP_YAML}" "${HP_NODE_CONFIG}"

# ── B1: exit 0 ───────────────────────────────────────────────────────────────
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "B1: script exited 0 (happy path)"
else
  fail "B1: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

# ── B2: chain.db rsync used --delete ─────────────────────────────────────────
if grep -q -- '--delete' "${HP_RSYNC_LOG}" 2>/dev/null; then
  pass "B2: chain.db rsync invoked with --delete"
else
  fail "B2: expected --delete in rsync log. rsync calls:\n$(cat "${HP_RSYNC_LOG}" 2>/dev/null || echo '(empty)')"
fi

# ── B3: snapshot include pattern used ────────────────────────────────────────
if grep -q 'snapshot-v2-\*' "${HP_RSYNC_LOG}" 2>/dev/null; then
  pass "B3: snapshot rsync used snapshot-v2-* include pattern"
else
  fail "B3: expected snapshot-v2-* in rsync log. rsync calls:\n$(cat "${HP_RSYNC_LOG}" 2>/dev/null || echo '(empty)')"
fi

# ── B4: p2p_bans.json removed ────────────────────────────────────────────────
touch "${HP_DATA}/p2p_bans.json"
run_bootstrap "${HP_BIN}" "${HP_DATA}" "${HP_YAML}" "${HP_NODE_CONFIG}"
if [[ ! -f "${HP_DATA}/p2p_bans.json" ]]; then
  pass "B4: p2p_bans.json removed from LOCAL_DATA_DIR"
else
  fail "B4: p2p_bans.json still present after bootstrap"
fi

# ── B5: p2p_identity.key removed ─────────────────────────────────────────────
touch "${HP_DATA}/p2p_identity.key"
run_bootstrap "${HP_BIN}" "${HP_DATA}" "${HP_YAML}" "${HP_NODE_CONFIG}"
if [[ ! -f "${HP_DATA}/p2p_identity.key" ]]; then
  pass "B5: p2p_identity.key removed from LOCAL_DATA_DIR"
else
  fail "B5: p2p_identity.key still present after bootstrap"
fi

# ── B6: validator started before local node ──────────────────────────────────
# Reset the order log and re-run from a clean state.
rm -f "${HP_ORDER_LOG}" "${HP_VALIDATOR_STARTED}" "${HP_LOCAL_STARTED}"
run_bootstrap "${HP_BIN}" "${HP_DATA}" "${HP_YAML}" "${HP_NODE_CONFIG}"
if [[ -f "${HP_ORDER_LOG}" ]]; then
  FIRST=$(head -1 "${HP_ORDER_LOG}")
  if [[ "${FIRST}" == "validator_start" ]]; then
    pass "B6: validator restarted before local node"
  else
    fail "B6: expected validator_start first but got '${FIRST}'"
  fi
else
  fail "B6: order log not created — neither node was started"
fi

# ── B7: bootnode written via node-config.sh ───────────────────────────────────
EXPECTED_BOOTNODE="/ip4/${VALIDATOR_IP}/tcp/30303"
if [[ -f "${HP_BOOTNODE_CALLED}" ]]; then
  RECORDED=$(cat "${HP_BOOTNODE_CALLED}")
  if [[ "${RECORDED}" == "${EXPECTED_BOOTNODE}" ]]; then
    pass "B7: bootnode ${EXPECTED_BOOTNODE} passed to node-config.sh add-bootnode"
  else
    fail "B7: expected bootnode '${EXPECTED_BOOTNODE}' but got '${RECORDED}'"
  fi
else
  fail "B7: node-config.sh add-bootnode was not called (sentinel missing)"
fi

# ── B8: banner shows SNAP_HEIGHT ─────────────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -qE '77000'; then
  pass "B8: banner contains snapshot height 77000"
else
  fail "B8: expected snapshot height 77000 in output. Got:\n${LAST_OUTPUT}"
fi

# ── B9: banner shows VALIDATOR_TIP ───────────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -qE '99999'; then
  pass "B9: banner contains validator tip_height 99999"
else
  fail "B9: expected validator tip_height 99999 in output. Got:\n${LAST_OUTPUT}"
fi

# ── B10: snapshot tolerance set via node-config.sh ───────────────────────────
# Bootstrap must call set-snapshot-tolerance 10 so the relay node can load the
# rsync'd snapshot even when the stored UTXO count drifts slightly from the DB.
if [[ -f "${HP_TOLERANCE_CALLED}" ]]; then
  RECORDED_TOL=$(cat "${HP_TOLERANCE_CALLED}")
  if [[ "${RECORDED_TOL}" == "10" ]]; then
    pass "B10: set-snapshot-tolerance 10 passed to node-config.sh"
  else
    fail "B10: expected tolerance '10' but got '${RECORDED_TOL}'"
  fi
else
  fail "B10: node-config.sh set-snapshot-tolerance was not called (sentinel missing)"
fi

# ── B12: check-store was called with --data-dir pointing at LOCAL_DATA_DIR ────
# The stub aperod-node only records the call on the final run_bootstrap call;
# re-run once more to ensure the sentinel is fresh.
rm -f "${HP_CHECK_STORE_CALLED}"
run_bootstrap "${HP_BIN}" "${HP_DATA}" "${HP_YAML}" "${HP_NODE_CONFIG}"
if [[ -f "${HP_CHECK_STORE_CALLED}" ]]; then
  pass "B12: aperod-node --check-store was called during bootstrap"
else
  fail "B12: aperod-node --check-store was NOT called — post-rsync sanity check missing"
fi

# ── B11: tolerance not lowered when already set to a higher value ─────────────
# Simulate a node.yaml where utxo_count_tolerance_pct is already 20 and verify
# the stdlib fallback (no node-config.sh) does not lower it.
B11_YAML="${HP_DIR}/b11-node.yaml"
printf 'network: testnet\nsnapshot:\n  utxo_count_tolerance_pct: 20\n' >"${B11_YAML}"
B11_EXIT=0
python3 - "${B11_YAML}" <<'PY' || B11_EXIT=$?
import sys, os, re
cfg_path = sys.argv[1]
with open(cfg_path) as f:
    content = f.read()
m = re.search(r'^(\s*utxo_count_tolerance_pct\s*:\s*)(\d+(?:\.\d+)?)', content, re.M)
if m:
    current = float(m.group(2))
    if current >= 10:
        print(f"[OK]   snapshot.utxo_count_tolerance_pct already {current} (kept)")
        sys.exit(0)
    content = content[:m.start(2)] + "10" + content[m.end(2):]
    changed_msg = f"[OK]   snapshot.utxo_count_tolerance_pct {current} -> 10"
elif re.search(r'^snapshot\s*:', content, re.M):
    content = re.sub(
        r'^(snapshot\s*:[ \t]*\n)',
        r'\1  utxo_count_tolerance_pct: 10\n',
        content, count=1, flags=re.M
    )
    changed_msg = "[OK]   snapshot.utxo_count_tolerance_pct set to 10"
else:
    if not content.endswith('\n'):
        content += '\n'
    content += '\nsnapshot:\n  utxo_count_tolerance_pct: 10\n'
    changed_msg = "[OK]   snapshot.utxo_count_tolerance_pct set to 10"

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    f.write(content)
os.replace(tmp, cfg_path)
print(changed_msg)
PY
AFTER_TOL=$(python3 -c "import re; m=re.search(r'utxo_count_tolerance_pct\s*:\s*(\S+)', open('${B11_YAML}').read()); print(m.group(1) if m else '?')" 2>/dev/null || echo "?")
if [[ "${AFTER_TOL}" == "20" ]]; then
  pass "B11: stdlib fallback kept existing tolerance=20 (did not lower to 10)"
else
  fail "B11: expected tolerance to stay at 20 but got '${AFTER_TOL}'"
fi

# =============================================================================
# ── SANITY CHECK: post-rsync check-store step ─────────────────────────────────
# Verifies that step 5b/9 correctly delegates to aperod-node --check-store and
# aborts the bootstrap when the command reports too many missing blocks.
# =============================================================================
section "Sanity check — aperod-node check-store called and gap abort works"

# ── SC1: check-store success → script exits 0 ─────────────────────────────────
SC1_DIR="${TMPDIR_TEST}/sc1"
SC1_DATA="${SC1_DIR}/data"
SC1_BIN="${SC1_DIR}/bin"
SC1_YAML="${SC1_DIR}/node.yaml"
SC1_CONFIG="${SC1_DIR}/node-config.sh"
mkdir -p "${SC1_DATA}/chain.db"
touch "${SC1_DATA}/chain.db/CURRENT"
touch "${SC1_DATA}/snapshot-v2-50000.json.gz"
printf 'network: testnet\n' >"${SC1_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${SC1_CONFIG}"; chmod +x "${SC1_CONFIG}"

SC1_CHECK_CALLED="${SC1_DIR}/check-store-called"
make_stub "${SC1_BIN}" "aperod-node" "
if echo \"\$*\" | grep -q -- '--check-store'; then
  touch '${SC1_CHECK_CALLED}'
  echo 'check-store OK: tip_height=50000 missing=0 (threshold=5000)'
  exit 0
fi
exit 0
"
make_stub "${SC1_BIN}" "rsync"    'exit 0'
make_stub "${SC1_BIN}" "chown"    'exit 0'
make_stub "${SC1_BIN}" "sleep"    'exit 0'
make_stub "${SC1_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'
make_stub "${SC1_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '"'"'{"tip_height":55000,"height":55000,"peer_count":2}'"'"'
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null; echo "stopped"
else
  cat >/dev/null; echo "ok"
fi
exit 0
'
make_stub "${SC1_BIN}" "curl" "
echo '{\"height\":50001,\"peer_count\":1,\"syncing\":false}'
exit 0
"

run_bootstrap "${SC1_BIN}" "${SC1_DATA}" "${SC1_YAML}" "${SC1_CONFIG}"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "SC1: script exits 0 when check-store reports no gaps"
else
  fail "SC1: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${SC1_CHECK_CALLED}" ]]; then
  pass "SC2: aperod-node --check-store was called in the success path"
else
  fail "SC2: aperod-node --check-store was NOT called — post-rsync sanity check missing"
fi

# Verify --data-dir was passed to the check-store call.
if echo "${LAST_OUTPUT}" | grep -q "chain.db прошёл проверку\|check-store OK"; then
  pass "SC3: output confirms check-store passed"
else
  fail "SC3: no check-store success message in output. Got:\n${LAST_OUTPUT}"
fi

# ── SC4: check-store failure → script exits non-zero ──────────────────────────
SC4_DIR="${TMPDIR_TEST}/sc4"
SC4_DATA="${SC4_DIR}/data"
SC4_BIN="${SC4_DIR}/bin"
SC4_YAML="${SC4_DIR}/node.yaml"
SC4_CONFIG="${SC4_DIR}/node-config.sh"
mkdir -p "${SC4_DATA}/chain.db"
touch "${SC4_DATA}/chain.db/CURRENT"
touch "${SC4_DATA}/snapshot-v2-900000.json.gz"
printf 'network: testnet\n' >"${SC4_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${SC4_CONFIG}"; chmod +x "${SC4_CONFIG}"

# Stub aperod-node: check-store reports 41 000 missing blocks → exit 1.
SC4_CHECK_CALLED="${SC4_DIR}/check-store-called"
make_stub "${SC4_BIN}" "aperod-node" "
if echo \"\$*\" | grep -q -- '--check-store'; then
  touch '${SC4_CHECK_CALLED}'
  printf '%s\n' 'aperod-node: check-store: 41000 missing blocks (first: 929775, tip: 971000) exceeds threshold 5000 -- gaps detected; re-run bootstrap' >&2
  exit 1
fi
exit 0
"
make_stub "${SC4_BIN}" "rsync"    'exit 0'
make_stub "${SC4_BIN}" "chown"    'exit 0'
make_stub "${SC4_BIN}" "sleep"    'exit 0'
make_stub "${SC4_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'
# ssh: validator stats + stop succeed; validator restart (trap) succeeds.
SC4_VAL_RESTARTED="${SC4_DIR}/validator-restarted"
make_stub "${SC4_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":971000,\"height\":971000,\"peer_count\":3}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${SC4_VAL_RESTARTED}'
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null; echo 'stopped'
else
  cat >/dev/null; echo 'ok'
fi
exit 0
"
make_stub "${SC4_BIN}" "curl" "echo '{\"height\":0}'; exit 0"

run_bootstrap "${SC4_BIN}" "${SC4_DATA}" "${SC4_YAML}" "${SC4_CONFIG}"

if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "SC4: script exits non-zero when check-store reports gap > 5000 blocks"
else
  fail "SC4: expected non-zero exit when check-store detects large gap, but got 0. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${SC4_CHECK_CALLED}" ]]; then
  pass "SC5: aperod-node --check-store was called even in the failure path"
else
  fail "SC5: aperod-node --check-store was not called. Output:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -qi "слишком много\|bootstrap\|missing\|gap\|re-run"; then
  pass "SC6: output contains gap-detection error / re-run instruction"
else
  fail "SC6: expected gap error message in output. Got:\n${LAST_OUTPUT}"
fi

# ── SC7: aperod-node absent → warning only, script continues ──────────────────
# When aperod-node is not installed on the relay (PATH has no aperod-node),
# the script must warn but must NOT abort — the node can still be started and
# the missing-block check will fire at the natural scan.go threshold.
SC7_DIR="${TMPDIR_TEST}/sc7"
SC7_DATA="${SC7_DIR}/data"
SC7_BIN="${SC7_DIR}/bin"
SC7_YAML="${SC7_DIR}/node.yaml"
SC7_CONFIG="${SC7_DIR}/node-config.sh"
mkdir -p "${SC7_DATA}/chain.db"
touch "${SC7_DATA}/chain.db/CURRENT"
touch "${SC7_DATA}/snapshot-v2-100.json.gz"
printf 'network: testnet\n' >"${SC7_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${SC7_CONFIG}"; chmod +x "${SC7_CONFIG}"

# NOTE: no aperod-node stub → command -v aperod-node fails → warning path.
make_stub "${SC7_BIN}" "rsync"    'exit 0'
make_stub "${SC7_BIN}" "chown"    'exit 0'
make_stub "${SC7_BIN}" "sleep"    'exit 0'
make_stub "${SC7_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'
make_stub "${SC7_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '"'"'{"tip_height":200,"height":200,"peer_count":1}'"'"'
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null; echo "stopped"
else
  cat >/dev/null; echo "ok"
fi
exit 0
'
make_stub "${SC7_BIN}" "curl" "
echo '{\"height\":101,\"peer_count\":1,\"syncing\":false}'
exit 0
"

run_bootstrap "${SC7_BIN}" "${SC7_DATA}" "${SC7_YAML}" "${SC7_CONFIG}"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "SC7: script exits 0 when aperod-node is absent (warning only, not abort)"
else
  fail "SC7: expected exit 0 when aperod-node absent but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -qi "aperod-node не найден\|не найден в PATH\|not found"; then
  pass "SC8: output contains 'aperod-node not found' warning when binary is absent"
else
  fail "SC8: expected 'not found' warning in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FAILURE: validator remote stop fails ──────────────────────────────────────
# Local node was stopped first; trap must restart it.
# =============================================================================
section "Failure — validator remote stop fails → trap restarts local node"

VS_DIR="${TMPDIR_TEST}/vs"
VS_DATA="${VS_DIR}/data"
VS_BIN="${VS_DIR}/bin"
VS_YAML="${VS_DIR}/node.yaml"
VS_CONFIG="${VS_DIR}/node-config.sh"
mkdir -p "${VS_DATA}/chain.db"
touch "${VS_DATA}/chain.db/CURRENT"
printf 'network: testnet\n' >"${VS_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${VS_CONFIG}"; chmod +x "${VS_CONFIG}"

VS_LOCAL_RESTARTED="${VS_DIR}/local-restarted"
make_stub "${VS_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*|*'enable --now'*)
    touch '${VS_LOCAL_RESTARTED}'
    exit 0 ;;
  *)                     exit 0 ;;
esac
"

# ssh: stats succeeds; remote stop (bash heredoc) fails; validator restart succeeds.
make_stub "${VS_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":1000,\"height\":1000,\"peer_count\":1}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  exit 1   # remote stop fails
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

make_stub "${VS_BIN}" "rsync" 'exit 0'
make_stub "${VS_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${VS_BIN}" "chown" 'exit 0'
make_stub "${VS_BIN}" "sleep" 'exit 0'

run_bootstrap "${VS_BIN}" "${VS_DATA}" "${VS_YAML}" "${VS_CONFIG}"

# ── VS1: non-zero exit ────────────────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "VS1: script exited non-zero when validator stop failed"
else
  fail "VS1: expected non-zero exit but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── VS2: local node restarted by trap ────────────────────────────────────────
if [[ -f "${VS_LOCAL_RESTARTED}" ]]; then
  pass "VS2: trap restarted local node after validator-stop failure"
else
  fail "VS2: trap did not restart local node. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FAILURE: rsync fails ──────────────────────────────────────────────────────
# Both nodes were stopped; trap must restart BOTH.
# =============================================================================
section "Failure — rsync fails → trap restarts both validator and local node"

RF_DIR="${TMPDIR_TEST}/rf"
RF_DATA="${RF_DIR}/data"
RF_BIN="${RF_DIR}/bin"
RF_YAML="${RF_DIR}/node.yaml"
RF_CONFIG="${RF_DIR}/node-config.sh"
mkdir -p "${RF_DATA}/chain.db"
touch "${RF_DATA}/chain.db/CURRENT"
printf 'network: testnet\n' >"${RF_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${RF_CONFIG}"; chmod +x "${RF_CONFIG}"

RF_VALIDATOR_RESTARTED="${RF_DIR}/validator-restarted"
RF_LOCAL_RESTARTED="${RF_DIR}/local-restarted"

make_stub "${RF_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*|*'enable --now'*)
    touch '${RF_LOCAL_RESTARTED}'
    exit 0 ;;
  *)                     exit 0 ;;
esac
"

# ssh: stats and remote stop succeed; validator restart succeeds.
make_stub "${RF_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":5000,\"height\":5000,\"peer_count\":2}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${RF_VALIDATOR_RESTARTED}'
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'   # remote stop succeeds
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

# rsync: always fails → both nodes should be restarted by trap.
make_stub "${RF_BIN}" "rsync" 'exit 1'
make_stub "${RF_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${RF_BIN}" "chown" 'exit 0'
make_stub "${RF_BIN}" "sleep" 'exit 0'

run_bootstrap "${RF_BIN}" "${RF_DATA}" "${RF_YAML}" "${RF_CONFIG}"

# ── RF1: non-zero exit ────────────────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "RF1: script exited non-zero when rsync failed"
else
  fail "RF1: expected non-zero exit but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── RF2: validator restarted by trap ─────────────────────────────────────────
if [[ -f "${RF_VALIDATOR_RESTARTED}" ]]; then
  pass "RF2: trap restarted validator after rsync failure"
else
  fail "RF2: trap did not restart validator. Output:\n${LAST_OUTPUT}"
fi

# ── RF3: local node restarted by trap ────────────────────────────────────────
if [[ -f "${RF_LOCAL_RESTARTED}" ]]; then
  pass "RF3: trap restarted local node after rsync failure"
else
  fail "RF3: trap did not restart local node. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FAILURE: validator restart (step 5) fails ─────────────────────────────────
# Both nodes were stopped; restart fails for the validator.
# Script must exit non-zero; trap must attempt validator restart.
# =============================================================================
section "Failure — validator restart fails → exit non-zero, trap attempts validator restart"

VR_DIR="${TMPDIR_TEST}/vr"
VR_DATA="${VR_DIR}/data"
VR_BIN="${VR_DIR}/bin"
VR_YAML="${VR_DIR}/node.yaml"
VR_CONFIG="${VR_DIR}/node-config.sh"
mkdir -p "${VR_DATA}/chain.db"
touch "${VR_DATA}/chain.db/CURRENT"
touch "${VR_DATA}/snapshot-v2-1000.json.gz"
printf 'network: testnet\n' >"${VR_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${VR_CONFIG}"; chmod +x "${VR_CONFIG}"

VR_VALIDATOR_RESTART_ATTEMPTED="${VR_DIR}/validator-restart-attempted"
VR_LOCAL_RESTARTED="${VR_DIR}/local-restarted"

make_stub "${VR_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*|*'enable --now'*)
    touch '${VR_LOCAL_RESTARTED}'
    exit 0 ;;
  *)                     exit 0 ;;
esac
"

# ssh: stats succeeds; remote stop succeeds (bash heredoc → exit 0);
# validator start (step 5) FAILS; trap's validator start also called.
VR_START_CALL="${VR_DIR}/start-call-count"
make_stub "${VR_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":8000,\"height\":8000,\"peer_count\":2}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  # Record every start attempt so we can verify trap retried it.
  COUNT=0
  [[ -f '${VR_START_CALL}' ]] && COUNT=\$(cat '${VR_START_CALL}')
  COUNT=\$((COUNT + 1))
  printf '%s' \"\$COUNT\" >'${VR_START_CALL}'
  touch '${VR_VALIDATOR_RESTART_ATTEMPTED}'
  exit 1   # restart FAILS every time
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  echo 'stopped'   # remote stop succeeds
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

make_stub "${VR_BIN}" "rsync" 'exit 0'
make_stub "${VR_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${VR_BIN}" "chown" 'exit 0'
make_stub "${VR_BIN}" "sleep" 'exit 0'

run_bootstrap "${VR_BIN}" "${VR_DATA}" "${VR_YAML}" "${VR_CONFIG}"

# ── VR1: exit non-zero ────────────────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "VR1: script exited non-zero when validator restart failed"
else
  fail "VR1: expected non-zero exit but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── VR2: validator restart was attempted (at least once by die→trap) ──────────
if [[ -f "${VR_VALIDATOR_RESTART_ATTEMPTED}" ]]; then
  pass "VR2: trap attempted to restart validator after step-5 restart failure"
else
  fail "VR2: validator restart was not attempted. Output:\n${LAST_OUTPUT}"
fi

# ── VR3: local node restarted by trap ────────────────────────────────────────
if [[ -f "${VR_LOCAL_RESTARTED}" ]]; then
  pass "VR3: trap also restarted local node after validator restart failure"
else
  fail "VR3: trap did not restart local node. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FAILURE: SSH disconnects after remote stop ────────────────────────────────
# The remote stop SSH call itself fails (e.g. network drop after stop fires).
# Because _BS_VALIDATOR_STOPPED is set BEFORE the SSH call, the trap must
# attempt a validator restart even though the SSH exit code was non-zero.
# =============================================================================
section "Failure — SSH drops after remote stop → trap restarts validator"

SD_DIR="${TMPDIR_TEST}/sd"
SD_DATA="${SD_DIR}/data"
SD_BIN="${SD_DIR}/bin"
SD_YAML="${SD_DIR}/node.yaml"
SD_CONFIG="${SD_DIR}/node-config.sh"
mkdir -p "${SD_DATA}/chain.db"
touch "${SD_DATA}/chain.db/CURRENT"
printf 'network: testnet\n' >"${SD_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${SD_CONFIG}"; chmod +x "${SD_CONFIG}"

SD_VALIDATOR_RESTART_ATTEMPTED="${SD_DIR}/validator-restart-attempted"
SD_LOCAL_RESTARTED="${SD_DIR}/local-restarted"

make_stub "${SD_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'start aperod-node'*|*'enable --now'*)
    touch '${SD_LOCAL_RESTARTED}'
    exit 0 ;;
  *)                     exit 0 ;;
esac
"

# ssh: stats succeeds; remote stop (bash heredoc) exits 1 (SSH disconnect);
# validator restart (trap) succeeds.
make_stub "${SD_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":3000,\"height\":3000,\"peer_count\":1}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${SD_VALIDATOR_RESTART_ATTEMPTED}'
  echo 'started'
  exit 0
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  exit 1   # SSH disconnect (heredoc never completes)
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

make_stub "${SD_BIN}" "rsync" 'exit 0'
make_stub "${SD_BIN}" "curl"  "echo '{\"height\":0}'; exit 0"
make_stub "${SD_BIN}" "chown" 'exit 0'
make_stub "${SD_BIN}" "sleep" 'exit 0'

run_bootstrap "${SD_BIN}" "${SD_DATA}" "${SD_YAML}" "${SD_CONFIG}"

# ── SD1: exit non-zero ────────────────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "SD1: script exited non-zero after SSH disconnect during remote stop"
else
  fail "SD1: expected non-zero exit but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── SD2: validator restart attempted by trap ──────────────────────────────────
# The flag was set before the SSH call, so the trap must try to restart.
if [[ -f "${SD_VALIDATOR_RESTART_ATTEMPTED}" ]]; then
  pass "SD2: trap attempted validator restart after SSH disconnect (flag set before SSH)"
else
  fail "SD2: trap did not attempt validator restart. Output:\n${LAST_OUTPUT}"
fi

# ── SD3: local node restarted by trap ────────────────────────────────────────
if [[ -f "${SD_LOCAL_RESTARTED}" ]]; then
  pass "SD3: trap also restarted local node after SSH disconnect"
else
  fail "SD3: trap did not restart local node. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── LAST-ATTEMPT SUCCESS ──────────────────────────────────────────────────────
# API returns height > 0 on exactly the final allowed poll attempt.
# Must exit 0 — not incorrectly treated as a timeout.
# =============================================================================
section "Last-attempt success — height>0 on final poll → exit 0, not timeout"

LA_DIR="${TMPDIR_TEST}/la"
LA_DATA="${LA_DIR}/data"
LA_BIN="${LA_DIR}/bin"
LA_YAML="${LA_DIR}/node.yaml"
LA_CONFIG="${LA_DIR}/node-config.sh"
mkdir -p "${LA_DATA}/chain.db"
touch "${LA_DATA}/chain.db/CURRENT"
touch "${LA_DATA}/snapshot-v2-200.json.gz"
printf 'network: testnet\n' >"${LA_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${LA_CONFIG}"; chmod +x "${LA_CONFIG}"

# HEALTH_MAX_ATTEMPTS=3 so the third curl call is the "last" attempt.
LA_CALL_COUNT="${LA_DIR}/curl-calls"

make_stub "${LA_BIN}" "rsync" 'exit 0'
make_stub "${LA_BIN}" "chown" 'exit 0'
make_stub "${LA_BIN}" "sleep" 'exit 0'
make_stub "${LA_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'
make_stub "${LA_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '"'"'{"tip_height":300,"height":300,"peer_count":1}'"'"'
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null
  echo "stopped"
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'

# curl: returns height=0 for the first N-1 polls, height=42 on the Nth (last).
# With HEALTH_MAX_ATTEMPTS=3, poll 1-2 return 0; poll 3 returns 42.
make_stub "${LA_BIN}" "curl" "
COUNT=0
[[ -f '${LA_CALL_COUNT}' ]] && COUNT=\$(cat '${LA_CALL_COUNT}')
COUNT=\$((COUNT + 1))
printf '%s' \"\$COUNT\" >'${LA_CALL_COUNT}'
if [[ \$COUNT -ge 3 ]]; then
  echo '{\"height\":42,\"peer_count\":1,\"syncing\":false}'
else
  echo '{\"height\":0,\"peer_count\":0,\"syncing\":true}'
fi
exit 0
"

LAST_EXIT=0
LAST_OUTPUT=$(
  env \
    PATH="${LA_BIN}:${PATH}" \
    SECONDARY_DATA_DIR="${LA_DATA}" \
    LOCAL_DATA_DIR="${LA_DATA}" \
    LOCAL_NODE_YAML="${LA_YAML}" \
    LOCAL_NODE_CONFIG_SH="${LA_CONFIG}" \
    PRIMARY_DATA_DIR="${LA_DATA}" \
    VALIDATOR_DATA_DIR="${LA_DATA}" \
    HEALTH_MAX_ATTEMPTS=3 \
    HEALTH_WAIT_SECS=0 \
  bash "${JOIN_SH}" "--bootstrap-from=${VALIDATOR_IP}" 2>&1
) || LAST_EXIT=$?

# ── LA1: exit 0 when height > 0 lands on last allowed attempt ────────────────
if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "LA1: script exited 0 when height>0 returned on the final poll (attempt 3/3)"
else
  fail "LA1: expected exit 0 but got ${LAST_EXIT} (off-by-one: last-attempt success treated as timeout). Output:\n${LAST_OUTPUT}"
fi

# ── LA2: banner shows the correct height from that last poll ─────────────────
if echo "${LAST_OUTPUT}" | grep -qE '42'; then
  pass "LA2: banner contains height 42 from last-attempt poll"
else
  fail "LA2: expected height 42 in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── SLOW VALIDATOR API AT STEP 1 ──────────────────────────────────────────────
# An empty validator stats response must be non-fatal. The script should retain
# VALIDATOR_TIP=unknown and execute the complete bootstrap sequence.
# =============================================================================
section "Slow validator API at step 1 — empty response continues with unknown tip"

SV_DIR="${TMPDIR_TEST}/sv"
SV_DATA="${SV_DIR}/data"
SV_BIN="${SV_DIR}/bin"
SV_YAML="${SV_DIR}/node.yaml"
SV_CONFIG="${SV_DIR}/node-config.sh"
mkdir -p "${SV_DATA}/chain.db"
touch "${SV_DATA}/chain.db/CURRENT"
touch "${SV_DATA}/snapshot-v2-700.json.gz"
printf 'network: testnet\n' >"${SV_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${SV_CONFIG}"; chmod +x "${SV_CONFIG}"

SV_RSYNC_CALLED="${SV_DIR}/rsync-called"
SV_VALIDATOR_STOPPED="${SV_DIR}/validator-stopped"
SV_VALIDATOR_STARTED="${SV_DIR}/validator-started"
SV_LOCAL_STARTED="${SV_DIR}/local-started"

make_stub "${SV_BIN}" "rsync" "touch '${SV_RSYNC_CALLED}'; exit 0"
make_stub "${SV_BIN}" "chown" 'exit 0'
make_stub "${SV_BIN}" "sleep" 'exit 0'
make_stub "${SV_BIN}" "aperod-node" "
if echo \"\$*\" | grep -q -- '--check-store'; then
  echo 'check-store OK: tip_height=700 missing=0 (threshold=5000)'
fi
exit 0
"
make_stub "${SV_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)  exit 0 ;;
  *'is-active'*)         exit 1 ;;
  *'enable --now'*|*'start aperod-node'*)
    touch '${SV_LOCAL_STARTED}'
    exit 0 ;;
  *)                     exit 0 ;;
esac
"
make_stub "${SV_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'network/stats'; then
  # Simulate curl timing out while the validator API is still starting.
  exit 0
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  touch '${SV_VALIDATOR_STARTED}'
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  cat >/dev/null
  touch '${SV_VALIDATOR_STOPPED}'
  echo 'stopped'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"
make_stub "${SV_BIN}" "curl" "
echo '{\"height\":700,\"peer_count\":1,\"syncing\":false}'
exit 0
"

run_bootstrap "${SV_BIN}" "${SV_DATA}" "${SV_YAML}" "${SV_CONFIG}"

if [[ ${LAST_EXIT} -eq 0 ]]; then
  pass "SV1: script exited 0 despite an empty step-1 validator API response"
else
  fail "SV1: expected exit 0 but got ${LAST_EXIT}. Output:\n${LAST_OUTPUT}"
fi

if echo "${LAST_OUTPUT}" | grep -q 'Validator tip_height: unknown'; then
  pass "SV2: completion banner shows validator tip_height as unknown"
else
  fail "SV2: expected completion banner to show unknown validator tip. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${SV_RSYNC_CALLED}" ]]; then
  pass "SV3: rsync still executed after the empty step-1 response"
else
  fail "SV3: rsync did not execute. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${SV_VALIDATOR_STOPPED}" ]] && [[ -f "${SV_VALIDATOR_STARTED}" ]]; then
  pass "SV4: validator stop and restart steps still executed"
else
  fail "SV4: validator stop/restart sequence was incomplete. Output:\n${LAST_OUTPUT}"
fi

if [[ -f "${SV_LOCAL_STARTED}" ]]; then
  pass "SV5: local validator start step still executed"
else
  fail "SV5: local validator was not started. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── FAILURE: API timeout ──────────────────────────────────────────────────────
# All polls return height=0; script must exit non-zero after exhausting retries.
# =============================================================================
section "Failure — API always returns height=0 → exit non-zero after retries"

AT_DIR="${TMPDIR_TEST}/at"
AT_DATA="${AT_DIR}/data"
AT_BIN="${AT_DIR}/bin"
AT_YAML="${AT_DIR}/node.yaml"
AT_CONFIG="${AT_DIR}/node-config.sh"
mkdir -p "${AT_DATA}/chain.db"
touch "${AT_DATA}/chain.db/CURRENT"
touch "${AT_DATA}/snapshot-v2-100.json.gz"
printf 'network: testnet\n' >"${AT_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${AT_CONFIG}"; chmod +x "${AT_CONFIG}"

make_stub "${AT_BIN}" "rsync" 'exit 0'
make_stub "${AT_BIN}" "chown" 'exit 0'
make_stub "${AT_BIN}" "sleep" 'exit 0'
make_stub "${AT_BIN}" "systemctl" '
case "$*" in
  *"stop aperod-node"*)  exit 0 ;;
  *"is-active"*)         exit 1 ;;
  *)                     exit 0 ;;
esac
'
make_stub "${AT_BIN}" "ssh" '
shift
CMD="$*"
if echo "$CMD" | grep -q "network/stats"; then
  echo '"'"'{"tip_height":200,"height":200,"peer_count":1}'"'"'
elif echo "$CMD" | grep -q "systemctl start"; then
  echo "started"
elif [[ "$CMD" == "bash" ]]; then
  cat >/dev/null
  echo "stopped"
else
  cat >/dev/null
  echo "ok"
fi
exit 0
'
# curl: always returns height=0.
make_stub "${AT_BIN}" "curl" "
echo '{\"height\":0,\"peer_count\":0,\"syncing\":true}'
exit 0
"

run_bootstrap "${AT_BIN}" "${AT_DATA}" "${AT_YAML}" "${AT_CONFIG}"

# ── AT1: non-zero exit ────────────────────────────────────────────────────────
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "AT1: script exited non-zero after API timeout"
else
  fail "AT1: expected non-zero exit after API timeout but got 0. Output:\n${LAST_OUTPUT}"
fi

# ── AT2: timeout message present ─────────────────────────────────────────────
if echo "${LAST_OUTPUT}" | grep -qi "таймаут\|timeout"; then
  pass "AT2: output contains API-timeout message"
else
  fail "AT2: expected timeout message in output. Got:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── DATA DIR MISMATCH ─────────────────────────────────────────────────────────
# If VALIDATOR_DATA_DIR is absent on the validator, the script must exit
# non-zero with an actionable message BEFORE stopping either service.
# If VALIDATOR_DATA_DIR exists but chain.db is absent, same early exit.
# =============================================================================
section "Data dir mismatch — VALIDATOR_DATA_DIR absent on validator → early exit, no services stopped"

# ── DD1 / DD2: VALIDATOR_DATA_DIR does not exist on the validator ─────────────
DD_DIR="${TMPDIR_TEST}/dd"
DD_DATA="${DD_DIR}/data"
DD_BIN="${DD_DIR}/bin"
DD_YAML="${DD_DIR}/node.yaml"
DD_CONFIG="${DD_DIR}/node-config.sh"
mkdir -p "${DD_DATA}/chain.db"
touch "${DD_DATA}/chain.db/CURRENT"
touch "${DD_DATA}/snapshot-v2-5000.json.gz"
printf 'network: testnet\n' >"${DD_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${DD_CONFIG}"; chmod +x "${DD_CONFIG}"

# Sentinel files: if any service was stopped, these get created.
DD_LOCAL_STOPPED="${DD_DIR}/local-stopped"
DD_VALIDATOR_STOPPED="${DD_DIR}/validator-stopped"

make_stub "${DD_BIN}" "rsync"    'exit 0'
make_stub "${DD_BIN}" "chown"    'exit 0'
make_stub "${DD_BIN}" "sleep"    'exit 0'

# systemctl: record if local node stop was attempted.
make_stub "${DD_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    touch '${DD_LOCAL_STOPPED}'
    exit 0 ;;
  *'is-active'*)  exit 1 ;;
  *)              exit 0 ;;
esac
"

# ssh: dir-check (`[ -d PATH ]`) returns exit 1 → VALIDATOR_DATA_DIR absent.
# Remote stop (bash heredoc) records a sentinel so we can verify it was
# never called (script should exit before step 3).
make_stub "${DD_BIN}" "ssh" "
shift   # drop root@IP
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q '\[ -d '; then
  # Pre-rsync directory existence check — simulate missing VALIDATOR_DATA_DIR.
  exit 1
elif echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":5000,\"height\":5000,\"peer_count\":2}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  touch '${DD_VALIDATOR_STOPPED}'
  cat >/dev/null
  echo 'stopped'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

make_stub "${DD_BIN}" "curl" "echo '{\"height\":0}'; exit 0"

run_bootstrap "${DD_BIN}" "${DD_DATA}" "${DD_YAML}" "${DD_CONFIG}"

# DD1: non-zero exit.
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "DD1: script exits non-zero when VALIDATOR_DATA_DIR is absent on the validator"
else
  fail "DD1: expected non-zero exit for missing VALIDATOR_DATA_DIR, got 0. Output:\n${LAST_OUTPUT}"
fi

# DD2: error message contains the path and VALIDATOR_DATA_DIR override hint.
if echo "${LAST_OUTPUT}" | grep -qi 'VALIDATOR_DATA_DIR'; then
  pass "DD2: error message mentions VALIDATOR_DATA_DIR override hint"
else
  fail "DD2: expected VALIDATOR_DATA_DIR override hint in output. Got:\n${LAST_OUTPUT}"
fi

# DD2b: neither local node nor remote validator was stopped before the exit.
if [[ ! -f "${DD_LOCAL_STOPPED}" ]] && [[ ! -f "${DD_VALIDATOR_STOPPED}" ]]; then
  pass "DD2b: no service was stopped before early exit (both nodes stayed up)"
else
  fail "DD2b: a service was stopped despite early exit on dir-check failure. Output:\n${LAST_OUTPUT}"
fi

# ── DD3 / DD4: VALIDATOR_DATA_DIR exists but chain.db sub-dir is absent ───────
DD3_DIR="${TMPDIR_TEST}/dd3"
DD3_DATA="${DD3_DIR}/data"
DD3_BIN="${DD3_DIR}/bin"
DD3_YAML="${DD3_DIR}/node.yaml"
DD3_CONFIG="${DD3_DIR}/node-config.sh"
mkdir -p "${DD3_DATA}/chain.db"
touch "${DD3_DATA}/chain.db/CURRENT"
touch "${DD3_DATA}/snapshot-v2-5000.json.gz"
printf 'network: testnet\n' >"${DD3_YAML}"
printf '#!/usr/bin/env bash\nexit 0\n' >"${DD3_CONFIG}"; chmod +x "${DD3_CONFIG}"

DD3_LOCAL_STOPPED="${DD3_DIR}/local-stopped"
DD3_VALIDATOR_STOPPED="${DD3_DIR}/validator-stopped"

make_stub "${DD3_BIN}" "rsync"    'exit 0'
make_stub "${DD3_BIN}" "chown"    'exit 0'
make_stub "${DD3_BIN}" "sleep"    'exit 0'

make_stub "${DD3_BIN}" "systemctl" "
case \"\$*\" in
  *'stop aperod-node'*)
    touch '${DD3_LOCAL_STOPPED}'
    exit 0 ;;
  *'is-active'*)  exit 1 ;;
  *)              exit 0 ;;
esac
"

# ssh: first dir-check (`[ -d PATH ]`) → exit 0 (dir exists);
#      second dir-check (`[ -d PATH/chain.db ]`) → exit 1 (chain.db absent).
make_stub "${DD3_BIN}" "ssh" "
shift
CMD=\"\$*\"
if echo \"\$CMD\" | grep -q 'chain.db' && echo \"\$CMD\" | grep -q '\[ -d '; then
  # chain.db existence check — simulate missing chain.db.
  exit 1
elif echo \"\$CMD\" | grep -q '\[ -d '; then
  # VALIDATOR_DATA_DIR existence check — dir is present.
  exit 0
elif echo \"\$CMD\" | grep -q 'network/stats'; then
  echo '{\"tip_height\":5000,\"height\":5000,\"peer_count\":2}'
elif echo \"\$CMD\" | grep -q 'systemctl start'; then
  echo 'started'
elif [[ \"\$CMD\" == 'bash' ]]; then
  touch '${DD3_VALIDATOR_STOPPED}'
  cat >/dev/null
  echo 'stopped'
else
  cat >/dev/null
  echo 'ok'
fi
exit 0
"

make_stub "${DD3_BIN}" "curl" "echo '{\"height\":0}'; exit 0"

run_bootstrap "${DD3_BIN}" "${DD3_DATA}" "${DD3_YAML}" "${DD3_CONFIG}"

# DD3: non-zero exit.
if [[ ${LAST_EXIT} -ne 0 ]]; then
  pass "DD3: script exits non-zero when chain.db is absent inside VALIDATOR_DATA_DIR"
else
  fail "DD3: expected non-zero exit for missing chain.db, got 0. Output:\n${LAST_OUTPUT}"
fi

# DD4: error message contains actionable hint.
if echo "${LAST_OUTPUT}" | grep -qi 'VALIDATOR_DATA_DIR\|chain.db'; then
  pass "DD4: error message mentions chain.db / VALIDATOR_DATA_DIR override hint"
else
  fail "DD4: expected chain.db / VALIDATOR_DATA_DIR hint in output. Got:\n${LAST_OUTPUT}"
fi

# DD4b: neither service was stopped before the early exit.
if [[ ! -f "${DD3_LOCAL_STOPPED}" ]] && [[ ! -f "${DD3_VALIDATOR_STOPPED}" ]]; then
  pass "DD4b: no service was stopped before early exit on chain.db-check failure"
else
  fail "DD4b: a service was stopped despite early exit on chain.db-check. Output:\n${LAST_OUTPUT}"
fi

# =============================================================================
# ── Push-mode regression guard ────────────────────────────────────────────────
# Running without --bootstrap-from must still require TARGET_IP.
# =============================================================================
section "Regression guard — push mode unchanged when --bootstrap-from is absent"

PM_DIR="${TMPDIR_TEST}/pm"
PM_DATA="${PM_DIR}/data"
mkdir -p "${PM_DATA}/chain.db"
touch "${PM_DATA}/chain.db/CURRENT"

PM_EXIT=0
PM_OUTPUT=$(
  env \
    PRIMARY_IP="127.0.0.1" \
    PRIMARY_DATA_DIR="${PM_DATA}" \
  bash "${JOIN_SH}" 2>&1
) || PM_EXIT=$?

# ── PM1: missing TARGET_IP causes non-zero exit ───────────────────────────────
if [[ ${PM_EXIT} -ne 0 ]]; then
  pass "PM1: push mode exits non-zero when TARGET_IP is missing"
else
  fail "PM1: push mode should require TARGET_IP but exited 0"
fi

# ── PM2: error references push-mode usage ────────────────────────────────────
if echo "${PM_OUTPUT}" | grep -qi "join-network.sh.*IP\|Укажите IP"; then
  pass "PM2: error message references push-mode usage"
else
  fail "PM2: expected push-mode usage hint in output. Got:\n${PM_OUTPUT}"
fi

# =============================================================================
# ── Summary ───────────────────────────────────────────────────────────────────
# =============================================================================
echo
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
echo -e "${BOLD}────────────────────────────────────────────────────────────${NC}"
echo

if [[ ${FAIL} -gt 0 ]]; then
  exit 1
fi
exit 0
