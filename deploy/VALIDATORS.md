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

## One-time Fix: validator.key File Permissions (existing validators)

If your validator node was installed before **July 2026**, the key file may have
wrong ownership or permissions, causing the node to fail with
`permission denied` or `unsafe permissions` on the next binary update.

Run this one-time fix **on the validator server**:

```bash
sudo chown aperod:aperod /etc/aperod/validator.key
sudo chmod 600 /etc/aperod/validator.key
```

Verify:
```bash
sudo ls -la /etc/aperod/validator.key
# expected: -rw------- aperod aperod ...
```

Newly installed validators (using the current `install-validator.sh`) have the
correct ownership set automatically.

---

## Managing Bootnodes Safely

**Never edit `node.yaml` bootnodes with `sed` or by hand** — a missed newline
silently merges entries into invalid YAML, which crash-loops the node.

Use `node-config.sh` instead:

```bash
# List current bootnodes
sudo bash /opt/aperod/blockchain/deploy/node-config.sh list-bootnodes

# Add a bootnode (Python rewrites the YAML; validates before writing)
sudo bash /opt/aperod/blockchain/deploy/node-config.sh add-bootnode /ip4/1.2.3.4/tcp/30303

# Remove a bootnode by exact address
sudo bash /opt/aperod/blockchain/deploy/node-config.sh remove-bootnode /ip4/1.2.3.4/tcp/30303
```

The script:
1. Parses the existing YAML with Python (not sed), so list structure is preserved.
2. Checks for duplicates before adding.
3. Shows a diff of exactly what changed before writing.
4. Validates the result with `aperod-node --validate-config` (falls back to
   `python3 yaml.safe_load` if the binary is not on PATH).
5. Only writes the file after validation passes — the original is never
   truncated on failure.

After any change, restart the node:

```bash
sudo systemctl restart aperod-node
```

Override the config path with `APEROD_CONFIG=/path/to/node.yaml` if your file
is not at the default `/etc/aperod/node.yaml`.

---

## Validating node.yaml Without Starting the Node

```bash
aperod-node --config /etc/aperod/node.yaml --validate-config
# config OK: /etc/aperod/node.yaml (network=mainnet)
```

Exits 0 on success, non-zero with an error message on any parse or semantic
validation failure.  Safe to run while the node is running — it does not
open any ports or touch the database.

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
