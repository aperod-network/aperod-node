package consensus

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/store"
)

// Run alone, serially, in each tree with the same Go toolchain. These vectors
// exercise the real prepare, payout, incoming-block validation and atomic store
// commit paths. They deliberately exclude optional audit-only settlement fields.
// They are cross-tree regression evidence, not historical-chain authentication.
func TestProductionSettlementCompatibilityVectors(t *testing.T) {
	old := crand.Reader
	t.Cleanup(func() { crand.Reader = old })
	reset := func(label string) { crand.Reader = &settlementCompatEntropy{seed: label} }
	for _, seeded := range []bool{false, true} {
		for _, tier := range []uint64{100_000, 500_000, 1_000_000, 5_000_000, 10_000_000, 30_000_000, 50_000_000, 80_000_000, 100_000_000} {
			name := fmt.Sprintf("validator/seeded=%t/tier=%d", seeded, tier)
			t.Run(name, func(t *testing.T) {
				reset(name)
				e, _, _, priv := lpodConsensusFixture(t)
				u := core.NewUTXOSet()
				e.SetTxVerifier(core.NewTxVerifier(u), u)
				snapshot := e.cfg.Registry.TakeSnapshot()
				entry := snapshot.Validators[priv.Public().Hex()]
				entry.Seeded, entry.StakeNAPR = false, tier*lpod.Unit
				if seeded {
					_, pub, err := crypto.GenerateValidatorKey()
					if err != nil {
						t.Fatal(err)
					}
					snapshot.Validators[pub.Hex()] = &core.ValidatorEntry{PubKey: pub, StakeNAPR: tier * lpod.Unit, Seeded: true, Status: core.ValidatorPending}
				}
				e.cfg.Registry.RestoreFromSnapshot(snapshot)
				settlementCompatCommit(t, e, priv, "activation")
				settlementCompatCommit(t, e, priv, "next-checkpoint")
			})
		}
	}
	for _, action := range []core.StakeAction{core.StakePartialWithdraw, core.StakeWithdraw} {
		name := fmt.Sprintf("validator-withdrawal/%d", action)
		t.Run(name, func(t *testing.T) {
			reset(name)
			e, _, _, priv := lpodConsensusFixture(t)
			u := core.NewUTXOSet()
			e.SetTxVerifier(core.NewTxVerifier(u), u)
			snapshot := e.cfg.Registry.TakeSnapshot()
			snapshot.Validators[priv.Public().Hex()].StakeNAPR = 500_000 * lpod.Unit
			e.cfg.Registry.RestoreFromSnapshot(snapshot)
			settlementCompatCommit(t, e, priv, "activation")
			tx := settlementCompatWithdrawal(t, e, priv, action, 100_000*lpod.Unit)
			settlementCompatCommit(t, e, priv, "withdrawal", tx)
		})
	}
	t.Run("ordinary-position-partial-complete-and-epoch", func(t *testing.T) {
		reset(t.Name())
		e, db, deposit, action, owner, priv, _ := positionEngine(t)
		settlementCompatCommit(t, e, priv, "deposit", *deposit)
		settlementCompatCommit(t, e, priv, "accrual-at-top-tier")
		action.Action, action.Nonce = core.LPoDWithdraw, 1
		action.WithdrawAmount = 99_500_000 * lpod.Unit
		tx, err := core.BuildLPoDPositionTx(action, owner.Spend.Private, crypto.Scalar32{})
		if err != nil {
			t.Fatal(err)
		}
		settlementCompatCommit(t, e, priv, "partial-position-withdrawal", *tx)
		settlementCompatCommit(t, e, priv, "accrual-at-lower-tier")
		action.Nonce, action.WithdrawAmount = 2, 400_000*lpod.Unit
		tx, err = core.BuildLPoDPositionTx(action, owner.Spend.Private, crypto.Scalar32{})
		if err != nil {
			t.Fatal(err)
		}
		settlementCompatCommit(t, e, priv, "complete-position-withdrawal", *tx)
		c, err := db.LoadLPoDCheckpoint()
		if err != nil {
			t.Fatal(err)
		}
		if c.PrincipalLocked != 0 || c.PrincipalReturned != c.PrincipalDeposited {
			t.Fatal("position principal not completely returned")
		}
		// Reach the actual consensus epoch boundary; do not simulate it by
		// calling UpdateEpoch or by fabricating a height-99 checkpoint.
		for e.chain.Tip().Header.Height < core.EpochLength+1 {
			settlementCompatCommit(t, e, priv, fmt.Sprintf("epoch-chain-%d", e.chain.Tip().Header.Height+1))
		}
	})
}

func settlementCompatWithdrawal(t *testing.T, e *Engine, priv crypto.ValidatorPrivKey, action core.StakeAction, amount uint64) core.Transaction {
	t.Helper()
	entry, ok := e.cfg.Registry.GetEntry(priv.Public())
	if !ok {
		t.Fatal("missing fixture validator")
	}
	if action == core.StakeWithdraw {
		amount = 0
	}
	a := core.StakeWithdrawalAuthorizationV2{Action: action, PubKey: priv.Public(), Amount: amount,
		Genesis: e.cfg.LPoDMigration.Genesis, Nonce: entry.StakeAuthNonce + 1,
		Generation: entry.StakeGeneration, ExpiryHeight: e.chain.Tip().Header.Height + 10}
	a.StakeRef = core.StakeReferenceV2(a.Genesis, a.PubKey, a.Generation, entry.StakeNAPR)
	var err error
	a.Signature, err = priv.Sign(core.StakeWithdrawalSignMsgV2(a))
	if err != nil {
		t.Fatal(err)
	}
	extra, err := core.EncodeStakeWithdrawalExtraV2(a)
	if err != nil {
		t.Fatal(err)
	}
	return core.Transaction{Version: core.TxVersionStake, Extra: extra}
}

// Observed-producer sentinels are the only active Seeded membership admitted
// by the registry API. They are explicitly forbidden from LPoD production and
// authenticated withdrawal; do not fabricate successful seeded withdrawals.
func TestProductionSeededPolicyCompatibilityVectors(t *testing.T) {
	old := crand.Reader
	t.Cleanup(func() { crand.Reader = old })
	for _, action := range []core.StakeAction{core.StakePartialWithdraw, core.StakeWithdraw} {
		t.Run(fmt.Sprint(action), func(t *testing.T) {
			crand.Reader = &settlementCompatEntropy{seed: t.Name()}
			e, _, parent, priv := lpodConsensusFixture(t)
			r := core.NewValidatorRegistry()
			r.SeedFromObservedProducers(e.cfg.Validators)
			if err := r.ConfigureStakeWithdrawalV2(e.cfg.LPoDMigration.Genesis, 2); err != nil {
				t.Fatal(err)
			}
			e.cfg.Registry = r
			entry, ok := r.GetEntry(priv.Public())
			if !ok || !entry.Seeded || entry.Status != core.ValidatorActive {
				t.Fatal("fixture did not create active observed-producer sentinel")
			}
			before := r.TakeSnapshot()
			tx := settlementCompatWithdrawal(t, e, priv, action, lpod.Unit)
			withdrawErr := r.ValidateBlockStakeTxs([]core.Transaction{tx}, 2)
			_, produceErr := e.produceBlock(2, 2, parent)
			if withdrawErr == nil || produceErr == nil {
				t.Fatalf("seeded guard missing: withdrawal=%v production=%v", withdrawErr, produceErr)
			}
			raw, err := json.Marshal(struct {
				Name                             string
				Before, After                    core.RegistrySnapshot
				Transaction                      core.Transaction
				WithdrawalError, ProductionError string
			}{t.Name(), before, r.TakeSnapshot(), tx, withdrawErr.Error(), produceErr.Error()})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("SEEDED_GUARD_VECTOR sha256=%x json=%s", sha256.Sum256(raw), raw)
		})
	}
}

func settlementCompatCommit(t *testing.T, e *Engine, priv crypto.ValidatorPrivKey, label string, operations ...core.Transaction) {
	t.Helper()
	parent := e.chain.Tip()
	draft := &core.Block{Header: core.BlockHeader{Height: parent.Header.Height + 1, Round: parent.Header.Round + 1,
		PrevHash: parent.Hash(), Timestamp: parent.Header.Timestamp + 3_000_000_000,
		ValidatorPub: priv.Public(), BaseFee: nextBaseFee(parent.Header.BaseFee, parent.Size())}, Txs: operations}
	r, c, payments, err := e.prepareLPoD(draft, crypto.Address(e.cfg.RewardAddress))
	if err != nil {
		t.Fatal(err)
	}
	payout, err := e.cfg.Store.PayoutLPoD(draft.Header.Height, r, c, payments)
	if err != nil {
		t.Fatal(err)
	}
	block := acceptPositionBlock(t, e, priv, 3_000_000_000, operations...)
	stored, err := e.cfg.Store.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest() != stored.Digest() {
		t.Fatal("preview differs from committed checkpoint")
	}
	vector := struct {
		Name          string                              `json:"name"`
		PreviousStake map[string]store.LPoDValidatorStake `json:"previous_stake"`
		Stake         map[string]store.LPoDValidatorStake `json:"stake"`
		Checkpoint    *store.LPoDCheckpoint               `json:"checkpoint"`
		Digest        crypto.Hash32                       `json:"digest"`
		Payments      []lpod.Payment                      `json:"payments"`
		Payout        core.Transaction                    `json:"payout"`
		Block         *core.Block                         `json:"block"`
		Registry      core.RegistrySnapshot               `json:"registry_after"`
	}{t.Name() + "/" + label, r.PreviousStake, r.Stake, stored, stored.Digest(), payments, payout, block, e.cfg.Registry.TakeSnapshot()}
	raw, err := json.Marshal(vector)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SETTLEMENT_VECTOR sha256=%x json=%s", sha256.Sum256(raw), raw)
}

// Deterministic test entropy, never production randomness. A one-byte probe is
// non-consuming so crypto/internal/randutil's optional read cannot shift keys.
type settlementCompatEntropy struct {
	seed    string
	counter uint64
	block   []byte
}

func (r *settlementCompatEntropy) Read(p []byte) (int, error) {
	if len(p) == 1 {
		p[0] = 0
		return 1, nil
	}
	n := len(p)
	for len(p) > 0 {
		if len(r.block) == 0 {
			var counter [8]byte
			binary.LittleEndian.PutUint64(counter[:], r.counter)
			sum := sha256.Sum256(append([]byte(r.seed), counter[:]...))
			r.counter++
			r.block = sum[:]
		}
		written := copy(p, r.block)
		p, r.block = p[written:], r.block[written:]
	}
	return n, nil
}
