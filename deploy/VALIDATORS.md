# Aperod Validator Rules

> **Network:** Aperod Mainnet &nbsp;·&nbsp; **Consensus:** Proof of Authority (stake-weighted, permissionless)

This document is the authoritative specification for validator participation in the Aperod network. All rules are enforced at the protocol level in `consensus/poa.go`.

---

## Table of Contents

- [Overview](#overview)
- [Eligibility](#eligibility)
- [Active Validator Set](#active-validator-set)
- [Epoch & Rotation](#epoch--rotation)
- [Rewards](#rewards)
- [Liveness Requirements](#liveness-requirements)
- [Slashing](#slashing)
- [Unbonding & Withdrawal](#unbonding--withdrawal)
- [Setup Guide](#setup-guide)

---

## Overview

Aperod uses a **permissionless, stake-weighted Proof-of-Authority** consensus:

- Validators are selected by stake size — no operator approval or whitelist.
- The **top 21 nodes** by staked APRO form the active set at any given epoch.
- Block proposers rotate in round-robin order among active validators.
- A block is finalised when **≥ 2/3 of active validators** sign it (BFT threshold).

---

## Eligibility

Any node may become a validator by meeting **all** of the following conditions:

| Condition | Requirement |
|-----------|-------------|
| Stake | ≥ **100,000 APRO** locked on-chain |
| Node software | Latest `aperod-node` binary, fully synced |
| Consensus key | Valid Ed25519 key registered on-chain |
| Reward address | Valid APRO wallet address configured |
| Connectivity | Port **30303/tcp+udp** reachable from the internet |
| Ranking | Top 21 by total stake at epoch boundary |

There is no application, KYC, or governance vote required.

---

## Active Validator Set

| Parameter | Value |
|-----------|-------|
| Active validators | **21** (maximum) |
| Selection criterion | Top 21 by total staked APRO |
| Set refresh | Every epoch boundary (every 100 blocks) |
| Churn limit | **3** new validators per epoch (prevents instability) |

If fewer than 21 nodes meet the minimum stake, the active set is smaller.  
A node ranked 22nd or below is **standby** — it does not produce blocks or earn rewards, but retains its stake.

---

## Epoch & Rotation

| Parameter | Value |
|-----------|-------|
| Epoch length | **100 blocks** (~300 seconds at 3 s/block) |
| Block time target | **3 seconds** |
| Proposer selection | Round-robin within the active set |
| Finality | BFT — block is final once 2/3 validators have signed |

At each epoch boundary the consensus engine:
1. Reads the current stake rankings from the UTXO set.
2. Computes the new active set (top 21, churn ≤ 3).
3. Rotates out validators that fell below rank 21.
4. Activates standby validators that entered the top 21.

---

## Rewards

| Item | Detail |
|------|--------|
| Block reward | **5 APRO** per block produced |
| Reward destination | Validator's configured `reward_address` (Telegram wallet) |
| Notification | Telegram push notification on every reward payment |
| Fee share | **0 %** — all fees are burned, none go to validators |
| Halving interval | Every **21,024,000 blocks** (~2 years) |

Block rewards are minted directly to the `reward_address` in `node.yaml`.  
You must configure a valid APRO address — rewards cannot be redirected after a block is produced.

---

## Liveness Requirements

Validators are expected to be online and signing continuously.

| Metric | Threshold |
|--------|-----------|
| Minimum blocks signed per epoch | **≥ 2/3 of epoch blocks** (≥ 67 of 100) |
| Maximum consecutive missed blocks | 50 before downtime flag is raised |
| Uptime target | ≥ 99 % |

Falling below the liveness threshold in an epoch triggers a **downtime penalty** (see Slashing).

---

## Slashing

Slashing is automatic, on-chain, and non-reversible.

### Double-Sign (Critical)

Signing two conflicting blocks at the same height.

| Consequence | Detail |
|-------------|--------|
| Stake penalty | **10 % of total stake** burned immediately |
| Set removal | Permanent ban — node cannot re-enter the validator set |
| Evidence window | Any time after the double-sign is detected |

Double-sign protection is enforced in the node software. Running duplicate processes on the same key is dangerous.

### Extended Downtime

Failing to sign ≥ 2/3 of blocks in an epoch (liveness violation).

| Consequence | Detail |
|-------------|--------|
| Stake penalty | **5 % of total stake** deducted |
| Set removal | Removed from active set at epoch boundary |
| Re-entry | Allowed after restaking to ≥ 100,000 APRO |

### Stake Falls Below Minimum

If a validator's stake drops below 100,000 APRO (due to slashing or partial withdrawal):

- Removed from active set at next epoch boundary.
- Must top up stake to re-enter.

---

## Unbonding & Withdrawal

Staked APRO is locked while the validator is active. To withdraw:

| Step | Detail |
|------|--------|
| 1. Initiate unbonding | Send unbonding request via [@aperod_bot](https://t.me/aperod_bot) |
| 2. Unbonding period | **7,200 blocks** (~6 hours) — node leaves active set |
| 3. Funds released | APRO returned to wallet after unbonding period |

Partial unbonding is supported. If remaining stake drops below 100,000 APRO, the node exits the validator set.

Withdrawals cannot be cancelled once initiated.

---

## Setup Guide

Complete installation instructions are in the main [README](README.md).

```bash
# Install validator node (interactive)
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash

# Uninstall
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/uninstall-validator.sh | sudo bash
```

**Key file locations after install:**

| File | Path |
|------|------|
| Node config | `/etc/aperod/node.yaml` |
| Consensus key | `/etc/aperod/validator.key` |
| Blockchain data | `/var/lib/aperod/` |
| Service logs | `journalctl -u aperod-node -f` |

---

## Updating the Node Binary

**Always use `update-node.sh` — never build and copy manually.**

Two silent failure modes occur with a manual update:

| Mistake | Symptom |
|---------|---------|
| Building to the wrong path (e.g. `/opt/aperod/data/aperod-node`) | Binary never picked up — service runs old code silently |
| Copying over a running binary | `Text file busy` (ETXTBSY) — copy fails, old binary still running |

The update script handles both correctly — it stops the service first, then installs to `/usr/local/bin/aperod-node` (the path the `aperod-node.service` unit executes), then starts the service and waits for the API to respond.

```bash
sudo bash /opt/aperod/blockchain/deploy/update-node.sh
```

The script performs these steps in order:

1. `git pull` latest source
2. `make build` — if this fails, the service is **not stopped** (old binary keeps running)
3. `systemctl stop aperod-node`
4. `cp build/aperod-node /usr/local/bin/aperod-node`
5. `systemctl start aperod-node`
6. Polls `http://localhost:8545/api/v1/status` until the node responds (sends a Telegram alert on failure)

**Optional env vars:**

```bash
SUPPORT_BOT_TOKEN=<token> \
SUPPORT_ADMIN_CHAT_ID=<chat_id> \
  sudo bash /opt/aperod/blockchain/deploy/update-node.sh
```

Set `SKIP_HEALTH_CHECK=1` if the RPC port is not exposed on this machine.

---

*Rules enforced by protocol — `consensus/poa.go`, `core/chain.go`.*  
*Last updated: 2024 — [aperod-network](https://github.com/aperod-network)*
