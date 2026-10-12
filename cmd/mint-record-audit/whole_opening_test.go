package main

import (
	"testing"

	"github.com/aperod/aperod/crypto"
)

func TestWholeOpeningUsesRecordedBlindWithoutAssumingMintDerivation(t *testing.T) {
	blind, err := crypto.DeterministicMintBlind(crypto.Point32{7}, 123)
	if err != nil {
		t.Fatal(err)
	}
	for _, whole := range []uint64{0, 1, 37, 1000, 65535} {
		c, err := crypto.Commit(whole*100000000, blind)
		if err != nil {
			t.Fatal(err)
		}
		v, err := wholeOpening(c, blind, 65535)
		if err != nil || v == nil || *v != whole*100000000 {
			t.Fatal("whole opening not recovered", whole, err)
		}
	}
	c, _ := crypto.Commit(100000001, blind)
	if v, err := wholeOpening(c, blind, 65535); err != nil || v != nil {
		t.Fatal("fractional output rounded into an integer")
	}
	c, _ = crypto.Commit(65536*100000000, blind)
	if v, err := wholeOpening(c, blind, 65535); err != nil || v != nil {
		t.Fatal("bounded search exceeded declared range")
	}
	if _, err := wholeOpening(c, blind, 10000000001); err == nil {
		t.Fatal("unbounded work accepted")
	}
}
