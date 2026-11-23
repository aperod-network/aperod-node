# Mainnet validator onboarding

## Current status

The existing Mainnet genesis and a checkpoint at height `4,076,315` have been
inspected on a closed copy on the new OVH server. Their bootstrap and AVM
signatures verify under the selected single-operator trust policy. That policy
is not a validator quorum, and a valid checkpoint signature is not proof that a
new node has joined the network.

On 2026-11-23, an isolated non-validator on the new OVH server connected to the
Mainnet relay and applied live blocks through height `4,083,185`. A graceful
shutdown saved a snapshot at `4,083,026`; after restart, the node reloaded that
snapshot, reconnected, and continued accepting blocks. At the recorded ready
check, the local API reported `syncing=false`, `utxo_rebuilding=false`, zero
phantom key images, and zero missing UTXO/store rows. The local API also matched
the signed genesis, height-1 index, and checkpoint anchors; the stale genesis
hash was rejected.

This proves snapshot recovery and outbound P2P sync for a manually staged
non-validator. It is **not** a clean install from the public installer, nor proof
of active-set admission or validator activation. The historical height-index
sweep could not verify `798,755` pruned block bodies, so this is not a complete
archive.

## Setup guide

A verified self-service mainnet bootstrap release is not yet published.
The legacy `install-validator.sh` must not be used as a mainnet installer;
do not bypass its safety checks or run it with a different network flag.

See [mainnet onboarding](MAINNET.md) for the chain anchors, the stale
public Explorer/API records, the operator-attestation boundary, and the OVH
sync/restart evidence. The public `chain/identity-evidence` endpoint still
returns HTTP 503 because its finality evidence does not meet the configured
one-member, one-signature operator rule. The existing public API and Explorer
have not been switched to the OVH node.

The OVH sync binary was built from a modified working tree
(`buildvcs.modified=true`), so it is test evidence, not a release binary. If an
older install attempt reports `[source-safety] REFUSED` for the checkout's
top-level `data/` directory, do not bypass the guard. If it reports
`Directory exists`, inspect the existing checkout and preserve it; see the
diagnosis in [MAINNET.md](MAINNET.md).

## Protocol values in source

These values are implemented in the current source; they are not, by
themselves, proof of the live network's activation schedule or an invitation to
send funds.

| Parameter | Source value |
|---|---:|
| Active validator capacity | 21 |
| Minimum stake | 100,000 APRO |
| Epoch length | 100 blocks |
| Churn limit | 3 new validators per epoch |
| Full unbonding | 144,000 blocks |
| Double-sign slash | 10% |
| Validator reward pool | 300,000,000 nAPRO per block |
| Tail emission | 100,000,000 nAPRO per block |

Validator capacity is unrelated to the 10-billion APRO nominal supply.
Consensus activation heights must come from one reviewed Mainnet release and
must not be selected independently on a new node.

## Requirements for a usable public release

The same reviewed release must include and verify:

1. The canonical genesis profile, operator-signed bootstrap statement, AVM
   checkpoint, migration witnesses, and consistent chain snapshot.
2. The exact compatible source revision, executable, service config, and
   checksums.
3. A verified Mainnet peer identity and an authorized admission procedure.
4. A clean-server install from the published package, sync to the canonical tip,
   and restart check on the new OVH host.
5. An owner-authorized on-chain stake deposit, active-set inclusion, and a
   vote or block accepted by the existing network.

A signed snapshot does not validate a binary or installer. A synchronized
non-validator does not prove stake eligibility. An application record or wallet
balance is not locked stake. Local tests do not prove that Mainnet accepted a
vote.

## Operator safety

- Never share or reuse another validator's consensus private key.
- Do not create a new genesis when a node is missing its verified chain state.
- Do not change consensus activation heights on one node.
- Do not delete an existing chain or wallet directory to retry installation.
- Do not treat an application, balance, or advertised minimum stake as proof
  of active-set membership.
- Test and deploy rehearsals belong on the new OVH server; preserve the primary
  node and remove only temporary rehearsal files after verification.
