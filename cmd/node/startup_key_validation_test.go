package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/config"
)

func TestValidateStartupKeyPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-validator.key")
	if err := validateStartupKeyPath(missing, dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing explicit key: expected not-exist error, got %v", err)
	}
	if err := validateStartupKeyPath("", dir); err != nil {
		t.Fatalf("absent default key should permit generation at startup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "validator.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight must not create a key: %v", err)
	}
	if err := validateStartupKeyPath(dir, dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory used as explicit key: got %v", err)
	}

	path := filepath.Join(dir, "validator.key")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateStartupKeyPath(path, dir); err != nil {
		t.Fatalf("private, readable file: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	for _, configuredPath := range []string{"", path} {
		if err := validateStartupKeyPath(configuredPath, dir); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
			t.Fatalf("unsafe key file permitted with explicit=%t: %v", configuredPath != "", err)
		}
	}
}

func TestRehearsalIdentityNeverPersistsOrLoadsValidatorKey(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.Consensus.NonValidator = true
	cfg.Consensus.RehearsalFinalityV2Height = 2
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := loadOrGenerateValidatorKey(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Destroy()
	path := filepath.Join(cfg.DataDir, "validator.key")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rehearsal must not persist a key: %v", err)
	}
	if err := os.WriteFile(path, []byte("must remain untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := loadOrGenerateValidatorKey(cfg, log)
	if err != nil {
		t.Fatalf("rehearsal must not read the existing default key: %v", err)
	}
	defer second.Destroy()
	if first.Public().Equals(second.Public()) {
		t.Fatal("rehearsals reused an identity")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "must remain untouched" {
		t.Fatalf("rehearsal changed the existing key file: %v", err)
	}
	cfg.Consensus.ValidatorKey = path
	if key, err := loadOrGenerateValidatorKey(cfg, log); key != nil || err == nil {
		t.Fatal("rehearsal accepted an explicit signing key")
	}
}
