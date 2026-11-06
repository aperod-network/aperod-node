# Production source/data separation: readiness and maintenance plan

## Current boundary

The production checkout at `/opt/aperod` is data-bearing. The node's current
configuration points at `/opt/aperod/data/testnet`; that path must be treated as
live chain state, not disposable build output. Do not run `git pull`, `reset`,
`clean`, or the all-components `deploy-server.sh` against this checkout.

`/opt/aperod-source-staging` is a separate, clean **sparse** source checkout. It
contains the reviewed API, node, deployment, and shared-library paths, while
excluding `data/testnet` and `blockchain/data/testnet`. It is not a complete
checkout for rebuilding every website artifact. Refreshing it with
`update-source-staging.sh` changes only that source checkout; it does not deploy
anything or change the running node.

The intended node-data target is `/var/lib/aperod/testnet`. This is a proposed
cutover target, not a claim that the current chain has been copied there. Keep
API/runtime settings under `/opt/aperod/data` unchanged unless a separately
reviewed API-data migration is planned.

## Read-only readiness gate

Run this only in a separately approved maintenance window, after arranging a
verified backup and stopping the node. It does not stop services, copy or
delete files, edit configuration, or install a binary:

```bash
sudo env APEROD_MIGRATION_EXPECTED_COMMIT=<reviewed-40-character-commit> \
  python3 /opt/aperod-source-staging/blockchain/deploy/source-data-migration-preflight.py
```

This checker is available on the server only after the staging checkout has
been refreshed to a reviewed commit containing the script. Do not copy an
unreviewed helper onto production just to make the check pass.

The checker fails closed unless all of these are true:

- the staging checkout is clean, on `main`, at the explicitly selected commit,
  and its local `origin/main` contains that commit;
- the reviewed sparse paths are exact and no chain-data tree is materialized
  in staging;
- `/etc/aperod/node.yaml` has exactly one `data_dir`, and its existing data
  directory resolves without symlinks;
- the node service is confirmed inactive;
- the target `/var/lib/aperod/testnet` does not already exist, is outside both
  checkouts, and its parent belongs to `aperod`;
- all existing node-data entries belong to `aperod`, and the target filesystem
  has room for a second copy plus at least 512 MiB or 10% headroom (whichever is
  larger);
- the backup receipt binds the exact source and target paths to an archive whose
  SHA-256 matches, and records a successful isolated restore test for that same
  archive.

The receipt is an operator-provided evidence record, not a digital signature.
The hash detects archive changes; it does not independently prove that the
restore test was performed. Preserve the restore logs and have a second operator
review the evidence before authorizing cutover.

### Backup receipt format

The JSON receipt must be stored outside both checkouts and the node-data target:

```json
{
  "schema": 1,
  "source_data_dir": "/opt/aperod/data/testnet",
  "target_data_dir": "/var/lib/aperod/testnet",
  "source_commit": "<same reviewed 40-character commit>",
  "archive_path": "/path/on/verified/backup/storage/node-data.tar",
  "archive_sha256": "<64 lowercase hex characters>",
  "node_stopped": true,
  "isolated_restore_status": "passed",
  "isolated_restore_archive_sha256": "<same 64 lowercase hex characters>"
}
```

Do not create a receipt by assertion alone. Generate it from the actual backup
and isolated restore rehearsal, then compare the restored chain database and
snapshot inventory against the source captured while the node was stopped.

## Cutover sequence (not executed by this preparation)

1. Confirm a maintenance window, current node height/hash, active service unit,
   data/config paths, filesystem capacity, and a verified off-host backup.
2. Stop `aperod-node` and verify `systemctl is-active aperod-node` reports
   `inactive`. Do not copy a live LevelDB directory with `rsync`.
3. Run the readiness gate above. A `BLOCKED` result means no cutover.
4. Copy `chain.db` and its associated snapshots together to the absent target;
   preserve file modes and `aperod:aperod` ownership. Do not delete or rename
   the original `/opt/aperod/data/testnet`.
5. Verify the copied LevelDB opens read-only, compare chain height/hash and
   snapshot inventory, and confirm the target tree ownership before changing
   `/etc/aperod/node.yaml` to `data_dir: /var/lib/aperod/testnet`.
6. Review the actual systemd unit's `ExecStart`, `WorkingDirectory`, config
   path, and permissions. Keep the source path and runtime data path separate;
   do not infer unit settings from an installer template.
7. Start the node only after the compatibility/replay approval is complete.
   Verify canonical height/hash, API health, peer connectivity, validator
   behavior, and wallet/reward history. Retain the original tree and backup
   until the agreed observation period and rollback criteria pass.
8. Only after successful verification, schedule a separate cleanup of the old
   copies. Never include old chain data in a Git cleanup operation.

This task prepares the safety gate and documents the migration. It does not
perform the maintenance-window copy or alter production services/configuration.
