package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/aperod/aperod/crypto"
)

// These are derived local indexes, not consensus/wire-format changes. rm/
// remains authoritative. Slots cover unique, unlocked rm/ keys, including spent
// outputs. A count is also the readiness marker, published only after backfill.
var (
	ringSamplingCountKey      = []byte("m/ring-sampling-v1/count")
	ringSamplingSlotPrefix    = []byte("rs1/")
	ringSamplingReversePrefix = []byte("ri1/")
)

func ringSamplingSlotKey(index uint64) []byte {
	key := make([]byte, len(ringSamplingSlotPrefix)+8)
	copy(key, ringSamplingSlotPrefix)
	binary.BigEndian.PutUint64(key[len(ringSamplingSlotPrefix):], index)
	return key
}

func ringSamplingReverseKey(pub crypto.Point32) []byte {
	return append(append([]byte(nil), ringSamplingReversePrefix...), pub[:]...)
}

func ringSamplingUint(value uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], value)
	return b[:]
}

type ringSamplingReader interface {
	Get([]byte, *opt.ReadOptions) ([]byte, error)
}

func ringSamplingCount(reader ringSamplingReader) (uint64, bool, error) {
	data, err := reader.Get(ringSamplingCountKey, nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if len(data) != 8 {
		return 0, false, fmt.Errorf("ring sampling: malformed count")
	}
	return binary.BigEndian.Uint64(data), true, nil
}

func decodeRingRecord(key, data []byte) (*StoredUTXO, error) {
	var u StoredUTXO
	if len(key) != len(prefixRingMember)+len(u.OneTimePub) ||
		!bytes.HasPrefix(key, prefixRingMember) {
		return nil, fmt.Errorf("ring sampling: malformed canonical key")
	}
	if err := json.Unmarshal(data, &u); err != nil {
		return nil, fmt.Errorf("ring sampling: decode canonical member: %w", err)
	}
	if !bytes.Equal(key[len(prefixRingMember):], u.OneTimePub[:]) {
		return nil, fmt.Errorf("ring sampling: canonical key/payload mismatch")
	}
	return &u, nil
}

// EnsureRingMemberIndex is a startup/maintenance operation, not part of a
// sampling request. A legacy database takes one streaming O(N) preparation.
// Memory is bounded by 1000 index entries (legacy u/ backfill uses 10000).
// Writers are excluded; an interrupted build has no readiness marker and is
// safely restarted. Never roll this out against a live production DB implicitly.
func (d *DB) EnsureRingMemberIndex() error {
	d.ringMu.Lock()
	defer d.ringMu.Unlock()
	if err := d.ensureLegacyRingMemberIndex(); err != nil {
		return err
	}
	if _, ready, err := ringSamplingCount(d.db); err != nil || ready {
		return err
	}
	// Remove incomplete derived state, including stale tail slots from a
	// previous interrupted build. Do not remove any canonical/spent members.
	for _, prefix := range [][]byte{ringSamplingSlotPrefix, ringSamplingReversePrefix} {
		iter := d.db.NewIterator(util.BytesPrefix(prefix), nil)
		batch := new(leveldb.Batch)
		for iter.Next() {
			batch.Delete(append([]byte(nil), iter.Key()...))
			if batch.Len() >= 1000 {
				if err := d.db.Write(batch, nil); err != nil {
					iter.Release()
					return err
				}
				batch.Reset()
			}
		}
		err := iter.Error()
		iter.Release()
		if err != nil {
			return err
		}
		if err := d.db.Write(batch, nil); err != nil {
			return err
		}
	}
	iter := d.db.NewIterator(util.BytesPrefix(prefixRingMember), nil)
	defer iter.Release()
	batch := new(leveldb.Batch)
	var count uint64
	for iter.Next() {
		u, err := decodeRingRecord(iter.Key(), iter.Value())
		if err != nil {
			return err
		}
		if u.ProtocolLocked {
			continue
		}
		if count == math.MaxUint64 {
			return fmt.Errorf("ring sampling: population overflow")
		}
		batch.Put(ringSamplingSlotKey(count), u.OneTimePub[:])
		batch.Put(ringSamplingReverseKey(u.OneTimePub), ringSamplingUint(count))
		count++
		if batch.Len() >= 2000 {
			if err := d.db.Write(batch, nil); err != nil {
				return err
			}
			batch.Reset()
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}
	batch.Put(ringSamplingCountKey, ringSamplingUint(count))
	return d.db.Write(batch, &opt.WriteOptions{Sync: true})
}

// InvalidateRingSamplingIndex fences sampling BEFORE an offline downgrade to a
// legacy writer, an old backup import or a canonical rm/ repair. Legacy writers
// do not maintain ordinal slots. After they stop, EnsureRingMemberIndex rebuilds
// from rm/. Caller must quiesce clients/writers and retain a coherent backup;
// this is not a production migration performed implicitly by a request.
func (d *DB) InvalidateRingSamplingIndex() error {
	d.ringMu.Lock()
	defer d.ringMu.Unlock()
	return d.db.Delete(ringSamplingCountKey, &opt.WriteOptions{Sync: true})
}

// stageRingSamplingChange runs under ringMu and stages every derived change
// into the SAME LevelDB batch as u/ and rm/. No memory population cache exists.
func (d *DB) stageRingSamplingChange(batch *leveldb.Batch, pub crypto.Point32, eligible bool) error {
	count, ready, err := ringSamplingCount(d.db)
	if err != nil {
		return err
	}
	if !ready {
		// A brand-new rm/ store starts at ordinal zero. Existing legacy rm/
		// stores keep accepting canonical writes but remain unavailable for
		// sampling until the explicit streaming preparation finishes.
		iter := d.db.NewIterator(util.BytesPrefix(prefixRingMember), nil)
		nonempty := iter.First()
		err := iter.Error()
		iter.Release()
		if err != nil {
			return err
		}
		if nonempty {
			return nil
		}
	}
	oldData, err := d.get(ringMemberKey(pub))
	if err != nil {
		return err
	}
	oldEligible := false
	if oldData != nil {
		old, err := decodeRingRecord(ringMemberKey(pub), oldData)
		if err != nil {
			return err
		}
		oldEligible = !old.ProtocolLocked
	}
	reverse, err := d.get(ringSamplingReverseKey(pub))
	if err != nil {
		return err
	}
	if oldEligible != (reverse != nil) {
		return fmt.Errorf("ring sampling: canonical/reverse membership mismatch")
	}
	var index uint64
	if oldEligible {
		if len(reverse) != 8 || count == 0 {
			return fmt.Errorf("ring sampling: malformed reverse index")
		}
		index = binary.BigEndian.Uint64(reverse)
		slot, err := d.get(ringSamplingSlotKey(index))
		if err != nil {
			return err
		}
		if index >= count || !bytes.Equal(slot, pub[:]) {
			return fmt.Errorf("ring sampling: reverse/slot mismatch")
		}
	}
	switch {
	case oldEligible && !eligible:
		last := count - 1
		if index != last {
			lastPub, err := d.get(ringSamplingSlotKey(last))
			if err != nil {
				return err
			}
			if len(lastPub) != len(pub) {
				return fmt.Errorf("ring sampling: missing tail slot")
			}
			var moved crypto.Point32
			copy(moved[:], lastPub)
			if moved == pub {
				return fmt.Errorf("ring sampling: duplicate tail member")
			}
			if _, err := readRingSamplingMember(d.db, last); err != nil {
				return err
			}
			batch.Put(ringSamplingSlotKey(index), lastPub)
			batch.Put(ringSamplingReverseKey(moved), ringSamplingUint(index))
		}
		batch.Delete(ringSamplingSlotKey(last))
		batch.Delete(ringSamplingReverseKey(pub))
		count--
	case !oldEligible && eligible:
		if count == math.MaxUint64 {
			return fmt.Errorf("ring sampling: population overflow")
		}
		// An unexpected existing tail is corruption, not permission to
		// overwrite it or return a biased partial sample.
		tail, err := d.get(ringSamplingSlotKey(count))
		if err != nil {
			return err
		}
		if tail != nil {
			return fmt.Errorf("ring sampling: occupied append slot")
		}
		batch.Put(ringSamplingSlotKey(count), pub[:])
		batch.Put(ringSamplingReverseKey(pub), ringSamplingUint(count))
		count++
	}
	batch.Put(ringSamplingCountKey, ringSamplingUint(count))
	return nil
}

// Resolve a slot against both the inverse mapping and authoritative rm/
// payload from one reader/snapshot. Missing/corrupt records are fatal.
func readRingSamplingMember(reader ringSamplingReader, index uint64) (*StoredUTXO, error) {
	data, err := reader.Get(ringSamplingSlotKey(index), nil)
	if err != nil {
		return nil, fmt.Errorf("ring sampling: read slot: %w", err)
	}
	var pub crypto.Point32
	if len(data) != len(pub) {
		return nil, fmt.Errorf("ring sampling: malformed slot")
	}
	copy(pub[:], data)
	reverse, err := reader.Get(ringSamplingReverseKey(pub), nil)
	if err != nil {
		return nil, fmt.Errorf("ring sampling: read reverse: %w", err)
	}
	if len(reverse) != 8 || binary.BigEndian.Uint64(reverse) != index {
		return nil, fmt.Errorf("ring sampling: inconsistent reverse")
	}
	data, err = reader.Get(ringMemberKey(pub), nil)
	if err != nil {
		return nil, fmt.Errorf("ring sampling: read canonical member: %w", err)
	}
	u, err := decodeRingRecord(ringMemberKey(pub), data)
	if err != nil {
		return nil, err
	}
	if u.ProtocolLocked {
		return nil, fmt.Errorf("ring sampling: locked member in eligible index")
	}
	return u, nil
}
