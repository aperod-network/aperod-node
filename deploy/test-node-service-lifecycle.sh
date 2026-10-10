#!/usr/bin/env bash
# Real systemd/Go/Git/LevelDB lifecycle, exclusively in a disposable CI guest.
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODE="${1:-all}"
case "$MODE" in install|validator|update|upgrade|all) ;; *) exit 2 ;; esac
if [[ "$MODE" == "all" ]]; then
  for mode in install validator update upgrade; do
    bash "$0" "$mode"
  done
  exit 0
fi
if ! command -v docker >/dev/null || ! docker info >/dev/null 2>&1; then
  echo "SKIP: node-service-$MODE (Docker CI guest unavailable)"
  echo "SKIP_SUMMARY: node-service-$MODE"
  exit 77
fi
JOB="$(mktemp -d)"
IMAGE="aperod-lifecycle:$(basename "$JOB" | tr '[:upper:]' '[:lower:]')"
CONTAINER=""
cleanup() {
  [[ -z "$CONTAINER" ]] || docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker image rm -f "$IMAGE" >/dev/null 2>&1 || true
  rm -rf "$JOB"
}
trap cleanup EXIT
cp -R "$DIR" "$JOB/deploy"
cp "$DIR/fixtures/node-lifecycle/Dockerfile" "$JOB/Dockerfile"
docker build -t "$IMAGE" "$JOB"
CONTAINER="$(docker run -d --privileged --cgroupns=private \
  --tmpfs /run --tmpfs /run/lock "$IMAGE")"
for _ in $(seq 1 40); do
  if docker exec "$CONTAINER" systemctl is-system-running --wait >/dev/null 2>&1; then break; fi
  if docker exec "$CONTAINER" systemctl show -p SystemState --value | grep -qE '^running$|^degraded$'; then break; fi
  sleep 1
done
docker exec "$CONTAINER" python3 /reviewed-deploy/test-node-service-lifecycle.py "$MODE"
