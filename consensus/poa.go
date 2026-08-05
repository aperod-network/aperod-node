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
	"sync/atomic"
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
        MyKey *crypto.LockedValidatorKey
        // OnBlockProduced is an optional callback called after each block is added
        // to the chain. Use it to persist blocks to durable storage.
        OnBlockProduced func(block *core.Block)
        // OnBlockAccepted is an optional callback called after every canonical
        // block is committed — whether produced locally by this node or received
        // from a P2P peer.  Use it for work that must run regardless of the
        // block source (e.g. periodic snapshots).
        OnBlockAccepted func(block *core.Block)
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

        // txVerifier performs full cryptographic verification of incoming block txs.
        // If nil, cryptographic verification is skipped (dev/test only — never in production).
        txVerifier *core.TxVerifier
        // utxos is the live UTXO set kept in sync with the chain.
        // Required for double-spend detection via txVerifier.
        utxos *core.UTXOSet
        // pendingVoteHeight maps block hash → height for blocks with pending (non-finalized)
        // votes. Used to prune the votes map by height to prevent unbounded growth.
        pendingVoteHeight map[crypto.Hash32]uint64

        // timestampRejected counts how many incoming P2P blocks have been rejected by
        // the timejacking guard since node start.  Incremented atomically; safe to
        // read from outside the engine goroutine (e.g. the API server).
        timestampRejected int64

        // oracleConsecFails is the number of consecutive oracle fetch failures.
        // Incremented atomically on each failure in fetchOraclePrice; reset to 0 on
        // success.  When it exceeds oracleErrorThreshold the log level escalates from
        // Warn to Error so the alert reaches the Telegram admin notification channel.
        oracleConsecFails int64

        // cachedOraclePrice holds the last successfully fetched oracle price in
        // fixed-point units (price_usd × oraclePriceScale).  Updated by the
        // background oracle fetcher goroutine; read atomically by produceBlock so
        // block production is never gated on oracle HTTP latency.  0 means the price
        // is unavailable (oracle down or not yet fetched).
        cachedOraclePrice uint64

        // Channels for external events
        newBlockCh chan *core.Block  // incoming blocks from P2P
        newVoteCh  chan FinalizeMsg  // incoming finalization votes
        producedCh chan *core.Block  // blocks produced by this node (for broadcast)
}

// NewEngine creates a new PoA consensus engine.
func NewEngine(cfg Config, chain *core.Chain, pool *core.Mempool, log *slog.Logger) *Engine {
        e := &Engine{
                cfg:               cfg,
                chain:             chain,
                pool:              pool,
                log:               log,
                votes:             make(map[crypto.Hash32]map[string][]byte),
                finalized:         make(map[uint64]bool),
                pendingVoteHeight: make(map[crypto.Hash32]uint64),
                slashing:          newSlashingDetector(),
                baseFee:           core.InitialBaseFeePerByte,
                newBlockCh:        make(chan *core.Block, 64),
                newVoteCh:         make(chan FinalizeMsg, 256),
                producedCh:        make(chan *core.Block, 64),
        }
        // Seed the registry with genesis validators so they start Active.
        if cfg.Registry != nil && len(cfg.Validators) > 0 {
                genesisStake := core.MinStakeNAPR * 10 // genesis validators credited 10× min
                cfg.Registry.InitFromGenesis(cfg.Validators, genesisStake)
        }
        return e
}

// TimestampRejectedCount returns the total number of incoming P2P blocks rejected
// by the timejacking guard since this node started.  Safe to call from any goroutine.
func (e *Engine) TimestampRejectedCount() int64 {
        return atomic.LoadInt64(&e.timestampRejected)
}

// SetTxVerifier attaches a full cryptographic transaction verifier to the engine.
// Must be called before Run() in production deployments so that incoming P2P
// blocks are validated before being accepted into the chain.
// utxos must be the same UTXOSet passed to the verifier so it stays in sync.
func (e *Engine) SetTxVerifier(v *core.TxVerifier, utxos *core.UTXOSet) {
        e.txVerifier = v
        e.utxos = utxos
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

// runOracleFetcher is a background goroutine that polls fetchOraclePrice() once
// per BlockTime and stores the result in cachedOraclePrice atomically.
// This decouples oracle HTTP latency from block production: produceBlock reads
// the cached value instantly instead of making a blocking HTTP call.
// Stops when stop is closed.
func (e *Engine) runOracleFetcher(stop <-chan struct{}) {
        // Fetch immediately on startup so the first produced block has a price.
        atomic.StoreUint64(&e.cachedOraclePrice, e.fetchOraclePrice())

        ticker := time.NewTicker(e.cfg.BlockTime)
        defer ticker.Stop()
        for {
                select {
                case <-stop:
                        return
                case <-ticker.C:
                        atomic.StoreUint64(&e.cachedOraclePrice, e.fetchOraclePrice())
                }
        }
}

// Run starts the consensus loop. Blocks until ctx is done.
func (e *Engine) Run(stop <-chan struct{}) {
        ticker := time.NewTicker(e.cfg.BlockTime)
        defer ticker.Stop()

        // Start background oracle price fetcher so produceBlock never blocks on
        // oracle HTTP latency.  Only started when an oracle URL is configured.
        if e.cfg.OracleURL != "" {
                go e.runOracleFetcher(stop)
        }

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

        // Sanity-check the block we just produced against the same coinbase policy
        // that peer blocks must satisfy.  Should always pass for engine-produced
        // blocks; a failure here indicates a bug in produceBlock itself.
        if err := validateCoinbasePolicy(block); err != nil {
                return fmt.Errorf("self-produced block %d failed coinbase policy (bug in produceBlock): %w",
                        block.Header.Height, err)
        }

        // Fast pre-check: detect invalid stake txs (bad sig, missing UTXO, duplicate
        // burn UTXO, below-minimum, etc.) before touching any state.  If validation
        // fails, evict ALL stake txs from this block so the next produced block does
        // not re-select them and cause an infinite production failure.
        if e.cfg.Registry != nil {
                if err := e.cfg.Registry.ValidateBlockStakeTxs(block.Txs, block.Header.Height); err != nil {
                        e.log.Warn("self-produced block contains invalid stake tx(s); evicting all stake txs from mempool",
                                "height", block.Header.Height, "err", err)
                        for _, tx := range block.Txs {
                                if tx.IsStake() {
                                        e.pool.Remove(tx.Hash())
                                }
                        }
                        return fmt.Errorf("self-produced block %d: invalid stake tx (all stake txs evicted): %w",
                                block.Header.Height, err)
                }
        }

        // Apply UTXO outputs BEFORE stake application and chain insertion.
        if e.utxos != nil {
                if err := e.utxos.ApplyBlock(block); err != nil {
                        return fmt.Errorf("utxo apply failed for self-produced block: %w", err)
                }
        }

        // Atomically apply stake txs (registry + UTXO staking) BEFORE AddBlock so
        // that any application failure prevents chain insertion — no post-insertion
        // error swallowing.  ApplyBlockStakeTxs holds r.mu for the whole batch so
        // a concurrent oracle UpdateMinStake cannot change effectiveMin mid-apply.
        var stakeRollback func()
        if e.cfg.Registry != nil {
                var applyErr error
                stakeRollback, applyErr = e.cfg.Registry.ApplyBlockStakeTxs(block.Txs, block.Header.Height)
                if applyErr != nil {
                        // Rollback UTXO outputs; stake registry was not mutated on error.
                        if e.utxos != nil {
                                if rbErr := e.utxos.RollbackBlock(block); rbErr != nil {
                                        e.log.Error("UTXO rollback failed after ApplyBlockStakeTxs error (self-produced)",
                                                "height", block.Header.Height, "err", rbErr)
                                }
                        }
                        // Evict the offending stake txs so they are not re-selected.
                        for _, tx := range block.Txs {
                                if tx.IsStake() {
                                        e.pool.Remove(tx.Hash())
                                }
                        }
                        return fmt.Errorf("self-produced block %d: stake apply failed (txs evicted): %w",
                                block.Header.Height, applyErr)
                }
        }

        // Add to canonical chain.  On failure, rollback BOTH UTXO outputs and stake state.
        if err := e.chain.AddBlock(block); err != nil {
                if e.utxos != nil {
                        if rbErr := e.utxos.RollbackBlock(block); rbErr != nil {
                                e.log.Error("UTXO rollback failed after chain.AddBlock error (self-produced)",
                                        "height", block.Header.Height,
                                        "chain_err", err, "rollback_err", rbErr)
                        }
                }
                if stakeRollback != nil {
                        stakeRollback()
                }
                return fmt.Errorf("add produced block: %w", err)
        }
        // Stake txs already applied above — no processStakeTxs call needed.
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
        // Notify any block-source-agnostic listeners (e.g. periodic snapshotting).
        if e.cfg.OnBlockAccepted != nil {
                e.cfg.OnBlockAccepted(block)
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

// DefaultBlockRewardNAPR is the block reward paid to the producing validator
// when no override is set in Config.BlockRewardNAPR.
// 5 APRO in base units: 1 APRO = 100_000_000 nAPRO → 5 APRO = 500_000_000 nAPRO.
// At 3 s/block this yields ~52,560,000 APRO/year across all 21 validators.
// Source of truth: deploy/BURN_POLICY.md — "Block reward: 5 APRO per block".
const DefaultBlockRewardNAPR uint64 = 500_000_000

// HalvingIntervalBlocks is the number of blocks between each block-reward
// halving event.  At 3 s/block, 21 024 000 blocks ≈ 2 years.
// Must match the "Halving interval" row in deploy/VALIDATORS.md and
// the "Halving interval" row in deploy/BURN_POLICY.md.
const HalvingIntervalBlocks uint64 = 21_024_000

// defaultBlockRewardNAPR is an unexported alias kept for backward-compat
// within this package.  External code should use DefaultBlockRewardNAPR.
const defaultBlockRewardNAPR = DefaultBlockRewardNAPR

// blockRewardAtHeight returns the coinbase reward in nAPRO for a block at the
// given height, applying halvings per HalvingIntervalBlocks.
// Era 0: DefaultBlockRewardNAPR; era 1: /2; era 2: /4; …
// The reward is halved at most 63 times (era ≥ 64 pays 0 to avoid overflow).
func blockRewardAtHeight(base uint64, height uint64) uint64 {
        era := height / HalvingIntervalBlocks
        if era >= 64 {
                return 0
        }
        return base >> era
}

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

// oracleErrorThreshold is the number of consecutive oracle fetch failures after
// which fetchOraclePrice escalates its log message from Warn to Error.  An Error
// is visible in structured-log pipelines that feed the Telegram admin channel,
// whereas a Warn may be silently filtered.
const oracleErrorThreshold int64 = 10

// oraclePriceResponse is the minimal subset of the oracle API JSON we need.
type oraclePriceResponse struct {
        PriceUSD float64 `json:"price_usd"`
}

// fetchOraclePrice calls cfg.OracleURL and returns the embedded fixed-point price.
// Returns 0 if OracleURL is empty, the request fails, or the price is zero/negative.
//
// Failure escalation: on each consecutive failure the internal oracleConsecFails
// counter is incremented atomically.  Once the counter exceeds oracleErrorThreshold
// the log level escalates from Warn to Error so the message is visible in
// structured-log pipelines that feed the Telegram admin notification channel.
// The counter is reset to 0 on any successful fetch.
func (e *Engine) fetchOraclePrice() uint64 {
        if e.cfg.OracleURL == "" {
                return 0
        }
        client := &http.Client{Timeout: 3 * time.Second}
        resp, err := client.Get(e.cfg.OracleURL)
        if err != nil {
                fails := atomic.AddInt64(&e.oracleConsecFails, 1)
                if fails > oracleErrorThreshold {
                        e.log.Error("oracle price fetch failed (consecutive failures exceed threshold — check oracle_url in node.yaml)",
                                "url", e.cfg.OracleURL, "err", err, "consecutive_failures", fails)
                } else {
                        e.log.Warn("oracle price fetch failed", "url", e.cfg.OracleURL, "err", err, "consecutive_failures", fails)
                }
                return 0
        }
        defer resp.Body.Close()
        var p oraclePriceResponse
        if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
                fails := atomic.AddInt64(&e.oracleConsecFails, 1)
                if fails > oracleErrorThreshold {
                        e.log.Error("oracle price decode failed (consecutive failures exceed threshold — check oracle_url in node.yaml)",
                                "err", err, "consecutive_failures", fails)
                } else {
                        e.log.Warn("oracle price decode failed", "err", err, "consecutive_failures", fails)
                }
                return 0
        }
        if p.PriceUSD <= 0 {
                return 0
        }
        // Successful fetch: reset consecutive-failure counter.
        atomic.StoreInt64(&e.oracleConsecFails, 0)
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

// maxCoinbasesPerBlock is the hard limit on zero-input (coinbase) transactions
// per block: 1 engine reward + up to 10 privileged admin mints.
const maxCoinbasesPerBlock = 11

// validateCoinbasePolicy checks coinbase rules for every accepted block:
//
//  1. At most maxCoinbasesPerBlock coinbase transactions.
//  2. All coinbase transactions must form a contiguous prefix (appear before
//     any non-coinbase transaction).
//
// The reward-amount cap cannot be checked here because the Pedersen-committed
// output amount is hidden.  The cap is enforced structurally: external coinbases
// are blocked by mempool.Add; the engine-reward coinbase is capped by
// blockRewardAtHeight; and admin mints go through AddPrivileged which is only
// reachable from the localhost-only admin RPC.
//
// Stake transactions (TxVersionStake) have zero inputs but are NOT coinbases —
// they do not create supply and are exempt from this policy.
func validateCoinbasePolicy(block *core.Block) error {
        count := 0
        seenNonCoinbase := false
        for i, tx := range block.Txs {
                if tx.IsCoinbase() && !tx.IsStake() {
                        count++
                        if seenNonCoinbase {
                                return fmt.Errorf("coinbase tx at index %d appears after a non-coinbase tx "+
                                        "(all coinbases must form a block prefix)", i)
                        }
                } else {
                        seenNonCoinbase = true
                }
        }
        if count > maxCoinbasesPerBlock {
                return fmt.Errorf("block contains %d coinbase transactions (maximum %d)",
                        count, maxCoinbasesPerBlock)
        }
        return nil
}

// produceBlock assembles a new block from the mempool.
func (e *Engine) produceBlock(height, round uint64, parent *core.Block) (*core.Block, error) {
        raw := e.pool.SelectTxs(2000) // up to 2000 txs per block (verifier hard limit)

        // Defense-in-depth: strip any NON-PRIVILEGED coinbase (zero-input) txs that
        // may have bypassed the mempool guard.  mempool.Add() already rejects
        // external coinbases, so finding one here indicates a bug; log and drop it.
        // PRIVILEGED coinbases (added via mempool.AddPrivileged, e.g. admin mints)
        // are intentional and must pass through to the block.
        //
        // Stake transactions (TxVersionStake) also have zero inputs — they carry
        // their payload in Extra, not in Inputs.  They are NOT coinbases and must
        // NOT be dropped here; they enter the pool via the public POST /api/v1/stake
        // endpoint and must survive into the block for ProcessStakeTx to apply them.
        txs := raw[:0]
        for _, tx := range raw {
                if tx.IsCoinbase() && !tx.IsStake() {
                        h := tx.Hash()
                        if !e.pool.IsPrivileged(h) {
                                e.log.Warn("produceBlock: dropping unexpected non-privileged coinbase from pool",
                                        "hash", h)
                                continue
                        }
                        // Privileged admin mint — keep it.
                }
                txs = append(txs, tx)
        }

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
                baseReward := e.cfg.BlockRewardNAPR
                if baseReward == 0 {
                        baseReward = defaultBlockRewardNAPR
                }
                // Apply halving schedule: reward halves every HalvingIntervalBlocks.
                // See deploy/BURN_POLICY.md for the emission schedule table.
                rewardNAPR := blockRewardAtHeight(baseReward, height)
                // Validator earns block reward + priority tips from all txs in this block.
                totalReward := rewardNAPR + tips
                mintTx, err := core.BuildMintTx(crypto.Address(e.cfg.RewardAddress), totalReward, height)
                if err != nil {
                        e.log.Warn("failed to build coinbase reward tx", "err", err)
                } else {
                        txs = append([]core.Transaction{*mintTx}, txs...)
                }
        }

        // Read the last price fetched by the background oracle goroutine.
        // This is an atomic load — never blocks on network I/O.
        oraclePrice := atomic.LoadUint64(&e.cachedOraclePrice)

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
        if err := header.Sign(e.cfg.MyKey.PrivKey()); err != nil {
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
// maxClockSkewNs is the maximum allowed difference (in nanoseconds) between a
// peer-submitted block's timestamp and our local wall clock.  Blocks whose
// timestamp drifts more than ±15 s are rejected to prevent timejacking attacks
// where a malicious peer shifts the chain tip into the far future, causing
// legitimate blocks to be rejected as "too old" (#418).
const maxClockSkewNs = int64(15 * 1_000_000_000)

func (e *Engine) handleIncomingBlock(block *core.Block) error {
        // Basic structural validation
        if err := block.Validate(); err != nil {
                return fmt.Errorf("invalid block: %w", err)
        }

        // Timejacking guard (#418): reject blocks whose timestamp is too far in
        // the FUTURE relative to the local wall clock.  Only future timestamps
        // are an attack vector — a malicious peer shifts the chain tip into the
        // far future so that legitimate blocks produced at time.Now() are then
        // rejected as "too old".  Historical sync blocks always have past
        // timestamps and must never be rejected by this check; applying a ±
        // window would prevent relay nodes from catching up after a gap.
        nowNs := time.Now().UTC().UnixNano()
        futureSkewNs := block.Header.Timestamp - nowNs
        if futureSkewNs > maxClockSkewNs {
                atomic.AddInt64(&e.timestampRejected, 1)
                return fmt.Errorf("block %d: timestamp too far in the future (skew +%dms, max %dms)",
                        block.Header.Height, futureSkewNs/1_000_000, maxClockSkewNs/1_000_000)
        }

        // Check proposer is a known validator
        if !e.isKnownValidator(block.Header.ValidatorPub) {
                return fmt.Errorf("block from unknown validator %s", block.Header.ValidatorPub.ID())
        }

        // Proposer-slot check: only the scheduled proposer may produce this round's block.
        // Without this any known validator could front-run the legitimate proposer,
        // capture all block rewards, and censor transactions.
        if expected := e.proposerAt(block.Header.Round); expected != nil {
                if !expected.Equals(block.Header.ValidatorPub) {
                        return fmt.Errorf("block round %d: producer %s is not the scheduled proposer %s",
                                block.Header.Round, block.Header.ValidatorPub.ID(), expected.ID())
                }
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

        // Block-level key-image uniqueness check: detect duplicate key images
        // across ALL transactions in the block before touching the UTXO set.
        // TxVerifier.VerifyTx only catches duplicates within a single transaction;
        // two separate txs in the same block can both reuse the same key image and
        // each pass per-tx verification. This check closes that gap.
        if err := blockKeyImageCheck(block); err != nil {
                return fmt.Errorf("block key-image check failed: %w", err)
        }

        // Coinbase policy: at most one coinbase per block, at index 0.
        // This closes the free-mint attack: a block produced by a malicious or
        // compromised proposer that includes multiple zero-input transactions
        // (each creating new UTXOs) is rejected before any UTXO state is applied.
        if err := validateCoinbasePolicy(block); err != nil {
                return fmt.Errorf("block %d: coinbase policy violation: %w", block.Header.Height, err)
        }

        // Pre-acceptance stake transaction validation: ordered, stateful dry-run of
        // all stake txs before UTXO state is applied or the block is inserted.
        // TxVerifier.VerifyBlock skips stake txs (no ring-sig inputs), so this is
        // the only place where signature, UTXO existence, commitment correctness,
        // duplicate-burn-UTXO within the block, and stateful registry checks (min
        // stake, cap, unbonding state, top-up overflow) are enforced for incoming
        // blocks.  A failing check rejects the block before any state mutation.
        if e.cfg.Registry != nil {
                if err := e.cfg.Registry.ValidateBlockStakeTxs(block.Txs, block.Header.Height); err != nil {
                        return fmt.Errorf("block %d: stake validation failed: %w", block.Header.Height, err)
                }
        }

        // Full cryptographic transaction verification: ring sigs, range proofs,
        // Pedersen commitment balance, and double-spend against historical UTXO set.
        // This MUST run before UTXO application and chain insertion.
        //
        // Fail-closed: a nil verifier is a misconfiguration, not an optional path.
        // Accepting blocks without verification would silently allow inflation and
        // forged-signature attacks — reject rather than warn-and-continue.
        if e.txVerifier == nil {
                return fmt.Errorf("tx verifier not configured: refusing to accept block %d without cryptographic verification",
                        block.Header.Height)
        }
        if err := e.txVerifier.VerifyBlock(block); err != nil {
                return fmt.Errorf("tx crypto verification failed: %w", err)
        }

        // Apply UTXO outputs BEFORE stake application and chain insertion.
        if e.utxos != nil {
                if err := e.utxos.ApplyBlock(block); err != nil {
                        return fmt.Errorf("utxo state transition rejected block: %w", err)
                }
        }

        // Atomically apply stake txs (registry + UTXO staking) BEFORE AddBlock.
        // ApplyBlockStakeTxs re-validates under a held registry write lock so a
        // concurrent oracle UpdateMinStake cannot slip in between the earlier dry-run
        // and actual application.  Any failure here rolls back UTXO outputs and
        // rejects the block without ever touching the canonical chain.
        var stakeRollback func()
        if e.cfg.Registry != nil {
                var applyErr error
                stakeRollback, applyErr = e.cfg.Registry.ApplyBlockStakeTxs(block.Txs, block.Header.Height)
                if applyErr != nil {
                        if e.utxos != nil {
                                if rbErr := e.utxos.RollbackBlock(block); rbErr != nil {
                                        e.log.Error("UTXO rollback failed after ApplyBlockStakeTxs error",
                                                "height", block.Header.Height, "err", rbErr)
                                }
                        }
                        return fmt.Errorf("block %d: stake apply failed (block rejected): %w",
                                block.Header.Height, applyErr)
                }
        }

        if err := e.chain.AddBlock(block); err != nil {
                // Chain insertion failed after UTXO and stake state were already updated.
                // Roll back both to keep all state consistent.
                if e.utxos != nil {
                        if rbErr := e.utxos.RollbackBlock(block); rbErr != nil {
                                e.log.Error("UTXO rollback failed after chain.AddBlock error",
                                        "height", block.Header.Height,
                                        "chain_err", err, "rollback_err", rbErr)
                        }
                }
                if stakeRollback != nil {
                        stakeRollback()
                }
                return fmt.Errorf("add block: %w", err)
        }
        // Stake txs already applied above — no processStakeTxs call needed.

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

        // Notify any block-source-agnostic listeners (e.g. periodic snapshotting).
        if e.cfg.OnBlockAccepted != nil {
                e.cfg.OnBlockAccepted(block)
        }

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
                // Record the height for this block hash so we can prune by height.
                e.pendingVoteHeight[vote.BlockHash] = vote.Height
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
                // Clean up vote records for this block once finalized.
                delete(e.votes, vote.BlockHash)
                delete(e.pendingVoteHeight, vote.BlockHash)
        }

        // Prune vote records and finalized entries for heights far below the
        // current tip to prevent unbounded growth when blocks never reach quorum
        // (e.g. due to network partitions or validator churn).
        // Keep the most recent 256 heights of pending votes and finalized entries.
        const votePruneDepth uint64 = 256
        if vote.Height > votePruneDepth {
                pruneBelow := vote.Height - votePruneDepth
                for bh, ht := range e.pendingVoteHeight {
                        if ht < pruneBelow {
                                delete(e.votes, bh)
                                delete(e.pendingVoteHeight, bh)
                        }
                }
                for h := range e.finalized {
                        if h < pruneBelow {
                                delete(e.finalized, h)
                        }
                }
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

// blockKeyImageCheck detects duplicate key images across ALL transactions in a
// block.  TxVerifier.VerifyTx only catches duplicates within a single
// transaction; two distinct txs in the same block can each carry the same key
// image and both pass per-tx verification.  This function closes that gap by
// collecting every key image from every spending transaction in the block and
// rejecting the block if any appears more than once.
func blockKeyImageCheck(block *core.Block) error {
        seen := make(map[crypto.KeyImage]int) // ki → first tx index
        for txIdx, tx := range block.Txs {
                if tx.IsCoinbase() || tx.IsStake() {
                        continue
                }
                for _, inp := range tx.Inputs {
                        if first, dup := seen[inp.KeyImage]; dup {
                                return fmt.Errorf(
                                        "block %d: duplicate key image %x in tx[%d] already seen in tx[%d]",
                                        block.Header.Height, inp.KeyImage[:8], txIdx, first,
                                )
                        }
                        seen[inp.KeyImage] = txIdx
                }
        }
        return nil
}
