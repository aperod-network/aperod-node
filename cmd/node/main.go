// Command node is the Aperod blockchain node process.
// It initializes the chain, starts the P2P network, consensus engine,
// and optionally the RPC API server.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
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

	log.Info("starting Aperod node",
		"network", cfg.Network,
		"data_dir", cfg.DataDir,
	)

	// ── 3. Open storage ───────────────────────────────────────────────────────
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	db, err := store.Open(cfg.DataDir + "/chain.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()

	// ── 4. Load or create genesis ─────────────────────────────────────────────
	genesisConfig, err := core.LoadGenesis(cfg.Genesis.File)
	if err != nil {
		return fmt.Errorf("load genesis: %w", err)
	}

	// ── 5. Initialize chain ───────────────────────────────────────────────────
	chain := core.NewChain()
	mempool := core.NewMempool(core.DefaultMempoolConfig())

	// Check if we already have a stored chain tip
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return fmt.Errorf("get tip: %w", err)
	}

	if tipHash == (crypto.Hash32{}) {
		// Fresh node: create and persist genesis block
		log.Info("initializing genesis block", "chain_id", genesisConfig.ChainID)
		validatorKey, _, err := crypto.GenerateValidatorKey()
		if err != nil {
			return fmt.Errorf("generate validator key: %w", err)
		}
		genesis, err := core.CreateGenesisBlock(genesisConfig, validatorKey)
		if err != nil {
			return fmt.Errorf("create genesis: %w", err)
		}
		if err := chain.SetGenesis(genesis); err != nil {
			return fmt.Errorf("set genesis: %w", err)
		}
		// Persist genesis block so it survives restarts
		if err := storeBlock(db, genesis); err != nil {
			return fmt.Errorf("store genesis: %w", err)
		}
		if err := db.PutTip(genesis.Hash(), 0); err != nil {
			return fmt.Errorf("store genesis tip: %w", err)
		}
		genesisHash := genesis.Hash()
		log.Info("genesis block created", "hash", fmt.Sprintf("%x", genesisHash[:8]))
	} else {
		// Resume: load all blocks from DB (height 0 → tipHeight)
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
				log.Warn("block unmarshal failed, stopping resume", "height", h, "err", err)
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

	// ── 6. Setup consensus engine ─────────────────────────────────────────────
	// Parse validator public keys from genesis
	validators := make([]crypto.ValidatorPubKey, 0, len(genesisConfig.Validators))
	for _, hexKey := range genesisConfig.Validators {
		keyBytes := hexDecode(hexKey)
		if keyBytes == nil {
			return fmt.Errorf("invalid validator key: %s", hexKey)
		}
		pub, err := crypto.ValidatorPubKeyFromBytes(keyBytes)
		if err != nil {
			return fmt.Errorf("parse validator key: %w", err)
		}
		validators = append(validators, pub)
	}

	// Load this node's validator key if configured
	var myKey *crypto.ValidatorPrivKey
	if cfg.Consensus.ValidatorKey != "" {
		privBytes, err := os.ReadFile(cfg.Consensus.ValidatorKey)
		if err != nil {
			return fmt.Errorf("read validator key file: %w", err)
		}
		priv, err := crypto.ValidatorPrivKeyFromBytes(privBytes)
		if err != nil {
			return fmt.Errorf("parse validator private key: %w", err)
		}
		myKey = &priv
		log.Info("loaded validator key", "pub", priv.Public().ID())
	}

	engine := consensus.NewEngine(consensus.Config{
		BlockTime:    cfg.Consensus.BlockTime,
		BFTThreshold: genesisConfig.BFTThreshold,
		Validators:   validators,
		MyKey:        myKey,
		// Persist every produced block to LevelDB
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

	// ── 7. Start subsystems ───────────────────────────────────────────────────
	stop := make(chan struct{})

	// Consensus loop
	go engine.Run(stop)

	// API server (JSON-RPC + REST + WebSocket)
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
		"api", cfg.API.ListenAddr,
	)

	// ── 8. Wait for signal ────────────────────────────────────────────────────
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	log.Info("shutting down...")
	close(stop)
	return nil
}

// storeBlock serialises block to JSON and writes it via PutRawBlock.
func storeBlock(db *store.DB, b *core.Block) error {
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("marshal block: %w", err)
	}
	hash := b.Hash()
	return db.PutRawBlock(hash, b.Header.Height, data)
}

// hexDecode decodes a hex string to bytes. Returns nil on error.
func hexDecode(s string) []byte {
	if len(s)%2 != 0 {
		return nil
	}
	b := make([]byte, len(s)/2)
	for i := 0; i < len(s)/2; i++ {
		_, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &b[i])
		if err != nil {
			return nil
		}
	}
	return b
}
