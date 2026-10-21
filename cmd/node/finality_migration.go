package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const maxFinalityMigrationFileBytes = 1 << 20

func loadFinalityCommitteeMigration(path string, authorityHex []string) (*store.FinalityCommitteeMigration, []crypto.ValidatorPubKey, error) {
	if path == "" {
		if len(authorityHex) != 0 {
			return nil, nil, fmt.Errorf("finality migration authorities configured without a migration file")
		}
		return nil, nil, nil
	}
	if len(authorityHex) == 0 {
		return nil, nil, fmt.Errorf("finality migration file requires explicit trust authorities")
	}
	if len(authorityHex) > 256 {
		return nil, nil, fmt.Errorf("finality migration has too many trust authorities")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("stat finality migration: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxFinalityMigrationFileBytes {
		return nil, nil, fmt.Errorf("finality migration must be a regular file no larger than %d bytes",
			maxFinalityMigrationFileBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read finality migration: %w", err)
	}
	if len(raw) > maxFinalityMigrationFileBytes {
		return nil, nil, fmt.Errorf("finality migration exceeds the %d-byte limit", maxFinalityMigrationFileBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var migration store.FinalityCommitteeMigration
	if err := decoder.Decode(&migration); err != nil {
		return nil, nil, fmt.Errorf("decode finality migration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, nil, fmt.Errorf("finality migration contains trailing data")
	}
	if len(migration.Approvals) == 0 {
		return nil, nil, fmt.Errorf("finality migration has no validator approvals")
	}
	if len(migration.Approvals) > 256 {
		return nil, nil, fmt.Errorf("finality migration has too many validator approvals")
	}
	if _, err := migration.SigningMessage(); err != nil {
		return nil, nil, err
	}

	authorities := make([]crypto.ValidatorPubKey, 0, len(authorityHex))
	seen := make(map[string]bool, len(authorityHex))
	for _, value := range authorityHex {
		rawKey, err := hex.DecodeString(value)
		if err != nil || len(rawKey) != 32 {
			return nil, nil, fmt.Errorf("finality migration trust authority must be 32-byte hex")
		}
		key, err := crypto.ValidatorPubKeyFromBytes(rawKey)
		if err != nil || isZeroFinalityMigrationAuthority(key) {
			return nil, nil, fmt.Errorf("finality migration trust authority is invalid")
		}
		if seen[key.Hex()] {
			return nil, nil, fmt.Errorf("duplicate finality migration trust authority")
		}
		seen[key.Hex()] = true
		authorities = append(authorities, key)
	}
	sort.Slice(authorities, func(i, j int) bool {
		return bytes.Compare(authorities[i], authorities[j]) < 0
	})
	return &migration, authorities, nil
}

func isZeroFinalityMigrationAuthority(key crypto.ValidatorPubKey) bool {
	if len(key) != 32 {
		return false
	}
	for _, b := range key {
		if b != 0 {
			return false
		}
	}
	return true
}
