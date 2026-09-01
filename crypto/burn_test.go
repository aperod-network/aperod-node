package crypto

import "testing"

func TestBurnAddressCanonicalAndNetworkSpecific(t *testing.T) {
	mainnet := MainnetBurnAddress()
	if mainnet != BurnAddress(MainnetByte) {
		t.Fatalf("mainnet canonical burn address changed between helpers: %q / %q", mainnet, BurnAddress(MainnetByte))
	}
	for _, network := range []NetworkByte{MainnetByte, TestnetByte, DevnetByte} {
		address := BurnAddress(network)
		if err := Validate(address); err != nil {
			t.Fatalf("burn address for network %x is invalid: %v", byte(network), err)
		}
		if !IsBurnAddress(address) {
			t.Fatalf("burn address for network %x was not recognized", byte(network))
		}
		if address != BurnAddress(network) {
			t.Fatalf("burn address for network %x is not stable", byte(network))
		}
	}
	if BurnAddress(MainnetByte) == BurnAddress(TestnetByte) ||
		BurnAddress(MainnetByte) == BurnAddress(DevnetByte) ||
		BurnAddress(TestnetByte) == BurnAddress(DevnetByte) {
		t.Fatal("burn addresses must be network-specific")
	}
}
