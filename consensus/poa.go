// Package consensus implements Aperod's Proof of Authority (PoA) consensus engine.
// Validators are a hardcoded list from genesis. Block production uses round-robin
// slot assignment. Finalization requires 2/3 of validators to sign (BFT threshold).
package consensus

import (
        "encoding/json"
        "fmt"
        "log/slog"
        "math"
        "net/http"
        "sync"
        "time"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// Config holds PoA consensus parameters.
type Config struct {
        BlockTime    time.Duration
        BFTThreshold float64 // fraction of validators needed to finalize (e.g. 0.667)
        // Validators is the bootstrap list from genesis (used to seed the Registry).
        // After startup the active set is managed by Registry.
        Validators []crypto.ValidatorPubKey
        // Registry is the live stake-based validator registry.
        // If nil the engine falls back to the static Validators list.
        Registry *core.ValidatorRegistry
        // MyKey is this node's validator key (nil if not a validator).
        MyKey *crypto.ValidatorPrivKey
        // OnBlockProduced is an optional callback called after each block is added
        // to the chain. Use it to persist blocks to durable storage.
        OnBlockProduced func(block *core.Block)
        // RewardAddress is the APRO wallet address that receives block rewards.
        // If empty, no coinbase transaction is added to produced blocks.
        RewardAddress string
        // BlockRewardNAPR is the block reward in base units (nAPRO).
        // 0 uses the default: 10_000_000 nAPRO = 0.1 APRO.
        BlockRewardNAPR uint64
        // OracleURL is the HTTP endpoint from which the node fetches the current
        // APRO/USD price before embedding it in each produced block header.
        // Expected response: JSON object with a numeric "price_usd" field.
        // If empty, oracle price embedding is skipped (OraclePrice = 0).
        OracleURL string
        // OracleMaxDeviation is the maximum allowed fractional deviation between
        // a peer's embedded OraclePrice and our own local price (e.g. 0.05 = 5%).
        // Incoming blocks whose price deviates beyond this are rejected.
        // Zero (default) disables the deviation check.
        OracleMaxDeviation float64
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
        // baseFee is the current EIP-1559 base fee per byte (nAPRO/byte).
        // Updated after every accepted block; embedded in every produced block header.
        baseFee   uint64
        // accumulatedTips holds priority tips collected across blocks until the
        // next coinbase is produced by this validator, when they are paid out.
        accumulatedTips uint64

        // Channels for external events
        newBlockCh chan *core.Block  // incoming blocks from P2P
        newVoteCh  chan FinalizeMsg  // incoming finalization votes
        producedCh chan *core.Block  // blocks produced by this node (for broadcast)
}

// NewEngine creates a new PoA consensus engine.
func NewEngine(cfg Config, chain *core.Chain, pool *core.Mempool, log *slog.Logger) *Engine {
        e := &Engine{
                cfg:        cfg,
                chain:      chain,
                pool:       pool,
                log:        log,
                votes:      make(map[crypto.Hash32]map[string][]byte),
                finalized:  make(map[uint64]bool),
                slashing:   newSlashingDetector(),
                baseFee:    core.InitialBaseFeePerByte,
                newBlockCh: make(chan *core.Block, 64),
                newVoteCh:  make(chan FinalizeMsg, 256),
                producedCh: make(chan *core.Block, 64),
        }
        // Seed the registry with genesis validators so they start Active.
        if cfg.Registry != nil && len(cfg.Validators) > 0 {
                genesisStake := core.MinStakeNAPR * 10 // genesis validators credited 10× min
                cfg.Registry.InitFromGenesis(cfg.Validators, genesisStake)
        }
        return e
}

// activeValidators returns the current active validator set.
// Reads from the Registry if available; falls back to the static genesis list.
func (e *Engine) activeValidators() []crypto.ValidatorPubKey {
        if e.cfg.Registry != nil {
                vs := e.cfg.Registry.GetActiveValidators()
                if len(vs) > 0 {
                        return vs
                }
        }
        return e.cfg.Validators
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

        // Process stake txs and epoch updates for self-produced blocks
        e.processStakeTxs(block)
        if e.cfg.Registry != nil && block.Header.Height%core.EpochLength == 0 {
                newSet := e.cfg.Registry.UpdateEpoch(block.Header.Height)
                e.log.Info("epoch updated (self-produced)",
                        "height", block.Header.Height,
                        "epoch", block.Header.Height/core.EpochLength,
                        "active_validators", len(newSet),
                )
        }

        // Remove included transactions from mempool (same as acceptBlock does for
        // incoming blocks — without this, txs would be re-included every block).
        e.pool.RemoveBlock(block)

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

// defaultBlockRewardNAPR is 5 APRO in base units (TZ §1.2 Tail Emission).
// 1 APRO = 100_000_000 nAPRO, so 5 APRO = 500_000_000 nAPRO.
// At 1 block/second this yields 157,680,000 APRO/year across all 21 validators.
const defaultBlockRewardNAPR uint64 = 500_000_000

// EIP-1559–style dynamic base fee constants.
const (
        // targetBlockSizeBytes is the ideal block size (50% of practical max).
        // With up to 500 txs at ~2 000 bytes each the max is ~1 MB; target = 500 KB.
        targetBlockSizeBytes = 500_000

        // baseFeeMaxChangePct is the maximum ±change per block (12.5%, same as EIP-1559).
        // At exactly 2× target the base fee rises by 12.5%; at 0 bytes it falls by 12.5%.
        baseFeeMaxChangePct = 0.125
)

// nextBaseFee computes the base fee for the next block using EIP-1559 adjustment.
// blockSizeBytes is the total byte size of all transactions in the current block.
// The fee rises when blockSizeBytes > targetBlockSizeBytes and falls otherwise,
// capped at ±12.5% per block, and never below core.MinBaseFeePerByte.
func nextBaseFee(current uint64, blockSizeBytes int) uint64 {
        if current == 0 {
                current = core.InitialBaseFeePerByte
        }
        // delta = current × (blockSize - target) / target × maxChangePct
        // Simplified: delta = current × fillRatio × maxChangePct
        // where fillRatio = (blockSize - target) / target  (can be negative)
        diff := float64(blockSizeBytes) - float64(targetBlockSizeBytes)
        delta := float64(current) * (diff / float64(targetBlockSizeBytes)) * baseFeeMaxChangePct
        next := int64(current) + int64(math.Round(delta))
        if next < int64(core.MinBaseFeePerByte) {
                next = int64(core.MinBaseFeePerByte)
        }
        return uint64(next)
}

// blockFeeStats computes burned nAPRO and priority-tip nAPRO for a block's transactions.
// burned   = Σ tx.Size() × baseFeePerByte  (100% destroyed)
// tipTotal = Σ tx.Fee - burned              (goes to validator)
func blockFeeStats(txs []core.Transaction, baseFeePerByte uint64) (burned, tipTotal uint64) {
        for _, tx := range txs {
                if tx.IsCoinbase() || tx.IsStake() {
                        continue
                }
                minFee := tx.MinFeeAt(baseFeePerByte)
                if tx.Fee >= minFee {
                        burned += minFee
                        tipTotal += tx.Fee - minFee
                } else {
                        // Malformed tx slipped through; treat entire fee as burned.
                        burned += tx.Fee
                }
        }
        return
}

// oraclePriceScale is the fixed-point scale factor for the OraclePrice field.
// OraclePrice = price_usd × oraclePriceScale  (i.e. 9 decimal places).
const oraclePriceScale float64 = 1_000_000_000

// oraclePriceResponse is the minimal subset of the oracle API JSON we need.
type oraclePriceResponse struct {
        PriceUSD float64 `json:"price_usd"`
}

// fetchOraclePrice calls cfg.OracleURL and returns the embedded fixed-point price.
// Returns 0 if OracleURL is empty, the request fails, or the price is zero/negative.
func (e *Engine) fetchOraclePrice() uint64 {
        if e.cfg.OracleURL == "" {
                return 0
        }
        client := &http.Client{Timeout: 3 * time.Second}
        resp, err := client.Get(e.cfg.OracleURL)
        if err != nil {
                e.log.Warn("oracle price fetch failed", "url", e.cfg.OracleURL, "err", err)
                return 0
        }
        defer resp.Body.Close()
        var p oraclePriceResponse
        if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
                e.log.Warn("oracle price decode failed", "err", err)
                return 0
        }
        if p.PriceUSD <= 0 {
                return 0
        }
        return uint64(math.Round(p.PriceUSD * oraclePriceScale))
}

// checkOraclePriceDeviation returns an error if the block's embedded oracle price
// deviates from the local price by more than cfg.OracleMaxDeviation.
// Skipped when OracleURL or OracleMaxDeviation are unset, or when either price is zero.
func (e *Engine) checkOraclePriceDeviation(blockPrice uint64) error {
        if e.cfg.OracleURL == "" || e.cfg.OracleMaxDeviation <= 0 || blockPrice == 0 {
                return nil
        }
        localPrice := e.fetchOraclePrice()
        if localPrice == 0 {
                return nil // can't verify if we can't fetch our own price
        }
        deviation := math.Abs(float64(blockPrice)-float64(localPrice)) / float64(localPrice)
        if deviation > e.cfg.OracleMaxDeviation {
                return fmt.Errorf(
                        "oracle price deviation too large: block=%d local=%d deviation=%.2f%% (max %.2f%%)",
                        blockPrice, localPrice,
                        deviation*100, e.cfg.OracleMaxDeviation*100,
                )
        }
        return nil
}

// produceBlock assembles a new block from the mempool.
func (e *Engine) produceBlock(height, round uint64, parent *core.Block) (*core.Block, error) {
        txs := e.pool.SelectTxs(500) // up to 500 txs per block

        // EIP-1559–style 100% base-fee burn: fees are NOT forwarded to the
        // validator — they are destroyed upon block finalization by never
        // appearing in any output.  Validators earn only the explicit coinbase
        // mint reward below.  Log the burned amount for observability.
        var burnedNAPR uint64
        for _, tx := range txs {
                if !tx.IsCoinbase() && !tx.IsStake() {
                        burnedNAPR += tx.Fee
                }
        }
        if burnedNAPR > 0 {
                e.log.Info("base fee burned (100%)",
                        "height", height,
                        "burned_napro", burnedNAPR,
                        "burned_apro", float64(burnedNAPR)/1e8,
                        "tx_count", len(txs),
                )
        }

        // Compute burned fees and priority tips for selected txs, then include
        // tips in the coinbase reward (validator earns block reward + tips).
        e.mu.Lock()
        currentBaseFee := e.baseFee
        tips := e.accumulatedTips
        e.accumulatedTips = 0
        e.mu.Unlock()

        _, tipThisBlock := blockFeeStats(txs, currentBaseFee)
        tips += tipThisBlock

        // Prepend coinbase block reward transaction when reward_address is configured.
        if e.cfg.RewardAddress != "" {
                rewardNAPR := e.cfg.BlockRewardNAPR
                if rewardNAPR == 0 {
                        rewardNAPR = defaultBlockRewardNAPR
                }
                // Validator earns block reward + priority tips from all txs in this block.
                totalReward := rewardNAPR + tips
                mintTx, err := core.BuildMintTx(crypto.Address(e.cfg.RewardAddress), totalReward, height)
                if err != nil {
                        e.log.Warn("failed to build coinbase reward tx", "err", err)
                } else {
                        txs = append([]core.Transaction{*mintTx}, txs...)
                }
        }

        oraclePrice := e.fetchOraclePrice()

        header := core.BlockHeader{
                Height:       height,
                PrevHash:     parent.Hash(),
                MerkleRoot:   core.MerkleRoot(txs),
                Timestamp:    time.Now().UTC().UnixNano(),
                Round:        uint32(round),
                ValidatorPub: e.cfg.MyKey.Public(),
                OraclePrice:  oraclePrice,
                BaseFee:      currentBaseFee,
        }
        if err := header.Sign(*e.cfg.MyKey); err != nil {
                return nil, err
        }
        if oraclePrice > 0 {
                e.log.Info("embedded oracle price in block",
                        "height", height,
                        "oracle_price_fixed", oraclePrice,
                        "price_usd", float64(oraclePrice)/oraclePriceScale,
                )
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

        // Oracle price sanity check: reject blocks whose embedded price deviates
        // too far from our local oracle reading (BFT oracle enforcement).
        if err := e.checkOraclePriceDeviation(block.Header.OraclePrice); err != nil {
                return fmt.Errorf("oracle price check failed: %w", err)
        }

        if err := e.chain.AddBlock(block); err != nil {
                return fmt.Errorf("add block: %w", err)
        }

        // ── EIP-1559 base fee update ─────────────────────────────────────────────
        blockBaseFee := block.Header.BaseFee
        if blockBaseFee == 0 {
                blockBaseFee = core.InitialBaseFeePerByte
        }
        burnedNAPR, tipNAPR := blockFeeStats(block.Txs, blockBaseFee)
        newFee := nextBaseFee(blockBaseFee, block.Size())

        e.mu.Lock()
        e.baseFee = newFee
        // Accumulate tips so our next produced block's coinbase can pay them out.
        e.accumulatedTips += tipNAPR
        e.mu.Unlock()

        // Propagate new base fee to the mempool so incoming txs are validated at
        // the correct rate immediately.
        e.pool.SetBaseFee(newFee)

        if burnedNAPR > 0 || tipNAPR > 0 {
                e.log.Info("block fees",
                        "height", block.Header.Height,
                        "base_fee_per_byte", blockBaseFee,
                        "burned_napro", burnedNAPR,
                        "burned_apro", float64(burnedNAPR)/1e8,
                        "tip_napro", tipNAPR,
                        "tip_apro", float64(tipNAPR)/1e8,
                        "next_base_fee", newFee,
                )
        }

        // Process any stake transactions included in the block
        e.processStakeTxs(block)

        // At epoch boundaries, recompute the active validator set
        if e.cfg.Registry != nil && block.Header.Height%core.EpochLength == 0 {
                newSet := e.cfg.Registry.UpdateEpoch(block.Header.Height)
                active, total := e.cfg.Registry.Count()
                e.log.Info("epoch updated",
                        "height", block.Header.Height,
                        "epoch", block.Header.Height/core.EpochLength,
                        "active_validators", active,
                        "total_registered", total,
                        "new_set_size", len(newSet),
                )
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

        // Check if we've reached BFT threshold (2/3 of active validators)
        needed := int(float64(len(e.activeValidators()))*e.cfg.BFTThreshold) + 1
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
        vs := e.activeValidators()
        if len(vs) == 0 {
                return nil
        }
        return vs[int(round)%len(vs)]
}

// isKnownValidator returns true if pub is in the current active validator set.
func (e *Engine) isKnownValidator(pub crypto.ValidatorPubKey) bool {
        for _, v := range e.activeValidators() {
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

// Registry returns the validator registry (may be nil for legacy static config).
func (e *Engine) Registry() *core.ValidatorRegistry { return e.cfg.Registry }

// processStakeTxs scans a block for stake transactions and applies them to the registry.
func (e *Engine) processStakeTxs(block *core.Block) {
        if e.cfg.Registry == nil {
                return
        }
        for _, tx := range block.Txs {
                if !tx.IsStake() {
                        continue
                }
                if err := e.cfg.Registry.ProcessStakeTx(tx, block.Header.Height); err != nil {
                        e.log.Warn("stake tx rejected",
                                "height", block.Header.Height,
                                "tx", tx.Hash(),
                                "err", err,
                        )
                }
        }
}

// HandleVote processes a finalization vote (exported for testing and P2P).
func (e *Engine) HandleVote(vote FinalizeMsg) error {
        return e.handleVote(vote)
}
