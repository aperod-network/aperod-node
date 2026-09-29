package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const maxRegistryReplayBlocks = uint64(50_000)

type anchoredRegistry struct {
	Height   uint64                `json:"height"`
	Hash     crypto.Hash32         `json:"hash"`
	Registry core.RegistrySnapshot `json:"registry"`
}

func readAnchoredRegistry(path string) (*anchoredRegistry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open validator-set snapshot: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > 4<<20 {
		return nil, fmt.Errorf("validator-set snapshot must be at most 4 MiB")
	}
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var snapshot anchoredRegistry
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode validator-set snapshot: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("validator-set snapshot has trailing data")
	}
	return &snapshot, nil
}

func checkOperatorRegistrySnapshot(registry core.RegistrySnapshot, expected crypto.ValidatorPubKey) error {
	if len(registry.Validators) != 1 {
		return fmt.Errorf("operator-supplied registry does not list exactly one authority; its block anchor does not authenticate this list")
	}
	entry, ok := registry.Validators[expected.Hex()]
	if !ok || entry == nil || !bytes.Equal(entry.PubKey, expected) || entry.Status != core.ValidatorActive ||
		entry.StakeNAPR < core.MinStakeNAPR || entry.UnbondEndBlock != 0 || len(entry.UnbondingQueue) != 0 {
		return fmt.Errorf("operator-supplied registry does not list the expected active validator as its only entry")
	}
	return nil
}

func verifyCanonicalParent(db *store.DB, height uint64, expectedHash crypto.Hash32, expectedValidator crypto.ValidatorPubKey) error {
	indexed, found, err := db.GetCanonicalHash(height)
	if err != nil || !found || indexed != expectedHash {
		return fmt.Errorf("expected parent hash is not in the canonical height index: %v", err)
	}
	raw, err := db.GetRawBlock(indexed)
	if err != nil || raw == nil {
		return fmt.Errorf("expected canonical parent block body is unavailable: %v", err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return fmt.Errorf("decode canonical parent block: %w", err)
	}
	if block.Header.Height != height || block.Hash() != expectedHash ||
		!bytes.Equal(block.Header.ValidatorPub, expectedValidator) ||
		block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
		return fmt.Errorf("canonical parent body, modern block hash, or transaction Merkle root is invalid")
	}
	if !block.Header.VerifySignature() {
		return fmt.Errorf("canonical parent block signature is invalid")
	}
	return nil
}

func verifyExpectedGenesis(db *store.DB, expected crypto.Hash32) error {
	// Match the node's trusted-genesis verifier: prefer a body whose modern
	// header hash is the canonical index; only otherwise try the strict,
	// signature-checked legacy genesis identity.
	indexed, found, err := db.GetCanonicalHash(0)
	if err != nil || !found {
		return fmt.Errorf("canonical genesis index is unavailable: %v", err)
	}
	raw, err := db.GetRawBlock(indexed)
	if err != nil || raw == nil {
		return fmt.Errorf("canonical genesis body is unavailable: %v", err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return fmt.Errorf("decode canonical genesis body: %w", err)
	}
	header := &block.Header
	if header.Height != 0 || core.MerkleRoot(block.Txs) != header.MerkleRoot {
		return fmt.Errorf("canonical genesis body has an invalid height or Merkle root")
	}
	if block.Hash() == indexed {
		if block.Hash() != expected || !header.VerifySignature() {
			return fmt.Errorf("modern-indexed canonical genesis does not match the expected hash or signature")
		}
		return nil
	}
	if header.PrevHash != (crypto.Hash32{}) || header.OraclePrice != 0 || header.BaseFee != 0 ||
		block.Hash() != expected {
		return fmt.Errorf("legacy-indexed canonical genesis does not match the expected hash")
	}
	if legacyGenesisID(header) != indexed || !header.ValidatorPub.Verify(indexed, header.Signature) {
		return fmt.Errorf("canonical genesis body has an invalid legacy index or signature")
	}
	return nil
}

func verifyRegistryAnchorAndReplay(db *store.DB, snapshot *anchoredRegistry, parentHeight uint64, parentHash crypto.Hash32, expected crypto.ValidatorPubKey) error {
	indexedAnchor, found, err := db.GetCanonicalHash(snapshot.Height)
	if err != nil || !found || indexedAnchor != snapshot.Hash {
		return fmt.Errorf("validator-set snapshot anchor is not canonical: %v", err)
	}
	raw, err := db.GetRawBlock(snapshot.Hash)
	if err != nil || raw == nil {
		return fmt.Errorf("validator-set snapshot anchor body is unavailable: %v", err)
	}
	var anchor core.Block
	if err := json.Unmarshal(raw, &anchor); err != nil {
		return fmt.Errorf("decode validator-set snapshot anchor: %w", err)
	}
	if anchor.Header.Height != snapshot.Height || anchor.Hash() != snapshot.Hash ||
		!bytes.Equal(anchor.Header.ValidatorPub, expected) ||
		anchor.Header.MerkleRoot != core.MerkleRoot(anchor.Txs) || !anchor.Header.VerifySignature() {
		return fmt.Errorf("validator-set snapshot anchor body is not a modern, signed canonical block")
	}

	previous := snapshot.Hash
	for height := snapshot.Height + 1; height <= parentHeight; height++ {
		indexed, found, err := db.GetCanonicalHash(height)
		if err != nil || !found {
			return fmt.Errorf("canonical index is missing at height %d: %v", height, err)
		}
		raw, err := db.GetRawBlock(indexed)
		if err != nil || raw == nil {
			return fmt.Errorf("canonical block body is missing at height %d: %v", height, err)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			return fmt.Errorf("decode canonical block at height %d: %w", height, err)
		}
		if block.Header.Height != height || block.Hash() != indexed || block.Header.PrevHash != previous ||
			!bytes.Equal(block.Header.ValidatorPub, expected) ||
			block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
			return fmt.Errorf("canonical ancestry, modern hash, or Merkle root is invalid at height %d", height)
		}
		if !block.Header.VerifySignature() {
			return fmt.Errorf("canonical block signature is invalid at height %d", height)
		}
		for _, tx := range block.Txs {
			if tx.IsStake() {
				return fmt.Errorf("stake transaction at height %d makes the anchored validator set uncertain", height)
			}
		}
		previous = indexed
		if height == parentHeight {
			break
		}
	}
	if previous != parentHash {
		return fmt.Errorf("canonical replay does not reach the expected parent")
	}
	if err := checkOperatorRegistrySnapshot(snapshot.Registry, expected); err != nil {
		return err
	}
	return nil
}

func legacyGenesisID(header *core.BlockHeader) crypto.Hash32 {
	var height, timestamp [8]byte
	var round [4]byte
	binary.LittleEndian.PutUint64(height[:], header.Height)
	binary.LittleEndian.PutUint64(timestamp[:], uint64(header.Timestamp))
	binary.LittleEndian.PutUint32(round[:], header.Round)
	return crypto.HashBytes(height[:], header.PrevHash[:], header.MerkleRoot[:],
		timestamp[:], round[:], header.ValidatorPub)
}

func verifyOneValidatorCertificate(cert *store.FinalityCertificate, height uint64, blockHash crypto.Hash32, expected crypto.ValidatorPubKey) error {
	if cert == nil || cert.Version != 1 || cert.Height != height || cert.BlockHash != blockHash {
		return fmt.Errorf("durable finality certificate does not bind the exact canonical parent height and hash")
	}
	if len(cert.Votes) != 1 || !bytes.Equal(cert.Votes[0].Validator, expected) {
		return fmt.Errorf("finality certificate must contain exactly one vote from the expected validator; this does not prove a quorum")
	}
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), blockHash[:])
	if !cert.Votes[0].Validator.Verify(message, cert.Votes[0].Signature) {
		return fmt.Errorf("durable finality certificate has an invalid expected-validator signature")
	}
	return nil
}
