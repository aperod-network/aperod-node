# Aperod Node (APRO)

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

## Becoming a Validator

Aperod uses **stake-based validator selection** — the top 21 nodes by staked APRO are active validators.

### ⚠️ Before installing the node — get your wallet

All block rewards go directly to your **Aperod Telegram wallet**.  
You must have an APRO address before installing the node.

**Step 1 — Create your wallet:**
1. Open the bot: **https://t.me/aperod_bot**
2. Tap **"Create wallet"**
3. Copy your **APRO address** (~95 characters)

You'll use this address during node installation. Rewards will arrive in your Telegram wallet and you'll receive a **Telegram notification** for each payment.

---

### Installation steps

**Step 2 — Install the validator node:**
```bash
sudo bash install-validator.sh
```

The script will:
- Ask for your APRO address (from Telegram wallet above)
- Generate a **consensus key** for block signing (separate from your wallet)
- Configure the node with your reward address
- Start the node as a systemd service

Non-interactive install (CI/server):
```bash
APEROD_REWARD_ADDRESS=<your-apro-address> sudo bash install-validator.sh
```

**Step 3 — Register your node:**

After installation the script prints the registration command. Run it:
```bash
curl -s -X POST https://aperod.com/api/validators/apply \
  -H 'Content-Type: application/json' \
  -d '{
    "pubKey":   "<your-consensus-pubkey>",
    "alias":    "my-validator",
    "endpoint": "/ip4/<your-ip>/tcp/30303",
    "address":  "<your-apro-address>"
  }'
```

**Step 4 — Send the stake:**

Transfer at least **100,000 APRO** to your wallet address from your Telegram wallet.  
The node will activate automatically in the next epoch (~100 blocks, ≈1.7 min).

**Step 5 — Done!**

- Rewards accumulate in your Telegram wallet
- You receive a Telegram notification for every block reward
- Check balance anytime in the bot

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

## How rewards work

| Action | Where it goes |
|--------|--------------|
| Block reward | Your Telegram wallet (APRO address in config) |
| Telegram notification | Sent to you on every reward |
| Stake lock | 100,000 APRO locked while you validate |
| Withdrawal | Any time from Telegram wallet |
| Slashing (double-sign) | 10% of stake deducted |

---

## Key Commands

```bash
# Node management
systemctl status aperod-node          # node status
journalctl -u aperod-node -f          # live logs
systemctl restart aperod-node         # restart

# Check reward address in config
grep reward_address /etc/aperod/node.yaml

# Update reward address (then restart node)
# Edit /etc/aperod/node.yaml → reward_address: <new-address>
systemctl restart aperod-node
```

---

## Validator Parameters

| Parameter | Value |
|-----------|-------|
| Minimum stake | 100,000 APRO |
| Maximum active validators | 21 |
| Epoch length | 100 blocks (~1.7 min) |
| Withdrawal lock | 7,200 blocks (~2 hours) |
| Slashing (double-sign) | 10% of stake |

---

## Architecture

```
aperod-node/
├── crypto/      — Ed25519, SHA3, RingCT, Pedersen commitments, Bulletproofs
├── core/        — Block, Transaction, UTXO, Mempool, Chain
├── consensus/   — PoA engine (round-robin proposer + BFT 2/3 threshold)
├── store/       — LevelDB storage
├── p2p/         — Peer-to-peer networking
├── cmd/node/    — aperod-node binary
├── cmd/cli/     — aperod CLI
├── config/      — testnet.yaml, genesis-testnet.yaml
└── deploy/      — install-node.sh, install-validator.sh
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
| Privacy | MLSAG Ring Signatures (ring size 16) |

---

## Security

- Block rewards go to your Telegram wallet — consensus key has no spending ability
- RPC port 8545 binds to localhost only — never expose externally
- Validator consensus key stored with `chmod 640` permissions
- Double-signing protection with automatic slashing (10% of stake)
- View key can be shared without spending ability (Monero-style dual-key)

---

## Uninstall

To completely remove the validator node from a server:

```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/uninstall-validator.sh | sudo bash
```

Or if you already have the file locally:
```bash
sudo bash uninstall-validator.sh
```

The script stops and removes:
- `aperod-node` systemd service
- Binaries (`/usr/local/bin/aperod-node`, `/usr/local/bin/aperod`)
- Config and keys (`/etc/aperod/`)
- Blockchain data (`/var/lib/aperod/`)
- Source files (`/opt/aperod/`)
- System user `aperod`
- ufw rules for port 30303

> Your **Telegram wallet** (`t.me/aperod_bot`) and APRO balance are **not affected** — they live on the blockchain, not on your server.

Non-interactive (automated):
```bash
APEROD_UNINSTALL_CONFIRM=YES sudo bash uninstall-validator.sh
```

---

## License

Apache 2.0 — see [LICENSE](LICENSE)
