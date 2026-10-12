// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

func (s *Server) SetGenesisVestingConfig(height uint64) { s.genesisVestingHeight = height }

// Public execution evidence comes from the committed reserve ledger, never
// from editable SQL transaction hashes or the visitor's wall-clock countdown.
func (s *Server) restGenesisVesting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet { writeJSONError(w, http.StatusMethodNotAllowed, "GET only"); return }
	out := map[string]interface{}{
		"state": "disabled", "activation_height": s.genesisVestingHeight,
		"recipient": store.GenesisVestingRecipient,
		"total_napro": strconv.FormatUint(store.GenesisVestingReserve, 10),
		"funding_basis": store.GenesisVestingBasis,
		"historical_issuance_proven": false,
		"remaining_napro": nil, "paid_napro": nil,
	}
	if s.genesisVestingHeight > 0 { out["state"] = "pending_activation" }
	var v *store.GenesisVestingState
	var settledHeight uint64
	if s.blockStore != nil {
		checkpoint, err := s.blockStore.LoadLPoDCheckpoint()
		if err != nil { writeJSONError(w, 503, "canonical reserve unavailable"); return }
		if checkpoint != nil && checkpoint.GenesisVesting != nil {
			v = checkpoint.GenesisVesting
			settledHeight = checkpoint.State.LastHeight
			out["state"] = "active"
			out["activation_height"] = v.FundingHeight
			out["remaining_napro"] = strconv.FormatUint(v.Remaining, 10)
			out["paid_napro"] = strconv.FormatUint(v.Paid, 10)
			out["reserve"] = v
if v.LiquidityPaid > 0 {
hash, ok, err := s.blockStore.GetCanonicalHash(v.FundingHeight)
if err != nil || !ok { writeJSONError(w, 503, "liquidity funding unavailable"); return }
raw, err := s.blockStore.GetRawBlockByHeight(v.FundingHeight)
var funding core.Block
if err != nil || json.Unmarshal(raw, &funding) != nil || funding.Hash() != hash || len(funding.Txs) < 2 {
writeJSONError(w, 503, "liquidity funding body unavailable"); return
}
liquidityTxHash := funding.Txs[0].Hash()
out["liquidity"] = map[string]interface{}{
"address": v.LiquidityRecipient, "funded_napro": strconv.FormatUint(v.LiquidityPaid, 10),
"funding_height": v.FundingHeight, "block_hash": hex.EncodeToString(hash[:]),
"tx_hash": hex.EncodeToString(liquidityTxHash[:]),
"finalized": s.lpodFinalized != nil && s.lpodFinalized(v.FundingHeight, hash),
"custody": "operator_treasury_wallet",
}
}
		}
	}
	rows := make([]map[string]interface{}, 0, 10)
	for i := 0; i < 10; i++ {
		deadline, amount, _ := store.GenesisVestingTranche(i)
		row := map[string]interface{}{
			"year": 2027+i, "unlock_date_utc": time.Unix(0, deadline).UTC().Format("2006-01-02"),
			"amount_napro": strconv.FormatUint(amount, 10), "address": store.GenesisVestingRecipient,
			"paid_height": nil, "block_hash": nil, "tx_hash": nil, "finalized": false,
		}
		if v != nil && v.PaidHeights[i] > 0 {
			h := v.PaidHeights[i]
			raw, err := s.blockStore.GetRawBlockByHeight(h)
			var b core.Block
			if err != nil || json.Unmarshal(raw, &b) != nil || b.Header.Height != h ||
				len(b.Txs) < 2 || !b.Txs[0].IsLPoDPayout() {
				writeJSONError(w, 503, "canonical vesting payment body unavailable"); return
			}
			hash, ok, err := s.blockStore.GetCanonicalHash(h)
			if err != nil || !ok || hash != b.Hash() {
				writeJSONError(w, 503, "vesting payment is not canonical"); return
			}
			txHash := b.Txs[0].Hash()
			row["paid_height"], row["block_hash"], row["tx_hash"] = h, hex.EncodeToString(hash[:]), hex.EncodeToString(txHash[:])
			if s.lpodFinalized != nil {
				row["finalized"] = s.lpodFinalized(h, hash)
				// A finalized descendant's signed ledger also commits these
				// payments; old height-specific in-memory caches may expire.
				if settledHeight > h {
					tip, tipHeight, err := s.blockStore.GetTip()
					if err == nil && tipHeight == settledHeight && s.lpodFinalized(tipHeight, tip) {
						row["finalized"] = true
					}
				}
			}
		}
		rows = append(rows, row)
	}
	out["schedule"] = rows
	writeJSON(w, http.StatusOK, out)
}