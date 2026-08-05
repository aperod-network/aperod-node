# Подключение нового узла к сети Aperod

> **Этот документ** — пошаговое руководство для операторов, которые хотят присоединить новый сервер к работающей сети Aperod.

---

## Быстрый старт — одна команда (рекомендуется)

Запустите **на НОВОМ сервере** (не на основном):

```bash
sudo bash /opt/aperod/deploy/aperod-join.sh <PRIMARY_IP>:<API_PORT>
```

**Пример для тестнета:**

```bash
sudo bash /opt/aperod/deploy/aperod-join.sh 89.169.53.128:8545
```

Если основной узел настроен с API-ключом (`api.key` в `node.yaml`):

```bash
sudo bash /opt/aperod/deploy/aperod-join.sh 89.169.53.128:8545 --api-key <ваш_api_key>
```

Скрипт сделает всё автоматически за 5–10 минут. Если хотите понять процесс — читайте ниже.

---

## Что делает aperod-join.sh

| Шаг | Действие |
|-----|----------|
| 1 | Проверяет доступность основного узла по HTTP |
| 2 | Останавливает `aperod-node` на **новом** сервере |
| 3 | Очищает старые данные в `data_dir` |
| 4 | Скачивает `chain.db` через `GET /api/v1/chaindb/export` (~1–2 ГБ) |
| 5 | Скачивает UTXO-snapshot через `GET /api/v1/snapshot/export` |
| 6 | Удаляет `p2p_identity.key` (нода генерирует новый при старте) |
| 7 | Применяет drop-in конфиги systemd (TimeoutStopSec, GOMEMLIMIT) |
| 8 | Запускает `aperod-node` и ждёт готовности API |

---

## Требования

**На новом сервере:**
- Ubuntu 22.04 / 24.04 или Debian 12
- `aperod-node` уже установлен (`install-node.sh` запущен ранее)
- Порт **30303/tcp** открыт в firewall
- HTTP-доступ к API основного узла (порт 8545 по умолчанию)

**На основном узле:**
- Запущен `aperod-node` версии с поддержкой экспорта
- Порт API (8545) доступен с нового сервера (или туннель)
- API-ключ совпадает с `--api-key` в команде join (если настроен)

---

## Опции скрипта

```
aperod-join.sh <PRIMARY_IP>:<PORT> [OPTIONS]

Опции:
  --api-key  <key>   X-API-Key для аутентификации на основном узле
  --data-dir <path>  Директория данных (по умолчанию: /var/lib/aperod)
  --user     <name>  Пользователь-владелец данных (по умолчанию: aperod)
  --skip-start       Не запускать ноду после загрузки (только данные)
  --no-chaindb       Пропустить загрузку chain.db (только snapshot)
```

**Пример с нестандартными путями:**
```bash
sudo bash aperod-join.sh 89.169.53.128:8545 \
  --api-key secret123 \
  --data-dir /opt/aperod/data/testnet \
  --user aperod
```

---

## HTTP-эндпоинты экспорта (на основном узле)

Скрипт использует два новых эндпоинта API основного узла:

### `GET /api/v1/snapshot/export`

Отдаёт последний UTXO-snapshot (файл `snapshot-v2-<height>.json.gz`).  
Snapshot ускоряет запуск новой ноды — без него пересборка key-image индекса займёт 5–10 минут.

Заголовки ответа:
- `X-Snapshot-Height` — высота блока, для которого сделан snapshot
- `X-Snapshot-Filename` — имя файла (`snapshot-v2-<height>.json.gz`)

### `GET /api/v1/chaindb/export`

Стримит директорию `chain.db` (LevelDB) как tar.gz-архив.  
Распаковывается в `data_dir` командой `tar -xzf chaindb.tar.gz -C <data_dir>`.

**Безопасность:**
- Оба эндпоинта требуют `X-API-Key`, если в `node.yaml` настроен `api.key`
- В dev-режиме (без ключа) — открыты

---

## Типы узлов

| Тип | `validator_key` | `non_validator` | Что делает |
|-----|----------------|-----------------|------------|
| **Полный валидатор** | ✅ установлен | `false` (по умолчанию) | Производит блоки, голосует, получает награды |
| **Синхронизирующий узел** | не важно | `true` | Синхронизирует цепь, ретранслирует блоки, не производит |
| **RPC/API нода** | не важно | `true` | То же + API для внешних запросов |

---

## Почему нельзя просто запустить ноду?

У Aperod есть особенность: **genesis-блок включает публичный ключ первого валидатора**. Если два сервера используют разные ключи, их genesis-хэши будут различаться, и они никогда не синхронизируются — даже если данные идентичны.

**Решение:** Новый узел получает копию `chain.db` с существующего узла через HTTP. Это гарантирует идентичный genesis-блок.

---

## Пошаговый процесс вручную

Если по каким-то причинам скрипт не подходит:

### Шаг 1: Остановить ноду на новом сервере

```bash
systemctl disable --now aperod-node
```

> ⚠️ Используйте `disable --now`, а не просто `stop` — без этого systemd автоматически перезапустит ноду.

### Шаг 2: Скачать chain.db

```bash
# Без API-ключа
curl -f http://<PRIMARY_IP>:8545/api/v1/chaindb/export \
  -o /tmp/chaindb.tar.gz

# С API-ключом
curl -f -H "X-API-Key: <key>" \
  http://<PRIMARY_IP>:8545/api/v1/chaindb/export \
  -o /tmp/chaindb.tar.gz

# Распаковать
tar -xzf /tmp/chaindb.tar.gz -C /var/lib/aperod/
rm /tmp/chaindb.tar.gz
```

### Шаг 3: Скачать snapshot

```bash
# Определяем имя файла из заголовков
SNAP_FILE=$(curl -sI -H "X-API-Key: <key>" \
  http://<PRIMARY_IP>:8545/api/v1/snapshot/export \
  | grep -i X-Snapshot-Filename | tr -d '\r' | awk '{print $2}')

# Скачиваем
curl -f -H "X-API-Key: <key>" \
  http://<PRIMARY_IP>:8545/api/v1/snapshot/export \
  -o "/var/lib/aperod/${SNAP_FILE}"
```

### Шаг 4: Удалить скопированный p2p identity

```bash
rm -f /var/lib/aperod/p2p_identity.key
```

> ⚠️ Без этого оба сервера используют одинаковый TLS-ключ и видят друг друга как self-connection. `peer_count` останется 0 навсегда.

### Шаг 5: Настроить права и запустить

```bash
chown -R aperod:aperod /var/lib/aperod/
systemctl enable --now aperod-node
```

### Шаг 6: Дождаться готовности (~5 минут)

```bash
# Следить за логами
journalctl -u aperod-node -f --no-pager
# Искать: "API server ready" → "p2p started" → "peer connected"

# Проверить статус
curl -s http://127.0.0.1:8545/api/v1/network/stats | python3 -m json.tool
```

---

## Конфигурация node.yaml

### Валидатор с собственным ключом (полный участник консенсуса)

```yaml
consensus:
  validator_key: /etc/aperod/validator.key   # ваш собственный Ed25519 ключ
  reward_address: aproec<ваш_адрес>
  block_time: 3s
```

> Чтобы производить блоки, ваш ключ должен быть в **validator set** (зарегистрирован через StakeTx).

### Синхронизирующий узел без производства блоков

```yaml
consensus:
  non_validator: true    # отключает производство блоков
  # validator_key не нужен
  reward_address: aproec<ваш_адрес>
```

> Используйте `non_validator: true` для RPC-нод, explorer-нод и резервных серверов.

---

## Частые ошибки

| Ошибка | Причина | Решение |
|--------|---------|---------|
| `403 Forbidden` при скачивании | API-ключ не совпадает | Добавьте `--api-key` или проверьте `node.yaml` |
| `connection refused` | Основной узел недоступен | Откройте порт 8545 в firewall основного узла |
| `permission denied` при старте | Файлы принадлежат root | `chown -R aperod:aperod /var/lib/aperod/` |
| `block at height N missing` | Неполная загрузка chain.db | Запустите скрипт заново (он очищает старые данные) |
| `peer_count: 0` навсегда | Скопированный `p2p_identity.key` | `rm /var/lib/aperod/p2p_identity.key`, restart |
| Нода расходится с сетью | Нет `non_validator: true`, ключ не в validator set | Добавить `non_validator: true` в node.yaml |

---

## Регистрация как валидатор

Чтобы ваш узел производил блоки и получал награды:

1. Получите APRO на `reward_address` (минимум **100 000 APRO**)
2. Отправьте **StakeTx** через кошелёк (Telegram Wallet → Staking)
3. Дождитесь следующего epoch (~100 блоков ≈ 5 минут)
4. Уберите `non_validator: true` из `node.yaml` (или убедитесь что он отсутствует)
5. Перезапустите ноду: `systemctl restart aperod-node`

После включения в активный validator set нода начнёт получать задания на производство блоков.

---

## Проверка подключения

```bash
# Статус текущего узла
curl -s http://127.0.0.1:8545/api/v1/network/stats | python3 -m json.tool

# Ожидаемые значения:
# "peer_count": 1,      ← подключён к сети
# "height": 958XXX,     ← синхронизирован

# Статус валидаторов
curl -s http://127.0.0.1:8545/api/v1/validators
```

---

## Старый способ (rsync с основного узла)

Если `aperod-join.sh` недоступен или нужен rsync:

```bash
# На ОСНОВНОМ УЗЛЕ (89.169.53.128)
sudo bash /opt/aperod/deploy/join-network.sh <IP_НОВОГО_СЕРВЕРА>
```

Этот скрипт требует SSH-доступ с основного узла на новый. Используйте `aperod-join.sh` (HTTP) как предпочтительный метод.

---

*Последнее обновление: Август 2026 · [aperod-network](https://github.com/aperod-network/aperod-node)*
