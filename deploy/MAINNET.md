# Aperod mainnet: validator onboarding

## Release status

This guide is for the **existing Aperod mainnet**, not a new network.
The public source tree does **not yet contain a verified, self-service
mainnet bootstrap release**. A successful source build, a GitHub signature,
or passing CI is not proof that a binary can replay the live chain.
Do not register or fund a new validator on the assumption that installation
alone makes it eligible.

The automatic `install-validator.sh` currently bootstraps a different network.
It is not the mainnet installation procedure. Do not bypass its network guard,
replace a network name in its template, or start a new empty chain.

## What a mainnet release must supply

Before installing, obtain a reviewed release which binds all of these together:

1. An immutable source revision and reproducible node binary with checksums.
2. The canonical genesis **block hash** and matching genesis configuration.
   A filename or `network: mainnet` alone is not chain identity.
3. The live chain's consensus parameters, coordinated activation heights,
   and all required migration/authority witnesses. Do not invent activation
   heights or copy values from a development configuration.
4. A consistent, hash-verified bootstrap snapshot of the same chain, including
   the state needed to validate subsequent blocks, with its height and block hash.
   Never copy a live LevelDB directory with rsync.
5. Verified mainnet peers and an authenticated download path. A bootstrap
   procedure for a public participant must not require root SSH access to
   an existing validator.
6. A tested registration and on-chain stake-locking procedure for this release.

The release must be rehearsed on an isolated replica before it is presented as
ready for independent operators. Checksums of a snapshot establish transfer
integrity, not independent correctness of its contents.

## Safe installation sequence

Once that release is published, the onboarding procedure must complete these
steps in order:

1. Verify release signatures/checksums and the network identity before executing
   installation code. Review the exact revision; do not pipe mutable scripts
   into a root shell.
2. Check the supported OS, architecture, available memory and disk reserve
   against the release and snapshot sizes. Pruning reduces retained history;
   it does not make an arbitrary small disk sufficient for any snapshot.
3. Install into a fresh, isolated directory. Preserve any existing configuration,
   keys and blockchain data; use the release's separate update procedure if
   an installation already exists. Keep errors inside a child shell so they
   do not terminate the operator's SSH session.
4. Create a **new local consensus key** with mode `0600`. Never copy another
   active validator's signing key. Keep the wallet mnemonic and spend key out
   of the server configuration and shell history.
5. Verify and import the mainnet snapshot and its required witnesses. Confirm
   the imported genesis and checkpoint block hashes against the release anchors.
6. Configure the node with `network: mainnet`, the release's complete consensus
   profile, a loopback-only authenticated RPC listener, and verified peers.
   Keep block production disabled while onboarding. An empty-chain sync-only
   node is not a substitute for a compatible bootstrap snapshot.
7. Validate configuration using the **same binary that will run the service**:

   ```bash
   (
     set -euo pipefail
     test -x /usr/local/bin/aperod-node
     test -f /etc/aperod/node.yaml
     sudo /usr/local/bin/aperod-node \
       --config /etc/aperod/node.yaml --validate-config
   )
   ```

   This is a configuration check, **not an installer**, chain replay,
   synchronization proof or validator activation.
8. Start the sync-only node only after the release's state-verification checks
   pass. Confirm peer connectivity, advancing canonical height, matching block
   hashes and finality on the existing mainnet.
9. Register the new validator public key and lock the required APRO as
   **on-chain stake** using the release's supported owner-authorized flow.
   An API application record or an ordinary wallet balance is not proof of
   locked stake. Never send a mnemonic or private key to an application API.
10. Verify the canonical registry entry and eligible stake, then enable
    validator mode according to the release procedure. Confirm inclusion in
    the active set at the relevant epoch and actual accepted signatures/blocks.
    Installation, registration and active-set selection are separate states.

## Eligibility and ongoing operation

The documented participation threshold is 100,000 APRO of eligible locked stake;
active-set capacity is at most 21. Meeting the threshold is not a promise of
immediate selection when higher-ranked eligible validators fill the active set.
Read [validator rules](VALIDATORS.md) together with the verified release profile.

Expose only the required P2P port (normally TCP 30303). Keep RPC, metrics and
management interfaces private; preserve the host's SSH and firewall rules.
Do not reset a firewall as part of onboarding.

Before calling a deployment ready, test restart recovery, snapshot validation,
stake persistence and the configured slashing behavior on an isolated network.
A rule specifying a 10% penalty must actually debit 10% as prescribed; detection,
warnings or exclusion alone are not enforcement.

## Existing mainnet installations

Historical filenames and directories may still include an old network label.
Do not rename live data directories, rewrite genesis or alter consensus values
to make names look consistent. Identity is determined by canonical blocks and
the verified consensus profile. Such migrations require their own reviewed
compatibility and recovery procedure.

Until a complete mainnet release passes the sequence above, there is no honest
copy-and-paste command that can be advertised as making a clean server a fully
participating mainnet validator.
