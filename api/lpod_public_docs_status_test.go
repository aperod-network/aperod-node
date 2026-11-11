package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	currentMainnetStatusMarker = regexp.MustCompile(`(?i)\b(?:public network status|current public network|mainnet status|current public mainnet|mainnet lpod v3)\b`)
	nonCurrentLpodScope        = regexp.MustCompile(`(?i)\b(?:historical|rehearsal|new network|another chain|default-disabled|repository/config)\b`)
	staleLpodActivationClaim   = regexp.MustCompile(`(?i)(?:\b(?:if|when|once)\s+lpod(?:\s+v3)?\b.{0,120}\b(?:activated|enabled|funded)\b|\blpod(?:\s+v3)?\b.{0,120}\b(?:will be activated|will activate|has not(?: yet)? been activated|(?:is|was) not(?: yet)? active|not yet active|remains inactive|awaits activation|activation is planned)\b)`)
	futureLpodVersion          = regexp.MustCompile(`(?i)\blpod\s+v[4-9][0-9]*\b`)
	currentLpodVersion         = regexp.MustCompile(`(?i)\blpod\s+v3\b`)
)

func isStaleCurrentLpodClaim(text string) bool {
	if futureLpodVersion.MatchString(text) && !currentLpodVersion.MatchString(text) {
		return false
	}
	if nonCurrentLpodScope.MatchString(text) && !currentMainnetStatusMarker.MatchString(text) {
		return false
	}
	return staleLpodActivationClaim.MatchString(text)
}

func findPublishedDocsRoot(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for dir := workingDir; ; dir = filepath.Dir(dir) {
		if exists(filepath.Join(dir, "README.md")) &&
			exists(filepath.Join(dir, "LPOD.md")) &&
			exists(filepath.Join(dir, "deploy", "README-public.md")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("published root documents are not present in this monorepo checkout")
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func paragraphAtMarker(document, marker string) (string, bool) {
	index := strings.Index(document, marker)
	if index < 0 {
		return "", false
	}
	start := strings.LastIndex(document[:index], "\n\n") + 2
	if end := strings.Index(document[index:], "\n\n"); end >= 0 {
		return document[start : index+end], true
	}
	return document[start:], true
}

func assertCurrentLpodStatus(t *testing.T, document, marker string) {
	t.Helper()
	paragraph, ok := paragraphAtMarker(document, marker)
	if !ok {
		t.Fatalf("current-status marker %q is missing", marker)
	}
	if !strings.Contains(strings.ToLower(paragraph), "lpod v3") {
		t.Errorf("current-status paragraph %q does not name LPoD v3", marker)
	}
	normalizedHeight := strings.NewReplacer(",", "").Replace(
		strings.Join(strings.Fields(paragraph), ""),
	)
	if !strings.Contains(normalizedHeight, "2493218") {
		t.Errorf("current-status paragraph %q does not include canonical activation height 2,493,218", marker)
	}
	if !regexp.MustCompile(`(?i)\b(?:active|activated|funded|complete)\b`).MatchString(paragraph) {
		t.Errorf("current-status paragraph %q does not say the mainnet deployment is active and funded", marker)
	}
	if isStaleCurrentLpodClaim(paragraph) {
		t.Errorf("current-status paragraph %q reverted to a future-activation claim: %s", marker, paragraph)
	}
}

func TestPublishedLpodDocumentsDescribeCurrentMainnetStatus(t *testing.T) {
	root := findPublishedDocsRoot(t)
	documents := []struct {
		path    string
		markers []string
	}{
		{
			path:    filepath.Join(root, "README.md"),
			markers: []string{"LPoD v3 is active on Aperod from canonical height", "**Current public network:**"},
		},
		{
			path:    filepath.Join(root, "LPOD.md"),
			markers: []string{"Mainnet status: LPoD v3", "Mainnet LPoD v3 funding and activation are complete:"},
		},
		{
			path:    filepath.Join(root, "deploy", "README-public.md"),
			markers: []string{"The current public mainnet also has funded, active LPoD v3"},
		},
	}

	for _, doc := range documents {
		content, err := os.ReadFile(doc.path)
		if err != nil {
			t.Fatalf("read published document %s: %v", doc.path, err)
		}
		for _, marker := range doc.markers {
			t.Run(filepath.Base(doc.path)+"/"+marker, func(t *testing.T) {
				assertCurrentLpodStatus(t, string(content), marker)
			})
		}
	}
}

func TestLpodStatusRegressionScopePreservesHistoricalAndOperationalStates(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "unscoped future activation is stale",
			text: "If LPoD v3 is activated and funded, the pool will distribute rewards.",
			want: true,
		},
		{
			name: "historical report",
			text: "Historical report: LPoD v3 was not active at that checkpoint.",
			want: false,
		},
		{
			name: "new network default-disabled",
			text: "New network repository/config default-disabled: LPoD v3 will be activated only with its own authenticated witness.",
			want: false,
		},
		{
			name: "future protocol version",
			text: "LPoD v4 will be activated in a future version.",
			want: false,
		},
		{
			name: "truthful API state",
			text: "The LPoD v3 API response state can be pending, disabled, or unavailable.",
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isStaleCurrentLpodClaim(test.text); got != test.want {
				t.Fatalf("isStaleCurrentLpodClaim(%q) = %t, want %t", test.text, got, test.want)
			}
		})
	}
}
