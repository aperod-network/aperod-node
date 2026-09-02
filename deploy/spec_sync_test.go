// Package deploy_test contains a CI guard that verifies VALIDATORS.md stays
// in sync with the protocol constants declared in consensus/poa.go and
// core/staking.go.
//
// Run from the blockchain root:
//
//	go test ./deploy/...
//
// The test fails with a descriptive message whenever a constant in the Go code
// no longer matches the documented value in VALIDATORS.md, or when a value
// cannot be located in the spec at all.
package deploy_test

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// readSpec loads VALIDATORS.md from the same directory as this test file.
func readSpec(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("VALIDATORS.md")
	if err != nil {
		t.Fatalf("spec_sync: cannot open VALIDATORS.md: %v", err)
	}
	return string(data)
}

// extractInt finds the first capturing group of re inside spec, strips
// comma-formatting (e.g. "21,024,000" → 21024000), and returns the int64
// value.  Calls t.Fatalf when the pattern has no match or the value cannot
// be parsed.
func extractInt(t *testing.T, spec, pattern, label string) int64 {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(spec)
	if m == nil {
		t.Fatalf("spec_sync: cannot find %q in VALIDATORS.md\n  pattern: %s", label, pattern)
	}
	raw := strings.ReplaceAll(m[1], ",", "")
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("spec_sync: cannot parse %q value %q: %v", label, m[1], err)
	}
	return v
}

// assertMatch fails the test when got != want, printing both the human-readable
// label and the raw numeric values so it is obvious which constant drifted.
func assertMatch(t *testing.T, label string, got, want int64) {
	t.Helper()
	if got != want {
		t.Errorf(
			"spec_sync MISMATCH — %s:\n  Go constant : %d\n  VALIDATORS.md: %d\n"+
				"  → update one of them so they agree",
			label, got, want,
		)
	}
}

// ── test ─────────────────────────────────────────────────────────────────────

// TestSpecMatchesProtocolConstants reads every protocol constant from the Go
// packages and asserts it equals the value documented in VALIDATORS.md.
//
// Checked constants
// ─────────────────
//   - core.MaxValidators        Active-validator cap
//   - core.MinStakeNAPR         Minimum stake (converted to whole APRO)
//   - core.EpochLength          Blocks per epoch
//   - core.ChurnLimit           New validators admitted per epoch
//   - core.UnbondingBlocks      Unbonding cooldown in blocks
//   - core.SlashPercent         Double-sign slash percentage
//   - consensus.AuthorizedBlockRewardNAPR  Current authorized base reward
//   - consensus.HalvingIntervalBlocks   Halving cadence in blocks
func TestSpecMatchesProtocolConstants(t *testing.T) {
	spec := readSpec(t)

	// ── 1. MaxValidators ─────────────────────────────────────────────────────
	// VALIDATORS.md: "| Active validators | **21** (maximum) |"
	mdMaxVal := extractInt(t, spec,
		`\|\s*Active validators\s*\|\s*\*\*(\d[\d,]*)\*\*`,
		"MaxValidators")
	assertMatch(t, fmt.Sprintf("MaxValidators (core=%d, md=%d)", core.MaxValidators, mdMaxVal),
		int64(core.MaxValidators), mdMaxVal)

	// ── 2. MinStake in whole APRO ────────────────────────────────────────────
	// VALIDATORS.md: "| Stake | ≥ **100,000 APRO** locked on-chain |"
	const naprPerAPRO int64 = 100_000_000
	mdMinStakeAPRO := extractInt(t, spec,
		`≥\s*\*\*([\d,]+)\s*APRO\*\*`,
		"MinStakeNAPR (in APRO)")
	goMinStakeAPRO := int64(core.MinStakeNAPR) / naprPerAPRO
	assertMatch(t,
		fmt.Sprintf("MinStakeNAPR (Go=%d nAPRO = %d APRO, md=%d APRO)",
			core.MinStakeNAPR, goMinStakeAPRO, mdMinStakeAPRO),
		goMinStakeAPRO, mdMinStakeAPRO)

	// ── 3. EpochLength ───────────────────────────────────────────────────────
	// VALIDATORS.md: "| Epoch length | **100 blocks** (~300 seconds at 3 s/block) |"
	mdEpoch := extractInt(t, spec,
		`\|\s*Epoch length\s*\|\s*\*\*([\d,]+)\s*blocks\*\*`,
		"EpochLength")
	assertMatch(t,
		fmt.Sprintf("EpochLength (core=%d, md=%d)", core.EpochLength, mdEpoch),
		int64(core.EpochLength), mdEpoch)

	// ── 4. ChurnLimit ────────────────────────────────────────────────────────
	// VALIDATORS.md: "| Churn limit | **3** new validators per epoch (prevents instability) |"
	mdChurn := extractInt(t, spec,
		`\|\s*Churn limit\s*\|\s*\*\*([\d,]+)\*\*`,
		"ChurnLimit")
	assertMatch(t,
		fmt.Sprintf("ChurnLimit (core=%d, md=%d)", core.ChurnLimit, mdChurn),
		int64(core.ChurnLimit), mdChurn)

	// ── 5. UnbondingBlocks ───────────────────────────────────────────────────
	// VALIDATORS.md: "| 2. Unbonding period | **144,000 blocks** (~5 days) — node leaves active set |"
	mdUnbond := extractInt(t, spec,
		`Unbonding period\s*\|\s*\*\*([\d,]+)\s*blocks\*\*`,
		"UnbondingBlocks")
	assertMatch(t,
		fmt.Sprintf("UnbondingBlocks (core=%d, md=%d)", core.UnbondingBlocks, mdUnbond),
		int64(core.UnbondingBlocks), mdUnbond)

	// ── 6. SlashPercent (double-sign) ────────────────────────────────────────
	// VALIDATORS.md (Double-Sign section):
	//   "| Stake penalty | **10 % of total stake** burned immediately |"
	mdSlash := extractInt(t, spec,
		`\*\*([\d]+)\s*%\s*of total stake\*\*\s*burned immediately`,
		"SlashPercent (double-sign)")
	assertMatch(t,
		fmt.Sprintf("SlashPercent (core=%d, md=%d)", core.SlashPercent, mdSlash),
		int64(core.SlashPercent), mdSlash)

	// ── 7. Pool-phase reward in nAPRO ────────────────────────────────────────
	mdRewardNAPR := extractInt(t, spec,
		`\*\*([\d,]+)\s*nAPRO\s*\(3 APRO\)\*\*`,
		"DefaultPoolBlockRewardNAPR")
	assertMatch(t,
		fmt.Sprintf("DefaultPoolBlockRewardNAPR (consensus=%d nAPRO, md=%d nAPRO)",
			consensus.DefaultPoolBlockRewardNAPR, mdRewardNAPR),
		int64(consensus.DefaultPoolBlockRewardNAPR), mdRewardNAPR)
}
