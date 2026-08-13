#!/usr/bin/env bash
# =============================================================================
#  aperod-node-watchdog.sh — Restart aperod-node when its API stops responding,
#                            RSS exceeds the RAM threshold, or block production
#                            stalls while the process is alive.
#
#  Invoked every 45–60 s by aperod-node-watchdog.timer.
#  Sends GET /api/v1/status to 127.0.0.1:8545 with a 5 s timeout.
#  If the response is not HTTP 200, triggers `systemctl restart aperod-node`.
#
#  Optional env vars (set in the .service unit's Environment= lines or in
#  /etc/aperod/watchdog.env):
#    NODE_API_URL          — base URL of the Go node API (default: http://127.0.0.1:8545)
#    TIMEOUT_SECS          — curl timeout in seconds (default: 5)
#    RAM_THRESHOLD_MB      — restart when RSS exceeds this (default: 4800; 0=disable)
#    STALL_CHECKS_MAX      — consecutive no-new-block checks before restart (default: 3; 0=disable)
#    WATCHDOG_INTERVAL_SECS — probe interval, used only in Telegram messages (default: 60)
#    SUPPORT_BOT_TOKEN      — Telegram bot token for alerts (optional)
#    SUPPORT_ADMIN_CHAT_ID  — Telegram chat ID for alerts (optional)
#    PEER_WAIT_MINS         — minutes with peer_count=0 before alerting (default: 10; 0=disable)
#    DISK_CHECK_PATH        — filesystem to check for free space (default: /)
#    DISK_WARN_PCT          — alert via Telegram when free space < N % (default: 15; 0=disable)
#    DISK_CRIT_PCT          — auto-truncate syslog/daemon.log when free space < N % (default: 5; 0=disable)
#    MOCK_RSS_KB            — inject fake RSS for testing
#    MOCK_HEIGHT            — inject fake block height for testing
#    MOCK_PEER_COUNT        — inject fake peer count for testing
#    MOCK_DISK_FREE_PCT     — inject fake disk-free percentage for testing
# =============================================================================
set -euo pipefail

NODE_API_URL="${NODE_API_URL:-http://127.0.0.1:8545}"
TIMEOUT_SECS="${TIMEOUT_SECS:-5}"
WATCHDOG_INTERVAL_SECS="${WATCHDOG_INTERVAL_SECS:-60}"
STATUS_URL="${NODE_API_URL}/api/v1/status"

# How long to wait between Telegram alerts for the same ongoing outage (default: 1 h).
# Prevents message flood when the node is down for an extended period.
ALERT_COOLDOWN_SECS="${ALERT_COOLDOWN_SECS:-3600}"

# State files written every run so the Admin Panel can show watchdog status
# STATE_DIR may be overridden by tests via the environment variable.
STATE_DIR="${STATE_DIR:-/var/lib/aperod}"
LAST_CHECK_FILE="${STATE_DIR}/watchdog-last-check"
LAST_RESTART_FILE="${STATE_DIR}/watchdog-last-restart"
RESTART_COUNT_FILE="${STATE_DIR}/watchdog-restarts"
LAST_ALERT_FILE="${STATE_DIR}/watchdog-last-alert"
# Individual restart timestamps for the 24-h crash-loop counter (one Unix-ms per line)
RESTART_EVENTS_FILE="${STATE_DIR}/watchdog-restart-events"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "$(date -u '+%Y-%m-%dT%H:%M:%SZ') [watchdog] $*"; }

send_telegram() {
  local msg="$1"
  if [[ -n "${SUPPORT_BOT_TOKEN:-}" && -n "${SUPPORT_ADMIN_CHAT_ID:-}" ]]; then
    curl -s -X POST \
      "https://api.telegram.org/bot${SUPPORT_BOT_TOKEN}/sendMessage" \
      -d chat_id="${SUPPORT_ADMIN_CHAT_ID}" \
      -d text="${msg}" \
      -d parse_mode="HTML" \
      >/dev/null 2>&1 || true
  fi
}

# Record the current UTC timestamp to a file (creates state dir if needed)
write_timestamp() {
  local file="$1"
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  date -u '+%Y-%m-%dT%H:%M:%SZ' > "${file}" || true
}

# Atomically increment the restart counter file
increment_restart_count() {
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  local count=0
  if [[ -f "${RESTART_COUNT_FILE}" ]]; then
    count=$(cat "${RESTART_COUNT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
  fi
  echo $(( count + 1 )) > "${RESTART_COUNT_FILE}" || true
}

# Append a Unix-millisecond timestamp to the 24-h restart-events log and prune
# entries older than 25 hours.  Using date +%s (seconds) * 1000 for portability
# across GNU and BSD date; sub-second precision is not needed for 24-h tracking.
append_restart_event() {
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  local ts_ms
  ts_ms=$(( $(date +%s) * 1000 ))
  echo "${ts_ms}" >> "${RESTART_EVENTS_FILE}" || true
  # Prune events older than 25 h (90000 s) to keep the file small
  if [[ -f "${RESTART_EVENTS_FILE}" ]]; then
    local cutoff_ms
    cutoff_ms=$(( ( $(date +%s) - 90000 ) * 1000 ))
    awk -v c="${cutoff_ms}" '$1+0 >= c' "${RESTART_EVENTS_FILE}" \
      > "${RESTART_EVENTS_FILE}.tmp" 2>/dev/null \
      && mv "${RESTART_EVENTS_FILE}.tmp" "${RESTART_EVENTS_FILE}" 2>/dev/null || true
  fi
}

# ---------------------------------------------------------------------------
# Record this check run (always — so Admin Panel can detect liveness)
# ---------------------------------------------------------------------------
write_timestamp "${LAST_CHECK_FILE}"

# ---------------------------------------------------------------------------
# Health check (body captured for block-height parsing)
# ---------------------------------------------------------------------------
_body_file=$(mktemp /tmp/aperod-wdog-XXXXXX)
HTTP_CODE=$(curl -s -o "${_body_file}" -w "%{http_code}" \
  --max-time "${TIMEOUT_SECS}" \
  "${STATUS_URL}" 2>/dev/null || echo "000")
RESPONSE_BODY=$(cat "${_body_file}" 2>/dev/null || echo "")
rm -f "${_body_file}"

if [[ "$HTTP_CODE" == "200" ]]; then
  log "API OK (HTTP ${HTTP_CODE})"

  # ── RAM guard — proactive restart before OOM/SIGKILL ─────────────────────
  # Even when the API is healthy, RSS can grow unboundedly due to a memory
  # leak in the block validator.  We restart proactively at RAM_THRESHOLD_MB
  # (default: 4800 MB = 4.8 GB) so systemd never has to SIGKILL the process,
  # which avoids LevelDB corruption from a mid-write forced kill.
  #
  # Set RAM_THRESHOLD_MB=0 to disable this check (archive nodes with large RAM).
  # MOCK_RSS_KB may be set in tests to inject a fake RSS value.
  RAM_THRESHOLD_MB="${RAM_THRESHOLD_MB:-4800}"
  if [[ "${RAM_THRESHOLD_MB}" -gt 0 ]]; then
    if [[ -n "${MOCK_RSS_KB:-}" ]]; then
      RSS_KB="${MOCK_RSS_KB}"
    else
      RSS_KB=$(ps aux | grep '/usr/local/bin/aperod-node' | grep -v grep \
               | awk '{print $6}' | head -1 2>/dev/null || true)
    fi
    RSS_KB="${RSS_KB:-0}"
    THRESHOLD_KB=$(( RAM_THRESHOLD_MB * 1024 ))

    if [[ "${RSS_KB}" -gt "${THRESHOLD_KB}" ]]; then
      RSS_MB=$(( RSS_KB / 1024 ))
      log "RAM threshold exceeded: ${RSS_MB} MB > ${RAM_THRESHOLD_MB} MB — restarting aperod-node"

      # Respect cooldown for RAM alerts (separate from API-failure alerts)
      LAST_RAM_ALERT_FILE="${STATE_DIR}/watchdog-last-ram-alert"
      _now_r=$(date +%s)
      _last_r=0
      if [[ -f "${LAST_RAM_ALERT_FILE}" ]]; then
        _last_r=$(cat "${LAST_RAM_ALERT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
      fi
      if (( ( _now_r - _last_r ) >= ALERT_COOLDOWN_SECS )); then
        send_telegram "🧠 <b>aperod-node RAM watchdog</b>
Server: $(hostname)
RSS: ${RSS_MB} MB ≥ threshold: ${RAM_THRESHOLD_MB} MB
Action: <code>systemctl restart aperod-node</code>
(proactive restart — API was still healthy)"
        mkdir -p "${STATE_DIR}" 2>/dev/null || true
        echo "${_now_r}" > "${LAST_RAM_ALERT_FILE}" || true
      fi

      systemctl restart aperod-node
      write_timestamp "${LAST_RESTART_FILE}"
      increment_restart_count
      append_restart_event
      log "aperod-node restarted due to RAM threshold"
      exit 0
    fi
  fi

  # ── Disk space guard — alert + emergency cleanup ───────────────────────────
  # A full disk causes LevelDB write failures → crash-restart loop → even more
  # logs written → disk fills faster.  The watchdog breaks this cycle by:
  #   1. Alerting (Telegram) when free space falls below DISK_WARN_PCT (15 %).
  #   2. Auto-truncating /var/log/syslog and /var/log/daemon.log when free
  #      space falls below DISK_CRIT_PCT (5 %) so the node can keep running.
  #
  # Set DISK_WARN_PCT=0 to disable both alert and auto-cleanup.
  DISK_CHECK_PATH="${DISK_CHECK_PATH:-/}"
  DISK_WARN_PCT="${DISK_WARN_PCT:-15}"
  DISK_CRIT_PCT="${DISK_CRIT_PCT:-5}"
  LAST_DISK_ALERT_FILE="${STATE_DIR}/watchdog-last-disk-alert"

  if [[ "${DISK_WARN_PCT}" -gt 0 ]]; then
    if [[ -n "${MOCK_DISK_FREE_PCT:-}" ]]; then
      DISK_FREE_PCT="${MOCK_DISK_FREE_PCT}"
    else
      # df --output=pcent prints "Use%" header + "NN%" — strip both and invert.
      _used_pct=$(df "${DISK_CHECK_PATH}" --output=pcent 2>/dev/null \
                  | tail -1 | tr -dc '0-9' || echo "0")
      DISK_FREE_PCT=$(( 100 - _used_pct ))
    fi

    log "disk ${DISK_CHECK_PATH}: ${DISK_FREE_PCT}% free (warn<${DISK_WARN_PCT}% crit<${DISK_CRIT_PCT}%)"

    if [[ "${DISK_FREE_PCT}" -lt "${DISK_WARN_PCT}" ]]; then
      _now_d=$(date +%s)
      _last_da=0
      if [[ -f "${LAST_DISK_ALERT_FILE}" ]]; then
        _last_da=$(cat "${LAST_DISK_ALERT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
      fi

      if (( ( _now_d - _last_da ) >= ALERT_COOLDOWN_SECS )); then
        _du_syslog=$(du -sh /var/log/syslog /var/log/daemon.log 2>/dev/null \
                     | awk '{s+=$1} END {print s"K"}' || echo "?")
        send_telegram "💾 <b>aperod-node disk space warning</b>
Server: $(hostname)
Filesystem: <code>${DISK_CHECK_PATH}</code>
Free: <b>${DISK_FREE_PCT}%</b> (threshold: ${DISK_WARN_PCT}%)
Syslog+daemon.log: ~${_du_syslog}

If disk fills completely the node will crash-loop.
Run: <code>truncate -s 0 /var/log/syslog /var/log/daemon.log</code>"
        mkdir -p "${STATE_DIR}" 2>/dev/null || true
        echo "${_now_d}" > "${LAST_DISK_ALERT_FILE}" || true
        log "Telegram disk-space alert sent (${DISK_FREE_PCT}% free)"
      else
        log "Disk alert suppressed (cooldown)"
      fi

      # ── Critical: auto-cleanup to prevent a crash-loop ──────────────────
      if [[ "${DISK_CRIT_PCT}" -gt 0 && "${DISK_FREE_PCT}" -lt "${DISK_CRIT_PCT}" ]]; then
        log "CRITICAL: disk ${DISK_FREE_PCT}% free < ${DISK_CRIT_PCT}% — auto-truncating syslog files"
        for _logfile in /var/log/syslog /var/log/daemon.log \
                        /var/log/syslog.1 /var/log/daemon.log.1; do
          if [[ -f "${_logfile}" ]]; then
            truncate -s 0 "${_logfile}" 2>/dev/null || true
            log "  truncated ${_logfile}"
          fi
        done
        journalctl --vacuum-size=200M >/dev/null 2>&1 || true
        _free_after=$(df "${DISK_CHECK_PATH}" --output=pcent 2>/dev/null \
                      | tail -1 | tr -dc '0-9' || echo "?")
        _free_pct_after=$(( 100 - ${_free_after:-0} ))
        log "auto-cleanup done — disk now ${_free_pct_after}% free"
        send_telegram "🧹 <b>aperod-node disk auto-cleanup</b>
Server: $(hostname)
Triggered at: <b>${DISK_FREE_PCT}%</b> free (critical threshold: ${DISK_CRIT_PCT}%)
After cleanup: <b>${_free_pct_after}%</b> free

Truncated: /var/log/syslog, /var/log/daemon.log
Install logrotate to prevent recurrence:
<code>cp /opt/aperod/blockchain/deploy/aperod-syslog.logrotate /etc/logrotate.d/aperod-syslog</code>"
      fi
    fi
  fi

  # ── Block-production stall check ───────────────────────────────────────────
  # Detects a silent freeze: node process alive, API returns 200, but block
  # height has not advanced for STALL_CHECKS_MAX consecutive probes.
  # This catches the "255% CPU, blocks frozen, API healthy" failure mode that
  # the RAM check and API check both miss.
  #
  # Set STALL_CHECKS_MAX=0 to disable.
  # MOCK_HEIGHT may be set in tests to inject a fake height.
  STALL_CHECKS_MAX="${STALL_CHECKS_MAX:-3}"
  HEIGHT_FILE="${STATE_DIR}/watchdog-last-height"
  STALL_COUNT_FILE="${STATE_DIR}/watchdog-stall-count"
  LAST_STALL_ALERT_FILE="${STATE_DIR}/watchdog-last-stall-alert"

  if [[ "${STALL_CHECKS_MAX}" -gt 0 ]]; then
    if [[ -n "${MOCK_HEIGHT:-}" ]]; then
      HEIGHT="${MOCK_HEIGHT}"
      IS_SYNCING="false"
    else
      HEIGHT=$(echo "${RESPONSE_BODY}" | grep -o '"height":[0-9]*' \
               | grep -o '[0-9]*$' | head -1 || true)
      IS_SYNCING=$(echo "${RESPONSE_BODY}" | grep -o '"syncing":[a-z]*' \
                   | grep -o '[a-z]*$' | head -1 || echo "false")
    fi

    if [[ -n "${HEIGHT:-}" && "${HEIGHT}" =~ ^[0-9]+$ && "${IS_SYNCING}" != "true" ]]; then
      mkdir -p "${STATE_DIR}" 2>/dev/null || true
      LAST_HEIGHT=0
      if [[ -f "${HEIGHT_FILE}" ]]; then
        LAST_HEIGHT=$(cat "${HEIGHT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
      fi

      if [[ "${HEIGHT}" -gt "${LAST_HEIGHT}" ]]; then
        # Height advancing — healthy, reset stall counter
        echo "${HEIGHT}" > "${HEIGHT_FILE}" || true
        echo "0" > "${STALL_COUNT_FILE}" || true

      elif [[ "${HEIGHT}" -lt "${LAST_HEIGHT}" ]]; then
        # Height regressed (post-restart snapshot) — accept and reset
        echo "${HEIGHT}" > "${HEIGHT_FILE}" || true
        echo "0" > "${STALL_COUNT_FILE}" || true
        log "Block height regressed to ${HEIGHT} (post-restart snapshot load) — stall counter reset"

      else
        # Height unchanged — potential stall
        STALL_COUNT=0
        if [[ -f "${STALL_COUNT_FILE}" ]]; then
          STALL_COUNT=$(cat "${STALL_COUNT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
        fi
        STALL_COUNT=$(( STALL_COUNT + 1 ))
        echo "${STALL_COUNT}" > "${STALL_COUNT_FILE}" || true
        log "Block height stalled at ${HEIGHT} (stall check ${STALL_COUNT}/${STALL_CHECKS_MAX})"

        if [[ "${STALL_COUNT}" -ge "${STALL_CHECKS_MAX}" ]]; then
          _stall_secs=$(( STALL_COUNT * WATCHDOG_INTERVAL_SECS ))
          log "Stall threshold reached (~${_stall_secs}s at height ${HEIGHT}) — restarting aperod-node"

          _now_st=$(date +%s)
          _last_st=0
          if [[ -f "${LAST_STALL_ALERT_FILE}" ]]; then
            _last_st=$(cat "${LAST_STALL_ALERT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
          fi
          if (( ( _now_st - _last_st ) >= ALERT_COOLDOWN_SECS )); then
            send_telegram "🧊 <b>aperod-node block stall watchdog</b>
Server: $(hostname)
Height frozen at: ${HEIGHT} for ~${_stall_secs}s (${STALL_COUNT} checks)
Action: <code>systemctl restart aperod-node</code>
(API was healthy — process frozen without producing blocks)"
            echo "${_now_st}" > "${LAST_STALL_ALERT_FILE}" || true
          fi

          echo "0" > "${STALL_COUNT_FILE}" || true
          systemctl restart aperod-node
          write_timestamp "${LAST_RESTART_FILE}"
          increment_restart_count
          append_restart_event
          log "aperod-node restarted due to block production stall at height ${HEIGHT}"
          exit 0
        fi
      fi
    fi
  fi

  # ── Peer-count zero alert ──────────────────────────────────────────────────
  # Fires a Telegram alert when peer_count stays at 0 for longer than
  # PEER_WAIT_MINS (default: 10 minutes).  Typical causes include a blocked
  # port 30303, a wrong bootnode address, or a stale p2p_identity.key.
  #
  # peer_count is read from GET /api/v1/network/stats (not /api/v1/status).
  # When the field is absent or non-numeric the stats probe is treated as
  # "unknown" and the zero-since timer is RESET — only consecutive confirmed
  # zero-peer observations may advance the timer toward an alert.
  #
  # Set PEER_WAIT_MINS=0 to disable.  Non-integer values fall back to 10.
  # MOCK_PEER_COUNT may be set in tests to inject a fake value (bypasses curl).
  _raw_peer_wait="${PEER_WAIT_MINS:-10}"
  if [[ "${_raw_peer_wait}" =~ ^[0-9]+$ ]]; then
    PEER_WAIT_MINS="${_raw_peer_wait}"
  else
    log "PEER_WAIT_MINS='${_raw_peer_wait}' is not a non-negative integer — using default 10"
    PEER_WAIT_MINS=10
  fi
  PEERS_ZERO_SINCE_FILE="${STATE_DIR}/watchdog-peers-zero-since"
  LAST_PEER_ALERT_FILE="${STATE_DIR}/watchdog-last-peer-alert"
  STATS_URL="${NODE_API_URL}/api/v1/network/stats"

  if [[ "${PEER_WAIT_MINS}" -eq 0 ]]; then
    # Feature disabled — clear stale state so re-enabling never alerts based on
    # a non-continuous/unobserved absence recorded before the disable.
    rm -f "${PEERS_ZERO_SINCE_FILE}" "${LAST_PEER_ALERT_FILE}" 2>/dev/null || true
  else
    if [[ -n "${MOCK_PEER_COUNT:-}" ]]; then
      PEER_COUNT="${MOCK_PEER_COUNT}"
    else
      _stats_body=$(curl -s --max-time "${TIMEOUT_SECS}" "${STATS_URL}" 2>/dev/null || true)
      PEER_COUNT=$(echo "${_stats_body}" | grep -o '"peer_count":[0-9]*' \
                   | grep -o '[0-9]*$' | head -1 || true)
    fi

    # Only proceed when peer_count is an explicit non-empty integer.
    # An absent or malformed field is treated as "unknown" — reset the zero-since
    # timer so that only continuously confirmed zero-peer observations can alert.
    if [[ ! "${PEER_COUNT:-}" =~ ^[0-9]+$ ]]; then
      rm -f "${PEERS_ZERO_SINCE_FILE}" 2>/dev/null || true
      log "peer_count unavailable from ${STATS_URL} — zero-peer timer reset"
    elif [[ "${PEER_COUNT}" -gt 0 ]]; then
      # Peers present — clear both the zero-since marker and the per-outage alert
      # cooldown so the next distinct outage always gets its own fresh alert.
      rm -f "${PEERS_ZERO_SINCE_FILE}" "${LAST_PEER_ALERT_FILE}" 2>/dev/null || true
      log "peer_count=${PEER_COUNT} — peer-zero timer and alert cooldown cleared"
    else
      # peer_count confirmed 0
      _now_p=$(date +%s)
      mkdir -p "${STATE_DIR}" 2>/dev/null || true

      if [[ ! -f "${PEERS_ZERO_SINCE_FILE}" ]]; then
        echo "${_now_p}" > "${PEERS_ZERO_SINCE_FILE}" || true
        log "peer_count=0 — started zero-peer timer"
      else
        _zero_since=$(cat "${PEERS_ZERO_SINCE_FILE}" 2>/dev/null | tr -dc '0-9' || echo "${_now_p}")
        _zero_secs=$(( _now_p - _zero_since ))
        _threshold_secs=$(( PEER_WAIT_MINS * 60 ))

        log "peer_count=0 for ${_zero_secs}s (threshold: ${_threshold_secs}s)"

        if (( _zero_secs >= _threshold_secs )); then
          _last_pa=0
          if [[ -f "${LAST_PEER_ALERT_FILE}" ]]; then
            _last_pa=$(cat "${LAST_PEER_ALERT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
          fi
          if (( ( _now_p - _last_pa ) >= ALERT_COOLDOWN_SECS )); then
            _zero_mins=$(( _zero_secs / 60 ))
            send_telegram "🔌 <b>aperod-node: no peers for ${_zero_mins} min</b>
Server: $(hostname)
peer_count has been <b>0</b> for <b>${_zero_mins} min</b>.

Possible causes:
• Port 30303/tcp blocked by firewall
• Wrong bootnode address in <code>/etc/aperod/node.yaml</code>
• Stale <code>p2p_identity.key</code> (delete and restart to regenerate)

Check: <code>curl -s ${STATS_URL} | grep peer</code>"
            echo "${_now_p}" > "${LAST_PEER_ALERT_FILE}" || true
            log "Telegram peer-zero alert sent (${_zero_mins} min with 0 peers)"
          else
            log "Peer-zero alert suppressed (cooldown)"
          fi
        fi
      fi
    fi
  fi

  log "OK — API healthy, RAM within threshold, blocks advancing"
  exit 0
fi

# ---------------------------------------------------------------------------
# Probe failed — restart the node
# ---------------------------------------------------------------------------
log "FAIL: ${STATUS_URL} returned HTTP ${HTTP_CODE} (timeout=${TIMEOUT_SECS}s) — restarting aperod-node"

# Respect cooldown: only send Telegram alert if ALERT_COOLDOWN_SECS have passed
# since the last alert. This prevents flooding the chat during a prolonged outage.
_now=$(date +%s)
_last_alert=0
if [[ -f "${LAST_ALERT_FILE}" ]]; then
  _last_alert=$(cat "${LAST_ALERT_FILE}" 2>/dev/null | tr -dc '0-9' || echo 0)
fi
_elapsed=$(( _now - _last_alert ))

if (( _elapsed >= ALERT_COOLDOWN_SECS )); then
  send_telegram "🔄 <b>aperod-node watchdog</b>
Server: $(hostname)
Probe: <code>${STATUS_URL}</code>
Result: HTTP ${HTTP_CODE}
Action: <code>systemctl restart aperod-node</code>"
  mkdir -p "${STATE_DIR}" 2>/dev/null || true
  echo "${_now}" > "${LAST_ALERT_FILE}" || true
else
  log "Telegram alert suppressed (cooldown: ${_elapsed}s elapsed of ${ALERT_COOLDOWN_SECS}s required)"
fi

systemctl restart aperod-node

# Record the restart event
write_timestamp "${LAST_RESTART_FILE}"
increment_restart_count
append_restart_event

log "aperod-node restarted"
exit 0
