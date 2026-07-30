// Package core — ValidatorRegistry implements stake-based permissionless validator
// selection for the Aperod PoA network.
//
// Design:
//   - Validators deposit MinStakeNAPR to enter the activation queue.
//   - Every EpochLength blocks the top-staked pending nodes are activated
//     (up to ChurnLimit per epoch) until the MaxValidators cap is reached.
//   - Withdrawing validators enter an UnbondingBlocks cooldown before their
//     stake is released.
//   - Genesis validators are pre-seeded as Active with a genesis stake amount.
//   - No admin approval: selection is fully deterministic from stake data.
package core

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/aperod/aperod/crypto"
)

// ─── Staking constants ────────────────────────────────────────────────────────

const (
	// MaxValidators is the hard cap on simultaneously active validators.
	// Matches BNB Chain's model: deterministic, manageable committee size.
	MaxValidators = 21

	// MinStakeNAPR is the minimum stake to enter the activation queue.
	// 100 000 APRO × 10^8 = 10 000 000 000 000 nAPRO.
	MinStakeNAPR uint64 = 10_000_000_000_000

	// EpochLength is the number of blocks per epoch.
	// Validator-set changes only happen at epoch boundaries.
	EpochLength uint64 = 100

	// ChurnLimit is the maximum number of new validators that can be
	// activated per epoch (ETH-style entry rate limit).
	ChurnLimit = 3

	// MaxStakeNAPR is the per-validator hard cap (100 M APRO × 10^8).
	// Prevents C-3 uint64-overflow: Extra.amount cannot wrap StakeNAPR to MaxUint64.
	MaxStakeNAPR uint64 = 10_000_000_000_000_000

	// UnbondingBlocks is how long stake is locked after a withdrawal request.
	// ~2 hours at 1-second blocks.
	UnbondingBlocks uint64 = 7_200

	// SlashPercent is the percentage of stake slashed for double-signing.
	SlashPercent = 10
)

// ─── Stake payload encoding ───────────────────────────────────────────────────

// StakeAction is the type of stake operation encoded in a StakeTx.
type StakeAction uint8

const (
	StakeDeposit         StakeAction = 1 // Lock APRO to enter validator queue
	StakeWithdraw        StakeAction = 2 // Initiate full unbonding period
	StakePartialWithdraw StakeAction = 3 // Partially withdraw excess stake (7-day unbonding)
)

// PartialUnbondingBlocks is the unbonding period for partial stake withdrawals.
// 7 days × 86,400 seconds/day = 604,800 blocks at 1 block/second.
const PartialUnbondingBlocks uint64 = 604_800

// StakePayloadSize is the fixed byte length of tx.Extra for TxVersionStake.
// Layout: action(1) + pubkey(32) + amount_nAPR(8) + ed25519_sig(64) = 105
const StakePayloadSize = 105

// EncodeStakeExtra packs a stake operation into the 105-byte Extra field.
// sig must be an ED25519 signature of StakeSignMsg(action, pub, amount).
func EncodeStakeExtra(action StakeAction, pub crypto.ValidatorPubKey, amount uint64, sig []byte) ([]byte, error) {
	if len(pub) != 32 {
		return nil, fmt.Errorf("stake extra: pubkey must be 32 bytes, got %d", len(pub))
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("stake extra: signature must be 64 bytes, got %d", len(sig))
	}
	b := make([]byte, StakePayloadSize)
	b[0] = byte(action)
	copy(b[1:33], pub)
	binary.BigEndian.PutUint64(b[33:41], amount)
	copy(b[41:105], sig)
	return b, nil
}

// DecodeStakeExtra unpacks the 105-byte Extra field from a stake transaction.
func DecodeStakeExtra(extra []byte) (action StakeAction, pub crypto.ValidatorPubKey, amount uint64, sig []byte, err error) {
	if len(extra) != StakePayloadSize {
		err = fmt.Errorf("stake extra: expected %d bytes, got %d", StakePayloadSize, len(extra))
		return
	}
	action = StakeAction(extra[0])
	pub = crypto.ValidatorPubKey(extra[1:33])
	amount = binary.BigEndian.Uint64(extra[33:41])
	sig = extra[41:105]
	return
}

// StakeSignMsg returns the canonical message that a validator must sign to
// authorize a stake deposit or withdrawal (prevents replay attacks).
func StakeSignMsg(action StakeAction, pub crypto.ValidatorPubKey, amount uint64) crypto.Hash32 {
	ab := make([]byte, 8)
	binary.BigEndian.PutUint64(ab, amount)
	return crypto.HashBytes(
		[]byte("aperod/stake/v1"),
		[]byte{byte(action)},
		[]byte(pub),
		ab,
	)
}

// ─── Validator lifecycle ──────────────────────────────────────────────────────

// ValidatorStatus is the lifecycle state of a registered validator.
type ValidatorStatus uint8

const (
	ValidatorPending   ValidatorStatus = iota + 1 // staked, waiting for activation
	ValidatorActive                               // producing / voting in consensus
	ValidatorUnbonding                            // withdrawal requested, stake locked
	ValidatorExited                               // stake fully released
)

// String returns a human-readable status name.
func (s ValidatorStatus) String() string {
	switch s {
	case ValidatorPending:
		return "pending"
	case ValidatorActive:
		return "active"
	case ValidatorUnbonding:
		return "unbonding"
	case ValidatorExited:
		return "exited"
	default:
		return "unknown"
	}
}

// UnbondingEntry records one partial stake withdrawal in progress.
// The amount is locked for PartialUnbondingBlocks to prevent slashing evasion.
type UnbondingEntry struct {
	Amount   uint64 // nAPRO being unbonded
	EndBlock uint64 // block height after which this entry is released
}

// ValidatorEntry is one validator's record in the registry.
type ValidatorEntry struct {
	PubKey          crypto.ValidatorPubKey
	StakeNAPR       uint64
	Status          ValidatorStatus
	ActivationEpoch uint64         // epoch when the validator first becomes eligible
	UnbondEndBlock  uint64         // block height after which stake is released (full Unbonding only)
	UnbondingQueue  []UnbondingEntry // partial withdrawal queue; released in UpdateEpoch
}

// APRStake returns the stake in whole APRO (for display).
func (e *ValidatorEntry) APRStake() float64 {
	return float64(e.StakeNAPR) / float64(BaseUnitsPerAPR)
}

// PendingUnbondingNAPR returns the total nAPRO currently in the partial unbonding queue.
func (e *ValidatorEntry) PendingUnbondingNAPR() uint64 {
	var total uint64
	for _, ub := range e.UnbondingQueue {
		total += ub.Amount
	}
	return total
}

// ─── Dynamic Stake Threshold ──────────────────────────────────────────────────

// DSTTargetUSD is the USD value a validator must stake after Phase 2 begins.
// Formula: minStakeNAPR = DSTTargetUSD / currentAPROPriceUSD * BaseUnitsPerAPR
// Before Phase 2 the static MinStakeNAPR constant takes precedence.
const DSTTargetUSD float64 = 10_000 // $10,000 per validator seat

// Phase2StartYear is the calendar year when DST becomes active (post-devfund).
const Phase2StartYear = 2031

// ─── ValidatorRegistry ────────────────────────────────────────────────────────

// ValidatorRegistry tracks all validator stakes and drives automatic set
// selection.  It is thread-safe and is updated as blocks are processed.
type ValidatorRegistry struct {
	mu             sync.RWMutex
	validators     map[string]*ValidatorEntry // hex(pubkey) → entry
	dynamicMinNAPR uint64                     // 0 = use static MinStakeNAPR constant
}

// NewValidatorRegistry creates an empty registry.
func NewValidatorRegistry() *ValidatorRegistry {
	return &ValidatorRegistry{
		validators:     make(map[string]*ValidatorEntry),
		dynamicMinNAPR: 0,
	}
}

// UpdateMinStake recalculates the minimum stake from the current APRO market
// price (in USD).  Called by the oracle price-update path on each new price.
//
//   minStakeNAPR = ⌈DSTTargetUSD / priceUSD × BaseUnitsPerAPR⌉
//
// If priceUSD ≤ 0 the dynamic threshold is cleared and the static constant
// MinStakeNAPR is used instead (prevents division-by-zero and protects
// Phase 1 where price is not yet reliable).
//
// Thread-safe; can be called concurrently with block processing.
func (r *ValidatorRegistry) UpdateMinStake(priceUSD float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if priceUSD <= 0 {
		r.dynamicMinNAPR = 0
		return
	}
	napro := (DSTTargetUSD / priceUSD) * float64(BaseUnitsPerAPR)
	// Never go below 1 APRO (sanity floor for extremely high prices)
	if napro < float64(BaseUnitsPerAPR) {
		napro = float64(BaseUnitsPerAPR)
	}
	r.dynamicMinNAPR = uint64(napro + 0.5) // round to nearest nAPRO
}

// CurrentMinStake returns the effective minimum stake in nAPRO.
// Returns the static constant when no oracle price has been set yet.
func (r *ValidatorRegistry) CurrentMinStake() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.dynamicMinNAPR > 0 {
		return r.dynamicMinNAPR
	}
	return MinStakeNAPR
}

// InitFromGenesis pre-seeds genesis validators as Active, bypassing the queue.
// Called once when the node starts from genesis or after a full resync.
func (r *ValidatorRegistry) InitFromGenesis(pubs []crypto.ValidatorPubKey, genesisStake uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, pub := range pubs {
		key := pub.Hex()
		if _, exists := r.validators[key]; !exists {
			r.validators[key] = &ValidatorEntry{
				PubKey:          pub,
				StakeNAPR:       genesisStake,
				Status:          ValidatorActive,
				ActivationEpoch: 0,
			}
		}
	}
}

// ProcessStakeTx validates and applies a stake transaction to the registry.
// Called by the consensus engine for every StakeTx included in a block.
func (r *ValidatorRegistry) ProcessStakeTx(tx Transaction, height uint64) error {
	if tx.Version != TxVersionStake {
		return fmt.Errorf("registry: not a stake tx (version=%d)", tx.Version)
	}
	action, pub, amount, sig, err := DecodeStakeExtra(tx.Extra)
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}

	// Every stake action is authorized by the validator's own self-signed key.
	// StakePartialWithdraw submitted via the admin endpoint uses the node's own
	// key — which is valid only when pub == node's own pubkey.
	msg := StakeSignMsg(action, pub, amount)
	if !pub.Verify(msg, sig) {
		return fmt.Errorf("registry: invalid stake signature from %s", pub.ID())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	key := pub.Hex()
	switch action {
	case StakeDeposit:
		return r.applyDeposit(key, pub, amount, height)
	case StakeWithdraw:
		return r.applyWithdraw(key, height)
	case StakePartialWithdraw:
		return r.applyPartialWithdraw(key, amount, height)
	default:
		return fmt.Errorf("registry: unknown stake action %d", action)
	}
}

func (r *ValidatorRegistry) applyDeposit(key string, pub crypto.ValidatorPubKey, amount, height uint64) error {
	// Use dynamic minimum stake (DST) if oracle price has been set, otherwise
	// fall back to the static MinStakeNAPR constant.
	effectiveMin := r.dynamicMinNAPR
	if effectiveMin == 0 {
		effectiveMin = MinStakeNAPR
	}
	if amount < effectiveMin {
		return fmt.Errorf("deposit too low: %.4f APRO < minimum %.4f APRO",
			float64(amount)/float64(BaseUnitsPerAPR),
			float64(effectiveMin)/float64(BaseUnitsPerAPR))
	}

	// C-3 / C-1 guard: cap stake amount to prevent uint64 overflow attacks.
	if amount > MaxStakeNAPR {
		return fmt.Errorf("deposit amount %.4f APRO exceeds per-validator cap %.4f APRO",
			float64(amount)/float64(BaseUnitsPerAPR),
			float64(MaxStakeNAPR)/float64(BaseUnitsPerAPR))
	}

	if existing, ok := r.validators[key]; ok {
		switch existing.Status {
		case ValidatorUnbonding, ValidatorExited:
			return fmt.Errorf("validator is unbonding/exited; wait %d blocks before re-staking",
				UnbondingBlocks)
		default:
			// Top-up: increase stake — checked addition prevents uint64 overflow.
			if existing.StakeNAPR > math.MaxUint64-amount {
				return fmt.Errorf("stake top-up overflow: current %.4f + deposit %.4f would exceed uint64",
					float64(existing.StakeNAPR)/float64(BaseUnitsPerAPR),
					float64(amount)/float64(BaseUnitsPerAPR))
			}
			existing.StakeNAPR += amount
			return nil
		}
	}

	epoch := height / EpochLength
	r.validators[key] = &ValidatorEntry{
		PubKey:          pub,
		StakeNAPR:       amount,
		Status:          ValidatorPending,
		ActivationEpoch: epoch + 1, // earliest: next epoch
	}
	return nil
}

func (r *ValidatorRegistry) applyWithdraw(key string, height uint64) error {
	entry, ok := r.validators[key]
	if !ok {
		return fmt.Errorf("validator %s not registered", key[:8])
	}
	if entry.Status == ValidatorUnbonding || entry.Status == ValidatorExited {
		return fmt.Errorf("validator already unbonding or exited")
	}
	entry.Status = ValidatorUnbonding
	entry.UnbondEndBlock = height + UnbondingBlocks
	return nil
}

// applyPartialWithdraw reduces the validator's stake by amount and queues it for
// release after PartialUnbondingBlocks.  The validator remains Active/Pending as
// long as the remaining stake satisfies the current minimum stake threshold.
func (r *ValidatorRegistry) applyPartialWithdraw(key string, amount, height uint64) error {
	entry, ok := r.validators[key]
	if !ok {
		return fmt.Errorf("validator %s not registered", key[:8])
	}
	if entry.Status == ValidatorUnbonding || entry.Status == ValidatorExited {
		return fmt.Errorf("validator is unbonding/exited; cannot partial withdraw")
	}
	if amount == 0 {
		return fmt.Errorf("partial withdraw amount must be > 0")
	}
	if amount >= entry.StakeNAPR {
		return fmt.Errorf("withdrawal amount %d >= current stake %d; use full StakeWithdraw instead",
			amount, entry.StakeNAPR)
	}

	remaining := entry.StakeNAPR - amount
	effectiveMin := r.dynamicMinNAPR
	if effectiveMin == 0 {
		effectiveMin = MinStakeNAPR
	}
	if remaining < effectiveMin {
		return fmt.Errorf("remaining stake %.4f APRO < minimum %.4f APRO; reduce withdrawal amount or use full exit",
			float64(remaining)/float64(BaseUnitsPerAPR),
			float64(effectiveMin)/float64(BaseUnitsPerAPR))
	}

	entry.StakeNAPR = remaining
	entry.UnbondingQueue = append(entry.UnbondingQueue, UnbondingEntry{
		Amount:   amount,
		EndBlock: height + PartialUnbondingBlocks,
	})
	return nil
}

// UpdateEpoch is called at every epoch boundary (height % EpochLength == 0).
// It finalises unbonding validators, activates the top-staked pending ones
// (up to ChurnLimit), and rebalances the active set to MaxValidators.
// Returns the new active validator set.
func (r *ValidatorRegistry) UpdateEpoch(height uint64) []crypto.ValidatorPubKey {
	r.mu.Lock()
	defer r.mu.Unlock()

	currentEpoch := height / EpochLength

	// 1. Release fully-unbonded validators and clean up completed partial unbonding entries
	for _, e := range r.validators {
		if e.Status == ValidatorUnbonding && height >= e.UnbondEndBlock {
			e.Status = ValidatorExited
		}
		// Drop partial-unbonding entries whose lock period has elapsed
		if len(e.UnbondingQueue) > 0 {
			active := e.UnbondingQueue[:0]
			for _, ub := range e.UnbondingQueue {
				if height < ub.EndBlock {
					active = append(active, ub)
				}
				// entries past EndBlock are released (dropped silently — funds unlocked)
			}
			e.UnbondingQueue = active
		}
	}

	// 2. Count active slots
	activeCount := 0
	for _, e := range r.validators {
		if e.Status == ValidatorActive {
			activeCount++
		}
	}

	// 3. Activate pending validators: top-staked, up to ChurnLimit
	if activeCount < MaxValidators {
		slots := MaxValidators - activeCount
		if slots > ChurnLimit {
			slots = ChurnLimit
		}
		pending := r.sortedPendingLocked()
		activated := 0
		for _, e := range pending {
			if activated >= slots {
				break
			}
			if currentEpoch >= e.ActivationEpoch {
				e.Status = ValidatorActive
				activated++
			}
		}
	}

	// 4. Rebalance: demote lowest-stake active validators if > MaxValidators
	r.rebalanceLocked()

	return r.activeSetLocked()
}

// rebalanceLocked demotes the lowest-stake active validators if over cap.
// Must be called with r.mu held for writing.
func (r *ValidatorRegistry) rebalanceLocked() {
	active := make([]*ValidatorEntry, 0)
	for _, e := range r.validators {
		if e.Status == ValidatorActive {
			active = append(active, e)
		}
	}
	if len(active) <= MaxValidators {
		return
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].StakeNAPR > active[j].StakeNAPR
	})
	for i := MaxValidators; i < len(active); i++ {
		active[i].Status = ValidatorPending
		active[i].ActivationEpoch++ // re-queue for next epoch
	}
}

// sortedPendingLocked returns pending entries sorted by stake descending.
// Must be called with r.mu held.
func (r *ValidatorRegistry) sortedPendingLocked() []*ValidatorEntry {
	out := make([]*ValidatorEntry, 0)
	for _, e := range r.validators {
		if e.Status == ValidatorPending {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StakeNAPR > out[j].StakeNAPR
	})
	return out
}

// activeSetLocked returns sorted active validator pubkeys.
// Must be called with r.mu held.
func (r *ValidatorRegistry) activeSetLocked() []crypto.ValidatorPubKey {
	out := make([]crypto.ValidatorPubKey, 0)
	for _, e := range r.validators {
		if e.Status == ValidatorActive {
			out = append(out, e.PubKey)
		}
	}
	// Sort lexicographically for deterministic round-robin ordering
	sort.Slice(out, func(i, j int) bool {
		return out[i].Hex() < out[j].Hex()
	})
	return out
}

// ─── Read-only query methods ──────────────────────────────────────────────────

// GetActiveValidators returns the current active validator pubkeys (thread-safe).
func (r *ValidatorRegistry) GetActiveValidators() []crypto.ValidatorPubKey {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.activeSetLocked()
}

// IsActive returns true if the pubkey belongs to a currently active validator.
func (r *ValidatorRegistry) IsActive(pub crypto.ValidatorPubKey) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.validators[pub.Hex()]
	return ok && e.Status == ValidatorActive
}

// GetEntry returns a snapshot of a validator's entry, or false if not found.
func (r *ValidatorRegistry) GetEntry(pub crypto.ValidatorPubKey) (ValidatorEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.validators[pub.Hex()]
	if !ok {
		return ValidatorEntry{}, false
	}
	return *e, true
}

// AllEntries returns a snapshot of all validator entries sorted by stake.
func (r *ValidatorRegistry) AllEntries() []ValidatorEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ValidatorEntry, 0, len(r.validators))
	for _, e := range r.validators {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].StakeNAPR > out[j].StakeNAPR
	})
	return out
}

// Count returns (active, total) validator counts.
func (r *ValidatorRegistry) Count() (active, total int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range r.validators {
		total++
		if e.Status == ValidatorActive {
			active++
		}
	}
	return
}

