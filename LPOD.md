# LPoD: function, status, and exact license scope

## Concise public summary

**EN.** LPoD is a protocol subsystem developed and owned by the web3 Aperod
APRO team. It records opt-in Guardian positions backed by eligible on-chain
outputs, links accrual to validator state, commits deterministic accounting and
payout state, prevents source reuse, and refunds Guardian principal exactly
once after a signed withdrawal is canonically confirmed. The immediate
Guardian-principal refund does not remove or shorten the validator's separate
stake lock.

LPoD is disabled by default. The repository contains implementation and
coordination machinery, but no approved production activation package and no
claim that a 1B APRO LPoD allocation is live. Production activation requires
the protocol's own attestations, review, finality conditions, coordinated
upgrade, and deployment authorization.

**RU.** LPoD — подсистема протокола, разработанная и принадлежащая web3 команде
Aperod APRO. Она учитывает добровольные позиции Guardian, обеспеченные
подходящими ончейн-выходами, связывает начисления с состоянием валидатора,
фиксирует детерминированный учёт и выплаты, предотвращает повторное
использование источника и однократно возвращает основную сумму Guardian после
канонического подтверждения подписанного вывода. Немедленный возврат основной
суммы Guardian не отменяет и не сокращает отдельную блокировку стейка
валидатора.

LPoD по умолчанию отключён. Репозиторий содержит реализацию и механизмы
координации, но не содержит утверждённого пакета production-активации и не
заявляет, что распределение 1B APRO для LPoD запущено. Для production-активации
нужны предусмотренные протоколом аттестации, проверка, условия финальности,
скоординированное обновление и разрешение на развёртывание.

## Software approval is not protocol activation

Prior explicit written approval under [LICENSE-LPOD](LICENSE-LPOD) concerns
developer activities such as execution, development, deployment, forking, or
reuse of Covered Code. It is separate from an end user's supported transaction
on an authorized deployment and separate from any chain-level activation.
Requests must use the verified official route
[@sup_apro_bot](https://t.me/sup_apro_bot); a request or automated response is
not approval.

## Exact Covered Code inventory

Covered Code is limited to the original copyrightable source-code expression
of the Aperod APRO team in the following public-repository paths, each marked
with `SPDX-License-Identifier: LicenseRef-Aperod-LPoD`:

- `api/lpod_pool.go`
- `api/lpod_pool_test.go`
- `api/lpod_positions.go`
- `api/lpod_positions_test.go`
- `cmd/node/lpod.go`
- `cmd/wallet-wasm/lpod.go`
- `cmd/wallet-wasm/lpod_js_wasm.go`
- `cmd/wallet-wasm/lpod_test.go`
- `consensus/lpod.go`
- `consensus/lpod_finality_test.go`
- `consensus/lpod_positions_test.go`
- `consensus/lpod_test.go`
- `core/lpod.go`
- `core/lpod_position.go`
- `lpod/accounting.go`
- `lpod/accounting_test.go`
- `lpod/arrears_test.go`
- `store/lpod.go`
- `store/lpod_indices.go`
- `store/lpod_migration.go`
- `store/lpod_migration_test.go`
- `store/lpod_positions.go`
- `store/lpod_positions_test.go`
- `store/lpod_test.go`

The inventory does **not** cover shared files merely modified to call LPoD,
documentation, unrelated Aperod code, dependencies, generated code, or
third-party material. If any inventoried file contains such material,
[LICENSE-LPOD](LICENSE-LPOD) applies only to the original portions owned by the
applicable copyright holder.

## License boundary

The Aperod APRO team has not issued permission to execute, develop, deploy,
fork, distribute, or reuse Covered Code. Prior explicit written approval is
required for every such use. All rights not expressly granted for inspection
are reserved.

The root Apache License 2.0 applies separately to non-Covered Code identified
under it. Dependencies and third-party material remain subject to their own
terms. `LICENSE-LPOD` claims no ownership of abstract ideas, methods, protocol
concepts, or independently developed code; it governs only the designated
original copyrightable expression. See [NOTICE](NOTICE).