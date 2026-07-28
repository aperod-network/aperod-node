<div align="center">

# APEROD · APRO

**Privacy-preserving blockchain built for the real world.**

RingCT confidential transactions &nbsp;·&nbsp; Telegram-native wallet &nbsp;·&nbsp; 100 % fee burn &nbsp;·&nbsp; Permissionless PoA consensus

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)](https://go.dev/doc/go1.25)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Network](https://img.shields.io/badge/Network-Mainnet-brightgreen)](https://aperod.com)
[![Explorer](https://img.shields.io/badge/Explorer-aperod.com-informational)](https://aperod.com/explorer/)
[![Telegram](https://img.shields.io/badge/Wallet-@aperod__bot-2CA5E0?logo=telegram&logoColor=white)](https://t.me/aperod_bot)

**[⬇ Install Node](#-install-a-full-node) &nbsp;·&nbsp; [🛡 Become Validator](#-become-a-validator) &nbsp;·&nbsp; [🌐 Explorer](https://aperod.com/explorer/) &nbsp;·&nbsp; [💬 Telegram Wallet](https://t.me/aperod_bot)**

</div>

---

## Table of Contents

- [Why Aperod](#-why-aperod)
- [Install a Full Node](#-install-a-full-node)
- [Become a Validator](#-become-a-validator)
- [Validator Rules](#-validator-rules)
- [Tokenomics & Fee Burn](#-tokenomics--fee-burn)
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
| **Block rewards** | 0.1 APRO per block paid directly to the validator's Telegram wallet + push notification |
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
Total supply cap:     100,000,000 APRO
Circulating (launch): 21,000,000  APRO
Block reward:         0.1 APRO per block
Halving interval:     every 2,100,000 blocks (~2 years)
Transaction fee:      0.5 APRO flat
Fee destination:      🔥 burned (removed from supply forever)
```

Validators earn **block rewards only**. Zero fees reach validator wallets.  
The burn is enforced at the consensus layer — not a governance parameter, not toggleable.

See [**BURN\_POLICY.md**](BURN_POLICY.md) for full tokenomics and deflationary mechanics.

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

## 🔒 Security

- **Consensus key ≠ wallet key** — the key that signs blocks has no ability to move funds
- **RPC (port 8545) binds to `127.0.0.1` by default** — never expose externally without a firewall
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
