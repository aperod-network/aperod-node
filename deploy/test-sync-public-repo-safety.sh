#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CANONICAL="${SCRIPT_DIR}/sync-public-repo.sh"
WRAPPER="${SCRIPT_DIR}/../../scripts/sync-public-repo.sh"
PUBLISHER="${SCRIPT_DIR}/publish_public_repo.py"
LEGACY="${SCRIPT_DIR}/sync_public_repo.py"
DRIFT_WORKFLOW="${SCRIPT_DIR}/../../.github/workflows/check-public-repo-drift.yml"
DRIFT_ALERT_TEST="${SCRIPT_DIR}/../../.github/scripts/test_alert_public_repo_drift.py"

bash -n "${CANONICAL}"
bash -n "${WRAPPER}"
python3 -m py_compile "${PUBLISHER}" "${LEGACY}"
python3 "${SCRIPT_DIR}/test_sync_public_repo.py"
PYTHONPATH="${SCRIPT_DIR}/../../.github/scripts" python3 "${DRIFT_ALERT_TEST}"

grep -Fq 'publish_public_repo.py' "${CANONICAL}"
grep -Fq 'Unverified publication is forbidden.' "${CANONICAL}"
grep -Fq 'private_repo_markers=markers' "${PUBLISHER}"
grep -Fq 'verify_public_repo_policy(request)' "${PUBLISHER}"
grep -Fq 'f"{API}/git/blobs"' "${PUBLISHER}"
grep -Fq 'f"{API}/git/trees"' "${PUBLISHER}"
grep -Fq 'f"{API}/git/commits"' "${PUBLISHER}"
grep -Fq 'f"{API}/git/refs"' "${PUBLISHER}"
grep -Fq 'f"{API}/pulls"' "${PUBLISHER}"
grep -Fq '"author": AUTHOR' "${PUBLISHER}"
grep -Fq '"committer": AUTHOR' "${PUBLISHER}"
if grep -Fq '/contents/' "${PUBLISHER}" || grep -Fq 'refs/heads/main' "${PUBLISHER}"; then
  echo "publisher must use scanned Git Data objects and protected pull requests" >&2
  exit 1
fi
if ! grep -Fq 'Legacy publication disabled' "${LEGACY}"; then
  echo "legacy publication route must remain disabled" >&2
  exit 1
fi
grep -Fq 'cron: "17 3 * * *"' "${DRIFT_WORKFLOW}"
grep -Fq 'sync-public-repo.sh --check-drift' "${DRIFT_WORKFLOW}"
grep -Fq 'steps.check.outputs.status != '\''0'\''' "${DRIFT_WORKFLOW}"
grep -Fq 'ADMIN_BOT_TOKEN: ${{ secrets.ADMIN_BOT_TOKEN }}' "${DRIFT_WORKFLOW}"
grep -Fq 'ADMIN_CHAT_ID: ${{ secrets.ADMIN_CHAT_ID }}' "${DRIFT_WORKFLOW}"

grep -Fq 'exec "${REPO_ROOT}/blockchain/deploy/sync-public-repo.sh" "$@"' "${WRAPPER}"
if grep -Fq '/contents/${remote_path}' "${WRAPPER}"; then
  echo "legacy wrapper must not push files individually" >&2
  exit 1
fi

echo "PASS: public source publication is scanner-gated, author-verified, and PR-only"