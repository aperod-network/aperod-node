// Package consensus implements Aperod's Proof of Authority (PoA) consensus engine.
// Validators are a hardcoded list from genesis. Block production uses round-robin
// slot assignment. Finalization requires 2/3 of validators to sign (BFT threshold).
package consensus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// AdminMintRecordStore persists the idempotency state machine for admin mints.
// It is separate from the concrete Store so tests and alternate durable stores
// can precisely control these state transitions without affecting staking data.
type AdminMintRecordStore interface {
	StoreAdminMintRecord(idempotencyKey string, record store.AdminMintRecord) error
	LoadAdminMintRecord(idempotencyKey string) (record store.AdminMintRecord, found bool, err error)
}

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
	// to the chain locally. Use it for source-specific notification such as
	// gossip; canonical persistence belongs in OnCanonicalBlock.
	OnBlockProduced func(block *core.Block) error
	// OnCanonicalBlock is the required durability boundary for every accepted
	// block, local or P2P. The prepared AVM write set must be committed in the
	// same atomic batch as block, height index and tip.
	OnCanonicalBlock func(block *core.Block, prepared *avm.PreparedBlock) error
	// OnBlockAccepted is an optional callback called after every canonical
	// block is committed — whether produced locally by this node or received
	// from a P2P peer.  Use it for work that must run regardless of the
	// block source (e.g. periodic snapshots).
	OnBlockAccepted func(block *core.Block)
	// RewardAddress is the APRO wallet address that receives block rewards.
	// If empty, no coinbase transaction is added to produced blocks.
	RewardAddress string
	// BlockRewardNAPR is the block reward in base units (nAPRO).
	// In pool mode the deployed value is 300_000_000 nAPRO = 3 APRO.
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

	// TimestampBanThreshold is the number of future-timestamped blocks that
	// must be received from the same validator public key before OnTimestampBan
	// is called.  0 disables per-validator ban tracking (the global
	// TimestampRejectedCount counter is always incremented regardless).
	// Default when left at 0: ban callback is never invoked.
	// Recommended production value: 5.
	TimestampBanThreshold int
	// OnTimestampBan, when non-nil, is called the first time a validator's
	// per-validator timestamp-rejection count crosses TimestampBanThreshold,
	// and again on every subsequent rejection from the same validator.
	// pub identifies the offending block signer; count is the total number
	// of future-timestamped blocks received from that validator since start.
	// The callback is invoked from the engine goroutine; it must not block.
	// Typical implementation: disconnect and ban the peer by source IP.
	OnTimestampBan func(pub crypto.ValidatorPubKey, count int)

	// StakingPoolNAPR is the total pre-allocated validator reward pool in nAPRO.
	// When > 0, block rewards are drawn from this pool rather than minted as new
	// tokens, keeping Total Supply at 10 B (deflationary from day 1).
	// After exhaustion, TailRewardNAPR is minted per block instead.
	// Set to 0 to disable pool-based rewards.
	StakingPoolNAPR uint64

	// TailRewardNAPR is the per-block mint in nAPRO once the pool is exhausted.
	// 0 defaults to defaultTailRewardNAPR (100_000_000 = 1 APRO/block).
	TailRewardNAPR uint64

	// Store is the LevelDB store used to persist the pool balance across restarts.
	// When nil the pool state is held in memory only (lost on restart).
	Store *store.DB
	// AdminMintStore optionally overrides Store for durable admin-mint
	// idempotency records. When nil, Store is used.
	AdminMintStore AdminMintRecordStore
	// RingCTV4ActivationHeight is the first height at which v4 RingCT
	// transactions are accepted. Zero means active from genesis.
	RingCTV4ActivationHeight uint64
	// RingCTCLSAGActivationHeight is the first height requiring v5; zero
	// disables activation to avoid an accidental uncoordinated fork.
	RingCTCLSAGActivationHeight uint64
	// AVMActivationHeight is the first height accepting v6 AVM transactions.
	// Zero disables AVM consensus.
	AVMActivationHeight uint64
	// AVMExecutor performs deterministic block preparation. It is mandatory
	// once AVMActivationHeight is reached.
	AVMExecutor *avm.BlockExecutor
	// RewardAuthorizationActivationHeight is the first height at which
	// validator rewards must carry a consensus-verifiable on-chain
	// authorization. Zero keeps the authorization feature disabled.
	RewardAuthorizationActivationHeight uint64
}

// FinalizeMsg is a vote by a validator to finalize a block.
type FinalizeMsg struct {
	BlockHash    crypto.Hash32
	Height       uint64
	ValidatorPub crypto.ValidatorPubKey
	Signature    []byte
}

// Engine is the PoA consensus engine.
type Engine struct {
	cfg   Config
	chain *core.Chain
	pool  *core.Mempool
	log   *slog.Logger

	mu sync.Mutex
	// votes collected for each block hash: pubkey hex → signature
	votes map[crypto.Hash32]map[string][]byte
	// finalized tracks which heights have been finalized
	finalized map[uint64]bool
	// slashing detector for double-sign evidence
	slashing *slashingDetector
	// baseFee is the current EIP-1559 base fee per byte (nAPRO/byte).
	// Updated after every accepted block; embedded in every produced block header.
	baseFee uint64
	// halted is set when a transactional rollback cannot be persisted.  The
	// process stays available for diagnostics but refuses all further consensus
	// transitions until an operator restarts it after repairing storage.
	halted atomic.Bool
	// txVerifier performs full cryptographic verification of incoming block txs.
	// If nil, cryptographic verification is skipped (dev/test only — never in production).
	txVerifier *core.TxVerifier
	// utxos is the live UTXO set kept in sync with the chain.
	// Required for double-spend detection via txVerifier.
	utxos *core.UTXOSet
	// pendingVoteHeight maps block hash → height for blocks with pending (non-finalized)
	// votes. Used to prune the votes map by height to prevent unbounded growth.
	pendingVoteHeight map[crypto.Hash32]uint64

	// staking pool state ─────────────────────────────────────────────────────
	// stakingPoolInit is the initial pool size in nAPRO (0 = pool disabled).
	stakingPoolInit uint64
	// stakingPoolRemaining is the current pool balance in nAPRO.
	// Updated atomically; -1 means "pool disabled" (stakingPoolInit == 0).
	stakingPoolRemaining int64
	// tailRewardNAPR is the per-block mint once the pool is exhausted.
	tailRewardNAPR uint64
	// store is the LevelDB backing for pool persistence across restarts.
	store *store.DB
	// adminMintStore is the durable idempotency store for admin mints.
	adminMintStore AdminMintRecordStore

	// timestampRejected counts how many incoming P2P blocks have been rejected by
	// the timejacking guard since node start.  Incremented atomically; safe to
	// read from outside the engine goroutine (e.g. the API server).
	timestampRejected int64

	// tsMu guards tsPerValidator so concurrent reads (API) cannot race the
	// engine goroutine's writes inside handleIncomingBlock.
	tsMu sync.Mutex
	// tsPerValidator tracks how many future-timestamped blocks the engine has
	// received per block signer (keyed by hex-encoded ValidatorPub ID).
	// Only populated when cfg.TimestampBanThreshold > 0.
	tsPerValidator map[string]int

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
	newBlockCh chan *core.Block // incoming blocks from P2P
	newVoteCh  chan FinalizeMsg // incoming finalization votes
	producedCh chan *core.Block // blocks produced by this node (for broadcast)

	// ── Admin mint scheduling ────────────────────────────────────────────────
	// Admin mints must be built at block-production time so the one-time pub
	// is spend_pub + height*G with the REAL inclusion height.  The legacy path
	// (BuildMintTx with height=0 via the mempool) gave every mint to the same
	// address the same one-time pub — and therefore the same key image — so a
	// single spent/phantom key-image entry permanently blocked all future
	// mints to that address.
	mintMu       sync.Mutex
	mintQueue    []*adminMintReq // waiting to be included in a produced block
	mintInFlight []*adminMintReq // included in a produced-but-not-yet-committed block
	mintByKey    map[string]*adminMintReq
}

// rollbackUnpersistedBlock reverses the in-memory parts of a canonical
// transition when the atomic durability callback rejects the block. The engine
// still halts after a persistence error, but API readers must never observe a
// chain/UTXO/stake state that the database did not commit.
func (e *Engine) rollbackUnpersistedBlock(block *core.Block, stakeRollback func()) error {
	var rollbackErrors []error
	if err := e.chain.RollbackLastBlock(block); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("chain rollback: %w", err))
	}
	if e.utxos != nil {
		if err := e.utxos.RollbackBlock(block); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("UTXO rollback: %w", err))
		}
	}
	if stakeRollback != nil {
		stakeRollback()
	}
	return errors.Join(rollbackErrors...)
}

func (e *Engine) failCanonicalPersistence(
	block *core.Block,
	stakeRollback func(),
	persistErr error,
) error {
	rollbackErr := e.rollbackUnpersistedBlock(block, stakeRollback)
	e.resolveAdminMintsPersistenceFailed(persistErr)
	e.halted.Store(true)
	if rollbackErr != nil {
		return fmt.Errorf("%w; fatal in-memory rollback failure: %v", persistErr, rollbackErr)
	}
	return persistErr
}

// adminMintOutcome is delivered on adminMintReq.result once the mint's block
// is committed to the chain (or the build failed).
type adminMintOutcome struct {
	txHash crypto.Hash32
	height uint64
	err    error
}

// adminMintReq is one queued admin mint. ready is closed after outcome is set,
// allowing any number of callers sharing the key to observe the same result.
type adminMintReq struct {
	key     string
	addr    string
	amount  uint64
	ready   chan struct{}
	outcome adminMintOutcome
	done    bool
	txHash  crypto.Hash32
	height  uint64
}

// defaultTailRewardNAPR is the per-block mint once the staking pool is exhausted.
// 1 APRO = 100_000_000 nAPRO.  Chosen as a minimal tail emission that keeps
// validators incentivised without significant inflationary pressure once
// EIP-1559 fee burns are non-trivial.
const defaultTailRewardNAPR uint64 = 100_000_000

// DefaultPoolBlockRewardNAPR is the deployed pool-phase reward:
// 3 APRO per block, drawn from the pre-allocated 2B APRO staking pool.
const DefaultPoolBlockRewardNAPR uint64 = 300_000_000

// NewEngine creates a new PoA consensus engine.
func NewEngine(cfg Config, chain *core.Chain, pool *core.Mempool, log *slog.Logger) *Engine {
	adminMintStore := cfg.AdminMintStore
	if adminMintStore == nil && cfg.Store != nil {
		adminMintStore = cfg.Store
	}
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
		newBlockCh:        make(chan *core.Block, 600),
		newVoteCh:         make(chan FinalizeMsg, 256),
		producedCh:        make(chan *core.Block, 64),
		tsPerValidator:    make(map[string]int),
		mintByKey:         make(map[string]*adminMintReq),
		store:             cfg.Store,
		adminMintStore:    adminMintStore,
	}
	// Seed the registry with genesis validators so they start Active.
	if cfg.Registry != nil && len(cfg.Validators) > 0 {
		genesisStake := core.MinStakeNAPR * 10 // genesis validators credited 10× min
		cfg.Registry.InitFromGenesis(cfg.Validators, genesisStake)
	}
	// ── Staking pool initialisation ──────────────────────────────────────────
	if cfg.StakingPoolNAPR > 0 {
		e.stakingPoolInit = cfg.StakingPoolNAPR
		e.tailRewardNAPR = cfg.TailRewardNAPR
		if e.tailRewardNAPR == 0 {
			e.tailRewardNAPR = defaultTailRewardNAPR
		}
		// Try to restore persisted pool balance from LevelDB so restarts
		// do not re-initialise to the full 2 B.
		if cfg.Store != nil {
			if rem, found, err := cfg.Store.LoadStakingPoolRemaining(); err != nil {
				log.Warn("staking pool: failed to load from store; using config value", "err", err)
				atomic.StoreInt64(&e.stakingPoolRemaining, int64(cfg.StakingPoolNAPR))
			} else if found {
				atomic.StoreInt64(&e.stakingPoolRemaining, int64(rem))
				log.Info("staking pool: restored from store",
					"remaining_napro", rem,
					"remaining_apro", float64(rem)/1e8,
				)
			} else {
				// First boot with pool enabled — persist initial balance.
				atomic.StoreInt64(&e.stakingPoolRemaining, int64(cfg.StakingPoolNAPR))
				_ = cfg.Store.StoreStakingPoolRemaining(cfg.StakingPoolNAPR)
				log.Info("staking pool: initialised",
					"total_napro", cfg.StakingPoolNAPR,
					"total_apro", float64(cfg.StakingPoolNAPR)/1e8,
				)
			}
		} else {
			atomic.StoreInt64(&e.stakingPoolRemaining, int64(cfg.StakingPoolNAPR))
		}
	} else {
		// Pool disabled — -1 signals legacy mint behaviour.
		atomic.StoreInt64(&e.stakingPoolRemaining, -1)
	}
	// Reconstruct the fee expected for the block after the current tip.  Without
	// this restart path every node would reset to the genesis fee and could
	// disagree with peers that stayed online.
	if chain != nil {
		if tip := chain.Tip(); tip != nil && tip.Header.Height > 0 {
			tipBaseFee := tip.Header.BaseFee
			if tipBaseFee == 0 {
				tipBaseFee = core.InitialBaseFeePerByte
			}
			e.baseFee = nextBaseFee(tipBaseFee, tip.Size())
		}
	}
	if pool != nil {
		pool.SetBaseFee(e.baseFee)
	}
	return e
}

// ─── Staking pool accessors ───────────────────────────────────────────────────

// StakingPoolRemaining returns the current pool balance in nAPRO.
// Returns math.MaxUint64 when the pool is disabled (legacy mint mode).
func (e *Engine) StakingPoolRemaining() uint64 {
	v := atomic.LoadInt64(&e.stakingPoolRemaining)
	if v < 0 {
		return math.MaxUint64
	}
	return uint64(v)
}

// StakingPoolInit returns the initial pool size in nAPRO (0 if disabled).
func (e *Engine) StakingPoolInit() uint64 { return e.stakingPoolInit }

// RewardMode returns "pool" when drawing from the pre-allocated staking pool,
// "tail_emission" when the pool is exhausted and rewards are minted, or
// "legacy_mint" when the pool feature is disabled entirely.
func (e *Engine) RewardMode() string {
	v := atomic.LoadInt64(&e.stakingPoolRemaining)
	switch {
	case v < 0:
		return "legacy_mint"
	case v == 0:
		return "tail_emission"
	default:
		return "pool"
	}
}

// CurrentBlockRewardNAPR returns the base reward for the next block.
// Reward authorization changes how the reward is authenticated, not its
// economics: the amount remains the pool draw or tail emission.
func (e *Engine) CurrentBlockRewardNAPR() uint64 {
	height := e.chain.Height() + 1
	return e.blockRewardNAPRAt(height)
}

func (e *Engine) blockRewardNAPRAt(height uint64) uint64 {
	base := e.cfg.BlockRewardNAPR
	if base == 0 {
		if e.stakingPoolInit > 0 {
			base = DefaultPoolBlockRewardNAPR
		} else {
			base = defaultBlockRewardNAPR
		}
	}
	if e.stakingPoolInit > 0 && atomic.LoadInt64(&e.stakingPoolRemaining) == 0 {
		return e.tailRewardNAPR
	}
	if e.stakingPoolInit > 0 {
		remaining := atomic.LoadInt64(&e.stakingPoolRemaining)
		if remaining > 0 && uint64(remaining) < base {
			return uint64(remaining)
		}
		return base
	}
	return blockRewardAtHeight(base, height)
}

// DecrementPool draws one block-reward from the staking pool.
// Must be called exactly once per accepted block (in OnBlockAccepted) so that
// the pool balance tracks the real chain state regardless of whether the block
// was produced locally or received from a peer.
// Safe to call concurrently; uses atomic CAS.  Does nothing when pool is disabled.
func (e *Engine) DecrementPool(height uint64) {
	if e.stakingPoolInit == 0 {
		return // pool disabled
	}
	baseReward := e.cfg.BlockRewardNAPR
	if baseReward == 0 {
		baseReward = DefaultPoolBlockRewardNAPR
	}
	amount := baseReward
	if amount == 0 {
		return
	}
	for {
		cur := atomic.LoadInt64(&e.stakingPoolRemaining)
		if cur <= 0 {
			return // already exhausted
		}
		deduct := amount
		if uint64(cur) < deduct {
			deduct = uint64(cur) // clamp to remaining
		}
		next := int64(uint64(cur) - deduct)
		if atomic.CompareAndSwapInt64(&e.stakingPoolRemaining, cur, next) {
			if e.store != nil {
				if err := e.store.StoreStakingPoolRemaining(uint64(next)); err != nil {
					e.log.Warn("staking pool: failed to persist remaining", "height", height, "err", err)
				}
			}
			if next == 0 {
				e.log.Info("staking pool exhausted — tail emission now active",
					"height", height,
					"tail_reward_napro", e.tailRewardNAPR,
					"tail_reward_apro", float64(e.tailRewardNAPR)/1e8,
				)
			}
			return
		}
	}
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
	if e.halted.Load() {
		return fmt.Errorf("consensus engine halted after persistent rollback failure")
	}
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
	if err := e.validateCoinbasePolicy(block); err != nil {
		return fmt.Errorf("self-produced block %d failed coinbase policy (bug in produceBlock): %w",
			block.Header.Height, err)
	}
	if err := e.validateBlockEconomics(block); err != nil {
		return fmt.Errorf("self-produced block %d failed fee policy: %w",
			block.Header.Height, err)
	}
	if err := e.validateRingCTV4Activation(block); err != nil {
		return fmt.Errorf("self-produced block %d failed RingCT activation policy: %w",
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

	preparedAVM, err := e.prepareAVMBlock(block)
	if err != nil {
		for _, tx := range block.Txs {
			if tx.IsAVM() {
				e.pool.BanTx(tx.Hash())
			}
		}
		return fmt.Errorf("self-produced block %d AVM execution failed: %w", block.Header.Height, err)
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
					e.halted.Store(true)
					return fmt.Errorf("fatal persistent UTXO rollback failure at self-produced block %d: %w",
						block.Header.Height, rbErr)
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
		var rollbackErr error
		if e.utxos != nil {
			if rbErr := e.utxos.RollbackBlock(block); rbErr != nil {
				rollbackErr = rbErr
				e.log.Error("UTXO rollback failed after chain.AddBlock error (self-produced)",
					"height", block.Header.Height,
					"chain_err", err, "rollback_err", rbErr)
			}
		}
		if stakeRollback != nil {
			stakeRollback()
		}
		if rollbackErr != nil {
			e.halted.Store(true)
			return fmt.Errorf("fatal persistent UTXO rollback failure at self-produced block %d: %w",
				block.Header.Height, rollbackErr)
		}
		return fmt.Errorf("add produced block: %w", err)
	}

	// Persist the just-added block and prepared AVM state before exposing any
	// commit side effects. This callback is shared with incoming P2P blocks.
	if e.cfg.OnCanonicalBlock != nil {
		if err := e.cfg.OnCanonicalBlock(block, preparedAVM); err != nil {
			persistErr := fmt.Errorf("persist canonical block %d: %w", block.Header.Height, err)
			return e.failCanonicalPersistence(block, stakeRollback, persistErr)
		}
	} else {
		persistErr := fmt.Errorf("persist canonical block %d: OnCanonicalBlock is required", block.Header.Height)
		return e.failCanonicalPersistence(block, stakeRollback, persistErr)
	}

	// Notify local-producer listeners after the canonical durability boundary.
	// A failure leaves the durable admin-mint records prepared and stops this
	// engine: continuing would advance an in-memory-only chain.
	//
	// Deprecated for persistence: use OnCanonicalBlock.
	if e.cfg.OnBlockProduced != nil {
		if err := e.cfg.OnBlockProduced(block); err != nil {
			persistErr := fmt.Errorf("persist produced block %d: %w", block.Header.Height, err)
			e.resolveAdminMintsPersistenceFailed(persistErr)
			e.halted.Store(true)
			return persistErr
		}
	}

	// Advance the fee state for locally produced blocks exactly as for peer
	// blocks.  Omitting this transition makes a consecutive local proposal
	// reuse the previous fee and diverge from validating peers.
	blockBaseFee := block.Header.BaseFee
	if blockBaseFee == 0 {
		blockBaseFee = e.expectedBaseFeeAt(block.Header.Height)
	}
	newFee := nextBaseFee(blockBaseFee, block.Size())
	e.mu.Lock()
	e.baseFee = newFee
	e.mu.Unlock()
	e.pool.SetBaseFee(newFee)

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

	// Notify any block-source-agnostic listeners (e.g. periodic snapshotting).
	if e.cfg.OnBlockAccepted != nil {
		e.cfg.OnBlockAccepted(block)
	}

	// The proposal identity was durably prepared before AddBlock.  Run block
	// persistence callbacks before publishing the completed mint outcome so a
	// successful response is never emitted ahead of the configured chain
	// durability boundary.
	e.resolveAdminMintsCommitted(block.Header.Height)

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

// DefaultBlockRewardNAPR is the legacy fallback used only when neither an
// explicit block reward nor staking-pool economics are configured.
const DefaultBlockRewardNAPR uint64 = 500_000_000

// AuthorizedBlockRewardNAPR is retained for compatibility with external code.
// Authorization authenticates the active pool/tail amount; it does not define
// a separate emission schedule.
const AuthorizedBlockRewardNAPR uint64 = DefaultPoolBlockRewardNAPR

// HalvingIntervalBlocks is the number of blocks between each block-reward
// halving event. At a 3-second target interval, 21,024,000 blocks is about
// two years.
const HalvingIntervalBlocks uint64 = 21_024_000

// defaultBlockRewardNAPR is an unexported alias kept for backward-compat
// within this package.  External code should use DefaultBlockRewardNAPR.
const defaultBlockRewardNAPR = DefaultBlockRewardNAPR

// blockRewardAtHeight returns the coinbase reward in nAPRO for a block at the
// given height, applying halvings per HalvingIntervalBlocks.
// Era 0: base; era 1: base/2; era 2: base/4; …
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

// blockFeeStats computes the total nAPRO destroyed by a block's transactions.
// Only the protocol base fee is burned.  A transaction that carries the signed
// intentional-burn marker adds its public marker amount to that destroyed
// total.  The marker amount is already included in tx.Fee for commitment
// balance purposes, so it must be separated from (rather than added to) the
// fee before calculating the validator priority tip.
func blockFeeStats(txs []core.Transaction, baseFee uint64) (burned, tipTotal uint64) {
	for _, tx := range txs {
		if tx.IsCoinbase() || tx.IsStake() {
			continue
		}
		minFee := tx.MinFeeAt(baseFee)
		// Invalid underpriced transactions cannot enter a validated block, but
		// retain a bounded result for callers inspecting historical data.
		burnForTx := minFee
		if tx.Fee < minFee {
			burnForTx = tx.Fee
		}
		requiredFee := minFee
		if intentionalBurn, isBurn := tx.BurnAmount(); isBurn {
			if burnForTx > ^uint64(0)-intentionalBurn ||
				requiredFee > ^uint64(0)-intentionalBurn {
				return ^uint64(0), 0
			}
			burnForTx += intentionalBurn
			requiredFee += intentionalBurn
		}
		if tx.Fee > requiredFee {
			tipTotal += tx.Fee - requiredFee
		}
		if burned > ^uint64(0)-burnForTx {
			return ^uint64(0), tipTotal
		}
		burned += burnForTx
	}
	return
}

// expectedAuthorizedRewardAmount is the exact amount an activated on-chain
// reward authorization must mint. It uses only immutable protocol constants and
// block data, so every validator derives the same value across restarts.
func (e *Engine) expectedAuthorizedRewardAmount(block *core.Block) (uint64, error) {
	return e.blockRewardNAPRAt(block.Header.Height), nil
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

// validateCoinbasePolicy applies two reward-authorization eras:
//   - structural coinbase rules before reward authorization activation;
//   - exactly one consensus-priced, proposer-signed validator reward once the
//     on-chain authorization protocol is activated.
//
// RingCT activation is deliberately not consulted here: it governs transfer
// proofs and fee rules, not whether the configured validator reward exists.
func (e *Engine) validateCoinbasePolicy(block *core.Block) error {
	if err := validateCoinbasePolicy(block); err != nil {
		return err
	}

	var coinbases []*core.Transaction
	for i := range block.Txs {
		tx := &block.Txs[i]
		if tx.IsCoinbase() && !tx.IsStake() {
			coinbases = append(coinbases, tx)
		}
	}

	rewardActivation := e.cfg.RewardAuthorizationActivationHeight
	if rewardActivation > 0 && block.Header.Height >= rewardActivation {
		expectedAmount, err := e.expectedAuthorizedRewardAmount(block)
		if err != nil {
			return err
		}
		if expectedAmount == 0 {
			if len(coinbases) != 0 {
				return fmt.Errorf("validator reward era has ended, got %d coinbase transaction(s)",
					len(coinbases))
			}
			return nil
		}
		if len(coinbases) != 1 {
			return fmt.Errorf("authorized reward block must contain exactly one coinbase transaction, got %d",
				len(coinbases))
		}
		auth, err := core.ValidateAuthorizedRewardTx(
			coinbases[0],
			block.Header.Height,
			block.Header.PrevHash,
			block.Header.ValidatorPub,
		)
		if err != nil {
			return fmt.Errorf("invalid validator reward authorization: %w", err)
		}
		if auth.Amount != expectedAmount {
			return fmt.Errorf("authorized reward amount %d does not match consensus amount %d",
				auth.Amount, expectedAmount)
		}
		return nil
	}

	return nil
}

func (e *Engine) validateBlockEconomics(block *core.Block) error {
	activation := e.cfg.RingCTV4ActivationHeight
	if activation > 0 && block.Header.Height < activation {
		return nil
	}

	expectedBaseFee := e.expectedBaseFeeAt(block.Header.Height)
	// Historical blocks encoded zero as "use the current protocol fee".
	// Preserve that representation, but derive the effective fee locally.
	if block.Header.BaseFee == 0 {
		return fmt.Errorf("base fee must be explicitly committed after activation")
	}
	if block.Header.BaseFee != expectedBaseFee {
		return fmt.Errorf("base fee mismatch: got %d, want %d",
			block.Header.BaseFee, expectedBaseFee)
	}

	var totalFees uint64
	var totalMinimum uint64
	var totalAVMGas uint64
	for i := range block.Txs {
		tx := &block.Txs[i]
		if tx.IsCoinbase() || tx.IsStake() {
			continue
		}
		minFee := tx.MinFeeAt(expectedBaseFee)
		requiredFee := minFee
		if tx.IsAVM() {
			gasLimit := tx.AVM.GasLimit
			if gasLimit > core.AVMMaxBlockGas-totalAVMGas {
				return fmt.Errorf("transaction %d exceeds AVM block gas limit: %d + %d > %d",
					i, totalAVMGas, gasLimit, core.AVMMaxBlockGas)
			}
			totalAVMGas += gasLimit
			gasFee, err := core.AVMGasFee(gasLimit)
			if err != nil || requiredFee > ^uint64(0)-gasFee {
				return fmt.Errorf("transaction %d AVM gas fee requirement overflows uint64", i)
			}
			requiredFee += gasFee
		}
		if intentionalBurn, isBurn := tx.BurnAmount(); isBurn {
			if requiredFee > ^uint64(0)-intentionalBurn {
				return fmt.Errorf("transaction %d intentional burn fee requirement overflows uint64", i)
			}
			requiredFee += intentionalBurn
		}
		if tx.Fee < requiredFee {
			return fmt.Errorf("transaction %d fee below consensus minimum: got %d, want at least %d",
				i, tx.Fee, requiredFee)
		}
		if totalFees > ^uint64(0)-tx.Fee {
			return fmt.Errorf("aggregate transaction fees overflow uint64")
		}
		totalFees += tx.Fee
		if totalMinimum > ^uint64(0)-minFee {
			return fmt.Errorf("aggregate minimum fees overflow uint64")
		}
		totalMinimum += minFee
	}
	return nil
}

func (e *Engine) expectedBaseFee() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.baseFee == 0 {
		return core.InitialBaseFeePerByte
	}
	return e.baseFee
}

func (e *Engine) expectedBaseFeeAt(height uint64) uint64 {
	activation := e.cfg.RingCTV4ActivationHeight
	if activation > 0 && height == activation {
		// Establish an explicit consensus anchor at the hard-fork boundary.
		// This makes the first strict block independent of how a pre-activation
		// node represented or reconstructed legacy zero-valued fee headers.
		return core.InitialBaseFeePerByte
	}
	return e.expectedBaseFee()
}

// ─── Admin mint scheduling ────────────────────────────────────────────────────

// ScheduleAdminMint queues an admin mint and blocks until the mint is included
// in a committed block (returning its tx hash and inclusion height) or the
// timeout expires.
//
// The mint transaction is built inside produceBlock with the block's own
// height, so its one-time pub is spend_pub + height*G — cryptographically
// unique per mint.  This replaces the legacy height=0 mempool path, where all
// admin mints to one address shared a single key image and one spent/phantom
// key-image index entry blocked every future mint to that address.
//
// A caller timeout does not cancel the keyed request: it remains queued or
// in-flight, and a retry with the same key joins it rather than minting again.
func (e *Engine) ScheduleAdminMint(idempotencyKey, addr string, amountNAPR uint64, timeout time.Duration) (crypto.Hash32, uint64, error) {
	if strings.TrimSpace(idempotencyKey) == "" || len(idempotencyKey) > 128 {
		return crypto.Hash32{}, 0, fmt.Errorf("idempotency key must be between 1 and 128 bytes")
	}
	tip := e.chain.Tip()
	if tip != nil {
		nextHeight := tip.Header.Height + 1
		activation := e.cfg.RingCTV4ActivationHeight
		if activation == 0 || nextHeight >= activation {
			return crypto.Hash32{}, 0, fmt.Errorf(
				"admin mint is disabled from RingCT v4 activation height; use an on-chain authorized mint protocol")
		}
	}
	if amountNAPR == 0 {
		return crypto.Hash32{}, 0, fmt.Errorf("mint amount must be > 0")
	}
	if e.cfg.RewardAddress != "" && addr == e.cfg.RewardAddress {
		// The per-block coinbase reward already mints to RewardAddress at
		// every height — an admin mint in the same block would produce a
		// second output with the identical one-time pub (same key image),
		// leaving only one of the two spendable.
		return crypto.Hash32{}, 0, fmt.Errorf(
			"cannot admin-mint to the validator reward address %s: the per-block "+
				"coinbase reward would collide with the same one-time pub", addr)
	}
	e.mintMu.Lock()
	if req := e.mintByKey[idempotencyKey]; req != nil {
		if req.addr != addr || req.amount != amountNAPR {
			e.mintMu.Unlock()
			return crypto.Hash32{}, 0, fmt.Errorf("idempotency key already used with different address or amount")
		}
		e.mintMu.Unlock()
		return waitAdminMint(req, timeout)
	}
	if e.adminMintStore != nil {
		record, found, err := e.adminMintStore.LoadAdminMintRecord(idempotencyKey)
		if err != nil {
			e.mintMu.Unlock()
			return crypto.Hash32{}, 0, err
		}
		if found {
			if record.Address != addr || record.AmountNAPR != amountNAPR {
				e.mintMu.Unlock()
				return crypto.Hash32{}, 0, fmt.Errorf("idempotency key already used with different address or amount")
			}
			switch record.State {
			case "", "completed":
				e.mintMu.Unlock()
				return record.TxHash, record.Height, nil
			case "prepared":
				// A crash may happen after a proposal was prepared but before
				// its result was recorded.  Only declare it complete when the
				// exact prepared transaction is in the canonical block.
				block := e.chain.GetByHeight(record.Height)
				if block != nil {
					for _, tx := range block.Txs {
						if tx.Hash() == record.TxHash {
							record.State = "completed"
							if err := e.adminMintStore.StoreAdminMintRecord(idempotencyKey, record); err != nil {
								e.mintMu.Unlock()
								return crypto.Hash32{}, 0, fmt.Errorf("persist reconciled admin mint outcome: %w", err)
							}
							e.mintMu.Unlock()
							return record.TxHash, record.Height, nil
						}
					}
				}
				e.mintMu.Unlock()
				return crypto.Hash32{}, 0, fmt.Errorf("admin mint has an unresolved prepared operation; refusing to remint")
			case "intent":
				e.mintMu.Unlock()
				return crypto.Hash32{}, 0, fmt.Errorf("admin mint has an unresolved persisted intent; refusing to remint")
			default:
				e.mintMu.Unlock()
				return crypto.Hash32{}, 0, fmt.Errorf("admin mint record has invalid state %q", record.State)
			}
		}
	}
	// Write the durable fence before exposing this request to the producer.
	// If this fails, it is safer to reject than to mint an operation which a
	// crash could later forget.
	if e.adminMintStore != nil {
		if err := e.adminMintStore.StoreAdminMintRecord(idempotencyKey, store.AdminMintRecord{
			State: "intent", Address: addr, AmountNAPR: amountNAPR,
		}); err != nil {
			e.mintMu.Unlock()
			return crypto.Hash32{}, 0, fmt.Errorf("persist admin mint intent: %w", err)
		}
	}
	req := &adminMintReq{
		key: idempotencyKey, addr: addr, amount: amountNAPR,
		ready: make(chan struct{}),
	}
	e.mintByKey[idempotencyKey] = req
	e.mintQueue = append(e.mintQueue, req)
	e.mintMu.Unlock()
	return waitAdminMint(req, timeout)
}

func waitAdminMint(req *adminMintReq, timeout time.Duration) (crypto.Hash32, uint64, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-req.ready:
		out := req.outcome
		return out.txHash, out.height, out.err
	case <-timer.C:
		// Prefer a concurrently published result over a timeout.
		select {
		case <-req.ready:
			out := req.outcome
			return out.txHash, out.height, out.err
		default:
		}
		return crypto.Hash32{}, 0, fmt.Errorf(
			"admin mint not committed within %s; it remains scheduled and retrying with the same idempotency key will return its outcome", timeout)
	}
}
func (e *Engine) takeQueuedAdminMints(height uint64, maxTake int) []core.Transaction {
	e.mintMu.Lock()
	defer e.mintMu.Unlock()

	if len(e.mintInFlight) > 0 {
		e.mintQueue = append(e.mintInFlight, e.mintQueue...)
		e.mintInFlight = nil
	}
	if len(e.mintQueue) == 0 || maxTake <= 0 {
		return nil
	}

	var txs []core.Transaction
	seen := make(map[string]bool)
	remaining := e.mintQueue[:0]
	for _, req := range e.mintQueue {
		if len(txs) >= maxTake || seen[req.addr] {
			remaining = append(remaining, req) // next block
			continue
		}
		tx, err := core.BuildMintTx(crypto.Address(req.addr), req.amount, height)
		if err != nil {
			req.done = true
			req.outcome = adminMintOutcome{err: fmt.Errorf("build mint tx: %w", err)}
			close(req.ready)
			continue
		}
		seen[req.addr] = true
		req.txHash = tx.Hash()
		req.height = height
		// Persist the exact transaction identity before it can enter the
		// candidate block.  This makes the crash window fail closed and lets a
		// restarted engine reconcile a completed canonical inclusion.
		if e.adminMintStore != nil {
			if err := e.adminMintStore.StoreAdminMintRecord(req.key, store.AdminMintRecord{
				State: "prepared", Address: req.addr, AmountNAPR: req.amount,
				TxHash: req.txHash, Height: req.height,
			}); err != nil {
				req.done = true
				req.outcome = adminMintOutcome{err: fmt.Errorf("persist prepared admin mint: %w", err)}
				close(req.ready)
				continue
			}
		}
		e.mintInFlight = append(e.mintInFlight, req)
		txs = append(txs, *tx)
	}
	e.mintQueue = remaining
	return txs
}

// resolveAdminMintsCommitted delivers success outcomes for all in-flight admin
// mints once their block has been committed to the chain.  Called from tick()
// after chain.AddBlock succeeds.
//
// Publishing the outcome happens under mintMu; closing ready then broadcasts
// the immutable result to every waiter sharing the idempotency key.
func (e *Engine) resolveAdminMintsCommitted(height uint64) {
	e.mintMu.Lock()
	inflight := e.mintInFlight
	e.mintInFlight = nil
	for _, req := range inflight {
		out := adminMintOutcome{txHash: req.txHash, height: req.height}
		if e.adminMintStore != nil {
			if err := e.adminMintStore.StoreAdminMintRecord(req.key, store.AdminMintRecord{
				State: "completed", Address: req.addr, AmountNAPR: req.amount, TxHash: req.txHash, Height: req.height,
			}); err != nil {
				out = adminMintOutcome{err: fmt.Errorf("persist committed admin mint outcome: %w", err)}
			}
		}
		req.done = true
		req.outcome = out
		close(req.ready)
		// Durable state is the source of truth after a terminal outcome. Existing
		// waiters retain req directly, while a later caller must reload either
		// the completed record or the prepared record left by a failed completed
		// write and reconcile it against the canonical chain.
		if e.adminMintStore != nil && e.mintByKey[req.key] == req {
			delete(e.mintByKey, req.key)
		}
	}
	e.mintMu.Unlock()

	if len(inflight) > 0 {
		e.log.Info("admin mint(s) committed", "count", len(inflight), "height", height)
	}
}

func (e *Engine) resolveAdminMintsPersistenceFailed(err error) {
	e.mintMu.Lock()
	inflight := e.mintInFlight
	e.mintInFlight = nil
	for _, req := range inflight {
		req.done = true
		req.outcome = adminMintOutcome{err: fmt.Errorf("admin mint persistence failed: %w", err)}
		close(req.ready)
	}
	e.mintMu.Unlock()
}
func (e *Engine) produceBlock(height, round uint64, parent *core.Block) (*core.Block, error) {
	raw := e.pool.SelectTxs(2000) // up to 2000 txs per block (verifier hard limit)
	rewardActivation := e.cfg.RewardAuthorizationActivationHeight
	rewardAuthorizationActive := rewardActivation > 0 && height >= rewardActivation

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
	var selectedAVMGas uint64
	for _, tx := range raw {
		if tx.IsCoinbase() && !tx.IsStake() {
			h := tx.Hash()
			if !e.pool.IsPrivileged(h) {
				e.log.Warn("produceBlock: dropping unexpected non-privileged coinbase from pool",
					"hash", h)
				continue
			}
			if rewardAuthorizationActive {
				e.log.Warn("produceBlock: dropping privileged legacy mint after reward authorization activation",
					"hash", h, "height", height)
				e.pool.Remove(h)
				continue
			}
			// Privileged admin mint — keep it in the historical era.
		}
		if tx.IsAVM() {
			gasLimit := tx.AVM.GasLimit
			if gasLimit > core.AVMMaxBlockGas-selectedAVMGas {
				continue
			}
			selectedAVMGas += gasLimit
		}
		txs = append(txs, tx)
	}

	// Pre-screen: re-verify each ring-sig tx against the current UTXO set
	// BEFORE including it in the block.  A tx that passed Add() when it
	// entered the mempool may now have an invalid ring sig if the UTXOSet
	// was corrected after an OOM snapshot repair, or if its ring-member
	// decoy UTXOs have since been spent or removed (pruning, fork, etc.).
	//
	// Rather than letting such a tx stall block production indefinitely
	// (validator tries → VerifyBlock fails → error → retry forever), we
	// evict and ban it so that:
	//   1. This block slot succeeds without the invalid tx.
	//   2. The freed key image(s) immediately allow legitimate spends from
	//      the same UTXO to be submitted and mined.
	//   3. P2P peers cannot re-inject the tx and recreate the phantom
	//      key-image lock that blocks user withdrawals.
	if e.txVerifier != nil {
		screened := txs[:0]
		for i := range txs {
			tx := &txs[i]
			if tx.IsCoinbase() || tx.IsStake() {
				screened = append(screened, *tx)
				continue
			}
			if err := core.ValidateTxVersionAtHeight(
				tx,
				height,
				e.cfg.RingCTV4ActivationHeight,
				e.cfg.RingCTCLSAGActivationHeight,
				e.cfg.AVMActivationHeight,
			); err != nil {
				hash := tx.Hash()
				e.log.Warn("produceBlock: banning tx rejected by RingCT activation policy",
					"hash", hash,
					"height", height,
					"err", err,
				)
				e.pool.BanTx(hash)
				continue
			}
			if err := e.txVerifier.VerifyTx(tx); err != nil {
				hash := tx.Hash()
				e.log.Warn("produceBlock: banning mempool tx that failed re-verification",
					"hash", hash,
					"height", height,
					"err", err,
				)
				e.pool.BanTx(hash)
				// Do not append — tx is excluded from this block.
				continue
			}
			screened = append(screened, *tx)
		}
		txs = screened
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

	// Every transaction fee is burned in full. Validator compensation comes
	// only from the pool reward or tail emission.
	currentBaseFee := e.expectedBaseFeeAt(height)

	// Prepend queued admin mints, built at THIS block's height so every mint
	// gets a unique one-time pub (spend_pub + height*G) and therefore a
	// unique key image.  Placed before the reward-prepend below, so the final
	// order is [reward, adminMints..., pool txs...] — all coinbases remain a
	// block prefix as required by validateCoinbasePolicy.
	//
	// Capacity: never exceed maxCoinbasesPerBlock counting the reward coinbase
	// (added below) and any privileged coinbases already selected from the
	// pool — an over-limit block would fail our own validateCoinbasePolicy and
	// stall production forever while the mints re-queue each slot.
	ringCTActivation := e.cfg.RingCTV4ActivationHeight
	adminMintActive := ringCTActivation > 0 && height < ringCTActivation
	rewardWillBeBuilt := e.cfg.RewardAddress != ""
	mintCapacity := maxCoinbasesPerBlock
	if rewardWillBeBuilt {
		mintCapacity--
	}
	for i := range txs {
		if txs[i].IsCoinbase() {
			mintCapacity--
		}
	}
	if adminMintActive && !rewardAuthorizationActive {
		if adminMints := e.takeQueuedAdminMints(height, mintCapacity); len(adminMints) > 0 {
			txs = append(adminMints, txs...)
		}
	}

	// Once reward authorization is active, the local address only selects where
	// this proposer wants to be paid. Peers validate its exact signed value from
	// the on-chain payload and derive the amount from protocol constants.
	if rewardAuthorizationActive {
		if e.cfg.RewardAddress == "" {
			return nil, fmt.Errorf("reward_address is required from reward authorization activation height %d",
				rewardActivation)
		}
		rewardNAPR := e.blockRewardNAPRAt(height)
		if rewardNAPR > 0 {
			mintTx, err := core.BuildAuthorizedRewardTx(
				crypto.Address(e.cfg.RewardAddress),
				rewardNAPR,
				height,
				parent.Hash(),
				e.cfg.MyKey.PrivKey(),
			)
			if err != nil {
				return nil, fmt.Errorf("build authorized validator reward: %w", err)
			}
			txs = append([]core.Transaction{*mintTx}, txs...)
		}
	} else if e.cfg.RewardAddress != "" {
		baseReward := e.cfg.BlockRewardNAPR
		if baseReward == 0 {
			if e.stakingPoolInit > 0 {
				baseReward = DefaultPoolBlockRewardNAPR
			} else {
				baseReward = defaultBlockRewardNAPR
			}
		}

		var rewardNAPR uint64
		if e.stakingPoolInit > 0 {
			// Pool-based mode: check current balance.
			// DecrementPool() is called in OnBlockAccepted AFTER this block is
			// committed, so we read the balance before this block's deduction.
			remaining := atomic.LoadInt64(&e.stakingPoolRemaining)
			if remaining > 0 {
				// Pool phase — draw from pre-allocated staking pool.
				// Total Supply does NOT increase; 10 B stays constant.
				poolDraw := baseReward
				if poolDraw > uint64(remaining) {
					poolDraw = uint64(remaining) // last partial draw
				}
				rewardNAPR = poolDraw
			} else {
				// Tail emission phase — mint minimal reward.
				rewardNAPR = e.tailRewardNAPR
			}
		} else {
			// Legacy: halving-schedule mint (increases Total Supply).
			rewardNAPR = blockRewardAtHeight(baseReward, height)
		}

		mintTx, err := core.BuildMintTx(crypto.Address(e.cfg.RewardAddress), rewardNAPR, height)
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
// peer-submitted block's timestamp and our local wall clock.  Only blocks
// whose timestamp is more than 15 s in the FUTURE are rejected to prevent
// timejacking attacks where a malicious peer shifts the chain tip far ahead.
// Past-timestamped blocks (e.g. historical sync blocks) are always accepted so
// that a relay node can catch up after a restart without false positives.
const maxClockSkewNs = int64(15 * 1_000_000_000)

func (e *Engine) handleIncomingBlock(block *core.Block) error {
	if e.halted.Load() {
		return fmt.Errorf("consensus engine halted after persistent rollback failure")
	}
	// Basic structural validation
	if err := block.Validate(); err != nil {
		return fmt.Errorf("invalid block: %w", err)
	}

	// Timejacking guard: only reject blocks whose timestamp is more than
	// 15 s in the FUTURE.  Past-timestamped sync blocks must be accepted
	// so a restarting relay node can fill historical gaps.  A negative
	// skewNs means the block is in the past — always safe to accept.
	nowNs := time.Now().UTC().UnixNano()
	skewNs := block.Header.Timestamp - nowNs
	if skewNs > maxClockSkewNs {
		total := atomic.AddInt64(&e.timestampRejected, 1)

		// Per-validator ban tracking: count rejections per block signer so a
		// rogue peer cannot flood future-timestamped blocks indefinitely.
		// Once the count crosses cfg.TimestampBanThreshold, OnTimestampBan is
		// called on every subsequent rejection so the caller can escalate
		// (e.g. disconnect and ban the peer's TCP connection).
		if e.cfg.TimestampBanThreshold > 0 && e.cfg.OnTimestampBan != nil {
			pubID := block.Header.ValidatorPub.ID()
			e.tsMu.Lock()
			e.tsPerValidator[pubID]++
			count := e.tsPerValidator[pubID]
			e.tsMu.Unlock()
			if count >= e.cfg.TimestampBanThreshold {
				e.log.Warn("timejacking ban threshold crossed",
					"validator", pubID,
					"per_validator_count", count,
					"global_total", total,
					"threshold", e.cfg.TimestampBanThreshold)
				e.cfg.OnTimestampBan(block.Header.ValidatorPub, count)
			}
		}

		return fmt.Errorf("block %d: timestamp too far from local clock (skew %dms, max %dms)",
			block.Header.Height, skewNs/1_000_000, maxClockSkewNs/1_000_000)
	}

	// A candidate must extend the current canonical tip by exactly one height
	// and one round.  Do this before accepting its proposer or recording
	// slashing evidence, so stale/future blocks cannot influence either path.
	tip := e.chain.Tip()
	if tip == nil {
		return fmt.Errorf("no genesis block")
	}
	if tip.Header.Height == math.MaxUint64 || block.Header.Height != tip.Header.Height+1 {
		return fmt.Errorf("block height %d doesn't extend tip %d",
			block.Header.Height, tip.Header.Height)
	}
	if tip.Header.Round == math.MaxUint32 || block.Header.Round != tip.Header.Round+1 {
		return fmt.Errorf("block round %d doesn't extend tip round %d",
			block.Header.Round, tip.Header.Round)
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
	if err := e.validateCoinbasePolicy(block); err != nil {
		return fmt.Errorf("block %d: coinbase policy violation: %w", block.Header.Height, err)
	}
	if err := e.validateBlockEconomics(block); err != nil {
		return fmt.Errorf("block %d: fee policy violation: %w", block.Header.Height, err)
	}
	if err := e.validateRingCTV4Activation(block); err != nil {
		return fmt.Errorf("block %d: RingCT activation policy violation: %w", block.Header.Height, err)
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

	preparedAVM, err := e.prepareAVMBlock(block)
	if err != nil {
		return fmt.Errorf("block %d: AVM execution failed: %w", block.Header.Height, err)
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
					e.halted.Store(true)
					return fmt.Errorf("fatal persistent UTXO rollback failure at block %d: %w",
						block.Header.Height, rbErr)
				}
			}
			return fmt.Errorf("block %d: stake apply failed (block rejected): %w",
				block.Header.Height, applyErr)
		}
	}

	if err := e.chain.AddBlock(block); err != nil {
		// Chain insertion failed after UTXO and stake state were already updated.
		// Roll back both to keep all state consistent.
		var rollbackErr error
		if e.utxos != nil {
			if rbErr := e.utxos.RollbackBlock(block); rbErr != nil {
				rollbackErr = rbErr
				e.log.Error("UTXO rollback failed after chain.AddBlock error",
					"height", block.Header.Height,
					"chain_err", err, "rollback_err", rbErr)
			}
		}
		if stakeRollback != nil {
			stakeRollback()
		}
		if rollbackErr != nil {
			e.halted.Store(true)
			return fmt.Errorf("fatal persistent UTXO rollback failure at block %d: %w",
				block.Header.Height, rollbackErr)
		}
		return fmt.Errorf("add block: %w", err)
	}

	if e.cfg.OnCanonicalBlock != nil {
		if err := e.cfg.OnCanonicalBlock(block, preparedAVM); err != nil {
			persistErr := fmt.Errorf("persist canonical block %d: %w", block.Header.Height, err)
			return e.failCanonicalPersistence(block, stakeRollback, persistErr)
		}
	} else {
		persistErr := fmt.Errorf("persist canonical block %d: OnCanonicalBlock is required", block.Header.Height)
		return e.failCanonicalPersistence(block, stakeRollback, persistErr)
	}
	// Stake txs already applied above — no processStakeTxs call needed.

	// ── EIP-1559 base fee update ─────────────────────────────────────────────
	blockBaseFee := block.Header.BaseFee
	if blockBaseFee == 0 {
		blockBaseFee = e.expectedBaseFee()
	}
	burnedNAPR, _ := blockFeeStats(block.Txs, blockBaseFee)
	newFee := nextBaseFee(blockBaseFee, block.Size())

	e.mu.Lock()
	e.baseFee = newFee
	e.mu.Unlock()

	// Propagate new base fee to the mempool so incoming txs are validated at
	// the correct rate immediately.
	e.pool.SetBaseFee(newFee)

	if burnedNAPR > 0 {
		e.log.Info("block fees",
			"height", block.Header.Height,
			"base_fee_per_byte", blockBaseFee,
			"burned_napro", burnedNAPR,
			"burned_apro", float64(burnedNAPR)/1e8,
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

func (e *Engine) validateRingCTV4Activation(block *core.Block) error {
	activation := e.cfg.RingCTV4ActivationHeight
	for i := range block.Txs {
		if err := core.ValidateTxVersionAtHeight(
			&block.Txs[i],
			block.Header.Height,
			activation,
			e.cfg.RingCTCLSAGActivationHeight,
			e.cfg.AVMActivationHeight,
		); err != nil {
			return fmt.Errorf("tx[%d]: %w", i, err)
		}
	}
	return nil
}

func (e *Engine) prepareAVMBlock(block *core.Block) (*avm.PreparedBlock, error) {
	hasAVM := false
	for i := range block.Txs {
		if block.Txs[i].IsAVM() {
			hasAVM = true
			break
		}
	}
	if e.cfg.AVMActivationHeight == 0 || block.Header.Height < e.cfg.AVMActivationHeight {
		if hasAVM {
			return nil, fmt.Errorf("AVM is not active at height %d", block.Header.Height)
		}
		return &avm.PreparedBlock{Height: block.Header.Height, BlockHash: block.Hash()}, nil
	}
	if e.cfg.AVMExecutor == nil {
		return nil, fmt.Errorf("AVM executor is not configured at active height %d", block.Header.Height)
	}
	return e.cfg.AVMExecutor.PrepareBlock(context.Background(), block)
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
