# Aperod Mainnet identity and validator onboarding

This guide pins the existing Mainnet chain and defines the checks required
before a new validator can safely join it. A signed checkpoint is not a
validator-install package: it does not by itself configure P2P access, activate
stake deposits, or establish that a new validator has entered the active set.

## Canonical chain

The existing chain is `aperod-mainnet-1`. Its operator-signed bootstrap
statement identifies:

| Anchor | Value |
| --- | --- |
| Genesis header hash | `c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f` |
| Legacy genesis index hash | `da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1` |
| Block 1 hash | `513c6385aa51b651e671bfa26bc1beebeb163264da52dee35b0995cf5f0fa075` |
| Signed checkpoint height | `4,024,017` |
| Signed checkpoint hash | `63c5b24951137717f1636c4081ea77f2f93e8a5119030e9776ce2e27316e47de` |
| Checkpoint signer | `08fd38ba8857d32d2ac807f0246a0fd298c9edf336fbd0fea3bb7bd818c91137` |

The statement uses the explicitly selected `operator_1_of_1` trust model. This
is an operator signature, not a validator quorum. The genesis header hash and
legacy index hash are distinct protocol values: compare each only with fields
that use the same representation.

## Public Explorer and API check

The public block APIs have previously returned stale records for heights 0 and
1, including a different genesis hash, while returning the signed checkpoint.
The stale genesis hash observed in those responses was
`ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb`;
it is not the canonical Mainnet genesis.
Do not use a successful checkpoint lookup alone to conclude that the Explorer's
genesis entry is correct. Before relying on the public API for chain identity,
require all of the following:

- Height 0 returns the signed genesis header hash and is not marked pruned.
- Looking up the signed genesis hash returns the same height-0 block.
- The first block's parent matches the legacy genesis index hash.
- The previous genesis hash is rejected, not returned as an HTTP 200 block.
- The signed checkpoint height still returns the pinned checkpoint hash.

Run this read-only check with Python 3. It checks both public APIs, including
the stale genesis hash, and exits nonzero if any response is inconsistent:

```bash
python3 - <<'PY'
import json
import urllib.error
import urllib.request

GENESIS = "c8fde86c1764a5603eabd6d9c56c9e47b4cf06d88d299a55665694473e2c512f"
GENESIS_INDEX = "da452e4c9d3bd8a21d73289c88a3583960eb774d0fa312206b72020b5dc54de1"
FIRST = "513c6385aa51b651e671bfa26bc1beebeb163264da52dee35b0995cf5f0fa075"
CHECKPOINT = "63c5b24951137717f1636c4081ea77f2f93e8a5119030e9776ce2e27316e47de"
STALE = "ab5b8fb48ffaa04e6ca8a70d8a32667b74d2acaa8763e6add510e95b4afebbfb"

def get(host, block):
    req = urllib.request.Request(
        f"https://{host}/api/v1/blocks/{block}",
        headers={"Cache-Control": "no-cache", "User-Agent": "Aperod read-only check"},
    )
    try:
        with urllib.request.urlopen(req, timeout=10) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as error:
        return error.code, None

failed = False
for host in ("api.aperod.com", "explorer.aperod.com"):
    for key, height, block_hash, parent in (
        ("0", 0, GENESIS, "0" * 64),
        (GENESIS, 0, GENESIS, "0" * 64),
        ("1", 1, FIRST, GENESIS_INDEX),
        ("4024017", 4024017, CHECKPOINT, None),
    ):
        try:
            status, block = get(host, key)
            valid = (
                status == 200 and isinstance(block, dict)
                and block.get("height") == height
                and block.get("hash") == block_hash
                and (parent is None or block.get("prev_hash") == parent)
                and (height > 1 or block.get("pruned") is not True)
            )
        except (OSError, ValueError):
            valid = False
        if not valid:
            print(f"FAIL {host} block {key}")
            failed = True
    status, _ = get(host, STALE)
    if status != 404:
        print(f"FAIL {host} stale genesis returned HTTP {status}")
        failed = True
if failed:
    raise SystemExit(1)
print("Both public APIs match the signed Mainnet anchors.")
PY
```

A nonzero exit means the public endpoints must not be used as bootstrap
identity evidence.

## Validator onboarding

The repository does not yet contain a verified, public, self-service release
that completes installation, Mainnet synchronization, on-chain stake locking,
and active-set admission. The existing `install-validator.sh` is not a
Mainnet installer; pinning its source commit does not change its network guard.
Do not bypass that guard, initialize a competing genesis, or send APRO based on
an application record or an unverified guide.

Before publishing a usable validator procedure, a release must bind together:

1. The canonical genesis and authenticated checkpoint above.
2. The matching chain snapshot, migration witnesses and registry attestation.
3. An independently reviewed source revision, executable, service profile,
   startup snapshot and checksums.
4. The canonical P2P identity and peer-admission procedure.
5. The consensus-wide activation for the supported stake-deposit format.
6. A tested clean-server installation that restores UTXOs and the validator
   registry, synchronizes through authenticated peers, and restarts safely.
7. An owner-authorized stake deposit that is included on-chain, enters the
   active set at the correct epoch, and produces a block or vote accepted by
   the existing network.

These are separate checks. A valid signature does not prove the source code or
installer is compatible; a running non-validator does not prove stake
eligibility; an API application or wallet balance is not locked stake; and
local tests do not prove Mainnet accepted a vote.

Do not choose a stake-deposit activation height locally. It is a consensus
setting and must match every block-validating node before use. Use only the
activation profile distributed in the reviewed Mainnet release.

## Safe installation rule

Until that release and clean-server rehearsal are available, there is no
supported command in this repository that makes an arbitrary new server a
Mainnet validator. Do not run mutable `main` installers as root, reuse another
validator's signing key, or delete an existing data directory to retry.
Preserve existing wallet, key, and chain data. A configuration check on an
already-installed node is not proof of synchronization or validator
activation.
