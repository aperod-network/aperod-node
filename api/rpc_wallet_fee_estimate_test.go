package api_test

// Tests for apr_walletEstimateFee — the amount-aware fee dry-run (wallet must
// show the SAME fee the node will actually charge; fee grows with the number
// of selected inputs).
//
// Covers:
//   1. Fragmented balance (2+ inputs): the quoted fee equals the fee of the
//      transaction apr_walletSend actually builds — parity check.
//   2. Duplicate OneTimePub (height-0 mints to one address): candidates are
//      deduplicated exactly like TxBuilder.Build, so the quote counts ONE
//      input, not three.
//   3. Unresolvable candidate UTXO: it is skipped (reported in skipped_utxos)
//      and does not distort the input count or fee.

import (
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// feeFor mirrors the node's fee formula for n inputs / 2 outputs at the
// default rate (core.ExportedEstimateFee).
func feeFor(nInputs int) uint64 {
	return core.ExportedEstimateFee(nInputs, 2, core.InitialBaseFeePerByte)
}

type feeEstTestEnv struct {
	srv      *api.Server
	keys     *crypto.WalletKeyPair
	addr     crypto.Address
	utxoList []map[string]interface{}
}

// newFeeEstEnv builds a chain where the wallet owns one mint per entry in
// mintSpecs (height → amount).  Mints at DISTINCT heights get distinct
// OneTimePubs (spendable together); repeated height 0 reproduces the legacy
// shared-key-image admin mints.
func newFeeEstEnv(t *testing.T, mintSpecs []struct {
	Height uint64
	Amount uint64
}) *feeEstTestEnv {
	t.Helper()

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	valPriv, valPub, _ := crypto.GenerateValidatorKey()
	chain := core.NewChain()
	cb := func() core.Transaction { return core.CoinbaseTx(crypto.Point32(valPub), 1_000_000) }

	makeBlock := func(height uint64, prevHash crypto.Hash32, txs []core.Transaction) *core.Block {
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: valPub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if err := hdr.Sign(valPriv); err != nil {
			t.Fatalf("Sign block h=%d: %v", height, err)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	genesis := makeBlock(0, crypto.Hash32{}, []core.Transaction{cb()})
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}

	// One block per mint so distinct-height mints land at their real height.
	utxosSet := core.NewUTXOSet()
	prev := genesis.Hash()
	utxoList := make([]map[string]interface{}, 0, len(mintSpecs))
	mintTxs := make([]*core.Transaction, 0, len(mintSpecs))
	for i, spec := range mintSpecs {
		mt, err := core.BuildMintTx(addr, spec.Amount, spec.Height)
		if err != nil {
			t.Fatalf("BuildMintTx #%d: %v", i, err)
		}
		blockHeight := uint64(i + 1)
		blk := makeBlock(blockHeight, prev, []core.Transaction{cb(), *mt})
		if err := chain.AddBlock(blk); err != nil {
			t.Fatalf("AddBlock h=%d: %v", blockHeight, err)
		}
		prev = blk.Hash()
		mintTxs = append(mintTxs, mt)
	}
	// Register outputs smallest-first so, for shared OneTimePubs, the LAST
	// Add (largest) wins in byPubKey — same as rpc_wallet_max_test.go.
	for i := len(mintTxs) - 1; i >= 0; i-- {
		mt := mintTxs[i]
		h := mt.Hash()
		utxosSet.Add(&core.UTXO{
			TxHash:       h,
			OutputIndex:  0,
			OneTimePub:   mt.Outputs[0].OneTimePub,
			TxPubKey:     mt.Outputs[0].TxPubKey,
			AmountCommit: mt.Outputs[0].AmountCommit,
			BlockHeight:  uint64(i + 1),
		})
	}
	for i, mt := range mintTxs {
		h := mt.Hash()
		utxoList = append(utxoList, map[string]interface{}{
			"tx_hash":     fmt.Sprintf("%x", h[:]),
			"out_idx":     uint32(0),
			"amount_napr": mintSpecs[i].Amount,
			"blind_hex":   "", // server derives DeterministicMintBlind
		})
	}

	mp := core.NewMempool(core.DefaultMempoolConfig())
	return &feeEstTestEnv{
		srv:      api.NewServer(":0", chain, mp, utxosSet, testLogger()),
		keys:     keys,
		addr:     addr,
		utxoList: utxoList,
	}
}

func (e *feeEstTestEnv) call(t *testing.T, method string, extra map[string]interface{}) map[string]interface{} {
	t.Helper()
	params := map[string]interface{}{
		"spend_key_hex": hex.EncodeToString(e.keys.Spend.Private[:]),
		"view_key_hex":  hex.EncodeToString(e.keys.View.Private[:]),
		"utxos":         e.utxoList,
	}
	for k, v := range extra {
		params[k] = v
	}
	resp := rpcCallMax(t, e.srv, method, params)
	if errObj, hasErr := resp["error"].(map[string]interface{}); hasErr {
		t.Fatalf("%s returned error: %v", method, errObj["message"])
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: no result object: %v", method, resp)
	}
	return result
}

// 1. Fragmented balance: two 10-APRO mints at distinct heights; sending
// 12 APRO needs BOTH inputs → the quote must show the 2-input fee, and the
// actually built transaction must charge exactly that fee.
func TestRPC_WalletEstimateFee_TwoInputsParityWithSend(t *testing.T) {
	env := newFeeEstEnv(t, []struct {
		Height uint64
		Amount uint64
	}{
		{Height: 1, Amount: 1_000_000_000},
		{Height: 2, Amount: 1_000_000_000},
	})
	const amount = uint64(1_200_000_000) // needs both UTXOs

	est := env.call(t, "apr_walletEstimateFee", map[string]interface{}{"amount_napr": amount})

	quotedFee := uint64(est["fee_napr"].(float64))
	inputCount := int(est["input_count"].(float64))
	if inputCount != 2 {
		t.Fatalf("input_count = %d, want 2", inputCount)
	}
	if quotedFee != feeFor(2) {
		t.Fatalf("fee_napr = %d, want 2-input fee %d", quotedFee, feeFor(2))
	}
	if quotedFee <= feeFor(1) {
		t.Fatalf("2-input fee %d must exceed 1-input fee %d", quotedFee, feeFor(1))
	}
	if est["sufficient"].(bool) != true {
		t.Fatalf("sufficient = false, want true")
	}
	if n := int(est["spendable_utxo_count"].(float64)); n != 2 {
		t.Fatalf("spendable_utxo_count = %d, want 2", n)
	}

	// Parity: build the real transaction for the same amount + UTXO set.
	bobKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys (bob): %v", err)
	}
	bobAddr := crypto.AddressFromKeys(crypto.MainnetByte, bobKeys)
	send := env.call(t, "apr_walletSend", map[string]interface{}{
		"to_address":     bobAddr.String(),
		"change_address": env.addr.String(),
		"amount_napr":    amount,
	})
	actualFee := uint64(send["total_fee_napr"].(float64))
	if actualFee != quotedFee {
		t.Fatalf("actual send fee %d != quoted fee %d — wallet would show the wrong fee", actualFee, quotedFee)
	}
	t.Logf("parity OK: quoted=%d actual=%d inputs=%d", quotedFee, actualFee, inputCount)
}

// 2. Height-0 mints sharing one OneTimePub must be deduplicated: only ONE of
// them is spendable, so the quote must count 1 input — not 3 — and must not
// claim sufficiency from phantom duplicates.
func TestRPC_WalletEstimateFee_DedupsSharedOneTimePub(t *testing.T) {
	env := newFeeEstEnv(t, []struct {
		Height uint64
		Amount uint64
	}{
		{Height: 0, Amount: 5_000_000_000}, // largest — the only spendable one
		{Height: 0, Amount: 3_000_000_000},
		{Height: 0, Amount: 2_000_000_000},
	})

	est := env.call(t, "apr_walletEstimateFee", map[string]interface{}{
		"amount_napr": uint64(1_000_000_000),
	})
	if n := int(est["spendable_utxo_count"].(float64)); n != 1 {
		t.Fatalf("spendable_utxo_count = %d, want 1 (shared OneTimePub)", n)
	}
	if n := int(est["input_count"].(float64)); n != 1 {
		t.Fatalf("input_count = %d, want 1", n)
	}
	if fee := uint64(est["fee_napr"].(float64)); fee != feeFor(1) {
		t.Fatalf("fee_napr = %d, want 1-input fee %d", fee, feeFor(1))
	}
	if total := uint64(est["spendable_total_napr"].(float64)); total != 5_000_000_000 {
		t.Fatalf("spendable_total_napr = %d, want 5_000_000_000 (largest only)", total)
	}
	// A send that would need the phantom duplicates must be flagged insufficient.
	est2 := env.call(t, "apr_walletEstimateFee", map[string]interface{}{
		"amount_napr": uint64(9_000_000_000),
	})
	if est2["sufficient"].(bool) != false {
		t.Fatalf("sufficient = true for 9 APRO, want false (only 5 APRO truly spendable)")
	}
}

// 3. An unresolvable candidate (tx not on chain) is skipped — reported in
// skipped_utxos — and does not inflate the input count or fee.
func TestRPC_WalletEstimateFee_SkipsUnresolvableUTXO(t *testing.T) {
	env := newFeeEstEnv(t, []struct {
		Height uint64
		Amount uint64
	}{
		{Height: 1, Amount: 10_000_000_000},
	})
	// Append a bogus candidate that no resolution path can find.
	env.utxoList = append(env.utxoList, map[string]interface{}{
		"tx_hash":     "ff00000000000000000000000000000000000000000000000000000000000000",
		"out_idx":     uint32(0),
		"amount_napr": uint64(7_000_000_000),
		"blind_hex":   "",
	})

	est := env.call(t, "apr_walletEstimateFee", map[string]interface{}{
		"amount_napr": uint64(1_000_000_000),
	})
	skipped, _ := est["skipped_utxos"].([]interface{})
	if len(skipped) != 1 {
		t.Fatalf("skipped_utxos len = %d, want 1", len(skipped))
	}
	if n := int(est["spendable_utxo_count"].(float64)); n != 1 {
		t.Fatalf("spendable_utxo_count = %d, want 1 (bogus UTXO excluded)", n)
	}
	if fee := uint64(est["fee_napr"].(float64)); fee != feeFor(1) {
		t.Fatalf("fee_napr = %d, want 1-input fee %d", fee, feeFor(1))
	}
}
