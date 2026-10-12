package sale

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// AllocationEpochBasis opens an operator-attested NOMINAL allocation ledger.
// It is not the net-holdings basis and does not assert that all legacy and new
// coins together fit under CapNAPRO. Legacy holdings remain unknown, not zero.
// Funding identifies the existing canonical LPoD epoch. Principal is protected
// unchanged and recorded explicitly, not newly minted or charged twice.
type AllocationEpochBasis struct {
	FundingHeight            uint64        `json:"funding_height"`
	FundingHash              crypto.Hash32 `json:"funding_hash"`
	Burned                   uint64        `json:"burned_napro,string"`
	Available                uint64        `json:"available_napro,string"`
	ProtectedLPoDPrincipal   uint64        `json:"protected_lpod_principal_napro,string"`
	LegacyHoldingsUnverified bool          `json:"legacy_holdings_unverified"`
}

func (e AllocationEpochBasis) Validate() error {
	if e.FundingHash == zeroHash || !e.LegacyHoldingsUnverified ||
		e.Available == 0 || e.Available > CapNAPRO || e.Burned > CapNAPRO ||
		e.ProtectedLPoDPrincipal > CapNAPRO {
		return fmt.Errorf("sale: explicit nominal LPoD epoch and unverified legacy holdings required")
	}
	return nil
}

func (e AllocationEpochBasis) Digest() crypto.Hash32 {
	return crypto.HashBytes(
		[]byte("aperod/sale/operator-allocation-epoch/legacy-holdings-unverified/v1"),
		u64(e.FundingHeight), e.FundingHash[:], u64(e.Burned), u64(e.Available),
		u64(e.ProtectedLPoDPrincipal), []byte{boolByte(e.LegacyHoldingsUnverified)},
	)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
