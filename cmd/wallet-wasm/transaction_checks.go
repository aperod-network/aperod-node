package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// verifyBuiltPayment checks transaction-bound payment facts before the WASM
// API exposes them, including the one-time recipient derivation using the
// private proof material retained transiently by TxBuilder.
func verifyBuiltPayment(result *core.BuildResult, amount uint64, recipient crypto.Address) (string, error) {
	if result == nil {
		return "", fmt.Errorf("transaction build returned no result")
	}
	defer result.DiscardPaymentRecipientProof()
	tx := &result.Tx
	if int(result.PayOutIdx) < 0 || result.PayOutIdx >= len(tx.Outputs) {
		return "", fmt.Errorf("payment output index %d is outside transaction outputs", result.PayOutIdx)
	}

	payment := tx.Outputs[result.PayOutIdx]
	if _, _, _, err := crypto.DecodeAddress(recipient); err != nil {
		return "", fmt.Errorf("invalid recipient address: %w", err)
	}
	if payment.TxPubKey == (crypto.Point32{}) || payment.OneTimePub == (crypto.Point32{}) {
		return "", fmt.Errorf("payment output is missing one-time recipient keys")
	}
	if err := result.VerifyPaymentRecipient(recipient); err != nil {
		return "", fmt.Errorf("verify payment recipient ownership: %w", err)
	}

	opened, err := crypto.Commit(amount, result.PayBlind)
	if err != nil {
		return "", fmt.Errorf("open payment output commitment: %w", err)
	}
	if opened != payment.AmountCommit {
		return "", fmt.Errorf("payment output commitment does not open to requested amount")
	}
	if tx.Fee != result.TotalFee {
		return "", fmt.Errorf("transaction fee %d does not match build fee %d", tx.Fee, result.TotalFee)
	}

	// Match the canonical transaction ID across the serialized transaction
	// representation used by the WASM response. Consensus transaction IDs are
	// defined by Transaction.Hash; JSON itself is not a consensus encoding.
	serialized, err := json.Marshal(tx)
	if err != nil {
		return "", fmt.Errorf("serialize built transaction: %w", err)
	}
	var roundTripped core.Transaction
	if err := json.Unmarshal(serialized, &roundTripped); err != nil {
		return "", fmt.Errorf("deserialize built transaction: %w", err)
	}
	hash := tx.Hash()
	if roundTripped.Hash() != hash {
		return "", fmt.Errorf("transaction hash changed after serialization")
	}

	return hex.EncodeToString(hash[:]), nil
}
