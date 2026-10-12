package core_test

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// The valid control is essential: an unfunded ring can otherwise make every
// tampering case pass at the fabricated-input guard, without testing commitments.
func TestSecurityReview_FundedCommitmentTampering(t *testing.T) {
	owner, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	const inputAmount, outputAmount, fee = uint64(10_000_000), uint64(9_000_000), uint64(1_000_000)
	commit := func(amount uint64) crypto.Commitment {
		c, e := crypto.Commit(amount, crypto.BlindFactor{})
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = owner.Spend.Public
	for i := 1; i < len(ring); i++ {
		decoy, e := crypto.GenerateWalletKeys()
		if e != nil {
			t.Fatal(e)
		}
		ring[i] = decoy.Spend.Public
	}
	ki, err := crypto.ComputeKeyImage(owner.Spend.Private, owner.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := crypto.ProveRange(outputAmount, crypto.BlindFactor{})
	if err != nil {
		t.Fatal(err)
	}
	tx := core.Transaction{
		Version: core.TxVersionBase,
		Inputs:  []core.RingInput{{KeyImage: ki, Ring: ring, AmountCommit: commit(inputAmount)}},
		Outputs: []core.Output{{OneTimePub: recipient.Spend.Public, AmountCommit: commit(outputAmount)}},
		Fee:     fee, FeeCommit: commit(fee), RangeProofs: []*crypto.RangeProof{proof},
	}
	sign := func(v *core.Transaction) {
		sig, e := crypto.MLSAGSign(core.RingSignMessage(v.Hash(), 0), ring, 0, owner.Spend.Private)
		if e != nil {
			t.Fatal(e)
		}
		v.Signatures = []*crypto.MLSAGSignature{sig}
	}
	utxos := core.NewUTXOSet()
	utxos.Add(&core.UTXO{TxHash: crypto.HashStr("security-review-funded-control"),
		OneTimePub: owner.Spend.Public, AmountCommit: commit(inputAmount)})
	verifier := core.NewTxVerifier(utxos)
	sign(&tx)
	if err := verifier.VerifyTx(&tx); err != nil {
		t.Fatalf("valid funded control must pass: %v", err)
	}
	for _, kind := range []string{"output", "input", "fee"} {
		t.Run(kind, func(t *testing.T) {
			bad := tx
			bad.Inputs = append([]core.RingInput(nil), tx.Inputs...)
			bad.Outputs = append([]core.Output(nil), tx.Outputs...)
			switch kind {
			case "output":
				bad.Outputs[0].AmountCommit = commit(outputAmount + 1)
			case "input":
				bad.Inputs[0].AmountCommit = commit(inputAmount + 1)
			case "fee":
				bad.FeeCommit = commit(fee + 1)
			}
			// Re-sign the modified message so an invalid signature cannot
			// accidentally substitute for the commitment rejection.
			sign(&bad)
			err := verifier.VerifyTx(&bad)
			if err == nil || (!strings.Contains(err.Error(), "commitment") &&
				!strings.Contains(err.Error(), "balance") && !strings.Contains(err.Error(), "range proof")) {
				t.Fatalf("expected commitment-specific rejection, got %v", err)
			}
		})
	}
}

func TestSecurityReview_KeyImageAcrossLevelDBReopen(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	ki, err := crypto.ComputeKeyImage(keys.Spend.Private, keys.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first := core.NewUTXOSetWithDB(db)
	first.MarkSpent(ki)
	first.CompactKeyImages()
	if !first.IsSpent(ki) {
		t.Fatal("spent image missing before close")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted := core.NewUTXOSetWithDB(db)
	if !restarted.IsSpent(ki) {
		t.Fatal("spent image lost across actual LevelDB close/reopen")
	}
	fresh, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	unspent, err := crypto.ComputeKeyImage(fresh.Spend.Private, fresh.Spend.Public)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.IsSpent(unspent) {
		t.Fatal("fresh unspent image falsely marked spent")
	}
}
