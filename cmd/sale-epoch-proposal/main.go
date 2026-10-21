// Prepare an explicit nominal epoch from a CLOSED canonical database.
// It does not start consensus, mutate allocation state, or admit payments.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/aperod/aperod/config"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
)

func run() error {
	dbPath := flag.String("db", "", "closed canonical chain.db path")
	cfgPath := flag.String("config", "", "node config; required only for signing")
	out := flag.String("out", "", "new configuration file, never overwritten")
	burn := flag.String("burned-napro", "", "explicit operator-attested recorded burn baseline")
	sign := flag.Bool("sign", false, "sign with the current producer after authorized checks")
	flag.Parse()
	if *dbPath == "" || *out == "" || *burn == "" {
		return fmt.Errorf("db, out and exact burned-napro required")
	}
	burned, err := strconv.ParseUint(*burn, 10, 64)
	if err != nil || strconv.FormatUint(burned, 10) != *burn {
		return fmt.Errorf("invalid exact recorded burn")
	}
	d, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("closed database required: %w", err)
	}
	defer d.Close()
	active, err := d.LoadSaleState()
	if err != nil || active != nil {
		return fmt.Errorf("existing sale ledger must not be reopened as a fresh budget")
	}
	parent, height, err := d.GetTip()
	if err != nil || height == ^uint64(0) {
		return fmt.Errorf("canonical tip unavailable")
	}
	raw, err := d.GetRawBlockByHeight(0)
	if err != nil {
		return err
	}
	var genesis core.Block
	if json.Unmarshal(raw, &genesis) != nil {
		return fmt.Errorf("canonical genesis unavailable")
	}
	raw, err = d.GetRawBlockByHeight(height)
	if err != nil {
		return err
	}
	var head core.Block
	if json.Unmarshal(raw, &head) != nil || head.Hash() != parent {
		return fmt.Errorf("canonical parent body mismatch")
	}
	l, err := d.LoadLPoDCheckpoint()
	if err != nil || l == nil || l.Allocation == nil {
		return fmt.Errorf("canonical LPoD checkpoint unavailable")
	}
	e := &sale.AllocationEpochBasis{FundingHeight: l.Allocation.FundingHeight, FundingHash: l.Allocation.FundingBlock,
		Burned: burned, ProtectedLPoDPrincipal: l.PrincipalLocked, LegacyHoldingsUnverified: true}
	r := sale.Reconciliation{AllocationEpoch: e, Genesis: genesis.Hash(), Parent: parent,
		LPoDCommitment: l.Digest(), OtherClaimsRoot: crypto.HashBytes([]byte("aperod/sale/no-additional-obligations/v1")),
		HistoricIssued: burned, ValidatorReserved: l.Allocation.ValidatorRemaining,
		GuardianReserved: l.State.Balance, LPoDReserved: l.Allocation.Remaining}
	left := sale.CapNAPRO
	for _, n := range []uint64{burned, r.ValidatorReserved, r.GuardianReserved, r.LPoDReserved} {
		if n > left {
			return fmt.Errorf("nominal reserves exceed allocation")
		}
		left -= n
	}
	e.Available = left
	if err := store.VerifySaleReserveFloors(l, r); err != nil {
		return err
	}
	_, reserved, err := r.Unassigned()
	if err != nil {
		return err
	}
	cp := sale.Checkpoint{Genesis: genesis.Hash(), Network: crypto.MainnetByte, Parent: parent, Height: height + 1,
		SnapshotRoot: r.Digest(), HistoricIssued: burned, OtherReserved: reserved,
		SaleRemaining: left, SaleAuthority: head.Header.ValidatorPub}
	m := store.SaleMigration{Checkpoint: cp, Reconciliation: r}
	if *sign {
		if *cfgPath == "" {
			return fmt.Errorf("explicit node config required for signing")
		}
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}
		authority, err := signingAuthority(cfg.Consensus, d)
		if err != nil {
			return err
		}
		certificate, err := d.LoadFinalityCertificate()
		if err != nil || certificate == nil || certificate.Height != height || certificate.BlockHash != parent {
			return fmt.Errorf("exact canonical parent finality certificate required before signing")
		}
		producerVote := false
		message := crypto.HashBytes([]byte("aperod/finalize/v1"), parent[:])
		for _, vote := range certificate.Votes {
			if vote.Validator.Equals(head.Header.ValidatorPub) && authority.Authority(vote.Validator, height).Verify(message, vote.Signature) {
				producerVote = true
			}
		}
		if !producerVote {
			return fmt.Errorf("canonical producer finality vote required before signing")
		}
		info, err := os.Stat(cfg.Consensus.ValidatorKey)
		if err != nil || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("protected producer key file required")
		}
		private, err := os.ReadFile(cfg.Consensus.ValidatorKey)
		if err != nil {
			return fmt.Errorf("cannot read configured signer")
		}
		key, err := crypto.NewLockedValidatorKey(private, nil)
		crypto.ZeroBytes(private)
		if err != nil {
			return fmt.Errorf("invalid configured signer")
		}
		defer key.Destroy()
		if !key.Public().Equals(authority.Authority(head.Header.ValidatorPub, height+1)) {
			return fmt.Errorf("signer is not the canonical parent producer")
		}
		// Sale transactions are signed by the active key, not the retained stake ID.
		cp.SaleAuthority = key.Public()
		m.Checkpoint.SaleAuthority = key.Public()
		signature, err := key.Sign(cp.Message())
		if err != nil {
			return err
		}
		m.Trusted = []crypto.ValidatorPubKey{key.Public()}
		m.Checkpoint.Attestations = []sale.Attestation{{Validator: key.Public(), Signature: signature}}
		known, err := store.SaleKnownIssuedFloor(l)
		if err != nil {
			return err
		}
		if err := sale.VerifyReconciledCheckpoint(m.Checkpoint, r, genesis.Hash(), parent, l.Digest(), height+1, known, m.Trusted); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(&m); err != nil {
		f.Close()
		os.Remove(*out)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(*out)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	checkpointHash := cp.Message()
	fmt.Printf("epoch_proposal height=%d available_napro=%d checkpoint=%x historical_supply_verified=false signed=%t\n", height+1, left, checkpointHash[:], *sign)
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
