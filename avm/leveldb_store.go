package avm

import (
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/store"
)

// LevelStore adapts the node's LevelDB to the AVM Store interface.
type LevelStore struct {
	DB *store.DB
}

func (s LevelStore) Get(key []byte) ([]byte, bool, error) {
	if s.DB == nil {
		return nil, false, fmt.Errorf("avm: nil LevelDB store")
	}
	return s.DB.GetAVMState(key)
}

func (s LevelStore) Apply(writes []Write) error {
	if s.DB == nil {
		return fmt.Errorf("avm: nil LevelDB store")
	}
	return s.DB.ApplyAVMState(toStoreWrites(writes))
}

// CommitCanonicalBlock persists a prepared write set in the same fsynced
// LevelDB batch as the canonical block and tip.
func CommitCanonicalBlock(db *store.DB, block *core.Block, rawBlock []byte, prepared *PreparedBlock) error {
	if db == nil || block == nil || prepared == nil {
		return fmt.Errorf("avm: canonical commit requires db, block and prepared effects")
	}
	hash := block.Hash()
	if prepared.Height != block.Header.Height || prepared.BlockHash != hash {
		return fmt.Errorf("avm: prepared effects do not match block")
	}
	return db.CommitRawBlockWithAVM(
		hash,
		block.Header.Height,
		rawBlock,
		toStoreWrites(prepared.Writes),
		prepared.WriteSetCommitment,
	)
}

func toStoreWrites(writes []Write) []store.AVMWrite {
	converted := make([]store.AVMWrite, len(writes))
	for i, write := range writes {
		converted[i] = store.AVMWrite{Key: write.Key, Value: write.Value}
	}
	return converted
}
