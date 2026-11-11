#!/usr/bin/env bash
# Sourced by deployment entrypoints. Never execute the candidate to inspect it.
# ldd is required here: an unavailable verifier is not permission to deploy.
explorer_indexer_static_guard() {
  if [[ $# -ne 1 ]]; then
    echo "Usage: explorer_indexer_static_guard BINARY" >&2
    return 1
  fi
  local binary="$1" output magic
  if [[ ! -f "$binary" || ! -x "$binary" ]]; then
    echo "Static-link check FAILED: explorer-indexer candidate is absent or not executable: $binary" >&2
    return 1
  fi
  magic=$(head -c 4 -- "$binary") || return 1
  if [[ "$magic" != $'\x7fELF' ]]; then
    echo "Static-link check FAILED: explorer-indexer candidate is not an ELF binary: $binary" >&2
    return 1
  fi
  if ! command -v ldd >/dev/null 2>&1; then
    echo "Static-link check FAILED: ldd is unavailable; cannot verify explorer-indexer." >&2
    return 1
  fi
  # glibc ldd normally exits 1 for a static executable; its output is decisive.
  output=$(LC_ALL=C ldd "$binary" 2>&1 || true)
  if [[ "$output" != *"not a dynamic executable"* ]]; then
    echo "Static-link check FAILED: explorer-indexer is dynamically linked or unverifiable: $binary" >&2
    printf '%s\n' "$output" >&2
    echo "Fix: ensure CGO_ENABLED=0 in the Makefile build-explorer-indexer target." >&2
    return 1
  fi
  echo "explorer-indexer: ldd confirmed a statically linked binary."
}
