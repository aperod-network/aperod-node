# Privileged tool updates are separate from binary updates

The original clean-checkout guard is a layout/data-loss check, not evidence
that a locally committed script is approved to execute as root. A local clean
commit can still contain an unauthorized backup or deployment script.

Node, API and validator update entry points no longer load the old tool
synchronizer or replace installed backup/deployment tools. Validator binary
updates also no longer upload/install a backup script remotely. Existing tools
remain unchanged and keep working; these commands update their primary
application only.

The retained manual sync helper requires a third argument: the exact SHA256
of an independently reviewed tool release. A node commit pin, clean Git
status, approval environment variable or manifest inside the checkout does
not authorize it. Operators must not compute this approval from the very
checkout they are trying to authenticate.

The helper rejects source/destination symlinks and symlink parents, compares
the approved digest before copying, and authenticates the staged bytes again
before an atomic replacement. Empty or syntactically invalid staged scripts
are refused without replacing the working installed file. Installing approved
bytes does not execute them.

Regressions in `deploy/test-privileged-tool-trust.py` use real temporary Git
repositories, installed-byte/mode comparisons, controlled payload markers and
a staged-copy mutation. They cover dirty files, clean unauthorized commits,
self-manifests/node pins, source/destination/parent symlinks and the actual
former node-update tool-sync section. The Go deployment suite invokes them.
Positive controls explicitly approve their synthetic fixture bytes.

This is the tool-replacement boundary, not a claim that all executable
libraries loaded by a root updater have a complete authenticated package
lifecycle. Existing root bootstrap scripts must still come from a trusted
release. Production hosts, keys, databases and installed tools were not
changed for these regressions.
