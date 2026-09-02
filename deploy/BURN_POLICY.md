# Aperod Fee Burn Policy

> **100 % of every transaction fee is permanently burned.**  
> This is a protocol-level rule — not a governance parameter, not toggleable by validators.

---

## Summary

| Parameter | Value |
|-----------|-------|
| Transaction fee | **dynamic EIP-1559** — base 200 nAPRO/byte, adjusts ±12.5%/block |
| Typical P2P fee | **≈ 0.004 APRO** (2 KB transfer at genesis base fee) |
| Typical game/NFT fee | **≈ 0.008 APRO** (4 KB tx at genesis base fee) |
| Base fee destination | 🔥 Burned **100%** — removed from supply forever |
| Priority tip destination | → Validator (fee − base fee × size) |
| Genesis supply | **10,000,000,000 APRO** (10B) |
| Circulating at launch | **9,000,000,000 APRO** (9B — 90% Public/IDO/Liquidity) |
| Dev Fund | **1,000,000,000 APRO** (10%, 12-month cliff + 48-month linear vest) |
| Block time | **3 seconds** |
| Block throughput | **28,800 blocks / day** |
| Authorized base reward | **0.1 APRO** (**10,000,000 nAPRO**) per block in the current era |
| Annual base issuance | **1,051,200 APRO / year** at a 3-second block target |
| Per-validator base reward | **≈ 50,057 APRO / year** with 21 equally productive validators |
| Halving interval | Every **21,024,000 blocks** (~2 years) |

---

## How the Burn Works

Every time a transaction is included in a block, the network charges a **dynamic EIP-1559 fee**:  
`fee = tx_size_bytes × (base_fee_per_byte + priority_tip_per_byte)`

The **base fee portion is burned 100%** — it is not forwarded to the block proposer or any treasury, and simply does not exist in the next UTXO set. Only the optional **priority tip** goes to the validator.

```
User sends TX  →  base fee (200 nAPRO/byte × size)  →  PoA engine burns it  →  supply decreases
                  priority tip (optional)             →  validator reward address
```

The base fee adjusts ±12.5% per block toward a 500 KB target block size (same mechanism as Ethereum EIP-1559). At genesis base fee (200 nAPRO/byte):

| Transaction type | Typical size | Fee burned |
|-----------------|-------------|------------|
| P2P transfer | ~2 KB | **≈ 0.004 APRO** |
| Game / NFT tx | ~4 KB | **≈ 0.008 APRO** |

The burn is recorded in the `emission_config` table and visible on the [Block Explorer](https://aperod.com/explorer/) under **Tokenomics → Burned**.

---

## Why 100 % Burn?

Aperod is designed to offset protocol reward issuance through
**deflationary usage**. Net supply change is validator reward issuance plus
priority tips minus the base fees burned by network activity.

- Validators are incentivised by **block rewards**, not by fee extraction — aligning their interests with network uptime rather than fee maximisation.
- A predictable flat fee makes transaction costs easier to reason about for users and developers.
- Full burn prevents any privileged party from capturing fee revenue.

---

## Emission Schedule

| Era | Blocks | Block Reward | Annual emission | Duration (~) |
|-----|--------|--------------|-----------------|--------------|
| 1 | 0 – 21,023,999 | **0.1 APRO** | ~1,051,200 APRO | ~2 years |
| 2 | 21,024,000 – 42,047,999 | **0.05 APRO** | ~525,600 APRO | ~2 years |
| 3 | 42,048,000 – 63,071,999 | **0.025 APRO** | ~262,800 APRO | ~2 years |
| … | … | halved each era | halved each era | … |

> **Block math:** 28,800 blocks/day × 365 days = 10,512,000 blocks/year.  
> Halving every 21,024,000 blocks ≈ every 2 years.

The geometric reward schedule has finite cumulative issuance. Whether total
supply rises or falls in a period depends on protocol issuance versus
consensus-enforced fee burns.

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

The burn and reward policy are enforced in `consensus/poa.go`. The base-fee
portion is never routed to a reward address; only an optional priority tip is
included in the validator's signed reward authorization.

---

*[aperod-network](https://github.com/aperod-network) — [aperod.com](https://aperod.com)*
