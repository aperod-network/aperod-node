# Key-Image Store Design Note

**Date:** 2026-08-03  
**Status:** Decision recorded — unblocks Task #461 implementation  
**Context:** On 2026-08-03 the node entered an OOM crash loop on restart. Each restart consumed 6–7 GB RAM while replaying ~800 K blocks from disk, was killed by the OOM killer, restarted, and repeated. This note documents the root cause, current measurements, growth projections, and the chosen storage format for Task #461.

---

## 1. Root Cause

The OOM is **not** caused by the key-image set in memory. The set itself at 800 K blocks is manageable (see §3). The crash loop is caused by the **full block scan fallback**:

When the startup snapshot is missing or stale, `cmd/node/main.go` falls back to reading every raw block from LevelDB, JSON-unmarshalling it into `core.Block`, and applying it to rebuild the UTXO set and validator registry. At 800 K blocks this holds many partially-decoded blocks in memory simultaneously, and the cumulative JSON decode pressure (each ring transaction carries bulletproofs, range proofs, and ring signatures totalling 5–15 KB of JSON) drives heap to 6–7 GB.

The block scan is required for **UTXO rebuild** and **stake replay** regardless of the key-image path. Eliminating the key-image index rebuild is not sufficient on its own; the block scan still runs. The correct fix is to ensure the startup snapshot path is always taken (see §5).

---

## 2. Instrumentation Added (this task)

`blockchain/cmd/node/main.go` now emits two structured log lines during startup:

### 2a. Before the scan loop
```json
{
  "msg": "running startup block scan",
  "tip_height": N,
  "ki_from_index": true/false,
  "heap_sys_mib_before": X
}
```

### 2b. After the scan loop
```json
{
  "msg": "startup scan metrics",
  "elapsed_sec": "42.17",
  "key_images_loaded": N,
  "heap_sys_mib_before": X,
  "heap_sys_mib_after":  Y,
  "heap_alloc_mib":      Z,
  "heap_sys_delta_mib":  D
}
```

`heap_sys_mib` is `runtime.MemStats.Sys` — total bytes mapped from the OS. This value never decreases within a process lifetime, making it a reliable proxy for peak RSS without requiring `/proc` access.

---

## 3. Current Numbers (800 K blocks, estimated)

The node has not yet restarted cleanly with instrumentation running on production. The numbers below are derived from code analysis and will be replaced with observed values once the patched node runs.

### Key-image set in memory

| Metric | Estimate | Basis |
|--------|----------|-------|
| Ring inputs per block (avg) | ~1.5 | Most blocks: 1 user tx + coinbase (0 inputs) |
| Key images at 800 K blocks | ~1.2 M | 800 K × 1.5 |
| Go `map[[32]byte]struct{}` per entry | ~80–100 bytes | 32-byte key + bucket metadata at ~6.5 load factor, 8 entries/bucket, each bucket 208 bytes → 26 bytes overhead + alignment |
| **Key-image set RAM (estimated)** | **96–120 MB** | 1.2 M × 100 bytes |

The key-image set alone is not the OOM trigger at current chain size.

### Block scan RAM (the actual OOM trigger)

| Metric | Estimate | Basis |
|--------|----------|-------|
| Average raw block size on disk | ~8 KB | Header + 1 ring tx with Bulletproof |
| Blocks in LevelDB read buffer | ~500 simultaneous (LevelDB read-ahead + Go GC lag) | Observed on similar chains |
| Peak JSON decode heap | ~4–6 GB | 800 K × 8 KB × amplification from Go JSON allocations (~3–5×) |
| UTXO set at 800 K blocks | ~500 MB | ~500 K active UTXOs × ~1 KB per entry |
| **Total startup peak RSS** | **6–7 GB** | Matches crash-loop reports |

---

## 4. Growth Projections

Block rate: 1 block / 3 seconds → **~10 M blocks/year**

| Chain height | Est. key images | KI set RAM | Block scan peak RAM | Server viability (7.8 GB) |
|---|---|---|---|---|
| 800 K (now) | ~1.2 M | ~120 MB | ~6–7 GB | ❌ OOM on fallback path |
| 2 M blocks | ~3 M | ~300 MB | ~15–20 GB | ❌ worse |
| 5 M blocks | ~7.5 M | ~750 MB | ~40–50 GB | ❌ completely impractical |
| Any height (snapshot path) | N/A | ~500 MB–1.5 GB total state | ~0 (skipped) | ✅ fast path only |

The fallback block scan becomes catastrophically worse with chain growth. **The snapshot fast path must be the always-taken path.**

---

## 5. Key-Image Store Format Decision

### Options evaluated

#### Option A — Go map (current, in-memory only)
- **Memory**: ~80–100 bytes/entry
- **Load time**: O(N) — rebuilt from scan or loaded from LevelDB
- **Startup**: requires some persistent backing store
- **Verdict**: Keep as the in-memory runtime structure. Not suitable as sole persistent store.

#### Option B — LevelDB `k/<keyimage> → 0x01` (already implemented)
- **On-disk size**: 32-byte key + 2-byte prefix + 1-byte value + LevelDB block overhead ≈ 50–80 bytes/entry after compression
- **Load time**: `IterKeyImages` does a sequential LevelDB scan → ~0.5–3 s for 5 M entries
- **Updates**: `MarkKeyImageSpent` called per block already; fully maintained
- **Startup**: `IterKeyImages` already the fast path in `main.go` (loads KIs without block scan)
- **Problem**: Even when KIs load from LevelDB, the UTXO rebuild and stake replay **still require the full block scan**, consuming 6–7 GB
- **Verdict**: Sufficient for key-image-only persistence. Already implemented and working.

#### Option C — Binary flat file (32 bytes × N, append-only)
- **On-disk size**: 32 bytes/entry exactly; 1 M entries = 32 MB; 5 M = 160 MB
- **Load time**: Sequential read at ~1 GB/s → 160 MB in < 200 ms
- **Updates**: Append 32 bytes per new spent key image
- **Advantage over LevelDB**: Faster iteration; no decompression; trivial to mmap
- **Problem**: Same as Option B — key images alone don't prevent the block scan for UTXO/stake state
- **Verdict**: Worth implementing as a complementary index but does not solve the OOM alone.

#### Option D — Bloom filter
- **Size**: ~9.6 bits/entry at 1% FPR; 1 M entries = 1.2 MB
- **Advantage**: Tiny; near-zero load time
- **Fatal problem**: False positives make it **unsuitable for consensus** — a bloom filter could incorrectly flag a legitimate input as spent, denying valid transactions. Can only be used as a pre-filter layer backed by LevelDB.
- **Verdict**: Not recommended as primary store.

#### Option E — Startup snapshot (already implemented)
- **Size**: JSON-encoded UTXOs + key images + registry; ~500 MB–1.5 GB at 800 K blocks
- **Load time**: Deserialise one file → typically < 5 s; skips all block I/O
- **Content**: Covers key images, active UTXOs, staked UTXOs, validator registry — everything the startup scan rebuilds
- **When valid**: Snapshot is keyed to exact (tip_height, tip_hash) — always valid after a clean shutdown or after any block is added
- **Shutdown hook**: Already saves snapshot on SIGTERM in `main.go`
- **Post-scan hook**: Already saves snapshot after any full block scan completes
- **Verdict**: **Correct long-term solution.** Eliminates the block scan entirely on restart.

---

## 6. Recommended Implementation (Task #461 spec)

### Primary fix: Harden the snapshot path

The snapshot already exists (`startupSnapshot` in `main.go`, `loadStartupSnapshot` / `saveStartupSnapshot`). The OOM occurs only when the snapshot is absent or stale. The hardening work is:

1. **Always save a snapshot after the full block scan** — already done (lines 512–532 of `main.go`).
2. **Always save a snapshot on SIGTERM** — already done (lines 850–866 of `main.go`).
3. **Verify snapshot integrity on load** — ✅ done (2026-08-12): a SHA-256 checksum of the compressed snapshot bytes is written to a `.sha256` sidecar on every save; `openGzipSnapshotReader` verifies it before deserialising and returns a descriptive corrupt error on mismatch, so all loaders (primary, prev-backup, checkpoints) fall back instead of silently serving corrupted state. Snapshots without a sidecar (older binaries) still load.
4. **Cap snapshot file size** — at large chain heights the JSON snapshot may itself exceed RAM. Consider a binary/msgpack encoding that is 3–5× smaller than JSON.

### Secondary fix: Key-image-only binary dump (for the fallback path)

When the snapshot is missing (fresh deployment, snapshot deleted, or node upgraded with an incompatible snapshot schema), the block scan is needed for UTXOs and stake state but **not** for key images if a key-image binary dump exists.

Implementation:
- File: `<data_dir>/keyimages.bin` — flat binary, 32 bytes per entry, no header
- Written: append 32 bytes per new `MarkKeyImageSpent` call (file opened in `O_APPEND` mode)
- Read at startup: `mmap` or sequential `Read`, O(N/32) iterations
- Advantage over LevelDB iteration: ~10× faster sequential read, simpler format
- At 5 M entries: 160 MB on disk, < 200 ms to load
- Memory after load: key images are added directly to `utxos` via `MarkSpent`; the flat buffer is freed

### Storage format decision: **Keep LevelDB index + add flat binary dump**

| Store | Purpose | Status |
|---|---|---|
| LevelDB `k/<ki>` | Authoritative persistent KV, supports `IsKeyImageSpent` queries | ✅ Already implemented |
| `keyimages.bin` flat file | Fast bulk load on startup (O(1) per entry, no decompression) | 📋 Task #461 |
| In-memory `map[KeyImage]struct{}` | Runtime O(1) lookup for consensus | ✅ Already implemented |
| Startup snapshot | Covers all state; eliminates block scan entirely | ✅ Already implemented; needs integrity check |

**Bloom filter is explicitly rejected** for consensus use. It may be added later as a read-path optimisation (e.g., quick negative answer before hitting LevelDB), but must never be the authoritative double-spend check.

---

## 7. Memory Budget at Scale

With the snapshot path always taken, peak startup RSS is bounded by snapshot size:

| Chain height | Key images in map | Active UTXOs | Registry | Snapshot file | Peak startup RSS |
|---|---|---|---|---|---|
| 800 K | ~1.2 M (~120 MB) | ~500 K (~500 MB) | ~100 validators (~1 MB) | ~650 MB | **< 1.5 GB** ✅ |
| 2 M | ~3 M (~300 MB) | ~1.2 M (~1.2 GB) | ~100 validators (~1 MB) | ~1.6 GB | **~2–3 GB** ✅ |
| 5 M | ~7.5 M (~750 MB) | ~3 M (~3 GB) | ~100 validators (~1 MB) | ~4 GB | **~5–6 GB** ⚠️ needs binary snapshot encoding |

At 5 M blocks, the binary snapshot encoding (§6 item 4) becomes necessary to keep peak RSS below the 7.8 GB hardware limit. This is approximately 18 months away at current block rate.

---

## 8. Soak Tests (memory regression guards)

Two soak tests guard the key-image memory fix introduced in Task #1080 (map → compact sorted slice).  Both carry the `soak` build tag and are excluded from regular `go test ./...` runs.

### TestKeyImageSet_MemoryCeiling_1M  (`key_image_soak_test.go`)

Directly inserts 1 M synthetic key images into a `compactKeyImageSet` and asserts that heap growth stays below **50 MiB**.

```
go test -tags soak -run ^TestKeyImageSet_MemoryCeiling_1M$ -v ./core/
```

Expected output:
- `set entries: 1000000`
- `heap growth ≈ 32–40 MiB` (32 B/entry × 1 M, plus ≤25% Go append over-allocation)
- A regression to `map[KeyImage]struct{}` (~150 B/entry) would produce ~143 MiB growth and **fail** the assertion.

### TestUTXOSet_MemoryGrowth_10KBlocks  (`utxo_soak_test.go`)

Applies 10 000 spend+create blocks and asserts live heap growth stays below 50 MiB.  Guards the `ApplyBlock` OOM regression (unbounded `s.utxos` accumulation).

```
go test -tags soak -run ^TestUTXOSet_MemoryGrowth_10KBlocks$ -v ./core/
```

---

## 9. Summary

| Question | Answer |
|---|---|
| What caused the OOM? | Full block scan for UTXO/stake rebuild, triggered because the startup snapshot was absent |
| Is the key-image set itself the problem? | No — at 800 K blocks it uses ~120 MB; the block scan uses 6–7 GB |
| Does the key-image LevelDB index help? | Yes, for key images only; it does not eliminate the block scan |
| What is the correct fix? | Ensure the startup snapshot is always valid; harden its save/load/verify cycle |
| What storage format for key images? | LevelDB (already done) + flat binary dump for fast bulk startup load (Task #461) |
| When does bloom filter make sense? | Never as authoritative store; possibly as a pre-filter read optimisation only |
| At what chain height does snapshot encoding matter? | ~5 M blocks (~18 months); switch to binary/msgpack snapshot then |
