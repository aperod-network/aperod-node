# Aperod Fee Burn Policy

> **100 % of every transaction fee is permanently burned.**  
> This is a protocol-level rule — not a governance parameter, not toggleable by validators.

---

## Summary

| Parameter | Value |
|-----------|-------|
| Transaction fee | **0.5 APRO** (flat) |
| Fee destination | 🔥 Burned — removed from supply forever |
| Validator fee share | **0 %** |
| Enforcement | Protocol layer — `consensus/poa.go` |
| Total supply cap | **100,000,000 APRO** |
| Circulating at launch | **21,000,000 APRO** |
| Block time | **3 seconds** |
| Block throughput | **28,800 blocks / day** |
| Block reward | **5 APRO** per block |
| Annual emission | **52,560,000 APRO / year** (≈ 52.56 M APRO) |
| Per-validator income | **≈ 2,503,000 APRO / year** (21 validators, round-robin) |
| Halving interval | Every **21,024,000 blocks** (~2 years) |

---

## How the Burn Works

Every time a transaction is included in a block, the network charges a **flat fee of 0.5 APRO**.  
That fee is not forwarded to the block proposer or any treasury. It is **destroyed at the consensus layer** — the output simply does not exist in the next UTXO set.

```
User sends TX  →  fee output (0.5 APRO)  →  PoA engine burns it  →  supply decreases
```

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
