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

Source is acquired into an owned isolated temporary directory. Existing clones,
keys, configuration, databases and snapshots are not source-cleanup targets.
Candidate builds must have clean revision metadata, CGO disabled and a static
ELF before replacement; missing verification tools or mismatched approvals abort.

The existing `v0.1.1-p2p` binary release is unchanged. Deployment-tools packages
are separate from node binaries and must not be mistaken for an automatic
production upgrade or a comprehensive security audit.
