# Aperod Mainnet identity and validator readiness

This page records the identity of the existing Aperod mainnet and the checks
completed on the new OVH server. It is not yet a public install
recipe: a signed chain anchor does not prove a compatible node release, peer
access, or validator admission.

## Current OVH recovery and build (checked 2026-11-26 14:48 UTC)

The existing Mainnet producer on OVH and the relay now extend the same restored
parent at height **4,108,808**, hash
`8647a55a0be17d8e0de0b64253754a70af84781961d7d33d77d2cacb336b37fa`.
At 14:48:02 UTC the producer, relay, `explorer.aperod.com`, and `aperod.com`
all reported height **4,108,902**. A second observation confirmed continuing
height growth. Fixed-height block hashes also matched both nodes, both public
origins, and the PostgreSQL index. Independent requests may straddle block
production or the indexer's polling interval; compare the same height and hash.

The running node executable is SHA-256
`f3e4c43b6d74a6b47443d6f784f92b4e7114b6d6e3782d243c1149e3c42c7637`.
It is a narrow derivative of the previously reproduced `ed529818…c26600`
image below, not a build of the complete current workspace. Its production
source archive SHA-256 is
`5a2cc0147d3b288e37e9018285b0ed8036e156c1fc2023d8694f46f60ee37691`.
The checksum-verified official Go 1.26.11 distribution reproduced the unchanged
baseline byte-for-byte before this derivative was built. Only the producer's
oracle-price selection/self-validation and its regression test were changed.
The existing incoming-block 5% parent-price rule was not relaxed.

With explicit recovery approval, the relay was cleanly stopped for a direct
stream to OVH, without staging a database archive on its nearly full disk.
All 4,493 captured files were SHA-256 verified; an additional bounded audit
verified 3,492 retained blocks, their oracle transitions, authority signatures,
and the tip-bound LPoD checkpoint. This is not a full historical state replay
or an independent historical-finality proof.

The rejected OVH suffix through height 4,122,224 was retained, not destroyed.
All 13,416 replaced blocks contained only LPoD payout/checkpoint transactions,
with no transaction inputs. A consistent PostgreSQL custom-format backup was
fully read/decompressed before reconciling only the derived `blocks`,
`transactions`, `address_txs`, and `chain_stats` suffix. Wallet/application tables
and the local node identity keys were not changed.

Recovery receipts, both inactive state backups, the PostgreSQL archive, and
reproducible build inputs are retained privately under
`/var/backups/aperod/recovery-common-4108808-20261126`.
The build inputs are in `release-inputs`; live services continue to use the
single tree at `/var/lib/aperod/mainnet`. The retired server was not used.
Temporary transfer credentials and one-use workers were removed after public
verification. These checks do not certify unrelated historical index rows or
make the private source snapshot an authenticated public release.

## Previous OVH operation (recorded 2026-11-25 06:07 UTC)

The single active OVH Mainnet tree is `/var/lib/aperod/mainnet`. The node is
managed by the enabled `aperod-node.service`, runs as the restricted `aperod`
account, and is explicitly configured with `network: mainnet` and
`consensus.non_validator: false`. Its local API listens on
`127.0.0.1:18545`, and its P2P listener is enabled on port `30303`.

At the 2026-11-25 06:07 UTC check, the local API reported height `4,120,290`,
`syncing=false`, `utxo_rebuilding=false`, and zero missing store/UTXO rows. The
only local validator registry entry was `08fd…`, status `active`, with
1,000,000 APRO recorded stake. After restart, the node finalized block
`4,120,146` and continued producing blocks. This confirms local active status
and production, not independent acceptance by another validator. One outbound
P2P peer completed a height-bearing
keepalive but reported height `4,108,808` (11,482 blocks behind the local tip).
The peer connection is live; peer convergence to the current tip is not
established.

Read-only checks from the new OVH showed the relay and OVH share the same hash
at height `4,108,808` (`8647a55a…`). The next OVH block, height `4,108,809`,
extends that hash but has `oracle_price=0`; the relay log rejects it with
`oracle price is missing after hardening activation`, then rejects later blocks
because they cannot extend its unchanged tip. At the 06:07 UTC check the relay
was still at `4,108,808`, 11,482 blocks behind OVH. Both nodes use the same
binary hash, and both configuration files specify
`security_hardening_activation_height=3,800,000`; the OVH service started after
its config file was written. The local validator has since produced blocks
with nonzero oracle prices, but this does not repair the first rejected
ancestor. This is a consensus divergence, not a simple peer-disconnect issue.
The OVH validator was gracefully stopped for the authorized snapshot rehearsal
at 05:54:19 UTC and restarted at 05:56:13 UTC; the relay was not changed. No
consensus settings or production source were changed.

The production log for this block records an oracle JSON decode failure at
`20:19:39 UTC`, followed by `producing block` and `block finalized` for height
`4,108,809` at `20:19:42 UTC`. The exact source snapshot later reproduced from
the active binary calls `checkOraclePriceDeviation` while validating
peer-received blocks, but not in `Engine.produceBlock`. This source-level gap is
consistent with the observed zero-price block; it does not by itself prove the
historical incident's root cause. A source patch now runs the existing oracle
check for self-produced blocks before state mutation, with a regression test.
Targeted tests passed on Go 1.26.11 in a temporary workspace on the new OVH; the
temporary source, toolchain, module cache, and test files were removed. The
patch is not deployed. The validator restart saved a snapshot at height
`4,120,145`; a consistent copy of the complete tree was `18,847,403,159` bytes,
and the copied snapshot passed `--check-snapshot` at that tip with a matching
SHA-256. The live node resumed from the same tip, passed startup integrity and
snapshot UTXO-count checks, became API-ready, and finalized block `4,120,146`.
By 06:04 UTC the API reported `syncing=false`, `utxo_rebuilding=false`, and zero
missing store/UTXO rows. The temporary copy and helper were removed. This proves
the current snapshot restart path, not rollback or repair of the divergent
ancestor; the relay remains stalled at `4,108,808`.

The node's identity endpoint returned HTTP 200 under the `operator_1_of_1`
trust model. It reported a valid signature over the canonical genesis index
hash and a valid checkpoint signature at height `4,105,317`; it explicitly did
**not** verify finality quorum. The signed statement declares
`canonical_index_hash` as the genesis signature target, so the genesis header
hash and canonical index hash differ. The statement signer begins `b4c0…`,
while the active validator key begins `08fd…`; this difference must remain
visible when assessing which authority signed the bootstrap statement. This
endpoint is evidence from the serving node, not independent quorum
verification.

At the 11:14 UTC check, the node and PostgreSQL database
`aperod_mainnet_api_20261124` both reached height `4,099,675` with the same
block hash. `aperod-explorer-indexer.service` is active and caught up to that
height, and is enabled for boot. This confirms the local indexer pipeline only;
the public API and Explorer have not been switched to OVH.

The 11:05 UTC restart verified the signed Mainnet genesis/checkpoint and
resumed the existing chain. The height-index repair took about 2 minutes 26
seconds but skipped `798,755` heights because their block bodies are absent.
That count matches the already-audited historical body gap
`924,790–1,723,544`; the height-index sentinel was therefore not created.
Current blocks are accepted and indexed, but the historical index is not
complete and must not be described as fully verified.

### Active binary source provenance (read-only audit, 2026-11-25)

The active `aperod-node.service` was not stopped or changed. The running
executable (`/proc/1392/exe`) and installed path
`/var/lib/aperod/mainnet/aperod-node` had the same SHA-256:
`ed529818d460260dbe68d4dfe0ed63e9caa02a831e548d125bc8ecd041c26600`.
The active executable and pre-existing
`/var/tmp/aperod-mainnet-compat-20261124/aperod-node.next` have the same SHA-256.
Embedded Go build information identifies Go `1.26.11`, `linux/amd64`,
`GOAMD64=v1`, `CGO_ENABLED=0`, and `-trimpath`; the main module is
`github.com/aperod/aperod (devel)`, with no VCS revision, timestamp, or modified
tree marker.

The on-server source copy at `/opt/aperod/blockchain` has no `.git` metadata and
its `go.mod` declares Go `1.25.13`, `golang.org/x/net v0.56.0`, and
`golang.org/x/sys v0.46.0`; it does not match the active binary's toolchain and
linked dependency versions. The saved candidate
`/opt/aperod/candidates/86244868/aperod-node` is also a different binary:
SHA-256 `9b925cac3ddf57d4f5fc29a0b83ba0b7b7bf97d6c7af6c605107c133bf0d4586`.
That candidate embeds clean VCS revision
`86244868c5761441524884ca09b1ad4731f70573`, which GitHub reports as a valid
signed commit, but its hash does not match the active executable.

The source archive
`/var/tmp/aperod-mainnet-compat-20261124/source-next.tgz` has SHA-256
`31090bfbc9d31b97bfe20a4b0e3a061679267635a5439efc02597876c8db1b2c`; its
extracted source tree has no `.git` metadata. A fresh build from that tree on
OVH, using the checksum-verified official Go `1.26.11` toolchain
(`go.tar.gz` SHA-256
`9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec`) and
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -trimpath ./cmd/node`,
reproduced the active binary exactly: SHA-256
`ed529818d460260dbe68d4dfe0ed63e9caa02a831e548d125bc8ecd041c26600`,
22,010,387 bytes. `go mod verify` passed, and the source `go.mod` and `go.sum`
were unchanged. This supersedes the earlier failed build from local commit
`2028aedd0ad2f80029ff5750948bb9cee216181d`; that commit is not the active
source.

The 872 files in the archived source snapshot match the corresponding files in
local commit `01d6a17018bd7701cb04159715fdd2bfb5c59a26`. That commit was created
after the archived build input, is unsigned, and is not present in the public
`aperod-network/aperod-node` repository. It also contains five additional
tracked artifacts (two binaries and three JSON files) absent from the archived
snapshot. Therefore the exact source snapshot and build inputs are verified by
byte-for-byte reproduction, but there is still no authenticated public
commit/tag that names that exact archived tree.

**Source-mode CVE scan:** `govulncheck v1.10.0` ran against `./...` from the
reproduced source tree on OVH and reported `No vulnerabilities found.` The scan
used the current Go vulnerability database. The earlier binary-mode result and
workspace scanners are separate evidence and are not substitutes for this
source-mode result.

No deploy, service restart, or chain-data operation was performed. Only
task-created temporary build and scan directories were removed; the pre-existing
archive and staged binary were left untouched. The node remained active.

## Canonical Mainnet anchors

The new OVH server inspected a closed copy of the existing chain database and
verified an operator-signed bootstrap statement and state-only AVM checkpoint.
The statement uses the explicitly selected `operator_1_of_1` trust model. It is
an operator attestation, **not validator-committee quorum evidence**.

| Anchor | Verified value |
| --- | --- |
| Network / chain ID | `mainnet` / `aperod-mainnet-1` |
| Genesis header hash | `c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f` |
| Genesis height-index hash | `da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1` |
| Genesis timestamp | `1782932857` seconds |
| Genesis signer | `08fd38ba8857d32d2ac807f0246a0fd298c9edf336fbd0fea3bb7bd818c91137` |
| Genesis transactions | `0` |
| Signed checkpoint | height `4,076,315`, hash `48f2668a598b9fe9111bed594ce7fcf646bdb15385eac91542188dd5a82aea70` |
| Checkpoint registry digest | `7f20fab06735fbddc014316f37c8da667e99ce122a6e28bd4db28a6722506254` |
| Snapshot tree SHA-256 | `18a50686c11d5772606057ab89c08698d71ae698496e4e9bfceb1dceea29d795` |
| Bootstrap operator public key | `8bf6b47c87e075007527993d3ea1c8f45424c1f382cb43eab8ab7f5c19cb650f` |

The closed snapshot tree contains 14,064 regular files totaling 16,401,821,101
bytes. Its tree SHA-256 matches the value in the signed bootstrap manifest. The
bootstrap signature, registry attestation, and AVM checkpoint signature all
passed verification on OVH. The AVM checkpoint is state-only and does not prove
that the chain's historical blocks were replayed. The finality certificate's
signature is valid, but the available committee evidence does not establish a
quorum.

The signed public artifacts are
[`mainnet-bootstrap-4076315.json`](mainnet-bootstrap-4076315.json) and
[`mainnet-avm-checkpoint-4076315.json`](mainnet-avm-checkpoint-4076315.json).
Verify their signatures from the repository root with:

```bash
go run ./cmd/mainnet-bootstrap-attest evidence-verify \
  --manifest deploy/mainnet-bootstrap-4076315.json \
  --avm-checkpoint deploy/mainnet-avm-checkpoint-4076315.json \
  --avm-activation-height 1850000
```

This checks the operator signature and AVM checkpoint binding only. Its output
deliberately reports `snapshot_payload_verified:false` and
`finality_quorum_established:false`; verifying the database requires the full
closed snapshot and the migration artifacts in the full `verify` command.

[`config/genesis-mainnet.yaml`](../config/genesis-mainnet.yaml) is the
Mainnet registry-seed profile: its timestamp and signer match the inspected
genesis block. It cannot replace the signed chain database or initialize an
empty Mainnet node. The 10-billion APRO supply is a protocol/configuration value;
it is not encoded in the signed bootstrap statement.

## Public API and Explorer

The block API `hash` field is the canonical height-index ID, not always the
serialized header hash. At height 0, the canonical index ID is
`da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1`; the
separately signed genesis header hash is
`c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f`. The
canonical height-1 index ID in the signed OVH snapshot is
`056476ad6cf64f8c47a7ec0f0033128cc7759d2bdfd699a319d44d6146a94654`, and its
parent must be the genesis index ID.

Read-only requests from the new OVH server on 2026-11-24 confirmed that
`api.aperod.com` and `explorer.aperod.com` expose the same conflicting history:

| Request | Observed result |
| --- | --- |
| `/api/v1/blocks/0` | HTTP 200, stale genesis `ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb` |
| `/api/v1/blocks/da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1` | HTTP 200, signed genesis header `c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f` |
| `/api/v1/blocks/1` | HTTP 200, stale block `61457b6a59a321588cd501365ab4d3bbbe4ab9ed7e03c326cdeda86adcb28809` with parent `fd30cd3c20994dce47cf834feab5df9dd8f03ef07c1aa02eda6cbc8ed06eda9b` |
| `/api/v1/blocks/4076315` | HTTP 200, signed checkpoint hash `48f2668a598b9fe9111bed594ce7fcf646bdb15385eac91542188dd5a82aea70` |
| Lookup by either stale hash | HTTP 200, not the required 404 |

Thus the matching checkpoint does not establish a single, internally
consistent public chain history. The canonical height-1 index ID and parent
remain the signed OVH values listed above. Do not use the public Explorer/API
as a genesis source until both endpoints stop serving these stale records.

Run this read-only check from the repository root:

```bash
python3 deploy/verify_public_mainnet.py
```

It checks both public APIs against the genesis, first-block parent, and signed
checkpoint, and requires stale block hashes to return HTTP 404. A failure means
the public Explorer/API must not be treated as authoritative chain identity.
Pass a single API base URL, such as `http://127.0.0.1:18545`, to check an
isolated node before changing any public route.

## Validator installation status

On 2026-11-23, targeted Go tests passed on the new OVH server, including storage
compatibility, checkpoint binding, and key-image-anchor checks. A manually
staged non-validator node loaded the copied Mainnet state, verified the signed
genesis/checkpoint and AVM state, replayed the operator-attested key-image
anchor, and restored the registry at height `4,076,315`.

The running test binary reports `buildvcs.modified=true`; it was built from a
modified working tree and is not an immutable or approved release artifact.
The sync/restart rehearsal is evidence about node behavior only and must not be
used as a downloadable validator binary.

The node then established outbound P2P to the authenticated Mainnet relay and
applied live blocks through height `4,083,026`. A graceful shutdown saved a
snapshot at that height. After restart, it reloaded the saved snapshot,
reconnected to the relay, and accepted blocks through at least height
`4,083,185`. At the ready check, the local API reported `syncing=false`,
`utxo_rebuilding=false`, `phantom_ki_count=0`, `store_missing_blocks=0`, and
`utxo_store_missing=0`. The local API verifier passed for genesis, the first
height-index entry and parent, and the signed checkpoint; a stale genesis hash
returned 404. The node ran with `non_validator: true` and did not produce
blocks.

This was a **copied-state rehearsal**, not a clean install from a published
installer. A source regression test now verifies the distinction between the
collateral identity in finality records and a height-scoped replacement
signing key. A read-only logical snapshot taken from the live OVH node at height
`4,093,877` contained the latest finality certificate at height `4,076,315`.
Signature verification across every retained block from that checkpoint through
the snapshot tip found `17,563` signatures under replacement signer `eb9650…`,
with collateral identity `08fd…` in the headers; none of those signatures
verified under the identity key or the new staged key `056a…`. This shows that
`eb9650…` is required to verify the current chain history as the configured
signing authority for the `08fd…` collateral identity. It does **not** show that
the key should sign future blocks. “Historical only” can mean retaining its
public verification authority without using its private key on OVH; removing it
from historical verification would make this chain fail validation.

The staged key `056a…` is not yet bound by an approved authority transition or
the signed bootstrap artifacts. During an earlier 2026-11-24 restart attempt,
the restored binary returned HTTP 503 after requiring a persisted finality
certificate, and a candidate build also failed its checkpoint-signature check.
Those observations describe that earlier attempt only. The later live check at
05:28 UTC, recorded at the top of this page, returned HTTP 200 for the
operator-signed genesis/checkpoint evidence. That response still does not
establish validator-quorum finality or independent peer convergence.

The post-restart height-index sweep took about 2 minutes 22 seconds and remained
incomplete: `798,755` heights had no retained block body, so no
height-index-verified sentinel was created. The restored node completed its
operator-attested snapshot replay and became API-ready after about 6 minutes 21
seconds. At recorded local checks it reported heights from `4,094,720` to
`4,094,730`,
`syncing=false`, and `utxo_rebuilding=false`. Local height lookups returned the
canonical genesis and first-block IDs, and both known stale block hashes returned
404. This proves the current local API's block index, not a complete historical
archive. Do not copy an older `node.yaml` from another server: only a
release-bound Mainnet configuration with the verified chain identity and peer
policy is suitable.

No public, signed software-and-snapshot release has passed a clean-server
installation, and active-validator admission has not been tested. Direct P2P
access from OVH to the primary timed out. Do not generate a competing genesis,
bypass the installer's safety checks, or enable block production on the
rehearsal server.

An earlier read-only comparison on 2026-11-24 found the primary at height
`4,095,578` and OVH at `4,095,577`; both returned the same canonical block hash
`3e6579833d018e732a38bfe87f53fd80ff6a360dcc5f4540187b785a4fe23756` at the
common height. Both nodes reported `syncing=false`. This confirmed that the OVH
sync process followed the same chain at that time; it did not prove validator
admission or block production.

### Public API and Explorer status (2026-11-24)

Read-only requests from OVH to both `api.aperod.com` and `explorer.aperod.com`
still returned the old genesis hash
`ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb` and the
old height-1 hash `61457b6a59a321588cd501365ab4d3bbbe4ab9ed7e03c326cdeda86adcb28809`.
Cloudflare reported `DYNAMIC` with `Cache-Control: no-store`, so the stale
response came from the origin, not an edge-cache hit. Read-only inspection found
the public API service on the primary and confirmed its active bundle lacks the
canonical early-block guard present in the current source.

The primary API database was copied into a separate OVH PostgreSQL database
without changing the source. Its block rows matched the OVH node at the signed
checkpoint (`4,093,877`) and at the exported database tip (`4,095,914`). At the
initial check, the OVH node had advanced to `4,095,953`, so the copied index
needed to catch up by 39 blocks. The database copy still contains the stale
height-0/1 rows; the current API source must serve canonical node records instead.
The temporary dump was removed.

On OVH, the current API router was then tested in an isolated, temporary
integration harness against that copied database and the local Mainnet node:
3/3 checks passed. Height lookups for 0 and 1 returned the local node's canonical
blocks, both stale database hashes returned 404, and the database tip was within
100 blocks of the node. A subsequent read-only check recorded DB tip
`4,096,227`, node height `4,096,240`, and a matching canonical hash at the DB tip.
The temporary harness, source tree, dependency cache, and state files were
removed. Importing the router also triggered a historical burn scan; it hit
unavailable pruned bodies, so this check does not verify burn totals. At that
time, the harness was stopped and the copied index was not continuously updated,
so it fell behind the live node. A later read-only check recorded DB tip
`4,096,227` against node height `4,096,270` (43 blocks behind), with a matching
canonical hash at the DB tip.

A later read-only check found the indexer behind the live node, so it was
configured on OVH to use `aperod_mainnet_api_20261124` and the local node API.
The service's PostgreSQL write permissions and target were verified before it
was started. It now catches up continuously, and its systemd unit depends on
PostgreSQL only; it cannot start a second node.

Separately, the early-block regression tests, API typecheck, and candidate build
passed on OVH in a temporary source tree that was removed after verification.
No full API service was installed or started on OVH and no public routing was
changed. The live Explorer/API therefore remains incorrect and is not ready for
validator onboarding.

The signed snapshot was copied to OVH and its destination tree matched the
primary source and signed manifest exactly: 14,064 regular files,
16,401,821,101 bytes, SHA-256
`18a50686c11d5772606057ab89c08698d71ae698496e4e9bfceb1dceea29d795`. After
that match, only the closed staging duplicate was removed from the primary;
the active Mainnet database and node were not changed or restarted. The
primary's reported free root space increased to about 38 GB.

This records the earlier pre-activation sync-only state, not the live state
checked at 05:28 UTC above. At that earlier point the node was configured with
`non_validator: true`, its P2P listener was loopback-only, and its stored key
was not admitted to the active set. The current validator role, peer lag, and
identity-evidence limitations are documented at the top of this page. No
public API/Explorer route or old-primary cutover has occurred.

Startup also reported `trust=operator_attested` with
`historical_committee_authenticated=false` for the registry snapshot. This
does not prove the historical committee or independent quorum.

Before publishing a verified, self-service validator command, one release must
bind:

1. The canonical genesis and signed checkpoint artifacts listed above.
2. The source revision, executable, config, migrations, and a
   consistent, hash-verified bootstrap snapshot.
3. The network's coordinated consensus activation profile.
4. A verified Mainnet P2P peer pin and admission path.
5. A clean-server restore, sync, restart, and canonical-tip verification on OVH.
6. An owner-authorized on-chain stake locking transaction and proof of
   admission to the active set.
7. A block or vote accepted by the existing Mainnet.

Until those checks pass, there is no supported command in this repository that
makes an arbitrary server a Mainnet validator. Do not copy another validator's
signing key or delete an existing chain directory to retry.

The source-pinning shell block is not a mainnet installer. If a prior attempt
left a checkout, inspect it before deciding whether to reuse it.
Preserve that directory and its contents rather than deleting them to retry.
Run guarded commands that use `set -e` or `exit` in a child shell (`(...)`) so
a failed check does not close the SSH session.

## Diagnosis of the earlier installation command

The previously shared pin `de38c4b084c4377b5925d501d853cfe3da555a92` fails
because its source-safety guard treats the repository's tracked top-level
`data/` web assets as runtime data and refuses the checkout. Do not bypass that
guard. A later signed public source revision narrows the check to actual
network-data paths, but its generic install scripts still do not provide a
verified Mainnet bootstrap; that guard fix does **not** make them Mainnet
installers.

The separate message `Directory exists` comes from the shell block's deliberate
`test ! -e "$INSTALLER_DIR"` check. It means the requested checkout directory
already exists. Inspect and preserve it; do not delete it blindly or rerun the
same block with the same path.
