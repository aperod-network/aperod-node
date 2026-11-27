package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

func TestReleasePreflightChecksNativeSnapshotChecksum(t *testing.T) {
	root := filepath.Join(t.TempDir(), "payload[verified]")
	dir := filepath.Join(root, releaseState)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "snapshot-v2-42.json.gz")
	if err := os.WriteFile(snapshot, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := mainnetbootstrap.FileSHA256(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		sidecar string
		ok      bool
	}{
		{digest + "\n", true},
		{strings.Repeat("a", 64) + "\n", false},
		{digest + "  snapshot-v2-42.json.gz\n", false},
	} {
		if err := os.WriteFile(snapshot+".sha256", []byte(tc.sidecar), 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkReleaseStartupSnapshot(root); (err == nil) != tc.ok {
			t.Fatalf("check checksum %q: %v", tc.sidecar, err)
		}
	}
}

func TestReleasePreflightRequiresBothConsensusPins(t *testing.T) {
	err := runRelease("release-preflight", []string{"--payload", "payload", "--source-commit", strings.Repeat("a", 40), "--manifest", "release.json"})
	if err == nil || !strings.Contains(err.Error(), "requires --authority-fork-sha256 and --avm-activation-height") {
		t.Fatalf("missing consensus pins: %v", err)
	}
}

func TestReleaseWitnessProfileUsesPinnedInputs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "config")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("APEROD_DATA_DIR", "")
	digest := strings.Repeat("a", 64)
	profile := fmt.Sprintf(`network: mainnet
data_dir: state/mainnet
genesis: {file: config/genesis.yaml}
mainnet_bootstrap_file: %s
consensus:
  validator_authority_fork_file: %s
  validator_authority_fork_sha256: %s
  sale_migration_file: %s
  lpod_migration_file: %s
  avm_checkpoint_file: %s
  avm_activation_height: 1850000
  avm_checkpoint_trust_authorities: [%q]
  avm_checkpoint_witness_threshold: 1
`, releaseBootstrap, releaseAuthority, digest, releaseSale, releaseLPoD, "../../"+releaseAVM, mainnetbootstrap.BootstrapSignerPublicKeyHex)
	for _, tc := range []struct{ old, replacement, want string }{
		{"", "", ""},
		{releaseBootstrap, "/etc/bootstrap.json", "mainnet_bootstrap_file"},
		{"../../" + releaseAVM, releaseAVM, "avm_checkpoint_file"},
		{"../../" + releaseAVM, "../../witnesses/other.json", "avm_checkpoint_file"},
		{"1850000", "1850001", "avm_activation_height"},
		{digest, strings.Repeat("b", 64), "validator_authority_fork_sha256"},
		{"threshold: 1", "threshold: 2", "AVM must trust"},
		{mainnetbootstrap.BootstrapSignerPublicKeyHex, strings.Repeat("c", 64), "AVM must trust"},
	} {
		text := profile
		if tc.old != "" {
			text = strings.Replace(text, tc.old, tc.replacement, 1)
		}
		if err := os.WriteFile(filepath.Join(dir, "node.yaml"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		err := checkReleaseWitnessProfile(root, digest, 1_850_000)
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Fatalf("got %v, want %q", err, tc.want)
		}
	}
}

func TestCheckReleaseFinalityScheduleOnClosedSnapshot(t *testing.T) {
	activation := config.MainnetFinalityVoteV2ActivationHeight
	type finalityScheduleCase struct {
		name    string
		height  uint64
		version uint8
		errText string
	}
	var cases []finalityScheduleCase
	if activation == 0 {
		height := config.MainnetSecurityHardeningActivationHeight + 1
		cases = []finalityScheduleCase{
			{"legacy after hardening", height, 1, ""},
			{"premature v2 after hardening", height, 2, "uses vote v2; validator profile expects v1"},
		}
	} else {
		cases = []finalityScheduleCase{
			{"legacy before activation", activation - 1, 0, ""},
			{"legacy at activation", activation, 1, "uses vote v1; validator profile expects v2"},
			{"height-bound at activation", activation, 2, ""},
			{"height-bound before activation", activation - 1, 2, "uses vote v2; validator profile expects v1"},
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, "config"), 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "config", "node.yaml")
			profile := fmt.Sprintf("network: mainnet\nconsensus:\n  security_hardening_activation_height: %d\n", config.MainnetSecurityHardeningActivationHeight)
			if err := os.WriteFile(path, []byte(profile), 0600); err != nil {
				t.Fatal(err)
			}
			db, err := store.Open(filepath.Join(root, "state", "mainnet", "chain.db"))
			if err != nil {
				t.Fatal(err)
			}
			hash := crypto.HashBytes([]byte("canonical block"))
			writeErr := db.RepairHeightIndex(tc.height, hash)
			if writeErr == nil {
				writeErr = db.PutTip(hash, tc.height)
			}
			if writeErr == nil {
				writeErr = db.SaveFinalityCertificate(store.FinalityCertificate{
					Version: 1, Height: tc.height, BlockHash: hash,
					Votes: []store.FinalityVote{{VoteVersion: tc.version}},
				})
			}
			closeErr := db.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("write fixture: %v; close: %v", writeErr, closeErr)
			}
			err = checkReleaseFinalitySchedule(root)
			if tc.errText == "" && err != nil || tc.errText != "" && (err == nil || !strings.Contains(err.Error(), tc.errText)) {
				t.Fatalf("got %v; want %q", err, tc.errText)
			}
		})
	}
}

func TestReleasePreflightRejectsUnauthenticatedPayloadBeforeBootstrap(t *testing.T) {
	root := t.TempDir()
	payload := filepath.Join(root, "payload")
	if err := os.Mkdir(payload, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "release.json")
	commit := strings.Repeat("a", 40)
	text := `{"release":{"schema":"aperod-mainnet-release-v1","chain_id":"aperod-mainnet-1","source_commit":"` +
		commit + `","payload_tree_sha256":"` + strings.Repeat("b", 64) + `"},"signature":"` + strings.Repeat("0", 128) + `"}`
	if err := os.WriteFile(manifest, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	err := runRelease("release-preflight", []string{
		"--payload", payload, "--source-commit", commit, "--manifest", manifest,
		"--authority-fork-sha256", strings.Repeat("c", 64), "--avm-activation-height", "42",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid Mainnet operator release signature") {
		t.Fatalf("unauthenticated payload was not rejected first: %v", err)
	}
}

func TestCheckReleaseVoteJournal(t *testing.T) {
	const tip = config.MainnetSecurityHardeningActivationHeight + 500
	const activation = config.MainnetSecurityHardeningActivationHeight
	const priorEpochHeight = tip/core.EpochLength*core.EpochLength - 1
	if tip-priorEpochHeight >= store.FinalityVoteJournalDepth {
		t.Fatal("test must cross an epoch within the journal window")
	}
	_, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name             string
		height           uint64
		version          uint8
		local, committee bool
		want             string
	}{
		{"legacy local", tip, 0, true, false, "local vote"},
		{"future local", tip + 1, 2, true, false, "local vote"},
		{"current legacy journal", tip, 1, false, false, "persisted vote"},
		{"current v2 journal", tip, 2, false, false, ""},
		{"old journal without committee", priorEpochHeight, 1, false, false, ""},
		{"old journal with committee", priorEpochHeight, 1, false, true, "persisted vote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			hash := crypto.HashBytes([]byte("test block"))
			sig := make([]byte, 64)
			if tc.local {
				err = db.SaveLocalFinalityVote(store.LocalFinalityVote{Version: 1, Height: tc.height, BlockHash: hash, Validator: pub, Signature: sig, VoteVersion: tc.version})
			} else {
				err = db.SaveFinalityVote(store.PersistedFinalityVote{Version: 1, Height: tc.height, BlockHash: hash, Validator: pub, Signature: sig, VoteVersion: tc.version})
			}
			if err == nil && tc.committee {
				err = db.SaveFinalityCommitteeSnapshot(store.FinalityCommitteeSnapshot{Version: 1, Epoch: tc.height / core.EpochLength, Validators: []crypto.ValidatorPubKey{pub}, BFTThresholdPPM: 667_000})
			}
			if closeErr := db.Close(); err != nil || closeErr != nil {
				t.Fatalf("write fixture: %v; close: %v", err, closeErr)
			}
			db, err = store.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			err = checkReleaseVoteJournal(db, tip, activation)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("got %v; want %q", err, tc.want)
			}
		})
	}
}
