// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"encoding/json"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestSetAVMGasBurnActivationHeightUpdatesRESTAndWebSocketConfig(t *testing.T) {
	server := NewServer(":0", core.NewChain(), nil, nil, slog.Default())
	server.SetAVMGasBurnActivationHeight(27)
	if server.avmGasBurnActivationHeight != 27 || server.hub.avmGasBurnActivationHeight != 27 {
		t.Fatalf("activation height not propagated: REST=%d websocket=%d",
			server.avmGasBurnActivationHeight, server.hub.avmGasBurnActivationHeight)
	}
}

func responseString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func TestBlockToResponseBurnBreakdown(t *testing.T) {
	tx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
		Fee:     math.MaxUint64,
		Extra:   core.IntentionalBurnExtra(37),
	}
	block := &core.Block{Header: core.BlockHeader{BaseFee: 10}, Txs: []core.Transaction{tx}}

	response, err := blockToResponse(block, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantProtocol := new(big.Int).Mul(big.NewInt(10), big.NewInt(int64(tx.Size()))).String()
	wantTotal, ok := new(big.Int).SetString(wantProtocol, 10)
	if !ok {
		t.Fatal("could not parse expected protocol burn")
	}
	wantTotal.Add(wantTotal, big.NewInt(37))
	if responseString(response.ProtocolFeeBurnedNAPRO) != wantProtocol ||
		responseString(response.IntentionalBurnNAPRO) != "37" ||
		responseString(response.FeesBurnedNAPRO) != wantTotal.String() {
		t.Fatalf("burn breakdown: protocol=%s intentional=%s total=%s; want %s, 37, %s",
			responseString(response.ProtocolFeeBurnedNAPRO), responseString(response.IntentionalBurnNAPRO), responseString(response.FeesBurnedNAPRO),
			wantProtocol, wantTotal.String())
	}
}

func TestBlockToResponseBurnOverflowIsExact(t *testing.T) {
	tx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
		Fee:     math.MaxUint64,
		Extra:   core.IntentionalBurnExtra(math.MaxUint64),
	}
	block := &core.Block{Header: core.BlockHeader{BaseFee: 1}, Txs: []core.Transaction{tx, tx}}
	response, err := blockToResponse(block, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantIntentional := new(big.Int).Mul(new(big.Int).SetUint64(math.MaxUint64), big.NewInt(2)).String()
	wantProtocol := new(big.Int).Mul(big.NewInt(int64(tx.Size())), big.NewInt(2)).String()
	wantTotal := new(big.Int).Add(new(big.Int).SetUint64(math.MaxUint64), new(big.Int).SetUint64(math.MaxUint64))
	wantTotal.Add(wantTotal, new(big.Int).Mul(big.NewInt(int64(tx.Size())), big.NewInt(2)))
	if responseString(response.IntentionalBurnNAPRO) != wantIntentional || responseString(response.ProtocolFeeBurnedNAPRO) != wantProtocol ||
		responseString(response.FeesBurnedNAPRO) != wantTotal.String() {
		t.Fatalf("overflowing burn was not exact: protocol=%s intentional=%s total=%s; want %s, %s, %s",
			responseString(response.ProtocolFeeBurnedNAPRO), responseString(response.IntentionalBurnNAPRO), responseString(response.FeesBurnedNAPRO),
			wantProtocol, wantIntentional, wantTotal.String())
	}
}

func TestBlockToResponseRejectsMalformedBurnMarker(t *testing.T) {
	tx := core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
		Fee:     math.MaxUint64,
		Extra:   append(core.IntentionalBurnExtra(1), 0),
	}
	if _, err := blockToResponse(&core.Block{Txs: []core.Transaction{tx}}, 0); err == nil {
		t.Fatal("malformed intentional burn marker was silently omitted")
	}
}

func TestBlockToResponseZeroBurnIsAvailable(t *testing.T) {
	response, err := blockToResponse(&core.Block{
		Txs: []core.Transaction{core.CoinbaseTx(crypto.Point32{}, 1)},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if responseString(response.FeesBurnedNAPRO) != "0" || responseString(response.ProtocolFeeBurnedNAPRO) != "0" ||
		responseString(response.IntentionalBurnNAPRO) != "0" || responseString(response.AVMGasBurnedNAPRO) != "0" {
		t.Fatalf("valid zero-burn response must expose exact zeroes: %#v", response)
	}
}

func TestBlockToResponseZeroHeaderFeeFailsClosed(t *testing.T) {
	tx := core.Transaction{Version: core.TxVersionBase, Inputs: []core.RingInput{{}}, Fee: 123}
	block := &core.Block{Header: core.BlockHeader{Height: 42}, Txs: []core.Transaction{tx}}
	response, err := blockToResponse(block, 0)
	if err != nil {
		t.Fatalf("ambiguous historical fee evidence should not reject block detail: %v", err)
	}
	if response.FeesBurnedNAPRO != nil || response.ProtocolFeeBurnedNAPRO != nil ||
		response.IntentionalBurnNAPRO != nil || response.AVMGasBurnedNAPRO != nil ||
		response.BurnEvidenceStatus == "" {
		t.Fatalf("ambiguous zero-header burns must be unavailable, not guessed: %#v", response)
	}
}

func TestRESTBlockWithUnprovableHistoricalBaseFeeErrors(t *testing.T) {
	genesis := &core.Block{
		Header: core.BlockHeader{Height: 0},
		Txs:    []core.Transaction{core.CoinbaseTx(crypto.Point32{}, 1)},
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	tx := core.Transaction{Version: core.TxVersionBase, Inputs: []core.RingInput{{}}, Fee: 123}
	block := &core.Block{
		Header: core.BlockHeader{Height: 1, PrevHash: genesis.Hash(), BaseFee: 0},
		Txs:    []core.Transaction{tx},
	}
	if err := chain.AddBlock(block); err != nil {
		t.Fatal(err)
	}
	server := NewServer(":0", chain, nil, nil, slog.Default())
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/blocks/1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("REST status=%d body=%s, want readable block detail", recorder.Code, recorder.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"fees_burned_napro", "protocol_fee_burned_napro", "intentional_burn_napro", "avm_gas_burned_napro"} {
		if value, exists := body[field]; !exists || value != nil {
			t.Fatalf("REST %s=%#v (present=%v), want null unavailable evidence", field, value, exists)
		}
	}
	if body["burn_evidence_status"] == nil || body["hash"] == nil || body["height"] != float64(1) {
		t.Fatalf("REST block details/status missing while burns unavailable: %#v", body)
	}
	rpcValue, err := server.aprGetBlockByHeight(json.RawMessage(`{"height":1}`))
	if err != nil {
		t.Fatalf("RPC block detail rejected readable historical block: %v", err)
	}
	rpcResponse, ok := rpcValue.(BlockResponse)
	if !ok || rpcResponse.FeesBurnedNAPRO != nil || rpcResponse.BurnEvidenceStatus == "" {
		t.Fatalf("RPC did not preserve null unavailable burn evidence: %#v", rpcValue)
	}
	if err := server.hub.BroadcastBlock(block); err != nil {
		t.Fatalf("WebSocket block event rejected readable historical block: %v", err)
	}
}

func TestBlockToResponseAVMGasBurnActivationFeeTipAndIntentional(t *testing.T) {
	const (
		baseFee     = uint64(3)
		gasLimit    = uint64(100)
		intentional = uint64(7)
		tip         = uint64(13)
		activation  = uint64(50)
	)
	tx := core.Transaction{
		Version: core.TxVersionAVM,
		Inputs:  []core.RingInput{{}},
		Extra:   core.IntentionalBurnExtra(intentional),
		AVM:     &core.AVMPayload{GasLimit: gasLimit},
	}
	minimum := uint64(tx.Size()) * baseFee
	gasFee, err := core.AVMGasFee(gasLimit)
	if err != nil {
		t.Fatal(err)
	}
	tx.Fee = minimum + intentional + gasFee + tip

	for _, tc := range []struct {
		name      string
		height    uint64
		wantGas   string
		wantTotal string
	}{
		{name: "pre-activation", height: activation - 1, wantGas: "0", wantTotal: new(big.Int).Add(new(big.Int).SetUint64(minimum), new(big.Int).SetUint64(intentional)).String()},
		{name: "activation-height", height: activation, wantGas: big.NewInt(int64(gasFee)).String(), wantTotal: new(big.Int).Add(new(big.Int).Add(new(big.Int).SetUint64(minimum), new(big.Int).SetUint64(intentional)), new(big.Int).SetUint64(gasFee)).String()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := &core.Block{Header: core.BlockHeader{Height: tc.height, BaseFee: baseFee}, Txs: []core.Transaction{tx}}
			response, err := blockToResponse(block, activation)
			if err != nil {
				t.Fatal(err)
			}
			if responseString(response.ProtocolFeeBurnedNAPRO) != new(big.Int).SetUint64(minimum).String() ||
				responseString(response.IntentionalBurnNAPRO) != new(big.Int).SetUint64(intentional).String() ||
				responseString(response.AVMGasBurnedNAPRO) != tc.wantGas || responseString(response.FeesBurnedNAPRO) != tc.wantTotal {
				t.Fatalf("wrong AVM fee projection: %#v", response)
			}
		})
	}
}
