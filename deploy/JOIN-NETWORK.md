# Mainnet node connection / Подключение узла к mainnet

## Status

This legacy join procedure is retired. It stopped the source node, replaced
destination chain data, and used a data path whose name does not prove network
identity. Do not run old `aperod-join.sh` or `join-network.sh` examples to add a
mainnet validator.

The verified, self-service mainnet bootstrap release is not yet available.
Follow [MAINNET.md](MAINNET.md) for the current release gates and
[VALIDATORS.md](VALIDATORS.md) for the separately verified participation rules.
Do not move chain data, start signing, or fund a new validator until the
canonical mainnet identity and a matching trusted checkpoint are published and
successfully rehearsed.

## Peer admission

The joining node must pin the relay's SHA-256 TLS SPKI fingerprint. The
relay must also admit the joining node's distinct TLS fingerprint; an IP
allowlist entry does not replace that check. Exchange only public
fingerprints over an authenticated operator channel. Never send a peer
private key, validator signing key or wallet mnemonic. Before staking,
confirm the authenticated connection and compare a post-checkpoint block
hash with the canonical reference. Public self-service relay admission is
not yet verified.

## Статус

Старая процедура подключения выведена из эксплуатации. Она останавливала
исходный узел, заменяла данные целевого узла и использовала каталог, название
которого не доказывает идентичность сети. Не запускайте старые примеры
`aperod-join.sh` или `join-network.sh` для подключения валидатора mainnet.

Проверенный релиз для самостоятельной установки mainnet ещё не опубликован.
Текущие условия готовности указаны в [MAINNET.md](MAINNET.md), а подтверждённые
правила участия — отдельно в [VALIDATORS.md](VALIDATORS.md). Не переносите
данные цепи, не включайте подпись и не переводите залог, пока каноническая
идентичность mainnet и соответствующая доверенная контрольная точка не будут
опубликованы и успешно проверены на отдельной репетиции.

## Допуск узла к relay

Новый узел закрепляет SHA-256-отпечаток открытого TLS-ключа relay (SPKI).
Relay также должен разрешить отдельный TLS-отпечаток нового узла: добавление
IP-адреса не заменяет эту проверку. Передавайте только публичные отпечатки
по подтверждённому каналу связи с оператором. Не передавайте закрытый
TLS-ключ, ключ валидатора или мнемонику кошелька. До внесения залога
подтвердите TLS-подключение и совпадение хеша блока после контрольной точки
с канонической цепью. Самостоятельный публичный допуск к relay ещё не проверен.
