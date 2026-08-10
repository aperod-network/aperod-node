// Package store provides LevelDB-backed persistent storage for the Aperod node.
// All block data, UTXO set entries, and node metadata are stored here.
package store

import (
        "encoding/binary"
        "encoding/json"
        "fmt"

        "github.com/aperod/aperod/crypto"
        "github.com/syndtr/goleveldb/leveldb"
        "github.com/syndtr/goleveldb/leveldb/opt"
        "github.com/syndtr/goleveldb/leveldb/util"
)

// Key prefixes separate different namespaces in the single LevelDB instance.
var (
        prefixBlock      = []byte("b/")  // b/<hash32>              → Block JSON
        prefixHeight     = []byte("h/")  // h/<uint64>              → hash32 (canonical height index)
        prefixUTXO       = []byte("u/")  // u/<txhash32><outIdx4>   → UTXO JSON (all outputs ever created)
        prefixKeyImage   = []byte("k/")  // k/<keyimage32>          → 0x01 (spent)
        prefixMeta       = []byte("m/")  // m/<key>                 → value (metadata)
        prefixTxIdx      = []byte("t/")  // t/<txhash32>            → height[8]+txIdx[4] (tx location)
        prefixSpentUTXO  = []byte("su/") // su/<txhash32><outIdx4>  → 0x01 (spent-UTXO fast-start index)
        prefixStakeBlock = []byte("sb/") // sb/<height8>            → 0x01 (block contains stake txs)
)

// DB wraps a LevelDB database with typed methods for blockchain data.
type DB struct {
        db *leveldb.DB
}

// Open opens or creates a LevelDB database at path.
func Open(path string) (*DB, error) {
        opts := &opt.Options{
                BlockCacheCapacity:     32 * opt.MiB,
                WriteBuffer:            16 * opt.MiB,
                CompactionTableSize:    4 * opt.MiB,
        }
        db, err := leveldb.OpenFile(path, opts)
        if err != nil {
                return nil, fmt.Errorf("open leveldb %s: %w", path, err)
        }
        return &DB{db: db}, nil
}

// Recover opens a LevelDB database using RecoverFile, which rebuilds the
// on-disk SST files from the WAL and MANIFEST.  Use this when the normal
// Open fails or when a startup integrity check detects that a putSync write
// is not surviving reopen (symptom: corrupted SST entry overrides WAL).
// After Recover returns the DB is fully consistent and can be used normally.
func Recover(path string) (*DB, error) {
        opts := &opt.Options{
                BlockCacheCapacity:     32 * opt.MiB,
                WriteBuffer:            16 * opt.MiB,
                CompactionTableSize:    4 * opt.MiB,
        }
        db, err := leveldb.RecoverFile(path, opts)
        if err != nil {
                return nil, fmt.Errorf("recover leveldb %s: %w", path, err)
        }
        return &DB{db: db}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Compact triggers a full LevelDB compaction across the entire key range.
// This reclaims physical disk space that was logically freed by Delete calls
// (e.g. after pruning removes old TxData entries).  The operation is
// synchronous and may take several minutes on large databases; it is safe to
// interrupt — LevelDB remains consistent.  Call only when the node is stopped
// so LevelDB is not opened by two processes at the same time.
func (d *DB) Compact() error {
        return d.db.CompactRange(util.Range{})
}

// ─── Raw helpers ─────────────────────────────────────────────────────────────

func (d *DB) put(key, val []byte) error {
        return d.db.Put(key, val, nil)
}

// putSync writes key/val with fsync so the entry survives a subsequent
// process kill before a normal db.Close().  Use for critical repairs.
func (d *DB) putSync(key, val []byte) error {
        return d.db.Put(key, val, &opt.WriteOptions{Sync: true})
}

func (d *DB) get(key []byte) ([]byte, error) {
        val, err := d.db.Get(key, nil)
        if err == leveldb.ErrNotFound {
                return nil, nil
        }
        return val, err
}

func (d *DB) del(key []byte) error {
        return d.db.Delete(key, nil)
}

func (d *DB) has(key []byte) (bool, error) {
        return d.db.Has(key, nil)
}

// ─── Block storage ────────────────────────────────────────────────────────────

// StoredBlock is a JSON-serializable representation of a block.
// (In production use a more compact binary encoding like protobuf/CBOR.)
type StoredBlock struct {
        Height    uint64           `json:"height"`
        PrevHash  crypto.Hash32    `json:"prev_hash"`
        Hash      crypto.Hash32    `json:"hash"`
        Timestamp int64            `json:"timestamp"`
        Round     uint32           `json:"round"`
        TxCount   int              `json:"tx_count"`
        // Transactions are stored as raw JSON for simplicity.
        TxData    []json.RawMessage `json:"tx_data,omitempty"`
}

// PutBlock persists a block.
// The block body and height-index entry are written atomically in a single
// fsynced batch so that a SIGKILL immediately after this call cannot leave
// the height index pointing at a missing block or vice-versa.
func (d *DB) PutBlock(hash crypto.Hash32, sb *StoredBlock) error {
        data, err := json.Marshal(sb)
        if err != nil {
                return fmt.Errorf("marshal block: %w", err)
        }
        blockKey := append(append([]byte{}, prefixBlock...), hash[:]...)
        hKey := heightKey(sb.Height)
        batch := new(leveldb.Batch)
        batch.Put(blockKey, data)
        batch.Put(hKey, hash[:])
        return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

// GetBlock retrieves a block by hash.
func (d *DB) GetBlock(hash crypto.Hash32) (*StoredBlock, error) {
        key := append(prefixBlock, hash[:]...)
        data, err := d.get(key)
        if err != nil || data == nil {
                return nil, err
        }
        var sb StoredBlock
        if err := json.Unmarshal(data, &sb); err != nil {
                return nil, fmt.Errorf("unmarshal block: %w", err)
        }
        return &sb, nil
}

// GetBlockByHeight retrieves the canonical block hash at height h, then the block.
func (d *DB) GetBlockByHeight(height uint64) (*StoredBlock, error) {
        hKey := heightKey(height)
        hashBytes, err := d.get(hKey)
        if err != nil || hashBytes == nil {
                return nil, err
        }
        var hash crypto.Hash32
        copy(hash[:], hashBytes)
        return d.GetBlock(hash)
}

// ─── UTXO storage ─────────────────────────────────────────────────────────────

// StoredUTXO is the persistent UTXO representation.
type StoredUTXO struct {
        TxHash      crypto.Hash32    `json:"tx_hash"`
        OutputIndex uint32           `json:"output_index"`
        OneTimePub  crypto.Point32   `json:"one_time_pub"`
        TxPubKey    crypto.Point32   `json:"tx_pub_key"`
        AmountCommit crypto.Commitment `json:"amount_commit"`
        EncAmount   [8]byte          `json:"enc_amount"`
        BlockHeight uint64           `json:"block_height"`
}

// PutUTXO persists a UTXO.
func (d *DB) PutUTXO(txHash crypto.Hash32, outIdx uint32, u *StoredUTXO) error {
        data, err := json.Marshal(u)
        if err != nil {
                return err
        }
        key := utxoKey(txHash, outIdx)
        return d.put(key, data)
}

// GetUTXO retrieves a UTXO. Returns nil if not found.
func (d *DB) GetUTXO(txHash crypto.Hash32, outIdx uint32) (*StoredUTXO, error) {
        data, err := d.get(utxoKey(txHash, outIdx))
        if err != nil || data == nil {
                return nil, err
        }
        var u StoredUTXO
        if err := json.Unmarshal(data, &u); err != nil {
                return nil, err
        }
        return &u, nil
}

// DeleteUTXO removes a UTXO (when spent).
func (d *DB) DeleteUTXO(txHash crypto.Hash32, outIdx uint32) error {
        return d.del(utxoKey(txHash, outIdx))
}

// ─── Spent-UTXO fast-start index (su/) ───────────────────────────────────────
//
// The su/ namespace records which UTXOs have been spent so that
// IterActiveUTXOs can reconstruct the active set without scanning blocks.
// Entries are written by the OnUTXOSpent callback wired in main.go; they
// are never deleted (spent UTXOs stay spent on a linear chain).

// spentUTXOKey builds the su/ key for (txHash, outIdx).
func spentUTXOKey(txHash crypto.Hash32, outIdx uint32) []byte {
        key := make([]byte, len(prefixSpentUTXO)+32+4)
        n := copy(key, prefixSpentUTXO)
        copy(key[n:], txHash[:])
        binary.BigEndian.PutUint32(key[n+32:], outIdx)
        return key
}

// MarkUTXOSpent records that the output (txHash, outIdx) has been consumed by
// a spending transaction.  Called from the OnUTXOSpent callback wired in
// main.go; non-fatal on error (index is a startup-performance optimisation).
func (d *DB) MarkUTXOSpent(txHash crypto.Hash32, outIdx uint32) error {
        return d.put(spentUTXOKey(txHash, outIdx), []byte{0x01})
}

// IsUTXOSpent reports whether the given output has been marked as spent in the
// su/ index.  Returns false on any lookup error (conservative — treats unknown
// as unspent so callers can re-check with GetUTXO if needed).
func (d *DB) IsUTXOSpent(txHash crypto.Hash32, outIdx uint32) bool {
        data, err := d.get(spentUTXOKey(txHash, outIdx))
        return err == nil && data != nil
}

// SpentUTXOIndexSize returns the number of entries in the su/ index.
// Used at startup to detect whether the index has been populated by at least
// one previous full-scan run.
func (d *DB) SpentUTXOIndexSize() (int, error) {
        iter := d.db.NewIterator(util.BytesPrefix(prefixSpentUTXO), nil)
        defer iter.Release()
        n := 0
        for iter.Next() {
                n++
        }
        return n, iter.Error()
}

// IterActiveUTXOs calls fn for every UTXO that has NOT been marked spent.
// Phase 1 collects the complete su/ spent-set (a sequential scan with good
// locality); Phase 2 iterates u/ and skips any entry whose key appears in
// the spent-set.  This avoids N random point-lookups while still being a
// single linear pass over each namespace.
func (d *DB) IterActiveUTXOs(fn func(*StoredUTXO) error) error {
        // Phase 1: build in-memory spent-UTXO key set from su/.
        const keyLen = 36 // 32-byte txHash + 4-byte outIdx
        type utxoID [keyLen]byte
        spent := make(map[utxoID]struct{})
        spentIter := d.db.NewIterator(util.BytesPrefix(prefixSpentUTXO), nil)
        for spentIter.Next() {
                suffix := spentIter.Key()[len(prefixSpentUTXO):]
                if len(suffix) == keyLen {
                        var id utxoID
                        copy(id[:], suffix)
                        spent[id] = struct{}{}
                }
        }
        spentIter.Release()
        if err := spentIter.Error(); err != nil {
                return fmt.Errorf("iter spent-utxo index: %w", err)
        }

        // Phase 2: iterate u/ and skip spent entries.
        iter := d.db.NewIterator(util.BytesPrefix(prefixUTXO), nil)
        defer iter.Release()
        for iter.Next() {
                var u StoredUTXO
                if err := json.Unmarshal(iter.Value(), &u); err != nil {
                        return err
                }
                var id utxoID
                copy(id[:32], u.TxHash[:])
                binary.BigEndian.PutUint32(id[32:], u.OutputIndex)
                if _, isSpent := spent[id]; isSpent {
                        continue
                }
                if err := fn(&u); err != nil {
                        return err
                }
        }
        return iter.Error()
}

// ─── Stake-block height index (sb/) ──────────────────────────────────────────
//
// The sb/ namespace records which block heights contain stake transactions.
// Together with the su/ index it lets the startup fast-path replay only the
// small fraction of blocks that carry stake logic, skipping the rest entirely.

// stakeBlockKey builds the sb/ key for height h.
func stakeBlockKey(h uint64) []byte {
        key := make([]byte, len(prefixStakeBlock)+8)
        n := copy(key, prefixStakeBlock)
        binary.BigEndian.PutUint64(key[n:], h)
        return key
}

// PutStakeBlockHeight records that height h contains stake transactions.
func (d *DB) PutStakeBlockHeight(h uint64) error {
        return d.put(stakeBlockKey(h), []byte{0x01})
}

// HasStakeBlockIndex returns true when the sb/ namespace has at least one
// entry, meaning PutStakeBlockHeight has been called during a prior run.
func (d *DB) HasStakeBlockIndex() (bool, error) {
        iter := d.db.NewIterator(util.BytesPrefix(prefixStakeBlock), nil)
        defer iter.Release()
        has := iter.Next()
        return has, iter.Error()
}

// IterStakeBlockHeights calls fn for every indexed stake-block height in
// ascending order.
func (d *DB) IterStakeBlockHeights(fn func(uint64) error) error {
        iter := d.db.NewIterator(util.BytesPrefix(prefixStakeBlock), nil)
        defer iter.Release()
        for iter.Next() {
                suffix := iter.Key()[len(prefixStakeBlock):]
                if len(suffix) != 8 {
                        continue
                }
                h := binary.BigEndian.Uint64(suffix)
                if err := fn(h); err != nil {
                        return err
                }
        }
        return iter.Error()
}

// ─── Key Image tracking ───────────────────────────────────────────────────────

// MarkKeyImageSpent records that a key image has been used.
// The key image is normalised to its canonical prime-order representative
// before storage so that any torsion variant maps to the same DB entry.
// If canonicalization fails (malformed / non-prime-order point), the raw
// key image is stored as a best-effort guard — consistent with the fallback
// in UTXOSet.MarkSpent so that the two indexes never disagree.
func (d *DB) MarkKeyImageSpent(ki crypto.KeyImage) error {
        canonical, err := crypto.CanonicalKeyImage(ki)
        if err != nil {
                // Store raw image; non-canonical torsion variants will
                // still be caught because IsKeyImageSpent has the same fallback.
                key := append(append([]byte{}, prefixKeyImage...), ki[:]...)
                return d.put(key, []byte{0x01})
        }
        key := append(append([]byte{}, prefixKeyImage...), canonical[:]...)
        return d.put(key, []byte{0x01})
}

// IsKeyImageSpent returns true if the key image has been recorded as spent.
// Normalises to the canonical representative before lookup so that any
// torsion variant of a spent key image is correctly detected as spent.
// Falls back to the raw key image on canonicalization failure, mirroring
// the MarkKeyImageSpent fallback path.
func (d *DB) IsKeyImageSpent(ki crypto.KeyImage) (bool, error) {
        canonical, err := crypto.CanonicalKeyImage(ki)
        if err != nil {
                key := append(append([]byte{}, prefixKeyImage...), ki[:]...)
                return d.has(key)
        }
        key := append(append([]byte{}, prefixKeyImage...), canonical[:]...)
        return d.has(key)
}

// ─── Metadata ─────────────────────────────────────────────────────────────────

// PutMeta stores a metadata key-value pair.
func (d *DB) PutMeta(key string, value []byte) error {
        k := append(prefixMeta, []byte(key)...)
        return d.put(k, value)
}

// GetMeta retrieves a metadata value. Returns nil if not found.
func (d *DB) GetMeta(key string) ([]byte, error) {
        k := append(prefixMeta, []byte(key)...)
        return d.get(k)
}

// ─── Staking pool ─────────────────────────────────────────────────────────────

// StoreStakingPoolRemaining persists the remaining staking reward pool in nAPRO.
// Called after every block accepted so the balance survives a restart.
func (d *DB) StoreStakingPoolRemaining(napro uint64) error {
        buf := make([]byte, 8)
        binary.LittleEndian.PutUint64(buf, napro)
        return d.PutMeta("staking_pool_remaining", buf)
}

// LoadStakingPoolRemaining returns the stored staking pool balance in nAPRO.
// Returns (0, false, nil) if the value has never been stored (first boot with pool enabled).
func (d *DB) LoadStakingPoolRemaining() (napro uint64, found bool, err error) {
        v, err := d.GetMeta("staking_pool_remaining")
        if err != nil {
                return 0, false, err
        }
        if v == nil {
                return 0, false, nil
        }
        if len(v) != 8 {
                return 0, false, fmt.Errorf("store: staking_pool_remaining corrupted (%d bytes, want 8)", len(v))
        }
        return binary.LittleEndian.Uint64(v), true, nil
}

// StoreTxTotal persists the cumulative non-coinbase transaction count as a
// metadata value so the API server can restore the counter after a restart
// without scanning all blocks.
func (d *DB) StoreTxTotal(n int64) error {
        buf := make([]byte, 8)
        binary.LittleEndian.PutUint64(buf, uint64(n))
        return d.PutMeta("tx_total", buf)
}

// LoadTxTotal returns the stored cumulative transaction count.
// Returns (0, nil) if the value has never been stored (e.g. fresh chain).
func (d *DB) LoadTxTotal() (int64, error) {
        v, err := d.GetMeta("tx_total")
        if err != nil {
                return 0, err
        }
        if v == nil {
                return 0, nil
        }
        if len(v) != 8 {
                return 0, fmt.Errorf("store: tx_total metadata corrupted (got %d bytes, want 8)", len(v))
        }
        return int64(binary.LittleEndian.Uint64(v)), nil
}

// PutTip records the current canonical chain tip hash and height.
// Both metadata keys are written atomically in a single fsynced batch so
// that a SIGKILL cannot leave tip/hash and tip/height out of sync with
// each other or with the block data written by PutBlock/PutRawBlock.
func (d *DB) PutTip(hash crypto.Hash32, height uint64) error {
        var hb [8]byte
        binary.LittleEndian.PutUint64(hb[:], height)
        hashKey := append(append([]byte{}, prefixMeta...), []byte("tip/hash")...)
        heightKey2 := append(append([]byte{}, prefixMeta...), []byte("tip/height")...)
        batch := new(leveldb.Batch)
        batch.Put(hashKey, hash[:])
        batch.Put(heightKey2, hb[:])
        return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

// GetTip returns the stored tip hash and height. Returns zero values if not set.
func (d *DB) GetTip() (hash crypto.Hash32, height uint64, err error) {
        hb, err := d.GetMeta("tip/hash")
        if err != nil || hb == nil {
                return
        }
        copy(hash[:], hb)
        hbh, err := d.GetMeta("tip/height")
        if err != nil || len(hbh) < 8 {
                return
        }
        height = binary.LittleEndian.Uint64(hbh)
        return
}

// ─── Raw block storage ────────────────────────────────────────────────────────

// PutRawBlock stores a fully-serialized block (as raw bytes) keyed by its hash,
// and maintains the height → hash index. Used by node to persist core.Block data.
// Both entries are written atomically in a single fsynced batch so that a SIGKILL
// cannot leave a height-index entry pointing at a missing block or vice-versa.
func (d *DB) PutRawBlock(hash crypto.Hash32, height uint64, data []byte) error {
        blockKey := append(append([]byte{}, prefixBlock...), hash[:]...)
        hKey := heightKey(height)
        batch := new(leveldb.Batch)
        batch.Put(blockKey, data)
        batch.Put(hKey, hash[:])
        return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

// GetRawBlock returns the raw bytes for a block by hash. Returns nil if not found.
func (d *DB) GetRawBlock(hash crypto.Hash32) ([]byte, error) {
        key := append(prefixBlock, hash[:]...)
        return d.get(key)
}

// RepairHeightIndex overwrites the height → hash index entry for height with
// the supplied hash.  Use this to correct a zeroed or missing height-index
// entry that was detected by the startup integrity check; the block data
// (stored under its hash key) is left untouched.
// putSync is used so the write is fsynced before returning — this guarantees
// persistence even if the process is killed immediately after the call.
func (d *DB) RepairHeightIndex(height uint64, hash crypto.Hash32) error {
        return d.putSync(heightKey(height), hash[:])
}

// GetRawBlockByHeight returns the raw bytes for the canonical block at height h.
func (d *DB) GetRawBlockByHeight(height uint64) ([]byte, error) {
        hashBytes, err := d.get(heightKey(height))
        if err != nil || hashBytes == nil {
                return nil, err
        }
        var hash crypto.Hash32
        copy(hash[:], hashBytes)
        return d.GetRawBlock(hash)
}

// StoreSnapshotSaveDuration persists the wall-clock duration (in milliseconds)
// of the most recent successful shutdown snapshot save.  The value survives a
// process restart so checkStartupSnapshotTiming can warn on the next boot when
// the observed save time already approaches the configured TimeoutStopSec —
// before waiting for the next shutdown to discover the problem.
func (d *DB) StoreSnapshotSaveDuration(ms int64) error {
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, uint64(ms))
	return d.PutMeta("last_snap_save_ms", buf)
}

// LoadSnapshotSaveDuration returns the persisted snapshot save duration in
// milliseconds.  Returns (0, false, nil) when no value has been stored yet
// (e.g. first boot or a pre-feature DB).
func (d *DB) LoadSnapshotSaveDuration() (ms int64, found bool, err error) {
	v, err := d.GetMeta("last_snap_save_ms")
	if err != nil {
		return 0, false, err
	}
	if v == nil {
		return 0, false, nil
	}
	if len(v) != 8 {
		return 0, false, fmt.Errorf("store: last_snap_save_ms metadata corrupted (%d bytes, want 8)", len(v))
	}
	return int64(binary.LittleEndian.Uint64(v)), true, nil
}

// ─── Iteration helpers ────────────────────────────────────────────────────────

// StoreActiveUTXOCount persists the number of currently-active (unspent)
// UTXOs for the snapshot identified by tipHashHex.  Keying by the block hash
// ensures that concurrent goroutines saving snapshots at different heights
// write to separate DB entries and cannot overwrite each other.  The value is
// used by the startup divergence check to compare the snapshot's declared
// active UTXO count against an authoritative count recorded at save time.
func (d *DB) StoreActiveUTXOCount(tipHashHex string, n int) error {
        buf := make([]byte, 8)
        binary.LittleEndian.PutUint64(buf, uint64(n))
        return d.PutMeta("active_utxo_count/"+tipHashHex, buf)
}

// LoadActiveUTXOCount returns the active (unspent) UTXO count that was stored
// for the snapshot identified by tipHashHex.  ok is false when no entry exists
// for this specific hash (e.g. the process crashed before the metadata write,
// or the snapshot pre-dates this feature); the caller should skip the
// divergence check in that case rather than treating zero as a valid count.
func (d *DB) LoadActiveUTXOCount(tipHashHex string) (n int, ok bool, err error) {
        v, err := d.GetMeta("active_utxo_count/" + tipHashHex)
        if err != nil {
                return 0, false, err
        }
        if v == nil {
                return 0, false, nil
        }
        if len(v) != 8 {
                return 0, false, fmt.Errorf("store: active_utxo_count metadata corrupted (got %d bytes, want 8)", len(v))
        }
        return int(binary.LittleEndian.Uint64(v)), true, nil
}

// IterUTXOs calls fn for every UTXO in the database (for scanning).
func (d *DB) IterUTXOs(fn func(*StoredUTXO) error) error {
        iter := d.db.NewIterator(util.BytesPrefix(prefixUTXO), nil)
        defer iter.Release()
        for iter.Next() {
                var u StoredUTXO
                if err := json.Unmarshal(iter.Value(), &u); err != nil {
                        return err
                }
                if err := fn(&u); err != nil {
                        return err
                }
        }
        return iter.Error()
}

// IterKeyImages calls fn for every spent key image recorded in the database.
func (d *DB) IterKeyImages(fn func(crypto.KeyImage) error) error {
        iter := d.db.NewIterator(util.BytesPrefix(prefixKeyImage), nil)
        defer iter.Release()
        for iter.Next() {
                key := iter.Key()
                raw := key[len(prefixKeyImage):]
                if len(raw) != 32 {
                        continue
                }
                var ki crypto.KeyImage
                copy(ki[:], raw)
                if err := fn(ki); err != nil {
                        return err
                }
        }
        return iter.Error()
}

// ─── Tx location index ────────────────────────────────────────────────────────

// TxIdxEntry is the stored location for a transaction hash.
type TxIdxEntry struct {
        Height uint64
        TxIdx  int
}

// PutTxIdx persists the canonical location (block height, tx position) for a
// transaction hash.  Called by storeBlock so that FastForwardWithIndex can
// restore the in-memory tx index at startup without recomputing tx.Hash().
func (d *DB) PutTxIdx(txHash crypto.Hash32, height uint64, txIdx int) error {
        val := make([]byte, 12)
        binary.BigEndian.PutUint64(val[:8], height)
        binary.BigEndian.PutUint32(val[8:], uint32(txIdx))
        key := append([]byte(nil), prefixTxIdx...)
        key = append(key, txHash[:]...)
        return d.put(key, val)
}

// LoadTxIndex loads all tx-index entries whose block height is >= minHeight.
// Returns (nil, nil) when no entries exist yet (node predates this feature).
func (d *DB) LoadTxIndex(minHeight uint64) (map[crypto.Hash32]TxIdxEntry, error) {
        iter := d.db.NewIterator(util.BytesPrefix(prefixTxIdx), nil)
        defer iter.Release()

        result := make(map[crypto.Hash32]TxIdxEntry)
        prefixLen := len(prefixTxIdx)
        for iter.Next() {
                key := iter.Key()
                if len(key) < prefixLen+32 {
                        continue
                }
                val := iter.Value()
                if len(val) < 12 {
                        continue
                }
                height := binary.BigEndian.Uint64(val[:8])
                if height < minHeight {
                        continue
                }
                txIdx := int(binary.BigEndian.Uint32(val[8:]))
                var hash crypto.Hash32
                copy(hash[:], key[prefixLen:])
                result[hash] = TxIdxEntry{Height: height, TxIdx: txIdx}
        }
        if err := iter.Error(); err != nil {
                return nil, err
        }
        if len(result) == 0 {
                return nil, nil // index not yet populated
        }
        return result, nil
}

// LookupTxIdx returns the persisted location (block height, tx position) for a
// single transaction hash.  Returns (nil, nil) when the hash is not in the
// index (e.g. the block predates the tx-index feature or has been pruned).
// This is the point-lookup complement of LoadTxIndex (which scans a range).
func (d *DB) LookupTxIdx(txHash crypto.Hash32) (*TxIdxEntry, error) {
        key := append(append([]byte{}, prefixTxIdx...), txHash[:]...)
        val, err := d.db.Get(key, nil)
        if err == leveldb.ErrNotFound {
                return nil, nil
        }
        if err != nil {
                return nil, fmt.Errorf("tx index lookup %x: %w", txHash[:4], err)
        }
        if len(val) < 12 {
                return nil, nil
        }
        return &TxIdxEntry{
                Height: binary.BigEndian.Uint64(val[:8]),
                TxIdx:  int(binary.BigEndian.Uint32(val[8:])),
        }, nil
}

// CountMissingHeights returns the number of missing h/ height-index entries in
// the range [1, tipHeight] by iterating the sorted height-prefix keys in one
// pass.  It also returns the lowest missing height (firstMissing = 0 when
// nothing is missing).  This is used by the --check-store bootstrap sanity
// check to detect LevelDB gaps introduced by rsyncing a live database.
func (d *DB) CountMissingHeights(tipHeight uint64) (missing uint64, firstMissing uint64, err error) {
        if tipHeight == 0 {
                return 0, 0, nil
        }
        startKey := heightKey(1)
        endKey := heightKey(tipHeight + 1) // exclusive upper bound
        iter := d.db.NewIterator(&util.Range{Start: startKey, Limit: endKey}, nil)
        defer iter.Release()

        expected := uint64(1)
        for iter.Next() {
                key := iter.Key()
                if len(key) < len(prefixHeight)+8 {
                        continue
                }
                h := binary.BigEndian.Uint64(key[len(prefixHeight):])
                // Record each height in [expected, h-1] as missing.
                for ; expected < h; expected++ {
                        if missing == 0 {
                                firstMissing = expected
                        }
                        missing++
                }
                expected = h + 1
        }
        // Trailing gap: heights in [expected, tipHeight] are absent.
        for ; expected <= tipHeight; expected++ {
                if missing == 0 {
                        firstMissing = expected
                }
                missing++
        }
        err = iter.Error()
        return
}

// ─── Key builders ─────────────────────────────────────────────────────────────

func heightKey(h uint64) []byte {
        key := make([]byte, len(prefixHeight)+8)
        copy(key, prefixHeight)
        binary.BigEndian.PutUint64(key[len(prefixHeight):], h)
        return key
}

func utxoKey(txHash crypto.Hash32, outIdx uint32) []byte {
        key := make([]byte, len(prefixUTXO)+32+4)
        copy(key, prefixUTXO)
        copy(key[len(prefixUTXO):], txHash[:])
        binary.LittleEndian.PutUint32(key[len(prefixUTXO)+32:], outIdx)
        return key
}
