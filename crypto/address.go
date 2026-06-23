package crypto

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcutil/base58"
)

// NetworkByte prefixes an address to identify which chain it belongs to.
type NetworkByte byte

const (
	MainnetByte NetworkByte = 0x41 // 'A' prefix → "apr1q..."
	TestnetByte NetworkByte = 0x54 // 'T' prefix → "tapr..."
	DevnetByte  NetworkByte = 0x44 // 'D' prefix
)

// Address is the human-readable representation of a wallet (spend + view public keys).
// Format: Base58Check( version_byte || spend_pubkey[32] || view_pubkey[32] )
// Total payload: 1 + 32 + 32 = 65 bytes → ~95 Base58Check chars with prefix "apr".
type Address string

// EncodeAddress encodes a spend+view public key pair into a Aperod address.
func EncodeAddress(net NetworkByte, spend, view Point32) Address {
	// Payload: version || spend_pub || view_pub
	payload := make([]byte, 1+32+32)
	payload[0] = byte(net)
	copy(payload[1:33], spend[:])
	copy(payload[33:65], view[:])

	// Append 4-byte checksum (first 4 bytes of double-SHA3)
	checksum := DoubleSHA3(payload)
	full := append(payload, checksum[:4]...)

	return Address(base58.Encode(full))
}

// DecodeAddress decodes an Aperod address into its components.
// Returns an error if the checksum is invalid or length is wrong.
func DecodeAddress(addr Address) (net NetworkByte, spend, view Point32, err error) {
	decoded := base58.Decode(string(addr))
	// 1 + 32 + 32 + 4 = 69 bytes
	if len(decoded) != 69 {
		return 0, Point32{}, Point32{}, fmt.Errorf(
			"invalid address length: want 69 bytes, got %d", len(decoded))
	}

	payload := decoded[:65]
	gotChecksum := decoded[65:]
	wantChecksum := DoubleSHA3(payload)
	if !bytes.Equal(gotChecksum, wantChecksum[:4]) {
		return 0, Point32{}, Point32{}, fmt.Errorf("invalid address checksum")
	}

	net = NetworkByte(payload[0])
	copy(spend[:], payload[1:33])
	copy(view[:], payload[33:65])
	return net, spend, view, nil
}

// Validate checks an address without decoding its keys.
func Validate(addr Address) error {
	_, _, _, err := DecodeAddress(addr)
	return err
}

// AddressFromKeys is a convenience wrapper for WalletKeyPair.
func AddressFromKeys(net NetworkByte, kp *WalletKeyPair) Address {
	return EncodeAddress(net, kp.Spend.Public, kp.View.Public)
}

// String implements Stringer.
func (a Address) String() string { return string(a) }

// Short returns the first 12 chars for logging.
func (a Address) Short() string {
	s := string(a)
	if len(s) <= 12 {
		return s
	}
	return s[:12] + "..."
}
