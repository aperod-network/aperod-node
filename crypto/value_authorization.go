package crypto

import (
	"crypto/sha512"
	"encoding/binary"
	"fmt"

	"filippo.io/edwards25519"
)

// ValueAuthorization proves C - amount*H = blind*G without disclosing blind.
// It is a value-opening proof, not permission to issue currency. The separate
// protocol authority must sign the exact output and allocation debit.
type ValueAuthorization [64]byte

func valueAuthorizationChallenge(amount uint64, commit Commitment, nonce []byte) *edwards25519.Scalar {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], amount)
	h := sha512.New()
	h.Write([]byte("aperod/sale/known-value-opening/v1"))
	h.Write(n[:])
	h.Write(commit[:])
	h.Write(nonce)
	s, _ := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	return s
}

func ProveValueAuthorization(amount uint64, blind BlindFactor) (Commitment, ValueAuthorization, error) {
	commit, err := Commit(amount, blind)
	if err != nil {
		return Commitment{}, ValueAuthorization{}, err
	}
	r, err := ScalarFromBytes(blind[:])
	if err != nil {
		return Commitment{}, ValueAuthorization{}, err
	}
	nonce, err := NewBlindFactor()
	if err != nil {
		return Commitment{}, ValueAuthorization{}, err
	}
	k, err := ScalarFromBytes(nonce[:])
	if err != nil {
		return Commitment{}, ValueAuthorization{}, err
	}
	R := new(edwards25519.Point).ScalarBaseMult(k)
	challenge := valueAuthorizationChallenge(amount, commit, R.Bytes())
	response := new(edwards25519.Scalar).MultiplyAdd(challenge, r, k)
	var proof ValueAuthorization
	copy(proof[:32], R.Bytes())
	copy(proof[32:], response.Bytes())
	return commit, proof, nil
}

func VerifyValueAuthorization(amount uint64, commit Commitment, proof ValueAuthorization) error {
	C, err := checkedCLSAGSubgroupPoint(Point32(commit), "sale commitment", true)
	if err != nil {
		return err
	}
	var Rbytes Point32
	copy(Rbytes[:], proof[:32])
	R, err := checkedCLSAGPrimePoint(Rbytes, "sale opening nonce")
	if err != nil {
		return err
	}
	s, err := ScalarFromBytes(proof[32:])
	if err != nil {
		return err
	}
	difference := new(edwards25519.Point).Subtract(C,
		new(edwards25519.Point).ScalarMult(scalarFromUint64(amount), hGenerator))
	challenge := valueAuthorizationChallenge(amount, commit, proof[:32])
	want := new(edwards25519.Point).Add(R, new(edwards25519.Point).ScalarMult(challenge, difference))
	if new(edwards25519.Point).ScalarBaseMult(s).Equal(want) != 1 {
		return fmt.Errorf("sale: output value differs from authorized debit")
	}
	return nil
}

func ValidateAuthorizedOutputPoints(pub, ephemeral Point32) error {
	if _, err := checkedCLSAGPrimePoint(pub, "sale recipient output"); err != nil {
		return err
	}
	_, err := checkedCLSAGPrimePoint(ephemeral, "sale ephemeral output")
	return err
}
