# Aperod (APRO) — International Investor & Validator Guide

> **Audience:** English-speaking validators, node operators, and investors evaluating
> APRO as an asset. For the Russian-language version see `VALIDATORS.md`.

---

## Table of Contents

1. [What is Aperod?](#1-what-is-aperod)
2. [Token Economics at a Glance](#2-token-economics-at-a-glance)
3. [The Deflationary Case](#3-the-deflationary-case)
4. [Validator Economics](#4-validator-economics)
5. [Three Demand Scenarios](#5-three-demand-scenarios)
6. [How to Become a Validator](#6-how-to-become-a-validator)
7. [Risk Factors](#7-risk-factors)
8. [Useful Links](#8-useful-links)

---

## 1. What is Aperod?

Aperod is a privacy-preserving BFT-PoS blockchain built for real-world payments
and gaming. Confidential transfers use **RingCT** with Pedersen commitments,
Bulletproof range proofs, and stealth addresses. **CLSAG v5** is active on the
live network from coordinated activation block 1,769,500.
Coinbase and staking transactions follow separate consensus formats.

Key differentiators:

| Property | Value |
|---|---|
| Consensus | Permissionless BFT-PoS (stake-weighted active set, rotating proposer, signed ≥2/3 finality) |
| Privacy | RingCT; active CLSAG v5 with 16-member rings |
| Native token | APRO (1 APRO = 10⁸ nAPRO) |
| Total supply (genesis) | **10 000 000 000 APRO** (10 billion) |
| Fee model | EIP-1559: 100 % of base fee is **permanently burned** |
| Block time target | 3 seconds |
| Native wallet | Telegram bot `@sup_apro_bot` |
| Explorer | [explorer.aperod.com](https://explorer.aperod.com/) |

---

## 2. Token Economics at a Glance

```
Allocation                     Amount (APRO)     %
──────────────────────────────────────────────────
Validator rewards (block)      vesting / per-block  ~15 %
Team & development             2-year cliff linear  ~20 %
Ecosystem fund                 4-year linear        ~25 %
Public (launch / exchanges)    immediate            ~40 %
```

> Exact vesting schedules are visible on the
> [Tokenomics page](https://aperod.com/explorer/tokenomics).

**Emission:** Validators receive **3 APRO per block** from a pre-allocated
**2B APRO pool**. Pool rewards do not mint new supply and do not halve. After
the pool is exhausted (approximately 63 years at the 3-second target), the
protocol switches to a constant **1 APRO per block** tail emission.

**Fee burn:** Every byte of every transaction costs a dynamic base fee.  That fee is
**destroyed** — not paid to validators, not redistributed — which permanently reduces
circulating supply every time the network is used.

---

## 3. The Deflationary Case

### EIP-1559 mechanics

The base fee adjusts ±12.5 % per block to target 50 % block utilisation (500 KB
per 1 MB max block).  Current floor: **50 nAPRO / byte**.

A typical P2P payment is ~2 KB → **≈ 0.004 APRO burned per transaction.**  
A typical game move is ~4 KB → **≈ 0.008 APRO burned per move.**

### Steady-state burn estimate

With 5 000 daily transactions (conservative mainnet), annual burn ≈:

```
5 000 tx/day × 365 days × 0.004 APRO/tx ≈ 7 300 APRO/year
```

Once GameFi ecosystems launch (target: 20–30 integrated games), the multiplier
rises to tens of millions of APRO per year — see scenarios below.

---

## 4. Validator Economics

### Minimum stake

To join the active validator set a node must stake at least **100,000 APRO**
(10,000,000,000,000 nAPRO). This is fixed by the protocol; the staking UI enforces
it, and the Go node rejects partial-unstake requests that would drop a validator
below the minimum.

### Block rewards

```
Pool phase    : 300,000,000 nAPRO (3 APRO) per block from the 2B APRO pool
Tail emission : 100,000,000 nAPRO (1 APRO) per block after pool exhaustion
Halving       : none
```

Rewards are issued as **transparent coinbase outputs** (not RingCT) so that
validators can track their earned APRO without needing a ring-signature scan.

### Unbonding

Full withdrawal: **144,000 blocks** (~5 days) unbonding period.
Partial (excess) withdrawal: **43,200 blocks** (~36 hours) unbonding period.

Partial unstake lets a validator remove excess stake above 100,000 APRO without
losing their validator slot, subject to the partial-unbonding lock.

### Running costs (reference, not a promise)

| Item | Estimated cost / month |
|---|---|
| VPS (2 vCPU, 4 GB RAM) | $10 – $30 |
| Bandwidth (1 TB) | included or $5 |
| **Total** | **$15 – $35** |

Validator returns depend on produced blocks, missed slots, operating costs and
market conditions. Transaction fees are burned and are not validator income.
No payback period is guaranteed.

---

## 5. Three Demand Scenarios

The following scenarios are illustrative and do **not** constitute financial advice.

| Scenario | Driver | Supply Δ | Price target |
|---|---|---|---|
| **A · Low activity** | Reward issuance exceeds fee burn | Net supply increases | Market outcome is not predictable from protocol data alone |
| **B · Balanced activity** | Fee burn approaches reward issuance | Net supply is approximately stable | Depends on real transaction demand and fees |
| **C · High activity** | Fee burn exceeds reward issuance | Net supply decreases | Depends on sustained usage; no price target is implied |

These scenarios describe protocol supply mechanics only. They are not price
forecasts, return projections or guarantees.

---

## 6. How to Become a Validator

### Quick-start (Linux / Ubuntu 22.04+)

```bash
# Install and configure the validator service interactively.
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash

# Verify the service and local REST API.
systemctl status aperod-node
curl -s http://127.0.0.1:8545/api/v1/status
```

See [install-validator.sh](install-validator.sh) and
[install-node.sh](install-node.sh) for full automation scripts.

### System requirements

| Component | Minimum | Recommended |
|---|---|---|
| CPU | 2 vCPU x86-64 | 4 vCPU |
| RAM | 4 GB | 8 GB |
| Disk | 40 GB SSD | 200 GB NVMe |
| OS | Ubuntu 22.04 | Ubuntu 22.04 / Debian 12 |
| Network ports | 30303/tcp (P2P) | 30303/tcp; 8545/tcp on loopback for local API access |

---

## 7. Risk Factors

* **Price volatility:** APRO is a nascent asset.  Early-stage tokens can lose
  significant value.
* **Slashing:** Future protocol versions may introduce slashing for double-signing.
  Always run a single validator key per node.
* **Regulatory risk:** Privacy-focused blockchains face evolving legal treatment in
  some jurisdictions.  Consult local legal counsel.
* **Protocol risk:** The codebase is under active development.  Breaking changes
  may require node updates.
* **No guarantee of returns:** Block rewards and fee burns are protocol-defined but
  depend on actual network utilisation.

---

## 8. Useful Links

| Resource | URL |
|---|---|
| Main website | https://aperod.com |
| Block explorer | https://explorer.aperod.com/ |
| Tokenomics | https://explorer.aperod.com/tokenomics |
| Telegram wallet | https://t.me/sup_apro_bot |
| Public node repo | https://github.com/aperod-network/aperod-node |
| Security policy | [SECURITY.md](SECURITY.md) |
| Burn policy | [BURN_POLICY.md](BURN_POLICY.md) |
| Validator guide (RU) | [VALIDATORS.md](VALIDATORS.md) |

---

*Last updated: July 2026.  This document is provided for informational purposes only
and does not constitute financial, legal, or investment advice.*
