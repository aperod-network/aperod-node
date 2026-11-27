// mainnet-bootstrap-attest creates and verifies the signed Mainnet snapshot
// manifest. The sign command prints only the operator public key and signature.
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
)

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("usage: mainnet-bootstrap-attest inspect|sign|verify|evidence-verify|authority-fork|digest|finality|committee-sign|release-sign|release-verify|release-preflight [options]"))
	}
	var err error
	switch os.Args[1] {
	case "inspect":
		err = runInspect(os.Args[2:])
	case "sign":
		err = runSign(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "evidence-verify":
		err = runEvidenceVerify(os.Args[2:])
	case "authority-fork":
		err = runAuthorityFork(os.Args[2:])
	case "digest":
		err = runDigest(os.Args[2:])
	case "finality":
		err = runFinality(os.Args[2:])
	case "committee-sign":
		err = runCommitteeSign(os.Args[2:])
	case "release-sign", "release-verify", "release-preflight":
		err = runRelease(os.Args[1], os.Args[2:])
	default:
		err = errors.New("unknown mainnet-bootstrap-attest command")
	}
	if err != nil {
		fail(err)
	}
}

type evidenceVerifyReport struct {
	SignatureVerified         bool   `json:"signature_verified"`
	SnapshotPayloadVerified   bool   `json:"snapshot_payload_verified"`
	AVMCheckpointVerified     bool   `json:"avm_checkpoint_verified"`
	FinalityQuorumEstablished bool   `json:"finality_quorum_established"`
	Network                   string `json:"network"`
	ChainID                   string `json:"chain_id"`
	TrustModel                string `json:"trust_model"`
	SignerPublicKey           string `json:"signer_public_key"`
	GenesisHeaderHash         string `json:"genesis_header_hash"`
	GenesisIndexHash          string `json:"genesis_index_hash"`
	CheckpointHeight          uint64 `json:"checkpoint_height"`
	CheckpointHeaderHash      string `json:"checkpoint_header_hash"`
	SnapshotTreeSHA256        string `json:"snapshot_tree_sha256"`
	RegistrySHA256            string `json:"registry_sha256"`
	AVMActivationHeight       uint64 `json:"avm_activation_height,omitempty"`
}

func verifyEvidence(manifestPath, avmCheckpointPath string, avmActivationHeight uint64) (*evidenceVerifyReport, error) {
	if manifestPath == "" {
		return nil, errors.New("evidence-verify requires --manifest")
	}
	if (avmCheckpointPath == "") != (avmActivationHeight == 0) {
		return nil, errors.New("--avm-checkpoint and a nonzero --avm-activation-height must be supplied together")
	}
	file, err := os.Open(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("open signed manifest: %w", err)
	}
	signed, decodeErr := mainnetbootstrap.ReadStrict(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return nil, decodeErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close signed manifest: %w", closeErr)
	}
	if err := signed.Verify(); err != nil {
		return nil, err
	}
	report := &evidenceVerifyReport{
		SignatureVerified:         true,
		SnapshotPayloadVerified:   false,
		AVMCheckpointVerified:     false,
		FinalityQuorumEstablished: false,
		Network:                   signed.Statement.Network,
		ChainID:                   signed.Statement.ChainID,
		TrustModel:                signed.Statement.TrustModel,
		SignerPublicKey:           signed.SignerPublicKey,
		GenesisHeaderHash:         signed.Statement.GenesisHeaderHash,
		GenesisIndexHash:          signed.Statement.GenesisIndexHash,
		CheckpointHeight:          signed.Statement.CheckpointHeight,
		CheckpointHeaderHash:      signed.Statement.CheckpointHeaderHash,
		SnapshotTreeSHA256:        signed.Statement.SnapshotTreeSHA256,
		RegistrySHA256:            signed.Statement.RegistrySHA256,
		AVMActivationHeight:       avmActivationHeight,
	}
	if avmCheckpointPath != "" {
		checkpoint, present, err := avm.ReadCheckpointFile(avmCheckpointPath)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, errors.New("signed AVM checkpoint not found")
		}
		if err := verifyBootstrapAVM(
			signed.Statement, checkpoint, avmActivationHeight,
			mainnetbootstrap.BootstrapSignerPublicKeyHex,
		); err != nil {
			return nil, err
		}
		report.AVMCheckpointVerified = true
	}
	return report, nil
}

func runEvidenceVerify(args []string) error {
	flags := flag.NewFlagSet("evidence-verify", flag.ContinueOnError)
	manifestPath := flags.String("manifest", "", "path to the signed Mainnet bootstrap statement")
	avmCheckpointPath := flags.String("avm-checkpoint", "", "optional signed AVM checkpoint for this bootstrap")
	avmActivationHeight := flags.Uint64("avm-activation-height", 0, "AVM activation height from the reviewed Mainnet profile")
	if err := flags.Parse(args); err != nil {
		return err
	}
	report, err := verifyEvidence(*manifestPath, *avmCheckpointPath, *avmActivationHeight)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode evidence report: %w", err)
	}
	_, err = os.Stdout.Write(append(encoded, '\n'))
	return err
}

func runInspect(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	snapshotPath := flags.String("snapshot", "", "path to the closed chain.db directory")
	authorityForkPath := flags.String("authority-fork", "", "path to the pinned validator authority fork")
	authorityForkSHA256 := flags.String("authority-fork-sha256", "", "approved SHA-256 of the authority fork file")
	saleMigrationPath := flags.String("sale-migration", "", "path to the protocol sale migration artifact")
	lpodWitnessPath := flags.String("lpod-witness", "", "path to the LPoD migration witness")
	keyImageAnchorPath := flags.String("key-image-anchor", "", "optional operator-attested key-image export for an earlier snapshot height")
	bindRegistry := flags.Bool("bind-registry", false, "include the exact checkpoint registry digest for operator review and signature")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *snapshotPath == "" || *authorityForkPath == "" || *authorityForkSHA256 == "" ||
		*saleMigrationPath == "" || *lpodWitnessPath == "" {
		return errors.New("inspect requires --snapshot, authority-fork, sale-migration and lpod-witness files")
	}
	digest, err := mainnetbootstrap.SnapshotTreeSHA256(*snapshotPath)
	if err != nil {
		return err
	}
	fork, err := mainnetbootstrap.LoadPinnedAuthorityFork(*authorityForkPath, *authorityForkSHA256)
	if err != nil {
		return err
	}
	saleDigest, err := mainnetbootstrap.FileSHA256(*saleMigrationPath)
	if err != nil {
		return err
	}
	lpodDigest, err := mainnetbootstrap.FileSHA256(*lpodWitnessPath)
	if err != nil {
		return err
	}
	statement, err := mainnetbootstrap.InspectSnapshotDB(
		*snapshotPath, digest, fork, *authorityForkSHA256, saleDigest, lpodDigest,
	)
	if err != nil {
		return err
	}
	if *bindRegistry {
		statement.RegistrySHA256, err = mainnetbootstrap.InspectRegistryEvidence(*snapshotPath, statement)
		if err != nil {
			return err
		}
	}
	if *keyImageAnchorPath != "" {
		statement.KeyImageAnchorSHA256, statement.KeyImageAnchorBlockHash,
			statement.KeyImageAnchorHeight, err = mainnetbootstrap.InspectKeyImageAnchor(
			*snapshotPath, *keyImageAnchorPath, statement.CheckpointHeight,
		)
		if err != nil {
			return err
		}
	}
	encoded, err := json.MarshalIndent(statement, "", "  ")
	if err != nil {
		return fmt.Errorf("encode inspected statement: %w", err)
	}
	_, err = os.Stdout.Write(append(encoded, '\n'))
	return err
}

func runSign(args []string) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := flags.String("key", "", "path to the protected raw validator key")
	statementPath := flags.String("statement", "", "path to the unsigned statement JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || *statementPath == "" {
		return errors.New("sign requires --key and --statement")
	}

	var statementReader io.Reader
	var statementFile *os.File
	var err error
	if *statementPath == "-" {
		statementReader = os.Stdin
	} else {
		statementFile, err = os.Open(*statementPath)
		if err != nil {
			return fmt.Errorf("open unsigned statement: %w", err)
		}
		statementReader = statementFile
	}
	decoder := json.NewDecoder(statementReader)
	decoder.DisallowUnknownFields()
	var statement mainnetbootstrap.Statement
	decodeErr := decoder.Decode(&statement)
	var extra any
	if decodeErr == nil {
		if trailingErr := decoder.Decode(&extra); trailingErr != io.EOF {
			if trailingErr == nil {
				decodeErr = errors.New("trailing JSON value")
			} else {
				decodeErr = fmt.Errorf("trailing data: %w", trailingErr)
			}
		}
	}
	var closeErr error
	if statementFile != nil {
		closeErr = statementFile.Close()
	}
	if decodeErr != nil {
		return fmt.Errorf("read unsigned statement: %w", decodeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close unsigned statement: %w", closeErr)
	}

	key, err := loadOperatorKey(*keyPath)
	if err != nil {
		return err
	}
	defer key.Destroy()

	signed, err := mainnetbootstrap.SignOperator(statement, key)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(signed, "", "  ")
	if err != nil {
		return fmt.Errorf("encode signed statement: %w", err)
	}
	_, err = os.Stdout.Write(append(encoded, '\n'))
	return err
}

func loadOperatorKey(path string) (*crypto.LockedValidatorKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect operator key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0600 {
		return nil, errors.New("operator key must be a regular, non-symlink file with mode 0600")
	}
	privateBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read operator key: %w", err)
	}
	key, keyErr := crypto.NewLockedValidatorKey(privateBytes, nil)
	crypto.ZeroBytes(privateBytes)
	if keyErr != nil {
		return nil, fmt.Errorf("load operator key: %w", keyErr)
	}
	return key, nil
}

func runDigest(args []string) error {
	flags := flag.NewFlagSet("digest", flag.ContinueOnError)
	snapshotPath := flags.String("snapshot", "", "path to the closed chain.db directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *snapshotPath == "" {
		return errors.New("digest requires --snapshot")
	}
	digest, err := mainnetbootstrap.SnapshotTreeSHA256(*snapshotPath)
	if err != nil {
		return err
	}
	fmt.Printf("{\"snapshot_tree_sha256\":%q}\n", digest)
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	manifestPath := flags.String("manifest", "", "path to the signed manifest")
	snapshotPath := flags.String("snapshot", "", "path to the closed chain.db directory")
	authorityForkPath := flags.String("authority-fork", "", "path to the pinned validator authority fork")
	authorityForkSHA256 := flags.String("authority-fork-sha256", "", "approved SHA-256 of the authority fork file")
	saleMigrationPath := flags.String("sale-migration", "", "path to the protocol sale migration artifact")
	lpodWitnessPath := flags.String("lpod-witness", "", "path to the LPoD migration witness")
	keyImageAnchorPath := flags.String("key-image-anchor", "", "signed earlier key-image anchor file, when present in the manifest")
	avmCheckpointPath := flags.String("avm-checkpoint", "", "path to the signed AVM checkpoint for this exact bootstrap snapshot")
	avmActivationHeight := flags.Uint64("avm-activation-height", 0, "AVM activation height from the verified consensus profile")
	requireRegistry := flags.Bool("require-registry", false, "require an authenticated checkpoint registry")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if (*avmCheckpointPath == "") != (*avmActivationHeight == 0) {
		return errors.New("--avm-checkpoint and a nonzero --avm-activation-height must be supplied together")
	}
	if *manifestPath == "" || *snapshotPath == "" || *authorityForkPath == "" ||
		*authorityForkSHA256 == "" || *saleMigrationPath == "" || *lpodWitnessPath == "" {
		return errors.New("verify requires signed manifest, snapshot and all three migration artifacts")
	}
	file, err := os.Open(*manifestPath)
	if err != nil {
		return fmt.Errorf("open signed manifest: %w", err)
	}
	signed, decodeErr := mainnetbootstrap.ReadStrict(file)
	closeErr := file.Close()
	if decodeErr != nil {
		return decodeErr
	}
	if closeErr != nil {
		return fmt.Errorf("close signed manifest: %w", closeErr)
	}
	if err := signed.Verify(); err != nil {
		return err
	}
	if *requireRegistry && signed.Statement.RegistrySHA256 == "" {
		return errors.New("signed bootstrap manifest does not bind the checkpoint registry")
	}
	if *avmCheckpointPath != "" {
		checkpoint, present, err := avm.ReadCheckpointFile(*avmCheckpointPath)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("signed AVM checkpoint not found")
		}
		if err := verifyBootstrapAVM(signed.Statement, checkpoint, *avmActivationHeight, mainnetbootstrap.BootstrapSignerPublicKeyHex); err != nil {
			return err
		}
	}
	fork, err := mainnetbootstrap.LoadPinnedAuthorityFork(*authorityForkPath, *authorityForkSHA256)
	if err != nil {
		return err
	}
	saleDigest, err := mainnetbootstrap.FileSHA256(*saleMigrationPath)
	if err != nil {
		return err
	}
	lpodDigest, err := mainnetbootstrap.FileSHA256(*lpodWitnessPath)
	if err != nil {
		return err
	}
	digest, err := mainnetbootstrap.SnapshotTreeSHA256(*snapshotPath)
	if err != nil {
		return err
	}
	if digest != signed.Statement.SnapshotTreeSHA256 {
		return errors.New("snapshot tree SHA-256 does not match the signed manifest")
	}
	if err := mainnetbootstrap.VerifySnapshotDBWithKeyImageAnchor(
		*snapshotPath, signed.Statement, fork, *authorityForkSHA256,
		saleDigest, lpodDigest, *keyImageAnchorPath,
	); err != nil {
		return err
	}
	fmt.Printf("{\"success\":true,\"network\":%q,\"chain_id\":%q,\"checkpoint_height\":%d,\"checkpoint_hash\":%q,\"checkpoint_signature_hash_kind\":%q,\"snapshot_tree_sha256\":%q,\"signer\":%q,\"avm_checkpoint_verified\":%t,\"registry_attestation_verified\":%t}\n",
		signed.Statement.Network, signed.Statement.ChainID,
		signed.Statement.CheckpointHeight, signed.Statement.CheckpointHeaderHash,
		signed.Statement.CheckpointSignatureHashKind, digest, signed.SignerPublicKey, *avmCheckpointPath != "", signed.Statement.RegistrySHA256 != "")
	return nil
}

func verifyBootstrapAVM(statement mainnetbootstrap.Statement, checkpoint *avm.AVMCheckpoint, activationHeight uint64, authority string) error {
	// AVM recovery uses canonical index hashes, not recomputed legacy headers.
	if checkpoint == nil || checkpoint.Height != statement.CheckpointHeight ||
		hex.EncodeToString(checkpoint.GenesisHash[:]) != statement.GenesisIndexHash ||
		hex.EncodeToString(checkpoint.BlockHash[:]) != statement.CheckpointIndexHash {
		return errors.New("AVM checkpoint does not match the signed bootstrap genesis and tip")
	}
	authorities, err := avm.ParseCheckpointAuthorities([]string{authority})
	if err != nil {
		return err
	}
	return avm.VerifyCheckpointQuorum(checkpoint, activationHeight, authorities, 1)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mainnet-bootstrap-attest:", err)
	os.Exit(1)
}
