// Package api provides the Aperod JSON-RPC 2.0 API server.
// Methods follow the apr_ namespace convention.
package api

import (
        "encoding/hex"
        "encoding/json"
        "fmt"
        "log/slog"
        "net/http"
        "sync/atomic"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
)

// Server is the JSON-RPC 2.0 HTTP server.
type Server struct {
        addr        string
        chain       *core.Chain
        mempool     *core.Mempool
        utxos       *core.UTXOSet
        registry    *core.ValidatorRegistry  // live PoS validator registry (optional)
        myKey       *crypto.ValidatorPrivKey // node's own validator key for admin stake ops (optional)
        blockStore  *store.DB               // optional: LevelDB store for pruned-block fallback
        log         *slog.Logger
        mux         *http.ServeMux
        hub         *Hub
        apiKey      string   // optional; empty = dev mode (no auth)
        corsOrigins []string // empty = allow all ("*")
        rateLimiter *RateLimiter
        peerCounter func() int // optional; wired to p2p.Host.PeerCount by cmd/node

        // txTotal is an O(1) cached total non-coinbase tx count.
        // Updated atomically so no lock is needed in hot paths.
        txTotal int64
}

// NewServer creates a new API server.
func NewServer(addr string, chain *core.Chain, mempool *core.Mempool, utxos *core.UTXOSet, log *slog.Logger) *Server {
        s := &Server{
                addr:        addr,
                chain:       chain,
                mempool:     mempool,
                utxos:       utxos,
                log:         log,
                mux:         http.NewServeMux(),
                hub:         NewHub(log),
                rateLimiter: NewRateLimiter(),
        }
        s.registerRoutes()
        return s
}

// SetRegistry wires the live PoS validator registry so the API can serve
// /api/v1/validators and include validator_count in network stats.
func (s *Server) SetRegistry(r *core.ValidatorRegistry) { s.registry = r }

// SetValidatorKey provides the node's own validator private key to the API
// server so the /api/v1/admin/partial-unstake endpoint can create properly
// signed StakeAdminWithdraw transactions.  Optional — endpoint returns 503
// when no key is configured.
func (s *Server) SetValidatorKey(key *crypto.ValidatorPrivKey) { s.myKey = key }

// APIKeyConfig optionally sets the required API key for write operations.
// Call before Start(). Empty string disables key enforcement (dev mode).
func (s *Server) SetAPIKey(key string) { s.apiKey = key }

// SetAllowedOrigins configures the CORS origin whitelist.
// Empty slice allows all origins ("*").
func (s *Server) SetAllowedOrigins(origins []string) { s.corsOrigins = origins }

// SetStore wires the LevelDB block store so the API can fall back to disk
// when looking up old or pruned blocks that have been evicted from memory.
// Optional — endpoints return 404 for old blocks when no store is wired.
func (s *Server) SetStore(db *store.DB) { s.blockStore = db }

// SetPeerCounter wires a function returning the live P2P peer count so
// /metrics can report it. Optional — /metrics reports 0 peers if unset.
func (s *Server) SetPeerCounter(f func() int) { s.peerCounter = f }

// SetTxTotal sets the initial total non-coinbase tx count (call once after
// loading the chain from disk to avoid an O(n) scan on every stats request).
func (s *Server) SetTxTotal(n int64) { atomic.StoreInt64(&s.txTotal, n) }

// AddTxCount increments the cached tx counter by delta (call from OnBlockProduced).
func (s *Server) AddTxCount(delta int64) { atomic.AddInt64(&s.txTotal, delta) }

// TxTotal returns the current cached total non-coinbase tx count.
func (s *Server) TxTotal() int64 { return atomic.LoadInt64(&s.txTotal) }

// Hub returns the WebSocket hub (for node to push events).
func (s *Server) Hub() *Hub { return s.hub }

func (s *Server) registerRoutes() {
        s.mux.HandleFunc("/", s.handleRPC)
        s.mux.HandleFunc("/health", s.handleHealth)
        s.mux.HandleFunc("/metrics", s.handleMetrics)
        s.mux.Handle("/ws", s.hub.Handler())
        s.registerRESTRoutes()
}

// ServeHTTP implements http.Handler so Server can be used with httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
        s.mux.ServeHTTP(w, r)
}

// Start binds and serves. Blocks until server returns.
// The full middleware chain is: CORS → RateLimit → routes.
func (s *Server) Start() error {
        cors := CORSConfig{AllowedOrigins: s.corsOrigins}
        handler := cors.Middleware(s.rateLimiter.Middleware(s.mux))
        srv := &http.Server{
                Addr:         s.addr,
                Handler:      handler,
                ReadTimeout:  15 * time.Second,
                WriteTimeout: 120 * time.Second,
                IdleTimeout:  120 * time.Second,
        }
        s.log.Info("API server listening", "addr", s.addr)
        return srv.ListenAndServe()
}

// ─── JSON-RPC 2.0 types ───────────────────────────────────────────────────────

type rpcRequest struct {
        JSONRPC string          `json:"jsonrpc"`
        ID      interface{}     `json:"id"`
        Method  string          `json:"method"`
        Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
        JSONRPC string      `json:"jsonrpc"`
        ID      interface{} `json:"id"`
        Result  interface{} `json:"result,omitempty"`
        Error   *rpcError   `json:"error,omitempty"`
}

type rpcError struct {
        Code    int    `json:"code"`
        Message string `json:"message"`
}

const (
        errCodeParse   = -32700
        errCodeInvalid = -32600
        errCodeMethod  = -32601
        errCodeParams  = -32602
        errCodeInternal = -32603
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]interface{}{
                "status": "ok",
                "height": s.chain.Height(),
                "time":   time.Now().UTC().Format(time.RFC3339),
        })
}

// handleMetrics exposes a minimal Prometheus text-format snapshot of node
// health for scraping. No external client library is used — the format is
// simple enough to hand-write and keeps the Go module dependency-free.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
        peers := 0
        if s.peerCounter != nil {
                peers = s.peerCounter()
        }
        activeValidators, totalValidators := 0, 0
        if s.registry != nil {
                activeValidators, totalValidators = s.registry.Count()
        }

        w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

        fmt.Fprintf(w, "# HELP aperod_chain_height Current chain tip height.\n")
        fmt.Fprintf(w, "# TYPE aperod_chain_height gauge\n")
        fmt.Fprintf(w, "aperod_chain_height %d\n", s.chain.Height())

        fmt.Fprintf(w, "# HELP aperod_mempool_size Number of transactions currently in the mempool.\n")
        fmt.Fprintf(w, "# TYPE aperod_mempool_size gauge\n")
        fmt.Fprintf(w, "aperod_mempool_size %d\n", s.mempool.Count())

        fmt.Fprintf(w, "# HELP aperod_utxo_count Number of unspent outputs tracked in memory.\n")
        fmt.Fprintf(w, "# TYPE aperod_utxo_count gauge\n")
        fmt.Fprintf(w, "aperod_utxo_count %d\n", s.utxos.Count())

        fmt.Fprintf(w, "# HELP aperod_peer_count Number of connected P2P peers.\n")
        fmt.Fprintf(w, "# TYPE aperod_peer_count gauge\n")
        fmt.Fprintf(w, "aperod_peer_count %d\n", peers)

        fmt.Fprintf(w, "# HELP aperod_validator_count_active Number of currently active PoA/PoS validators.\n")
        fmt.Fprintf(w, "# TYPE aperod_validator_count_active gauge\n")
        fmt.Fprintf(w, "aperod_validator_count_active %d\n", activeValidators)

        fmt.Fprintf(w, "# HELP aperod_validator_count_total Total number of registered PoA/PoS validators.\n")
        fmt.Fprintf(w, "# TYPE aperod_validator_count_total gauge\n")
        fmt.Fprintf(w, "aperod_validator_count_total %d\n", totalValidators)

        fmt.Fprintf(w, "# HELP aperod_up Whether the API server is reachable (always 1 when scraped successfully).\n")
        fmt.Fprintf(w, "# TYPE aperod_up gauge\n")
        fmt.Fprintf(w, "aperod_up 1\n")
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        // CORS
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
        if r.Method == http.MethodOptions {
                w.WriteHeader(http.StatusOK)
                return
        }
        if r.Method != http.MethodPost {
                s.writeError(w, nil, errCodeInvalid, "only POST is supported")
                return
        }

        var req rpcRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                s.writeError(w, nil, errCodeParse, "parse error: "+err.Error())
                return
        }
        if req.JSONRPC != "2.0" {
                s.writeError(w, req.ID, errCodeInvalid, "jsonrpc must be '2.0'")
                return
        }

        result, err := s.dispatch(req.Method, req.Params)
        if err != nil {
                s.writeError(w, req.ID, errCodeInternal, err.Error())
                return
        }
        json.NewEncoder(w).Encode(rpcResponse{
                JSONRPC: "2.0",
                ID:      req.ID,
                Result:  result,
        })
}

func (s *Server) dispatch(method string, params json.RawMessage) (interface{}, error) {
        switch method {
        case "apr_getNodeInfo":
                return s.aprGetNodeInfo()
        case "apr_getBlockByHeight":
                return s.aprGetBlockByHeight(params)
        case "apr_getBlockByHash":
                return s.aprGetBlockByHash(params)
        case "apr_getTransaction":
                return s.aprGetTransaction(params)
        case "apr_getMempoolInfo":
                return s.aprGetMempoolInfo()
        case "apr_getMempoolTxs":
                return s.aprGetMempoolTxs()
        case "apr_sendRawTransaction":
                return s.aprSendRawTransaction(params)
        case "apr_getBalance":
                return s.aprGetBalance(params)
        case "apr_validateAddress":
                return s.aprValidateAddress(params)
        case "apr_estimateFee":
                return s.aprEstimateFee(params)
        case "apr_walletSend":
                return s.aprWalletSend(params)
        case "apr_scanUTXOs":
                return s.aprScanUTXOs(params)
        default:
                return nil, fmt.Errorf("method not found: %s", method)
        }
}

// ─── RPC Methods ──────────────────────────────────────────────────────────────

// NodeInfo is the response for apr_getNodeInfo.
type NodeInfo struct {
        ChainID   string `json:"chain_id"`
        Height    uint64 `json:"height"`
        TipHash   string `json:"tip_hash"`
        Timestamp string `json:"timestamp"`
        Mempool   int    `json:"mempool_count"`
        Version   string `json:"version"`
}

func (s *Server) aprGetNodeInfo() (interface{}, error) {
        tip := s.chain.Tip()
        if tip == nil {
                return nil, fmt.Errorf("chain not initialized")
        }
        tipHash := tip.Hash()
        return NodeInfo{
                ChainID:   "aperod",
                Height:    tip.Header.Height,
                TipHash:   fmt.Sprintf("%x", tipHash[:]),
                Timestamp: time.Unix(0, tip.Header.Timestamp).UTC().Format(time.RFC3339),
                Mempool:   s.mempool.Count(),
                Version:   "0.1.0",
        }, nil
}

// BlockResponse is returned by block-fetching methods.
type BlockResponse struct {
        Hash         string `json:"hash"`
        Height       uint64 `json:"height"`
        PrevHash     string `json:"prev_hash"`
        MerkleRoot   string `json:"merkle_root"`
        Timestamp    string `json:"timestamp"`
        Round        uint32 `json:"round"`
        ValidatorPub string `json:"validator_pub"`
        TxCount      int    `json:"tx_count"`
        Size         int    `json:"size"`
        // OraclePrice is the APRO/USD price embedded by the validator,
        // expressed as USD-per-APRO × 10^9 (9-decimal fixed-point uint64).
        // Zero means no price was embedded (pre-oracle or non-oracle block).
        OraclePrice uint64 `json:"oracle_price"`
        // FeesBurnedNAPRO is the sum of fees from all non-coinbase transactions
        // in this block, expressed in nAPRO. All base fees are burned (100%).
        FeesBurnedNAPRO uint64 `json:"fees_burned_napro"`
}

func blockToResponse(b *core.Block) BlockResponse {
        h := b.Hash()
        baseFee := b.Header.BaseFee
        if baseFee == 0 {
                baseFee = core.InitialBaseFeePerByte
        }
        var burned uint64
        for i := range b.Txs {
                tx := &b.Txs[i]
                if tx.IsCoinbase() || tx.IsStake() {
                        continue
                }
                minFee := tx.MinFeeAt(baseFee)
                if tx.Fee < minFee {
                        burned += tx.Fee
                } else {
                        burned += minFee
                }
        }
        return BlockResponse{
                Hash:            fmt.Sprintf("%x", h[:]),
                Height:          b.Header.Height,
                PrevHash:        fmt.Sprintf("%x", b.Header.PrevHash[:]),
                MerkleRoot:      fmt.Sprintf("%x", b.Header.MerkleRoot[:]),
                Timestamp:       time.Unix(0, b.Header.Timestamp).UTC().Format(time.RFC3339),
                Round:           b.Header.Round,
                ValidatorPub:    b.Header.ValidatorPub.Hex(),
                TxCount:         len(b.Txs),
                Size:            b.Size(),
                OraclePrice:     b.Header.OraclePrice,
                FeesBurnedNAPRO: burned,
        }
}

func (s *Server) aprGetBlockByHeight(params json.RawMessage) (interface{}, error) {
        var args struct {
                Height uint64 `json:"height"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        block := s.chain.GetByHeight(args.Height)
        if block == nil {
                return nil, fmt.Errorf("block not found at height %d", args.Height)
        }
        return blockToResponse(block), nil
}

func (s *Server) aprGetBlockByHash(params json.RawMessage) (interface{}, error) {
        var args struct {
                Hash string `json:"hash"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        b, err := hex.DecodeString(args.Hash)
        if err != nil || len(b) != 32 {
                return nil, fmt.Errorf("invalid hash: must be 64 hex chars")
        }
        var hash crypto.Hash32
        copy(hash[:], b)
        block := s.chain.GetByHash(hash)
        if block == nil {
                return nil, fmt.Errorf("block not found: %s", args.Hash[:16])
        }
        return blockToResponse(block), nil
}

func (s *Server) aprGetMempoolInfo() (interface{}, error) {
        hashes := s.mempool.Hashes()
        return map[string]interface{}{
                "count": len(hashes),
        }, nil
}

func (s *Server) aprGetMempoolTxs() (interface{}, error) {
        hashes := s.mempool.Hashes()
        out := make([]string, len(hashes))
        for i, h := range hashes {
                out[i] = fmt.Sprintf("%x", h[:])
        }
        return out, nil
}

func (s *Server) aprSendRawTransaction(params json.RawMessage) (interface{}, error) {
        var args struct {
                Tx     json.RawMessage `json:"tx"`
                APIKey string          `json:"api_key"` // alternative to X-API-Key header
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        // Check API key when one is configured
        if s.apiKey != "" && args.APIKey != s.apiKey {
                return nil, fmt.Errorf("unauthorized: missing or invalid api_key")
        }
        var tx core.Transaction
        if err := json.Unmarshal(args.Tx, &tx); err != nil {
                return nil, fmt.Errorf("decode tx: %w", err)
        }
        if err := s.mempool.Add(tx); err != nil {
                return nil, fmt.Errorf("rejected: %w", err)
        }
        hash := tx.Hash()
        return map[string]string{"hash": fmt.Sprintf("%x", hash[:])}, nil
}

func (s *Server) aprGetBalance(params json.RawMessage) (interface{}, error) {
        var args struct {
                Address  string `json:"address"`
                ViewKey  string `json:"view_key"` // hex-encoded view private scalar
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        addr := crypto.Address(args.Address)
        if err := crypto.Validate(addr); err != nil {
                return nil, fmt.Errorf("invalid address: %w", err)
        }

        // If view key provided, scan for balance; otherwise report 0 (privacy model)
        balance := uint64(0)
        if args.ViewKey != "" {
                // TODO Phase 2: instantiate WalletScanner and scan chain
        }

        return map[string]interface{}{
                "address": args.Address,
                "balance": balance,
                "unit":    "nAPRO",
        }, nil
}

func (s *Server) aprValidateAddress(params json.RawMessage) (interface{}, error) {
        var args struct {
                Address string `json:"address"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        addr := crypto.Address(args.Address)
        err := crypto.Validate(addr)
        valid := err == nil
        result := map[string]interface{}{
                "address": args.Address,
                "valid":   valid,
        }
        if !valid {
                result["error"] = err.Error()
        }
        return result, nil
}

// ─── apr_getTransaction (2.1.4) ───────────────────────────────────────────────

// TxResponse is returned by apr_getTransaction.
type TxResponse struct {
        Hash        string `json:"hash"`
        BlockHash   string `json:"block_hash"`
        BlockHeight uint64 `json:"block_height"`
        TxIndex     int    `json:"tx_index"`
        IsCoinbase  bool   `json:"is_coinbase"`
        Inputs      int    `json:"inputs"`
        Outputs     int    `json:"outputs"`
        Fee         uint64 `json:"fee"`
        Size        int    `json:"size"`
        Version     uint8  `json:"version"`
        // Pending is true when the tx is in the mempool but not yet confirmed.
        Pending bool `json:"pending,omitempty"`
}

func (s *Server) aprGetTransaction(params json.RawMessage) (interface{}, error) {
        var args struct {
                Hash string `json:"hash"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
        }
        b, err := hex.DecodeString(args.Hash)
        if err != nil || len(b) != 32 {
                return nil, fmt.Errorf("invalid hash: must be 64 hex chars")
        }
        var hash crypto.Hash32
        copy(hash[:], b)

        // Search confirmed chain first
        tx, loc, ok := s.chain.GetTransaction(hash)
        if ok {
                bHash := loc.Block.Hash()
                return TxResponse{
                        Hash:        args.Hash,
                        BlockHash:   fmt.Sprintf("%x", bHash[:]),
                        BlockHeight: loc.Block.Header.Height,
                        TxIndex:     loc.TxIndex,
                        IsCoinbase:  tx.IsCoinbase(),
                        Inputs:      len(tx.Inputs),
                        Outputs:     len(tx.Outputs),
                        Fee:         tx.Fee,
                        Size:        tx.Size(),
                        Version:     uint8(tx.Version),
                }, nil
        }

        // Check mempool for unconfirmed tx
        if mp, found := s.mempool.Get(hash); found {
                return TxResponse{
                        Hash:       args.Hash,
                        IsCoinbase: mp.IsCoinbase(),
                        Inputs:     len(mp.Inputs),
                        Outputs:    len(mp.Outputs),
                        Fee:        mp.Fee,
                        Size:       mp.Size(),
                        Version:    uint8(mp.Version),
                        Pending:    true,
                }, nil
        }

        return nil, fmt.Errorf("transaction not found: %s", args.Hash[:16])
}

// ─── apr_estimateFee (2.1.9) ─────────────────────────────────────────────────

func (s *Server) aprEstimateFee(params json.RawMessage) (interface{}, error) {
        var args struct {
                // SizeBytes is the estimated serialized transaction size.
                // If omitted, returns the minimum fee for a typical RingCT tx.
                SizeBytes int `json:"size_bytes"`
        }
        // params may be null — tolerate unmarshal failure
        _ = json.Unmarshal(params, &args)

        // Dynamic EIP-1559 fee: baseFeePerByte × tx_size_bytes.
        // Return the current InitialBaseFeePerByte as a reference rate.
        // Callers should multiply by their actual tx size; use /estimate_fee RPC for precision.
        sizeBytes := args.SizeBytes
        if sizeBytes == 0 {
                sizeBytes = 2000 // default: typical P2P transfer ~2 KB
        }
        estimatedFee := uint64(sizeBytes) * core.InitialBaseFeePerByte
        return map[string]interface{}{
                "fee":                estimatedFee,
                "base_fee_per_byte":  core.InitialBaseFeePerByte,
                "size_bytes":         sizeBytes,
                "unit":               "nAPRO",
                "flat":               false,
                "model":              "size_based_eip1559",
        }, nil
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func (s *Server) writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
        json.NewEncoder(w).Encode(rpcResponse{
                JSONRPC: "2.0",
                ID:      id,
                Error:   &rpcError{Code: code, Message: msg},
        })
}
