// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func loadLPoDMigration(path string, db *store.DB, chain *core.Chain, validators []crypto.ValidatorPubKey) (*store.LPoDMigration, error) {
	var m *store.LPoDMigration
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("lpod reconciliation witness: %w", err)
		}
		if err := json.Unmarshal(raw, &m); err != nil || m == nil {
			return nil, fmt.Errorf("lpod: invalid reconciliation witness: %v", err)
		}
		g := chain.Genesis()
		m.TrustedValidators = validators
		if err := m.VerifyAuthorization(); err != nil {
			return nil, err
		}
		if g == nil || m.Genesis != g.Hash() || m.Height == 0 || m.Version != 1 || m.Root() != m.ReconciliationRoot {
			return nil, fmt.Errorf("lpod: invalid canonical genesis, activation or proof commitment")
		}
		// Witness is necessarily complete only once the parent is canonical.
		// No funds are credited at startup or when scheduling a future height.
		if chain.Height() == m.Height-1 {
			if _, _, err := db.VerifyLPoDMigration(m); err != nil {
				return nil, err
			}
		}
		if chain.Height() >= m.Height {
			c, err := db.LoadLPoDCheckpoint()
			if err != nil || c == nil || c.Allocation == nil || c.Allocation.ReconciliationRoot != m.ReconciliationRoot {
				return nil, fmt.Errorf("lpod: past activation requires canonical funded checkpoint; no retroactive credit")
			}
		}
	}
	if err := db.CheckLPoDConfig(m); err != nil {
		return nil, err
	}
	return m, nil
}
