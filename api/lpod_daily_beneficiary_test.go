package api

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/store"
)

func TestDailyPositionBeneficiarySurvivesClosureAndResume(t *testing.T) {
	positions := make(map[string]*lpodDailyPositionSum)
	row := store.LPoDAuditPosition{ID: "position", Beneficiary: "guardian", EffectiveVault: "vault", EffectiveVaultAfter: "vault", Accrued: 100, Paid: 40}
	if err := accumulateDailyPosition(positions, row); err != nil {
		t.Fatal(err)
	}
	// Resume the exact historical aggregate; no current active-position lookup.
	prior := positions[row.ID]
	progress := dailyPositionProgress{Vault: prior.vault, After: prior.vaultAfter, Beneficiary: prior.beneficiary, Accrued: prior.accrued, Paid: prior.paid}
	bytes, err := json.Marshal(progress)
	if err != nil { t.Fatal(err) }
	var resumed dailyPositionProgress
	if err := json.Unmarshal(bytes, &resumed); err != nil { t.Fatal(err) }
	positions[row.ID] = &lpodDailyPositionSum{id: row.ID, beneficiary: resumed.Beneficiary, vault: resumed.Vault, vaultAfter: resumed.After, accrued: resumed.Accrued, paid: resumed.Paid}
	// Final closed-position row repays the remaining reward and the principal.
	row.Accrued, row.Paid, row.PrincipalReturned = 0, 60, 500_000_000_000_000
	if err := accumulateDailyPosition(positions, row); err != nil { t.Fatal(err) }
	output := dailyPositionOutput(positions[row.ID])
	if output.Beneficiary != "guardian" || output.Accrued != "100" || output.Paid != "100" || output.PrincipalReturned != "500000000000000" {
		t.Fatalf("closed payout lost owner or exact amounts: %+v", output)
	}
	encoded, err := json.Marshal(output)
	if err != nil { t.Fatal(err) }
	var decoded map[string]string
	if err := json.Unmarshal(encoded, &decoded); err != nil { t.Fatal(err) }
	if decoded["beneficiary"] != "guardian" { t.Fatal("beneficiary missing from audit JSON") }
}

func TestDailyPositionRejectsBeneficiaryReplacementWithoutMutatingTotals(t *testing.T) {
	positions := make(map[string]*lpodDailyPositionSum)
	row := store.LPoDAuditPosition{ID: "position", Beneficiary: "owner", Accrued: 10}
	if err := accumulateDailyPosition(positions, row); err != nil { t.Fatal(err) }
	row.Beneficiary, row.Accrued = "attacker", 20
	if err := accumulateDailyPosition(positions, row); err == nil { t.Fatal("beneficiary replacement accepted") }
	if positions[row.ID].accrued != 10 || positions[row.ID].beneficiary != "owner" { t.Fatal("failed row changed aggregate") }
	row.Beneficiary = ""
	if err := accumulateDailyPosition(positions, row); err == nil { t.Fatal("missing beneficiary accepted") }
}