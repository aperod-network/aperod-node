// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

// SetLPoDConfig installs public protocol parameters and a hash-specific finality
// oracle. It does not authorize or initialize a balance.
func (s *Server) SetLPoDConfig(m *store.LPoDMigration, finalized func(uint64, crypto.Hash32) bool) {
	s.lpodMigration, s.lpodFinalized = m, finalized
}

func (s *Server) restLPoDPool(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	out := map[string]interface{}{
		"version": 1, "protocol_version": 1, "state": "disabled",
		"accounting_basis":              "canonical_protocol_ledger",
		"initial_napro":                 strconv.FormatUint(lpod.InitialNAPRO, 10),
		"guardian_membership_supported": true,
		"immediate_exit_supported":      true,
		"partial_exit_supported":        true,
		"additional_deposits_supported": true,
	}
	for _, k := range []string{"balance_napro", "reward_inflow_napro", "leader_paid_napro", "angel_paid_napro",
		"surplus_inflow_napro", "deficit_outflow_napro", "unfunded_liability_napro", "funding_debit_napro",
		"total_guardian_stake_napro", "last_settled_height", "allocation_remaining_napro", "funding_height",
		"funding_block_hash", "chain_anchor", "reconciliation_root", "finalized_height",
		"principal_deposited_napro", "principal_locked_napro", "principal_returned_napro",
		"validator_remaining_napro", "tail_issued_napro", "position_count"} {
		out[k] = nil
	}
	defer func() { writeJSON(w, http.StatusOK, out) }()
	if s.lpodMigration == nil {
		return
	}
	out["state"] = "pending"
	if s.blockStore == nil {
		out["state"] = "unavailable"
		return
	}
	tip, height, err := s.blockStore.GetTip()
	if err != nil {
		out["state"] = "unavailable"
		return
	}
	c, err := s.blockStore.LoadLPoDCheckpointAt(tip)
	if err != nil {
		out["state"] = "unavailable"
		return
	}
	if c == nil || c.Allocation == nil {
		return
	}
	a := c.Allocation
	if a.Genesis != s.lpodMigration.Genesis || a.ReconciliationRoot != s.lpodMigration.ReconciliationRoot ||
		a.FundingHeight != s.lpodMigration.Height {
		out["state"] = "unavailable"
		return
	}
	if s.lpodFinalized == nil || !s.lpodFinalized(height, tip) {
		return
	}
	// Recheck the durable tip after the finality callback to avoid pairing a
	// checkpoint with a concurrently replaced branch.
	after, afterHeight, err := s.blockStore.GetTip()
	if err != nil || after != tip || afterHeight != height || c.State.LastHeight != height {
		return
	}
	out["state"] = "active"
	for key, value := range map[string]uint64{
		"balance_napro": c.State.Balance, "reward_inflow_napro": c.State.RewardInflow,
		"leader_paid_napro": c.State.LeaderPaid, "angel_paid_napro": c.State.AngelPaid,
		"surplus_inflow_napro": c.State.SurplusInflow, "deficit_outflow_napro": c.State.DeficitOutflow,
		"unfunded_liability_napro": c.State.UnfundedLiability, "funding_debit_napro": c.State.FundingDebit,
		"total_guardian_stake_napro": c.PrincipalLocked, "allocation_remaining_napro": a.Remaining,
		"principal_deposited_napro": c.PrincipalDeposited, "principal_locked_napro": c.PrincipalLocked,
		"principal_returned_napro": c.PrincipalReturned, "validator_remaining_napro": a.ValidatorRemaining,
		"tail_issued_napro": a.TailIssued,
	} {
		out[key] = strconv.FormatUint(value, 10)
	}
	out["last_settled_height"], out["finalized_height"] = height, height
	openPositions := 0
	for _, p := range c.Positions {
		// Closed records may remain for arrears or replay accounting; they are
		// not open principal positions.
		if p.Deposit.Amount > p.Withdrawn {
			openPositions++
		}
	}
	out["position_count"] = openPositions
	out["funding_height"] = a.FundingHeight
	out["funding_block_hash"] = fmt.Sprintf("%x", a.FundingBlock[:])
	out["chain_anchor"] = fmt.Sprintf("%x", a.Genesis[:])
	out["reconciliation_root"] = fmt.Sprintf("%x", a.ReconciliationRoot[:])
}
