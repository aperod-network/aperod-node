package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

var saleRetentionIndexKey = []byte("sale/retention-index/v1")
var saleRetainedBlockPrefix = []byte("sale/retained-block/v1/")

const (
	saleRetainedActivation byte = 1 << iota
	saleRetainedDelivery
	saleRetainedKnownFlags = saleRetainedActivation | saleRetainedDelivery
)

func saleRetainedBlockKey(height uint64) []byte {
	key := append([]byte(nil), saleRetainedBlockPrefix...)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], height)
	return append(key, encoded[:]...)
}

func saleRetentionHeightValue(height uint64) []byte {
	value := make([]byte, 8)
	binary.LittleEndian.PutUint64(value, height)
	return value
}

func validateSaleRetentionHeader(raw []byte, height uint64, hash crypto.Hash32) error {
	var block core.Block
	if len(raw) == 0 || json.Unmarshal(raw, &block) != nil ||
		block.Header.Height != height || block.Hash() != hash {
		return fmt.Errorf("sale: retained block header is unavailable or not canonical at height %d", height)
	}
	return nil
}

func validateSaleRetentionBody(raw []byte, height uint64, hash crypto.Hash32) error {
	if err := validateSaleRetentionHeader(raw, height, hash); err != nil {
		return err
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil ||
		block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
		return fmt.Errorf("sale: retained delivery body is incomplete or not bound to its header at height %d", height)
	}
	return nil
}

func saleRetentionMarker(value []byte, height uint64) (byte, crypto.Hash32, bool, error) {
	var hash crypto.Hash32
	if value == nil {
		return 0, hash, false, nil
	}
	if len(value) != 1+len(hash) || value[0] == 0 || value[0]&^saleRetainedKnownFlags != 0 {
		return 0, hash, false, fmt.Errorf("sale: corrupt retained block marker at height %d", height)
	}
	copy(hash[:], value[1:])
	return value[0], hash, true, nil
}

func (d *DB) stageSaleRetainedBlock(batch *leveldb.Batch, height uint64, hash crypto.Hash32, kind byte) error {
	if kind == 0 || kind&^saleRetainedKnownFlags != 0 {
		return fmt.Errorf("sale: invalid retained block kind")
	}
	key := saleRetainedBlockKey(height)
	existing, err := d.get(key)
	if err != nil {
		return err
	}
	flags := kind
	if existing != nil {
		oldFlags, oldHash, _, err := saleRetentionMarker(existing, height)
		if err != nil {
			return err
		}
		if oldHash != hash {
			return fmt.Errorf("sale: conflicting retained canonical hash at height %d", height)
		}
		flags |= oldFlags
	}
	value := make([]byte, 1+len(hash))
	value[0] = flags
	copy(value[1:], hash[:])
	batch.Put(key, value)
	return nil
}

// EnsureSaleRetention installs the pruning index for an already activated
// ledger. It only records canonical activation/delivery bodies; this metadata
// is not a finality certificate or proof of LPoD history.
func (d *DB) EnsureSaleRetention(m *SaleMigration) error {
	if m == nil {
		return nil
	}
	cp := m.Checkpoint
	if cp.SnapshotRoot != m.Reconciliation.Digest() {
		return fmt.Errorf("sale: retention configuration has unsigned reconciliation")
	}
	if err := cp.Verify(cp.Genesis, cp.Parent, cp.Height, m.Trusted); err != nil {
		return fmt.Errorf("sale: retention configuration checkpoint: %w", err)
	}

	d.canonicalMu.Lock()
	defer d.canonicalMu.Unlock()

	state, err := d.LoadSaleState()
	if err != nil || state == nil {
		return err
	}
	if state.Checkpoint != cp.Message() {
		return fmt.Errorf("sale: retention configuration does not match active ledger")
	}
	tip, tipHeight, err := d.GetTip()
	if err != nil {
		return err
	}
	if state.Hash != tip || state.Height != tipHeight || tipHeight < cp.Height {
		return fmt.Errorf("sale: retention ledger does not match canonical tip")
	}

	activationHash, found, err := d.GetCanonicalHash(cp.Height)
	if err != nil || !found {
		return fmt.Errorf("sale: canonical activation hash unavailable: %v", err)
	}
	activationRaw, err := d.GetRawBlock(activationHash)
	if err != nil {
		return err
	}
	if err := validateSaleRetentionHeader(activationRaw, cp.Height, activationHash); err != nil {
		return err
	}
	var activation core.Block
	if err := json.Unmarshal(activationRaw, &activation); err != nil ||
		activation.Header.PrevHash != cp.Parent {
		return fmt.Errorf("sale: retained activation does not bind the signed parent")
	}

	index, err := d.get(saleRetentionIndexKey)
	if err != nil {
		return err
	}
	expectedIndex := saleRetentionHeightValue(cp.Height)
	if index != nil && (len(index) != 8 || !bytes.Equal(index, expectedIndex)) {
		return fmt.Errorf("sale: retained activation height conflicts with signed configuration")
	}
	activationMarker, err := d.get(saleRetainedBlockKey(cp.Height))
	if err != nil {
		return err
	}
	flags, markerHash, retained, markerErr := saleRetentionMarker(activationMarker, cp.Height)
	if markerErr != nil {
		return markerErr
	}
	if index != nil && retained && flags&saleRetainedActivation != 0 {
		if markerHash != activationHash {
			return fmt.Errorf("sale: retained activation marker conflicts with canonical hash")
		}
		return nil // the index and activation marker were committed atomically
	}

	batch := new(leveldb.Batch)
	if index != nil {
		// A missing activation marker means the prior backfill was incomplete;
		// clear its completion claim before any bounded batch is persisted.
		batch.Delete(saleRetentionIndexKey)
	}
	if err := d.stageSaleRetainedBlock(batch, cp.Height, activationHash, saleRetainedActivation); err != nil {
		return err
	}

	// Older nodes did not index sale delivery heights. Backfill from durable
	// order fences, validating every referenced body against the canonical index
	// before pruning can rely on it. Batch size is bounded; the completion index
	// is written only after the whole scan succeeds, so a restart can retry.
	const maxBatchEntries = 4096
	entries := 1
	flush := func() error {
		if entries == 0 {
			return nil
		}
		if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
			return err
		}
		batch.Reset()
		entries = 0
		return nil
	}
	iter := d.db.NewIterator(util.BytesPrefix([]byte{'s', 'a', 'l', 'e', '/', 'o', '/'}), nil)
	defer iter.Release()
	prefixLen := len([]byte{'s', 'a', 'l', 'e', '/', 'o', '/'})
	for iter.Next() {
		key, value := iter.Key(), iter.Value()
		if len(key) != prefixLen+len(crypto.Hash32{}) || len(value) != len(crypto.Hash32{}) {
			return fmt.Errorf("sale: corrupt order delivery fence during retention backfill")
		}
		var id, fencedHash crypto.Hash32
		copy(id[:], key[prefixLen:])
		copy(fencedHash[:], value)
		delivery, err := d.saleOrderDeliveryLocked(id)
		if err != nil {
			return fmt.Errorf("sale: cannot backfill canonical delivery retention: %w", err)
		}
		if delivery == nil || delivery.Block != fencedHash {
			return fmt.Errorf("sale: delivery fence disagrees with its canonical transaction")
		}
		raw, err := d.GetRawBlock(delivery.Block)
		if err != nil {
			return err
		}
		if err := validateSaleRetentionBody(raw, delivery.Height, delivery.Block); err != nil {
			return err
		}
		kind := saleRetainedDelivery
		if delivery.Height == cp.Height {
			kind |= saleRetainedActivation
		}
		if err := d.stageSaleRetainedBlock(batch, delivery.Height, delivery.Block, kind); err != nil {
			return err
		}
		entries++
		if entries >= maxBatchEntries {
			if err := flush(); err != nil {
				return fmt.Errorf("sale: persist delivery retention backfill: %w", err)
			}
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("sale: scan delivery fences for retention: %w", err)
	}
	batch.Put(saleRetentionIndexKey, expectedIndex)
	if err := d.db.Write(batch, &opt.WriteOptions{Sync: true}); err != nil {
		return fmt.Errorf("sale: commit retention index: %w", err)
	}
	return nil
}

func (d *DB) saleRetentionHeight() (uint64, bool, error) {
	value, err := d.get(saleRetentionIndexKey)
	if err != nil || value == nil {
		return 0, false, err
	}
	if len(value) != 8 {
		return 0, false, fmt.Errorf("sale: corrupt retained activation height")
	}
	return binary.LittleEndian.Uint64(value), true, nil
}

func (d *DB) verifyRetainedSaleBlock(height uint64, hash crypto.Hash32, requireDeliveryBody bool) error {
	canonical, found, err := d.GetCanonicalHash(height)
	if err != nil || !found || canonical != hash {
		return fmt.Errorf("sale: retained block is no longer canonical at height %d", height)
	}
	raw, err := d.GetRawBlock(hash)
	if err != nil {
		return err
	}
	if requireDeliveryBody {
		return validateSaleRetentionBody(raw, height, hash)
	}
	return validateSaleRetentionHeader(raw, height, hash)
}

func (d *DB) validateSaleRetentionIndex() error {
	state, err := d.LoadSaleState()
	if err != nil || state == nil {
		return err
	}
	height, found, err := d.saleRetentionHeight()
	if err != nil || !found {
		return fmt.Errorf("sale: retention index missing for active ledger: %v", err)
	}
	marker, err := d.get(saleRetainedBlockKey(height))
	if err != nil {
		return err
	}
	flags, hash, retained, markerErr := saleRetentionMarker(marker, height)
	if markerErr != nil {
		return fmt.Errorf("sale: activation retention marker corrupt: %w", markerErr)
	}
	if !retained || flags&saleRetainedActivation == 0 {
		return fmt.Errorf("sale: activation retention marker missing")
	}
	if err := d.verifyRetainedSaleBlock(height, hash, flags&saleRetainedDelivery != 0); err != nil {
		return err
	}
	return nil
}
