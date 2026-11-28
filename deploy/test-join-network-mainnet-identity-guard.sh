#!/usr/bin/env bash
# Mainnet must not be stopped or overwritten until a reviewed, independently
# authenticated chain-identity release is available.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
JOIN_SH="${SCRIPT_DIR}/join-network.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

mkdir -p "${TMP}/bin" "${TMP}/source/chain.db" "${TMP}/target/chain.db"
printf 'source chain bytes\n' >"${TMP}/source/chain.db/CURRENT"
printf 'target chain bytes\n' >"${TMP}/target/chain.db/CURRENT"
printf 'source private data\n' >"${TMP}/source/marker"
printf 'target private data\n' >"${TMP}/target/marker"

cat >"${TMP}/push-mainnet.yaml" <<'YAML'
network: mainnet
data_dir: /var/lib/aperod
genesis:
  file: /etc/aperod/genesis-mainnet.yaml
YAML
cat >"${TMP}/bootstrap-mainnet.yaml" <<'YAML'
network: mainnet
data_dir: /var/lib/aperod
genesis:
  file: /etc/aperod/genesis-mainnet.yaml
YAML

cat >"${TMP}/bin/ssh" <<'SH'
#!/usr/bin/env bash
echo "ssh $*" >>"${JOIN_TEST_CALL_LOG}"
exit 0
SH
cat >"${TMP}/bin/systemctl" <<'SH'
#!/usr/bin/env bash
echo "systemctl $*" >>"${JOIN_TEST_CALL_LOG}"
exit 0
SH
cat >"${TMP}/bin/rsync" <<'SH'
#!/usr/bin/env bash
echo "rsync $*" >>"${JOIN_TEST_CALL_LOG}"
exit 0
SH
chmod +x "${TMP}/bin/"*

tree_hash() {
  python3 - "$1" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
h = hashlib.sha256()
for current, dirs, files in os.walk(root):
    dirs.sort()
    for name in sorted(files):
        path = os.path.join(current, name)
        rel = os.path.relpath(path, root).encode()
        h.update(len(rel).to_bytes(4, "big"))
        h.update(rel)
        with open(path, "rb") as f:
            while chunk := f.read(65536):
                h.update(chunk)
print(h.hexdigest())
PY
}

SOURCE_BEFORE="$(tree_hash "${TMP}/source")"
TARGET_BEFORE="$(tree_hash "${TMP}/target")"
export JOIN_TEST_CALL_LOG="${TMP}/calls.log"
export PATH="${TMP}/bin:${PATH}"

# Push mode: source is configured as mainnet. It must refuse before probing or
# stopping the target, stopping the source, creating a sentinel, or rsync.
PUSH_EXIT=0
PUSH_OUTPUT=$(
  PRIMARY_IP=192.0.2.10 \
  PRIMARY_DATA_DIR="${TMP}/source" \
  PRIMARY_NODE_YAML="${TMP}/push-mainnet.yaml" \
  SECONDARY_DATA_DIR="${TMP}/target" \
  bash "${JOIN_SH}" 192.0.2.20 2>&1
) || PUSH_EXIT=$?
[[ ${PUSH_EXIT} -ne 0 ]] || fail "push mode accepted mainnet without an identity release"
[[ "${PUSH_OUTPUT}" == *"нет независимо подтверждённого"* ]] ||
  fail "push mode did not explain the missing authenticated mainnet identity: ${PUSH_OUTPUT}"
[[ ! -s "${JOIN_TEST_CALL_LOG}" ]] || fail "push mode called SSH/systemctl/rsync before refusal"
[[ "$(tree_hash "${TMP}/source")" == "${SOURCE_BEFORE}" ]] || fail "push mode changed source data"
[[ "$(tree_hash "${TMP}/target")" == "${TARGET_BEFORE}" ]] || fail "push mode changed target data"
pass "push mode refuses before external calls and preserves source and target"

# Bootstrap mode: the local target is configured as mainnet. It must refuse
# before stopping either node, writing a sentinel, or starting rsync.
BOOT_EXIT=0
BOOT_OUTPUT=$(
  SECONDARY_DATA_DIR="${TMP}/target" \
  LOCAL_NODE_YAML="${TMP}/bootstrap-mainnet.yaml" \
  PRIMARY_DATA_DIR="${TMP}/source" \
  VALIDATOR_DATA_DIR="${TMP}/source" \
  bash "${JOIN_SH}" --bootstrap-from=192.0.2.10 2>&1
) || BOOT_EXIT=$?
[[ ${BOOT_EXIT} -ne 0 ]] || fail "bootstrap mode accepted mainnet without an identity release"
[[ "${BOOT_OUTPUT}" == *"нет независимо подтверждённого"* ]] ||
  fail "bootstrap mode did not explain the missing authenticated mainnet identity: ${BOOT_OUTPUT}"
[[ ! -s "${JOIN_TEST_CALL_LOG}" ]] || fail "bootstrap mode called SSH/systemctl/rsync before refusal"
[[ "$(tree_hash "${TMP}/source")" == "${SOURCE_BEFORE}" ]] || fail "bootstrap mode changed source data"
[[ "$(tree_hash "${TMP}/target")" == "${TARGET_BEFORE}" ]] || fail "bootstrap mode changed target data"
pass "bootstrap mode refuses before external calls and preserves source and target"

echo "All 2 mainnet identity guard scenarios passed."
