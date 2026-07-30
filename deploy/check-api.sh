#!/usr/bin/env bash
# =============================================================
#  check-api.sh — быстрая диагностика aperod-api на сервере
#  Запуск: bash /opt/aperod/blockchain/deploy/check-api.sh
# =============================================================
set -euo pipefail
API_PORT=${API_PORT:-3001}
DIST=/opt/aperod/artifacts/api-server/dist/index.mjs

echo "━━━ PM2 STATUS ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
pm2 status

echo ""
echo "━━━ BUILD VERSION CHECK ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -n "X-Real-IP code in dist:      "
grep -c "x-real-ip" "$DIST" 2>/dev/null && echo " hit(s)" || echo "0 hits — OLD BUILD"
echo -n "New rate-limit label in dist: "
grep -c "10 req" "$DIST" 2>/dev/null && echo " hit(s)" || echo "0 hits — OLD BUILD"
echo -n "pool.query in dist:           "
grep -c "pool.query" "$DIST" 2>/dev/null && echo " hit(s)" || echo "0 hits — OLD BUILD"
echo -n "auto-ban code in dist:        "
grep -c "AUTO_BAN" "$DIST" 2>/dev/null && echo " hit(s)" || echo "0 hits — OLD BUILD"
echo -n "Dist last modified:           "
stat -c "%y" "$DIST" 2>/dev/null || echo "not found"

echo ""
echo "━━━ DATABASE_URL SET ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
pm2 env 0 2>/dev/null | grep DATABASE_URL | sed 's/=.*/=<HIDDEN>/' || echo "Could not read PM2 env"

echo ""
echo "━━━ LAST 20 ERROR LOG LINES ━━━━━━━━━━━━━━━━━━━━━━━━━━━"
pm2 logs aperod-api --lines 20 --nostream --err 2>/dev/null || echo "(no logs)"

echo ""
echo "━━━ HOURLY ENDPOINT (direct to :${API_PORT}) ━━━━━━━━━━━━━━━━━"
curl -sf "http://127.0.0.1:${API_PORT}/api/admin/notification-log/hourly?window=60&bucket=60" \
     -o /tmp/hourly_resp.json \
     -w "HTTP %{http_code}\n" \
     -H "Cookie: admin_token=diagnostic" 2>/dev/null \
  && echo "Response body:" && cat /tmp/hourly_resp.json | head -c 500 \
  || echo "(connection refused — server not listening on :${API_PORT})"

echo ""
echo "━━━ HEALTHZ ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
curl -sf "http://127.0.0.1:${API_PORT}/healthz" -w " HTTP %{http_code}\n" 2>/dev/null || echo "FAILED"

echo ""
echo "━━━ BAN LIST ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
curl -sf "http://127.0.0.1:${API_PORT}/api/admin/security/bans" \
     -H "Cookie: admin_token=diagnostic" 2>/dev/null | head -c 500 || echo "(no response)"

echo ""
echo "━━━ NGINX RATE LIMIT ACTIVE IPs ━━━━━━━━━━━━━━━━━━━━━━━"
journalctl -u nginx --since "5 minutes ago" --no-pager 2>/dev/null \
  | grep "limiting requests" | awk '{print $NF}' | sort | uniq -c | sort -rn | head -10 \
  || echo "(no nginx journal data)"

echo ""
echo "Done."
