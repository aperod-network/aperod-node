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
| Consensus manipulation | PoA double-sign, fork, finality break |
| Cryptographic flaws | RingCT ring sigs, Bulletproofs range proofs, HD key derivation |
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

### Privacy Layer — RingCT + Bulletproofs

- **Transaction unlinkability**: one-time stealth addresses (per-height mint derivation: `mint_pub = spend_pub + height·G`)
- **Amount confidentiality**: Pedersen commitments with Bulletproof range proofs
- **Ring signatures**: linkable ring signatures hide sender among ring members
- **Blind balancing**: transaction builder enforces `Σin_blinds = Σout_blinds + fee_blind`; unbalanced transactions are rejected at consensus

### Key Management

- **HD derivation**: BIP-39 + SLIP-0010 + Ed25519 (matches Go node exactly)
- **Address checksum**: double-SHA-256 (not SHA-3 despite internal naming — tested against Go reference)
- **Consensus key ≠ spend key**: compromise of one does not affect the other
- **Validator key permissions**: file must be `chmod 640`, owned by `aperod` user; node refuses to start on wrong permissions

### Halving Schedule

- Block reward: **5 APRO** at genesis
- Halving interval: **21,024,000 blocks** (≈ 2 years at 3 s/block)
- Era boundaries checked in `consensus/poa.go`; tested to ensure finalization does not break at halving height

---

## Network Security

### P2P Layer

- **MaxPeers enforced** on both outbound and inbound connections
- **DNS bootnode resolution** on every reconnect cycle (not cached at startup)
- **Message deadline**: each `ReadMsg` has a 30-second internal timeout; connections that stall are closed via goroutine + `conn.Close()`
- **Peer map key**: always `IP:port`, never hostname — prevents DNS-rebinding confusion

### RPC / REST API (`/api/v1/*`)

- **EIP-1559 fee model**: base fee (200 nAPRO/byte) burned 100%; priority tip to validator. Dynamic ±12.5%/block adjustment
- **Rate limiting**: per-IP request limits on all public endpoints (60 req/min default; stricter on heavy endpoints)
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
- [ ] Consensus key: `/etc/aperod/validator.key`, `chmod 640`, owned by `aperod`
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
