#!/usr/bin/env bash
# sync-backup-script.sh — manual, separately approved tool installation only.
# Do NOT run directly.
#
# Manual-only API: _sync_backup_script installed_path repo_path approved_sha256
# approved_sha256 must come from an independently reviewed tool release, not
# from this checkout, its clean status, a node pin or a manifest stored in it.
# Node/API updates deliberately do not source or invoke this helper.
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
# Missing/unconfigured files are harmless no-ops. Authentication, symlink,
# staged-byte and syntax failures are fatal and retain the installed tool.
# Infrastructure behavior retained for manual callers:
#   • If the installed file is absent (backup not set up), it returns 0.
#   • If the repo file is missing, it returns 0.
#   • Any staging or rename failure is printed to stderr and the function
#     returns 0 with a warning; binary/API updaters never call this helper.
#
# Path override (used by automated tests):
#   Call as: _sync_backup_script /path/to/installed /path/to/repo APPROVED_SHA256
#
# =============================================================================

_sync_backup_script() {
  local installed="${1:-/usr/local/bin/aperod_backup.sh}"
  local approved="${3:-}"

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

  [[ "$approved" =~ ^[0-9a-f]{64}$ ]] || {
    echo "[tool-trust] REFUSED: separate externally approved tool SHA256 is required; no files changed." >&2
    return 1
  }
  # Reject file and ancestor symlinks before reading or staging either path.
  local path part current
  for path in "$installed" "$repo"; do
    [[ "$path" == /* ]] || path="$PWD/$path"
    current="/"
    local -a parts
    IFS='/' read -r -a parts <<< "${path#/}"
    for part in "${parts[@]}"; do
      current="${current%/}/$part"
      [[ ! -L "$current" ]] || {
        echo "[tool-trust] REFUSED: symlink path; no files changed." >&2
        return 1
      }
    done
  done
  local digest
  digest="$(sha256sum -- "$repo")" || return 1
  [[ "${digest%% *}" == "$approved" ]] || {
    echo "[tool-trust] REFUSED: source is not the separately approved tool; no files changed." >&2
    return 1
  }

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

  # Authenticate the staged bytes as well: a source can change after the first
  # digest check. Syntax checks must also precede replacement of the good tool.
  if [[ "$staged" == true ]]; then
    digest="$(sha256sum -- "$tmp")" || digest=""
    if [[ "${digest%% *}" != "$approved" ]] || [[ ! -s "$tmp" ]] || ! bash -n "$tmp" 2>/dev/null; then
      rm -f -- "$tmp"
      echo "[tool-trust] REFUSED: staged tool failed approval or syntax; installed tool retained." >&2
      return 1
    fi
  fi

  if [[ "$staged" == true ]] && mv -f "$tmp" "$installed" 2>/dev/null; then
    echo "  [sync] aperod_backup.sh updated: repo → ${install_dir}/ (versions differed)"

    # Self-check: verify the newly installed file is non-empty, executable,
    # and syntactically valid.  A truncated repo copy (e.g. disk-full during
    # git pull, partial write, or partial download) would silently leave the
    # running backup job with a broken script — catch it now, before the next
    # scheduled backup runs.
    #
    # These checks mirror the guards in install-node.sh step 12 and
    # setup-backup.sh.  Unlike the rest of _sync_backup_script (which is
    # intentionally non-fatal for infrastructure reasons such as the backup
    # not being configured on this server), a corrupted installed copy is a
    # data-integrity problem that must abort the deploy loudly.
    if [[ ! -s "$installed" ]]; then
      echo "  [ERROR] aperod_backup.sh self-check FAILED: installed file is empty — possible truncated write" >&2
      echo "          Path: $installed" >&2
      return 1
    fi
    if [[ ! -x "$installed" ]]; then
      echo "  [ERROR] aperod_backup.sh self-check FAILED: installed file is not executable after chmod 700" >&2
      echo "          Path: $installed" >&2
      return 1
    fi
    if ! bash -n "$installed" 2>/dev/null; then
      echo "  [ERROR] aperod_backup.sh self-check FAILED: bash -n syntax check failed — repo copy may be truncated or corrupted" >&2
      echo "          Path: $installed" >&2
      return 1
    fi
    echo "  [sync] aperod_backup.sh self-check passed: non-empty, executable, bash -n OK"
  else
    rm -f "$tmp" 2>/dev/null || true
    echo "  [warn] aperod_backup.sh sync FAILED — staging or rename failed; check permissions on ${install_dir}" >&2
  fi
}
