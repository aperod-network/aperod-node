package core

import (
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

func preflightTestKeyImage(t *testing.T) crypto.KeyImage {
	t.Helper()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	ring := make([]crypto.RingMember, crypto.RingSize)
	ring[0] = keys.Spend.Public
	for i := 1; i < len(ring); i++ {
		decoy, genErr := crypto.GenerateWalletKeys()
		if genErr != nil {
			t.Fatalf("GenerateWalletKeys decoy: %v", genErr)
		}
		ring[i] = decoy.Spend.Public
	}
	sig, err := crypto.MLSAGSign(crypto.Hash32{}, ring, 0, keys.Spend.Private)
	if err != nil {
		t.Fatalf("MLSAGSign: %v", err)
	}
	return sig.KeyImage
}

func preflightTestUTXO(t *testing.T, set *UTXOSet, marker byte) *UTXO {
	t.Helper()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	commit, err := crypto.Commit(uint64(marker)+1, crypto.BlindFactor{})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	var txHash crypto.Hash32
	txHash[0] = marker
	u := &UTXO{
		TxHash:       txHash,
		OutputIndex:  uint32(marker),
		OneTimePub:   keys.Spend.Public,
		AmountCommit: commit,
	}
	set.Add(u)
	return u
}

func TestApplyBlockPreflightRejectsWithoutMutation(t *testing.T) {
	tests := []struct {
		name      string
		makeInput func(t *testing.T, existing *UTXO) RingInput
		version   TxVersion
		wantErr   string
	}{
		{
			name: "missing input",
			makeInput: func(t *testing.T, _ *UTXO) RingInput {
				missing, err := crypto.GenerateWalletKeys()
				if err != nil {
					t.Fatal(err)
				}
				return RingInput{
					KeyImage:     preflightTestKeyImage(t),
					Ring:         []crypto.RingMember{missing.Spend.Public},
					AmountCommit: crypto.Commitment{1},
				}
			},
			version: TxVersionBase,
			wantErr: "no active ring member",
		},
		{
			name: "forged v4 commitment",
			makeInput: func(t *testing.T, existing *UTXO) RingInput {
				return RingInput{
					KeyImage:     preflightTestKeyImage(t),
					Ring:         []crypto.RingMember{existing.OneTimePub},
					RealIndex:    0,
					AmountCommit: crypto.Commitment{0xff},
				}
			},
			version: TxVersionCommitmentBinding,
			wantErr: "commitment mismatch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			set := NewUTXOSet()
			existing := preflightTestUTXO(t, set, 0x31)
			recipient, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatal(err)
			}
			inp := tc.makeInput(t, existing)
			tx := Transaction{
				Version: tc.version,
				Inputs:  []RingInput{inp},
				Outputs: []Output{{OneTimePub: recipient.Spend.Public, AmountCommit: existing.AmountCommit}},
			}
			block := &Block{Header: BlockHeader{Height: 44}, Txs: []Transaction{tx}}
			callbacks := 0
			set.OnUTXOSpent = func(crypto.Hash32, uint32) { callbacks++ }

			err = set.ApplyBlock(block)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ApplyBlock error = %v, want containing %q", err, tc.wantErr)
			}
			if set.IsSpent(inp.KeyImage) {
				t.Fatal("failed preflight marked key image spent")
			}
			if set.Get(existing.TxHash, existing.OutputIndex) != existing {
				t.Fatal("failed preflight removed existing UTXO")
			}
			if set.Get(tx.Hash(), 0) != nil {
				t.Fatal("failed preflight added transaction output")
			}
			if callbacks != 0 {
				t.Fatalf("failed preflight invoked %d callbacks", callbacks)
			}
			set.mu.RLock()
			journalLen := len(set.rollbackJournal[block.Header.Height])
			set.mu.RUnlock()
			if journalLen != 0 {
				t.Fatalf("failed preflight wrote %d journal entries", journalLen)
			}
		})
	}
}

func TestApplyBlockPreflightRejectsDuplicateUTXOConsumption(t *testing.T) {
	set := NewUTXOSet()
	existing := preflightTestUTXO(t, set, 0x41)
	input := func() RingInput {
		return RingInput{
			KeyImage:     preflightTestKeyImage(t),
			Ring:         []crypto.RingMember{existing.OneTimePub},
			AmountCommit: existing.AmountCommit,
		}
	}
	tx1 := Transaction{Version: TxVersionBase, Inputs: []RingInput{input()}}
	tx2 := Transaction{Version: TxVersionBase, Inputs: []RingInput{input()}}
	block := &Block{Header: BlockHeader{Height: 45}, Txs: []Transaction{tx1, tx2}}

	if err := set.ApplyBlock(block); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("ApplyBlock error = %v, want duplicate UTXO consumption", err)
	}
	if set.Get(existing.TxHash, existing.OutputIndex) == nil {
		t.Fatal("duplicate-consumption rejection removed UTXO")
	}
	if set.IsSpent(tx1.Inputs[0].KeyImage) || set.IsSpent(tx2.Inputs[0].KeyImage) {
		t.Fatal("duplicate-consumption rejection marked a key image spent")
	}
}

func TestApplyBlockPreflightAcceptsLegacyWithAbsentDecoys(t *testing.T) {
	set := NewUTXOSet()
	existing := preflightTestUTXO(t, set, 0x51)
	ring := []crypto.RingMember{existing.OneTimePub}
	for i := 0; i < 3; i++ {
		decoy, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		ring = append(ring, decoy.Spend.Public)
	}
	inp := RingInput{
		KeyImage:     preflightTestKeyImage(t),
		Ring:         ring,
		AmountCommit: existing.AmountCommit,
	}
	block := &Block{
		Header: BlockHeader{Height: 46},
		Txs: []Transaction{{
			Version: TxVersionBase,
			Inputs:  []RingInput{inp},
		}},
	}

	if err := set.ApplyBlock(block); err != nil {
		t.Fatalf("ApplyBlock: %v", err)
	}
	if set.Get(existing.TxHash, existing.OutputIndex) != nil {
		t.Fatal("valid legacy spend did not consume matching active UTXO")
	}
	if !set.IsSpent(inp.KeyImage) {
		t.Fatal("valid legacy spend did not mark key image spent")
	}
}
