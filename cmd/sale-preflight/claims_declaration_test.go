package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimsFileRejectsAmbiguousShapeAndOversize(t *testing.T) {
	valid := filepath.Join(t.TempDir(), "valid.json")
	if err := os.WriteFile(valid, []byte(`{"scope":"operator-forward-declaration-not-activation-v1","guardian_total_napro":"100000000000000000","additional_off_registry_napro":"0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if claims, err := loadClaims(valid); err != nil || claims.Additional != "0" {
		t.Fatalf("valid declaration rejected: %v", err)
	}
	for _, content := range []string{
		`{"scope":"operator-forward-declaration-not-activation-v1","guardian_total_napro":100000000000000000,"additional_off_registry_napro":"0"}`,
		`{"unknown":"0"}`,
		`{} {}`,
		`{}` + strings.Repeat(" ", 4096),
	} {
		file := filepath.Join(t.TempDir(), "claims.json")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadClaims(file); err == nil {
			t.Fatal("unsafe claims file accepted")
		}
	}
}

func TestExactAmountRejectsAmbiguousDecimalAndOverflow(t *testing.T) {
	for _, value := range []string{"", "00", "-1", "1e8", "1.0", "18446744073709551616"} {
		if _, err := exactAmount(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	for _, value := range []string{"0", "1", "1000000000000000000"} {
		if _, err := exactAmount(value); err != nil {
			t.Fatalf("rejected %q: %v", value, err)
		}
	}
}
