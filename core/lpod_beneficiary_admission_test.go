package core

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/crypto"
)

func beneficiaryAdmissionFixture(t *testing.T) (LPoDPositionAction, crypto.Scalar32, crypto.Scalar32, *UTXOSet) {
	t.Helper()
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	source := crypto.ScalarFromUint64(19)
	sourcePub, err := crypto.PublicKeyFromPrivate(source)
	if err != nil {
		t.Fatal(err)
	}
	action := LPoDPositionAction{
		Action: LPoDDeposit, Genesis: crypto.HashStr("beneficiary-admission-genesis"),
		SourceTx: crypto.HashStr("beneficiary-admission-funded-source"), SourcePub: sourcePub,
		Beneficiary: crypto.AddressFromKeys(crypto.MainnetByte, keys), Owner: keys.Spend.Public,
		Amount: 10_100_000_000, Blind: crypto.BlindFactor(crypto.ScalarFromUint64(23)),
	}
	action.PositionID = LPoDPositionID(action.Genesis, action.SourceTx, action.SourceIndex)
	commit, err := crypto.Commit(action.Amount, action.Blind)
	if err != nil {
		t.Fatal(err)
	}
	utxos := NewUTXOSet()
	utxos.Add(&UTXO{TxHash: action.SourceTx, OutputIndex: action.SourceIndex,
		OneTimePub: sourcePub, AmountCommit: commit, BlockHeight: 1})
	return action, keys.Spend.Private, source, utxos
}

// Re-sign every modified action using throwaway keys. This intentionally uses
// the historical wire construction rather than bypassing authentication.
func signHistoricalBeneficiaryAction(t *testing.T, action LPoDPositionAction, owner, source crypto.Scalar32) *Transaction {
	t.Helper()
	body, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	ownerProof, err := crypto.MLSAGSignV4(action.Message(), LPoDOwnershipRing(action.Owner), 0, owner, crypto.BlindFactor(owner), 0)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := crypto.MLSAGSignV4(action.Message(), LPoDOwnershipRing(action.SourcePub), 0, source, action.Blind, action.Amount)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := crypto.Commit(action.Amount, action.Blind)
	if err != nil {
		t.Fatal(err)
	}
	version := TxVersionLPoDPosition
	if action.Action == LPoDTopUp {
		version = TxVersionLPoDTopUp
	}
	tx := &Transaction{Version: version, Extra: body,
		Signatures: []*crypto.MLSAGSignature{ownerProof, proof},
		Inputs:     []RingInput{{Ring: LPoDOwnershipRing(action.SourcePub), AmountCommit: commit, KeyImage: proof.KeyImage}}}
	if err := tx.Validate(); err != nil {
		t.Fatalf("signed historical control is invalid: %v", err)
	}
	return tx
}

func TestLPoDBeneficiaryAdmissionRejectsUnsafeKeysBeforeReservations(t *testing.T) {
	for name, view := range map[string]crypto.Point32{
		"identity": {1}, "identity sign alias": {0: 1, 31: 0x80}, "torsion": {},
	} {
		t.Run(name, func(t *testing.T) {
			action, owner, source, utxos := beneficiaryAdmissionFixture(t)
			valid := signHistoricalBeneficiaryAction(t, action, owner, source)
			action.Beneficiary = crypto.EncodeAddress(crypto.MainnetByte, action.Owner, view)
			invalid := signHistoricalBeneficiaryAction(t, action, owner, source)
			cfg := DefaultMempoolConfig()
			cfg.Network, cfg.Verifier = "mainnet", NewTxVerifier(utxos)
			pool := NewMempool(cfg)
			if err := pool.Add(*invalid); err == nil || !strings.Contains(err.Error(), "beneficiary admission") {
				t.Fatalf("unsafe beneficiary admission error = %v", err)
			}
			if err := pool.AddPrivileged(*invalid); err == nil || !strings.Contains(err.Error(), "beneficiary admission") {
				t.Fatalf("privileged path bypassed beneficiary policy: %v", err)
			}
			if len(pool.Hashes()) != 0 || pool.TotalBytes() != 0 || len(pool.keyImages) != 0 ||
				utxos.IsSpent(invalid.Inputs[0].KeyImage) || utxos.Get(action.SourceTx, action.SourceIndex) == nil {
				t.Fatal("rejected deposit changed the pool or spent its funding")
			}
			if _, err := BuildLPoDPositionTx(action, owner, source); err == nil {
				t.Fatal("local builder produced unsafe beneficiary deposit")
			}
			if err := pool.Add(*valid); err != nil {
				t.Fatalf("same valid funding could not be used after rejection: %v", err)
			}
		})
	}
}

func TestLPoDBeneficiaryAdmissionPinsConfiguredNetwork(t *testing.T) {
	action, owner, source, utxos := beneficiaryAdmissionFixture(t)
	_, spend, view, err := crypto.DecodeAddress(action.Beneficiary)
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []crypto.NetworkByte{crypto.MainnetByte, crypto.TestnetByte, crypto.DevnetByte} {
		action.Beneficiary = crypto.EncodeAddress(network, spend, view)
		tx := signHistoricalBeneficiaryAction(t, action, owner, source)
		for _, configured := range []string{"mainnet", "testnet", "devnet", "unknown"} {
			cfg := DefaultMempoolConfig()
			cfg.Network, cfg.Verifier = configured, NewTxVerifier(utxos)
			err := NewMempool(cfg).Add(*tx)
			wantOK := configured == "mainnet" && network == crypto.MainnetByte ||
				configured == "testnet" && network == crypto.TestnetByte ||
				configured == "devnet" && network == crypto.DevnetByte
			if (err == nil) != wantOK {
				t.Fatalf("address network=%x node=%s: error=%v", network, configured, err)
			}
		}
	}
}

func TestLPoDBeneficiaryAdmissionRechecksPersistedPool(t *testing.T) {
	action, owner, source, utxos := beneficiaryAdmissionFixture(t)
	action.Beneficiary = crypto.EncodeAddress(crypto.MainnetByte, action.Owner, crypto.Point32{1})
	tx := signHistoricalBeneficiaryAction(t, action, owner, source)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	persisted := NewMempool(DefaultMempoolConfig(), log)
	// Simulate an old binary's already-persisted entry, not admission by the new policy.
	hash := tx.Hash()
	persisted.entries[hash] = &mempoolEntry{Tx: *tx, Hash: hash, Size: tx.Size(), Received: time.Now()}
	dir := t.TempDir()
	if err := persisted.Save(dir); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultMempoolConfig()
	cfg.Network, cfg.Verifier = "mainnet", NewTxVerifier(utxos)
	restored := NewMempool(cfg, log)
	if count := restored.Load(dir, log); count != 0 || len(restored.Hashes()) != 0 || len(restored.keyImages) != 0 {
		t.Fatalf("unsafe beneficiary survived persisted-pool reload: %d", count)
	}
}

func TestLPoDBeneficiaryAdmissionDoesNotRewriteLegacyWithdrawal(t *testing.T) {
	action, owner, _, _ := beneficiaryAdmissionFixture(t)
	action.Action, action.Nonce = LPoDWithdraw, 1
	action.Beneficiary = crypto.EncodeAddress(crypto.MainnetByte, action.Owner, crypto.Point32{1})
	tx, err := BuildLPoDPositionTx(action, owner, crypto.Scalar32{})
	if err != nil {
		t.Fatalf("historical withdrawal construction was disabled: %v", err)
	}
	if err := tx.Validate(); err != nil {
		t.Fatalf("historical consensus parser changed: %v", err)
	}
}

func TestLPoDTopUpBeneficiaryAdmissionCannotBypassDepositPolicy(t *testing.T) {
	action, owner, source, _ := beneficiaryAdmissionFixture(t)
	action.Action, action.Nonce = LPoDTopUp, 1
	valid := signHistoricalBeneficiaryAction(t, action, owner, source)
	action.Beneficiary = crypto.EncodeAddress(crypto.MainnetByte, action.Owner, crypto.Point32{1})
	invalid := signHistoricalBeneficiaryAction(t, action, owner, source)
	cfg := DefaultMempoolConfig()
	cfg.Network = "mainnet"
	pool := NewMempool(cfg)
	if err := pool.Add(*invalid); err == nil || !strings.Contains(err.Error(), "beneficiary admission") {
		t.Fatalf("top-up bypassed recipient admission policy: %v", err)
	}
	if err := pool.AddPrivileged(*invalid); err == nil {
		t.Fatal("privileged top-up bypassed recipient admission policy")
	}
	if _, err := BuildLPoDPositionTx(action, owner, source); err == nil {
		t.Fatal("builder produced top-up with unsafe beneficiary")
	}
	if err := pool.Add(*valid); err != nil {
		t.Fatalf("valid top-up control rejected: %v", err)
	}
}
