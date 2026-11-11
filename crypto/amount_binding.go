package crypto

import (
	"crypto/sha512"
	"fmt"
	"sync"

	"filippo.io/edwards25519"
)

const AmountCipherChunkCount = 4
const amountCipherChunkBits = 16

// AmountCipherChunk encrypts one 16-bit digit under the output's one-time
// public key. Its range proof and equality proof bind the ciphertext to the
// corresponding Pedersen commitment.
type AmountCipherChunk struct {
	Commitment Commitment
	R          Point32
	E          Point32
	RangeProof *RangeProof
	LinkProof  AmountLinkProof
}

// AmountBinding is the version-13/14 output amount encoding. The weighted chunk
// commitments must sum to the enclosing output commitment.
type AmountBinding struct {
	Chunks [AmountCipherChunkCount]AmountCipherChunk
}

// AmountLinkProof is a Fiat-Shamir proof that one value opens Commitment and
// is also encrypted in (R,E) under the output one-time public key.
type AmountLinkProof struct {
	TCommit Point32
	TCipher Point32
	TRandom Point32
	ZBlind  [32]byte
	ZValue  [32]byte
	ZRandom [32]byte
}

// CreateAmountBinding creates four independently blinded 16-bit commitments,
// range proofs, and ElGamal ciphertexts. The weighted chunk blind sum matches
// outputBlind, so the aggregate commitment is identical to the legacy output
// commitment and remains compatible with RingCT balance verification.
func CreateAmountBinding(value uint64, outputBlind BlindFactor, outputPub Point32) (*AmountBinding, Commitment, error) {
	pub, err := PointFromBytes(outputPub[:])
	if err != nil || pub.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return nil, Commitment{}, fmt.Errorf("amount binding: invalid output public key")
	}
	targetBlind, err := ScalarFromBytes(outputBlind[:])
	if err != nil {
		return nil, Commitment{}, fmt.Errorf("amount binding: invalid output blind: %w", err)
	}

	var blinds [AmountCipherChunkCount]BlindFactor
	weightedBlind := edwards25519.NewScalar()
	for i := 0; i < AmountCipherChunkCount-1; i++ {
		blinds[i], err = NewBlindFactor()
		if err != nil {
			return nil, Commitment{}, fmt.Errorf("amount binding blind %d: %w", i, err)
		}
		bi, parseErr := ScalarFromBytes(blinds[i][:])
		if parseErr != nil {
			return nil, Commitment{}, parseErr
		}
		weightedBlind.MultiplyAdd(amountChunkWeight(i), bi, weightedBlind)
	}
	lastWeight := amountChunkWeight(AmountCipherChunkCount - 1)
	lastBlind := edwards25519.NewScalar().Multiply(
		edwards25519.NewScalar().Subtract(targetBlind, weightedBlind),
		edwards25519.NewScalar().Invert(lastWeight),
	)
	copy(blinds[AmountCipherChunkCount-1][:], lastBlind.Bytes())

	binding := &AmountBinding{}
	aggregate := new(edwards25519.Point).Set(edwards25519.NewIdentityPoint())
	for i := range binding.Chunks {
		digit := (value >> (amountCipherChunkBits * i)) & ((1 << amountCipherChunkBits) - 1)
		commit, commitErr := Commit(digit, blinds[i])
		if commitErr != nil {
			return nil, Commitment{}, fmt.Errorf("amount binding commitment %d: %w", i, commitErr)
		}
		rangeProof, proofErr := ProveRangeBits(digit, blinds[i], amountCipherChunkBits)
		if proofErr != nil {
			return nil, Commitment{}, fmt.Errorf("amount binding range proof %d: %w", i, proofErr)
		}

		nonce, nonceErr := randomScalar()
		if nonceErr != nil {
			return nil, Commitment{}, fmt.Errorf("amount binding encryption nonce %d: %w", i, nonceErr)
		}
		randomPoint := (&edwards25519.Point{}).ScalarBaseMult(nonce)
		valuePoint := (&edwards25519.Point{}).ScalarBaseMult(scalarFromUint64(digit))
		cipherPoint := new(edwards25519.Point).Add(valuePoint, new(edwards25519.Point).ScalarMult(nonce, pub))

		chunk := AmountCipherChunk{Commitment: commit, RangeProof: rangeProof}
		copy(chunk.R[:], randomPoint.Bytes())
		copy(chunk.E[:], cipherPoint.Bytes())
		link, linkErr := proveAmountLink(outputPub, commit, chunk.R, chunk.E, blinds[i], digit, nonce)
		nonce.Set(edwards25519.NewScalar())
		if linkErr != nil {
			return nil, Commitment{}, fmt.Errorf("amount binding link proof %d: %w", i, linkErr)
		}
		chunk.LinkProof = link
		binding.Chunks[i] = chunk

		commitPoint, parseErr := PointFromBytes(commit[:])
		if parseErr != nil {
			return nil, Commitment{}, fmt.Errorf("amount binding commitment point %d: %w", i, parseErr)
		}
		aggregate.Add(aggregate, new(edwards25519.Point).ScalarMult(amountChunkWeight(i), commitPoint))
	}
	var aggregateCommitment Commitment
	copy(aggregateCommitment[:], aggregate.Bytes())
	return binding, aggregateCommitment, nil
}

// VerifyAmountBinding verifies chunk ranges, ciphertext/commitment equality,
// and the weighted sum against the output's canonical Pedersen commitment.
func VerifyAmountBinding(outputCommit Commitment, outputPub Point32, binding *AmountBinding) (bool, error) {
	if binding == nil {
		return false, fmt.Errorf("nil amount binding")
	}
	pub, err := PointFromBytes(outputPub[:])
	if err != nil || pub.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return false, fmt.Errorf("invalid amount-binding output public key")
	}
	aggregate := new(edwards25519.Point).Set(edwards25519.NewIdentityPoint())
	for i := range binding.Chunks {
		chunk := &binding.Chunks[i]
		ok, proofErr := VerifyRange(chunk.RangeProof)
		if proofErr != nil {
			return false, fmt.Errorf("amount-binding chunk %d range proof: %w", i, proofErr)
		}
		if !ok || chunk.RangeProof.Bits != amountCipherChunkBits ||
			chunk.RangeProof.ValueCommit != chunk.Commitment {
			return false, nil
		}
		ok, proofErr = verifyAmountLink(outputPub, chunk.Commitment, chunk.R, chunk.E, chunk.LinkProof)
		if proofErr != nil {
			return false, fmt.Errorf("amount-binding chunk %d link proof: %w", i, proofErr)
		}
		if !ok {
			return false, nil
		}
		commitPoint, parseErr := PointFromBytes(chunk.Commitment[:])
		if parseErr != nil {
			return false, fmt.Errorf("amount-binding chunk %d commitment: %w", i, parseErr)
		}
		aggregate.Add(aggregate, new(edwards25519.Point).ScalarMult(amountChunkWeight(i), commitPoint))
	}
	target, err := PointFromBytes(outputCommit[:])
	if err != nil {
		return false, fmt.Errorf("invalid output commitment: %w", err)
	}
	return aggregate.Equal(target) == 1, nil
}

// DecryptAmountBinding decrypts the bound amount using the one-time private
// spend scalar. A bounded 16-bit discrete-log table recovers each digit.
func DecryptAmountBinding(binding *AmountBinding, oneTimePriv Scalar32) (uint64, error) {
	if binding == nil {
		return 0, fmt.Errorf("nil amount binding")
	}
	x, err := ScalarFromBytes(oneTimePriv[:])
	if err != nil {
		return 0, fmt.Errorf("invalid one-time private key: %w", err)
	}
	table := amountDigitTable()
	var value uint64
	for i := range binding.Chunks {
		chunk := &binding.Chunks[i]
		r, parseErr := PointFromBytes(chunk.R[:])
		if parseErr != nil || r.Equal(edwards25519.NewIdentityPoint()) == 1 {
			return 0, fmt.Errorf("invalid amount ciphertext R at chunk %d", i)
		}
		e, parseErr := PointFromBytes(chunk.E[:])
		if parseErr != nil {
			return 0, fmt.Errorf("invalid amount ciphertext E at chunk %d: %w", i, parseErr)
		}
		plain := new(edwards25519.Point).Subtract(e, new(edwards25519.Point).ScalarMult(x, r))
		digit, found := table[[32]byte(plain.Bytes())]
		if !found {
			return 0, fmt.Errorf("amount ciphertext chunk %d is outside 16-bit range", i)
		}
		value |= uint64(digit) << (amountCipherChunkBits * i)
	}
	return value, nil
}

func proveAmountLink(pub Point32, commitment Commitment, rPoint, ePoint Point32,
	blind BlindFactor, value uint64, nonce *edwards25519.Scalar,
) (AmountLinkProof, error) {
	b, err := ScalarFromBytes(blind[:])
	if err != nil {
		return AmountLinkProof{}, err
	}
	alphaBlind, err := randomScalar()
	if err != nil {
		return AmountLinkProof{}, err
	}
	alphaValue, err := randomScalar()
	if err != nil {
		return AmountLinkProof{}, err
	}
	alphaRandom, err := randomScalar()
	if err != nil {
		return AmountLinkProof{}, err
	}
	encryptionKey, err := PointFromBytes(pub[:])
	if err != nil {
		return AmountLinkProof{}, fmt.Errorf("invalid output public key: %w", err)
	}
	t1 := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarBaseMult(alphaBlind),
		new(edwards25519.Point).ScalarMult(alphaValue, hGenerator),
	)
	t2 := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarBaseMult(alphaValue),
		new(edwards25519.Point).ScalarMult(alphaRandom, encryptionKey),
	)
	t3 := new(edwards25519.Point).ScalarBaseMult(alphaRandom)
	var proof AmountLinkProof
	copy(proof.TCommit[:], t1.Bytes())
	copy(proof.TCipher[:], t2.Bytes())
	copy(proof.TRandom[:], t3.Bytes())
	challenge := amountLinkChallenge(pub, commitment, rPoint, ePoint, proof.TCommit, proof.TCipher, proof.TRandom)
	zBlind := edwards25519.NewScalar().MultiplyAdd(challenge, b, alphaBlind)
	zValue := edwards25519.NewScalar().MultiplyAdd(challenge, scalarFromUint64(value), alphaValue)
	zRandom := edwards25519.NewScalar().MultiplyAdd(challenge, nonce, alphaRandom)
	copy(proof.ZBlind[:], zBlind.Bytes())
	copy(proof.ZValue[:], zValue.Bytes())
	copy(proof.ZRandom[:], zRandom.Bytes())
	alphaBlind.Set(edwards25519.NewScalar())
	alphaValue.Set(edwards25519.NewScalar())
	alphaRandom.Set(edwards25519.NewScalar())
	return proof, nil
}

func verifyAmountLink(pub Point32, commitment Commitment, rPoint, ePoint Point32, proof AmountLinkProof) (bool, error) {
	p, err := PointFromBytes(pub[:])
	if err != nil || p.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return false, fmt.Errorf("invalid encryption key")
	}
	c, err := PointFromBytes(commitment[:])
	if err != nil {
		return false, fmt.Errorf("invalid chunk commitment: %w", err)
	}
	r, err := PointFromBytes(rPoint[:])
	if err != nil || r.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return false, fmt.Errorf("invalid ciphertext R")
	}
	e, err := PointFromBytes(ePoint[:])
	if err != nil {
		return false, fmt.Errorf("invalid ciphertext E: %w", err)
	}
	t1, err := PointFromBytes(proof.TCommit[:])
	if err != nil {
		return false, fmt.Errorf("invalid link commitment: %w", err)
	}
	t2, err := PointFromBytes(proof.TCipher[:])
	if err != nil {
		return false, fmt.Errorf("invalid link ciphertext commitment: %w", err)
	}
	t3, err := PointFromBytes(proof.TRandom[:])
	if err != nil {
		return false, fmt.Errorf("invalid link randomness commitment: %w", err)
	}
	zb, err := ScalarFromBytes(proof.ZBlind[:])
	if err != nil {
		return false, fmt.Errorf("invalid blind response: %w", err)
	}
	zv, err := ScalarFromBytes(proof.ZValue[:])
	if err != nil {
		return false, fmt.Errorf("invalid value response: %w", err)
	}
	zr, err := ScalarFromBytes(proof.ZRandom[:])
	if err != nil {
		return false, fmt.Errorf("invalid encryption response: %w", err)
	}
	challenge := amountLinkChallenge(pub, commitment, rPoint, ePoint, proof.TCommit, proof.TCipher, proof.TRandom)
	check1 := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarBaseMult(zb),
		new(edwards25519.Point).ScalarMult(zv, hGenerator),
	)
	expected1 := new(edwards25519.Point).Add(t1, new(edwards25519.Point).ScalarMult(challenge, c))
	check2 := new(edwards25519.Point).Add(
		new(edwards25519.Point).ScalarBaseMult(zv),
		new(edwards25519.Point).ScalarMult(zr, p),
	)
	expected2 := new(edwards25519.Point).Add(t2, new(edwards25519.Point).ScalarMult(challenge, e))
	check3 := new(edwards25519.Point).ScalarBaseMult(zr)
	expected3 := new(edwards25519.Point).Add(t3, new(edwards25519.Point).ScalarMult(challenge, r))
	return check1.Equal(expected1) == 1 && check2.Equal(expected2) == 1 && check3.Equal(expected3) == 1, nil
}

func amountLinkChallenge(pub Point32, commitment Commitment, rPoint, ePoint, t1, t2, t3 Point32) *edwards25519.Scalar {
	h := sha512.New()
	h.Write([]byte("Aperod/RingCT/amount-binding/v1"))
	h.Write(pub[:])
	h.Write(commitment[:])
	h.Write(rPoint[:])
	h.Write(ePoint[:])
	h.Write(t1[:])
	h.Write(t2[:])
	h.Write(t3[:])
	var wide [64]byte
	copy(wide[:], h.Sum(nil))
	s, _ := edwards25519.NewScalar().SetUniformBytes(wide[:])
	return s
}

func amountChunkWeight(i int) *edwards25519.Scalar {
	return scalarFromUint64(uint64(1) << (amountCipherChunkBits * i))
}

func amountDigitTable() map[[32]byte]uint16 {
	amountDigitTableOnce.Do(func() {
		amountDigitTableValue = make(map[[32]byte]uint16, 1<<amountCipherChunkBits)
		point := edwards25519.NewIdentityPoint()
		step := edwards25519.NewGeneratorPoint()
		for i := uint32(0); i < 1<<amountCipherChunkBits; i++ {
			var encoded [32]byte
			copy(encoded[:], point.Bytes())
			amountDigitTableValue[encoded] = uint16(i)
			point.Add(point, step)
		}
	})
	return amountDigitTableValue
}

var (
	amountDigitTableOnce  sync.Once
	amountDigitTableValue map[[32]byte]uint16
)
