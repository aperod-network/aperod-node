#!/usr/bin/env bash
# =============================================================================
#  test-upgrade-node.sh — End-to-end tests for upgrade-node.sh
#
#  Strategy
#  ────────
#  upgrade-node.sh is the canonical upgrade entry point.  It must:
#    1. Call ensure-dropin.sh (to write / verify memory-protection drop-ins).
#    2. Call update-node.sh   (which rebuilds the binary and restarts the
#                              service) ONLY AFTER ensure-dropin.sh returns.
#
#  Two scenarios are run, each inside a disposable Ubuntu 22.04 Docker container:
#
#  Scenario A — normal upgrade (drop-ins already present):
#    The DROPIN_DIR already contains gomemlimit.conf and timeout.conf before
#    upgrade-node.sh runs.  The stub ensure-dropin.sh records its call; the
#    stub update-node.sh records when it would restart the service.
#    Assertions:
#      A1. upgrade-node.sh exits 0.
#      A2. ensure-dropin.sh was called (call record exists).
#      A3. ensure-dropin.sh was called BEFORE the service restart logged by the
#          stub update-node.sh (ordering guarantee).
#      A4. Both drop-in files still exist after the run.
#
#  Scenario B — pre-drop-in install (no drop-in files exist):
#    The DROPIN_DIR is empty.  The stub ensure-dropin.sh creates the drop-in
#    files AND logs its call.  The stub update-node.sh then records service
#    restart.  This simulates upgrading a node that was installed before
#    task-1429 added the drop-ins.
#    Assertions:
#      B1. upgrade-node.sh exits 0.
#      B2. ensure-dropin.sh was called.
#      B3. ensure-dropin.sh was called BEFORE the service restart.
#      B4. gomemlimit.conf was created by ensure-dropin.sh (did not exist before).
#      B5. timeout.conf was created by ensure-dropin.sh (did not exist before).
#      B6. gomemlimit.conf contains [Service] and GOMEMLIMIT=.
#      B7. timeout.conf contains [Service] and TimeoutStopSec=.
#
#  Skip condition:
#    Docker is not found in PATH or daemon is not reachable → exits 0 (SKIP).
#
#  Run from anywhere:
#    bash blockchain/deploy/test-upgrade-node.sh
#
#  Exit codes:
#    0 — all assertions passed (or Docker unavailable → skipped)
#    1 — one or more assertions failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
BOLD='\033[1m'; NC='\033[0m'

pass()    { echo -e "${GREEN}  PASS${NC}  $*"; }
fail()    { echo -e "${RED}  FAIL${NC}  $*"; }
section() { echo -e "\n${BOLD}▶ $*${NC}"; }

# ── Check Docker availability ─────────────────────────────────────────────────
if ! command -v docker &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker not found in PATH — skipping upgrade-node e2e test."
  exit 0
fi

if ! docker info &>/dev/null 2>&1; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker daemon not reachable — skipping upgrade-node e2e test."
  exit 0
fi

IMAGE_TAG="aperod-upgrade-node-test:latest"

# ── Build context in a temp directory ────────────────────────────────────────
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"; docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true' EXIT

# Copy the whole deploy directory so upgrade-node.sh finds all sibling scripts.
cp -r "$SCRIPT_DIR" "$CTX/deploy"

# ── preseed.sh: minimal "previously installed" state ─────────────────────────
# Creates the binary and service file that upgrade-node.sh checks for before
# proceeding, plus the aperod system user required by the service unit.
cat > "$CTX/preseed.sh" << 'PRESEED'
#!/usr/bin/env bash
set -euo pipefail

# aperod system user (mirrors what install-node.sh creates)
useradd --system --no-create-home --shell /usr/sbin/nologin aperod 2>/dev/null || true

# Standard directory layout
mkdir -p /opt/aperod /etc/systemd/system /etc/aperod /var/log/aperod

# "Old" binary — a simple placeholder so the pre-install guard passes
cat > /usr/local/bin/aperod-node << 'BINARY'
#!/usr/bin/env bash
echo "aperod-node v0.0.0-old-stub"
BINARY
chmod +x /usr/local/bin/aperod-node

# Minimal service file so the other guard passes
cat > /etc/systemd/system/aperod-node.service << 'UNIT'
[Unit]
Description=Aperod Node
After=network.target

[Service]
User=aperod
ExecStart=/usr/local/bin/aperod-node
Restart=always

[Install]
WantedBy=multi-user.target
UNIT

echo "[preseed] Minimal installed-node state created."
PRESEED
chmod +x "$CTX/preseed.sh"

# ── Stub ensure-dropin.sh (placed alongside upgrade-node.sh in the image) ─────
# This stub replaces the real ensure-dropin.sh in the same directory so
# upgrade-node.sh picks it up automatically via UPGRADE_DIR resolution.
#
# Behaviour:
#   1. Appends "ensure-dropin-called" to the ordering log.
#   2. Writes both drop-in files to DROPIN_DIR (simulating real work).
#   3. Exits 0 so upgrade-node.sh continues to the update step.
#
# The DROPIN_DIR env var is forwarded by upgrade-node.sh, so the stub
# can write to the test-controlled directory.
cat > "$CTX/stub-ensure-dropin.sh" << 'STUB'
#!/usr/bin/env bash
# Stub ensure-dropin.sh — records call and writes drop-in files.
set -euo pipefail

DROPIN_DIR="${DROPIN_DIR:-/etc/systemd/system/aperod-node.service.d}"
SEQ_LOG="${SEQ_LOG:-/tmp/upgrade-sequence.log}"

mkdir -p "${DROPIN_DIR}"

# Record ordering event FIRST (before any file I/O)
echo "ensure-dropin-called" >> "${SEQ_LOG}"

# Write the drop-in files (matches what the real ensure-dropin.sh produces)
cat > "${DROPIN_DIR}/gomemlimit.conf" << 'DROPIN'
[Service]
Environment="GOMEMLIMIT=5905580032"
DROPIN

cat > "${DROPIN_DIR}/timeout.conf" << 'DROPIN'
# Aperod node — shutdown timeout drop-in
[Service]
TimeoutStopSec=900
DROPIN

echo "[stub-ensure-dropin] drop-ins written to ${DROPIN_DIR}"
exit 0
STUB
chmod +x "$CTX/stub-ensure-dropin.sh"

# ── Stub update-node.sh (placed alongside upgrade-node.sh in the image) ───────
# Records when the service restart step would have occurred.
# upgrade-node.sh calls it AFTER ensure-dropin.sh returns.
cat > "$CTX/stub-update-node.sh" << 'STUB'
#!/usr/bin/env bash
# Stub update-node.sh — records the service-start event for ordering check.
set -euo pipefail

SEQ_LOG="${SEQ_LOG:-/tmp/upgrade-sequence.log}"

echo "[stub-update-node] starting (simulating upgrade sequence)"

# Record the service-restart event in the ordering log.
echo "service-restart-called" >> "${SEQ_LOG}"

echo "[stub-update-node] done"
exit 0
STUB
chmod +x "$CTX/stub-update-node.sh"

# ── Scenario A harness ────────────────────────────────────────────────────────
cat > "$CTX/test-harness-a.sh" << 'HARNESS'
#!/usr/bin/env bash
# Scenario A — normal upgrade: drop-in files already present before upgrade.
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail_assert() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }

echo "══════════════════════════════════════════════════"
echo "  [Scenario A] Pre-seeding minimal installed-node state"
echo "══════════════════════════════════════════════════"
bash /preseed.sh

# ── Set up a controlled DROPIN_DIR with drop-ins already present ──────────────
DROPIN_DIR_A="$(mktemp -d)"
SEQ_LOG_A="/tmp/upgrade-seq-a.log"
rm -f "$SEQ_LOG_A"

# Pre-populate drop-ins (simulating an existing install with drop-ins)
cat > "$DROPIN_DIR_A/gomemlimit.conf" << 'DROPIN'
[Service]
Environment="GOMEMLIMIT=5905580032"
DROPIN
cat > "$DROPIN_DIR_A/timeout.conf" << 'DROPIN'
[Service]
TimeoutStopSec=900
DROPIN

echo "[scenario-a] DROPIN_DIR_A=$DROPIN_DIR_A — pre-populated with both drop-ins."

# ── Run upgrade-node.sh with stubs injected via PATH ─────────────────────────
# upgrade-node.sh finds ensure-dropin.sh and update-node.sh relative to itself
# (UPGRADE_DIR), so we place the stubs in /opt/aperod/blockchain/deploy/
# alongside a copy of the real upgrade-node.sh.
mkdir -p /opt/aperod/blockchain/deploy
cp /deploy/upgrade-node.sh          /opt/aperod/blockchain/deploy/upgrade-node.sh
cp /stub-ensure-dropin.sh           /opt/aperod/blockchain/deploy/ensure-dropin.sh
cp /stub-update-node.sh             /opt/aperod/blockchain/deploy/update-node.sh
chmod +x /opt/aperod/blockchain/deploy/upgrade-node.sh
chmod +x /opt/aperod/blockchain/deploy/ensure-dropin.sh
chmod +x /opt/aperod/blockchain/deploy/update-node.sh

echo ""
echo "══════════════════════════════════════════════════"
echo "  [Scenario A] Running upgrade-node.sh"
echo "══════════════════════════════════════════════════"

DROPIN_DIR="${DROPIN_DIR_A}" \
SEQ_LOG="${SEQ_LOG_A}" \
  bash /opt/aperod/blockchain/deploy/upgrade-node.sh
UPGRADE_EXIT=$?

echo ""
echo "══════════════════════════════════════════════════"
echo "  [Scenario A] Assertions"
echo "══════════════════════════════════════════════════"

# A1: upgrade-node.sh must exit 0
if [[ $UPGRADE_EXIT -eq 0 ]]; then
  pass_assert "A1: upgrade-node.sh exited 0"
else
  fail_assert "A1: upgrade-node.sh exited $UPGRADE_EXIT (expected 0)"
fi

# A2: ensure-dropin.sh must have been called
if grep -q "ensure-dropin-called" "${SEQ_LOG_A}" 2>/dev/null; then
  pass_assert "A2: ensure-dropin.sh was called (entry found in sequence log)"
else
  fail_assert "A2: ensure-dropin.sh was NOT called (no entry in sequence log)"
  echo "     sequence log:" >&2
  cat "${SEQ_LOG_A}" >&2 || echo "     (log missing)" >&2
fi

# A3: ensure-dropin.sh must appear BEFORE the service-restart in the log
if [[ -f "${SEQ_LOG_A}" ]]; then
  DROPIN_LINE=$(grep -n "ensure-dropin-called"    "${SEQ_LOG_A}" | head -1 | cut -d: -f1)
  RESTART_LINE=$(grep -n "service-restart-called" "${SEQ_LOG_A}" | head -1 | cut -d: -f1)
  if [[ -n "${DROPIN_LINE}" && -n "${RESTART_LINE}" && "${DROPIN_LINE}" -lt "${RESTART_LINE}" ]]; then
    pass_assert "A3: ensure-dropin.sh (line ${DROPIN_LINE}) ran before service-restart (line ${RESTART_LINE})"
  else
    fail_assert "A3: ordering wrong — ensure-dropin line='${DROPIN_LINE:-<absent>}', service-restart line='${RESTART_LINE:-<absent>}'"
    echo "     sequence log contents:" >&2
    cat "${SEQ_LOG_A}" >&2 || echo "     (log missing)" >&2
  fi
else
  fail_assert "A3: sequence log file missing — cannot check ordering"
fi

# A4: both drop-in files must still exist after the run
if [[ -f "${DROPIN_DIR_A}/gomemlimit.conf" && -f "${DROPIN_DIR_A}/timeout.conf" ]]; then
  pass_assert "A4: both drop-in files exist in ${DROPIN_DIR_A} after upgrade"
else
  fail_assert "A4: one or both drop-in files missing in ${DROPIN_DIR_A}"
  ls "${DROPIN_DIR_A}" >&2 || true
fi

echo ""
echo "──────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL Scenario A assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL Scenario A assertions FAILED.${NC}"
  exit 1
fi
HARNESS
chmod +x "$CTX/test-harness-a.sh"

# ── Scenario B harness ────────────────────────────────────────────────────────
cat > "$CTX/test-harness-b.sh" << 'HARNESS'
#!/usr/bin/env bash
# Scenario B — pre-drop-in install: no drop-in files exist before upgrade.
# Simulates a node installed before task-1429 added memory-protection drop-ins.
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail_assert() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }

echo "══════════════════════════════════════════════════"
echo "  [Scenario B] Pre-seeding minimal installed-node state (no drop-ins)"
echo "══════════════════════════════════════════════════"
bash /preseed.sh

# ── Set up a controlled DROPIN_DIR that is initially empty ───────────────────
DROPIN_DIR_B="$(mktemp -d)"
SEQ_LOG_B="/tmp/upgrade-seq-b.log"
rm -f "$SEQ_LOG_B"

echo "[scenario-b] DROPIN_DIR_B=$DROPIN_DIR_B — empty (no pre-existing drop-ins)."

# Confirm the directory is truly empty before the upgrade runs.
if [[ -f "${DROPIN_DIR_B}/gomemlimit.conf" || -f "${DROPIN_DIR_B}/timeout.conf" ]]; then
  echo "[scenario-b] ERROR: DROPIN_DIR_B is not empty — test setup is invalid." >&2
  exit 1
fi

# ── Copy stubs into place alongside upgrade-node.sh ─────────────────────────
mkdir -p /opt/aperod/blockchain/deploy
cp /deploy/upgrade-node.sh          /opt/aperod/blockchain/deploy/upgrade-node.sh
cp /stub-ensure-dropin.sh           /opt/aperod/blockchain/deploy/ensure-dropin.sh
cp /stub-update-node.sh             /opt/aperod/blockchain/deploy/update-node.sh
chmod +x /opt/aperod/blockchain/deploy/upgrade-node.sh
chmod +x /opt/aperod/blockchain/deploy/ensure-dropin.sh
chmod +x /opt/aperod/blockchain/deploy/update-node.sh

echo ""
echo "══════════════════════════════════════════════════"
echo "  [Scenario B] Running upgrade-node.sh (no pre-existing drop-ins)"
echo "══════════════════════════════════════════════════"

DROPIN_DIR="${DROPIN_DIR_B}" \
SEQ_LOG="${SEQ_LOG_B}" \
  bash /opt/aperod/blockchain/deploy/upgrade-node.sh
UPGRADE_EXIT=$?

echo ""
echo "══════════════════════════════════════════════════"
echo "  [Scenario B] Assertions"
echo "══════════════════════════════════════════════════"

# B1: upgrade-node.sh must exit 0
if [[ $UPGRADE_EXIT -eq 0 ]]; then
  pass_assert "B1: upgrade-node.sh exited 0 on pre-drop-in install"
else
  fail_assert "B1: upgrade-node.sh exited $UPGRADE_EXIT (expected 0)"
fi

# B2: ensure-dropin.sh must have been called
if grep -q "ensure-dropin-called" "${SEQ_LOG_B}" 2>/dev/null; then
  pass_assert "B2: ensure-dropin.sh was called on pre-drop-in install"
else
  fail_assert "B2: ensure-dropin.sh was NOT called on pre-drop-in install"
  echo "     sequence log:" >&2
  cat "${SEQ_LOG_B}" >&2 || echo "     (log missing)" >&2
fi

# B3: ensure-dropin.sh must appear BEFORE the service-restart
if [[ -f "${SEQ_LOG_B}" ]]; then
  DROPIN_LINE=$(grep -n "ensure-dropin-called"    "${SEQ_LOG_B}" | head -1 | cut -d: -f1)
  RESTART_LINE=$(grep -n "service-restart-called" "${SEQ_LOG_B}" | head -1 | cut -d: -f1)
  if [[ -n "${DROPIN_LINE}" && -n "${RESTART_LINE}" && "${DROPIN_LINE}" -lt "${RESTART_LINE}" ]]; then
    pass_assert "B3: ensure-dropin.sh (line ${DROPIN_LINE}) ran before service-restart (line ${RESTART_LINE})"
  else
    fail_assert "B3: ordering wrong — ensure-dropin line='${DROPIN_LINE:-<absent>}', service-restart line='${RESTART_LINE:-<absent>}'"
    echo "     sequence log contents:" >&2
    cat "${SEQ_LOG_B}" >&2 || echo "     (log missing)" >&2
  fi
else
  fail_assert "B3: sequence log file missing — cannot check ordering"
fi

# B4: gomemlimit.conf must have been created by ensure-dropin.sh
if [[ -f "${DROPIN_DIR_B}/gomemlimit.conf" ]]; then
  pass_assert "B4: gomemlimit.conf was created in ${DROPIN_DIR_B} (pre-drop-in install)"
else
  fail_assert "B4: gomemlimit.conf was NOT created — ensure-dropin.sh may not have written it"
fi

# B5: timeout.conf must have been created
if [[ -f "${DROPIN_DIR_B}/timeout.conf" ]]; then
  pass_assert "B5: timeout.conf was created in ${DROPIN_DIR_B} (pre-drop-in install)"
else
  fail_assert "B5: timeout.conf was NOT created — ensure-dropin.sh may not have written it"
fi

# B6: gomemlimit.conf must have the required content
if [[ -f "${DROPIN_DIR_B}/gomemlimit.conf" ]]; then
  if grep -q "^\[Service\]$" "${DROPIN_DIR_B}/gomemlimit.conf" \
     && grep -q "GOMEMLIMIT=" "${DROPIN_DIR_B}/gomemlimit.conf"; then
    pass_assert "B6: gomemlimit.conf contains [Service] and GOMEMLIMIT= directive"
  else
    fail_assert "B6: gomemlimit.conf content invalid (content: $(cat "${DROPIN_DIR_B}/gomemlimit.conf" 2>/dev/null))"
  fi
else
  fail_assert "B6: gomemlimit.conf missing — cannot check content"
fi

# B7: timeout.conf must have the required content
if [[ -f "${DROPIN_DIR_B}/timeout.conf" ]]; then
  if grep -q "^\[Service\]$" "${DROPIN_DIR_B}/timeout.conf" \
     && grep -q "TimeoutStopSec=" "${DROPIN_DIR_B}/timeout.conf"; then
    pass_assert "B7: timeout.conf contains [Service] and TimeoutStopSec= directive"
  else
    fail_assert "B7: timeout.conf content invalid (content: $(cat "${DROPIN_DIR_B}/timeout.conf" 2>/dev/null))"
  fi
else
  fail_assert "B7: timeout.conf missing — cannot check content"
fi

echo ""
echo "──────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL Scenario B assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL Scenario B assertions FAILED.${NC}"
  exit 1
fi
HARNESS
chmod +x "$CTX/test-harness-b.sh"

# ── Dockerfile ────────────────────────────────────────────────────────────────
cat > "$CTX/Dockerfile" << 'DOCKER'
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Install only what the test harnesses genuinely need:
#   bash        — upgrade-node.sh and all stubs are bash scripts
#   coreutils   — mkdir, cat, cp, grep, stat etc.
#   passwd      — provides useradd (for the aperod system user)
#   util-linux  — provides runuser
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      bash coreutils passwd util-linux \
 && rm -rf /var/lib/apt/lists/*

# Real deploy directory — upgrade-node.sh + all sibling scripts
COPY deploy/               /deploy/
# Shared stub scripts
COPY stub-ensure-dropin.sh /stub-ensure-dropin.sh
COPY stub-update-node.sh   /stub-update-node.sh
# Pre-seed and test harnesses
COPY preseed.sh            /preseed.sh
COPY test-harness-a.sh     /test-harness-a.sh
COPY test-harness-b.sh     /test-harness-b.sh

# Pre-create standard system directories
RUN mkdir -p /etc/systemd/system /etc/aperod /var/log/aperod /opt/aperod

CMD ["bash", "/test-harness-a.sh"]
DOCKER

# ── Build the test image ──────────────────────────────────────────────────────
echo -e "\n${BOLD}Building Docker test image…${NC}"
if ! docker build --quiet -t "$IMAGE_TAG" "$CTX" 2>&1; then
  docker build -t "$IMAGE_TAG" "$CTX"
  echo -e "${RED}[ERR]${NC}  Docker build failed" >&2
  exit 1
fi
echo -e "${GREEN}[OK]${NC}   Image built: $IMAGE_TAG"

# ── Run Scenario A: normal upgrade (drop-ins already present) ─────────────────
echo -e "\n${BOLD}[Scenario A] Normal upgrade — drop-ins already present…${NC}"
echo "────────────────────────────────────────────────────"

SA_PASS=false
if docker run --rm "$IMAGE_TAG" bash /test-harness-a.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}Scenario A (normal upgrade) PASSED.${NC}"
  SA_PASS=true
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}Scenario A (normal upgrade) FAILED.${NC}"
fi

# ── Run Scenario B: pre-drop-in install ───────────────────────────────────────
echo -e "\n${BOLD}[Scenario B] Pre-drop-in install — no drop-ins before upgrade…${NC}"
echo "────────────────────────────────────────────────────"

SB_PASS=false
if docker run --rm "$IMAGE_TAG" bash /test-harness-b.sh; then
  echo "────────────────────────────────────────────────────"
  echo -e "${GREEN}${BOLD}Scenario B (pre-drop-in install) PASSED.${NC}"
  SB_PASS=true
else
  echo "────────────────────────────────────────────────────"
  echo -e "${RED}${BOLD}Scenario B (pre-drop-in install) FAILED.${NC}"
fi

# ── Overall result ────────────────────────────────────────────────────────────
echo ""
if [[ "$SA_PASS" == "true" && "$SB_PASS" == "true" ]]; then
  echo -e "${GREEN}${BOLD}All upgrade-node.sh e2e scenarios PASSED.${NC}"
  exit 0
else
  echo -e "${RED}${BOLD}One or more upgrade-node.sh e2e scenarios FAILED.${NC}"
  exit 1
fi
