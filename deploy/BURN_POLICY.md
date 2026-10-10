# Aperod Fee Burn Policy

> **100 % of the protocol base fee is permanently burned.**
> This is a protocol-level rule — not a governance parameter, not toggleable by validators.

---

## Summary

| Parameter | Value |
|-----------|-------|
| Transaction fee | **dynamic EIP-1559** — base 200 nAPRO/byte, adjusts ±12.5%/block |
| Typical P2P fee | **≈ 0.004 APRO** (2 KB transfer at genesis base fee) |
| Typical game/NFT fee | **≈ 0.008 APRO** (4 KB tx at genesis base fee) |
| Base-fee destination | 🔥 Burned **100%** — removed from supply forever |
| Priority tip | Optional validator compensation; not part of the protocol burn |
| Genesis supply | **10,000,000,000 APRO** (10B) |
| Circulating at launch | **9,000,000,000 APRO** (9B — 90% Public/IDO/Liquidity) |
| Dev Fund | **1,000,000,000 APRO** (10%, 12-month cliff + 48-month linear vest) |
| Block time | **3 seconds** |
| Block throughput | **28,800 blocks / day** |
| Pool-phase reward | **3 APRO** (**300,000,000 nAPRO**) per block |
| Pre-allocated reward pool | **2,000,000,000 APRO** |
| Pool-phase issuance | **0 APRO** — rewards redistribute genesis supply |
| Tail emission | **1 APRO** per block after pool exhaustion (~63 years) |
| Halving | **None** |

---

## How the Burn Works

Every time a transaction is included in a block, the network charges a **dynamic EIP-1559 fee**:  
`minimum_fee = tx_size_bytes × base_fee_per_byte`

The **protocol base fee is burned 100%**. An optional priority tip may be paid
to the block proposer; it is separate from the burned base fee. No base-fee
revenue is routed to a validator or treasury.

```
User sends TX  →  base fee burned by consensus  →  supply decreases
              ↘ optional priority tip paid to proposer
```

The base fee adjusts ±12.5% per block toward a 500 KB target block size (same mechanism as Ethereum EIP-1559). At genesis base fee (200 nAPRO/byte):

| Transaction type | Typical size | Fee burned |
|-----------------|-------------|------------|
| P2P transfer | ~2 KB | **≈ 0.004 APRO** |
| Game / NFT tx | ~4 KB | **≈ 0.008 APRO** |

The burn is recorded in the `emission_config` table and visible on the [Block Explorer](https://aperod.com/explorer/) under **Tokenomics → Burned**.

---

## Why 100 % Burn?

Aperod is designed to offset tail emission through **deflationary usage**.
During the pool phase, rewards redistribute pre-allocated genesis supply, so
network fees reduce total supply from the first transaction.

- Validators are incentivised by **block rewards and optional priority tips**, while the complete base fee is removed from supply.
- A deterministic size-based base fee makes transaction costs easier to reason about for users and developers.
- Full burn prevents any privileged party from capturing fee revenue.

---

## Emission Schedule

| Phase | Block Reward | Supply effect | Duration |
|-------|--------------|---------------|----------|
| Validator pool | **3 APRO** | No issuance; drawn from the pre-allocated 2B APRO pool | ~63 years |
| Tail emission | **1 APRO** | 1 APRO minted per block, offset by fee burn | After pool exhaustion |

> **Block math:** 28,800 blocks/day × 365 days = 10,512,000 blocks/year.
> The reward does not halve.

---

## Cumulative Burn

The running total of burned APRO is tracked on-chain and exposed via the API:

```
GET https://aperod.com/api/v1/tokenomics
```

```json
{
  "total_burned_apro": 12450.5,
  "total_burned_napro": "1245050000000",
  "burn_height_cursor": 248010
}
```

The same figure is shown on the **Tokenomics** page of the [Block Explorer](https://aperod.com/explorer/tokenomics).

---

## Code Reference

The burn and reward policy are enforced in `consensus/poa.go`. Base fees are
never routed to a validator or treasury; only an explicit priority tip may be
paid to the proposer.

---

*[aperod-network](https://github.com/aperod-network) — [aperod.com](https://aperod.com)*
