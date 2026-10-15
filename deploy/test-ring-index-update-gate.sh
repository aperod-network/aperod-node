#!/usr/bin/env bash
set -euo pipefail
script="$(dirname "$0")/update-node.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
# Extract exact production functions, substituting only the independent
# executable location in this synthetic fixture; never add an env bypass.
awk '/^_ring_index_gate\(\)/{on=1} on{print} /^_ring_index_start\(\)/{start=1} start&&/^}/{exit}' "$script" |
  sed "s|/usr/local/libexec/aperod-ring-index-maintenance|$root/approved-tool|" >"$root/functions.sh"
source "$root/functions.sh"
BINARY_DST="$root/node"; SERVICE_NAME=synthetic-only
calls="$root/calls"; export calls
systemctl() { printf '%s\n' "$*" >>"$calls"; }
if _ring_index_start; then echo "missing helper accepted" >&2;exit 1;fi
[[ ! -e "$calls" ]]
cat >"$root/approved-tool" <<'EOF'
#!/usr/bin/env bash
exit 42
EOF
chmod 755 "$root/approved-tool"
if _ring_index_start; then echo "unknown guard exit accepted" >&2;exit 1;fi
[[ ! -e "$calls" ]]
cat >"$root/approved-tool" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$calls"
exit 0
EOF
_ring_index_start
grep -q '^--phase guard-update --candidate .* --current .* --config ' "$calls"
[[ "$(tail -1 "$calls")" == "start synthetic-only" ]]
# Every start path, including backup failure and rollback, must use wrapper.
[[ "$(grep -c 'systemctl start "${SERVICE_NAME}"' "$script")" == 1 ]]
grep -q '_ring_index_gate "${BINARY_SRC}" "${BINARY_DST}" || exit 1' "$script"
echo "PASS: missing/refused guards never start; all update/rollback starts gated"
