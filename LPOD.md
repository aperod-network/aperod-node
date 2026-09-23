# LPoD: a guide to positions, rewards, withdrawals, and activation

LPoD is an Aperod protocol subsystem developed and owned by the web3
**Aperod APRO team**. It lets a Guardian reserve eligible on-chain APRO
with a selected validator vault, accrue validator-linked rewards, and
request the return of some or all of that principal.
This guide uses the protocol name without inventing an acronym expansion.

**Production status: disabled by default; not funded or activated by this
repository.** The code, a wallet control, or an API capability flag is not
evidence that the 1B APRO reserve exists on the production chain.
An approved reconciliation, validator attestations, coordinated upgrade,
canonical activation block, and appropriate deployment authorization are required.

**Guardian principal is liquid reserved principal, not validator bonded stake.**
A valid full or partial Guardian withdrawal creates its refund in the same
canonical block that includes the request. There is no Guardian unbonding period.
The validator's own stake retains its separate validator-protocol lock.

This document describes implemented rules and current integration boundaries.
It is not a promise of yield, production readiness, or an instruction to activate
an unapproved fork. Software permissions are described at the end.

## 1. The problem LPoD addresses

A block's scheduled proposer receives actual protocol income, but Guardians
attached to other active validators also have time-based accrual.
Those two streams need not match in any individual block.
LPoD records them separately, routes available income through a deterministic
settlement, and uses a dedicated APR reserve to cover eligible shortfalls.
Unfunded amounts remain explicit liabilities instead of being silently minted.

Every position must start with a real, eligible UTXO and authenticated ownership.
No web session, database balance, administrator label, or claimed deposit
amount creates principal. Checkpoints bind the resulting accounting and
the outputs that actually pay recipients.

### Glossary

| Term | Meaning here |
| --- | --- |
| Validator | A validator registered under the existing chain rules, with its own validator stake and status. |
| Leader / proposer | The validator proposing the particular block; its configured beneficiary receives that block's leader share. |
| Guardian | The owner of wallet-backed principal reserved in an LPoD position. |
| Angels payment | The accounting/code name for funded Guardian rewards, not another type of principal. |
| Vault | The accounting group identified by a validator public key, combining its own stake and active Guardian principal for tier selection. |
| Position | One authenticated source-output deposit, identified independently of a web account. |
| Principal | Deposited APRO that belongs to the Guardian, distinct from earned interest. |
| APR reserve | The separately conserved 1B APRO allocation used for reward shortfalls. |
| Arrears | Earned rewards recorded as due but not yet funded and paid. |
| UTXO | An on-chain transaction output that can be spent with the required ownership proof. |
| Key image | The cryptographic spent-source identifier used to prevent reuse. |
| Canonical | Selected as part of the node's current chain. |
| Finalized | Supported by the required consensus evidence for the exact block hash and height. |

## 2. Three balances that must not be confused

1. **Spendable wallet outputs:** ordinary eligible outputs the owner can spend.
2. **Reserved Guardian principal:** consumed source outputs represented by
   canonical positions. They are no longer ordinary spendable UTXOs while reserved.
3. **The APR reserve:** protocol funds available for Guardian reward shortfalls.
   This is not the sum of Guardian deposits.

A deposit reduces ordinary spendable sources and increases reserved principal.
A withdrawal reduces reserved principal and creates a new spendable refund.
Earned rewards create separate value transfers from authorized reward income
and, if necessary, the APR reserve.

**Principal refunds do not debit the APR reserve.** Their backing is the
Guardian's previously consumed deposit. Debiting the reserve as well would
charge the same principal twice.
Consequently, `principal_locked_napro` can fall while `balance_napro`
increases because that block also has reward surplus.

Validator self-stake is a fourth, separate category governed by
[validator staking](core/staking.go).
Full validator withdrawal still uses `UnbondingBlocks = 144000`;
the separate partial-validator withdrawal rule uses `43200` blocks.
Neither waiting period applies to a Guardian position.

## 3. Choosing a validator and entering a vault

Use current canonical validator and position data, not a remembered UI total.
New deposits require an authenticated active validator entry; seeded entries
are not eligible under the native LPoD checks.

The vault total used for eligibility is:

```text
validator own stake + all Guardians' remaining active principal in that vault
```

The owner chooses the validator public key and signs that choice.
It is not a transfer to the validator's personal wallet.
The beneficiary must belong to the signing Guardian's wallet spend key.

A deposit consumes exactly its declared source amount.
The resulting vault must stay within the 100M APRO cap, and the deposit must
meet the minimum for the tier selected by that resulting total.
An increase is another authenticated deposit and another source-derived
position, not an unsigned edit of a balance.
Existing minimums and capacity limits still apply to additional deposits.

New deposits do not receive retroactive interest for the interval before
their inclusion. The prior canonical principal determines that block's
accrual and proposer tier; new principal affects subsequent accrual.

### Exact tier table

Amounts below are **APRO**, not nAPRO.
The selected tier is the highest threshold not exceeding the eligible total.
The final row applies at the 100M cap for new-deposit eligibility.

| Vault threshold | Minimum new deposit | Guardian annual APR | Proposer leader share of actual income |
| ---: | ---: | ---: | ---: |
| 100,000 | 100 | 3% | 8% |
| 500,000 | 1,000 | 5% | 10% |
| 1,000,000 | 10,000 | 6% | 12% |
| 5,000,000 | 20,000 | 8% | 15% |
| 10,000,000 | 50,000 | 9% | 16% |
| 30,000,000 | 80,000 | 9% | 16% |
| 50,000,000 | 100,000 | 9% | 16% |
| 80,000,000 | 150,000 | 9% | 16% |
| 100,000,000 | 200,000 | 10% | 20% |

The APR applies annually to Guardian principal, not to a block reward.
The leader percentage applies to the proposer's actual block income,
not annually to the Guardian deposit.
Changing a vault total within a tier does not change its percentage at every coin.
Crossing a threshold changes the applicable tier.
No automatic post-2088 percentage change is inferred from projections.

Source of truth: [tier definitions and accounting](lpod/accounting.go).

## 4. Accrual and the settlement waterfall

Every relevant vault is considered, including a vault whose validator did
not propose the block. New Guardian APR accrues only while its validator is active.
Previously earned arrears remain due even after validator inactivity or exit.

Only the actual proposer supplies block reward income to this settlement.
It is not valid to assign a full block reward to every vault.
The reward schedule remains 3 APRO while the validator reserve supports it,
the remaining partial draw when smaller than 3 APRO, and then the existing
1 APRO tail. Tail issuance is tracked separately.

For each vault, in deterministic sorted vault-ID order:

1. Calculate the leader share of actual income, retaining its integer remainder.
   A non-proposer has no leader payment from nonexistent block income.
2. Determine newly earned Guardian APR and add previously unpaid arrears.
3. Use income remaining after the leader share for those Guardian entitlements.
   Only an excess over entitlements is credited as reserve surplus.
4. If that residual income is insufficient, draw the shortfall from the
   available APR reserve, without taking it below zero.
5. Record any still-unfunded amount as arrears, not as a paid reward.

Within a vault, funded Guardian payments are allocated proportionally to
position-specific amounts due, with deterministic position-ID ordering and
integer rounding. Beneficiaries are aggregated for actual output construction.

This is a sequential waterfall, not a claim that every vault shares all
future income immediately. At reserve exhaustion, vault order matters:
surplus arriving from a later vault need not pay an earlier vault's arrears
until a subsequent settlement. No universal payout guarantee follows from APR.

### Time, smallest units, and rounding

```text
1 APRO = 100,000,000 nAPRO
year = 365 × 24 × 60 × 60 = 31,536,000 seconds
elapsed_ns = min(block_timestamp − parent_timestamp, 15,000,000,000)
D = 100 × 31,536,000 × 1,000,000,000
N = remaining_principal_napro × APR_percent × elapsed_ns + prior_APR_carry
new_due_napro = floor(N / D)
next_APR_carry = N mod D
```

Canonical timestamps must advance. The 15-second cap means a long outage
does not accrue an unlimited catch-up interval in the next block.
Individual integer carries persist across ordinary settlements and partial exits.
Fractional nAPRO is not an independently spendable output.
APR is simple accrual on remaining principal; paid rewards are not automatically
redeposited or compounded.

## 5. Worked accounting examples

These are arithmetic illustrations with stated assumptions, not production balances.

### A. Small first-tier position, reward surplus

Assume an already-active 100 APRO position in a first-tier vault, no prior
carry or arrears, and a three-second interval in a block proposed by its validator.

```text
Actual income:                    3 APRO
Leader: 3 × 8%                    0.24 APRO
Residual for Guardian settlement: 2.76 APRO
New Guardian due:                 28 nAPRO = 0.00000028 APRO
Reserve surplus:                  2.75999972 APRO
Next individual APR carry:        1,699,200,000,000,000,000
```

The reward is not rounded up to a whole APRO.
The carry preserves the division remainder for future accrual.
The 100 APRO principal remains reserved and is not part of this reward split.

### B. Top-tier position, funded deficit

Assume validator self-stake of 100,000 APRO and Guardian principal of
99,900,000 APRO: total 100M, APR 10%, leader share 20%.
For a 15-second interval with zero initial carry and no arrears:

```text
Actual income:          3 APRO
Leader:                 0.6 APRO
Residual:               2.4 APRO
New Guardian due:       4.75171232 APRO
Reserve draw required:  2.35171232 APRO
```

If the reserve can supply the draw, the Guardian receives 4.75171232 APRO.
If only 1 APRO is available, the Guardian receives 3.4 APRO;
1.35171232 APRO remains arrears. The leader still receives 0.6 APRO.
This does not authorize borrowing the Guardian's principal to pay interest.

### C. A beginner's deposit, decrease, increase, and exit

Alice owns eligible outputs of 500 APRO and 100 APRO.
She chooses an active first-tier validator with sufficient capacity.

1. Alice locally signs a deposit consuming the 500 APRO output.
   After canonical inclusion, reserved principal is 500 APRO.
2. She signs a 200 APRO partial withdrawal with the next position nonce.
   That block returns 200 APRO plus any funded reward included for her.
   Remaining principal is 300 APRO and continues to accrue.
3. She deposits her separate 100 APRO output into the same vault.
   Reserved principal is now 400 APRO across two positions.
   The topup is backed by its own consumed output.
4. She signs full withdrawals for both positions.
   The inclusion block returns the remaining 400 APRO, not the original
   500 APRO again. Earned arrears, if any, remain claimable through settlement.

Cumulative deposited principal is 600 APRO and cumulative returned principal
is 600 APRO after the final exits. Current reserved principal is zero.
Interest and ordinary spending fees are separate from those principal totals.

## 6. Full and partial withdrawal rules

There is no Guardian freeze, including when the associated validator becomes
inactive. A withdrawal does not require that validator's private key or approval.
It does require the Guardian owner's valid signature and canonical position state.

For a native withdrawal:

- Keep the original deposit/source identity, owner, beneficiary, and vault unchanged.
- Use `Nonce = current position nonce + 1`.
- Set `withdraw_amount_napro` to a positive requested partial amount,
  or omit it/use zero to request all remaining principal.
- Do not request more than the remaining principal.
- Sign and submit the actual transaction; a form submission alone changes nothing.

The original `Amount` remains the deposit's source-opening amount.
It is not rewritten after a partial exit.
`Withdrawn` is the position's cumulative returned amount:

```text
remaining principal = Deposit.Amount − Withdrawn
```

The elapsed interval through the withdrawal block is accrued first.
Thereafter only any remaining principal earns new APR.
A full exit sets `Returned = true` and records its inclusion height in
`UnlockHeight`; that legacy field name does not create a waiting period.

The same canonical block creates the refund output.
It can be used in a subsequent ordinary spend after the wallet discovers it
and normal spend validation succeeds. There is no promise of spending an
unconfirmed mempool request or of spending an output before it exists.

Closed records with unpaid arrears remain.
Fully returned records with no arrears can be pruned on a later transition.
The pool API's `position_count` counts positions with remaining principal,
not all retained records.

## 7. Native transactions and ownership

The relevant transaction versions are:

| Version | Role |
| ---: | --- |
| 8 | Checkpoint commitment to the resulting LPoD accounting and position state. |
| 9 | Signed Guardian deposit or withdrawal operation. |
| 10 | Zero-input protocol payout containing the authorized leader, Guardian, and principal-refund outputs. |

A version-10 zero-input payout is not a general-purpose user mint.
Consensus recomputes the complete entitlement plan and requires exactly the
corresponding outputs and checkpoint. Extra protocol payouts are rejected.
Existing validator-stake transactions retain their own classification and validation.

Deposits use the existing MLSAG-v4 direct ownership/opening proof and a linked
source key image. A separate proof authenticates the beneficiary spend key.
The signed action binds genesis, source, position, vault, owner, beneficiary,
amount, withdrawal amount where applicable, action, and nonce.

`LPoDPositionID` derives identity from genesis, source transaction, and output index.
An additional source creates an additional position ID.
Spent key images prevent reusing the original source even after a closed
position record is pruned.
Withdrawals use advancing position nonces and remaining-principal checks.
Neither a replayed full exit nor an overdraw becomes a second refund.

Payout outputs use wallet-scannable keys derived deterministically from
the parent/checkpoint context. Beneficiaries are aggregated and sorted;
the protocol rejects duplicate output keys within the block.
The deterministic ephemeral values are public, not privacy secrets.
Ordinary CLSAG spending of the resulting outputs is exercised in tests.

### Important privacy warning

Native version-9 positions are **public protocol records**.
The source reference, amount opening/blind, beneficiary, vault, and action
are disclosed as part of the deposit protocol.
Do not describe an LPoD deposit as an unlinkable private transfer.
An opening is not a wallet spend key, but it reveals information about
that source and the amount reserved.
Version-10 beneficiaries and amounts are likewise protocol-visible.

## 8. Wallet operation and live totals

The native wallet builder performs signing locally in WASM.
Mnemonic-derived private keys belong in local signing memory, not an HTTP
request to a node or an application server.
Public unsigned position data can come from the API; the signer verifies
the owner, beneficiary, genesis, and source/position relationship.

The deposit builder requires **exactly one owned source output**.
It deposits that output's full amount and does not silently split an arbitrary
requested amount, choose hidden change, or round a balance.
If suitable outputs are unavailable, a separately reviewed ordinary wallet
transaction may be needed first. Automatic arbitrary splitting is not supplied
by this LPoD builder.

Wallet reserved totals must come from canonical positions:

- Per owner: sum remaining principal for that beneficiary.
- Per vault: validator self-stake plus remaining Guardian principal in that vault.
- Network Guardian total: canonical `PrincipalLocked`.
- Spendable funds: eligible discovered outputs, excluding consumed and pending-spent sources.

The wallet-facing integration refreshes canonical data on a roughly ten-second
poll and when focus returns, and refreshes after transaction activity.
That is UI synchronization, not a ten-second protocol settlement schedule.
Missing/failing canonical data must not be displayed as an invented zero balance.
Percentages change at tier thresholds, not after every individual coin change.

The wallet integration indexes authentic version-10 outputs in the same
canonical commit and rollback batches as the protocol state.
Finalized beneficiary-indexed pages are merged into signing material only
when their checkpoints agree. They do not create fabricated SQL wallet records.
Local WASM scanning verifies ownership and derives key images; the node
checks canonical spent status, output references, and pending spends.
Native spendable balances shown on wallet home are locally scanned results,
not authoritative SQL credits. Snapshot misalignment fails closed.

The full-workspace integration test has passed with an initially empty SQL
wallet index, the actual WASM signer/scanner, and a native test chain:
deposit, partial refund, rollback/replay, rewards, full refund, ordinary
version-5 spending, and a version-9 redeposit.
This is integration evidence, not a production activation or multi-node
finality claim.

The current wallet scan has a **4,096-native-output bound**.
Exceeding it returns an explicit error, rather than silently truncating a
balance or claiming unlimited scalability.
This per-wallet scan bound is distinct from the protocol's position capacity.
An old activated store without the wallet-index readiness marker requires
canonical replay from activation; an index-not-ready error is not proof of
an empty wallet. Production has not been activated by this work.
Verify deployment of the matching node, WASM bundle, discovery integration,
and index together before relying on a deployed wallet.

## 9. Pending, included, finalized, and reorganized

A broadcast hash means the request was submitted, not that money moved.
Mempool admission is not canonical inclusion.
State-dependent stale requests can be rejected or evicted without changing principal.

Canonical inclusion applies the operation, records the new checkpoint, and
creates any funded output. The read APIs additionally require finality evidence
for the exact current canonical tip before exposing active monetary projections.
An included refund and an API still reporting `pending` are therefore different
stages, not necessarily contradictory observations.

Checkpoints are keyed by block hash rather than an independently mutable balance.
Funding, positions, principal counters, carries, payouts, relevant indices,
and the canonical block/AVM commit use the atomic persistence path.
Restart loads committed state instead of defaulting to a 1B balance.
It does not invent finality evidence from height alone.

Ancestor rollback restores the selected accounting and native indices;
orphaned refunds must disappear from the spendable set.
Alternate blocks must be accepted through normal validation.
The implemented rollback path requires rewinding to a common ancestor rather
than arbitrarily selecting an unrelated stored tip.
Going earlier than the attested funding parent requires additional historical
budget evidence and otherwise fails closed.

These controls address replay and branch consistency.
They are not a claim that forks, compromised trusted authorities, or operator
misconfiguration are cryptographically impossible.

## 10. Conservation and the 1B allocation

All following identities use exact nAPRO integers, not floating-point UI amounts.
Let `F` be the once-only 1B allocation, `R` accumulated authorized reward income,
`B` reserve balance, `L` paid leader rewards, and `G` paid Guardian rewards:

```text
F + R = B + L + G
B = F + cumulative surplus − cumulative deficit draws
accrued Guardian liability = paid Guardian rewards + unfunded liability
cumulative principal deposited = principal locked + principal returned
```

Principal is not added to the first equation.
Reserving an already-issued coin changes custody/accounting, not issuance.
Arrears are liabilities, not spendable outputs and not an extra supply allocation.

### Funding inside the existing 10B budget

Let `I` be reconciled historical issuance and `V0` the authenticated remaining
validator reward budget at activation. The implementation protects an additional
1B development reservation:

```text
available before LPoD = 10B − I − V0 − 1B development reservation
allocation remaining after funding = available before LPoD − 1B LPoD debit
```

There must be enough proved availability.
The validator budget must equal its existing durable value; activation may not
reset it to 2B or silently reduce it to create room.
Historical validator issuance is already part of `I`; it is not charged again
as though the original whole validator allocation were still unspent.

The general gross allocation identity is:

```text
I + protected development reservation + allocation remaining
  + current validator budget + B + L + G
  = 10B + explicitly recorded tail issuance
```

These are gross issuance/allocation identities, not claims about circulating
free float. Canonical fee burns must be considered separately when interpreting
circulating supply. Locked Guardian principal must not be counted a second time.

The 1B reserve is an allocation debit and corresponding protocol credit,
not permission to mint an extra 1B on top of the supply plan.
It becomes a spendable wallet output only through an authorized funded payout.

## 11. Historical reconciliation and activation

There is no self-declared “set pool to 1B” configuration path.
The witness must cover every historical coinbase output in canonical order
through activation height minus one, with amount openings verified against
the actual commitments.
Pruned or missing required bodies cannot simply be replaced by supply estimates.

Legacy transaction hashes alone are not an unambiguous historical body proof.
The reconciliation additionally commits to a length-framed canonical full-body
root, historical issuance, validator budget, genesis, height, and openings.
The witness requires signatures from **strictly more than two thirds of
distinct trusted genesis validators**.
Trusted authorities are injected from node configuration, not supplied by
the witness's own assertions about who may approve it.
This remains an explicit governance trust boundary requiring independent review.

### Witness representation

The `LPoDMigration` JSON fields are:

```text
version, height, genesis, reconciliation_root, openings, body_root,
historical_issued_napro, validator_remaining_napro, attestations
```

Each opening contains `height`, `tx_index`, `output_index`,
`amount_napro`, and `blind`.
Each attestation contains `validator` and `signature`.
Monetary witness fields are decimal strings.
With the current Go encoding, hashes and blind factors are 32-byte numeric
arrays; validator public keys and signatures are base64.
Do not substitute API-style hex strings into this witness format.
Generate and verify it with the actual Go types and hash helpers.

Useful helpers in [migration verification](store/lpod_migration.go) are
`LPoDBodyRootStep`, `LPoDMigration.Root`, `AttestationMessage`,
`VerifyAuthorization`, and `DB.VerifyLPoDMigration`.
The exact root serialization and domain separation are defined there.
They must not be re-created by guessing JSON field order or hash algorithms.

### Conceptual activation checklist

1. Obtain required written software/deployment permissions separately from
   chain governance approval.
2. Review the implementation, consensus change, threat model, and wallet path.
   Exercise independent multi-node acceptance, restart, rollback, and discovery.
3. Agree on the exact chain/genesis and an activation checkpoint.
   Arrange coordinated checkpoint/signing timing: the witness includes bodies
   through `H−1`, which cannot be truthfully invented for unknown future blocks.
4. Obtain complete authentic canonical history and valid issuance openings.
   Reconcile the existing durable validator budget without changing entitlement.
5. Independently review the full-body commitment and gross allocation equation.
   Confirm the protected development reservation and available 1B debit.
6. Obtain the required distinct trusted-validator attestations using authorized
   key-management procedures. Never place private validator keys in a witness.
7. Distribute and independently verify the identical approved witness and
   compatible software/configuration across participating nodes.
8. Configure `consensus.lpod_migration_file` to the approved local witness.
   Keep the incompatible legacy Guardian allocation disabled.
   Reward authorization must activate no later than this fork; reward
   parameters must match the supported validator-pool and tail schedule.
9. At the agreed height, normal canonical block validation and atomic commit
   perform the one-time debit. Startup or loading a JSON file does not fund it.
10. Verify exact-hash finality, reserve/source conservation, validator budget,
    wallet-index readiness, real output discovery, and an authorized spend.
    Keep alerts and an approved recovery plan in place.

No complete reconciliation-generation/attestation CLI is supplied.
The existing `cmd/mintblind` utility is a limited legacy mint-candidate tool,
not a migration tool; its floating-point argument path is not an appropriate
source of exact funding arithmetic.
This guide intentionally supplies no fake witness, private-key command,
remote-validator access procedure, or bypass of a failed verification.

## 12. Public node API

These are native node routes, not a promise that every web gateway uses
the same URL. See [route registration](api/rest.go).
Monetary quantities are decimal nAPRO strings; do not coerce them into
JavaScript floating-point arithmetic.

### `GET /api/v1/lpod-pool`

The response includes `version`, `protocol_version`, `state`,
`accounting_basis: "canonical_protocol_ledger"`, and
`initial_napro: "100000000000000000"`.
The initial value is a protocol target, not evidence of funding.

| State | Interpretation |
| --- | --- |
| `disabled` | No configured LPoD migration. |
| `pending` | Awaiting funding/checkpoint or exact-tip finality; a concurrent tip change can also defer publication. |
| `unavailable` | Required store or canonical/configuration evidence cannot be established. |
| `active` | Matching canonical checkpoint and exact-tip finality passed the read checks. |

Monetary/state projection fields are explicitly null before active publication:
`balance_napro`, `reward_inflow_napro`, `leader_paid_napro`,
`angel_paid_napro`, `surplus_inflow_napro`, `deficit_outflow_napro`,
`unfunded_liability_napro`, `funding_debit_napro`,
`total_guardian_stake_napro`, `principal_deposited_napro`,
`principal_locked_napro`, `principal_returned_napro`,
`allocation_remaining_napro`, `validator_remaining_napro`, and `tail_issued_napro`.

Metadata includes `last_settled_height`, `finalized_height`, `position_count`,
`funding_height`, `funding_block_hash`, `chain_anchor`, and `reconciliation_root`.
`position_count` counts remaining-principal positions; retained arrears records
can exist when this count and locked principal are both zero.

`guardian_membership_supported`, `immediate_exit_supported`,
`partial_exit_supported`, and `additional_deposits_supported` describe code
capabilities. They do not activate the protocol.

### `GET /api/v1/lpod/positions?address=...`

A valid address is required. The projection includes `state`, `address`,
`reserved_napro`, `positions`, `checkpoint_hash`, `finalized_height`,
and `wallet_mutations_supported`.
Active data additionally exposes `chain_anchor` and eligible `vaults`.

Each position has `id`, `vault`, `principal_napro` (remaining),
`returned_napro` (cumulative), `due_napro`, `nonce`, and
`deposit_action_json` (the already-public original native action).
Rows are sorted by position ID.
Vault rows include `id`, `total_napro`, `minimum_napro`,
`apr_percent`, and `leader_percent`.
Clients must still allow the node to revalidate eligibility at inclusion.

### Payout discovery and spent-source checks

`GET /api/v1/lpod/wallet-outputs?address=...&cursor=...` is a bounded,
address-indexed discovery route, with `outputs`, `next_cursor`, and
an active `checkpoint_hash`. A page examines at most 128 index entries.
Clients must bind pagination to one consistent checkpoint.
Output records include `tx_hash`, `out_idx`, `block_height`,
`one_time_pub`, `tx_pub_key`, `amount_commit`, and `enc_amount`.

The route requires finalized canonical state and an index ready for the actual
funding block. It returns explicit errors when that index requires canonical
replay, and a conflict when the tip changes during a read.
An empty page alone does not establish an empty wallet.

`POST /api/v1/wallet/key-images` accepts bounded public `key_images`
and matching `refs` containing `tx_hash` and `out_idx`.
It returns spent-status information and a checkpoint hash.
Local scanning and ownership verification are still required; neither endpoint
needs a mnemonic, wallet private key, or session-created monetary balance.

## 13. Limits, failure cases, and frequently asked questions

**What happens when the APR reserve reaches zero?**
Available residual income can still fund rewards.
The unpaid remainder stays as arrears; displayed APR is not guaranteed cash.
Principal refunds remain separately backed and are not blocked by reserve depletion.

**Can an inactive validator trap my Guardian deposit?**
The Guardian exit rule has no validator-active requirement.
New accrual stops while the validator is inactive; earned arrears remain due.
New deposits require an eligible active validator.

**Does every retained position count toward capacity?**
There is a 4,096-position bound.
Closed records with unpaid liabilities remain necessary; fully returned,
fully paid records are released by subsequent processing.
Capacity and open-position count are therefore different concepts.

**Can a validator self-stake change raise a vault above 100M?**
New Guardian deposits may not push it above the cap.
If subsequent validator self-stake growth raises an existing vault above it,
accrual uses the top tier rather than halting the ledger.
Clients must not infer new-deposit eligibility from that accrual treatment.

**Does a topup require the validator's key?**
No. It requires an eligible selected vault and the Guardian's authenticated
owned output. It is a new source-backed position.

**Can I withdraw rewards that are still arrears?**
Arrears cannot be spent before an actual funded output exists.
An exit preserves them; later settlements retry funding them.

**Why can an active API temporarily become pending?**
Its publication rule concerns the exact current tip.
A new tip may not yet have the required finality evidence.
Do not substitute stale values and label them current finality.

**Does this prevent every possible fork or unauthorized deployment?**
No. Consensus validation, governance trust, local security, operational
coordination, and software licensing are distinct boundaries.
The implementation does not make hostile forks logically impossible.

## 14. Developer map and verification

All paths and commands in this guide are relative to the **public Go repository
root**, where `go.mod` resides.
The source inventory below also uses public-root paths.

| Area | Starting point |
| --- | --- |
| Actions, ownership, signing, payout keys | [core/lpod_position.go](core/lpod_position.go) |
| Checkpoint transaction | [core/lpod.go](core/lpod.go) |
| Tiers, rounding, reward conservation | [lpod/accounting.go](lpod/accounting.go) |
| Position transitions, partial returns, arrears | [store/lpod_positions.go](store/lpod_positions.go) |
| Checkpoint persistence and payout validation | [store/lpod.go](store/lpod.go) |
| Historical allocation proof and attestations | [store/lpod_migration.go](store/lpod_migration.go) |
| Canonical preparation and stateful filtering | [consensus/lpod.go](consensus/lpod.go) |
| Local native wallet signing | [cmd/wallet-wasm/lpod.go](cmd/wallet-wasm/lpod.go) |
| Wallet index and rollback | [store/lpod_wallet_index.go](store/lpod_wallet_index.go) |
| Pool/owner projections | [api/lpod_pool.go](api/lpod_pool.go), [api/lpod_positions.go](api/lpod_positions.go) |
| Discovery route | [api/lpod_wallet_outputs.go](api/lpod_wallet_outputs.go) |
| Startup witness loading | [cmd/node/lpod.go](cmd/node/lpod.go) |

`LPoDPositionAction`, `LPoDPosition`, `LPoDCheckpoint`, and
`LPoDMigration` are distinct data structures, not interchangeable balances.
Use the signing/validation helpers rather than copying an API balance into
a transaction and expecting ownership to follow.

For an appropriately authorized development environment, relevant checks are:

```sh
go test ./lpod ./store ./core ./config ./consensus ./api ./cmd/node ./avm ./node -count=1
go test ./cmd/wallet-wasm -run LPoD -count=1
go test -race ./lpod ./store ./consensus -run 'LPoD|Arrears' -count=1
```

The complete WASM/discovery integration can be reproduced, from the Go root
**within the authorized full development workspace**, with:

```sh
LPOD_WASM_E2E=1 go test ./consensus -run TestLPoDCanonicalWalletPayoutDiscovery -count=1 -v
```

That opt-in path invokes the JavaScript integration harness and its package
manager. The harness is not included in the standalone public Go repository;
do not treat the opt-in command as a self-contained public-repository script.
Without the opt-in environment variable, the Go discovery test still exercises
its native index/API and canonical rollback portion.

See [position integration tests](consensus/lpod_positions_test.go),
[arrears/exit tests](store/lpod_positions_test.go),
[migration regressions](store/lpod_migration_test.go),
[finality regressions](consensus/lpod_finality_test.go),
[local signer tests](cmd/wallet-wasm/lpod_test.go), and
[wallet discovery tests](consensus/lpod_wallet_discovery_test.go).
Tests cover real deposits/refunds and ordinary spending, partial exits/topups,
APR boundaries, replay/overdraw rejection, restart/rollback, and preserved
validator unbonding. Depleted-reserve and API projection fixtures explicitly
isolate arithmetic/projection cases; they are not authentic production witnesses.
Passing tests do not replace independent multi-node validation or authorization.

## 15. Software approval is not protocol activation

Prior explicit written approval under [LICENSE-LPOD](LICENSE-LPOD) concerns
developer activities such as execution, development, deployment, forking, or
reuse of Covered Code. It is separate from an end user's supported transaction
on an authorized deployment and separate from any chain-level activation.
Requests must use the verified official route
[@sup_apro_bot](https://t.me/sup_apro_bot); a request or automated response is
not approval. Issued permissions will be published in the official
[Aperod LPoD permission registry](https://aperod.com/vaults#lpod-permissions), which
currently records no permissions. Publication does not expand an approval
beyond its identified recipient, Covered Code, permitted activity, or other
stated terms.

## 16. Exact Covered Code inventory

Covered Code is limited to the original copyrightable source-code expression
of the Aperod APRO team in the following **27 public-repository paths**, each marked
with `SPDX-License-Identifier: LicenseRef-Aperod-LPoD`:

- `api/lpod_pool.go`
- `api/lpod_pool_test.go`
- `api/lpod_positions.go`
- `api/lpod_positions_test.go`
- `api/lpod_wallet_outputs.go`
- `cmd/node/lpod.go`
- `cmd/wallet-wasm/lpod.go`
- `cmd/wallet-wasm/lpod_js_wasm.go`
- `cmd/wallet-wasm/lpod_test.go`
- `consensus/lpod.go`
- `consensus/lpod_finality_test.go`
- `consensus/lpod_positions_test.go`
- `consensus/lpod_test.go`
- `consensus/lpod_wallet_discovery_test.go`
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
- `store/lpod_wallet_index.go`

The inventory does **not** cover shared files merely modified to call LPoD,
documentation, unrelated Aperod code, dependencies, generated code, or
third-party material. If any inventoried file contains such material,
[LICENSE-LPOD](LICENSE-LPOD) applies only to the original portions owned by the
applicable copyright holder.

## 17. License boundary

The Aperod APRO team has not issued permission to execute, develop, deploy,
fork, distribute, or reuse Covered Code. Prior explicit written approval is
required for every such use. All rights not expressly granted for inspection
are reserved.

Issued permissions will be recorded at
[aperod.com/vaults#lpod-permissions](https://aperod.com/vaults#lpod-permissions).
The registry is currently empty. A registry listing is not a general license
and does not authorize use outside the recipient and scope stated in the
written approval.

The root Apache License 2.0 applies separately to non-Covered Code identified
under it. Dependencies and third-party material remain subject to their own
terms. `LICENSE-LPOD` claims no ownership of abstract ideas, methods, protocol
concepts, or independently developed code; it governs only the designated
original copyrightable expression. See [NOTICE](NOTICE).