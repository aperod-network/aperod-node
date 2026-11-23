# Mainnet validator onboarding

This document describes the requirements for joining the existing Aperod
Mainnet. It does not claim that a new validator can currently be installed or
activated from these instructions.

## Current status

There is no verified public release that completes a clean installation,
syncs a new node to the canonical chain, locks stake on-chain, and proves
active-set admission. The existing `install-validator.sh` is not a Mainnet
installer. Do not bypass its network guard, run an unpinned script as root, or
send APRO based on an API application or a wallet balance.

The canonical chain anchors and the read-only public API check are in
[mainnet onboarding](MAINNET.md). The checkpoint is signed under the selected
`operator_1_of_1` trust model; that is one operator signature, not a
multi-validator quorum or proof of current validator participation.

## Setup guide

Use [MAINNET.md](MAINNET.md) for the canonical genesis, checkpoint, and
read-only endpoint checks. A verified self-service mainnet bootstrap release
is not yet published. The existing `install-validator.sh` must not be used as
a mainnet installer; do not bypass its network guard or substitute another
chain's configuration.

## Protocol parameters

These values are protocol parameters in the reviewed source. They are not a
promise that a new validator can currently register or become active.

| Parameter | Mainnet value |
|---|---|
| Active validators | **21** maximum; validator capacity, not token supply |
| Minimum stake | ≥ **100,000 APRO** locked on-chain |
| Epoch length | **100 blocks** (~300 seconds at 3 seconds per block) |
| Churn limit | **3** new validators per epoch (prevents instability) |
| 2. Unbonding period | **144,000 blocks** (~5 days) — node leaves active set |
| Stake penalty | **10 % of total stake** burned immediately |
| Pool reward | **300,000,000 nAPRO (3 APRO)** per block, from the pre-allocated pool |
| Tail emission | **1 APRO** per block after the pool is exhausted; no halving |

## What a verified release must establish

Before a new validator procedure can be published, the same reviewed release
must bind and test all of the following:

1. Canonical genesis, authenticated checkpoint, registry and compatible
   startup snapshot.
2. Exact source revision, executable, platform, service configuration and
   checksums.
3. The supported P2P bootstrap and peer-admission procedure.
4. A consensus-wide activation profile for the supported stake-deposit format.
5. A fresh-server rehearsal showing the node joins the existing chain without
   creating another genesis and restores its wallet/UTXO state after restart.
6. An owner-authorized stake deposit confirmed on-chain.
7. Active-set inclusion at the expected epoch and a vote or block accepted by
   the existing network.

These outcomes are independent. A signed checkpoint does not validate an
installer; a synchronized non-validator does not prove stake eligibility; an
off-chain application is not on-chain registration; and a local vote test
does not prove that Mainnet accepted it.

## Operator safety

- Never share, copy, or reuse another validator's consensus private key.
- Do not pick or change a stake activation height on an individual node; it is
  a consensus parameter and must match the network's reviewed release.
- Do not delete an existing chain or wallet directory to retry an install.
- Preserve recovery evidence and keep test artifacts separate from the
  validator's runtime data.
- Do not treat a balance, application response, or advertised minimum stake as
  proof of locked stake or active-set membership.
- Validator capacity is 21 active validators; this is unrelated to the APRO
  supply, which is 10 billion APRO at genesis.

## Penalties and rewards

Penalty and reward rules in source code or a protocol proposal are not proof
that the live Mainnet enforces them. In particular, no downtime-penalty debit
or slashing outcome should be represented as production behavior without a
confirmed canonical transaction and a verified post-state.

Once a reviewed Mainnet release and end-to-end rehearsal exist, this page can
be replaced with its exact install, synchronization, stake, and verification
commands. Until then, this repository has no supported command that makes an
arbitrary server a Mainnet validator.
