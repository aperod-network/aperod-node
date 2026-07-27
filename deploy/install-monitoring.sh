#!/usr/bin/env bash
# ==========================================================
#  Aperod Monitoring Stack Installer
#  Устанавливает: node_exporter, nginx-prometheus-exporter, Prometheus, Grafana
#  Использование: sudo bash install-monitoring.sh
#  Поддерживается: Ubuntu 22.04 / 24.04 / Debian 12
# ==========================================================
set -euo pipefail

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'
CYAN='\033[0;36m'; BOLD='\033[1m'; NC='\033[0m'
info()  { echo -e "${CYAN}[INFO]${NC}  $*"; }
ok()    { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
die()   { echo -e "${RED}[ERR]${NC}   $*"; exit 1; }

[[ $(id -u) -ne 0 ]] && die "Запустите от root: sudo bash install-monitoring.sh"

echo -e "
${BOLD}╔═══════════════════════════════════════════════════╗
║   Aperod Monitoring Stack — Installer             ║
║   node_exporter + nginx-exporter + Prometheus     ║
║   + Grafana dashboard                             ║
╚═══════════════════════════════════════════════════╝${NC}
"

# ── Версии ────────────────────────────────────────────────
NODE_EXPORTER_VER="1.8.2"
NGINX_EXPORTER_VER="1.3.0"
ARCH=$( [[ $(uname -m) == "aarch64" ]] && echo "arm64" || echo "amd64" )

# ── 1. Системные зависимости ─────────────────────────────
info "Обновляем пакеты…"
apt-get update -q
apt-get install -y -q prometheus grafana curl wget nginx
ok "Базовые пакеты установлены"

# ── 2. node_exporter ─────────────────────────────────────
info "Устанавливаем node_exporter ${NODE_EXPORTER_VER}…"
NE_URL="https://github.com/prometheus/node_exporter/releases/download/v${NODE_EXPORTER_VER}/node_exporter-${NODE_EXPORTER_VER}.linux-${ARCH}.tar.gz"
wget -q "$NE_URL" -O /tmp/node_exporter.tar.gz
tar -xzf /tmp/node_exporter.tar.gz -C /tmp
cp "/tmp/node_exporter-${NODE_EXPORTER_VER}.linux-${ARCH}/node_exporter" /usr/local/bin/node_exporter
chmod +x /usr/local/bin/node_exporter
rm -rf /tmp/node_exporter.tar.gz "/tmp/node_exporter-${NODE_EXPORTER_VER}.linux-${ARCH}"

# Директория для textfile_collector (backup.sh пишет сюда .prom файлы)
TEXTFILE_DIR="/var/lib/node_exporter/textfile_collector"
mkdir -p "$TEXTFILE_DIR"

useradd --no-create-home --shell /bin/false node_exporter 2>/dev/null || true
chown -R node_exporter:node_exporter /var/lib/node_exporter

cat > /etc/systemd/system/node_exporter.service <<'EOF'
[Unit]
Description=Prometheus Node Exporter
After=network.target

[Service]
User=node_exporter
ExecStart=/usr/local/bin/node_exporter \
  --collector.textfile.directory=/var/lib/node_exporter/textfile_collector \
  --collector.systemd \
  --collector.processes
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now node_exporter
ok "node_exporter запущен на :9100"

# ── 3. nginx — включаем stub_status ──────────────────────
info "Настраиваем nginx stub_status…"
NGINX_STATUS_CONF="/etc/nginx/conf.d/stub_status.conf"
if [[ ! -f "$NGINX_STATUS_CONF" ]]; then
  cat > "$NGINX_STATUS_CONF" <<'EOF'
# nginx stub_status — только localhost (для nginx-prometheus-exporter)
server {
    listen 127.0.0.1:8080;
    server_name _;

    location /nginx_status {
        stub_status;
        allow 127.0.0.1;
        deny all;
    }
}
EOF
  nginx -t && systemctl reload nginx
  ok "nginx stub_status добавлен на 127.0.0.1:8080/nginx_status"
else
  warn "stub_status.conf уже существует, пропускаем"
fi

# ── 4. nginx-prometheus-exporter ─────────────────────────
info "Устанавливаем nginx-prometheus-exporter ${NGINX_EXPORTER_VER}…"
NE2_URL="https://github.com/nginx/nginx-prometheus-exporter/releases/download/v${NGINX_EXPORTER_VER}/nginx-prometheus-exporter_${NGINX_EXPORTER_VER}_linux_${ARCH}.tar.gz"
wget -q "$NE2_URL" -O /tmp/nginx_exporter.tar.gz
tar -xzf /tmp/nginx_exporter.tar.gz -C /tmp
cp /tmp/nginx-prometheus-exporter /usr/local/bin/nginx-prometheus-exporter
chmod +x /usr/local/bin/nginx-prometheus-exporter
rm -f /tmp/nginx_exporter.tar.gz /tmp/nginx-prometheus-exporter

useradd --no-create-home --shell /bin/false nginx_exporter 2>/dev/null || true

cat > /etc/systemd/system/nginx_exporter.service <<'EOF'
[Unit]
Description=Nginx Prometheus Exporter
After=network.target nginx.service

[Service]
User=nginx_exporter
ExecStart=/usr/local/bin/nginx-prometheus-exporter \
  --nginx.scrape-uri=http://127.0.0.1:8080/nginx_status \
  --web.listen-address=:9113
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now nginx_exporter
ok "nginx-prometheus-exporter запущен на :9113"

# ── 5. Prometheus — конфиг ────────────────────────────────
info "Настраиваем Prometheus…"
cp /opt/aperod/blockchain/deploy/prometheus.yml /etc/prometheus/prometheus.yml
systemctl enable --now prometheus
systemctl restart prometheus
ok "Prometheus запущен на :9090"

# ── 6. Grafana — конфиг ──────────────────────────────────
info "Настраиваем Grafana…"
GRAFANA_INI="/etc/grafana/grafana.ini"

# Удаляем старые вхождения (включая закомментированные) — чтобы не дублировать
sed -i '/^\s*;*\s*root_url\s*=/d'            "$GRAFANA_INI"
sed -i '/^\s*;*\s*serve_from_sub_path\s*=/d' "$GRAFANA_INI"

# Добавляем оба параметра сразу после [server]
if grep -q '^\[server\]' "$GRAFANA_INI"; then
  sed -i '/^\[server\]/a serve_from_sub_path = true\nroot_url = https://aperod.com/grafana/' "$GRAFANA_INI"
else
  # Секции [server] нет вообще — дописываем в конец
  cat >> "$GRAFANA_INI" <<'GEOF'

[server]
root_url = https://aperod.com/grafana/
serve_from_sub_path = true
GEOF
fi

systemctl enable --now grafana-server
systemctl restart grafana-server
ok "Grafana запущена на :3000 (путь /grafana/)"

# ── 7. Cron для backup.sh ─────────────────────────────────
info "Настраиваем cron для aperod_backup.sh…"
CRON_LINE="0 */12 * * * /usr/local/bin/aperod_backup.sh >> /var/log/aperod_backup.log 2>&1"
CRON_FILE="/etc/cron.d/aperod_backup"
echo "$CRON_LINE" > "$CRON_FILE"
chmod 644 "$CRON_FILE"
ok "Cron настроен: каждые 12 часов, лог: /var/log/aperod_backup.log"

# Убедимся, что скрипт бэкапа установлен
if [[ -f "/opt/aperod/blockchain/deploy/aperod_backup.sh" ]]; then
  cp /opt/aperod/blockchain/deploy/aperod_backup.sh /usr/local/bin/aperod_backup.sh
  chmod 700 /usr/local/bin/aperod_backup.sh
  ok "aperod_backup.sh установлен в /usr/local/bin/"
fi

# ── 8. Grafana dashboard ──────────────────────────────────
info "Импортируем Grafana dashboard…"
DASHBOARD_FILE="/opt/aperod/blockchain/deploy/grafana-dashboard.json"
if [[ -f "$DASHBOARD_FILE" ]]; then
  # Ждём пока Grafana поднимется
  for i in {1..10}; do
    if curl -s http://localhost:3000/api/health | grep -q '"database": "ok"'; then
      break
    fi
    sleep 3
  done

  curl -s -X POST http://admin:admin@localhost:3000/api/dashboards/db \
    -H "Content-Type: application/json" \
    -d "{\"dashboard\": $(cat "$DASHBOARD_FILE"), \"overwrite\": true, \"folderId\": 0}" \
    | grep -q '"status":"success"' && ok "Dashboard импортирован" || warn "Dashboard: импортируйте вручную через Grafana UI"
else
  warn "grafana-dashboard.json не найден, импортируйте вручную"
fi

# ── Итог ─────────────────────────────────────────────────
echo
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo -e "${GREEN}${BOLD}  ✓  Мониторинг установлен и запущен!${NC}"
echo -e "${GREEN}${BOLD}══════════════════════════════════════════════════════════${NC}"
echo
echo -e "  ${BOLD}Сервисы:${NC}"
echo -e "  Prometheus           → http://localhost:9090"
echo -e "  Grafana              → https://aperod.com/grafana/"
echo -e "  node_exporter        → http://localhost:9100/metrics"
echo -e "  nginx-exporter       → http://localhost:9113/metrics"
echo -e "  Backup log           → /var/log/aperod_backup.log"
echo -e "  Backup metrics       → /var/lib/node_exporter/textfile_collector/aperod_backup.prom"
echo
echo -e "  ${BOLD}Grafana первый вход:${NC} admin / admin (смените пароль!)"
echo -e "  ${BOLD}Добавьте datasource:${NC} http://localhost:9090 (тип: Prometheus)"
echo -e "  ${BOLD}Импортируйте dashboard:${NC} blockchain/deploy/grafana-dashboard.json"
echo
echo -e "  ${BOLD}Диагностика:${NC}"
echo -e "  systemctl status node_exporter nginx_exporter prometheus grafana-server"
echo -e "  curl -s http://localhost:9100/metrics | grep aperod_backup"
echo -e "  curl -s http://localhost:9113/metrics | grep nginx_"
echo
