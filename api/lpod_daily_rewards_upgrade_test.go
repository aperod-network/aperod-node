package api

import "testing"

func TestDailyRewardsUpgradeReplaysOnlyLegacyProgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *dailyProgress
		want bool
	}{
		{"absent", nil, false},
		{"new", &dailyProgress{}, false},
		{"legacy", &dailyProgress{Next: 42}, true},
		{"detailed", &dailyProgress{Next: 42, Rewards: newDailyRewardProgress()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dailyRewardsNeedReplay(tc.p); got != tc.want {
				t.Fatalf("needs replay = %v, want %v", got, tc.want)
			}
		})
	}
}
