#!/usr/bin/env bash
# Focused safety tests for rollout hook ordering and the offline installer.
set -euo pipefail

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NODE_DIR="$(cd "${DEPLOY_DIR}/.." && pwd)"
ROOT_DIR="$(cd "${DEPLOY_DIR}/../.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

PROD_OVERRIDE="${PROD:-}"
PROD="${PROD_OVERRIDE:-${ROOT_DIR}/scripts/production-node-verified-rollout.sh}"
CHECK_PROD=1
if [[ ! -f "$PROD" ]]; then
  if [[ -n "$PROD_OVERRIDE" || -f "${ROOT_DIR}/pnpm-workspace.yaml" ]]; then
    fail "required verified rollout script is missing"
  fi
  [[ -f "${NODE_DIR}/go.mod" ]] || fail "cannot identify standalone node source layout"
  CHECK_PROD=0
  echo "NOT_APPLICABLE: deployment-specific rollout script is not shipped in standalone node source; public updater and installer checks remain required"
fi
if (( CHECK_PROD )); then
bash -n "$PROD"
python3 - "$PROD" <<'PY'
import pathlib,sys
source=pathlib.Path(sys.argv[1]).read_text()
marker = "python3 - \"$release/stopped-copy\" <<'PY'\n"
body=source.split(marker,1)[1].split("\nPY\n",1)[0]
compile(body, str(sys.argv[1]) + ":stopped-copy-mount-check", "exec")
PY
fi
bash -n "${DEPLOY_DIR}/update-node.sh"
bash -n "${DEPLOY_DIR}/setup-rollout-cleanup.sh"
python3 -c 'import pathlib,sys; [compile(pathlib.Path(p).read_text(), p, "exec") for p in sys.argv[1:]]' \
  "${DEPLOY_DIR}/rollout_cleanup/__init__.py" "${DEPLOY_DIR}/rollout_cleanup/cli.py"
pass "rollout scripts and stdlib Python CLI pass syntax checks"

if (( CHECK_PROD )); then
begin_call="$(grep -n '^rollout_begin$' "$PROD" | cut -d: -f1)"
stop_call="$(grep -n '^systemctl stop aperod-node$' "$PROD" | cut -d: -f1)"
copy_check="$(grep -n 'snapshot-check.txt' "$PROD" | cut -d: -f1)"
stopped_mark="$(grep -n 'rollout_mark "\$release/stopped-copy" stopped-copy' "$PROD" | cut -d: -f1)"
candidate_mark="$(grep -n 'rollout_mark "\$release/node.candidate" candidate' "$PROD" | cut -d: -f1)"
symlink_guard="$(grep -n 'find -P "\$release/stopped-copy" -xdev -type l' "$PROD" | cut -d: -f1)"
mount_guard="$(grep -n '/proc/self/mountinfo' "$PROD" | cut -d: -f1)"
root_normalize="$(grep -n 'chown -R --no-dereference root:root -- "\$release/stopped-copy"' "$PROD" | cut -d: -f1)"
complete_call="$(grep -n 'expected-binary-sha256 "\$expected_new"' "$PROD" | cut -d: -f1)"
success_banner="$(grep -n '^echo "ROLLOUT_SUCCESS"$' "$PROD" | cut -d: -f1)"
[[ -n "$begin_call" && "$begin_call" -lt "$stop_call" ]] || fail "begin hook is not before node stop"
[[ "$stopped_mark" -gt "$copy_check" && "$candidate_mark" -gt "$copy_check" ]] \
  || fail "artifacts are marked before the stopped snapshot check"
[[ -n "$symlink_guard" && -n "$mount_guard" && "$symlink_guard" -lt "$mount_guard" \
   && "$mount_guard" -lt "$root_normalize" && "$root_normalize" -lt "$copy_check" ]] \
  || fail "stopped-copy is normalized before rejecting unsafe links or mounts"
[[ "$(grep -c 'chown -R' "$PROD")" -eq 1 ]] \
  || fail "ownership normalization is broader than the isolated stopped-copy"
[[ "$complete_call" -lt "$success_banner" ]] || fail "complete hook is not before success banner"
pass "verified production rollout begins before stop, marks after the coherent copy, and completes before success"
fi

UPDATE="${DEPLOY_DIR}/update-node.sh"
grep -Fq 'install -o root -g root -m 755 "${BINARY_SRC}" "${ROLLOUT_RELEASE}/node.candidate"' "$UPDATE" \
  || fail "update-node does not make a release-owned candidate copy"
grep -Fq -- '--artifact "${ROLLOUT_RELEASE}/node.candidate" --kind candidate' "$UPDATE" \
  || fail "update-node does not register its release-owned candidate"
! grep -Fq -- '--artifact "${BINARY_SRC}"' "$UPDATE" || fail "shared BINARY_SRC is marked as an artifact"
! grep -Fq 'rm -f "${BINARY_BACKUP}"' "$UPDATE" || fail "latest pre-update binary is deleted before startup health"
grep -Fq '_rollout_complete_after_readiness' "$UPDATE" \
  || fail "update-node does not attempt completion after readiness verification"
grep -Fq 'complete --id "${ROLLOUT_ID}"' "$UPDATE" \
  || fail "update-node does not complete a proven candidate registration"
grep -Fq 'bounded /api/v1/status check did not prove' "$UPDATE" \
  || fail "failed readiness proof does not explicitly preserve an incomplete job"
pass "update-node registers only its owned candidate and preserves the pre-update binary"

# Exercise the completion gate with a fake API/helper and the current test
# process as the stable service PID. No node service or live helper is invoked.
ROLLOUT_FUNCTION="$(awk '
  /^_rollout_complete_after_readiness\(\) \{/ { capture=1 }
  capture { print }
  capture && /^}$/ { exit }
' "$UPDATE")"
[[ -n "$ROLLOUT_FUNCTION" ]] || fail "could not extract updater completion gate"
eval "$ROLLOUT_FUNCTION"
ROLLOUT_RELEASE="${TMP}/rollout"
mkdir -p "$ROLLOUT_RELEASE"
cp "$(readlink -f "/proc/$$/exe")" "${ROLLOUT_RELEASE}/node.candidate"
chmod 755 "${ROLLOUT_RELEASE}/node.candidate"
BINARY_DST="${TMP}/aperod-node"
cp "${ROLLOUT_RELEASE}/node.candidate" "$BINARY_DST"
ROLLOUT_CANDIDATE_SHA256="$(sha256sum "${ROLLOUT_RELEASE}/node.candidate" | cut -d' ' -f1)"
ROLLOUT_CLI="${TMP}/rollout-cli"
ROLLOUT_HELPER_LOG="${TMP}/rollout-helper.log"
SYSTEMCTL_TEST_LOG="${TMP}/rollout-systemctl.log"
cat > "$ROLLOUT_CLI" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$ROLLOUT_HELPER_LOG"
if [[ "$1" == complete ]]; then
  printf '%s\n' '{"status":"completed"}'
else
  exit 90
fi
STUB
chmod +x "$ROLLOUT_CLI"
curl() {
  if [[ "${MOCK_STATUS_MODE}" == ready ]]; then
    printf '%s\n' '{"ok":true,"syncing":false,"utxo_rebuilding":false,"height":101}'
  else
    printf '%s\n' '{"ok":true,"syncing":true,"utxo_rebuilding":false,"height":101}'
  fi
}
systemctl() {
  printf '%s\n' "$*" >> "$SYSTEMCTL_TEST_LOG"
  case "$*" in
    "is-active --quiet aperod-node") return 0 ;;
    "show aperod-node -p MainPID --value") printf '%s\n' "$$" ;;
    *) return 90 ;;
  esac
}
export ROLLOUT_HELPER_LOG
SERVICE_NAME="aperod-node"
HEALTH_URL="http://127.0.0.1:8545/api/v1/status"
ROLLOUT_ID="11111111-1111-4111-8111-111111111111"
ROLLOUT_CANDIDATE_MARKED=1
ROLLOUT_BEGIN_HEIGHT=100
SKIP_HEALTH_CHECK=0
ROLLOUT_READY_MAX_ATTEMPTS=2
ROLLOUT_READY_TIMEOUT_SECS=5
ROLLOUT_READY_WAIT_SECS=0
ROLLOUT_COMPLETED=0
ROLLOUT_COMPLETION_REPORTED=0
MOCK_STATUS_MODE=ready
export MOCK_STATUS_MODE SYSTEMCTL_TEST_LOG
if ! _rollout_complete_after_readiness > "${TMP}/rollout-success.out" 2>&1; then
  fail "successful readiness proof caused an updater failure"
fi
[[ "$ROLLOUT_COMPLETED" == 1 ]] || fail "advancing-ready status did not complete the registered job"
grep -Fq "complete --id ${ROLLOUT_ID} --expected-binary-sha256 ${ROLLOUT_CANDIDATE_SHA256}" \
  "$ROLLOUT_HELPER_LOG" || fail "successful proof did not invoke complete with candidate SHA"
grep -Fq "Completion verified by cleanup helper" "${TMP}/rollout-success.out" \
  || fail "successful complete result was not reported"

: > "$ROLLOUT_HELPER_LOG"
: > "$SYSTEMCTL_TEST_LOG"
ROLLOUT_COMPLETED=0
ROLLOUT_COMPLETION_REPORTED=0
MOCK_STATUS_MODE=syncing
if ! _rollout_complete_after_readiness > "${TMP}/rollout-incomplete.out" 2>&1; then
  fail "failed readiness proof changed the completed update's exit behavior"
fi
[[ "$ROLLOUT_COMPLETED" == 0 && ! -s "$ROLLOUT_HELPER_LOG" ]] \
  || fail "failed readiness proof called complete or changed the registration"
grep -Fq 'remains incomplete; artifacts are preserved' "${TMP}/rollout-incomplete.out" \
  || fail "failed readiness proof did not explicitly warn that artifacts remain"
! grep -Eq '(^| )(start|stop|restart)( |$)' "$SYSTEMCTL_TEST_LOG" \
  || fail "completion check restarted or stopped the updated node"
pass "update-node completes only after bounded advancing readiness and binary/PID parity"

SERVICE="${DEPLOY_DIR}/aperod-rollout-cleanup.service"
grep -Fq 'ReadOnlyPaths=/opt/aperod/data' "$SERVICE" || fail "live data is not read-only in cleanup service"
grep -Fq 'ReadWritePaths=/var/lib/aperod-rollout-cleanup /opt/aperod/releases' "$SERVICE" \
  || fail "cleanup service write paths are not narrowly scoped"
grep -Fq 'CapabilityBoundingSet=CAP_DAC_OVERRIDE CAP_FOWNER CAP_SYS_PTRACE' "$SERVICE" \
  || fail "cleanup service cannot inspect unprivileged process references"
grep -Fq 'CAP_SYS_PTRACE is needed to inspect ape-user /proc cwd, fd, and exe entries' "$SERVICE" \
  || fail "cleanup service does not document why CAP_SYS_PTRACE is required"
grep -Fq 'unreadable process references are a refusal, never treated as inactivity' "$SERVICE" \
  || fail "cleanup unit does not document fail-closed handling of unreadable process references"
! grep -Eq '^(PrivateTmp=yes|ProtectProc=|ProcSubset=|PrivateMounts=yes)' "$SERVICE" \
  || fail "cleanup service hides host process or mount references"
! grep -Eq 'systemctl (restart|start|stop) aperod-node' "$SERVICE" \
  || fail "cleanup service can restart the node"
pass "cleanup service keeps host visibility, live data read-only, and has no node restart action"

# All systemctl calls are intercepted. The staging destination ensures the
# installer cannot replace any host file even if its logic regresses.
SYSTEMCTL_LOG="${TMP}/systemctl.log"
cat > "${TMP}/systemctl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SYSTEMCTL_LOG"
case "$*" in
  "daemon-reload"|"enable --now aperod-rollout-cleanup.timer"|"enable --now aperod-historical-retirement.timer") exit 0 ;;
  *) echo "unexpected systemctl invocation: $*" >&2; exit 90 ;;
esac
STUB
chmod +x "${TMP}/systemctl"
export SYSTEMCTL_LOG
DEST="${TMP}/root"
SOURCE_DIR="${TMP}/source"
mkdir -p "${SOURCE_DIR}/rollout_cleanup/future_package"
cp "${DEPLOY_DIR}/setup-rollout-cleanup.sh" \
  "${DEPLOY_DIR}/aperod-rollout-cleanup.service" \
  "${DEPLOY_DIR}/aperod-rollout-cleanup.timer" \
  "${DEPLOY_DIR}/aperod-historical-retirement.service" \
  "${DEPLOY_DIR}/aperod-historical-retirement.timer" "$SOURCE_DIR/"
cp -R "${DEPLOY_DIR}/rollout_cleanup/." "${SOURCE_DIR}/rollout_cleanup/"
printf '%s\n' '"""Additional module used to test bundle atomicity."""' > "${SOURCE_DIR}/rollout_cleanup/future_package/__init__.py"
printf '%s\n' 'PACKAGE_GENERATION = "one"' > "${SOURCE_DIR}/rollout_cleanup/future_package/module.py"
SYSTEMCTL="${TMP}/systemctl" bash "${SOURCE_DIR}/setup-rollout-cleanup.sh" --destdir "$DEST" >/dev/null
POLICY="${DEST}/etc/aperod/rollout-cleanup.json"
[[ "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mode"])' "$POLICY")" == dry-run ]] \
  || fail "fresh install did not default to dry-run"
[[ "$(stat -c '%a' "$POLICY")" == 600 ]] || fail "policy is not owner-only"
INSTALLED_CLI="${DEST}/usr/local/bin/aperod-rollout-cleanup"
"$INSTALLED_CLI" --help >/dev/null || fail "installed launcher cannot load its coherent package bundle"
PACKAGE_DIR="$(find "${DEST}/usr/local/lib/aperod-rollout-cleanup/releases" -mindepth 2 -maxdepth 2 -type d -name rollout_cleanup -print -quit)"
[[ -n "$PACKAGE_DIR" && -f "$PACKAGE_DIR/__init__.py" && -f "$PACKAGE_DIR/cli.py" ]] \
  || fail "versioned package bundle is incomplete"
[[ -f "$PACKAGE_DIR/future_package/module.py" ]] || fail "installer omitted a nested package module"
[[ "$(stat -c '%a' "$PACKAGE_DIR/cli.py")" == 644 ]] || fail "installed package source is writable by group/other"

printf '%s\n' 'PACKAGE_GENERATION = "two"' > "${SOURCE_DIR}/rollout_cleanup/future_package/module.py"
SYSTEMCTL="${TMP}/systemctl" bash "${SOURCE_DIR}/setup-rollout-cleanup.sh" --destdir "$DEST" --enable-apply >/dev/null
[[ "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mode"])' "$POLICY")" == apply ]] \
  || fail "--enable-apply did not update the root-owned policy"
[[ "$(find "${DEST}/usr/local/lib/aperod-rollout-cleanup/releases" -mindepth 2 -maxdepth 2 -type d -name rollout_cleanup | wc -l)" -eq 2 ]] \
  || fail "content change did not atomically install a distinct complete package version"
"$INSTALLED_CLI" --help >/dev/null || fail "launcher did not switch to the latest complete package version"
SYSTEMCTL="${TMP}/systemctl" bash "${SOURCE_DIR}/setup-rollout-cleanup.sh" --destdir "$DEST" >/dev/null
[[ "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["mode"])' "$POLICY")" == apply ]] \
  || fail "ordinary reinstall silently changed the explicit apply policy"
[[ "$(wc -l < "$SYSTEMCTL_LOG")" -eq 9 ]] || fail "installer used unexpected or real systemctl actions"
! grep -Eq 'restart|start aperod-node|stop aperod-node' "$SYSTEMCTL_LOG" \
  || fail "installer attempted to restart or stop the node"
pass "installer defaults to dry-run, opts into apply only explicitly, and uses only the systemctl stub"

echo "All rollout cleanup hook/installer tests passed."