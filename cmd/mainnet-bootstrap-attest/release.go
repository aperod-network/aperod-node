package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func runRelease(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	root := flags.String("payload", "", "private, immutable unpacked payload directory")
	revision := flags.String("source-commit", "", "independently reviewed full Git commit ID")
	var input *string
	var authorityDigest *string
	var avmHeight *uint64
	if command == "release-sign" {
		input = flags.String("key", "", "protected operator key kept outside the payload")
	} else {
		input = flags.String("manifest", "", "signed manifest, outside the payload")
	}
	if command == "release-preflight" {
		authorityDigest = flags.String("authority-fork-sha256", "", "independently approved authority fork digest")
		avmHeight = flags.Uint64("avm-activation-height", 0, "approved AVM activation height")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *root == "" || *revision == "" || *input == "" || flags.NArg() != 0 {
		return errors.New("provide --payload, --source-commit and either --key or --manifest")
	}
	if command == "release-preflight" && (*authorityDigest == "" || *avmHeight == 0) {
		return errors.New("release-preflight requires --authority-fork-sha256 and --avm-activation-height")
	}
	if err := requireOutsidePayload(*root, *input); err != nil {
		return err
	}
	if command == "release-sign" {
		info, err := os.Stat(*input)
		if err != nil {
			return err
		}
		if err := filepath.WalkDir(*root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type().IsRegular() {
				file, err := entry.Info()
				if err != nil {
					return err
				}
				if os.SameFile(info, file) {
					return errors.New("operator key is linked inside the release payload")
				}
			}
			return nil
		}); err != nil {
			return err
		}
		r, err := mainnetbootstrap.InspectRelease(*root, *revision)
		if err != nil {
			return err
		}
		key, err := loadOperatorKey(*input)
		if err != nil {
			return err
		}
		defer key.Destroy()
		signed, err := mainnetbootstrap.SignRelease(r, key)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(signed)
	}
	file, err := os.Open(*input)
	if err != nil {
		return err
	}
	defer file.Close()
	dec := json.NewDecoder(io.LimitReader(file, 8193))
	dec.DisallowUnknownFields()
	var s mainnetbootstrap.SignedRelease
	if err := dec.Decode(&s); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data in release manifest")
	}
	if err := s.Verify(*root, *revision); err != nil {
		return err
	}
	if command == "release-preflight" {
		if err := checkReleaseWitnessProfile(*root, *authorityDigest, *avmHeight); err != nil {
			return err
		}
		if err := checkReleaseFinalitySchedule(*root); err != nil {
			return err
		}
		if err := checkReleaseStartupSnapshot(*root); err != nil {
			return err
		}
		return runVerify(append(bundleBootstrapArgs(*root, *authorityDigest, *avmHeight), "--require-registry"))
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"success": true, "source_commit": s.Release.SourceCommit})
}

// Called only after release verification has rejected unexpected files,
// multiple snapshots and unmatched names. The node expects a raw hex
// checksum, not the filename-bearing format emitted by sha256sum.
func checkReleaseStartupSnapshot(root string) error {
	state := filepath.Join(root, releaseState)
	entries, err := os.ReadDir(state)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "snapshot-v2-") || !strings.HasSuffix(name, ".json.gz.sha256") {
			continue
		}
		sidecar := filepath.Join(state, name)
		f, err := os.Open(sidecar)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(io.LimitReader(f, 129))
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		want := strings.TrimSpace(string(raw))
		if len(raw) > 128 || len(want) != 64 {
			return errors.New("release: invalid native startup snapshot SHA-256 sidecar")
		}
		digest, err := mainnetbootstrap.FileSHA256(strings.TrimSuffix(sidecar, ".sha256"))
		if err != nil {
			return err
		}
		if !strings.EqualFold(want, digest) {
			return errors.New("release: native startup snapshot SHA-256 mismatch")
		}
	}
	return nil
}

const (
	releaseBootstrap = "witnesses/bootstrap-signed.json"
	releaseAuthority = "witnesses/aperod-authority-fork.json"
	releaseSale      = "witnesses/aperod-protocol-sale-opening.json"
	releaseLPoD      = "witnesses/aperod-lpod-v3-witness.json"
	releaseAVM       = "witnesses/avm-signed.json"
	releaseState     = "state/mainnet"
)

// Verify the same inputs that the node will load from the payload root.
// Authenticating a witness in isolation would not detect a signed profile
// that points somewhere else or that schedules AVM at a different height.
func checkReleaseWitnessProfile(root, authorityDigest string, avmHeight uint64) error {
	cfg, err := config.Load(filepath.Join(root, "config", "node.yaml"))
	if err != nil {
		return err
	}
	c := cfg.Consensus
	for _, pin := range []struct{ field, got, want string }{
		{"data_dir", cfg.DataDir, releaseState},
		{"genesis.file", cfg.Genesis.File, "config/genesis.yaml"},
		{"mainnet_bootstrap_file", cfg.MainnetBootstrapFile, releaseBootstrap},
		{"validator_authority_fork_file", c.ValidatorAuthorityForkFile, releaseAuthority},
		{"validator_authority_fork_sha256", c.ValidatorAuthorityForkSHA256, authorityDigest},
		{"sale_migration_file", c.SaleMigrationFile, releaseSale},
		{"lpod_migration_file", c.LPoDMigrationFile, releaseLPoD},
		{"avm_checkpoint_file", filepath.ToSlash(cfg.AVMCheckpointPath()), releaseAVM},
	} {
		if pin.got != pin.want {
			return fmt.Errorf("release profile: %s must match the verified bundle input %q", pin.field, pin.want)
		}
	}
	if c.AVMActivationHeight != avmHeight {
		return fmt.Errorf("release profile: avm_activation_height must match the independently pinned height %d", avmHeight)
	}
	if c.AVMCheckpointWitnessThreshold != 1 || len(c.AVMCheckpointTrustAuthorities) != 1 ||
		c.AVMCheckpointTrustAuthorities[0] != mainnetbootstrap.BootstrapSignerPublicKeyHex {
		return errors.New("release profile: AVM must trust only the pinned bootstrap operator with threshold 1")
	}
	return nil
}

// This checks certificate compatibility, not signatures or committee quorum.
// Call only after authenticating the closed payload and its existing LOCK file.
func checkReleaseFinalitySchedule(root string) error {
	cfg, err := config.Load(filepath.Join(root, "config", "node.yaml"))
	if err != nil {
		return err
	}
	if cfg.Consensus.SecurityHardeningActivationHeight != config.MainnetSecurityHardeningActivationHeight {
		return fmt.Errorf("release finality: configuration must specify the validator's coordinated security hardening height %d", config.MainnetSecurityHardeningActivationHeight)
	}
	activation := cfg.FinalityVoteV2ActivationHeight()
	db, err := store.OpenReadOnly(filepath.Join(root, "state", "mainnet", "chain.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	cert, err := db.LoadFinalityCertificate()
	if err != nil {
		return err
	}
	if cert == nil || cert.Height == 0 || len(cert.Votes) == 0 {
		return errors.New("release finality: snapshot does not contain a usable certificate")
	}
	_, tip, err := db.GetTip()
	if err != nil {
		return err
	}
	hash, found, err := db.GetCanonicalHash(cert.Height)
	if err != nil {
		return err
	}
	if cert.Height > tip || !found || hash != cert.BlockHash {
		return errors.New("release finality: certificate is not bound to the canonical snapshot")
	}
	if cfg.Consensus.FinalityMigrationFile != "" {
		migration, err := mainnetbootstrap.ReadReleaseHistoricalCommittee(root)
		if err != nil {
			return err
		}
		historicalHash, found, err := db.GetCanonicalHash(migration.CertificateHeight)
		if err != nil || !found || historicalHash != migration.CertificateBlockHash ||
			cert.Height < migration.CertificateHeight {
			return errors.New("release finality: historical committee statement is not bound to the snapshot")
		}
	}
	expected := crypto.FinalityVoteVersionAt(cert.Height, activation)
	for _, vote := range cert.Votes {
		version := vote.VoteVersion
		if version == 0 {
			version = crypto.FinalityVoteVersionLegacy
		}
		if version != expected {
			return fmt.Errorf("release finality: certificate at height %d uses vote v%d; validator profile expects v%d", cert.Height, version, expected)
		}
	}
	return checkReleaseVoteJournal(db, tip, activation)
}

// Mirror the startup vote-version checks without modifying the closed snapshot.
// A compatible certificate alone does not make the persisted votes compatible.
func checkReleaseVoteJournal(db *store.DB, tip, activation uint64) error {
	check := func(kind string, height uint64, version uint8) error {
		if version == 0 {
			version = crypto.FinalityVoteVersionLegacy
		}
		expected := crypto.FinalityVoteVersionAt(height, activation)
		if height > tip || version != expected {
			return fmt.Errorf("release finality: %s at height %d uses vote v%d; validator profile expects v%d and a height at or below tip %d", kind, height, version, expected, tip)
		}
		return nil
	}
	local, err := db.LoadLocalFinalityVote()
	if err != nil {
		return fmt.Errorf("release finality: load local vote: %w", err)
	}
	if local != nil {
		if err := check("local vote", local.Height, local.VoteVersion); err != nil {
			return err
		}
	}
	minHeight := tip - min(tip, store.FinalityVoteJournalDepth-1)
	votes, err := db.LoadFinalityVotes(minHeight, tip)
	if err != nil {
		return fmt.Errorf("release finality: load vote journal: %w", err)
	}
	for _, vote := range votes {
		epoch := vote.Height / core.EpochLength
		if epoch != tip/core.EpochLength {
			committee, err := db.LoadFinalityCommitteeSnapshot(epoch)
			if err != nil {
				return fmt.Errorf("release finality: load journal committee for epoch %d: %w", epoch, err)
			}
			if committee == nil {
				continue // Startup does not replay old journals without a committee.
			}
		}
		if err := check("persisted vote", vote.Height, vote.VoteVersion); err != nil {
			return err
		}
	}
	return nil
}

func bundleBootstrapArgs(root, authorityDigest string, avmHeight uint64) []string {
	artifact := func(path string) string { return filepath.Join(root, filepath.FromSlash(path)) }
	return []string{
		"--manifest", artifact(releaseBootstrap),
		"--snapshot", filepath.Join(root, releaseState, "chain.db"),
		"--authority-fork", artifact(releaseAuthority),
		"--authority-fork-sha256", authorityDigest,
		"--sale-migration", artifact(releaseSale),
		"--lpod-witness", artifact(releaseLPoD),
		"--avm-checkpoint", artifact(releaseAVM),
		"--avm-activation-height", strconv.FormatUint(avmHeight, 10),
	}
}

func requireOutsidePayload(root, input string) error {
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	input, err = filepath.EvalSymlinks(input)
	if err != nil {
		return err
	}
	input, err = filepath.Abs(input)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, input)
	if err != nil {
		return err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("the key or manifest must be outside the release payload")
	}
	return nil
}
