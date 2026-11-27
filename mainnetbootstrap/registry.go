package mainnetbootstrap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/aperod/aperod/core"
	chaincrypto "github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const registryDomain = "aperod/mainnet-checkpoint-registry/v1\x00"

// InspectRegistryEvidence reads an auxiliary operator record from a closed
// database. Its digest can be signed independently of the database layout.
// A successful inspection is not proof of historical replay or validator eligibility.
func InspectRegistryEvidence(dbPath string, statement Statement) (string, error) {
	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	snapshot, tip, err := readCheckpointRegistry(db, statement)
	if err != nil {
		return "", err
	}
	return registrySnapshotDigest(statement, tip, snapshot)
}

// InspectCheckpointRegistrySnapshot returns the exact registry recorded for
// the canonical checkpoint. It is used only to construct a height-scoped
// consensus transition; the caller must verify the snapshot's checkpoint
// height and hash before signing.
func InspectCheckpointRegistrySnapshot(dbPath string, statement Statement) (core.RegistrySnapshot, error) {
	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return core.RegistrySnapshot{}, err
	}
	defer db.Close()
	snapshot, _, err := readCheckpointRegistry(db, statement)
	return snapshot, err
}

// VerifyCheckpointRegistry authenticates the exact-tip registry on an already
// open database. It does not authenticate the UTXO set or enable signing.
func VerifyCheckpointRegistry(db *store.DB, signed SignedStatement) (core.RegistrySnapshot, error) {
	if err := signed.Verify(); err != nil {
		return core.RegistrySnapshot{}, err
	}
	if signed.Statement.RegistrySHA256 == "" {
		return core.RegistrySnapshot{}, errors.New("Mainnet bootstrap does not attest to the registry")
	}
	snapshot, tip, err := readCheckpointRegistry(db, signed.Statement)
	if err != nil {
		return core.RegistrySnapshot{}, err
	}
	if err := VerifyRegistrySnapshot(signed.Statement, tip, snapshot); err != nil {
		return core.RegistrySnapshot{}, err
	}
	return snapshot, nil
}

func readCheckpointRegistry(db *store.DB, statement Statement) (core.RegistrySnapshot, chaincrypto.Hash32, error) {
	tip, height, err := db.GetTip()
	if err != nil {
		return core.RegistrySnapshot{}, tip, err
	}
	indexedTip, found, err := db.GetCanonicalHash(height)
	if err != nil {
		return core.RegistrySnapshot{}, tip, err
	}
	if height != statement.CheckpointHeight || hex.EncodeToString(tip[:]) != statement.CheckpointIndexHash || !found || indexedTip != tip {
		return core.RegistrySnapshot{}, tip, errors.New("registry evidence requires the exact canonical checkpoint tip")
	}
	record, err := db.LoadLPoDAuditAtBounded(tip, 4<<20)
	if err != nil {
		return core.RegistrySnapshot{}, tip, err
	}
	snapshot, err := checkpointRegistrySnapshot(statement, tip, record)
	return snapshot, tip, err
}

func checkpointRegistryDigest(statement Statement, tip chaincrypto.Hash32, record *store.LPoDBlockAudit) (string, error) {
	snap, err := checkpointRegistrySnapshot(statement, tip, record)
	if err != nil {
		return "", err
	}
	return registrySnapshotDigest(statement, tip, snap)
}

func checkpointRegistrySnapshot(statement Statement, tip chaincrypto.Hash32, record *store.LPoDBlockAudit) (core.RegistrySnapshot, error) {
	// LPoD migration state binds the canonical genesis header hash, not
	// the separate legacy key used to index genesis in LevelDB.
	if record == nil || !record.Available || record.Height != statement.CheckpointHeight || record.BlockHash != tip ||
		hex.EncodeToString(record.FundingGenesis[:]) != statement.GenesisHeaderHash || record.RegistryAfter == nil {
		return core.RegistrySnapshot{}, errors.New("missing or misbound registry at the canonical checkpoint")
	}
	return *record.RegistryAfter, nil
}

// VerifyRegistrySnapshot compares independently restored state with an
// already authenticated statement, without replacing that restored state.
func VerifyRegistrySnapshot(statement Statement, tip chaincrypto.Hash32, snap core.RegistrySnapshot) error {
	if statement.CheckpointIndexHash != hex.EncodeToString(tip[:]) || statement.RegistrySHA256 == "" {
		return errors.New("registry verification requires a signed digest for this checkpoint")
	}
	digest, err := registrySnapshotDigest(statement, tip, snap)
	if err != nil {
		return err
	}
	if digest != statement.RegistrySHA256 {
		return errors.New("restored Mainnet registry differs from the signed checkpoint registry")
	}
	return nil
}

func registrySnapshotDigest(statement Statement, tip chaincrypto.Hash32, snap core.RegistrySnapshot) (string, error) {
	for key, entry := range snap.Validators {
		if entry == nil || len(entry.PubKey) != 32 || key != hex.EncodeToString(entry.PubKey) ||
			bytes.Equal(entry.PubKey, make([]byte, 32)) || entry.Seeded {
			return "", errors.New("checkpoint registry contains an invalid or synthetic entry")
		}
	}
	registry := core.NewValidatorRegistry()
	registry.RestoreFromSnapshot(snap)
	if len(registry.GetActiveValidators()) == 0 {
		return "", errors.New("checkpoint registry has no active validator")
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	digest.Write([]byte(registryDomain))
	digest.Write([]byte(statement.GenesisIndexHash))
	digest.Write(tip[:])
	digest.Write(raw)
	return hex.EncodeToString(digest.Sum(nil)), nil
}
