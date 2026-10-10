package deploy_test

import (
	"os"
	"strings"
	"testing"
)

// TestPublicRewardDocsRejectLegacyEconomics prevents the public operator
// documentation from silently reverting to reward models that are no longer
// active on current networks.
func TestPublicRewardDocsRejectLegacyEconomics(t *testing.T) {
	files := []string{
		"SECURITY.md",
		"BURN_POLICY.md",
		"VALIDATORS.md",
		"INVESTORS.md",
		"README-public.md",
	}
	forbidden := []string{
		"Block reward: **5 APRO**",
		"| Block reward | **5 APRO**",
		"0.1 APRO",
		"Halving interval",
		"chmod 640",
		"@aperod_bot",
	}

	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reward_docs_sync: cannot open %s: %v", name, err)
		}
		doc := string(data)
		for _, stale := range forbidden {
			if strings.Contains(doc, stale) {
				t.Errorf("%s contains legacy statement %q", name, stale)
			}
		}
	}
}

func TestCurrentRewardIsDocumentedAcrossPublicSpecs(t *testing.T) {
	for _, name := range []string{"SECURITY.md", "BURN_POLICY.md", "VALIDATORS.md", "README-public.md"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reward_docs_sync: cannot open %s: %v", name, err)
		}
		doc := string(data)
		if !strings.Contains(doc, "3 APRO") || !strings.Contains(doc, "1 APRO") {
			t.Errorf("%s must document the 3 APRO pool reward and 1 APRO tail emission", name)
		}
	}
}
