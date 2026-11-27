// finality-audit inspects an offline chain copy; it never opens the live DB.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func main() {
	dbPath := flag.String("db", "", "disposable copy of chain.db under /tmp")
	snapshotPath := flag.String("snapshot-json", "", "decompressed historical startup snapshot under /tmp")
	anchorHeight := flag.Uint64("anchor-height", 0, "height of the independently checked snapshot")
	anchorHex := flag.String("anchor-hash", "", "snapshot tip hash")
	authorityPath := flag.String("authority-fork", "", "pinned validator authority fork JSON under /tmp")
	authoritySHA256 := flag.String("authority-fork-sha256", "", "expected SHA-256 from node configuration")
	flag.Parse()
	path, err := filepath.Abs(*dbPath)
	if err != nil || !strings.HasPrefix(path, "/tmp/") || *anchorHeight == 0 {
		fatal("requires an offline DB copy under /tmp and a nonzero anchor height")
	}
	snapshot, err := filepath.Abs(*snapshotPath)
	if err != nil || !strings.HasPrefix(snapshot, "/tmp/") {
		fatal("requires an offline snapshot under /tmp")
	}
	anchorBytes, err := hex.DecodeString(*anchorHex)
	if err != nil || len(anchorBytes) != 32 {
		fatal("invalid anchor hash")
	}
	var anchor crypto.Hash32
	copy(anchor[:], anchorBytes)
	registry := readHistoricalRegistry(snapshot, *anchorHeight, *anchorHex)
	authorityFork, err := loadAuthorityFork(*authorityPath, *authoritySHA256)
	if err != nil {
		fatal("load pinned validator authority fork: %v", err)
	}
	db, err := store.OpenReadOnly(path)
	if err != nil {
		fatal("open offline DB: %v", err)
	}
	defer db.Close()
	if err := verifyAuthorityForkAnchor(db, authorityFork); err != nil {
		fatal("verify validator authority fork: %v", err)
	}
	cert, err := db.LoadFinalityCertificate()
	if err != nil || cert == nil {
		fatal("read finality certificate: %v", err)
	}
	if cert.Height <= *anchorHeight {
		fatal("certificate is not after snapshot anchor")
	}
	indexedAnchor, found, err := db.GetCanonicalHash(*anchorHeight)
	if err != nil || !found || indexedAnchor != anchor {
		fatal("snapshot anchor differs from canonical height index: %v", err)
	}
	previous := anchor
	var txs, stakeTxs int
	for h := *anchorHeight; h <= cert.Height; h++ {
		indexed, found, err := db.GetCanonicalHash(h)
		if err != nil || !found {
			fatal("height %d: missing or malformed canonical index: %v", h, err)
		}
		raw, err := db.GetRawBlock(indexed)
		if err != nil || raw == nil {
			fatal("height %d: missing block body: %v", h, err)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			fatal("height %d: decode block: %v", h, err)
		}
		if block.Header.Height != h || block.Hash() != indexed {
			fatal("height %d: body/header differs from canonical index", h)
		}
		if h > *anchorHeight && block.Header.PrevHash != previous {
			fatal("height %d: broken canonical ancestry", h)
		}
		if h > *anchorHeight && !block.Header.VerifySignature(authorityFor(authorityFork, block.Header.ValidatorPub, h)) {
			fatal("height %d: invalid proposer signature", h)
		}
		if block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
			fatal("height %d: missing/pruned transactions or invalid Merkle root", h)
		}
		for i := range block.Txs {
			if block.Txs[i].IsStake() {
				stakeTxs++
			}
		}
		txs += len(block.Txs)
		previous = indexed
	}
	if previous != cert.BlockHash {
		fatal("certified hash differs from canonical replay")
	}
	// Without replaying stake transitions, the snapshot's committee is only
	// applicable if the canonical interval contains no stake transactions.
	if stakeTxs != 0 {
		fatal("stake transactions occurred after the snapshot; full registry replay required")
	}
	active := 0
	for _, entry := range registry.Validators {
		if entry.Status == core.ValidatorActive {
			active++
		}
	}
	if active == 0 {
		fatal("historical snapshot has no active validators")
	}
	seen := make(map[string]bool)
	for _, vote := range cert.Votes {
		message := crypto.FinalityVoteMessage(vote.VoteVersion, cert.Height, cert.BlockHash)
		if !authorityFor(authorityFork, vote.Validator, cert.Height).Verify(message, vote.Signature) {
			fatal("certificate contains an invalid vote signature")
		}
		key := vote.Validator.Hex()
		if seen[key] || registry.Validators[key] == nil || registry.Validators[key].Status != core.ValidatorActive {
			fatal("certificate contains a duplicate or non-committee vote")
		}
		seen[key] = true
	}
	needed := int(float64(active)*0.667) + 1
	if len(seen) < needed {
		fatal("historical snapshot committee requires %d votes; found %d", needed, len(seen))
	}
	forkHeight := uint64(0)
	if authorityFork != nil {
		forkHeight = authorityFork.Height
	}
	fmt.Printf("historical_snapshot_consistent anchor=%d certified=%d blocks=%d txs=%d stake_txs=%d active=%d required=%d votes=%d authority_fork_height=%d trust=operator_snapshot_not_consensus_attestation\n",
		*anchorHeight, cert.Height, cert.Height-*anchorHeight+1, txs, stakeTxs, active, needed, len(cert.Votes), forkHeight)
}

func loadAuthorityFork(path, expectedSHA256 string) (*core.ValidatorAuthorityFork, error) {
	if path == "" && expectedSHA256 == "" {
		return nil, nil
	}
	if path == "" || expectedSHA256 == "" {
		return nil, fmt.Errorf("authority fork path and pinned SHA-256 must be supplied together")
	}
	absPath, err := filepath.Abs(path)
	if err != nil || !strings.HasPrefix(absPath, "/tmp/") {
		return nil, fmt.Errorf("authority fork must be a disposable file under /tmp")
	}
	want, err := hex.DecodeString(expectedSHA256)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("invalid expected authority fork SHA-256")
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("read authority fork: %w", err)
	}
	got := sha256.Sum256(raw)
	if !bytes.Equal(got[:], want) {
		return nil, fmt.Errorf("authority fork differs from the configured SHA-256")
	}
	var fork core.ValidatorAuthorityFork
	if err := json.Unmarshal(raw, &fork); err != nil {
		return nil, fmt.Errorf("decode authority fork: %w", err)
	}
	if err := fork.Validate(); err != nil {
		return nil, err
	}
	return &fork, nil
}

func authorityFor(fork *core.ValidatorAuthorityFork, identity crypto.ValidatorPubKey, height uint64) crypto.ValidatorPubKey {
	if fork == nil {
		return identity
	}
	return fork.Authority(identity, height)
}

func verifyAuthorityForkAnchor(db *store.DB, fork *core.ValidatorAuthorityFork) error {
	if fork == nil {
		return nil
	}
	genesis, found, err := db.GetCanonicalHash(0)
	if err != nil || !found || genesis != fork.Genesis {
		return fmt.Errorf("configured fork genesis differs from the canonical height-zero index")
	}
	parent, found, err := db.GetCanonicalHash(fork.Height - 1)
	if err != nil || !found || parent != fork.Parent {
		return fmt.Errorf("configured fork parent differs from the canonical activation parent")
	}
	activationHash, found, err := db.GetCanonicalHash(fork.Height)
	if err != nil || !found {
		return fmt.Errorf("canonical authority-fork activation block is unavailable")
	}
	raw, err := db.GetRawBlock(activationHash)
	if err != nil || len(raw) == 0 {
		return fmt.Errorf("authority-fork activation block body is unavailable: %v", err)
	}
	var activation core.Block
	if err := json.Unmarshal(raw, &activation); err != nil {
		return fmt.Errorf("decode authority-fork activation block: %w", err)
	}
	if activation.Header.Height != fork.Height ||
		activation.Hash() != activationHash ||
		activation.Header.PrevHash != fork.Parent {
		return fmt.Errorf("authority-fork activation block does not match the pinned canonical parent")
	}
	if !activation.Header.VerifySignature(authorityFor(fork, activation.Header.ValidatorPub, fork.Height)) {
		return fmt.Errorf("authority-fork activation block signature is invalid")
	}
	if activation.Header.MerkleRoot != core.MerkleRoot(activation.Txs) {
		return fmt.Errorf("authority-fork activation block Merkle root is invalid")
	}
	return nil
}

func readHistoricalRegistry(path string, height uint64, hash string) core.RegistrySnapshot {
	file, err := os.Open(path)
	if err != nil {
		fatal("open historical snapshot: %v", err)
	}
	defer file.Close()
	prefix := make([]byte, 512)
	n, err := file.Read(prefix)
	if err != nil && err != io.EOF {
		fatal("read snapshot metadata: %v", err)
	}
	end := bytes.Index(prefix[:n], []byte(`,"utxos":`))
	if end < 0 {
		fatal("snapshot metadata is not at the expected location")
	}
	var meta struct {
		Version   int    `json:"v"`
		TipHeight uint64 `json:"tip_height"`
		TipHash   string `json:"tip_hash"`
	}
	if err := json.Unmarshal(append(append([]byte{}, prefix[:end]...), '}'), &meta); err != nil {
		fatal("decode snapshot metadata: %v", err)
	}
	if meta.Version != 2 || meta.TipHeight != height || meta.TipHash != hash {
		fatal("snapshot metadata does not match the requested canonical anchor")
	}
	stat, err := file.Stat()
	if err != nil || stat.Size() < 1<<20 {
		fatal("snapshot missing or truncated: %v", err)
	}
	size := int64(1 << 20)
	if _, err := file.Seek(-size, io.SeekEnd); err != nil {
		fatal("seek snapshot registry: %v", err)
	}
	tail := make([]byte, size)
	if _, err := io.ReadFull(file, tail); err != nil {
		fatal("read snapshot registry: %v", err)
	}
	start := bytes.LastIndex(tail, []byte(`"registry":`))
	if start < 0 {
		fatal("historical registry not present at the end of snapshot")
	}
	var registry core.RegistrySnapshot
	if err := json.NewDecoder(bytes.NewReader(tail[start+len(`"registry":`):])).Decode(&registry); err != nil {
		fatal("decode historical registry: %v", err)
	}
	if len(registry.Validators) == 0 {
		fatal("historical registry is empty")
	}
	return registry
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
