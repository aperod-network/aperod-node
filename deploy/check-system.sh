#!/bin/bash
# ==========================================================
# Aperod System Diagnostic Script
# Запуск: sudo bash blockchain/deploy/check-system.sh
#
# Проверяет:
#   1. Переменные окружения API-сервера
#   2. Файл integration-settings.json (ключи настроены?)
#   3. API-сервер: health, ключи интеграций (redacted)
#   4. Курсы TON/TRX/BTC с биржи
#   5. TronGrid — баланс TRX тестового адреса
#   6. NowNodes — баланс BTC тестового адреса
#   7. Подключение к S3/Backblaze (rclone)
#   8. rclone установлен?
#   9. APEROD_BACKUP_PASSWORD задан?
#  10. Статус бэкап-cron
#  11. Textfile collector (метрики Prometheus)
# ==========================================================

RED='\033[0;31m'; YEL='\033[1;33m'; GRN='\033[0;32m'; NC='\033[0m'; BLD='\033[1m'
ok()   { echo -e "  ${GRN}✓${NC} $*"; }
warn() { echo -e "  ${YEL}⚠${NC}  $*"; }
err()  { echo -e "  ${RED}✗${NC} $*"; }
hdr()  { echo -e "\n${BLD}═══ $* ═══${NC}"; }

DATA_DIR="${DATA_DIR:-/opt/aperod/data}"
SETTINGS="${DATA_DIR}/integration-settings.json"
API_BASE="${API_BASE:-http://127.0.0.1:3001}"

# ────────────────────────────────────────────────────────────────────────────
hdr "1. Переменные окружения API-сервера"

# DATA_DIR из systemd unit
UNIT_DATA_DIR=$(systemctl show aperod-api --property=Environment 2>/dev/null \
  | grep -o 'DATA_DIR=[^ ]*' | cut -d= -f2)
if [ -n "$UNIT_DATA_DIR" ]; then
  ok "DATA_DIR в systemd unit: $UNIT_DATA_DIR"
  DATA_DIR="$UNIT_DATA_DIR"
  SETTINGS="${DATA_DIR}/integration-settings.json"
else
  warn "DATA_DIR не задан в systemd unit aperod-api"
  echo "     → Добавьте: sudo systemctl edit aperod-api"
  echo "       [Service]"
  echo "       Environment=DATA_DIR=/opt/aperod/data"
  echo "     Текущий DATA_DIR: ${DATA_DIR} (предположительно)"
fi

BACKUP_PASS=$(systemctl show aperod-api --property=Environment 2>/dev/null \
  | grep -o 'APEROD_BACKUP_PASSWORD=[^ ]*' | cut -d= -f2)
if [ -n "$BACKUP_PASS" ]; then
  ok "APEROD_BACKUP_PASSWORD задан в systemd unit"
elif [ -n "${APEROD_BACKUP_PASSWORD:-}" ]; then
  ok "APEROD_BACKUP_PASSWORD задан в среде shell"
else
  warn "APEROD_BACKUP_PASSWORD не задан"
  echo "     → sudo bash -c 'echo \"APEROD_BACKUP_PASSWORD=ВАШ_ПАРОЛЬ\" >> /etc/environment'"
  echo "     → или в systemd: Environment=APEROD_BACKUP_PASSWORD=..."
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "2. Файл integration-settings.json"

if [ ! -f "$SETTINGS" ]; then
  err "Файл не найден: ${SETTINGS}"
  echo "     Возможные причины:"
  echo "     а) DATA_DIR задан неверно (проверьте п.1 выше)"
  echo "     б) Ключи ещё не сохранялись через /admin-panel/integrations"
  echo ""
  echo "     Проверить что видит API: ls -la ${DATA_DIR}/"
  ls -la "${DATA_DIR}/" 2>/dev/null || echo "     (директория не существует или нет доступа)"
else
  ok "Файл найден: ${SETTINGS}"
  echo "     Размер: $(wc -c < "$SETTINGS") байт  |  Изменён: $(date -r "$SETTINGS" '+%Y-%m-%d %H:%M:%S')"

  _py() { python3 -c "import json; d=json.load(open('${SETTINGS}')); print($1)" 2>/dev/null || echo "(ошибка)"; }

  # ChangeNOW
  CN_KEY=$(_py "len(d.get('changenow',{}).get('apiKey',''))")
  CN_EN=$(_py "d.get('changenow',{}).get('enabled',False)")
  [ "$CN_KEY" != "0" ] && ok "ChangeNOW: ключ настроен (${CN_KEY} симв.), enabled=${CN_EN}" \
                        || warn "ChangeNOW: ключ НЕ задан (enabled=${CN_EN})"

  # NowNodes
  NN_KEY=$(_py "len(d.get('nownodes',{}).get('apiKey',''))")
  [ "$NN_KEY" != "0" ] && ok "NowNodes: ключ настроен (${NN_KEY} симв.)" \
                        || err "NowNodes: ключ НЕ задан — BTC/LTC/DOGE/ZEC не будут работать"

  # TronGrid
  TG_KEY=$(_py "len(d.get('trongrid',{}).get('apiKey',''))")
  [ "$TG_KEY" != "0" ] && ok "TronGrid: ключ настроен (${TG_KEY} симв.)" \
                        || err "TronGrid: ключ НЕ задан — TRX/USDT-TRC20 будут работать на публичном лимите"

  # Ankr
  ANKR_KEY=$(_py "len(d.get('ankr',{}).get('apiKey',''))")
  ANKR_ETH=$(_py "d.get('ankr',{}).get('ethRpc','')")
  ANKR_BNB=$(_py "d.get('ankr',{}).get('bnbRpc','')")
  [ "$ANKR_KEY" != "0" ] && ok "Ankr: ключ настроен (${ANKR_KEY} симв.)" \
                           || warn "Ankr: ключ не задан (будут публичные RPC)"
  echo "     ETH RPC: ${ANKR_ETH}"
  echo "     BNB RPC: ${ANKR_BNB}"

  # S3
  S3_ENDPOINT=$(_py "d.get('s3backup',{}).get('endpoint','')")
  S3_BUCKET=$(_py "d.get('s3backup',{}).get('bucket','')")
  S3_REGION=$(_py "d.get('s3backup',{}).get('region','')")
  S3_ACC_LEN=$(_py "len(d.get('s3backup',{}).get('accessKeyId',''))")
  S3_SEC_LEN=$(_py "len(d.get('s3backup',{}).get('secretAccessKey',''))")
  S3_DAYS=$(_py "d.get('s3backup',{}).get('retentionDays',14)")

  if [ "$S3_ACC_LEN" != "0" ] && [ "$S3_SEC_LEN" != "0" ]; then
    ok "S3 backup: ключи настроены"
    echo "     endpoint=${S3_ENDPOINT}  bucket=${S3_BUCKET}  region=${S3_REGION}  retain=${S3_DAYS}д"
  else
    err "S3 backup: accessKeyId и/или secretAccessKey НЕ заданы"
    echo "     → Заполните раздел S3 в /admin-panel/integrations"
  fi
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "3. API-сервер"

HEALTH=$(curl -sf "${API_BASE}/api/healthz" 2>/dev/null) && ok "GET /api/healthz → OK: ${HEALTH}" \
  || { err "GET /api/healthz — API-сервер недоступен на ${API_BASE}"; echo "     Проверьте: sudo systemctl status aperod-api"; }

INT_PUB=$(curl -sf "${API_BASE}/api/v1/integrations/public" 2>/dev/null)
if [ -n "$INT_PUB" ]; then
  ok "GET /api/v1/integrations/public:"
  echo "$INT_PUB" | python3 -m json.tool 2>/dev/null | sed 's/^/     /'
else
  err "GET /api/v1/integrations/public — нет ответа"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "4. Курсы с биржи (Bybit / MEXC)"

echo "  Запрос TON, TRX, BTC через API-сервер..."
PRICES=$(curl -sf "${API_BASE}/api/v1/market-prices?coins=ton,trx,btc,eth,usdt" 2>/dev/null)
if [ -n "$PRICES" ]; then
  ok "GET /api/v1/market-prices:"
  echo "$PRICES" | python3 -m json.tool 2>/dev/null | sed 's/^/     /'
  # Проверяем что TON есть
  TON_PRICE=$(echo "$PRICES" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('prices',{}).get('ton','НЕТУ'))" 2>/dev/null)
  TRX_PRICE=$(echo "$PRICES" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('prices',{}).get('trx','НЕТУ'))" 2>/dev/null)
  [ "$TON_PRICE" != "НЕТУ" ] && ok "TON цена: \$${TON_PRICE}" || err "TON цена не получена — Bybit/MEXC заблокированы с этого IP?"
  [ "$TRX_PRICE" != "НЕТУ" ] && ok "TRX цена: \$${TRX_PRICE}" || err "TRX цена не получена"
else
  err "GET /api/v1/market-prices — нет ответа от API"
fi

# Прямая проверка Bybit (для диагностики сети)
echo ""
echo "  Прямой запрос к Bybit (TONUSDT)..."
BYBIT=$(curl -sf --max-time 8 \
  "https://api.bybit.com/v5/market/tickers?category=spot&symbol=TONUSDT" 2>/dev/null)
if [ -n "$BYBIT" ]; then
  TON_DIRECT=$(echo "$BYBIT" | python3 -c \
    "import json,sys; d=json.load(sys.stdin); print(d['result']['list'][0]['lastPrice'])" 2>/dev/null)
  ok "Bybit доступен напрямую: TON = \$${TON_DIRECT}"
else
  err "Bybit недоступен напрямую с сервера — IP может быть заблокирован"
  echo "     Попробуйте: curl -v https://api.bybit.com/v5/market/tickers?category=spot&symbol=TONUSDT"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "5. TronGrid API (TRX / USDT-TRC20)"

if [ -f "$SETTINGS" ]; then
  TG_APIKEY=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('trongrid',{}).get('apiKey',''))" 2>/dev/null)
fi

# Тестовый публичный TRX адрес (трон-основатель Justin Sun — всегда есть баланс)
TEST_TRX_ADDR="TN3W4H6rK2ce4vX9YnFQHwKENnHjoxb3m9"
echo "  Тестовый адрес: ${TEST_TRX_ADDR}"

TRON_ARGS=("-sf" "--max-time" "8")
[ -n "${TG_APIKEY:-}" ] && TRON_ARGS+=("-H" "TRON-PRO-API-KEY: ${TG_APIKEY}")

TRON_RESP=$(curl "${TRON_ARGS[@]}" \
  "https://api.trongrid.io/v1/accounts/${TEST_TRX_ADDR}" 2>/dev/null)
if [ -n "$TRON_RESP" ]; then
  TRX_BAL=$(echo "$TRON_RESP" | python3 -c \
    "import json,sys; d=json.load(sys.stdin); bal=d.get('data',[{}])[0].get('balance',0); print(f'{bal/1e6:.2f} TRX')" 2>/dev/null)
  ok "TronGrid доступен: баланс тестового адреса = ${TRX_BAL}"
  [ -n "${TG_APIKEY:-}" ] && echo "     (с API-ключом)" || warn "     (без API-ключа — публичный лимит)"
else
  err "TronGrid недоступен с сервера"
  echo "     curl -H 'TRON-PRO-API-KEY: ВАШ_КЛЮЧ' https://api.trongrid.io/v1/accounts/${TEST_TRX_ADDR}"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "6. NowNodes API (BTC)"

if [ -f "$SETTINGS" ]; then
  NN_APIKEY=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('nownodes',{}).get('apiKey',''))" 2>/dev/null)
fi

if [ -z "${NN_APIKEY:-}" ]; then
  err "NowNodes ключ не задан — пропускаем проверку"
  echo "     Без ключа адрес отображается, но баланс BTC не загрузится"
else
  # Проверяем ключ через /api/v2 (статус API, не требует адреса)
  echo "  Проверка ключа через btcbook.nownodes.io/api/v2 ..."
  NN_HTTP=$(curl -s -o /tmp/nn_resp.json -w "%{http_code}" --max-time 10 \
    "https://btcbook.nownodes.io/api/v2" \
    -H "api-key: ${NN_APIKEY}" 2>/dev/null)
  NN_RESP=$(cat /tmp/nn_resp.json 2>/dev/null)
  if [ "$NN_HTTP" = "200" ]; then
    NN_CHAIN=$(echo "$NN_RESP" | python3 -c \
      "import json,sys; d=json.load(sys.stdin); print(d.get('name','?'), 'blocks:', d.get('bestHeight','?'))" 2>/dev/null)
    ok "NowNodes доступен (HTTP 200): ${NN_CHAIN}"
    echo "     Ключ валиден — BTC/LTC/DOGE/ZEC балансы будут работать"
  else
    err "NowNodes ответил HTTP ${NN_HTTP:-timeout}"
    echo "     Ответ сервера: $(echo "$NN_RESP" | head -c 300)"
    echo ""
    if [ "$NN_HTTP" = "401" ] || [ "$NN_HTTP" = "403" ]; then
      echo "     → Ключ неверный. Зайдите nownodes.io → Dashboard → скопируйте API Key"
      echo "     → Вставьте в /admin-panel/integrations → NowNodes → Сохранить"
    elif [ "$NN_HTTP" = "429" ]; then
      echo "     → Превышен лимит запросов — подождите минуту"
    elif [ -z "$NN_HTTP" ]; then
      echo "     → Timeout. Проверьте сеть: curl -v https://btcbook.nownodes.io/"
    fi
  fi
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "7. rclone + S3/Backblaze"

if ! command -v rclone &>/dev/null; then
  err "rclone НЕ установлен"
  echo "     → curl https://rclone.org/install.sh | sudo bash"
else
  RCLONE_VER=$(rclone version 2>/dev/null | head -1)
  ok "rclone установлен: ${RCLONE_VER}"

  if [ -f "$SETTINGS" ]; then
    S3_EP=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('s3backup',{}).get('endpoint',''))" 2>/dev/null)
    S3_AC=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('s3backup',{}).get('accessKeyId',''))" 2>/dev/null)
    S3_SC=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('s3backup',{}).get('secretAccessKey',''))" 2>/dev/null)
    S3_BK=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('s3backup',{}).get('bucket','aperod-vault'))" 2>/dev/null)
    S3_RG=$(python3 -c "import json; d=json.load(open('${SETTINGS}')); print(d.get('s3backup',{}).get('region','us-west-004'))" 2>/dev/null)

    if [ -n "$S3_AC" ] && [ -n "$S3_SC" ] && [ -n "$S3_EP" ]; then
      echo "  Проверяем подключение к S3 (bucket=${S3_BK})..."
      LS_OUT=$(RCLONE_CONFIG_S3BACKUP_TYPE=s3 \
        RCLONE_CONFIG_S3BACKUP_PROVIDER=Other \
        RCLONE_CONFIG_S3BACKUP_ENDPOINT="$S3_EP" \
        RCLONE_CONFIG_S3BACKUP_ACCESS_KEY_ID="$S3_AC" \
        RCLONE_CONFIG_S3BACKUP_SECRET_ACCESS_KEY="$S3_SC" \
        RCLONE_CONFIG_S3BACKUP_REGION="$S3_RG" \
        rclone ls "s3backup:${S3_BK}" --s3-no-check-bucket 2>&1)
      RC=$?
      if [ $RC -eq 0 ]; then
        FILE_COUNT=$(echo "$LS_OUT" | grep -c '\.gpg' || true)
        ok "S3 подключение работает! Файлов в bucket: ${FILE_COUNT}"
        echo "$LS_OUT" | tail -5 | sed 's/^/     /'
      else
        err "S3 подключение НЕ работает (rclone exit ${RC}):"
        echo "$LS_OUT" | sed 's/^/     /'
        echo ""
        echo "     ── Диагностика по типу ошибки ──────────────────────────"
        if echo "$LS_OUT" | grep -q "Malformed Access Key"; then
          echo "     ОШИБКА: InvalidAccessKeyId — Malformed Access Key Id"
          echo ""
          echo "     Backblaze B2 использует ДВА разных значения:"
          echo "       1. keyID  (Application Key ID)  — начинается с «004» или «00»"
          echo "          Вставляется в поле «Access Key ID» в admin-panel"
          echo "       2. applicationKey  — длинная строка, показывается ОДИН РАЗ"
          echo "          Вставляется в поле «Secret Access Key» в admin-panel"
          echo ""
          echo "     Частая ошибка: оба поля заполнены одинаковым значением,"
          echo "     или keyID и applicationKey перепутаны местами."
          echo ""
          echo "     Как исправить:"
          echo "       1. Зайдите на backblaze.com → My Account → App Keys"
          echo "       2. Создайте новый ключ (старый applicationKey уже не вернуть)"
          echo "       3. Скопируйте keyID в «Access Key ID»"
          echo "       4. Скопируйте applicationKey в «Secret Access Key»"
          echo "       5. Сохраните в /admin-panel/integrations → S3"
        elif echo "$LS_OUT" | grep -q "InvalidAccessKeyId.*is not valid"; then
          echo "     ОШИБКА: InvalidAccessKeyId — endpoint не совпадает с регионом ключа"
          echo ""
          echo "     Текущий endpoint: ${S3_EP}"
          echo ""
          echo "     Backblaze B2 имеет несколько кластеров, и ключ привязан к конкретному:"
          echo "       keyID начинается с «002» → endpoint: s3.eu-central-003.backblazeb2.com"
          echo "       keyID начинается с «004» → endpoint: s3.us-west-004.backblazeb2.com"
          echo "       keyID начинается с «005» → endpoint: s3.us-east-005.backblazeb2.com"
          echo ""
          echo "     Текущий keyID: ${S3_AC}"
          echo "     Откройте bucket в Backblaze → Bucket Settings → поле «Endpoint»"
          echo "     Вставьте правильный endpoint в /admin-panel/integrations → S3"
          echo ""
          echo "     Или исправьте напрямую на сервере:"
          echo "       python3 -c \""
          echo "         import json; path='/opt/aperod/data/integration-settings.json'"
          echo "         d=json.load(open(path)); d.setdefault('s3backup',{})['endpoint']='https://s3.ПРАВИЛЬНЫЙ.backblazeb2.com'"
          echo "         json.dump(d, open(path,'w'), indent=2)\""
        elif echo "$LS_OUT" | grep -q "403\|AccessDenied"; then
          echo "     ОШИБКА 403 — ключ верный, но нет доступа к bucket '${S3_BK}'"
          echo "     • Убедитесь что ключ создан с доступом к bucket '${S3_BK}'"
          echo "     • Или создайте ключ с типом «All Buckets» в Backblaze"
        elif echo "$LS_OUT" | grep -q "NoSuchBucket\|does not exist"; then
          echo "     ОШИБКА — bucket '${S3_BK}' не существует"
          echo "     • Создайте bucket в Backblaze B2 с именем '${S3_BK}'"
          echo "     • Или измените «Имя bucket» в /admin-panel/integrations"
        elif echo "$LS_OUT" | grep -q "connection\|network\|timeout"; then
          echo "     ОШИБКА СЕТИ — не удалось подключиться к ${S3_EP}"
          echo "     • Проверьте endpoint URL в /admin-panel/integrations"
          echo "     • Для Backblaze us-west-004: https://s3.us-west-004.backblazeb2.com"
        fi
        echo "     ────────────────────────────────────────────────────────"
      fi
    else
      warn "S3 ключи не заданы в ${SETTINGS} — пропускаем проверку rclone"
    fi
  fi
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "8. GPG (шифрование бэкапов)"

if command -v gpg &>/dev/null; then
  ok "gpg установлен: $(gpg --version | head -1)"
else
  err "gpg НЕ установлен"
  echo "     → sudo apt-get install -y gnupg"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "9. APEROD_BACKUP_PASSWORD"

SECRETS_FILE="/etc/aperod/backup-secrets.env"
SECRETS_PASS=$(grep 'APEROD_BACKUP_PASSWORD' "${SECRETS_FILE}" 2>/dev/null | head -1)
ENV_PASS=$(grep 'APEROD_BACKUP_PASSWORD' /etc/environment 2>/dev/null | head -1)
SYSTEMD_PASS=$(systemctl show aperod-api --property=Environment 2>/dev/null | grep -o 'APEROD_BACKUP_PASSWORD=[^ ]*')
SHELL_PASS="${APEROD_BACKUP_PASSWORD:-}"

if [ -n "$SECRETS_PASS" ]; then
  SECRETS_PERMS=$(stat -c '%a %U:%G' "${SECRETS_FILE}" 2>/dev/null)
  ok "Задан в ${SECRETS_FILE} (${SECRETS_PERMS})"
  # Warn if permissions are too open
  PERMS=$(stat -c '%a' "${SECRETS_FILE}" 2>/dev/null)
  if [ "${PERMS}" != "600" ] && [ "${PERMS}" != "400" ]; then
    warn "  ВНИМАНИЕ: права файла ${PERMS} — должны быть 600 (root-only)"
    echo "     → sudo chmod 600 ${SECRETS_FILE}"
  fi
elif [ -n "$ENV_PASS" ]; then
  warn "Задан в /etc/environment (небезопасно — этот файл доступен всем пользователям)"
  echo "     → Переместите в ${SECRETS_FILE} (root:root 0600):"
  echo "     → sudo mkdir -p /etc/aperod"
  echo "     → sudo bash -c 'grep APEROD_BACKUP_PASSWORD /etc/environment >> ${SECRETS_FILE}'"
  echo "     → sudo chmod 600 ${SECRETS_FILE} && sudo chown root:root ${SECRETS_FILE}"
  echo "     → sudo sed -i '/APEROD_BACKUP_PASSWORD/d' /etc/environment"
elif [ -n "$SYSTEMD_PASS" ]; then
  warn "Задан в systemd unit (небезопасно — виден через systemctl show)"
else
  err "НЕ задан нигде"
  echo "     → sudo bash blockchain/deploy/setup-backup.sh"
  echo "       (скрипт генерирует пароль и сохраняет в ${SECRETS_FILE} автоматически)"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "10. Cron задание бэкапа"

CRON_FILE="/etc/cron.d/aperod-backup"
if [ -f "$CRON_FILE" ]; then
  ok "Cron задание найдено: ${CRON_FILE}"
  cat "$CRON_FILE" | sed 's/^/     /'
  echo "  Последний запуск (из лога):"
  tail -5 /var/log/aperod_backup.log 2>/dev/null | sed 's/^/     /' \
    || warn "Лог /var/log/aperod_backup.log не найден (бэкап ещё не запускался?)"
else
  err "Cron задание НЕ настроено: ${CRON_FILE}"
  echo "     → sudo bash -c 'echo \"0 */12 * * * root APEROD_BACKUP_PASSWORD=ВАШ_ПАРОЛЬ DATA_DIR=/opt/aperod/data /usr/local/bin/aperod_backup.sh >> /var/log/aperod_backup.log 2>&1\" > /etc/cron.d/aperod-backup'"
  echo "     ИЛИ добавьте переменные в /etc/environment и пропишите без них:"
  echo "     → sudo bash -c 'echo \"0 */12 * * * root /usr/local/bin/aperod_backup.sh >> /var/log/aperod_backup.log 2>&1\" > /etc/cron.d/aperod-backup'"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "11. Textfile collector (Prometheus метрики бэкапа)"

TEXTFILE_DIR="/var/lib/node_exporter/textfile_collector"
PROM_FILE="${TEXTFILE_DIR}/aperod_backup.prom"
if [ -d "$TEXTFILE_DIR" ]; then
  ok "Директория textfile_collector существует"
  if [ -f "$PROM_FILE" ]; then
    ok "Файл метрик найден:"
    cat "$PROM_FILE" | grep -v '^#' | sed 's/^/     /'
  else
    warn "Файл метрик ещё не создан — бэкап ещё не запускался"
    echo "     Запустите вручную: sudo /usr/local/bin/aperod_backup.sh"
  fi
else
  err "Директория отсутствует: ${TEXTFILE_DIR}"
  echo "     → sudo mkdir -p ${TEXTFILE_DIR}"
  echo "     → sudo chown node_exporter:node_exporter ${TEXTFILE_DIR} 2>/dev/null || true"
  echo "     → sudo chmod 755 ${TEXTFILE_DIR}"
fi

# ────────────────────────────────────────────────────────────────────────────
hdr "12. Сводка и следующие шаги"

echo ""
echo "  Команды для быстрого теста бэкапа (после настройки APEROD_BACKUP_PASSWORD):"
echo ""
echo "  # Запустить бэкап вручную:"
echo "  sudo bash /usr/local/bin/aperod_backup.sh"
echo ""
echo "  # Посмотреть что лежит в Backblaze bucket:"
echo "  sudo RCLONE_CONFIG_S3BACKUP_TYPE=s3 \\"
echo "    RCLONE_CONFIG_S3BACKUP_PROVIDER=Other \\"
echo "    RCLONE_CONFIG_S3BACKUP_ENDPOINT=\$(python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['s3backup']['endpoint'])\") \\"
echo "    RCLONE_CONFIG_S3BACKUP_ACCESS_KEY_ID=\$(python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['s3backup']['accessKeyId'])\") \\"
echo "    RCLONE_CONFIG_S3BACKUP_SECRET_ACCESS_KEY=\$(python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['s3backup']['secretAccessKey'])\") \\"
echo "    RCLONE_CONFIG_S3BACKUP_REGION=\$(python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['s3backup']['region'])\") \\"
echo "    rclone ls s3backup:\$(python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['s3backup']['bucket'])\")"
echo ""
echo "  # Если курсы не доходят (Bybit/MEXC заблокированы) — тест напрямую:"
echo "  curl -s 'https://api.bybit.com/v5/market/tickers?category=spot&symbol=TONUSDT' | python3 -m json.tool"
echo "  curl -s 'https://api.mexc.com/api/v3/ticker/price?symbol=TONUSDT'"
echo ""
echo "  # Тест TronGrid с ключом из settings:"
TRON_KEY_CMD="python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['trongrid']['apiKey'])\""
echo "  TG_KEY=\$(${TRON_KEY_CMD})"
echo "  curl -H \"TRON-PRO-API-KEY: \$TG_KEY\" https://api.trongrid.io/v1/accounts/TN3W4H6rK2ce4vX9YnFQHwKENnHjoxb3m9"
echo ""
echo "  # Тест NowNodes с ключом из settings:"
NN_KEY_CMD="python3 -c \"import json; d=json.load(open('${SETTINGS}')); print(d['nownodes']['apiKey'])\""
echo "  NN_KEY=\$(${NN_KEY_CMD})"
echo "  curl -H \"api-key: \$NN_KEY\" https://btcbook.nownodes.io/api/v2/address/1A1zP1eP5QGefi2DMPTfTL5SLmv7Divfna"
echo ""
echo "══════════════════════════════════════════════════════════════"
