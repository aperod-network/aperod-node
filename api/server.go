// Package api provides the Aperod JSON-RPC 2.0 API server.
// Methods follow the apr_ namespace convention.
package api

import (
        "encoding/hex"
        "encoding/json"
        "fmt"
        "log/slog"
        "net/http"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// Server is the JSON-RPC 2.0 HTTP server.
type Server struct {
        addr    string
        chain   *core.Chain
        mempool *core.Mempool
        utxos   *core.UTXOSet
        log     *slog.Logger
        mux     *http.ServeMux
}

// NewServer creates a new API server.
func NewServer(addr string, chain *core.Chain, mempool *core.Mempool, utxos *core.UTXOSet, log *slog.Logger) *Server {
        s := &Server{
                addr:    addr,
                chain:   chain,
                mempool: mempool,
                utxos:   utxos,
                log:     log,
                mux:     http.NewServeMux(),
        }
        s.registerRoutes()
        return s
}

func (s *Server) registerRoutes() {
        s.mux.HandleFunc("/", s.handleRPC)
        s.mux.HandleFunc("/health", s.handleHealth)
}

// ServeHTTP implements http.Handler so Server can be used with httptest.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
        s.mux.ServeHTTP(w, r)
}

// Start binds and serves. Blocks until server returns.
func (s *Server) Start() error {
        srv := &http.Server{
                Addr:         s.addr,
                Handler:      s.mux,
                ReadTimeout:  10 * time.Second,
                WriteTimeout: 10 * time.Second,
                IdleTimeout:  60 * time.Second,
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
        Hash         string               `json:"hash"`
        Height       uint64               `json:"height"`
        PrevHash     string               `json:"prev_hash"`
        MerkleRoot   string               `json:"merkle_root"`
        Timestamp    string               `json:"timestamp"`
        Round        uint32               `json:"round"`
        ValidatorPub string               `json:"validator_pub"`
        TxCount      int                  `json:"tx_count"`
        Size         int                  `json:"size"`
}

func blockToResponse(b *core.Block) BlockResponse {
        h := b.Hash()
        return BlockResponse{
                Hash:         fmt.Sprintf("%x", h[:]),
                Height:       b.Header.Height,
                PrevHash:     fmt.Sprintf("%x", b.Header.PrevHash[:]),
                MerkleRoot:   fmt.Sprintf("%x", b.Header.MerkleRoot[:]),
                Timestamp:    time.Unix(0, b.Header.Timestamp).UTC().Format(time.RFC3339),
                Round:        b.Header.Round,
                ValidatorPub: b.Header.ValidatorPub.Hex(),
                TxCount:      len(b.Txs),
                Size:         b.Size(),
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
                Tx json.RawMessage `json:"tx"`
        }
        if err := json.Unmarshal(params, &args); err != nil {
                return nil, fmt.Errorf("invalid params: %w", err)
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
                "unit":    "nAPR",
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

// ─── helpers ──────────────────────────────────────────────────────────────────

func (s *Server) writeError(w http.ResponseWriter, id interface{}, code int, msg string) {
        json.NewEncoder(w).Encode(rpcResponse{
                JSONRPC: "2.0",
                ID:      id,
                Error:   &rpcError{Code: code, Message: msg},
        })
}
