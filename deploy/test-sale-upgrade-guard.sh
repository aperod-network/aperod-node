#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
source "$here/sale-upgrade-guard.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
touch "$tmp/config.yaml"
cat > "$tmp/candidate" <<'SH'
#!/usr/bin/env bash
if [[ "$1" == --sale-upgrade-preflight-capability ]]; then
    [[ "$2" == --validate-config && "$3" == --config && -f "$4" ]] || exit 19
    printf '%s' '{"schema":1,"command":"sale-upgrade-preflight"}'
    exit 0
fi
[[ "$1" == --sale-upgrade-preflight && "$2" == --config && -f "$3" ]] || exit 19
printf '%s' "$RECEIPT"
exit "${RESULT:-0}"
SH
chmod 755 "$tmp/candidate"
valid='{"schema":1,"status":"ok","mode":"active","tip_height":42,"tip_hash":"1111111111111111111111111111111111111111111111111111111111111111"}'
for receipt in "$valid" '{"status":"ok"}' 'config OK' '{"schema":true,"status":"ok"}' \
    '{"schema":1,"status":"ok","mode":"active","tip_height":true,"tip_hash":"1111111111111111111111111111111111111111111111111111111111111111"}'; do
    export RECEIPT="$receipt"
    rm -f "$tmp/stopped"
    if sale_upgrade_guard "$tmp/candidate" "$tmp/config.yaml" > /dev/null 2>&1; then
        touch "$tmp/stopped"
    fi
    if [[ "$receipt" == "$valid" ]]; then
        test -f "$tmp/stopped"
    else
        test ! -f "$tmp/stopped"
    fi
done
export RECEIPT="$valid" RESULT=77
if sale_upgrade_guard "$tmp/candidate" "$tmp/config.yaml" >/dev/null 2>&1; then
    echo "unexpected checker exit accepted" >&2; exit 1
fi
cat > "$tmp/legacy-candidate" <<'SH'
#!/usr/bin/env bash
for arg in "$@"; do
    if [[ "$arg" == --validate-config ]]; then echo "config OK"; exit 0; fi
done
touch "$LEGACY_STARTED"
exit 0
SH
chmod 755 "$tmp/legacy-candidate"
export LEGACY_STARTED="$tmp/legacy-started"
if sale_upgrade_guard "$tmp/legacy-candidate" "$tmp/config.yaml" >/dev/null 2>&1; then
    echo "legacy candidate accepted" >&2; exit 1
fi
test ! -e "$LEGACY_STARTED"
python3 - "$here" <<'PY'
from pathlib import Path
import sys
d=Path(sys.argv[1])
public_paths=[d/'update-node.sh']
private_paths=[d.parent.parent/'deploy/deploy-server.sh',d.parent.parent/'deploy/deploy.sh']
for path in public_paths+[path for path in private_paths if path.is_file()]:
    text=path.read_text()
    calls=[i for i,l in enumerate(text.splitlines()) if 'sale_upgrade_guard ' in l and not l.lstrip().startswith('#')]
    stops=[i for i,l in enumerate(text.splitlines()) if 'systemctl stop ' in l and not l.lstrip().startswith('#')]
    assert calls and stops and min(calls)<min(stops), path
if not all(path.is_file() for path in private_paths):
    print("SKIP: private monorepo deployment entrypoints are not part of the standalone public source")
PY
export RESULT=0
# Execute the exact remote heredoc, not a hand-written approximation. The
# remote function and candidate must refuse before any systemctl stop/cp/start.
python3 - "$here" "$tmp" <<'PY'
from pathlib import Path
import sys
d,tmp=map(Path,sys.argv[1:])
text=(d/'update-validator.sh').read_text()
body=text.split("REMOTE_SCRIPT=$(cat <<'REMOTE_EOF'\n",1)[1].split('\nREMOTE_EOF',1)[0]
assert '"bash -s" <<< "${REMOTE_SCRIPT}"' in text
assert '"sudo bash -s"' not in text
body=body.replace('BINARY_SRC="/tmp/aperod-node-new"',f'BINARY_SRC="{tmp}/candidate"')
body=body.replace('__NODE_CONFIG__',str(tmp/'config.yaml'))
(tmp/'remote-body.sh').write_text(body)
PY
mkdir "$tmp/bin"
cat > "$tmp/bin/sudo" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$SYSTEM_EVENTS"
exit 99
SH
chmod 755 "$tmp/bin/sudo"
cat > "$tmp/bin/ssh" <<'SH'
#!/usr/bin/env bash
# Model a deploy account permitting only a normal shell, never sudo bash.
[[ "${*: -1}" == "bash -s" ]] || exit 91
exec bash -s
SH
chmod 755 "$tmp/bin/ssh"
{ declare -f sale_upgrade_guard; cat "$tmp/remote-body.sh"; } > "$tmp/remote-script.sh"
export SYSTEM_EVENTS="$tmp/events"
for receipt in '{"status":"blocked"}' 'unsupported command'; do
    export RECEIPT="$receipt"
    rm -f "$SYSTEM_EVENTS"
    if PATH="$tmp/bin:$PATH" ssh "aperod@restricted-validator" "bash -s" < "$tmp/remote-script.sh" >/dev/null 2>&1; then
        echo "remote refusal unexpectedly succeeded" >&2; exit 1
    fi
    test ! -e "$SYSTEM_EVENTS"
done
export RECEIPT="$valid"
rm -f "$SYSTEM_EVENTS"
PATH="$tmp/bin:$PATH" ssh "aperod@restricted-validator" "bash -s" < "$tmp/remote-script.sh" >/dev/null 2>&1 || true
grep -q 'systemctl stop' "$SYSTEM_EVENTS"
echo "PASS: each validator refuses before remote stop; aligned control reaches stop"
echo "PASS: strict refusal, success, unexpected exit and pre-stop integration"
