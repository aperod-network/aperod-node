// Command node is the Aperod blockchain node process.
package main

import (
        "encoding/json"
        "fmt"
        "log/slog"
        "os"
        "os/signal"
        "path/filepath"
        "syscall"

        "github.com/aperod/aperod/api"
        "github.com/aperod/aperod/config"
        "github.com/aperod/aperod/consensus"
        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
)

func main() {
        if err := run(); err != nil {
                fmt.Fprintf(os.Stderr, "aperod-node: %v\n", err)
                os.Exit(1)
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
        // If no key path is configured, auto-generate one and persist it so the
        // same key (and thus the same genesis) is reused across restarts.
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
        // This makes the testnet work out of the box without manual key config.
        validators := []crypto.ValidatorPubKey{myKey.Public()}

        // ── 6. Initialize chain ───────────────────────────────────────────────────
        chain := core.NewChain()
        mempool := core.NewMempool(core.DefaultMempoolConfig())

        tipHash, tipHeight, err := db.GetTip()
        if err != nil {
                return fmt.Errorf("get tip: %w", err)
        }

        if tipHash == (crypto.Hash32{}) {
                // Fresh node: create and persist genesis block
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
                // Resume: load blocks from DB
                log.Info("resuming from stored chain", "height", tipHeight, "tip", fmt.Sprintf("%x", tipHash[:8]))
                loaded := uint64(0)
                for h := uint64(0); h <= tipHeight; h++ {
                        raw, err := db.GetRawBlockByHeight(h)
                        if err != nil || raw == nil {
                                log.Warn("block missing in store, stopping resume", "height", h)
                                break
                        }
                        var b core.Block
                        if err := json.Unmarshal(raw, &b); err != nil {
                                log.Warn("block unmarshal failed", "height", h, "err", err)
                                break
                        }
                        if h == 0 {
                                if err := chain.SetGenesis(&b); err != nil {
                                        return fmt.Errorf("restore genesis: %w", err)
                                }
                        } else {
                                if err := chain.AddBlock(&b); err != nil {
                                        log.Warn("resume: add block failed", "height", h, "err", err)
                                        break
                                }
                        }
                        loaded++
                }
                log.Info("chain restored from storage", "blocks_loaded", loaded)
        }

        // ── 7. Setup consensus engine ─────────────────────────────────────────────
        engine := consensus.NewEngine(consensus.Config{
                BlockTime:    cfg.Consensus.BlockTime,
                BFTThreshold: genesisConfig.BFTThreshold,
                Validators:   validators,
                MyKey:        myKey,
                OnBlockProduced: func(block *core.Block) {
                        if err := storeBlock(db, block); err != nil {
                                log.Error("failed to persist block", "height", block.Header.Height, "err", err)
                        } else {
                                hash := block.Hash()
                                if err := db.PutTip(hash, block.Header.Height); err != nil {
                                        log.Error("failed to update tip", "height", block.Header.Height, "err", err)
                                }
                        }
                },
        }, chain, mempool, log)

        // ── 8. Start subsystems ───────────────────────────────────────────────────
        stop := make(chan struct{})
        go engine.Run(stop)

        utxos := core.NewUTXOSet()
        if cfg.API.Enabled && cfg.API.ListenAddr != "" {
                apiSrv := api.NewServer(cfg.API.ListenAddr, chain, mempool, utxos, log)
                apiSrv.SetAllowedOrigins(cfg.API.CORS)
                go func() {
                        if err := apiSrv.Start(); err != nil {
                                log.Error("API server stopped", "err", err)
                        }
                }()
        }

        log.Info("node is running",
                "validators", len(validators),
                "my_pub", myKey.Public().ID(),
                "api", cfg.API.ListenAddr,
        )

        // ── 9. Wait for signal ────────────────────────────────────────────────────
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
        <-sig

        log.Info("shutting down...")
        close(stop)
        return nil
}

// loadOrGenerateValidatorKey returns the node's validator private key.
// Priority: config file → auto-generated persistent key at data_dir/validator.key.
func loadOrGenerateValidatorKey(cfg *config.Config, log *slog.Logger) (*crypto.ValidatorPrivKey, error) {
        // 1. Explicit key file from config
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

        // 2. Auto-persistent key at data_dir/validator.key
        keyPath := filepath.Join(cfg.DataDir, "validator.key")
        if data, err := os.ReadFile(keyPath); err == nil {
                priv, err := crypto.ValidatorPrivKeyFromBytes(data)
                if err != nil {
                        return nil, fmt.Errorf("parse persisted validator key: %w", err)
                }
                log.Info("loaded persisted validator key", "pub", priv.Public().ID())
                return &priv, nil
        }

        // 3. Generate a new key and persist it
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
