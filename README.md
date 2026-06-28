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

  Aperod selects the top **21 nodes by staked APR** as active validators.
  Block rewards go directly to your **Telegram wallet** — no admin approval needed.

  ### ⚠️ Before installing — get your APR wallet address

  All block rewards go to your Aperod Telegram wallet. Create one first:

  1. Open **https://t.me/aperod_bot**
  2. Tap **"Create wallet"**
  3. Copy your **APR address** (~95 characters)

  You'll enter this address during node installation. Every block reward triggers a Telegram notification.

  ### Steps

  **1. Install the validator node** (the script will ask for your APR address):
  ```bash
  curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-validator.sh | sudo bash
  ```

  Non-interactive (CI/automated):
  ```bash
  APEROD_REWARD_ADDRESS=<your-apr-address> sudo bash install-validator.sh
  ```

  **2. Register your node** (the installer prints the exact command at the end):
  ```bash
  curl -s -X POST https://aperod.com/api/validators/apply \
    -H 'Content-Type: application/json' \
    -d '{
      "pubKey":   "<consensus-pubkey-from-installer>",
      "alias":    "my-validator",
      "endpoint": "/ip4/<your-ip>/tcp/30303",
      "address":  "<your-apr-address>"
    }'
  ```

  **3. Send the minimum stake** — transfer at least **100,000 APR** to your wallet address.
  Your node activates automatically at the next epoch (~100 blocks, ≈1.7 min).

  ### Validator Parameters

  | Parameter | Value |
  |-----------|-------|
  | Minimum stake | 100,000 APR |
  | Maximum active validators | 21 |
  | Epoch length | 100 blocks (~1.7 min) |
  | Withdrawal lock | 7,200 blocks (~2 hours) |
  | Slashing (double-sign) | 10% of stake |

---


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

## Uninstall

  To completely remove the validator node from a server:

  ```bash
  curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/uninstall-validator.sh | sudo bash
  ```

  The script stops and removes: systemd service, binaries, config and keys (`/etc/aperod/`), blockchain data (`/var/lib/aperod/`), source files, system user `aperod`, and ufw rules for port 30303.

  > Your **Telegram wallet** and APR balance are **not affected** — they live on the blockchain, not on your server.

  Non-interactive:
  ```bash
  APEROD_UNINSTALL_CONFIRM=YES sudo bash uninstall-validator.sh
  ```

---

## License

Apache 2.0 — see [LICENSE](LICENSE)
