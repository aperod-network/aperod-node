package consensus

import (
	"testing"

	"github.com/aperod/aperod/core"
)

func TestBlockFeeStatsBurnDoesNotBecomeValidatorTip(t *testing.T) {
	tx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
		Fee:     0,
		Extra:   core.IntentionalBurnExtra(50),
	}
	base := uint64(2)
	minimum := tx.MinFeeAt(base)
	tx.Fee = minimum + 50 + 7
	burned, tips := blockFeeStats([]core.Transaction{tx}, base)
	if burned != tx.Fee+50 {
		t.Fatalf("burned = %d, want %d", burned, tx.Fee+50)
	}
	if tips != 0 {
		t.Fatalf("tips = %d, want 0", tips)
	}
}
