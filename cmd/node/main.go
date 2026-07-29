// Command node is the Aperod blockchain node process.
package main

import (
        "encoding/json"
        "fmt"
        "log/slog"
        "os"
        "os/signal"
        "path/filepath"
        "regexp"
        "syscall"
        "time"

        "github.com/aperod/aperod/api"
        "github.com/aperod/aperod/config"
        "github.com/aperod/aperod/consensus"
        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/p2p"
        "github.com/aperod/aperod/store"
)

func main() {
        if err := run(); err != nil {
                fmt.Fprintf(os.Stderr, "aperod-node: %v\n", err)
                os.Exit(1)
        }
}

// multiaddrRe matches /ip4/<host>/tcp/<port> and extracts host and port.
var multiaddrRe = regexp.MustCompile(`/ip4/([\d.]+)/tcp/(\d+)`)

// parseP2PAddr converts a multiaddr like "/ip4/0.0.0.0/tcp/30303" to "0.0.0.0:30303".
// Falls back to addr as-is if it doesn't match (allows plain "host:port" too).
func parseP2PAddr(addr string) string {
        if m := multiaddrRe.FindStringSubmatch(addr); len(m) == 3 {
                return m[1] + ":" + m[2]
        }
        return addr
}

// nodeHandler implements p2p.Handler using the consensus engine and in-memory chain.
type nodeHandler struct {
        engine *consensus.Engine
        chain  *core.Chain
        pool   *core.Mempool
        db     *store.DB
        log    *slog.Logger
}

func (h *nodeHandler) CurrentHeight() uint64 {
        return h.chain.Height()
}

func (h *nodeHandler) CurrentTailHashes(n int) []crypto.Hash32 {
        return h.chain.TailHashes(n)
}

func (h *nodeHandler) GetBlock(hash crypto.Hash32) *core.Block {
        return h.chain.GetByHash(hash)
}

// OnBlock forwards an externally received block to the consensus engine.
func (h *nodeHandler) OnBlock(block *core.Block) {
        select {
        case h.engine.NewBlockCh() <- block:
        default:
                h.log.Warn("p2p: incoming block channel full — dropped", "height", block.Header.Height)
        }
}

// OnTransaction adds an externally received transaction to the mempool.
func (h *nodeHandler) OnTransaction(tx *core.Transaction) {
        if err := h.pool.Add(*tx); err != nil {
                h.log.Debug("p2p: mempool rejected tx", "err", err)
        }
}

// OnVote forwards a p2p vote to the consensus engine.
func (h *nodeHandler) OnVote(vote p2p.VoteMsg) {
        var pub crypto.ValidatorPubKey
        copy(pub[:], vote.ValidatorPub)
        fm := consensus.FinalizeMsg{
                BlockHash:    vote.BlockHash,
                Height:       vote.Height,
                ValidatorPub: pub,
                Signature:    vote.Signature,
        }
        select {
        case h.engine.NewVoteCh() <- fm:
        default:
                h.log.Warn("p2p: vote channel full — dropped")
        }
}

func run() error {
        // ── 1. Load configuration ─────────────────────────────────────────────────
        cfgPath := "config/testnet.yaml"
        if len(os.Args) > 2 && os.Args[1] == "--config" {
                cfgPath = os.Args[2]
        }

        cfg, err := config.Load(cfgPath)
        if err != nil {
                return fmt.Errorf("load config: %w", err)
        }
        if err := cfg.Validate(); err != nil {
                return fmt.Errorf("invalid config: %w", err)
        }

        // ── 2. Setup logger ───────────────────────────────────────────────────────
        level := slog.LevelInfo
        switch cfg.LogLevel {
        case "debug":
                level = slog.LevelDebug
        case "warn":
                level = slog.LevelWarn
        case "error":
                level = slog.LevelError
        }
        log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
        slog.SetDefault(log)

        log.Info("starting Aperod node", "network", cfg.Network, "data_dir", cfg.DataDir)

        // ── 3. Open storage ───────────────────────────────────────────────────────
        if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
                return fmt.Errorf("create data dir: %w", err)
        }
        db, err := store.Open(cfg.DataDir + "/chain.db")
        if err != nil {
                return fmt.Errorf("open store: %w", err)
        }
        defer db.Close()

        // ── 4. Load or generate a persistent validator key ────────────────────────
        myKey, err := loadOrGenerateValidatorKey(cfg, log)
        if err != nil {
                return fmt.Errorf("validator key: %w", err)
        }

        // ── 5. Load genesis config ────────────────────────────────────────────────
        genesisConfig, err := core.LoadGenesis(cfg.Genesis.File)
        if err != nil {
                return fmt.Errorf("load genesis: %w", err)
        }

        // Override validator set with our own key so the node can always propose.
        validators := []crypto.ValidatorPubKey{myKey.Public()}

        // ── 6. Initialize chain ───────────────────────────────────────────────────
        chain := core.NewChain()
        mempool := core.NewMempool(core.DefaultMempoolConfig())

        // Create the UTXO set here (before chain loading) so the resume path
        // can populate it from stored blocks, ensuring historical spent key
        // images are known to TxVerifier before the first peer block arrives.
        utxos := core.NewUTXOSet()

        tipHash, tipHeight, err := db.GetTip()
        if err != nil {
                return fmt.Errorf("get tip: %w", err)
        }

        if tipHash == (crypto.Hash32{}) {
                log.Info("initializing genesis block", "chain_id", genesisConfig.ChainID)
                genesis, err := core.CreateGenesisBlock(genesisConfig, *myKey)
                if err != nil {
                        return fmt.Errorf("create genesis: %w", err)
                }
                if err := chain.SetGenesis(genesis); err != nil {
                        return fmt.Errorf("set genesis: %w", err)
                }
                if err := storeBlock(db, genesis); err != nil {
                        return fmt.Errorf("store genesis: %w", err)
                }
                if err := db.PutTip(genesis.Hash(), 0); err != nil {
                        return fmt.Errorf("store genesis tip: %w", err)
                }
                h := genesis.Hash()
                log.Info("genesis block created", "hash", fmt.Sprintf("%x", h[:8]))
        } else {
                log.Info("resuming from stored chain", "height", tipHeight, "tip", fmt.Sprintf("%x", tipHash[:8]))

                // Always load genesis (height 0).
                genesisRaw, err := db.GetRawBlockByHeight(0)
                if err != nil || genesisRaw == nil {
                        return fmt.Errorf("genesis block missing in store")
                }
                var genesisBlk core.Block
                if err := json.Unmarshal(genesisRaw, &genesisBlk); err != nil {
                        return fmt.Errorf("unmarshal genesis: %w", err)
                }
                if err := chain.SetGenesis(&genesisBlk); err != nil {
                        return fmt.Errorf("restore genesis: %w", err)
                }

                // Load only the most recent maxInMemoryBlocks blocks.
                // Older blocks are kept in SQLite; the in-memory window is bounded.
                const maxLoad = core.MaxInMemoryBlocks
                startLoad := uint64(1)
                if tipHeight >= maxLoad {
                        startLoad = tipHeight - maxLoad + 1
                }
                recentBlocks := make([]*core.Block, 0, maxLoad)
                for h := startLoad; h <= tipHeight; h++ {
                        raw, err := db.GetRawBlockByHeight(h)
                        if err != nil || raw == nil {
                                log.Warn("block missing in store during resume", "height", h)
                                break
                        }
                        var b core.Block
                        if err := json.Unmarshal(raw, &b); err != nil {
                                log.Warn("block unmarshal failed", "height", h, "err", err)
                                break
                        }
                        recentBlocks = append(recentBlocks, &b)
                }
                chain.FastForward(recentBlocks)
                log.Info("chain restored from storage",
                        "tip_height", tipHeight,
                        "blocks_loaded_in_memory", len(recentBlocks),
                )

                // Rebuild the spent key-image set from FULL chain history so that
                // TxVerifier.VerifyBlock can detect double-spends against any UTXO
                // ever created, not only those within the recent in-memory window.
                //
                // We scan every stored block and mark each input's key image spent
                // via MarkSpent (no IsSpent check — trusted store data).
                //
                // This is FAIL-CLOSED: any missing or corrupt block in the store
                // is a fatal error.  Continuing with a partial set would give false
                // confidence — the node would accept blocks that re-spend key images
                // from the uncovered height range, which is the exact vulnerability
                // this fix is meant to close.  Aborting is the only safe behaviour.
                log.Info("rebuilding spent key-image set from full chain history",
                        "tip_height", tipHeight)
                kiCount := 0
                for h := uint64(1); h <= tipHeight; h++ {
                        raw, fetchErr := db.GetRawBlockByHeight(h)
                        if fetchErr != nil || raw == nil {
                                return fmt.Errorf(
                                        "key-image rebuild failed: block at height %d missing from store (%v) — "+
                                                "node cannot start safely; repair the store and restart",
                                        h, fetchErr)
                        }
                        var b core.Block
                        if parseErr := json.Unmarshal(raw, &b); parseErr != nil {
                                return fmt.Errorf(
                                        "key-image rebuild failed: cannot decode block at height %d: %w — "+
                                                "node cannot start safely; repair the store and restart",
                                        h, parseErr)
                        }
                        for _, tx := range b.Txs {
                                for _, inp := range tx.Inputs {
                                        utxos.MarkSpent(inp.KeyImage)
                                        kiCount++
                                }
                        }
                }
                log.Info("spent key-image set rebuilt",
                        "key_images_marked", kiCount,
                        "blocks_scanned", tipHeight)
        }

        // initialTxTotal starts at 0; the counter accumulates from this run forward.
        // Historical total is not critical — the value is only shown in /network/stats.
        var initialTxTotal int64 = 0

        // ── 7. Setup consensus engine ─────────────────────────────────────────────
        // host and apiSrv are declared here so OnBlockProduced can reference them
        // (both are assigned after engine creation, but closures capture by reference).
        var host *p2p.Host
        var apiSrv *api.Server

        registry := core.NewValidatorRegistry()

        engine := consensus.NewEngine(consensus.Config{
                BlockTime:          cfg.Consensus.BlockTime,
                BFTThreshold:       genesisConfig.BFTThreshold,
                Validators:         validators,
                Registry:           registry,
                MyKey:              myKey,
                RewardAddress:      cfg.Consensus.RewardAddress,
                BlockRewardNAPR:    cfg.Consensus.BlockRewardNAPR,
                OracleURL:          cfg.Consensus.OracleURL,
                OracleMaxDeviation: cfg.Consensus.OracleMaxDeviation,
                OnBlockProduced: func(block *core.Block) {
                        if err := storeBlock(db, block); err != nil {
                                log.Error("failed to persist block", "height", block.Header.Height, "err", err)
                        } else {
                                hash := block.Hash()
                                if err := db.PutTip(hash, block.Header.Height); err != nil {
                                        log.Error("failed to update tip", "height", block.Header.Height, "err", err)
                                }
                        }
                        // Increment cached tx counter (skip index-0 coinbase reward).
                        if apiSrv != nil {
                                var delta int64
                                for txIdx, tx := range block.Txs {
                                        if txIdx == 0 && tx.IsCoinbase() {
                                                continue
                                        }
                                        delta++
                                }
                                if delta > 0 {
                                        apiSrv.AddTxCount(delta)
                                }
                        }
                        // Broadcast newly produced block to P2P peers (non-blocking).
                        if host != nil {
                                host.BroadcastBlock(block)
                        }
                        // Persist spent key images to the LevelDB key-image index so
                        // that future restarts can use db.IterKeyImages() instead of
                        // scanning every raw block.  Non-fatal on error — the block is
                        // already accepted; the index is a startup-performance optimisation.
                        for _, tx := range block.Txs {
                                for _, inp := range tx.Inputs {
                                        if kiErr := db.MarkKeyImageSpent(inp.KeyImage); kiErr != nil {
                                                log.Warn("failed to persist key image",
                                                        "height", block.Header.Height, "err", kiErr)
                                        }
                                }
                        }
                },
        }, chain, mempool, log)

        // ── 8. Start subsystems ───────────────────────────────────────────────────
        stop := make(chan struct{})
        go engine.Run(stop)

        // Drain ProducedCh so the consensus engine never blocks on a full channel.
        go func() {
                for {
                        select {
                        case <-stop:
                                return
                        case <-engine.ProducedCh():
                                // Consumed; broadcasting is handled in OnBlockProduced above.
                        }
                }
        }()

        // Wire full cryptographic tx verification into the consensus engine.
        // utxos was created and populated from stored blocks above.
        txVerifier := core.NewTxVerifier(utxos)
        engine.SetTxVerifier(txVerifier, utxos)

        if cfg.API.Enabled && cfg.API.ListenAddr != "" {
                apiSrv = api.NewServer(cfg.API.ListenAddr, chain, mempool, utxos, log)
                apiSrv.SetAllowedOrigins(cfg.API.CORS)
                apiSrv.SetRegistry(engine.Registry())
                apiSrv.SetValidatorKey(myKey)
                apiSrv.SetTxTotal(initialTxTotal)
                apiSrv.SetStore(db) // enables pruned-block fallback in the REST API
                go func() {
                        if err := apiSrv.Start(); err != nil {
                                log.Error("API server stopped", "err", err)
                        }
                }()
        }

        // ── 9. Start P2P networking ───────────────────────────────────────────────
        if cfg.P2P.ListenAddr != "" {
                tcpAddr := parseP2PAddr(cfg.P2P.ListenAddr)

                // Convert bootnode multiaddrs to plain TCP addrs.
                bootnodes := make([]string, 0, len(cfg.P2P.Bootnodes))
                for _, bn := range cfg.P2P.Bootnodes {
                        parsed := parseP2PAddr(bn)
                        // Skip self-connections (same port as our listener).
                        if parsed != tcpAddr {
                                bootnodes = append(bootnodes, parsed)
                        }
                }

                handler := &nodeHandler{
                        engine: engine,
                        chain:  chain,
                        pool:   mempool,
                        db:     db,
                        log:    log,
                }
                host = p2p.NewHost(p2p.Config{
                        ListenAddr: tcpAddr,
                        Bootnodes:  bootnodes,
                        MaxPeers:   cfg.P2P.MaxPeers,
                        MinPeers:   cfg.P2P.MinPeers,
                        NodeID:     myKey.Public().ID(),
                        UserAgent:  "aperod-node/1.0",
                }, handler, log)

                if err := host.Start(); err != nil {
                        log.Error("p2p failed to start", "err", err)
                        // Non-fatal: node runs standalone if P2P fails.
                } else {
                        log.Info("p2p started", "listen", tcpAddr, "bootnodes", len(bootnodes))
                        defer host.Stop()
                        if apiSrv != nil {
                                apiSrv.SetPeerCounter(host.PeerCount)
                        }
                }
        }

        log.Info("node is running",
                "validators", len(validators),
                "my_pub", myKey.Public().ID(),
                "api", cfg.API.ListenAddr,
                "p2p", cfg.P2P.ListenAddr,
        )

        // ── 10. Background pruning worker (light mode only) ────────────────────────
        // Strips RingCT/Bulletproof tx data from blocks older than KeepBlocks.
        // Fires once per epoch (~100 blocks × block_time).
        if cfg.Pruning.Mode == "light" {
                epochDur := cfg.Consensus.BlockTime * 100
                if epochDur <= 0 {
                        epochDur = 5 * 60 * 1000000000 // 5 min default
                }
                go func() {
                        t := time.NewTicker(epochDur)
                        defer t.Stop()
                        for {
                                select {
                                case <-stop:
                                        return
                                case <-t.C:
                                        n, err := chain.PruneOldData(db, cfg.Pruning.KeepBlocks)
                                        if err != nil {
                                                log.Warn("pruning error", "err", err)
                                        } else if n > 0 {
                                                log.Info("pruned old block tx data",
                                                        "blocks", n,
                                                        "keep_blocks", cfg.Pruning.KeepBlocks)
                                        }
                                }
                        }
                }()
                log.Info("light pruning enabled", "keep_blocks", cfg.Pruning.KeepBlocks)
        }

        // ── 11. Wait for signal ───────────────────────────────────────────────────
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
        <-sig

        log.Info("shutting down...")
        close(stop)
        return nil
}

// loadOrGenerateValidatorKey returns the node's validator private key.
func loadOrGenerateValidatorKey(cfg *config.Config, log *slog.Logger) (*crypto.ValidatorPrivKey, error) {
        if cfg.Consensus.ValidatorKey != "" {
                privBytes, err := os.ReadFile(cfg.Consensus.ValidatorKey)
                if err != nil {
                        return nil, fmt.Errorf("read validator key file: %w", err)
                }
                priv, err := crypto.ValidatorPrivKeyFromBytes(privBytes)
                if err != nil {
                        return nil, fmt.Errorf("parse validator private key: %w", err)
                }
                log.Info("loaded validator key from config", "pub", priv.Public().ID())
                return &priv, nil
        }

        keyPath := filepath.Join(cfg.DataDir, "validator.key")
        if data, err := os.ReadFile(keyPath); err == nil {
                priv, err := crypto.ValidatorPrivKeyFromBytes(data)
                if err != nil {
                        return nil, fmt.Errorf("parse persisted validator key: %w", err)
                }
                log.Info("loaded persisted validator key", "pub", priv.Public().ID())
                return &priv, nil
        }

        priv, _, err := crypto.GenerateValidatorKey()
        if err != nil {
                return nil, fmt.Errorf("generate validator key: %w", err)
        }
        if err := os.WriteFile(keyPath, priv.Bytes(), 0600); err != nil {
                return nil, fmt.Errorf("save validator key: %w", err)
        }
        log.Info("generated new validator key", "pub", priv.Public().ID(), "saved", keyPath)
        return &priv, nil
}

// storeBlock serialises a block to JSON and writes it via PutRawBlock.
func storeBlock(db *store.DB, b *core.Block) error {
        data, err := json.Marshal(b)
        if err != nil {
                return fmt.Errorf("marshal block: %w", err)
        }
        hash := b.Hash()
        return db.PutRawBlock(hash, b.Header.Height, data)
}
