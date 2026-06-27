# Aperod Node (APR)

> Privacy-focused blockchain with RingCT transactions, stake-based validator selection, and game integration.

## Quick Install

### Full Node (non-validator)
```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-node.sh | sudo bash
```

### Validator Node
```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash
```

Supported: **Ubuntu 22.04 / 24.04 / Debian 12** (x86_64 and ARM64)

---

## Requirements

| Component | Minimum | Recommended |
|-----------|---------|-------------|
| CPU | 2 cores | 4 cores |
| RAM | 4 GB | 8 GB |
| Disk | 50 GB SSD | 200 GB NVMe |
| Network | 10 Mbps | 100 Mbps |
| OS | Ubuntu 22.04 | Ubuntu 24.04 LTS |
| Ports | 30303/tcp+udp | — |

---

## Becoming a Validator

Aperod uses **permissionless stake-based validator selection** — no admin approval needed.  
The top **21 nodes by staked APR** are automatically selected as active validators.

### Steps

**1. Install and sync the node** (run `install-validator.sh` above)

**2. Get at least 100,000 APR** — minimum stake requirement

**3. Send a stake deposit transaction:**
```bash
aperod validator stake \
  --key /etc/aperod/validator.key \
  --amount 100000 \
  --node http://127.0.0.1:8545
```

**4. Wait for the next epoch** (~100 blocks, ≈1.7 minutes)  
Your node will be activated automatically if it's in the top 21 by stake.

**5. Check your status:**
```bash
aperod validator status \
  --pubkey <your-hex-pubkey> \
  --node http://127.0.0.1:8545
```

### Validator Parameters

| Parameter | Value |
|-----------|-------|
| Minimum stake | 100,000 APR |
| Maximum active validators | 21 |
| Epoch length | 100 blocks (~1.7 min) |
| Withdrawal lock | 7,200 blocks (~2 hours) |
| Slashing (double-sign) | 10% of stake |

---

## Manual Build

```bash
# Requirements: Go 1.22+
git clone https://github.com/aperod-network/aperod-node.git
cd aperod-node
make deps
make build

./build/aperod-node --help
./build/aperod wallet create
```

---

## Key Commands

```bash
# Node management
systemctl status aperod-node          # node status
journalctl -u aperod-node -f          # live logs
systemctl restart aperod-node         # restart

# Wallet
aperod wallet create                  # create new wallet
aperod wallet balance                 # check balance
aperod wallet send --to <addr> --amount <n>  # send APR

# Validator
aperod validator keygen --out ./validator.key   # generate key
aperod validator stake --key ./validator.key --amount 100000
aperod validator status --pubkey <hex>
aperod validator unstake --key ./validator.key
```

---

## Network

| Parameter | Value |
|-----------|-------|
| Network | Aperod Testnet |
| Chain ID | `aperod-testnet-1` |
| Consensus | Proof of Authority (stake-weighted) |
| Block time | 1 second |
| P2P port | 30303 |
| RPC port | 8545 (localhost only) |
| Privacy | MLSAG Ring Signatures (ring size 11) |

---

## Exchange / DEX Integration

REST API for exchanges, OTC desks, and swap services:

```
GET  /api/v1/chain/info
GET  /api/v1/chain/blocks
GET  /api/v1/wallet/balance/:address
POST /api/v1/wallet/send
GET  /api/v1/validators
```

Full API documentation: https://aperod.com/exchange/docs

---

## Architecture

```
aperod-node/
├── crypto/      — Ed25519, SHA3, RingCT, Pedersen commitments, Bulletproofs
├── core/        — Block, Transaction, UTXO, Mempool, Chain, Staking
├── consensus/   — PoA engine (round-robin proposer + BFT 2/3 threshold)
├── store/       — LevelDB storage
├── p2p/         — Peer-to-peer networking
├── cmd/node/    — aperod-node binary
├── cmd/cli/     — aperod CLI (wallet + chain inspection)
├── config/      — testnet.yaml, genesis-testnet.yaml
└── deploy/      — install-node.sh, install-validator.sh
```

---

## Security

- View key can be shared without spending ability (Monero-style dual-key)
- RPC port 8545 binds to localhost only — never expose externally
- Validator key stored with `chmod 600` permissions
- Double-signing protection with automatic slashing

---

## License

Apache 2.0 — see [LICENSE](LICENSE)
