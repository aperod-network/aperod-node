#!/usr/bin/env bash
# Install the versioned, root-owned rollout cleanup CLI and its systemd timer.
# This installer works from a local source checkout; it never fetches or resets
# git state. Use --destdir only for isolated installer tests/package staging.
set -euo pipefail

ENABLE_APPLY=0
DESTDIR="${DESTDIR:-}"
while (($#)); do
  case "$1" in
    --enable-apply) ENABLE_APPLY=1 ;;
    --destdir)
      (($# >= 2)) || { echo "--destdir requires a directory" >&2; exit 2; }
      DESTDIR="$2"
      shift
      ;;
    -h|--help)
      echo "Usage: $0 [--enable-apply] [--destdir DIRECTORY]"
      exit 0
      ;;
    *) echo "Unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PACKAGE_DIR="${SCRIPT_DIR}/rollout_cleanup"
[[ -f "${PACKAGE_DIR}/__init__.py" && -f "${PACKAGE_DIR}/cli.py" ]] || {
  echo "rollout_cleanup package source files are missing" >&2
  exit 1
}
[[ -z "$(find "$PACKAGE_DIR" -type l -print -quit)" ]] || {
  echo "rollout_cleanup source package contains a symlink" >&2
  exit 1
}
if [[ -z "${DESTDIR}" && "$(id -u)" -ne 0 ]]; then
  echo "Run as root: sudo bash $0 [--enable-apply]" >&2
  exit 1
fi
if [[ -n "${DESTDIR}" && -z "${SYSTEMCTL:-}" ]]; then
  echo "Installer staging requires an explicit SYSTEMCTL stub" >&2
  exit 2
fi
if [[ -n "${DESTDIR}" && ( "${DESTDIR}" != /* || "${DESTDIR}" == "/" ) ]]; then
  echo "--destdir must be an absolute, non-root staging directory" >&2
  exit 2
fi

target() { printf '%s%s' "${DESTDIR%/}" "$1"; }
SYSTEMCTL="${SYSTEMCTL:-systemctl}"
INSTALL_OWNER=()
if [[ "$(id -u)" -eq 0 ]]; then
  INSTALL_OWNER=(-o root -g root)
fi

LIB_ROOT="$(target /usr/local/lib/aperod-rollout-cleanup)"
VERSION_ROOT="${LIB_ROOT}/releases"
STATE_ROOT="$(target /var/lib/aperod-rollout-cleanup)"
RELEASE_ROOT="$(target /opt/aperod/releases)"
SYSTEMD_ROOT="$(target /etc/systemd/system)"
BIN_ROOT="$(target /usr/local/bin)"
POLICY_FILE="$(target /etc/aperod/rollout-cleanup.json)"

install -d "${INSTALL_OWNER[@]}" -m 755 "${VERSION_ROOT}" "${BIN_ROOT}" \
  "${SYSTEMD_ROOT}" "$(dirname "${POLICY_FILE}")" "${RELEASE_ROOT}"
install -d "${INSTALL_OWNER[@]}" -m 700 "${STATE_ROOT}"

# Build one immutable version directory from every Python module in the
# package. Hash relative names and contents with length framing, then switch
# the launcher only after the complete bundle is staged and syntax-checked.
stage="$(mktemp -d "${VERSION_ROOT}/.stage.XXXXXXXX")"
cleanup_stage() { [[ -z "${stage:-}" ]] || rm -rf -- "${stage}"; }
trap cleanup_stage EXIT
install -d "${INSTALL_OWNER[@]}" -m 755 "${stage}/rollout_cleanup"
PACKAGE_PY_FILES=()
while IFS= read -r -d '' source_file; do
  PACKAGE_PY_FILES+=("$source_file")
  relative="${source_file#${PACKAGE_DIR}/}"
  staged_file="${stage}/rollout_cleanup/${relative}"
  install -d "${INSTALL_OWNER[@]}" -m 755 "$(dirname "$staged_file")"
  install "${INSTALL_OWNER[@]}" -m 644 "$source_file" "$staged_file"
done < <(find "$PACKAGE_DIR" -type f -name '*.py' -print0 | sort -z)
[[ ${#PACKAGE_PY_FILES[@]} -gt 0 ]] || { echo "No Python package modules found" >&2; exit 1; }
python3 - "${stage}/rollout_cleanup" <<'PY'
import pathlib, sys
root = pathlib.Path(sys.argv[1])
for path in sorted(root.rglob("*.py")):
    compile(path.read_text(encoding="utf-8"), str(path), "exec")
PY
find "${stage}/rollout_cleanup" -type f -name '*.py' -exec chmod 644 {} +
if [[ "$(id -u)" -eq 0 ]]; then
  find "${stage}/rollout_cleanup" -type f -name '*.py' -exec chown root:root {} +
fi
bundle_hash="$(
  python3 - "${stage}/rollout_cleanup" <<'PY'
import hashlib, pathlib, sys
root = pathlib.Path(sys.argv[1])
digest = hashlib.sha256()
for path in sorted(root.rglob("*.py"), key=lambda item: item.relative_to(root).as_posix()):
    relative = path.relative_to(root).as_posix().encode("utf-8")
    content = path.read_bytes()
    digest.update(len(relative).to_bytes(8, "big"))
    digest.update(relative)
    digest.update(len(content).to_bytes(8, "big"))
    digest.update(content)
print(digest.hexdigest())
PY
)"
bundle="${VERSION_ROOT}/${bundle_hash}"
if [[ -e "${bundle}" ]]; then
  [[ -z "$(find "$bundle" -type l -print -quit)" ]] || {
    echo "Existing versioned package contains a symlink" >&2
    exit 1
  }
  python3 - "${stage}/rollout_cleanup" "${bundle}/rollout_cleanup" <<'PY'
import pathlib, sys
source, installed = (pathlib.Path(item) for item in sys.argv[1:])
def modules(root):
    return {item.relative_to(root).as_posix(): item.read_bytes()
            for item in root.rglob("*.py") if item.is_file() and not item.is_symlink()}
if modules(source) != modules(installed):
    raise SystemExit("Existing versioned rollout package differs from its content hash")
PY
  rm -rf -- "${stage}"
  stage=""
else
  mv -- "${stage}" "${bundle}"
  stage=""
fi
if [[ "$(id -u)" -eq 0 ]]; then
  chown -R root:root "${bundle}"
fi
find "${bundle}" -type d -exec chmod 755 {} +
find "${bundle}" -type f -name '*.py' -exec chmod 644 {} +

launcher_tmp="$(mktemp "${BIN_ROOT}/.aperod-rollout-cleanup.XXXXXXXX")"
cat > "${launcher_tmp}" <<EOF
#!/usr/bin/env python3
import json
import os
import stat
import sys
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, "${VERSION_ROOT}/${bundle_hash}")
from rollout_cleanup.cli import main

# The root-owned policy defaults to dry-run. --enable-apply is the only
# installer switch that changes unattended service behavior to deletion.
args = sys.argv[1:]
if args and args[0] in ("scan", "retirement-scan") and "--apply" not in args and "--dry-run" not in args:
    policy_path = Path("${POLICY_FILE}")
    try:
        info = policy_path.lstat()
        if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) \\
                or info.st_uid != 0 or info.st_mode & 0o077:
            raise ValueError("unsafe cleanup policy file")
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        if policy.get("mode") == "apply":
            args.append("--apply")
        elif policy.get("mode") != "dry-run":
            raise ValueError("unsupported cleanup policy mode")
    except (OSError, ValueError, AttributeError) as exc:
        print("aperod-rollout-cleanup: refusing scan: " + str(exc), file=sys.stderr)
        raise SystemExit(2)
raise SystemExit(main(args))
EOF
chmod 755 "${launcher_tmp}"
if [[ "$(id -u)" -eq 0 ]]; then
  chown root:root "${launcher_tmp}"
fi
python3 -c 'import pathlib,sys; compile(pathlib.Path(sys.argv[1]).read_text(), sys.argv[1], "exec")' "${launcher_tmp}"
mv -f "${launcher_tmp}" "$(target /usr/local/bin/aperod-rollout-cleanup)"

policy_tmp="$(mktemp "$(dirname "${POLICY_FILE}")/.rollout-cleanup.XXXXXXXX")"
if (( ENABLE_APPLY )); then
  printf '%s\n' '{"mode":"apply"}' > "${policy_tmp}"
else
  # An ordinary reinstall never silently turns an explicitly enabled host back
  # to dry-run (or enables deletion). A missing file is always created dry-run.
  if [[ -e "${POLICY_FILE}" || -L "${POLICY_FILE}" ]]; then
    [[ -f "${POLICY_FILE}" && ! -L "${POLICY_FILE}" ]] || {
      echo "Existing rollout cleanup policy is not a regular file" >&2
      exit 1
    }
    _policy_owner="$(stat -c '%u' "${POLICY_FILE}")"
    _policy_mode="$(stat -c '%a' "${POLICY_FILE}")"
    if [[ "$(id -u)" -eq 0 ]]; then _expected_owner=0; else _expected_owner="$(id -u)"; fi
    [[ "${_policy_owner}" == "${_expected_owner}" && "${_policy_mode}" == 600 ]] || {
      echo "Existing rollout cleanup policy has unsafe ownership or mode" >&2
      exit 1
    }
    python3 - "${POLICY_FILE}" <<'PY'
import json,sys
with open(sys.argv[1], encoding="utf-8") as stream:
    policy = json.load(stream)
if not isinstance(policy, dict) or policy.get("mode") not in ("dry-run", "apply"):
    raise SystemExit("Existing rollout cleanup policy has an unsupported mode")
PY
    rm -f "${policy_tmp}"
    policy_tmp=""
  else
    printf '%s\n' '{"mode":"dry-run"}' > "${policy_tmp}"
  fi
fi
if [[ -n "${policy_tmp}" ]]; then
  chmod 600 "${policy_tmp}"
  if [[ "$(id -u)" -eq 0 ]]; then
    chown root:root "${policy_tmp}"
  fi
  mv -f "${policy_tmp}" "${POLICY_FILE}"
fi

install "${INSTALL_OWNER[@]}" -m 644 "${SCRIPT_DIR}/aperod-rollout-cleanup.service" \
  "$(target /etc/systemd/system/aperod-rollout-cleanup.service)"
install "${INSTALL_OWNER[@]}" -m 644 "${SCRIPT_DIR}/aperod-rollout-cleanup.timer" \
  "$(target /etc/systemd/system/aperod-rollout-cleanup.timer)"
install "${INSTALL_OWNER[@]}" -m 644 "${SCRIPT_DIR}/aperod-historical-retirement.service" \
  "$(target /etc/systemd/system/aperod-historical-retirement.service)"
install "${INSTALL_OWNER[@]}" -m 644 "${SCRIPT_DIR}/aperod-historical-retirement.timer" \
  "$(target /etc/systemd/system/aperod-historical-retirement.timer)"

"${SYSTEMCTL}" daemon-reload
"${SYSTEMCTL}" enable --now aperod-rollout-cleanup.timer
"${SYSTEMCTL}" enable --now aperod-historical-retirement.timer
echo "Installed root-owned rollout cleanup CLI and daily timer."
if (( ENABLE_APPLY )); then
  echo "Apply policy enabled explicitly; deletion still requires the core's verified B2 backup proof."
else
  echo "Default policy is dry-run. Review results before opting in with --enable-apply."
fi