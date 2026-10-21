package core

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
)

// Exercise pure ring placement only: generated curve points, no transaction
// construction, range proof, ring signature, broadcast or canonical-chain claim.
func TestRingPlacementDistinctDecoys(t *testing.T) {
	point := func(t *testing.T) crypto.Point32 {
		t.Helper()
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		return keys.Spend.Public
	}
	tests := []struct {
		name string
		edit func([]DecoyUTXO, crypto.Point32) []DecoyUTXO
		fail bool
	}{
		{"distinct members", func(d []DecoyUTXO, _ crypto.Point32) []DecoyUTXO { return d }, false},
		{"exact pair repeated", func(d []DecoyUTXO, _ crypto.Point32) []DecoyUTXO { d[1] = d[0]; return d }, true},
		{"same key conflicting commit", func(d []DecoyUTXO, _ crypto.Point32) []DecoyUTXO { d[1].OneTimePub = d[0].OneTimePub; return d }, true},
		{"real input repeated", func(d []DecoyUTXO, p crypto.Point32) []DecoyUTXO { d[1].OneTimePub = p; return d }, true},
		{"many aliases", func(d []DecoyUTXO, _ crypto.Point32) []DecoyUTXO {
			aliases := make([]DecoyUTXO, 512)
			for i := range aliases {
				aliases[i] = d[i%3]
			}
			return aliases
		}, true},
		{"shared commit distinct keys", func(d []DecoyUTXO, _ crypto.Point32) []DecoyUTXO {
			for i := range d {
				d[i].AmountCommit = d[0].AmountCommit
			}
			return d
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			real := point(t)
			decoys := make([]DecoyUTXO, crypto.RingSize-1)
			for i := range decoys {
				decoys[i] = DecoyUTXO{OneTimePub: point(t), AmountCommit: crypto.Commitment(point(t))}
			}
			decoys = tc.edit(decoys, real)
			before := append([]DecoyUTXO(nil), decoys...)
			ring, index, fallback, err := txBuildRing(real, decoys)
			if !reflect.DeepEqual(before, decoys) {
				t.Fatal("ring placement mutated the supplied canonical pairs")
			}
			if tc.fail {
				if err == nil || !strings.Contains(err.Error(), "duplicate decoy") || ring != nil {
					t.Fatalf("duplicate accepted: ring=%v error=%v", ring, err)
				}
				return
			}
			if err != nil || len(ring) != crypto.RingSize || fallback != 0 || ring[index] != real {
				t.Fatalf("invalid placement: size=%d fallback=%d error=%v", len(ring), fallback, err)
			}
			seen := make(map[crypto.Point32]bool)
			for _, pub := range ring {
				if seen[pub] {
					t.Fatal("ring contains repeated public key")
				}
				seen[pub] = true
			}
		})
	}
}

// Regression for the old public-key-derived placement rule. This intentionally
// does not construct transactions or signatures; it measures only synthetic
// public rings and the candidate set from the old public predicate.
func TestRandomizedRingPositionBreaksPublicKeyPredicate(t *testing.T) {
	const trials = 256
	totalCandidates := 0
	minCandidates := crypto.RingSize
	maxCandidates := 0
	truePositionMatches := 0
	positionCounts := make([]int, crypto.RingSize)
	entropyBytes := make([]byte, trials)
	for i := range entropyBytes {
		entropyBytes[i] = byte(i % crypto.RingSize)
	}
	entropy := bytes.NewReader(entropyBytes)
	realKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	for trial := 0; trial < trials; trial++ {
		decoys := make([]DecoyUTXO, crypto.RingSize-1)
		for i := range decoys {
			keys, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatal(err)
			}
			decoys[i] = DecoyUTXO{OneTimePub: keys.Spend.Public}
		}
		ring, realIdx, _, err := txBuildRingWithEntropy(realKeys.Spend.Public, decoys, entropy)
		if err != nil {
			t.Fatal(err)
		}
		positionCounts[realIdx]++
		if realIdx == int(realKeys.Spend.Public[0])%crypto.RingSize {
			truePositionMatches++
		}
		candidates := 0
		for slot, member := range ring {
			if int(member[0])%crypto.RingSize == slot {
				candidates++
			}
		}
		totalCandidates += candidates
		if candidates < minCandidates {
			minCandidates = candidates
		}
		if candidates > maxCandidates {
			maxCandidates = candidates
		}
	}
	t.Logf("public predicate survivors across %d generated rings: mean %.2f, min %d, max %d; true-key relation %d/%d; positions %v; ring size %d",
		trials, float64(totalCandidates)/trials, minCandidates, maxCandidates, truePositionMatches, trials, positionCounts, crypto.RingSize)
	if truePositionMatches != trials/crypto.RingSize {
		t.Fatalf("real-key position matched public predicate %d times, want %d for independent positions", truePositionMatches, trials/crypto.RingSize)
	}
	for index, count := range positionCounts {
		if count != trials/crypto.RingSize {
			t.Fatalf("position %d selected %d times; want %d", index, count, trials/crypto.RingSize)
		}
	}
	if float64(totalCandidates)/trials >= 4 {
		t.Fatalf("public predicate retained too many candidates on average: %.2f", float64(totalCandidates)/trials)
	}
}

func TestRingPositionSelectionFailsClosedOnEntropyError(t *testing.T) {
	realKeys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	decoys := make([]DecoyUTXO, crypto.RingSize-1)
	for i := range decoys {
		keys, err := crypto.GenerateWalletKeys()
		if err != nil {
			t.Fatal(err)
		}
		decoys[i] = DecoyUTXO{OneTimePub: keys.Spend.Public}
	}
	for name, entropy := range map[string]io.Reader{
		"nil reader":   nil,
		"reader error": failingRingPositionEntropy{},
	} {
		t.Run(name, func(t *testing.T) {
			ring, _, _, err := txBuildRingWithEntropy(realKeys.Spend.Public, decoys, entropy)
			if err == nil || ring != nil || !strings.Contains(err.Error(), "select real ring position") {
				t.Fatalf("expected fail-closed entropy error, got ring=%v error=%v", ring, err)
			}
		})
	}
}

type failingRingPositionEntropy struct{}

func (failingRingPositionEntropy) Read([]byte) (int, error) {
	return 0, errors.New("injected entropy failure")
}
