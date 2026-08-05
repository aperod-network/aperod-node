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
        prefixBlock    = []byte("b/") // b/<hash32> → Block JSON
        prefixHeight   = []byte("h/") // h/<uint64> → hash32 (canonical height index)
        prefixUTXO     = []byte("u/") // u/<txhash>/<outIdx> → UTXO JSON
        prefixKeyImage = []byte("k/") // k/<keyimage> → 0x01 (spent)
        prefixMeta     = []byte("m/") // m/<key> → value (metadata)
        prefixTxIdx    = []byte("t/") // t/<txhash32> → height[8] + txIdx[4] (tx location index)
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

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// ─── Raw helpers ─────────────────────────────────────────────────────────────

func (d *DB) put(key, val []byte) error {
        return d.db.Put(key, val, nil)
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
func (d *DB) PutBlock(hash crypto.Hash32, sb *StoredBlock) error {
        data, err := json.Marshal(sb)
        if err != nil {
                return fmt.Errorf("marshal block: %w", err)
        }
        key := append(prefixBlock, hash[:]...)
        if err := d.put(key, data); err != nil {
                return err
        }
        // Height index
        hKey := heightKey(sb.Height)
        return d.put(hKey, hash[:])
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

// ─── Key Image tracking ───────────────────────────────────────────────────────

// MarkKeyImageSpent records that a key image has been used.
// The key image is normalised to its canonical prime-order representative
// before storage so that any torsion variant maps to the same DB entry.
func (d *DB) MarkKeyImageSpent(ki crypto.KeyImage) error {
        canonical, err := crypto.CanonicalKeyImage(ki)
        if err != nil {
                return fmt.Errorf("key image canonicalization: %w", err)
        }
        key := append(prefixKeyImage, canonical[:]...)
        return d.put(key, []byte{0x01})
}

// IsKeyImageSpent returns true if the key image has been recorded as spent.
// Normalises to the canonical representative before lookup so that any
// torsion variant of a spent key image is correctly detected as spent.
func (d *DB) IsKeyImageSpent(ki crypto.KeyImage) (bool, error) {
        canonical, err := crypto.CanonicalKeyImage(ki)
        if err != nil {
                return false, fmt.Errorf("key image canonicalization: %w", err)
        }
        key := append(prefixKeyImage, canonical[:]...)
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
func (d *DB) PutTip(hash crypto.Hash32, height uint64) error {
        if err := d.PutMeta("tip/hash", hash[:]); err != nil {
                return err
        }
        var hb [8]byte
        binary.LittleEndian.PutUint64(hb[:], height)
        return d.PutMeta("tip/height", hb[:])
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
func (d *DB) PutRawBlock(hash crypto.Hash32, height uint64, data []byte) error {
        key := append(prefixBlock, hash[:]...)
        if err := d.put(key, data); err != nil {
                return err
        }
        return d.put(heightKey(height), hash[:])
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
func (d *DB) RepairHeightIndex(height uint64, hash crypto.Hash32) error {
        return d.put(heightKey(height), hash[:])
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
