package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/aperod/aperod/crypto"
)

// mempoolDumpEntry is the on-disk representation of one mempool entry.
type mempoolDumpEntry struct {
	Tx       Transaction `json:"tx"`
	Received time.Time   `json:"received"`
}

// mempoolDump is the top-level JSON written to mempool.json.
type mempoolDump struct {
	SavedAt time.Time          `json:"saved_at"`
	Entries []mempoolDumpEntry `json:"entries"`
}

// MempoolConfig holds tuning parameters for the transaction pool.
type MempoolConfig struct {
	MaxSize        int           // maximum number of transactions
	MaxBytes       int           // maximum total byte size of all transactions (RAM cap)
	MaxTxSize      int           // maximum size of a single transaction in bytes
	TTL            time.Duration // evict transactions older than this
	BaseFeePerByte uint64        // current network base fee in nAPRO/byte (updated each block)
	// RingCTV4ActivationHeight and CurrentHeight let production mempools reject
	// transactions that cannot be included in the next block. Keeping this gate
	// at admission prevents callers from receiving a successful hash for a tx
	// that the block producer will immediately evict.
	RingCTV4ActivationHeight    uint64
	RingCTCLSAGActivationHeight uint64
	AVMActivationHeight         uint64
	CurrentHeight               func() uint64
	// Verifier performs full RingCT/ring-sig/range-proof verification in Add().
	// When nil the mempool only runs structural Validate() (dev/test mode).
	// Production nodes MUST set this to prevent C-0/C-1 inflation attacks.
	Verifier *TxVerifier
}

// DefaultMempoolConfig returns sensible production defaults.
func DefaultMempoolConfig() MempoolConfig {
	return MempoolConfig{
		MaxSize:        10_000,
		MaxBytes:       256 * 1024 * 1024, // 256 MB
		MaxTxSize:      100_000,
		TTL:            2 * time.Hour,
		BaseFeePerByte: InitialBaseFeePerByte, // 200 nAPRO/byte
	}
}

// mempoolEntry wraps a transaction with metadata.
type mempoolEntry struct {
	Tx       Transaction
	Hash     crypto.Hash32
	Size     int
	Received time.Time
	// privileged marks entries added via AddPrivileged (e.g. admin mint).
	// Privileged coinbase txs bypass the external-coinbase rejection guard
	// and are NOT stripped by the consensus engine's coinbase filter in
	// produceBlock, unlike coinbases that somehow bypass the public Add path.
	privileged bool
}

// bannedTxTTL is how long a banned tx hash is kept.  After expiry it may be
// re-accepted if somehow the underlying issue was resolved; in practice an
// invalid ring-sig tx will be re-banned within one block slot.
const bannedTxTTL = 24 * time.Hour

// Mempool is a thread-safe pool of pending (unconfirmed) transactions.
type Mempool struct {
	mu         sync.RWMutex
	cfg        MempoolConfig
	entries    map[crypto.Hash32]*mempoolEntry
	totalBytes int // running total of all entry sizes for RAM-cap enforcement
	// Track key images to detect double-spend attempts before they reach a block.
	keyImages map[crypto.KeyImage]crypto.Hash32 // ki → txHash
	// stakeSenders prevents scripted validators from flooding the mempool with
	// multiple stake transactions before any of them are confirmed.
	// Maps hex(validatorPubKey) → txHash of the pending stake TX.
	stakeSenders map[string]crypto.Hash32
	log          *slog.Logger
	// evictionsTotal counts every transaction evicted from the pool since
	// process start (TTL expiry, capacity-pressure fee-rate eviction, and
	// FIFO fallback).  Exposed via /api/v1/network/stats and /metrics so
	// operators can detect mempool-flood attacks in real time.  In-memory
	// only — resets to zero on node restart by design.
	evictionsTotal uint64
	// bannedHashes tracks tx hashes that have been banned from the pool because
	// they failed block production (e.g. invalid ring sig after snapshot repair,
	// missing ring member UTXOs, etc.).  Banned txes are rejected in Add() so
	// P2P peers cannot re-inject them and cause phantom key-image locks.
	// Expires after bannedTxTTL; cleaned during Evict().
	bannedHashes map[crypto.Hash32]time.Time
}

// NewMempool creates a new empty mempool with the given config.
// An optional *slog.Logger may be provided; if omitted a discard logger is used.
func NewMempool(cfg MempoolConfig, logger ...*slog.Logger) *Mempool {
	var l *slog.Logger
	if len(logger) > 0 && logger[0] != nil {
		l = logger[0]
	} else {
		l = slog.Default()
	}
	return &Mempool{
		cfg:          cfg,
		entries:      make(map[crypto.Hash32]*mempoolEntry),
		keyImages:    make(map[crypto.KeyImage]crypto.Hash32),
		stakeSenders: make(map[string]crypto.Hash32),
		bannedHashes: make(map[crypto.Hash32]time.Time),
		log:          l,
	}
}

// SetBaseFee updates the minimum fee rate used to validate incoming transactions.
// Called by the consensus engine after each accepted block so new txs must meet
// the network's current EIP-1559 base fee.
func (m *Mempool) SetBaseFee(baseFeePerByte uint64) {
	if baseFeePerByte < MinBaseFeePerByte {
		baseFeePerByte = MinBaseFeePerByte
	}
	m.mu.Lock()
	m.cfg.BaseFeePerByte = baseFeePerByte
	m.mu.Unlock()
}

// NextSpendVersion returns the RingCT format that can be included in the next
// block under the configured activation policy.
func (m *Mempool) NextSpendVersion() TxVersion {
	if m.cfg.CurrentHeight == nil {
		return TxVersionCommitmentBinding
	}
	nextHeight := m.cfg.CurrentHeight()
	if nextHeight < ^uint64(0) {
		nextHeight++
	}
	if m.cfg.RingCTCLSAGActivationHeight > 0 && nextHeight >= m.cfg.RingCTCLSAGActivationHeight {
		return TxVersionCLSAG
	}
	if m.cfg.RingCTV4ActivationHeight > 0 && nextHeight < m.cfg.RingCTV4ActivationHeight {
		return TxVersionBase
	}
	return TxVersionCommitmentBinding
}

// Add attempts to add a transaction to the mempool.
// Returns an error if the tx is invalid, duplicate, too large, or a double-spend.
func (m *Mempool) Add(tx Transaction) error {
	// Security: coinbase (zero-input) transactions are synthesized exclusively
	// by the consensus engine inside produceBlock.  Accepting one from an
	// external caller (P2P peer, admin RPC, etc.) would let an attacker inject
	// supply-creating UTXOs without spending anything.  Reject unconditionally
	// at this layer; the engine never routes its own coinbase through the pool.
	//
	// Stake transactions (TxVersionStake) also have zero inputs — their payload
	// is carried in Extra.  Exempt them from the coinbase rejection so they can
	// be submitted via the public POST /api/v1/stake broadcast endpoint.
	if tx.IsCoinbase() && !tx.IsStake() {
		return fmt.Errorf("mempool: coinbase (zero-input) transactions are not accepted from external sources")
	}

	if m.cfg.CurrentHeight != nil {
		nextHeight := m.cfg.CurrentHeight()
		if nextHeight < ^uint64(0) {
			nextHeight++
		}
		if err := ValidateTxVersionAtHeight(
			&tx,
			nextHeight,
			m.cfg.RingCTV4ActivationHeight,
			m.cfg.RingCTCLSAGActivationHeight,
			m.cfg.AVMActivationHeight,
		); err != nil {
			return fmt.Errorf("mempool: transaction activation policy: %w", err)
		}
	}

	if err := tx.Validate(); err != nil {
		return fmt.Errorf("mempool: invalid tx: %w", err)
	}

	// Full RingCT cryptographic verification (ring sigs, range proofs, Pedersen balance).
	// C-0/C-1: prevents supply inflation via forged AmountCommit or unbound stake amount.
	// Stake txs are included — they carry ring inputs whose proofs must be valid.
	if m.cfg.Verifier != nil {
		if err := m.cfg.Verifier.VerifyTx(&tx); err != nil {
			return fmt.Errorf("mempool: crypto verification failed: %w", err)
		}
	}

	size := tx.Size()
	if size > m.cfg.MaxTxSize {
		return fmt.Errorf("mempool: tx too large: %d bytes (max %d)", size, m.cfg.MaxTxSize)
	}

	// Stake transactions are fee-exempt:
	// stake = validator deposit/withdrawal (protocol-level, not ring-sig tx)
	if !tx.IsStake() {
		m.mu.RLock()
		baseFee := m.cfg.BaseFeePerByte
		m.mu.RUnlock()
		minFee := tx.MinFeeAt(baseFee)
		if tx.IsAVM() {
			gasFee, err := AVMGasFee(tx.AVM.GasLimit)
			if err != nil || minFee > ^uint64(0)-gasFee {
				return fmt.Errorf("mempool: AVM gas fee overflows")
			}
			minFee += gasFee
		}
		if burn, isBurn := tx.BurnAmount(); isBurn {
			if minFee > ^uint64(0)-burn || tx.Fee < minFee+burn {
				return fmt.Errorf("mempool: intentional burn fee too low: %d nAPRO < %d nAPRO minimum base fee plus burn",
					tx.Fee, minFee+burn)
			}
		}
		if tx.Fee < minFee {
			return fmt.Errorf("mempool: fee too low: %d nAPRO < %d nAPRO minimum (base plus reserved AVM gas)",
				tx.Fee, minFee)
		}
	}

	hash := tx.Hash()

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.entries[hash]; exists {
		return fmt.Errorf("mempool: duplicate tx %x", hash[:8])
	}

	// Reject banned txes (e.g. those that failed block production due to an
	// invalid ring sig after a snapshot repair / UTXO-set correction).
	// This prevents P2P peers from re-injecting permanently invalid txes and
	// causing phantom key-image locks that block legitimate withdrawals.
	if _, banned := m.bannedHashes[hash]; banned {
		return fmt.Errorf("mempool: tx %x is banned — failed block production and cannot be re-accepted", hash[:8])
	}

	// C-4: per-address stake-TX rate limit.
	// Only one stake deposit/withdrawal per validator pubkey may sit in the
	// mempool at a time.  Scripted depositors that fire multiple identical
	// transactions before the first is confirmed are rejected here with a
	// clear message rather than consuming block space and alert bandwidth.
	//
	// stakeExtraPubKey handles both v1 (105-byte withdraw) and v2 (173-byte
	// deposit) payloads — the pubkey occupies bytes [1:33] in both layouts.
	var stakeSenderKey string
	if tx.IsStake() {
		stakePub, err := stakeExtraPubKey(tx.Extra)
		if err != nil {
			return fmt.Errorf("mempool: malformed stake extra: %w", err)
		}
		stakeSenderKey = stakePub.Hex()
		if conflicting, pending := m.stakeSenders[stakeSenderKey]; pending {
			return fmt.Errorf("mempool: stake tx from validator %s already pending (tx %x); wait for confirmation before submitting another",
				stakeSenderKey[:8], conflicting[:8])
		}
	}

	// Check for double-spend via key images
	for _, inp := range tx.Inputs {
		if conflicting, spent := m.keyImages[inp.KeyImage]; spent {
			return fmt.Errorf("mempool: double-spend attempt, key image conflicts with tx %x",
				conflicting[:8])
		}
	}

	// Evict lowest-fee-rate transaction(s) if at capacity (count or bytes) to
	// resist mempool-flood DDoS.  A spammer filling the pool with dust-fee txs
	// gets their own transactions displaced first, preserving room for
	// legitimate high-value activity.
	if len(m.entries) >= m.cfg.MaxSize || (m.cfg.MaxBytes > 0 && m.totalBytes+size > m.cfg.MaxBytes) {
		m.log.Warn("mempool: capacity reached, evicting lowest-fee-rate tx",
			"count", len(m.entries),
			"total_bytes", m.totalBytes,
			"max_size", m.cfg.MaxSize,
			"max_bytes", m.cfg.MaxBytes,
		)
		for len(m.entries) >= m.cfg.MaxSize || (m.cfg.MaxBytes > 0 && m.totalBytes+size > m.cfg.MaxBytes) {
			if !m.evictLowestFeeRate() {
				// Pool is empty — nothing left to evict; reject the incoming tx.
				return fmt.Errorf("mempool: pool is at capacity and no transactions could be evicted to make room")
			}
		}
	}

	entry := &mempoolEntry{
		Tx:       tx,
		Hash:     hash,
		Size:     size,
		Received: time.Now(),
	}
	m.entries[hash] = entry
	m.totalBytes += size
	// Note: privileged defaults to false for Add() — external callers.

	for _, inp := range tx.Inputs {
		m.keyImages[inp.KeyImage] = hash
	}
	if stakeSenderKey != "" {
		m.stakeSenders[stakeSenderKey] = hash
	}

	return nil
}

// Remove removes a transaction from the mempool (called after block inclusion).
func (m *Mempool) Remove(hash crypto.Hash32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.entries[hash]; ok {
		for _, inp := range entry.Tx.Inputs {
			delete(m.keyImages, inp.KeyImage)
		}
		m.removeStakeSenderLocked(entry)
		m.totalBytes -= entry.Size
		if m.totalBytes < 0 {
			m.totalBytes = 0
		}
		delete(m.entries, hash)
	}
}

// removeStakeSenderLocked clears the stakeSenders entry for a stake tx entry.
// Must be called with m.mu held for writing.  Safe to call on non-stake entries
// (no-op).
func (m *Mempool) removeStakeSenderLocked(entry *mempoolEntry) {
	if !entry.Tx.IsStake() {
		return
	}
	stakePub, err := stakeExtraPubKey(entry.Tx.Extra)
	if err != nil {
		return
	}
	key := stakePub.Hex()
	// Only delete if the map still points to this entry's hash (not a later one).
	if m.stakeSenders[key] == entry.Hash {
		delete(m.stakeSenders, key)
	}
}

// stakeExtraPubKey extracts the validator public key from a stake Extra payload.
// Handles v1 (105-byte withdraw/partial-withdraw), v2 (173-byte deposit), and
// v3 (237-byte deposit with one-time-key ownership proof — F-049 fix) layouts.
// In all three cases the 32-byte pubkey occupies bytes [1:33].
func stakeExtraPubKey(extra []byte) (crypto.ValidatorPubKey, error) {
	if len(extra) != StakePayloadSize && len(extra) != StakePayloadSizeV2 && len(extra) != StakePayloadSizeV3 {
		return nil, fmt.Errorf("stake extra: expected %d or %d bytes, got %d",
			StakePayloadSize, StakePayloadSizeV2, len(extra))
	}
	return crypto.ValidatorPubKey(extra[1:33]), nil
}

// RemoveBlock removes all transactions included in the given block.
func (m *Mempool) RemoveBlock(block *Block) {
	for _, tx := range block.Txs {
		m.Remove(tx.Hash())
	}
}

// Get retrieves a transaction by hash.
func (m *Mempool) Get(hash crypto.Hash32) (Transaction, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[hash]
	if !ok {
		return Transaction{}, false
	}
	return e.Tx, true
}

// AddPrivileged adds a coinbase (or other engine-internal) transaction to the
// mempool bypassing the external-coinbase rejection guard in Add().
//
// MUST only be called by trusted internal code (admin RPC, consensus engine).
// Never call from P2P handlers or public API routes — use Add() for those.
// All other guards (size, duplicate, double-spend) still apply.
func (m *Mempool) AddPrivileged(tx Transaction) error {
	if err := tx.Validate(); err != nil {
		return fmt.Errorf("mempool: invalid tx: %w", err)
	}
	// C1 fix: run full cryptographic verification even for privileged transactions,
	// EXCEPT for coinbase (zero-input) transactions.  VerifyTx explicitly rejects
	// coinbase txs to prevent external inflation attacks; engine-synthesized mints
	// are trusted by construction and must not be re-verified through that path.
	// Any non-coinbase tx routed through AddPrivileged is still fully verified.
	if m.cfg.Verifier != nil && !tx.IsCoinbase() {
		if err := m.cfg.Verifier.VerifyTx(&tx); err != nil {
			return fmt.Errorf("mempool: crypto verification failed (privileged): %w", err)
		}
	}
	size := tx.Size()
	if size > m.cfg.MaxTxSize {
		return fmt.Errorf("mempool: tx too large: %d bytes (max %d)", size, m.cfg.MaxTxSize)
	}
	hash := tx.Hash()
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[hash]; exists {
		return fmt.Errorf("mempool: duplicate tx %x", hash[:8])
	}
	if len(m.entries) >= m.cfg.MaxSize || (m.cfg.MaxBytes > 0 && m.totalBytes+size > m.cfg.MaxBytes) {
		m.log.Warn("mempool: capacity reached, evicting lowest-fee-rate tx (privileged add)",
			"count", len(m.entries),
			"total_bytes", m.totalBytes,
		)
		for len(m.entries) >= m.cfg.MaxSize || (m.cfg.MaxBytes > 0 && m.totalBytes+size > m.cfg.MaxBytes) {
			if !m.evictLowestFeeRate() {
				return fmt.Errorf("mempool: pool is at capacity and no transactions could be evicted to make room")
			}
		}
	}
	entry := &mempoolEntry{
		Tx:         tx,
		Hash:       hash,
		Size:       size,
		Received:   time.Now(),
		privileged: true,
	}
	m.entries[hash] = entry
	m.totalBytes += size
	return nil
}

// IsPrivileged reports whether the tx identified by hash was added via
// AddPrivileged.  Used by produceBlock to decide whether to keep a coinbase
// that came from the pool (admin mint) vs. drop it (shouldn't exist, but
// defense-in-depth against any future bypass).
func (m *Mempool) IsPrivileged(hash crypto.Hash32) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if entry, ok := m.entries[hash]; ok {
		return entry.privileged
	}
	return false
}

// SelectTxs returns up to n transactions ordered by fee rate nAPRO/byte (highest first).
// Sorting by fee/size selects transactions that pay the best rate, maximising
// validator tip income and ensuring high-priority txs are included first.
// Used by the block proposer to fill a block.
func (m *Mempool) SelectTxs(n int) []Transaction {
	m.mu.RLock()
	defer m.mu.RUnlock()

	entries := make([]*mempoolEntry, 0, len(m.entries))
	for _, e := range m.entries {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		// fee rate = fee / size; avoid division by zero
		si, sj := entries[i].Size, entries[j].Size
		if si == 0 {
			si = 1
		}
		if sj == 0 {
			sj = 1
		}
		ri := entries[i].Tx.Fee / uint64(si)
		rj := entries[j].Tx.Fee / uint64(sj)
		return ri > rj
	})

	if n > len(entries) {
		n = len(entries)
	}
	txs := make([]Transaction, n)
	for i := 0; i < n; i++ {
		txs[i] = entries[i].Tx
	}
	return txs
}

// Count returns the number of pending transactions.
// MempoolConfig returns a snapshot of the config the pool was created with.
// Useful for exposing limits in metrics endpoints.
func (m *Mempool) MempoolConfig() MempoolConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

func (m *Mempool) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// BanTx removes a transaction from the pool and bans it from re-entry for
// bannedTxTTL (24 h).  The consensus engine calls this when a tx fails block
// production (e.g. invalid ring sig after a UTXO-set / snapshot repair) so
// that P2P peers cannot re-inject it and recreate a phantom key-image lock.
//
// Safe to call with a hash that is not currently in the pool (ban-only mode,
// e.g. for a tx that was never accepted but must be blocked pre-emptively).
func (m *Mempool) BanTx(hash crypto.Hash32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.entries[hash]; ok {
		for _, inp := range entry.Tx.Inputs {
			delete(m.keyImages, inp.KeyImage)
		}
		m.removeStakeSenderLocked(entry)
		m.totalBytes -= entry.Size
		if m.totalBytes < 0 {
			m.totalBytes = 0
		}
		delete(m.entries, hash)
		m.evictionsTotal++
	}
	m.bannedHashes[hash] = time.Now()
	m.log.Warn("mempool: tx banned — rejected from P2P re-entry for 24 h",
		"hash", fmt.Sprintf("%x", hash[:8]))
}

// IsBanned reports whether a tx hash is currently banned.
func (m *Mempool) IsBanned(hash crypto.Hash32) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.bannedHashes[hash]
	return ok
}

// TotalBytes returns the current total byte size of all pending transactions.
func (m *Mempool) TotalBytes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalBytes
}

// EvictionsTotal returns the number of transactions evicted from the pool
// since process start (TTL expiry + capacity-pressure evictions).
// The counter is in-memory only and resets on node restart.
func (m *Mempool) EvictionsTotal() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.evictionsTotal
}

// SetVerifier wires a TxVerifier into the mempool after creation.
// Called once the UTXO set has been populated from the stored chain.
// Until this is called the mempool runs in structural-only mode (dev/test).
func (m *Mempool) SetVerifier(v *TxVerifier) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Verifier = v
}

// Hashes returns all transaction hashes currently in the pool.
func (m *Mempool) Hashes() []crypto.Hash32 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]crypto.Hash32, 0, len(m.entries))
	for h := range m.entries {
		out = append(out, h)
	}
	return out
}

// Evict removes transactions older than TTL and cleans up expired ban entries.
func (m *Mempool) Evict() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	removed := 0
	for hash, e := range m.entries {
		if now.Sub(e.Received) > m.cfg.TTL {
			for _, inp := range e.Tx.Inputs {
				delete(m.keyImages, inp.KeyImage)
			}
			m.removeStakeSenderLocked(e)
			m.totalBytes -= e.Size
			if m.totalBytes < 0 {
				m.totalBytes = 0
			}
			delete(m.entries, hash)
			m.evictionsTotal++
			removed++
		}
	}
	// Expire old ban entries so the map doesn't grow unboundedly.
	for hash, bannedAt := range m.bannedHashes {
		if now.Sub(bannedAt) > bannedTxTTL {
			delete(m.bannedHashes, hash)
		}
	}
	return removed
}

// evictOldest removes the single oldest mempool entry (called under lock).
// Returns true if an entry was removed, false if the pool is empty.
func (m *Mempool) evictOldest() bool {
	var oldest *mempoolEntry
	for _, e := range m.entries {
		if oldest == nil || e.Received.Before(oldest.Received) {
			oldest = e
		}
	}
	if oldest == nil {
		return false // pool is empty
	}
	for _, inp := range oldest.Tx.Inputs {
		delete(m.keyImages, inp.KeyImage)
	}
	m.removeStakeSenderLocked(oldest)
	m.totalBytes -= oldest.Size
	if m.totalBytes < 0 {
		m.totalBytes = 0
	}
	delete(m.entries, oldest.Hash)
	m.evictionsTotal++
	return true
}

// mempoolPath returns the canonical path for the persisted mempool file.
func mempoolPath(dataDir string) string {
	return filepath.Join(dataDir, "mempool.json")
}

// staleTmpMaxAge is the threshold after which a leftover mempool.json.tmp file
// is considered stale and safe to delete on startup.
const staleTmpMaxAge = 5 * time.Minute

// CleanStaleTmpFiles removes any mempool.json.tmp file in dataDir that is
// older than staleTmpMaxAge (5 minutes).  This handles the rare case where an
// OOM kill interrupted Save() between WriteFile and Rename, leaving an orphaned
// .tmp file on disk.  The function is intentionally non-fatal: a failure to
// stat or remove the file is logged and ignored so node startup continues.
//
// Call this BEFORE Load() so Load never sees a half-written .tmp as a valid
// mempool.json (Load reads only the final path, so the .tmp is never read, but
// cleaning it up prevents indefinite disk accumulation over many crash cycles).
func CleanStaleTmpFiles(dataDir string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	tmp := mempoolPath(dataDir) + ".tmp"
	info, err := os.Stat(tmp)
	if err != nil {
		// File does not exist or is not accessible — nothing to clean up.
		return
	}
	age := time.Since(info.ModTime())
	if age < staleTmpMaxAge {
		// File is recent enough that it might belong to a concurrent Save().
		return
	}
	if err := os.Remove(tmp); err != nil {
		log.Warn("mempool: failed to remove stale tmp file (ignoring)",
			"path", tmp,
			"age", age.Round(time.Second).String(),
			"err", err,
		)
		return
	}
	log.Info("mempool: removed stale tmp file from previous crash",
		"path", tmp,
		"age", age.Round(time.Second).String(),
	)
}

// Save atomically persists all current mempool entries to dataDir/mempool.json.
// Entries older than TTL are skipped — they would be evicted on Load anyway.
// Non-fatal: caller may log and continue on error.
func (m *Mempool) Save(dataDir string) error {
	m.mu.RLock()
	now := time.Now()
	entries := make([]mempoolDumpEntry, 0, len(m.entries))
	for _, e := range m.entries {
		// Privileged admission is an in-memory authorization boundary. Never
		// write it (or its coinbase) to disk, where it could be forged and
		// replayed after restart.
		if e.privileged {
			continue
		}
		// Skip already-expired entries — no point persisting them.
		if m.cfg.TTL > 0 && now.Sub(e.Received) >= m.cfg.TTL {
			continue
		}
		entries = append(entries, mempoolDumpEntry{
			Tx:       e.Tx,
			Received: e.Received,
		})
	}
	m.mu.RUnlock()

	dump := mempoolDump{SavedAt: now, Entries: entries}
	data, err := json.Marshal(dump)
	if err != nil {
		return fmt.Errorf("mempool save: marshal: %w", err)
	}

	// Atomic write: temp file → rename.
	path := mempoolPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		// Remove the temp file on write failure — a partial write (e.g. ENOSPC
		// after the file was created) must never be left on disk, as it would
		// confuse the next startup's CleanStaleTmpFiles / Load path.
		_ = os.Remove(tmp)
		return fmt.Errorf("mempool save: write temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mempool save: rename: %w", err)
	}
	return nil
}

// Load reads dataDir/mempool.json and re-adds surviving transactions.
// Must be called AFTER SetVerifier so full cryptographic re-verification runs.
// Expired, duplicate, or invalid entries are silently dropped.
// Returns the number of transactions successfully restored.
func (m *Mempool) Load(dataDir string, log *slog.Logger) int {
	if log == nil {
		log = slog.Default()
	}
	path := mempoolPath(dataDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("mempool load: read error (ignoring)", "err", err)
		}
		return 0
	}

	var dump mempoolDump
	if err := json.Unmarshal(data, &dump); err != nil {
		log.Warn("mempool load: corrupt file removed to allow clean restart",
			"path", path,
			"err", err,
		)
		// Remove the corrupt file so subsequent restarts don't re-emit this
		// warning on every boot.  The node starts with an empty pool instead.
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			log.Warn("mempool load: failed to remove corrupt file (ignoring)",
				"path", path,
				"err", removeErr,
			)
		}
		return 0
	}

	now := time.Now()
	restored := 0
	skipped := 0
	for _, entry := range dump.Entries {
		// Drop entries that expired while the node was down.
		age := now.Sub(entry.Received)
		if m.cfg.TTL > 0 && age >= m.cfg.TTL {
			skipped++
			continue
		}

		// Disk state is never trusted to confer privileged admission. In
		// particular, a crafted legacy "privileged":true coinbase entry must
		// take the public Add path and be rejected.
		addErr := m.Add(entry.Tx)
		if addErr != nil {
			log.Debug("mempool load: skipping tx", "err", addErr)
			skipped++
			continue
		}

		// Restore original receive time so TTL eviction uses the real age.
		m.mu.Lock()
		h := entry.Tx.Hash()
		if e, ok := m.entries[h]; ok {
			e.Received = entry.Received
		}
		m.mu.Unlock()

		restored++
	}

	log.Info("mempool restored from disk",
		"restored", restored,
		"skipped", skipped,
		"saved_at", dump.SavedAt,
	)
	// Remove the file so a second restart with an empty chain does not replay
	// already-confirmed transactions (they will be rejected as double-spends,
	// but it is cleaner to delete it once it has been consumed).
	_ = os.Remove(path)
	return restored
}

// evictLowestFeeRate removes the lowest-fee-rate (fee/byte) non-system entry
// (called under lock).  Returns true if any entry was removed, false only when
// the pool is completely empty and nothing could be evicted.
//
// Priority order:
//  1. Lowest fee-rate regular (non-coinbase, non-stake) transaction — the
//     correct economic ordering that discourages dust-fee flooding.
//  2. Oldest entry of any kind — last-resort when only system transactions
//     remain but caps are still exceeded.
//
// Using fee-rate (fee/byte) rather than absolute fee means a large low-priority
// tx is evicted before a tiny one — maximising freed bytes per eviction step.
func (m *Mempool) evictLowestFeeRate() bool {
	var cheapest *mempoolEntry
	var cheapestRate uint64
	for _, e := range m.entries {
		if e.Tx.IsCoinbase() || e.Tx.IsStake() {
			continue
		}
		sz := uint64(e.Size)
		if sz == 0 {
			sz = 1
		}
		rate := e.Tx.Fee / sz
		if cheapest == nil || rate < cheapestRate {
			cheapest = e
			cheapestRate = rate
		}
	}
	if cheapest == nil {
		// Only system transactions remain — fall back to FIFO eviction so
		// the byte/count cap can still be enforced.
		return m.evictOldest()
	}
	for _, inp := range cheapest.Tx.Inputs {
		delete(m.keyImages, inp.KeyImage)
	}
	m.totalBytes -= cheapest.Size
	if m.totalBytes < 0 {
		m.totalBytes = 0
	}
	delete(m.entries, cheapest.Hash)
	m.evictionsTotal++
	return true
}
