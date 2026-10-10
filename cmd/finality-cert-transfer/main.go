// finality-cert-transfer moves one signed finality certificate between
// independently validated offline/ stopped database copies.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

const (
	maxCertificateFile = 1 << 20
	maxRegistryFile    = 4 << 20
	maxRegistryReplay  = uint64(50_000)
)

type anchoredRegistry struct {
	Height   uint64                `json:"height"`
	Hash     crypto.Hash32         `json:"hash"`
	Registry core.RegistrySnapshot `json:"registry"`
}

func main() {
	mode := flag.String("mode", "", "export or import")
	dbPath := flag.String("db", "", "LevelDB path (export requires an offline copy under /tmp)")
	certificatePath := flag.String("certificate", "", "certificate JSON path (new output for export; input for import)")
	registryPath := flag.String("registry", "", "operator-trusted anchored registry JSON (required for import)")
	validatorHex := flag.String("expected-validator", "", "expected validator public key hex")
	genesisHex := flag.String("expected-genesis", "", "expected modern genesis block hash hex")
	apply := flag.Bool("apply", false, "persist an import; default is validation-only dry-run")
	confirmStopped := flag.Bool("confirm-stopped", false, "confirm the relay process is stopped before importing")
	flag.Parse()

	expected, err := decodeValidator(*validatorHex)
	if err != nil {
		fatal("%v", err)
	}
	genesis, err := decodeHash(*genesisHex)
	if err != nil {
		fatal("expected genesis: %v", err)
	}
	switch *mode {
	case "export":
		if err := exportCertificate(*dbPath, *certificatePath, expected, genesis); err != nil {
			fatal("%v", err)
		}
	case "import":
		if *registryPath == "" {
			fatal("--registry is required for import")
		}
		if *apply && !*confirmStopped {
			fatal("--apply requires explicit --confirm-stopped")
		}
		if err := importCertificate(*dbPath, *certificatePath, *registryPath, expected, genesis, *apply); err != nil {
			fatal("%v", err)
		}
	default:
		fatal("--mode must be export or import")
	}
}

func exportCertificate(dbPath, outputPath string, expected crypto.ValidatorPubKey, genesis crypto.Hash32) error {
	dbAbs, err := filepath.Abs(dbPath)
	if err != nil || !underTmp(dbAbs) {
		return fmt.Errorf("export requires an offline database copy under /tmp")
	}
	resolved, err := filepath.EvalSymlinks(dbAbs)
	if err != nil || !underTmp(resolved) {
		return fmt.Errorf("export database copy must resolve under /tmp")
	}
	db, err := store.OpenReadOnly(resolved)
	if err != nil {
		return fmt.Errorf("open offline database read-only: %w", err)
	}
	defer db.Close()
	cert, err := db.LoadFinalityCertificate()
	if err != nil {
		return fmt.Errorf("read source finality certificate: %w", err)
	}
	if cert == nil {
		return fmt.Errorf("source database has no finality certificate")
	}
	if err := verifyCertificateAtTip(db, cert, expected, genesis); err != nil {
		return err
	}
	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve output path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(outputAbs))
	if err != nil || !underTmp(parent) {
		return fmt.Errorf("export output must be under /tmp")
	}
	outputAbs = filepath.Join(parent, filepath.Base(outputAbs))
	raw, err := json.MarshalIndent(cert, "", "  ")
	if err != nil {
		return fmt.Errorf("encode certificate: %w", err)
	}
	raw = append(raw, '\n')
	f, err := os.OpenFile(outputAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create new exclusive certificate file: %w", err)
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(outputAbs)
		}
	}()
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("set certificate file mode: %w", err)
	}
	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("write certificate file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync certificate file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close certificate file: %w", err)
	}
	ok = true
	fmt.Printf("exported version=1 height=%d block_hash=%x file=%s mode=0600\n", cert.Height, cert.BlockHash, outputAbs)
	return nil
}

func importCertificate(dbPath, certPath, registryPath string, expected crypto.ValidatorPubKey, genesis crypto.Hash32, apply bool) error {
	cert, err := readCertificate(certPath)
	if err != nil {
		return err
	}
	snapshot, err := readAnchoredRegistry(registryPath)
	if err != nil {
		return err
	}
	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open relay read-only for preflight: %w", err)
	}
	if err := verifyCertificateAtTip(db, &cert, expected, genesis); err != nil {
		_ = db.Close()
		return err
	}
	tipHash, tipHeight, err := db.GetTip()
	if err == nil {
		err = verifyRegistryAnchorAndReplay(db, snapshot, tipHeight, tipHash, expected)
	}
	if err != nil {
		_ = db.Close()
		return err
	}
	prior, err := db.LoadFinalityCertificate()
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("read existing relay certificate: %w", err)
	}
	if err := checkExistingCertificate(prior, &cert); err != nil {
		_ = db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close relay after read-only preflight: %w", err)
	}
	if !apply {
		fmt.Printf("dry-run valid version=1 height=%d block_hash=%x write=none\n", cert.Height, cert.BlockHash)
		return nil
	}

	// This open is the stopped-relay/exclusive-LevelDB-lock gate.
	writable, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open relay writable (must be stopped and exclusively unlocked): %w", err)
	}
	defer writable.Close()
	recheckedHash, recheckedHeight, err := writable.GetTip()
	if err != nil || recheckedHash != tipHash || recheckedHeight != tipHeight {
		return fmt.Errorf("relay canonical tip changed after preflight; refusing write")
	}
	if err := verifyCertificateAtTip(writable, &cert, expected, genesis); err != nil {
		return fmt.Errorf("relay changed after preflight: %w", err)
	}
	prior, err = writable.LoadFinalityCertificate()
	if err != nil {
		return fmt.Errorf("recheck existing relay certificate: %w", err)
	}
	if err := checkExistingCertificate(prior, &cert); err != nil {
		return err
	}
	if prior != nil && prior.Height == cert.Height && prior.BlockHash == cert.BlockHash {
		fmt.Printf("already-present height=%d block_hash=%x write=none\n", cert.Height, cert.BlockHash)
		return nil
	}
	if err := writable.SaveFinalityCertificate(cert); err != nil {
		return fmt.Errorf("save finality certificate: %w", err)
	}
	fmt.Printf("imported height=%d block_hash=%x fsync=true\n", cert.Height, cert.BlockHash)
	return nil
}

func verifyCertificateAtTip(db *store.DB, cert *store.FinalityCertificate, expected crypto.ValidatorPubKey, genesis crypto.Hash32) error {
	hashBytes, err := db.GetMeta("tip/hash")
	if err != nil || len(hashBytes) != len(crypto.Hash32{}) {
		return fmt.Errorf("canonical tip hash metadata missing or malformed: %v", err)
	}
	heightBytes, err := db.GetMeta("tip/height")
	if err != nil || len(heightBytes) != 8 {
		return fmt.Errorf("canonical tip height metadata missing or malformed: %v", err)
	}
	tipHash, tipHeight, err := db.GetTip()
	var metaHash crypto.Hash32
	copy(metaHash[:], hashBytes)
	if err != nil || tipHash == (crypto.Hash32{}) || tipHash != metaHash ||
		tipHeight != binary.LittleEndian.Uint64(heightBytes) {
		return fmt.Errorf("canonical tip metadata is inconsistent: %v", err)
	}
	indexed, found, err := db.GetCanonicalHash(tipHeight)
	if err != nil || !found || indexed != tipHash {
		return fmt.Errorf("canonical tip does not match the height index: %v", err)
	}
	if cert == nil || cert.Version != 1 || cert.Height != tipHeight || cert.BlockHash != tipHash {
		return fmt.Errorf("certificate must be version 1 and bind exact database tip height/hash")
	}
	if err := verifyOneVote(cert, expected); err != nil {
		return err
	}
	if err := verifyBlockAt(db, tipHeight, tipHash, expected); err != nil {
		return fmt.Errorf("certified canonical block: %w", err)
	}
	if err := verifyExpectedGenesis(db, genesis); err != nil {
		return err
	}
	return nil
}

func verifyOneVote(cert *store.FinalityCertificate, expected crypto.ValidatorPubKey) error {
	if cert == nil || cert.Version != 1 {
		return fmt.Errorf("unsupported or missing finality certificate version")
	}
	if len(cert.Votes) != 1 || !bytes.Equal(cert.Votes[0].Validator, expected) {
		return fmt.Errorf("certificate must contain exactly one vote from the expected validator")
	}
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), cert.BlockHash[:])
	if !cert.Votes[0].Validator.Verify(message, cert.Votes[0].Signature) {
		return fmt.Errorf("finality certificate has an invalid expected-validator signature")
	}
	return nil
}

func verifyBlockAt(db *store.DB, height uint64, hash crypto.Hash32, expected crypto.ValidatorPubKey) error {
	indexed, found, err := db.GetCanonicalHash(height)
	if err != nil || !found || indexed != hash {
		return fmt.Errorf("canonical height index does not match expected hash: %v", err)
	}
	raw, err := db.GetRawBlock(indexed)
	if err != nil || raw == nil {
		return fmt.Errorf("canonical block body is unavailable: %v", err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return fmt.Errorf("decode canonical block body: %w", err)
	}
	if block.Header.Height != height || block.Hash() != hash ||
		!bytes.Equal(block.Header.ValidatorPub, expected) ||
		block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
		return fmt.Errorf("canonical block body, height, hash, proposer or Merkle root is invalid")
	}
	if !block.Header.VerifySignature() {
		return fmt.Errorf("canonical proposer signature is invalid")
	}
	return nil
}

func verifyExpectedGenesis(db *store.DB, expected crypto.Hash32) error {
	indexed, found, err := db.GetCanonicalHash(0)
	if err != nil || !found {
		return fmt.Errorf("canonical genesis index unavailable: %v", err)
	}
	raw, err := db.GetRawBlock(indexed)
	if err != nil || raw == nil {
		return fmt.Errorf("canonical genesis body unavailable: %v", err)
	}
	var block core.Block
	if err := json.Unmarshal(raw, &block); err != nil {
		return fmt.Errorf("decode canonical genesis: %w", err)
	}
	header := &block.Header
	if header.Height != 0 || header.MerkleRoot != core.MerkleRoot(block.Txs) {
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
		return fmt.Errorf("legacy-indexed canonical genesis does not match the expected modern hash")
	}
	if legacyGenesisID(header) != indexed || !header.ValidatorPub.Verify(indexed, header.Signature) {
		return fmt.Errorf("canonical genesis body has an invalid legacy index or signature")
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

func readCertificate(path string) (store.FinalityCertificate, error) {
	var cert store.FinalityCertificate
	f, err := openRegularNoFollow(path, maxCertificateFile)
	if err != nil {
		return cert, fmt.Errorf("open certificate JSON: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxCertificateFile+1))
	if err != nil || len(raw) > maxCertificateFile {
		return cert, fmt.Errorf("certificate JSON exceeds %d bytes or could not be read", maxCertificateFile)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cert); err != nil {
		return cert, fmt.Errorf("decode certificate JSON: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return cert, fmt.Errorf("certificate JSON has trailing data")
	}
	if cert.Version != 1 {
		return cert, fmt.Errorf("certificate JSON version must be 1")
	}
	return cert, nil
}

func readAnchoredRegistry(path string) (*anchoredRegistry, error) {
	f, err := openRegularNoFollow(path, maxRegistryFile)
	if err != nil {
		return nil, fmt.Errorf("open validator-set snapshot: %w", err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRegistryFile+1))
	if err != nil || len(raw) > maxRegistryFile {
		return nil, fmt.Errorf("validator-set snapshot exceeds %d bytes or could not be read", maxRegistryFile)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var snapshot anchoredRegistry
	if err := dec.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode validator-set snapshot: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("validator-set snapshot has trailing data")
	}
	return &snapshot, nil
}

func openRegularNoFollow(path string, maxBytes int64) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		_ = f.Close()
		return nil, fmt.Errorf("input must be a regular file no larger than %d bytes", maxBytes)
	}
	return f, nil
}

func verifyRegistryAnchorAndReplay(db *store.DB, snapshot *anchoredRegistry, parentHeight uint64, parentHash crypto.Hash32, expected crypto.ValidatorPubKey) error {
	if snapshot == nil || snapshot.Height > parentHeight || parentHeight-snapshot.Height > maxRegistryReplay {
		return fmt.Errorf("registry snapshot anchor must be at or before the tip and replay at most %d blocks", maxRegistryReplay)
	}
	if err := checkOperatorRegistrySnapshot(snapshot.Registry, expected); err != nil {
		return err
	}
	indexedAnchor, found, err := db.GetCanonicalHash(snapshot.Height)
	if err != nil || !found || indexedAnchor != snapshot.Hash {
		return fmt.Errorf("validator-set snapshot anchor is not canonical: %v", err)
	}
	if err := verifyBlockAt(db, snapshot.Height, snapshot.Hash, expected); err != nil {
		return fmt.Errorf("validator-set snapshot anchor: %w", err)
	}
	previous := snapshot.Hash
	if snapshot.Height == parentHeight {
		if previous != parentHash {
			return fmt.Errorf("registry anchor does not equal expected parent")
		}
		return nil
	}
	for height := snapshot.Height + 1; height <= parentHeight; height++ {
		indexed, found, err := db.GetCanonicalHash(height)
		if err != nil || !found {
			return fmt.Errorf("canonical index missing at height %d: %v", height, err)
		}
		raw, err := db.GetRawBlock(indexed)
		if err != nil || raw == nil {
			return fmt.Errorf("canonical block body missing at height %d: %v", height, err)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			return fmt.Errorf("decode canonical block at height %d: %w", height, err)
		}
		if block.Header.Height != height || block.Hash() != indexed || block.Header.PrevHash != previous ||
			!bytes.Equal(block.Header.ValidatorPub, expected) || block.Header.MerkleRoot != core.MerkleRoot(block.Txs) {
			return fmt.Errorf("canonical ancestry, modern hash, proposer or Merkle root invalid at height %d", height)
		}
		if !block.Header.VerifySignature() {
			return fmt.Errorf("canonical proposer signature invalid at height %d", height)
		}
		for _, tx := range block.Txs {
			if tx.IsStake() {
				return fmt.Errorf("stake transaction at height %d makes registry snapshot uncertain", height)
			}
		}
		previous = indexed
		if height == parentHeight {
			break
		}
	}
	if previous != parentHash {
		return fmt.Errorf("canonical registry replay does not reach the expected tip")
	}
	return nil
}

func checkOperatorRegistrySnapshot(registry core.RegistrySnapshot, expected crypto.ValidatorPubKey) error {
	if len(registry.Validators) != 1 {
		return fmt.Errorf("operator-supplied registry must list exactly one validator")
	}
	entry := registry.Validators[expected.Hex()]
	if entry == nil || !bytes.Equal(entry.PubKey, expected) || entry.Status != core.ValidatorActive ||
		entry.StakeNAPR < core.MinStakeNAPR || entry.UnbondEndBlock != 0 || len(entry.UnbondingQueue) != 0 {
		return fmt.Errorf("operator-supplied registry does not list the expected active validator as its only entry")
	}
	return nil
}

func checkExistingCertificate(prior, incoming *store.FinalityCertificate) error {
	if prior == nil {
		return nil
	}
	if prior.Height > incoming.Height {
		return fmt.Errorf("relay already has a newer finality certificate at height %d", prior.Height)
	}
	if prior.Height == incoming.Height && prior.BlockHash != incoming.BlockHash {
		return fmt.Errorf("relay has a conflicting finality certificate at height %d", prior.Height)
	}
	return nil
}

func decodeValidator(value string) (crypto.ValidatorPubKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("expected-validator must be 32-byte hex: %w", err)
	}
	pub, err := crypto.ValidatorPubKeyFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("expected-validator: %w", err)
	}
	return pub, nil
}

func decodeHash(value string) (crypto.Hash32, error) {
	var out crypto.Hash32
	raw, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("must be exactly 32-byte hex")
	}
	copy(out[:], raw)
	if out == (crypto.Hash32{}) {
		return out, errors.New("must not be zero")
	}
	return out, nil
}

func underTmp(path string) bool {
	return path == "/tmp" || strings.HasPrefix(path, "/tmp/")
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
