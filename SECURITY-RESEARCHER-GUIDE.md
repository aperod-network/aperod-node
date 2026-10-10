# APD Remediation Verification Guide

This guide is for researchers repeating the APD-2026 proof-of-concept tests.
Earlier results obtained from commit `be8e801` do not describe the current
implementation.

## Required baseline

Test the latest `main` at or after remediation baseline:

```text
bee843142f1f1239b261753d76ce39a8d88d8cbe
```

Record the exact commit in the report:

```bash
git fetch origin
git checkout main
git pull --ff-only
git rev-parse HEAD
go version
```

## Remediation map

| Finding | Current control | Primary regression tests |
|---|---|---|
| APD-2026-001 | `UTXOSet.ApplyBlock` completes a read-only preflight before mutating UTXOs, key images, outputs, or rollback state. Startup replay fails closed on application errors. | `core/utxo_preflight_test.go` |
| APD-2026-002 | RingCT v4 binds the real input index, public key, and referenced commitment opening. Legacy history remains replayable before coordinated activation. | `core/ringct_v4_security_test.go`, `crypto/ringct_v4_test.go` |
| CLSAG v5 migration | v5 removes the serialized real index, binds every ring key to its commitment and pseudo-output, and verifies members against a persistent canonical output index. Activation remains separately height-gated. | `core/ringct_v5_test.go`, `core/clsag_ring_store_test.go`, `crypto/clsag_test.go`, `store/ring_member_index_test.go` |
| APD-2026-004 | Privileged mempool state is not restored from disk. Post-activation block rewards require authorization bound to the proposer, parent, height, amount, and authorization ID. | `core/mempool_test.go`, `core/reward_test.go`, `consensus/coinbase_security_test.go` |
| APD-2026-006 | In-flight outbound handshakes reserve capacity, inbound admission counts reservations, and a final ban/capacity check occurs immediately before registration. | `p2p/admission_race_test.go`, `p2p/admission_closure_internal_test.go` |

## Test commands

Run the targeted suites first:

```bash
go test ./core ./crypto ./consensus ./p2p
go test -race ./core ./crypto ./consensus ./p2p
```

Then run the complete repository suites:

```bash
go test ./...
go test -race ./...
```

## Activation-gated checks

APD-2026-002 and the reward-authorization portion of APD-2026-004 are
consensus migrations. Production configuration coordinates both at height
`1,750,000`:

```yaml
ringct_v4_activation_height: 1750000
ring_ct_clsag_activation_height: 0  # disabled until coordinated v5 rollout
reward_authorization_activation_height: 1750000
```

For a local proof of concept, configure a fresh isolated test chain with both
activation heights reachable during the test. Verify behavior immediately
before and after activation. Do not reuse production keys or production chain
data.

Expected post-activation results:

- a copied or substituted real-member commitment is rejected;
- a reward without valid proposer authorization is rejected;
- authorization replay at another height, parent, validator, or amount is
  rejected;
- historical pre-activation blocks still replay successfully.
- when testing v5 on an isolated chain, all ring members resolve from the persistent index, key images prevent reuse, and no real ring index is serialized.

## Reporting results

Include:

1. the full commit hash;
2. the activation heights and test-chain height;
3. the exact command or proof of concept;
4. complete output for any failure;
5. whether the behavior occurred before or after activation.

Send sensitive findings privately using the process in
[`deploy/SECURITY.md`](deploy/SECURITY.md). Do not publish an exploitable proof
of concept before coordinated disclosure.