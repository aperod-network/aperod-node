package crypto

import (
	"bytes"
	"testing"
)

func TestDecodeWalletAddressValidatesBothPublicKeys(t *testing.T) {
	keys, err := GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range []NetworkByte{MainnetByte, TestnetByte, DevnetByte} {
		addr := AddressFromKeys(network, keys)
		net, spend, view, err := DecodeWalletAddress(addr)
		if err != nil || net != network || spend != keys.Spend.Public || view != keys.View.Public {
			t.Fatalf("valid wallet address rejected or changed: %v", err)
		}
	}
	var noncanonical Point32
	copy(noncanonical[:], bytes.Repeat([]byte{0xff}, 32))
	noncanonical[0], noncanonical[31] = 0xee, 0x7f // y=p+1, identity alias
	var negativeIdentity Point32
	negativeIdentity[0], negativeIdentity[31] = 1, 0x80
	var orderTwo Point32
	copy(orderTwo[:], bytes.Repeat([]byte{0xff}, 32))
	orderTwo[0], orderTwo[31] = 0xec, 0x7f
	for name, point := range map[string]Point32{
		"identity": {1}, "negative identity": negativeIdentity,
		"noncanonical identity": noncanonical, "order two": orderTwo,
		"order four": {},
	} {
		for _, field := range []string{"spend", "view"} {
			t.Run(name+"/"+field, func(t *testing.T) {
				spend, view := keys.Spend.Public, keys.View.Public
				if field == "spend" {
					spend = point
				} else {
					view = point
				}
				addr := EncodeAddress(MainnetByte, spend, view)
				if _, _, _, err := DecodeAddress(addr); err != nil {
					t.Fatalf("fixture must be accepted by historical wire decoder: %v", err)
				}
				if _, _, _, err := DecodeWalletAddress(addr); err == nil {
					t.Fatal("unsafe wallet key accepted")
				}
			})
		}
	}
}
