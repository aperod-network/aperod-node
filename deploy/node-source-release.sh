#!/usr/bin/env bash
# Node-only source acquisition. Never refresh, reset or clean a live checkout.
APEROD_PUBLIC_NODE_ORIGIN="https://github.com/aperod-network/aperod-node.git"
APEROD_PUBLIC_NODE_ROOT="4bfab534469dc61296910d968ab13cab27554a97"
_NODE_SOURCE_HELPERS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${_NODE_SOURCE_HELPERS}/source-safe-guard.sh"

_node_source_git_env() {
  local name
  while IFS= read -r name; do unset "$name" || return 1; done < <(compgen -A variable GIT_)
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1
  export GIT_TERMINAL_PROMPT=0
}

node_source_pin_guard() {
  [[ "${APEROD_NODE_SOURCE_COMMIT:-}" =~ ^[0-9a-f]{40}$ ]] || {
    echo "[node-source] REFUSED / ОТКАЗ: set APEROD_NODE_SOURCE_COMMIT to an explicitly reviewed full public commit / укажите явно проверенный полный публичный коммит." >&2
    return 1
  }
}

node_source_fresh_state_guard() {
  local path entries
  for path in "$@"; do
    _source_guard_has_symlink_component "$path" && {
      echo "[node-source] REFUSED / ОТКАЗ: fresh-install state path has a symlink component / путь состояния новой установки содержит символьную ссылку." >&2; return 1;
    }
    if [[ -e "$path" ]]; then
      [[ -d "$path" ]] || return 1
      entries="$(find "$path" -mindepth 1 -maxdepth 1 -print -quit)" || return 1
      [[ -z "$entries" ]] || {
        echo "[node-source] REFUSED / ОТКАЗ: state/configuration already exists; use a baseline-approved update or isolated recovery / данные или конфигурация уже существуют; используйте одобренное обновление или изолированное восстановление." >&2
        return 1
      }
    fi
  done
}

# This is a scoped operator approval, not an automatic consensus rehearsal.
# A blanket 'compatible=true' is deliberately insufficient.
node_source_baseline_guard() {
  local binary="$1" actual
  node_source_pin_guard || return 1
  [[ -f "$binary" && ! -L "$binary" ]] || return 1
  actual="$(sha256sum -- "$binary")" || return 1
  actual="${actual%% *}"
  [[ "${APEROD_BASELINE_APPROVED_SOURCE:-}" == "$APEROD_NODE_SOURCE_COMMIT" &&
     "${APEROD_BASELINE_APPROVED_BINARY_SHA256:-}" =~ ^[0-9a-f]{64}$ &&
     "${APEROD_BASELINE_APPROVED_BINARY_SHA256}" == "$actual" ]] || {
    echo "[node-source] REFUSED / ОТКАЗ: baseline approval must bind this running binary SHA256 to the requested public source commit; rehearse compatibility first / разрешение должно связать SHA256 работающего бинарника с публичным коммитом; сначала проверьте совместимость." >&2
    return 1
  }
}

# expected_origin/root are explicit trusted inputs. Entry points below hardcode
# the public identity; local tests supply independent real Git fixture identities.
node_source_fetch_verified() (
  set -euo pipefail
  local origin="$1" expected_origin="$2" commit="$3" root="$4" destination="$5"
  [[ "$origin" == "$expected_origin" && "$commit" =~ ^[0-9a-f]{40}$ &&
     "$root" =~ ^[0-9a-f]{40}$ && "$destination" == /* &&
     ! -e "$destination" && ! -L "$destination" ]] || {
    echo "[node-source] REFUSED / ОТКАЗ: wrong origin/commit or destination already exists / неверный источник или коммит, либо каталог назначения уже существует." >&2
    exit 1
  }
  _source_guard_has_symlink_component "$destination" && {
    echo "[node-source] REFUSED / ОТКАЗ: symlink destination component / путь назначения содержит символьную ссылку." >&2; exit 1;
  }
  _node_source_git_env || exit 1
  mkdir -m 700 -- "$destination" || exit 1
  git -c core.hooksPath=/dev/null -c init.templateDir= init -q "$destination" || exit 1
  [[ -d "$destination/.git" && ! -L "$destination/.git" ]] || exit 1
  git -C "$destination" remote add origin "$origin" || exit 1
  git -c core.hooksPath=/dev/null -C "$destination" fetch --no-tags origin \
    '+refs/heads/main:refs/remotes/origin/main' || exit 1
  [[ "$(git -C "$destination" config --get remote.origin.url)" == "$expected_origin" ]] || exit 1
  [[ "$(git -C "$destination" rev-parse --verify "$commit^{commit}")" == "$commit" ]] || exit 1
  git -C "$destination" merge-base --is-ancestor "$commit" refs/remotes/origin/main || exit 1
  [[ "$(git -C "$destination" rev-list --max-parents=0 "$commit")" == "$root" ]] || {
    echo "[node-source] REFUSED / ОТКАЗ: commit is not descended from the reviewed public root / коммит не является потомком проверенной публичной основы." >&2
    exit 1
  }
  git -c core.hooksPath=/dev/null -C "$destination" checkout --detach -q "$commit" || exit 1
  [[ -f "$destination/go.mod" && ! -L "$destination/go.mod" &&
     -f "$destination/Makefile" && ! -L "$destination/Makefile" &&
     -d "$destination/cmd/node" && ! -d "$destination/blockchain" ]] || exit 1
  source_checkout_guard "$destination" "" "/dev/null" || exit 1
  local status
  status="$(git -C "$destination" status --porcelain --untracked-files=all)" || exit 1
  [[ -z "$status" ]] || exit 1
  printf '[node-source] verified public commit / проверен публичный коммит %s\n' "$commit"
)

node_source_prepare() {
  node_source_pin_guard || return 1
  NODE_SOURCE_JOB="$(mktemp -d /var/tmp/aperod-node-source.XXXXXXXX)" || return 1
  NODE_SOURCE_DIR="$NODE_SOURCE_JOB/repo"
  trap 'node_source_cleanup' EXIT
  node_source_fetch_verified "$APEROD_PUBLIC_NODE_ORIGIN" "$APEROD_PUBLIC_NODE_ORIGIN" \
    "$APEROD_NODE_SOURCE_COMMIT" "$APEROD_PUBLIC_NODE_ROOT" "$NODE_SOURCE_DIR" || return 1
}

# Root installers retain a private, root-owned checkout for real VCS stamping.
# Never whitelist a service-account-writable repository for a root build.
node_source_build_context() {
  [[ "$1" == "${NODE_SOURCE_DIR:-}" && -d "$1/.git" ]] || return 1
  [[ "$(stat -c %u "$NODE_SOURCE_JOB")" == "$EUID" &&
     "$(stat -c %u "$1")" == "$EUID" &&
     "$(stat -c %u "$1/.git")" == "$EUID" ]] || return 1
  _node_source_git_env || return 1
}

node_source_candidate_info_guard() {
  local info="$1" commit="$2"
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || return 1
  grep -Eq '^[[:space:]]*build[[:space:]]*CGO_ENABLED=0$' <<< "$info" &&
    grep -Eq "^[[:space:]]*build[[:space:]]*vcs[.]revision=${commit}$" <<< "$info" &&
    grep -Eq '^[[:space:]]*build[[:space:]]*vcs[.]modified=false$' <<< "$info"
}

node_source_candidate_guard() (
  local checkout="$1" binary="$2" revision status info segments
  _node_source_git_env || return 1
  node_source_pin_guard || return 1
  [[ -d "$checkout/.git" && ! -L "$checkout/.git" &&
     -f "$binary" && ! -L "$binary" ]] || return 1
  revision="$(git -c safe.directory="$checkout" -C "$checkout" rev-parse --verify HEAD)" || return 1
  status="$(git -c safe.directory="$checkout" -C "$checkout" status --porcelain --untracked-files=no)" || return 1
  [[ "$revision" == "$APEROD_NODE_SOURCE_COMMIT" && -z "$status" ]] || {
    echo "[node-source] REFUSED / ОТКАЗ: selected source changed during the build / выбранный исходный код изменился во время сборки." >&2; return 1;
  }
  command -v go >/dev/null && command -v readelf >/dev/null || {
    echo "[node-source] REFUSED / ОТКАЗ: go and readelf are required to verify the candidate / для проверки нужны Go и readelf." >&2; return 1;
  }
  info="$(go version -m "$binary")" || return 1
  node_source_candidate_info_guard "$info" "$APEROD_NODE_SOURCE_COMMIT" || {
      echo "[node-source] REFUSED / ОТКАЗ: candidate lacks clean, portable build provenance for the selected commit / у сборки нет подтверждения чистого и переносимого происхождения из выбранного коммита." >&2
      return 1
    }
  segments="$(readelf -l "$binary")" || return 1
  [[ "$segments" != *INTERP* ]] || {
    echo "[node-source] REFUSED / ОТКАЗ: candidate has a dynamic ELF interpreter / бинарник зависит от динамического ELF-интерпретатора." >&2; return 1;
  }
)

node_source_cleanup() {
  # Only a mktemp-owned job is disposable. Never a configurable live path.
  [[ "${NODE_SOURCE_JOB:-}" == /var/tmp/aperod-node-source.* &&
     "${NODE_SOURCE_JOB##*/}" =~ ^aperod-node-source\.[A-Za-z0-9]{8}$ ]] || return 0
  cd / || return 1
  rm -rf -- "$NODE_SOURCE_JOB"
}
