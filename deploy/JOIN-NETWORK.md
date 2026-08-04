# Подключение нового узла к сети Aperod

> **Этот документ** — пошаговое руководство для операторов, которые хотят присоединить новый сервер к работающей сети Aperod.

---

## Быстрый старт (один скрипт)

Запустите **на основном узле** (disturbing-blush / 89.169.53.128):

```bash
sudo bash /opt/aperod/blockchain/deploy/join-network.sh <IP_НОВОГО_СЕРВЕРА>
```

Скрипт сделает всё автоматически. Если хотите понять что происходит — читайте ниже.

---

## Типы узлов

| Тип | `validator_key` | `non_validator` | Что делает |
|-----|----------------|-----------------|------------|
| **Полный валидатор** | ✅ установлен | `false` (по умолчанию) | Производит блоки, голосует, получает награды |
| **Синхронизирующий узел** | не важно | `true` | Синхронизирует цепь, ретранслирует блоки, не производит |
| **RPC/API нода** | не важно | `true` | То же + API для внешних запросов |

---

## Предварительные требования

На новом сервере:
- Ubuntu 22.04 / 24.04 или Debian 12
- aperod-node уже установлен (`install-validator.sh` был запущен)
- Порт **30303/tcp** открыт в firewall
- SSH-доступ с основного узла без пароля (или с паролем — скрипт запросит)

---

## Почему нельзя просто запустить ноду?

У Aperod есть особенность: **genesis-блок включает публичный ключ первого валидатора**. Если два сервера используют разные ключи, их genesis-хэши будут различаться, и они никогда не синхронизируются — даже если данные идентичны.

**Решение:** Новый узел получает копию данных (`chain.db`) с существующего узла через rsync. Это гарантирует идентичный genesis-блок.

---

## Пошаговый процесс вручную

Если по каким-то причинам скрипт не подходит:

### Шаг 1: Остановить ноду на новом сервере

```bash
# На НОВОМ сервере
systemctl disable --now aperod-node
```

> ⚠️ Используйте `disable --now`, а не просто `stop` — systemd автоматически перезапускает ноду после падения. `disable` отключает автозапуск до явного `enable`.

### Шаг 2: Rsync данных с основного узла

```bash
# На ОСНОВНОМ УЗЛЕ (89.169.53.128)
rsync -az --delete --progress --ignore-errors \
  /opt/aperod/data/testnet/ \
  root@<IP_НОВОГО>:/var/lib/aperod/
```

> ⚠️ Флаг `--delete` обязателен. Без него старые SST-файлы LevelDB остаются на диске и вызывают ошибку `"block at height N missing from store"` при старте.

### Шаг 3: Удалить скопированный p2p identity

```bash
# На НОВОМ сервере
rm -f /var/lib/aperod/p2p_identity.key
```

> ⚠️ rsync копирует `p2p_identity.key` с основного узла. Если оба сервера используют одинаковый TLS-ключ, P2P-соединение отклоняется как self-connection и peer_count остаётся 0 навсегда. Удаление файла заставляет ноду сгенерировать новый уникальный ключ при старте.

### Шаг 4: Настроить права и запустить

```bash
# На НОВОМ сервере
chown -R aperod:aperod /var/lib/aperod/
systemctl enable --now aperod-node
```

### Шаг 5: Дождаться готовности (~5 минут)

Нода перестраивает key-image индекс по всей цепи (~958K+ блоков). Это занимает ~5 минут.

```bash
# Проверить статус
journalctl -u aperod-node -f --no-pager
# Искать строку: "spent key-image set rebuilt"
# После неё появится: "p2p started" и "peer connected"
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
| `permission denied` при старте | Файлы принадлежат root после rsync | `chown -R aperod:aperod /var/lib/aperod/` |
| `block at height N missing from store` | rsync без `--delete` оставил старые файлы | Очистить: `rm -rf /var/lib/aperod/*`, rsync заново с `--delete` |
| `peer_count: 0` навсегда | Скопированный `p2p_identity.key` (TLS-дубликат) | `rm /var/lib/aperod/p2p_identity.key`, restart |
| Нода производит блоки, цепи расходятся | Нет `non_validator: true`, ключ не в validator set | Добавить `non_validator: true` в node.yaml |
| `key-image rebuild failed` | Смешанный LevelDB (старые + новые блоки) | Полная очистка + rsync с `--delete` |

---

## Регистрация как валидатор

Чтобы ваш узел производил блоки и получал награды:

1. Получите APRO на reward_address (минимум **100 000 APRO**)
2. Отправьте **StakeTx** через кошелёк (Telegram Wallet → Staking)
3. Дождитесь следующего epoch (~100 блоков ≈ 5 минут)
4. Уберите `non_validator: true` из node.yaml (или убедитесь что он отсутствует)
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
curl -s http://127.0.0.1:8545/api/v1/network/validators 2>/dev/null || \
  echo "Validator API not available in this version"
```

---

*Последнее обновление: Август 2026 · [aperod-network](https://github.com/aperod-network/aperod-node)*
