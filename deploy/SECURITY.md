# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest `main` | ✅ Active |
| Older tags | ❌ No patches |

Always run the latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-node.sh | sudo bash
```

---

## Reporting a Vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Report privately via Telegram: **[@sup_apro_bot](https://t.me/sup_apro_bot)**

Include:
- Description and impact
- Steps to reproduce (or proof-of-concept)
- Affected component (`core/`, `consensus/`, `p2p/`, etc.)

Researchers repeating APD-2026 findings should first follow the
[`APD Remediation Verification Guide`](../SECURITY-RESEARCHER-GUIDE.md). It
identifies the current remediation baseline, coordinated activation behavior, and
the exact regression suites for each finding.

Response within **48 hours**. Critical issues patched within **7 days**.

---

## Bug Bounty

| Severity | Reward |
|----------|--------|
| **Critical** (consensus break, key extraction, double-spend, network halt) | up to **$5,000** |
| **High** (RCE, eclipse attack, auth bypass) | up to **$2,000** |
| **Medium** (data integrity, privacy leaks, DoS) | up to **$500** |
| **Low / Informational** | up to **$100** |

Rewards paid in APRO at the 7-day average market price on disclosure acceptance date.  
Duplicates and already-known issues are not eligible.  
All reward amounts are at the sole discretion of the Aperod team and subject to change without prior notice.

---

## Threat Model

### In Scope

| Area | Notes |
|------|-------|
| Consensus manipulation | BFT-PoS double-sign, fork, finality break |
| Cryptographic flaws | RingCT/CLSAG signatures, Bulletproof range proofs, HD key derivation |
| Double-spend / UTXO attacks | Replay, blind manipulation, output reuse |
| P2P eclipse / Sybil attacks | Peer routing, DoS, partition |
| RPC endpoint vulnerabilities | `/api/v1/*` public endpoints |
| Install script supply-chain | Installer integrity |
| Wallet key generation | Entropy, derivation path |

### Out of Scope

| Area |
|------|
| Third-party Go dependency CVEs (report upstream) |
| Validator server operating system hygiene |
| Social engineering |

---

## Cryptographic Architecture

### Privacy Layer — RingCT + CLSAG v5 + Bulletproofs

- **Transaction unlinkability**: one-time stealth addresses (per-height mint derivation: `mint_pub = spend_pub + height·G`)
- **Amount confidentiality**: Pedersen commitments with Bulletproof range proofs
- **Ring signatures**: CLSAG v5 proves ownership within a 16-member ring while binding every public key to its commitment and pseudo-output; v5 is active on the live network from coordinated activation block 1,769,500
- **Historical compatibility**: legacy v1–v4 transactions remain replayable; activation does not rewrite chain history
- **Spentness boundary**: public nodes cannot identify the real CLSAG ring member; wallets determine owned-output spentness from owner-derived key images
- **Blind balancing**: transaction builder enforces `Σin_blinds = Σout_blinds + fee_blind`; unbalanced transactions are rejected at consensus

### Key Management

- **HD derivation**: BIP-39 + SLIP-0010 + Ed25519 (matches Go node exactly)
- **Address checksum**: double-SHA-256 (not SHA-3 despite internal naming — tested against Go reference)
- **Consensus key ≠ spend key**: compromise of one does not affect the other
- **Validator key permissions**: file must be `chmod 600`, owned by `aperod` user; node refuses to start on unsafe permissions

### Validator Rewards

- Pool phase: **3 APRO per block** from the pre-allocated **2B APRO** validator pool
- Pool rewards redistribute genesis supply; they do not create new supply
- After pool exhaustion (~63 years at a 3-second block target): **1 APRO per block** tail emission
- There is **no halving**
- Peers derive the expected reward independently and reject incorrectly valued coinbase transactions

---

## Network Security

### P2P Layer

- **MaxPeers enforced** on both outbound and inbound connections
- **DNS bootnode resolution** on every reconnect cycle (not cached at startup)
- **Message deadline**: each `ReadMsg` has a 30-second internal timeout; connections that stall are closed via goroutine + `conn.Close()`
- **Peer map key**: always `IP:port`, never hostname — prevents DNS-rebinding confusion

### RPC / REST API (`/api/v1/*`)

- **EIP-1559 fee model**: every protocol base fee is burned 100%; an explicit priority tip may compensate the proposer. Dynamic ±12.5%/block base-fee adjustment
- **Rate limiting**: per-IP token-bucket limits — general 30-burst/1-per-sec; heavy endpoints (`/v1/blocks`, `/v1/network/stats`, `/v1/transactions`, `/v1/oracle`, `/v1/tokenomics`) capped at 10 req/min
- **DDoS auto-ban**: any IP triggering the rate limiter 5+ times within 60 seconds is **permanently** blocked with no expiry. Bans persist across restarts (stored in DB). Applies to both heavy and general flood paths. Admin Telegram notification sent on every auto-ban.
- **Input validation**: all user-supplied values validated and sanitised before DB writes or file operations
- **File path safety**: all server-side file operations use `safeResolvePath()` — no user-controlled input can escape the designated directory

### Admin Interface

- **Non-standard path**: admin UI is not served at any predictable or enumerable URL
- **IP allowlist**: `/api/admin/*` routes reject requests outside the configured CIDR before session checks
- **Brute-force lockout**: configurable threshold (default 5 attempts); auto-IP-ban at hard limit; Telegram alert on each lockout
- **TOTP mandatory**: all admin accounts require TOTP; setup enforced at first login
- **JWT rotation**: short-lived access tokens (15 min) + refresh tokens; jti revocation on logout/ban
- **All paths validated**: file reads/writes inside admin routes use `safeResolvePath()` — path traversal impossible
- **Security headers**: `helmet`, HSTS (2 years), `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`

---

## Pruning & Data Integrity

- **Archive mode**: full chain stored; all blocks verifiable
- **Light mode**: `PruneBlocksOlderThan()` with cursor in metadata; pruned blocks return validator + merkle root sentinel instead of blank fields
- **Merkle empty sentinel**: `MerkleRoot(nil) = SHA3-256("aperod/merkle-empty/v1")` — all-zeros always means uninitialised, never empty block

---

## Security Hardening Checklist (Validators)

- [ ] Firewall: only `30303/tcp+udp` open externally; `8545` blocked
- [ ] Consensus key: `/etc/aperod/validator.key`, `chmod 600`, owned by `aperod`
- [ ] No duplicate processes: two instances with the same key trigger double-sign
- [ ] Separate keys: consensus key ≠ wallet spend key
- [ ] Updates: subscribe to GitHub releases for security patches
- [ ] Data directory: `/opt/aperod/data` owned by `aperod:aperod`

---

## Dependency & Static Analysis

The codebase is continuously checked by:

- **`gosec`** — Go static analysis for security anti-patterns
- **SAST** — JavaScript/TypeScript static analysis on all API routes
- **Dependency audit** — npm/pnpm CVE scanning on every CI run
- **HoundDog** — dataflow analysis for secrets and PII exposure

Current status: **0 critical / 0 high findings** across all scanners.

---

*[aperod-network](https://github.com/aperod-network) — [aperod.com](https://aperod.com)*
