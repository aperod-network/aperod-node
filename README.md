<div align="center">

# APEROD · APRO

**Privacy-preserving blockchain built for the real world.**

RingCT confidential transactions &nbsp;·&nbsp; Telegram-native wallet &nbsp;·&nbsp; 100 % fee burn &nbsp;·&nbsp; Permissionless PoA consensus

[![Build Check](https://github.com/aperod-network/aperod-node/actions/workflows/build-check.yml/badge.svg)](https://github.com/aperod-network/aperod-node/actions/workflows/build-check.yml)
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/go1.25)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Network](https://img.shields.io/badge/Network-Mainnet-brightgreen)](https://aperod.com)
[![Explorer](https://img.shields.io/badge/Explorer-aperod.com-informational)](https://explorer.aperod.com/)
[![Telegram](https://img.shields.io/badge/Wallet-@aperod__bot-2CA5E0?logo=telegram&logoColor=white)](https://t.me/aperod_bot)

**[⬇ Install Node](#-install-a-full-node) &nbsp;·&nbsp; [🛡 Become Validator](#-become-a-validator) &nbsp;·&nbsp; [🌐 Explorer](https://explorer.aperod.com/) &nbsp;·&nbsp; [💬 Telegram Wallet](https://t.me/aperod_bot)**

</div>

<p align="center">
  <img src="https://github.com/aperod-network/aperod-node/blob/main/.github/images/og.png?raw=true" alt="APEROD Preview" width="100%">
</p>

---

## Table of Contents

- [Why Aperod](#-why-aperod)
- [Install a Full Node](#-install-a-full-node)
- [Become a Validator](#-become-a-validator)
- [Validator Rules](#-validator-rules)
- [Tokenomics & Fee Burn](#-tokenomics--fee-burn)
- [📈 Why APRO? The Deflationary Case](#-why-apro-the-deflationary-case)
- [Architecture](#-architecture)
- [Building from Source](#-building-from-source)
- [Requirements](#-requirements)
- [Security](#-security)
- [License](#-license)
- [Contributors](#-contributors)

---

## ✦ Why Aperod

| Feature | Detail |
|---------|--------|
| **Privacy** | MLSAG ring signatures (ring size 16), Pedersen commitments, Bulletproofs range proofs — receiver and amount hidden on-chain |
| **Stealth addresses** | Every payment generates a one-time address; receiver identity is never revealed |
| **Telegram wallet** | Full wallet inside Telegram — create, send, receive, and stake APRO without any app download |
| **100 % fee burn** | Every transaction fee is permanently destroyed, reducing total supply with every block |
| **Permissionless validators** | Anyone holding ≥ 100,000 APRO can run a validator — no whitelist, no approval needed |
| **Block rewards** | 5 APRO per block paid directly to the validator's Telegram wallet + push notification |
| **Game integration** | Native protocol support for in-game asset transfers and micropayments |
| **Open source** | Go 1.25, Apache 2.0, independently auditable cryptographic primitives |

---

## ⬇ Install a Full Node

### One-line install (Ubuntu / Debian)

```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-node.sh | sudo bash
```

Supported platforms: **Ubuntu 22.04 · Ubuntu 24.04 LTS · Debian 12 · x86\_64 · ARM64**

The node installs as a `systemd` service and connects to the Aperod network automatically.

### Verify the node is running

```bash
systemctl status aperod-node          # service status
journalctl -u aperod-node -f          # live logs
curl -s http://localhost:8545/api/v1/status | jq .   # chain tip
```

### Watchdog — configurable probe interval

The installer sets up a **watchdog timer** (`aperod-node-watchdog.timer`) that probes the node API every 60 seconds and automatically restarts the node if it stops responding.

To change the interval **without editing unit files or redeploying**:

```bash
# 1. Open the watchdog config (created automatically by the installer)
sudo nano /etc/aperod/watchdog.env

# 2. Set the desired interval in seconds (minimum: 5)
WATCHDOG_INTERVAL_SECS=15   # faster detection for HA setups
# WATCHDOG_INTERVAL_SECS=60  # default — suitable for most nodes
# WATCHDOG_INTERVAL_SECS=120  # reduced noise for low-power validators

# 3. Apply the change (writes a systemd drop-in and restarts the timer)
sudo aperod-watchdog-set-interval
```

Verify the new interval is active:

```bash
systemctl list-timers aperod-node-watchdog.timer
```

The `60 s` default is preserved for all existing deployments — only nodes that explicitly set `WATCHDOG_INTERVAL_SECS` in `watchdog.env` and run `aperod-watchdog-set-interval` will use a different value.

---

## 🛡 Become a Validator

Aperod uses **stake-weighted, permissionless validator selection.**  
The top 21 nodes by staked APRO form the active validator set — no operator approval, no whitelist.

### Step 1 — Get your Aperod wallet address

Block rewards go directly to your **Telegram wallet**. You need an APRO address before installing the node.

1. Open **[@aperod_bot](https://t.me/aperod_bot)**
2. Tap **Create wallet**
3. Copy your APRO address (≈ 95 characters, starts with `apr…`)

### Step 2 — Install the validator node

```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash
```

The installer will:
- Prompt for your APRO reward address
- Generate a **consensus key** (signs blocks only — cannot move funds)
- Configure the node as a `systemd` service and start it

**Non-interactive install (CI / cloud-init):**

```bash
APEROD_REWARD_ADDRESS=<your-apro-address> \
  curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash
```

### Step 3 — Register your node

After install, the script prints your registration command. Run it:

```bash
curl -s -X POST https://aperod.com/api/validators/apply \
  -H 'Content-Type: application/json' \
  -d '{
    "pubKey":   "<consensus-public-key>",
    "alias":    "my-validator",
    "endpoint": "/ip4/<server-ip>/tcp/30303",
    "address":  "<apro-reward-address>"
  }'
```

### Step 4 — Stake APRO

Transfer **≥ 100,000 APRO** to your wallet address via [@aperod_bot](https://t.me/aperod_bot).  
Your node enters the active set automatically at the next epoch (~100 blocks · ≈ 5 min).

### Step 5 — You're live

- Block rewards accumulate in your Telegram wallet
- You receive a Telegram notification for **every reward payment**
- Check balance and staking status anytime in the bot

---

## 📋 Validator Rules

See [**VALIDATORS.md**](VALIDATORS.md) for the complete rule set and protocol specification.

**Quick reference:**

| Parameter | Value |
|-----------|-------|
| Minimum stake | **100,000 APRO** |
| Maximum active validators | **21** |
| Epoch length | 100 blocks (~5 min) |
| Churn limit per epoch | 3 new validators |
| Unbonding period | 7,200 blocks (~6 hours) |
| Liveness requirement | Sign ≥ 2/3 of blocks per epoch |
| Slashing — double-sign | **10 % of stake, permanent ban** |
| Slashing — extended downtime | **5 % of stake** |
| Reward destination | Validator's APRO wallet address |

---

## 🔥 Tokenomics & Fee Burn

> **Every transaction fee is permanently burned. 100 %. Always.**

Aperod has a **fixed supply cap** and a **deflationary fee model**:

```
Total supply cap:     10,000,000,000 APRO  (10B)
Circulating (launch):  9,000,000,000 APRO  (9B — 90% Public / IDO / Liquidity)
Dev Fund locked:       1,000,000,000 APRO  (10%, 12-month cliff + 48-month linear vest)
Block time:            3 seconds
Block throughput:      28,800 blocks / day
Block reward:          5 APRO per block
Annual emission:       52,560,000 APRO / year  (≈ 52.56 M APRO)
Per-validator income:  ≈ 2,503,000 APRO / year (21 active validators, round-robin)
Halving interval:      every 21,024,000 blocks  (~2 years)
Transaction fee:       dynamic EIP-1559 · base 200 nAPRO/byte · adjusts ±12.5%/block
                       P2P transfer ~2 KB ≈ 0.004 APRO
                       Game / NFT tx ~4 KB ≈ 0.008 APRO
Fee destination:       🔥 base fee — burned 100% · priority tip → validator
```

Validators earn **block rewards only**. Zero fees reach validator wallets.  
The burn is enforced at the consensus layer — not a governance parameter, not toggleable.

See [**BURN\_POLICY.md**](BURN_POLICY.md) for full tokenomics and deflationary mechanics.

---

## 📈 Why APRO? The Deflationary Case

### EIP-1559 = Automatic Token Buyback

> *"The EIP-1559 mechanism in Aperod works like an automatic buyback — for every type of transaction. A simple wallet transfer burns APRO. An NFT trade burns APRO. A DeFi swap burns APRO. A game action burns APRO. The more the network is used for anything, the fewer coins remain in circulation. By year 5, even modest everyday usage alone shrinks the supply by ~10%, creating organic scarcity that pushes APRO price up — without any manipulation."*

Every on-chain transaction type permanently destroys the base fee:

| Transaction type | Approx. fee burned per tx |
|-----------------|--------------------------|
| P2P transfer (~2 KB) | ~0.004 APRO |
| Token swap / DeFi (~3 KB) | ~0.006 APRO |
| NFT trade (~4 KB) | ~0.008 APRO |
| Game action (~4 KB) | ~0.008 APRO |

The burn is consensus-enforced — not a governance parameter, not toggleable.  
**Starting at launch: −9.87 % supply reduction by 2031** from ordinary network usage alone.

---

### 3 Price Growth Scenarios

*Starting price $0.001 · Supply 10 B APRO · 5-year horizon (2026–2031)*

| Scenario | Driver | Price target | Change |
|----------|--------|-------------|--------|
| **A · Conservative** — deflation only, demand stable | Supply shrinks 9.87 %; market cap stays at $10 M. Pure math, no extra demand needed. | $0.001 → **$0.00111** | **+10.9 %** |
| **B · Realistic Web3** — deflation + organic gaming demand | Real network usage (transfers, DeFi, NFTs) + 20–30 games. Supply −10 % meets demand ×10–15 (normal for any active L1). Market cap grows to $100 M. | $0.001 → **$0.011** | **+1,100 %** |
| **C · Maximum** — Global GameFi hub, year 25 | 3–4 B tokens burned over 25 years. Aperod reaches top-tier L1 network status. Market cap $1–2 B. | $0.001 → **$0.15 – $0.30** | **+15,000 % – +30,000 %** |

---

### Your APRO Validator Reward Grows in USD as Price Rises

Validators earn **≈ 2,503,000 APRO / year** (round-robin across 21 active validators).  
As deflation drives the price up, that fixed reward becomes worth exponentially more in USD:

| APRO price | Annual validator income (USD) |
|-----------|------------------------------|
| $0.001 (launch) | **$2,503 / year** |
| $0.0011 (Scenario A, 2031) | **$2,753 / year** |
| $0.011 (Scenario B, 2031) | **$27,533 / year** |
| $0.15 (Scenario C, low) | **$375,450 / year** |
| $0.30 (Scenario C, high) | **$750,900 / year** |

Run the node, earn APRO. Let deflation do the rest.

> **Stake requirement:** ≥ 100,000 APRO &nbsp;·&nbsp; **Reward destination:** your Telegram wallet &nbsp;·&nbsp; **No approval needed**

---

## 🏗 Architecture

```
aperod-node/
├── cmd/
│   ├── node/        — aperod-node binary  (full node + RPC)
│   └── cli/         — aperod binary       (wallet CLI + chain inspection)
├── consensus/       — PoA engine: round-robin proposer selection, BFT 2/3 finality votes
├── core/            — Block, Transaction, UTXO, Mempool, Chain, Merkle root
├── crypto/          — Ed25519, SHA3-256/512, RingCT, Pedersen commitments, Bulletproofs
├── p2p/             — Peer discovery, block/tx propagation, DNS bootnode resolution
├── store/           — LevelDB with typed key prefixes; archive + light pruning modes
├── wallet/          — HD wallet (BIP-39 + SLIP-0010 + Ed25519), stealth address builder
├── config/          — node.yaml schema, genesis configuration
└── deploy/          — install scripts, Dockerfile, monitoring stack
```

**Cryptographic primitives:**

| Primitive | Library / Standard |
|-----------|-------------------|
| Elliptic curve | Ed25519 — `filippo.io/edwards25519` |
| Hash function | SHA3-256 / SHA3-512 — `golang.org/x/crypto` |
| Ring signatures | MLSAG, ring size 16 |
| Commitments | Pedersen over Ed25519 |
| Range proofs | Bulletproofs (IPA variant) |
| HD key derivation | BIP-39 mnemonics + SLIP-0010 + Ed25519 |
| Address format | Base58Check — dual-key (spend key + view key) |

---

## 🔨 Building from Source

### Prerequisites

- **Go 1.25+** — [install](https://go.dev/doc/install)
- `make`, `git`

### Clone & build

```bash
git clone https://github.com/aperod-network/aperod-node.git
cd aperod-node
make build
```

Outputs:

| Binary | Path | Description |
|--------|------|-------------|
| `aperod-node` | `build/aperod-node` | Full node process |
| `aperod` | `build/aperod` | CLI wallet & chain inspector |

### Common commands

```bash
# Start a full node
./build/aperod-node --config config/testnet.yaml

# Generate a new wallet (mnemonic + addresses)
./build/aperod wallet create

# Check wallet balance
./build/aperod wallet balance <address>

# Generate a validator consensus key
./build/aperod validator keygen --out ./keys/validator.key

# Inspect a block
./build/aperod chain block <height>
```

### Updating a running node (production)

**Use `update-node.sh` — do not build and copy manually.**

```bash
sudo bash /opt/aperod/blockchain/deploy/update-node.sh
```

The script stops the service, builds the binary, installs it to `/usr/local/bin/aperod-node` (the path the `systemd` service runs), starts the service, and waits for the API to respond. Telegram alerts are sent on build or startup failure.

> **Why not `make build` + `cp` directly?**
> Copying over a running binary fails with `Text file busy`. Building to any path other than `/usr/local/bin/aperod-node` is silently ignored by the service. The update script prevents both mistakes.

### Run tests

```bash
make test                    # full test suite with race detector
make test-cover              # with HTML coverage report
```

---

## ⚙ Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| CPU | 2 cores | 4 cores |
| RAM | 4 GB | 8 GB |
| Disk | 50 GB SSD | 200 GB NVMe |
| Network | 10 Mbps | 100 Mbps |
| OS | Ubuntu 22.04 | Ubuntu 24.04 LTS |
| Open ports | 30303 / tcp + udp | — |
| RPC | 8545 / tcp (localhost only) | — |

---

## 🔍 Diagnosing CPU / Memory Spikes (pprof)

The node ships a built-in Go [pprof](https://pkg.go.dev/net/http/pprof) endpoint that lets you capture CPU flame graphs, heap snapshots, and goroutine dumps in seconds — no rebuild required.

### Enable

In `node.yaml` (or the production overlay), set:

```yaml
pprof:
  enabled: true
  listen_addr: "127.0.0.1:8546"   # loopback only — never expose publicly
```

Restart the node.  You will see:

```
{"level":"INFO","msg":"pprof endpoint started","addr":"127.0.0.1:8546","hint":"go tool pprof http://127.0.0.1:8546/debug/pprof/profile?seconds=30"}
```

### Capture profiles (from the server)

```bash
# 30-second CPU flame graph
go tool pprof http://127.0.0.1:8546/debug/pprof/profile?seconds=30

# Heap snapshot
go tool pprof http://127.0.0.1:8546/debug/pprof/heap

# Goroutine dump (text, great for deadlock diagnosis)
curl -s "http://127.0.0.1:8546/debug/pprof/goroutine?debug=2"

# Allocs, mutex, block profiles
go tool pprof http://127.0.0.1:8546/debug/pprof/allocs
go tool pprof http://127.0.0.1:8546/debug/pprof/mutex
go tool pprof http://127.0.0.1:8546/debug/pprof/block
```

### Via SSH tunnel (remote server)

```bash
# On your laptop — forward remote 8546 to local 8546
ssh -L 8546:127.0.0.1:8546 user@your-server

# Then locally
go tool pprof http://127.0.0.1:8546/debug/pprof/profile?seconds=30
```

### Security notes

- `listen_addr` **must** be `127.0.0.1:…` (loopback).  Never bind to `0.0.0.0`.
- Disable (`enabled: false`) when not actively diagnosing — pprof exposes internal runtime metrics.
- The endpoint runs on a **separate port** (default 8546) and is completely isolated from the public API (port 8545).

---

## 🔒 Security

- **Consensus key ≠ wallet key** — the key that signs blocks has no ability to move funds
- **RPC (port 8545) binds to `127.0.0.1` by default** — never expose externally without a firewall
- **pprof (port 8546) disabled by default** — enable only for active diagnosis, loopback only
- **Consensus key stored with `chmod 640`** — readable only by the `aperod` system user
- **Double-sign protection** — automatic on-chain slashing (10 % of stake, permanent ban from validator set)
- **View key sharing** — share your view key for read-only auditing without granting spending ability

To report a vulnerability, see [SECURITY.md](SECURITY.md).

---

## 📄 License

Copyright 2024 [aperod-network](https://github.com/aperod-network)

Licensed under the **Apache License, Version 2.0** — see [LICENSE](LICENSE) for the full text.

---

## 👥 Contributors

<table>
  <tr>
    <td align="center">
      <a href="https://github.com/aperod-network">
        <img src="https://github.com/aperod-network.png" width="80" alt="aperod-network" style="border-radius:50%"/><br />
        <b>aperod-network</b>
      </a>
    </td>
  </tr>
</table>

---

<div align="center">
  <sub>
    <a href="https://aperod.com">aperod.com</a> &nbsp;·&nbsp;
    <a href="https://t.me/aperod_bot">Telegram Wallet</a> &nbsp;·&nbsp;
    <a href="https://aperod.com/explorer/">Block Explorer</a>
  </sub>
</div>
