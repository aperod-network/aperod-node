// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package api

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/aperod/aperod/crypto"
	"net/http"
)

func (s *Server) restLPoDWalletOutputs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, 405, "GET only")
		return
	}
	address := crypto.Address(r.URL.Query().Get("address"))
	if _, _, _, err := crypto.DecodeAddress(address); err != nil {
		writeJSONError(w, 400, "invalid address")
		return
	}
	if s.lpodMigration == nil {
		writeJSON(w, 200, map[string]interface{}{"state": "disabled", "outputs": []interface{}{}, "next_cursor": ""})
		return
	}
	if s.blockStore == nil {
		writeJSONError(w, 503, "canonical store unavailable")
		return
	}
	hash, height, err := s.blockStore.GetTip()
	if err != nil || s.lpodFinalized == nil || !s.lpodFinalized(height, hash) {
		writeJSONError(w, 503, "finalized tip required")
		return
	}
	c, err := s.blockStore.LoadLPoDCheckpointAt(hash)
	if err != nil || c == nil || c.Allocation == nil || c.Allocation.Genesis != s.lpodMigration.Genesis || c.Allocation.ReconciliationRoot != s.lpodMigration.ReconciliationRoot || c.Allocation.FundingHeight != s.lpodMigration.Height || c.State.LastHeight != height {
		writeJSONError(w, 503, "canonical LPoD checkpoint required")
		return
	}
	ready, err := s.blockStore.LPoDWalletIndexReady(c.Allocation.FundingBlock)
	if err != nil || !ready {
		writeJSONError(w, 503, "native wallet index requires canonical replay from activation")
		return
	}
	rows, next, err := s.blockStore.LPoDWalletOutputs(address, r.URL.Query().Get("cursor"), 128)
	if err != nil {
		writeJSONError(w, 400, err.Error())
		return
	}
	outputs := []map[string]interface{}{}
	for _, u := range rows {
		outputs = append(outputs, map[string]interface{}{"tx_hash": fmt.Sprintf("%x", u.TxHash[:]), "out_idx": u.OutputIndex, "block_height": u.BlockHeight,
			"one_time_pub": fmt.Sprintf("%x", u.OneTimePub[:]), "tx_pub_key": fmt.Sprintf("%x", u.TxPubKey[:]), "amount_commit": fmt.Sprintf("%x", u.AmountCommit[:]), "enc_amount": fmt.Sprintf("%x", u.EncAmount[:])})
	}
	after, h, err := s.blockStore.GetTip()
	if err != nil || after != hash || h != height {
		writeJSONError(w, 409, "canonical tip changed")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"state": "active", "checkpoint_hash": fmt.Sprintf("%x", hash[:]), "outputs": outputs, "next_cursor": next})
}

// Public key images, not keys/blinds, identify locally scanned consumed outputs.
func (s *Server) restWalletKeyImages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, 405, "POST only")
		return
	}
	var req struct {
		Images []string `json:"key_images"`
		Refs   []struct {
			Hash  string `json:"tx_hash"`
			Index uint32 `json:"out_idx"`
		} `json:"refs"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 80000)).Decode(&req) != nil || len(req.Images) > 256 || len(req.Images) != len(req.Refs) {
		writeJSONError(w, 400, "bounded key_images and matching refs required")
		return
	}
	if s.blockStore == nil {
		writeJSONError(w, 503, "canonical store unavailable")
		return
	}
	hash, height, err := s.blockStore.GetTip()
	if err != nil {
		writeJSONError(w, 503, "tip unavailable")
		return
	}
	pending := map[string]bool{}
	if s.mempool != nil {
		for _, h := range s.mempool.Hashes() {
			tx, ok := s.mempool.Get(h)
			if !ok {
				continue
			}
			for _, in := range tx.Inputs {
				ki, err := crypto.CanonicalKeyImage(in.KeyImage)
				if err != nil {
					continue
				}
				pending[hex.EncodeToString(ki[:])] = true
			}
		}
	}
	statuses := map[string]bool{}
	for i, text := range req.Images {
		raw, err := hex.DecodeString(text)
		if err != nil || len(raw) != 32 {
			writeJSONError(w, 400, "invalid key image")
			return
		}
		var ki crypto.KeyImage
		copy(ki[:], raw)
		ki, err = crypto.CanonicalKeyImage(ki)
		if err != nil {
			writeJSONError(w, 400, "invalid key image")
			return
		}
		spent, err := s.blockStore.IsKeyImageSpent(ki)
		if err != nil {
			writeJSONError(w, 503, "key image index unavailable")
			return
		}
		ref := req.Refs[i]
		raw, err = hex.DecodeString(ref.Hash)
		if err != nil || len(raw) != 32 {
			writeJSONError(w, 400, "invalid source hash")
			return
		}
		var th crypto.Hash32
		copy(th[:], raw)
		u, err := s.blockStore.GetUTXO(th, ref.Index)
		if err != nil {
			writeJSONError(w, 503, "output index unavailable")
			return
		}
		directSpent, err := s.blockStore.IsUTXOSpentChecked(th, ref.Index)
		if err != nil {
			writeJSONError(w, 503, "source index unavailable")
			return
		}
		if u == nil && !spent && !directSpent {
			writeJSONError(w, 409, "source is no longer indexed; refresh canonical signing material")
			return
		}
		statuses[text] = spent || u == nil || directSpent || pending[hex.EncodeToString(ki[:])]
	}
	after, h, err := s.blockStore.GetTip()
	if err != nil || after != hash || h != height {
		writeJSONError(w, 409, "tip changed")
		return
	}
	writeJSON(w, 200, map[string]interface{}{"spent": statuses, "checkpoint_hash": fmt.Sprintf("%x", hash[:])})
}
