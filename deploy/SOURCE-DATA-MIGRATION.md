# Mainnet source/data separation: safety requirements

This is a fail-closed planning document, not a migration or installation
procedure. The canonical chain identity and current public endpoint checks are
defined in [MAINNET.md](MAINNET.md). A signed checkpoint does not establish a
compatible binary, a safe data directory, or validator admission.

## Mainnet data boundary

- Treat every existing chain database, snapshot, migration witness, validator
  key, and recovery receipt as valuable until a verified backup and restore
  rehearsal proves otherwise.
- Keep source checkouts and runtime chain data in separate, explicitly reviewed
  paths. Never infer a Mainnet data path from an old config file or installer.
- Do not run `git pull`, `reset`, `clean`, an all-components deployment, or a
  source/data migration against a live validator checkout.
- Never copy a live LevelDB directory. A transfer requires a stopped source,
  matching snapshots, a verified archive, and a read-only restore check.
- Do not delete, rename, or reinitialize the existing canonical chain to retry
  an installation. A new node must join the existing genesis and ancestry.

## Evidence required before a migration

Any future, separately approved migration must first bind all of the following:

1. The authenticated Mainnet genesis and checkpoint in `MAINNET.md`.
2. An exact reviewed source revision, executable, configuration, and checksums.
3. The exact source and destination paths, confirmed from the running service
   rather than inferred from templates.
4. An off-host backup whose digest is verified and whose exact archive passes an
   isolated restore rehearsal.
5. A stopped source, sufficient destination capacity, and unchanged file
   ownership, permissions, snapshots, and migration witnesses.
6. Read-only checks proving that the restored data belongs to the same
   canonical chain before any service is started.

A hash proves that bytes match an archive; it does not prove that the archive is
valid Mainnet state or that a restore rehearsal occurred. Preserve the logs and
recovery evidence for independent review.

## Current limitation

No public, verified source/data migration or self-service validator bootstrap
release is currently available. Do not substitute guessed paths, reuse another
validator's key, create a competing genesis, or describe a node as Mainnet-ready
until a clean-server rehearsal joins the authenticated chain and passes the
on-chain stake and active-set checks.
