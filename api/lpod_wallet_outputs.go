// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func (s *Server) restLPoDWalletOutputs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("snapshot") == "1" || r.URL.Query().Has("snapshot_id") {
		s.restLPoDWalletOutputsSnapshot(w, r)
		return
	}
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

func (s *Server) restLPoDEarningsOutputs(w http.ResponseWriter, r *http.Request) {
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
	if s.lpodMigration == nil || s.lpodMigration.Version != 3 {
		writeJSONError(w, 503, "v3 LPoD earnings are not active")
		return
	}
	if s.blockStore == nil {
		writeJSONError(w, 503, "canonical store unavailable")
		return
	}
	query := r.URL.Query()
	hash := crypto.Hash32{}
	var height uint64
	var rows []store.StoredUTXO
	var next string
	var err error
	var pinnedSnapshotID string

	if query.Has("snapshot_id") {
		pinnedSnapshotID = query.Get("snapshot_id")
		lease := s.acquireWalletSnapshot(w, pinnedSnapshotID, address)
		if lease == nil {
			return
		}
		defer lease.Release()

		entry := lease.entry
		checkpoint := entry.checkpoint
		if entry.read == nil || checkpoint == nil || checkpoint.Allocation == nil ||
			checkpoint.Allocation.Version != 3 ||
			checkpoint.Allocation.Genesis != s.lpodMigration.Genesis ||
			checkpoint.Allocation.ReconciliationRoot != s.lpodMigration.ReconciliationRoot ||
			checkpoint.Allocation.FundingHeight != s.lpodMigration.Height ||
			checkpoint.State.LastHeight != entry.height {
			writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", "pinned v3 LPoD checkpoint is unavailable")
			return
		}
		ready, readyErr := entry.read.LPoDEarningsIndexReady(
			checkpoint.Allocation.FundingBlock, entry.hash, entry.height,
		)
		if readyErr != nil || !ready {
			writeWalletSnapshotError(w, http.StatusServiceUnavailable, "EARNINGS_INDEX_NOT_READY", "LPoD earnings index is incomplete at the pinned wallet snapshot")
			return
		}
		hash, height = entry.hash, entry.height
		cursor, ok := unwrapLPoDEarningsCursor(w, query.Get("cursor"), hash)
		if !ok {
			return
		}
		rows, next, err = entry.read.LPoDEarningsOutputs(address, cursor, 128)
	} else {
		hash, height, err = s.blockStore.GetTip()
		if err != nil || s.lpodFinalized == nil || !s.lpodFinalized(height, hash) {
			writeJSONError(w, 503, "finalized tip required")
			return
		}
		checkpoint, checkpointErr := s.blockStore.LoadLPoDCheckpointAt(hash)
		if checkpointErr != nil || checkpoint == nil || checkpoint.Allocation == nil || checkpoint.Allocation.Version != 3 ||
			checkpoint.Allocation.Genesis != s.lpodMigration.Genesis ||
			checkpoint.Allocation.ReconciliationRoot != s.lpodMigration.ReconciliationRoot ||
			checkpoint.Allocation.FundingHeight != s.lpodMigration.Height || checkpoint.State.LastHeight != height {
			writeJSONError(w, 503, "canonical v3 LPoD checkpoint required")
			return
		}
		ready, readyErr := s.blockStore.LPoDEarningsIndexReady(checkpoint.Allocation.FundingBlock, hash, height)
		if readyErr != nil || !ready {
			writeJSONError(w, 503, "v2 earnings index is incomplete through the finalized tip")
			return
		}
		cursor, ok := unwrapLPoDEarningsCursor(w, query.Get("cursor"), hash)
		if !ok {
			return
		}
		rows, next, err = s.blockStore.LPoDEarningsOutputs(address, cursor, 128)
		if err == nil {
			after, afterHeight, tipErr := s.blockStore.GetTip()
			if tipErr != nil || after != hash || afterHeight != height ||
				s.lpodFinalized == nil || !s.lpodFinalized(afterHeight, after) {
				writeJSONError(w, 409, "canonical finalized checkpoint changed")
				return
			}
		}
	}
	if err != nil {
		writeJSONError(w, 400, err.Error())
		return
	}
	outputs := []map[string]interface{}{}
	for _, u := range rows {
		outputs = append(outputs, map[string]interface{}{
			"tx_hash":       fmt.Sprintf("%x", u.TxHash[:]),
			"out_idx":       u.OutputIndex,
			"block_height":  u.BlockHeight,
			"one_time_pub":  fmt.Sprintf("%x", u.OneTimePub[:]),
			"tx_pub_key":    fmt.Sprintf("%x", u.TxPubKey[:]),
			"amount_commit": fmt.Sprintf("%x", u.AmountCommit[:]),
			"enc_amount":    fmt.Sprintf("%x", u.EncAmount[:]),
			"amount_napro":  fmt.Sprintf("%d", u.AmountNAPRO),
		})
	}
	nextCursor := ""
	if next != "" {
		token := fmt.Sprintf("%x:%s", hash[:], next)
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(token))
	}
	response := map[string]interface{}{
		"state": "active", "checkpoint_hash": fmt.Sprintf("%x", hash[:]),
		"outputs": outputs, "next_cursor": nextCursor,
	}
	if pinnedSnapshotID != "" {
		response["snapshot_id"] = pinnedSnapshotID
		response["checkpoint_height"] = height
	}
	writeJSON(w, 200, response)
}

func unwrapLPoDEarningsCursor(
	w http.ResponseWriter,
	cursor string,
	checkpointHash crypto.Hash32,
) (string, bool) {
	if cursor == "" {
		return "", true
	}
	if len(cursor) > 512 {
		writeJSONError(w, 400, "invalid cursor")
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		writeJSONError(w, 400, "invalid cursor")
		return "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		writeJSONError(w, 400, "invalid cursor")
		return "", false
	}
	bound, err := hex.DecodeString(parts[0])
	if err != nil || len(bound) != 32 {
		writeJSONError(w, 400, "invalid cursor")
		return "", false
	}
	if !strings.EqualFold(parts[0], fmt.Sprintf("%x", checkpointHash[:])) {
		writeJSONError(w, 409, "cursor is bound to a different finalized checkpoint")
		return "", false
	}
	return parts[1], true
}

// Public key images, not keys/blinds, identify locally scanned consumed outputs.
func (s *Server) restWalletKeyImages(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeJSONError(w, 405, "POST only")
		return
	}
	var req struct {
		Images     []string `json:"key_images"`
IncludeHistory bool `json:"include_history,omitempty"`
		SnapshotID string   `json:"snapshot_id,omitempty"`
		Address    string   `json:"address,omitempty"`
		Refs       []struct {
			Hash  string `json:"tx_hash"`
			Index uint32 `json:"out_idx"`
		} `json:"refs"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 80000)).Decode(&req) != nil || len(req.Images) > 256 || len(req.Images) != len(req.Refs) {
		writeJSONError(w, 400, "bounded key_images and matching refs required")
		return
	}
	if req.SnapshotID != "" {
s.restWalletKeyImagesSnapshot(w, req.Images, req.Refs, req.SnapshotID, req.Address, req.IncludeHistory)
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
