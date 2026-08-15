// Package node ties all subsystems together into a single runnable node.
package node

import (
        "encoding/json"
        "fmt"
        "log/slog"
        "os"
        "os/signal"
        "syscall"
        "time"

        "github.com/aperod/aperod/consensus"
        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/p2p"
        "github.com/aperod/aperod/store"
)

// Node is the complete Aperod node: chain + consensus + p2p + API.
type Node struct {
        cfg        *Config
        log        *slog.Logger
        chain      *core.Chain
        utxos      *core.UTXOSet
        txVerifier *core.TxVerifier
        mempool    *core.Mempool
        verifier   *core.BlockVerifier
        engine     *consensus.Engine
        host       *p2p.Host
        db         *store.DB
        stop       chan struct{}
}

// Config holds all node configuration.
type Config struct {
        DataDir        string
        Network        string
        LogLevel       string
        GenesisFile    string
        ValidatorKeyFile string
        P2PAddr        string
        Bootnodes      []string
        MaxPeers       int
        MinPeers       int
        BlockTime      time.Duration
        APIAddr        string
        APIEnabled     bool
}

// New creates a fully initialized (but not yet started) node.
func New(cfg *Config, log *slog.Logger) (*Node, error) {
        // ── Storage ───────────────────────────────────────────────────────────────
        if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
                return nil, fmt.Errorf("create data dir: %w", err)
        }
        db, err := store.Open(cfg.DataDir + "/chain.db")
        if err != nil {
                return nil, fmt.Errorf("open store: %w", err)
        }

        // ── Chain state ───────────────────────────────────────────────────────────
        chain := core.NewChain()
        utxos := core.NewUTXOSetWithDB(db)
        mempool := core.NewMempool(core.DefaultMempoolConfig())

        // ── Genesis ───────────────────────────────────────────────────────────────
        genesisCfg, err := core.LoadGenesis(cfg.GenesisFile)
        if err != nil {
                return nil, fmt.Errorf("load genesis: %w", err)
        }

        tipHash, tipHeight, err := db.GetTip()
        if err != nil {
                return nil, fmt.Errorf("get tip: %w", err)
        }

        if tipHash == (crypto.Hash32{}) {
                // First start: create and persist genesis block.
                validatorPriv, _, err := crypto.GenerateValidatorKey()
                if err != nil {
                        return nil, fmt.Errorf("genesis validator key: %w", err)
                }
                genesis, err := core.CreateGenesisBlock(genesisCfg, validatorPriv)
                if err != nil {
                        return nil, fmt.Errorf("create genesis: %w", err)
                }
                if err := chain.SetGenesis(genesis); err != nil {
                        return nil, err
                }
                if err := persistBlockToDB(db, genesis); err != nil {
                        return nil, fmt.Errorf("persist genesis: %w", err)
                }
                // Apply genesis outputs to in-memory UTXO set (no inputs, so
                // no double-spend risk; this is safe to call before the engine starts).
                if err := utxos.ApplyBlock(genesis); err != nil {
                        return nil, fmt.Errorf("apply genesis to utxo set: %w", err)
                }
                genesisHash := genesis.Hash()
                log.Info("genesis block created",
                        "chain_id", genesisCfg.ChainID,
                        "hash", fmt.Sprintf("%x", genesisHash[:8]),
                )
        } else {
                // Subsequent start: restore in-memory chain + UTXO set from LevelDB.
                log.Info("resuming chain", "height", tipHeight, "tip", fmt.Sprintf("%x", tipHash[:8]))
                if err := restoreChain(db, chain, utxos, tipHeight, log); err != nil {
                        return nil, fmt.Errorf("restore chain: %w", err)
                }
        }

        // ── Block/Tx verifiers ────────────────────────────────────────────────────
        txV := core.NewTxVerifier(utxos)
        // Wire vesting enforcement using the actual genesis block timestamp.
        // chain.Genesis() is always non-nil here because both fresh-start and resume
        // paths above call chain.SetGenesis before this point.
        // Use Header.Timestamp / 1e9 (nanoseconds → seconds) to get Unix seconds.
        if genesisBlock := chain.Genesis(); genesisBlock != nil {
                genesisTimeSec := genesisBlock.Header.Timestamp / 1e9
                vl, vlErr := core.BuildVestingLock(genesisCfg, genesisTimeSec)
                if vlErr != nil {
                        // A BuildVestingLock failure means the genesis config is
                        // malformed (e.g. duplicate spend keys).  Starting with
                        // enforcement disabled would be a silent security bypass —
                        // return a hard error so the caller rejects startup.
                        return nil, fmt.Errorf("vesting lock build failed — refusing to start without enforcement: %w", vlErr)
                }
                txV.SetVestingLock(vl)
                log.Info("vesting lock loaded", "locked_allocs", vl.LockedAllocsCount())
        }
        // Wire the verifier into the mempool so P2P-submitted transactions are
        // fully verified (ring sigs, range proofs, vesting locks) before entering.
        mempool.SetVerifier(txV)
        blockV := core.NewBlockVerifier(core.DefaultBlockVerifierConfig(), chain, txV)

        // ── Validator key ─────────────────────────────────────────────────────────
        var myKey *crypto.LockedValidatorKey
        if cfg.ValidatorKeyFile != "" {
                raw, err := os.ReadFile(cfg.ValidatorKeyFile)
                if err != nil {
                        return nil, fmt.Errorf("read validator key: %w", err)
                }
                lk, err := crypto.NewLockedValidatorKey(raw, func(mlockErr error) {
                        log.Warn("mlock validator key bytes failed (non-fatal)", "err", mlockErr)
                })
                crypto.ZeroBytes(raw) // zero transient bytes after key derivation
                if err != nil {
                        return nil, fmt.Errorf("parse validator key: %w", err)
                }
                myKey = lk
                log.Info("loaded validator key", "pub", lk.Public().ID())
        }

        // ── Validator set from genesis ────────────────────────────────────────────
        validators := make([]crypto.ValidatorPubKey, 0, len(genesisCfg.Validators))
        for _, hexKey := range genesisCfg.Validators {
                b := mustHexDecode(hexKey)
                if b == nil || allZero(b) {
                        continue // skip placeholder keys
                }
                pub, err := crypto.ValidatorPubKeyFromBytes(b)
                if err != nil {
                        return nil, fmt.Errorf("invalid validator key %s: %w", hexKey[:8], err)
                }
                validators = append(validators, pub)
        }

        // ── Consensus engine ──────────────────────────────────────────────────────
        engine := consensus.NewEngine(consensus.Config{
                BlockTime:    cfg.BlockTime,
                BFTThreshold: genesisCfg.BFTThreshold,
                Validators:   validators,
                MyKey:        myKey,
                // Persist every self-produced block synchronously inside tick()
                // so the produced-block channel is only used for P2P broadcast.
                OnBlockProduced: func(b *core.Block) {
                        // UTXO state is already updated by engine.tick() before this
                        // callback fires. persistBlockToDB is DB-only — no ApplyBlock.
                        if err := persistBlockToDB(db, b); err != nil {
                                log.Error("persist produced block failed", "height", b.Header.Height, "err", err)
                        }
                },
        }, chain, mempool, log)

        // ── P2P host ──────────────────────────────────────────────────────────────
        p2pHandler := &p2pAdapter{
                chain:   chain,
                mempool: mempool,
                engine:  engine,
                blockV:  blockV,
                utxos:   utxos,
                log:     log,
        }
        host := p2p.NewHost(p2p.Config{
                ListenAddr: cfg.P2PAddr,
                Bootnodes:  cfg.Bootnodes,
                MaxPeers:   cfg.MaxPeers,
                MinPeers:   cfg.MinPeers,
                NodeID:     "aperod-node-v0.1",
                UserAgent:  "Aperod/0.1.0",
        }, p2pHandler, log)

        return &Node{
                cfg:        cfg,
                log:        log,
                chain:      chain,
                utxos:      utxos,
                txVerifier: txV,
                mempool:    mempool,
                verifier:   blockV,
                engine:     engine,
                host:       host,
                db:         db,
                stop:       make(chan struct{}),
        }, nil
}

// Start launches all subsystems. Blocks until SIGINT/SIGTERM.
func (n *Node) Start() error {
        n.log.Info("starting Aperod node",
                "network", n.cfg.Network,
                "data_dir", n.cfg.DataDir,
                "p2p", n.cfg.P2PAddr,
        )

        // Start P2P
        if err := n.host.Start(); err != nil {
                return fmt.Errorf("p2p start: %w", err)
        }

        // Wire TxVerifier BEFORE starting the engine goroutine so that
        // handleIncomingBlock never runs with e.txVerifier == nil (fail-closed).
        n.engine.SetTxVerifier(n.txVerifier, n.utxos)

        // Start consensus loop
        go n.engine.Run(n.stop)

        // Wire consensus → p2p: broadcast produced blocks and votes
        go n.broadcastLoop()

        n.log.Info("node running",
                "chain_height", n.chain.Height(),
                "peers", n.host.PeerCount(),
        )

        // Wait for shutdown
        sig := make(chan os.Signal, 1)
        signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
        <-sig

        return n.Stop()
}

// Stop shuts down all subsystems gracefully.
func (n *Node) Stop() error {
        n.log.Info("shutting down node...")
        close(n.stop)
        n.host.Stop()
        n.db.Close()
        n.log.Info("node stopped")
        return nil
}

// broadcastLoop reads produced blocks from consensus and broadcasts them to peers.
// Block persistence is handled synchronously in the OnBlockProduced callback
// inside tick(), so this loop is solely responsible for P2P propagation.
func (n *Node) broadcastLoop() {
        for {
                select {
                case <-n.stop:
                        return
                case block := <-n.engine.ProducedCh():
                        n.host.BroadcastBlock(block)
                }
        }
}

// persistBlock saves a block to LevelDB (DB-only; UTXO state already updated by engine).
func (n *Node) persistBlock(block *Block) {
        if err := persistBlockToDB(n.db, block); err != nil {
                n.log.Error("persist block failed", "height", block.Header.Height, "err", err)
        } else {
                n.log.Debug("block persisted", "height", block.Header.Height, "txs", len(block.Txs))
        }
}

// persistBlockToDB serializes block → LevelDB and persists UTXO/key-image
// indexes for future startup restores.  It does NOT mutate the in-memory
// UTXOSet — that state transition is owned by the consensus engine (tick /
// handleIncomingBlock) which calls utxos.ApplyBlock before calling this.
// Calling ApplyBlock here a second time would cause a double-apply error for
// any block that spends inputs.
func persistBlockToDB(db *store.DB, block *core.Block) error {
        // 1. Serialize the full block as JSON.
        data, err := json.Marshal(block)
        if err != nil {
                return fmt.Errorf("marshal block: %w", err)
        }
        blockHash := block.Hash()
        if err := db.PutRawBlock(blockHash, block.Header.Height, data); err != nil {
                return fmt.Errorf("put raw block: %w", err)
        }

        // 2. Persist key images and UTXOs to the LevelDB indexes so that
        //    restoreChain can reconstruct in-memory state on next startup.
        for _, tx := range block.Txs {
                txHash := tx.Hash()
                for _, inp := range tx.Inputs {
                        if err := db.MarkKeyImageSpent(inp.KeyImage); err != nil {
                                return fmt.Errorf("mark key image spent: %w", err)
                        }
                }
                for i, out := range tx.Outputs {
                        su := &store.StoredUTXO{
                                TxHash:       txHash,
                                OutputIndex:  uint32(i),
                                OneTimePub:   out.OneTimePub,
                                TxPubKey:     out.TxPubKey,
                                AmountCommit: out.AmountCommit,
                                EncAmount:    out.EncAmount,
                                BlockHeight:  block.Header.Height,
                        }
                        if err := db.PutUTXO(txHash, uint32(i), su); err != nil {
                                return fmt.Errorf("put utxo: %w", err)
                        }
                }
        }

        // 3. Index stake-bearing blocks for the db-index fast-path startup scan.
        for _, tx := range block.Txs {
                if tx.IsStake() {
                        if sbErr := db.PutStakeBlockHeight(block.Header.Height); sbErr != nil {
                                return fmt.Errorf("put stake block height: %w", sbErr)
                        }
                        break
                }
        }

        // 4. Update chain tip pointer.
        if err := db.PutTip(blockHash, block.Header.Height); err != nil {
                return fmt.Errorf("put tip: %w", err)
        }
        return nil
}

// restoreChain rebuilds the in-memory chain and UTXO set from LevelDB.
// Reads blocks sequentially from height 0 to tipHeight, then restores UTXOs
// and spent key images from their respective DB namespaces.
func restoreChain(db *store.DB, chain *core.Chain, utxos *core.UTXOSet, tipHeight uint64, log *slog.Logger) error {
        // ── Restore canonical chain ────────────────────────────────────────────────
        for h := uint64(0); h <= tipHeight; h++ {
                data, err := db.GetRawBlockByHeight(h)
                if err != nil {
                        return fmt.Errorf("get block at height %d: %w", h, err)
                }
                if data == nil {
                        return fmt.Errorf("missing block at height %d in DB", h)
                }
                var block core.Block
                if err := json.Unmarshal(data, &block); err != nil {
                        return fmt.Errorf("unmarshal block at height %d: %w", h, err)
                }
                if h == 0 {
                        if err := chain.SetGenesis(&block); err != nil {
                                return fmt.Errorf("set genesis from DB: %w", err)
                        }
                } else {
                        if err := chain.AddBlock(&block); err != nil {
                                return fmt.Errorf("add block %d from DB: %w", h, err)
                        }
                }
        }
        log.Info("chain restored", "height", tipHeight)

        // ── Restore UTXO set from persisted UTXOs ────────────────────────────────
        utxoCount := 0
        if err := db.IterUTXOs(func(su *store.StoredUTXO) error {
                utxos.Add(&core.UTXO{
                        TxHash:       su.TxHash,
                        OutputIndex:  su.OutputIndex,
                        OneTimePub:   su.OneTimePub,
                        TxPubKey:     su.TxPubKey,
                        AmountCommit: su.AmountCommit,
                        EncAmount:    su.EncAmount,
                        BlockHeight:  su.BlockHeight,
                })
                utxoCount++
                return nil
        }); err != nil {
                return fmt.Errorf("iter utxos: %w", err)
        }
        log.Info("UTXOs restored", "count", utxoCount)

        // ── Restore spent key images ──────────────────────────────────────────────
        kiCount := 0
        if err := db.IterKeyImages(func(ki crypto.KeyImage) error {
                utxos.MarkSpent(ki)
                kiCount++
                return nil
        }); err != nil {
                return fmt.Errorf("iter key images: %w", err)
        }
        log.Info("key images restored", "count", kiCount)

        // ── Rebuild Phase 2 spent-decoy pool ─────────────────────────────────────
        // Re-iterate all canonical blocks and call ApplyBlockForSpentDecoys for
        // each one.  This reconstructs the spentPubKeys pool that was populated at
        // runtime by ApplyBlock — without it SampleDecoys returns nothing after a
        // restart and wallet sends silently fall back to Phase 1 random keys.
        //
        // The block data is already persisted in LevelDB from the first loop above;
        // this second pass reads only the block JSON, which is cheap relative to the
        // full chain replay done by the consensus layer.
        for h := uint64(0); h <= tipHeight; h++ {
                data, err := db.GetRawBlockByHeight(h)
                if err != nil {
                        return fmt.Errorf("get block at height %d for decoy rebuild: %w", h, err)
                }
                if data == nil {
                        return fmt.Errorf("missing block at height %d during decoy rebuild", h)
                }
                var block core.Block
                if err := json.Unmarshal(data, &block); err != nil {
                        return fmt.Errorf("unmarshal block at height %d for decoy rebuild: %w", h, err)
                }
                utxos.ApplyBlockForSpentDecoys(&block)
        }
        log.Info("spent decoy pool rebuilt", "size", utxos.SpentDecoyCount())
        return nil
}

// Chain returns the node's chain (for API use).
func (n *Node) Chain() *Chain { return n.chain }

// Mempool returns the node's mempool (for API use).
func (n *Node) Mempool() *Mempool { return n.mempool }

// ─── p2p adapter ──────────────────────────────────────────────────────────────

type p2pAdapter struct {
        chain   *core.Chain
        mempool *core.Mempool
        engine  *consensus.Engine
        blockV  *core.BlockVerifier
        utxos   *core.UTXOSet
        log     *slog.Logger
}

func (a *p2pAdapter) OnBlock(block *core.Block) {
        if err := a.blockV.VerifyBlock(block); err != nil {
                a.log.Warn("p2p: invalid block rejected", "height", block.Header.Height, "err", err)
                return
        }
        // Forward to consensus engine
        select {
        case a.engine.NewBlockCh() <- block:
        default:
                a.log.Warn("p2p: consensus block channel full, dropping block", "height", block.Header.Height)
        }
}

func (a *p2pAdapter) OnTransaction(tx *core.Transaction) {
        if err := a.mempool.Add(*tx); err != nil {
                a.log.Debug("p2p: tx rejected", "err", err)
        }
}

func (a *p2pAdapter) OnVote(vote p2p.VoteMsg) {
        pub, err := crypto.ValidatorPubKeyFromBytes(vote.ValidatorPub)
        if err != nil {
                a.log.Warn("p2p: invalid vote pub key", "err", err)
                return
        }
        select {
        case a.engine.NewVoteCh() <- consensus.FinalizeMsg{
                BlockHash:    vote.BlockHash,
                Height:       vote.Height,
                ValidatorPub: pub,
                Signature:    vote.Signature,
        }:
        default:
                a.log.Warn("p2p: vote channel full")
        }
}

func (a *p2pAdapter) CurrentHeight() uint64 { return a.chain.Height() }

func (a *p2pAdapter) CurrentTailHashes(n int) []crypto.Hash32 {
        return a.chain.TailHashes(n)
}

func (a *p2pAdapter) GetBlock(hash crypto.Hash32) *core.Block {
        return a.chain.GetByHash(hash)
}

// ─── Type aliases for node package ───────────────────────────────────────────

type Block = core.Block
type Transaction = core.Transaction
type Mempool = core.Mempool
type Chain = core.Chain

// ─── Helpers ──────────────────────────────────────────────────────────────────

func mustHexDecode(s string) []byte {
        if len(s)%2 != 0 {
                return nil
        }
        b := make([]byte, len(s)/2)
        for i := 0; i < len(s)/2; i++ {
                var v byte
                if _, err := fmt.Sscanf(s[i*2:i*2+2], "%02x", &v); err != nil {
                        return nil
                }
                b[i] = v
        }
        return b
}

func allZero(b []byte) bool {
        for _, v := range b {
                if v != 0 {
                        return false
                }
        }
        return true
}
