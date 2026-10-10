// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package core

import (
	"bytes"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

const TxVersionLPoD TxVersion = 8

var lpodPrefix = []byte("APRO-LPOD-CHECKPOINT\x01")

// LPoDCheckpointTx commits a protocol ledger transition but creates no coins.
// Spendable leader, Angels and principal outputs use the native v10 payout plan.
func LPoDCheckpointTx(digest crypto.Hash32) Transaction {
	return Transaction{Version: TxVersionLPoD, Extra: append(append([]byte{}, lpodPrefix...), digest[:]...)}
}

func (tx *Transaction) IsLPoD() bool { return tx != nil && tx.Version == TxVersionLPoD }

func (tx *Transaction) validateLPoD() error {
	if len(tx.Inputs) != 0 || len(tx.Outputs) != 0 || tx.Fee != 0 ||
		tx.FeeCommit != (crypto.Commitment{}) || len(tx.RangeProofs) != 0 ||
		len(tx.Signatures) != 0 || len(tx.CLSAGSignatures) != 0 || tx.AVM != nil ||
		len(tx.Extra) != len(lpodPrefix)+32 || !bytes.Equal(tx.Extra[:len(lpodPrefix)], lpodPrefix) {
		return fmt.Errorf("lpod: malformed checkpoint transaction")
	}
	return nil
}

func (tx *Transaction) LPoDCheckpointDigest() (crypto.Hash32, error) {
	var digest crypto.Hash32
	if !tx.IsLPoD() {
		return digest, fmt.Errorf("lpod: not a checkpoint")
	}
	if err := tx.validateLPoD(); err != nil {
		return digest, err
	}
	copy(digest[:], tx.Extra[len(lpodPrefix):])
	return digest, nil
}
