package main

// keyImageIndexTrusted is the FAIL-CLOSED gate for the startup key-image
// fast path (Task: use the key-image index at startup instead of scanning
// all blocks).
//
// The k/ LevelDB index is maintained incrementally by MarkKeyImageSpent on
// every accepted block, so on a healthy node it is complete and the startup
// scan can be skipped entirely — restarts take seconds instead of the
// minutes required to read 800K+ raw blocks.
//
// The index must NOT be trusted when:
//   - iterating it failed (iterErr != nil): the index may be truncated by
//     a corrupt SST file (e.g. after an OOM kill);
//   - it is empty while the chain has blocks (kiCount == 0 && tipHeight > 0):
//     an empty index on a non-genesis chain means the DB was written by an
//     older binary that never populated k/ — trusting it would let every
//     historical key image be double-spent.
//
// In both cases the caller falls back to the full block scan, which rebuilds
// the set from the raw blocks (slow but always correct).  A genesis chain
// (tipHeight == 0) legitimately has zero key images and is trusted.
//
// Note: an all-coinbase chain (blocks exist but no inputs were ever spent)
// also has zero key images and will take the scan fallback.  That is an
// accepted cost: the scan is correct, and distinguishing "legitimately
// empty" from "never populated" would require a marker the older binaries
// never wrote.
func keyImageIndexTrusted(iterErr error, kiCount int, tipHeight uint64) bool {
	if iterErr != nil {
		return false
	}
	return kiCount > 0 || tipHeight == 0
}
