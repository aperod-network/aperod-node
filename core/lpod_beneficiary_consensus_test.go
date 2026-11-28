package core

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestLPoDBeneficiaryConsensusForkPreservesReplayAndRejectsNewUnsafePositions(t *testing.T) {
	for name, operation := range map[string]uint8{"deposit": LPoDDeposit, "top-up": LPoDTopUp} {
		t.Run(name, func(t *testing.T) {
			action, owner, source, utxos := beneficiaryAdmissionFixture(t)
			if operation == LPoDTopUp {
				action.Action, action.Nonce = LPoDTopUp, 1
			}
			valid := signHistoricalBeneficiaryAction(t, action, owner, source)
			verifier := NewTxVerifier(utxos)
			verifier.SetLPoDDailyUpgradeHeight(1)
			verifier.SetLPoDBeneficiaryActivationHeight(100, "mainnet")
			for name, view := range map[string]crypto.Point32{
				"identity": {1}, "identity alias": {0: 1, 31: 0x80}, "torsion": {},
			} {
				t.Run(name, func(t *testing.T) {
					changed := action
					changed.Beneficiary = crypto.EncodeAddress(crypto.MainnetByte, action.Owner, view)
					invalid := signHistoricalBeneficiaryAction(t, changed, owner, source)
					// Both messages carry genuine signatures over their respective
					// payloads and reference a funded unspent source.
					for _, height := range []uint64{1, 99} {
						block := &Block{Header: BlockHeader{Height: height}, Txs: []Transaction{*invalid}}
						if err := verifier.VerifyBlock(block); err != nil {
							t.Fatalf("historical replay changed at %d: %v", height, err)
						}
					}
					block := &Block{Header: BlockHeader{Height: 100}, Txs: []Transaction{*invalid}}
					if err := verifier.VerifyBlock(block); err == nil || !strings.Contains(err.Error(), "consensus beneficiary") {
						t.Fatalf("unsafe independently constructed block accepted: %v", err)
					}
					if err := verifier.VerifyTxAtHeight(invalid, 100); err == nil {
						t.Fatal("height-aware transaction verification bypassed new rule")
					}
					if utxos.Get(action.SourceTx, action.SourceIndex) == nil || utxos.IsSpent(invalid.Inputs[0].KeyImage) {
						t.Fatal("rejection changed funding or key-image spentness")
					}
					block.Txs[0] = *valid
					if err := verifier.VerifyBlock(block); err != nil {
						t.Fatalf("valid funded control rejected after malformed candidate: %v", err)
					}
				})
			}
		})
	}
}

func TestLPoDBeneficiaryConsensusPinsNetworkAndRequiresHeight(t *testing.T) {
	action, owner, source, utxos := beneficiaryAdmissionFixture(t)
	tx := signHistoricalBeneficiaryAction(t, action, owner, source)
	v := NewTxVerifier(utxos)
	for _, network := range []string{"", "unknown", "testnet", "devnet"} {
		v.SetLPoDBeneficiaryActivationHeight(100, network)
		if err := v.VerifyTxAtHeight(tx, 99); err != nil {
			t.Fatalf("disabled historical network policy changed: %v", err)
		}
		if err := v.VerifyTxAtHeight(tx, 100); err == nil || !strings.Contains(err.Error(), "network") {
			t.Fatalf("network %q accepted at boundary: %v", network, err)
		}
	}
	v.SetLPoDBeneficiaryActivationHeight(100, "mainnet")
	if err := v.VerifyTxAtHeight(tx, 100); err != nil {
		t.Fatal(err)
	}
	if err := v.VerifyTx(tx); err == nil || !strings.Contains(err.Error(), "height-aware") {
		t.Fatalf("heightless verifier bypass: %v", err)
	}
	if err := v.VerifyTxAtHeight(tx, 0); err == nil {
		t.Fatal("zero-height transaction bypass")
	}
	if err := v.VerifyBlock(&Block{Txs: []Transaction{*tx}}); err == nil {
		t.Fatal("zero-height block bypass")
	}
	v.SetLPoDBeneficiaryActivationHeight(0, "")
	if err := v.VerifyTx(tx); err != nil {
		t.Fatalf("zero gate did not preserve existing verification: %v", err)
	}
}

func TestLPoDBeneficiaryConsensusKeepsSignedLegacyRecoveryWithdrawal(t *testing.T) {
	action, owner, _, _ := beneficiaryAdmissionFixture(t)
	action.Action, action.Nonce = LPoDWithdraw, 1
	action.Beneficiary = crypto.EncodeAddress(crypto.MainnetByte, action.Owner, crypto.Point32{1})
	tx, err := BuildLPoDPositionTx(action, owner, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	v := NewTxVerifier(nil)
	v.SetLPoDBeneficiaryActivationHeight(100, "mainnet")
	if err := v.VerifyTxAtHeight(tx, 100); err != nil {
		t.Fatalf("new rule blocked historical owner-authorized recovery: %v", err)
	}
	if err := v.VerifyBlock(&Block{Header: BlockHeader{Height: 100}, Txs: []Transaction{*tx}}); err != nil {
		t.Fatal(err)
	}
	// Recovery is not an exception to the original ownership signature.
	tx.Signatures[0].SS[0][0] ^= 1
	if err := v.VerifyTxAtHeight(tx, 100); err == nil {
		t.Fatal("mutated legacy recovery signature accepted")
	}
}
