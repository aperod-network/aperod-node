// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

const (
	walletChangesMaxEvents       = 128
	walletChangesMaxHeights      = 128
	walletChangesMaxBlockBytes   = 2 << 20
	walletChangesMaxDecodeBytes  = 8 << 20
	walletChangesMaxResponseSize = 512 << 10
	walletChangesMaxPendingKeys  = 4096
	walletChangesMaxResumeEvents = 16384
)

type walletChangesCursor struct {
	SnapshotID string `json:"s"`
	Address    string `json:"a"`
	FromHeight uint64 `json:"f"`
	FromHash   string `json:"x"`
	Target     uint64 `json:"t"`
	TargetHash string `json:"h"`
	Height     uint64 `json:"n"`
	EventIndex int    `json:"i"`
}

func signWalletChangesCursor(key []byte, c walletChangesCursor) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(append(payload, mac.Sum(nil)...))
}

func parseWalletChangesCursor(key []byte, id, text string) (walletChangesCursor, error) {
	var c walletChangesCursor
	raw, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || len(raw) <= sha256.Size {
		return c, fmt.Errorf("invalid cursor")
	}
	payload, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) || json.Unmarshal(payload, &c) != nil {
		return c, fmt.Errorf("invalid cursor signature")
	}
	if c.SnapshotID != id || c.Height == 0 || c.EventIndex < 0 {
		return c, fmt.Errorf("cursor belongs to a different snapshot")
	}
	return c, nil
}

func parseHash32(text string) (crypto.Hash32, error) {
	var hash crypto.Hash32
	raw, err := hex.DecodeString(text)
	if err != nil || len(raw) != len(hash) || len(text) != 64 {
		return hash, fmt.Errorf("expected 64 hexadecimal characters")
	}
	copy(hash[:], raw)
	return hash, nil
}

func walletBlockEvents(
	block *core.Block,
	publicAmount func(crypto.Hash32, uint32) (uint64, bool, error),
) ([]map[string]interface{}, error) {
	events := make([]map[string]interface{}, 0)
	var zeroPoint crypto.Point32
	for _, tx := range block.Txs {
		txHash := tx.Hash()
		txHashText := fmt.Sprintf("%x", txHash[:])
		for i, output := range tx.Outputs {
			outputData := map[string]interface{}{
				"tx_hash": txHashText, "out_idx": uint32(i), "block_height": block.Header.Height,
				"one_time_pub":  fmt.Sprintf("%x", output.OneTimePub[:]),
				"tx_pub_key":    fmt.Sprintf("%x", output.TxPubKey[:]),
				"amount_commit": fmt.Sprintf("%x", output.AmountCommit[:]),
				"enc_amount":    fmt.Sprintf("%x", output.EncAmount[:]),
			}
			if output.TxPubKey == zeroPoint {
				var amount uint64
				var known bool
				var err error
				if publicAmount != nil {
					amount, known, err = publicAmount(txHash, uint32(i))
				}
				if err != nil {
					return nil, fmt.Errorf("read public amount metadata at block %d: %w", block.Header.Height, err)
				}
				if known && amount > 0 {
					outputData["amount_napr"] = fmt.Sprintf("%d", amount)
				} else {
					outputData["opening_status"] = "OUTPUT_OPENING_UNAVAILABLE"
				}
			}
			events = append(events, map[string]interface{}{
				"kind":   "output",
				"output": outputData,
			})
		}
		for _, input := range tx.Inputs {
			keyImage, err := crypto.CanonicalKeyImage(input.KeyImage)
			if err != nil {
				return nil, fmt.Errorf("invalid key image in canonical block %d", block.Header.Height)
			}
			events = append(events, map[string]interface{}{
				"kind": "spent_key_image", "key_image_hex": fmt.Sprintf("%x", keyImage[:]),
				"block_height": block.Header.Height,
			})
		}
		if tx.IsLPoDPosition() {
			action, err := tx.LPoDPositionAction()
			if err != nil {
				return nil, fmt.Errorf("invalid native position spend at height %d", block.Header.Height)
			}
			if action.Action == core.LPoDDeposit {
				events = append(events, map[string]interface{}{
					"kind": "spent_ref", "tx_hash": fmt.Sprintf("%x", action.SourceTx[:]),
					"out_idx": action.SourceIndex, "block_height": block.Header.Height,
				})
			}
		}
		if tx.IsStake() && len(tx.Extra) > 0 && core.StakeAction(tx.Extra[0]) == core.StakeDeposit {
			var source crypto.Hash32
			var index uint32
			switch len(tx.Extra) {
			case core.StakePayloadSizeV2:
				action, _, _, _, hash, outIndex, _, err := core.DecodeStakeExtraV2(tx.Extra)
				if err != nil || action != core.StakeDeposit {
					return nil, fmt.Errorf("invalid direct stake spend at height %d", block.Header.Height)
				}
				source, index = hash, outIndex
			case core.StakePayloadSizeV3:
				action, _, _, _, hash, outIndex, _, _, err := core.DecodeStakeExtraV3(tx.Extra)
				if err != nil || action != core.StakeDeposit {
					return nil, fmt.Errorf("invalid direct stake spend at height %d", block.Header.Height)
				}
				source, index = hash, outIndex
			default:
				// V1 deposits do not carry an explicit source reference.
				continue
			}
			events = append(events, map[string]interface{}{
				"kind": "spent_ref", "tx_hash": fmt.Sprintf("%x", source[:]),
				"out_idx": index, "block_height": block.Header.Height,
			})
		}
	}
	return events, nil
}

func walletSnapshotBlockEvents(entry *walletSnapshotEntry, block *core.Block) ([]map[string]interface{}, error) {
	return walletBlockEvents(block, func(txHash crypto.Hash32, index uint32) (uint64, bool, error) {
		utxo, err := entry.read.GetUTXO(txHash, index)
		if err != nil || utxo == nil {
			return 0, false, err
		}
		return utxo.AmountNAPRO, utxo.AmountNAPRO > 0, nil
	})
}

func (s *Server) pendingKeyImagesFor(entry *walletSnapshotEntry) ([]string, error) {
	entry.pendingOnce.Do(func() {
		images := make(map[string]struct{})
		if s.mempool != nil {
			for _, hash := range s.mempool.Hashes() {
				tx, ok := s.mempool.Get(hash)
				if !ok {
					continue
				}
				for _, input := range tx.Inputs {
					image, err := crypto.CanonicalKeyImage(input.KeyImage)
					if err != nil {
						continue
					}
					images[hex.EncodeToString(image[:])] = struct{}{}
					if len(images) > walletChangesMaxPendingKeys {
						entry.pendingErr = fmt.Errorf("pending key-image set exceeds %d entries", walletChangesMaxPendingKeys)
						return
					}
				}
			}
		}
		entry.pendingKeyImages = make([]string, 0, len(images))
		for image := range images {
			entry.pendingKeyImages = append(entry.pendingKeyImages, image)
		}
		sort.Strings(entry.pendingKeyImages)
	})
	return entry.pendingKeyImages, entry.pendingErr
}

func reconciliationError(w http.ResponseWriter, from, to uint64, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusConflict, map[string]interface{}{
		"code": "RECONCILIATION_REQUIRED", "error": message,
		"missing_range": map[string]uint64{"from_height": from, "to_height": to},
	})
}

func reorgError(w http.ResponseWriter, from uint64, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusConflict, map[string]interface{}{
		"code": "REORG", "error": message, "from_height": from,
	})
}

func (s *Server) restWalletChanges(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	q := r.URL.Query()
	address, err := validSnapshotAddress(q.Get("address"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid address")
		return
	}
	fromText, fromHashText := q.Get("from_height"), q.Get("from_hash")
	fromHeight, err := strconv.ParseUint(fromText, 10, 64)
	if err != nil || fromText == "" {
		writeJSONError(w, http.StatusBadRequest, "from_height must be an unsigned integer")
		return
	}
	fromHash, err := parseHash32(fromHashText)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "from_hash must be 64 hexadecimal characters")
		return
	}
	limit := walletChangesMaxEvents
	if text := q.Get("limit"); text != "" {
		limit, err = strconv.Atoi(text)
		if err != nil || limit < 1 || limit > walletChangesMaxEvents {
			writeJSONError(w, http.StatusBadRequest, "limit must be between 1 and 128")
			return
		}
	}
	if q.Has("witness_height") != q.Has("witness_hash") {
		writeJSONError(w, http.StatusBadRequest, "witness_height and witness_hash must be supplied together")
		return
	}
	witnessProvided := q.Has("witness_height")
	var requestedWitnessHeight uint64
	var requestedWitnessHash crypto.Hash32
	if witnessProvided {
		requestedWitnessHeight, err = strconv.ParseUint(q.Get("witness_height"), 10, 64)
		if err != nil || requestedWitnessHeight < fromHeight {
			writeJSONError(w, http.StatusBadRequest, "invalid witness_height")
			return
		}
		requestedWitnessHash, err = parseHash32(q.Get("witness_hash"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "witness_hash must be 64 hexadecimal characters")
			return
		}
	}
	id := q.Get("snapshot_id")
	resumeProvided := q.Has("resume_height") || q.Has("resume_hash") || q.Has("resume_event_index")
	if resumeProvided &&
		(!q.Has("resume_height") || !q.Has("resume_hash") || !q.Has("resume_event_index")) {
		writeJSONError(w, http.StatusBadRequest, "resume_height, resume_hash, and resume_event_index must be supplied together")
		return
	}
	var resumeHeight uint64
	var resumeHash crypto.Hash32
	var resumeEventIndex uint64
	if resumeProvided {
		if id != "" || !witnessProvided || q.Get("cursor") != "" {
			writeJSONError(w, http.StatusBadRequest, "resume coordinates require a new snapshot and a witness")
			return
		}
		resumeHeight, err = strconv.ParseUint(q.Get("resume_height"), 10, 64)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid resume_height")
			return
		}
		resumeHash, err = parseHash32(q.Get("resume_hash"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "resume_hash must be 64 hexadecimal characters")
			return
		}
		resumeEventIndex, err = strconv.ParseUint(q.Get("resume_event_index"), 10, 64)
		if err != nil || resumeEventIndex > walletChangesMaxResumeEvents {
			writeJSONError(w, http.StatusBadRequest, "resume_event_index exceeds the supported event index")
			return
		}
	}

	var lease *walletSnapshotLease
	created := id == ""
	if s.walletSnapshots == nil {
		writeWalletSnapshotError(w, http.StatusServiceUnavailable, "SNAPSHOT_UNAVAILABLE", "wallet snapshot manager unavailable")
		return
	}
	if created {
		if q.Get("cursor") != "" {
			writeJSONError(w, http.StatusBadRequest, "cursor requires snapshot_id")
			return
		}
		lease, err = s.walletSnapshots.capture(r.Context(), s, address)
	} else {
		lease, err = s.walletSnapshots.acquire(id, address)
	}
	if err != nil {
		writeSnapshotLeaseError(w, err)
		return
	}
	if created {
		id = lease.entry.id
	}
	delivered := !created
	defer func() {
		if created && !delivered {
			_ = s.walletSnapshots.close(lease.entry.id, address)
		}
		lease.Release()
	}()

	entry := lease.entry
	if witnessProvided {
		if requestedWitnessHeight > entry.height {
			reorgError(w, requestedWitnessHeight, "witness target is above the newly captured checkpoint")
			return
		}
		canonical, found, readErr := entry.read.GetCanonicalHash(requestedWitnessHeight)
		if readErr != nil || !found {
			reconciliationError(w, requestedWitnessHeight, requestedWitnessHeight, "witness canonical header is unavailable")
			return
		}
		if canonical != requestedWitnessHash {
			reorgError(w, requestedWitnessHeight, "witness_hash is not canonical in the newly captured snapshot")
			return
		}
		if created {
			_, _, err = s.walletSnapshots.bindWitness(entry, requestedWitnessHeight, requestedWitnessHash)
		} else {
			boundHeight, boundHash, bound := s.walletSnapshots.witnessFor(entry)
			if !bound || boundHeight != requestedWitnessHeight || boundHash != requestedWitnessHash {
				err = fmt.Errorf("witness does not match the existing snapshot")
			}
		}
		if err != nil {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", err.Error())
			return
		}
	}
	pendingKeyImages, err := s.pendingKeyImagesFor(entry)
	if err != nil {
		writeWalletSnapshotError(w, http.StatusServiceUnavailable, "PENDING_SET_LIMIT", err.Error())
		return
	}
	fromHashText = fmt.Sprintf("%x", fromHash[:])
	targetHashText := fmt.Sprintf("%x", entry.hash[:])
	cursor := walletChangesCursor{
		SnapshotID: id, Address: string(address), FromHeight: fromHeight, FromHash: fromHashText,
		Target: entry.height, TargetHash: targetHashText, Height: fromHeight + 1,
	}
	if cursor.Height == 0 { // from_height overflow
		writeJSONError(w, http.StatusBadRequest, "from_height is out of range")
		return
	}
	if resumeProvided {
		if !witnessProvided || resumeHeight < cursor.Height ||
			(resumeHeight > requestedWitnessHeight && resumeHeight-requestedWitnessHeight > 1) {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "resume position is outside the witnessed range")
			return
		}
		if resumeHeight > requestedWitnessHeight {
			if resumeHash != requestedWitnessHash || resumeEventIndex != 0 {
				writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "resume position past the witness must use its hash and a zero event index")
				return
			}
		} else {
			canonical, ok, readErr := entry.read.GetCanonicalHash(resumeHeight)
			if readErr != nil || !ok {
				reconciliationError(w, resumeHeight, resumeHeight, "resume canonical header is unavailable")
				return
			}
			if canonical != resumeHash {
				reorgError(w, resumeHeight, "resume_hash is not canonical in the captured snapshot")
				return
			}
			block, readErr := entry.read.ReadCanonicalBlockBounded(resumeHeight, walletChangesMaxBlockBytes)
			if readErr != nil {
				reconciliationError(w, resumeHeight, resumeHeight, "resume canonical block body is unavailable or exceeds bounds")
				return
			}
			resumeEvents, eventErr := walletSnapshotBlockEvents(entry, block)
			if eventErr != nil {
				reconciliationError(w, resumeHeight, resumeHeight, "resume block contains malformed public spend metadata")
				return
			}
			if len(resumeEvents) > walletChangesMaxResumeEvents {
				reconciliationError(w, resumeHeight, resumeHeight, "resume block exceeds the supported event index")
				return
			}
			encodedEvents, marshalErr := json.Marshal(resumeEvents)
			if marshalErr != nil || len(encodedEvents) > walletChangesMaxDecodeBytes {
				reconciliationError(w, resumeHeight, resumeHeight, "resume block event list exceeds decode bounds")
				return
			}
			if resumeEventIndex > uint64(len(resumeEvents)) {
				writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "resume event index is outside the canonical block")
				return
			}
		}
		cursor.Height = resumeHeight
		cursor.EventIndex = int(resumeEventIndex)
	}
	if token := q.Get("cursor"); token != "" {
		cursor, err = parseWalletChangesCursor(entry.cursorKey[:], id, token)
		if err != nil {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", err.Error())
			return
		}
		if cursor.Address != string(address) || cursor.FromHeight != fromHeight ||
			cursor.FromHash != fromHashText || cursor.Target != entry.height || cursor.TargetHash != targetHashText {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "cursor parameters do not match snapshot")
			return
		}
	}
	if fromHeight > entry.height {
		writeJSONError(w, http.StatusBadRequest, "from_height is above the snapshot checkpoint")
		return
	}
	canonicalStart, found, err := entry.read.GetCanonicalHash(fromHeight)
	if err != nil || !found {
		reconciliationError(w, fromHeight, fromHeight, "starting canonical header is unavailable")
		return
	}
	if canonicalStart != fromHash {
		reorgError(w, fromHeight, "from_hash is not canonical in the captured snapshot")
		return
	}
	if cursor.Height > entry.height && cursor.Height-entry.height > 1 {
		writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "cursor is beyond snapshot checkpoint")
		return
	}

	events := make([]map[string]interface{}, 0, limit)
	next := cursor
	expectedParent := fromHash
	if cursor.Height > fromHeight+1 {
		parent, ok, err := entry.read.GetCanonicalHash(cursor.Height - 1)
		if err != nil || !ok {
			reconciliationError(w, cursor.Height-1, cursor.Height-1, "canonical parent header is unavailable")
			return
		}
		expectedParent = parent
	}
	heightsExamined, decodedBytes := 0, 0
	more := false
	for height := cursor.Height; height <= entry.height && heightsExamined < walletChangesMaxHeights; height++ {
		if decodedBytes >= walletChangesMaxDecodeBytes {
			next.Height, next.EventIndex = height, 0
			more = true
			break
		}
		block, bodyBytes, err := entry.read.ReadCanonicalBlockBoundedWithSize(height, walletChangesMaxBlockBytes)
		if err != nil {
			reconciliationError(w, height, height, "canonical block body is unavailable or exceeds bounds")
			return
		}
		if decodedBytes+bodyBytes > walletChangesMaxDecodeBytes {
			next.Height, next.EventIndex, more = height, 0, true
			break
		}
		canonicalHash, ok, err := entry.read.GetCanonicalHash(height)
		if err != nil || !ok {
			reconciliationError(w, height, height, "canonical header is unavailable")
			return
		}
		if block.Header.PrevHash != expectedParent {
			reorgError(w, height, "captured canonical ancestry is discontinuous")
			return
		}
		if block.Hash() != canonicalHash {
			reorgError(w, height, "captured canonical block hash changed")
			return
		}
		expectedParent = canonicalHash
		heightsExamined++
		decodedBytes += bodyBytes
		blockEvents, err := walletSnapshotBlockEvents(entry, block)
		if err != nil {
			reconciliationError(w, height, height, "canonical block contains malformed public spend metadata")
			return
		}
		if len(blockEvents) > walletChangesMaxResumeEvents {
			reconciliationError(w, height, height, "canonical block exceeds the supported event index")
			return
		}
		start := 0
		if height == cursor.Height {
			start = cursor.EventIndex
		}
		if start > len(blockEvents) {
			writeWalletSnapshotError(w, http.StatusConflict, "SNAPSHOT_CURSOR_MISMATCH", "cursor event position is invalid")
			return
		}
		for index := start; index < len(blockEvents); index++ {
			candidate := append(events, blockEvents[index])
			response := walletChangesResponse(id, entry, fromHeight, fromHashText, candidate, "", false)
			encoded, _ := json.Marshal(response)
			if len(candidate) > limit || len(encoded) > walletChangesMaxResponseSize {
				if len(events) == 0 {
					writeWalletSnapshotError(w, http.StatusServiceUnavailable, "EVENT_TOO_LARGE", "a single wallet event exceeds response bounds")
					return
				}
				next.Height, next.EventIndex, more = height, index, true
				break
			}
			events = candidate
			next.Height, next.EventIndex = height, index+1
		}
		if more {
			break
		}
		next.Height, next.EventIndex = height+1, 0
		if len(events) >= limit {
			more = height < entry.height
			break
		}
	}
	if !more && next.Height <= entry.height {
		more = true
	}
	complete := !more && next.Height > entry.height
	nextCursor := ""
	if more {
		nextCursor = signWalletChangesCursor(entry.cursorKey[:], next)
	}
	response := walletChangesResponse(id, entry, fromHeight, fromHashText, events, nextCursor, complete)
	response["pending_complete"] = complete
	if complete {
		response["pending_key_images"] = pendingKeyImages
	} else {
		if next.Height > entry.height {
			reconciliationError(w, entry.height, entry.height, "incomplete page has no canonical resume height")
			return
		}
		resumeHash, found, readErr := entry.read.GetCanonicalHash(next.Height)
		if readErr != nil || !found {
			reconciliationError(w, next.Height, next.Height, "resume canonical header is unavailable")
			return
		}
		response["resume_height"] = next.Height
		response["resume_hash"] = fmt.Sprintf("%x", resumeHash[:])
		response["resume_event_index"] = next.EventIndex
	}
	if created {
		delivered = writeCreatedWalletSnapshot(w, r, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func walletChangesResponse(id string, entry *walletSnapshotEntry, fromHeight uint64, fromHash string, events []map[string]interface{}, cursor string, complete bool) map[string]interface{} {
	response := map[string]interface{}{
		"snapshot_id": id, "checkpoint_hash": fmt.Sprintf("%x", entry.hash[:]),
		"checkpoint_height": entry.height, "from_height": fromHeight, "from_hash": fromHash,
		"changes": events, "page_complete": true, "complete": complete,
	}
	if cursor != "" {
		response["cursor"] = cursor
	}
	if genesis, found, err := entry.read.GetCanonicalHash(0); err == nil && found {
		response["genesis_hash"] = fmt.Sprintf("%x", genesis[:])
	}
	return response
}
