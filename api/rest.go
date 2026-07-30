package api

// REST API handlers for Phase 2 (blocks 2.1.10-2.1.13).
// Routes:
//   GET  /api/v1/blocks                                — paginated block list
//   GET  /api/v1/blocks/{id}                           — block by height or hash
//   GET  /api/v1/transactions/{hash}                   — tx by hash
//   GET  /api/v1/address/{addr}/transactions           — incoming tx for address
//   GET  /api/v1/network/stats                         — network statistics
//   GET  /api/v1/validators                            — all registry validators + stake state
//   GET  /api/v1/validators/{pubkey}/unbonding         — partial-unbonding queue for one validator
//   POST /api/v1/admin/mint                            — admin-only: mint APRO to address
//   POST /api/v1/admin/partial-unstake                 — admin-only: apply partial stake withdrawal

import (
        "encoding/hex"
        "encoding/json"
        "fmt"
        "math"
        "net/http"
        "strconv"
        "strings"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
)

// registerRESTRoutes adds all REST routes to the server mux.
func (s *Server) registerRESTRoutes() {
        s.mux.HandleFunc("/api/v1/blocks", s.restBlocks)
        s.mux.HandleFunc("/api/v1/blocks/", s.restBlockByIDOrTxs)
        s.mux.HandleFunc("/api/v1/transactions/", s.restTransaction)
        s.mux.HandleFunc("/api/v1/address/", s.restAddressTxs)
        s.mux.HandleFunc("/api/v1/network/stats", s.restNetworkStats)
        s.mux.HandleFunc("/api/v1/validators", s.restValidators)
        s.mux.HandleFunc("/api/v1/validators/", s.restValidatorUnbonding)
        s.mux.HandleFunc("/api/v1/admin/mint", s.restAdminMint)
        s.mux.HandleFunc("/api/v1/admin/partial-unstake", s.restAdminPartialUnstake)
        s.mux.HandleFunc("/api/v1/my-validator", s.restMyValidator)
        s.mux.HandleFunc("/api/v1/network/identity", s.restNetworkIdentity)
        s.mux.HandleFunc("/api/v1/network/bans", s.restNetworkBans)
        s.mux.HandleFunc("/api/v1/network/bans/", s.restNetworkBanByAddr)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
        w.Header().Set("Content-Type", "application/json")
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.WriteHeader(code)
        json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
        writeJSON(w, code, map[string]string{"error": msg})
}

// pathSuffix extracts the path segment after prefix.
// e.g. pathSuffix("/api/v1/blocks/", "/api/v1/blocks/42") == "42"
func pathSuffix(prefix, path string) string {
        return strings.TrimPrefix(path, prefix)
}

// ─── GET /api/v1/blocks (2.1.10) ─────────────────────────────────────────────

func (s *Server) restBlocks(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        q := r.URL.Query()
        limit := 20
        offset := uint64(0)

        if l := q.Get("limit"); l != "" {
                if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
                        limit = n
                }
        }
        if o := q.Get("offset"); o != "" {
                if n, err := strconv.ParseUint(o, 10, 64); err == nil {
                        offset = n
                }
        }

        tip := s.chain.Tip()
        if tip == nil {
                writeJSON(w, http.StatusOK, map[string]interface{}{"blocks": []interface{}{}, "total": 0})
                return
        }
        total := tip.Header.Height + 1 // heights 0..tip

        // Return blocks from offset upward (most-recent-first when no offset given)
        startHeight := int64(tip.Header.Height) - int64(offset)
        if startHeight < 0 {
                writeJSON(w, http.StatusOK, map[string]interface{}{"blocks": []interface{}{}, "total": total})
                return
        }

        blocks := make([]BlockResponse, 0, limit)
        for h := startHeight; h >= 0 && len(blocks) < limit; h-- {
                b := s.chain.GetByHeight(uint64(h))
                if b != nil {
                        blocks = append(blocks, blockToResponse(b))
                }
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "blocks": blocks,
                "total":  total,
                "limit":  limit,
                "offset": offset,
        })
}

// ─── GET /api/v1/blocks/{id} and /api/v1/blocks/{id}/transactions ────────────

// restBlockByIDOrTxs dispatches:
//   GET /api/v1/blocks/{id}              → block detail
//   GET /api/v1/blocks/{id}/transactions → transactions in block
func (s *Server) restBlockByIDOrTxs(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }
        tail := pathSuffix("/api/v1/blocks/", r.URL.Path)
        if tail == "" {
                s.restBlocks(w, r)
                return
        }

        // Check for /transactions suffix
        const txsSuffix = "/transactions"
        if strings.HasSuffix(tail, txsSuffix) {
                id := strings.TrimSuffix(tail, txsSuffix)
                // Try in-memory chain first (O(1), covers the recent window).
                if b := s.lookupBlockMem(id); b != nil {
                        s.writeBlockTransactions(w, b)
                        return
                }
                // Validate ID syntax: reject before hitting disk to give 400 not 404.
                if !isValidBlockID(id) {
                        writeJSONError(w, http.StatusBadRequest, "id must be a height (integer) or 64-hex-char hash")
                        return
                }
                // Fall back to the on-disk store for old or pruned blocks.
                if s.blockStore != nil {
                        fullBlock, prunedBlock, _ := s.lookupBlockFromDisk(id)
                        if fullBlock != nil {
                                s.writeBlockTransactions(w, fullBlock)
                                return
                        }
                        if prunedBlock != nil {
                                s.writePrunedBlockTransactions(w, prunedBlock)
                                return
                        }
                }
                writeJSONError(w, http.StatusNotFound, "block not found")
                return
        }

        // Block detail endpoint
        if b := s.lookupBlockMem(tail); b != nil {
                writeJSON(w, http.StatusOK, blockToResponse(b))
                return
        }
        // Validate ID syntax before disk fallback.
        if !isValidBlockID(tail) {
                writeJSONError(w, http.StatusBadRequest, "id must be a height (integer) or 64-hex-char hash")
                return
        }
        // Fall back to disk for old / pruned blocks.
        if s.blockStore != nil {
                fullBlock, prunedBlock, _ := s.lookupBlockFromDisk(tail)
                if fullBlock != nil {
                        writeJSON(w, http.StatusOK, blockToResponse(fullBlock))
                        return
                }
                if prunedBlock != nil {
                        writeJSON(w, http.StatusOK, prunedBlockDetailResponse(prunedBlock))
                        return
                }
        }
        writeJSONError(w, http.StatusNotFound, "block not found")
}

// isValidBlockID returns true when id is either a decimal height or a
// 64-character lowercase hex block hash (32 bytes).
func isValidBlockID(id string) bool {
        if _, err := strconv.ParseUint(id, 10, 64); err == nil {
                return true
        }
        raw, err := hex.DecodeString(id)
        return err == nil && len(raw) == 32
}

// lookupBlockMem returns the block from the in-memory sliding window, or nil.
func (s *Server) lookupBlockMem(id string) *core.Block {
        if height, err := strconv.ParseUint(id, 10, 64); err == nil {
                return s.chain.GetByHeight(height)
        }
        raw, err := hex.DecodeString(id)
        if err != nil || len(raw) != 32 {
                return nil
        }
        var hash crypto.Hash32
        copy(hash[:], raw)
        return s.chain.GetByHash(hash)
}

// lookupBlockFromDisk reads a block from LevelDB by height or hash string.
// It handles both storage formats:
//
//   - core.Block JSON: written by the node via json.Marshal(b) / PutRawBlock.
//     Identified by the top-level "Header" key (Go's default field name).
//
//   - StoredBlock JSON: written by the pruner via PutBlock after TxData is
//     stripped.  Identified by the "tx_count" key (json struct tag).
//
// Returns (fullBlock, nil, nil) for the native format, (nil, prunedBlock, nil)
// for the pruned format, or (nil, nil, nil) if not found / unrecognised.
func (s *Server) lookupBlockFromDisk(id string) (*core.Block, *store.StoredBlock, error) {
        var rawBytes []byte
        var ioErr error

        if height, err := strconv.ParseUint(id, 10, 64); err == nil {
                rawBytes, ioErr = s.blockStore.GetRawBlockByHeight(height)
        } else {
                rawSlice, hexErr := hex.DecodeString(id)
                if hexErr != nil || len(rawSlice) != 32 {
                        return nil, nil, fmt.Errorf("invalid id")
                }
                var h crypto.Hash32
                copy(h[:], rawSlice)
                rawBytes, ioErr = s.blockStore.GetRawBlock(h)
        }

        if ioErr != nil || len(rawBytes) == 0 {
                return nil, nil, ioErr
        }

        // Probe the top-level JSON to determine which format was stored.
        // core.Block is marshaled with Go default field names ("Header", "Txs").
        // StoredBlock uses json struct tags ("tx_count", "tx_data", etc.).
        var probe struct {
                Header  *json.RawMessage `json:"Header"`   // present in core.Block JSON
                TxCount *int             `json:"tx_count"` // present in StoredBlock JSON
        }
        if err := json.Unmarshal(rawBytes, &probe); err != nil {
                return nil, nil, nil // unrecognised format — treat as not found
        }

        if probe.Header != nil {
                // Native core.Block format.
                var b core.Block
                if err := json.Unmarshal(rawBytes, &b); err != nil {
                        return nil, nil, nil
                }
                return &b, nil, nil
        }

        if probe.TxCount != nil {
                // Pruned StoredBlock format written by prune.go.
                var sb store.StoredBlock
                if err := json.Unmarshal(rawBytes, &sb); err != nil {
                        return nil, nil, nil
                }
                return nil, &sb, nil
        }

        return nil, nil, nil
}

// writePrunedBlockTransactions returns the transactions endpoint response for a
// block whose TxData was stripped by the pruner.  pruned:true signals to the
// explorer that it should show an "archived" notice rather than "no txs".
func (s *Server) writePrunedBlockTransactions(w http.ResponseWriter, sb *store.StoredBlock) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "block_hash":   fmt.Sprintf("%x", sb.Hash[:]),
                "block_height": sb.Height,
                "tx_count":     sb.TxCount,
                "transactions": []interface{}{},
                "pruned":       true,
        })
}

// prunedBlockDetailResponse builds a block-detail response from a pruned
// StoredBlock.  Fields not preserved by the pruner (validator_pub, merkle_root,
// oracle_price) are empty/zero; pruned:true is always set.
func prunedBlockDetailResponse(sb *store.StoredBlock) map[string]interface{} {
        return map[string]interface{}{
                "hash":          fmt.Sprintf("%x", sb.Hash[:]),
                "height":        sb.Height,
                "prev_hash":     fmt.Sprintf("%x", sb.PrevHash[:]),
                "timestamp":     time.Unix(0, sb.Timestamp).UTC().Format(time.RFC3339),
                "round":         sb.Round,
                "tx_count":      sb.TxCount,
                "validator_pub": "",
                "merkle_root":   "",
                "size":          0,
                "oracle_price":       0,
                "fees_burned_napro": 0,
                "pruned":            true,
        }
}

// resolveBlock looks up a block by height or hash; writes error and returns nil on failure.
// Kept for RPC handlers that don't need store fallback.
func (s *Server) resolveBlock(w http.ResponseWriter, id string) *core.Block {
        if b := s.lookupBlockMem(id); b != nil {
                return b
        }
        // Validate id syntax before returning 404 vs 400.
        if _, err := strconv.ParseUint(id, 10, 64); err != nil {
                raw, hexErr := hex.DecodeString(id)
                if hexErr != nil || len(raw) != 32 {
                        writeJSONError(w, http.StatusBadRequest, "id must be a height (integer) or 64-hex-char hash")
                        return nil
                }
        }
        writeJSONError(w, http.StatusNotFound, "block not found")
        return nil
}

// BlockTxItem is one transaction summary inside a block.
type BlockTxItem struct {
        Hash       string `json:"hash"`
        TxIndex    int    `json:"tx_index"`
        IsCoinbase bool   `json:"is_coinbase"`
        Inputs     int    `json:"inputs"`
        Outputs    int    `json:"outputs"`
        Fee        uint64 `json:"fee"`
        Size       int    `json:"size"`
}

// writeBlockTransactions writes the transactions of a block as JSON.
func (s *Server) writeBlockTransactions(w http.ResponseWriter, b *core.Block) {
        txs := make([]BlockTxItem, 0, len(b.Txs))
        for i, tx := range b.Txs {
                h := tx.Hash()
                txs = append(txs, BlockTxItem{
                        Hash:       fmt.Sprintf("%x", h[:]),
                        TxIndex:    i,
                        IsCoinbase: i == 0 && tx.IsCoinbase(),
                        Inputs:     len(tx.Inputs),
                        Outputs:    len(tx.Outputs),
                        Fee:        tx.Fee,
                        Size:       tx.Size(),
                })
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "block_hash":   fmt.Sprintf("%x", func() [32]byte { h := b.Hash(); return h }()),
                "block_height": b.Header.Height,
                "tx_count":     len(txs),
                "transactions": txs,
        })
}

// ─── GET /api/v1/transactions/{hash} (2.1.11) ────────────────────────────────

func (s *Server) restTransaction(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }
        hashStr := pathSuffix("/api/v1/transactions/", r.URL.Path)
        raw, err := hex.DecodeString(hashStr)
        if err != nil || len(raw) != 32 {
                writeJSONError(w, http.StatusBadRequest, "invalid hash: must be 64 hex chars")
                return
        }
        var hash crypto.Hash32
        copy(hash[:], raw)

        tx, loc, ok := s.chain.GetTransaction(hash)
        if ok {
                bHash := loc.Block.Hash()
                writeJSON(w, http.StatusOK, TxResponse{
                        Hash:        hashStr,
                        BlockHash:   fmt.Sprintf("%x", bHash[:]),
                        BlockHeight: loc.Block.Header.Height,
                        TxIndex:     loc.TxIndex,
                        IsCoinbase:  loc.TxIndex == 0 && tx.IsCoinbase(),
                        Inputs:      len(tx.Inputs),
                        Outputs:     len(tx.Outputs),
                        Fee:         tx.Fee,
                        Size:        tx.Size(),
                        Version:     uint8(tx.Version),
                })
                return
        }

        // Check mempool
        if mp, found := s.mempool.Get(hash); found {
                writeJSON(w, http.StatusOK, TxResponse{
                        Hash:       hashStr,
                        IsCoinbase: mp.IsCoinbase(),
                        Inputs:     len(mp.Inputs),
                        Outputs:    len(mp.Outputs),
                        Fee:        mp.Fee,
                        Size:       mp.Size(),
                        Version:    uint8(mp.Version),
                        Pending:    true,
                })
                return
        }

        writeJSONError(w, http.StatusNotFound, "transaction not found")
}

// ─── GET /api/v1/address/{addr}/transactions (2.1.12) ────────────────────────

// AddressTx is a simplified tx record for address history.
type AddressTx struct {
        TxHash      string `json:"tx_hash"`
        BlockHeight uint64 `json:"block_height"`
        TxIndex     int    `json:"tx_index"`
        OutputIndex int    `json:"output_index"`
}

func (s *Server) restAddressTxs(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        // Path: /api/v1/address/{addr}/transactions
        tail := pathSuffix("/api/v1/address/", r.URL.Path)
        parts := strings.SplitN(tail, "/", 2)
        addrStr := parts[0]

        addr := crypto.Address(addrStr)
        if err := crypto.Validate(addr); err != nil {
                writeJSONError(w, http.StatusBadRequest, "invalid address: "+err.Error())
                return
        }

        // Decode spend pub from address for output matching
        _, spendPub, _, decErr := crypto.DecodeAddress(addr)
        if decErr != nil {
                writeJSONError(w, http.StatusBadRequest, "cannot decode address")
                return
        }

        // Scan canonical chain for outputs matching the spend pub key (transparent match only).
        // Full privacy scanning requires a view key — that is a Phase 2+ feature.
        tip := s.chain.Tip()
        if tip == nil {
                writeJSON(w, http.StatusOK, map[string]interface{}{"transactions": []AddressTx{}, "address": addrStr})
                return
        }

        limit := 50
        if l := r.URL.Query().Get("limit"); l != "" {
                if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
                        limit = n
                }
        }

        results := make([]AddressTx, 0, 16)
        for h := int64(tip.Header.Height); h >= 0 && len(results) < limit; h-- {
                b := s.chain.GetByHeight(uint64(h))
                if b == nil {
                        continue
                }
                txHash := func(i int) string {
                        hh := b.Txs[i].Hash()
                        return fmt.Sprintf("%x", hh[:])
                }
                // Mint outputs at this height use mint_pub = spend_pub + height*G
                // (see core.BuildMintTx). Recompute the expected key for this height
                // so per-block coinbase rewards are still discoverable by address scan.
                expectedMintPub := spendPub
                if heightPub, hErr := crypto.ScalarMulBase(crypto.ScalarFromUint64(uint64(h))); hErr == nil {
                        if shifted, aErr := crypto.AddPoints(spendPub, heightPub); aErr == nil {
                                expectedMintPub = shifted
                        }
                }
                for i, tx := range b.Txs {
                        for j, out := range tx.Outputs {
                                if out.OneTimePub == spendPub || out.OneTimePub == expectedMintPub {
                                        results = append(results, AddressTx{
                                                TxHash:      txHash(i),
                                                BlockHeight: uint64(h),
                                                TxIndex:     i,
                                                OutputIndex: j,
                                        })
                                }
                        }
                }
        }

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "address":      addrStr,
                "transactions": results,
                "note":         "shows transparent (non-stealth) outputs only; full privacy scanning requires view key",
        })
}

// ─── GET /api/v1/network/bans ─────────────────────────────────────────────────
//
// Returns all currently active P2P bans.  Requires the ban-list function to be
// wired via SetBanListFunc; returns 503 when the P2P layer is not running.
//
// DELETE /api/v1/network/bans/:addr
//
// Lifts the ban for addr. Returns 404 when no active ban exists for that addr.

// banAddRequest is the JSON body for POST /api/v1/network/bans.
type banAddRequest struct {
        Addr            string `json:"addr"`             // IP or IP:port
        Reason          string `json:"reason"`           // human-readable reason
        DurationMinutes int    `json:"duration_minutes"` // 0 → permanent ban (~100 years); negative → default 60 minutes
}

func (s *Server) restNetworkBans(w http.ResponseWriter, r *http.Request) {
        switch r.Method {
        case http.MethodGet:
                if s.banListFn == nil {
                        writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                        return
                }
                bans := s.banListFn()
                if bans == nil {
                        bans = []BanEntry{}
                }
                writeJSON(w, http.StatusOK, map[string]interface{}{"bans": bans})

        case http.MethodPost:
                if s.banAddFn == nil {
                        writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                        return
                }
                var req banAddRequest
                if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                        writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
                        return
                }
                if req.Addr == "" {
                        writeJSONError(w, http.StatusBadRequest, "addr is required")
                        return
                }
                permanent := req.DurationMinutes == 0
                if req.DurationMinutes < 0 {
                        req.DurationMinutes = 60
                }
                var d time.Duration
                if permanent {
                        d = 100 * 365 * 24 * time.Hour // ~100 years sentinel for permanent bans
                } else {
                        d = time.Duration(req.DurationMinutes) * time.Minute
                }
                s.banAddFn(req.Addr, req.Reason, d)
                expiresAt := time.Now().Add(d).UTC()
                writeJSON(w, http.StatusCreated, map[string]interface{}{
                        "message":    "ban added",
                        "addr":       req.Addr,
                        "reason":     req.Reason,
                        "expires_at": expiresAt,
                        "permanent":  permanent,
                })

        default:
                writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
        }
}

func (s *Server) restNetworkBanByAddr(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodDelete {
                writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
                return
        }
        if s.banLiftFn == nil {
                writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                return
        }
        addr := pathSuffix("/api/v1/network/bans/", r.URL.Path)
        if addr == "" {
                writeJSONError(w, http.StatusBadRequest, "addr is required")
                return
        }
        lifted := s.banLiftFn(addr)
        if !lifted {
                writeJSONError(w, http.StatusNotFound, "no active ban for this address")
                return
        }
        writeJSON(w, http.StatusOK, map[string]string{"message": "ban lifted", "addr": addr})
}

// ─── GET /api/v1/network/stats (2.1.13) ──────────────────────────────────────

func (s *Server) restNetworkStats(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        tip := s.chain.Tip()
        height := uint64(0)
        var tipHash string
        var tipTime string
        totalTxs := 0

        if tip != nil {
                height = tip.Header.Height
                h := tip.Hash()
                tipHash = fmt.Sprintf("%x", h[:])
                tipTime = time.Unix(0, tip.Header.Timestamp).UTC().Format(time.RFC3339)

                // Count txs in last 10 blocks for TPS approximation
                const window = 10
                windowStart := int64(height) - window + 1
                if windowStart < 0 {
                        windowStart = 0
                }
                windowTxs := 0
                windowBlocks := 0
                for h2 := windowStart; h2 <= int64(height); h2++ {
                        b := s.chain.GetByHeight(uint64(h2))
                        if b != nil {
                                for txIdx, tx := range b.Txs {
                                        // Skip block-reward coinbase (always index 0, no inputs).
                                        // Admin mints (index > 0) are counted as user activity.
                                        if txIdx == 0 && tx.IsCoinbase() {
                                                continue
                                        }
                                        windowTxs++
                                }
                                windowBlocks++
                        }
                }
                // Use cached counter — maintained atomically as blocks arrive.
                // Avoids O(chain-length) scan on every stats call.
                totalTxs = int(s.TxTotal())

                // TPS estimate: assume ~3s block time
                tps := float64(0)
                if windowBlocks > 1 {
                        tps = float64(windowTxs) / (float64(windowBlocks) * 3.0)
                }
                writeJSON(w, http.StatusOK, map[string]interface{}{
                        "height":                   height,
                        "tip_hash":                 tipHash,
                        "tip_time":                 tipTime,
                        "total_txs":                totalTxs,
                        "mempool_count":            s.mempool.Count(),
                        "tps_last_10":              tps,
                        "block_time_secs":          3,
                        "timestamp_rejected_count": s.TimestampRejectedCount(),
                })
                return
        }

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "height":                   0,
                "tip_hash":                 "",
                "tip_time":                 "",
                "total_txs":                0,
                "mempool_count":            s.mempool.Count(),
                "tps_last_10":              0,
                "timestamp_rejected_count": s.TimestampRejectedCount(),
        })
}

// ─── GET /api/v1/validators ──────────────────────────────────────────────────

// validatorResponse is one validator entry returned by the REST API.
type validatorResponse struct {
	PubKey              string                   `json:"pub_key"`
	StakeNAPR           uint64                   `json:"stake_napr"`
	StakeAPR            float64                  `json:"stake_apr"`
	Status              string                   `json:"status"`
	ActivationEpoch     uint64                   `json:"activation_epoch"`
	UnbondEndBlock      uint64                   `json:"unbond_end_block,omitempty"`
	PendingUnbondingNAPR uint64                  `json:"pending_unbonding_napr"`
	PendingUnbondingAPR  float64                 `json:"pending_unbonding_apr"`
	UnbondingQueue      []unbondingEntryResponse `json:"unbonding_queue"`
}

// unbondingEntryResponse is one entry in a validator's partial unbonding queue.
type unbondingEntryResponse struct {
	AmountNAPR     uint64  `json:"amount_napr"`
	AmountAPR      float64 `json:"amount_apr"`
	EndBlock       uint64  `json:"end_block"`
	EndEstimatedMs int64   `json:"end_estimated_ms"` // wall-clock estimate at 1 block/s
}

func validatorToResponse(e core.ValidatorEntry, currentHeight uint64) validatorResponse {
	queue := make([]unbondingEntryResponse, 0, len(e.UnbondingQueue))
	nowMs := time.Now().UnixMilli()
	for _, ub := range e.UnbondingQueue {
		blocksLeft := int64(0)
		if ub.EndBlock > currentHeight {
			blocksLeft = int64(ub.EndBlock - currentHeight)
		}
		queue = append(queue, unbondingEntryResponse{
			AmountNAPR:     ub.Amount,
			AmountAPR:      float64(ub.Amount) / 1e8,
			EndBlock:       ub.EndBlock,
			EndEstimatedMs: nowMs + blocksLeft*1000, // 1 block ≈ 1 second
		})
	}
	pending := e.PendingUnbondingNAPR()
	return validatorResponse{
		PubKey:               e.PubKey.Hex(),
		StakeNAPR:            e.StakeNAPR,
		StakeAPR:             float64(e.StakeNAPR) / 1e8,
		Status:               e.Status.String(),
		ActivationEpoch:      e.ActivationEpoch,
		UnbondEndBlock:       e.UnbondEndBlock,
		PendingUnbondingNAPR: pending,
		PendingUnbondingAPR:  float64(pending) / 1e8,
		UnbondingQueue:       queue,
	}
}

func (s *Server) restValidators(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.registry == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"validators": []interface{}{}})
		return
	}
	currentHeight := uint64(0)
	if tip := s.chain.Tip(); tip != nil {
		currentHeight = tip.Header.Height
	}
	entries := s.registry.AllEntries()
	result := make([]validatorResponse, 0, len(entries))
	for _, e := range entries {
		result = append(result, validatorToResponse(e, currentHeight))
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"validators": result,
		"count":      len(result),
	})
}

// ─── GET /api/v1/validators/{pubkey}/unbonding ───────────────────────────────

func (s *Server) restValidatorUnbonding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	// Path: /api/v1/validators/{pubkey}/unbonding
	tail := pathSuffix("/api/v1/validators/", r.URL.Path)
	const unbondingSuffix = "/unbonding"
	if !strings.HasSuffix(tail, unbondingSuffix) {
		writeJSONError(w, http.StatusNotFound, "not found; use /api/v1/validators/{pubkey}/unbonding")
		return
	}
	pubHex := strings.TrimSuffix(tail, unbondingSuffix)
	if pubHex == "" {
		writeJSONError(w, http.StatusBadRequest, "pubkey required")
		return
	}
	if s.registry == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"pub_key": pubHex, "pending_unbonding_napr": 0, "pending_unbonding_apr": 0.0, "unbonding_queue": []interface{}{},
		})
		return
	}
	pubBytes, err := hex.DecodeString(pubHex)
	if err != nil || len(pubBytes) != 32 {
		writeJSONError(w, http.StatusBadRequest, "pubkey must be 64 hex chars")
		return
	}
	var pub crypto.ValidatorPubKey = pubBytes
	entry, ok := s.registry.GetEntry(pub)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "validator not found in registry")
		return
	}
	currentHeight := uint64(0)
	if tip := s.chain.Tip(); tip != nil {
		currentHeight = tip.Header.Height
	}
	resp := validatorToResponse(entry, currentHeight)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pub_key":               resp.PubKey,
		"stake_napr":            resp.StakeNAPR,
		"stake_apr":             resp.StakeAPR,
		"status":                resp.Status,
		"pending_unbonding_napr": resp.PendingUnbondingNAPR,
		"pending_unbonding_apr":  resp.PendingUnbondingAPR,
		"unbonding_queue":        resp.UnbondingQueue,
	})
}

// ─── POST /api/v1/admin/partial-unstake ──────────────────────────────────────

type partialUnstakeRequest struct {
	PubKey     string `json:"pub_key"`    // 64-hex target validator pubkey
	AmountNAPR uint64 `json:"amount_napr"` // nAPRO to withdraw
}

// restAdminPartialUnstake builds a signed StakePartialWithdraw transaction and
// submits it to the mempool.  The node may only unstake its OWN validator
// (pub_key in the request must equal this node's configured validator pubkey).
// This preserves the existing signature invariant (pub.Verify == target) while
// allowing the admin REST endpoint to initiate the withdrawal.
func (s *Server) restAdminPartialUnstake(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.myKey == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "validator key not configured on this node")
		return
	}
	if s.registry == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "validator registry not initialised")
		return
	}

	var req partialUnstakeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.AmountNAPR == 0 {
		writeJSONError(w, http.StatusBadRequest, "amount_napr must be > 0")
		return
	}
	pubBytes, err := hex.DecodeString(req.PubKey)
	if err != nil || len(pubBytes) != 32 {
		writeJSONError(w, http.StatusBadRequest, "pub_key must be a 64-hex-char Ed25519 public key")
		return
	}
	targetPub := crypto.ValidatorPubKey(pubBytes)

	// Security: this endpoint can only unstake the LOCAL node's own validator.
	// StakePartialWithdraw requires pub.Verify(sig) — only the key holder can
	// authorize their own withdrawal.  Cross-validator admin unstake is not
	// supported via REST; validators must submit their own signed tx.
	myPub := s.myKey.Public()
	if !myPub.Equals(targetPub) {
		writeJSONError(w, http.StatusForbidden,
			"this endpoint can only unstake the local node's own validator; connect to the target validator's node to initiate its withdrawal")
		return
	}

	// Pre-validate against registry so we fail fast before touching the mempool.
	entry, ok := s.registry.GetEntry(targetPub)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "validator not found in registry")
		return
	}
	effectiveMin := s.registry.CurrentMinStake()
	if entry.StakeNAPR <= req.AmountNAPR {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"withdrawal %d >= current stake %d; use full StakeWithdraw instead",
			req.AmountNAPR, entry.StakeNAPR))
		return
	}
	remaining := entry.StakeNAPR - req.AmountNAPR
	if remaining < effectiveMin {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf(
			"remaining stake %.4f APRO < minimum %.4f APRO; reduce withdrawal amount",
			float64(remaining)/1e8, float64(effectiveMin)/1e8))
		return
	}

	// Sign with the node's own key (which IS the target validator's key).
	// pub.Verify(StakeSignMsg(StakePartialWithdraw, pub, amount), sig) will pass
	// because myKey.Public() == targetPub.
	msg := core.StakeSignMsg(core.StakePartialWithdraw, targetPub, req.AmountNAPR)
	sig, err := s.myKey.Sign(msg)
	if err != nil {
		s.log.Error("admin partial unstake: sign failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "sign: "+err.Error())
		return
	}
	extra, err := core.EncodeStakeExtra(core.StakePartialWithdraw, targetPub, req.AmountNAPR, sig)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode extra: "+err.Error())
		return
	}
	tx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}
	if err := s.mempool.Add(tx); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "mempool: "+err.Error())
		return
	}

	txHash := tx.Hash()
	txHashHex := fmt.Sprintf("%x", txHash[:])
	s.log.Info("admin partial unstake queued",
		"pub_key", req.PubKey[:8],
		"amount_napr", req.AmountNAPR,
		"tx_hash", txHashHex,
	)

	currentHeight := uint64(0)
	if tip := s.chain.Tip(); tip != nil {
		currentHeight = tip.Header.Height
	}
	endBlock := currentHeight + core.PartialUnbondingBlocks
	endEstimatedMs := time.Now().UnixMilli() + int64(core.PartialUnbondingBlocks)*1000

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"tx_hash":          txHashHex,
		"pub_key":          req.PubKey,
		"amount_napr":      req.AmountNAPR,
		"amount_apr":       float64(req.AmountNAPR) / 1e8,
		"end_block":        endBlock,
		"end_estimated_ms": endEstimatedMs,
		"status":           "pending",
		"message":          "StakePartialWithdraw transaction submitted to mempool; applied when included in the next block",
	})
}

// restMyValidator exposes the node's own validator pubkey so the admin panel
// can show the "Withdraw Excess Stake" action only for the locally-managed validator.
func (s *Server) restMyValidator(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	if s.myKey == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"pub_key": nil})
		return
	}
	pub := s.myKey.Public()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pub_key": pub.Hex(),
	})
}

// ─── POST /api/v1/admin/mint ──────────────────────────────────────────────────

// mintRequest is the JSON body for the admin mint endpoint.
type mintRequest struct {
        Address   string  `json:"address"`    // Aperod wallet address
        AmountAPR float64 `json:"amount_apr"` // amount in APRO, fractional allowed (converted to nAPRO internally)
}

// mintResponse is returned on success.
type mintResponse struct {
        TxHash    string  `json:"tx_hash"`
        AmountAPR float64 `json:"amount_apr"`
        Address   string  `json:"address"`
        BlindHex  string  `json:"blind_hex"` // hex-encoded blind factor used for the commitment
}

// ─── GET /api/v1/network/identity ────────────────────────────────────────────

// restNetworkIdentity returns the node's P2P TLS fingerprint, listen address,
// and node ID.  Requires the X-API-Key header when an API key is configured.
func (s *Server) restNetworkIdentity(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }
        if s.apiKey != "" && r.Header.Get("X-API-Key") != s.apiKey {
                writeJSONError(w, http.StatusUnauthorized, "missing or invalid X-API-Key")
                return
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "node_id":         s.nodeID,
                "tls_fingerprint": s.tlsFingerprint,
                "listen_addr":     s.p2pListenAddr,
        })
}

// restAdminMint creates a coinbase-style mint transaction and adds it to the mempool.
// Called by the Node.js API server after it records the mint in PostgreSQL.
// This endpoint is only reachable from localhost (127.0.0.1:8545 is not exposed
// to the internet), so no additional auth is required.
func (s *Server) restAdminMint(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
                return
        }

        var req mintRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
                return
        }

        if req.Address == "" {
                writeJSONError(w, http.StatusBadRequest, "address required")
                return
        }
        if err := crypto.Validate(crypto.Address(req.Address)); err != nil {
                writeJSONError(w, http.StatusBadRequest, "invalid address: "+err.Error())
                return
        }
        if math.IsNaN(req.AmountAPR) || math.IsInf(req.AmountAPR, 0) || req.AmountAPR <= 0 || req.AmountAPR > 100_000_000 {
                writeJSONError(w, http.StatusBadRequest, "amount_apr must be > 0 and <= 100000000")
                return
        }

        // Convert APRO → nAPRO (1 APRO = 10^8 nAPRO). Round to the nearest nAPRO
        // to absorb float64 rounding error; amounts are validated above to be
        // well within uint64 range so the conversion cannot overflow.
        const nAPRPerAPR float64 = 100_000_000
        amountNAPR := uint64(math.Round(req.AmountAPR * nAPRPerAPR))
        if amountNAPR == 0 {
                writeJSONError(w, http.StatusBadRequest, "amount_apr too small: rounds to 0 nAPRO")
                return
        }

        // height=0 is intentional for one-off admin mints: their future block
        // inclusion height isn't known at submission time (they sit in the
        // mempool like any other tx). This reproduces the legacy transparent
        // behavior (mint_pub == spend_pub) and is only unsafe if the exact same
        // address+amount is minted more than once — see BuildMintTx doc comment.
        tx, err := core.BuildMintTx(crypto.Address(req.Address), amountNAPR, 0)
        if err != nil {
                s.log.Error("admin mint: build tx failed", "err", err)
                writeJSONError(w, http.StatusInternalServerError, "build mint tx: "+err.Error())
                return
        }

        // Use AddPrivileged so the coinbase bypasses the external-coinbase rejection
        // guard in Add().  This endpoint is only reachable from localhost (127.0.0.1:8545).
        if err := s.mempool.AddPrivileged(*tx); err != nil {
                s.log.Error("admin mint: mempool add failed", "err", err)
                writeJSONError(w, http.StatusInternalServerError, "mempool: "+err.Error())
                return
        }

        hash := tx.Hash()
        txHashHex := fmt.Sprintf("%x", hash[:])

        // Compute the deterministic blind used in the commitment so the caller
        // can store it and always recover the spend path without relying on the
        // blind being re-derived from a potentially different algorithm later.
        _, spendPub, _, err := crypto.DecodeAddress(crypto.Address(req.Address))
        if err != nil {
                writeJSONError(w, http.StatusInternalServerError, "decode address for blind: "+err.Error())
                return
        }
        mintBlind, err := crypto.DeterministicMintBlind(spendPub, amountNAPR)
        if err != nil {
                writeJSONError(w, http.StatusInternalServerError, "compute mint blind: "+err.Error())
                return
        }
        blindHex := fmt.Sprintf("%x", mintBlind[:])

        s.log.Info("admin mint submitted", "address", req.Address, "amount_apr", req.AmountAPR, "tx_hash", txHashHex)

        writeJSON(w, http.StatusCreated, mintResponse{
                TxHash:    txHashHex,
                AmountAPR: req.AmountAPR,
                Address:   req.Address,
                BlindHex:  blindHex,
        })
}
