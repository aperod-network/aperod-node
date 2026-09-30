package store

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
)

// Uses identical setup derived from the baseline's signed V3 funding fixture.
// Cross-tree bytes, not a copied settlement algorithm, are the oracle.
func TestProductionV3CompatibilityVectors(t *testing.T) {
	old := crand.Reader
	crand.Reader = &productionV3Entropy{}
	t.Cleanup(func() { crand.Reader = old })
	db, m, priv, address := productionV3Fixture(t)
	t.Cleanup(func() { db.Close() })
	if err := db.CheckLPoDConfig(m); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"activation", "next-checkpoint"} {
		migration := m
		if label != "activation" {
			migration = nil
		}
		b, r, raw := productionV3Block(t, db, migration, priv, address)
		c, payments, err := db.PreviewLPoD(b.Header.Height, r)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.CommitRawBlockWithAVM(b.Hash(), b.Header.Height, raw, nil, crypto.Hash32{}, r); err != nil {
			t.Fatal(err)
		}
		stored, err := db.LoadLPoDCheckpoint()
		if err != nil || stored == nil {
			t.Fatalf("checkpoint: %v", err)
		}
		if stored.Digest() != c.Digest() || stored.Allocation == nil || stored.Allocation.Version != 3 {
			t.Fatal("V3 preview/commit mismatch")
		}
		if err := db.CheckLPoDConfig(m); err != nil {
			t.Fatal(err)
		}
		binding, err := db.get([]byte("lpod/activation/v3"))
		if err != nil {
			t.Fatal(err)
		}
		vector, err := json.Marshal(struct {
			Name                 string
			Migration            *LPoDMigration
			PreviousStake, Stake map[string]LPoDValidatorStake
			Checkpoint           *LPoDCheckpoint
			Digest               crypto.Hash32
			Payments             []lpod.Payment
			Block                *core.Block
			Binding              []byte
		}{label, m, r.PreviousStake, r.Stake, stored, stored.Digest(), payments, b, binding})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("V3_VECTOR sha256=%x json=%s", sha256.Sum256(vector), vector)
	}
}

// Canonical setup derived from the baseline V3 fixture. Kept here rather than
// calling tree-local helpers: the candidate's lpodActivationBlock adds audit
// inputs and PreviousStake that the baseline helper does not provide.
func productionV3Fixture(t *testing.T) (*DB, *LPoDMigration, crypto.ValidatorPrivKey, crypto.Address) {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, ValidatorPub: pub}}
	genesis.Header.MerkleRoot = core.MerkleRoot(nil)
	if err := genesis.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	genesisRaw, err := json.Marshal(genesis)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CommitRawBlockWithAVM(genesis.Hash(), 0, genesisRaw, nil, crypto.Hash32{}); err != nil {
		t.Fatal(err)
	}
	parent := &core.Block{Header: core.BlockHeader{Height: LPoDV2ActivationHeight - 1, PrevHash: genesis.Hash(), ValidatorPub: pub, Timestamp: 1}}
	parent.Header.MerkleRoot = core.MerkleRoot(nil)
	if err := parent.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	parentRaw, err := json.Marshal(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(parent.Hash(), parent.Header.Height, parentRaw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(parent.Hash(), parent.Header.Height); err != nil {
		t.Fatal(err)
	}
	if err := db.StoreStakingPoolRemaining(2_000_000_000 * lpod.Unit); err != nil {
		t.Fatal(err)
	}
	m := &LPoDMigration{
		Version: 3, PositionLifecycleVersion: 1, Height: LPoDV2ActivationHeight,
		Genesis: genesis.Hash(), ParentHash: parent.Hash(), TrustAssumption: LPoDV3TrustAssumption,
		NominalEligible: 6_000_000_000 * lpod.Unit, ValidatorRemaining: 2_000_000_000 * lpod.Unit,
		TrustedValidators: []crypto.ValidatorPubKey{pub},
	}
	m.NominalSnapshot = &LPoDNominalSnapshot{
		Basis: LPoDV3NominalBasis, NominalCirculatingBefore: 6_000_000_000 * lpod.Unit,
		EligibleNominal: m.NominalEligible, PreexistingGuardianReservation: 0,
		ValidatorRemaining: m.ValidatorRemaining, EligibilityRule: LPoDV3EligibilityRule,
	}
	m.SnapshotRoot = m.NominalSnapshot.Root()
	m.ReconciliationRoot = m.Root()
	sig, err := priv.Sign(m.AttestationMessage())
	if err != nil {
		t.Fatal(err)
	}
	m.Attestations = []LPoDAttestation{{Validator: pub, Signature: sig}}
	input, err := json.Marshal(struct {
		Genesis, Parent *core.Block
		Migration       *LPoDMigration
		Address         crypto.Address
		Pool            uint64
	}{genesis, parent, m, address, 2_000_000_000 * lpod.Unit})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("V3_SETUP_INPUT sha256=%x json=%s", sha256.Sum256(input), input)
	return db, m, priv, address
}

func productionV3Block(t *testing.T, db *DB, m *LPoDMigration, priv crypto.ValidatorPrivKey, address crypto.Address) (*core.Block, *LPoDSettlement, []byte) {
	t.Helper()
	parent, height, err := db.GetTip()
	if err != nil {
		t.Fatal(err)
	}
	parentBlock, err := db.readLPoDCanonicalBlock(height)
	if err != nil {
		t.Fatal(err)
	}
	r := &LPoDSettlement{Parent: parent, Migration: m, PositionProtocol: true,
		Timestamp: parentBlock.Header.Timestamp + 3_000_000_000, Proposer: priv.Public().Hex(), Leader: address,
		Stake: map[string]LPoDValidatorStake{priv.Public().Hex(): {Amount: 100_000 * lpod.Unit, Active: true}},
	}
	// Serialize only explicitly supplied baseline fields, before any preview.
	// This is an INPUT record, not output filtering or normalization.
	input, err := json.Marshal(struct {
		Height               uint64
		Parent               crypto.Hash32
		Migration            *LPoDMigration
		PositionProtocol     bool
		Timestamp            int64
		Proposer             string
		Leader               crypto.Address
		PreviousStake, Stake map[string]LPoDValidatorStake
	}{height + 1, r.Parent, r.Migration, r.PositionProtocol, r.Timestamp, r.Proposer, r.Leader, r.PreviousStake, r.Stake})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("V3_SETTLEMENT_INPUT sha256=%x json=%s", sha256.Sum256(input), input)
	c, pay, err := db.PreviewLPoD(height+1, r)
	if err != nil {
		t.Fatal(err)
	}
	reward, err := db.PayoutLPoD(height+1, r, c, pay)
	if err != nil {
		t.Fatal(err)
	}
	b := &core.Block{Header: core.BlockHeader{Height: height + 1, PrevHash: parent, ValidatorPub: priv.Public(), Timestamp: r.Timestamp},
		Txs: []core.Transaction{reward, core.LPoDCheckpointTx(c.Digest())}}
	b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
	if err := b.Header.Sign(priv); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return b, r, raw
}

type productionV3Entropy struct {
	counter uint64
	block   []byte
}

func (r *productionV3Entropy) Read(p []byte) (int, error) {
	if len(p) == 1 {
		p[0] = 0
		return 1, nil
	}
	n := len(p)
	for len(p) > 0 {
		if len(r.block) == 0 {
			var counter [8]byte
			binary.LittleEndian.PutUint64(counter[:], r.counter)
			sum := sha256.Sum256(append([]byte("production-v3-compat"), counter[:]...))
			r.counter++
			r.block = sum[:]
		}
		written := copy(p, r.block)
		p, r.block = p[written:], r.block[written:]
	}
	return n, nil
}
