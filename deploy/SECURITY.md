# Security Policy

## Supported Versions

| Version | Supported |
|---------|-----------|
| Latest `main` | ✅ Active |
| Older tags | ❌ No patches |

Always run the latest release. Use the install script to stay current:

```bash
curl -fsSL https://raw.githubusercontent.com/aperod-network/aperod-node/main/deploy/install-node.sh | sudo bash
```

---

## Reporting a Vulnerability

**Please do not open a public GitHub issue for security vulnerabilities.**

Report security issues privately via Telegram: **[@sup_apro_bot](https://t.me/sup_apro_bot)** — describe the vulnerability in plain text.

Include:
- A description of the vulnerability and its impact
- Steps to reproduce (or a proof-of-concept)
- Affected component (`crypto/`, `consensus/`, `p2p/`, etc.)

You will receive a response within **48 hours**. Critical issues are patched within **7 days**.

---

## Bug Bounty

| Severity | Reward |
|----------|--------|
| **Critical** (consensus break, key extraction, double-spend, network halt) | **$1,000,000** |
| **High** (remote code execution, eclipse attack, auth bypass) | up to **$100,000** |
| **Medium** (data integrity, privacy leaks, DoS) | up to **$100,000** |
| **Low / Informational** | up to **$100,000** |

Rewards are paid in APRO at the 7-day average market price on date of disclosure acceptance.  
Duplicate reports and reports for issues already known to the team are not eligible.

---

## Threat Model

| Scope | In scope |
|-------|----------|
| Consensus manipulation | ✅ |
| Cryptographic flaws (ring sigs, Bulletproofs, HD derivation) | ✅ |
| Double-spend / UTXO attacks | ✅ |
| P2P eclipse / Sybil attacks | ✅ |
| RPC endpoint vulnerabilities | ✅ |
| Install script supply-chain issues | ✅ |
| Wallet key generation weaknesses | ✅ |

| Scope | Out of scope |
|-------|-------------|
| Telegram bot infrastructure | ❌ |
| Third-party Go dependencies | ❌ (report upstream) |
| Validator operator server hygiene | ❌ |

---

## Security Hardening Checklist (Validators)

- [ ] **Firewall**: only port 30303/tcp+udp open to the internet; 8545 blocked externally
- [ ] **Consensus key**: stored at `/etc/aperod/validator.key`, `chmod 640`, owned by `aperod` user
- [ ] **No duplicate processes**: never run two instances with the same consensus key — triggers double-sign slashing
- [ ] **Updates**: subscribe to GitHub releases for security patches
- [ ] **Separate keys**: consensus key ≠ wallet spend key; compromise of one does not affect the other

---

*[aperod-network](https://github.com/aperod-network) — [aperod.com](https://aperod.com)*
