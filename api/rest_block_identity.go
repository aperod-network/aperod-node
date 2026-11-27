package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// canonicalBlockID resolves the public block ID through the persisted height
// index. Older headers were hashed before OraclePrice and BaseFee were added;
// for those records, accept only a known historical header encoding whose
// signature verifies against the indexed ID. This is an API/read-path check,
// not a consensus rule or an independent chain-authentication claim.
func (s *Server) canonicalBlockID(block *core.Block) (crypto.Hash32, bool, error) {
	if block == nil {
		return crypto.Hash32{}, false, fmt.Errorf("block is nil")
	}
	currentHash := block.Hash()
	if s.blockStore == nil {
		return currentHash, false, nil
	}

	height := block.Header.Height
	indexedHash, found, err := s.blockStore.GetCanonicalHash(height)
	if err != nil {
		return crypto.Hash32{}, false, fmt.Errorf("read canonical index at height %d: %w", height, err)
	}
	if !found {
		return currentHash, false, nil
	}
	if identity := s.mainnetIdentity; identity != nil {
		switch height {
		case 0:
			if indexedHash != identity.genesisIndexHash || currentHash != identity.genesisHeaderHash {
				return crypto.Hash32{}, true, fmt.Errorf("height-zero block does not match the signed Mainnet genesis")
			}
		case 1:
			parentHash, parentFound, parentErr := s.blockStore.GetCanonicalHash(0)
			if parentErr != nil || !parentFound || parentHash != identity.genesisIndexHash {
				return crypto.Hash32{}, true, fmt.Errorf("height-one parent does not match the signed Mainnet genesis index")
			}
		}
		if height == identity.checkpointHeight &&
			(indexedHash != identity.checkpointIndexHash || currentHash != identity.checkpointHeaderHash) {
			return crypto.Hash32{}, true, fmt.Errorf("checkpoint block does not match the signed Mainnet identity")
		}
	}
	if height == 0 {
		if !block.IsGenesis() {
			return crypto.Hash32{}, true, fmt.Errorf("canonical height zero is not a genesis block")
		}
	} else {
		parentHash, parentFound, err := s.blockStore.GetCanonicalHash(height - 1)
		if err != nil {
			return crypto.Hash32{}, true, fmt.Errorf("read canonical parent at height %d: %w", height-1, err)
		}
		if parentFound && block.Header.PrevHash != parentHash {
			return crypto.Hash32{}, true, fmt.Errorf("block at height %d does not extend the canonical parent", height)
		}
	}

	if currentHash == indexedHash {
		return indexedHash, true, nil
	}
	authority := s.identitySignatureAuthority(block.Header.ValidatorPub, height)
	for _, candidate := range historicalHeaderHashCandidates(block.Header) {
		if candidate == indexedHash && authority.Verify(indexedHash, block.Header.Signature) {
			return indexedHash, true, nil
		}
	}
	return crypto.Hash32{}, true, fmt.Errorf("block header at height %d does not match its canonical index", height)
}

func (s *Server) canonicalPrunedBlockID(block *store.StoredBlock) (crypto.Hash32, error) {
	if block == nil || s.blockStore == nil {
		return crypto.Hash32{}, fmt.Errorf("pruned block or canonical store is unavailable")
	}
	indexedHash, found, err := s.blockStore.GetCanonicalHash(block.Height)
	if err != nil {
		return crypto.Hash32{}, fmt.Errorf("read canonical index at height %d: %w", block.Height, err)
	}
	if !found || indexedHash != block.Hash {
		return crypto.Hash32{}, fmt.Errorf("pruned block at height %d differs from its canonical index", block.Height)
	}
	if identity := s.mainnetIdentity; identity != nil {
		switch block.Height {
		case 0:
			if block.Hash != identity.genesisIndexHash {
				return crypto.Hash32{}, fmt.Errorf("pruned height zero does not match the signed Mainnet genesis index")
			}
		case 1:
			if block.PrevHash != identity.genesisIndexHash {
				return crypto.Hash32{}, fmt.Errorf("pruned height one does not extend the signed Mainnet genesis")
			}
		}
		if block.Height == identity.checkpointHeight && block.Hash != identity.checkpointIndexHash {
			return crypto.Hash32{}, fmt.Errorf("pruned checkpoint does not match the signed Mainnet identity")
		}
	}
	if block.Height == 0 {
		if block.PrevHash != (crypto.Hash32{}) {
			return crypto.Hash32{}, fmt.Errorf("canonical height zero has a nonzero parent")
		}
	} else {
		parentHash, parentFound, err := s.blockStore.GetCanonicalHash(block.Height - 1)
		if err != nil {
			return crypto.Hash32{}, fmt.Errorf("read canonical parent at height %d: %w", block.Height-1, err)
		}
		if parentFound && block.PrevHash != parentHash {
			return crypto.Hash32{}, fmt.Errorf("pruned block at height %d does not extend the canonical parent", block.Height)
		}
	}
	return indexedHash, nil
}

func historicalHeaderHashCandidates(header core.BlockHeader) []crypto.Hash32 {
	return []crypto.Hash32{
		historicalHeaderHash(header, false, false),
		historicalHeaderHash(header, true, false),
		historicalHeaderHash(header, false, true),
	}
}

// historicalHeaderHash is used only to render already-indexed historical
// records. It mirrors the pre-field header encodings and must never be used to
// admit blocks into consensus.
func historicalHeaderHash(header core.BlockHeader, includeOraclePrice, includeBaseFee bool) crypto.Hash32 {
	var encoded bytes.Buffer
	var u64 [8]byte
	binary.LittleEndian.PutUint64(u64[:], header.Height)
	encoded.Write(u64[:])
	encoded.Write(header.PrevHash[:])
	encoded.Write(header.MerkleRoot[:])
	binary.LittleEndian.PutUint64(u64[:], uint64(header.Timestamp))
	encoded.Write(u64[:])
	var u32 [4]byte
	binary.LittleEndian.PutUint32(u32[:], header.Round)
	encoded.Write(u32[:])
	encoded.Write(header.ValidatorPub)
	if includeOraclePrice {
		binary.LittleEndian.PutUint64(u64[:], header.OraclePrice)
		encoded.Write(u64[:])
	}
	if includeBaseFee {
		binary.LittleEndian.PutUint64(u64[:], header.BaseFee)
		encoded.Write(u64[:])
	}
	sum := sha256.Sum256(encoded.Bytes())
	return crypto.Hash32(sum)
}
