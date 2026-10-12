package main

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestMintDisplayHintRequiresActualOpening(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	spend, view := keys.Spend.Public, keys.View.Public
	address := crypto.EncodeAddress(crypto.MainnetByte, spend, view)
	const n = 55105 * 100000000
	blind, err := crypto.DeterministicMintBlind(spend, n)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := crypto.Commit(n, blind)
	if err != nil {
		t.Fatal(err)
	}
	r := record{Address: string(address), BlindHex: hex.EncodeToString(blind[:]), CandidateAPRO: "0"}
	u := &store.StoredUTXO{AmountCommit: commit}
	amount, status := recoverAmount(r, u, 3, []uint64{n})
	if amount == nil || *amount != n || status != "opening_verified" {
		t.Fatal("exact opening not recovered")
	}
	r.CandidateAPRO = "55105"
	amount, _ = recoverAmount(r, u, 0, nil)
	if amount == nil || *amount != n {
		t.Fatal("verified candidate rejected")
	}
	u.AmountCommit[0] ^= 1
	if amount, _ := recoverAmount(r, u, 0, []uint64{n}); amount != nil {
		t.Fatal("display hint used despite mismatching actual commitment")
	}
	randomBlind := blind
	randomBlind[0] ^= 1
	randomCommit, err := crypto.Commit(n, randomBlind)
	if err != nil {
		t.Fatal(err)
	}
	r.BlindHex = hex.EncodeToString(randomBlind[:])
	u.AmountCommit = randomCommit
	amount, _ = recoverAmount(r, u, 0, nil)
	if amount == nil || *amount != n {
		t.Fatal("legacy random-blind opening rejected")
	}
}

func TestMintAuditNeedsCanonicalBodyAndNeverMutates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chain")
	d, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	spend, view := keys.Spend.Public, keys.View.Public
	address := crypto.EncodeAddress(crypto.MainnetByte, spend, view)
	blind, _ := crypto.DeterministicMintBlind(spend, 100000000)
	commit, _ := crypto.Commit(100000000, blind)
	tx := core.Transaction{Outputs: []core.Output{{AmountCommit: commit}}}
	hash := tx.Hash()
	u := &store.StoredUTXO{TxHash: hash, BlockHeight: 5, AmountCommit: commit}
	if err := d.PutUTXO(hash, 0, u); err != nil {
		t.Fatal(err)
	}
	r := record{ID: 1, Address: string(address), TxHash: hex.EncodeToString(hash[:]), CandidateAPRO: "1", BlindHex: hex.EncodeToString(blind[:])}
	missing := audit(d, r, 0, nil)
	if missing.Amount == nil || missing.CanonicalBody {
		t.Fatal("opening alone became canonical membership")
	}
	txData, _ := json.Marshal(tx)
	blockHash := crypto.HashBytes([]byte("canonical block"))
	if err := d.PutBlock(blockHash, &store.StoredBlock{Height: 5, Hash: blockHash, TxData: []json.RawMessage{txData}}); err != nil {
		t.Fatal(err)
	}
	verified := audit(d, r, 0, nil)
	if verified.Amount == nil || !verified.CanonicalBody {
		t.Fatal("canonical matched output not verified")
	}
	r.OutputIndex = 1
	absent := audit(d, r, 0, nil)
	if absent.Amount != nil || absent.Status != "output_absent_not_reminted" {
		t.Fatal("missing output recreated")
	}
	if u, err := d.GetUTXO(hash, 1); err != nil || u != nil {
		t.Fatal("audit wrote missing output")
	}
	r.OutputIndex = 0
	if err := d.DeleteUTXO(hash, 0); err != nil {
		t.Fatal(err)
	}
	r.PublicCommit = hex.EncodeToString(commit[:])
	r.PublicHeight = 5
	public := audit(d, r, 0, nil)
	if public.Amount == nil || !public.CanonicalBody || public.MaterialSource != "caller_public_material_not_snapshot_output" {
		t.Fatal("caller material not checked against canonical body")
	}
	r.OutputIndex = 1
	unbound := audit(d, r, 0, nil)
	if unbound.CanonicalBody || unbound.Status != "canonical_output_mismatch" {
		t.Fatal("public opening became fabricated canonical output")
	}
}

func TestMintCandidateUnitConversionsAreOnlyHints(t *testing.T) {
	for _, s := range []string{"-1", "NaN", "Infinity", "0.000000001", "184467440737.09551616"} {
		if _, ok := amountCandidate(s); ok {
			t.Fatal("invalid hint accepted", s)
		}
	}
	if n, ok := amountCandidate("0.1"); !ok || n != 10000000 {
		t.Fatal("APRO conversion incorrect")
	}
}
