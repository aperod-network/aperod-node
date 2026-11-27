#!/usr/bin/env bash
# Install and configure the protected Mainnet monitoring stack on the new OVH
# only. This deliberately never restarts or edits the block producer or API.
set -euo pipefail

SOURCE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXPECTED_OVH_IPV4="146.59.82.116"
EXPECTED_HOSTNAME="aperod-ovh1"
NODE_CONFIG="/var/lib/aperod/mainnet/node.yaml"
API_ENV="/etc/aperod-api-mainnet.env"
NGINX_SITE="/etc/nginx/sites-available/aperod-mainnet"
BACKUP_DIR="/var/backups/aperod-monitoring/$(date -u +%Y%m%dT%H%M%SZ)"
TEMP_DIR="$(mktemp -d /tmp/aperod-monitoring-setup.XXXXXX)"
BACKUP_LIST="$TEMP_DIR/backup-list"
touch "$BACKUP_LIST"
SUCCESS=0
PERSISTENT_CONFIG_CHANGED=0
EXPORTER_INSTALLED=0
EXPORTER_WAS_ACTIVE=0
EXPORTER_WAS_ENABLED=0
MASKED_SERVICES=()
rollback_and_cleanup() {
  local status=$?
  trap - EXIT
  # Keep the exporter running on a successful EXIT; stop it only before rollback.
  if [[ "$status" -ne 0 && "$SUCCESS" -eq 0 && "$EXPORTER_INSTALLED" -eq 1 ]]; then
    systemctl stop aperod-validator-health-exporter.service >/dev/null 2>&1 || true
  fi
  for unit in "${MASKED_SERVICES[@]}"; do
    if [[ -L "/etc/systemd/system/$unit" && "$(readlink "/etc/systemd/system/$unit")" == "/dev/null" ]]; then
      rm -f "/etc/systemd/system/$unit"
    fi
  done
  if [[ "$status" -ne 0 && "$SUCCESS" -eq 0 && -s "$BACKUP_LIST" ]]; then
    printf '[WARN] Setup failed; restoring the previous monitoring configuration.\n' >&2
    while IFS=$'\t' read -r state path; do
      [[ -n "$path" ]] || continue
      if [[ "$state" == PRESENT ]]; then
        rm -f -- "$path"
        install -d -m 0755 "$(dirname "$path")"
        cp -a -- "$BACKUP_DIR$path" "$path"
      else
        rm -f -- "$path"
      fi
    done < "$BACKUP_LIST"
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [[ "$EXPORTER_INSTALLED" -eq 1 ]]; then
      if [[ "$EXPORTER_WAS_ENABLED" -eq 1 ]]; then
        systemctl enable aperod-validator-health-exporter.service >/dev/null 2>&1 || true
      else
        systemctl disable aperod-validator-health-exporter.service >/dev/null 2>&1 || true
      fi
      if [[ "$EXPORTER_WAS_ACTIVE" -eq 1 ]]; then
        systemctl restart aperod-validator-health-exporter.service >/dev/null 2>&1 || true
      fi
    fi
    systemctl restart prometheus.service >/dev/null 2>&1 || true
    systemctl restart prometheus-alertmanager.service >/dev/null 2>&1 || true
    systemctl restart prometheus-node-exporter.service >/dev/null 2>&1 || true
    systemctl restart grafana-server.service >/dev/null 2>&1 || true
    nginx -t >/dev/null 2>&1 && systemctl reload nginx.service >/dev/null 2>&1 || true
    if [[ "$PERSISTENT_CONFIG_CHANGED" -eq 0 ]]; then
      rm -rf -- "$BACKUP_DIR"
    fi
  fi
  rm -rf -- "$TEMP_DIR"
  exit "$status"
}
trap rollback_and_cleanup EXIT

die() { printf '[ERROR] %s\n' "$*" >&2; exit 1; }
info() { printf '[INFO] %s\n' "$*"; }

[[ "$(id -u)" -eq 0 ]] || die "Run this script with sudo."
[[ "$(hostname -s)" == "$EXPECTED_HOSTNAME" ]] || die "Refusing to configure a host other than the new OVH."
ip -4 -o addr show | awk '{print $4}' | cut -d/ -f1 | grep -qx "$EXPECTED_OVH_IPV4" \
  || die "This host does not have the expected new OVH IPv4 address."
systemctl is-active --quiet aperod-node.service || die "Mainnet node is not active; no monitoring changes made."
[[ -r "$NODE_CONFIG" ]] || die "The Mainnet node config is missing."
grep -Eq '^[[:space:]]*network:[[:space:]]*mainnet([[:space:]#]|$)' "$NODE_CONFIG" \
  || die "The active node config is not explicitly Mainnet."
[[ -r "$API_ENV" ]] || die "The API environment file is missing."
[[ -f "$NGINX_SITE" ]] || die "The active Aperod nginx site is missing."
systemctl is-active --quiet grafana-server.service || die "Existing Grafana is not active; inspect it before configuration."
systemctl is-active --quiet prometheus-node-exporter.service \
  || die "The existing node_exporter service is not active; inspect it before configuration."
[[ "$(curl --noproxy '*' -sS -o /dev/null -w '%{http_code}' --max-time 4 http://127.0.0.1:3000/api/health)" == "200" ]] \
  || die "Existing Grafana health check failed."
for binary in \
  /usr/local/bin/prometheus /usr/local/bin/promtool \
  /usr/local/bin/prometheus-alertmanager /usr/local/bin/amtool \
  /usr/local/bin/prometheus-node-exporter; do
  [[ -x "$binary" ]] || die "The verified monitoring binary is missing: $binary"
done
/usr/local/bin/prometheus --version 2>&1 | grep -q 'version 3.16.0' \
  || die "Expected Prometheus 3.16.0; refuse to configure an unverified release."
/usr/local/bin/prometheus-alertmanager --version 2>&1 | grep -q 'version 0.35.1' \
  || die "Expected Alertmanager 0.35.1; refuse to configure an unverified release."
/usr/local/bin/prometheus-node-exporter --version 2>&1 | grep -q 'version 1.13.1' \
  || die "Expected node_exporter 1.13.1; refuse to configure an unverified release."
[[ -f "$SOURCE_DIR/prometheus.yml" && -f "$SOURCE_DIR/aperod-mainnet-alerts.yml" ]] \
  || die "Monitoring source files are missing."
[[ -f "$SOURCE_DIR/validator_health_exporter.py" \
  && -f "$SOURCE_DIR/aperod-validator-health-exporter.service" ]] \
  || die "Local validator exporter source or systemd unit is missing."
if [[ -L /etc/systemd/system/aperod-validator-health-exporter.service \
  || ( -e /etc/systemd/system/aperod-validator-health-exporter.service \
    && ! -f /etc/systemd/system/aperod-validator-health-exporter.service ) ]]; then
  die "A non-file exporter service unit already exists; inspect it before continuing."
fi
if [[ -f /etc/systemd/system/aperod-validator-health-exporter.service ]] \
  && ! grep -Fq 'Aperod Mainnet local validator health metrics exporter' \
    /etc/systemd/system/aperod-validator-health-exporter.service; then
  die "A different validator exporter unit already exists; inspect it before continuing."
fi
if ss -H -ltn 'sport = :18547' | grep -q .; then
  die "Local exporter port 18547 is already in use; no monitoring changes made."
fi
if systemctl is-active --quiet aperod-validator-health-exporter.service; then
  EXPORTER_WAS_ACTIVE=1
fi
if systemctl is-enabled --quiet aperod-validator-health-exporter.service; then
  EXPORTER_WAS_ENABLED=1
fi

install -d -m 0700 -o root -g root "$BACKUP_DIR"
backup_file() {
  local path="$1"
  if [[ -e "$path" ]]; then
    install -d -m 0700 -o root -g root "$BACKUP_DIR$(dirname "$path")"
    cp -a -- "$path" "$BACKUP_DIR$path"
    printf 'PRESENT\t%s\n' "$path" >> "$BACKUP_LIST"
  else
    printf 'MISSING\t%s\n' "$path" >> "$BACKUP_LIST"
  fi
}

backup_file /etc/prometheus/prometheus.yml
backup_file /etc/prometheus/rules/aperod-mainnet.yml
backup_file /usr/local/lib/aperod/validator_health_exporter.py
backup_file /etc/systemd/system/aperod-validator-health-exporter.service
backup_file /etc/alertmanager/alertmanager.yml
backup_file /etc/alertmanager/telegram-bot-token
backup_file /etc/grafana/grafana.ini
backup_file /etc/grafana/provisioning/dashboards/aperod-mainnet.yml
backup_file /etc/systemd/system/prometheus-alertmanager.service.d/override.conf
backup_file /etc/systemd/system/prometheus-node-exporter.service.d/override.conf
backup_file /etc/systemd/system/grafana-server.service.d/override.conf
backup_file /etc/nginx/sites-available/aperod-mainnet

for unit in prometheus-alertmanager.service; do
  if systemctl is-active --quiet "$unit"; then
    dpkg-query -W -f='${Status}' prometheus-alertmanager 2>/dev/null | grep -qx 'install ok installed' \
      || die "An unmanaged Alertmanager is active; refusing to replace it."
    continue
  fi
  unit_path="/etc/systemd/system/$unit"
  [[ ! -e "$unit_path" && ! -L "$unit_path" ]] \
    || die "A custom systemd unit already exists at $unit_path; inspect it before continuing."
  ln -s /dev/null "$unit_path"
  MASKED_SERVICES+=("$unit")
done
systemctl daemon-reload

info "Refreshing signed package indexes and installing Alertmanager."
apt-get update -qq
apt-get install --no-upgrade -y -qq ca-certificates curl python3
apt-get install --no-upgrade -y -qq prometheus-alertmanager

for unit in "${MASKED_SERVICES[@]}"; do
  rm -f "/etc/systemd/system/$unit"
done
MASKED_SERVICES=()
systemctl daemon-reload

AM_SERVICE="prometheus-alertmanager.service"
AM_USER="$(systemctl show -p User --value "$AM_SERVICE")"
[[ -n "$AM_USER" ]] || AM_USER="prometheus"
AM_GROUP="$(id -gn "$AM_USER")"
PROM_USER="$(systemctl show -p User --value prometheus.service)"
[[ -n "$PROM_USER" ]] || PROM_USER="prometheus"
PROM_GROUP="$(id -gn "$PROM_USER")"

PERSISTENT_CONFIG_CHANGED=1
install -d -m 0755 -o root -g "$PROM_GROUP" /etc/prometheus/rules
install -o root -g "$PROM_GROUP" -m 0640 "$SOURCE_DIR/prometheus.yml" /etc/prometheus/prometheus.yml
install -o root -g "$PROM_GROUP" -m 0640 "$SOURCE_DIR/aperod-mainnet-alerts.yml" /etc/prometheus/rules/aperod-mainnet.yml
  /usr/local/bin/promtool check rules /etc/prometheus/rules/aperod-mainnet.yml
  /usr/local/bin/promtool check config /etc/prometheus/prometheus.yml

install -d -o root -g root -m 0755 /usr/local/lib/aperod
install -o root -g root -m 0644 "$SOURCE_DIR/validator_health_exporter.py" \
  /usr/local/lib/aperod/validator_health_exporter.py
install -o root -g root -m 0644 "$SOURCE_DIR/aperod-validator-health-exporter.service" \
  /etc/systemd/system/aperod-validator-health-exporter.service

info "Loading only the existing admin Telegram bot token and effective admin chat."
python3 - "$API_ENV" "$TEMP_DIR/token" <<'PY'
import pathlib, re, sys
env_file, output = map(pathlib.Path, sys.argv[1:])
values = {}
for raw in env_file.read_text(encoding="utf-8").splitlines():
    line = raw.strip()
    if not line or line.startswith("#") or "=" not in line:
        continue
    key, value = line.split("=", 1)
    if key.strip() == "TELEGRAM_BOT_TOKEN":
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[key.strip()] = value
token = values.get("TELEGRAM_BOT_TOKEN", "")
if not re.fullmatch(r"[0-9]{5,}:[A-Za-z0-9_-]{20,}", token):
    raise SystemExit("The existing admin Telegram bot token is missing or malformed.")
output.write_text(token, encoding="utf-8")
output.chmod(0o600)
PY
install -d -m 0750 -o root -g "$AM_GROUP" /etc/alertmanager
install -o root -g "$AM_GROUP" -m 0640 "$TEMP_DIR/token" /etc/alertmanager/telegram-bot-token

ADMIN_CHAT_ID="$(runuser -u postgres -- psql --no-psqlrc -X -A -t -d aperod_api_mainnet \
  -c 'SELECT chat_id FROM telegram_config WHERE id = 1' 2>/dev/null | tr -d '\r\n' || true)"
if [[ -z "$ADMIN_CHAT_ID" ]]; then
  ADMIN_CHAT_ID="$(python3 - "$API_ENV" <<'PY'
import pathlib, sys
for raw in pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    line = raw.strip()
    if line.startswith("ADMIN_TELEGRAM_CHAT_ID="):
        value = line.split("=", 1)[1].strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        print(value)
        break
PY
)"
fi
[[ "$ADMIN_CHAT_ID" =~ ^-?[0-9]{1,20}$ && "$ADMIN_CHAT_ID" != "0" ]] \
  || die "The effective admin Telegram chat ID is not configured; no message was sent."
sed "s/__ADMIN_TELEGRAM_CHAT_ID__/${ADMIN_CHAT_ID}/" "$SOURCE_DIR/alertmanager.yml.template" \
  > "$TEMP_DIR/alertmanager.yml"
install -o root -g "$AM_GROUP" -m 0640 "$TEMP_DIR/alertmanager.yml" /etc/alertmanager/alertmanager.yml
install -d -m 0750 -o "$AM_USER" -g "$AM_GROUP" /var/lib/prometheus/alertmanager
install -d -m 0755 /etc/systemd/system/"$AM_SERVICE".d
cat > /etc/systemd/system/"$AM_SERVICE".d/override.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/prometheus-alertmanager --config.file=/etc/alertmanager/alertmanager.yml --storage.path=/var/lib/prometheus/alertmanager --web.listen-address=127.0.0.1:9093
EOF
/usr/local/bin/amtool check-config /etc/alertmanager/alertmanager.yml

NODE_EXPORTER_SERVICE="prometheus-node-exporter.service"
install -d -m 0755 /etc/systemd/system/"$NODE_EXPORTER_SERVICE".d
cat > /etc/systemd/system/"$NODE_EXPORTER_SERVICE".d/override.conf <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/local/bin/prometheus-node-exporter --web.listen-address=127.0.0.1:9100 --collector.systemd
EOF

install -d -m 0755 /etc/grafana/provisioning/dashboards
install -d -m 0755 /var/lib/grafana/dashboards/aperod-mainnet
install -d -m 0755 /etc/systemd/system/grafana-server.service.d
cat > /etc/systemd/system/grafana-server.service.d/override.conf <<'EOF'
[Service]
Environment=GF_PLUGINS_PREINSTALL_DISABLED=true
Environment=GF_PLUGINS_PREINSTALL_AUTO_UPDATE=false
EOF
GRAFANA_DATASOURCE_UID="$(python3 - /var/lib/grafana/grafana.db <<'PY'
import pathlib, sqlite3, sys
path = pathlib.Path(sys.argv[1])
if not path.exists():
    print("prometheus")
else:
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    try:
        row = conn.execute("SELECT uid FROM data_source WHERE name = ? AND type = ? ORDER BY is_default DESC LIMIT 1",
                           ("prometheus", "prometheus")).fetchone()
        print(row[0] if row and row[0] else "prometheus")
    finally:
        conn.close()
PY
)"
[[ "$GRAFANA_DATASOURCE_UID" =~ ^[A-Za-z0-9_-]+$ ]] \
  || die "Existing Grafana Prometheus datasource UID is invalid."
install -o root -g grafana -m 0640 "$SOURCE_DIR/grafana-dashboard-provider.yml" \
  /etc/grafana/provisioning/dashboards/aperod-mainnet.yml
python3 "$SOURCE_DIR/render-grafana-dashboard.py" "$SOURCE_DIR/grafana-dashboard.json" \
  "$TEMP_DIR/aperod-node-overview.json" "$GRAFANA_DATASOURCE_UID"
install -o root -g grafana -m 0640 "$TEMP_DIR/aperod-node-overview.json" \
  /var/lib/grafana/dashboards/aperod-mainnet/aperod-node-overview.json

python3 - /etc/grafana/grafana.ini <<'PY'
import pathlib, re, sys
path = pathlib.Path(sys.argv[1])
lines = path.read_text(encoding="utf-8").splitlines()
settings = {
    "server": {
        "http_addr": "127.0.0.1",
        "http_port": "3000",
        "domain": "aperod.com",
        "root_url": "https://aperod.com/grafana/",
        "serve_from_sub_path": "true",
    },
    "auth": {"disable_login_form": "true"},
    "plugins": {"preinstall_disabled": "true", "preinstall_auto_update": "false"},
    "auth.proxy": {
        "enabled": "true",
        "header_name": "X-WEBAUTH-USER",
        "header_property": "username",
        "auto_sign_up": "true",
        "sync_ttl": "0",
        "whitelist": "127.0.0.1",
        "headers": "Role:X-WEBAUTH-ROLE",
        "enable_login_token": "false",
    },
    "users": {"auto_assign_org_role": "Admin"},
    "security": {"content_security_policy": "true"},
    "analytics": {"reporting_enabled": "false", "check_for_updates": "false"},
}
for section, values in settings.items():
    start = next((i for i, line in enumerate(lines) if line.strip() == f"[{section}]"), None)
    if start is None:
        if lines and lines[-1].strip():
            lines.append("")
        lines.append(f"[{section}]")
        start = len(lines) - 1
    end = next((i for i in range(start + 1, len(lines)) if re.match(r"^\s*\[[^\]]+\]\s*$", lines[i])), len(lines))
    for key, value in values.items():
        pattern = re.compile(rf"^\s*{re.escape(key)}\s*=")
        positions = [i for i in range(start + 1, end) if pattern.match(lines[i])]
        if positions:
            lines[positions[0]] = f"{key} = {value}"
            for i in reversed(positions[1:]):
                del lines[i]
                end -= 1
        else:
            lines.insert(end, f"{key} = {value}")
            end += 1
path.write_text("\n".join(lines) + "\n", encoding="utf-8")
PY

python3 - "$NGINX_SITE" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
needle = "    location /grafana/ {"
if text.count(needle) != 1:
    raise SystemExit("Expected exactly one Grafana proxy block; nginx was not changed.")
start = text.index(needle)
end = text.index("\n    }", start) + len("\n    }")
block = text[start:end]
if "auth_request /_admin_gate_check;" not in block:
    block = block.replace(needle, needle + "\n        auth_request /_admin_gate_check;\n        error_page 401 =404 @admin_gate_not_found;", 1)
if "proxy_set_header    X-WEBAUTH-USER" not in block:
    anchor = "        proxy_set_header    X-Forwarded-Proto $scheme;"
    if anchor not in block:
        raise SystemExit("Could not find the trusted Grafana proxy-header insertion point.")
    block = block.replace(anchor, anchor + "\n        proxy_set_header    X-WEBAUTH-USER    aperod-admin;\n        proxy_set_header    X-WEBAUTH-ROLE    Admin;", 1)
if "grafana-stateless-proxy-cookies" not in block:
    anchor = "        proxy_set_header    X-WEBAUTH-ROLE    Admin;"
    if anchor not in block:
        raise SystemExit("Could not find the trusted Grafana role-header insertion point.")
    cookie_guard = """        # grafana-stateless-proxy-cookies: Admin Gate is checked by auth_request.
        # Grafana authenticates using trusted proxy headers; discard stale browser sessions.
        proxy_set_header    Cookie            "";
        proxy_hide_header   Set-Cookie;
        add_header Set-Cookie "grafana_session=; Path=/grafana/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Secure; HttpOnly; SameSite=Lax" always;
        add_header Set-Cookie "grafana_session_expiry=; Path=/grafana/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Secure; SameSite=Lax" always;
        add_header Set-Cookie "grafana_session=; Path=/grafana; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Secure; HttpOnly; SameSite=Lax" always;
        add_header Set-Cookie "grafana_session_expiry=; Path=/grafana; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Secure; SameSite=Lax" always;
        add_header Set-Cookie "grafana_session=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Secure; HttpOnly; SameSite=Lax" always;
        add_header Set-Cookie "grafana_session_expiry=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Max-Age=0; Secure; SameSite=Lax" always;"""
    block = block.replace(anchor, anchor + "\n" + cookie_guard, 1)
text = text[:start] + block + text[end:]
path.write_text(text, encoding="utf-8")
PY

nginx -t
/usr/local/bin/promtool check config /etc/prometheus/prometheus.yml
/usr/local/bin/amtool check-config /etc/alertmanager/alertmanager.yml

systemctl daemon-reload
EXPORTER_INSTALLED=1
systemctl enable --now aperod-validator-health-exporter.service
systemctl restart aperod-validator-health-exporter.service
systemctl enable --now prometheus-alertmanager.service
systemctl restart prometheus-alertmanager.service
systemctl restart prometheus-node-exporter.service
systemctl restart prometheus.service
systemctl enable --now grafana-server.service
systemctl restart grafana-server.service
systemctl reload nginx.service

info "Monitoring services are up; the Mainnet node and API were not restarted."
systemctl is-active aperod-validator-health-exporter.service prometheus.service prometheus-alertmanager.service prometheus-node-exporter.service grafana-server.service nginx.service
for attempt in $(seq 1 30); do
  if curl --noproxy '*' -fsS --max-time 2 http://127.0.0.1:3000/api/health >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl --noproxy '*' -fsS --max-time 4 http://127.0.0.1:3000/api/health >/dev/null \
  || die "Grafana did not remain healthy after provisioning."
curl --noproxy '*' -fsS --max-time 4 \
  -H 'X-WEBAUTH-USER: aperod-admin' -H 'X-WEBAUTH-ROLE: Admin' \
  http://127.0.0.1:3000/api/user >/dev/null \
  || die "Grafana auth-proxy check failed."
DASHBOARD_VISIBLE=0
for attempt in $(seq 1 60); do
  if curl --noproxy '*' -fsS --max-time 4 \
    -H 'X-WEBAUTH-USER: aperod-admin' -H 'X-WEBAUTH-ROLE: Admin' \
    'http://127.0.0.1:3000/api/search?query=*&type=dash-db' \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if any(x.get("uid")=="aperod-node-overview" for x in d) else 1)' \
    2>/dev/null; then
    DASHBOARD_VISIBLE=1
    break
  fi
  sleep 1
done
[[ "$DASHBOARD_VISIBLE" -eq 1 ]] || die "Provisioned dashboard did not become visible."
GRAFANA_CSP="$(curl --noproxy '*' -sS --max-time 4 -D - -o /dev/null \
  http://127.0.0.1:3000/grafana/login 2>/dev/null || true)"
grep -qiE '^Content-Security-Policy:.*nonce-' <<< "$GRAFANA_CSP" \
  || die "Grafana nonce CSP header is missing."
curl --noproxy '*' -fsS --max-time 4 http://127.0.0.1:9093/-/healthy >/dev/null \
  || die "Alertmanager health check failed."
curl --noproxy '*' -k -sS --resolve aperod.com:443:127.0.0.1 --max-time 4 \
  -o /dev/null -w '%{http_code}' https://aperod.com/grafana/ \
  | grep -qx '404' || die "Grafana is not returning 404 to unauthenticated requests."
TARGETS_UP=0
for attempt in $(seq 1 30); do
  if curl --noproxy '*' -fsS --max-time 4 http://127.0.0.1:9090/api/v1/targets \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); ts=d["data"]["activeTargets"]; sys.exit(0 if ts and all(t.get("health")=="up" for t in ts) else 1)' \
    2>/dev/null; then
    TARGETS_UP=1
    break
  fi
  sleep 1
done
[[ "$TARGETS_UP" -eq 1 ]] || die "One or more Prometheus targets are still down after restart."
STATUS_EXPORTER_READY=0
for attempt in $(seq 1 30); do
  if VALIDATOR_METRICS="$(curl --noproxy '*' -fsS --max-time 4 http://127.0.0.1:18547/metrics 2>/dev/null)" \
    && grep -qx 'aperod_validator_monitor_status_available 1' <<< "$VALIDATOR_METRICS"; then
    STATUS_EXPORTER_READY=1
    break
  fi
  sleep 1
done
[[ "$STATUS_EXPORTER_READY" -eq 1 ]] \
  || die "The local validator status exporter cannot read the node's API."
SUCCESS=1
