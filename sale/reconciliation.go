package sale

import (
	"fmt"

	"github.com/aperod/aperod/crypto"
)

// The initial protocol sale allocation must preserve the separately earmarked
// Guardian allocation within the primary 10B cap.
const MinimumGuardianReserveNAPRO uint64 = 1_000_000_000 * 100_000_000

// Reconciliation is a forward, authority-attested accounting declaration, not
// historical proof. Its LPoD commitment must be obtained from the canonical
// finalized activation parent, never supplied by a purchase API or explorer.
// Categories are disjoint: locked issued principal belongs to HistoricIssued,
// not to an independently spendable reserve as well.
type Reconciliation struct {
	// Non-nil selects an EXPLICIT new signed accounting basis. Legacy witnesses
	// keep their original domain and interpretation; there is no implicit upgrade.
	NetAllocation *NetAllocationBasis `json:"net_allocation,omitempty"`
	// Explicitly signed nominal budget, not an implicit V1/V2 reinterpretation.
	// HistoricIssued represents its consumed burn counter, not legacy holdings.
	AllocationEpoch   *AllocationEpochBasis `json:"allocation_epoch,omitempty"`
	Genesis           crypto.Hash32
	Parent            crypto.Hash32
	LPoDCommitment    crypto.Hash32
	OtherClaimsRoot   crypto.Hash32
	HistoricIssued    uint64
	ValidatorReserved uint64
	LPoDReserved      uint64
	GuardianReserved  uint64
	OtherReserved     uint64
}

func (r Reconciliation) Digest() crypto.Hash32 {
	legacy := crypto.HashBytes(
		[]byte("aperod/sale/disjoint-forward-reconciliation/v1"),
		r.Genesis[:], r.Parent[:], r.LPoDCommitment[:], r.OtherClaimsRoot[:],
		u64(r.HistoricIssued), u64(r.ValidatorReserved), u64(r.LPoDReserved),
		u64(r.GuardianReserved), u64(r.OtherReserved),
	)
	if r.AllocationEpoch != nil {
		epoch := r.AllocationEpoch.Digest()
		if r.NetAllocation != nil {
			net := r.NetAllocation.Digest()
			return crypto.HashBytes([]byte("aperod/sale/invalid-mixed-allocation-bases/v1"), legacy[:], epoch[:], net[:])
		}
		return crypto.HashBytes([]byte("aperod/sale/operator-epoch-reconciliation/v3"), legacy[:], epoch[:])
	}
	if r.NetAllocation == nil {
		return legacy
	}
	net := r.NetAllocation.Digest()
	return crypto.HashBytes([]byte("aperod/sale/disjoint-forward-reconciliation/v2"), legacy[:], net[:])
}

func (r Reconciliation) Unassigned() (uint64, uint64, error) {
	if r.AllocationEpoch != nil {
		if r.NetAllocation != nil {
			return 0, 0, fmt.Errorf("sale: mixed allocation bases")
		}
		if err := r.AllocationEpoch.Validate(); err != nil {
			return 0, 0, err
		}
		if r.HistoricIssued != r.AllocationEpoch.Burned {
			return 0, 0, fmt.Errorf("sale: nominal epoch burn counter mismatch")
		}
	}
	if r.NetAllocation != nil {
		consumed, err := r.NetAllocation.Consumed()
		if err != nil {
			return 0, 0, err
		}
		if consumed != r.HistoricIssued {
			return 0, 0, fmt.Errorf("sale: post-burn consumed allocation mismatch")
		}
	}
	remaining := CapNAPRO
	for _, claim := range []uint64{r.HistoricIssued, r.ValidatorReserved, r.LPoDReserved, r.GuardianReserved, r.OtherReserved} {
		if claim > remaining {
			return 0, 0, fmt.Errorf("sale: overlapping declarations or reserves exceed the 10B cap")
		}
		remaining -= claim
	}
	if r.AllocationEpoch != nil && remaining != r.AllocationEpoch.Available {
		return 0, 0, fmt.Errorf("sale: declared available allocation does not reconcile nominal reserves")
	}
	return remaining, CapNAPRO - remaining - r.HistoricIssued, nil
}

// VerifyReconciledCheckpoint requires a caller-established canonical LPoD
// commitment and the already attested minimum nominal issuance. It cannot
// reduce that issuance to make the public available-allocation headline fit.
// This is ONLY an activation preflight; it does not grant payment acceptance.
func VerifyReconciledCheckpoint(c Checkpoint, r Reconciliation,
	genesis, parent, canonicalLPoD crypto.Hash32, height, lpodDeclaredIssued uint64,
	trusted []crypto.ValidatorPubKey) error {
	if r.AllocationEpoch != nil {
		if r.NetAllocation != nil {
			return fmt.Errorf("sale: mixed allocation bases")
		}
		if err := r.AllocationEpoch.Validate(); err != nil {
			return err
		}
		if r.AllocationEpoch.FundingHeight >= height {
			return fmt.Errorf("sale: LPoD epoch is not before sale activation")
		}
		if c.SaleRemaining != r.AllocationEpoch.Available {
			return fmt.Errorf("sale: epoch checkpoint must open the current available allocation")
		}
		lpodDeclaredIssued = r.AllocationEpoch.Burned
	}
	if r.NetAllocation != nil {
		// The V3 nominal unlocked allocation is not independently proved wallet
		// issuance. Only an explicitly signed V2 baseline can change this reading.
		consumed, err := r.NetAllocation.Consumed()
		if err != nil {
			return err
		}
		lpodDeclaredIssued = consumed
	}
	if canonicalLPoD == zeroHash || r.LPoDCommitment != canonicalLPoD ||
		r.OtherClaimsRoot == zeroHash || r.Genesis != genesis || r.Parent != parent ||
		r.HistoricIssued < lpodDeclaredIssued || c.SnapshotRoot != r.Digest() {
		return fmt.Errorf("sale: reconciliation is not bound to the canonical LPoD parent and declared issuance")
	}
	available, reserved, err := r.Unassigned()
	if err != nil {
		return err
	}
	if c.HistoricIssued != r.HistoricIssued || c.OtherReserved != reserved || c.SaleRemaining > available {
		return fmt.Errorf("sale: checkpoint does not reconcile its disjoint allocation claims")
	}
	return c.Verify(genesis, parent, height, trusted)
}
