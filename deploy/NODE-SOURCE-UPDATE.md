# Installing and updating after the source-history transition

The public repository uses standalone Go paths at its root. Old clones have an
unrelated history; do not force-reset, clean, or remove their node state.

Download the **complete deployment-tools package** for the reviewed source
revision and verify its published SHA-256 before extraction. Keep its `deploy/`
directory and all sibling helpers together, outside live source/data directories.
Do not run a single downloaded installer without its helpers.

Select the exact reviewed public commit explicitly:

```sh
export APEROD_NODE_SOURCE_COMMIT=<full-reviewed-public-commit>
```

For a fresh installation, run `deploy/install-node.sh` or
`deploy/install-validator.sh` from that package. Installers reject an existing
binary, configuration, or nonempty state directory; an existing node is not a
fresh install. Validator setup also needs the operator's reward address.

Updating an existing node additionally requires:

```sh
export APEROD_BASELINE_APPROVED_SOURCE=<same-reviewed-public-commit>
export APEROD_BASELINE_APPROVED_BINARY_SHA256=<sha256-of-current-node-binary>
```

These values are operator approvals, not automatic proof of chain compatibility.
Before setting them, independently rehearse activation, replay, snapshots,
configuration and network behavior against that node's actual baseline. Use
`deploy/update-node.sh` only after that review. A source commit being clean or
passing build/tests does not establish compatibility with a running chain.

## Isolated host-service tests

`bash deploy/test-node-service-lifecycle.sh all` runs fresh full-node and validator
installations, existing-node updates, and the upgrade entrypoint in disposable
Ubuntu systemd CI guests. It uses independent real Git histories, portable Go
binaries with real VCS metadata, HTTP health checks, and seeded real LevelDB
records. It checks retained runtime bytes/ownership, actual binary-hash changes
before and during the build, owned source cleanup, and health-failure rollback.

The service and chain identities in this harness are synthetic. Package tools
are preinstalled in the image; package-manager and external-IP calls are bounded
fixture operations. Git, Go, make, systemctl, candidate/source guards and HTTP
health checks are not replaced or skipped. Docker is required on the isolated CI
host, not on production nodes; unavailable infrastructure exits 77 with a named
skip summary, and an explicitly enabled Go E2E wrapper reports incomplete coverage.

These tests establish deployment lifecycle behavior, not compatibility with any
live consensus baseline or a cryptographic/security audit of the whole node.

Source is acquired into an owned isolated temporary directory. Existing clones,
keys, configuration, databases and snapshots are not source-cleanup targets.
Candidate builds must have clean revision metadata, CGO disabled and a static
ELF before replacement; missing verification tools or mismatched approvals abort.

The existing `v0.1.1-p2p` binary release is unchanged. Deployment-tools packages
are separate from node binaries and must not be mistaken for an automatic
production upgrade or a comprehensive security audit.
