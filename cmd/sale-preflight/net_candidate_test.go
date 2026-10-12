package main

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func TestNetCandidateProtectsPrincipalAndDeclaredWalletsWithoutOldNominalDeduction(t *testing.T) {
	genesis := crypto.HashBytes([]byte("genesis"))
	parent := crypto.HashBytes([]byte("parent"))
	c := &store.LPoDCheckpoint{
		Allocation: &store.LPoDAllocation{Version: 3, Genesis: genesis, ValidatorRemaining: 199731332900000000,
			InitialValidatorRemaining: 199731332900000000 + 62503922060849,
			NominalSnapshot:           &store.LPoDNominalSnapshot{NominalCirculatingBefore: 623000000000000000}},
		State:              lpod.State{FundingDebit: 100000000000000000, RewardInflow: 62503922060849, SurplusInflow: 62503922060849, Balance: 100062503922060849},
		PrincipalDeposited: 500000000000000, PrincipalLocked: 500000000000000,
	}
	claims := operatorClaims{Scope: "operator-forward-declaration-not-activation-v1", GuardianTotal: "100000000000000000", Additional: "0"}
	root := crypto.HashBytes([]byte("synthetic accounting only"))
	text := hex.EncodeToString(root[:])
	r, budget, err := netCandidate(c, parent, claims, 13745052310704501, 100010000000000, text)
	if err != nil {
		t.Fatal(err)
	}
	if budget != 685861100867234650 || r.NetAllocation == nil || r.LPoDReserved != 500000000000000 {
		t.Fatalf("wrong classified candidate %d", budget)
	}
	for _, bad := range []string{"", strings.Repeat("0", 64), strings.ToUpper(text), text + "00"} {
		if _, _, err := netCandidate(c, parent, claims, 13745052310704501, 100010000000000, bad); err == nil {
			t.Fatal("missing or ambiguous accounting root accepted")
		}
	}
	claims.GuardianTotal = "200000000000000000"
	if _, _, err := netCandidate(c, parent, claims, 13745052310704501, 100010000000000, text); err == nil {
		t.Fatal("second Guardian allocation accepted")
	}
}
