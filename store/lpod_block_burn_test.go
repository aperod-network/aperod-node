// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package store

import (
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
)

func blockBurnTx(extra []byte) core.Transaction {
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs:  []core.RingInput{{}},
		Extra:   extra,
	}
}

func TestLPoDBlockBurnComponentsActivationFeesTipsAndIntentional(t *testing.T) {
	const baseFee = uint64(7)
	intentional := blockBurnTx(core.IntentionalBurnExtra(19))
	intentionalMinimum := baseFee * uint64(intentional.Size())
	intentional.Fee = intentionalMinimum + 11 // excess is a validator tip, not a burn

	underpaid := blockBurnTx(nil)
	underpaid.Fee = 5

	avm := core.Transaction{
		Version: core.TxVersionAVM,
		Inputs:  []core.RingInput{{}},
		AVM:     &core.AVMPayload{GasLimit: 2},
	}
	avmMinimum := baseFee * uint64(avm.Size())
	avm.Fee = avmMinimum + 13

	block := &core.Block{
		Header: core.BlockHeader{Height: 20, BaseFee: baseFee},
		Txs: []core.Transaction{
			{Version: core.TxVersionBase, Fee: math.MaxUint64},  // coinbase fee is excluded
			{Version: core.TxVersionStake, Fee: math.MaxUint64}, // stake fee is excluded
			intentional, underpaid, avm,
		},
	}
	preActivation, err := deriveLPoDBlockBurns(block, 21)
	if err != nil {
		t.Fatal(err)
	}
	wantProtocol := intentionalMinimum + underpaid.Fee + avmMinimum
	if preActivation.ProtocolBaseFee != wantProtocol ||
		preActivation.SignedIntentional != 19 || preActivation.AVMGas != 0 ||
		preActivation.Total != wantProtocol+19 {
		t.Fatalf("pre-activation burn mismatch: got %+v protocol=%d", preActivation, wantProtocol)
	}

	block.Header.Height = 21
	postActivation, err := deriveLPoDBlockBurns(block, 21)
	if err != nil {
		t.Fatal(err)
	}
	gasBurn, err := core.AVMGasFee(2)
	if err != nil {
		t.Fatal(err)
	}
	if postActivation.ProtocolBaseFee != wantProtocol ||
		postActivation.SignedIntentional != 19 || postActivation.AVMGas != gasBurn ||
		postActivation.Total != wantProtocol+19+gasBurn {
		t.Fatalf("post-activation burn mismatch: got %+v protocol=%d gas=%d", postActivation, wantProtocol, gasBurn)
	}
	encoded, err := json.Marshal(LPoDBlockAudit{
		ProtocolBaseFeeBurnNAPRO:   auditAmountPointer(postActivation.ProtocolBaseFee),
		SignedIntentionalBurnNAPRO: auditAmountPointer(postActivation.SignedIntentional),
		AVMGasBurnNAPRO:            auditAmountPointer(postActivation.AVMGas),
		TotalBurnNAPRO:             auditAmountPointer(postActivation.Total),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"protocol_base_fee_burn_napro":"` + new(big.Int).SetUint64(wantProtocol).String() + `"`,
		`"signed_intentional_burn_napro":"19"`,
		`"avm_gas_burn_napro":"` + new(big.Int).SetUint64(gasBurn).String() + `"`,
		`"total_burn_napro":"` + new(big.Int).SetUint64(postActivation.Total).String() + `"`,
	} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("monetary burn field is not a decimal JSON string: %s", encoded)
		}
	}
}

func TestLPoDBlockBurnZeroHeaderMalformedAndUnavailableInputs(t *testing.T) {
	feePaying := blockBurnTx(nil)
	feePaying.Fee = 1
	if _, err := deriveLPoDBlockBurns(&core.Block{Txs: []core.Transaction{feePaying}}, 0); err == nil {
		t.Fatal("zero-header fee-paying block was treated as zero burn")
	}

	zeroFee := &core.Block{Txs: []core.Transaction{
		{Version: core.TxVersionBase, Fee: math.MaxUint64},
		{Version: core.TxVersionStake, Fee: math.MaxUint64},
	}}
	zero, err := deriveLPoDBlockBurns(zeroFee, 0)
	if err != nil || zero != (lpodBlockBurns{}) {
		t.Fatalf("zero-header block without fee-paying transactions: burns=%+v err=%v", zero, err)
	}

	malformedMarker := blockBurnTx(append(core.IntentionalBurnExtra(1), 0))
	if _, err := deriveLPoDBlockBurns(&core.Block{Header: core.BlockHeader{BaseFee: 1}, Txs: []core.Transaction{malformedMarker}}, 0); err == nil {
		t.Fatal("malformed intentional burn marker was silently omitted")
	}
	missingAVM := core.Transaction{Version: core.TxVersionAVM, Inputs: []core.RingInput{{}}}
	if _, err := deriveLPoDBlockBurns(&core.Block{Header: core.BlockHeader{Height: 5, BaseFee: 1}, Txs: []core.Transaction{missingAVM}}, 5); err == nil {
		t.Fatal("missing activated AVM payload was accepted")
	}
	overflowAVM := core.Transaction{Version: core.TxVersionAVM, Inputs: []core.RingInput{{}},
		AVM: &core.AVMPayload{GasLimit: math.MaxUint64}}
	if _, err := deriveLPoDBlockBurns(&core.Block{Header: core.BlockHeader{Height: 5, BaseFee: 1}, Txs: []core.Transaction{overflowAVM}}, 5); err == nil {
		t.Fatal("overflowing AVM gas fee was accepted")
	}

	pruned, err := json.Marshal(StoredBlock{Height: 5, TxCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeLPoDAuditBlock(pruned); err == nil {
		t.Fatal("pruned block body decoded as full burn evidence")
	}
}

func TestLPoDBlockBurnAggregationOverflowFailsClosed(t *testing.T) {
	txA, txB := blockBurnTx(nil), blockBurnTx(nil)
	txA.Fee, txB.Fee = math.MaxUint64, math.MaxUint64
	block := &core.Block{
		Header: core.BlockHeader{BaseFee: math.MaxUint64},
		Txs:    []core.Transaction{txA, txB},
	}
	if _, err := deriveLPoDBlockBurns(block, 0); err == nil {
		t.Fatal("overflowing aggregate burn silently wrapped")
	}

	intentional := blockBurnTx(core.IntentionalBurnExtra(math.MaxUint64))
	intentional.Fee = math.MaxUint64
	block = &core.Block{Header: core.BlockHeader{BaseFee: 1}, Txs: []core.Transaction{intentional}}
	if _, err := deriveLPoDBlockBurns(block, 0); err == nil {
		t.Fatal("overflowing total burn silently wrapped")
	}
}
