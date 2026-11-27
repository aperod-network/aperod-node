# Mainnet validator onboarding

## Current status

The live OVH check is recorded below. The following genesis inspection and
sync-only rehearsal are historical observations from 2026-11-23, not the
current validator role.

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
of active-set admission or validator activation. The 2026-11-24 restart measured
the height-index sweep at about 2 minutes 22 seconds, but it still skipped
`798,755` heights with no retained block bodies and did not create a verified
index sentinel. The node completed its registry replay and returned to API-ready
status after about 6 minutes 21 seconds. This is not a complete historical
archive.

## Setup guide

A verified self-service mainnet bootstrap release is not yet published.
The legacy `install-validator.sh` must not be used as a mainnet installer;
do not bypass its safety checks or run it with a different network flag.

See [mainnet onboarding](MAINNET.md) for the chain anchors, the stale
public Explorer/API records, the operator-attestation boundary, and the OVH
sync/restart evidence. A read-only logical snapshot confirmed that the pinned
historical replacement signer `eb9650…` verifies all `17,563` retained blocks
from checkpoint height `4,076,315` through snapshot tip `4,093,877`, while the
headers retain collateral identity `08fd…`. Keep that public key in historical
verification; do not use its private key for future signing. The new key
`056a…` is not yet bound by an approved authority transition.

At the 2026-11-25 06:04 UTC live check, `/api/v1/chain/identity-evidence`
returned HTTP 200 under `operator_1_of_1`. The serving node reported the
canonical-index genesis signature and checkpoint signature at height
`4,105,317` as valid; it explicitly reported finality quorum as unverified.
The genesis header hash differs from the canonical index hash, and the signed
statement names the canonical index hash as its signature target. The statement
signer begins `b4c0…`, distinct from the active validator key `08fd…`; do not
describe the evidence as a validator quorum or silently treat those keys as the
same authority.

At the 2026-11-25 06:07 UTC status check, the local API reported height
`4,120,290`,
`syncing=false`, `utxo_rebuilding=false`, and zero missing store/UTXO rows. It
reported `non_validator=false`; the sole validator registry entry `08fd…` was
active with 1,000,000 APRO stake. After restart, the node finalized block
`4,120,146` and continued producing blocks. One outbound peer reported height
`4,108,808`; the local tip was `4,120,290` (11,482 blocks ahead). The relay and
OVH shared the same block hash at height `4,108,808`, but the next OVH block
(`4,108,809`) has `oracle_price=0` and the relay rejected it with
`oracle price is missing after hardening activation`. This explains the stalled
peer: later blocks cannot extend the relay's tip. The active binaries match and
both config files specify security hardening at height `3,800,000`. The likely
producer-side cause is an asymmetric check in the source: incoming blocks call
the oracle validator, while the self-produced path did not.
Current nonzero prices on later blocks do not repair that ancestor. The OVH
validator was gracefully stopped and restarted for an authorized snapshot
rehearsal; it resumed from height `4,120,145`, became API-ready, and finalized
block `4,120,146`. The relay was not changed, and no consensus settings or
production source were deployed. Peer convergence and independent acceptance
are not established. The public API and Explorer still
expose the stale genesis records described in `MAINNET.md`; this is not a clean
public validator installation.

The OVH journal records an oracle JSON decode failure at `20:19:39 UTC`, then
finalizes block `4,108,809` three seconds later. Source review found
`checkOraclePriceDeviation` only on the peer-received path; self-produced
validation checks transactions and key images without invoking it. The running
binary's source revision could not be independently matched to this checkout,
so this is a likely source-level diagnosis, not a verified release mapping. A
source patch now runs the existing oracle check for self-produced blocks before
state mutation, with a regression test. Targeted tests passed on Go 1.26.11 in
a temporary workspace on the new OVH; the source, toolchain, module cache, and
test files were removed afterward. The patch is not deployed. A consistent
18,847,403,159-byte copy of the live tree passed `--check-snapshot` at tip
`4,120,145` and the snapshot SHA-256 matched. The live node resumed from that
tip, passed startup integrity and snapshot UTXO-count checks, and produced new
blocks; the temporary copy was removed after verification. This validates
snapshot recovery only, not rollback of the rejected ancestor or relay
convergence.

The OVH sync binary was built from a modified working tree
(`buildvcs.modified=true`), so it is test evidence, not a release binary. If an
older install attempt reports `[source-safety] REFUSED` for the checkout's
top-level `data/` directory, do not bypass the guard. If it reports
`Directory exists`, inspect the existing checkout and preserve it; see the
diagnosis in [MAINNET.md](MAINNET.md).

An earlier read-only comparison on 2026-11-24 found the primary at height
`4,095,578` and OVH at `4,095,577`; both returned the same canonical hash
`3e6579833d018e732a38bfe87f53fd80ff6a360dcc5f4540187b785a4fe23756` at the
common height, with `syncing=false`. This confirms the OVH sync process follows
the same current chain; it does not prove validator admission.

The public Explorer/API is still not safe to use as a Mainnet source of truth:
on 2026-11-24 both public hosts returned stale genesis and height-1 records.
Cloudflare marked the responses `DYNAMIC` and `no-store`, and the active API
bundle on the primary lacks the canonical early-block guard. The current source
has regression coverage for stale hash lookups and canonical height lookups;
31 targeted route tests, typecheck, and build passed on OVH. A live isolated
router check against the copied OVH database and local node also passed 3/3
checks, including canonical heights 0/1 and 404 for stale hashes. The copied DB
tip was 43 blocks behind the node at a later read-only check, with a matching
hash at that tip. Since then, the persistent indexer was configured on OVH to
use `aperod_mainnet_api_20261124` and the local Mainnet node. It now catches up
continuously. Historical burn totals remain unverified because pruned bodies
blocked the scan. The live API service has not been installed there or cut over.

At the 11:14 UTC check on 2026-11-24, both the OVH node API and its PostgreSQL
index reached height `4,099,675` with the same block hash. Both node and indexer
services were active and enabled for boot, with exactly one node process; the
indexer unit depends on PostgreSQL only and cannot start a second node. This
proves local catch-up, not that the public API/Explorer has been corrected or
switched over.

The signed snapshot was copied to OVH and matched its source and signed
manifest exactly: 14,064 files, 16,401,821,101 bytes, tree SHA-256
`18a50686c11d5772606057ab89c08698d71ae698496e4e9bfceb1dceea29d795`. The
closed staging duplicate was then removed from the primary; its active Mainnet
database was left untouched.

At the 2026-11-25 05:28 UTC check, the single OVH tree was still
`/var/lib/aperod/mainnet`, but its live role had changed: `non_validator=false`,
and `/api/v1/my-validator` matched the sole local registry entry `08fd…`
(`active`, 1,000,000 APRO stake). All 25 most recent block records named that
key as producer. The local status endpoint reported height `4,119,630`,
`syncing=false`, and zero missing store/UTXO rows.

The node had one outbound P2P peer with a height-bearing keepalive, but the
peer reported height `4,108,808`, 10,822 blocks behind the local tip. This proves
a live peer connection, not peer convergence. `/api/v1/chain/identity-evidence`
returned HTTP 200 with `operator_1_of_1`; the canonical-index genesis signature
and checkpoint signature at height `4,105,317` verified, while finality quorum
did not. The signed bootstrap statement names `canonical_index_hash` as the
genesis signature target and is signed by a key beginning `b4c0…`, distinct
from the active validator key `08fd…`. The evidence is served by this node and
is not independent quorum verification.

These checks do not establish an independently confirmed canonical tip,
validator-quorum finality, or a clean public validator installation. The
historical body gap and index-completeness limits recorded above remain.

The restart verified the signed genesis/checkpoint and completed the startup
height-index sweep in about 2 minutes 26 seconds. The sweep skipped `798,755`
heights in the known historical body gap `924,790–1,723,544`, so it did not
write the verified-index sentinel. Current sync and local indexing work, but
historical height-index completeness is not established. No old-primary
cutover or public API/Explorer change has occurred.

## Protocol values in source

These values are implemented in the current source; they are not, by
themselves, proof of the live network's activation schedule or an invitation to
send funds.

| Parameter | Source value |
|---|---:|
| Active validators | **21** (maximum) |
| Stake | ≥ **100,000 APRO** locked on-chain |
| Epoch length | **100 blocks** (~300 seconds at 3 seconds per block) |
| Churn limit | **3** new validators per epoch |
| Unbonding period | **144,000 blocks** (~5 days) — node leaves active set |
| Pool-phase block reward | **300,000,000 nAPRO (3 APRO)** |
| Tail emission | **1 APRO** (100,000,000 nAPRO) per block |

## Double-sign penalty

The source specifies a penalty of **10 % of total stake** burned immediately.
This is a source-level value, not proof that the rule is active at the current
Mainnet height.

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
