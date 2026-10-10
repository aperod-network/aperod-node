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
| 7 | Прописывает `/ip4/<PRIMARY_IP>/tcp/30303` в `p2p.bootnodes` в `/etc/aperod/node.yaml` |
| 8 | Применяет drop-in конфиги systemd (TimeoutStopSec, GOMEMLIMIT) |
| 9 | Запускает `aperod-node` и ждёт готовности API |

---

## Рекомендуемый порядок установки

Если IP основного (primary) узла известен заранее, передайте его флагом `--primary-ip`
прямо при установке — это сразу пропишет bootnode и избавит от шага join:

```bash
# Установка + автоматическое добавление bootnode
sudo bash /opt/aperod/deploy/install-node.sh --primary-ip 89.169.53.128
```

Если IP стал известен позже или нужна полная синхронизация chain.db — запустите
`aperod-join.sh` отдельно (он заменит данные и пропишет bootnode сам):

```bash
# Сначала установка без флага (bootnode не прописан — нода не стартует в сеть)
sudo bash /opt/aperod/deploy/install-node.sh

# Затем, когда IP станет известен — подключение к сети:
sudo bash /opt/aperod/deploy/aperod-join.sh 89.169.53.128:8545
```

> ⚠️ **Не запускайте ноду до выполнения одного из двух шагов выше.**
> Нода без bootnode может сформировать блок с несовместимым genesis-хэшем,
> после чего ре-join потребует полного удаления данных.

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
  --p2p-port <port>  P2P-порт основного узла (по умолчанию: 30303)
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

### Шаг 5: Прописать bootnode в node.yaml

```bash
# Через node-config.sh (рекомендуется)
sudo bash /opt/aperod/blockchain/deploy/node-config.sh \
  add-bootnode /ip4/<PRIMARY_IP>/tcp/30303

# Или вручную — добавить в /etc/aperod/node.yaml:
# p2p:
#   bootnodes:
#     - /ip4/<PRIMARY_IP>/tcp/30303
```

> ⚠️ Без bootnode оба узла ждут **входящего** подключения и никогда не устанавливают соединение — `peer_count` остаётся 0 бесконечно.
> Подробнее: [раздел «Bootnode — почему он обязателен»](#bootnode--почему-он-обязателен).

### Шаг 6: Настроить права и запустить

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

## Bootnode — почему он обязателен

После копирования цепи у нового узла в `node.yaml` нет записей в `p2p.bootnodes`.
Без хотя бы одного bootnode оба узла (основной и новый) ждут **входящего** подключения
и никогда не устанавливают соединение — `peer_count` остаётся 0 бесконечно.

**Оба скрипта** автоматически прописывают основной узел как bootnode:

- `aperod-join.sh` — шаг 7/8, на новом сервере (использует IP из первого аргумента)
- `join-network.sh` — шаг 5/7, по SSH с основного сервера

Результирующий `node.yaml`:

```yaml
p2p:
  bootnodes:
    - /ip4/<PRIMARY_IP>/tcp/30303
```

Оба формата адреса — `host:port` и `/ip4/…/tcp/…` — принимаются `resolveBootnode()`
в `p2p/dns.go`.

**Если нестандартный P2P-порт** (не 30303), передайте его явно при вызове `aperod-join.sh`:

```bash
sudo bash aperod-join.sh 89.169.53.128:8545 --p2p-port 30304
```

**Для `join-network.sh`** (rsync-путь): если PRIMARY_IP определяется неверно (например,
возвращается внутренний 10.x вместо внешнего адреса), переопределите его явно:

```bash
PRIMARY_IP=89.169.53.128 sudo bash join-network.sh <TARGET_IP>
```

---

## Частые ошибки

| Ошибка | Причина | Решение |
|--------|---------|---------|
| `403 Forbidden` при скачивании | API-ключ не совпадает | Добавьте `--api-key` или проверьте `node.yaml` |
| `connection refused` | Основной узел недоступен | Откройте порт 8545 в firewall основного узла |
| `permission denied` при старте | Файлы принадлежат root | `chown -R aperod:aperod /var/lib/aperod/` |
| `block at height N missing` | Неполная загрузка chain.db | Запустите скрипт заново (он очищает старые данные) |
| `peer_count: 0` навсегда | Нет bootnode **или** скопированный `p2p_identity.key` | Проверьте `p2p.bootnodes` в `/etc/aperod/node.yaml`; `rm /var/lib/aperod/p2p_identity.key`, restart |
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

### ⚠ Кратковременный простой (~60 с)

`join-network.sh` **останавливает `aperod-node` на основном узле** перед rsync и
перезапускает его сразу после завершения.

**Почему это необходимо:** LevelDB небезопасно копировать в работающем состоянии.
Во время rsync движок непрерывно пишет WAL-записи и компактирует `.ldb`-файлы.
Скопированная директория оказывается внутренне несогласованной: при старте LevelDB
откатывается на меньшую высоту, чем источник, и блок на этой высоте имеет другой
хэш. P2P-протокол не может автоматически устранить такое расхождение — нода
застревает в цикле «подключиться → отвергнуть → отключиться».

Если остановить основную ноду не удаётся (нет SSH-доступа, ошибка systemctl),
скрипт **прерывается** вместо того, чтобы выполнять rsync поверх живой базы.

**Типичное время простоя:** остановка (`TimeoutStopSec=300`, фактически ~5–15 с) +
rsync (~30–60 с) + запуск (~5 с) = **≈60–90 с**.

---

*Последнее обновление: Август 2026 · [aperod-network](https://github.com/aperod-network/aperod-node)*
