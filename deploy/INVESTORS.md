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

Aperod is a privacy-preserving proof-of-authority blockchain built for real-world
payments and gaming.  Every transaction uses **RingCT** — the same scheme that powers
Monero — so balances and sender/receiver identities are hidden on-chain.

Key differentiators:

| Property | Value |
|---|---|
| Consensus | Permissioned PoA (stake-weighted, rotating) |
| Privacy | RingCT confidential transactions |
| Native token | APRO (1 APRO = 10⁸ nAPRO) |
| Total supply (genesis) | **10 000 000 000 APRO** (10 billion) |
| Fee model | EIP-1559: 100 % of base fee is **permanently burned** |
| Block time | ~2 seconds |
| Native wallet | Telegram bot `@aperod_bot` |
| Explorer | [aperod.com/explorer](https://aperod.com/explorer/) |

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

**Emission:** Validators earn a fixed block reward per block produced.  Rewards are
halved every **21 024 000 blocks** (≈ 486 days at 2 s/block).  The first halving
reduces the reward to ~50 % of genesis, the second to 25 %, and so on — mirroring
Bitcoin's halving model but on a faster schedule because block time is 10× shorter.

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

To join the active validator set a node must stake at least **10 000 APRO**
(10 000 000 000 000 nAPRO).  This is fixed by the protocol; the staking UI enforces
it, and the Go node rejects partial-unstake requests that would drop a validator
below the minimum.

### Block rewards

```
Phase 1 (genesis)  :  BlockReward  nAPRO per block produced
Phase 2 (halving 1): ÷2 after 21 024 000 blocks (~486 days)
Phase 3 (halving 2): ÷4 …
```

Rewards are issued as **transparent coinbase outputs** (not RingCT) so that
validators can track their earned APRO without needing a ring-signature scan.

### Unbonding

Full withdrawal: **7 200 blocks** (~4 hours) unbonding period.  
Partial (excess) withdrawal: **604 800 blocks** (~7 days) unbonding period.

Partial unstake lets a validator remove excess stake above 10 000 APRO without
losing their validator slot, subject to the 7-day lock.

### Running costs (reference, not a promise)

| Item | Estimated cost / month |
|---|---|
| VPS (2 vCPU, 4 GB RAM) | $10 – $30 |
| Bandwidth (1 TB) | included or $5 |
| **Total** | **$15 – $35** |

Break-even: at the genesis block reward rate and current APRO price, payback is
typically reached within 6–12 months — exact figures depend on network TPS.

---

## 5. Three Demand Scenarios

The following scenarios are illustrative and do **not** constitute financial advice.

| Scenario | Driver | Supply Δ | Price target |
|---|---|---|---|
| **A · Conservative** | Deflation only, demand stable | −9.87 % by halving 2 | $0.001 → ~$0.00111 (+10.9 %) |
| **B · Realistic Web3** | 20–30 games, organic demand ×10–15 | −10 % supply + demand shock | $0.001 → ~$0.011 (+1 100 %) |
| **C · Global GameFi hub** | Top-tier L1, 3–4 B tokens burned (25 yr) | dominant supply sink | $0.001 → $0.15 – $0.30 (+15 000 – 30 000 %) |

Scenario A requires nothing beyond the protocol working as designed.  
Scenario B requires game developer adoption.  
Scenario C is a maximum-extrapolation long-horizon model.

---

## 6. How to Become a Validator

### Quick-start (Linux / Ubuntu 22.04+)

```bash
# 1. Install the node binary
curl -fsSL https://aperod.com/install-validator.sh | bash

# 2. Generate a validator key
aperod wallet keygen --network mainnet --out /opt/aperod/validator.key

# 3. Configure the node
cp /opt/aperod/config/mainnet.yaml.example /opt/aperod/config/mainnet.yaml
# Set: validator_key, reward_address, p2p.external_ip

# 4. Start the service
systemctl enable aperod --now

# 5. Stake APRO
#    Send a StakeDeposit transaction via the admin panel or:
aperod tx stake --amount 10000 --config /opt/aperod/config/mainnet.yaml
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
| Outbound ports | 26656 (P2P) | 26656 + 8080 (API) |

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
| Block explorer | https://aperod.com/explorer/ |
| Tokenomics | https://aperod.com/explorer/tokenomics |
| Telegram wallet | https://t.me/aperod_bot |
| Public node repo | https://github.com/aperod-network/aperod-node |
| Security policy | [SECURITY.md](SECURITY.md) |
| Burn policy | [BURN_POLICY.md](BURN_POLICY.md) |
| Validator guide (RU) | [VALIDATORS.md](VALIDATORS.md) |

---

*Last updated: July 2026.  This document is provided for informational purposes only
and does not constitute financial, legal, or investment advice.*
