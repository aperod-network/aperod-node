package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/mainnetbootstrap"
	"github.com/aperod/aperod/store"
)

// Diagnostic only: report authenticated vote format separately from committee
// evidence. A verified individual signature is not by itself finality.
func runFinality(args []string) error {
	flags := flag.NewFlagSet("finality", flag.ContinueOnError)
	snapshot := flags.String("snapshot", "", "path to a closed, isolated chain.db copy (never the live database)")
	forkFile := flags.String("authority-fork", "", "path to the pinned validator authority fork")
	forkDigest := flags.String("authority-fork-sha256", "", "expected SHA-256 of the validator authority fork")
	genesisHeader := flags.String("genesis-header-hash", "", "independently pinned genesis header hash")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *snapshot == "" || *forkFile == "" || *forkDigest == "" || *genesisHeader == "" {
		return errors.New("finality requires a closed snapshot, pinned authority fork and pinned genesis header hash")
	}
	path, err := filepath.Abs(*snapshot)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path || filepath.Base(path) != "chain.db" {
		return errors.New("finality requires a real chain.db directory without symlinks")
	}
	manifestPath := filepath.Join(filepath.Dir(path), "manifest.json")
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Size() > 4096 {
		return errors.New("completed checkpoint manifest is required; do not open the live database")
	}
	rawManifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest struct {
		Success bool   `json:"success"`
		Height  uint64 `json:"tip_height"`
		Hash    string `json:"tip_hash"`
	}
	if err := json.Unmarshal(rawManifest, &manifest); err != nil || !manifest.Success || manifest.Height == 0 {
		return errors.New("invalid completed checkpoint manifest")
	}
	fork, err := mainnetbootstrap.LoadPinnedAuthorityFork(*forkFile, *forkDigest)
	if err != nil {
		return err
	}
	db, err := store.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	readBlock := func(height uint64) (crypto.Hash32, core.Block, error) {
		hash, ok, err := db.GetCanonicalHash(height)
		var block core.Block
		if err != nil || !ok {
			return hash, block, fmt.Errorf("canonical index unavailable at height %d", height)
		}
		raw, err := db.GetRawBlock(hash)
		if err != nil {
			return hash, block, err
		}
		if err := json.Unmarshal(raw, &block); err != nil || block.Header.Height != height {
			return hash, block, fmt.Errorf("canonical block unavailable at height %d", height)
		}
		return hash, block, nil
	}
	genesisHash, genesis, err := readBlock(0)
	if err != nil {
		return err
	}
	if genesisHash != fork.Genesis || fmt.Sprintf("%x", genesis.Hash()) != *genesisHeader ||
		genesis.Header.ValidatorPub.Hex() != mainnetbootstrap.GenesisSignerPublicKeyHex ||
		!(genesis.Header.VerifySignature() || genesis.Header.ValidatorPub.Verify(genesisHash, genesis.Header.Signature)) {
		return errors.New("genesis does not match the independently pinned Mainnet identity")
	}
	tip, tipHeight, err := db.GetTip()
	if err != nil {
		return err
	}
	if tipHeight != manifest.Height || fmt.Sprintf("%x", tip) != manifest.Hash {
		return errors.New("stored tip does not match completed checkpoint manifest")
	}
	indexedTip, ok, err := db.GetCanonicalHash(tipHeight)
	if err != nil || !ok || indexedTip != tip {
		return errors.New("stored tip does not match canonical index")
	}
	cert, err := db.LoadFinalityCertificate()
	if err != nil {
		return err
	}
	if cert == nil || cert.Height == 0 || cert.Height > tipHeight {
		return errors.New("no usable persisted certificate")
	}
	for _, transition := range fork.Transitions() {
		if transition.Height <= tipHeight {
			parent, ok, err := db.GetCanonicalHash(transition.Height - 1)
			if err != nil || !ok || parent != transition.Parent {
				return errors.New("authority transition parent does not match canonical index")
			}
		}
	}
	hash, block, err := readBlock(cert.Height)
	if err != nil {
		return err
	}
	if hash != cert.BlockHash || block.Hash() != hash ||
		block.Header.MerkleRoot != core.MerkleRoot(block.Txs) || fork.CheckParent(block.Header) != nil ||
		!block.Header.VerifySignature(fork.Authority(block.Header.ValidatorPub, cert.Height)) {
		return errors.New("certificate does not bind a verified canonical block")
	}
	version, err := verifySingleOperatorVote(cert, fork)
	if err != nil {
		return err
	}
	committee, err := db.LoadFinalityCommitteeSnapshot(cert.Height / core.EpochLength)
	if err != nil {
		return err
	}
	status := "not_established_missing_committee"
	if committee != nil {
		if len(committee.Validators) != 1 || !committee.Validators[0].Equals(cert.Votes[0].Validator) ||
			committee.BFTThresholdPPM == 0 || committee.BFTThresholdPPM >= 1_000_000 {
			return errors.New("stored committee does not corroborate the single operator")
		}
		status = "verified_against_stored_committee"
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"snapshot_tip_height": tipHeight, "certificate_height": cert.Height,
		"certificate_block_hash": fmt.Sprintf("%x", hash), "vote_version": version,
		"signature_verified": true, "quorum_status": status, "history_replayed": false,
	})
}

func verifySingleOperatorVote(cert *store.FinalityCertificate, fork *core.ValidatorAuthorityFork) (uint8, error) {
	if cert == nil || cert.Version != 1 || len(cert.Votes) != 1 ||
		cert.Votes[0].Validator.Hex() != mainnetbootstrap.GenesisSignerPublicKeyHex {
		return 0, errors.New("expected exactly one vote from the pinned operator identity")
	}
	vote := cert.Votes[0]
	version := vote.VoteVersion
	if version == 0 {
		version = crypto.FinalityVoteVersionLegacy
	}
	if version != crypto.FinalityVoteVersionLegacy && version != crypto.FinalityVoteVersionHeightBound {
		return 0, errors.New("unsupported finality vote version")
	}
	msg := crypto.FinalityVoteMessage(version, cert.Height, cert.BlockHash)
	if !fork.Authority(vote.Validator, cert.Height).Verify(msg, vote.Signature) {
		return 0, errors.New("invalid finality vote signature")
	}
	return version, nil
}
