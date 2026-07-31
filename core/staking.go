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

// ─── Stake payload v2 (UTXO-backed deposit) ──────────────────────────────────
//
// StakeDeposit transactions must use the v2 payload which includes a reference
// to a real UTXO and its Pedersen blinding factor.  The verifier recomputes
// Commit(amount, burnBlind) and requires it to equal the UTXO's AmountCommit,
// proving the sender actually owns the committed funds.  The UTXO is then
// removed from the active set (burned) so it cannot be double-spent.
//
// Withdraw/PartialWithdraw continue to use the 105-byte v1 format.

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

	// UnbondingBlocks is how long stake is locked after a full withdrawal request.
	// 10 days × 86,400 s/day ÷ 6 s/block = 144,000 blocks.
	UnbondingBlocks uint64 = 144_000

	// SlashPercent is the percentage of stake slashed for double-signing.
	SlashPercent = 10
)

// ─── Stake payload encoding ───────────────────────────────────────────────────

// StakeAction is the type of stake operation encoded in a StakeTx.
type StakeAction uint8

const (
	StakeDeposit         StakeAction = 1 // Lock APRO to enter validator queue
	StakeWithdraw        StakeAction = 2 // Initiate full unbonding period
	StakePartialWithdraw StakeAction = 3 // Partially withdraw excess stake (3-day unbonding)
)

// PartialUnbondingBlocks is the unbonding period for partial stake withdrawals.
// 3 days × 86,400 s/day ÷ 6 s/block = 43,200 blocks.
const PartialUnbondingBlocks uint64 = 43_200

// StakePayloadSize is the fixed byte length of tx.Extra for v1 stake txs.
// Layout: action(1) + pubkey(32) + amount_nAPR(8) + ed25519_sig(64) = 105
// Used for StakeWithdraw and StakePartialWithdraw only.
const StakePayloadSize = 105

// StakePayloadSizeV2 is the byte length of tx.Extra for v2 stake deposits.
// Layout: action(1) + pubkey(32) + amount(8) + sig(64) +
//
//	burnTxHash(32) + burnOutIdx(4) + burnBlind(32) = 173
//
// The burn fields prove the depositor owns a real on-chain UTXO whose
// Pedersen commitment opens to (amount, burnBlind).  The verifier recomputes
// Commit(amount, burnBlind) and requires it to equal the UTXO's AmountCommit.
const StakePayloadSizeV2 = 173

// EncodeStakeExtra packs a withdraw/partial-withdraw operation into 105 bytes.
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

// DecodeStakeExtra unpacks the 105-byte v1 Extra field (withdraw/partial-withdraw).
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

// StakeSignMsg returns the canonical signing message for v1 withdraw/partial-withdraw.
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

// EncodeStakeExtraV2 packs a UTXO-backed deposit into 173 bytes.
// sig must be an ED25519 signature of StakeSignMsgV2(action, pub, amount, burnTxHash, burnOutIdx).
// burnBlind is the Pedersen blinding factor of the burn UTXO — revealed here
// because stake amounts are public in the validator registry.
func EncodeStakeExtraV2(
	action StakeAction,
	pub crypto.ValidatorPubKey,
	amount uint64,
	sig []byte,
	burnTxHash crypto.Hash32,
	burnOutIdx uint32,
	burnBlind crypto.BlindFactor,
) ([]byte, error) {
	if len(pub) != 32 {
		return nil, fmt.Errorf("stake extra v2: pubkey must be 32 bytes, got %d", len(pub))
	}
	if len(sig) != 64 {
		return nil, fmt.Errorf("stake extra v2: signature must be 64 bytes, got %d", len(sig))
	}
	b := make([]byte, StakePayloadSizeV2)
	b[0] = byte(action)
	copy(b[1:33], pub)
	binary.BigEndian.PutUint64(b[33:41], amount)
	copy(b[41:105], sig)
	copy(b[105:137], burnTxHash[:])
	binary.BigEndian.PutUint32(b[137:141], burnOutIdx)
	copy(b[141:173], burnBlind[:])
	return b, nil
}

// DecodeStakeExtraV2 unpacks the 173-byte v2 Extra field from a deposit tx.
func DecodeStakeExtraV2(extra []byte) (
	action StakeAction,
	pub crypto.ValidatorPubKey,
	amount uint64,
	sig []byte,
	burnTxHash crypto.Hash32,
	burnOutIdx uint32,
	burnBlind crypto.BlindFactor,
	err error,
) {
	if len(extra) != StakePayloadSizeV2 {
		err = fmt.Errorf("stake extra v2: expected %d bytes, got %d", StakePayloadSizeV2, len(extra))
		return
	}
	action = StakeAction(extra[0])
	pub = crypto.ValidatorPubKey(extra[1:33])
	amount = binary.BigEndian.Uint64(extra[33:41])
	sig = extra[41:105]
	copy(burnTxHash[:], extra[105:137])
	burnOutIdx = binary.BigEndian.Uint32(extra[137:141])
	copy(burnBlind[:], extra[141:173])
	return
}

// StakeSignMsgV2 returns the canonical signing message for a v2 deposit.
// Binds the signature to a specific burn UTXO (prevents replay with a different UTXO).
func StakeSignMsgV2(action StakeAction, pub crypto.ValidatorPubKey, amount uint64, burnTxHash crypto.Hash32, burnOutIdx uint32) crypto.Hash32 {
	ab := make([]byte, 8)
	binary.BigEndian.PutUint64(ab, amount)
	oidxb := make([]byte, 4)
	binary.BigEndian.PutUint32(oidxb, burnOutIdx)
	return crypto.HashBytes(
		[]byte("aperod/stake/v2"),
		[]byte{byte(action)},
		[]byte(pub),
		ab,
		burnTxHash[:],
		oidxb,
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
	utxos          *UTXOSet                   // required for C-1 UTXO-backed deposit check
}

// NewValidatorRegistry creates an empty registry.
func NewValidatorRegistry() *ValidatorRegistry {
	return &ValidatorRegistry{
		validators:     make(map[string]*ValidatorEntry),
		dynamicMinNAPR: 0,
	}
}

// SetUTXOSet wires the UTXO set into the registry so ProcessStakeTx can verify
// and burn the depositor's UTXO.  Must be called before any blocks are processed.
func (r *ValidatorRegistry) SetUTXOSet(utxos *UTXOSet) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.utxos = utxos
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
//
// Extra payload routing:
//   - 105 bytes (v1): StakeWithdraw / StakePartialWithdraw only.
//   - 173 bytes (v2): StakeDeposit only — must include a valid UTXO burn proof.
//     The verifier recomputes Commit(amount, burnBlind) and requires it to
//     equal the referenced UTXO's AmountCommit (C-1 full fix).
func (r *ValidatorRegistry) ProcessStakeTx(tx Transaction, height uint64) error {
	if tx.Version != TxVersionStake {
		return fmt.Errorf("registry: not a stake tx (version=%d)", tx.Version)
	}

	switch len(tx.Extra) {

	// ── v1: withdraw / partial-withdraw ──────────────────────────────────────
	case StakePayloadSize:
		action, pub, amount, sig, err := DecodeStakeExtra(tx.Extra)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if action == StakeDeposit {
			return fmt.Errorf("registry: StakeDeposit requires v2 payload (173 bytes) with UTXO burn proof — v1 deposits no longer accepted (C-1 fix)")
		}
		msg := StakeSignMsg(action, pub, amount)
		if !pub.Verify(msg, sig) {
			return fmt.Errorf("registry: invalid stake signature from %s", pub.ID())
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		key := pub.Hex()
		switch action {
		case StakeWithdraw:
			return r.applyWithdraw(key, height)
		case StakePartialWithdraw:
			return r.applyPartialWithdraw(key, amount, height)
		default:
			return fmt.Errorf("registry: unknown stake action %d in v1 payload", action)
		}

	// ── v2: UTXO-backed deposit ───────────────────────────────────────────────
	case StakePayloadSizeV2:
		action, pub, amount, sig, burnTxHash, burnOutIdx, burnBlind, err := DecodeStakeExtraV2(tx.Extra)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if action != StakeDeposit {
			return fmt.Errorf("registry: v2 payload (173 bytes) is only valid for StakeDeposit, got action=%d", action)
		}

		// Verify self-signature — binds the deposit to a specific burn UTXO,
		// preventing replay attacks with a different UTXO at the same amount.
		msg := StakeSignMsgV2(action, pub, amount, burnTxHash, burnOutIdx)
		if !pub.Verify(msg, sig) {
			return fmt.Errorf("registry: invalid stake signature (v2) from %s", pub.ID())
		}

		// ── C-1 full fix: verify and burn the referenced UTXO ────────────────
		if r.utxos == nil {
			return fmt.Errorf("registry: UTXO set not wired — cannot verify deposit (C-1 check)")
		}
		burnUTXO := r.utxos.Get(burnTxHash, burnOutIdx)
		if burnUTXO == nil {
			return fmt.Errorf("registry: burn UTXO %x:%d not found in active set (C-1 check)",
				burnTxHash[:8], burnOutIdx)
		}
		// Recompute Commit(amount, burnBlind) and require it to match the UTXO's
		// on-chain AmountCommit.  If it doesn't, the depositor does not own the
		// UTXO or has fabricated the amount (C-1 full check).
		expectedCommit, commitErr := crypto.Commit(amount, burnBlind)
		if commitErr != nil {
			return fmt.Errorf("registry: burn commitment computation failed: %w", commitErr)
		}
		if expectedCommit != burnUTXO.AmountCommit {
			return fmt.Errorf("registry: burn UTXO %x:%d AmountCommit mismatch — "+
				"claimed amount/blind does not open to the on-chain commitment (C-1 check)",
				burnTxHash[:8], burnOutIdx)
		}
		// Burn the UTXO: remove from active set and prevent re-use as ring decoy.
		if err := r.utxos.MarkStaked(burnTxHash, burnOutIdx); err != nil {
			return fmt.Errorf("registry: failed to burn stake UTXO: %w", err)
		}

		r.mu.Lock()
		defer r.mu.Unlock()
		return r.applyDeposit(pub.Hex(), pub, amount, height)

	default:
		return fmt.Errorf("registry: invalid stake extra length %d (expected %d for withdraw or %d for deposit)",
			len(tx.Extra), StakePayloadSize, StakePayloadSizeV2)
	}
}

// deepCopyValidators returns a deep copy of the validators map (including
// each ValidatorEntry's UnbondingQueue slice) for transactional rollback.
// Caller must hold r.mu.
func deepCopyValidators(src map[string]*ValidatorEntry) map[string]*ValidatorEntry {
	dst := make(map[string]*ValidatorEntry, len(src))
	for k, v := range src {
		clone := *v // copy value fields
		if len(v.UnbondingQueue) > 0 {
			clone.UnbondingQueue = make([]UnbondingEntry, len(v.UnbondingQueue))
			copy(clone.UnbondingQueue, v.UnbondingQueue)
		}
		dst[k] = &clone
	}
	return dst
}

// applyOneTxLocked applies a single stake transaction to the registry and UTXO
// set.  It assumes r.mu is already held by the caller (Write lock).
// Returns the UTXOKey that was staked (v2 deposits only) or nil, plus any error.
// On error, the caller must rollback all previously applied changes.
func (r *ValidatorRegistry) applyOneTxLocked(tx Transaction, height uint64) (*UTXOKey, error) {
	if tx.Version != TxVersionStake {
		return nil, fmt.Errorf("not a stake tx (version=%d)", tx.Version)
	}
	switch len(tx.Extra) {

	case StakePayloadSize:
		action, pub, amount, sig, err := DecodeStakeExtra(tx.Extra)
		if err != nil {
			return nil, err
		}
		if action == StakeDeposit {
			return nil, fmt.Errorf("StakeDeposit requires v2 payload (C-1 fix)")
		}
		msg := StakeSignMsg(action, pub, amount)
		if !pub.Verify(msg, sig) {
			return nil, fmt.Errorf("invalid stake signature from %s", pub.ID())
		}
		key := pub.Hex()
		switch action {
		case StakeWithdraw:
			return nil, r.applyWithdraw(key, height) // modifies r.validators (r.mu held)
		case StakePartialWithdraw:
			return nil, r.applyPartialWithdraw(key, amount, height) // ditto
		default:
			return nil, fmt.Errorf("unknown stake action %d", action)
		}

	case StakePayloadSizeV2:
		action, pub, amount, sig, burnTxHash, burnOutIdx, burnBlind, err := DecodeStakeExtraV2(tx.Extra)
		if err != nil {
			return nil, err
		}
		if action != StakeDeposit {
			return nil, fmt.Errorf("v2 payload only valid for StakeDeposit, got action=%d", action)
		}
		msg := StakeSignMsgV2(action, pub, amount, burnTxHash, burnOutIdx)
		if !pub.Verify(msg, sig) {
			return nil, fmt.Errorf("invalid v2 stake signature from %s", pub.ID())
		}
		if r.utxos == nil {
			return nil, fmt.Errorf("UTXO set not wired (C-1 check)")
		}
		// UTXO access: r.utxos.Get acquires utxos.mu (RLock) independently.
		// Safe to call while r.mu (Write) is held — no inverse lock ordering exists.
		burnUTXO := r.utxos.Get(burnTxHash, burnOutIdx)
		if burnUTXO == nil {
			return nil, fmt.Errorf("burn UTXO %x:%d not found in active set", burnTxHash[:8], burnOutIdx)
		}
		expectedCommit, commitErr := crypto.Commit(amount, burnBlind)
		if commitErr != nil {
			return nil, fmt.Errorf("commitment computation failed: %w", commitErr)
		}
		if expectedCommit != burnUTXO.AmountCommit {
			return nil, fmt.Errorf("burn UTXO %x:%d AmountCommit mismatch (C-1)", burnTxHash[:8], burnOutIdx)
		}
		// Stake the UTXO (utxos.mu briefly acquired internally — safe).
		if err := r.utxos.MarkStaked(burnTxHash, burnOutIdx); err != nil {
			return nil, fmt.Errorf("MarkStaked: %w", err)
		}
		// Apply registry deposit (r.mu already held — use internal helper directly).
		key := pub.Hex()
		if applyErr := r.applyDeposit(key, pub, amount, height); applyErr != nil {
			// MarkStaked succeeded but registry update failed — unmark the UTXO.
			r.utxos.UnmarkStaked(burnTxHash, burnOutIdx)
			return nil, applyErr
		}
		k := UTXOKey{TxHash: burnTxHash, OutputIndex: burnOutIdx}
		return &k, nil

	default:
		return nil, fmt.Errorf("invalid stake extra length %d", len(tx.Extra))
	}
}

// ApplyBlockStakeTxs atomically applies all stake transactions from a block to
// the registry and UTXO staking state.  It holds r.mu for the entire operation
// so that a concurrent oracle UpdateMinStake cannot alter the effective minimum
// between transactions.
//
// If any stake tx fails to apply, all previously applied changes within the block
// are rolled back (registry snapshot restored, staked UTXOs unmarked) before the
// error is returned.  The block must NOT be inserted into the chain on error.
//
// On success, a rollback function is returned for use if the subsequent
// chain.AddBlock call fails.  The caller MUST invoke rollback() if and only if
// chain insertion fails; invoking it after a successful AddBlock corrupts state.
func (r *ValidatorRegistry) ApplyBlockStakeTxs(txs []Transaction, height uint64) (rollback func(), err error) {
	if r == nil {
		return func() {}, nil
	}
	hasStake := false
	for _, tx := range txs {
		if tx.IsStake() {
			hasStake = true
			break
		}
	}
	if !hasStake {
		return func() {}, nil
	}

	// Acquire registry write lock for entire apply: prevents oracle from changing
	// dynamicMinNAPR between transactions (atomicity with ValidateBlockStakeTxs).
	r.mu.Lock()
	defer r.mu.Unlock()

	// Snapshot registry state for rollback.
	snap := deepCopyValidators(r.validators)
	var stakedKeys []UTXOKey

	for _, tx := range txs {
		if !tx.IsStake() {
			continue
		}
		k, txErr := r.applyOneTxLocked(tx, height)
		if txErr != nil {
			// Rollback: restore registry snapshot.
			r.validators = snap
			// Rollback: unmark any UTXOs staked by earlier txs in this block.
			for _, sk := range stakedKeys {
				r.utxos.UnmarkStaked(sk.TxHash, sk.OutputIndex)
			}
			return func() {}, txErr
		}
		if k != nil {
			stakedKeys = append(stakedKeys, *k)
		}
	}

	// Return a rollback closure for use if chain.AddBlock fails.
	// Captured snap and stakedKeys are final at this point.
	snapFinal := snap
	stakedFinal := stakedKeys
	rollback = func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.validators = snapFinal
		for _, sk := range stakedFinal {
			r.utxos.UnmarkStaked(sk.TxHash, sk.OutputIndex)
		}
	}
	return rollback, nil
}

// ValidateBlockStakeTxs performs an ordered, stateful dry-run of every stake
// transaction in the block BEFORE the block is added to the chain.  It:
//
//   - verifies each payload size, decode, and Ed25519 self-signature
//   - enforces the v1-deposit ban (C-1 fix)
//   - for v2 deposits: checks UTXO existence, commitment correctness, and
//     min/max amount against the live registry state + oracle-adjusted minimum
//   - detects duplicate burn UTXOs within the same block (two txs burning the
//     same UTXO would both pass independent checks but only the first is applied)
//   - simulates stateful registry changes in order so that later stake txs in the
//     block see the effect of earlier ones (top-ups, status changes, etc.)
//
// Returns a non-nil error naming the offending tx index if any check fails.
// The function returns an error before any state is mutated; the caller must
// reject the block outright.
func (r *ValidatorRegistry) ValidateBlockStakeTxs(txs []Transaction, height uint64) error {
	if r == nil {
		return nil
	}

	// Snapshot the effective minimum stake and current validator states under
	// the read lock, then release it before accessing the UTXO set.
	r.mu.RLock()
	effectiveMin := r.dynamicMinNAPR
	if effectiveMin == 0 {
		effectiveMin = MinStakeNAPR
	}
	type shadowEntry struct {
		stakeNAPR uint64
		status    ValidatorStatus
	}
	snapshot := make(map[string]shadowEntry, len(r.validators))
	for k, v := range r.validators {
		snapshot[k] = shadowEntry{stakeNAPR: v.StakeNAPR, status: v.Status}
	}
	r.mu.RUnlock()

	// Per-block reservation: burn UTXOs claimed by earlier txs in this block.
	type utxoKey struct {
		TxHash crypto.Hash32
		OutIdx uint32
	}
	reserved := make(map[utxoKey]int) // key → first tx index that reserved it

	for i, tx := range txs {
		if !tx.IsStake() {
			continue
		}
		if tx.Version != TxVersionStake {
			return fmt.Errorf("stake tx[%d]: wrong version %d", i, tx.Version)
		}

		switch len(tx.Extra) {

		// ── v1: withdraw / partial-withdraw ──────────────────────────────
		case StakePayloadSize:
			action, pub, amount, sig, err := DecodeStakeExtra(tx.Extra)
			if err != nil {
				return fmt.Errorf("stake tx[%d]: decode: %w", i, err)
			}
			if action == StakeDeposit {
				return fmt.Errorf("stake tx[%d]: StakeDeposit requires v2 payload with UTXO proof (C-1 fix)", i)
			}
			msg := StakeSignMsg(action, pub, amount)
			if !pub.Verify(msg, sig) {
				return fmt.Errorf("stake tx[%d]: invalid signature from %s", i, pub.ID())
			}
			key := pub.Hex()
			se, known := snapshot[key]
			switch action {
			case StakeWithdraw:
				if !known {
					return fmt.Errorf("stake tx[%d]: validator %s not registered", i, pub.ID()[:8])
				}
				if se.status == ValidatorUnbonding || se.status == ValidatorExited {
					return fmt.Errorf("stake tx[%d]: validator already unbonding/exited", i)
				}
				se.status = ValidatorUnbonding
				snapshot[key] = se
			case StakePartialWithdraw:
				if !known {
					return fmt.Errorf("stake tx[%d]: validator %s not registered", i, pub.ID()[:8])
				}
				if se.status == ValidatorUnbonding || se.status == ValidatorExited {
					return fmt.Errorf("stake tx[%d]: validator is unbonding/exited; cannot partial withdraw", i)
				}
				if amount == 0 {
					return fmt.Errorf("stake tx[%d]: partial withdraw amount must be > 0", i)
				}
				if amount >= se.stakeNAPR {
					return fmt.Errorf("stake tx[%d]: withdrawal amount %d >= current stake %d", i, amount, se.stakeNAPR)
				}
				remaining := se.stakeNAPR - amount
				if remaining < effectiveMin {
					return fmt.Errorf("stake tx[%d]: remaining stake %.4f APRO < minimum %.4f APRO",
						i, float64(remaining)/float64(BaseUnitsPerAPR), float64(effectiveMin)/float64(BaseUnitsPerAPR))
				}
				se.stakeNAPR = remaining
				snapshot[key] = se
			default:
				return fmt.Errorf("stake tx[%d]: unknown action %d", i, action)
			}

		// ── v2: UTXO-backed deposit ────────────────────────────────────
		case StakePayloadSizeV2:
			action, pub, amount, sig, burnTxHash, burnOutIdx, burnBlind, err := DecodeStakeExtraV2(tx.Extra)
			if err != nil {
				return fmt.Errorf("stake tx[%d]: decode v2: %w", i, err)
			}
			if action != StakeDeposit {
				return fmt.Errorf("stake tx[%d]: v2 payload only valid for StakeDeposit, got action=%d", i, action)
			}
			msg := StakeSignMsgV2(action, pub, amount, burnTxHash, burnOutIdx)
			if !pub.Verify(msg, sig) {
				return fmt.Errorf("stake tx[%d]: invalid v2 signature from %s", i, pub.ID())
			}
			// Within-block duplicate burn UTXO.
			uk := utxoKey{TxHash: burnTxHash, OutIdx: burnOutIdx}
			if firstIdx, dup := reserved[uk]; dup {
				return fmt.Errorf("stake tx[%d]: burn UTXO %x:%d already claimed by tx[%d] in this block",
					i, burnTxHash[:8], burnOutIdx, firstIdx)
			}
			// Cross-block UTXO existence and commitment check (C-1).
			if r.utxos == nil {
				return fmt.Errorf("stake tx[%d]: UTXO set not wired (C-1 check)", i)
			}
			burnUTXO := r.utxos.Get(burnTxHash, burnOutIdx)
			if burnUTXO == nil {
				return fmt.Errorf("stake tx[%d]: burn UTXO %x:%d not found in active set (C-1 check)",
					i, burnTxHash[:8], burnOutIdx)
			}
			expectedCommit, commitErr := crypto.Commit(amount, burnBlind)
			if commitErr != nil {
				return fmt.Errorf("stake tx[%d]: commitment computation failed: %w", i, commitErr)
			}
			if expectedCommit != burnUTXO.AmountCommit {
				return fmt.Errorf("stake tx[%d]: burn UTXO %x:%d AmountCommit mismatch (C-1 check)",
					i, burnTxHash[:8], burnOutIdx)
			}
			// Stateful deposit checks — must exactly mirror applyDeposit ordering.
			// Minimum check applies to ALL deposits (new and top-up) before any
			// other stateful check, matching applyDeposit's unconditional gate.
			if amount < effectiveMin {
				return fmt.Errorf("stake tx[%d]: deposit %.4f APRO < minimum %.4f APRO",
					i, float64(amount)/float64(BaseUnitsPerAPR), float64(effectiveMin)/float64(BaseUnitsPerAPR))
			}
			if amount > MaxStakeNAPR {
				return fmt.Errorf("stake tx[%d]: deposit %.4f APRO exceeds cap %.4f APRO",
					i, float64(amount)/float64(BaseUnitsPerAPR), float64(MaxStakeNAPR)/float64(BaseUnitsPerAPR))
			}
			key := pub.Hex()
			se, existing := snapshot[key]
			if existing {
				switch se.status {
				case ValidatorUnbonding, ValidatorExited:
					return fmt.Errorf("stake tx[%d]: validator is unbonding/exited; cannot stake", i)
				default:
					if se.stakeNAPR > math.MaxUint64-amount {
						return fmt.Errorf("stake tx[%d]: stake top-up overflow", i)
					}
					se.stakeNAPR += amount
				}
			} else {
				se = shadowEntry{stakeNAPR: amount, status: ValidatorPending}
			}
			reserved[uk] = i
			snapshot[key] = se

		default:
			return fmt.Errorf("stake tx[%d]: invalid extra length %d (expected %d or %d)",
				i, len(tx.Extra), StakePayloadSize, StakePayloadSizeV2)
		}
	}
	return nil
}

// ValidateStakeTx performs all pre-acceptance checks for a single stake
// transaction without applying any state changes.  Prefer ValidateBlockStakeTxs
// when validating multiple txs in one block (it also handles duplicate UTXOs
// and simulates stateful registry transitions in order).
//
// Checks performed:
//   - payload size and decode correctness
//   - Ed25519 self-signature (binds deposit to a specific UTXO)
//   - v1 StakeDeposit rejected (C-1 fix: v2 required)
//   - UTXO existence in the active set (C-1)
//   - Pedersen commitment match: Commit(amount, blind) == UTXO.AmountCommit (C-1)
func (r *ValidatorRegistry) ValidateStakeTx(tx Transaction) error {
	if tx.Version != TxVersionStake {
		return fmt.Errorf("registry: not a stake tx (version=%d)", tx.Version)
	}
	switch len(tx.Extra) {

	case StakePayloadSize:
		action, pub, amount, sig, err := DecodeStakeExtra(tx.Extra)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if action == StakeDeposit {
			return fmt.Errorf("registry: StakeDeposit requires v2 payload (173 bytes) with UTXO burn proof — v1 deposits no longer accepted (C-1 fix)")
		}
		msg := StakeSignMsg(action, pub, amount)
		if !pub.Verify(msg, sig) {
			return fmt.Errorf("registry: invalid stake signature from %s", pub.ID())
		}
		return nil

	case StakePayloadSizeV2:
		action, pub, amount, sig, burnTxHash, burnOutIdx, burnBlind, err := DecodeStakeExtraV2(tx.Extra)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		if action != StakeDeposit {
			return fmt.Errorf("registry: v2 payload (173 bytes) is only valid for StakeDeposit, got action=%d", action)
		}
		msg := StakeSignMsgV2(action, pub, amount, burnTxHash, burnOutIdx)
		if !pub.Verify(msg, sig) {
			return fmt.Errorf("registry: invalid stake signature (v2) from %s", pub.ID())
		}
		if r.utxos == nil {
			return fmt.Errorf("registry: UTXO set not wired — cannot verify deposit (C-1 check)")
		}
		burnUTXO := r.utxos.Get(burnTxHash, burnOutIdx)
		if burnUTXO == nil {
			return fmt.Errorf("registry: burn UTXO %x:%d not found in active set (C-1 check)",
				burnTxHash[:8], burnOutIdx)
		}
		expectedCommit, commitErr := crypto.Commit(amount, burnBlind)
		if commitErr != nil {
			return fmt.Errorf("registry: burn commitment computation failed: %w", commitErr)
		}
		if expectedCommit != burnUTXO.AmountCommit {
			return fmt.Errorf("registry: burn UTXO %x:%d AmountCommit mismatch — "+
				"claimed amount/blind does not open to the on-chain commitment (C-1 check)",
				burnTxHash[:8], burnOutIdx)
		}
		return nil

	default:
		return fmt.Errorf("registry: invalid stake extra length %d (expected %d for withdraw or %d for deposit)",
			len(tx.Extra), StakePayloadSize, StakePayloadSizeV2)
	}
}

// ReplayBlockStakeTxs re-applies stake transactions from a previously committed
// block during node startup.  It restores both the ValidatorRegistry entries and
// the UTXOSet.stakedUTXOs map so that burned collateral cannot be reused as a
// ring decoy or re-staked after a restart.
//
// Key differences from ApplyBlockStakeTxs (the live apply path):
//   - Signature is still verified (store-integrity check).
//   - UTXO existence in the active set is NOT checked — the UTXO was already
//     removed from the active set in the previous run and is no longer present.
//   - Pedersen commitment is NOT re-verified against the active UTXO set for the
//     same reason; it was verified when the block was first accepted.
//   - MarkStakedKnown is called instead of MarkStaked — idempotent, does not
//     require the UTXO to be in the active set.
//   - No rollback function is returned — committed blocks are never rolled back.
//
// Must be called in block-height order during startup before any peer connections
// or API traffic are accepted, so the registry and staked-UTXO set are complete
// before the first incoming stake tx or ring tx arrives.
func (r *ValidatorRegistry) ReplayBlockStakeTxs(txs []Transaction, height uint64) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tx := range txs {
		if !tx.IsStake() {
			continue
		}
		if err := r.replayOneTxLocked(tx, height); err != nil {
			return err
		}
	}
	return nil
}

// replayOneTxLocked applies one stake tx during startup replay.
// Assumes r.mu is held by the caller (Write lock).
func (r *ValidatorRegistry) replayOneTxLocked(tx Transaction, height uint64) error {
	if tx.Version != TxVersionStake {
		return nil // not a stake tx — nothing to do
	}
	switch len(tx.Extra) {

	case StakePayloadSize:
		action, pub, amount, sig, err := DecodeStakeExtra(tx.Extra)
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}
		msg := StakeSignMsg(action, pub, amount)
		if !pub.Verify(msg, sig) {
			return fmt.Errorf("replay: invalid stake sig from %s at height %d", pub.ID(), height)
		}
		key := pub.Hex()
		switch action {
		case StakeWithdraw:
			return r.applyWithdraw(key, height)
		case StakePartialWithdraw:
			return r.applyPartialWithdraw(key, amount, height)
		case StakeDeposit:
			// v1 deposits should not exist in a chain that enforces C-1, but handle
			// them gracefully for chains that predate the C-1 fix.
			return r.applyDeposit(key, pub, amount, height)
		default:
			return fmt.Errorf("replay: unknown stake action %d at height %d", action, height)
		}

	case StakePayloadSizeV2:
		action, pub, amount, sig, burnTxHash, burnOutIdx, burnBlind, err := DecodeStakeExtraV2(tx.Extra)
		if err != nil {
			return fmt.Errorf("replay: %w", err)
		}
		if action != StakeDeposit {
			return fmt.Errorf("replay: v2 payload with non-deposit action=%d at height %d", action, height)
		}
		msg := StakeSignMsgV2(action, pub, amount, burnTxHash, burnOutIdx)
		if !pub.Verify(msg, sig) {
			return fmt.Errorf("replay: invalid v2 stake sig from %s at height %d", pub.ID(), height)
		}
		// Reconstruct the burn-UTXO descriptor from payload data so MarkStakedKnown
		// can restore it to the staked set.  The OneTimePub is set to zero — staked
		// UTXOs are never used as ring decoys, so byPubKey lookup is irrelevant.
		expectedCommit, _ := crypto.Commit(amount, burnBlind)
		burnUTXO := &UTXO{
			TxHash:       burnTxHash,
			OutputIndex:  burnOutIdx,
			AmountCommit: expectedCommit,
		}
		if r.utxos != nil {
			r.utxos.MarkStakedKnown(burnTxHash, burnOutIdx, burnUTXO)
		}
		key := pub.Hex()
		return r.applyDeposit(key, pub, amount, height)

	default:
		return fmt.Errorf("replay: invalid stake extra length %d at height %d", len(tx.Extra), height)
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

