package wallet

// multisig.go — 2-of-3 Shamir threshold signature scheme (block 3.3).
//
// Model: Trusted Dealer.  One party runs MultisigSetup to split the combined
// spend key into 3 shares using a degree-1 polynomial over the Ed25519 scalar
// field.  Any 2 of the 3 shares suffice to reconstruct the spend key via
// Lagrange interpolation.
//
// Signing flow:
//  1. Each participant calls PartialSign with their share and the co-signer's index.
//  2. The 2 PartialSig values are combined with CombinePartials → spend key scalar.
//  3. The caller uses the reconstructed scalar to sign with crypto.MLSAGSign.
//
// Security note:
//   This implementation is correct but uses a trusted-dealer setup.  A production
//   deployment should replace the dealer with a Distributed Key Generation (DKG)
//   protocol so that no single party ever holds the combined secret.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"

	"filippo.io/edwards25519"
	"github.com/aperod/aperod/crypto"
)

// edOrder is the prime order of the Ed25519 group.
// l = 2^252 + 27742317777372353535851937790883648493
var edOrder, _ = new(big.Int).SetString(
	"7237005577332262213973186563042994240857116359379907606001950938285454250989", 10)

// ─── Public types ─────────────────────────────────────────────────────────────

// MultisigShare is one participant's secret in the 2-of-3 scheme.
type MultisigShare struct {
	Index    int                // participant index (1, 2, or 3)
	ShareKey crypto.Scalar32    // Shamir share of the combined spend key
	SpendPub crypto.Point32     // combined (aggregated) spend public key
	ViewPriv crypto.Scalar32    // shared view private key (same for all)
	ViewPub  crypto.Point32     // shared view public key (same for all)
	Network  crypto.NetworkByte // network identifier
}

// MultisigAddress is the public address shared by all 3 participants.
type MultisigAddress struct {
	SpendPub crypto.Point32
	ViewPub  crypto.Point32
	Network  crypto.NetworkByte
	Address  crypto.Address
}

// PartialSig is a Lagrange-weighted contribution from one participant.
// Two PartialSigs with distinct indices can be combined to recover the spend key.
type PartialSig struct {
	Index int             // participant index (1, 2, or 3)
	Value crypto.Scalar32 // Lagrange-weighted share
}

// ─── Setup ────────────────────────────────────────────────────────────────────

// MultisigSetup creates a 2-of-3 threshold wallet using a trusted-dealer model.
// Returns 3 participant shares and the shared multisig address.
//
// The combined spend private key is split so that any 2 of 3 shares can
// reconstruct it.  The view key is the same for all participants (needed for
// scanning incoming transactions).
func MultisigSetup(net crypto.NetworkByte) ([]MultisigShare, MultisigAddress, error) {
	// Combined spend keypair
	spendPriv, spendPub, err := genScalarKeyPair()
	if err != nil {
		return nil, MultisigAddress{}, fmt.Errorf("generate spend key: %w", err)
	}
	// Shared view keypair
	viewPriv, viewPub, err := genScalarKeyPair()
	if err != nil {
		return nil, MultisigAddress{}, fmt.Errorf("generate view key: %w", err)
	}
	// Shamir 2-of-3 split of spend key
	shareKeys, err := shamirSplit2of3(spendPriv)
	if err != nil {
		return nil, MultisigAddress{}, fmt.Errorf("shamir split: %w", err)
	}

	msAddr := MultisigAddress{
		SpendPub: spendPub,
		ViewPub:  viewPub,
		Network:  net,
		Address:  crypto.EncodeAddress(net, spendPub, viewPub),
	}
	shares := make([]MultisigShare, 3)
	for i := range shares {
		shares[i] = MultisigShare{
			Index:    i + 1,
			ShareKey: shareKeys[i],
			SpendPub: spendPub,
			ViewPriv: viewPriv,
			ViewPub:  viewPub,
			Network:  net,
		}
	}
	return shares, msAddr, nil
}

// ─── Signing ─────────────────────────────────────────────────────────────────

// PartialSign computes the Lagrange-weighted share for this participant.
// coSignerIndex must be the index of the other participant who will co-sign
// (both must call PartialSign with each other's index).
func PartialSign(share MultisigShare, coSignerIndex int) (PartialSig, error) {
	if share.Index == coSignerIndex {
		return PartialSig{}, errors.New("multisig: own index and co-signer index must differ")
	}
	if share.Index < 1 || share.Index > 3 {
		return PartialSig{}, fmt.Errorf("multisig: share index %d out of range [1,3]", share.Index)
	}
	if coSignerIndex < 1 || coSignerIndex > 3 {
		return PartialSig{}, fmt.Errorf("multisig: co-signer index %d out of range [1,3]", coSignerIndex)
	}

	siBig := scalar32ToBig(share.ShareKey)
	LiBig := lagrangeCoeff(share.Index, coSignerIndex)

	weighted := new(big.Int).Mul(siBig, LiBig)
	weighted.Mod(weighted, edOrder)

	var val crypto.Scalar32
	bigToScalar32(weighted, &val)
	return PartialSig{Index: share.Index, Value: val}, nil
}

// CombinePartials reconstructs the spend private key from 2 partial signatures.
// The partials must have distinct indices.
func CombinePartials(partials []PartialSig) (crypto.Scalar32, error) {
	if len(partials) != 2 {
		return crypto.Scalar32{}, fmt.Errorf("multisig: need exactly 2 partials, got %d", len(partials))
	}
	if partials[0].Index == partials[1].Index {
		return crypto.Scalar32{}, fmt.Errorf("multisig: duplicate partial indices: %d", partials[0].Index)
	}
	a := scalar32ToBig(partials[0].Value)
	b := scalar32ToBig(partials[1].Value)
	sum := new(big.Int).Add(a, b)
	sum.Mod(sum, edOrder)
	var result crypto.Scalar32
	bigToScalar32(sum, &result)
	return result, nil
}

// CombineShares reconstructs the spend private key directly from 2 raw shares
// (without the intermediate PartialSign step).
// Useful for offline/batch signing where both parties trust each other with their shares.
func CombineShares(shares []MultisigShare) (crypto.Scalar32, error) {
	if len(shares) != 2 {
		return crypto.Scalar32{}, fmt.Errorf("multisig: need exactly 2 shares, got %d", len(shares))
	}
	if shares[0].Index == shares[1].Index {
		return crypto.Scalar32{}, fmt.Errorf("multisig: duplicate share indices: %d", shares[0].Index)
	}
	xi, xj := shares[0].Index, shares[1].Index
	si := scalar32ToBig(shares[0].ShareKey)
	sj := scalar32ToBig(shares[1].ShareKey)

	Li := lagrangeCoeff(xi, xj)
	Lj := lagrangeCoeff(xj, xi)

	term1 := new(big.Int).Mul(si, Li)
	term2 := new(big.Int).Mul(sj, Lj)
	secret := new(big.Int).Add(term1, term2)
	secret.Mod(secret, edOrder)

	var result crypto.Scalar32
	bigToScalar32(secret, &result)
	return result, nil
}

// SpendPublic derives the public spend key from a reconstructed spend scalar.
// Use this to verify that the reconstructed key matches the multisig address.
func SpendPublic(spendScalar crypto.Scalar32) (crypto.Point32, error) {
	s, err := edwards25519.NewScalar().SetCanonicalBytes(spendScalar[:])
	if err != nil {
		return crypto.Point32{}, fmt.Errorf("invalid spend scalar: %w", err)
	}
	pt := (&edwards25519.Point{}).ScalarBaseMult(s)
	var pub crypto.Point32
	copy(pub[:], pt.Bytes())
	return pub, nil
}

// ─── Internal helpers ─────────────────────────────────────────────────────────

// genScalarKeyPair generates a random Ed25519 scalar and its base-point multiple.
func genScalarKeyPair() (crypto.Scalar32, crypto.Point32, error) {
	var buf [64]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return crypto.Scalar32{}, crypto.Point32{}, err
	}
	s, err := edwards25519.NewScalar().SetUniformBytes(buf[:])
	if err != nil {
		return crypto.Scalar32{}, crypto.Point32{}, err
	}
	pt := (&edwards25519.Point{}).ScalarBaseMult(s)
	var priv crypto.Scalar32
	copy(priv[:], s.Bytes())
	var pub crypto.Point32
	copy(pub[:], pt.Bytes())
	return priv, pub, nil
}

// shamirSplit2of3 splits secret into 3 shares using f(x) = secret + a·x (mod order).
// Shares: s_i = f(i) for i = 1, 2, 3.  Any 2 shares reconstruct f(0) = secret.
func shamirSplit2of3(secret crypto.Scalar32) ([]crypto.Scalar32, error) {
	sBig := scalar32ToBig(secret)

	// Random coefficient a (from a uniform scalar)
	var buf [64]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return nil, err
	}
	uScalar, err := edwards25519.NewScalar().SetUniformBytes(buf[:])
	if err != nil {
		return nil, err
	}
	// Convert the uniform scalar to big.Int
	aBig := bytesToBigInt(uScalar.Bytes()) // little-endian → big.Int

	shares := make([]crypto.Scalar32, 3)
	for i := 1; i <= 3; i++ {
		xi := big.NewInt(int64(i))
		// f(i) = secret + a·i  mod order
		fi := new(big.Int).Mul(aBig, xi)
		fi.Add(fi, sBig)
		fi.Mod(fi, edOrder)
		var sh crypto.Scalar32
		bigToScalar32(fi, &sh)
		shares[i-1] = sh
	}
	return shares, nil
}

// lagrangeCoeff returns L_xi(0) for a 2-point interpolation with the other point at xj.
// L_xi(0) = (0 - xj) / (xi - xj) = (−xj) · (xi − xj)^{−1}  mod order
func lagrangeCoeff(xi, xj int) *big.Int {
	num := new(big.Int).SetInt64(int64(-xj))
	num.Mod(num, edOrder)

	denom := new(big.Int).SetInt64(int64(xi - xj))
	denom.Mod(denom, edOrder)

	inv := new(big.Int).ModInverse(denom, edOrder)

	result := new(big.Int).Mul(num, inv)
	result.Mod(result, edOrder)
	return result
}

// scalar32ToBig converts a little-endian Scalar32 to big.Int.
func scalar32ToBig(s crypto.Scalar32) *big.Int {
	return bytesToBigInt(s[:])
}

// bytesToBigInt interprets b as a little-endian unsigned integer.
func bytesToBigInt(b []byte) *big.Int {
	rev := make([]byte, len(b))
	for i, v := range b {
		rev[len(b)-1-i] = v
	}
	return new(big.Int).SetBytes(rev)
}

// bigToScalar32 writes n as a little-endian 32-byte Scalar32.
func bigToScalar32(n *big.Int, out *crypto.Scalar32) {
	be := make([]byte, 32)
	n.FillBytes(be) // big-endian, zero-padded
	for i := 0; i < 32; i++ {
		out[i] = be[31-i] // reverse to little-endian
	}
}
