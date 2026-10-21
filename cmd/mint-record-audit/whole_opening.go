package main

import (
	"encoding/binary"
	"fmt"
	"math"

	"filippo.io/edwards25519"

	"github.com/aperod/aperod/crypto"
)

// wholeOpening is a bounded baby-step/giant-step opening of P-rG = vH.
// It needs the recorded blind, not any spend/view secret. Fractional-APRO
// outputs will not match this search and remain unknown; no rounding is used.
func wholeOpening(commit crypto.Commitment, blind crypto.BlindFactor, max uint64) (*uint64, error) {
	if max > 10000000000 {
		return nil, fmt.Errorf("whole amount range exceeds primary cap")
	}
	p, err := new(edwards25519.Point).SetBytes(commit[:])
	if err != nil {
		return nil, err
	}
	zero, err := crypto.Commit(0, blind)
	if err != nil {
		return nil, err
	}
	rg, err := new(edwards25519.Point).SetBytes(zero[:])
	if err != nil {
		return nil, err
	}
	p.Subtract(p, rg)
	var noBlind crypto.BlindFactor
	hCommit, err := crypto.Commit(100000000, noBlind)
	if err != nil {
		return nil, err
	}
	h, err := new(edwards25519.Point).SetBytes(hCommit[:])
	if err != nil {
		return nil, err
	}
	m := uint64(math.Sqrt(float64(max))) + 1
	key := func(p *edwards25519.Point) [32]byte { var k [32]byte; copy(k[:], p.Bytes()); return k }
	table := make(map[[32]byte]uint64, m)
	jPoint := edwards25519.NewIdentityPoint()
	for j := uint64(0); j < m; j++ {
		table[key(jPoint)] = j
		jPoint.Add(jPoint, h)
	}
	var encoded [32]byte
	binary.LittleEndian.PutUint64(encoded[:], m)
	scalar, err := new(edwards25519.Scalar).SetCanonicalBytes(encoded[:])
	if err != nil {
		return nil, err
	}
	stride := new(edwards25519.Point).ScalarMult(scalar, h)
	for i := uint64(0); i <= max/m; i++ {
		if j, ok := table[key(p)]; ok {
			n := i*m + j
			if n <= max {
				amount := n * 100000000
				opening, err := crypto.Commit(amount, blind)
				if err == nil && opening == commit {
					return &amount, nil
				}
			}
		}
		p.Subtract(p, stride)
	}
	return nil, nil
}
