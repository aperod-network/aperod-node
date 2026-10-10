#!/usr/bin/env bash
# Targeted integration tests for closed-checkpoint backup and remote verification.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BACKUP_SH="$SCRIPT_DIR/aperod_backup.sh"
TMPDIR_TEST=$(mktemp -d)
SERVER_PIDS=()
cleanup() {
  local pid
  for pid in "${SERVER_PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
  rm -rf "$TMPDIR_TEST"
}
trap cleanup EXIT

PASS=0
FAIL=0
pass() { echo "PASS: $*"; ((PASS+=1)); }
fail() { echo "FAIL: $*" >&2; ((FAIL+=1)); }

if [[ ! -f "$BACKUP_SH" ]]; then
  echo "backup script not found: $BACKUP_SH" >&2
  exit 1
fi

if grep -q -- '--ignore-failed-read\|--warning=no-file-changed' "$BACKUP_SH"; then
  fail "tar must not ignore unreadable or changing files"
else
  pass "tar fails closed on unreadable/changing archive inputs"
fi
if grep -Eq 'tar .*(-C "\$NODE_DATA_DIR"|"\$NODE_DATA_DIR" \.)' "$BACKUP_SH"; then
  fail "archive must not use the live NODE_DATA_DIR as a tar source"
else
  pass "live node data directory is not archived directly"
fi
for required in 'chain.db' 'manifest.json' 'explorer_db.dump' 'sha256sum' \
  'unix-socket' '_verify_remote_archive' 'CHAIN_DATA_DIR' \
  '_cleanup_validated_snapshot' 'realpath -e' 'PREVIOUS_OBJECT' \
  'uploads[:keep_count]' 'du -sk --apparent-size' \
  'APEROD_BACKUP_VERIFY_BIN:-/usr/local/bin/aperod-backup-verify' \
  'pg_restore --file /dev/null' 'b2_delete_file_version' \
  '_copy_remote_fresh' '--ignore-times' \
  'LEGACY_OBJECT_PREFIX="${BACKUP_NAME}_legacy_"' '--legacy-stage' \
  'preserved-legacy-copy.tar.gpg' 'aperod_backup_legacy_migration' \
  'BACKUP_ROOT_REQUESTED="${APEROD_BACKUP_DIR_OVERRIDE}"' \
  'mktemp -d -- "${BACKUP_ROOT%/}/aperod_backups_XXXXXX"' 'BACKUP_DIR_OWNED=1'; do
  if grep -qF -- "$required" "$BACKUP_SH"; then pass "backup implementation contains $required"
  else fail "backup implementation is missing $required"; fi
done
DB_PREFLIGHT_LINE=$(grep -n 'CHAIN_REQUIRED_KB=' "$BACKUP_SH" | head -1 | cut -d: -f1)
CHECKPOINT_POST_LINE=$(grep -n '"http://localhost/checkpoint"' "$BACKUP_SH" | head -1 | cut -d: -f1)
if [[ -n "$DB_PREFLIGHT_LINE" && -n "$CHECKPOINT_POST_LINE" && "$DB_PREFLIGHT_LINE" -lt "$CHECKPOINT_POST_LINE" ]]; then
  pass "active chain.db size plus reserve is checked before checkpoint request"
else
  fail "checkpoint request can occur before the active-database disk preflight"
fi
AUTH_BEGIN_LINE=$(grep -n 'if "\$AUTH_HELPER" backup-begin' "$BACKUP_SH" | head -1 | cut -d: -f1)
AUTH_PIN_LINE=$(grep -n '"$AUTH_HELPER" backup-pin' "$BACKUP_SH" | head -1 | cut -d: -f1)
FIXED_UPLOAD_LINE=$(grep -n 'rclone copyto "$ARCHIVE_FILE" "$FIXED_REMOTE"' "$BACKUP_SH" | head -1 | cut -d: -f1)
FIXED_READBACK_LINE=$(grep -n 'if ! _verify_remote_archive "$FIXED_REMOTE"' "$BACKUP_SH" | head -1 | cut -d: -f1)
AUTH_PUBLISH_LINE=$(grep -n '"$AUTH_HELPER" backup-publish' "$BACKUP_SH" | head -1 | cut -d: -f1)
RETENTION_END_LINE=$(grep -n 'done <<< "$PRESERVED_LEGACY_OBJECTS"' "$BACKUP_SH" | tail -1 | cut -d: -f1)
if [[ -n "$AUTH_BEGIN_LINE" && "$AUTH_BEGIN_LINE" -lt "$CHECKPOINT_POST_LINE" ]]; then
  pass "backup-begin captures authorization context and anchors before checkpoint request"
else
  fail "backup-begin is missing or runs after checkpoint request"
fi
if [[ -n "$AUTH_PIN_LINE" && -n "$FIXED_UPLOAD_LINE" && -n "$FIXED_READBACK_LINE" \
  && "$FIXED_UPLOAD_LINE" -lt "$AUTH_PIN_LINE" && "$AUTH_PIN_LINE" -lt "$FIXED_READBACK_LINE" ]]; then
  pass "B2 exact-object pin is ordered after fixed upload and before its fresh readback"
else
  fail "B2 pin is not ordered between fixed upload and fixed readback"
fi
if [[ -n "$AUTH_PUBLISH_LINE" && "$AUTH_PUBLISH_LINE" -gt "$FIXED_READBACK_LINE" \
  && -n "$RETENTION_END_LINE" && "$AUTH_PUBLISH_LINE" -gt "$RETENTION_END_LINE" ]] \
  && grep -q '_disable_backup_authorization "backup-pin-failed"' "$BACKUP_SH" \
  && grep -q '_disable_backup_authorization "backup-proof-failed"' "$BACKUP_SH" \
  && grep -q '_abort_backup_authorization' "$BACKUP_SH"; then
  pass "authorization publishes only after fixed readback/retention and helper/proof failures disable with an abort hook"
else
  fail "authorization failure handling or post-retention publish ordering is incomplete"
fi
AUTH_FUNCTIONS="$TMPDIR_TEST/auth-functions.sh"
for function_name in _disable_backup_authorization _abort_backup_authorization; do
  awk -v name="$function_name" '
    $0 ~ "^" name "\\(\\) \\{" { emit=1 }
    emit { print }
    emit && /^}$/ { exit }
  ' "$BACKUP_SH" >> "$AUTH_FUNCTIONS"
done
AUTH_MOCK="$TMPDIR_TEST/auth-helper-mock"
cat > "$AUTH_MOCK" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$AUTH_HELPER_LOG"
[[ "${MOCK_AUTH_ABORT_FAIL:-0}" != 1 ]]
STUB
chmod +x "$AUTH_MOCK"
(
  source "$AUTH_FUNCTIONS"
  _auth_output_is_safe() { [[ "${MOCK_CONTEXT_SAFE:-0}" == 1 ]]; }
  AUTH_HELPER_READY=1
  AUTH_HELPER="$AUTH_MOCK"
  AUTH_CONTEXT_FILE="$TMPDIR_TEST/auth-context.json"
  AUTH_ATTEMPT_REGISTERED=1
  AUTH_ENABLED=1
  AUTH_PINNED=1
  AUTH_HELPER_LOG="$TMPDIR_TEST/auth-helper.log"
  MOCK_CONTEXT_SAFE=1
  export AUTH_HELPER_LOG
  export MOCK_CONTEXT_SAFE
  _disable_backup_authorization "mock-proof-failure"
  [[ "$AUTH_ENABLED" == 0 && "$AUTH_PINNED" == 0 ]]
  AUTH_ATTEMPT_REGISTERED=1
  _abort_backup_authorization
  [[ "$AUTH_ATTEMPT_REGISTERED" == 0 ]]
  MOCK_AUTH_ABORT_FAIL=1
  export MOCK_AUTH_ABORT_FAIL
  AUTH_ENABLED=1
  _disable_backup_authorization "mock-helper-failure"
  [[ "$AUTH_ENABLED" == 0 ]]
  [[ "$(wc -l < "$AUTH_HELPER_LOG")" -eq 3 ]]
) && pass "authorization proof failure aborts registered context and leaves backup flow non-fatal" \
  || fail "authorization disable/abort failure did not leave independent backup flow non-fatal"
VERIFY_LINE=$(grep -n 'if ! _verify_remote_archive "$FIXED_REMOTE"' "$BACKUP_SH" | head -1 | cut -d: -f1)
CANDIDATE_DELETE_LINE=$(grep -n 'rclone deletefile "$CANDIDATE_REMOTE"' "$BACKUP_SH" | tail -1 | cut -d: -f1)
if [[ -n "$VERIFY_LINE" && -n "$CANDIDATE_DELETE_LINE" && "$CANDIDATE_DELETE_LINE" -gt "$VERIFY_LINE" ]]; then
  pass "candidate is retained until fixed replacement verification succeeds"
else
  fail "candidate can be deleted before fixed replacement is verified"
fi
B2_CANDIDATE_CLEAN_LINE=$(grep -n '_cleanup_b2_object_versions "$CANDIDATE_OBJECT" 0' "$BACKUP_SH" | tail -1 | cut -d: -f1)
B2_MIGRATION_CANDIDATE_CLEAN_LINE=$(grep -n '_cleanup_b2_object_versions "$CANDIDATE_OBJECT" 0' "$BACKUP_SH" | head -1 | cut -d: -f1)
FIXED_PROMOTION_VERIFY_LINE=$(grep -n 'if ! _verify_remote_archive "$FIXED_REMOTE"' "$BACKUP_SH" | head -1 | cut -d: -f1)
PREVIOUS_VERIFY_LINE=$(grep -n 'HAVE_PREVIOUS_VERIFIED=1' "$BACKUP_SH" | tail -1 | cut -d: -f1)
if [[ -n "$B2_CANDIDATE_CLEAN_LINE" && -n "$PREVIOUS_VERIFY_LINE" \
  && "$B2_CANDIDATE_CLEAN_LINE" -gt "$PREVIOUS_VERIFY_LINE" ]] \
  && grep -B4 -A2 -F 'rclone deletefile "$CANDIDATE_REMOTE"' "$BACKUP_SH" \
    | grep -q 'if \[ "$IS_B2" -eq 1 \]'; then
  pass "B2 candidate versions are removed natively after verification; generic deletion is provider-guarded"
else
  fail "B2 candidate cleanup can create hide markers or run before verification"
fi

# Exercise the production EXIT cleanup trap without requiring root/network tools.
CLEANUP_FUNCTIONS="$TMPDIR_TEST/cleanup-functions.sh"
awk '
  /^_cleanup_validated_snapshot\(\) \{/ { emit=1 }
  emit { print }
  /^trap _on_exit EXIT$/ { exit }
' "$BACKUP_SH" > "$CLEANUP_FUNCTIONS"
CLEANUP_ROOT="$TMPDIR_TEST/cleanup-trap"
mkdir -p "$CLEANUP_ROOT/staging/closed-stage" "$CLEANUP_ROOT/workroot" "$CLEANUP_ROOT/caller-work"
printf 'preserve\n' > "$CLEANUP_ROOT/caller-work/keep"
printf 'parent sentinel\n' > "$CLEANUP_ROOT/workroot/keep"
OWNED_WORK=$(mktemp -d "$CLEANUP_ROOT/workroot/aperod_backups_XXXXXX")
OWNED_ROOT=$(realpath -e "$CLEANUP_ROOT/workroot")
OWNED_CANONICAL=$(realpath -e "$OWNED_WORK")
set +e
(
  source "$CLEANUP_FUNCTIONS"
  _write_history_log() { :; }
  STAGING_ROOT="$CLEANUP_ROOT/staging"
  SNAPSHOT_DIR="$CLEANUP_ROOT/staging/closed-stage"
  SNAPSHOT_VALIDATED=1
  BACKUP_ROOT="$OWNED_ROOT"
  BACKUP_DIR="$OWNED_CANONICAL"
  BACKUP_DIR_CANONICAL="$OWNED_CANONICAL"
  BACKUP_DIR_OWNED=1
  AUTH_ATTEMPT_REGISTERED=0
  exit 23
)
CLEANUP_STATUS=$?
set -e
if [[ $CLEANUP_STATUS -eq 23 ]] \
  && [[ ! -e "$CLEANUP_ROOT/staging/closed-stage" ]] \
  && [[ ! -e "$OWNED_WORK" ]] \
  && [[ -f "$CLEANUP_ROOT/workroot/keep" ]]; then
  pass "EXIT trap removes only its unique owned child and validated stage, preserving status and parent"
else
  fail "EXIT trap cleanup or exit-status preservation failed (exit=$CLEANUP_STATUS)"
fi
set +e
(
  source "$CLEANUP_FUNCTIONS"
  _write_history_log() { :; }
  SNAPSHOT_VALIDATED=0
  STAGING_ROOT=""
  SNAPSHOT_DIR=""
  BACKUP_ROOT="$CLEANUP_ROOT"
  BACKUP_DIR="$CLEANUP_ROOT/caller-work"
  BACKUP_DIR_CANONICAL="$CLEANUP_ROOT/caller-work"
  BACKUP_DIR_OWNED=0
  AUTH_ATTEMPT_REGISTERED=0
  exit 19
)
UNOWNED_CLEANUP_STATUS=$?
set -e
if [[ $UNOWNED_CLEANUP_STATUS -eq 19 ]] && [[ -f "$CLEANUP_ROOT/caller-work/keep" ]]; then
  pass "EXIT trap never removes a preexisting caller-provided work directory"
else
  fail "EXIT trap removed or modified a caller-provided work directory"
fi

mkdir -p "$CLEANUP_ROOT/staging/real-stage"
ln -s real-stage "$CLEANUP_ROOT/staging/link-stage"
(
  source "$CLEANUP_FUNCTIONS"
  _write_history_log() { :; }
  STAGING_ROOT="$CLEANUP_ROOT/staging"
  SNAPSHOT_DIR="$CLEANUP_ROOT/staging/link-stage"
  SNAPSHOT_VALIDATED=1
  BACKUP_ROOT="$CLEANUP_ROOT"
  BACKUP_DIR="$CLEANUP_ROOT/work-link"
  BACKUP_DIR_CANONICAL="$CLEANUP_ROOT/work-link"
  BACKUP_DIR_OWNED=0
  AUTH_ATTEMPT_REGISTERED=0
  trap - EXIT
  if _cleanup_validated_snapshot 2>/dev/null; then exit 1; fi
  [[ -d "$CLEANUP_ROOT/staging/real-stage" ]]
)
if [[ -L "$CLEANUP_ROOT/staging/link-stage" ]] && [[ -d "$CLEANUP_ROOT/staging/real-stage" ]]; then
  pass "EXIT cleanup refuses a symlink and leaves its target untouched"
else
  fail "EXIT cleanup followed or removed a symlink target"
fi

# Exercise the production B2 exact-name version deletion twice against a local
# urllib shim: deletion is idempotent and never touches similarly named objects.
B2_FUNCTIONS="$TMPDIR_TEST/b2-functions.sh"
awk '
  /^_cleanup_b2_object_versions\(\) \{/ { emit=1 }
  emit { print }
  /^# ── Guard: create a private/ { exit }
' "$BACKUP_SH" > "$B2_FUNCTIONS"
B2_MOCK_ROOT="$TMPDIR_TEST/b2-mock"
mkdir -p "$B2_MOCK_ROOT/urllib"
cat > "$B2_MOCK_ROOT/urllib/__init__.py" <<'PY'
PY
cat > "$B2_MOCK_ROOT/urllib/request.py" <<'PY'
import json, os

class Request:
    def __init__(self, url, data=None, headers=None):
        self.full_url = url
        self.data = data
        self.headers = headers or {}

class Response:
    def __init__(self, payload):
        self.payload = json.dumps(payload).encode()
    def __enter__(self):
        return self
    def __exit__(self, *args):
        return False
    def read(self, *args):
        payload, self.payload = self.payload, b""
        return payload

def urlopen(request, timeout=30):
    if request.full_url.endswith("/b2_authorize_account"):
        return Response({
            "apiUrl": "https://api.mock",
            "authorizationToken": "test-token",
            "accountId": "test-account",
            "allowed": {"bucketId": "test-bucket-id"},
        })
    payload = json.loads(request.data.decode())
    state_path = os.environ["B2_MOCK_STATE"]
    with open(state_path, encoding="utf-8") as source:
        state = json.load(source)
    if request.full_url.endswith("/b2_list_file_versions"):
        items = [entry for entry in state["files"]
                 if entry["fileName"] == payload["prefix"]]
        return Response({"files": items, "nextFileName": None, "nextFileId": None})
    if request.full_url.endswith("/b2_delete_file_version"):
        if os.environ.get("B2_FAIL_DELETE_FILE") == payload["fileName"]:
            raise RuntimeError("mocked B2 exact-version deletion failure")
        before = len(state["files"])
        state["files"] = [entry for entry in state["files"]
                          if entry["fileId"] != payload["fileId"]]
        if len(state["files"]) == before:
            raise RuntimeError("delete requested for an unknown fileId")
        with open(state_path, "w", encoding="utf-8") as target:
            json.dump(state, target)
        return Response({})
    raise RuntimeError("unexpected B2 API request")
PY
B2_MOCK_STATE="$B2_MOCK_ROOT/state.json"
cat > "$B2_MOCK_STATE" <<'JSON'
{"files":[
 {"fileName":"aperod_backup_candidate_test.tar.gpg","fileId":"candidate-upload","action":"upload","uploadTimestamp":30,"contentLength":100},
 {"fileName":"aperod_backup_candidate_test.tar.gpg","fileId":"candidate-hide","action":"hide","uploadTimestamp":31,"contentLength":0},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-current","action":"upload","uploadTimestamp":30,"contentLength":200},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-previous","action":"upload","uploadTimestamp":20,"contentLength":190},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-legacy","action":"upload","uploadTimestamp":10,"contentLength":180},
 {"fileName":"aperod_backup_legacy_fixture.tar.gpg","fileId":"preserved-legacy","action":"upload","uploadTimestamp":25,"contentLength":180},
 {"fileName":"aperod_backup_candidate_other.tar.gpg","fileId":"other-candidate","action":"upload","uploadTimestamp":30,"contentLength":110}
]}
JSON
(
  source "$B2_FUNCTIONS"
  S3_ENDPOINT="https://s3.backblazeb2.com"
  S3_ACCESS="test-access"
  S3_SECRET="test-secret"
  S3_BUCKET="test-bucket"
  export B2_MOCK_STATE
  export PYTHONPATH="$B2_MOCK_ROOT${PYTHONPATH:+:$PYTHONPATH}"
  _cleanup_b2_object_versions "aperod_backup_candidate_test.tar.gpg" 0
  _cleanup_b2_object_versions "aperod_backup_candidate_test.tar.gpg" 0
)
if python3 - "$B2_MOCK_STATE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    remaining = {entry["fileId"] for entry in json.load(source)["files"]}
raise SystemExit(0 if remaining == {
    "fixed-current", "fixed-previous", "fixed-legacy",
    "preserved-legacy", "other-candidate"
} else 1)
PY
then
  pass "first-migration B2 candidate is deleted exactly while fixed and preserved legacy versions remain"
else
  fail "B2 first-migration cleanup removed a fixed/preserved legacy version or leaked candidate versions"
fi
cat > "$B2_MOCK_STATE" <<'JSON'
{"files":[
 {"fileName":"aperod_backup_candidate_migration.tar.gpg","fileId":"candidate-upload","action":"upload","uploadTimestamp":40,"contentLength":160},
 {"fileName":"aperod_backup_candidate_migration.tar.gpg","fileId":"candidate-hide","action":"hide","uploadTimestamp":41,"contentLength":0},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-new","action":"upload","uploadTimestamp":40,"contentLength":160},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-legacy","action":"upload","uploadTimestamp":30,"contentLength":200},
 {"fileName":"aperod_backup_legacy_fixture.tar.gpg","fileId":"preserved-legacy","action":"upload","uploadTimestamp":39,"contentLength":200}
]}
JSON
if (
  source "$B2_FUNCTIONS"
  S3_ENDPOINT="https://s3.backblazeb2.com"
  S3_ACCESS="test-access"
  S3_SECRET="test-secret"
  S3_BUCKET="test-bucket"
  export B2_MOCK_STATE
  export PYTHONPATH="$B2_MOCK_ROOT${PYTHONPATH:+:$PYTHONPATH}"
  export B2_FAIL_DELETE_FILE="aperod_backup_candidate_migration.tar.gpg"
  _cleanup_b2_object_versions "aperod_backup_candidate_migration.tar.gpg" 0 2>/dev/null
); then
  fail "B2 candidate cleanup unexpectedly succeeded despite the mocked delete failure"
elif python3 - "$B2_MOCK_STATE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    ids = {entry["fileId"] for entry in json.load(source)["files"]}
raise SystemExit(0 if {
    "fixed-new", "fixed-legacy", "preserved-legacy"
}.issubset(ids) else 1)
PY
then
  pass "failed B2 candidate deletion leaves the verified fixed and preserved legacy copies intact"
else
  fail "B2 candidate cleanup failure affected fixed or preserved legacy recovery versions"
fi
cat > "$B2_MOCK_STATE" <<'JSON'
{"files":[
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-current","action":"upload","uploadTimestamp":30,"contentLength":200},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-previous","action":"upload","uploadTimestamp":20,"contentLength":190},
 {"fileName":"aperod_backup.tar.gpg","fileId":"fixed-legacy","action":"upload","uploadTimestamp":10,"contentLength":180},
 {"fileName":"aperod_backup_legacy_fixture.tar.gpg","fileId":"preserved-legacy","action":"upload","uploadTimestamp":25,"contentLength":180},
 {"fileName":"aperod_backup_candidate_other.tar.gpg","fileId":"other-candidate","action":"upload","uploadTimestamp":30,"contentLength":110}
]}
JSON
(
  source "$B2_FUNCTIONS"
  S3_ENDPOINT="https://s3.backblazeb2.com"
  S3_ACCESS="test-access"
  S3_SECRET="test-secret"
  S3_BUCKET="test-bucket"
  export B2_MOCK_STATE
  export PYTHONPATH="$B2_MOCK_ROOT${PYTHONPATH:+:$PYTHONPATH}"
  _cleanup_b2_object_versions "aperod_backup.tar.gpg" 2
)
if python3 - "$B2_MOCK_STATE" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    remaining = {entry["fileId"] for entry in json.load(source)["files"]}
raise SystemExit(0 if remaining == {
    "fixed-current", "fixed-previous", "preserved-legacy", "other-candidate"
} else 1)
PY
then
  pass "B2 retains the two newest fixed upload IDs while pruning a legacy third version"
else
  fail "B2 fixed retention failed to preserve exactly current and previous generations"
fi
if [[ -n "$B2_CANDIDATE_CLEAN_LINE" && -n "$PREVIOUS_VERIFY_LINE" \
  && "$B2_CANDIDATE_CLEAN_LINE" -gt "$PREVIOUS_VERIFY_LINE" ]]; then
  pass "failed promotion path cannot reach B2 candidate-version cleanup"
else
  fail "B2 candidate cleanup is not ordered after successful fixed and previous verification"
fi
B2_MIGRATION_CANDIDATE_B2_GUARD=0
if [[ -n "$B2_MIGRATION_CANDIDATE_CLEAN_LINE" ]] \
  && sed -n "$((B2_MIGRATION_CANDIDATE_CLEAN_LINE - 1))p" "$BACKUP_SH" \
    | grep -qF 'if [ "$IS_B2" -eq 1 ]; then'; then
  B2_MIGRATION_CANDIDATE_B2_GUARD=1
fi
if [[ "$B2_MIGRATION_CANDIDATE_B2_GUARD" -eq 1 \
  && -n "$B2_MIGRATION_CANDIDATE_CLEAN_LINE" && -n "$FIXED_PROMOTION_VERIFY_LINE" \
  && "$B2_MIGRATION_CANDIDATE_CLEAN_LINE" -gt "$FIXED_PROMOTION_VERIFY_LINE" ]]; then
  pass "B2 legacy-migration candidate uses exact fileId cleanup only after verified fixed promotion"
else
  fail "B2 migration candidate cleanup can precede independent fixed verification"
fi

# Reproduce rclone's same-size/same-mtime skip behavior. Candidate verification
# must not leave a reusable download that can mask corrupt bytes at the fixed
# key, and rollback must freshly retrieve and verify the previous bytes.
REMOTE_COPY_FUNCTIONS="$TMPDIR_TEST/remote-copy-functions.sh"
awk '
  /^_copy_remote_fresh\(\) \{/ { emit=1; kind=1 }
  /^_verify_remote_archive\(\) \{/ { emit=1; kind=2 }
  emit { print }
  kind == 1 && /^}$/ { emit=0; kind=0 }
  kind == 2 && /^}$/ { exit }
' "$BACKUP_SH" > "$REMOTE_COPY_FUNCTIONS"
RESTORE_FUNCTION="$TMPDIR_TEST/restore-previous-fixed.sh"
awk '
  /^_restore_previous_fixed\(\) \{/ { emit=1 }
  emit { print }
  emit && /^}$/ { exit }
' "$BACKUP_SH" > "$RESTORE_FUNCTION"
REMOTE_COPY_ROOT="$TMPDIR_TEST/forced-remote-copy"
mkdir -p "$REMOTE_COPY_ROOT/store"
printf 'candidate-good' > "$REMOTE_COPY_ROOT/store/candidate.tar.gpg"
printf 'candidate-evil' > "$REMOTE_COPY_ROOT/store/fixed.tar.gpg"
printf 'previous-good' > "$REMOTE_COPY_ROOT/previous.tar.gpg"
touch -t 202001010101 "$REMOTE_COPY_ROOT/store/candidate.tar.gpg" \
  "$REMOTE_COPY_ROOT/store/fixed.tar.gpg"
REMOTE_COPY_SIZE=$(stat -c '%s' "$REMOTE_COPY_ROOT/store/candidate.tar.gpg")
REMOTE_COPY_SHA=$(sha256sum "$REMOTE_COPY_ROOT/store/candidate.tar.gpg" | awk '{print $1}')
REMOTE_PREVIOUS_SHA=$(sha256sum "$REMOTE_COPY_ROOT/previous.tar.gpg" | awk '{print $1}')
(
  source "$REMOTE_COPY_FUNCTIONS"
  source "$RESTORE_FUNCTION"
  _require_download_space() { :; }
  _verify_archive_payload() { [[ "$(cat "$1")" == "candidate-good" ]]; }
  rclone() {
    local action="$1" source="$2" target="${3:-}" file ignore_times=0
    shift 3 || true
    if [[ "$action" == copyto ]]; then
      for argument in "$@"; do [[ "$argument" != --ignore-times ]] || ignore_times=1; done
      printf '%s\n' "$source $target $*" >> "$REMOTE_COPY_ROOT/copy.log"
      if [[ "$source" == s3backup:* ]]; then
        file="$REMOTE_COPY_ROOT/store/${source##*/}"
        [[ -f "$file" ]]
        if [[ $ignore_times -eq 0 && -f "$target" ]] \
          && [[ "$(stat -c '%s:%Y' "$file")" == "$(stat -c '%s:%Y' "$target")" ]]; then
          return 0
        fi
        cp -p "$file" "$target"
      else
        cp -p "$source" "$REMOTE_COPY_ROOT/store/${target##*/}"
      fi
      return
    fi
    if [[ "$action" == size ]]; then
      file="$REMOTE_COPY_ROOT/store/${source##*/}"
      printf '{"count":1,"bytes":%s}\n' "$(stat -c '%s' "$file")"
      return
    fi
    return 2
  }
  BACKUP_DIR="$REMOTE_COPY_ROOT"
  ARCHIVE_SIZE="$REMOTE_COPY_SIZE"
  ARCHIVE_SHA256="$REMOTE_COPY_SHA"
  DOWNLOAD_FILE="$REMOTE_COPY_ROOT/download.tar.gpg"
  CANDIDATE_REMOTE="s3backup:test-bucket/candidate.tar.gpg"
  FIXED_REMOTE="s3backup:test-bucket/fixed.tar.gpg"
  FIXED_EXISTS=yes
  PREVIOUS_FIXED="$REMOTE_COPY_ROOT/previous.tar.gpg"
  PREVIOUS_FIXED_SIZE="$REMOTE_COPY_SIZE"
  PREVIOUS_FIXED_SHA="$REMOTE_PREVIOUS_SHA"
  if ! _verify_remote_archive "$CANDIDATE_REMOTE" "$DOWNLOAD_FILE" \
    "$REMOTE_COPY_SIZE" "$REMOTE_COPY_SHA"; then
    exit 1
  fi
  if _verify_remote_archive "$FIXED_REMOTE" "$DOWNLOAD_FILE" \
    "$REMOTE_COPY_SIZE" "$REMOTE_COPY_SHA"; then
    exit 1
  fi
  if ! _restore_previous_fixed; then exit 1; fi
  cmp -s "$REMOTE_COPY_ROOT/store/fixed.tar.gpg" "$REMOTE_COPY_ROOT/previous.tar.gpg"
  cmp -s "$REMOTE_COPY_ROOT/store/candidate.tar.gpg" <(printf 'candidate-good')
  grep -q -- '--ignore-times' "$REMOTE_COPY_ROOT/copy.log"
)
if [[ $? -eq 0 ]]; then
  pass "same-size/same-mtime corrupt fixed download fails verification; rollback preserves prior fixed and candidate"
else
  fail "corrupt fixed download was masked or rollback failed to preserve prior/candidate"
fi

# Run the production safe extractor against a valid archive and a traversal
# archive, independent of root, GPG, the live node, and remote storage.
EXTRACTOR_SOURCE="$TMPDIR_TEST/safe-extract.py"
awk '
  /cat > "\$extractor"/ { emit=1; next }
  emit && /^PY$/ { exit }
  emit { print }
' "$BACKUP_SH" > "$EXTRACTOR_SOURCE"
EXTRACT_ROOT="$TMPDIR_TEST/extractor"
mkdir -p "$EXTRACT_ROOT/source/chain.db" "$EXTRACT_ROOT/unpacked"
printf 'leveldb fixture\n' > "$EXTRACT_ROOT/source/chain.db/CURRENT"
printf '{"success":true,"tip_height":1,"tip_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' > "$EXTRACT_ROOT/source/manifest.json"
printf 'pg dump fixture\n' > "$EXTRACT_ROOT/source/explorer_db.dump"
tar -czf "$EXTRACT_ROOT/valid.tar.gz" -C "$EXTRACT_ROOT/source" chain.db manifest.json explorer_db.dump
if [[ -s "$EXTRACTOR_SOURCE" ]] \
  && python3 "$EXTRACTOR_SOURCE" --inspect "$EXTRACT_ROOT/valid.tar.gz" >/dev/null \
  && python3 "$EXTRACTOR_SOURCE" --extract "$EXTRACT_ROOT/valid.tar.gz" "$EXTRACT_ROOT/unpacked" \
  && cmp -s "$EXTRACT_ROOT/source/chain.db/CURRENT" "$EXTRACT_ROOT/unpacked/chain.db/CURRENT"; then
  pass "remote archive extractor accepts valid members and extracts into isolated stage"
else
  fail "remote archive safe extractor failed valid archive"
fi
python3 - "$EXTRACT_ROOT/valid.tar.gz" "$EXTRACT_ROOT/unsafe.tar.gz" <<'PY'
import io, sys, tarfile
with tarfile.open(sys.argv[1], "r:gz") as source, tarfile.open(sys.argv[2], "w:gz") as target:
    for member in source.getmembers():
        target.addfile(member, source.extractfile(member) if member.isfile() else None)
    bad = tarfile.TarInfo("../escape")
    bad.size = 1
    target.addfile(bad, io.BytesIO(b"x"))
PY
if python3 "$EXTRACTOR_SOURCE" --inspect "$EXTRACT_ROOT/unsafe.tar.gz" >/dev/null 2>&1; then
  fail "remote archive extractor accepted path traversal"
else
  pass "remote archive extractor rejects path traversal before extraction"
fi
mkdir -p "$EXTRACT_ROOT/legacy-source/testnet/chain.db"
printf 'legacy leveldb fixture\n' > "$EXTRACT_ROOT/legacy-source/testnet/chain.db/CURRENT"
printf 'legacy pg dump fixture\n' > "$EXTRACT_ROOT/legacy-source/explorer_db.dump"
printf '{"legacy":true}\n' > "$EXTRACT_ROOT/legacy-source/old-config.json"
tar -czf "$EXTRACT_ROOT/legacy.tar.gz" -C "$EXTRACT_ROOT/legacy-source" testnet explorer_db.dump old-config.json
if python3 "$EXTRACTOR_SOURCE" --inspect "$EXTRACT_ROOT/legacy.tar.gz" >/dev/null 2>&1; then
  fail "legacy testnet/chain.db archive was accepted as a new-format verified generation"
else
  pass "legacy testnet/chain.db layout fails closed rather than being counted as a new-format generation"
fi
mkdir -p "$EXTRACT_ROOT/legacy-unpacked"
if python3 "$EXTRACTOR_SOURCE" --inspect-legacy "$EXTRACT_ROOT/legacy.tar.gz" >/dev/null \
  && python3 "$EXTRACTOR_SOURCE" --extract-legacy "$EXTRACT_ROOT/legacy.tar.gz" "$EXTRACT_ROOT/legacy-unpacked" \
  && cmp -s "$EXTRACT_ROOT/legacy-source/testnet/chain.db/CURRENT" \
    "$EXTRACT_ROOT/legacy-unpacked/testnet/chain.db/CURRENT"; then
  pass "legacy extractor safely preserves required relative paths and permits extra regular files"
else
  fail "legacy extractor rejected a valid relative layout or extracted incorrectly"
fi
python3 - "$EXTRACT_ROOT/legacy.tar.gz" "$EXTRACT_ROOT/legacy-symlink.tar.gz" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[1], "r:gz") as source, tarfile.open(sys.argv[2], "w:gz") as target:
    for member in source.getmembers():
        target.addfile(member, source.extractfile(member) if member.isfile() else None)
    link = tarfile.TarInfo("testnet/chain.db/unsafe-link")
    link.type = tarfile.SYMTYPE
    link.linkname = "/etc/passwd"
    target.addfile(link)
PY
if python3 "$EXTRACTOR_SOURCE" --inspect-legacy "$EXTRACT_ROOT/legacy-symlink.tar.gz" >/dev/null 2>&1; then
  fail "legacy extractor accepted a symlink member"
else
  pass "legacy extractor rejects symlinks before extraction"
fi
FIXED_ARCHIVE_VERIFY_LINE=$(grep -n '_verify_archive_payload "$PREVIOUS_FIXED"' "$BACKUP_SH" | head -1 | cut -d: -f1)
CANDIDATE_UPLOAD_LINE=$(grep -n 'rclone copyto "$ARCHIVE_FILE" "$CANDIDATE_REMOTE"' "$BACKUP_SH" | head -1 | cut -d: -f1)
if [[ -n "$FIXED_ARCHIVE_VERIFY_LINE" && -n "$CANDIDATE_UPLOAD_LINE" \
  && "$FIXED_ARCHIVE_VERIFY_LINE" -lt "$CANDIDATE_UPLOAD_LINE" ]]; then
  pass "unverifiable existing fixed object stops before upload or overwrite, preserving the legacy ciphertext"
else
  fail "existing fixed object can be overwritten before verification"
fi

if command -v gpg >/dev/null 2>&1; then
  ARCHIVE_VERIFY_FUNCTIONS="$TMPDIR_TEST/archive-verify-functions.sh"
  awk '
    /^_cleanup_archive_verify_dir\(\) \{/ { emit=1 }
    /^_verify_remote_archive\(\) \{/ { exit }
    emit { print }
  ' "$BACKUP_SH" > "$ARCHIVE_VERIFY_FUNCTIONS"
  VERIFY_MODE_ROOT="$TMPDIR_TEST/verifier-modes"
  mkdir -p "$VERIFY_MODE_ROOT/bin"
  cat > "$VERIFY_MODE_ROOT/bin/verifier-mock" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  --stage)
    python3 - "$2" <<'PY'
import json, os, sys
root = sys.argv[1]
with open(os.path.join(root, "manifest.json"), encoding="utf-8") as source:
    manifest = json.load(source)
print(json.dumps({"success": True, "tip_hash": manifest["tip_hash"],
                  "tip_height": manifest["tip_height"]}))
PY
    ;;
  --legacy-stage)
    [[ -f "$2/testnet/chain.db/CURRENT" && -f "$2/explorer_db.dump" ]]
    printf '%s\n' '{"success":true,"legacy":true,"tip_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tip_height":41}'
    ;;
  *) exit 2 ;;
esac
STUB
  cat > "$VERIFY_MODE_ROOT/bin/pg_restore" <<'STUB'
#!/usr/bin/env bash
[[ "$1" == --file && "$2" == /dev/null && -f "$3" ]]
STUB
  chmod +x "$VERIFY_MODE_ROOT/bin/verifier-mock" "$VERIFY_MODE_ROOT/bin/pg_restore"
  tar -czf "$VERIFY_MODE_ROOT/legacy.tar.gz" -C "$EXTRACT_ROOT/legacy-source" testnet explorer_db.dump old-config.json
  gpg --batch --yes --passphrase 'test-only-passphrase' --symmetric --cipher-algo AES256 \
    -o "$VERIFY_MODE_ROOT/legacy.tar.gpg" "$VERIFY_MODE_ROOT/legacy.tar.gz" >/dev/null 2>&1
  tar -czf "$VERIFY_MODE_ROOT/new.tar.gz" -C "$EXTRACT_ROOT/source" chain.db manifest.json explorer_db.dump
  gpg --batch --yes --passphrase 'test-only-passphrase' --symmetric --cipher-algo AES256 \
    -o "$VERIFY_MODE_ROOT/new.tar.gpg" "$VERIFY_MODE_ROOT/new.tar.gz" >/dev/null 2>&1
  if (
    source "$ARCHIVE_VERIFY_FUNCTIONS"
    BACKUP_DIR="$VERIFY_MODE_ROOT"
    ENCRYPTION_PASSWORD="test-only-passphrase"
    BACKUP_VERIFY_BIN="$VERIFY_MODE_ROOT/bin/verifier-mock"
    PATH="$VERIFY_MODE_ROOT/bin:$PATH"
    _verify_archive_payload "$VERIFY_MODE_ROOT/legacy.tar.gpg" "" "" legacy
    _verify_archive_payload "$VERIFY_MODE_ROOT/new.tar.gpg" "1" \
      "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" new
  ); then
    pass "production archive verifier invokes distinct legacy/new verifier modes and pg_restore"
  else
    fail "production archive verifier rejected a valid legacy or new-format fixture"
  fi
else
  echo "SKIP: encrypted legacy/new verifier mode test requires gpg."
fi

if [[ "$EUID" -ne 0 ]]; then
  echo "SKIP: end-to-end socket/archive scenarios require root; cleanup trap tests ran."
  if [[ $FAIL -eq 0 ]]; then echo "All $PASS targeted backup tests passed."; exit 0; fi
  echo "$FAIL failure(s), $PASS pass(es)." >&2
  exit 1
fi
make_fixture() {
  local root="$1" mode="${2:-valid}"
  local node="$root/node-data/testnet" stage="$root/node-data/testnet/.backup-staging/checkpoint-test"
  mkdir -p "$stage" "$root/metrics" "$root/remote"
  mkdir -p "$node/chain.db"
  printf 'active leveldb fixture\n' > "$node/chain.db/CURRENT"
  mkdir -p "$stage/chain.db"
  printf '0000000000000001\n' > "$stage/chain.db/CURRENT"
  printf 'leveldb-manifest\n' > "$stage/chain.db/MANIFEST-000001"
  printf 'table data\n' > "$stage/chain.db/000003.ldb"
  printf '{"success":true,"tip_height":42,"tip_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' > "$stage/manifest.json"
  local stage_response="$stage"
  case "$mode" in
    outside)
      mkdir -p "$root/outside/chain.db"
      printf 'outside\n' > "$root/outside/chain.db/CURRENT"
      printf '{"success":true,"tip_height":42,"tip_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' > "$root/outside/manifest.json"
      stage_response="$root/outside"
      ;;
    mismatch)
      printf '{"success":true,"tip_height":41,"tip_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' > "$stage/manifest.json"
      ;;
    symlink)
      mv "$stage" "$root/node-data/testnet/.backup-staging/checkpoint-real"
      ln -s checkpoint-real "$root/node-data/testnet/.backup-staging/checkpoint-link"
      stage_response="$root/node-data/testnet/.backup-staging/checkpoint-link"
      ;;
  esac
  printf '{"path":"%s","tip_height":42,"tip_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\n' "$stage_response" > "$root/checkpoint.json"
  # A real socket inode is required. The fake curl returns the configured fixture response.
  rm -f "$node/.aperod-backup.sock"
  python3 - "$node/.aperod-backup.sock" <<'PY' &
import socket, sys, time
s = socket.socket(socket.AF_UNIX)
s.bind(sys.argv[1])
while True:
    time.sleep(60)
PY
  SERVER_PIDS+=("$!")
  for _ in {1..50}; do [[ -S "$node/.aperod-backup.sock" ]] && break; sleep 0.02; done
}

make_command_mocks() {
  local root="$1"
  local bin="$root/bin"
  mkdir -p "$bin"
  cat > "$bin/curl" <<'STUB'
#!/usr/bin/env bash
if [[ " $* " == *" --unix-socket "* ]]; then
  [[ "${MOCK_CHECKPOINT_MODE:-ok}" != unavailable ]] || exit 7
  out=""
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == --output ]]; then out="$2"; shift 2; else shift; fi
  done
  [[ -n "$out" ]]
  cat "$MOCK_CHECKPOINT_JSON" > "$out"
  exit 0
fi
printf '%s\n' "$*" >> "$MOCK_CURL_LOG"
exit 0
STUB
  cat > "$bin/sudo" <<'STUB'
#!/usr/bin/env bash
exit "${MOCK_PGDUMP_EXIT:-0}"
STUB
  cat > "$bin/pg_restore" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$MOCK_PG_RESTORE_LOG"
exit "${MOCK_PG_RESTORE_EXIT:-0}"
STUB
  cat > "$bin/aperod-backup-verify" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_VERIFIER_LOG"
mode="$1"; root="$2"
case "$mode" in
  --stage)
    python3 - "$root" <<'PY'
import json, os, sys
root = sys.argv[1]
if not os.path.isdir(os.path.join(root, "chain.db")):
    raise SystemExit("missing new-format chain.db")
with open(os.path.join(root, "manifest.json"), encoding="utf-8") as source:
    manifest = json.load(source)
if not os.path.isfile(os.path.join(root, "explorer_db.dump")):
    raise SystemExit("missing new-format pg dump")
print(json.dumps({"success": True, "tip_hash": manifest["tip_hash"],
                  "tip_height": manifest["tip_height"]}))
PY
    ;;
  --legacy-stage)
    [[ -d "$root/testnet/chain.db" && -f "$root/explorer_db.dump" ]]
    python3 - "$root/testnet/chain.db/CURRENT" <<'PY'
import os, sys
if not os.path.isfile(sys.argv[1]):
    raise SystemExit("legacy chain database fixture is incomplete")
print('{"success":true,"legacy":true,"tip_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tip_height":41}')
PY
    ;;
  *)
    echo "unexpected verifier mode" >&2
    exit 2
    ;;
esac
STUB
  cat > "$bin/rclone" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
store="$MOCK_REMOTE_DIR"
remote_path() { printf '%s' "${1##*/}"; }
case "$1" in
  lsjson)
    python3 - "$store" <<'PY'
import json, os, sys
print(json.dumps([{"Name": n, "Size": os.path.getsize(os.path.join(sys.argv[1], n)), "IsDir": False}
                  for n in sorted(os.listdir(sys.argv[1]))
                  if os.path.isfile(os.path.join(sys.argv[1], n))]))
PY
    ;;
  copyto)
    src="$2"; dst="$3"
    if [[ "$src" == s3backup:* ]]; then
      file="$store/$(remote_path "$src")"
      [[ -f "$file" ]]
      printf '%s -> %s\n' "$src" "$dst" >> "$MOCK_DOWNLOAD_LOG"
      cp "$file" "$dst"
      if [[ "${MOCK_CORRUPT_DOWNLOAD:-0}" == 1 && "$src" == *candidate_* ]]; then printf x >> "$dst"; fi
    else
      cp "$src" "$store/$(remote_path "$dst")"
      if [[ "${MOCK_CORRUPT_LEGACY_PRESERVE:-0}" == 1 && "$dst" == *"_legacy_"* ]]; then
        printf x >> "$store/$(remote_path "$dst")"
      fi
      if [[ "${MOCK_CORRUPT_FIXED_PROMOTION:-0}" == 1 \
        && "$(basename "$src")" == "aperod_backup.tar.gpg" \
        && "$(remote_path "$dst")" == "aperod_backup.tar.gpg" ]]; then
        printf x >> "$store/$(remote_path "$dst")"
      fi
    fi
    ;;
  size)
    file="$store/$(remote_path "$2")"
    python3 - "$file" <<'PY'
import json, os, sys
print(json.dumps({"count": 1, "bytes": os.path.getsize(sys.argv[1])}))
PY
    ;;
  deletefile)
    rm -f "$store/$(remote_path "$2")"
    ;;
  delete)
    for file in "$store"/aperod_backup_*.tar.gpg; do
      [[ -f "$file" ]] || continue
      base="$(basename "$file")"
      if [[ "$base" != "aperod_backup.tar.gpg" && "$base" != "aperod_backup_previous.tar.gpg" ]]; then
        rm -f "$file"
      fi
    done
    ;;
  *)
    echo "unexpected rclone command" >&2
    exit 2
    ;;
esac
STUB
  chmod +x "$bin/curl" "$bin/sudo" "$bin/pg_restore" "$bin/aperod-backup-verify" "$bin/rclone"
  printf '%s' "$bin"
}

run_backup() {
  local root="$1" mock_mode="${2:-ok}" corrupt="${3:-0}"
  local bin
  bin=$(make_command_mocks "$root")
  APEROD_BACKUP_PASSWORD="test-only-passphrase" \
    DATA_DIR="$root/settings" \
    APEROD_NODE_DATA_DIR_OVERRIDE="$root/node-data" \
    APEROD_CHAIN_DATA_DIR_OVERRIDE="$root/node-data/testnet" \
    APEROD_TEXTFILE_DIR="$root/metrics" \
    APEROD_HISTORY_LOG="$root/history.log" \
    APEROD_BACKUP_DIR_OVERRIDE="$root/work" \
    APEROD_BACKUP_VERIFY_BIN="$bin/aperod-backup-verify" \
    MOCK_CHECKPOINT_JSON="$root/checkpoint.json" \
    MOCK_CHECKPOINT_MODE="$mock_mode" \
    MOCK_REMOTE_DIR="$root/remote" \
    MOCK_CURL_LOG="$root/curl.log" \
    MOCK_VERIFIER_LOG="$root/verifier.log" \
    MOCK_PG_RESTORE_LOG="$root/pg-restore.log" \
    MOCK_DOWNLOAD_LOG="$root/download.log" \
    MOCK_CORRUPT_DOWNLOAD="$corrupt" \
    MOCK_CORRUPT_LEGACY_PRESERVE="${MOCK_CORRUPT_LEGACY_PRESERVE_OVERRIDE:-0}" \
    MOCK_CORRUPT_FIXED_PROMOTION="${MOCK_CORRUPT_FIXED_PROMOTION_OVERRIDE:-0}" \
    MOCK_PG_RESTORE_EXIT="${MOCK_PG_RESTORE_EXIT_OVERRIDE:-0}" \
    MOCK_PGDUMP_EXIT="${MOCK_PGDUMP_EXIT_OVERRIDE:-0}" \
    PATH="$bin:$PATH" \
    bash "$BACKUP_SH" >"$root/run.log" 2>&1
}

settings() {
  mkdir -p "$1"
  cat > "$1/integration-settings.json" <<'JSON'
{"s3backup":{"endpoint":"https://s3.example.test","accessKeyId":"test-access","secretAccessKey":"test-secret","bucket":"test-bucket","region":"us-east-1"}}
JSON
}

section_root="$TMPDIR_TEST/socket-unavailable"
mkdir -p "$section_root/settings" "$section_root/metrics" "$section_root/remote" "$section_root/node-data/testnet"
settings "$section_root/settings"
printf '{}' > "$section_root/checkpoint.json"
set +e
run_backup "$section_root" unavailable
status=$?
set -e
if [[ $status -ne 0 ]] && ! compgen -G "$section_root/remote/*" >/dev/null; then
  pass "unavailable node socket fails closed before upload"
else
  fail "unavailable node socket did not fail closed"
fi

for invalid in outside mismatch; do
  root="$TMPDIR_TEST/invalid-$invalid"
  mkdir -p "$root/settings"
  settings "$root/settings"
  make_fixture "$root" "$invalid"
  set +e
  run_backup "$root"
  status=$?
  set -e
  if [[ $status -ne 0 ]] && ! compgen -G "$root/remote/*" >/dev/null; then
    pass "invalid $invalid checkpoint rejected before upload"
  else
    fail "invalid $invalid checkpoint was not rejected"
  fi
done

root="$TMPDIR_TEST/symlink-stage"
mkdir -p "$root/settings"
settings "$root/settings"
make_fixture "$root" symlink
set +e
run_backup "$root"
status=$?
set -e
if [[ $status -ne 0 ]] \
  && [[ -L "$root/node-data/testnet/.backup-staging/checkpoint-link" ]] \
  && [[ -f "$root/node-data/testnet/.backup-staging/checkpoint-real/chain.db/CURRENT" ]]; then
  pass "symlink stage is refused and its target is not followed or removed"
else
  fail "symlink stage was accepted or cleanup followed its target"
fi

if ! command -v gpg >/dev/null 2>&1; then
  root="$TMPDIR_TEST/realistic-chain-directory"
  mkdir -p "$root/settings"
  settings "$root/settings"
  make_fixture "$root"
  set +e
  run_backup "$root"
  status=$?
  set -e
  if [[ $status -ne 0 ]] && ! grep -q 'checkpoint chain.db is not a real directory' "$root/run.log" \
    && [[ ! -e "$root/node-data/testnet/.backup-staging/checkpoint-test" ]] \
    && ! compgen -G "$root/remote/*" >/dev/null; then
    pass "realistic LevelDB stage is validated then cleaned on archive failure"
  else
    fail "realistic LevelDB stage was rejected, leaked, or uploaded without verification"
  fi
fi

root="$TMPDIR_TEST/cleanup-exit-status"
mkdir -p "$root/settings"
settings "$root/settings"
make_fixture "$root"
set +e
MOCK_PGDUMP_EXIT_OVERRIDE=23 run_backup "$root"
status=$?
set -e
if [[ $status -eq 23 ]] \
  && [[ ! -e "$root/node-data/testnet/.backup-staging/checkpoint-test" ]]; then
  pass "EXIT cleanup removes only validated stage while preserving original failure status"
else
  fail "snapshot cleanup changed status or left validated stage (exit=$status)"
fi

if command -v gpg >/dev/null 2>&1; then
  make_legacy_fixed() {
    local root="$1"
    mkdir -p "$root/legacy-source/testnet/chain.db"
    printf 'legacy CURRENT\n' > "$root/legacy-source/testnet/chain.db/CURRENT"
    printf 'legacy LOCK\n' > "$root/legacy-source/testnet/chain.db/LOCK"
    printf 'legacy MANIFEST\n' > "$root/legacy-source/testnet/chain.db/MANIFEST-000001"
    printf 'legacy table\n' > "$root/legacy-source/testnet/chain.db/000001.ldb"
    printf 'legacy explorer dump fixture\n' > "$root/legacy-source/explorer_db.dump"
    tar -czf "$root/legacy.tar.gz" -C "$root/legacy-source" testnet explorer_db.dump
    gpg --batch --yes --passphrase 'test-only-passphrase' --symmetric --cipher-algo AES256 \
      -o "$root/remote/aperod_backup.tar.gpg" "$root/legacy.tar.gz" >/dev/null 2>&1
    cp "$root/remote/aperod_backup.tar.gpg" "$root/original-legacy.tar.gpg"
  }

  root="$TMPDIR_TEST/legacy-migration"
  mkdir -p "$root/settings"
  settings "$root/settings"
  make_fixture "$root"
  make_legacy_fixed "$root"
  LEGACY_SHA=$(sha256sum "$root/original-legacy.tar.gpg" | awk '{print $1}')
  set +e
  run_backup "$root"
  status=$?
  set -e
  PRESERVED_LEGACY=$(find "$root/remote" -maxdepth 1 -type f -name 'aperod_backup_legacy_*.tar.gpg' -print -quit)
  FIRST_NEW_SHA=$(sha256sum "$root/remote/aperod_backup.tar.gpg" | awk '{print $1}')
  OLD_FIXED_DOWNLOADS=$(grep -Fc 's3backup:test-bucket/aperod_backup.tar.gpg ->' "$root/download.log" || true)
  PRESERVED_COPY_DOWNLOADS=$(grep -Ec 's3backup:test-bucket/aperod_backup_legacy_.* ->' "$root/download.log" || true)
  CANDIDATE_DOWNLOADS=$(grep -Ec 's3backup:test-bucket/aperod_backup_candidate_.* ->' "$root/download.log" || true)
  if [[ $status -eq 0 ]] && [[ -n "$PRESERVED_LEGACY" ]] \
    && [[ "$(sha256sum "$PRESERVED_LEGACY" | awk '{print $1}')" == "$LEGACY_SHA" ]] \
    && [[ "$FIRST_NEW_SHA" != "$LEGACY_SHA" ]] \
    && [[ ! -e "$root/remote/aperod_backup_previous.tar.gpg" ]] \
    && compgen -G "$root/remote/aperod_backup_candidate_*.tar.gpg" >/dev/null \
    && [[ "$OLD_FIXED_DOWNLOADS" -eq 2 ]] \
    && [[ "$PRESERVED_COPY_DOWNLOADS" -eq 1 ]] \
    && [[ "$CANDIDATE_DOWNLOADS" -eq 1 ]] \
    && grep -q -- '--legacy-stage' "$root/verifier.log" \
    && grep -q 'aperod_backup_legacy_migration 1' "$root/metrics/aperod_backup.prom" \
    && grep -q '"degraded":true' "$root/history.log"; then
    pass "first non-B2 migration preserves exact legacy ciphertext and candidate while minimizing downloads"
  else
    fail "first legacy migration overwrote, misclassified, or failed to retain its legacy generation (exit=$status)"
    echo "--- first migration diagnostics ---" >&2
    cat "$root/run.log" >&2
    cat "$root/verifier.log" >&2 2>/dev/null || true
    find "$root/remote" -maxdepth 1 -type f -printf '%f %s bytes\n' >&2
    cat "$root/history.log" "$root/metrics/aperod_backup.prom" >&2 2>/dev/null || true
  fi

  make_fixture "$root"
  set +e
  run_backup "$root"
  status=$?
  set -e
  SECOND_FIXED_SHA=$(sha256sum "$root/remote/aperod_backup.tar.gpg" | awk '{print $1}')
  PREVIOUS_NEW_SHA=$(sha256sum "$root/remote/aperod_backup_previous.tar.gpg" 2>/dev/null | awk '{print $1}')
  if [[ $status -eq 0 ]] && [[ "$PREVIOUS_NEW_SHA" == "$FIRST_NEW_SHA" ]] \
    && [[ "$SECOND_FIXED_SHA" != "$FIRST_NEW_SHA" ]] \
    && ! compgen -G "$root/remote/aperod_backup_legacy_*.tar.gpg" >/dev/null \
    && ! compgen -G "$root/remote/aperod_backup_candidate_*.tar.gpg" >/dev/null \
    && grep -q -- '--stage' "$root/verifier.log"; then
    pass "second new-format rotation verifies fixed and previous before pruning the preserved legacy copy"
  else
    fail "second rotation pruned legacy too early or failed to establish two verified new-format generations (exit=$status)"
    echo "--- second rotation diagnostics ---" >&2
    cat "$root/run.log" >&2
    cat "$root/verifier.log" >&2 2>/dev/null || true
    find "$root/remote" -maxdepth 1 -type f -printf '%f %s bytes\n' >&2
    cat "$root/history.log" "$root/metrics/aperod_backup.prom" >&2 2>/dev/null || true
  fi

  root="$TMPDIR_TEST/legacy-preserve-failure"
  mkdir -p "$root/settings"
  settings "$root/settings"
  make_fixture "$root"
  make_legacy_fixed "$root"
  set +e
  MOCK_CORRUPT_LEGACY_PRESERVE_OVERRIDE=1 run_backup "$root"
  status=$?
  set -e
  if [[ $status -ne 0 ]] \
    && cmp -s "$root/remote/aperod_backup.tar.gpg" "$root/original-legacy.tar.gpg" \
    && ! compgen -G "$root/remote/aperod_backup_candidate_*.tar.gpg" >/dev/null; then
    pass "failed byte-verification of the durable legacy copy preserves old fixed and prevents candidate upload"
  else
    fail "legacy preservation failure changed fixed or proceeded with candidate promotion (exit=$status)"
    echo "--- legacy preservation diagnostics ---" >&2
    cat "$root/run.log" >&2
    cat "$root/verifier.log" >&2 2>/dev/null || true
    find "$root/remote" -maxdepth 1 -type f -printf '%f %s bytes\n' >&2
  fi

  root="$TMPDIR_TEST/legacy-promotion-failure"
  mkdir -p "$root/settings"
  settings "$root/settings"
  make_fixture "$root"
  make_legacy_fixed "$root"
  set +e
  MOCK_CORRUPT_FIXED_PROMOTION_OVERRIDE=1 run_backup "$root"
  status=$?
  set -e
  if [[ $status -ne 0 ]] \
    && cmp -s "$root/remote/aperod_backup.tar.gpg" "$root/original-legacy.tar.gpg" \
    && compgen -G "$root/remote/aperod_backup_candidate_*.tar.gpg" >/dev/null \
    && compgen -G "$root/remote/aperod_backup_legacy_*.tar.gpg" >/dev/null; then
    pass "failed fixed promotion restores old legacy ciphertext and retains candidate plus durable legacy copy"
  else
    fail "failed legacy migration promotion did not restore and retain recovery generations (exit=$status)"
    echo "--- promotion rollback diagnostics ---" >&2
    cat "$root/run.log" >&2
    cat "$root/verifier.log" >&2 2>/dev/null || true
    find "$root/remote" -maxdepth 1 -type f -printf '%f %s bytes\n' >&2
  fi
else
  echo "SKIP: root legacy-rotation scenarios require gpg; mocked verifier modes and static retention tests remain enabled."
fi

if [[ $FAIL -eq 0 ]]; then
  echo "All $PASS targeted backup tests passed."
  exit 0
fi
echo "$FAIL failure(s), $PASS pass(es)." >&2
exit 1