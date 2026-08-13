#!/usr/bin/env bash
# =============================================================================
#  aperod-api-deploy.sh — Atomic API rebuild + systemd restart
#
#  Run this instead of bare `systemctl restart aperod-api` after every
#  git pull.  A bare restart replays the OLD compiled dist/index.mjs;
#  new TypeScript changes are silently ignored until a rebuild is done.
#
#  Usage:
#    sudo aperod-deploy           # if installed via deploy.sh (see §7 of DEPLOY.md)
#    sudo bash /opt/aperod/deploy/aperod-api-deploy.sh
#
#  The script must run as root so it can switch to the 'aperod' user for the
#  pnpm build, then call systemctl as root.
#
#  Optional env vars for Telegram failure alerts:
#    SUPPORT_BOT_TOKEN      — Telegram bot token  (e.g. 123456:ABC-xxx)
#    SUPPORT_ADMIN_CHAT_ID  — Telegram chat/user ID to receive the alert
#
#  If the build fails the service is NOT restarted — the old binary keeps
#  running so users are never served a broken API.
# =============================================================================
set -euo pipefail

APEROD_DIR="${APEROD_DIR:-/opt/aperod}"
APP_USER="${APP_USER:-aperod}"
API_SERVICE="${API_SERVICE:-aperod-api}"
API_FILTER="@workspace/api-server"
ADMIN_FILTER="@workspace/admin-panel"
HEALTH_URL="${HEALTH_URL:-http://localhost:3001/api/health}"
HEALTH_RETRIES="${HEALTH_RETRIES:-10}"   # × 3 s = 30 s max
SKIP_HEALTH_CHECK="${SKIP_HEALTH_CHECK:-0}"
API_ENV_FILE="${API_ENV_FILE:-/etc/aperod-api.env}"
# Variables the API server refuses to start without (mirrors REQUIRED_VARS in src/index.ts).
REQUIRED_API_VARS=("PORT" "DATABASE_URL" "SESSION_SECRET")

# ── colour helpers ─────────────────────────────────────────────────────────
GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'
CYAN='\033[0;36m'; NC='\033[0m'
info()    { echo -e "${CYAN}[deploy]${NC}  $*"; }
success() { echo -e "${GREEN}[deploy]${NC}  $*"; }
warn()    { echo -e "${YELLOW}[deploy]${NC}  $*"; }
fail()    { echo -e "${RED}[deploy]${NC}  $*" >&2; }

# ── Self-update: keep /usr/local/bin/aperod-deploy in sync with the repo ──
# After `git pull` the repo copy of this script may differ from the installed
# binary.  When hashes differ:
#   1. Write the new copy atomically to a temp file inside /usr/local/bin/
#      (root-owned, not writable by the aperod service account).
#   2. chown root:root + chmod 755, then `mv` into place — no truncation window.
#   3. Re-exec from the *installed* path (trusted, root-owned) — never directly
#      from the worktree — so the rest of this run uses the new logic.
#
# Trust model: copying from the worktree is intentional and exactly equivalent
# to the previous manual `sudo cp` step the operator performed after every pull.
# The attack surface is unchanged; the only difference is the operator no longer
# has to remember to do it.
#
# Prerequisite: /usr/local/bin/aperod-deploy must already exist (one-time
# bootstrap — see INSTALL.md §7).  If it is absent, this block is skipped and
# the operator's direct invocation (`sudo bash deploy/aperod-api-deploy.sh`)
# continues normally.
_REPO_SCRIPT="${APEROD_DIR}/deploy/aperod-api-deploy.sh"
_INSTALLED_SCRIPT="/usr/local/bin/aperod-deploy"
if [[ -f "$_REPO_SCRIPT" && -f "$_INSTALLED_SCRIPT" ]]; then
  _repo_hash=$(sha256sum "$_REPO_SCRIPT"      | awk '{print $1}')
  _inst_hash=$(sha256sum "$_INSTALLED_SCRIPT" | awk '{print $1}')
  if [[ "$_repo_hash" != "$_inst_hash" ]]; then
    echo "[deploy] Script updated in repo — syncing $_INSTALLED_SCRIPT and re-executing..."
    # Atomic install: stage in /usr/local/bin then rename (same filesystem → O_RENAME).
    _TMP=$(mktemp /usr/local/bin/.aperod-deploy.XXXXXX)
    cp "$_REPO_SCRIPT" "$_TMP"
    chown root:root "$_TMP"
    chmod 755 "$_TMP"
    mv "$_TMP" "$_INSTALLED_SCRIPT"
    # Re-exec from the root-owned installed path, not from the worktree.
    exec "$_INSTALLED_SCRIPT" "$@"
  fi
fi

# ── prerequisite checks ────────────────────────────────────────────────────
[[ $EUID -ne 0 ]] && { fail "Run as root: sudo aperod-deploy"; exit 1; }
[[ -d "$APEROD_DIR" ]] || { fail "Project directory not found: $APEROD_DIR"; exit 1; }

# ── Telegram alert helper (never aborts the script) ───────────────────────
send_telegram() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      > /dev/null 2>&1 || true
  fi
}

# ── Load Telegram credentials from /etc/aperod/watchdog.env if present ────
# (same credentials file used by the node watchdog)
if [[ -f /etc/aperod/watchdog.env ]]; then
  # shellcheck source=/dev/null
  source /etc/aperod/watchdog.env 2>/dev/null || true
fi

# =============================================================================
# Step 0 — Required environment variable check
# =============================================================================
# The API server exits with code 1 at startup when PORT, DATABASE_URL, or
# SESSION_SECRET are absent.  Catching this before the build prevents a
# crash-loop caused by restarting into an incomplete environment.
# See §2a of INSTALL.md for env-file setup instructions.
# =============================================================================
info "[0/3] Checking required API environment variables..."

_missing_vars=()
for _var in "${REQUIRED_API_VARS[@]}"; do
  # Primary: check the env file directly (reliable before daemon-reload).
  if [[ -f "$API_ENV_FILE" ]] && grep -qE "^${_var}=.+" "$API_ENV_FILE"; then
    continue
  fi
  # Fallback: check the currently-loaded systemd environment (covers vars set
  # via inline Environment= in override.conf, not only the env file).
  if systemctl show "$API_SERVICE" --property=Environment 2>/dev/null \
       | tr ';' '\n' | grep -qE "^${_var}=."; then
    continue
  fi
  _missing_vars+=("$_var")
done

if [[ ${#_missing_vars[@]} -gt 0 ]]; then
  fail ""
  fail "Required environment variables are NOT set:"
  for _v in "${_missing_vars[@]}"; do
    fail "  • $_v"
  done
  fail ""
  fail "See INSTALL.md §2a for full setup instructions."
  fail ""
  fail "Quick fix:"
  fail "  1. sudo bash ${APEROD_DIR}/scripts/setup-env.sh"
  fail "  2. Edit ${API_ENV_FILE} — replace every CHANGE_ME"
  fail "  3. Ensure the EnvironmentFile drop-in exists:"
  fail "       sudo bash -c 'printf \"[Service]\\\\nEnvironmentFile=${API_ENV_FILE}\\\\n\" >"
  fail "         /etc/systemd/system/${API_SERVICE}.service.d/env-file.conf'"
  fail "  4. sudo systemctl daemon-reload"
  fail "  5. sudo aperod-deploy"
  fail ""
  fail "Health check (verify vars are visible to systemd):"
  fail "  sudo systemctl show ${API_SERVICE} --property=Environment \\"
  fail "    | tr ';' '\\n' | grep -E '^(PORT|DATABASE_URL|SESSION_SECRET)='"
  fail ""
  fail "${API_SERVICE} was NOT restarted — old binary still running."
  send_telegram "⚠️ <b>aperod-deploy: отсутствуют обязательные переменные окружения</b>
Сервер: $(hostname)
Не заданы: ${_missing_vars[*]}
Укажите их в <code>${API_ENV_FILE}</code> и запустите <code>aperod-deploy</code> повторно."
  exit 1
fi

success "All required environment variables are set."

# =============================================================================
# Step 1 — Admin Panel frontend build
# =============================================================================
info "[1/4] Building admin panel (pnpm --filter ${ADMIN_FILTER} run build)..."

if [[ -d "${APEROD_DIR}/artifacts/admin-panel/dist" ]]; then
  chown -R "${APP_USER}:${APP_USER}" "${APEROD_DIR}/artifacts/admin-panel/dist" 2>/dev/null || true
fi

if ! sudo -u "$APP_USER" bash -c "
    export PATH=\$PATH:/usr/local/go/bin
    cd '${APEROD_DIR}'
    pnpm --filter '${ADMIN_FILTER}' run build
"; then
  fail ""
  fail "Admin panel build FAILED — API was NOT restarted."
  send_telegram "⚠️ <b>aperod-deploy: сборка admin panel ПРОВАЛИЛАСЬ</b>
Сервер: $(hostname)
Исправьте ошибку и запустите <code>aperod-deploy</code> повторно."
  exit 1
fi

success "Admin panel built — dist/public/ updated."

# =============================================================================
# Step 2 — TypeScript build (API server)
# =============================================================================
info "[2/4] Building TypeScript (pnpm --filter ${API_FILTER} run build)..."

# Ensure dist/ is owned by APP_USER so the build can overwrite it even after
# an emergency root restart left root-owned files there (EACCES on unlink).
if [[ -d "${APEROD_DIR}/artifacts/api-server/dist" ]]; then
  chown -R "${APP_USER}:${APP_USER}" "${APEROD_DIR}/artifacts/api-server/dist" 2>/dev/null || true
fi

if ! sudo -u "$APP_USER" bash -c "
    export PATH=\$PATH:/usr/local/go/bin
    cd '${APEROD_DIR}'
    pnpm --filter '${API_FILTER}' run build
"; then
  fail ""
  fail "Build FAILED — ${API_SERVICE} was NOT restarted."
  fail "The old compiled binary is still running. Fix the error and re-run aperod-deploy."
  send_telegram "⚠️ <b>aperod-api: сборка ПРОВАЛИЛАСЬ</b>
Сервер: $(hostname)
Сервис <b>НЕ перезапущен</b> — старый бинарник всё ещё работает.
Исправьте ошибку TypeScript и запустите <code>aperod-deploy</code> повторно."
  exit 1
fi

success "Build succeeded — dist/index.mjs updated."

# =============================================================================
# Step 3 — Restart the systemd service
# =============================================================================
info "[3/4] Restarting ${API_SERVICE} via systemctl..."

systemctl restart "${API_SERVICE}"
success "${API_SERVICE} restarted."

# =============================================================================
# Step 4 — Health check
# =============================================================================
if [[ "$SKIP_HEALTH_CHECK" == "1" ]]; then
  info "[4/4] SKIP_HEALTH_CHECK=1 — skipping health probe."
else
  info "[4/4] Waiting for API to come up (up to $((HEALTH_RETRIES * 3)) s)..."
  healthy=0
  for i in $(seq 1 "$HEALTH_RETRIES"); do
    if curl -sf "$HEALTH_URL" > /dev/null 2>&1; then
      healthy=1
      break
    fi
    sleep 3
  done

  if [[ "$healthy" -eq 1 ]]; then
    success "API is healthy: $HEALTH_URL"
  else
    fail "Health check FAILED — API did not respond within $((HEALTH_RETRIES * 3)) s."
    fail "Check logs: journalctl -u ${API_SERVICE} -n 100 --no-pager"
    send_telegram "⚠️ <b>aperod-api: health check ПРОВАЛИЛСЯ</b>
Сервер: $(hostname)
Сервис перезапущен, но не ответил на <code>${HEALTH_URL}</code>.
Проверьте логи: <code>journalctl -u ${API_SERVICE} -n 100 --no-pager</code>"
    exit 1
  fi
fi

# =============================================================================
# Keep /usr/local/bin/aperod-frontend-deploy in sync with the repo
# (same pattern as aperod-deploy self-sync)
# =============================================================================
_FE_SRC="${APEROD_DIR}/deploy/aperod-frontend-deploy.sh"
if [[ -f "$_FE_SRC" ]]; then
  if ! diff -q "$_FE_SRC" /usr/local/bin/aperod-frontend-deploy &>/dev/null; then
    cp "$_FE_SRC" /usr/local/bin/aperod-frontend-deploy
    chmod +x /usr/local/bin/aperod-frontend-deploy
    success "aperod-frontend-deploy синхронизирован"
  fi
fi

# =============================================================================
# Done
# =============================================================================
echo ""
echo -e "${GREEN}══════════════════════════════════════════════${NC}"
echo -e "${GREEN}  aperod deployed successfully! (admin panel + API)${NC}"
echo -e "${GREEN}══════════════════════════════════════════════${NC}"
echo ""
echo -e "  Logs:   journalctl -u ${API_SERVICE} -f"
echo -e "  Status: systemctl status ${API_SERVICE}"
echo ""
