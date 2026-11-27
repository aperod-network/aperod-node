package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
)

type authorityForkReport struct {
	ActivationHeight     uint64 `json:"activation_height"`
	ParentHash           string `json:"parent_hash"`
	IdentityPublicKey    string `json:"identity_public_key"`
	PreviousSigner       string `json:"previous_signer"`
	ReplacementSigner    string `json:"replacement_signer"`
	RegistrySnapshotSHA  string `json:"registry_snapshot_sha256"`
	AuthorityForkFileSHA string `json:"authority_fork_file_sha256"`
}

func runAuthorityFork(args []string) error {
	flags := flag.NewFlagSet("authority-fork", flag.ContinueOnError)
	snapshotPath := flags.String("snapshot", "", "path to the stopped canonical chain.db")
	previousPath := flags.String("previous-fork", "", "path to the currently pinned authority fork")
	previousSHA := flags.String("previous-fork-sha256", "", "expected SHA-256 of the previous authority fork")
	salePath := flags.String("sale-migration", "", "path to the pinned sale migration artifact")
	lpodPath := flags.String("lpod-witness", "", "path to the pinned LPoD witness artifact")
	keyPath := flags.String("key", "", "0600 successor seed/private-key file")
	expectedCurrent := flags.String("expected-current-signer", "", "expected active historical signer public key")
	replacementPublic := flags.String("replacement-public-key", "", "pinned successor public key")
	outputPath := flags.String("out", "", "new authority-fork JSON output; existing files are never overwritten")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *snapshotPath == "" || *previousPath == "" || *previousSHA == "" ||
		*salePath == "" || *lpodPath == "" || *keyPath == "" ||
		*expectedCurrent == "" || *replacementPublic == "" || *outputPath == "" {
		return errors.New("authority-fork requires snapshot, previous fork and digest, migration artifacts, key, signer pins, and output path")
	}

	fork, err := mainnetbootstrap.LoadPinnedAuthorityFork(*previousPath, *previousSHA)
	if err != nil {
		return fmt.Errorf("load pinned previous authority fork: %w", err)
	}
	snapshotDigest, err := mainnetbootstrap.SnapshotTreeSHA256(*snapshotPath)
	if err != nil {
		return fmt.Errorf("hash stopped canonical snapshot: %w", err)
	}
	saleDigest, err := mainnetbootstrap.FileSHA256(*salePath)
	if err != nil {
		return fmt.Errorf("hash pinned sale migration: %w", err)
	}
	lpodDigest, err := mainnetbootstrap.FileSHA256(*lpodPath)
	if err != nil {
		return fmt.Errorf("hash pinned LPoD witness: %w", err)
	}
	statement, err := mainnetbootstrap.InspectSnapshotDB(
		*snapshotPath, snapshotDigest, fork, *previousSHA, saleDigest, lpodDigest,
	)
	if err != nil {
		return fmt.Errorf("inspect canonical Mainnet snapshot: %w", err)
	}
	registry, err := mainnetbootstrap.InspectCheckpointRegistrySnapshot(*snapshotPath, statement)
	if err != nil {
		return fmt.Errorf("read registry at the exact canonical tip: %w", err)
	}
	parentBytes, err := hex.DecodeString(statement.CheckpointIndexHash)
	if err != nil || len(parentBytes) != len(crypto.Hash32{}) {
		return errors.New("inspected canonical checkpoint hash is malformed")
	}
	var parentHash crypto.Hash32
	copy(parentHash[:], parentBytes)
	privateKey, err := readSuccessorPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(privateKey.Bytes())

	successor, registrySHA, err := mainnetbootstrap.BuildSuccessorAuthorityFork(
		fork,
		statement.CheckpointHeight,
		parentHash,
		registry,
		privateKey,
		*expectedCurrent,
		*replacementPublic,
	)
	if err != nil {
		return fmt.Errorf("build successor authority fork: %w", err)
	}
	encoded, err := json.MarshalIndent(successor, "", "  ")
	if err != nil {
		return fmt.Errorf("encode successor authority fork: %w", err)
	}
	file, err := os.OpenFile(*outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create authority fork without overwriting existing files: %w", err)
	}
	data := append(encoded, '\n')
	written, err := file.Write(data)
	if err != nil || written != len(data) {
		_ = file.Close()
		_ = os.Remove(*outputPath)
		if err == nil {
			err = io.ErrShortWrite
		}
		return fmt.Errorf("write successor authority fork: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(*outputPath)
		return fmt.Errorf("sync successor authority fork: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(*outputPath)
		return fmt.Errorf("close successor authority fork: %w", err)
	}
	fileDigest := sha256.Sum256(append(encoded, '\n'))
	report := authorityForkReport{
		ActivationHeight:     successor.Height,
		ParentHash:           hex.EncodeToString(successor.Parent[:]),
		IdentityPublicKey:    successor.Identity.Hex(),
		PreviousSigner:       *expectedCurrent,
		ReplacementSigner:    successor.Replacement.Hex(),
		RegistrySnapshotSHA:  registrySHA,
		AuthorityForkFileSHA: hex.EncodeToString(fileDigest[:]),
	}
	out, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode authority-fork report: %w", err)
	}
	_, err = os.Stdout.Write(append(out, '\n'))
	return err
}

func readSuccessorPrivateKey(path string) (crypto.ValidatorPrivKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect successor private key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm() != 0o600 {
		return nil, errors.New("successor private key must be a regular non-symlink file with mode 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open successor private key: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 65))
	if err != nil {
		crypto.ZeroBytes(raw)
		return nil, fmt.Errorf("read successor private key: %w", err)
	}
	parsed, err := crypto.ValidatorPrivKeyFromBytes(raw)
	if err != nil {
		crypto.ZeroBytes(raw)
		return nil, fmt.Errorf("parse successor private key: %w", err)
	}
	privateBytes := append([]byte(nil), parsed.Bytes()...)
	crypto.ZeroBytes(raw)
	crypto.ZeroBytes(parsed.Bytes())
	return crypto.ValidatorPrivKey(privateBytes), nil
}
