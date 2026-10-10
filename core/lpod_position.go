// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package core

import (
	"crypto/sha512"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"reflect"

	"filippo.io/edwards25519"
	"github.com/aperod/aperod/crypto"
)

// LPOD payouts use standard wallet-scannable stealth outputs, with public,
// deterministic ephemeral scalars bound to the unpredictable canonical parent.
// This avoids pre-creating a future height-only mint key to poison its key image.
// Amounts and recipients remain public protocol information, not private sends.
func BuildLPoDPayoutOutput(address crypto.Address, amount, height uint64, parent, digest crypto.Hash32) (Output, error) {
	if amount == 0 {
		return Output{}, fmt.Errorf("lpod: zero payout")
	}
	_, spend, view, err := crypto.DecodeAddress(address)
	if err != nil {
		return Output{}, err
	}
	h := sha512.New()
	h.Write([]byte("aperod/lpod/payout-ephemeral/v1"))
	h.Write(parent[:])
	h.Write(digest[:])
	var hb [8]byte
	binary.LittleEndian.PutUint64(hb[:], height)
	h.Write(hb[:])
	h.Write([]byte(address))
	scalar, err := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	if err != nil {
		return Output{}, err
	}
	var r [32]byte
	copy(r[:], scalar.Bytes())
	if r == ([32]byte{}) {
		r[0] = 1
	}
	stealth, err := crypto.CreateStealthOutputFromEphemeralBytes(spend, view, r)
	if err != nil {
		return Output{}, err
	}
	blind, err := crypto.DeterministicPaymentBlind(stealth.HsScalar, amount)
	if err != nil {
		return Output{}, err
	}
	commit, err := crypto.Commit(amount, blind)
	if err != nil {
		return Output{}, err
	}
	return Output{OneTimePub: stealth.OneTimePub, TxPubKey: stealth.TxPubKey, AmountCommit: commit,
		EncAmount: EncryptAmount(amount, &stealth.HsScalar)}, nil
}

const TxVersionLPoDPosition TxVersion = 9
const TxVersionLPoDPayout TxVersion = 10
const LPoDDeposit uint8 = 1
const LPoDWithdraw uint8 = 2

// Position ownership is independent of validator keys. OwnerProof proves knowledge
// of the beneficiary's wallet spend scalar; DepositProof proves knowledge of the
// source one-time scalar, amount opening AND its correctly linked key image.
type LPoDPositionAction struct {
	Action      uint8
	Genesis     crypto.Hash32
	PositionID  crypto.Hash32
	Vault       crypto.Point32
	Nonce       uint64
	Beneficiary crypto.Address
	Owner       crypto.Point32
	SourceTx    crypto.Hash32
	SourceIndex uint32
	SourcePub   crypto.Point32
	Amount      uint64
	Blind       crypto.BlindFactor
	// Zero requests all remaining principal; a positive value requests a
	// partial return. Amount remains the immutable source/deposit opening.
	WithdrawAmount uint64 `json:"withdraw_amount_napro,string,omitempty"`
}

func (a LPoDPositionAction) Message() crypto.Hash32 {
	b, _ := json.Marshal(a)
	return crypto.HashBytes([]byte("aperod/lpod/position/v1"), b)
}

func LPoDPositionID(genesis, source crypto.Hash32, index uint32) crypto.Hash32 {
	b, _ := json.Marshal(struct {
		Genesis, Source crypto.Hash32
		Index           uint32
	}{genesis, source, index})
	return crypto.HashBytes([]byte("aperod/lpod/position-id/v1"), b)
}

func LPoDOwnershipRing(pub crypto.Point32) []crypto.RingMember {
	r := make([]crypto.RingMember, crypto.RingSize)
	r[0] = pub
	for i := 1; i < len(r); i++ {
		data := append([]byte("aperod/lpod/direct-proof-decoy/v1"), pub[:]...)
		data = append(data, byte(i))
		copy(r[i][:], crypto.HashToCurvePoint(data).Bytes())
	}
	return r
}

func (tx *Transaction) IsLPoDPosition() bool { return tx != nil && tx.Version == TxVersionLPoDPosition }
func (tx *Transaction) IsLPoDPayout() bool   { return tx != nil && tx.Version == TxVersionLPoDPayout }

func (tx *Transaction) LPoDPositionAction() (*LPoDPositionAction, error) {
	if !tx.IsLPoDPosition() || len(tx.Extra) > 4096 || len(tx.Extra) == 0 {
		return nil, fmt.Errorf("lpod: invalid position payload")
	}
	var a LPoDPositionAction
	if err := json.Unmarshal(tx.Extra, &a); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(a)
	if string(canonical) != string(tx.Extra) {
		return nil, fmt.Errorf("lpod: noncanonical position encoding")
	}
	if a.Genesis == (crypto.Hash32{}) || a.PositionID != LPoDPositionID(a.Genesis, a.SourceTx, a.SourceIndex) ||
		(a.Action != LPoDDeposit && a.Action != LPoDWithdraw) || a.Amount == 0 {
		return nil, fmt.Errorf("lpod: invalid action or position identity")
	}
	_, spend, _, err := crypto.DecodeAddress(a.Beneficiary)
	if err != nil || spend != a.Owner {
		return nil, fmt.Errorf("lpod: beneficiary does not belong to signing owner")
	}
	if len(tx.Outputs) != 0 || tx.Fee != 0 || tx.FeeCommit != (crypto.Commitment{}) || len(tx.RangeProofs) != 0 ||
		len(tx.CLSAGSignatures) != 0 || tx.AVM != nil {
		return nil, fmt.Errorf("lpod: positions cannot mint or contain unaccounted fields")
	}
	expectedSignatures := 1
	if a.Action == LPoDDeposit {
		expectedSignatures = 2
	}
	if len(tx.Signatures) != expectedSignatures {
		return nil, fmt.Errorf("lpod: missing ownership proof")
	}
	ok, err := crypto.MLSAGVerifyV4(a.Message(), LPoDOwnershipRing(a.Owner), crypto.Commitment(a.Owner), 0, tx.Signatures[0])
	if err != nil || !ok {
		return nil, fmt.Errorf("lpod: owner signature rejected")
	}
	if a.Action == LPoDWithdraw {
		if len(tx.Inputs) != 0 || a.Nonce == 0 {
			return nil, fmt.Errorf("lpod: invalid withdrawal")
		}
		return &a, nil
	}
	if a.Nonce != 0 || a.WithdrawAmount != 0 || len(tx.Inputs) != 1 || tx.Signatures[1] == nil {
		return nil, fmt.Errorf("lpod: invalid deposit")
	}
	commit, err := crypto.Commit(a.Amount, a.Blind)
	if err != nil {
		return nil, err
	}
	expected := RingInput{Ring: LPoDOwnershipRing(a.SourcePub), AmountCommit: commit, KeyImage: tx.Signatures[1].KeyImage}
	if !reflect.DeepEqual(expected, tx.Inputs[0]) {
		return nil, fmt.Errorf("lpod: input differs from ownership opening")
	}
	ok, err = crypto.MLSAGVerifyV4(a.Message(), expected.Ring, commit, 0, tx.Signatures[1])
	if err != nil || !ok {
		return nil, fmt.Errorf("lpod: source ownership/key-image/opening proof rejected")
	}
	return &a, nil
}

func BuildLPoDPositionTx(a LPoDPositionAction, owner crypto.Scalar32, source crypto.Scalar32) (*Transaction, error) {
	body, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	sig, err := crypto.MLSAGSignV4(a.Message(), LPoDOwnershipRing(a.Owner), 0, owner, crypto.BlindFactor(owner), 0)
	if err != nil {
		return nil, err
	}
	tx := &Transaction{Version: TxVersionLPoDPosition, Extra: body, Signatures: []*crypto.MLSAGSignature{sig}}
	if a.Action == LPoDDeposit {
		proof, err := crypto.MLSAGSignV4(a.Message(), LPoDOwnershipRing(a.SourcePub), 0, source, a.Blind, a.Amount)
		if err != nil {
			return nil, err
		}
		commit, err := crypto.Commit(a.Amount, a.Blind)
		if err != nil {
			return nil, err
		}
		tx.Signatures = append(tx.Signatures, proof)
		tx.Inputs = []RingInput{{Ring: LPoDOwnershipRing(a.SourcePub), AmountCommit: commit, KeyImage: proof.KeyImage}}
	}
	if _, err := tx.LPoDPositionAction(); err != nil {
		return nil, err
	}
	return tx, nil
}

type LPoDPayoutAuthorization struct {
	Leader crypto.Address `json:"leader"`
	Digest crypto.Hash32  `json:"digest"`
}

func (tx *Transaction) LPoDPayoutAuthorization() (*LPoDPayoutAuthorization, error) {
	if !tx.IsLPoDPayout() || len(tx.Inputs) != 0 || len(tx.Outputs) > 4097 || tx.Fee != 0 ||
		tx.FeeCommit != (crypto.Commitment{}) || len(tx.Signatures) != 0 || len(tx.CLSAGSignatures) != 0 ||
		len(tx.RangeProofs) != 0 || tx.AVM != nil || len(tx.Extra) > 512 {
		return nil, fmt.Errorf("lpod: malformed payout")
	}
	var a LPoDPayoutAuthorization
	if err := json.Unmarshal(tx.Extra, &a); err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(a)
	if string(canonical) != string(tx.Extra) {
		return nil, fmt.Errorf("lpod: noncanonical payout authorization")
	}
	if _, _, _, err := crypto.DecodeAddress(a.Leader); err != nil {
		return nil, err
	}
	return &a, nil
}
