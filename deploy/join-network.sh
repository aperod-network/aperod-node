#!/usr/bin/env bash
# ============================================================
#  Aperod — Join Existing Network
#
#  РЕЖИМ 1 (push): Запуск на ОСНОВНОМ узле (validator).
#  Подключает новый сервер к работающей сети за один шаг.
#
#    sudo bash join-network.sh <IP_НОВОГО_СЕРВЕРА>
#
#    Пример: sudo bash join-network.sh <PRIMARY_IP>
#
#  РЕЖИМ 2 (bootstrap): Запуск на НОВОМ реле.
#  Копирует chain.db и актуальный снимок с валидатора, чтобы
#  реле не забанило валидатора при первом подключении.
#
#    sudo bash join-network.sh --bootstrap-from=<IP_ВАЛИДАТОРА>
#
#    Пример: sudo bash join-network.sh --bootstrap-from=<PRIMARY_IP>
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

  # ── Защитный trap (установлен ДО остановки любого сервиса) ─
  # Перезапускает каждый остановленный сервис независимо от того,
  # на каком шаге произошёл сбой. Снимается только после того,
  # как оба сервиса подтверждённо запущены.
  _bootstrap_cleanup() {
    local _exit=$?
    if [[ ${_exit} -ne 0 ]]; then
      warn "[TRAP] Bootstrap завершился с кодом ${_exit} — восстанавливаем ноды…"
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
  ssh "root@${VALIDATOR_IP}" bash <<'REMOTE_STOP'
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
    echo "stopped"
REMOTE_STOP
  ok "aperod-node на валидаторе остановлен"

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
  systemctl enable --now aperod-node
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

# ── Шаг 1: Останавливаем ноду на целевом сервере ──────────
info "Шаг 1/7: Останавливаем и отключаем aperod-node на ${TARGET_IP}…"
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
ok "aperod-node на источнике остановлен"

# ── Защитный trap: гарантирует запуск источника при любом сбое ──
# Срабатывает при выходе по ошибке (set -e / ERR) или по сигналу,
# пока нода на источнике остановлена. Снимается после успешного
# явного systemctl start ниже (trap - EXIT ERR).
_source_node_trap() {
  local _exit=$?
  if [[ ${_exit} -ne 0 ]]; then
    warn "[TRAP] rsync или последующий шаг завершился с кодом ${_exit}."
    warn "[TRAP] Автоматически запускаем aperod-node на ИСТОЧНИКЕ…"
    if systemctl start aperod-node 2>/dev/null; then
      ok "[TRAP] aperod-node на источнике запущен. Проверьте состояние сети вручную."
    else
      warn "[TRAP] Не удалось запустить aperod-node через trap — запустите вручную:"
      warn "       systemctl start aperod-node"
    fi
  fi
}
trap '_source_node_trap' EXIT ERR

# ── Шаг 3: Rsync данных с --delete ────────────────────────
info "Шаг 3/7: Синхронизируем цепь (rsync --delete)…"
info "  Это может занять несколько минут (~1-2 ГБ)"
rsync -az --delete --progress --ignore-errors \
  --exclude='p2p_identity.key' \
  "${PRIMARY_DATA_DIR}/" \
  "root@${TARGET_IP}:${SECONDARY_DATA_DIR}/"
ok "Rsync завершён"

# ── Перезапускаем ноду на ИСТОЧНИКЕ ───────────────────────
info "Перезапускаем aperod-node на источнике…"
if systemctl start aperod-node 2>/dev/null; then
  ok "aperod-node на источнике запущен"
  # Снимаем trap: источник запущен, дальнейшие ошибки его не касаются
  trap - EXIT ERR
else
  warn "Не удалось запустить aperod-node на источнике — проверьте вручную"
  trap - EXIT ERR
fi

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

# ── Шаг 6: Права и запуск ─────────────────────────────────
info "Шаг 6/7: Устанавливаем права, drop-in конфиги и запускаем ноду…"
ssh "root@${TARGET_IP}" "
  chown -R ${SECONDARY_USER}:${SECONDARY_USER} ${SECONDARY_DATA_DIR}/
  bash /opt/aperod/blockchain/deploy/ensure-dropin.sh
  systemctl enable --now aperod-node
  echo 'started'
"
ok "Нода запущена"

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
