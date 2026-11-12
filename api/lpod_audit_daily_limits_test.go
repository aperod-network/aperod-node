package api

import "testing"

func TestDailyAuditBlockLimitRemainsFortyThousand(t *testing.T) {
	if lpodDailyMaxBlocks != 40_000 {
		t.Fatalf("daily audit block limit = %d, want 40000", lpodDailyMaxBlocks)
	}
}
