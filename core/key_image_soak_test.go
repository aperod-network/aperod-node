//go:build soak

package core

// key_image_soak_test.go — memory-ceiling regression test for compactKeyImageSet.
//
// Build tag: soak (excluded from regular "go test ./..." runs).
//
// Run with:
//
//	go test -tags soak -run ^TestKeyImageSet_MemoryCeiling_1M$ -v ./core/
//
// The test inserts 1 M synthetic key images into a compactKeyImageSet and
// asserts that heap growth stays below 50 MiB.
//
// Theory of operation:
//
//	compactKeyImageSet stores key images as a sorted []crypto.KeyImage slice.
//	Each entry occupies exactly 32 bytes — the raw byte footprint of the
//	crypto.KeyImage type.  A Go map[[32]byte]struct{} consumes ~150 bytes per
//	entry (bucket headers, alignment, pointer overhead), making the sorted
//	slice ~4–5× more compact.
//
//	At 1 M entries:
//	  • Sorted slice  :  1 M × 32 B  ≈  32 MiB   (well within the 50 MiB limit)
//	  • Go map (old)  :  1 M × 150 B ≈ 143 MiB   (would exceed the limit)
//
//	This asymmetry means the test reliably catches a regression to map storage
//	while still leaving comfortable headroom for GC hysteresis and slice
//	over-allocation (Go's append growth factor is 1.25× above 256 entries,
//	giving at most ~1.25 × 32 MiB ≈ 40 MiB worst-case — still under 50 MiB).
//
// Key-image generation strategy:
//
//	Keys are constructed deterministically: bytes 0–7 hold the big-endian
//	uint64 index; bytes 8–31 are zero.  Big-endian encoding produces byte
//	strings in lexicographically ascending order (index 0 < index 1 < …),
//	so the in-place sort shift inside compactKeyImageSet.insert copies zero
//	bytes per call — each insertion appends to the tail of the slice and the
//	loop completes in under one second.
//
// Companion test: TestUTXOSet_MemoryGrowth_10KBlocks in utxo_soak_test.go
// verifies the ApplyBlock OOM regression (unbounded s.utxos accumulation).
// This test focuses specifically on the key-image slice's own heap budget.

import (
	"encoding/binary"
	"runtime"
	"testing"

	"github.com/aperod/aperod/crypto"
)

// kiSoakHeapInuseMiB forces a full GC cycle and returns the live heap in MiB.
// Using HeapInuse (bytes in in-use spans) rather than HeapAlloc (live objects
// only) gives a more conservative estimate that tracks closer to RSS.
func kiSoakHeapInuseMiB() float64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return float64(ms.HeapInuse) / (1024 * 1024)
}

// syntheticKeyImage generates a unique, deterministic 32-byte key image from
// an index.  The result is NOT a valid Ed25519 point, which is intentional:
// this test exercises the raw sorted-slice storage, not the Ed25519
// canonicalisation path.
//
// Big-endian encoding guarantees that sequential indices produce byte strings
// in lexicographically ascending order.  compactKeyImageSet sorts entries in
// the same order, so every insertion appends to the end of the slice and the
// binary-search + copy-tail cost is O(log n) + O(1) per call rather than
// O(log n) + O(n).
func syntheticKeyImage(i uint64) crypto.KeyImage {
	var ki crypto.KeyImage
	binary.BigEndian.PutUint64(ki[0:8], i)
	// bytes 8–31 remain zero; the unique prefix in bytes 0–7 is sufficient
	// to distinguish all 1 M entries and preserves ascending order.
	return ki
}

// TestKeyImageSet_MemoryCeiling_1M is the primary key-image memory assertion.
//
// It guards the memory-compaction fix (map → compact sorted slice): a
// regression to map[KeyImage]struct{} would consume ~143 MiB for 1 M entries,
// well above the 50 MiB limit, and the test would fail.
//
// The test also spot-checks contains() and the absence sentinel to confirm
// data integrity is not silently broken alongside any storage change.
func TestKeyImageSet_MemoryCeiling_1M(t *testing.T) {
	const numImages = 1_000_000
	const maxHeapGrowthMiB = 50.0

	// Take the baseline BEFORE allocating the set so GC-settled overhead from
	// the test harness itself is excluded from the growth measurement.
	baseline := kiSoakHeapInuseMiB()
	t.Logf("baseline heap: %.1f MiB", baseline)

	var s compactKeyImageSet

	// Insert 1 M synthetic key images in ascending byte order so every
	// insertion appends to the tail — O(log n) binary search, O(1) copy.
	for i := uint64(0); i < numImages; i++ {
		s.insert(syntheticKeyImage(i))
	}

	finalHeap := kiSoakHeapInuseMiB()
	growth := finalHeap - baseline

	expectedMiB := float64(numImages*32) / (1024 * 1024)
	mapEquivMiB := float64(numImages*150) / (1024 * 1024)

	t.Logf("after %d inserts:", numImages)
	t.Logf("  heap:             %.1f MiB  (growth %.1f MiB, limit %.0f MiB)",
		finalHeap, growth, maxHeapGrowthMiB)
	t.Logf("  set entries:      %d  (expected %d)", s.length(), numImages)
	t.Logf("  expected (slice): ≈%.1f MiB  (32 B × %d entries)", expectedMiB, numImages)
	t.Logf("  map equivalent:   ≈%.0f MiB  (~150 B × %d entries, old path)", mapEquivMiB, numImages)

	// Count sanity: every inserted key must be present.
	if s.length() != numImages {
		t.Errorf("compactKeyImageSet: got %d entries, expected %d", s.length(), numImages)
	}

	// Spot-check a handful of lookups spread across the range.
	for _, idx := range []uint64{0, 1, 499_999, 999_999} {
		if !s.contains(syntheticKeyImage(idx)) {
			t.Errorf("contains(syntheticKeyImage(%d)) returned false — data integrity failure", idx)
		}
	}
	// A key one past the inserted range must not be present.
	if s.contains(syntheticKeyImage(numImages)) {
		t.Error("contains(out-of-range key) returned true — false positive in binary search")
	}

	// Primary memory assertion: a map regression would push growth past 143 MiB.
	if growth > maxHeapGrowthMiB {
		t.Errorf(
			"heap grew %.1f MiB for %d key images — exceeds %.0f MiB limit\n"+
				"  Sorted-slice storage (32 B/entry) should use ≈%.1f MiB.\n"+
				"  A regression to Go map storage (~150 B/entry) would use ≈%.0f MiB.\n"+
				"  Verify that compactKeyImageSet.sorted is a []crypto.KeyImage slice\n"+
				"  and not a map[crypto.KeyImage]struct{}.",
			growth, numImages, maxHeapGrowthMiB, expectedMiB, mapEquivMiB)
	}
}
