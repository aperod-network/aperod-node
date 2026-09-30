#!/usr/bin/env bash
# Safely refresh source in a dedicated sparse checkout, never the live node
# checkout at /opt/aperod. The live checkout is consulted only for its origin
# URL when a new staging clone is needed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/source-safe-guard.sh"

STAGING_DIR="${APEROD_SOURCE_STAGING_DIR:-/opt/aperod-source-staging}"
BRANCH="${APEROD_SOURCE_STAGING_BRANCH:-main}"
LIVE_REPO="${APEROD_LIVE_SOURCE_REPO:-/opt/aperod}"
TEST_MODE="${APEROD_SOURCE_STAGING_TEST_MODE:-0}"
SPARSE_PATTERNS=(
  /artifacts/api-server/
  /blockchain/
  /deploy/
  /lib/api-zod/
  /lib/db/
  '!/data/testnet/'
  '!/blockchain/data/testnet/'
)
LEGACY_CONE_PATHS=(artifacts/api-server blockchain deploy lib/api-zod lib/db)

if [[ "$TEST_MODE" != "1" && "$STAGING_DIR" != "/opt/aperod-source-staging" ]]; then
  printf '[source-staging] REFUSED: production staging path must be /opt/aperod-source-staging\n' >&2
  exit 1
fi
if [[ "$STAGING_DIR" == "/opt/aperod" || "$STAGING_DIR" == "/opt/aperod/"* ]]; then
  printf '[source-staging] REFUSED: staging must be separate from the live checkout\n' >&2
  exit 1
fi
if [[ "$STAGING_DIR" == "/" || "$STAGING_DIR" == "/opt" ]]; then
  printf '[source-staging] REFUSED: unsafe staging directory\n' >&2
  exit 1
fi

source_stage_preflight() {
  local stage="$1" status_output
  [[ ! -L "$stage" ]] || {
    printf '[source-staging] REFUSED: staging directory is a symlink: %s\n' "$stage" >&2
    return 1
  }
  [[ -d "$stage/.git" || -f "$stage/.git" ]] || {
    printf '[source-staging] REFUSED: not a Git checkout: %s\n' "$stage" >&2
    return 1
  }
  [[ "$(realpath -e -- "$stage")" != "$(realpath -e -- "$LIVE_REPO" 2>/dev/null || printf '%s' "$LIVE_REPO")" ]] || {
    printf '[source-staging] REFUSED: staging resolves to the live checkout\n' >&2
    return 1
  }
  [[ "$(git -C "$stage" config --bool core.sparseCheckout 2>/dev/null || printf false)" == true ]] || {
    printf '[source-staging] REFUSED: sparse checkout is not enabled\n' >&2
    return 1
  }
  status_output=$(git -C "$stage" status --porcelain --untracked-files=all) || {
    printf '[source-staging] REFUSED: cannot inspect Git status\n' >&2
    return 1
  }
  [[ -z "$status_output" ]] || {
    printf '[source-staging] REFUSED: staging checkout is dirty\n' >&2
    return 1
  }
}

source_stage_patterns_match() {
  local stage="$1" expected="$2" pattern
  local -a actual=()
  while IFS= read -r pattern; do
    [[ -n "$pattern" ]] && actual+=("$pattern")
  done < <(git -C "$stage" sparse-checkout list)
  [[ "${actual[*]}" == "$expected" ]]
}

source_stage_validate_runtime_fixtures() {
  local stage="$1" root="$2" path current_seen=0 manifest_seen=0 table_seen=0
  local current="${root}/chain.db/CURRENT"
  local tracked_paths
  tracked_paths=$(git -C "$stage" ls-files -- "$root") || {
    printf '[source-staging] REFUSED: cannot inspect runtime fixture index: %s\n' "$root" >&2
    return 1
  }
  [[ -n "$tracked_paths" ]] || {
    printf '[source-staging] REFUSED: expected tracked runtime fixtures are missing: %s\n' "$root" >&2
    return 1
  }
  while IFS= read -r path; do
    [[ "$path" == "$root/chain.db/CURRENT" ||
       "$path" == "$root/chain.db/CURRENT.bak" ||
       "$path" == "$root/chain.db/LOCK" ||
       "$path" == "$root/chain.db/LOG" ||
       "$path" == "$root/chain.db/LOG.old" ||
       "$path" == "$root/chain.db"/MANIFEST-[0-9]* ||
       "$path" == "$root/chain.db"/[0-9]*.ldb ||
       "$path" == "$root/chain.db"/[0-9]*.sst ||
       "$path" == "$root/validator.key" ||
       "$path" == "$root/p2p_identity.key" ]] || {
      printf '[source-staging] REFUSED: unexpected tracked runtime fixture: %s\n' "$path" >&2
      return 1
    }
    [[ -f "$stage/$path" ]] || {
      printf '[source-staging] REFUSED: tracked runtime fixture is not materialized: %s\n' "$path" >&2
      return 1
    }
    [[ "$path" == "$current" ]] && current_seen=1
    [[ "$path" == "$root/chain.db"/MANIFEST-[0-9]* ]] && manifest_seen=1
    [[ "$path" == "$root/chain.db"/[0-9]*.ldb ]] && table_seen=1
  done <<< "$tracked_paths"
  [[ "$current_seen" == "1" && "$manifest_seen" == "1" && "$table_seen" == "1" ]] || {
    printf '[source-staging] REFUSED: runtime fixture set lacks CURRENT, MANIFEST and LevelDB table: %s\n' "$root" >&2
    return 1
  }
}

source_stage_migrate_legacy_cone() {
  local stage="$1" runtime_status
  local legacy_paths="${LEGACY_CONE_PATHS[*]}"
  source_stage_preflight "$stage" || return 1
  source_stage_patterns_match "$stage" "$legacy_paths" || {
    printf '[source-staging] REFUSED: staging sparse patterns are neither the reviewed source patterns nor the supported legacy cone\n' >&2
    return 1
  }

  # A legacy cone checkout materializes blockchain/data. Confirm it is clean
  # and that the only runtime files to be removed are the expected tracked
  # LevelDB/key fixtures before changing sparse patterns.
  runtime_status=$(git -C "$stage" status --porcelain --ignored=matching --untracked-files=all -- \
    data/testnet blockchain/data/testnet) || {
    printf '[source-staging] REFUSED: cannot inspect materialized runtime fixtures\n' >&2
    return 1
  }
  [[ -z "$runtime_status" ]] || {
    printf '[source-staging] REFUSED: runtime fixture paths contain ignored or untracked files\n' >&2
    return 1
  }
  source_stage_validate_runtime_fixtures "$stage" blockchain/data/testnet || return 1
  if [[ -e "$stage/data/testnet" || -L "$stage/data/testnet" ]]; then
    source_stage_validate_runtime_fixtures "$stage" data/testnet || return 1
  fi

  printf '%s\n' "${SPARSE_PATTERNS[@]}" |
    git -C "$stage" sparse-checkout set --no-cone --stdin
}

source_stage_guard() {
  local stage="$1" entry tag path tracked_entries
  local expected_patterns="${SPARSE_PATTERNS[*]}"
  source_stage_preflight "$stage" || return 1
  source_stage_patterns_match "$stage" "$expected_patterns" || {
    printf '[source-staging] REFUSED: sparse patterns differ from the reviewed source allowlist\n' >&2
    return 1
  }
  for path in data/testnet blockchain/data/testnet chain.db snapshots; do
    if [[ -L "$stage/$path" || -e "$stage/$path" ]]; then
      printf '[source-staging] REFUSED: runtime path is materialized in staging: %s\n' "$path" >&2
      return 1
    fi
  done
  tracked_entries=$(git -C "$stage" ls-files -v) || {
    printf '[source-staging] REFUSED: cannot enumerate tracked files\n' >&2
    return 1
  }
  while IFS= read -r entry; do
    tag="${entry:0:1}"
    path="${entry:2}"
    # These two tracked website assets live under blockchain/data but are not
    # chain state. The broad live-checkout guard intentionally treats the
    # whole data tree as unsafe; the sparse source stage can retain only these
    # explicitly reviewed assets.
    case "$path" in
      blockchain/data/favicon.png|blockchain/data/site-settings.json) continue ;;
    esac
    if _source_guard_is_runtime_path "$path" && [[ "$tag" != "S" ]]; then
      printf '[source-staging] REFUSED: tracked runtime path is not sparse-excluded: %s\n' "$path" >&2
      return 1
    fi
  done <<< "$tracked_entries"
}

if [[ ! -e "$STAGING_DIR" ]]; then
  REMOTE="${APEROD_SOURCE_STAGING_REMOTE:-}"
  if [[ -z "$REMOTE" && -d "$LIVE_REPO/.git" ]]; then
    REMOTE=$(git -C "$LIVE_REPO" config --get remote.origin.url || true)
  fi
  [[ -n "$REMOTE" ]] || {
    printf '[source-staging] REFUSED: no source remote available\n' >&2
    exit 1
  }
  STAGING_PARENT=$(dirname -- "$STAGING_DIR")
  mkdir -p "$STAGING_PARENT"
  TEMP_PARENT=$(mktemp -d "${STAGING_PARENT%/}/.aperod-source-staging.XXXXXX")
  trap 'rm -rf -- "$TEMP_PARENT"' EXIT
  git clone --filter=blob:none --no-checkout --branch "$BRANCH" "$REMOTE" "$TEMP_PARENT/repo"
  git -C "$TEMP_PARENT/repo" sparse-checkout init --no-cone
  printf '%s\n' "${SPARSE_PATTERNS[@]}" |
    git -C "$TEMP_PARENT/repo" sparse-checkout set --no-cone --stdin
  # A --no-checkout clone has an empty index. Populate only the sparse paths.
  git -C "$TEMP_PARENT/repo" read-tree -mu HEAD
  source_stage_guard "$TEMP_PARENT/repo"
  mv -- "$TEMP_PARENT/repo" "$STAGING_DIR"
  rmdir -- "$TEMP_PARENT"
  trap - EXIT
else
  source_stage_preflight "$STAGING_DIR"
  if ! source_stage_patterns_match "$STAGING_DIR" "${SPARSE_PATTERNS[*]}"; then
    source_stage_migrate_legacy_cone "$STAGING_DIR"
  fi
  source_stage_guard "$STAGING_DIR"
fi

STAGING_REAL=$(realpath -e -- "$STAGING_DIR")
git -C "$STAGING_REAL" fetch --prune origin "$BRANCH"
REMOTE_REF="refs/remotes/origin/${BRANCH}"
git -C "$STAGING_REAL" show-ref --verify --quiet "$REMOTE_REF" || {
  printf '[source-staging] REFUSED: origin branch is missing: %s\n' "$BRANCH" >&2
  exit 1
}
git -C "$STAGING_REAL" merge-base --is-ancestor HEAD "$REMOTE_REF" || {
  printf '[source-staging] REFUSED: staging branch is not a fast-forward of origin/%s\n' "$BRANCH" >&2
  exit 1
}
git -C "$STAGING_REAL" merge --ff-only "$REMOTE_REF"
source_stage_guard "$STAGING_REAL"
printf '[source-staging] Updated clean sparse source checkout: %s (%s)\n' \
  "$STAGING_REAL" "$(git -C "$STAGING_REAL" rev-parse --short HEAD)"