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
| Total supply cap | **10,000,000,000 APRO** (10B) |
| Circulating at launch | **9,000,000,000 APRO** (9B — 90% Public/IDO/Liquidity) |
| Dev Fund | **1,000,000,000 APRO** (10%, 12-month cliff + 48-month linear vest) |
| Block time | **3 seconds** |
| Block throughput | **28,800 blocks / day** |
| Block reward | **5 APRO** per block |
| Annual emission | **52,560,000 APRO / year** (≈ 52.56 M APRO) |
| Per-validator income | **≈ 2,503,000 APRO / year** (21 validators, round-robin) |
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

Aperod is designed to be **deflationary by usage**. The more the network is used, the faster supply shrinks toward zero.

- Validators are incentivised by **block rewards**, not by fee extraction — aligning their interests with network uptime rather than fee maximisation.
- A predictable flat fee makes transaction costs easier to reason about for users and developers.
- Full burn prevents any privileged party from capturing fee revenue.

---

## Emission Schedule

| Era | Blocks | Block Reward | Annual emission | Duration (~) |
|-----|--------|--------------|-----------------|--------------|
| 1 | 0 – 21,023,999 | **5 APRO** | ~52,560,000 APRO | ~2 years |
| 2 | 21,024,000 – 42,047,999 | **2.5 APRO** | ~26,280,000 APRO | ~2 years |
| 3 | 42,048,000 – 63,071,999 | **1.25 APRO** | ~13,140,000 APRO | ~2 years |
| … | … | halved each era | halved each era | … |

> **Block math:** 28,800 blocks/day × 365 days = 10,512,000 blocks/year.  
> Halving every 21,024,000 blocks ≈ every 2 years.

Total newly minted APRO converges to a finite cap. Combined with the continuous fee burn, the **real circulating supply decreases over time**.

---

## Cumulative Burn

The running total of burned APRO is tracked on-chain and exposed via the API:

```
GET https://aperod.com/api/v1/tokenomics
```

```json
{
  "total_burned_apro": 12450.5,
  "total_burned_napro": 1245050000000,
  "burn_height_cursor": 248010
}
```

The same figure is shown on the **Tokenomics** page of the [Block Explorer](https://aperod.com/explorer/tokenomics).

---

## Code Reference

The burn is enforced in `consensus/poa.go` — the `tick()` function that produces each block.  
There is no code path that routes fees to a reward address. Auditors are encouraged to verify this directly.

---

*[aperod-network](https://github.com/aperod-network) — [aperod.com](https://aperod.com)*
