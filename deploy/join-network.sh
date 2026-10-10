#!/usr/bin/env bash
# ============================================================
#  Aperod — Join Existing Network
#
#  РЕЖИМ 1 (push): Запуск на ОСНОВНОМ узле (validator).
#  Подключает новый сервер к работающей сети за один шаг.
#
#    sudo bash join-network.sh <IP_НОВОГО_СЕРВЕРА>
#
#    Пример: sudo bash join-network.sh 77.221.153.86
#
#  РЕЖИМ 2 (bootstrap): Запуск на НОВОМ реле.
#  Копирует chain.db и актуальный снимок с валидатора, чтобы
#  реле не забанило валидатора при первом подключении.
#
#    sudo bash join-network.sh --bootstrap-from=<IP_ВАЛИДАТОРА>
#
#    Пример: sudo bash join-network.sh --bootstrap-from=89.169.53.128
#
#  Что делает скрипт (push-режим):
#    1. Останавливает и отключает aperod-node на новом сервере
#    2. Rsync цепи с --delete (гарантирует чистый LevelDB)
#    3. Исключает p2p_identity.key из rsync и удаляет остаток (новый генерируется при старте)
#    4. Устанавливает правильные права aperod:aperod
#    5. Включает и запускает aperod-node на новом сервере
#    6. Ждёт готовности API и проверяет peer_count
#
#  Что делает скрипт (bootstrap-режим):
#    1. Читает tip_height валидатора через API
#    2. Устанавливает защитный trap (восстанавливает обе ноды при сбое)
#    3. Останавливает local aperod-node
#    4. Останавливает aperod-node на валидаторе (кратковременно)
#    5. Rsync chain.db + актуальный снимок с валидатора на local
#    6. Перезапускает валидатор
#    7. Очищает p2p_bans.json/p2p_identity.key, устанавливает права
#    8. Прописывает валидатор как bootnode в local node.yaml
#    9. Запускает local aperod-node, снимает trap
#   10. Ждёт готовности API, печатает высоту снимка и tip_height валидатора
# ============================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[ERR]${NC}   $*"; exit 1; }

# ── Параметры ─────────────────────────────────────────────
# PRIMARY_DATA_DIR — путь к данным на ЭТОМ сервере (источнике rsync).
# Переопределите переменной окружения PRIMARY_DATA_DIR если стандартный
# путь не используется (например, в тестах или нестандартных установках).
PRIMARY_DATA_DIR="${PRIMARY_DATA_DIR:-/opt/aperod/data/testnet}"
# SECONDARY_DATA_DIR: overridable via env var so that tests can redirect the
# path to a temp directory without requiring root access or a real SSH target.
# Production default is the canonical path used by install-node.sh.
SECONDARY_DATA_DIR="${SECONDARY_DATA_DIR:-/var/lib/aperod}"
SECONDARY_USER="aperod"
HEALTH_MAX_ATTEMPTS=30
HEALTH_WAIT_SECS=5
PRIMARY_P2P_PORT=30303
# SECONDARY_NODE_YAML / SECONDARY_NODE_CONFIG_SH: overridable via env vars so
# that tests can redirect the paths to temp files without requiring root access
# or a real remote SSH target.  Production defaults are the canonical paths used
# by install-validator.sh / install-node.sh.
SECONDARY_NODE_YAML="${SECONDARY_NODE_YAML:-/etc/aperod/node.yaml}"
SECONDARY_NODE_CONFIG_SH="${SECONDARY_NODE_CONFIG_SH:-/opt/aperod/blockchain/deploy/node-config.sh}"

# ── Аргументы ─────────────────────────────────────────────
BOOTSTRAP_FROM=""
TARGET_IP=""

for _arg in "$@"; do
  case "${_arg}" in
    --bootstrap-from=*)
      BOOTSTRAP_FROM="${_arg#--bootstrap-from=}"
      ;;
    -*)
      die "Неизвестный флаг: ${_arg}
  Использование:
    bash join-network.sh <TARGET_IP>
    bash join-network.sh --bootstrap-from=<VALIDATOR_IP>"
      ;;
    *)
      TARGET_IP="${_arg}"
      ;;
  esac
done

# ══════════════════════════════════════════════════════════════
#  BOOTSTRAP-РЕЖИМ: этот сервер (реле) тянет данные с валидатора
# ══════════════════════════════════════════════════════════════
if [[ -n "${BOOTSTRAP_FROM}" ]]; then
  VALIDATOR_IP="${BOOTSTRAP_FROM}"
  # Data dir on the REMOTE validator — same canonical path as PRIMARY_DATA_DIR.
  # Override with VALIDATOR_DATA_DIR env var if the validator uses a different path.
  VALIDATOR_DATA_DIR="${VALIDATOR_DATA_DIR:-${PRIMARY_DATA_DIR}}"
  # Local data dir and user on THIS relay node.
  LOCAL_DATA_DIR="${SECONDARY_DATA_DIR}"
  LOCAL_USER="${SECONDARY_USER}"
  # Local node.yaml and node-config.sh paths (relay's own config files).
  # Override with LOCAL_NODE_YAML / LOCAL_NODE_CONFIG_SH env vars for tests.
  LOCAL_NODE_YAML="${LOCAL_NODE_YAML:-${SECONDARY_NODE_YAML}}"
  LOCAL_NODE_CONFIG_SH="${LOCAL_NODE_CONFIG_SH:-${SECONDARY_NODE_CONFIG_SH}}"
  # Bootnode multiaddr pointing at the validator.
  VALIDATOR_BOOTNODE="/ip4/${VALIDATOR_IP}/tcp/${PRIMARY_P2P_PORT}"

  # Validate VALIDATOR_IP is a dotted-quad IPv4.
  if ! echo "${VALIDATOR_IP}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
    die "--bootstrap-from='${VALIDATOR_IP}' не является IPv4-адресом."
  fi

  command -v rsync >/dev/null 2>&1 || die "rsync не установлен"
  command -v ssh >/dev/null 2>&1   || die "ssh не установлен"

  echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod — Bootstrap from Validator                   ║
║        Валидатор ${VALIDATOR_IP} → этот сервер
╚════════════════════════════════════════════════════════════╝${NC}
"
  info "Валидатор:        ${VALIDATOR_IP}:${VALIDATOR_DATA_DIR}"
  info "Локальные данные: ${LOCAL_DATA_DIR}"
  echo

  # ── State tracking: which services were stopped ────────────
  # The cleanup trap below checks these before trying to restart.
  # Set to 1 immediately after each stop, cleared to 0 on confirmed restart.
  _BS_LOCAL_STOPPED=0
  _BS_VALIDATOR_STOPPED=0

  # _BS_RSYNC_STARTED tracks whether rsync has begun writing to LOCAL_DATA_DIR.
  # Set to 1 immediately before the first rsync command; reset to 0 after the
  # sentinel is removed (meaning the transfer completed and data is consistent).
  # The cleanup trap uses this to decide whether it is safe to restart the local
  # relay:
  #   0 → rsync never ran (or finished OK and sentinel was removed) → safe to restart
  #   1 → rsync started but did not complete → data may be partially overwritten
  #         → do NOT restart; leave sentinel; require operator action
  _BS_RSYNC_STARTED=0

  # ── Защитный trap (установлен ДО остановки любого сервиса) ─
  # Перезапускает каждый остановленный сервис независимо от того,
  # на каком шаге произошёл сбой. Снимается только после того,
  # как оба сервиса подтверждённо запущены.
  _bootstrap_cleanup() {
    local _exit=$?
    if [[ ${_exit} -ne 0 ]]; then
      warn "[TRAP] Bootstrap завершился с кодом ${_exit} — восстанавливаем ноды…"
      # The source (validator) was stopped for rsync — always restart it
      # so the network keeps producing blocks, regardless of what failed.
      if [[ ${_BS_VALIDATOR_STOPPED} -eq 1 ]]; then
        warn "[TRAP] Перезапускаем aperod-node на валидаторе (${VALIDATOR_IP})…"
        if ssh "root@${VALIDATOR_IP}" "systemctl start aperod-node 2>/dev/null && echo started" 2>/dev/null; then
          ok "[TRAP] aperod-node на валидаторе запущен."
        else
          warn "[TRAP] Не удалось запустить aperod-node на валидаторе — запустите вручную:"
          warn "       ssh root@${VALIDATOR_IP} systemctl start aperod-node"
        fi
      fi
      if [[ ${_BS_LOCAL_STOPPED} -eq 1 ]]; then
        # Always attempt to restart the local node — the .rsync-in-progress
        # sentinel (written before rsync runs) prevents aperod-node from loading
        # a partially-written chain.db, so it is safe to call systemctl start
        # regardless of whether rsync touched any data.
        if [[ ${_BS_RSYNC_STARTED} -eq 1 ]]; then
          warn "[TRAP] rsync был прерван — chain.db может быть в частичном состоянии."
          warn "       Файл-sentinel .rsync-in-progress оставлен в ${LOCAL_DATA_DIR}."
          warn "       Нода не запустится, пока sentinel не будет удалён вручную."
          warn "       Для восстановления:"
          warn "         1. Повторите bootstrap: bash join-network.sh --bootstrap-from=${VALIDATOR_IP}"
          warn "         2. ИЛИ восстановите chain.db из бэкапа, затем:"
          warn "              rm ${LOCAL_DATA_DIR}/.rsync-in-progress"
          warn "              systemctl start aperod-node"
        fi
        warn "[TRAP] Перезапускаем local aperod-node…"
        if systemctl start aperod-node 2>/dev/null; then
          ok "[TRAP] Local aperod-node запущен."
        else
          warn "[TRAP] Не удалось запустить local aperod-node — запустите вручную:"
          warn "       systemctl start aperod-node"
        fi
      fi
    fi
  }
  trap '_bootstrap_cleanup' EXIT ERR

  # ── Шаг 1: Читаем tip_height валидатора через API ─────────
  info "Шаг 1/9: Читаем tip_height валидатора через API…"
  _VAL_STATS=$(ssh "root@${VALIDATOR_IP}" \
    "curl -s --max-time 5 http://127.0.0.1:8545/api/v1/network/stats 2>/dev/null || echo ''"
  ) || true
  VALIDATOR_TIP="unknown"
  if [[ -n "${_VAL_STATS}" ]] && echo "${_VAL_STATS}" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    VALIDATOR_TIP=$(echo "${_VAL_STATS}" | python3 -c \
      "import sys,json; d=json.load(sys.stdin); print(d.get('tip_height', d.get('height', 'unknown')))" 2>/dev/null || echo "unknown")
  fi
  info "  Validator tip_height = ${VALIDATOR_TIP}"

  # ── Шаг 1b: Проверяем наличие данных валидатора ─────────
  # MUST run BEFORE stopping any service: if the validator uses a non-standard
  # data dir the rsync would silently copy nothing, and the relay would ban the
  # validator on first connect.  Catching the mismatch here — while both nodes
  # are still running — lets the operator fix VALIDATOR_DATA_DIR without
  # triggering the cleanup trap.
  info "Шаг 1b/9: Проверяем наличие ${VALIDATOR_DATA_DIR} на валидаторе (${VALIDATOR_IP})…"
  if ! ssh "root@${VALIDATOR_IP}" true 2>/dev/null; then
    die "SSH-соединение с валидатором ${VALIDATOR_IP} недоступно.
  Проверьте сетевую доступность, SSH-сервис и авторизацию, затем повторите bootstrap."
  fi
  if ! ssh "root@${VALIDATOR_IP}" "[ -d '${VALIDATOR_DATA_DIR}' ]" 2>/dev/null; then
    die "Директория данных валидатора не найдена: ${VALIDATOR_DATA_DIR}
  Если валидатор установлен по нестандартному пути, переопределите переменную:
    VALIDATOR_DATA_DIR=/путь/к/данным bash join-network.sh --bootstrap-from=${VALIDATOR_IP}"
  fi
  if ! ssh "root@${VALIDATOR_IP}" "[ -d '${VALIDATOR_DATA_DIR}/chain.db' ]" 2>/dev/null; then
    die "Поддиректория chain.db не найдена внутри ${VALIDATOR_DATA_DIR} на валидаторе.
  Убедитесь что aperod-node на валидаторе был запущен хотя бы раз, или переопределите путь:
    VALIDATOR_DATA_DIR=/путь/к/данным bash join-network.sh --bootstrap-from=${VALIDATOR_IP}"
  fi
  ok "VALIDATOR_DATA_DIR и chain.db подтверждены на валидаторе"

  # ── Шаг 2: Останавливаем local aperod-node ────────────────
  info "Шаг 2/9: Останавливаем local aperod-node…"
  systemctl stop aperod-node 2>/dev/null || true
  for _i in $(seq 1 15); do
    systemctl is-active --quiet aperod-node || break
    sleep 1
  done
  if systemctl is-active --quiet aperod-node; then
    die "Local aperod-node не остановился за 15 с. Прерываем."
  fi
  _BS_LOCAL_STOPPED=1
  ok "Local aperod-node остановлен"

  # ── Шаг 3: Останавливаем aperod-node на ВАЛИДАТОРЕ ────────
  # LevelDB небезопасно копировать на ходу (WAL-записи / компакция .ldb).
  info "Шаг 3/9: Останавливаем aperod-node на валидаторе (${VALIDATOR_IP})…"
  info "  ⚠ Кратковременный простой ~60 с — LevelDB нельзя копировать на ходу"
  # Set the flag BEFORE sending the SSH command so that an SSH disconnect
  # after the remote stop fires (but before the heredoc returns) still causes
  # the trap to attempt a restart, not silently leave the validator down.
  _BS_VALIDATOR_STOPPED=1
  # The heredoc below is single-quoted (no local expansion), so the locally
  # resolved VALIDATOR_DATA_DIR must be forwarded explicitly: a printf prologue
  # (with %q quoting) is prepended to the stdin stream so the remote shell sees
  # the exact directory that rsync will copy — not a hardcoded default.
  {
    printf 'VALIDATOR_DATA_DIR=%q\n' "${VALIDATOR_DATA_DIR}"
    cat <<'REMOTE_STOP'
    if ! systemctl stop aperod-node 2>/dev/null; then
      echo "[ERR] Не удалось остановить aperod-node на валидаторе" >&2
      exit 1
    fi
    for _i in $(seq 1 15); do
      systemctl is-active --quiet aperod-node || break
      sleep 1
    done
    if systemctl is-active --quiet aperod-node; then
      echo "[ERR] aperod-node на валидаторе не остановился за 15 с" >&2
      exit 1
    fi
    # systemd is down, but chain.db may still be held open by a non-systemd
    # process (manually launched node, stuck Go test).  Rsync of an open
    # LevelDB produces a corrupt copy — check fuser/lsof before proceeding.
    # VALIDATOR_DATA_DIR is injected by the printf prologue above and matches
    # the exact directory the subsequent rsync will copy.
    _CHAIN_DB_DIR="${VALIDATOR_DATA_DIR}/chain.db"
    _DB_BUSY=0
    if [ -d "${_CHAIN_DB_DIR}" ]; then
      # FAIL CLOSED: without a trustworthy inspection result we must not rsync.
      if command -v fuser >/dev/null 2>&1; then
        _RC=0; fuser -s "${_CHAIN_DB_DIR}"/* 2>/dev/null || _RC=$?
        case "${_RC}" in
          0) _DB_BUSY=1 ;;   # at least one process holds a file open
          1) : ;;            # documented "no process found" — DB free
          *)
            echo "[ERR] fuser на валидаторе завершился с неожиданным кодом ${_RC} — результату нельзя доверять, rsync прерван (fail closed)" >&2
            exit 1 ;;
        esac
      elif command -v lsof >/dev/null 2>&1; then
        _RC=0; lsof +D "${_CHAIN_DB_DIR}" >/dev/null 2>&1 || _RC=$?
        case "${_RC}" in
          0) _DB_BUSY=1 ;;   # at least one process holds a file open
          1) : ;;            # documented "no open files found" — DB free
          *)
            echo "[ERR] lsof на валидаторе завершился с неожиданным кодом ${_RC} — результату нельзя доверять, rsync прерван (fail closed)" >&2
            exit 1 ;;
        esac
      else
        echo "[ERR] Ни fuser, ни lsof не найдены на валидаторе — невозможно проверить, что LevelDB не открыт другим процессом" >&2
        echo "[ERR] rsync без этой проверки может создать повреждённую копию — прерываем ДО rsync (fail closed)" >&2
        echo "[ERR] Установите на валидаторе: apt-get install -y psmisc   # fuser (или lsof)" >&2
        exit 1
      fi
    fi
    if [ "${_DB_BUSY}" -eq 1 ]; then
      echo "[ERR] chain.db (${_CHAIN_DB_DIR}) на валидаторе всё ещё открыт другим процессом — rsync прерван" >&2
      exit 1
    fi
    echo "stopped"
REMOTE_STOP
  } | ssh "root@${VALIDATOR_IP}" bash
  ok "aperod-node на валидаторе остановлен"

  # ── Шаг 3.5: Записываем sentinel непосредственно перед rsync ──
  # Written here — after both nodes are stopped and immediately before any
  # data is transferred — so the flag accurately reflects "rsync has begun".
  # aperod-node refuses to start when this file is present, protecting against
  # watchdog or stale systemd timer restarts during the transfer.
  # Removed explicitly in step 4c after both rsync operations succeed.
  # NOT removed by the cleanup trap on failure: if rsync started but failed,
  # the data dir may be partially overwritten and the relay must stay blocked
  # until the operator re-runs bootstrap or restores from a known-good backup.
  _BS_SENTINEL="${LOCAL_DATA_DIR}/.rsync-in-progress"
  mkdir -p "${LOCAL_DATA_DIR}"
  touch "${_BS_SENTINEL}"
  # Set the rsync-started flag AFTER writing the sentinel.
  # From this point any failure leaves data in an unknown partial state.
  _BS_RSYNC_STARTED=1
  info "Шаг 3.5/9: Sentinel .rsync-in-progress записан — rsync начинается"

  # ── Шаг 4: Rsync chain.db и снимков с валидатора ──────────
  info "Шаг 4/9: Rsync chain.db с валидатора (--delete)…"
  info "  Это может занять несколько минут (~1-2 ГБ)"
  mkdir -p "${LOCAL_DATA_DIR}/chain.db"
  rsync -az --delete --progress --ignore-errors \
    "root@${VALIDATOR_IP}:${VALIDATOR_DATA_DIR}/chain.db/" \
    "${LOCAL_DATA_DIR}/chain.db/"
  ok "chain.db синхронизирован"

  info "Шаг 4b/9: Rsync снимков (snapshot-v2-*.json.gz) с валидатора…"
  # Remove old local snapshots first so no stale heights confuse the node.
  rm -f "${LOCAL_DATA_DIR}"/snapshot-v*.json.gz \
        "${LOCAL_DATA_DIR}"/snapshot-v*.json \
        "${LOCAL_DATA_DIR}"/snapshot-v*.json.gz.tmp 2>/dev/null || true
  rsync -az --progress --ignore-errors \
    --include='snapshot-v2-*.json.gz' \
    --include='snapshot-v2-*-prev.json.gz' \
    --exclude='*' \
    "root@${VALIDATOR_IP}:${VALIDATOR_DATA_DIR}/" \
    "${LOCAL_DATA_DIR}/"
  ok "Снимки синхронизированы"

  # ── Шаг 4c/9: Снимаем sentinel ────────────────────────────
  # Both chain.db and snapshots were transferred successfully — the node may
  # now start safely.  Remove the sentinel that blocked premature starts.
  rm -f "${_BS_SENTINEL}" 2>/dev/null || true
  # Reset the rsync-started flag: data is now consistent; if any later step
  # fails, the cleanup trap may safely restart the local relay.
  _BS_RSYNC_STARTED=0
  info "Шаг 4c/9: Sentinel .rsync-in-progress удалён (rsync завершён успешно)"

  # Determine the snapshot height from the copied file name.
  SNAP_HEIGHT=$(ls "${LOCAL_DATA_DIR}"/snapshot-v2-*.json.gz 2>/dev/null \
    | sed 's/.*snapshot-v2-\([0-9]*\)\.json\.gz$/\1/' \
    | grep -E '^[0-9]+$' | sort -n | tail -1 || echo "unknown")
  info "  Скопирован снимок высоты: ${SNAP_HEIGHT}"

  # ── Шаг 5: Перезапускаем валидатор ───────────────────────
  # FATAL: if the validator cannot be restarted we must not proceed.
  # Continuing would remove the cleanup trap while _BS_VALIDATOR_STOPPED=1,
  # leaving the validator silently down for operators to discover later.
  # Exiting here causes the trap to fire and make a recovery attempt.
  info "Шаг 5/9: Перезапускаем aperod-node на валидаторе…"
  if ! ssh "root@${VALIDATOR_IP}" "systemctl start aperod-node 2>/dev/null && echo started"; then
    die "Не удалось перезапустить aperod-node на валидаторе. \
Trap попытается перезапустить обе ноды — проверьте состояние вручную после завершения."
  fi
  _BS_VALIDATOR_STOPPED=0
  ok "aperod-node на валидаторе запущен"

  # ── Шаг 5b/9: Sanity check — считаем пропущенные блоки в rsync'd chain.db ──
  # An rsync of a live LevelDB (when the source was not stopped) leaves WAL
  # writes unflushed, producing gaps of tens-of-thousands of missing heights.
  # The startup scan tolerates up to 5 000 by default; gaps beyond that cause
  # a fatal "too many missing blocks" crash-loop.  Running aperod-node
  # --check-store here catches the problem before the relay ever starts, while
  # both nodes are already running and the operator can re-run bootstrap.
  info "Шаг 5b/9: Проверяем chain.db на пропущенные блоки (порог: 5000)…"
  if command -v aperod-node >/dev/null 2>&1; then
    _CHECK_OUT=$(aperod-node --check-store \
      --data-dir="${LOCAL_DATA_DIR}" \
      --max-missing=5000 2>&1) || {
      # Print the check-store error so operators can see the gap count.
      echo "${_CHECK_OUT}" >&2
      die "Sanity check: chain.db содержит слишком много пропущенных блоков.
  Вероятная причина: rsync был выполнен с работающего LevelDB (источник не был остановлен).
  Решение: перезапустите bootstrap — скрипт сам остановит источник перед rsync:
    bash join-network.sh --bootstrap-from=${VALIDATOR_IP}"
    }
    ok "chain.db прошёл проверку (${_CHECK_OUT})"
  else
    warn "aperod-node не найден в PATH — проверка chain.db пропущена"
    warn "  Установите aperod-node перед bootstrap для автоматической проверки целостности"
  fi

  # ── Шаг 6: Очищаем баны, удаляем identity, устанавливаем права ──
  info "Шаг 6/9: Очищаем p2p_bans.json и устанавливаем права…"
  rm -f "${LOCAL_DATA_DIR}/p2p_bans.json"
  rm -f "${LOCAL_DATA_DIR}/p2p_identity.key"
  chown -R "${LOCAL_USER}:${LOCAL_USER}" "${LOCAL_DATA_DIR}/"
  ok "p2p_bans.json очищен, p2p_identity.key удалён (новый создастся при старте)"

  # ── Шаг 7: Прописываем валидатор как bootnode в local node.yaml ──
  # Без этого шага реле не инициирует P2P-подключение к валидатору
  # и никогда не синхронизируется (peer_count=0 навсегда).
  info "Шаг 7/9: Прописываем bootnode ${VALIDATOR_BOOTNODE} в ${LOCAL_NODE_YAML}…"
  if [[ -f "${LOCAL_NODE_YAML}" ]]; then
    if [[ -x "${LOCAL_NODE_CONFIG_SH}" ]]; then
      APEROD_CONFIG="${LOCAL_NODE_YAML}" bash "${LOCAL_NODE_CONFIG_SH}" add-bootnode "${VALIDATOR_BOOTNODE}"
    else
      # Fallback: python3 — same YAML-safe migration logic as push-mode step 5.
      python3 - "${LOCAL_NODE_YAML}" "${VALIDATOR_BOOTNODE}" <<'PY'
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes.
legacy = cfg.pop("bootnodes", None)

p2p = cfg.setdefault("p2p", {})
nodes = list(p2p.get("bootnodes") or [])

if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in nodes:
            nodes.append(entry)

if bootnode not in nodes:
    nodes.append(bootnode)
p2p["bootnodes"] = nodes

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
print(f"[OK]   p2p.bootnodes updated: {nodes}")
PY
    fi
    ok "Bootnode ${VALIDATOR_BOOTNODE} прописан в ${LOCAL_NODE_YAML}"

    # ── Прописываем IP валидатора в peer_whitelist ────────────────────────
    # Предотвращает накопление bad-block страйков (→ 24-ч бан) пока реле
    # не догонит цепь и не начнёт принимать gossip-блоки валидатора.
    info "Шаг 7.5/9: Прописываем ${VALIDATOR_IP} в p2p.peer_whitelist…"
    python3 - "${LOCAL_NODE_YAML}" "${VALIDATOR_IP}" <<'PY'
import sys, yaml, os
cfg_path     = sys.argv[1]
validator_ip = sys.argv[2]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}
p2p = cfg.setdefault("p2p", {})
wl  = list(p2p.get("peer_whitelist") or [])
if validator_ip not in wl:
    wl.append(validator_ip)
    p2p["peer_whitelist"] = wl
    tmp = cfg_path + ".tmp"
    with open(tmp, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    os.replace(tmp, cfg_path)
    print(f"[OK]   p2p.peer_whitelist updated: {wl}")
else:
    print(f"[OK]   p2p.peer_whitelist already contains {validator_ip} — skipping")
PY
    ok "IP валидатора ${VALIDATOR_IP} добавлен в peer_whitelist"
  else
    warn "${LOCAL_NODE_YAML} не найден — bootnode не прописан."
    warn "Добавьте вручную в node.yaml:"
    warn "  p2p:"
    warn "    bootnodes:"
    warn "      - ${VALIDATOR_BOOTNODE}"
  fi

  # ── Шаг 7b: Устанавливаем snapshot.utxo_count_tolerance_pct ──
  # Relay nodes bootstrapped via rsync get a snapshot whose stored UTXO count
  # may differ from the on-disk DB count by more than the default 1% tolerance,
  # causing the node to silently skip RestoreFromSnapshot() and start with an
  # all-zeros validator registry (→ every block rejected as "not the scheduled
  # proposer").  10% gives enough headroom for any post-rsync drift without
  # compromising integrity.
  info "Шаг 7b/9: Устанавливаем snapshot.utxo_count_tolerance_pct=10 для реле…"
  if [[ -f "${LOCAL_NODE_YAML}" ]]; then
    if [[ -x "${LOCAL_NODE_CONFIG_SH}" ]]; then
      APEROD_CONFIG="${LOCAL_NODE_YAML}" bash "${LOCAL_NODE_CONFIG_SH}" set-snapshot-tolerance 10
    else
      # Fallback: stdlib-only Python (no PyYAML dependency).
      # Uses regex to set/update snapshot.utxo_count_tolerance_pct in node.yaml
      # without parsing full YAML — safe for this targeted single-key update.
      python3 - "${LOCAL_NODE_YAML}" <<'PY'
import sys, os, re

cfg_path = sys.argv[1]
with open(cfg_path) as f:
    content = f.read()

m = re.search(r'^(\s*utxo_count_tolerance_pct\s*:\s*)(\d+(?:\.\d+)?)', content, re.M)
if m:
    current = float(m.group(2))
    if current >= 10:
        print(f"[OK]   snapshot.utxo_count_tolerance_pct already {current} (kept)")
        sys.exit(0)
    content = content[:m.start(2)] + "10" + content[m.end(2):]
    changed_msg = f"[OK]   snapshot.utxo_count_tolerance_pct {current} -> 10"
elif re.search(r'^snapshot\s*:', content, re.M):
    content = re.sub(
        r'^(snapshot\s*:[ \t]*\n)',
        r'\1  utxo_count_tolerance_pct: 10\n',
        content, count=1, flags=re.M
    )
    changed_msg = "[OK]   snapshot.utxo_count_tolerance_pct set to 10"
else:
    if not content.endswith('\n'):
        content += '\n'
    content += '\nsnapshot:\n  utxo_count_tolerance_pct: 10\n'
    changed_msg = "[OK]   snapshot.utxo_count_tolerance_pct set to 10"

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    f.write(content)
os.replace(tmp, cfg_path)
print(changed_msg)
PY
    fi
    ok "snapshot.utxo_count_tolerance_pct настроен"
  else
    warn "${LOCAL_NODE_YAML} не найден — добавьте вручную:"
    warn "  snapshot:"
    warn "    utxo_count_tolerance_pct: 10"
  fi

  # ── Шаг 8: drop-in конфиги и запуск local aperod-node ────
  info "Шаг 8/9: Устанавливаем drop-in конфиги и запускаем local aperod-node…"
  if [[ -x /opt/aperod/blockchain/deploy/ensure-dropin.sh ]]; then
    bash /opt/aperod/blockchain/deploy/ensure-dropin.sh
  fi
  # IMPORTANT: _BS_LOCAL_STOPPED and the cleanup trap are intentionally NOT
  # cleared before this command.  If `systemctl enable --now aperod-node` fails
  # (e.g. unit file not installed), the ERR/EXIT trap will still fire with
  # _BS_LOCAL_STOPPED=1 and attempt to restart the local node via
  # `systemctl start aperod-node`.  Clearing those flags first would silence
  # the trap and leave the relay stopped with no recovery.
  if ! systemctl enable --now aperod-node; then
    die "Шаг 8: systemctl enable --now aperod-node завершился с ошибкой.
  Возможные причины:
    • unit-файл aperod-node.service не установлен (запустите install-node.sh)
    • Ошибка прав доступа (запустите скрипт от root / sudo)
  Trap попытается перезапустить local aperod-node — проверьте состояние:
    systemctl status aperod-node"
  fi
  _BS_LOCAL_STOPPED=0
  # Both services are now up (or we've warned about the validator).
  # Снимаем trap: неожиданный выход после этой точки не требует восстановления нод.
  trap - EXIT ERR
  ok "Local aperod-node запущен"

  # Issue a final reminder if validator restart had failed earlier.
  if [[ ${_BS_VALIDATOR_STOPPED} -eq 1 ]]; then
    warn "aperod-node на валидаторе мог не запуститься — проверьте:"
    warn "  ssh root@${VALIDATOR_IP} systemctl status aperod-node"
  fi

  # ── Шаг 9: Ожидаем готовности local API ──────────────────
  info "Шаг 9/9: Ожидаем готовности API (key-image rebuild, ~5 мин)…"
  ATTEMPT=0
  LOCAL_HEIGHT=0
  LOCAL_PEERS=0
  _API_READY=0
  while [[ ${ATTEMPT} -lt ${HEALTH_MAX_ATTEMPTS} ]]; do
    ATTEMPT=$((ATTEMPT + 1))
    _STATS=$(curl -s --max-time 3 "http://127.0.0.1:8545/api/v1/network/stats" 2>/dev/null || echo "")
    if [[ -n "${_STATS}" ]] && echo "${_STATS}" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
      LOCAL_HEIGHT=$(echo "${_STATS}" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('height',0))")
      LOCAL_PEERS=$(echo "${_STATS}"  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('peer_count',0))")
      echo -e "  Попытка ${ATTEMPT}/${HEALTH_MAX_ATTEMPTS}: height=${LOCAL_HEIGHT} peers=${LOCAL_PEERS}"
      if [[ "${LOCAL_HEIGHT}" -gt 0 ]]; then
        _API_READY=1
        break
      fi
    else
      echo -e "  Попытка ${ATTEMPT}/${HEALTH_MAX_ATTEMPTS}: API ещё не готов, ожидаем…"
    fi
    sleep ${HEALTH_WAIT_SECS}
  done

  if [[ ${_API_READY} -eq 0 ]]; then
    warn "Таймаут ожидания API. Проверьте логи: journalctl -u aperod-node -n 50 --no-pager"
    exit 1
  fi

  # ── Итог ──────────────────────────────────────────────────
  echo
  echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════╗"
  echo -e "  ✓  Bootstrap завершён"
  echo -e "     Снимок высоты:       ${SNAP_HEIGHT}"
  echo -e "     Validator tip_height: ${VALIDATOR_TIP}"
  echo -e "     Local height:         ${LOCAL_HEIGHT}  |  Peers: ${LOCAL_PEERS}"
  echo -e "╚══════════════════════════════════════════════════════════════╝${NC}"
  echo

  if [[ "${LOCAL_PEERS}" -eq 0 ]]; then
    warn "Peers = 0 — нода загружается, пиры появятся после key-image rebuild (~5 мин)"
    warn "Повторите проверку: curl -s http://127.0.0.1:8545/api/v1/network/stats"
  else
    ok "Peers = ${LOCAL_PEERS} — сеть работает!"
  fi

  echo
  info "Реле подключено. Следующие шаги (опционально):"
  echo "  1. Убедитесь что порт 30303/tcp открыт (firewall)"
  echo "  2. Через ~5 мин peers > 0 и высота начнёт расти"
  echo
  exit 0
fi

# ══════════════════════════════════════════════════════════════
#  PUSH-РЕЖИМ: запуск на основном узле, данные идут на TARGET
# ══════════════════════════════════════════════════════════════
[[ -z "${TARGET_IP}" ]] && die "Укажите IP нового сервера: bash join-network.sh <IP>
Или используйте: bash join-network.sh --bootstrap-from=<VALIDATOR_IP>"

# PRIMARY_IP — IP-адрес ЭТОГО сервера (откуда запускается скрипт).
# Переопределите переменной окружения PRIMARY_IP если auto-detect даёт
# неверный адрес (например, внутренний 10.x вместо внешнего IP).
if [[ -z "${PRIMARY_IP:-}" ]]; then
  # Explicitly pick the first IPv4 address (skip IPv6 addresses).
  # hostname -I can return IPv6 first on dual-stack hosts, but /ip4/.../tcp/...
  # multiaddrs require a dotted-quad IPv4 address.
  PRIMARY_IP=$(hostname -I 2>/dev/null | tr ' ' '\n' | grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$' | head -1)
  if [[ -z "${PRIMARY_IP}" ]]; then
    die "Не удалось определить IPv4-адрес этого сервера.
  Укажите вручную: PRIMARY_IP=<IPv4> bash join-network.sh <TARGET_IP>"
  fi
fi
# Validate that PRIMARY_IP is a dotted-quad to catch accidental IPv6 overrides.
if ! echo "${PRIMARY_IP}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
  die "PRIMARY_IP='${PRIMARY_IP}' не является IPv4-адресом.
  /ip4/.../tcp/... multiaddr требует dotted-quad IPv4. Укажите IPv4: PRIMARY_IP=<IPv4> bash join-network.sh <TARGET_IP>"
fi
PRIMARY_BOOTNODE="/ip4/${PRIMARY_IP}/tcp/${PRIMARY_P2P_PORT}"

# ── Проверки ──────────────────────────────────────────────
[[ -d "${PRIMARY_DATA_DIR}" ]] || die "Директория данных не найдена: ${PRIMARY_DATA_DIR}"
[[ -d "${PRIMARY_DATA_DIR}/chain.db" ]] || die "Поддиректория chain.db не найдена: ${PRIMARY_DATA_DIR}/chain.db
  Проверьте путь к данным валидатора или укажите его явно:
  PRIMARY_DATA_DIR=/путь/к/данным bash join-network.sh ${TARGET_IP}"
command -v rsync >/dev/null 2>&1 || die "rsync не установлен"
command -v ssh >/dev/null 2>&1   || die "ssh не установлен"

echo -e "
${BOLD}╔════════════════════════════════════════════════════════════╗
║        Aperod — Join Network Script                        ║
║        Основной узел → ${TARGET_IP}
╚════════════════════════════════════════════════════════════╝${NC}
"

info "Целевой сервер:   ${TARGET_IP}"
info "Источник данных:  ${PRIMARY_DATA_DIR}"
info "Назначение:       ${TARGET_IP}:${SECONDARY_DATA_DIR}"
info "Bootnode:         ${PRIMARY_BOOTNODE}"
echo

# ── State tracking: which services were stopped by this script ─────────────
# Flags are set immediately after each stop command so that the EXIT/ERR trap
# can restart whichever services are down, regardless of which step failed.
# Cleared to 0 after each confirmed restart so the trap is idempotent.
_TARGET_STOPPED=0
_SOURCE_STOPPED=0

# _PUSH_RSYNC_STARTED tracks whether rsync has begun writing to the target.
# Set to 1 immediately before the rsync command (after the sentinel is written);
# reset to 0 after the sentinel is removed (transfer complete, data consistent).
# The cleanup trap uses this flag to decide whether the target may be restarted:
#   0 → rsync never ran, OR finished OK and sentinel removed → safe to restart
#   1 → rsync started but did not complete → partial data; DO NOT restart;
#         leave sentinel; operator must re-run join-network.sh or restore backup
_PUSH_RSYNC_STARTED=0

# ── Защитный trap: гарантирует запуск ОБОИХ узлов при любом сбое ──────────
# Installed BEFORE Step 1 so it fires even if the script is interrupted
# immediately after the target node is stopped.
# Снимается после успешного явного systemctl start ниже (trap - EXIT ERR).
_push_cleanup() {
  local _exit=$?
  if [[ ${_exit} -ne 0 ]]; then
    warn "[TRAP] Скрипт завершился с кодом ${_exit} — восстанавливаем ноды…"
    # The source (this server) was stopped for rsync — always restart it so
    # the network keeps producing blocks regardless of what failed.
    if [[ ${_SOURCE_STOPPED} -eq 1 ]]; then
      warn "[TRAP] Перезапускаем aperod-node на ИСТОЧНИКЕ (этот сервер)…"
      if systemctl start aperod-node 2>/dev/null; then
        ok "[TRAP] aperod-node на источнике запущен. Проверьте состояние сети вручную."
      else
        warn "[TRAP] Не удалось запустить aperod-node через trap — запустите вручную:"
        warn "       systemctl start aperod-node"
      fi
    fi
    if [[ ${_TARGET_STOPPED} -eq 1 ]]; then
      if [[ ${_PUSH_RSYNC_STARTED} -eq 0 ]]; then
        # rsync never ran (failure before step 3) OR completed OK and the
        # sentinel was already removed (failure in a later config step).
        # Target chain.db is untouched or consistent — safe to restart.
        warn "[TRAP] Перезапускаем aperod-node на ЦЕЛЕВОМ сервере (${TARGET_IP})…"
        if ssh "root@${TARGET_IP}" "systemctl start aperod-node 2>/dev/null && echo started" 2>/dev/null; then
          ok "[TRAP] aperod-node на целевом сервере (${TARGET_IP}) запущен."
        else
          warn "[TRAP] Не удалось запустить aperod-node на целевом сервере по SSH."
          echo -e "${RED}${BOLD}"
          echo    "  ╔══════════════════════════════════════════════════════════════╗"
          echo    "  ║  ⚠  ТРЕБУЕТСЯ РУЧНОЕ ВОССТАНОВЛЕНИЕ ЦЕЛЕВОЙ НОДЫ           ║"
          echo    "  ║                                                              ║"
          echo    "  ║  SSH-соединение с ${TARGET_IP} потеряно.                    "
          echo    "  ║  Когда сеть восстановится, выполните на ЭТОМ сервере:       ║"
          echo    "  ║                                                              ║"
          printf  "  ║    ssh root@%s systemctl start aperod-node\n" "${TARGET_IP}"
          echo    "  ║                                                              ║"
          echo    "  ║  Или запустите готовый скрипт:                              ║"
          echo    "  ║    bash /tmp/aperod-join-recovery.sh                        ║"
          echo    "  ╚══════════════════════════════════════════════════════════════╝"
          echo -e "${NC}"
          # Write a ready-to-run recovery script so the operator can paste one
          # command once connectivity is restored — no need to remember the exact
          # syntax under pressure.
          local _recovery_script="/tmp/aperod-join-recovery.sh"
          cat >"${_recovery_script}" <<RECOVERY_SH
#!/usr/bin/env bash
# Aperod join-network.sh — recovery script
# Generated automatically because the SSH restart of the target node failed.
# Run this script from the SOURCE server once network connectivity to the
# target (${TARGET_IP}) is restored.
set -euo pipefail
TARGET_IP="${TARGET_IP}"
echo "Attempting to start aperod-node on \${TARGET_IP}..."
if ssh "root@\${TARGET_IP}" "systemctl start aperod-node 2>/dev/null && echo started"; then
  echo "[OK] aperod-node started on \${TARGET_IP}."
  echo "     Verify: ssh root@\${TARGET_IP} systemctl status aperod-node"
else
  echo "[ERR] Still cannot reach \${TARGET_IP} via SSH."
  echo "      Connect to the target server directly and run:"
  echo "        systemctl start aperod-node"
  exit 1
fi
RECOVERY_SH
          chmod +x "${_recovery_script}"
          warn "[TRAP] Скрипт восстановления записан: ${_recovery_script}"
          warn "       Запустите его как только SSH-доступ к ${TARGET_IP} будет восстановлен:"
          warn "       bash ${_recovery_script}"
        fi
      else
        # rsync started but did not complete — data may be partially written.
        # Do NOT restart the target; leave the sentinel so aperod-node cannot
        # start accidentally (e.g. via watchdog or stale systemd timer).
        warn "[TRAP] Целевая нода (${TARGET_IP}) НЕ запускается автоматически."
        warn "       rsync был прерван — chain.db может быть в частичном состоянии."
        warn "       Файл-sentinel .rsync-in-progress оставлен на ${TARGET_IP}:${SECONDARY_DATA_DIR}."
        warn "       Для восстановления:"
        warn "         1. Повторите: bash join-network.sh ${TARGET_IP}"
        warn "         2. ИЛИ восстановите chain.db из бэкапа, затем:"
        warn "              ssh root@${TARGET_IP} \"rm ${SECONDARY_DATA_DIR}/.rsync-in-progress\""
        warn "              ssh root@${TARGET_IP} systemctl start aperod-node"
      fi
    fi
  fi
}
trap '_push_cleanup' EXIT ERR

# ── Шаг 1: Останавливаем ноду на целевом сервере ──────────
info "Шаг 1/7: Останавливаем и отключаем aperod-node на ${TARGET_IP}…"
# Set the flag BEFORE sending the SSH command so that an SSH disconnect
# after the remote stop fires (but before the command returns) still causes
# the trap to attempt a restart, not silently leave the target node down.
_TARGET_STOPPED=1
ssh "root@${TARGET_IP}" "systemctl disable --now aperod-node 2>/dev/null; echo 'stopped'" || \
  ssh "root@${TARGET_IP}" "systemctl stop aperod-node 2>/dev/null || true; echo 'stopped'"
ok "Нода остановлена (systemd auto-restart отключён)"

# ── Шаг 2: Останавливаем ноду на ИСТОЧНИКЕ перед rsync ───
# LevelDB небезопасно копировать на ходу: WAL-записи и
# компакция .ldb создают внутренне несогласованную копию,
# которая восстанавливается на меньшей высоте и расходится
# с основной цепью. Остановка занимает ~60 секунд.
info "Шаг 2/7: Останавливаем aperod-node на ИСТОЧНИКЕ (этот сервер)…"
info "  ⚠ Кратковременный простой ~60 с — LevelDB нельзя копировать на ходу"
if ! systemctl stop aperod-node 2>/dev/null; then
  die "Не удалось остановить aperod-node на источнике. Прерываем — \
rsync живой LevelDB приведёт к повреждению данных на целевом сервере."
fi
# Убеждаемся что сервис действительно остановлен
for _i in $(seq 1 15); do
  systemctl is-active --quiet aperod-node || break
  sleep 1
done
if systemctl is-active --quiet aperod-node; then
  systemctl start aperod-node 2>/dev/null || true
  die "aperod-node на источнике не остановился за 15 с. Прерываем."
fi
_SOURCE_STOPPED=1
ok "aperod-node на источнике остановлен"

# ── Шаг 2a: Проверяем что chain.db не открыт ДРУГИМ процессом ──
# systemctl is-active only covers the systemd-managed service.  A manually
# launched aperod-node, a stuck Go test, or any other process may still hold
# the LevelDB open — rsync of an open LevelDB produces a corrupt copy exactly
# like copying a running service would.  fuser/lsof catches those cases.
info "Шаг 2a/7: Проверяем что LevelDB (${PRIMARY_DATA_DIR}/chain.db) не открыт другим процессом…"
_CHAIN_DB_DIR="${PRIMARY_DATA_DIR}/chain.db"
_DB_BUSY=0
if [[ -d "${_CHAIN_DB_DIR}" ]]; then
  # FAIL CLOSED: without an inspection tool we cannot prove the DB is free,
  # so we must not rsync.  The trap restarts the source (_SOURCE_STOPPED=1).
  if command -v fuser >/dev/null 2>&1; then
    _RC=0; fuser -s "${_CHAIN_DB_DIR}"/* 2>/dev/null || _RC=$?
    case "${_RC}" in
      0) _DB_BUSY=1 ;;   # at least one process holds a file open
      1) : ;;            # documented "no process found" result — DB free
      *) die "fuser завершился с неожиданным кодом ${_RC} — результат проверки нельзя доверять.
  rsync живого LevelDB может повредить копию — прерываем ДО rsync (fail closed).
  Проверьте вручную: fuser -v ${_CHAIN_DB_DIR}/*  и повторите: bash join-network.sh ${TARGET_IP}" ;;
    esac
  elif command -v lsof >/dev/null 2>&1; then
    _RC=0; lsof +D "${_CHAIN_DB_DIR}" >/dev/null 2>&1 || _RC=$?
    case "${_RC}" in
      0) _DB_BUSY=1 ;;   # at least one process holds a file open
      1) : ;;            # documented "no open files found" result — DB free
      *) die "lsof завершился с неожиданным кодом ${_RC} — результат проверки нельзя доверять.
  rsync живого LevelDB может повредить копию — прерываем ДО rsync (fail closed).
  Проверьте вручную: lsof +D ${_CHAIN_DB_DIR}  и повторите: bash join-network.sh ${TARGET_IP}" ;;
    esac
  else
    die "Ни fuser, ни lsof не найдены — невозможно проверить, что LevelDB не открыт другим процессом.
  rsync без этой проверки может создать повреждённую копию — прерываем ДО rsync (fail closed).
  Установите инструмент и повторите:
    apt-get install -y psmisc   # fuser
    # или: apt-get install -y lsof
  Затем: bash join-network.sh ${TARGET_IP}"
  fi
fi
if [[ ${_DB_BUSY} -eq 1 ]]; then
  # The EXIT/ERR trap will restart the source service (_SOURCE_STOPPED=1);
  # the foreign process holding the DB must be dealt with by the operator.
  die "chain.db всё ещё открыт другим процессом (не systemd-сервисом).
  rsync живого LevelDB приведёт к повреждению копии — прерываем ДО rsync.
  Найдите и остановите процесс:
    fuser -v ${_CHAIN_DB_DIR}/*    # или: lsof +D ${_CHAIN_DB_DIR}
  Затем повторите: bash join-network.sh ${TARGET_IP}"
fi
ok "chain.db не открыт ни одним процессом — rsync безопасен"

# ── Шаг 2b: Записываем sentinel на целевом сервере ────────
# The sentinel .rsync-in-progress tells aperod-node to refuse startup until
# it is removed.  This prevents a watchdog or stale systemd timer from
# starting the node against a half-written LevelDB while rsync is in flight.
# The sentinel is removed after a successful rsync (step 3b) and also by the
# cleanup trap on any error path.
info "Шаг 2b/7: Записываем sentinel .rsync-in-progress на ${TARGET_IP}:${SECONDARY_DATA_DIR}…"
ssh "root@${TARGET_IP}" "mkdir -p '${SECONDARY_DATA_DIR}' && touch '${SECONDARY_DATA_DIR}/.rsync-in-progress' && echo 'sentinel written'"
ok "Sentinel записан (нода заблокирована от преждевременного старта)"

# ── Шаг 3: Rsync данных с --delete ────────────────────────
info "Шаг 3/7: Синхронизируем цепь (rsync --delete)…"
info "  Это может занять несколько минут (~1-2 ГБ)"
# Set _PUSH_RSYNC_STARTED=1 immediately before rsync begins.
# From this point any failure means target data may be partially overwritten.
# The cleanup trap will leave the sentinel in place and NOT restart the target.
_PUSH_RSYNC_STARTED=1
# IMPORTANT: .rsync-in-progress must be excluded from --delete.
# The source does not have this file, so without the exclusion rsync's
# deletion pass would remove the sentinel from the target BEFORE the data
# transfer completes — defeating its purpose.
rsync -az --delete --progress --ignore-errors \
  --exclude='p2p_identity.key' \
  --exclude='.rsync-in-progress' \
  "${PRIMARY_DATA_DIR}/" \
  "root@${TARGET_IP}:${SECONDARY_DATA_DIR}/"
ok "Rsync завершён"

# ── Шаг 3b: Удаляем sentinel после успешного rsync ────────
# rsync completed without error — the data dir is now consistent.
# Remove the sentinel so the node can start normally, and reset the
# rsync-started flag so any subsequent failure allows the trap to restart
# the target (data is known-good from this point forward).
info "Шаг 3b/7: Rsync завершён успешно — удаляем sentinel .rsync-in-progress…"
ssh "root@${TARGET_IP}" "rm -f '${SECONDARY_DATA_DIR}/.rsync-in-progress' && echo 'sentinel removed'"
_PUSH_RSYNC_STARTED=0
ok "Sentinel удалён (нода готова к запуску)"

# ── Перезапускаем ноду на ИСТОЧНИКЕ ───────────────────────
info "Перезапускаем aperod-node на источнике…"
if systemctl start aperod-node 2>/dev/null; then
  ok "aperod-node на источнике запущен"
else
  warn "Не удалось запустить aperod-node на источнике — проверьте вручную"
fi
# Mark source as back up so the trap won't try to restart it again.
# We intentionally keep the trap active: the target node is still stopped
# and must be restarted by the trap if any subsequent step fails.
_SOURCE_STOPPED=0

# ── Шаг 4: Удаляем скопированный p2p identity ─────────────
info "Шаг 4/7: Удаляем скопированный p2p_identity.key…"
ssh "root@${TARGET_IP}" "rm -f ${SECONDARY_DATA_DIR}/p2p_identity.key && echo 'removed'"
ok "p2p_identity.key удалён (новый будет создан при старте)"

# ── Шаг 5: Прописываем bootnode в node.yaml нового узла ───
# Без этого шага secondary имеет bootnodes: [] и не инициирует
# P2P-подключение к primary — оба узла ждут входящего dial и
# никогда не видят друг друга (peer_count=0 навсегда).
info "Шаг 5/7: Прописываем bootnode ${PRIMARY_BOOTNODE} в ${SECONDARY_NODE_YAML}…"
ssh "root@${TARGET_IP}" bash <<ENDSSH
set -euo pipefail
NODE_YAML="${SECONDARY_NODE_YAML}"
BOOTNODE="${PRIMARY_BOOTNODE}"
NODE_CONFIG_SH="${SECONDARY_NODE_CONFIG_SH}"

# node.yaml must already exist from install-validator.sh / install-node.sh.
# Creating a stripped file here would silently discard network, data_dir,
# consensus, API and genesis settings and cause the service to fail to start.
if [[ ! -f "\${NODE_YAML}" ]]; then
  echo "[ERR]  \${NODE_YAML} не найден на целевом сервере." >&2
  echo "       Установите ноду через install-validator.sh (или install-node.sh)" >&2
  echo "       перед запуском join-network.sh." >&2
  exit 1
fi

# Always write the bootnode to p2p.bootnodes — the only field read by
# config.go P2PConfig (yaml:"bootnodes" under p2p:).
# Both node-config.sh and the Python fallback migrate any legacy root-level
# 'bootnodes' key (produced by older install-node.sh) into p2p.bootnodes
# and remove the ignored root key so the node actually dials the listed peers.

# Preferred path: node-config.sh (YAML-safe, idempotent, handles migration).
if [[ -x "\${NODE_CONFIG_SH}" ]]; then
  APEROD_CONFIG="\${NODE_YAML}" bash "\${NODE_CONFIG_SH}" add-bootnode "\${BOOTNODE}"
else
  # Fallback: python3 directly — same migration logic as node-config.sh.
  python3 - "\${NODE_YAML}" "\${BOOTNODE}" <<'PY'
import sys, yaml, os

cfg_path = sys.argv[1]
bootnode  = sys.argv[2]

with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}

# Migrate legacy root-level 'bootnodes' into p2p.bootnodes.
legacy = cfg.pop("bootnodes", None)

p2p = cfg.setdefault("p2p", {})
nodes = list(p2p.get("bootnodes") or [])

if legacy:
    for entry in (legacy if isinstance(legacy, list) else [legacy]):
        if entry and entry not in nodes:
            nodes.append(entry)

if bootnode not in nodes:
    nodes.append(bootnode)
p2p["bootnodes"] = nodes

tmp = cfg_path + ".tmp"
with open(tmp, "w") as f:
    yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
os.replace(tmp, cfg_path)
print(f"[OK]   p2p.bootnodes updated: {nodes}")
PY
fi
ENDSSH
ok "Bootnode ${PRIMARY_BOOTNODE} прописан в ${SECONDARY_NODE_YAML}"

# ── Шаг 5.5: Прописываем IP валидатора в peer_whitelist нового узла ──
# Без этого шага реле получает bad-block strike при каждом gossip-блоке
# (реле не может принять блок пока не синхронизировалось) и после 5 страйков
# забанит валидатор на 24 ч — полностью разрывая P2P-синхронизацию.
# Белый список (peer_whitelist) исключает валидатор из счётчика страйков.
info "Шаг 5.5/7: Прописываем ${PRIMARY_IP} в p2p.peer_whitelist нового узла…"
ssh "root@${TARGET_IP}" python3 - "${SECONDARY_NODE_YAML}" "${PRIMARY_IP}" <<'PY'
import sys, yaml, os
cfg_path     = sys.argv[1]
validator_ip = sys.argv[2]
with open(cfg_path) as f:
    cfg = yaml.safe_load(f) or {}
p2p = cfg.setdefault("p2p", {})
wl  = list(p2p.get("peer_whitelist") or [])
if validator_ip not in wl:
    wl.append(validator_ip)
    p2p["peer_whitelist"] = wl
    tmp = cfg_path + ".tmp"
    with open(tmp, "w") as f:
        yaml.dump(cfg, f, default_flow_style=False, allow_unicode=True)
    os.replace(tmp, cfg_path)
    print(f"[OK]   p2p.peer_whitelist updated: {wl}")
else:
    print(f"[OK]   p2p.peer_whitelist already contains {validator_ip} — skipping")
PY
ok "IP валидатора ${PRIMARY_IP} добавлен в peer_whitelist нового узла"

# ── Шаг 6: Права и запуск ─────────────────────────────────
info "Шаг 6/7: Устанавливаем права, drop-in конфиги и запускаем ноду…"
ssh "root@${TARGET_IP}" "
  chown -R ${SECONDARY_USER}:${SECONDARY_USER} ${SECONDARY_DATA_DIR}/
  bash /opt/aperod/blockchain/deploy/ensure-dropin.sh
  systemctl enable --now aperod-node
  echo 'started'
"
# Target is confirmed up — clear the flag and remove the trap so that
# subsequent failures (health-wait, verify-dropin) do not trigger a
# spurious restart of a node that is already running.
_TARGET_STOPPED=0
trap - EXIT ERR
ok "Нода запущена"

# ── Verify drop-in settings on the new node ───────────────
info "Верифицируем drop-in настройки на ${TARGET_IP}…"
_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if ! bash "${_SCRIPT_DIR}/verify-dropin.sh" "${TARGET_IP}"; then
  warn "Drop-in проверка не прошла на ${TARGET_IP}."
  warn "Убедитесь что ensure-dropin.sh выполнился корректно и повторите join-network.sh."
  exit 1
fi

# ── Шаг 7: Ожидаем готовности API ─────────────────────────
info "Шаг 7/7: Ожидаем готовности API (key-image rebuild, ~5 мин)…"
ATTEMPT=0
HEIGHT=0
PEERS=0
_API_READY=0
while [[ ${ATTEMPT} -lt ${HEALTH_MAX_ATTEMPTS} ]]; do
  ATTEMPT=$((ATTEMPT + 1))

  STATS=$(ssh "root@${TARGET_IP}" \
    "curl -s --max-time 3 http://127.0.0.1:8545/api/v1/network/stats 2>/dev/null || echo ''"
  )

  if [[ -n "${STATS}" ]] && echo "${STATS}" | python3 -c "import sys,json; json.load(sys.stdin)" 2>/dev/null; then
    HEIGHT=$(echo "${STATS}" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('height',0))")
    PEERS=$(echo "${STATS}"  | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('peer_count',0))")
    echo -e "  Попытка ${ATTEMPT}/${HEALTH_MAX_ATTEMPTS}: height=${HEIGHT} peers=${PEERS}"
    if [[ "${HEIGHT}" -gt 0 ]]; then
      _API_READY=1
      break
    fi
  else
    echo -e "  Попытка ${ATTEMPT}/${HEALTH_MAX_ATTEMPTS}: API ещё не готов, ожидаем…"
  fi
  sleep ${HEALTH_WAIT_SECS}
done

if [[ ${_API_READY} -eq 0 ]]; then
  warn "Таймаут ожидания API. Проверьте логи: journalctl -u aperod-node -n 50 --no-pager"
  exit 1
fi

# ── Итог ──────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}╔══════════════════════════════════════════════════════════════╗"
echo -e "  ✓  Узел ${TARGET_IP} подключён к сети"
echo -e "     Height: ${HEIGHT}  |  Peers: ${PEERS}"
echo -e "╚══════════════════════════════════════════════════════════════╝${NC}"
echo

if [[ "${PEERS}" -eq 0 ]]; then
  warn "Peers = 0 — нода загружается, пиры появятся после завершения key-image rebuild (~5 мин)"
  warn "Повторите проверку: ssh root@${TARGET_IP} \"curl -s http://127.0.0.1:8545/api/v1/network/stats\""
else
  ok "Peers = ${PEERS} — сеть работает!"
fi

echo
info "Следующие шаги для нового валидатора:"
echo "  1. Убедитесь что порт 30303/tcp открыт (firewall)"
echo "  2. Пополните reward_address APRO для стейкинга (мин. 100 000 APRO)"
echo "  3. Отправьте StakeTx через кошелёк для регистрации в validator set"
echo "  4. После следующего epoch (~100 блоков) нода начнёт производить блоки"
echo
info "Для синхронизации с другим ключом (не основного узла) добавьте в node.yaml:"
echo "  consensus:"
echo "    non_validator: true   # синхронизация без производства блоков"
echo "    # validator_key: ...  # раскомментировать когда ключ добавлен в validator set"
