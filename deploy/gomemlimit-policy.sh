#!/usr/bin/env bash
# Shared host-aware GOMEMLIMIT policy. Source this file, then call
# gomemlimit_resolve. GOMEMLIMIT_BYTES is deliberately honoured first so an
# operator can pin a value for a host with an unusual memory configuration.

GOMEMLIMIT_FLOOR_BYTES=$((1536 * 1024 * 1024)) # 1.5 GiB
GOMEMLIMIT_CAP_BYTES=5905580032                # canonical 5.5 GiB cap

gomemlimit_resolve() {
  if [[ -n "${GOMEMLIMIT_BYTES:-}" ]]; then
    [[ "${GOMEMLIMIT_BYTES}" =~ ^[0-9]+$ ]] \
      || { echo "GOMEMLIMIT_BYTES must be a non-negative integer number of bytes" >&2; return 1; }
    GOMEMLIMIT_SOURCE="override"
    export GOMEMLIMIT_BYTES GOMEMLIMIT_SOURCE
    return 0
  fi

  local meminfo="${GOMEMLIMIT_MEMINFO:-/proc/meminfo}"
  local total_kb="${GOMEMLIMIT_MEMTOTAL_KB:-}"
  if [[ -z "${total_kb}" ]]; then
    total_kb=$(awk '$1 == "MemTotal:" { print $2; exit }' "${meminfo}") \
      || { echo "Could not read MemTotal from ${meminfo}" >&2; return 1; }
  fi
  [[ "${total_kb}" =~ ^[0-9]+$ && "${total_kb}" -gt 0 ]] \
    || { echo "Could not parse MemTotal from ${meminfo}" >&2; return 1; }

  # The in-process RSS watchdog fires at 85% of GOMEMLIMIT. Using 75% of
  # physical RAM here made the effective restart threshold only 63.75% of RAM,
  # below the relay's normal transient RSS during snapshot reconciliation.
  # 87.5% keeps that RSS threshold at ~74% of physical RAM, leaving roughly a
  # quarter of RAM for the OS and non-Go overhead while allowing startup to
  # complete without a restart loop.
  local value=$(( total_kb * 1024 * 7 / 8 ))
  (( value < GOMEMLIMIT_FLOOR_BYTES )) && value=${GOMEMLIMIT_FLOOR_BYTES}
  (( value > GOMEMLIMIT_CAP_BYTES )) && value=${GOMEMLIMIT_CAP_BYTES}
  GOMEMLIMIT_BYTES=${value}
  GOMEMLIMIT_SOURCE="host (${total_kb} KiB RAM)"
  export GOMEMLIMIT_BYTES GOMEMLIMIT_SOURCE
}