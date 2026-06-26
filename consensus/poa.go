// Package consensus implements Aperod's Proof of Authority (PoA) consensus engine.
// Validators are a hardcoded list from genesis. Block production uses round-robin
// slot assignment. Finalization requires 2/3 of validators to sign (BFT threshold).
package consensus

import (
        "fmt"
        "log/slog"
        "sync"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// Config holds PoA consensus parameters.
type Config struct {
        BlockTime    time.Duration
        BFTThreshold float64 // fraction of validators needed to finalize (e.g. 0.667)
        // Validators is the ordered list of validator public keys from genesis.
        Validators []crypto.ValidatorPubKey
        // MyKey is this node's validator key (nil if not a validator).
        MyKey *crypto.ValidatorPrivKey
        // OnBlockProduced is an optional callback called after each block is added
        // to the chain. Use it to persist blocks to durable storage.
        OnBlockProduced func(block *core.Block)
}

// FinalizeMsg is a vote by a validator to finalize a block.
type FinalizeMsg struct {
        BlockHash crypto.Hash32
        Height    uint64
        ValidatorPub crypto.ValidatorPubKey
        Signature    []byte
}

// Engine is the PoA consensus engine.
type Engine struct {
        cfg   Config
        chain *core.Chain
        pool  *core.Mempool
        log   *slog.Logger

        mu        sync.Mutex
        // votes collected for each block hash: pubkey hex → signature
        votes     map[crypto.Hash32]map[string][]byte
        // finalized tracks which heights have been finalized
        finalized map[uint64]bool
        // slashing detector for double-sign evidence
        slashing  *slashingDetector

        // Channels for external events
        newBlockCh chan *core.Block  // incoming blocks from P2P
        newVoteCh  chan FinalizeMsg  // incoming finalization votes
        producedCh chan *core.Block  // blocks produced by this node (for broadcast)
}

// NewEngine creates a new PoA consensus engine.
func NewEngine(cfg Config, chain *core.Chain, pool *core.Mempool, log *slog.Logger) *Engine {
        return &Engine{
                cfg:        cfg,
                chain:      chain,
                pool:       pool,
                log:        log,
                votes:      make(map[crypto.Hash32]map[string][]byte),
                finalized:  make(map[uint64]bool),
                slashing:   newSlashingDetector(),
                newBlockCh: make(chan *core.Block, 64),
                newVoteCh:  make(chan FinalizeMsg, 256),
                producedCh: make(chan *core.Block, 64),
        }
}

// Run starts the consensus loop. Blocks until ctx is done.
func (e *Engine) Run(stop <-chan struct{}) {
        ticker := time.NewTicker(e.cfg.BlockTime)
        defer ticker.Stop()

        e.log.Info("consensus engine started",
                "validators", len(e.cfg.Validators),
                "block_time", e.cfg.BlockTime,
                "bft_threshold", e.cfg.BFTThreshold,
        )

        for {
                select {
                case <-stop:
                        e.log.Info("consensus engine stopped")
                        return

                case <-ticker.C:
                        if err := e.tick(); err != nil {
                                e.log.Warn("consensus tick error", "err", err)
                        }

                case block := <-e.newBlockCh:
                        if err := e.handleIncomingBlock(block); err != nil {
                                e.log.Warn("incoming block rejected", "height", block.Header.Height, "err", err)
                        }

                case vote := <-e.newVoteCh:
                        if err := e.handleVote(vote); err != nil {
                                e.log.Warn("vote rejected", "height", vote.Height, "err", err)
                        }
                }
        }
}

// tick is called once per block slot.
func (e *Engine) tick() error {
        tip := e.chain.Tip()
        if tip == nil {
                return fmt.Errorf("no genesis block")
        }

        nextHeight := tip.Header.Height + 1
        nextRound := tip.Header.Round + 1

        // Check if it's our turn to propose
        proposer := e.proposerAt(nextRound)
        if e.cfg.MyKey == nil || !proposer.Equals(e.cfg.MyKey.Public()) {
                return nil // not our slot
        }

        e.log.Info("producing block", "height", nextHeight, "round", nextRound)
        block, err := e.produceBlock(nextHeight, uint64(nextRound), tip)
        if err != nil {
                return fmt.Errorf("produce block: %w", err)
        }

        // Add to our own chain
        if err := e.chain.AddBlock(block); err != nil {
                return fmt.Errorf("add produced block: %w", err)
        }

        // Persist block to durable storage (if callback configured)
        if e.cfg.OnBlockProduced != nil {
                e.cfg.OnBlockProduced(block)
        }

        // Broadcast to P2P
        select {
        case e.producedCh <- block:
        default:
                e.log.Warn("produced block channel full")
        }

        // Cast our own finalization vote
        if err := e.castVote(block); err != nil {
                e.log.Warn("failed to cast own vote", "err", err)
        }

        return nil
}

// produceBlock assembles a new block from the mempool.
func (e *Engine) produceBlock(height, round uint64, parent *core.Block) (*core.Block, error) {
        txs := e.pool.SelectTxs(500) // up to 500 txs per block

        header := core.BlockHeader{
                Height:       height,
                PrevHash:     parent.Hash(),
                MerkleRoot:   core.MerkleRoot(txs),
                Timestamp:    time.Now().UTC().UnixNano(),
                Round:        uint32(round),
                ValidatorPub: e.cfg.MyKey.Public(),
        }
        if err := header.Sign(*e.cfg.MyKey); err != nil {
                return nil, err
        }
        return &core.Block{Header: header, Txs: txs}, nil
}

// handleIncomingBlock processes a block received from P2P.
func (e *Engine) handleIncomingBlock(block *core.Block) error {
        // Basic structural validation
        if err := block.Validate(); err != nil {
                return fmt.Errorf("invalid block: %w", err)
        }

        // Check proposer is a known validator
        if !e.isKnownValidator(block.Header.ValidatorPub) {
                return fmt.Errorf("block from unknown validator %s", block.Header.ValidatorPub.ID())
        }

        // Double-sign detection
        hash := block.Hash()
        if ev := e.slashing.CheckBlock(
                block.Header.Height, block.Header.Round,
                block.Header.ValidatorPub, hash, block.Header.Signature,
        ); ev != nil {
                e.log.Error("SLASHING: double-sign detected", "evidence", ev.String())
                return fmt.Errorf("double-sign: %s", ev.String())
        }

        // Check it extends our current tip
        tip := e.chain.Tip()
        if block.Header.Height != tip.Header.Height+1 {
                return fmt.Errorf("block height %d doesn't extend tip %d",
                        block.Header.Height, tip.Header.Height)
        }

        if err := e.chain.AddBlock(block); err != nil {
                return fmt.Errorf("add block: %w", err)
        }

        // Log checkpoint blocks
        if IsCheckpoint(block.Header.Height) {
                e.log.Info("CHECKPOINT reached", "height", block.Header.Height)
        }

        e.log.Info("accepted block",
                "height", block.Header.Height,
                "txs", len(block.Txs),
                "validator", block.Header.ValidatorPub.ID(),
        )

        // Remove included transactions from mempool
        e.pool.RemoveBlock(block)

        // Cast finalization vote if we are a validator
        if e.cfg.MyKey != nil {
                _ = e.castVote(block)
        }

        return nil
}

// castVote signs and broadcasts a finalization vote for a block.
func (e *Engine) castVote(block *core.Block) error {
        hash := block.Hash()
        msg := crypto.HashBytes([]byte("aperod/finalize/v1"), hash[:])
        sig, err := e.cfg.MyKey.Sign(msg)
        if err != nil {
                return err
        }
        vote := FinalizeMsg{
                BlockHash:    hash,
                Height:       block.Header.Height,
                ValidatorPub: e.cfg.MyKey.Public(),
                Signature:    sig,
        }
        // Process our own vote immediately
        return e.handleVote(vote)
}

// handleVote processes a finalization vote from any validator.
func (e *Engine) handleVote(vote FinalizeMsg) error {
        if !e.isKnownValidator(vote.ValidatorPub) {
                return fmt.Errorf("vote from unknown validator %s", vote.ValidatorPub.ID())
        }

        // Verify the vote signature
        msg := crypto.HashBytes([]byte("aperod/finalize/v1"), vote.BlockHash[:])
        if !vote.ValidatorPub.Verify(msg, vote.Signature) {
                return fmt.Errorf("invalid vote signature from %s", vote.ValidatorPub.ID())
        }

        e.mu.Lock()
        defer e.mu.Unlock()

        if e.finalized[vote.Height] {
                return nil // already finalized
        }

        if e.votes[vote.BlockHash] == nil {
                e.votes[vote.BlockHash] = make(map[string][]byte)
        }
        e.votes[vote.BlockHash][vote.ValidatorPub.Hex()] = vote.Signature

        // Check if we've reached BFT threshold (2/3 of validators)
        needed := int(float64(len(e.cfg.Validators))*e.cfg.BFTThreshold) + 1
        if len(e.votes[vote.BlockHash]) >= needed {
                e.finalized[vote.Height] = true
                e.log.Info("block finalized",
                        "height", vote.Height,
                        "votes", len(e.votes[vote.BlockHash]),
                        "needed", needed,
                )
                // Clean up old vote records
                delete(e.votes, vote.BlockHash)
        }

        return nil
}

// proposerAt returns the validator that should propose at round r (round-robin).
func (e *Engine) proposerAt(round uint32) crypto.ValidatorPubKey {
        idx := int(round) % len(e.cfg.Validators)
        return e.cfg.Validators[idx]
}

// isKnownValidator returns true if pub is in the validator set.
func (e *Engine) isKnownValidator(pub crypto.ValidatorPubKey) bool {
        for _, v := range e.cfg.Validators {
                if v.Equals(pub) {
                        return true
                }
        }
        return false
}

// NewBlockCh returns the channel for submitting incoming P2P blocks.
func (e *Engine) NewBlockCh() chan<- *core.Block { return e.newBlockCh }

// NewVoteCh returns the channel for submitting incoming P2P votes.
func (e *Engine) NewVoteCh() chan<- FinalizeMsg { return e.newVoteCh }

// ProducedCh returns the channel from which produced blocks can be read for broadcast.
func (e *Engine) ProducedCh() <-chan *core.Block { return e.producedCh }

// IsFinalized returns true if a block at height h has been finalized.
func (e *Engine) IsFinalized(height uint64) bool {
        e.mu.Lock()
        defer e.mu.Unlock()
        return e.finalized[height]
}

// Chain returns the engine's chain (for testing and API use).
func (e *Engine) Chain() *core.Chain { return e.chain }

// ProposerAt returns the validator that should propose at round r (exported for testing).
func (e *Engine) ProposerAt(round uint32) crypto.ValidatorPubKey {
        return e.proposerAt(round)
}

// HandleVote processes a finalization vote (exported for testing and P2P).
func (e *Engine) HandleVote(vote FinalizeMsg) error {
        return e.handleVote(vote)
}
