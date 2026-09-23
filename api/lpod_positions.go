// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

// This projection contains public protocol information only. In particular it
// never returns private keys. DepositJSON is the already-public v9 action:
// its deposit opening is disclosed by the protocol itself, not a wallet secret.
type lpodWalletPosition struct {
	ID          string `json:"id"`
	Vault       string `json:"vault"`
	Principal   string `json:"principal_napro"`
	Returned    string `json:"returned_napro"`
	Due         string `json:"due_napro"`
	Nonce       string `json:"nonce"`
	DepositJSON string `json:"deposit_action_json"`
}

func lpodWalletProjection(c *store.LPoDCheckpoint, address crypto.Address) ([]lpodWalletPosition, uint64, error) {
	rows := make([]lpodWalletPosition, 0)
	var reserved uint64
	for id, p := range c.Positions {
		if p.Deposit.Beneficiary != address {
			continue
		}
		if p.Withdrawn > p.Deposit.Amount {
			return nil, 0, fmt.Errorf("invalid principal")
		}
		principal := p.Deposit.Amount - p.Withdrawn
		if p.Returned {
			principal = 0
		}
		if principal > ^uint64(0)-reserved {
			return nil, 0, fmt.Errorf("principal overflow")
		}
		reserved += principal
		action, err := json.Marshal(p.Deposit)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, lpodWalletPosition{
			ID: id, Vault: fmt.Sprintf("%x", p.Deposit.Vault[:]),
			Principal: strconv.FormatUint(principal, 10), Returned: strconv.FormatUint(p.Withdrawn, 10),
			Due: strconv.FormatUint(p.Due, 10), Nonce: strconv.FormatUint(p.Nonce, 10),
			DepositJSON: string(action),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, reserved, nil
}

func (s *Server) restLPoDPositions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "GET only")
		return
	}
	address := crypto.Address(r.URL.Query().Get("address"))
	if _, _, _, err := crypto.DecodeAddress(address); err != nil {
		writeJSONError(w, 400, "valid wallet address required")
		return
	}
	out := map[string]interface{}{"version": 1, "state": "disabled", "address": address,
		"reserved_napro": nil, "positions": nil, "checkpoint_hash": nil, "finalized_height": nil,
		"wallet_mutations_supported": false}
	defer func() { writeJSON(w, 200, out) }()
	if s.lpodMigration == nil {
		return
	}
	out["state"] = "unavailable"
	if s.blockStore == nil {
		return
	}
	hash, height, err := s.blockStore.GetTip()
	if err != nil {
		return
	}
	c, err := s.blockStore.LoadLPoDCheckpointAt(hash)
	if err != nil {
		return
	}
	if c == nil || c.Allocation == nil {
		out["state"] = "pending"
		return
	}
	a := c.Allocation
	if a.Genesis != s.lpodMigration.Genesis || a.ReconciliationRoot != s.lpodMigration.ReconciliationRoot ||
		a.FundingHeight != s.lpodMigration.Height || c.State.LastHeight != height {
		return
	}
	if s.lpodFinalized == nil || !s.lpodFinalized(height, hash) {
		out["state"] = "pending"
		return
	}
	rows, total, err := lpodWalletProjection(c, address)
	if err != nil {
		return
	}
	vaults := make([]map[string]interface{}, 0)
	if s.registry != nil {
		for _, pub := range s.registry.GetActiveValidators() {
			entry, ok := s.registry.GetEntry(pub)
			if !ok || entry.Seeded {
				continue
			}
			total := entry.StakeNAPR
			for _, p := range c.Positions {
				if fmt.Sprintf("%x", p.Deposit.Vault[:]) != pub.Hex() || p.Returned {
					continue
				}
				n := p.Deposit.Amount - p.Withdrawn
				if n > ^uint64(0)-total {
					return
				}
				total += n
			}
			tier, err := lpod.TierFor(total)
			if err != nil {
				continue
			}
			vaults = append(vaults, map[string]interface{}{"id": pub.Hex(), "total_napro": strconv.FormatUint(total, 10),
				"minimum_napro": strconv.FormatUint(tier.MinimumDeposit, 10), "apr_percent": tier.APRPercent, "leader_percent": tier.LeaderPercent})
		}
	}
	after, h, err := s.blockStore.GetTip()
	if err != nil || after != hash || h != height {
		return
	}
	out["state"] = "active"
	out["wallet_mutations_supported"] = true
	out["chain_anchor"] = fmt.Sprintf("%x", a.Genesis[:])
	out["vaults"] = vaults
	out["positions"] = rows
	out["reserved_napro"] = strconv.FormatUint(total, 10)
	out["checkpoint_hash"] = fmt.Sprintf("%x", hash[:])
	out["finalized_height"] = height
}
