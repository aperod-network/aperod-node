package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestVerifyStageValidSmallStore(t *testing.T) {
	stage := t.TempDir()
	hash := crypto.Hash32{1, 2, 3}
	writeSmallStore(t, filepath.Join(stage, "chain.db"), hash, 7)
	writeManifest(t, stage, fmt.Sprintf(
		`{"success":true,"tip_hash":"%x","tip_height":7}`, hash[:]))

	gotHash, gotHeight, err := verifyStage(stage)
	if err != nil {
		t.Fatalf("verifyStage: %v", err)
	}
	if gotHash != fmt.Sprintf("%x", hash[:]) || gotHeight != 7 {
		t.Fatalf("got tip %s/%d, want %x/7", gotHash, gotHeight, hash[:])
	}
}

func TestVerifyStageRejectsMissingOrCorruptDB(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		stage := t.TempDir()
		writeManifest(t, stage, `{"success":true,"tip_hash":"`+repeatHex('0', 64)+`","tip_height":0}`)
		if _, _, err := verifyStage(stage); err == nil {
			t.Fatal("verifyStage succeeded without chain.db")
		}
	})

	t.Run("corrupt database", func(t *testing.T) {
		stage := t.TempDir()
		if err := os.WriteFile(filepath.Join(stage, "chain.db"), []byte("not a database"), 0600); err != nil {
			t.Fatal(err)
		}
		writeManifest(t, stage, `{"success":true,"tip_hash":"`+repeatHex('0', 64)+`","tip_height":0}`)
		if _, _, err := verifyStage(stage); err == nil {
			t.Fatal("verifyStage succeeded with a corrupt chain.db")
		}
	})
}

func TestVerifyStageRejectsTipMismatch(t *testing.T) {
	stage := t.TempDir()
	hash := crypto.Hash32{4, 5, 6}
	writeSmallStore(t, filepath.Join(stage, "chain.db"), hash, 12)
	writeManifest(t, stage, fmt.Sprintf(
		`{"success":true,"tip_hash":"%x","tip_height":11}`, hash[:]))
	if _, _, err := verifyStage(stage); err == nil {
		t.Fatal("verifyStage succeeded with a manifest tip mismatch")
	}
}

func TestVerifyLegacyStageValid(t *testing.T) {
	root := t.TempDir()
	testnet := filepath.Join(root, "testnet")
	if err := os.Mkdir(testnet, 0700); err != nil {
		t.Fatal(err)
	}
	hash := crypto.Hash32{7, 8, 9}
	writeSmallStore(t, filepath.Join(testnet, "chain.db"), hash, 21)
	if err := os.WriteFile(filepath.Join(root, "explorer_db.dump"), []byte("legacy dump"), 0600); err != nil {
		t.Fatal(err)
	}

	gotHash, gotHeight, err := verifyLegacyStage(root)
	if err != nil {
		t.Fatalf("verifyLegacyStage: %v", err)
	}
	if gotHash != fmt.Sprintf("%x", hash[:]) || gotHeight != 21 {
		t.Fatalf("got tip %s/%d, want %x/21", gotHash, gotHeight, hash[:])
	}
}

func TestVerifyLegacyStageRejectsUnreadableDB(t *testing.T) {
	root := makeLegacyRoot(t)
	if err := os.Mkdir(filepath.Join(root, "testnet", "chain.db"), 0700); err != nil {
		t.Fatal(err)
	}
	writeLegacyDump(t, root)
	if _, _, err := verifyLegacyStage(root); err == nil {
		t.Fatal("verifyLegacyStage succeeded with an unreadable database")
	}
}

func TestVerifyLegacyStageRejectsSymlink(t *testing.T) {
	root := makeLegacyRoot(t)
	testnet := filepath.Join(root, "testnet")
	external := filepath.Join(t.TempDir(), "chain.db")
	writeSmallStore(t, external, crypto.Hash32{2}, 1)
	if err := os.Symlink(external, filepath.Join(testnet, "chain.db")); err != nil {
		t.Skipf("cannot create test symlink: %v", err)
	}
	writeLegacyDump(t, root)
	if _, _, err := verifyLegacyStage(root); err == nil {
		t.Fatal("verifyLegacyStage accepted a symlinked chain.db")
	}
}

func TestVerifyLegacyStageRejectsMissingDump(t *testing.T) {
	root := makeLegacyRoot(t)
	writeSmallStore(t, filepath.Join(root, "testnet", "chain.db"), crypto.Hash32{3}, 2)
	if _, _, err := verifyLegacyStage(root); err == nil {
		t.Fatal("verifyLegacyStage succeeded without explorer_db.dump")
	}
}

func TestSelectModeRejectsIncompatibleMix(t *testing.T) {
	if _, err := selectMode("/new-format", "/legacy"); err == nil {
		t.Fatal("selectMode accepted both --stage and --legacy-stage")
	}
}

func TestReadManifestStrictValidation(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"unknown field", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":0,"extra":1}`},
		{"duplicate field", `{"success":true,"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":0}`},
		{"negative height", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":-1}`},
		{"fractional height", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":1.0}`},
		{"trailing JSON", `{"success":true,"tip_hash":"` + repeatHex('a', 64) + `","tip_height":0} {}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "manifest.json")
			if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readManifest(path); err == nil {
				t.Fatal("readManifest accepted invalid JSON")
			}
		})
	}
}

func writeSmallStore(t *testing.T, path string, hash crypto.Hash32, height uint64) {
	t.Helper()
	db, err := store.Open(path)
	if err != nil {
		t.Fatalf("create test chain.db: %v", err)
	}
	if err := db.PutRawBlock(hash, height, []byte(`{"test":"block"}`)); err != nil {
		_ = db.Close()
		t.Fatalf("write test block: %v", err)
	}
	if err := db.PutTip(hash, height); err != nil {
		_ = db.Close()
		t.Fatalf("write test tip: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close test chain.db: %v", err)
	}
}

func writeManifest(t *testing.T, stage, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), []byte(contents), 0600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

func makeLegacyRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "testnet"), 0700); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeLegacyDump(t *testing.T, root string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "explorer_db.dump"), []byte("legacy dump"), 0600); err != nil {
		t.Fatal(err)
	}
}

func repeatHex(c byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = c
	}
	return string(b)
}
