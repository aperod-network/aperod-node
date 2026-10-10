#!/usr/bin/env bash
# Shared fail-closed preflight for commands which may replace or update source.
# This deliberately refuses ambiguous checkout/data layouts rather than trying
# to infer whether a file below the source root is disposable.

_source_guard_die() {
  printf '[source-safety] REFUSED: %s\n' "$*" >&2
  return 1
}

_source_guard_is_runtime_path() {
  case "$1" in
    data|data/*|blockchain/data|blockchain/data/*|*/data/testnet|*/data/testnet/*|\
    chain.db|chain.db/*|*/chain.db|*/chain.db/*|snapshots|snapshots/*|\
    */snapshots|*/snapshots/*|*.db|*.sqlite|*.sqlite3|*.ldb|*.sst|\
    */CURRENT|*/MANIFEST-[0-9]*|\
    *.snapshot|*.snap|*.key|*.keyfile)
      return 0
      ;;
  esac
  return 1
}

_source_guard_has_symlink_component() {
  local path="$1" part current="/"
  [[ "$path" == /* ]] || return 1
  path="${path#/}"
  IFS='/' read -r -a _source_guard_parts <<< "$path"
  for part in "${_source_guard_parts[@]}"; do
    [[ -n "$part" ]] || continue
    current="${current%/}/$part"
    [[ ! -L "$current" ]] || return 0
  done
  return 1
}

_source_guard_check_external_path() {
  local checkout_real="$1" path="$2" label="$3" require_outside="${4:-0}" path_real
  [[ -n "$path" ]] || return 0
  [[ "$path" == /* ]] || path="$checkout_real/$path"
  if _source_guard_has_symlink_component "$path"; then
    _source_guard_die "$label uses a symlink component: $path"
    return 1
  fi
  if [[ -e "$path" ]]; then
    path_real=$(realpath -e -- "$path") || {
      _source_guard_die "cannot canonicalize $label: $path"
      return 1
    }
    case "$path_real" in
      "$checkout_real"|"$checkout_real"/*)
        _source_guard_die "$label resolves inside the source checkout: $path_real"
        return 1
        ;;
    esac
  else
    # Resolve the nearest existing parent so a not-yet-created path below a
    # symlink into the checkout cannot evade the boundary check.
    local parent
    parent=$(dirname -- "$path")
    while [[ ! -e "$parent" && ! -L "$parent" && "$parent" != "/" ]]; do
      parent=$(dirname -- "$parent")
    done
    if _source_guard_has_symlink_component "$parent"; then
      _source_guard_die "$label has a symlinked parent: $parent"
      return 1
    fi
    if [[ -e "$parent" ]]; then
      path_real=$(realpath -e -- "$parent") || {
        _source_guard_die "cannot canonicalize parent of $label: $parent"
        return 1
      }
      if [[ "$require_outside" == "1" ]]; then
        case "$path_real" in
          "$checkout_real"|"$checkout_real"/*)
            _source_guard_die "$label is below the source checkout: $path"
            return 1
            ;;
        esac
      fi
    fi
  fi
}

# Usage: source_checkout_guard CHECKOUT [RUNTIME_DATA_DIR] [NODE_CONFIG]
# A missing or empty checkout is allowed for a clean install. A populated
# non-Git checkout is not: replacing it could erase files whose purpose is
# unknown. Existing Git checkouts must be canonical, free of runtime paths,
# free of tracked runtime files, and clean (including untracked files).
source_checkout_guard() {
  local checkout="${1:-}" runtime_dir="${2:-}" config_file="${3:-}"
  local checkout_real git_root tracked config_count config_value status_output tracked_paths
  [[ -n "$checkout" ]] || {
    _source_guard_die "checkout path was not supplied"
    return 1
  }
  [[ "$checkout" == /* ]] || checkout="$PWD/$checkout"
  if [[ ! -e "$checkout" ]]; then
    return 0
  fi
  [[ ! -L "$checkout" ]] || {
    _source_guard_die "checkout itself is a symlink: $checkout"
    return 1
  }
  checkout_real=$(realpath -e -- "$checkout") || {
    _source_guard_die "cannot canonicalize checkout: $checkout"
    return 1
  }
  if ! git_root=$(git -C "$checkout_real" rev-parse --show-toplevel 2>/dev/null) ||
     [[ -z "$git_root" ]]; then
    if [[ -n "$(find "$checkout_real" -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)" ]]; then
      _source_guard_die "populated checkout is not a Git repository: $checkout_real"
      return 1
    fi
    # Empty directory on a fresh install.
    return 0
  fi
  git_root=$(realpath -e -- "$git_root") || {
    _source_guard_die "cannot canonicalize Git root"
    return 1
  }
  [[ "$git_root" == "$checkout_real" ]] || {
    _source_guard_die "expected checkout root $checkout_real but Git resolves to $git_root"
    return 1
  }

  local candidate
  for candidate in \
    "$checkout_real/data" \
    "$checkout_real/blockchain/data" \
    "$checkout_real/data/testnet" \
    "$checkout_real/chain.db" \
    "$checkout_real/snapshots"
  do
    _source_guard_check_external_path "$checkout_real" "$candidate" "runtime data path" || return 1
  done
  _source_guard_check_external_path "$checkout_real" "$runtime_dir" "runtime data path" 1 || return 1

  if [[ -n "$config_file" && -f "$config_file" ]]; then
    config_count=$(grep -Ec '^[[:space:]]*data_dir[[:space:]]*:' "$config_file" || true)
    if (( config_count > 1 )); then
      _source_guard_die "ambiguous duplicate data_dir entries in $config_file"
      return 1
    fi
    if (( config_count == 1 )); then
      config_value=$(sed -nE 's/^[[:space:]]*data_dir[[:space:]]*:[[:space:]]*([^#]*).*/\1/p' "$config_file")
      config_value="${config_value%$'\r'}"
      config_value="${config_value#\"}"; config_value="${config_value%\"}"
      config_value="${config_value#\'}"; config_value="${config_value%\'}"
      config_value="${config_value#"${config_value%%[![:space:]]*}"}"
      config_value="${config_value%"${config_value##*[![:space:]]}"}"
      [[ -n "$config_value" ]] || {
        _source_guard_die "empty data_dir in $config_file"
        return 1
      }
      _source_guard_check_external_path "$checkout_real" "$config_value" "configured data_dir" 1 || return 1
    fi
  fi

  tracked_paths=$(git -C "$checkout_real" ls-files) || {
    _source_guard_die "cannot enumerate Git-tracked files in $checkout_real"
    return 1
  }
  while IFS= read -r tracked; do
    if _source_guard_is_runtime_path "$tracked"; then
      _source_guard_die "Git tracks a runtime-data path: $tracked"
      return 1
    fi
  done <<< "$tracked_paths"

  status_output=$(git -C "$checkout_real" status --porcelain --untracked-files=all) || {
    _source_guard_die "cannot inspect Git status for $checkout_real"
    return 1
  }
  if [[ -n "$status_output" ]]; then
    _source_guard_die "Git checkout has staged, modified, or untracked files: $checkout_real"
    return 1
  fi
}