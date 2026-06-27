package core

import (
        "fmt"
        "sort"
        "sync"
        "time"

        "github.com/aperod/aperod/crypto"
)

// MempoolConfig holds tuning parameters for the transaction pool.
type MempoolConfig struct {
        MaxSize   int           // maximum number of transactions
        MaxTxSize int           // maximum size of a single transaction in bytes
        TTL       time.Duration // evict transactions older than this
        MinFee    uint64        // minimum flat fee in nAPR (0.5 APR = 500_000_000 nAPR)
}

// DefaultMempoolConfig returns sensible production defaults.
func DefaultMempoolConfig() MempoolConfig {
        return MempoolConfig{
                MaxSize:   5_000,
                MaxTxSize: 100_000,
                TTL:       2 * time.Hour,
                MinFee:    500_000_000, // 0.5 APR
        }
}

// mempoolEntry wraps a transaction with metadata.
type mempoolEntry struct {
        Tx       Transaction
        Hash     crypto.Hash32
        Size     int
        Received time.Time
}

// Mempool is a thread-safe pool of pending (unconfirmed) transactions.
type Mempool struct {
        mu      sync.RWMutex
        cfg     MempoolConfig
        entries map[crypto.Hash32]*mempoolEntry
        // Track key images to detect double-spend attempts before they reach a block.
        keyImages map[crypto.KeyImage]crypto.Hash32 // ki → txHash
}

// NewMempool creates a new empty mempool with the given config.
func NewMempool(cfg MempoolConfig) *Mempool {
        return &Mempool{
                cfg:       cfg,
                entries:   make(map[crypto.Hash32]*mempoolEntry),
                keyImages: make(map[crypto.KeyImage]crypto.Hash32),
        }
}

// Add attempts to add a transaction to the mempool.
// Returns an error if the tx is invalid, duplicate, too large, or a double-spend.
func (m *Mempool) Add(tx Transaction) error {
        if err := tx.Validate(); err != nil {
                return fmt.Errorf("mempool: invalid tx: %w", err)
        }

        size := tx.Size()
        if size > m.cfg.MaxTxSize {
                return fmt.Errorf("mempool: tx too large: %d bytes (max %d)", size, m.cfg.MaxTxSize)
        }

        // Coinbase and stake transactions are fee-exempt:
        // coinbase = block reward / admin mint (Fee=0 by design)
        // stake    = validator deposit/withdrawal (protocol-level, not ring-sig tx)
        if !tx.IsCoinbase() && !tx.IsStake() && tx.Fee < m.cfg.MinFee {
                return fmt.Errorf("mempool: fee too low: %d < %d nAPR (minimum flat fee)", tx.Fee, m.cfg.MinFee)
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

        // Evict oldest if at capacity
        if len(m.entries) >= m.cfg.MaxSize {
                m.evictOldest()
        }

        entry := &mempoolEntry{
                Tx:       tx,
                Hash:     hash,
                Size:     size,
                Received: time.Now(),
        }
        m.entries[hash] = entry

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

// SelectTxs returns up to n transactions ordered by fee rate (highest first).
// Used by the block proposer to fill a block.
func (m *Mempool) SelectTxs(n int) []Transaction {
        m.mu.RLock()
        defer m.mu.RUnlock()

        entries := make([]*mempoolEntry, 0, len(m.entries))
        for _, e := range m.entries {
                entries = append(entries, e)
        }
        sort.Slice(entries, func(i, j int) bool {
                return entries[i].Tx.Fee > entries[j].Tx.Fee
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
                        delete(m.entries, hash)
                        removed++
                }
        }
        return removed
}

// evictOldest removes the single oldest mempool entry (called under lock).
func (m *Mempool) evictOldest() {
        var oldest *mempoolEntry
        for _, e := range m.entries {
                if oldest == nil || e.Received.Before(oldest.Received) {
                        oldest = e
                }
        }
        if oldest != nil {
                for _, inp := range oldest.Tx.Inputs {
                        delete(m.keyImages, inp.KeyImage)
                }
                delete(m.entries, oldest.Hash)
        }
}
