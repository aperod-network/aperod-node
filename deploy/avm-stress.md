# Isolated AVM stress validation

This opt-in Linux test does **not** contact production, use operator wallets, or
write mainnet state. All keys, funding and databases are synthetic. Its HTTP
client rejects every non-loopback destination, including redirects.

## Run

From the repository root:

```sh
APEROD_RUN_AVM_STRESS=1 bash blockchain/deploy/test-avm-http-stress.sh
```

The wrapper runs the mixed-HTTP test. The existing direct-mempool benchmark
remains separate and unchanged in test-avm-stress.sh.
Default duration is two minutes, with 12 senders and two Go CPU
threads. The mixed test additionally takes about 30 seconds for baseline blocks
and time to construct cryptographically valid fixtures. The wrapper creates
and removes its own temporary build cache and databases; an explicitly supplied
GOCACHE remains caller-owned. Without opt-in, on non-Linux systems, or without Go,
it reports SKIP_SUMMARY and exits 77 rather than claiming a pass.

For a five-minute mixed-HTTP soak alone:

```sh
cd blockchain
APEROD_RUN_AVM_STRESS=1 APEROD_AVM_STRESS_DURATION=5m \
  APEROD_AVM_STRESS_WORKERS=12 GOMAXPROCS=2 GOMEMLIMIT=512MiB \
  GOSUMDB=sum.golang.org GOTOOLCHAIN=go1.26.10+auto \
  go test -p 1 ./consensus -run '^TestAVMMixedHTTPStress$' -v -count=1 -timeout=8m
```

## What is exercised

- Two independent consensus engines with separate LevelDB, UTXO, AVM and mempool
  state. Public requests use actual CORS/rate-limit middleware and AVM handlers.
- Real loopback HTTP carries transactions and signed blocks between nodes.
  Block production uses produceBlock; both nodes validate and durably accept
  the same block through handleIncomingBlock.
- Fresh fee-paying CLSAG transactions continue throughout the soak. The initial
  valid burst deliberately fills the test pool.
- Concurrent stale/future nonce, malformed Wasm, dynamic out-of-gas, oversized
  module and oversized HTTP body requests. Nonce/Wasm/gas controls are fully
  signed before submission and independently pass wallet-crypto verification.
  Structural oversize controls are expected to fail before crypto verification.
- Identical canonical state, receipts for valid work, no invalid admission,
  consensus halt or residual mempool entries after draining.

## Report and acceptance envelope

AVM_HTTP_CLASS reports attempts, actual rejected requests, 429s and admission
p95/p99 for each invalid class. **429s are excluded from admission percentiles**;
fast throttled responses must not conceal slow execution checks.

AVM_HTTP_STRESS_SUMMARY includes valid admission p95/p99, request throughput,
confirmed valid count, baseline and loaded block-commit p95/p99, block gaps,
process CPU seconds/average cores, sampled RSS/heap peaks, retained heap delta,
pool peaks and the highest reserved block gas. RSS/heap/pool sampling is 100 ms.

The mixed test uses the normal three-second block target. It asserts:

- At least 95% of the scheduled block count; commit p99 at most 500 ms,
  block-gap p99 at most 3.5 s, no gap above 6 s.
- Actual admission p99 at most one second, for valid and each invalid class.
- RSS growth at most 256 MiB and post-GC retained heap growth at most 64 MiB.
  Average process CPU at most GOMAXPROCS + 0.5 cores.
- Pool count/bytes never exceed the **lowered fixture limits** of 8 entries /
  24 KiB. Saturation is required, not inferred from an empty pool.
- Transactions reserve slightly over half the **real protocol block gas cap**,
  so the producer must defer additional work rather than exceed that cap.

## Evidence boundaries

This is a **two-engine, single-process integration network**, not a benchmark of
the real P2P transport, OS-process isolation, WAN partitions or production disk.
CPU/RSS figures cover the whole test process, not each validator separately.
Lowered pool limits prove enforcement at those limits; they do not prove safety
of filling the production 256-MiB pool or of an unlimited distributed attack.

Do not silently equate accelerated block schedules with the normal target.
A short 200-ms scheduling experiment exceeded its latency budget during the
valid burst; a normal-cadence soak is a separate claim, not a reversal of that
result. Race-detector instrumentation is also not a capacity benchmark.

## Recorded trial — 2026-10-20

The five-minute mixed-HTTP run passed on a two-Go-thread test process:

| Measurement | Result |
| --- | --- |
| Measured duration | 300.278 seconds |
| Requests / throughput | 138,738 / 462.03 requests per second |
| Fresh valid admissions / receipts | 64 / 58 |
| Invalid admissions / unexpected responses | 0 / 0 |
| Invalid requests reaching validation | 7,260; all rejected |
| Throttled requests | 131,414 |
| Valid admission p95 / p99 | 310.105 ms / 479.788 ms |
| Invalid admission p99 range | 180.981–237.744 ms, excluding 429s |
| Produced blocks | 100 |
| Baseline commit p99 | 248.053 ms |
| Loaded commit p95 / p99 | 323.561 ms / 465.786 ms |
| Block gap p99 / maximum | 3.317 s / 3.328 s |
| Average process CPU | 0.973 cores |
| Sampled process RSS peak | 120 MiB |
| Post-GC heap change | −6,785,544 bytes |
| Peak pool | 6 entries / 21,618 bytes |
| Peak reserved block gas | 5,000,001 / 10,000,000 |

Six valid admissions were evicted during deliberate pool saturation; admission
is not a promise of inclusion. The test does not count them as confirmed or
claim to have tested their resubmission. Both nodes ended with identical state,
no halt and empty pools after draining.

The separate unchanged direct-mempool benchmark was also run for 30 seconds
with 12 tight-loop senders and GOMAXPROCS=2. It **failed** its accelerated 20-ms
cadence gate: 421 blocks versus a required minimum of 750. That result is not
converted into a pass, nor evidence of a mainnet stall: it exercises a different,
unthrottled admission and scheduling envelope. Real-P2P/process-level saturation
and diverse-module compilation-cache churn remain outside this trial.
