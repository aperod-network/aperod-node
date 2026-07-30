package p2p

// Regression tests for the P2P block wire codec.
// These live in package p2p (not p2p_test) so they can access the
// unexported blockToMsg / msgToBlock helpers.

import (
	"testing"

	"github.com/aperod/aperod/core"
)

// TestBlockToMsg_OraclePriceAndBaseFee_RoundTrip ensures OraclePrice and
// BaseFee survive the blockToMsg → msgToBlock round-trip so the hash a
// receiving peer recomputes matches the hash the producer signed.
//
// Regression test for: P2P block codec drops OraclePrice and BaseFee —
// peer VerifySignature fails (finding #1, @DingXieee, 2026-07-30).
func TestBlockToMsg_OraclePriceAndBaseFee_RoundTrip(t *testing.T) {
	original := &core.Block{
		Header: core.BlockHeader{
			Height:      42,
			Timestamp:   1_700_000_000,
			Round:       42,
			OraclePrice: 1_234_567,
			BaseFee:     500,
		},
	}

	sb := blockToMsg(original)
	if sb.Header.OraclePrice != original.Header.OraclePrice {
		t.Fatalf("OraclePrice lost in blockToMsg: got %d, want %d",
			sb.Header.OraclePrice, original.Header.OraclePrice)
	}
	if sb.Header.BaseFee != original.Header.BaseFee {
		t.Fatalf("BaseFee lost in blockToMsg: got %d, want %d",
			sb.Header.BaseFee, original.Header.BaseFee)
	}

	restored := msgToBlock(sb)
	if restored.Header.OraclePrice != original.Header.OraclePrice {
		t.Fatalf("OraclePrice lost in msgToBlock: got %d, want %d",
			restored.Header.OraclePrice, original.Header.OraclePrice)
	}
	if restored.Header.BaseFee != original.Header.BaseFee {
		t.Fatalf("BaseFee lost in msgToBlock: got %d, want %d",
			restored.Header.BaseFee, original.Header.BaseFee)
	}

	// The restored block must hash identically to the original —
	// this is exactly what VerifySignature checks on the receiving peer.
	if original.Hash() != restored.Hash() {
		t.Fatalf("hash mismatch after round-trip: producer=%x peer=%x",
			original.Hash(), restored.Hash())
	}
}
