# Aperod Mainnet identity and validator readiness

This page records the identity of the existing Aperod mainnet and the checks
completed on the isolated new OVH server. It is not yet a public install
recipe: a signed chain anchor does not prove a compatible node release, peer
access, or validator admission.

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

The snapshot's 14,065 source-file hashes matched after transfer to OVH. The
bootstrap signature, registry attestation, and AVM checkpoint signature all
passed verification there. The AVM checkpoint is state-only and does not prove
that the chain's historical blocks were replayed. The finality certificate's
signature is valid, but the available committee evidence does not establish a
quorum.

The signed public artifacts are
[`mainnet-bootstrap-4076315.json`](mainnet-bootstrap-4076315.json) and
[`mainnet-avm-checkpoint-4076315.json`](mainnet-avm-checkpoint-4076315.json).
Verify their signatures from the repository root with:

```bash
cd blockchain
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

The public APIs at `api.aperod.com` and `explorer.aperod.com` report the same
later Mainnet tip as the signed OVH checkpoint, but their height-0 and height-1
records are stale. The observed stale genesis is
`ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb`; the
observed stale first block is
`61457b6a59a321588cd501365ab4d3bbbe4ab9ed7e03c326cdeda86adcb28809`. The
observed canonical height-1 index ID is listed above.

Run this read-only check from the repository root:

```bash
python3 blockchain/deploy/verify_public_mainnet.py
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
installer. The API's `/api/v1/chain/identity-evidence` endpoint still returns
HTTP 503 (`checkpoint signer is not the sole authorized operator`): the
operator-signed bootstrap statement is not a one-member on-chain finality
certificate or validator quorum.

The full height-index sweep remained incomplete: `798,755` historical heights
had no retained block body, so the node correctly did not create a
height-index-verified sentinel. The snapshot replay therefore proves startup
from this operator-attested copy, not a complete historical archive. Do not
copy an older `node.yaml` from another server: only a release-bound Mainnet
configuration with the verified chain identity and peer policy is suitable.

No public, signed software-and-snapshot release has passed a clean-server
installation, and active-validator admission has not been tested. Direct P2P
access from OVH to the primary timed out. Do not generate a competing genesis,
bypass the installer's safety checks, or enable block production on the
rehearsal server.

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
network-data paths, but its `install-node.sh` and `install-validator.sh` still
do not support Mainnet; that guard fix does **not** make either script a Mainnet
installer.

The separate message `Directory exists` comes from the shell block's deliberate
`test ! -e "$INSTALLER_DIR"` check. It means the requested checkout directory
already exists. Inspect and preserve it; do not delete it blindly or rerun the
same block with the same path.
