#!/usr/bin/env bash
# ==========================================================
#  Aperod Monitoring — Диагностика
#  Запуск: sudo bash check-monitoring.sh
# ==========================================================
set -euo pipefail

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
FAILED=0
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
fail() { FAILED=1; echo -e "${RED}[ERR]${NC}   $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
info() { echo -e "${CYAN}[INFO]${NC}  $*"; }
hdr()  { echo -e "\n${BOLD}━━━ $* ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

hdr "1. Сервисы systemd"
for svc in prometheus prometheus-alertmanager grafana-server prometheus-node-exporter aperod-validator-health-exporter nginx aperod-api aperod-node aperod-explorer-indexer; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        ok "$svc — active"
    else
        fail "$svc — НЕ ЗАПУЩЕН  (systemctl start $svc)"
    fi
done

while IFS='|' read -r label expected binary; do
    if [[ -x "$binary" ]] && "$binary" --version 2>&1 | grep -Fq "version $expected"; then
        ok "$label версии $expected"
    else
        fail "$label не соответствует ожидаемой версии $expected"
    fi
done <<'RELEASES'
Prometheus|3.16.0|/usr/local/bin/prometheus
Alertmanager|0.35.1|/usr/local/bin/prometheus-alertmanager
node_exporter|1.13.1|/usr/local/bin/prometheus-node-exporter
RELEASES

hdr "2. Порты (слушают ли процессы)"
for port_label in "9090:Prometheus" "9093:Alertmanager" "3000:Grafana" "9100:node_exporter" "18545:Mainnet-нода" "18547:локальный статус валидатора" "3001:API-сервер"; do
    port="${port_label%%:*}"
    label="${port_label##*:}"
    if ss -tlnp 2>/dev/null | grep -q ":${port}\b"; then
        ok "порт $port ($label)"
    else
        fail "порт $port ($label) — не слушает"
    fi
done

hdr "3. node_exporter — метрики CPU/RAM/Диск/Сеть"
NODE_EXPORTER_METRICS=$(curl -sf http://127.0.0.1:9100/metrics 2>/dev/null || echo "")
if [[ -n "$NODE_EXPORTER_METRICS" ]]; then
    ok "node_exporter отвечает на :9100/metrics"
    # Проверяем сетевой интерфейс
    IFACE=$(ip -o link show | awk -F': ' '{print $2}' | grep -vE '^lo$|^docker|^veth|^br-' | head -1)
    info "Основной интерфейс: $IFACE"
    if grep -Fq "node_network_receive_bytes_total{device=\"${IFACE}\"" <<< "$NODE_EXPORTER_METRICS"; then
        ok "Трафик метрика для $IFACE найдена"
    else
        warn "Интерфейс $IFACE не найден в метриках — панель трафика может не показывать данные"
        info "Доступные интерфейсы в метриках:"
        grep 'node_network_receive_bytes_total{device=' <<< "$NODE_EXPORTER_METRICS" | sed 's/.*device="\([^"]*\)".*/  → \1/' | sort -u
    fi
    if grep -q '^node_systemd_unit_state' <<< "$NODE_EXPORTER_METRICS"; then
        ok "systemd collector включён"
    else
        fail "node_exporter не публикует состояние служб systemd"
    fi
else
    fail "node_exporter не отвечает — проверьте: systemctl status node_exporter"
fi

hdr "4. Состояние служб через node_exporter"
for unit in aperod-node.service aperod-api.service aperod-explorer-indexer.service aperod-validator-health-exporter.service nginx.service; do
    if grep -Eq "^node_systemd_unit_state\\{name=\"$unit\",state=\"active\",type=\"[^\"]+\"\\} 1$" <<< "$NODE_EXPORTER_METRICS"; then
        ok "$unit — active по метрикам systemd"
    else
        fail "$unit — не подтверждена как active по метрикам systemd"
    fi
done

hdr "5. Prometheus — targets"
PROM_TARGETS=$(curl -sf "http://localhost:9090/api/v1/targets" 2>/dev/null || echo "")
if [[ -n "$PROM_TARGETS" ]]; then
    ok "Prometheus API отвечает"
    echo "$PROM_TARGETS" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for t in data.get('data', {}).get('activeTargets', []):
    job = t.get('labels', {}).get('job', '?')
    url = t.get('scrapeUrl', '?')
    health = t.get('health', '?')
    last_err = t.get('lastError', '')
    color = '\033[0;32m' if health == 'up' else '\033[0;31m'
    nc = '\033[0m'
    print(f'  {color}[{health.upper()}]{nc}  {job}  ({url})')
    if last_err:
        print(f'         ошибка: {last_err}')
"
    if echo "$PROM_TARGETS" | python3 -c "
import json, sys
expected = {'alertmanager', 'aperod-api-server', 'aperod-node', 'aperod-validator-health', 'node-exporter'}
targets = json.load(sys.stdin).get('data', {}).get('activeTargets', [])
jobs = {target.get('labels', {}).get('job') for target in targets}
ok = len(targets) == 5 and jobs == expected and all(target.get('health') == 'up' for target in targets)
sys.exit(0 if ok else 1)
"; then
        ok "Все пять ожидаемых целей Prometheus работают"
    else
        fail "Prometheus должен иметь ровно пять целей, все в состоянии up"
    fi
else
    fail "Prometheus не отвечает на :9090"
fi

hdr "6. Mainnet-нода и API — ключевые метрики"
NODE_METRICS=$(curl -sf http://127.0.0.1:18545/metrics 2>/dev/null || echo "")
API_METRICS=$(curl -sf http://localhost:3001/api/metrics 2>/dev/null || echo "")
VALIDATOR_METRICS=$(curl -sf http://127.0.0.1:18547/metrics 2>/dev/null || echo "")
if [[ -n "$NODE_METRICS" ]]; then
    ok "Нода /metrics отвечает на :18545"
    for metric in aperod_chain_height aperod_peer_count; do
        val=$(echo "$NODE_METRICS" | grep "^${metric} " | awk '{print $2}')
        if [[ -n "$val" ]]; then
            ok "$metric = $val"
        else
            warn "$metric — не найдена в метриках ноды"
        fi
    done
else
    fail "Метрики ноды не отвечают на :18545/metrics"
fi
if [[ -n "$VALIDATOR_METRICS" ]]; then
    ok "Экспортёр статуса валидатора отвечает на :18547/metrics"
    for metric in aperod_validator_monitor_status_available \
                  aperod_validator_monitor_validator_mode_enabled \
                  aperod_validator_monitor_local_key_loaded \
                  aperod_validator_monitor_local_active \
                  aperod_validator_monitor_p2p_connected \
                  aperod_validator_monitor_online \
                  aperod_validator_monitor_sync_healthy \
                  aperod_validator_monitor_peer_count \
                  aperod_validator_monitor_peer_sync_lag_blocks_max; do
        val=$(echo "$VALIDATOR_METRICS" | grep "^${metric} " | awk '{print $2}')
        if [[ -n "$val" ]]; then
            ok "$metric = $val"
        else
            fail "$metric — не найдена в метриках экспортёра валидатора"
        fi
    done
else
    fail "Экспортёр статуса валидатора не отвечает на :18547/metrics"
fi
LOCAL_VALIDATOR_HEALTH=$(curl --noproxy '*' -sf --max-time 4 \
    --resolve explorer.aperod.com:443:127.0.0.1 \
    https://explorer.aperod.com/api/v1/local-validator-health 2>/dev/null || echo "")
if [[ -n "$LOCAL_VALIDATOR_HEALTH" ]] && echo "$LOCAL_VALIDATOR_HEALTH" | python3 -c '
import json, sys
d = json.load(sys.stdin)
valid = (
    isinstance(d.get("status_available"), bool)
    and d.get("status") in {"online", "offline", "unknown"}
    and (d.get("online") is None or isinstance(d.get("online"), bool))
    and (d.get("validator_mode_enabled") is None or isinstance(d.get("validator_mode_enabled"), bool))
    and (d.get("online") is not True or d.get("validator_mode_enabled") is True)
    and (d.get("sync_healthy") is None or isinstance(d.get("sync_healthy"), bool))
    and all(d.get(k) is None or (isinstance(d.get(k), int) and not isinstance(d.get(k), bool) and d[k] >= 0)
            for k in ("peer_count", "max_peer_lag_blocks", "chain_height", "checked_at_ms"))
)
sys.exit(0 if valid else 1)
'; then
    ok "Explorer API локального валидатора отдаёт корректный online/offline/unknown статус"
else
    fail "Explorer API не отдаёт корректный /api/v1/local-validator-health"
fi
if [[ -n "$API_METRICS" ]]; then
    ok "API /api/metrics отвечает"
    for metric in aperod_api_up aperod_api_uptime_seconds aperod_api_http_requests_total \
                  aperod_daily_payout_last_run_success aperod_daily_payout_validators_paid; do
        val=$(echo "$API_METRICS" | grep -E "^${metric}(\\{|[[:space:]])" | awk '{print $NF}' | head -1)
        if [[ -n "$val" ]]; then
            ok "$metric = $val"
        else
            warn "$metric — не найдена в /api/metrics"
        fi
    done
else
    fail "API не отвечает на :3001/api/metrics"
fi

hdr "7. Grafana — доступ и защита"
GSTATUS=$(curl -sf -o /dev/null -w "%{http_code}" http://127.0.0.1:3000/api/health 2>/dev/null || echo "000")
if [[ "$GSTATUS" == "200" ]]; then
    ok "Grafana health endpoint отвечает локально"
    GRAFANA_VERSION=$(curl -sf --max-time 4 http://127.0.0.1:3000/api/health 2>/dev/null \
        | python3 -c 'import json,sys; print(json.load(sys.stdin).get("version", ""))' || true)
    if [[ "$GRAFANA_VERSION" == "13.3.2" ]]; then
        ok "Grafana версии 13.3.2"
    else
        fail "Ожидалась Grafana 13.3.2, получена ${GRAFANA_VERSION:-неизвестно}"
    fi
    if DASHBOARDS=$(curl --noproxy '*' -fsS --max-time 6 \
        -H 'X-WEBAUTH-USER: aperod-admin' -H 'X-WEBAUTH-ROLE: Admin' \
        'http://127.0.0.1:3000/api/search?query=*&type=dash-db' 2>/dev/null) \
        && printf '%s' "$DASHBOARDS" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if any(x.get("uid")=="aperod-node-overview" for x in d) else 1)' 2>/dev/null; then
        ok "Дашборд aperod-node-overview доступен"
    else
        fail "Дашборд aperod-node-overview не найден через Grafana API"
    fi
    if DATASOURCES=$(curl --noproxy '*' -fsS --max-time 6 \
        -H 'X-WEBAUTH-USER: aperod-admin' -H 'X-WEBAUTH-ROLE: Admin' \
        http://127.0.0.1:3000/api/datasources 2>/dev/null); then
        DS_UID=$(printf '%s' "$DATASOURCES" | python3 -c 'import json,sys; d=json.load(sys.stdin); p=[x for x in d if x.get("type")=="prometheus"]; print(p[0].get("uid","") if p else "")' 2>/dev/null || true)
        if [[ -n "$DS_UID" ]] \
            && DS_QUERY=$(curl --noproxy '*' -fsS --max-time 8 \
                -H 'X-WEBAUTH-USER: aperod-admin' -H 'X-WEBAUTH-ROLE: Admin' \
                "http://127.0.0.1:3000/api/datasources/proxy/uid/$DS_UID/api/v1/query?query=up" 2>/dev/null) \
            && printf '%s' "$DS_QUERY" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("status")=="success" else 1)' 2>/dev/null; then
            ok "Grafana выполняет запрос up через Prometheus datasource"
        else
            fail "Grafana datasource не может выполнить запрос к Prometheus"
        fi
    else
        fail "Grafana API не вернул datasource"
    fi
else
    fail "Grafana health endpoint вернул HTTP $GSTATUS"
fi
if grep -q 'auth_request /_admin_gate_check;' /etc/nginx/sites-available/aperod-mainnet 2>/dev/null; then
    ok "Путь Grafana закрыт Admin Gate"
else
    fail "Путь Grafana не найден за Admin Gate"
fi
if grep -q 'content_security_policy = true' /etc/grafana/grafana.ini 2>/dev/null; then
    ok "Grafana использует собственный nonce CSP"
else
    fail "Grafana native nonce CSP не включён"
fi
GRAFANA_CSP=$(curl --noproxy '*' -sS --max-time 4 -D - -o /dev/null http://127.0.0.1:3000/grafana/login 2>/dev/null || true)
if grep -qiE '^Content-Security-Policy:.*nonce-' <<< "$GRAFANA_CSP"; then
    ok "Ответ Grafana содержит nonce в CSP"
else
    fail "В ответе Grafana отсутствует nonce CSP"
fi
if grep -Eq '^[[:space:]]*preinstall_disabled[[:space:]]*=[[:space:]]*true' /etc/grafana/grafana.ini \
    && grep -Eq '^[[:space:]]*preinstall_auto_update[[:space:]]*=[[:space:]]*false' /etc/grafana/grafana.ini \
    && grep -Fxq 'Environment=GF_PLUGINS_PREINSTALL_DISABLED=true' /etc/systemd/system/grafana-server.service.d/override.conf \
    && grep -Fxq 'Environment=GF_PLUGINS_PREINSTALL_AUTO_UPDATE=false' /etc/systemd/system/grafana-server.service.d/override.conf; then
    ok "Grafana не меняет плагины при запуске"
else
    fail "Grafana может автоматически устанавливать или обновлять плагины"
fi
GATE_STATUS=$(curl --noproxy '*' -k -sS --resolve aperod.com:443:127.0.0.1 \
    --max-time 4 -o /dev/null -w '%{http_code}' https://aperod.com/grafana/ 2>/dev/null || echo "000")
if [[ "$GATE_STATUS" == "404" ]]; then
    ok "Неавторизованный доступ к Grafana закрыт Admin Gate"
else
    fail "Admin Gate должен скрывать Grafana от неавторизованных запросов (HTTP $GATE_STATUS)"
fi

hdr "Готово"
echo -e "  Prometheus правила хранятся в /etc/prometheus/rules/aperod-mainnet.yml"
echo -e "  Для Telegram-теста отправьте один synthetic alert; после проверки разрешите его POST-обновлением endsAt."
echo
exit "$FAILED"
