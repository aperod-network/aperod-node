package consensus

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestLPoDBeneficiaryScheduleReachesEngineBlockVerifier(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	source := crypto.ScalarFromUint64(19)
	sourcePub, err := crypto.PublicKeyFromPrivate(source)
	if err != nil {
		t.Fatal(err)
	}
	action := core.LPoDPositionAction{
		Action: core.LPoDDeposit, Genesis: crypto.HashStr("beneficiary-consensus-test"),
		SourceTx: crypto.HashStr("beneficiary-consensus-funded-source"), SourcePub: sourcePub,
		Beneficiary: crypto.AddressFromKeys(crypto.MainnetByte, keys), Owner: keys.Spend.Public,
		Amount: 10_100_000_000, Blind: crypto.BlindFactor(crypto.ScalarFromUint64(23)),
	}
	action.PositionID = core.LPoDPositionID(action.Genesis, action.SourceTx, action.SourceIndex)
	tx, err := core.BuildLPoDPositionTx(action, keys.Spend.Private, source)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := crypto.Commit(action.Amount, action.Blind)
	if err != nil {
		t.Fatal(err)
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: action.SourceTx, OneTimePub: sourcePub, AmountCommit: commit})
	v := core.NewTxVerifier(utxos)
	e := &Engine{cfg: Config{LPoDBeneficiaryActivationHeight: 100, LPoDBeneficiaryNetwork: "mainnet"}}
	e.SetTxVerifier(v, utxos)
	block := &core.Block{Header: core.BlockHeader{Height: 100}, Txs: []core.Transaction{*tx}}
	if err := v.VerifyBlock(block); err != nil {
		t.Fatalf("correct engine network rejected: %v", err)
	}
	e.cfg.LPoDBeneficiaryNetwork = "testnet"
	e.SetTxVerifier(v, utxos)
	if err := v.VerifyBlock(block); err == nil || !strings.Contains(err.Error(), "network") {
		t.Fatalf("engine network was not propagated to block verification: %v", err)
	}
	block.Header.Height = 99
	if err := v.VerifyBlock(block); err != nil {
		t.Fatalf("engine changed pre-fork history: %v", err)
	}
}
