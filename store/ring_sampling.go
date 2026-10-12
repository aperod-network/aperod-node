package store

import (
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/syndtr/goleveldb/leveldb"
)

// Bound untrusted request size and pathological entropy rejection. The store
// population can contain millions of outputs; work is O(count+len(exclude)),
// not O(population) or O(key-space gaps). No random-seek fallback is permitted.
const maxRingSamplingRequest = 4096
const maxRingEntropyRejections = 128
const maxStakedRingExclusions = 1_000_000

func (d *DB) SampleRingMembers(count int, exclude map[crypto.Point32]bool) ([]core.DecoyUTXO, error) {
	return d.sampleRingMembers(count, exclude, cryptorand.Reader)
}

// SampleRingMembersExcluding samples canonical ring records while excluding
// both caller-selected public keys and staked outputs identified by outpoint.
func (d *DB) SampleRingMembersExcluding(count int, exclude map[crypto.Point32]bool, excludeUTXOs map[core.UTXOKey]bool) ([]core.DecoyUTXO, error) {
	return d.sampleRingMembersExcluding(count, exclude, excludeUTXOs, cryptorand.Reader)
}

// Entropy is injected per call for deterministic tests, never package-global.
func (d *DB) sampleRingMembers(count int, exclude map[crypto.Point32]bool, entropy io.Reader) ([]core.DecoyUTXO, error) {
	return d.sampleRingMembersExcluding(count, exclude, nil, entropy)
}

func (d *DB) sampleRingMembersExcluding(count int, exclude map[crypto.Point32]bool, excludeUTXOs map[core.UTXOKey]bool, entropy io.Reader) ([]core.DecoyUTXO, error) {
	if count <= 0 {
		return nil, nil
	}
	if count > maxRingSamplingRequest || len(exclude) > maxRingSamplingRequest || len(excludeUTXOs) > maxStakedRingExclusions {
		return nil, fmt.Errorf("ring sampling: request exceeds bounded limits")
	}
	if entropy == nil {
		return nil, fmt.Errorf("ring sampling: cryptographic entropy required")
	}
	snapshot, err := d.db.GetSnapshot()
	if err != nil {
		return nil, err
	}
	defer snapshot.Release()
	return sampleRingSnapshotExcluding(snapshot, count, exclude, excludeUTXOs, entropy)
}

func sampleRingSnapshot(reader ringSamplingReader, count int, exclude map[crypto.Point32]bool, entropy io.Reader) ([]core.DecoyUTXO, error) {
	return sampleRingSnapshotExcluding(reader, count, exclude, nil, entropy)
}

func sampleRingSnapshotExcluding(reader ringSamplingReader, count int, exclude map[crypto.Point32]bool, excludeUTXOs map[core.UTXOKey]bool, entropy io.Reader) ([]core.DecoyUTXO, error) {
	if count <= 0 {
		return nil, nil
	}
	if count > maxRingSamplingRequest || len(exclude) > maxRingSamplingRequest || len(excludeUTXOs) > maxStakedRingExclusions || entropy == nil {
		return nil, fmt.Errorf("ring sampling: invalid bounded request/entropy")
	}
	size, ready, err := ringSamplingCount(reader)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, fmt.Errorf("ring sampling: ordinal index not ready; prepare ring-member index")
	}
	// Cheap boundary sanity checks do not claim to audit every disk record.
	// Each selected slot is independently cross-checked below.
	if size > 0 {
		if _, err := readRingSamplingMember(reader, size-1); err != nil {
			return nil, err
		}
	}
	if _, err := reader.Get(ringSamplingSlotKey(size), nil); !errors.Is(err, leveldb.ErrNotFound) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("ring sampling: slot past population boundary")
	}
	// Sparse virtual Fisher-Yates: swap the drawn ordinal with the last
	// remaining ordinal. It visits each slot once and stores only changed
	// mappings, not the complete population. Filtering a uniform permutation
	// yields a uniform ordered sample of its non-excluded members.
	swaps := make(map[uint64]uint64)
	seen := make(map[crypto.Point32]bool)
	out := make([]core.DecoyUTXO, 0, count)
	at := func(index uint64) uint64 {
		if mapped, ok := swaps[index]; ok {
			return mapped
		}
		return index
	}
	draws := 0
	for remaining := size; remaining > 0 && len(out) < count; remaining-- {
		draws++
		if draws > count+len(exclude)+len(excludeUTXOs) {
			return nil, fmt.Errorf("ring sampling: candidate consistency budget exceeded")
		}
		index, err := uniformRingOrdinal(entropy, remaining)
		if err != nil {
			return nil, err // discard ALL previously selected candidates
		}
		ordinal := at(index)
		swaps[index] = at(remaining - 1)
		delete(swaps, remaining-1)
		u, err := readRingSamplingMember(reader, ordinal)
		if err != nil {
			return nil, err
		}
		if seen[u.OneTimePub] {
			return nil, fmt.Errorf("ring sampling: duplicate canonical key")
		}
		seen[u.OneTimePub] = true
		if exclude[u.OneTimePub] || excludeUTXOs[core.UTXOKey{TxHash: u.TxHash, OutputIndex: u.OutputIndex}] {
			continue
		}
		out = append(out, core.DecoyUTXO{OneTimePub: u.OneTimePub, AmountCommit: u.AmountCommit})
	}
	return out, nil // genuine exhausted eligible population is an ordinary shortfall
}

func uniformRingOrdinal(entropy io.Reader, size uint64) (uint64, error) {
	if size == 0 || entropy == nil {
		return 0, fmt.Errorf("ring sampling: invalid entropy/population")
	}
	if size == 1 {
		return 0, nil
	}
	width := bits.Len64(size - 1)
	byteCount := (width + 7) / 8
	mask := byte(0xff >> (byteCount*8 - width))
	var buf [8]byte
	for attempt := 0; attempt < maxRingEntropyRejections; attempt++ {
		if _, err := io.ReadFull(entropy, buf[:byteCount]); err != nil {
			return 0, fmt.Errorf("ring sampling: entropy: %w", err)
		}
		buf[0] &= mask
		var value uint64
		for _, b := range buf[:byteCount] {
			value = value<<8 | uint64(b)
		}
		if value < size {
			return value, nil
		}
	}
	return 0, fmt.Errorf("ring sampling: entropy rejection limit exceeded")
}
