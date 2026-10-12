package core

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/aperod/aperod/crypto"
)

const TxVersionSale TxVersion = 11
const saleExtraSize = 280

var salePrefix = []byte{'A', 'P', 'S', 'A', 'L', 'E', 1, 0}

// Recipient addresses are deliberately absent from the public authorization.
// The trusted issuer signs the exact confidential output, and the buyer must
// discover that output locally before an off-chain order can be completed.
type SaleAuthorization struct {
	Genesis   crypto.Hash32
	Height    uint64
	Parent    crypto.Hash32
	Order     crypto.Hash32
	Payment   crypto.Hash32
	Amount    uint64
	Opening   crypto.ValueAuthorization
	Signature [64]byte
}

func (tx *Transaction) IsSale() bool { return tx != nil && tx.Version == TxVersionSale }

func (a SaleAuthorization) extra() []byte {
	out := make([]byte, saleExtraSize)
	copy(out[:8], salePrefix)
	copy(out[8:40], a.Genesis[:])
	binary.BigEndian.PutUint64(out[40:48], a.Height)
	copy(out[48:80], a.Parent[:])
	copy(out[80:112], a.Order[:])
	copy(out[112:144], a.Payment[:])
	binary.BigEndian.PutUint64(out[144:152], a.Amount)
	copy(out[152:216], a.Opening[:])
	copy(out[216:], a.Signature[:])
	return out
}

func (tx *Transaction) SaleAuthorization() (*SaleAuthorization, error) {
	if !tx.IsSale() || len(tx.Extra) != saleExtraSize || !bytes.Equal(tx.Extra[:8], salePrefix) ||
		len(tx.Inputs) != 0 || len(tx.Outputs) != 1 || tx.Fee != 0 ||
		tx.FeeCommit != (crypto.Commitment{}) || tx.AVM != nil ||
		len(tx.RangeProofs) != 0 || len(tx.Signatures) != 0 || len(tx.CLSAGSignatures) != 0 {
		return nil, fmt.Errorf("sale: noncanonical transaction structure")
	}
	var a SaleAuthorization
	copy(a.Genesis[:], tx.Extra[8:40])
	a.Height = binary.BigEndian.Uint64(tx.Extra[40:48])
	copy(a.Parent[:], tx.Extra[48:80])
	copy(a.Order[:], tx.Extra[80:112])
	copy(a.Payment[:], tx.Extra[112:144])
	a.Amount = binary.BigEndian.Uint64(tx.Extra[144:152])
	copy(a.Opening[:], tx.Extra[152:216])
	copy(a.Signature[:], tx.Extra[216:])
	if a.Height == 0 || a.Amount == 0 || a.Genesis == (crypto.Hash32{}) ||
		a.Parent == (crypto.Hash32{}) || a.Order == (crypto.Hash32{}) || a.Payment == (crypto.Hash32{}) {
		return nil, fmt.Errorf("sale: empty authorization field")
	}
	if err := crypto.ValidateAuthorizedOutputPoints(tx.Outputs[0].OneTimePub, tx.Outputs[0].TxPubKey); err != nil {
		return nil, err
	}
	if err := crypto.VerifyValueAuthorization(a.Amount, tx.Outputs[0].AmountCommit, a.Opening); err != nil {
		return nil, err
	}
	return &a, nil
}

func (tx *Transaction) SaleSigningMessage() crypto.Hash32 {
	unsigned := *tx
	unsigned.Extra = append([]byte(nil), tx.Extra...)
	if len(unsigned.Extra) == saleExtraSize {
		clear(unsigned.Extra[216:])
	}
	hash := unsigned.Hash()
	return crypto.HashBytes([]byte("aperod/sale/confidential-output-authorization/v1"), hash[:])
}

func BuildSaleTransaction(a SaleAuthorization, recipient crypto.Address,
	network crypto.NetworkByte, key interface {
		Sign(crypto.Hash32) ([]byte, error)
	}) (*Transaction, error) {
	if key == nil {
		return nil, fmt.Errorf("sale: authority signer unavailable")
	}
	actual, _, _, err := crypto.DecodeAddress(recipient)
	if err != nil || actual != network {
		return nil, fmt.Errorf("sale: recipient network mismatch")
	}
	output, blind, _, err := txBuildOutputWithEphemeral(recipient, a.Amount, nil)
	if err != nil {
		return nil, err
	}
	commit, proof, err := crypto.ProveValueAuthorization(a.Amount, blind)
	if err != nil {
		return nil, err
	}
	if commit != output.AmountCommit {
		return nil, fmt.Errorf("sale: output opening mismatch")
	}
	a.Opening = proof
	a.Signature = [64]byte{}
	tx := &Transaction{Version: TxVersionSale, Outputs: []Output{output}, Extra: a.extra()}
	signature, err := key.Sign(tx.SaleSigningMessage())
	if err != nil {
		return nil, err
	}
	if len(signature) != 64 {
		return nil, fmt.Errorf("sale: invalid authority signature")
	}
	copy(a.Signature[:], signature)
	tx.Extra = a.extra()
	if _, err = tx.SaleAuthorization(); err != nil {
		return nil, err
	}
	return tx, nil
}
