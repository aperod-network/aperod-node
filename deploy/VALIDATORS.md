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
| Pool-phase reward | **3 APRO** per block produced |
| Reward pool | **2,000,000,000 APRO**, pre-allocated at genesis |
| Tail emission | **1 APRO** per block after pool exhaustion (~63 years) |
| Reward destination | Validator's configured `reward_address` (Telegram wallet) |
| Notification | Telegram push notification on every reward payment |
| Fee share | None — every transaction fee is burned 100% |
| Halving | **None** |

During the pool phase each produced block transfers
**300,000,000 nAPRO (3 APRO)** from the pre-allocated validator pool. This
redistributes existing genesis supply and does not mint new APRO. After the pool
is exhausted, the protocol mints a constant **100,000,000 nAPRO (1 APRO)** tail
reward per block. The configured `reward_address` must be a valid APRO address.

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
| 1. Initiate unbonding | Send unbonding request via [@sup_apro_bot](https://t.me/sup_apro_bot) |
| 2. Unbonding period | **144,000 blocks** (~5 days) — node leaves active set |
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

## Keeping Validator Binaries in Sync with the Main Node

> **Critical:** The main node and every validator must run the **same binary
> version** at all times.  When the main node gains a new feature (e.g. TLS
> P2P encryption), validators running an older binary fail the handshake
> silently — `peer_count` stays 0 for hours with no obvious error in the logs.

### Standard update procedure

Every time you update the main node, push the new binary to all validators
immediately afterwards:

```bash
# Step 1 — Update the main node (builds binary, runs health + peer checks).
sudo bash /opt/aperod/blockchain/deploy/update-node.sh

# Step 2 — Push the same binary to every validator in validators.conf.
sudo bash /opt/aperod/blockchain/deploy/update-validator.sh
```

That's it.  The two scripts must always be run as a pair.

### Validator inventory: validators.conf

The list of validator SSH targets lives in `deploy/validators.conf` (one
`user@host` per line, `#` = comment).  Edit this file to add or remove
validators:

```
# deploy/validators.conf
aperod@203.0.113.10
aperod@203.0.113.20
```

> **Security note:** If this repository is public, keep real production IPs in
> a local file and point the script at it with the `VALIDATORS_CONF` env var:
> ```bash
> VALIDATORS_CONF=/etc/aperod/validators.conf \
>   sudo bash /opt/aperod/blockchain/deploy/update-validator.sh
> ```

### Updating a single validator

Pass the SSH target as a command-line argument to skip the conf file:

```bash
sudo bash /opt/aperod/blockchain/deploy/update-validator.sh aperod@203.0.113.10
```

### What update-validator.sh does

For each validator listed in `validators.conf` (or passed as arguments):

1. **SCP** the binary from `/usr/local/bin/aperod-node` on the main server to
   a temporary path on the validator.
2. **Stop** the `aperod-node` service (`systemctl stop`).
3. **Install** the binary to `/usr/local/bin/aperod-node`.
4. **Start** the service.
5. **Health check** — polls `http://127.0.0.1:8545/api/v1/status` until the
   node responds; fires a Telegram alert on failure.

On failure the script reports which validators failed and exits non-zero.

### Requirements

- SSH key-based access to every validator (no password prompts).  
  Default key: `~/.ssh/id_ed25519` — override with `SSH_KEY=<path>`.
- The `aperod` user on each validator has `sudo` rights for `systemctl`.
- Port 22 reachable from the main server to every validator.
- Every validator's SSH host key must be pre-recorded in the known-hosts
  file **before** the first run (see below).

### First-time host-key verification (run once per new validator)

The script uses `StrictHostKeyChecking=yes` — it will **refuse** to connect
to any host whose key is not already recorded.  This prevents MITM attacks
where an attacker intercepts the binary push.

Add a validator's key once, **after verifying the fingerprint out-of-band**
(e.g. via your cloud provider's console or an independent channel):

```bash
# 1. Fetch and display the fingerprint — verify it matches your provider:
ssh-keyscan -H <validator-ip> 2>/dev/null | ssh-keygen -lf - -E sha256

# 2. Once verified, append the key to the deploy known-hosts file:
sudo mkdir -p /etc/aperod
ssh-keyscan -H <validator-ip> | sudo tee -a /etc/aperod/validator_known_hosts
sudo chmod 600 /etc/aperod/validator_known_hosts
```

If a validator's host key changes (e.g. server was reprovisioned), the script
aborts with a clear error.  Remove the stale entry and add the new key after
re-verifying the fingerprint:

```bash
ssh-keygen -R <validator-ip> -f /etc/aperod/validator_known_hosts
ssh-keyscan -H <validator-ip> | sudo tee -a /etc/aperod/validator_known_hosts
```

### Optional env vars

| Variable | Default | Purpose |
|----------|---------|---------|
| `VALIDATORS_CONF` | `deploy/validators.conf` | Path to the inventory file |
| `SSH_KEY` | `~/.ssh/id_ed25519` | SSH private key |
| `KNOWN_HOSTS_FILE` | `/etc/aperod/validator_known_hosts` | Pre-verified host keys |
| `BINARY_SRC` | `/usr/local/bin/aperod-node` | Binary to push |
| `SKIP_HEALTH_CHECK` | `0` | Set to `1` to bypass health polling |
| `HEALTH_MAX_ATTEMPTS` | `15` | Poll attempts before declaring failure |
| `HEALTH_WAIT_SECS` | `2` | Seconds between health polls |
| `SUPPORT_BOT_TOKEN` | — | Telegram bot token for failure alerts |
| `SUPPORT_ADMIN_CHAT_ID` | — | Telegram chat ID for failure alerts |

---

## Updating the Main Node Binary

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
7. Polls `http://localhost:8545/api/v1/network/stats` for up to 30 s — warns if `peer_count` is still 0 (non-fatal)

**After running update-node.sh, always run update-validator.sh (see above).**

**Optional env vars:**

```bash
SUPPORT_BOT_TOKEN=<token> \
SUPPORT_ADMIN_CHAT_ID=<chat_id> \
  sudo bash /opt/aperod/blockchain/deploy/update-node.sh
```

Set `SKIP_HEALTH_CHECK=1` if the RPC port is not exposed on this machine.

---

*Rules enforced by protocol — `consensus/poa.go`, `core/chain.go`.*  
*Last updated: July 2026 — [aperod-network](https://github.com/aperod-network)*
