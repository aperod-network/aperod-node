package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// RingSamplingInventory is an offline local integrity receipt, not a consensus
// or historical-chain proof. The digest binds every canonical rm/ key/payload.
type RingSamplingInventory struct {
	Canonical       uint64 `json:"canonical"`
	Eligible        uint64 `json:"eligible"`
	Locked          uint64 `json:"locked"`
	CanonicalSHA256 string `json:"canonical_sha256"`
}

// Legacy preparation can resume from a partial u/→rm/ copy, but must not
// overwrite an independently repaired canonical rm/ record with stale u/.
// This is a maintenance precondition, not a change to sampling or startup's
// established migration contract. Ambiguous legacy layouts stay fenced.
func (d *DB) verifyLegacyRingBackfillPreservesCanonical() error {
	marker, err := d.GetMeta("ring-member-index-v1")
	if err != nil || marker != nil {
		return err
	}
	iter := d.db.NewIterator(util.BytesPrefix(prefixRingMember), nil)
	defer iter.Release()
	for iter.Next() {
		u, err := decodeRingRecord(iter.Key(), iter.Value())
		if err != nil {
			return err
		}
		old, err := d.db.Get(utxoKey(u.TxHash, u.OutputIndex), nil)
		if err != nil {
			return fmt.Errorf("ring maintenance: unprepared canonical record has no matching legacy output: %w", err)
		}
		if !bytes.Equal(old, iter.Value()) {
			return fmt.Errorf("ring maintenance: unprepared legacy backfill would overwrite canonical rm; explicit reconciliation required")
		}
	}
	return iter.Error()
}

// VerifyRingSamplingIndex performs a streaming complete bijection audit. A
// ready marker alone cannot prove that a legacy writer did not change rm/.
// Caller must quiesce clients and all writers before using this to release a
// maintenance fence. The snapshot makes the audit coherent, not a writer fence.
func (d *DB) VerifyRingSamplingIndex() (RingSamplingInventory, error) {
	var out RingSamplingInventory
	d.ringMu.Lock()
	defer d.ringMu.Unlock()
	snap, err := d.db.GetSnapshot()
	if err != nil {
		return out, err
	}
	defer snap.Release()
	count, ready, err := ringSamplingCount(snap)
	if err != nil {
		return out, err
	}
	if !ready {
		return out, fmt.Errorf("ring audit: readiness absent")
	}
	hash := sha256.New()
	iter := snap.NewIterator(util.BytesPrefix(prefixRingMember), nil)
	for iter.Next() {
		key, value := iter.Key(), iter.Value()
		u, err := decodeRingRecord(key, value)
		if err != nil {
			iter.Release()
			return out, err
		}
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		hash.Write(key)
		hash.Write(size[:])
		hash.Write(value)
		out.Canonical++
		reverse, lookupErr := snap.Get(ringSamplingReverseKey(u.OneTimePub), nil)
		if u.ProtocolLocked {
			if !errors.Is(lookupErr, leveldb.ErrNotFound) {
				iter.Release()
				if lookupErr != nil {
					return out, lookupErr
				}
				return out, fmt.Errorf("ring audit: locked member indexed")
			}
			out.Locked++
			continue
		}
		if lookupErr != nil {
			iter.Release()
			return out, lookupErr
		}
		if len(reverse) != 8 {
			iter.Release()
			return out, fmt.Errorf("ring audit: malformed inverse")
		}
		index := binary.BigEndian.Uint64(reverse)
		member, err := readRingSamplingMember(snap, index)
		if err != nil {
			iter.Release()
			return out, err
		}
		if index >= count || member.OneTimePub != u.OneTimePub {
			iter.Release()
			return out, fmt.Errorf("ring audit: canonical/inverse/slot mismatch")
		}
		out.Eligible++
	}
	err = iter.Error()
	iter.Release()
	if err != nil {
		return out, err
	}
	if out.Eligible != count {
		return out, fmt.Errorf("ring audit: canonical/count mismatch")
	}
	slots := uint64(0)
	iter = snap.NewIterator(util.BytesPrefix(ringSamplingSlotPrefix), nil)
	for iter.Next() {
		key := iter.Key()
		if len(key) != len(ringSamplingSlotPrefix)+8 ||
			binary.BigEndian.Uint64(key[len(ringSamplingSlotPrefix):]) != slots ||
			slots >= count {
			iter.Release()
			return out, fmt.Errorf("ring audit: non-dense or excess slot")
		}
		if _, err := readRingSamplingMember(snap, slots); err != nil {
			iter.Release()
			return out, err
		}
		slots++
	}
	err = iter.Error()
	iter.Release()
	if err != nil {
		return out, err
	}
	if slots != count {
		return out, fmt.Errorf("ring audit: missing slots")
	}
	reverses := uint64(0)
	iter = snap.NewIterator(util.BytesPrefix(ringSamplingReversePrefix), nil)
	for iter.Next() {
		key, value := iter.Key(), iter.Value()
		if len(key) != len(ringSamplingReversePrefix)+32 || len(value) != 8 {
			iter.Release()
			return out, fmt.Errorf("ring audit: malformed inverse entry")
		}
		var pub crypto.Point32
		copy(pub[:], key[len(ringSamplingReversePrefix):])
		index := binary.BigEndian.Uint64(value)
		member, err := readRingSamplingMember(snap, index)
		if err != nil {
			iter.Release()
			return out, err
		}
		if index >= count || !bytes.Equal(member.OneTimePub[:], pub[:]) {
			iter.Release()
			return out, fmt.Errorf("ring audit: orphan inverse")
		}
		reverses++
	}
	err = iter.Error()
	iter.Release()
	if err != nil {
		return out, err
	}
	if reverses != count {
		return out, fmt.Errorf("ring audit: inverse/count mismatch")
	}
	out.CanonicalSHA256 = hex.EncodeToString(hash.Sum(nil))
	return out, nil
}
