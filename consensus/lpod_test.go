// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

func lpodConsensusFixture(t *testing.T) (*Engine, *store.DB, *core.Block, crypto.ValidatorPrivKey) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Destroy)
	wallet, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	g := &core.Block{Header: core.BlockHeader{Height: 0, ValidatorPub: pub, Timestamp: 1, MerkleRoot: core.MerkleRoot(nil)}}
	if err := g.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(g); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(g)
	if err := db.CommitRawBlockWithAVM(g.Hash(), 0, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	parent := &core.Block{Header: core.BlockHeader{Height: 1, Round: 1, PrevHash: g.Hash(), ValidatorPub: pub, Timestamp: 2, MerkleRoot: core.MerkleRoot(nil)}}
	parent.Header.Sign(priv)
	if err := chain.AddBlock(parent); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(parent)
	if err := db.CommitRawBlockWithAVM(parent.Hash(), 1, raw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	m := &store.LPoDMigration{Version: 1, Height: 2, Genesis: g.Hash()}
	m.BodyRoot = store.LPoDBodyRootStep(store.LPoDBodyRootStep(crypto.HashBytes([]byte("aperod/lpod/historical-bodies/v1")), g), parent)
	m.ValidatorRemaining = 2_000_000_000*lpod.Unit - 3*lpod.Unit
	m.TrustedValidators = []crypto.ValidatorPubKey{pub}
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []store.LPoDAttestation{{Validator: pub, Signature: sig}}
	if err := db.StoreStakingPoolRemaining(2_000_000_000*lpod.Unit - 3*lpod.Unit); err != nil {
		t.Fatal(err)
	}
	registry := core.NewValidatorRegistry()
	// Genesis-authorized registry fixture. Production rejects observed-producer
	// sentinels and never constructs membership from web sessions.
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, 100_000*lpod.Unit)
	e := NewEngine(Config{MyKey: key, Validators: []crypto.ValidatorPubKey{pub}, Registry: registry,
		Store: db, LPoDMigration: m, StakingPoolNAPR: 2_000_000_000 * lpod.Unit,
		RewardAuthorizationActivationHeight: 2, RewardAddress: string(crypto.AddressFromKeys(crypto.MainnetByte, wallet)),
		BFTThreshold: 0.667,
		OnCanonicalBlock: func(b *core.Block, p *avm.PreparedBlock) error {
			raw, err := json.Marshal(b)
			if err != nil {
				return err
			}
			return avm.CommitCanonicalBlock(db, b, raw, p)
		},
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e, db, parent, priv
}

func TestLPoDProducerCommitsRealAllocationAndSplit(t *testing.T) {
	e, db, _, _ := lpodConsensusFixture(t)
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	b := e.chain.Tip()
	if b.Header.Height != 2 || len(b.Txs) != 2 || !b.Txs[1].IsLPoD() {
		t.Fatal("missing real checkpoint")
	}
	auth, err := b.Txs[0].LPoDPayoutAuthorization()
	if err != nil {
		t.Fatal(err)
	}
	out, err := core.BuildLPoDPayoutOutput(crypto.Address(e.cfg.RewardAddress), 24_000_000, 2, b.Header.PrevHash, auth.Digest)
	if err != nil || len(b.Txs[0].Outputs) != 1 || b.Txs[0].Outputs[0] != out {
		t.Fatalf("leader received full unsplit reward: %v", err)
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil || c == nil || c.State.Balance != lpod.InitialNAPRO+276_000_000 {
		t.Fatalf("unfunded pool: %+v %v", c, err)
	}
	if !e.IsFinalizedHash(2, b.Hash()) || e.IsFinalizedHash(2, crypto.HashBytes([]byte("other"))) {
		t.Fatal("finality not hash-bound")
	}
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	c, err = db.LoadLPoDCheckpoint()
	if err != nil || c.State.FundingDebit != lpod.InitialNAPRO || c.State.RewardInflow != 6*lpod.Unit {
		t.Fatal("duplicate allocation or lost income")
	}
}

func TestLPoDConsensusRejectsMutatedTransitions(t *testing.T) {
	e, _, parent, priv := lpodConsensusFixture(t)
	valid, err := e.produceBlock(2, 2, parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"early", "late", "missing", "duplicate", "digest", "extra_mint", "full_reward", "genesis"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(valid)
			var b core.Block
			json.Unmarshal(raw, &b)
			switch name {
			case "early":
				b.Header.Height = 1
			case "late":
				b.Header.Height = 3
			case "missing":
				b.Txs = b.Txs[:1]
			case "duplicate":
				b.Txs = append(b.Txs, b.Txs[1])
			case "digest":
				b.Txs[1].Extra[len(b.Txs[1].Extra)-1] ^= 1
			case "extra_mint":
				b.Txs = append(b.Txs, b.Txs[0])
			case "full_reward":
				full, err := core.BuildAuthorizedRewardTx(crypto.Address(e.cfg.RewardAddress), 3*lpod.Unit, 2, parent.Hash(), priv)
				if err != nil {
					t.Fatal(err)
				}
				b.Txs[0] = *full
			case "genesis":
				old := e.cfg.LPoDMigration.Genesis
				e.cfg.LPoDMigration.Genesis[0] ^= 1
				defer func() { e.cfg.LPoDMigration.Genesis = old }()
			}
			b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
			b.Header.Sign(priv)
			err := e.validateCoinbasePolicy(&b)
			if err == nil {
				err = e.validateRingCTV4Activation(&b)
			}
			if err == nil {
				t.Fatal("invalid transition accepted")
			}
		})
	}
}

func TestLPoDZeroGuardianProtocolRejectsSeededStake(t *testing.T) {
	e, _, parent, _ := lpodConsensusFixture(t)
	r := core.NewValidatorRegistry()
	r.SeedFromObservedProducers(e.cfg.Validators)
	e.cfg.Registry = r
	if _, err := e.produceBlock(2, 2, parent); err == nil {
		t.Fatal("synthetic stake selected payout tier")
	}
}
