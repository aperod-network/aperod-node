#!/usr/bin/env bash
# sync-backup-script.sh — Sourced by update-node.sh, update-api.sh, and tests.
# Do NOT run directly.
#
# Provides: _sync_backup_script [installed_path] [repo_path]
#
#   installed_path — path of the installed binary (default: /usr/local/bin/aperod_backup.sh)
#   repo_path      — path of the repo source      (default: derived from BLOCKCHAIN_DIR or APEROD_DIR)
#
# The function detects a content mismatch (cmp -s) between the installed copy
# and the repo copy, then replaces the installed copy using an atomic
# stage-then-rename pattern:
#
#   1. mktemp in the same directory as the destination (same filesystem)
#   2. cp + chmod (+ best-effort chown) into the staging file
#   3. mv -f staging → destination  (rename(2) — atomic, O_WRONLY never visible)
#
# This guarantees a running cron/systemd backup job sees either the old complete
# file or the new complete file, never a partially written one.
#
# The function is intentionally NON-FATAL:
#   • If the installed file is absent (backup not set up), it returns 0.
#   • If the repo file is missing, it returns 0.
#   • Any staging or rename failure is printed to stderr and the function
#     returns 0 so the caller (update-node/update-api) is never aborted.
#
# Path override (used by automated tests):
#   Call as: _sync_backup_script /path/to/installed /path/to/repo
#
# =============================================================================

_sync_backup_script() {
  local installed="${1:-/usr/local/bin/aperod_backup.sh}"

  # Derive the repo path from context when not explicitly provided.
  local repo
  if [[ -n "${2:-}" ]]; then
    repo="$2"
  elif [[ -n "${BLOCKCHAIN_DIR:-}" ]]; then
    repo="${BLOCKCHAIN_DIR}/deploy/aperod_backup.sh"
  elif [[ -n "${APEROD_DIR:-}" ]]; then
    repo="${APEROD_DIR}/blockchain/deploy/aperod_backup.sh"
  else
    echo "  [warn] aperod_backup.sh sync skipped — neither BLOCKCHAIN_DIR nor APEROD_DIR is set" >&2
    return 0
  fi

  # Backup not configured on this server — nothing to do.
  [[ -f "$installed" ]] || return 0
  # Repo copy missing (should never happen) — skip silently.
  [[ -f "$repo" ]]      || return 0

  if cmp -s "$installed" "$repo" 2>/dev/null; then
    echo "  [sync] aperod_backup.sh already up to date — no copy needed."
    return 0
  fi

  # Versions differ — stage in the same directory for an atomic rename.
  # mktemp in dirname(installed) ensures we stay on the same filesystem so
  # the subsequent mv -f is a rename(2) call and is therefore atomic.
  local install_dir
  install_dir="$(dirname "$installed")"
  local tmp
  if ! tmp=$(mktemp "${install_dir}/.aperod_backup_sync.XXXXXXXX" 2>/dev/null); then
    echo "  [warn] aperod_backup.sh sync FAILED — cannot create staging file in ${install_dir}" >&2
    return 0
  fi

  local staged=false
  if cp "$repo" "$tmp" 2>/dev/null && chmod 700 "$tmp" 2>/dev/null; then
    chown root:root "$tmp" 2>/dev/null || true   # best-effort; harmless if not root (tests)
    staged=true
  fi

  if [[ "$staged" == true ]] && mv -f "$tmp" "$installed" 2>/dev/null; then
    echo "  [sync] aperod_backup.sh updated: repo → ${install_dir}/ (versions differed)"
  else
    rm -f "$tmp" 2>/dev/null || true
    echo "  [warn] aperod_backup.sh sync FAILED — staging or rename failed; check permissions on ${install_dir}" >&2
  fi
}
