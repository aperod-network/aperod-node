#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/source-safe-guard.sh"
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
PASS=0
FAIL=0

pass() { printf 'PASS %s\n' "$*"; PASS=$((PASS + 1)); }
fail() { printf 'FAIL %s\n' "$*" >&2; FAIL=$((FAIL + 1)); }
assert_rejected() {
  local name="$1"; shift
  if "$@"; then fail "$name (unexpectedly accepted)"; else pass "$name"; fi
}

make_repo() {
  local path="$1"
  mkdir -p "$path"
  git -C "$path" init -q
  git -C "$path" config user.email test@example.invalid
  git -C "$path" config user.name test
  mkdir -p "$path/src"
  printf 'source\n' > "$path/main.go"
  printf 'source\n' > "$path/src/main.go"
  git -C "$path" add main.go src/main.go
  git -C "$path" commit -qm initial
}

section_guard() { printf '\n== %s ==\n' "$*"; }

section_guard "destructive entrypoints call the shared guard before mutation"
for pair in \
  "install-node.sh:node_source_pin_guard" \
  "uninstall-validator.sh:source_checkout_guard \"\$INSTALL_DIR\""; do
  script="${pair%%:*}"
  expected="${pair#*:}"
  if grep -Fq "$expected" "$SCRIPT_DIR/$script"; then
    pass "$script invokes the shared source guard"
  else
    fail "$script invokes the shared source guard"
  fi
done
INSTALL_GUARD_LINE=$(grep -nF 'node_source_pin_guard' "$SCRIPT_DIR/install-node.sh" | head -1 | cut -d: -f1)
INSTALL_PULL_LINE=$(grep -nF 'node_source_prepare' "$SCRIPT_DIR/install-node.sh" | head -1 | cut -d: -f1)
if [[ -n "$INSTALL_GUARD_LINE" && -n "$INSTALL_PULL_LINE" ]] &&
   (( INSTALL_GUARD_LINE < INSTALL_PULL_LINE )); then
  pass "install-node pin guard runs before isolated source acquisition"
else
  fail "install-node pin guard runs before isolated source acquisition"
fi
UNINSTALL_GUARD_LINE=$(grep -nF 'source_checkout_guard "$INSTALL_DIR"' "$SCRIPT_DIR/uninstall-validator.sh" | head -1 | cut -d: -f1)
UNINSTALL_DATA_GUARD_LINE=$(grep -nF 'uninstall_runtime_data_guard || exit 1' "$SCRIPT_DIR/uninstall-validator.sh" | head -1 | cut -d: -f1)
UNINSTALL_CONFIRM_LINE=$(grep -nF 'read -rp "Введите YES для подтверждения:' "$SCRIPT_DIR/uninstall-validator.sh" | head -1 | cut -d: -f1)
UNINSTALL_STOP_LINE=$(grep -nF 'systemctl stop aperod-node' "$SCRIPT_DIR/uninstall-validator.sh" | head -1 | cut -d: -f1)
UNINSTALL_REMOVE_LINE=$(grep -nF 'rm -rf "${INSTALL_DIR}"' "$SCRIPT_DIR/uninstall-validator.sh" | head -1 | cut -d: -f1)
if [[ -n "$UNINSTALL_GUARD_LINE" && -n "$UNINSTALL_DATA_GUARD_LINE" &&
      -n "$UNINSTALL_CONFIRM_LINE" && -n "$UNINSTALL_STOP_LINE" &&
      -n "$UNINSTALL_REMOVE_LINE" ]] &&
   (( UNINSTALL_GUARD_LINE < UNINSTALL_DATA_GUARD_LINE &&
      UNINSTALL_DATA_GUARD_LINE < UNINSTALL_CONFIRM_LINE &&
      UNINSTALL_CONFIRM_LINE < UNINSTALL_STOP_LINE &&
      UNINSTALL_CONFIRM_LINE < UNINSTALL_REMOVE_LINE )); then
  pass "uninstall data guard precedes confirmation/stop and existing confirmation remains before rm"
else
  fail "uninstall data guard precedes confirmation/stop and existing confirmation remains before rm"
fi

section_guard "tracked runtime data is rejected before a source replacement"
TRACKED="$TMP/tracked"
make_repo "$TRACKED"
mkdir -p "$TRACKED/data/testnet"
printf 'do not delete\n' > "$TRACKED/data/testnet/chain.db"
git -C "$TRACKED" add data/testnet/chain.db
git -C "$TRACKED" commit -qm 'track runtime data'
assert_rejected "tracked LevelDB path blocks checkout update" source_checkout_guard "$TRACKED"
[[ "$(cat "$TRACKED/data/testnet/chain.db")" == "do not delete" ]] &&
  pass "guard rejection leaves runtime data unchanged" ||
  fail "guard rejection leaves runtime data unchanged"
git -C "$TRACKED" sparse-checkout init --cone
git -C "$TRACKED" sparse-checkout set --cone src
[[ ! -e "$TRACKED/data" ]] &&
  pass "tracked data fixture is excluded from the working tree" ||
  fail "tracked data fixture is excluded from the working tree"
assert_rejected "Git index still tracking excluded chain data blocks update" \
  source_checkout_guard "$TRACKED"

section_guard "dirty source tree is rejected"
DIRTY="$TMP/dirty"
make_repo "$DIRTY"
printf 'modified\n' >> "$DIRTY/main.go"
assert_rejected "modified tracked source blocks updater" source_checkout_guard "$DIRTY"
git -C "$DIRTY" checkout -- main.go
printf 'staged\n' >> "$DIRTY/main.go"
git -C "$DIRTY" add main.go
assert_rejected "staged source blocks updater" source_checkout_guard "$DIRTY"
git -C "$DIRTY" checkout -- main.go
printf 'untracked\n' > "$DIRTY/new-file"
assert_rejected "untracked source files block destructive replacement" source_checkout_guard "$DIRTY"

section_guard "symlinked runtime data is rejected"
SYMLINK="$TMP/symlink"
OUTSIDE="$TMP/outside-data"
make_repo "$SYMLINK"
mkdir -p "$OUTSIDE"
ln -s "$OUTSIDE" "$SYMLINK/data"
assert_rejected "checkout data symlink blocks updater" source_checkout_guard "$SYMLINK"
rm "$SYMLINK/data"
EXTERNAL_LINK="$TMP/external-link"
ln -s "$OUTSIDE" "$EXTERNAL_LINK"
assert_rejected "configured external path may not be symlinked" \
  source_checkout_guard "$SYMLINK" "$EXTERNAL_LINK"

section_guard "clean source with data outside remains usable"
CLEAN="$TMP/clean"
EXTERNAL="$TMP/external-chain"
make_repo "$CLEAN"
mkdir -p "$EXTERNAL"
printf 'safe\n' > "$EXTERNAL/chain.db"
if source_checkout_guard "$CLEAN" "$EXTERNAL"; then
  pass "external runtime data permits safe source checkout"
else
  fail "external runtime data permits safe source checkout"
fi
FRESH="$TMP/fresh"
mkdir -p "$FRESH"
if source_checkout_guard "$FRESH" "$EXTERNAL"; then
  pass "empty fresh install path with external data is accepted"
else
  fail "empty fresh install path with external data is accepted"
fi
CONFIG="$TMP/node.yaml"
printf 'data_dir: %s\n' "$EXTERNAL" > "$CONFIG"
if source_checkout_guard "$CLEAN" "" "$CONFIG"; then
  pass "node config with external data directory is accepted"
else
  fail "node config with external data directory is accepted"
fi
printf 'data_dir: %s/data/testnet\n' "$CLEAN" > "$CONFIG"
assert_rejected "configured future data path inside checkout is rejected" \
  source_checkout_guard "$CLEAN" "" "$CONFIG"
printf 'data_dir: %s\n  data_dir: %s\n' "$EXTERNAL" "$EXTERNAL" > "$CONFIG"
assert_rejected "ambiguous duplicate data_dir entries are rejected" \
  source_checkout_guard "$CLEAN" "" "$CONFIG"

if [[ -f "$SCRIPT_DIR/update-source-staging.sh" ]]; then
section_guard "source-only sparse staging excludes both chain-data trees and fast-forwards"
ORIGIN="$TMP/origin"
git init -q --bare "$ORIGIN"
SEED="$TMP/seed"
make_repo "$SEED"
mkdir -p "$SEED/blockchain" "$SEED/deploy" "$SEED/artifacts/api-server" \
  "$SEED/lib/db" "$SEED/lib/api-zod" \
  "$SEED/data/testnet/chain.db" "$SEED/blockchain/data/testnet/chain.db"
printf 'node source\n' > "$SEED/blockchain/main.go"
printf 'website icon\n' > "$SEED/blockchain/data/favicon.png"
printf '{}\n' > "$SEED/blockchain/data/site-settings.json"
printf 'deploy source\n' > "$SEED/deploy/update.sh"
printf 'api source\n' > "$SEED/artifacts/api-server/index.ts"
printf 'database source\n' > "$SEED/lib/db/schema.ts"
printf 'zod source\n' > "$SEED/lib/api-zod/index.ts"
for runtime_root in "$SEED/data/testnet" "$SEED/blockchain/data/testnet"; do
  printf 'leveldb current\n' > "$runtime_root/chain.db/CURRENT"
  printf 'leveldb manifest\n' > "$runtime_root/chain.db/MANIFEST-000001"
  printf 'leveldb table\n' > "$runtime_root/chain.db/000001.ldb"
done
printf 'validator key fixture\n' > "$SEED/blockchain/data/testnet/validator.key"
git -C "$SEED" add blockchain deploy artifacts lib data
git -C "$SEED" branch -M main
git -C "$SEED" commit -qm 'add sparse source and runtime fixtures'
git -C "$SEED" remote add origin "$ORIGIN"
git -C "$SEED" push -q -u origin main
STAGING="$TMP/staging"
APEROD_SOURCE_STAGING_DIR="$STAGING" \
APEROD_SOURCE_STAGING_REMOTE="$ORIGIN" \
APEROD_SOURCE_STAGING_TEST_MODE=1 \
  bash "${SCRIPT_DIR}/update-source-staging.sh" > "$TMP/staging-first.log"
[[ -f "$STAGING/blockchain/main.go" && -f "$STAGING/deploy/update.sh" ]] &&
  pass "allowlisted source paths are present" || fail "allowlisted source paths are present"
[[ ! -e "$STAGING/data/testnet" && ! -e "$STAGING/blockchain/data/testnet" ]] &&
  pass "new no-cone clone excludes root and blockchain chain data" ||
  fail "new no-cone clone excludes root and blockchain chain data"
[[ "$(git -C "$STAGING" sparse-checkout list | tr '\n' ' ')" == \
   "/artifacts/api-server/ /blockchain/ /deploy/ /lib/api-zod/ /lib/db/ !/data/testnet/ !/blockchain/data/testnet/ " ]] &&
  pass "new clone has reviewed no-cone include/exclude patterns" ||
  fail "new clone has reviewed no-cone include/exclude patterns"
while IFS= read -r tracked_entry; do
  tracked_tag="${tracked_entry:0:1}"
  tracked_path="${tracked_entry:2}"
  case "$tracked_path" in
    data/testnet/*|blockchain/data/testnet/*)
      [[ "$tracked_tag" == "S" ]] ||
        fail "tracked runtime fixture is sparse-excluded: $tracked_path"
      ;;
  esac
done < <(git -C "$STAGING" ls-files -v)
pass "tracked fixtures in both data trees have sparse-excluded index flags"
UNSKIPPED_FIXTURE="blockchain/data/testnet/chain.db/CURRENT"
git -C "$STAGING" update-index --no-skip-worktree -- "$UNSKIPPED_FIXTURE"
git -C "$STAGING" checkout-index -f -- "$UNSKIPPED_FIXTURE"
[[ "$(git -C "$STAGING" ls-files -v -- "$UNSKIPPED_FIXTURE" | cut -c1)" == "H" &&
   -f "$STAGING/$UNSKIPPED_FIXTURE" &&
   -z "$(git -C "$STAGING" status --porcelain)" ]] &&
  pass "test fixture is tracked but deliberately not sparse-excluded" ||
  fail "test fixture is tracked but deliberately not sparse-excluded"
assert_rejected "materialized tracked runtime fixture blocks source update" env \
  APEROD_SOURCE_STAGING_DIR="$STAGING" \
  APEROD_SOURCE_STAGING_REMOTE="$ORIGIN" \
  APEROD_SOURCE_STAGING_TEST_MODE=1 \
  bash "${SCRIPT_DIR}/update-source-staging.sh"
[[ -f "$STAGING/$UNSKIPPED_FIXTURE" ]] &&
  pass "refused materialized chain.db fixture is preserved" ||
  fail "refused materialized chain.db fixture is preserved"
rm -rf "$STAGING/blockchain/data/testnet"
git -C "$STAGING" update-index --skip-worktree -- "$UNSKIPPED_FIXTURE"

DIRTY_CONE="$TMP/dirty-cone"
git clone -q --no-checkout --branch main "$ORIGIN" "$DIRTY_CONE"
git -C "$DIRTY_CONE" sparse-checkout init --cone
git -C "$DIRTY_CONE" sparse-checkout set --cone artifacts/api-server blockchain deploy lib/api-zod lib/db
git -C "$DIRTY_CONE" read-tree -mu HEAD
printf 'local change\n' >> "$DIRTY_CONE/blockchain/main.go"
assert_rejected "dirty legacy cone stage refuses conversion" env \
  APEROD_SOURCE_STAGING_DIR="$DIRTY_CONE" \
  APEROD_SOURCE_STAGING_REMOTE="$ORIGIN" \
  APEROD_SOURCE_STAGING_TEST_MODE=1 \
  bash "${SCRIPT_DIR}/update-source-staging.sh"
[[ -f "$DIRTY_CONE/blockchain/data/testnet/chain.db/CURRENT" &&
   "$(git -C "$DIRTY_CONE" sparse-checkout list | tr '\n' ' ')" == \
   "artifacts/api-server blockchain deploy lib/api-zod lib/db " ]] &&
  pass "dirty legacy stage retains materialized fixtures and cone patterns" ||
  fail "dirty legacy stage retains materialized fixtures and cone patterns"

LEGACY_CONE="$TMP/legacy-cone"
git clone -q --no-checkout --branch main "$ORIGIN" "$LEGACY_CONE"
git -C "$LEGACY_CONE" sparse-checkout init --cone
git -C "$LEGACY_CONE" sparse-checkout set --cone artifacts/api-server blockchain deploy lib/api-zod lib/db
git -C "$LEGACY_CONE" read-tree -mu HEAD
[[ -f "$LEGACY_CONE/blockchain/data/testnet/chain.db/CURRENT" &&
   ! -e "$LEGACY_CONE/data/testnet" ]] &&
  pass "legacy cone stage begins with its known tracked fixtures materialized" ||
  fail "legacy cone stage begins with its known tracked fixtures materialized"
APEROD_SOURCE_STAGING_DIR="$LEGACY_CONE" \
APEROD_SOURCE_STAGING_REMOTE="$ORIGIN" \
APEROD_SOURCE_STAGING_TEST_MODE=1 \
  bash "${SCRIPT_DIR}/update-source-staging.sh" > "$TMP/staging-migrate.log"
[[ ! -e "$LEGACY_CONE/data/testnet" &&
   ! -e "$LEGACY_CONE/blockchain/data/testnet" &&
   -z "$(git -C "$LEGACY_CONE" status --porcelain --untracked-files=all)" ]] &&
  pass "clean legacy cone stage migrates without leaving either chain-data tree" ||
  fail "clean legacy cone stage migrates without leaving either chain-data tree"
[[ "$(git -C "$LEGACY_CONE" sparse-checkout list | tr '\n' ' ')" == \
   "/artifacts/api-server/ /blockchain/ /deploy/ /lib/api-zod/ /lib/db/ !/data/testnet/ !/blockchain/data/testnet/ " ]] &&
  pass "legacy stage adopts the reviewed no-cone patterns" ||
  fail "legacy stage adopts the reviewed no-cone patterns"
printf 'keep this file\n' > "$STAGING/untracked"
assert_rejected "dirty sparse staging refuses update" env \
  APEROD_SOURCE_STAGING_DIR="$STAGING" \
  APEROD_SOURCE_STAGING_REMOTE="$ORIGIN" \
  APEROD_SOURCE_STAGING_TEST_MODE=1 \
  bash "${SCRIPT_DIR}/update-source-staging.sh"
[[ "$(cat "$STAGING/untracked")" == "keep this file" ]] &&
  pass "dirty staging file is preserved" || fail "dirty staging file is preserved"
rm "$STAGING/untracked"
BEFORE=$(git -C "$STAGING" rev-parse HEAD)
printf 'second version\n' >> "$SEED/deploy/update.sh"
git -C "$SEED" add deploy/update.sh
git -C "$SEED" commit -qm update
git -C "$SEED" push -q origin main
APEROD_SOURCE_STAGING_DIR="$STAGING" \
APEROD_SOURCE_STAGING_REMOTE="$ORIGIN" \
APEROD_SOURCE_STAGING_TEST_MODE=1 \
  bash "${SCRIPT_DIR}/update-source-staging.sh" > "$TMP/staging-second.log"
[[ "$(git -C "$STAGING" rev-parse HEAD)" != "$BEFORE" ]] &&
  pass "clean source staging fast-forwards to origin" || fail "clean source staging fast-forwards to origin"
[[ ! -e "$STAGING/data/testnet" && ! -e "$STAGING/blockchain/data/testnet" &&
   -z "$(git -C "$STAGING" status --porcelain)" ]] &&
  pass "updated staging remains clean and excludes both chain-data trees" ||
  fail "updated staging remains clean and excludes both chain-data trees"
elif [[ -f "$SCRIPT_DIR/../../blockchain/go.mod" ]]; then
  fail "workspace source-staging helper is required"
else
  section_guard "private source staging is not part of the standalone node distribution"
  printf 'NOT_APPLICABLE source-staging-only fixtures; all shared source/data guards ran\n'
fi

printf '\nResult: %d passed, %d failed\n' "$PASS" "$FAIL"
(( FAIL == 0 ))