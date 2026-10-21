# AVM recovery checkpoints

AVM checkpoints bound startup replay without changing block or consensus
rules. A v2 checkpoint contains the complete ordered AVM state, deterministic
state commitment, activation height, genesis hash, canonical block hash,
per-block write-set commitment, format version, and an m-of-n set of detached
Ed25519 signatures over the same payload.

## Trust model

Quorum-configured nodes accept only v2 checkpoints with at least the
configured number of distinct, valid signatures from explicitly pinned
authorities. Every supplied signature is checked; an invalid extra signature
also rejects the checkpoint. The node does not need witness private keys.

Legacy v1 checkpoints have a single operator-attested signature. They are not
quorum certificates and are rejected when the node is configured for quorum
mode. Do not describe v1 as independent consensus evidence.

Startup binds the signatures to activation height, genesis, canonical block
hash, state commitment, and AVM write-set commitment. It then checks the
checkpoint block against the canonical height index and replays the suffix,
checking block hashes, linkage, Merkle roots, signatures, and stored AVM
markers. Missing checkpoints are allowed only when the complete AVM history
fits within the one-million-block replay limit. A bad, stale, forked,
under-signed, or over-limit checkpoint fails closed.

## Build, sign, assemble

Build an unsigned draft from a stopped-node database copy. Each witness must
sign only after checking the candidate against its own canonical database
copy. The signing command replays that witness's chain through the candidate
height and refuses to sign if the payload differs. Keep witness keys on their
respective witness hosts; do not copy them to the node or to a shared
assembly host.

Build a draft:

```sh
cd blockchain
go run ./cmd/avm-checkpoint \
  -mode build \
  -db /path/to/stopped-copy/chain.db \
  -activation-height 123456 \
  -out /shared/avm-checkpoint-draft.json
```

Each witness independently verifies and signs it. Use a key file with mode
`0600`:

```sh
go run ./cmd/avm-checkpoint \
  -mode sign \
  -db /path/to/this-witness/chain.db \
  -activation-height 123456 \
  -candidate /shared/avm-checkpoint-draft.json \
  -key /path/to/this-witness/witness.key \
  -out /shared/witness-1-signature.json
```

Assemble at least `m` detached signature files against the complete pinned
authority list. The assembly step verifies every signature and rejects
duplicates, unknown keys, mismatched payloads, or insufficient count:

```sh
go run ./cmd/avm-checkpoint \
  -mode assemble \
  -candidate /shared/avm-checkpoint-draft.json \
  -signature /shared/witness-1-signature.json \
  -signature /shared/witness-2-signature.json \
  -authority "<witness-1-public-key-hex>" \
  -authority "<witness-2-public-key-hex>" \
  -authority "<witness-3-public-key-hex>" \
  -threshold 2 \
  -out /path/to/avm-state.checkpoint.json
```

Configure the same authority list and threshold on the node:

```yaml
consensus:
  avm_checkpoint_file: avm-state.checkpoint.json
  avm_checkpoint_trust_authorities:
    - "<witness-1-public-key-hex>"
    - "<witness-2-public-key-hex>"
    - "<witness-3-public-key-hex>"
  avm_checkpoint_witness_threshold: 2
```

Relative checkpoint paths resolve under `data_dir`. Key rotation must use an
overlapping authority set to produce a new checkpoint signed by the new
quorum before removing retired keys. A safe sequence is: add new keys while
keeping the old threshold valid, install a checkpoint signed by the intended
new quorum, then remove retired keys. A checkpoint signed only by retired keys
must fail after the authority list is changed.

The builder and every witness replay reject missing bodies, broken linkage,
invalid block signatures, and AVM commitment mismatches. Do not bypass those
checks to force signatures. Quorum signatures authenticate the state snapshot
as trusted witnesses; they do not replace the node's canonical block and
suffix validation.
