#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
CANONICAL="${SCRIPT_DIR}/sync-public-repo.sh"
WRAPPER="${SCRIPT_DIR}/../../scripts/sync-public-repo.sh"
PUSHER="${SCRIPT_DIR}/sync_public_repo.py"
DRIFT_WORKFLOW="${SCRIPT_DIR}/../../.github/workflows/check-public-repo-drift.yml"
DRIFT_ALERT_TEST="${SCRIPT_DIR}/../../.github/scripts/test_alert_public_repo_drift.py"

bash -n "${CANONICAL}"
bash -n "${WRAPPER}"
python3 -m py_compile "${PUSHER}"
python3 "${SCRIPT_DIR}/test_sync_public_repo.py"
PYTHONPATH="${SCRIPT_DIR}/../../.github/scripts" python3 "${DRIFT_ALERT_TEST}"

grep -Fq 'git clone --depth 1' "${CANONICAL}"
grep -Fq 'go build ./...' "${CANONICAL}"
grep -Fq 'go vet ./...' "${CANONICAL}"
grep -Fq 'comm -13 "${VERIFY_DIR}/public.list" "${VERIFY_DIR}/local.list"' "${CANONICAL}"
grep -Fq 'comm -23 "${VERIFY_DIR}/public.list" "${VERIFY_DIR}/local.list"' "${CANONICAL}"
grep -Fq 'unsynced drift file(s) found. Push aborted.' "${CANONICAL}"
grep -Fq '"sha": None' "${PUSHER}"
grep -Fq 'f"{API}/git/trees"' "${PUSHER}"
grep -Fq 'f"{API}/git/commits"' "${PUSHER}"
grep -Fq 'f"{API}/git/refs/heads/main"' "${PUSHER}"

if grep -Fq 'grep -v "_test\\.go"' "${CANONICAL}"; then
  echo "sync must include Go test files" >&2
  exit 1
fi
grep -Fq -- '--no-verify has been removed' "${CANONICAL}"
grep -Fq 'cron: "17 3 * * *"' "${DRIFT_WORKFLOW}"
grep -Fq 'sync-public-repo.sh --check-drift' "${DRIFT_WORKFLOW}"
grep -Fq 'steps.check.outputs.status != '\''0'\''' "${DRIFT_WORKFLOW}"
grep -Fq 'SUPPORT_BOT_TOKEN: ${{ secrets.SUPPORT_BOT_TOKEN }}' "${DRIFT_WORKFLOW}"
grep -Fq 'SUPPORT_ADMIN_CHAT_ID: ${{ secrets.SUPPORT_ADMIN_CHAT_ID }}' "${DRIFT_WORKFLOW}"

grep -Fq 'exec "${REPO_ROOT}/blockchain/deploy/sync-public-repo.sh" "$@"' "${WRAPPER}"
if grep -Fq '/contents/${remote_path}' "${WRAPPER}"; then
  echo "legacy wrapper must not push files individually" >&2
  exit 1
fi

TEMP_DIR=$(mktemp -d)
trap 'rm -rf "${TEMP_DIR}"' EXIT
mkdir -p "${TEMP_DIR}/bin" "${TEMP_DIR}/fixture/orphan"
printf 'package orphan\n' > "${TEMP_DIR}/fixture/orphan/deleted.go"
printf 'package orphan\n' > "${TEMP_DIR}/fixture/orphan/deleted_test.go"
cat > "${TEMP_DIR}/bin/git" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" != "clone" ]]; then
  if [[ "${1:-}" == "-C" && "${3:-}" == "rev-parse" && "${4:-}" == "HEAD" ]]; then
    printf 'head-sha\n'
    exit 0
  fi
  echo "unexpected fake git invocation: $*" >&2
  exit 1
fi
dest="${!#}"
mkdir -p "${dest}"
cp -a "${PUBLIC_REPO_FIXTURE}/." "${dest}/"
EOF
cat > "${TEMP_DIR}/bin/go" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${TEMP_DIR}/bin/git" "${TEMP_DIR}/bin/go"

(
  cd "${REPO_ROOT}"
  PATH="${TEMP_DIR}/bin:${PATH}" \
  PUBLIC_REPO_FIXTURE="${TEMP_DIR}/fixture" \
  PUBLIC_GITHUB_TOKEN="test-token-not-a-secret" \
    "${CANONICAL}" --dry-run > "${TEMP_DIR}/dry-run.log"
)
grep -Fq 'Including deletion in verified sync: orphan/deleted.go' "${TEMP_DIR}/dry-run.log"
grep -Fq 'Including deletion in verified sync: orphan/deleted_test.go' "${TEMP_DIR}/dry-run.log"
grep -Fq -- '--- Dry-run: public files that would be deleted ---' "${TEMP_DIR}/dry-run.log"

echo "PASS: public repo sync is verified, drift-safe, and atomic"