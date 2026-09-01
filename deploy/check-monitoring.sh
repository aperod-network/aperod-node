#!/usr/bin/env bash
# ==========================================================
#  Aperod Monitoring — Диагностика
#  Запуск: sudo bash check-monitoring.sh
# ==========================================================
set -euo pipefail

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
ok()   { echo -e "${GREEN}[OK]${NC}    $*"; }
fail() { echo -e "${RED}[ERR]${NC}   $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
info() { echo -e "${CYAN}[INFO]${NC}  $*"; }
hdr()  { echo -e "\n${BOLD}━━━ $* ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"; }

hdr "1. Сервисы systemd"
for svc in prometheus grafana-server node_exporter nginx_exporter nginx aperod-api; do
    if systemctl is-active --quiet "$svc" 2>/dev/null; then
        ok "$svc — active"
    else
        fail "$svc — НЕ ЗАПУЩЕН  (systemctl start $svc)"
    fi
done

hdr "2. Порты (слушают ли процессы)"
for port_label in "9090:Prometheus" "3000:Grafana" "9100:node_exporter" "9113:nginx_exporter" "3001:API-сервер"; do
    port="${port_label%%:*}"
    label="${port_label##*:}"
    if ss -tlnp 2>/dev/null | grep -q ":${port}\b"; then
        ok "порт $port ($label)"
    else
        fail "порт $port ($label) — не слушает"
    fi
done

hdr "3. node_exporter — метрики CPU/RAM/Диск/Сеть"
if curl -sf http://localhost:9100/metrics >/dev/null 2>&1; then
    ok "node_exporter отвечает на :9100/metrics"
    # Проверяем сетевой интерфейс
    IFACE=$(ip -o link show | awk -F': ' '{print $2}' | grep -vE '^lo$|^docker|^veth|^br-' | head -1)
    info "Основной интерфейс: $IFACE"
    if curl -sf http://localhost:9100/metrics | grep -q "node_network_receive_bytes_total{device=\"${IFACE}\""; then
        ok "Трафик метрика для $IFACE найдена"
    else
        warn "Интерфейс $IFACE не найден в метриках — панель трафика может не показывать данные"
        info "Доступные интерфейсы в метриках:"
        curl -sf http://localhost:9100/metrics | grep 'node_network_receive_bytes_total{device=' | sed 's/.*device="\([^"]*\)".*/  → \1/'
    fi
    # Проверяем textfile collector
    TEXTFILE_DIR="/var/lib/node_exporter/textfile_collector"
    if [[ -d "$TEXTFILE_DIR" ]]; then
        ok "Textfile collector: $TEXTFILE_DIR"
        if [[ -f "$TEXTFILE_DIR/aperod_backup.prom" ]]; then
            ok "aperod_backup.prom найден:"
            cat "$TEXTFILE_DIR/aperod_backup.prom" | sed 's/^/  /'
        else
            warn "aperod_backup.prom НЕ НАЙДЕН — запустите бэкап вручную:"
            info "  sudo /usr/local/bin/aperod_backup.sh"
        fi
    else
        fail "Textfile collector директория не найдена: $TEXTFILE_DIR"
        info "  mkdir -p $TEXTFILE_DIR && chown node_exporter:node_exporter $TEXTFILE_DIR"
    fi
else
    fail "node_exporter не отвечает — проверьте: systemctl status node_exporter"
fi

hdr "4. nginx + nginx-prometheus-exporter"
if curl -sf http://127.0.0.1:8080/nginx_status >/dev/null 2>&1; then
    ok "nginx stub_status :8080/nginx_status работает"
    echo "  Активные соединения:"
    curl -sf http://127.0.0.1:8080/nginx_status | sed 's/^/  /'
else
    fail "nginx stub_status НЕ РАБОТАЕТ — добавьте конфиг:"
    info "  sudo bash -c 'cat > /etc/nginx/conf.d/stub_status.conf << EOF
server {
    listen 127.0.0.1:8080;
    location /nginx_status { stub_status; allow 127.0.0.1; deny all; }
}
EOF'
  sudo nginx -t && sudo systemctl reload nginx"
fi

if curl -sf http://localhost:9113/metrics | grep -q nginx_connections_active; then
    ok "nginx-prometheus-exporter :9113 отвечает"
else
    fail "nginx-prometheus-exporter не отвечает на :9113"
    info "  systemctl start nginx_exporter  ИЛИ  sudo bash install-monitoring.sh"
fi

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
else
    fail "Prometheus не отвечает на :9090"
fi

hdr "6. Aperod API — ключевые метрики"
API_METRICS=$(curl -sf http://localhost:3001/api/metrics 2>/dev/null || echo "")
if [[ -n "$API_METRICS" ]]; then
    ok "API /api/metrics отвечает"
    for metric in aperod_up aperod_chain_height aperod_mempool_size aperod_peer_count \
                  aperod_api_up aperod_api_uptime_seconds \
                  aperod_daily_payout_last_run_success aperod_daily_payout_validators_paid; do
        val=$(echo "$API_METRICS" | grep "^${metric} " | awk '{print $2}')
        if [[ -n "$val" ]]; then
            ok "$metric = $val"
        else
            warn "$metric — не найдена в /api/metrics"
        fi
    done
else
    fail "API не отвечает на :3001/api/metrics"
fi

hdr "7. Grafana — nginx проксирование"
GSTATUS=$(curl -sf -o /dev/null -w "%{http_code}" http://localhost:3000/grafana/ 2>/dev/null || echo "000")
if [[ "$GSTATUS" =~ ^(200|302)$ ]]; then
    ok "Grafana отвечает на /grafana/ (HTTP $GSTATUS)"
else
    fail "Grafana /grafana/ вернула HTTP $GSTATUS"
    info "Проверьте /etc/grafana/grafana.ini:"
    grep -A3 '^\[server\]' /etc/grafana/grafana.ini 2>/dev/null || echo "  [server] секция не найдена"
fi

# Проверяем proxy_pass в nginx
NGINX_CONF=$(grep -r "proxy_pass.*grafana\|proxy_pass.*3000" /etc/nginx/ 2>/dev/null | head -5 || echo "")
if echo "$NGINX_CONF" | grep -q "3000/grafana/"; then
    ok "nginx proxy_pass содержит /grafana/ — правильно"
elif echo "$NGINX_CONF" | grep -q "3000/;"; then
    warn "nginx proxy_pass ведёт на 3000/ без /grafana/ — config.js будет просачиваться в /api/"
    info "  Исправьте: proxy_pass http://127.0.0.1:3000/grafana/;"
    info "  Файл конфига: sudo nano /etc/nginx/sites-available/aperod.com"
else
    warn "nginx proxy для Grafana не найден или путь неверный"
fi

hdr "8. Настройки интеграций (без секретов)"
SETTINGS_FILE="/opt/aperod/data/integration-settings.json"
if [[ -f "$SETTINGS_FILE" ]]; then
    ok "integration-settings.json найден"
    python3 -c "
import json
data = json.load(open('$SETTINGS_FILE'))
for section, vals in data.items():
    keys_set = [k for k, v in vals.items() if v and str(v) not in ('', 'false', '0')]
    keys_empty = [k for k, v in vals.items() if not v or str(v) in ('', 'false', '0')]
    print(f'  [{section}]')
    for k in keys_set:
        v = vals[k]
        if isinstance(v, str) and len(v) > 6:
            print(f'    {k} = {v[:3]}...{v[-3:]}  ✓')
        else:
            print(f'    {k} = {v}  ✓')
    for k in keys_empty:
        print(f'    {k} = (пусто)')
"
else
    fail "integration-settings.json не найден: $SETTINGS_FILE"
    info "  Файл создаётся после первого сохранения через /admin-panel/integrations"
    info "  Убедитесь, что API запущен и DATA_DIR=/opt/aperod/data"
fi

hdr "Готово"
echo -e "  Если блоки в Grafana показывают 'No data':"
echo -e "  1. Убедитесь, что все сервисы выше показывают [OK]"
echo -e "  2. Переимпортируйте дашборд: Dashboards → Import → grafana-dashboard.json"
echo -e "  3. Бэкап-панель появится ТОЛЬКО после первого запуска aperod_backup.sh"
echo -e "  4. Панель выплат появится после следующего запуска (20:00 МСК)"
echo
