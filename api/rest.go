package api

// REST API handlers for Phase 2 (blocks 2.1.10-2.1.13).
// Routes:
//   GET  /api/v1/blocks                        — paginated block list
//   GET  /api/v1/blocks/{id}                   — block by height or hash
//   GET  /api/v1/transactions/{hash}           — tx by hash
//   GET  /api/v1/address/{addr}/transactions   — incoming tx for address
//   GET  /api/v1/network/stats                 — network statistics
//   POST /api/v1/admin/mint                    — admin-only: mint APRO to address

import (
        "encoding/hex"
        "encoding/json"
        "fmt"
        "net/http"
        "strconv"
        "strings"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// registerRESTRoutes adds all REST routes to the server mux.
func (s *Server) registerRESTRoutes() {
        s.mux.HandleFunc("/api/v1/blocks", s.restBlocks)
        s.mux.HandleFunc("/api/v1/blocks/", s.restBlockByID)
        s.mux.HandleFunc("/api/v1/transactions/", s.restTransaction)
        s.mux.HandleFunc("/api/v1/address/", s.restAddressTxs)
        s.mux.HandleFunc("/api/v1/network/stats", s.restNetworkStats)
        s.mux.HandleFunc("/api/v1/admin/mint", s.restAdminMint)
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

// ─── GET /api/v1/blocks/{id} (2.1.10 / 2.1.11) ───────────────────────────────

func (s *Server) restBlockByID(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }
        id := pathSuffix("/api/v1/blocks/", r.URL.Path)
        if id == "" {
                s.restBlocks(w, r)
                return
        }

        // Try height first
        if height, err := strconv.ParseUint(id, 10, 64); err == nil {
                b := s.chain.GetByHeight(height)
                if b == nil {
                        writeJSONError(w, http.StatusNotFound, fmt.Sprintf("block not found at height %d", height))
                        return
                }
                writeJSON(w, http.StatusOK, blockToResponse(b))
                return
        }

        // Try as hash
        raw, err := hex.DecodeString(id)
        if err != nil || len(raw) != 32 {
                writeJSONError(w, http.StatusBadRequest, "id must be a height (integer) or 64-hex-char hash")
                return
        }
        var hash crypto.Hash32
        copy(hash[:], raw)
        b := s.chain.GetByHash(hash)
        if b == nil {
                writeJSONError(w, http.StatusNotFound, "block not found")
                return
        }
        writeJSON(w, http.StatusOK, blockToResponse(b))
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
                for i, tx := range b.Txs {
                        for j, out := range tx.Outputs {
                                if out.OneTimePub == spendPub {
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
                // Sum all user txs (skip block-reward coinbase at index 0)
                for h2 := int64(0); h2 <= int64(height); h2++ {
                        b := s.chain.GetByHeight(uint64(h2))
                        if b != nil {
                                for txIdx, tx := range b.Txs {
                                        if txIdx == 0 && tx.IsCoinbase() {
                                                continue
                                        }
                                        totalTxs++
                                }
                        }
                }

                // TPS estimate: assume ~3s block time
                tps := float64(0)
                if windowBlocks > 1 {
                        tps = float64(windowTxs) / (float64(windowBlocks) * 3.0)
                }
                writeJSON(w, http.StatusOK, map[string]interface{}{
                        "height":          height,
                        "tip_hash":        tipHash,
                        "tip_time":        tipTime,
                        "total_txs":       totalTxs,
                        "mempool_count":   s.mempool.Count(),
                        "tps_last_10":     tps,
                        "block_time_secs": 3,
                })
                return
        }

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "height":        0,
                "tip_hash":      "",
                "tip_time":      "",
                "total_txs":     0,
                "mempool_count": s.mempool.Count(),
                "tps_last_10":   0,
        })
}

// ─── POST /api/v1/admin/mint ──────────────────────────────────────────────────

// mintRequest is the JSON body for the admin mint endpoint.
type mintRequest struct {
        Address   string `json:"address"`   // Aperod wallet address
        AmountAPR uint64 `json:"amount_apr"` // amount in whole APRO (converted to nAPRO internally)
}

// mintResponse is returned on success.
type mintResponse struct {
        TxHash    string `json:"tx_hash"`
        AmountAPR uint64 `json:"amount_apr"`
        Address   string `json:"address"`
        BlindHex  string `json:"blind_hex"` // hex-encoded blind factor used for the commitment
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
        if req.AmountAPR == 0 || req.AmountAPR > 100_000_000 {
                writeJSONError(w, http.StatusBadRequest, "amount_apr must be 1–100000000")
                return
        }

        // Convert APRO → nAPRO (1 APRO = 10^8 nAPRO).
        const nAPRPerAPR uint64 = 100_000_000
        amountNAPR := req.AmountAPR * nAPRPerAPR

        tx, err := core.BuildMintTx(crypto.Address(req.Address), amountNAPR)
        if err != nil {
                s.log.Error("admin mint: build tx failed", "err", err)
                writeJSONError(w, http.StatusInternalServerError, "build mint tx: "+err.Error())
                return
        }

        if err := s.mempool.Add(*tx); err != nil {
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
