package store

import (
	"math"
	"testing"

	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/sale"
)

func netSourceFixture() *LPoDCheckpoint {
	return &LPoDCheckpoint{
		Allocation: &LPoDAllocation{Version: 3, ValidatorRemaining: 199731332900000000,
			InitialValidatorRemaining: 199731332900000000 + 62503922060849,
			NominalSnapshot:           &LPoDNominalSnapshot{NominalCirculatingBefore: 623000000000000000}},
		State:              lpod.State{FundingDebit: 100000000000000000, RewardInflow: 62503922060849, SurplusInflow: 62503922060849, Balance: 100062503922060849},
		PrincipalDeposited: 5000000 * 100000000, PrincipalLocked: 5000000 * 100000000,
	}
}

func TestSalePostBurnReviewDoesNotReapplyNominalCirculation(t *testing.T) {
	c := netSourceFixture()
	review, err := ReviewPostBurnSaleSource(c, 13745052310704501, nil)
	if err != nil {
		t.Fatal(err)
	}
	if review.PresentationResidual != 686461110867234650 {
		t.Fatalf("wrong observed residual: %d", review.PresentationResidual)
	}
	if review.BeforeOutsideWallets != 685961110867234650 {
		t.Fatal("principal exposed as free sale source")
	}
	if review.OutsideWalletNet != nil || review.AfterDeclaredWallets != nil || review.ActivationAuthorized {
		t.Fatal("unknown wallet liabilities became an activated budget")
	}
	c.Allocation.NominalSnapshot.NominalCirculatingBefore = 100000000
	other, err := ReviewPostBurnSaleSource(c, 13745052310704501, nil)
	if err != nil || other.PresentationResidual != review.PresentationResidual {
		t.Fatal("nominal vesting declaration treated as proved wallet issuance")
	}
	// Self-stake is issued outside the guardian principal, not a second reward reserve.
	issued := uint64(1000000*100000000 + 100*100000000)
	bound, err := ReviewPostBurnSaleSource(c, 13745052310704501, &issued)
	if err != nil || *bound.AfterDeclaredWallets != review.BeforeOutsideWallets-issued {
		t.Fatal("outside wallet/self-stake liabilities not protected")
	}
	issued++
	if *bound.OutsideWalletNet == issued {
		t.Fatal("review aliased mutable caller declaration")
	}
}

func TestSalePostBurnReviewPaidRewardsNeverBecomeNewCapacity(t *testing.T) {
	c := netSourceFixture()
	net := uint64(100000000)
	before, err := ReviewPostBurnSaleSource(c, 13745052310704501, &net)
	if err != nil {
		t.Fatal(err)
	}
	c.State.Balance -= 100000000
	c.State.AngelPaid += 100000000
	c.State.AccruedLiability += 100000000
	c.State.DeficitOutflow += 100000000
	net += 100000000
	after, err := ReviewPostBurnSaleSource(c, 13745052310704501, &net)
	if err != nil || *before.AfterDeclaredWallets != *after.AfterDeclaredWallets {
		t.Fatal("reward payment regenerated sale capacity")
	}
	c.PrincipalLocked -= 200000000
	c.PrincipalReturned += 200000000
	net += 200000000
	returned, err := ReviewPostBurnSaleSource(c, 13745052310704501, &net)
	if err != nil || *returned.AfterDeclaredWallets != *before.AfterDeclaredWallets {
		t.Fatal("principal refund regenerated sale capacity")
	}
}

func TestSalePostBurnReviewFailsClosed(t *testing.T) {
	for _, kind := range []string{"burn", "principal", "state", "wallet", "extra"} {
		t.Run(kind, func(t *testing.T) {
			c := netSourceFixture()
			burn := uint64(13745052310704501)
			var wallet *uint64
			switch kind {
			case "burn":
				burn = math.MaxUint64
			case "principal":
				c.PrincipalReturned = 1
			case "state":
				c.State.Balance++
			case "wallet":
				x := sale.CapNAPRO
				wallet = &x
			case "extra":
				c.GenesisVesting = &GenesisVestingState{}
			}
			if _, err := ReviewPostBurnSaleSource(c, burn, wallet); err == nil {
				t.Fatal("inconsistent accounting accepted")
			}
		})
	}
}
