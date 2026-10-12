package main

import (
	"encoding/hex"
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
)

func netCandidate(c *store.LPoDCheckpoint, parent crypto.Hash32, claims operatorClaims, burn, wallet uint64, rootText string) (sale.Reconciliation, uint64, error) {
	var r sale.Reconciliation
	if claims.Scope != "operator-forward-declaration-not-activation-v1" {
		return r, 0, fmt.Errorf("net candidate: explicit operator claims required")
	}
	guardian, err := exactAmount(claims.GuardianTotal)
	if err != nil {
		return r, 0, err
	}
	extra, err := exactAmount(claims.Additional)
	if err != nil {
		return r, 0, err
	}
	if c == nil || c.Allocation == nil || guardian != c.State.FundingDebit {
		return r, 0, fmt.Errorf("net candidate: canonical initial Guardian funding mismatch")
	}
	raw, err := hex.DecodeString(rootText)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != rootText {
		return r, 0, fmt.Errorf("net candidate: exact lowercase wallet accounting root required")
	}
	var root crypto.Hash32
	copy(root[:], raw)
	basis := &sale.NetAllocationBasis{Burned: burn, WalletNet: wallet, WalletAccountingRoot: root}
	consumed, err := basis.Consumed()
	if err != nil {
		return r, 0, err
	}
	review, err := store.ReviewPostBurnSaleSource(c, burn, &wallet)
	if err != nil {
		return r, 0, err
	}
	if parent == (crypto.Hash32{}) || c.Allocation.Genesis == (crypto.Hash32{}) {
		return r, 0, fmt.Errorf("net candidate: exact canonical identity required")
	}
	r = sale.Reconciliation{NetAllocation: basis, Genesis: c.Allocation.Genesis, Parent: parent, LPoDCommitment: c.Digest(),
		OtherClaimsRoot: crypto.HashBytes([]byte("aperod/sale/net-extra-claims/v2"), []byte(claims.Scope), []byte(claims.Additional)),
		HistoricIssued:  consumed, ValidatorReserved: review.ValidatorReserve, GuardianReserved: review.LPoDRewardBalance,
		LPoDReserved: review.LPoDPrincipal + review.LPoDUnassigned, OtherReserved: extra}
	if err := store.VerifySaleReserveFloors(c, r); err != nil {
		return r, 0, err
	}
	budget, _, err := r.Unassigned()
	return r, budget, err
}
