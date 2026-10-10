// Package deploy_test — Task #395
// Confirms that the values documented in BURN_POLICY.md match the protocol
// constants in consensus/poa.go so the two never drift silently.
//
// Checked constants
// ─────────────────
//   - consensus.DefaultPoolBlockRewardNAPR must match the pool-phase reward
//   - 28 800 blocks / day               derived from 3-second block time
//
// Run from the blockchain root:
//
//	go test ./deploy/...
package deploy_test

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/aperod/aperod/consensus"
)

// readBurnPolicy loads BURN_POLICY.md from the same directory as this file.
func readBurnPolicy(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("BURN_POLICY.md")
	if err != nil {
		t.Fatalf("burn_policy_sync: cannot open BURN_POLICY.md: %v", err)
	}
	return string(data)
}

// extractBurnInt finds the first match of re in doc, strips comma-formatting,
// and returns the int64 value.
func extractBurnInt(t *testing.T, doc, pattern, label string) int64 {
	t.Helper()
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("burn_policy_sync: cannot find %q in BURN_POLICY.md\n  pattern: %s", label, pattern)
	}
	raw := strings.ReplaceAll(m[1], ",", "")
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatalf("burn_policy_sync: cannot parse %q value %q: %v", label, m[1], err)
	}
	return v
}

// TestBurnPolicyMatchesProtocolConstants verifies that BURN_POLICY.md stays
// in sync with the Go constants that govern the network's economics.
func TestBurnPolicyMatchesProtocolConstants(t *testing.T) {
	doc := readBurnPolicy(t)

	// ── 1. Pool-phase reward ──────────────────────────────────────────────────
	mdRewardNAPR := extractBurnInt(t, doc,
		`Pool-phase reward\s*\|\s*\*\*3 APRO\*\*\s*\(\*\*([\d,]+)\s*nAPRO\*\*\)`,
		"DefaultPoolBlockRewardNAPR")
	if int64(consensus.DefaultPoolBlockRewardNAPR) != mdRewardNAPR {
		t.Errorf(
			"burn_policy_sync MISMATCH — DefaultPoolBlockRewardNAPR:\n  Go constant  : %d\n  BURN_POLICY.md: %d\n"+
				"  → update one of them so they agree",
			consensus.DefaultPoolBlockRewardNAPR, mdRewardNAPR)
	}

	// ── 2. Blocks per day ─────────────────────────────────────────────────────
	// BURN_POLICY.md: "| Block throughput | **28,800 blocks / day** |"
	// Block time is 3 seconds → 86 400 / 3 = 28 800 blocks/day.
	// There is no named Go constant for this; we derive it from the fixed
	// 3-second slot time that PoA enforces.
	const blockTimeSecs = 3
	const secsPerDay = 86_400
	const expectedBlocksPerDay = secsPerDay / blockTimeSecs // 28 800

	mdBlocksPerDay := extractBurnInt(t, doc,
		`Block throughput\s*\|\s*\*\*([\d,]+)\s*blocks\s*/\s*day\*\*`,
		"BlocksPerDay")
	if mdBlocksPerDay != expectedBlocksPerDay {
		t.Errorf(
			"burn_policy_sync MISMATCH — BlocksPerDay:\n  expected (86400/3s): %d\n  BURN_POLICY.md    : %d\n"+
				"  → BURN_POLICY.md must document 28,800 blocks/day",
			expectedBlocksPerDay, mdBlocksPerDay)
	}

	// Also verify the "Block math" footnote at the bottom of the file to make
	// sure it agrees with the table values.
	// "28,800 blocks/day × 365 days = 10,512,000 blocks/year."
	footnoteBlocksPerDay := extractBurnInt(t, doc,
		`([\d,]+)\s*blocks/day\s*×\s*365\s*days`,
		"BlocksPerDay (footnote)")
	if footnoteBlocksPerDay != expectedBlocksPerDay {
		t.Errorf(
			"burn_policy_sync MISMATCH — BlocksPerDay footnote:\n  expected: %d\n  found   : %d",
			expectedBlocksPerDay, footnoteBlocksPerDay)
	}
}
