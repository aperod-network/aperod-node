package main

import (
	"strings"
	"testing"
)

func TestDeriveMainnetAddressKnownMnemonic(t *testing.T) {
	// The BIP-39 all-zero entropy test vector.  Deriving it twice also protects
	// against accidentally introducing randomness into the browser boundary.
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	first, err := deriveMainnetAddress(mnemonic, 0, 0)
	if err != nil {
		t.Fatalf("derive known mnemonic: %v", err)
	}
	second, err := deriveMainnetAddress(mnemonic, 0, 0)
	if err != nil {
		t.Fatalf("derive known mnemonic again: %v", err)
	}
	if first != second || !strings.HasPrefix(first, "apro") {
		t.Fatalf("unexpected deterministic mainnet address %q", first)
	}
}

func TestDeriveMainnetAddressRejectsInvalidMnemonic(t *testing.T) {
	if _, err := deriveMainnetAddress("not a valid mnemonic", 0, 0); err == nil {
		t.Fatal("invalid mnemonic was accepted")
	}
}
