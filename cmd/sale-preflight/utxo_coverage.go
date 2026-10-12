package main

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// This is material coverage, not a supply or spendability calculation. Neither
// an optional amount field nor a public spent-output index opens a confidential
// commitment or identifies the owner's hidden real input.
type utxoMaterialCoverage struct {
	Scope                  string `json:"scope"`
	Rows                   uint64 `json:"stored_output_rows"`
	AmountMetadataPresent  uint64 `json:"amount_metadata_present_rows"`
	AmountMetadataMissing  uint64 `json:"amount_metadata_missing_rows"`
	EncryptedAmountRows    uint64 `json:"encrypted_amount_rows"`
	ZeroTxPublicKeyRows    uint64 `json:"zero_tx_public_key_rows"`
	ProtocolLockedRows     uint64 `json:"protocol_locked_rows"`
	GlobalBalanceVerified  bool   `json:"global_net_balance_verified"`
	OwnerSpentnessVerified bool   `json:"owner_spentness_verified"`
}

func reviewUTXOMaterial(iterate func(func(*store.StoredUTXO) error) error) (utxoMaterialCoverage, error) {
	report := utxoMaterialCoverage{
		Scope: "complete stored output namespace; may include spent outputs; metadata is not a verified amount opening",
	}
	err := iterate(func(u *store.StoredUTXO) error {
		if u == nil {
			return fmt.Errorf("UTXO material review: nil output")
		}
		report.Rows++
		if u.AmountNAPRO == 0 {
			// Absent metadata is unknown, not evidence of a zero-value output.
			report.AmountMetadataMissing++
		} else {
			report.AmountMetadataPresent++
		}
		if u.EncAmount != ([8]byte{}) {
			report.EncryptedAmountRows++
		}
		if u.TxPubKey == (crypto.Point32{}) {
			report.ZeroTxPublicKeyRows++
		}
		if u.ProtocolLocked {
			report.ProtocolLockedRows++
		}
		return nil
	})
	if err != nil {
		return utxoMaterialCoverage{}, fmt.Errorf("UTXO material review: %w", err)
	}
	return report, nil
}
