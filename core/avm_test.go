package core

import (
	stded25519 "crypto/ed25519"
	"testing"
)

func signedAVMPayload(t *testing.T) *AVMPayload {
	t.Helper()
	publicKey, privateKey, err := stded25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	var signer [32]byte
	copy(signer[:], publicKey)
	payload := &AVMPayload{
		Action:     AVMDeployContract,
		Code:       []byte{0, 'a', 's', 'm'},
		Entry:      "run",
		GasLimit:   10_000,
		Nonce:      7,
		Signer:     signer,
		AccessList: []AVMAccess{{Key: []byte("balance"), Write: true}},
	}
	payload.ContractID = DeriveAVMContractID(payload.Signer, payload.Nonce, payload.Code)
	signingHash := payload.SigningHash()
	copy(payload.Signature[:], stded25519.Sign(privateKey, signingHash[:]))
	return payload
}

func TestAVMPayloadValidateAndTamperDetection(t *testing.T) {
	payload := signedAVMPayload(t)
	if err := payload.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	payload.Calldata = []byte("tampered")
	if err := payload.Validate(); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestAVMPayloadRequiresCanonicalAccessList(t *testing.T) {
	payload := signedAVMPayload(t)
	payload.AccessList = []AVMAccess{{Key: []byte("z")}, {Key: []byte("a")}}
	if err := payload.Validate(); err == nil {
		t.Fatal("unsorted access list accepted")
	}
}

func TestAVMTransactionActivationFailsClosed(t *testing.T) {
	tx := &Transaction{Version: TxVersionAVM, AVM: signedAVMPayload(t), Inputs: []RingInput{{}}}
	err := ValidateTxVersionAtHeight(tx, 100, 1, 1)
	if err == nil {
		t.Fatal("AVM transaction active without an AVM activation height")
	}
	if err := ValidateTxVersionAtHeight(tx, 100, 1, 1, 100); err != nil {
		t.Fatalf("AVM activation rejected: %v", err)
	}
}

func TestAVMChangesTransactionHash(t *testing.T) {
	payload := signedAVMPayload(t)
	tx := &Transaction{Version: TxVersionAVM, AVM: payload}
	before := tx.Hash()
	tx.AVM.GasLimit++
	after := tx.Hash()
	if before == after {
		t.Fatal("AVM payload change did not change transaction hash")
	}
}
