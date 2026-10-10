package core

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestAuthorizedRewardValidationBindsEveryConsensusField(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	otherValidatorPriv, otherValidatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	_ = otherValidatorPriv
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	otherRecipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}

	const (
		height = uint64(1_234)
		amount = uint64(500_000_123)
	)
	parentHash := crypto.HashBytes([]byte("authorized-reward-parent"))
	address := crypto.AddressFromKeys(crypto.MainnetByte, recipient)
	tx, err := BuildAuthorizedRewardTx(address, amount, height, parentHash, validatorPriv)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ValidateAuthorizedRewardTx(tx, height, parentHash, validatorPub); err != nil {
		t.Fatalf("valid authorized reward rejected: %v", err)
	}

	t.Run("replay at another height", func(t *testing.T) {
		if _, err := ValidateAuthorizedRewardTx(tx, height+1, parentHash, validatorPub); err == nil {
			t.Fatal("reward authorization replayed at another height")
		}
	})

	t.Run("replay on another parent", func(t *testing.T) {
		otherParent := crypto.HashBytes([]byte("another-parent"))
		if _, err := ValidateAuthorizedRewardTx(tx, height, otherParent, validatorPub); err == nil {
			t.Fatal("reward authorization replayed on another parent")
		}
	})

	t.Run("wrong proposer", func(t *testing.T) {
		if _, err := ValidateAuthorizedRewardTx(tx, height, parentHash, otherValidatorPub); err == nil {
			t.Fatal("reward authorization accepted under another validator")
		}
	})

	t.Run("wrong amount", func(t *testing.T) {
		tampered := cloneRewardTx(tx)
		auth, err := DecodeRewardAuthorization(tampered.Extra)
		if err != nil {
			t.Fatal(err)
		}
		auth.Amount++
		if err := auth.Sign(validatorPriv); err != nil {
			t.Fatal(err)
		}
		tampered.Extra, err = EncodeRewardAuthorization(auth)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateAuthorizedRewardTx(tampered, height, parentHash, validatorPub); err == nil {
			t.Fatal("authorization amount was not bound to the reward output")
		}
	})

	t.Run("wrong recipient", func(t *testing.T) {
		tampered := cloneRewardTx(tx)
		auth, err := DecodeRewardAuthorization(tampered.Extra)
		if err != nil {
			t.Fatal(err)
		}
		auth.RecipientSpendPub = otherRecipient.Spend.Public
		auth.RecipientViewPub = otherRecipient.View.Public
		if err := auth.Sign(validatorPriv); err != nil {
			t.Fatal(err)
		}
		tampered.Extra, err = EncodeRewardAuthorization(auth)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateAuthorizedRewardTx(tampered, height, parentHash, validatorPub); err == nil {
			t.Fatal("authorization recipient was not bound to the reward output")
		}
	})

	t.Run("unsigned reward id tamper", func(t *testing.T) {
		tampered := cloneRewardTx(tx)
		auth, err := DecodeRewardAuthorization(tampered.Extra)
		if err != nil {
			t.Fatal(err)
		}
		auth.RewardID[0] ^= 0xff
		tampered.Extra, err = EncodeRewardAuthorization(auth)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ValidateAuthorizedRewardTx(tampered, height, parentHash, validatorPub); err == nil {
			t.Fatal("tampered reward id accepted")
		}
	})
}

func cloneRewardTx(tx *Transaction) *Transaction {
	cloned := *tx
	cloned.Inputs = append([]RingInput(nil), tx.Inputs...)
	cloned.Outputs = append([]Output(nil), tx.Outputs...)
	cloned.Extra = append([]byte(nil), tx.Extra...)
	return &cloned
}
