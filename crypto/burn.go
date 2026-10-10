package crypto

// BurnAddress returns the canonical, network-specific intentional-burn
// address.  Its keys are deterministic hash-to-curve points, not public keys
// derived from a private scalar; consequently no private spend key is known.
//
// The domain strings are consensus constants.  Do not change them: doing so
// changes the address to which wallet builders apply burn semantics.
func BurnAddress(net NetworkByte) Address {
	spend := burnPoint("aperod/canonical-burn/spend/v1", net)
	view := burnPoint("aperod/canonical-burn/view/v1", net)
	return EncodeAddress(net, spend, view)
}

// MainnetBurnAddress is the one canonical mainnet burn address.
func MainnetBurnAddress() Address { return BurnAddress(MainnetByte) }

// IsBurnAddress reports whether addr is the canonical burn address for its
// encoded network. Invalid addresses are never considered burn addresses.
func IsBurnAddress(addr Address) bool {
	net, _, _, err := DecodeAddress(addr)
	return err == nil && addr == BurnAddress(net)
}

func burnPoint(domain string, net NetworkByte) Point32 {
	p := HashToCurvePoint(append([]byte(domain), byte(net)))
	var out Point32
	copy(out[:], p.Bytes())
	return out
}
