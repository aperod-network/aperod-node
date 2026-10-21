package crypto

import (
	"testing"
)

func TestKeyImageDLEQProof(t *testing.T) {
	keys, err := GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	context := []byte("stake-v4 test context")
	ki, proof, err := ProveKeyImage(keys.Spend.Private, keys.Spend.Public, context)
	if err != nil {
		t.Fatalf("ProveKeyImage: %v", err)
	}
	wantKI, err := ComputeKeyImage(keys.Spend.Private, keys.Spend.Public)
	if err != nil {
		t.Fatalf("ComputeKeyImage: %v", err)
	}
	if ki != wantKI {
		t.Fatal("proof returned a key image different from ComputeKeyImage")
	}
	if err := VerifyKeyImageProof(keys.Spend.Public, ki, context, proof); err != nil {
		t.Fatalf("VerifyKeyImageProof: %v", err)
	}
}

func TestKeyImageDLEQProofRejectsMutationAndWrongContext(t *testing.T) {
	keys, err := GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	context := []byte("stake-v4 test context")
	ki, proof, err := ProveKeyImage(keys.Spend.Private, keys.Spend.Public, context)
	if err != nil {
		t.Fatalf("ProveKeyImage: %v", err)
	}
	if err := VerifyKeyImageProof(keys.Spend.Public, ki, []byte("different action"), proof); err == nil {
		t.Fatal("proof verified with a different context")
	}

	mutated := proof
	mutated.Response[0] ^= 1
	if err := VerifyKeyImageProof(keys.Spend.Public, ki, context, mutated); err == nil {
		t.Fatal("proof verified after response mutation")
	}

	mutatedKI := ki
	mutatedKI[0] ^= 1
	if err := VerifyKeyImageProof(keys.Spend.Public, mutatedKI, context, proof); err == nil {
		t.Fatal("proof verified for a different key image")
	}
}

func TestProveKeyImageRejectsMismatchedSecret(t *testing.T) {
	owner, err := GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	other, err := GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	if _, _, err := ProveKeyImage(owner.Spend.Private, other.Spend.Public, []byte("context")); err == nil {
		t.Fatal("ProveKeyImage accepted a secret for a different public key")
	}
}
