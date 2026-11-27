package api

import (
	"encoding/json"
	"math"
	"sync/atomic"
	"testing"
)

func TestHeightTelemetry_KeepsUnsignedValues(t *testing.T) {
	var s Server
	s.SetSyncProgress(math.MaxUint64-1, math.MaxUint64)
	s.SetStoreMissingBlocks(math.MaxUint64 - 2)
	s.SetStoreMissingFirstBlock(math.MaxUint64 - 1)
	s.SetStoreMissingLastBlock(math.MaxUint64)
	if s.SyncingHeight() != math.MaxUint64-1 || s.TipHeight() != math.MaxUint64 ||
		atomic.LoadUint64(&s.storeMissingBlocks) != math.MaxUint64-2 ||
		atomic.LoadUint64(&s.storeMissingFirstBlock) != math.MaxUint64-1 ||
		atomic.LoadUint64(&s.storeMissingLastBlock) != math.MaxUint64 {
		t.Fatal("height telemetry narrowed an unsigned value")
	}
}

func TestUnbondTime_RemainsANumberWithoutOverflow(t *testing.T) {
	for _, tc := range []struct {
		now    int64
		blocks uint64
		want   string
	}{
		{1700000000000, 0, "1700000000000"},
		{1700000000000, 3, "1700000003000"},
		{0, math.MaxUint64, "18446744073709551615000"},
		{math.MaxInt64, 1, "9223372036854776807"},
	} {
		got := estimatedUnbondTime(tc.now, tc.blocks)
		raw, err := json.Marshal(got)
		if err != nil || string(raw) != tc.want {
			t.Fatalf("numeric unbond estimate: %s %v want %s", raw, err, tc.want)
		}
	}
}
