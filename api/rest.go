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
//   POST /api/v1/admin/stake-deposit                   — admin-only: v2 UTXO-backed stake deposit

import (
        "encoding/binary"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "math"
        "net"
        "net/http"
        "strconv"
        "strings"
        "sync/atomic"
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
        s.mux.HandleFunc("/api/v1/scan/outputs", s.restScanOutputs)
        s.mux.HandleFunc("/api/v1/network/stats", s.restNetworkStats)
        s.mux.HandleFunc("/api/v1/network/mempool", s.restMempoolMetrics)
        s.mux.HandleFunc("/api/v1/fee-estimate", s.restFeeEstimate)
        s.mux.HandleFunc("/api/v1/validators", s.restValidators)
        s.mux.HandleFunc("/api/v1/validators/", s.restValidatorUnbonding)
        s.mux.HandleFunc("/api/v1/admin/mint", s.localOnly(s.restAdminMint))
        s.mux.HandleFunc("/api/v1/admin/partial-unstake", s.localOnly(s.restAdminPartialUnstake))
        s.mux.HandleFunc("/api/v1/admin/full-unstake", s.localOnly(s.restAdminFullUnstake))
        s.mux.HandleFunc("/api/v1/admin/stake-deposit", s.localOnly(s.restAdminStakeDeposit))
        s.mux.HandleFunc("/api/v1/my-validator", s.restMyValidator)
        s.mux.HandleFunc("/api/v1/network/identity", s.restNetworkIdentity)
        s.mux.HandleFunc("/api/v1/network/bans", s.localOnly(s.restNetworkBans))
        s.mux.HandleFunc("/api/v1/network/bans/", s.localOnly(s.restNetworkBanByAddr))
        s.mux.HandleFunc("/api/v1/network/whitelist", s.localOnly(s.restNetworkWhitelist))
        s.mux.HandleFunc("/api/v1/network/whitelist/", s.localOnly(s.restNetworkWhitelistByEntry))
        s.mux.HandleFunc("/api/v1/network/whitelist-exemptions", s.localOnly(s.restNetworkWhitelistExemptions))
        s.mux.HandleFunc("/api/v1/network/ban-events", s.localOnly(s.restNetworkBanEvents))
        s.mux.HandleFunc("/api/v1/utxos/decoys", s.restUTXODecoys)
        s.mux.HandleFunc("/api/v1/utxo/", s.restUTXO)
        s.mux.HandleFunc("/api/v1/stake", s.restStakeBroadcast)
        s.mux.HandleFunc("/api/v1/status", s.restStatus)
        // Node-join workflow: export endpoints consumed by aperod-join.sh.
        s.mux.HandleFunc("/api/v1/snapshot/export", s.restSnapshotExport)
        s.mux.HandleFunc("/api/v1/chaindb/export", s.restChainDBExport)
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
//   GET /api/v1/blocks/{id}/outputs      → all outputs (one_time_pub_hex) for the explorer indexer
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

        // Check for /outputs suffix — used by the standalone explorer indexer to
        // populate address_txs.  Returns one record per output, keyed by
        // one_time_pub_hex, so the indexer can build the address→tx mapping
        // without requiring a view key.  Pruned blocks return an empty list
        // (outputs are stripped by the pruner) rather than 404.
        const outputsSuffix = "/outputs"
        if strings.HasSuffix(tail, outputsSuffix) {
                id := strings.TrimSuffix(tail, outputsSuffix)
                if b := s.lookupBlockMem(id); b != nil {
                        s.writeBlockOutputs(w, b)
                        return
                }
                if !isValidBlockID(id) {
                        writeJSONError(w, http.StatusBadRequest, "id must be a height (integer) or 64-hex-char hash")
                        return
                }
                if s.blockStore != nil {
                        fullBlock, _, _ := s.lookupBlockFromDisk(id)
                        if fullBlock != nil {
                                s.writeBlockOutputs(w, fullBlock)
                                return
                        }
                }
                writeJSONError(w, http.StatusNotFound, "block not found")
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

// BlockOutputItem is one transaction output entry returned by the /outputs endpoint.
// one_time_pub_hex is the canonical output identifier used by the explorer indexer
// to populate the address_txs table — it uniquely identifies the output recipient
// without requiring a view-key scan.
type BlockOutputItem struct {
        TxHash        string `json:"tx_hash"`
        TxIndex       int    `json:"tx_index"`
        OutputIndex   int    `json:"output_index"`
        OneTimePubHex string `json:"one_time_pub_hex"`
        IsCoinbase    bool   `json:"is_coinbase"`
}

// writeBlockOutputs writes all outputs of a block as JSON for the explorer indexer.
// Each output is identified by its one_time_pub_hex.  Pruned blocks (nil Txs) return
// an empty list with pruned:true so the indexer can skip them gracefully.
func (s *Server) writeBlockOutputs(w http.ResponseWriter, b *core.Block) {
        bHash := b.Hash()
        items := make([]BlockOutputItem, 0)
        for i, tx := range b.Txs {
                txH := tx.Hash()
                txHashHex := fmt.Sprintf("%x", txH[:])
                isCoinbase := i == 0 && tx.IsCoinbase()
                for j, out := range tx.Outputs {
                        items = append(items, BlockOutputItem{
                                TxHash:        txHashHex,
                                TxIndex:       i,
                                OutputIndex:   j,
                                OneTimePubHex: fmt.Sprintf("%x", out.OneTimePub[:]),
                                IsCoinbase:    isCoinbase,
                        })
                }
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "block_hash":   fmt.Sprintf("%x", bHash[:]),
                "block_height": b.Header.Height,
                "output_count": len(items),
                "outputs":      items,
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

        // Disk fallback: the tx may be in an older block evicted from the
        // in-memory sliding window.  Check the persisted tx-location index.
        if diskTx, diskLoc, found, diskErr := s.getTransactionFromDisk(hash); diskErr == nil && found {
                bHash := diskLoc.Block.Hash()
                writeJSON(w, http.StatusOK, TxResponse{
                        Hash:        hashStr,
                        BlockHash:   fmt.Sprintf("%x", bHash[:]),
                        BlockHeight: diskLoc.Block.Header.Height,
                        TxIndex:     diskLoc.TxIndex,
                        IsCoinbase:  diskLoc.TxIndex == 0 && diskTx.IsCoinbase(),
                        Inputs:      len(diskTx.Inputs),
                        Outputs:     len(diskTx.Outputs),
                        Fee:         diskTx.Fee,
                        Size:        diskTx.Size(),
                        Version:     uint8(diskTx.Version),
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
        IsCoinbase  bool   `json:"is_coinbase"`
}

// AddressUTXO is a single unspent output returned by the address UTXO listing endpoint.
type AddressUTXO struct {
        TxHash          string  `json:"tx_hash"`
        OutIdx          uint32  `json:"out_idx"`
        AmountCommitHex string  `json:"amount_commit_hex"`
        EncAmountHex    string  `json:"enc_amount_hex"`
        BlockHeight     uint64  `json:"block_height"`
        // AmountNapr is the decrypted output amount in nAPRO. Populated when a
        // view key is available (via X-View-Key header or node.yaml view_key).
        // Null when no view key is configured.
        AmountNapr *uint64 `json:"amount_napr"`
        // IsCoinbase is true when this UTXO belongs to a per-block validator
        // reward (coinbase) transaction.  Wallet history displays must filter
        // these out so +0 APRO block-reward entries never pollute user history.
        IsCoinbase bool `json:"is_coinbase"`
}

// restAddressUTXOs handles GET /api/v1/address/{addr}/utxos.
//
// Without a view key: returns transparent / mint outputs (OneTimePub matches
// spend pub directly) with amount_napr=null.
//
// With a view key (X-View-Key header or view_key in node.yaml): also
// discovers stealth outputs via ECDH scan and sets amount_napr for them.
//
// Note on transparent/mint outputs: BuildMintTx leaves TxPubKey and EncAmount
// as zero — there is no ECDH shared secret to derive, so amount_napr remains
// null for those even when a view key is present.  Only stealth outputs
// (TxPubKey = r·G, EncAmount = encrypted via Hs) are decrypted on the fly.
func (s *Server) restAddressUTXOs(w http.ResponseWriter, r *http.Request, addrStr string) {
        addr := crypto.Address(addrStr)
        if err := crypto.Validate(addr); err != nil {
                writeJSONError(w, http.StatusBadRequest, "invalid address: "+err.Error())
                return
        }

        _, spendPub, _, decErr := crypto.DecodeAddress(addr)
        if decErr != nil {
                writeJSONError(w, http.StatusBadRequest, "cannot decode address")
                return
        }

        // Resolve view key: X-View-Key header is the only accepted source.
        // The view_key_hex query parameter was removed (F5 security fix) because
        // query parameters are logged by reverse proxies, access logs, and browser
        // history, leaking the private view scalar to anyone with log access.
        viewKeyHex := r.Header.Get("X-View-Key")
        if viewKeyHex == "" {
                viewKeyHex = s.nodeViewKeyHex
        }

        // Short-TTL response cache: restAddressUTXOs scans the entire UTXO set
        // with Ed25519 point ops per entry (O(n), pins CPU for 20-30 s).
        // The mint-UTXO monitor calls this every 5 min for each admin-mint address.
        // A 90-second TTL lets the first call in each window do the scan;
        // all subsequent calls within that window are served from cache in <1 ms.
        cacheKey := addrStr + "|" + viewKeyHex
        if raw, ok := s.utxoAddrCache.Load(cacheKey); ok {
                e := raw.(*utxoCacheEntry)
                if time.Now().Before(e.expiresAt) {
                        w.Header().Set("Content-Type", "application/json")
                        w.WriteHeader(http.StatusOK)
                        _, _ = w.Write(e.body)
                        return
                }
                s.utxoAddrCache.Delete(cacheKey) // expired, evict
        }

        var viewPriv *crypto.Scalar32
        if viewKeyHex != "" {
                raw, hexErr := hex.DecodeString(viewKeyHex)
                if hexErr == nil && len(raw) == 32 {
                        var vk crypto.Scalar32
                        copy(vk[:], raw)
                        viewPriv = &vk
                }
                // Silently ignore invalid hex — fall back to no-key behaviour.
        }

        // F10 security fix: cap the number of UTXOs scanned to prevent a
        // single request from saturating the node's CPU and memory.
        // 200 000 UTXOs ≈ 200 MB; chains with more active UTXOs should use
        // the paginated archive API instead of the live UTXO endpoint.
        const maxUTXOScan = 200_000
        all := s.utxos.All()
        scanLimited := len(all) > maxUTXOScan
        if scanLimited {
                all = all[:maxUTXOScan]
                s.log.Warn("restAddressUTXOs: UTXO set exceeds scan cap; returning partial results",
                        "cap", maxUTXOScan, "total", len(s.utxos.All()))
        }
        results := make([]AddressUTXO, 0)
        seen := make(map[string]bool) // dedup by "txhash:outidx"

        for _, u := range all {
                var hsScalar *crypto.Scalar32

                // Transparent match: OneTimePub == spendPub (direct payment / old-style).
                // Not coinbase — direct spend-pub payments are normal transfers.
                match := u.OneTimePub == spendPub
                isCoinbase := false

                // Mint match: OneTimePub == spendPub + height*G (coinbase reward outputs).
                // These ARE coinbase — per-block validator block rewards use this key form.
                if !match {
                        if heightPub, hErr := crypto.ScalarMulBase(crypto.ScalarFromUint64(u.BlockHeight)); hErr == nil {
                                if mintPub, aErr := crypto.AddPoints(spendPub, heightPub); aErr == nil {
                                        if u.OneTimePub == mintPub {
                                                match = true
                                                isCoinbase = true
                                        }
                                }
                        }
                }

                // Stealth match: ECDH scan using the view key.
                // ScanForOutput returns the Hs scalar when the output belongs to us;
                // nil when it doesn't.  Stealth outputs are never coinbase.
                if viewPriv != nil {
                        hs, _ := crypto.ScanForOutput(*viewPriv, spendPub, u.TxPubKey, u.OneTimePub)
                        if hs != nil {
                                match = true
                                hsScalar = hs
                                // isCoinbase stays false — ECDH stealth transfers are not coinbase
                        }
                }

                if !match {
                        continue
                }

                key := fmt.Sprintf("%x:%d", u.TxHash[:], u.OutputIndex)
                if seen[key] {
                        continue
                }
                seen[key] = true

                var amountNapr *uint64
                if hsScalar != nil {
                        amt := core.DecryptAmount(u.EncAmount, hsScalar)
                        amountNapr = &amt
                }

                results = append(results, AddressUTXO{
                        TxHash:          fmt.Sprintf("%x", u.TxHash[:]),
                        OutIdx:          u.OutputIndex,
                        AmountCommitHex: fmt.Sprintf("%x", u.AmountCommit[:]),
                        EncAmountHex:    fmt.Sprintf("%x", u.EncAmount[:]),
                        BlockHeight:     u.BlockHeight,
                        AmountNapr:      amountNapr,
                        IsCoinbase:      isCoinbase,
                })
        }

        note := "transparent outputs only; stealth outputs require view-key scanning"
        if viewPriv != nil {
                note = "includes stealth outputs discovered via view-key ECDH scan; amount_napr decoded inline for stealth outputs (transparent/mint outputs have null amount_napr)"
        }

        // Store result in cache before writing response.
        respPayload := map[string]interface{}{
                "address":      addrStr,
                "utxos":        results,
                "note":         note,
                "scan_limited": scanLimited, // true when UTXO set exceeds maxUTXOScan
        }
        if body, marshalErr := json.Marshal(respPayload); marshalErr == nil {
                s.utxoAddrCache.Store(cacheKey, &utxoCacheEntry{
                        body:      body,
                        expiresAt: time.Now().Add(utxoCacheTTL),
                })
        }
        writeJSON(w, http.StatusOK, respPayload)
}

func (s *Server) restAddressTxs(w http.ResponseWriter, r *http.Request) {
        // Path: /api/v1/address/{addr}/transactions
        //        /api/v1/address/{addr}/utxos        (GET)
        //        /api/v1/address/{addr}/scan         (POST — view key in request body)
        tail := pathSuffix("/api/v1/address/", r.URL.Path)
        parts := strings.SplitN(tail, "/", 2)
        addrStr := parts[0]

        // Dispatch sub-paths first so they can accept their own HTTP methods.
        if len(parts) >= 2 && parts[1] == "utxos" {
                if r.Method != http.MethodGet {
                        writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                        return
                }
                s.restAddressUTXOs(w, r, addrStr)
                return
        }
        if len(parts) >= 2 && parts[1] == "scan" {
                if r.Method != http.MethodPost {
                        writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
                        return
                }
                s.restAddressScan(w, r, addrStr)
                return
        }

        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

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
                        isCoinbase := tx.IsCoinbase()
                        for j, out := range tx.Outputs {
                                if out.OneTimePub == spendPub || out.OneTimePub == expectedMintPub {
                                        results = append(results, AddressTx{
                                                TxHash:      txHash(i),
                                                BlockHeight: uint64(h),
                                                TxIndex:     i,
                                                OutputIndex: j,
                                                IsCoinbase:  isCoinbase,
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

// ─── POST /api/v1/address/{addr}/scan ────────────────────────────────────────
//
// View-key-assisted stealth scan: accepts the view key in the POST body (not the
// URL) so it is protected from server/proxy access-log exposure.  Returns the
// same UTXO shape as the /utxos endpoint plus amount_napr decoded for stealth
// outputs.  Treat this as an opt-in privacy trade-off: the view key is sent to
// the server but is NOT sufficient to spend funds (only the spend key can do that).

type scanRequest struct {
        ViewKeyHex string `json:"view_key_hex"`
}

func (s *Server) restAddressScan(w http.ResponseWriter, r *http.Request, addrStr string) {
        var req scanRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
                return
        }
        if req.ViewKeyHex == "" {
                writeJSONError(w, http.StatusBadRequest, "view_key_hex is required")
                return
        }
        // Pass the view key via X-View-Key header (not query string — query params
        // are logged by reverse proxies; F-046 security fix).  Clone the request
        // to avoid mutating shared state under concurrent calls.
        r2 := r.Clone(r.Context())
        r2.Method = http.MethodGet
        r2.Header.Set("X-View-Key", req.ViewKeyHex)
        s.restAddressUTXOs(w, r2, addrStr)
}

// ─── GET /api/v1/scan/outputs ────────────────────────────────────────────────
//
// Returns raw transaction outputs from recent blocks, including all fields
// needed for a client-side stealth scan (one_time_pub_hex, tx_pub_key_hex,
// enc_amount_hex).  The view key never leaves the client — callers iterate the
// returned outputs and call checkStealthOutput() locally.
//
// Query params:
//   from_height  (uint64, default 0)  — inclusive start block height
//   limit        (int, default 50, max 200) — max number of outputs to return
//
// Response:
//   { "outputs": [...], "from_height": N, "next_height": M, "tip_height": T }

type ScanOutput struct {
        TxHash          string `json:"tx_hash"`
        OutIdx          int    `json:"out_idx"`
        BlockHeight     uint64 `json:"block_height"`
        OneTimePubHex   string `json:"one_time_pub_hex"`
        TxPubKeyHex     string `json:"tx_pub_key_hex"`
        AmountCommitHex string `json:"amount_commit_hex"`
        EncAmountHex    string `json:"enc_amount_hex"`
        // IsCoinbase is true when this output belongs to the first (coinbase)
        // transaction in its block.  Wallet clients must filter these out so
        // per-block validator rewards do not appear in user history.
        IsCoinbase bool `json:"is_coinbase"`
}

func (s *Server) restScanOutputs(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        tip := s.chain.Tip()
        if tip == nil {
                writeJSON(w, http.StatusOK, map[string]interface{}{
                        "outputs":     []ScanOutput{},
                        "from_height": 0,
                        "next_height": 0,
                        "tip_height":  0,
                        "note":        "scan outputs locally with your view key; view key stays on device",
                })
                return
        }
        tipHeight := tip.Header.Height

        q := r.URL.Query()
        fromHeight := uint64(0)
        if fh := q.Get("from_height"); fh != "" {
                if n, err := strconv.ParseUint(fh, 10, 64); err == nil {
                        fromHeight = n
                }
        }
        limit := 50
        if l := q.Get("limit"); l != "" {
                if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
                        limit = n
                }
        }

        outputs := make([]ScanOutput, 0, limit)
        nextHeight := fromHeight
        for h := fromHeight; h <= tipHeight && len(outputs) < limit; h++ {
                b := s.chain.GetByHeight(h)
                if b == nil {
                        nextHeight = h + 1
                        continue
                }
                for txIdx, tx := range b.Txs {
                        // The first transaction in a block is the coinbase (block reward).
                        // Wallet clients must filter these so per-block validator rewards
                        // do not appear as +0 APRO entries in user history.
                        isCoinbase := txIdx == 0 && tx.IsCoinbase()
                        hash := tx.Hash()
                        hashHex := fmt.Sprintf("%x", hash[:])
                        for j, out := range tx.Outputs {
                                if len(outputs) >= limit {
                                        break
                                }
                                outputs = append(outputs, ScanOutput{
                                        TxHash:          hashHex,
                                        OutIdx:          j,
                                        BlockHeight:     h,
                                        OneTimePubHex:   fmt.Sprintf("%x", out.OneTimePub[:]),
                                        TxPubKeyHex:     fmt.Sprintf("%x", out.TxPubKey[:]),
                                        AmountCommitHex: fmt.Sprintf("%x", out.AmountCommit[:]),
                                        EncAmountHex:    fmt.Sprintf("%x", out.EncAmount[:]),
                                        IsCoinbase:      isCoinbase,
                                })
                        }
                }
                nextHeight = h + 1
        }

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "outputs":     outputs,
                "from_height": fromHeight,
                "next_height": nextHeight,
                "tip_height":  tipHeight,
                "note":        "scan these outputs locally with your view key; the view key never leaves your device",
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

// ─── GET /api/v1/network/whitelist ───────────────────────────────────────────
//
// Returns the current peer IP whitelist entries.
// Requires the whitelist-get function to be wired via SetWhitelistGetFunc.
//
// POST /api/v1/network/whitelist
//
// Adds one entry (IP or CIDR) to the live whitelist.  JSON body: {"entry":"1.2.3.4"}
//
// DELETE /api/v1/network/whitelist/:entry
//
// Removes one entry from the live whitelist.

// whitelistAddRequest is the JSON body for POST /api/v1/network/whitelist.
type whitelistAddRequest struct {
        Entry string `json:"entry"`
}

func (s *Server) restNetworkWhitelist(w http.ResponseWriter, r *http.Request) {
        switch r.Method {
        case http.MethodGet:
                if s.whitelistGetFn == nil {
                        writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                        return
                }
                entries := s.whitelistGetFn()
                if entries == nil {
                        entries = []string{}
                }
                writeJSON(w, http.StatusOK, map[string]interface{}{"entries": entries})

        case http.MethodPost:
                if s.whitelistAddFn == nil {
                        writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                        return
                }
                var req whitelistAddRequest
                if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                        writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
                        return
                }
                if req.Entry == "" {
                        writeJSONError(w, http.StatusBadRequest, "entry is required")
                        return
                }
                // Validate the IP/CIDR format here so we can return 400 (not 500) for
                // malformed entries; any error from whitelistAddFn after this point is a
                // persistence failure and deserves 500.
                if net.ParseIP(req.Entry) == nil {
                        if _, _, err := net.ParseCIDR(req.Entry); err != nil {
                                writeJSONError(w, http.StatusBadRequest,
                                        fmt.Sprintf("invalid IP or CIDR: %q", req.Entry))
                                return
                        }
                }
                if err := s.whitelistAddFn(req.Entry); err != nil {
                        writeJSONError(w, http.StatusInternalServerError,
                                "whitelist persist failed: "+err.Error())
                        return
                }
                writeJSON(w, http.StatusCreated, map[string]string{
                        "message": "entry added",
                        "entry":   req.Entry,
                })

        default:
                writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
        }
}

func (s *Server) restNetworkWhitelistByEntry(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodDelete {
                writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
                return
        }
        if s.whitelistRemoveFn == nil {
                writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                return
        }
        entry := pathSuffix("/api/v1/network/whitelist/", r.URL.Path)
        if entry == "" {
                writeJSONError(w, http.StatusBadRequest, "entry is required")
                return
        }
        removed, err := s.whitelistRemoveFn(entry)
        if err != nil {
                writeJSONError(w, http.StatusInternalServerError,
                        "whitelist persist failed: "+err.Error())
                return
        }
        if !removed {
                writeJSONError(w, http.StatusNotFound, "entry not found in whitelist")
                return
        }
        writeJSON(w, http.StatusOK, map[string]string{"message": "entry removed", "entry": entry})
}

// ─── GET /api/v1/network/whitelist-exemptions ─────────────────────────────────
//
// Returns whitelist-exemption events recorded by the P2P layer: each entry is a
// block that arrived from a whitelisted peer far ahead of the node's tip, so
// the automatic ban strike was skipped.  Accepts an optional `since` query
// parameter (Unix milliseconds) to fetch only events recorded after that time.
//
// Response: { "events": [ { ip, peer_addr, block_height, our_tip, at } ] }
func (s *Server) restNetworkWhitelistExemptions(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }
        if s.whitelistExemptFn == nil {
                writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                return
        }

        var since time.Time
        if raw := r.URL.Query().Get("since"); raw != "" {
                ms, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        writeJSONError(w, http.StatusBadRequest, "since must be a Unix-ms integer")
                        return
                }
                since = time.UnixMilli(ms)
        }

        events := s.whitelistExemptFn(since)
        if events == nil {
                events = []WhitelistExemptionEntry{}
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{"events": events})
}

// ─── GET /api/v1/network/ban-events ──────────────────────────────────────────
//
// Returns peer-ban events recorded by the P2P layer: each entry is a peer that
// was banned for sending repeated out-of-range (wrong-fork) blocks.  Accepts an
// optional `since` query parameter (Unix milliseconds) to fetch only events
// recorded after that time.
//
// Response: { "events": [ { ip, peer_addr, peer_id, reason, violations, ban_duration_secs, at } ] }
func (s *Server) restNetworkBanEvents(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }
        if s.banEventFn == nil {
                writeJSONError(w, http.StatusServiceUnavailable, "P2P layer not running")
                return
        }

        var since time.Time
        if raw := r.URL.Query().Get("since"); raw != "" {
                ms, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        writeJSONError(w, http.StatusBadRequest, "since must be a Unix-ms integer")
                        return
                }
                since = time.UnixMilli(ms)
        }

        events := s.banEventFn(since)
        if events == nil {
                events = []BanEventEntry{}
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{"events": events})
}

// ─── GET /api/v1/fee-estimate ────────────────────────────────────────────────
//
// Returns the current network base fee per byte and a pre-computed estimate for
// a typical 1-in-2-out RingCT transfer so wallets can show an accurate fee.
//
// Response:
//   base_fee_per_byte  uint64  nAPRO per serialised byte (current tip)
//   estimated_fee_napro uint64  fee for a typical 1-in, 2-out APRO transfer
//   estimated_fee_apro  float64 same value converted to APRO (÷ 1e8)
//   tx_size_bytes       int     estimated serialised size used for the above
func (s *Server) restFeeEstimate(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        // Read current base fee from tip block header.
        var baseFeePerByte uint64 = core.InitialBaseFeePerByte
        if tip := s.chain.Tip(); tip != nil && tip.Header.BaseFee > 0 {
                baseFeePerByte = tip.Header.BaseFee
        }
        if baseFeePerByte < core.MinBaseFeePerByte {
                baseFeePerByte = core.MinBaseFeePerByte
        }

        // Typical transfer: 1 input, 2 outputs (payment + change).
        const typicalInputs = 1
        const typicalOutputs = 2
        estimatedFeeNapro := core.ExportedEstimateFee(typicalInputs, typicalOutputs, baseFeePerByte)
        txSizeBytes := int(estimatedFeeNapro / baseFeePerByte)

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "base_fee_per_byte":   baseFeePerByte,
                "estimated_fee_napro": estimatedFeeNapro,
                "estimated_fee_apro":  float64(estimatedFeeNapro) / 1e8,
                "tx_size_bytes":       txSizeBytes,
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
                        "timestamp_rejected_count":   s.TimestampRejectedCount(),
                        "peer_count":                 s.livePeerCount(),
                        "pending_handshakes":          s.livePendingHandshakes(),
                })
                return
        }

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "height":                     0,
                "tip_hash":                   "",
                "tip_time":                   "",
                "total_txs":                  0,
                "mempool_count":              s.mempool.Count(),
                "tps_last_10":                0,
                "timestamp_rejected_count":   s.TimestampRejectedCount(),
                "peer_count":                 s.livePeerCount(),
                "pending_handshakes":          s.livePendingHandshakes(),
        })
}

// livePeerCount returns the current P2P peer count, or 0 if not wired.
func (s *Server) livePeerCount() int {
        if s.peerCounter == nil {
                return 0
        }
        return s.peerCounter()
}

// livePendingHandshakes returns the number of inbound connections currently in
// the TLS handshake phase, or 0 if the counter has not been wired (Task #504).
func (s *Server) livePendingHandshakes() int64 {
        if s.pendingHandshakeCounter == nil {
                return 0
        }
        return s.pendingHandshakeCounter()
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

// ─── POST /api/v1/admin/full-unstake ─────────────────────────────────────────

// restAdminFullUnstake builds a signed StakeWithdraw transaction (full exit)
// and submits it to the mempool.  The validator is immediately moved to the
// ValidatorUnbonding status (inactive, no rewards) and the entire stake is
// locked for UnbondingBlocks (144 000 blocks ≈ 10 days).
func (s *Server) restAdminFullUnstake(w http.ResponseWriter, r *http.Request) {
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

        // Accept an optional JSON body with pub_key for consistency with the
        // partial-unstake endpoint, but it must equal the local node's key.
        var req struct {
                PubKey string `json:"pub_key"`
        }
        _ = json.NewDecoder(r.Body).Decode(&req) // body is optional

        myPub := s.myKey.Public()
        myPubHex := fmt.Sprintf("%x", []byte(myPub))

        // If the caller supplied a pub_key, verify it matches this node.
        if req.PubKey != "" && req.PubKey != myPubHex {
                writeJSONError(w, http.StatusForbidden,
                        "this endpoint can only unstake the local node's own validator; connect to the target validator's node to initiate its withdrawal")
                return
        }

        entry, ok := s.registry.GetEntry(myPub)
        if !ok {
                writeJSONError(w, http.StatusNotFound, "validator not found in registry")
                return
        }
        totalNAPR := entry.StakeNAPR

        // StakeWithdraw uses amount=0 by convention (full exit).
        msg := core.StakeSignMsg(core.StakeWithdraw, myPub, 0)
        sig, err := s.myKey.Sign(msg)
        if err != nil {
                s.log.Error("admin full unstake: sign failed", "err", err)
                writeJSONError(w, http.StatusInternalServerError, "sign: "+err.Error())
                return
        }
        extra, err := core.EncodeStakeExtra(core.StakeWithdraw, myPub, 0, sig)
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
        s.log.Info("admin full unstake queued",
                "pub_key", myPubHex[:8],
                "total_napr", totalNAPR,
                "tx_hash", txHashHex,
        )

        currentHeight := uint64(0)
        if tip := s.chain.Tip(); tip != nil {
                currentHeight = tip.Header.Height
        }
        endBlock := currentHeight + core.UnbondingBlocks
        endEstimatedMs := time.Now().UnixMilli() + int64(core.UnbondingBlocks)*6000 // 6 s/block

        writeJSON(w, http.StatusCreated, map[string]interface{}{
                "tx_hash":          txHashHex,
                "pub_key":          myPubHex,
                "amount_napr":      totalNAPR,
                "amount_apr":       float64(totalNAPR) / 1e8,
                "end_block":        endBlock,
                "end_estimated_ms": endEstimatedMs,
                "status":           "pending",
                "message":          "StakeWithdraw transaction submitted to mempool; validator enters unbonding immediately",
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

// restUTXODecoys handles GET /api/v1/utxos/decoys?count=N
//
// Phase 2: returns N randomly-sampled UTXOs from the active UTXO set for use
// as ring decoys.  count defaults to 120 (8 inputs × 15 decoys) and is capped
// at 512 to prevent abuse.  The response is safe to cache briefly — callers
// should request fresh decoys for every transaction.
func (s *Server) restUTXODecoys(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        const defaultCount = 120
        const maxCount = 512
        count := defaultCount
        if raw := r.URL.Query().Get("count"); raw != "" {
                n, err := strconv.Atoi(raw)
                if err != nil || n <= 0 {
                        writeJSONError(w, http.StatusBadRequest, "count must be a positive integer")
                        return
                }
                if n > maxCount {
                        n = maxCount
                }
                count = n
        }

        decoys := s.utxos.SampleDecoys(count, nil)

        type decoyEntry struct {
                OneTimePubHex   string `json:"one_time_pub_hex"`
                AmountCommitHex string `json:"amount_commit_hex"`
        }
        entries := make([]decoyEntry, len(decoys))
        for i, d := range decoys {
                entries[i] = decoyEntry{
                        OneTimePubHex:   fmt.Sprintf("%x", d.OneTimePub[:]),
                        AmountCommitHex: fmt.Sprintf("%x", d.AmountCommit[:]),
                }
        }
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "decoys": entries,
                "count":  len(entries),
        })
}

func (s *Server) restUTXO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	// Path: /api/v1/utxo/{txhash}/{idx}
	tail := pathSuffix("/api/v1/utxo/", r.URL.Path)
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSONError(w, http.StatusBadRequest, "path must be /api/v1/utxo/{txhash}/{idx}")
		return
	}
	txHashHex := parts[0]
	idxStr := parts[1]

	txHashBytes, err := hex.DecodeString(txHashHex)
	if err != nil || len(txHashBytes) != 32 {
		writeJSONError(w, http.StatusBadRequest, "txhash must be 64 hex characters")
		return
	}
	var txHash crypto.Hash32
	copy(txHash[:], txHashBytes)

	idx64, err := strconv.ParseUint(idxStr, 10, 32)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "idx must be a non-negative integer")
		return
	}
	outIdx := uint32(idx64)

	utxo := s.utxos.Get(txHash, outIdx)

	// amountCommit and blockHeight are populated from whichever source has the UTXO.
	var amountCommit crypto.Commitment
	var blockHeight uint64
	found := false

	if utxo != nil {
		amountCommit = utxo.AmountCommit
		blockHeight = utxo.BlockHeight
		found = true
	} else if s.blockStore != nil {
		// Fallback: check the persistent u/ LevelDB store.  This covers UTXOs from
		// blocks that pre-date the current in-memory window (e.g. after a restart or
		// when the UTXOSet was rebuilt from a snapshot that omitted old entries).
		if su, suErr := s.blockStore.GetUTXO(txHash, outIdx); suErr == nil && su != nil {
			amountCommit = su.AmountCommit
			blockHeight = su.BlockHeight
			found = true
		}
	}

	if !found {
		writeJSONError(w, http.StatusNotFound, s.utxoMissingReason(txHash, outIdx))
		return
	}

	resp := map[string]interface{}{
		"tx_hash":           txHashHex,
		"out_idx":           outIdx,
		"amount_commit_hex": fmt.Sprintf("%x", amountCommit[:]),
		"exists":            true,
		"block_height":      blockHeight,
	}
	// Include block_timestamp when the source block is available.
	// Try the in-memory sliding window first; fall back to blockStore for
	// blocks older than the in-memory window (archive nodes and restarted nodes).
	if blk := s.chain.GetByHeight(blockHeight); blk != nil {
		resp["block_timestamp"] = time.Unix(0, blk.Header.Timestamp).UTC().Format(time.RFC3339)
	} else if s.blockStore != nil {
		heightStr := strconv.FormatUint(blockHeight, 10)
		fullBlk, prunedBlk, _ := s.lookupBlockFromDisk(heightStr)
		switch {
		case fullBlk != nil:
			resp["block_timestamp"] = time.Unix(0, fullBlk.Header.Timestamp).UTC().Format(time.RFC3339)
		case prunedBlk != nil:
			resp["block_timestamp"] = time.Unix(0, prunedBlk.Timestamp).UTC().Format(time.RFC3339)
		}
	}

	// In light-pruning mode, include blocks_until_pruned so the CLI can
	// reject stake attempts whose UTXO block will be pruned before unbonding
	// completes.  Rules:
	//   - tipHeight >= pruneAt: block is already at/past the prune boundary →
	//     report 0 so the CLI's < PartialUnbondingBlocks guard always fires.
	//   - tipHeight < pruneAt and blocksLeft ≤ max(keepBlocks/10,
	//     PartialUnbondingBlocks): report the exact remaining block count.
	//   - Otherwise (safely far): omit the field.
	// The field is never emitted on archive nodes or when keepBlocks is zero.
	if s.pruningMode == "light" && s.keepBlocks > 0 {
		if tip := s.chain.Tip(); tip != nil {
			tipHeight := tip.Header.Height
			// pruneAt is the tip height at which this UTXO's block is pruned.
			pruneAt := blockHeight + s.keepBlocks
			if tipHeight >= pruneAt {
				// At or past the prune boundary — report 0 so the CLI rejects.
				resp["blocks_until_pruned"] = uint64(0)
			} else {
				// threshold = max(10% of window, PartialUnbondingBlocks)
				threshold := s.keepBlocks / 10
				if threshold < core.PartialUnbondingBlocks {
					threshold = core.PartialUnbondingBlocks
				}
				blocksLeft := pruneAt - tipHeight
				if blocksLeft <= threshold {
					resp["blocks_until_pruned"] = blocksLeft
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
func (s *Server) restAdminStakeDeposit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if s.myKey == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "validator key not configured on this node")
		return
	}

	var req stakeDepositRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// ── Validate fields ────────────────────────────────────────────────────────
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

	// Only the local node's own key can be used — the signature is over (pub, amount, utxo)
	// and must verify against pub itself.
	myPub := s.myKey.Public()
	if !myPub.Equals(targetPub) {
		writeJSONError(w, http.StatusForbidden,
			"pub_key must match this node's configured validator key")
		return
	}

	txHashBytes, err := hex.DecodeString(req.UTXOTxHash)
	if err != nil || len(txHashBytes) != 32 {
		writeJSONError(w, http.StatusBadRequest, "utxo_txhash must be a 64-hex-char transaction hash")
		return
	}
	var burnTxHash crypto.Hash32
	copy(burnTxHash[:], txHashBytes)

	// ── Pre-flight UTXO existence check ───────────────────────────────────────
	// Verify the burn UTXO is present in the active set before signing and
	// encoding the payload.  In light-pruning mode a missing UTXO likely means
	// the originating block was stripped; return a descriptive error so the
	// operator knows to use an archive node or acquire a newer UTXO.
	burnUTXO := s.utxos.Get(burnTxHash, req.UTXOOutIdx)
	if burnUTXO == nil {
		writeJSONError(w, http.StatusUnprocessableEntity,
			s.utxoMissingReason(burnTxHash, req.UTXOOutIdx))
		return
	}

	// Resolve the blinding factor: stealth scan-recovered outputs pass
	// blind_hex="" and supply hs_scalar_hex so the node derives the blind
	// deterministically via DeterministicPaymentBlind(HsScalar, amount).
	var burnBlind crypto.BlindFactor
	if req.BlindHex == "" {
		// Stealth path: derive blind from ECDH shared secret + amount.
		if req.HsScalarHex == "" {
			writeJSONError(w, http.StatusBadRequest,
				"blind_hex is empty; supply hs_scalar_hex for stealth scan-recovered outputs")
			return
		}
		hsBytes, hsErr := hex.DecodeString(req.HsScalarHex)
		if hsErr != nil || len(hsBytes) != 32 {
			writeJSONError(w, http.StatusBadRequest, "hs_scalar_hex must be a 64-hex-char scalar")
			return
		}
		var hsScalar crypto.Scalar32
		copy(hsScalar[:], hsBytes)
		derivedBlind, bErr := crypto.DeterministicPaymentBlind(hsScalar, req.AmountNAPR)
		if bErr != nil {
			writeJSONError(w, http.StatusBadRequest, "derive blind from hs_scalar_hex: "+bErr.Error())
			return
		}
		burnBlind = derivedBlind
	} else {
		// Transparent path: caller supplies the explicit blinding factor.
		blindBytes, blindErr := hex.DecodeString(req.BlindHex)
		if blindErr != nil || len(blindBytes) != 32 {
			writeJSONError(w, http.StatusBadRequest, "blind_hex must be a 64-hex-char blinding factor")
			return
		}
		copy(burnBlind[:], blindBytes)
	}

	// ── Pre-mempool commitment check ───────────────────────────────────────────
	// Recompute Commit(amount, blind) and require it to equal the UTXO's
	// on-chain AmountCommit.  A mismatch means the caller supplied a wrong
	// amount or blinding factor; reject before the tx ever reaches the mempool.
	expectedCommit, commitErr := crypto.Commit(req.AmountNAPR, burnBlind)
	if commitErr != nil {
		writeJSONError(w, http.StatusBadRequest, "commitment computation failed: "+commitErr.Error())
		return
	}
	if expectedCommit != burnUTXO.AmountCommit {
		writeJSONError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("commitment mismatch: Commit(amount=%d, blind) does not equal the UTXO's on-chain AmountCommit — verify amount_napr and blind_hex", req.AmountNAPR))
		return
	}

	// ── Sign ───────────────────────────────────────────────────────────────────
	msg := core.StakeSignMsgV2(core.StakeDeposit, targetPub, req.AmountNAPR, burnTxHash, req.UTXOOutIdx)
	sig, err := s.myKey.Sign(msg)
	if err != nil {
		s.log.Error("admin stake deposit: sign failed", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "sign: "+err.Error())
		return
	}

	// ── Build v2 Extra payload ─────────────────────────────────────────────────
	extra, err := core.EncodeStakeExtraV2(
		core.StakeDeposit, targetPub, req.AmountNAPR, sig,
		burnTxHash, req.UTXOOutIdx, burnBlind,
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode extra: "+err.Error())
		return
	}

	// ── Submit to mempool ──────────────────────────────────────────────────────
	tx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extra,
	}
	if err := s.mempool.Add(tx); err != nil {
		writeJSONError(w, http.StatusBadRequest, "mempool: "+err.Error())
		return
	}

	txHash := tx.Hash()
	txHashHex := fmt.Sprintf("%x", txHash[:])
	s.log.Info("admin stake deposit queued",
		"pub_key", req.PubKey[:8],
		"amount_napr", req.AmountNAPR,
		"utxo_txhash", req.UTXOTxHash[:8],
		"tx_hash", txHashHex,
	)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"tx_hash":        txHashHex,
		"pub_key":        req.PubKey,
		"amount_napr":    req.AmountNAPR,
		"amount_apr":     float64(req.AmountNAPR) / 1e8,
		"utxo_txhash":    req.UTXOTxHash,
		"utxo_out_idx":   req.UTXOOutIdx,
		"status":         "pending",
		"message":        "StakeDeposit v2 transaction submitted to mempool; applied when included in the next block",
	})
}

// ─── POST /api/v1/admin/stake-deposit ────────────────────────────────────────

// stakeDepositRequest is the JSON body for POST /api/v1/admin/stake-deposit.
// The node signs the deposit on behalf of its own configured validator key.
type stakeDepositRequest struct {
        PubKey      string `json:"pub_key"`       // 64-hex-char Ed25519 public key
        AmountNAPR  uint64 `json:"amount_napr"`   // stake amount in nAPRO (base units)
        UTXOTxHash  string `json:"utxo_txhash"`   // 64-hex-char tx hash of the burn UTXO
        UTXOOutIdx  uint32 `json:"utxo_out_idx"`  // output index of the burn UTXO
        BlindHex    string `json:"blind_hex"`     // 64-hex-char Pedersen blinding factor; "" for stealth outputs
        HsScalarHex string `json:"hs_scalar_hex"` // optional: ECDH shared secret for stealth outputs; used when blind_hex is empty
}

// ─── POST /api/v1/stake ───────────────────────────────────────────────────────

// stakeBroadcastRequest is the JSON body for POST /api/v1/stake.
// The caller supplies a pre-signed 173-byte v2 stake payload (CLI-signed).
type stakeBroadcastRequest struct {
        TxExtraHex string `json:"tx_extra_hex"` // hex-encoded 173-byte v2 stake payload
}

// localOnly wraps an http.HandlerFunc with a DNS-rebinding guard.
// It rejects any request whose Host header is not the loopback address,
// preventing a malicious web page from POST-ing to admin endpoints via
// DNS rebinding (attacker.com resolves to 127.0.0.1, browser sends
// Host: attacker.com — the guard catches it).
func (s *Server) localOnly(next http.HandlerFunc) http.HandlerFunc {
        return func(w http.ResponseWriter, r *http.Request) {
                host := r.Host
                // Strip port if present.
                if h, _, err := net.SplitHostPort(host); err == nil {
                        host = h
                }
                if host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1" {
                        writeJSONError(w, http.StatusForbidden, "forbidden: admin endpoints are local-only")
                        return
                }
                next(w, r)
        }
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
// Requires X-API-Key when an API key is configured; also restricted to localhost
// by the localOnly() wrapper so it is never reachable from the open internet.
func (s *Server) restAdminMint(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
                return
        }
        // Authentication: require X-API-Key when the node has one configured.
        // Defense-in-depth on top of localOnly() — guards against SSRF and
        // any future proxy configuration that widens the loopback restriction.
        if s.apiKey != "" && r.Header.Get("X-API-Key") != s.apiKey {
                writeJSONError(w, http.StatusUnauthorized, "missing or invalid X-API-Key")
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
        if math.IsNaN(req.AmountAPR) || math.IsInf(req.AmountAPR, 0) || req.AmountAPR <= 0 || req.AmountAPR > 10_000_000_000 {
                writeJSONError(w, http.StatusBadRequest, "amount_apr must be > 0 and <= 10000000000")
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

// ─── GET /api/v1/network/mempool (Task #435) ─────────────────────────────────
//
// Returns live mempool metrics: pending transaction count, total bytes in the
// pool, and the configured capacity limits.  Operators can poll this endpoint
// to detect congestion or flood conditions without parsing the full stats page.

func (s *Server) restMempoolMetrics(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
                return
        }

        count      := s.mempool.Count()
        totalBytes := s.mempool.TotalBytes()
        cfg        := s.mempool.MempoolConfig()

        writeJSON(w, http.StatusOK, map[string]interface{}{
                "count":       count,
                "total_bytes": totalBytes,
                "max_size":    cfg.MaxSize,
                "max_bytes":   cfg.MaxBytes,
                "max_tx_size": cfg.MaxTxSize,
        })
}

// utxoMissingReason examines why a UTXO is absent from the active in-memory
// set and returns a human-readable, actionable error string.  Three cases are
// distinguished using the persisted LevelDB UTXO record:
//
//  1. Not in LevelDB at all → the UTXO was never created; the caller supplied
//     a wrong tx hash or output index.
//
//  2. In LevelDB and the originating block height is below the node's prune
//     cursor (light mode only) → the UTXO came from a block whose transaction
//     data was pruned; after a node restart the key-image / burn record may
//     have been lost.  The operator is directed to an archive node.
//
//  3. In LevelDB but the block is not pruned → the UTXO was already spent or
//     burned by a prior stake deposit.
//
// If the block store is not wired a generic "not found in active set" message
// is returned rather than guessing.
func (s *Server) utxoMissingReason(txHash crypto.Hash32, outIdx uint32) string {
        prefix := fmt.Sprintf("burn UTXO %x…:%d", txHash[:4], outIdx)

        if s.blockStore == nil {
                return prefix + " not found in active set"
        }

        su, err := s.blockStore.GetUTXO(txHash, outIdx)
        if err != nil || su == nil {
                // Never persisted → the caller supplied a non-existent reference.
                return prefix + " does not exist (unknown tx hash or output index)"
        }

        // UTXO exists in LevelDB.  Check whether its originating block was pruned.
        if s.pruningMode == "light" {
                cursorBytes, cerr := s.blockStore.GetMeta("prune_cursor")
                if cerr == nil && len(cursorBytes) == 8 {
                        pruneBelow := binary.LittleEndian.Uint64(cursorBytes)
                        if su.BlockHeight < pruneBelow {
                                return fmt.Sprintf(
                                        "%s originated at block %d whose transaction data has been pruned "+
                                                "(this node runs in light-pruning mode, pruned up to block %d); "+
                                                "connect to an archive node or acquire a UTXO from a more "+
                                                "recent block to stake",
                                        prefix, su.BlockHeight, pruneBelow)
                        }
                }
        }

        // Block is not pruned: the UTXO was already spent or burned by a prior
        // transaction (e.g. an earlier stake deposit).
        return fmt.Sprintf(
                "%s was already spent or burned (originated at block %d, "+
                        "no longer in the active UTXO set)",
                prefix, su.BlockHeight)
}

func (s *Server) restStakeBroadcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	var req stakeBroadcastRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	extraBytes, err := hex.DecodeString(req.TxExtraHex)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "tx_extra_hex is not valid hex: "+err.Error())
		return
	}
	if len(extraBytes) != core.StakePayloadSizeV2 {
		writeJSONError(w, http.StatusBadRequest,
			fmt.Sprintf("tx_extra_hex must decode to %d bytes (v2 stake payload), got %d",
				core.StakePayloadSizeV2, len(extraBytes)))
		return
	}

	// ── Pre-flight UTXO existence + commitment check ──────────────────────────
	// Decode the payload to extract the burn UTXO reference and blinding factor,
	// then verify the UTXO exists and that Commit(amount, blind) matches its
	// on-chain AmountCommit before handing the tx to the mempool.  A missing
	// UTXO in light-pruning mode produces a descriptive error rather than a
	// generic rejection.  A commitment mismatch is rejected with 422 so the
	// caller receives a structured error instead of the tx being silently queued.
	_, _, broadcastAmount, _, burnTxHash, burnOutIdx, broadcastBlind, decodeErr := core.DecodeStakeExtraV2(extraBytes)
	if decodeErr == nil {
		broadcastUTXO := s.utxos.Get(burnTxHash, burnOutIdx)
		if broadcastUTXO == nil {
			writeJSONError(w, http.StatusUnprocessableEntity,
				s.utxoMissingReason(burnTxHash, burnOutIdx))
			return
		}
		// Pre-mempool commitment check: Commit(amount, blind) must equal the UTXO's AmountCommit.
		broadcastCommit, broadcastCommitErr := crypto.Commit(broadcastAmount, broadcastBlind)
		if broadcastCommitErr != nil {
			writeJSONError(w, http.StatusBadRequest, "commitment computation failed: "+broadcastCommitErr.Error())
			return
		}
		if broadcastCommit != broadcastUTXO.AmountCommit {
			writeJSONError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("commitment mismatch: Commit(amount=%d, blind) does not equal the UTXO's on-chain AmountCommit — the declared amount or blinding factor is incorrect", broadcastAmount))
			return
		}
	}

	tx := core.Transaction{
		Version: core.TxVersionStake,
		Extra:   extraBytes,
	}
	if err := s.mempool.Add(tx); err != nil {
		writeJSONError(w, http.StatusBadRequest, "mempool: "+err.Error())
		return
	}

	txHash := tx.Hash()
	txHashHex := fmt.Sprintf("%x", txHash[:])
	s.log.Info("stake deposit broadcast", "tx_hash", txHashHex)

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"tx_hash": txHashHex,
		"status":  "pending",
		"message": "StakeDeposit v2 transaction submitted to mempool; applied when included in the next block",
	})
}

// ─── GET /api/v1/status ───────────────────────────────────────────────────────
//
// Lightweight liveness endpoint used by the systemd watchdog (aperod-node-watchdog.timer).
// Returns 200 {"ok":true,"height":N} as long as the HTTP server is responsive.
// No authentication required. Exempted from the per-IP rate-limit bucket — see
// rateLimitExempt in middleware.go — so watchdog probes can never be throttled.

func (s *Server) restStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	height := uint64(0)
	if tip := s.chain.Tip(); tip != nil {
		height = tip.Header.Height
	}
	syncing := atomic.LoadInt32(&s.syncing) == 1
	syncingHeight := atomic.LoadInt64(&s.syncingHeight)
	tipHeight := atomic.LoadInt64(&s.tipHeight)
	storeMissing := atomic.LoadInt64(&s.storeMissingBlocks)
	resp := map[string]interface{}{
		"ok":                   true,
		"height":               height,
		"syncing":              syncing,
		"syncing_height":       syncingHeight,
		"tip_height":           tipHeight,
		"utxo_rebuilding":      atomic.LoadInt32(&s.utxoRebuilding) == 1,
		"store_missing_blocks":      storeMissing,
		"store_missing_first_block": atomic.LoadInt64(&s.storeMissingFirstBlock),
		"store_missing_last_block":  atomic.LoadInt64(&s.storeMissingLastBlock),
	}
	// Use the live whitelist from the P2P layer when wired; fall back to the
	// startup snapshot so /api/v1/status is never stale after live edits.
	var wl []string
	if s.whitelistGetFn != nil {
		wl = s.whitelistGetFn()
	} else {
		wl = s.peerWhitelist
	}
	if len(wl) > 0 {
		resp["peer_whitelist"] = wl
	}
	// Staking pool status — included only when the pool feature is enabled.
	if s.stakingPoolFn != nil {
		remaining, init, mode := s.stakingPoolFn()
		if init > 0 {
			resp["staking_pool_remaining_napro"] = remaining
			resp["staking_pool_init_napro"] = init
			resp["reward_mode"] = mode
		}
	}
	// Snapshot status — lets the API server monitor snapshot freshness without
	// hitting the filesystem.  last_snapshot_saved_at is a Unix timestamp (seconds);
	// last_snapshot_error is non-empty only when the most recent save failed.
	s.snapshotMu.Lock()
	snapH       := s.lastSnapshotHeight
	snapAt      := s.lastSnapshotSavedAt
	snapErr     := s.lastSnapshotErrStr
	snapDurMs   := s.lastSnapshotSaveDurMs
	snapTimeout := s.lastSnapshotTimeoutSec
	s.snapshotMu.Unlock()
	resp["last_snapshot_height"] = snapH
	if !snapAt.IsZero() {
		resp["last_snapshot_saved_at"] = snapAt.Unix()
	}
	if snapErr != "" {
		resp["last_snapshot_error"] = snapErr
	}
	// Expose snapshot timing so the Admin Panel can display the timeout-ratio
	// risk indicator without requiring log access.
	if snapDurMs > 0 {
		resp["snapshot_save_duration_ms"] = snapDurMs
		if snapTimeout > 0 {
			ratio := float64(snapDurMs) / 1000.0 / snapTimeout * 100.0
			// Round to one decimal place for readability.
			resp["snapshot_timeout_sec"] = snapTimeout
			resp["snapshot_ratio_pct"] = math.Round(ratio*10) / 10
		}
	}
	writeJSON(w, http.StatusOK, resp)
}
