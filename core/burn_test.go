package core_test

import (
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func buildBurnForTest(t *testing.T) (*core.BuildResult, uint64) {
	t.Helper()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	const supply uint64 = 20_000_000
	const amount uint64 = 1_000_000
	chain, _, blind := makeGenesisWithCoinbase(t, keys, supply)
	scanner := core.NewWalletScanner(keys.View.Private, keys.Spend.Public, keys.View.Public, crypto.MainnetByte)
	owned := scanner.ScanChain(chain, 0, chain.Height())
	if len(owned) != 1 {
		t.Fatalf("owned UTXOs = %d, want 1", len(owned))
	}
	owned[0].Blind = blind
	change := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	result, err := core.NewTxBuilder(keys.Spend.Private, keys.View.Private, keys.Spend.Public, owned, 1).
		Build(amount, crypto.MainnetBurnAddress(), change)
	if err != nil {
		t.Fatalf("build burn: %v", err)
	}
	return result, amount
}

func TestIntentionalBurnMarkerValidation(t *testing.T) {
	result, amount := buildBurnForTest(t)
	if got, ok := result.Tx.BurnAmount(); !ok || got != amount {
		t.Fatalf("BurnAmount = (%d, %v), want (%d, true)", got, ok, amount)
	}
	malformed := result.Tx
	malformed.Extra = malformed.Extra[:len(malformed.Extra)-1]
	if err := malformed.Validate(); err == nil {
		t.Fatal("truncated burn marker was accepted")
	}
	zero := result.Tx
	zero.Extra = core.IntentionalBurnExtra(0)
	if err := zero.Validate(); err == nil {
		t.Fatal("zero burn marker was accepted")
	}
	legacy := result.Tx
	legacy.Version = core.TxVersionBase
	if err := legacy.Validate(); err == nil {
		t.Fatal("non-v4 burn marker was accepted")
	}
}

func TestTxBuilderCanonicalBurnHasNoRecipientOutput(t *testing.T) {
	result, amount := buildBurnForTest(t)
	if !result.Tx.IsBurnTx() {
		t.Fatal("canonical burn build lacks burn marker")
	}
	if len(result.Tx.Outputs) != 1 || result.ChangeOutIdx != 0 || result.PayOutIdx != -1 {
		t.Fatalf("burn outputs=%d change_index=%d pay_index=%d; want optional change only",
			len(result.Tx.Outputs), result.ChangeOutIdx, result.PayOutIdx)
	}
	if result.TotalFee < amount || result.Tx.Fee != result.TotalFee {
		t.Fatalf("burn fee=%d total=%d does not include amount=%d", result.Tx.Fee, result.TotalFee, amount)
	}
	if got, ok := result.Tx.BurnAmount(); !ok || got != amount {
		t.Fatalf("marker amount = (%d, %v), want (%d, true)", got, ok, amount)
	}
}

func TestMempoolRejectsBurnBelowBasePlusAmount(t *testing.T) {
	result, _ := buildBurnForTest(t)
	tx := result.Tx
	tx.Fee = tx.MinFeeAt(1) // deliberately omits intentional burn amount
	pool := core.NewMempool(core.MempoolConfig{
		MaxSize: 10, MaxBytes: 1 << 20, MaxTxSize: 1 << 20,
		BaseFeePerByte: 1,
	})
	if err := pool.Add(tx); err == nil {
		t.Fatal("mempool accepted burn fee below base fee plus burn amount")
	}
}

func TestBurnActivationVersionPolicy(t *testing.T) {
	legacy := &core.Transaction{Version: core.TxVersionBase, Inputs: []core.RingInput{{}}}
	burn := &core.Transaction{
		Version: core.TxVersionCommitmentBinding,
		Inputs:  []core.RingInput{{}},
		Extra:   core.IntentionalBurnExtra(1),
	}
	const activation = uint64(100)
	if err := core.ValidateTxVersionAtHeight(legacy, activation-1, activation); err != nil {
		t.Fatalf("legacy tx before activation rejected: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(burn, activation-1, activation); err == nil {
		t.Fatal("v4 burn accepted before activation")
	}
	if err := core.ValidateTxVersionAtHeight(burn, activation, activation); err != nil {
		t.Fatalf("v4 burn at activation rejected: %v", err)
	}
	if err := core.ValidateTxVersionAtHeight(legacy, 0, 0); err == nil {
		t.Fatal("legacy tx accepted with activation height 0")
	}
	if err := core.ValidateTxVersionAtHeight(burn, 0, 0); err != nil {
		t.Fatalf("v4 burn rejected with activation height 0: %v", err)
	}
}
