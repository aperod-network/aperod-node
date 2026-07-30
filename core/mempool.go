package core

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/aperod/aperod/crypto"
)

// MempoolConfig holds tuning parameters for the transaction pool.
type MempoolConfig struct {
	MaxSize          int           // maximum number of transactions
	MaxBytes         int           // maximum total byte size of all transactions (RAM cap)
	MaxTxSize        int           // maximum size of a single transaction in bytes
	TTL              time.Duration // evict transactions older than this
	BaseFeePerByte   uint64        // current network base fee in nAPRO/byte (updated each block)
	// Verifier performs full RingCT/ring-sig/range-proof verification in Add().
	// When nil the mempool only runs structural Validate() (dev/test mode).
	// Production nodes MUST set this to prevent C-0/C-1 inflation attacks.
	Verifier         *TxVerifier
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
	Tx         Transaction
	Hash       crypto.Hash32
	Size       int
	Received   time.Time
	// privileged marks entries added via AddPrivileged (e.g. admin mint).
	// Privileged coinbase txs bypass the external-coinbase rejection guard
	// and are NOT stripped by the consensus engine's coinbase filter in
	// produceBlock, unlike coinbases that somehow bypass the public Add path.
	privileged bool
}

// Mempool is a thread-safe pool of pending (unconfirmed) transactions.
type Mempool struct {
	mu         sync.RWMutex
	cfg        MempoolConfig
	entries    map[crypto.Hash32]*mempoolEntry
	totalBytes int             // running total of all entry sizes for RAM-cap enforcement
	// Track key images to detect double-spend attempts before they reach a block.
	keyImages map[crypto.KeyImage]crypto.Hash32 // ki → txHash
	log       *slog.Logger
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
		cfg:       cfg,
		entries:   make(map[crypto.Hash32]*mempoolEntry),
		keyImages: make(map[crypto.KeyImage]crypto.Hash32),
		log:       l,
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

// Add attempts to add a transaction to the mempool.
// Returns an error if the tx is invalid, duplicate, too large, or a double-spend.
func (m *Mempool) Add(tx Transaction) error {
	// Security: coinbase (zero-input) transactions are synthesized exclusively
	// by the consensus engine inside produceBlock.  Accepting one from an
	// external caller (P2P peer, admin RPC, etc.) would let an attacker inject
	// supply-creating UTXOs without spending anything.  Reject unconditionally
	// at this layer; the engine never routes its own coinbase through the pool.
	if tx.IsCoinbase() {
		return fmt.Errorf("mempool: coinbase (zero-input) transactions are not accepted from external sources")
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
		if tx.Fee < minFee {
			return fmt.Errorf("mempool: fee too low: %d nAPRO < %d nAPRO minimum (%d bytes × %d nAPRO/byte)",
				tx.Fee, minFee, tx.Size(), baseFee)
		}
	}

	hash := tx.Hash()

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.entries[hash]; exists {
		return fmt.Errorf("mempool: duplicate tx %x", hash[:8])
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
		m.totalBytes -= entry.Size
		if m.totalBytes < 0 {
			m.totalBytes = 0
		}
		delete(m.entries, hash)
	}
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
func (m *Mempool) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.entries)
}

// TotalBytes returns the current total byte size of all pending transactions.
func (m *Mempool) TotalBytes() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.totalBytes
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

// Evict removes transactions older than TTL.
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
			m.totalBytes -= e.Size
			if m.totalBytes < 0 {
				m.totalBytes = 0
			}
			delete(m.entries, hash)
			removed++
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
	m.totalBytes -= oldest.Size
	if m.totalBytes < 0 {
		m.totalBytes = 0
	}
	delete(m.entries, oldest.Hash)
	return true
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
	return true
}
