package consensus

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
)

func TestValidateBlockEconomicsRequiresAVMGasPayment(t *testing.T) {
	engine := NewEngine(
		Config{RingCTV4ActivationHeight: 1},
		core.NewChain(10),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	tx := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: 1_000},
		Inputs:  []core.RingInput{{}},
	}
	baseFee := core.InitialBaseFeePerByte
	tx.Fee = tx.MinFeeAt(baseFee)
	block := &core.Block{
		Header: core.BlockHeader{Height: 1, BaseFee: baseFee},
		Txs:    []core.Transaction{tx},
	}
	if err := engine.validateBlockEconomics(block); err == nil || !strings.Contains(err.Error(), "fee below") {
		t.Fatalf("underpaid AVM gas accepted: %v", err)
	}
	gasFee, err := core.AVMGasFee(tx.AVM.GasLimit)
	if err != nil {
		t.Fatalf("AVMGasFee: %v", err)
	}
	block.Txs[0].Fee += gasFee
	if err := engine.validateBlockEconomics(block); err != nil {
		t.Fatalf("fully paid AVM gas rejected: %v", err)
	}
}

func TestValidateBlockEconomicsEnforcesAVMBlockGasLimit(t *testing.T) {
	engine := NewEngine(
		Config{RingCTV4ActivationHeight: 1},
		core.NewChain(10),
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	baseFee := core.InitialBaseFeePerByte
	first := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: 6_000_000},
		Inputs:  []core.RingInput{{}},
	}
	second := core.Transaction{
		Version: core.TxVersionAVM,
		AVM:     &core.AVMPayload{GasLimit: 6_000_000},
		Inputs:  []core.RingInput{{}},
	}
	for _, tx := range []*core.Transaction{&first, &second} {
		gasFee, err := core.AVMGasFee(tx.AVM.GasLimit)
		if err != nil {
			t.Fatalf("AVMGasFee: %v", err)
		}
		tx.Fee = tx.MinFeeAt(baseFee) + gasFee
	}
	block := &core.Block{
		Header: core.BlockHeader{Height: 1, BaseFee: baseFee},
		Txs:    []core.Transaction{first, second},
	}
	if err := engine.validateBlockEconomics(block); err == nil || !strings.Contains(err.Error(), "block gas limit") {
		t.Fatalf("over-gas block accepted: %v", err)
	}
}
