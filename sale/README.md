# Checkpoint-gated APRO protocol sales

This package implements signed allocation accounting. The accounting functions
alone do not create APRO. They are now integrated with versioned confidential
outputs in `core`, block validation/production in `consensus`, durable canonical
storage in `store`, and the private node HTTP bridge in `api`.

Implementation and an offline rehearsal do not enable live payments. A network
activation, pinned signed checkpoint and real merchant-payment trial require
separate approval. There is no privileged admin-mint or operator-wallet fallback.

## Explicit trust boundary

The starting point is a future, parent-hash-bound checkpoint signed by more
than two-thirds of an externally configured trusted authority set. Checkpoint
data cannot choose its own trusted authorities.

The checkpoint declares historical issuance, other reservations, the eligible
sale allocation, a snapshot commitment and a dedicated sale-authority key.
Those declarations are a **forward trust assumption**, not a reconstructed
historical supply proof. Their sum cannot exceed the nominal 10B APRO limit.
The explorer's displayed available-allocation amount is not automatically an
eligible sale budget and must not be copied into an activation witness without
reconciliation of the other two declarations.

The sale-authority signature declares that the payment was accepted. Validators
would verify that signature, not independently query NOWPayments. The future
payment bridge must validate the provider signature and invoice, currency,
amount, network and confirmation state before signing. A nonzero payment hash
alone is not payment proof.

## Implemented here

- Version-domain-separated signatures binding checkpoint and purchase fields.
- Canonical activation height, parent hash and genesis binding.
- Threshold governance authorization, rejecting duplicate and unknown signers.
- Exact integer nAPRO debit with an exhausted-budget guard.
- Signed recipient, amount, order and payment references.
- Rejection of both order reuse and payment reuse, including within one batch.
- Immutable successor accounting states: an invalid batch changes nothing;
  an orphaned branch does not consume orders in its parent.

## Canonical settlement integration

1. Transaction version 11 binds the signed authorization to the actual
   confidential output and a verifiable opening of its amount commitment.
2. Consensus rejects that version before the configured activation. The
   checkpoint binds genesis, exact parent, authority and disjoint reservations.
3. Canonical LevelDB batches commit the debit, order/payment fences, UTXO,
   block and tip together. Rollback restores the budget and paid obligation;
   reopening preserves them. An unavailable rollback witness fails closed.
4. Invoice reservations freeze exact decimal USD and crypto amounts before
   accepting payment. Invoice identity is globally fenced and retries are
   idempotent. The merchant bridge verifies actual received cryptocurrency,
   currency, invoice/deposit identity and final payment state.
5. Inclusion, quorum finality and owner recovery are separate states. Only a
   valid owner signature for the exact canonical credit marks an order complete.
   The wallet scans and verifies the actual credited output before signing.
6. Offline tests cover two independent validators, actual private HTTP
   reservation/invoice/payment/recovery endpoints, owner output discovery,
   duplicate requests, storage reopening, rejected mutations and rollback.

## Nominal LPoD allocation epoch

The optional, explicitly signed `AllocationEpochBasis` preserves canonical LPoD
funding, its exact validator/guardian/LPoD reserves and locked principal. The
sale budget must equal the declared available allocation within the 10B nominal
allocation boundary; it is not an additional independently spendable reserve.
Burn counters and protected principal are explicit.

This basis requires `LegacyHoldingsUnverified=true`. It does **not** assert that
old global holdings plus new outputs are proved to fit within 10B. Do not
reinterpret the public available-allocation headline as verified unissued supply,
or silently change the meaning of an older signed reconciliation. Choosing this
forward budget requires an explicit acceptance of the historical trust boundary.

Before live admission: review the exact-parent checkpoint and canonical reserves,
coordinate compatible binaries across validators, verify the pinned producer
capability and merchant catalogue, and separately approve real-payment testing.
Do not enable payments merely because these tests pass.

Neither `Begin` nor `ApplyAccountedPurchases` is evidence of credited APRO.
The fail-closed API capability and legacy admin-mint rejection remain mandatory.
Expired, failed or cancelled display states do not discard a subsequently
verified paid invoice; the durable recovery worker preserves that obligation.
Neither merchant success nor a display-only balance increment proves delivery.

## Isolated verification

```sh
cd blockchain
GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.25.13+auto go test -race ./sale ./store ./consensus ./api -run 'Sale|OperatorEpoch'
```

From the workspace root, verify the merchant adapter, frozen quotes and recovery:

```sh
pnpm --filter @workspace/api-server exec vitest run protocol-sale nowpayments-webhook allocation-purchase-invoice apro-purchase-price sale-readiness --maxWorkers=2
```

The tests use generated disposable keys and synthetic checkpoint data. They do
not contact production, pay an invoice or access a validator's private key.
