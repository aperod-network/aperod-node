# Verified rollout artifact cleanup

`aperod-rollout-cleanup` is an advisory registry and fail-closed cleanup tool
for explicitly registered node rollout artifacts. It does not roll back chain
data, restart the node, or infer that an old copy is safe from its age.

## Install and policy

Install from the local checkout (no git operation is performed):

```sh
sudo bash blockchain/deploy/setup-rollout-cleanup.sh
```

The installer installs a versioned Python package below
`/usr/local/lib/aperod-rollout-cleanup/releases/`, then atomically replaces
the root-owned `/usr/local/bin/aperod-rollout-cleanup` launcher. Old package
bundles are retained, so an in-flight launcher never imports a half-upgraded
package. Registry state is root-owned under
`/var/lib/aperod-rollout-cleanup` (mode 0700); job receipts and locks are
owner-only. The daily timer starts a scan at approximately 05:30 local time,
after `aperod-backup.service` when that unit is running. A manual scan is
available with `sudo systemctl start aperod-rollout-cleanup.service` or
`sudo aperod-rollout-cleanup scan`.

The default policy is dry-run. Review the journal and explicit CLI scan output
before deliberately enabling unattended apply:

```sh
sudo journalctl -u aperod-rollout-cleanup.service
sudo aperod-rollout-cleanup scan
sudo bash blockchain/deploy/setup-rollout-cleanup.sh --enable-apply
```

`--enable-apply` atomically updates the root-owned mode-0600 policy file. A
normal installer rerun does not change an already selected policy. Even with
apply enabled, the core helper must independently verify the rollout receipt,
artifact fingerprints, live process and mount references, and immutable
Backblaze B2 object version before deleting anything. Unsupported providers,
unversioned S3-compatible endpoints, a missing or changed `fileId`, stale or
incomplete proof, changed artifacts, or any failed safety check leave artifacts
in place. Age alone never authorizes deletion.

## Registration contract and hooks

The command API is shared by rollout scripts and the cleanup package:

```text
aperod-rollout-cleanup begin --release DIR --data-dir LIVE --config YAML \
  --service NAME --api-url http://127.0.0.1:8545
aperod-rollout-cleanup mark --id UUID --artifact ABSOLUTE_PATH \
  --kind stopped-copy|candidate
aperod-rollout-cleanup complete --id UUID --expected-binary-sha256 HEX
```

`begin` writes a root-owned incomplete job and returns `{"id":"UUID"}`. It is
called before the service is stopped. `mark` accepts only the direct
`stopped-copy` or `node.candidate` child of the unique, closed, root-owned
release directory belonging to that job. It records an immutable fingerprint;
the shared build output, live data, keys, witness/history data, frontend
assets, and an entire releases directory are never registration targets.
`complete` is called only after the rollout readiness checks and installed /
running executable SHA-256 parity checks. The core also checks chain readiness,
height advancement, stable post-restart process identity, and genesis
consistency. Any refusal leaves the job incomplete, which the scanner always
preserves.

The verified production rollout registers the stopped copy and its already
owned candidate. `update-node.sh` makes a unique release-local copy of the
build artifact and registers only that copy; it never marks the shared
`blockchain/build/aperod-node`. After its ordinary health check it performs a
bounded `/api/v1/status` readiness check (`ok`, not syncing, not rebuilding
UTXOs, and height advanced from the post-`begin` baseline), then verifies a
stable running MainPID and candidate/installed/running binary SHA-256 parity
before calling `complete`. If any proof is unavailable, it reports the job as
incomplete and preserves its artifacts without restarting or rolling back the
already-running update.

## Backup and recovery evidence

Cleanup depends on the core backup integration. A verified backup generation
must be newer than the completed rollout and contain the captured rollout
anchor. The root-owned `verified-generation.json` receipt binds proof
generation, source identity, archive digest, configured B2 endpoint/bucket,
and the immutable B2 `fileId`. Cleanup revalidates the exact B2 version against
the locally read-only `/opt/aperod/data/integration-settings.json`; S3 listings
that cannot prove version identity fail closed. The most recent working
`/usr/local/bin/aperod-node.pre-update` is not a rollout cleanup artifact and
is retained as migration/rollback evidence.

The service runs as root because it must inspect host `/proc`, mount references,
and root-owned releases. `ProtectSystem=strict` allows writes only to the
rollout registry and `/opt/aperod/releases`; `/opt/aperod/data` is explicitly
read-only. The unit does not set `PrivateTmp`, restrictive `ProtectProc`, or
`ProcSubset`, so process descriptors remain inspectable. Systemd's read-only
bind mounts for hardening are outside registered release candidates; they do
not grant cleanup permission, and a mount nested in a candidate is rejected by
the core. `CAP_SYS_PTRACE` is included with `CAP_DAC_OVERRIDE` and `CAP_FOWNER`
so the root scanner can inspect the unprivileged `aperod` process's `/proc`
`cwd`, `fd`, and `exe`. If a process reference is unreadable, the core refuses
cleanup rather than treating that process as inactive.