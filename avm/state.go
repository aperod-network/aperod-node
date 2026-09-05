package avm

import (
	"bytes"
	"fmt"
	"sort"
	"sync"
)

// Store is the deterministic key-value boundary used by the AVM. Apply must
// commit all supplied writes atomically or leave the store unchanged.
type Store interface {
	Get(key []byte) ([]byte, bool, error)
	Apply(writes []Write) error
}

// Write is one ordered state mutation. A nil Value deletes Key.
type Write struct {
	Key   []byte
	Value []byte
}

// MemoryStore is a concurrency-safe Store used by tests and embedded users.
// Production consensus wiring can implement Store on top of LevelDB batches.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string][]byte
}

// OverlayStore buffers writes over another Store. It is the block-level
// transaction boundary: individual contract calls can commit into the overlay,
// while canonical persistence receives one final ordered write set.
type OverlayStore struct {
	mu     sync.RWMutex
	base   Store
	writes map[string][]byte
}

func NewOverlayStore(base Store) *OverlayStore {
	return &OverlayStore{base: base, writes: make(map[string][]byte)}
}

func (s *OverlayStore) Get(key []byte) ([]byte, bool, error) {
	s.mu.RLock()
	value, staged := s.writes[string(key)]
	s.mu.RUnlock()
	if staged {
		if value == nil {
			return nil, false, nil
		}
		return bytes.Clone(value), true, nil
	}
	return s.base.Get(key)
}

func (s *OverlayStore) Apply(writes []Write) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, write := range writes {
		if len(write.Key) == 0 {
			return fmt.Errorf("avm overlay: empty key")
		}
	}
	for _, write := range writes {
		s.writes[string(write.Key)] = bytes.Clone(write.Value)
	}
	return nil
}

func (s *OverlayStore) Writes() []Write {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.writes))
	for key := range s.writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writes := make([]Write, 0, len(keys))
	for _, key := range keys {
		writes = append(writes, Write{Key: []byte(key), Value: bytes.Clone(s.writes[key])})
	}
	return writes
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string][]byte)}
}

func (s *MemoryStore) Get(key []byte) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok := s.data[string(key)]
	if !ok {
		return nil, false, nil
	}
	return bytes.Clone(value), true, nil
}

func (s *MemoryStore) Apply(writes []Write) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, write := range writes {
		if len(write.Key) == 0 {
			return fmt.Errorf("avm state: empty key")
		}
	}
	for _, write := range writes {
		if write.Value == nil {
			delete(s.data, string(write.Key))
			continue
		}
		s.data[string(write.Key)] = bytes.Clone(write.Value)
	}
	return nil
}

// Writes returns a deterministic snapshot of the complete in-memory state.
func (s *MemoryStore) Writes() []Write {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.data))
	for key := range s.data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writes := make([]Write, 0, len(keys))
	for _, key := range keys {
		writes = append(writes, Write{
			Key:   []byte(key),
			Value: bytes.Clone(s.data[key]),
		})
	}
	return writes
}

type transactionState struct {
	base   Store
	prefix []byte
	writes map[string][]byte
}

func newTransactionState(base Store, contractID [32]byte) *transactionState {
	prefix := make([]byte, len(contractID)+1)
	copy(prefix, contractID[:])
	prefix[len(contractID)] = '/'
	return &transactionState{
		base:   base,
		prefix: prefix,
		writes: make(map[string][]byte),
	}
}

func (s *transactionState) namespaced(key []byte) []byte {
	full := make([]byte, 0, len(s.prefix)+len(key))
	full = append(full, s.prefix...)
	full = append(full, key...)
	return full
}

func (s *transactionState) get(key []byte) ([]byte, bool, error) {
	full := s.namespaced(key)
	if value, ok := s.writes[string(full)]; ok {
		if value == nil {
			return nil, false, nil
		}
		return bytes.Clone(value), true, nil
	}
	return s.base.Get(full)
}

func (s *transactionState) put(key, value []byte) {
	s.writes[string(s.namespaced(key))] = bytes.Clone(value)
}

func (s *transactionState) orderedWrites() []Write {
	keys := make([]string, 0, len(s.writes))
	for key := range s.writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	writes := make([]Write, 0, len(keys))
	for _, key := range keys {
		writes = append(writes, Write{
			Key:   []byte(key),
			Value: bytes.Clone(s.writes[key]),
		})
	}
	return writes
}
