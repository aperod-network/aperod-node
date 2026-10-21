// Command avm-checkpoint builds, independently signs, or assembles a v2 AVM
// quorum checkpoint using canonical database copies and witness keys.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

type stringListFlag []string

type witnessSignatureFile struct {
	WitnessPublicKey string `json:"witness_public_key"`
	Signature        string `json:"signature"`
}

func (s *stringListFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringListFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	mode := flag.String("mode", "build", "build, sign, or assemble")
	dbPath := flag.String("db", "", "path to a canonical chain.db copy")
	activationHeight := flag.Uint64("activation-height", 0, "configured AVM activation height")
	candidatePath := flag.String("candidate", "", "unsigned v2 checkpoint draft")
	keyPath := flag.String("key", "", "0600 witness private-key file (sign mode only)")
	outputPath := flag.String("out", "", "output draft, partial signature, or final checkpoint")
	threshold := flag.Int("threshold", 0, "required m-of-n witness signature count (assemble mode)")
	var authorityKeys stringListFlag
	var signatureFiles stringListFlag
	flag.Var(&authorityKeys, "authority", "trusted witness public key in hex (repeat for each key)")
	flag.Var(&signatureFiles, "signature", "detached witness signature file (repeat for each signer)")
	flag.Parse()
	var err error
	switch *mode {
	case "build":
		if *dbPath == "" || *activationHeight == 0 || *outputPath == "" {
			flag.Usage()
			os.Exit(2)
		}
		err = runBuild(*dbPath, *activationHeight, *outputPath)
	case "sign":
		if *dbPath == "" || *activationHeight == 0 || *candidatePath == "" ||
			*keyPath == "" || *outputPath == "" {
			flag.Usage()
			os.Exit(2)
		}
		err = runSign(*dbPath, *activationHeight, *candidatePath, *keyPath, *outputPath)
	case "assemble":
		if *candidatePath == "" || *outputPath == "" || *threshold < 2 ||
			len(authorityKeys) < *threshold || len(signatureFiles) < *threshold {
			flag.Usage()
			os.Exit(2)
		}
		err = runAssemble(*candidatePath, signatureFiles, authorityKeys, *threshold, *outputPath)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "avm-checkpoint:", err)
		os.Exit(1)
	}
}

func runBuild(dbPath string, activationHeight uint64, outputPath string) error {
	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open chain database read-only: %w", err)
	}
	defer db.Close()
	tipHash, tipHeight, err := db.GetTip()
	if err != nil {
		return fmt.Errorf("read canonical tip: %w", err)
	}
	draft, err := avm.BuildCheckpointDraftFromCanonical(
		context.Background(),
		db,
		activationHeight,
		tipHeight,
		tipHash,
	)
	if err != nil {
		return err
	}
	if err := avm.WriteCheckpointFile(outputPath, draft); err != nil {
		return fmt.Errorf("write unsigned checkpoint draft: %w", err)
	}
	fmt.Printf(
		"unsigned quorum checkpoint draft written: height=%d block_hash=%x state_commitment=%x\n",
		draft.Height,
		draft.BlockHash,
		draft.StateCommitment,
	)
	return nil
}

func runSign(dbPath string, activationHeight uint64, candidatePath, keyPath, outputPath string) error {
	draft, found, err := avm.ReadCheckpointFile(candidatePath)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("checkpoint draft does not exist")
	}
	if err := avm.ValidateQuorumCheckpointDraft(draft, activationHeight); err != nil {
		return err
	}

	db, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return fmt.Errorf("open independent chain database read-only: %w", err)
	}
	defer db.Close()
	localDraft, err := avm.BuildCheckpointDraftFromCanonical(
		context.Background(), db, activationHeight, draft.Height, draft.BlockHash,
	)
	if err != nil {
		return fmt.Errorf("independent canonical replay: %w", err)
	}
	if !avm.CheckpointPayloadEqual(draft, localDraft) {
		return fmt.Errorf("candidate differs from this witness's canonical replay")
	}

	privateKey, err := readWitnessKey(keyPath)
	if err != nil {
		return err
	}
	defer crypto.ZeroBytes(privateKey.Bytes())
	signature, err := avm.SignCheckpointDraft(draft, activationHeight, privateKey)
	if err != nil {
		return err
	}
	if err := writeWitnessSignature(outputPath, signature); err != nil {
		return fmt.Errorf("write detached witness signature: %w", err)
	}
	fmt.Printf(
		"independent witness signature written: height=%d witness_public_key=%s\n",
		draft.Height,
		signature.WitnessPublicKey.Hex(),
	)
	return nil
}

func runAssemble(candidatePath string, signaturePaths, authorityHex []string, threshold int, outputPath string) error {
	draft, found, err := avm.ReadCheckpointFile(candidatePath)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("checkpoint draft does not exist")
	}
	if err := avm.ValidateQuorumCheckpointDraft(draft, draft.ActivationHeight); err != nil {
		return err
	}
	authorities, err := avm.ParseCheckpointAuthorities(authorityHex)
	if err != nil {
		return err
	}
	signatures := make([]avm.CheckpointWitnessSignature, 0, len(signaturePaths))
	for _, path := range signaturePaths {
		signature, err := readWitnessSignature(path)
		if err != nil {
			return err
		}
		signatures = append(signatures, signature)
	}
	checkpoint, err := avm.AttachCheckpointSignatures(
		draft, signatures, draft.ActivationHeight, authorities, threshold,
	)
	if err != nil {
		return err
	}
	if err := avm.WriteCheckpointFile(outputPath, checkpoint); err != nil {
		return fmt.Errorf("write quorum checkpoint: %w", err)
	}
	fmt.Printf(
		"quorum checkpoint assembled: height=%d signatures=%d threshold=%d\n",
		checkpoint.Height,
		len(checkpoint.WitnessSignatures),
		threshold,
	)
	return nil
}

func writeWitnessSignature(path string, signature avm.CheckpointWitnessSignature) error {
	wire := witnessSignatureFile{
		WitnessPublicKey: hex.EncodeToString(signature.WitnessPublicKey),
		Signature:        hex.EncodeToString(signature.Signature),
	}
	data, err := json.Marshal(wire)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return nil
}

func readWitnessSignature(path string) (avm.CheckpointWitnessSignature, error) {
	file, err := os.Open(path)
	if err != nil {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("open witness signature %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("stat witness signature %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("witness signature %q is not a bounded regular file", path)
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var wire witnessSignatureFile
	if err := decoder.Decode(&wire); err != nil {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("decode witness signature %q: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("witness signature %q has trailing data", path)
	}
	publicBytes, err := hex.DecodeString(wire.WitnessPublicKey)
	if err != nil {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("malformed witness public key in %q", path)
	}
	publicKey, err := crypto.ValidatorPubKeyFromBytes(publicBytes)
	if err != nil {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("malformed witness public key in %q", path)
	}
	signatureBytes, err := hex.DecodeString(wire.Signature)
	if err != nil || len(signatureBytes) != 64 {
		return avm.CheckpointWitnessSignature{}, fmt.Errorf("malformed witness signature in %q", path)
	}
	return avm.CheckpointWitnessSignature{
		WitnessPublicKey: publicKey,
		Signature:        signatureBytes,
	}, nil
}

func readWitnessKey(keyPath string) (crypto.ValidatorPrivKey, error) {
	keyFile, err := os.Open(keyPath)
	if err != nil {
		return nil, fmt.Errorf("open witness key: %w", err)
	}
	defer keyFile.Close()
	info, err := keyFile.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat witness key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("witness key must be a regular file with no group/other permissions")
	}
	keyBytes, err := io.ReadAll(io.LimitReader(keyFile, 65))
	if err != nil {
		crypto.ZeroBytes(keyBytes)
		return nil, fmt.Errorf("read witness key: %w", err)
	}
	parsed, err := crypto.ValidatorPrivKeyFromBytes(keyBytes)
	if err != nil {
		crypto.ZeroBytes(keyBytes)
		return nil, fmt.Errorf("parse witness key: %w", err)
	}
	// ValidatorPrivKeyFromBytes accepts a 64-byte key without copying it.
	// Copy before erasing the input, then erase both working buffers on exit.
	privateKeyBytes := append([]byte(nil), parsed.Bytes()...)
	crypto.ZeroBytes(keyBytes)
	crypto.ZeroBytes(parsed.Bytes())
	return crypto.ValidatorPrivKey(privateKeyBytes), nil
}
