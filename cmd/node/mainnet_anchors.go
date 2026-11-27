package main

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/aperod/aperod/internal/checkedio"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func verifyStartupMainnetAnchors(db *store.DB, manifestPath string) (_ mainnetbootstrap.SignedStatement, readCloseErr error) {
	// #nosec G304 -- Node operator's startup trust-manifest path, not an HTTP parameter.
	f, err := os.Open(manifestPath)
	if err != nil {
		return mainnetbootstrap.SignedStatement{}, fmt.Errorf("open bootstrap manifest: %w", err)
	}
	defer checkedio.CloseReadResult(f, &readCloseErr)
	signed, err := mainnetbootstrap.ReadStrict(f)
	if err != nil {
		return mainnetbootstrap.SignedStatement{}, fmt.Errorf("decode bootstrap manifest: %w", err)
	}
	if err := signed.Verify(); err != nil {
		return mainnetbootstrap.SignedStatement{}, fmt.Errorf("authenticate bootstrap manifest: %w", err)
	}
	return signed, checkMainnetAnchorIndexes(db, signed.Statement)
}

// Compare canonical index hashes, not the recomputed legacy header hashes.
// A running node can advance beyond its bootstrap checkpoint and prune old
// transaction bodies, so do not require the original file-tree digest here.
func checkMainnetAnchorIndexes(db *store.DB, statement mainnetbootstrap.Statement) error {
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return fmt.Errorf("read tip: %w", err)
	}
	if tipHeight < statement.CheckpointHeight {
		return fmt.Errorf("tip %d is below the signed checkpoint %d", tipHeight, statement.CheckpointHeight)
	}
	for _, anchor := range []struct {
		name   string
		height uint64
		hash   string
	}{
		{"genesis", 0, statement.GenesisIndexHash},
		{"checkpoint", statement.CheckpointHeight, statement.CheckpointIndexHash},
	} {
		indexHash, found, err := db.GetCanonicalHash(anchor.height)
		if err != nil {
			return fmt.Errorf("read signed %s at height %d: %w", anchor.name, anchor.height, err)
		}
		if !found || hex.EncodeToString(indexHash[:]) != anchor.hash {
			return fmt.Errorf("canonical height index does not match signed %s at height %d", anchor.name, anchor.height)
		}
		if anchor.height == tipHeight && indexHash != tipHash {
			return fmt.Errorf("tip pointer does not match signed %s at height %d", anchor.name, anchor.height)
		}
	}
	return nil
}
