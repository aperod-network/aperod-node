#!/usr/bin/env bash
# =============================================================================
#  test-setup-backup-syntax.sh
#  Tests that the bash -n syntax guard in setup-backup.sh correctly:
#    • Rejects a deliberately truncated aperod_backup.sh  (exit non-zero)
#    • Accepts a syntactically valid aperod_backup.sh     (exit 0)
#
#  Strategy
#  ────────
#  Spin up a disposable Ubuntu 22.04 Docker container where all system calls
#  that setup-backup.sh makes (systemctl, systemd-tmpfiles, openssl, sudo,
#  stat, …) are replaced by lightweight stubs in /stubs/ so the script can run
#  as root without a real systemd host.
#
#  Two test cases run sequentially inside the container:
#    T1 — truncated (invalid) aperod_backup.sh  → setup-backup.sh must exit ≠ 0
#    T2 — valid aperod_backup.sh                → setup-backup.sh must exit 0
#
#  Skip condition:
#    Docker is not found in PATH or daemon is not reachable → prints SKIP, exits 0.
#
#  Run from anywhere in the monorepo:
#    bash blockchain/deploy/test-setup-backup-syntax.sh
#
#  Exit codes:
#    0 — both assertions passed (or Docker unavailable → skipped)
#    1 — one or more assertions failed
# =============================================================================
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
BOLD='\033[1m'; NC='\033[0m'

pass() { echo -e "${GREEN}  PASS${NC}  $*"; }
fail() { echo -e "${RED}  FAIL${NC}  $*"; }

# ── Docker availability ───────────────────────────────────────────────────────
if ! command -v docker &>/dev/null; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker not found in PATH — skipping test."
  exit 0
fi
if ! docker info &>/dev/null 2>&1; then
  echo -e "${YELLOW}[SKIP]${NC}  Docker daemon not reachable — skipping test."
  exit 0
fi

IMAGE_TAG="aperod-backup-syntax-test:latest"

# ── Build context ─────────────────────────────────────────────────────────────
CTX=$(mktemp -d)
trap 'rm -rf "$CTX"; docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true' EXIT

# Copy the real deploy directory (setup-backup.sh + aperod_backup.sh)
cp -r "$SCRIPT_DIR" "$CTX/deploy"

# ── Stubs ─────────────────────────────────────────────────────────────────────
mkdir -p "$CTX/stubs"

# systemctl — no-op (daemon-reload, enable, restart, is-active, enable --now …)
cat > "$CTX/stubs/systemctl" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

# systemd-tmpfiles — create the directory that setup-backup.sh expects
cat > "$CTX/stubs/systemd-tmpfiles" << 'STUB'
#!/usr/bin/env bash
# Parse the conf file to create the directory listed in it.
# Format: d /path/to/dir <mode> <user> <group> -
CONF=""
for arg in "$@"; do
  case "$arg" in --create) ;; *) CONF="$arg" ;; esac
done
if [[ -f "$CONF" ]]; then
  while read -r type path mode owner group _; do
    [[ "$type" == "d" ]] || continue
    mkdir -p "$path"
    chmod "$mode" "$path" 2>/dev/null || true
    chown "$owner:$group" "$path" 2>/dev/null || true
  done < <(grep -v '^#' "$CONF")
fi
exit 0
STUB

# openssl — return a deterministic fake hex string for password generation
cat > "$CTX/stubs/openssl" << 'STUB'
#!/usr/bin/env bash
echo "deadbeef1234567890abcdef1234567890abcdef1234567890abcdef12345678"
exit 0
STUB

# sudo — strip "-u <user>" and run the remainder as the current user.
# The container runs as root so privilege-switching is a no-op.
cat > "$CTX/stubs/sudo" << 'STUB'
#!/usr/bin/env bash
args=()
skip_next=false
for arg in "$@"; do
  if $skip_next; then skip_next=false; continue; fi
  if [[ "$arg" == "-u" || "$arg" == "--user" ]]; then skip_next=true; continue; fi
  if [[ "$arg" =~ ^-u.+ ]]; then continue; fi
  args+=("$arg")
done
exec "${args[@]}"
STUB

# ufw — no-op
cat > "$CTX/stubs/ufw" << 'STUB'
#!/usr/bin/env bash
exit 0
STUB

chmod +x "$CTX/stubs/"*

# ── Test harness (runs inside the container) ──────────────────────────────────
cat > "$CTX/test-harness.sh" << 'HARNESS'
#!/usr/bin/env bash
set -uo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
PASS=0; FAIL=0

pass_assert() { echo -e "${GREEN}  PASS${NC}  $*"; ((PASS++)); }
fail_assert() { echo -e "${RED}  FAIL${NC}  $*"; ((FAIL++)); }

# Stubs take priority over system binaries
export PATH="/stubs:$PATH"

# setup-backup.sh checks `id "${APEROD_USER}"` to verify the system user exists.
# Create the aperod user so that check passes without a real system.
useradd --system --no-create-home aperod 2>/dev/null || true

# setup-backup.sh reads SCRIPT_DIR from its own location (BASH_SOURCE); it
# installs /deploy/aperod_backup.sh from SCRIPT_DIR.  We keep the real
# aperod_backup.sh in /deploy/ and swap it out per test case.
REAL_BACKUP="/deploy/aperod_backup.sh"
BACKUP_SRC_SAVE="/tmp/aperod_backup.sh.orig"
cp "$REAL_BACKUP" "$BACKUP_SRC_SAVE"

echo "════════════════════════════════════════════════════════"
echo "  T1: Truncated (invalid) aperod_backup.sh"
echo "      setup-backup.sh must exit non-zero"
echo "════════════════════════════════════════════════════════"

# Write a deliberately truncated / syntactically broken backup script.
# Ends mid-function — bash -n will report a syntax error.
cat > "$REAL_BACKUP" << 'TRUNCATED'
#!/bin/bash
# Deliberately truncated backup script for syntax-check test
set -euo pipefail

_broken_function() {
  echo "this function is never closed
TRUNCATED
chmod +x "$REAL_BACKUP"

T1_EXIT=0
bash /deploy/setup-backup.sh 2>&1 || T1_EXIT=$?

echo ""
echo "  setup-backup.sh exit code (truncated): $T1_EXIT"
if [[ "$T1_EXIT" -ne 0 ]]; then
  pass_assert "T1: setup-backup.sh exited non-zero ($T1_EXIT) for truncated script — guard caught the error"
else
  fail_assert "T1: setup-backup.sh exited 0 for a truncated script — bash -n guard did NOT fire"
fi

echo ""
echo "════════════════════════════════════════════════════════"
echo "  T2: Valid aperod_backup.sh"
echo "      setup-backup.sh must exit 0"
echo "════════════════════════════════════════════════════════"

# Restore the real (syntactically valid) backup script
cp "$BACKUP_SRC_SAVE" "$REAL_BACKUP"
chmod +x "$REAL_BACKUP"

T2_EXIT=0
bash /deploy/setup-backup.sh 2>&1 || T2_EXIT=$?

echo ""
echo "  setup-backup.sh exit code (valid): $T2_EXIT"
if [[ "$T2_EXIT" -eq 0 ]]; then
  pass_assert "T2: setup-backup.sh exited 0 for a valid script"
else
  fail_assert "T2: setup-backup.sh exited $T2_EXIT for a valid script — unexpected failure"
fi

echo ""
echo "────────────────────────────────────────────────────────"
TOTAL=$((PASS + FAIL))
if [[ $FAIL -eq 0 ]]; then
  echo -e "${GREEN}All $TOTAL assertions passed.${NC}"
  exit 0
else
  echo -e "${RED}$FAIL of $TOTAL assertions FAILED.${NC}"
  exit 1
fi
HARNESS
chmod +x "$CTX/test-harness.sh"

# ── Dockerfile ────────────────────────────────────────────────────────────────
cat > "$CTX/Dockerfile" << 'DOCKER'
FROM ubuntu:22.04

ENV DEBIAN_FRONTEND=noninteractive

# Packages required by setup-backup.sh and the test harness:
#   bash      — the script interpreter
#   passwd    — provides useradd (to create the aperod system user)
#   coreutils — stat, install, etc.
#   sed grep  — used inside setup-backup.sh
#   python3   — (none required here, but aperod_backup.sh references it)
# systemctl / systemd-tmpfiles / openssl / sudo / ufw are replaced by stubs.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      bash passwd coreutils sed grep \
 && rm -rf /var/lib/apt/lists/*

# Stub commands — prepended to PATH in the test harness
COPY stubs/       /stubs/
# The real deploy directory — setup-backup.sh + aperod_backup.sh live here
COPY deploy/      /deploy/
# Test harness script
COPY test-harness.sh /test-harness.sh

# Pre-create directories and baseline files that setup-backup.sh requires.
#
# /etc/environment must contain APEROD_BACKUP_PASSWORD so that step 8's
# grep pipeline exits 0 (match found).  Without this, grep exits 1 (no match)
# which, combined with set -euo pipefail inside a $() command substitution,
# causes bash to abort the script before any ok() call is printed.
#
# /etc/aperod/backup-secrets.env is pre-populated so setup-backup.sh takes
# the "already set" branch and skips the openssl password-generation block,
# keeping the test output predictable.
RUN mkdir -p /etc/systemd/system /etc/aperod /etc/cron.d \
             /etc/systemd/system/aperod-api.service.d \
             /etc/tmpfiles.d /run/fake-systemd \
 && echo 'APEROD_BACKUP_PASSWORD=test-dummy-password-for-syntax-check-test' \
      > /etc/environment \
 && echo 'APEROD_BACKUP_PASSWORD=test-dummy-password-for-syntax-check-test' \
      > /etc/aperod/backup-secrets.env \
 && chmod 600 /etc/aperod/backup-secrets.env

CMD ["bash", "/test-harness.sh"]
DOCKER

# ── Build ─────────────────────────────────────────────────────────────────────
echo -e "\n${BOLD}Building Docker test image…${NC}"
if ! docker build --quiet -t "$IMAGE_TAG" "$CTX" 2>&1; then
  docker build -t "$IMAGE_TAG" "$CTX"
  echo -e "${RED}[ERR]${NC}  Docker build failed" >&2
  exit 1
fi
echo -e "${GREEN}[OK]${NC}   Image built: $IMAGE_TAG"

# ── Run ───────────────────────────────────────────────────────────────────────
echo -e "\n${BOLD}Running setup-backup.sh syntax-guard test inside container…${NC}"
echo "────────────────────────────────────────────────────────"

if docker run --rm "$IMAGE_TAG" bash /test-harness.sh; then
  echo -e "\n${GREEN}${BOLD}setup-backup.sh syntax-guard test PASSED.${NC}"
  exit 0
else
  echo -e "\n${RED}${BOLD}setup-backup.sh syntax-guard test FAILED.${NC}"
  exit 1
fi
