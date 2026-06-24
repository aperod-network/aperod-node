package crypto

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/btcsuite/btcutil/base58"
)

// NetworkByte prefixes an address to identify which chain it belongs to.
type NetworkByte byte

const (
	MainnetByte NetworkByte = 0x41 // mainnet
	TestnetByte NetworkByte = 0x54 // testnet
	DevnetByte  NetworkByte = 0x44 // devnet
)

// Human-readable prefix prepended before the Base58Check payload.
// Mainnet addresses look like:  apr<base58...>
// Testnet addresses look like: tapr<base58...>
const (
	mainnetHRPrefix = "apr"
	testnetHRPrefix = "tapr"
	devnetHRPrefix  = "dapr"
)

func hrPrefix(net NetworkByte) string {
	switch net {
	case MainnetByte:
		return mainnetHRPrefix
	case TestnetByte:
		return testnetHRPrefix
	case DevnetByte:
		return devnetHRPrefix
	default:
		return fmt.Sprintf("net%02x", byte(net))
	}
}

// Address is the human-readable representation of a wallet (spend + view public keys).
// Format: <hrPrefix> + Base58Check( version_byte || spend_pubkey[32] || view_pubkey[32] )
// Example mainnet:  aprXXXXXXX...  (~98 chars total)
// Example testnet: taprXXXXXXX... (~99 chars total)
type Address string

// EncodeAddress encodes a spend+view public key pair into an Aperod address.
func EncodeAddress(net NetworkByte, spend, view Point32) Address {
	// Payload: version || spend_pub || view_pub
	payload := make([]byte, 1+32+32)
	payload[0] = byte(net)
	copy(payload[1:33], spend[:])
	copy(payload[33:65], view[:])

	// Append 4-byte checksum (first 4 bytes of double-SHA3)
	checksum := DoubleSHA3(payload)
	full := append(payload, checksum[:4]...)

	return Address(hrPrefix(net) + base58.Encode(full))
}

// DecodeAddress decodes an Aperod address into its components.
// Returns an error if the checksum is invalid or length is wrong.
func DecodeAddress(addr Address) (net NetworkByte, spend, view Point32, err error) {
	s := string(addr)

	// Detect and strip human-readable prefix
	var stripped string
	var expectedNet NetworkByte
	switch {
	case strings.HasPrefix(s, testnetHRPrefix):
		stripped = s[len(testnetHRPrefix):]
		expectedNet = TestnetByte
	case strings.HasPrefix(s, devnetHRPrefix):
		stripped = s[len(devnetHRPrefix):]
		expectedNet = DevnetByte
	case strings.HasPrefix(s, mainnetHRPrefix):
		stripped = s[len(mainnetHRPrefix):]
		expectedNet = MainnetByte
	default:
		return 0, Point32{}, Point32{}, fmt.Errorf("invalid address: missing network prefix (apr/tapr/dapr)")
	}

	decoded := base58.Decode(stripped)
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
	if net != expectedNet {
		return 0, Point32{}, Point32{}, fmt.Errorf(
			"address network byte mismatch: prefix says %s but payload byte is 0x%02x",
			hrPrefix(expectedNet), payload[0])
	}

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
