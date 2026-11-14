# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest `main` | ✅ Active |
| Older tags | ❌ No patches |

Use only an installer from a signed, immutable release after independently
verifying its source revision and checksum. Do not fetch scripts from mutable
`main` and pipe them to a root shell. No signed installer release is currently
published.

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
| Dependency CVEs | Go and npm/pnpm dependencies, including reachable impact |
| Server hardening | OS updates, SSH, firewall, reverse proxy, and backup-agent configuration |

### Out of Scope

| Area |
|------|
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
- **Rate limiting**: per-IP token buckets — general 100 tokens/second with a 200-token burst; heavy endpoints 2 tokens/second with a 5-token burst. Bucket state is capped at 65,536 IPs per limiter; additional IPs share a bounded overflow bucket.
- **Rate-limit response**: rejected requests receive HTTP 429. Do not assume a rate-limit event creates a permanent IP ban.
- **Input validation**: all user-supplied values validated and sanitised before DB writes or file operations
- **File path safety**: all server-side file operations use `safeResolvePath()` — no user-controlled input can escape the designated directory

### Admin Interface

- **Secret-cookie gate**: the browser gate runs before CORS, body parsing, and password/TOTP routes
- **Independent authentication**: password and TOTP checks remain separate; IP restrictions are secondary and must not replace the cookie gate
- **TOTP mandatory**: admin accounts require TOTP
- **JWT rotation**: short-lived access tokens (15 min) + refresh tokens; jti revocation on logout/ban
- **All paths validated**: file reads/writes inside admin routes use `safeResolvePath()` — path traversal impossible
- **Security headers**: the API middleware sets `X-Content-Type-Options`, `X-Frame-Options`, and `Referrer-Policy`; HTML routes own their document CSP. Verify the deployed proxy and Cloudflare headers together to avoid duplicates.

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

The repository CI runs Go build, vet, tests, and pinned
**`govulncheck v1.9.0`** for reachable Go vulnerabilities. The protected public
publication process also runs a pinned `govulncheck` against the immutable
candidate before upload. Do not treat these checks as a complete security audit.

Replit SAST and HoundDog scans are run on demand, not as CI gates. Security
findings must be checked against a fresh, dated scan before each release; this
policy does not claim that the current tree has zero findings.

---

*[aperod-network](https://github.com/aperod-network) — [aperod.com](https://aperod.com)*
