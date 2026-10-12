package api

import (
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestDailyRewardsUniquePeopleAndHistoricalTier(t *testing.T) {
	p := newDailyRewardProgress()
	block := &core.Block{}
	record := &store.LPoDBlockAudit{Height: 10,
		Vaults: []store.LPoDAuditVault{{ID: "vault", TierStake: 6000000 * 100000000, APRPercent: 8, GuardianPaid: 3}},
		Positions: []store.LPoDAuditPosition{
			{Beneficiary: "same-owner", EffectiveVault: "vault", Paid: 1},
			{Beneficiary: "same-owner", EffectiveVault: "vault", Paid: 2},
		}}
	if err := p.accumulate(block, record); err != nil {
		t.Fatal(err)
	}
	if err := p.accumulate(block, record); err != nil {
		t.Fatal(err)
	}
	if len(p.People[3]) != 1 || p.Tiers[3].Amount != 6 {
		t.Fatal("position count substituted for people or paid sum wrong")
	}
	record.Vaults[0].TierStake, record.Vaults[0].APRPercent = 1000000*100000000, 6
	if err := p.accumulate(block, record); err != nil {
		t.Fatal(err)
	}
	if len(p.People[2]) != 1 || p.Tiers[2].Amount != 3 {
		t.Fatal("historical tier change omitted")
	}
}

func TestDailyRewardsValidatorRecipientAndHeight(t *testing.T) {
	address := crypto.Address("aproegAWbHEE8wunGatJwGLM68f3hskmbQmT8HJik9s827DymS2jScRb2hxqAgD34qfXwfQKKDCoj2okTWeVwsi2CXtA9Ybr76")
	extra, _ := json.Marshal(core.LPoDPayoutAuthorization{Leader: address})
	block := &core.Block{Txs: []core.Transaction{{Version: core.TxVersionLPoDPayout, Extra: extra}}}
	p := newDailyRewardProgress()
	record := &store.LPoDBlockAudit{Height: 10, Vaults: []store.LPoDAuditVault{{ID: "vault", ActualLeaderPaid: 45}}}
	if err := p.accumulate(block, record); err != nil {
		t.Fatal(err)
	}
	record.Height = 12
	if err := p.accumulate(block, record); err != nil {
		t.Fatal(err)
	}
	r := p.Validators[string(address)]
	if r.Amount != 90 || r.First != 10 || r.Last != 12 || r.Blocks != 2 {
		t.Fatalf("wrong recipient aggregate: %+v", r)
	}
}

func TestDailyRewardsCheckpointCopiesDoNotLeakUncommittedChanges(t *testing.T) {
	p := newDailyRewardProgress()
	p.People[0]["owner"] = true
	saved := p.clone()
	p.People[0]["new-owner"] = true
	p.Tiers[0].Amount = 100
	if len(saved.People[0]) != 1 || saved.Tiers[0].Amount != 0 {
		t.Fatal("uncommitted progress leaked into saved checkpoint")
	}
	var absent *dailyRewardProgress
	if absent.clone() != nil {
		t.Fatal("old progress synthesized new monetary evidence")
	}
}
