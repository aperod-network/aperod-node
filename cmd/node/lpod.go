// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func guardLPoDMigrationAuthorities(validators []crypto.ValidatorPubKey, genesisAuthorities []string) error {
	if len(genesisAuthorities) == 0 {
		return fmt.Errorf("lpod: configured genesis authority set is empty")
	}
	if len(validators) == 0 {
		return fmt.Errorf("lpod: effective validator authority set is empty")
	}

	genesisSet := make(map[string]struct{}, len(genesisAuthorities))
	for i, encoded := range genesisAuthorities {
		raw, err := hex.DecodeString(encoded)
		if err != nil {
			return fmt.Errorf("lpod: malformed configured genesis authority at index %d", i)
		}
		pub, err := crypto.ValidatorPubKeyFromBytes(raw)
		if err != nil {
			return fmt.Errorf("lpod: invalid configured genesis authority at index %d", i)
		}
		if isZeroLPoDAuthority(pub) {
			return fmt.Errorf("lpod: zero configured genesis authority at index %d", i)
		}
		key := string(pub)
		if _, exists := genesisSet[key]; exists {
			return fmt.Errorf("lpod: duplicate configured genesis authority at index %d", i)
		}
		genesisSet[key] = struct{}{}
	}

	effectiveSet := make(map[string]struct{}, len(validators))
	for i, pub := range validators {
		if len(pub) != 32 {
			return fmt.Errorf("lpod: invalid effective validator authority at index %d", i)
		}
		if isZeroLPoDAuthority(pub) {
			return fmt.Errorf("lpod: zero effective validator authority at index %d", i)
		}
		key := string(pub)
		if _, exists := effectiveSet[key]; exists {
			return fmt.Errorf("lpod: duplicate effective validator authority at index %d", i)
		}
		effectiveSet[key] = struct{}{}
	}

	if len(effectiveSet) != len(genesisSet) {
		return fmt.Errorf("lpod: effective validator authorities do not match configured genesis authorities")
	}
	for key := range genesisSet {
		if _, exists := effectiveSet[key]; !exists {
			return fmt.Errorf("lpod: effective validator authorities do not match configured genesis authorities")
		}
	}
	return nil
}

func isZeroLPoDAuthority(pub crypto.ValidatorPubKey) bool {
	for _, b := range pub {
		if b != 0 {
			return false
		}
	}
	return true
}

func parseLPoDV2TrustAuthorities(configured []string) ([]crypto.ValidatorPubKey, error) {
	return parseLPoDTrustAuthorities(configured, 2)
}

func parseLPoDV3TrustAuthorities(configured []string) ([]crypto.ValidatorPubKey, error) {
	return parseLPoDTrustAuthorities(configured, 3)
}

func parseLPoDTrustAuthorities(configured []string, version int) ([]crypto.ValidatorPubKey, error) {
	if len(configured) == 0 {
		return nil, fmt.Errorf("lpod: explicit version %d trust authority set is required", version)
	}
	authorities := make([]crypto.ValidatorPubKey, 0, len(configured))
	seen := make(map[string]bool, len(configured))
	for i, encoded := range configured {
		raw, err := hex.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("lpod: malformed version %d trust authority at index %d", version, i)
		}
		pub, err := crypto.ValidatorPubKeyFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("lpod: version %d trust authority at index %d must be exactly 32 bytes", version, i)
		}
		if isZeroLPoDAuthority(pub) {
			return nil, fmt.Errorf("lpod: zero version %d trust authority at index %d", version, i)
		}
		key := string(pub)
		if seen[key] {
			return nil, fmt.Errorf("lpod: duplicate version %d trust authority at index %d", version, i)
		}
		seen[key] = true
		authorities = append(authorities, pub)
	}
	return authorities, nil
}

// Check the consensus authorities separately from the migration trust anchor.
// In particular, a non-validator with the legacy zero-key genesis list cannot
// safely validate proposer/finality signatures even if its v2 checkpoint
// authority configuration is correct.
func guardLPoDConsensusAuthorities(validators []crypto.ValidatorPubKey) error {
	if len(validators) == 0 {
		return fmt.Errorf("lpod: effective consensus validator set is empty")
	}
	seen := make(map[string]bool, len(validators))
	for i, pub := range validators {
		if len(pub) != 32 || isZeroLPoDAuthority(pub) {
			return fmt.Errorf("lpod: invalid effective consensus authority at index %d", i)
		}
		key := string(pub)
		if seen[key] {
			return fmt.Errorf("lpod: duplicate effective consensus authority at index %d", i)
		}
		seen[key] = true
	}
	return nil
}

func decodeLPoDMigrationWitness(raw []byte) (*store.LPoDMigration, error) {
	var version struct {
		Version uint8 `json:"version"`
	}
	if err := json.Unmarshal(raw, &version); err != nil {
		return nil, err
	}
	if err := rejectDuplicateLPoDVersionFields(raw); err != nil {
		return nil, err
	}
	var m *store.LPoDMigration
	if version.Version == 2 || version.Version == 3 {
		if err := rejectDuplicateLPoDJSONFields(raw); err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&m); err != nil || m == nil {
			return nil, fmt.Errorf("invalid trusted checkpoint witness: %v", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return nil, fmt.Errorf("trailing trusted checkpoint witness data")
		}
		return m, nil
	}
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("invalid reconciliation witness: %v", err)
	}
	return m, nil
}

func loadLPoDMigration(path string, db *store.DB, chain *core.Chain, validators []crypto.ValidatorPubKey,
	genesisAuthorities, v2TrustAuthorities, v3TrustAuthorities []string) (*store.LPoDMigration, error) {
	var m *store.LPoDMigration
	if path == "" && (len(v2TrustAuthorities) != 0 || len(v3TrustAuthorities) != 0) {
		return nil, fmt.Errorf("lpod: trusted checkpoint authorities are configured but the migration witness is missing")
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("lpod reconciliation witness: %w", err)
		}
		m, err = decodeLPoDMigrationWitness(raw)
		if err != nil {
			return nil, fmt.Errorf("lpod: invalid reconciliation witness: %w", err)
		}
		g := chain.Genesis()
		switch m.Version {
		case 1:
			if len(v2TrustAuthorities) != 0 || len(v3TrustAuthorities) != 0 {
				return nil, fmt.Errorf("lpod: trusted checkpoint authorities cannot authorize a version 1 witness")
			}
			if err := guardLPoDMigrationAuthorities(validators, genesisAuthorities); err != nil {
				return nil, err
			}
			m.TrustedValidators = append([]crypto.ValidatorPubKey(nil), validators...)
		case 2:
			if len(v3TrustAuthorities) != 0 {
				return nil, fmt.Errorf("lpod: version 3 authorities cannot authorize a version 2 witness")
			}
			if err := guardLPoDConsensusAuthorities(validators); err != nil {
				return nil, err
			}
			authorities, err := parseLPoDV2TrustAuthorities(v2TrustAuthorities)
			if err != nil {
				return nil, err
			}
			m.TrustedValidators = authorities
		case 3:
			if len(v2TrustAuthorities) != 0 {
				return nil, fmt.Errorf("lpod: version 2 authorities cannot authorize a version 3 witness")
			}
			if err := guardLPoDConsensusAuthorities(validators); err != nil {
				return nil, err
			}
			authorities, err := parseLPoDV3TrustAuthorities(v3TrustAuthorities)
			if err != nil {
				return nil, err
			}
			m.TrustedValidators = authorities
		default:
			return nil, fmt.Errorf("lpod: unsupported migration version %d", m.Version)
		}
		if err := m.VerifyAuthorization(); err != nil {
			return nil, err
		}
		if g == nil || m.Genesis != g.Hash() || m.Height == 0 ||
			(m.Version != 1 && m.Version != 2 && m.Version != 3) ||
			m.Root() != m.ReconciliationRoot {
			return nil, fmt.Errorf("lpod: invalid canonical genesis, activation or proof commitment")
		}
		// Witness is necessarily complete only once the parent is canonical.
		// No funds are credited at startup or when scheduling a future height.
		if chain.Height() == m.Height-1 {
			if _, _, err := db.VerifyLPoDMigration(m); err != nil {
				return nil, err
			}
		}
		if err := requireLPoDFundedTip(db, m, chain.Height()); err != nil {
			return nil, err
		}
	}
	if err := db.CheckLPoDConfig(m); err != nil {
		return nil, err
	}
	return m, nil
}

func requireLPoDFundedTip(db *store.DB, m *store.LPoDMigration, tipHeight uint64) error {
	if m == nil || tipHeight < m.Height {
		return nil
	}
	_, dbHeight, tipErr := db.GetTip()
	if tipErr != nil || dbHeight != tipHeight {
		return fmt.Errorf("lpod: chain tip does not match the durable canonical tip")
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil || c == nil || c.Allocation == nil ||
		c.Allocation.Version != m.Version ||
		c.Allocation.FundingHeight != m.Height ||
		c.Allocation.Genesis != m.Genesis ||
		c.Allocation.ReconciliationRoot != m.ReconciliationRoot {
		return fmt.Errorf("lpod: past activation requires canonical funded checkpoint; no retroactive credit")
	}
	return nil
}

func rejectDuplicateLPoDVersionFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("migration witness must be an object")
	}
	versionSeen := false
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := field.(string)
		if !ok {
			return fmt.Errorf("invalid migration witness field")
		}
		if name == "version" {
			if versionSeen {
				return fmt.Errorf("duplicate migration version field")
			}
			versionSeen = true
		}
		if err := consumeLPoDJSONValue(decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func consumeLPoDJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := consumeLPoDJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := consumeLPoDJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("invalid JSON delimiter")
	}
}

func rejectDuplicateLPoDJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok || seen[key] {
					return fmt.Errorf("duplicate or invalid JSON object field")
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("invalid JSON delimiter")
		}
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
