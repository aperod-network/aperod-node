package core_test

import (
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// TestProductionCompatibilityVector is an intentionally serial, deterministic
// cross-tree regression vector. Run it by itself when comparing source trees:
//
//	cd blockchain && go test ./core -run '^TestProductionCompatibilityVector$' -count=1 -v
//
// It replaces crypto/rand.Reader only for this test and restores it on return.
// Do not add t.Parallel: crypto/rand.Reader is process-global.
func TestProductionCompatibilityVector(t *testing.T) {
	beginProductionCompatTest(t)

	const supply = uint64(1_000_000_000)
	fixture := newProductionCompatFixture(t, supply)

	payment := buildProductionCompatTx(t, "payment", fixture.alice, fixture.owned, fixture.bobAddr, fixture.aliceAddr, 30_000_000)
	logProductionCompatTx(t, "payment-default", payment)

	exactFee := core.ExportedEstimateFee(1, 2, 1)
	if exactFee >= supply {
		t.Fatalf("estimated exact-spend fee %d is not below input supply %d", exactFee, supply)
	}
	exact := buildProductionCompatTx(t, "exact-spend", fixture.alice, fixture.owned, fixture.bobAddr, fixture.aliceAddr, supply-exactFee)
	if exact.ChangeAmount != 0 || len(exact.Tx.Outputs) != 1 {
		t.Errorf("exact spend produced change=%d outputs=%d; want no change and one output",
			exact.ChangeAmount, len(exact.Tx.Outputs))
	}
	logProductionCompatTx(t, "exact-spend", exact)

	burnAmount := uint64(25_000_000)
	burn := buildProductionCompatTx(t, "burn", fixture.alice, fixture.owned, crypto.MainnetBurnAddress(), fixture.aliceAddr, burnAmount)
	burned, isBurn := burn.Tx.BurnAmount()
	if !isBurn || burned != burnAmount {
		t.Errorf("burn marker=(%d,%v), want (%d,true)", burned, isBurn, burnAmount)
	}
	logProductionCompatTx(t, "burn", burn)
}

type productionCompatFixture struct {
	alice     *crypto.WalletKeyPair
	bobAddr   crypto.Address
	aliceAddr crypto.Address
	owned     []core.OwnedUTXO
}

func beginProductionCompatTest(t *testing.T) {
	t.Helper()
	oldReader := crand.Reader
	t.Cleanup(func() { crand.Reader = oldReader })
}

func resetProductionCompatEntropy(seed string) {
	crand.Reader = newProductionCompatReader(seed)
}

func newProductionCompatFixture(t *testing.T, supply uint64) productionCompatFixture {
	t.Helper()
	aliceSeed := sha256.Sum256([]byte("production-compat-alice-seed"))
	bobSeed := sha256.Sum256([]byte("production-compat-bob-seed"))

	// Construct the fixture from fixed wallet seeds and a fixed random stream.
	resetProductionCompatEntropy("fixture-seed")
	alice, err := crypto.WalletKeysFromSeed(aliceSeed[:])
	if err != nil {
		t.Fatalf("Alice keys: %v", err)
	}
	bob, err := crypto.WalletKeysFromSeed(bobSeed[:])
	if err != nil {
		t.Fatalf("Bob keys: %v", err)
	}
	aliceAddr := crypto.AddressFromKeys(crypto.TestnetByte, alice)
	bobAddr := crypto.AddressFromKeys(crypto.TestnetByte, bob)

	stealth, err := crypto.CreateStealthOutput(alice.Spend.Public, alice.View.Public)
	if err != nil {
		t.Fatalf("create source output: %v", err)
	}
	inputBlind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatalf("source blind: %v", err)
	}
	inputCommit, err := crypto.Commit(supply, inputBlind)
	if err != nil {
		t.Fatalf("source commitment: %v", err)
	}
	owned := []core.OwnedUTXO{{
		UTXO: core.UTXO{
			OneTimePub:   stealth.OneTimePub,
			TxPubKey:     stealth.TxPubKey,
			AmountCommit: inputCommit,
		},
		HsScalar: stealth.HsScalar,
		Amount:   supply,
		Blind:    inputBlind,
	}}
	return productionCompatFixture{alice: alice, bobAddr: bobAddr, aliceAddr: aliceAddr, owned: owned}
}

func buildProductionCompatTx(
	t *testing.T,
	label string,
	alice *crypto.WalletKeyPair,
	owned []core.OwnedUTXO,
	recipient, change crypto.Address,
	amount uint64,
) *core.BuildResult {
	t.Helper()
	// A fresh instance gives each named vector a reproducible random stream.
	resetProductionCompatEntropy("build-seed:" + label)
	builder := core.NewTxBuilder(
		alice.Spend.Private, alice.View.Private, alice.Spend.Public, owned, 1,
	)
	result, err := builder.Build(amount, recipient, change)
	if err != nil {
		t.Fatalf("%s Build(%d): %v", label, amount, err)
	}
	return result
}

func mustProductionCompatJSON(t *testing.T, tx core.Transaction) []byte {
	t.Helper()
	encoded, err := json.Marshal(tx)
	if err != nil {
		t.Fatalf("serialize transaction: %v", err)
	}
	return encoded
}

func logProductionCompatTx(t *testing.T, label string, result *core.BuildResult) {
	t.Helper()
	serialized := mustProductionCompatJSON(t, result.Tx)
	hash := result.Tx.Hash()
	t.Logf("%s tx_hash=%s size=%d fee=%d result_fee=%d change=%d",
		label, hex.EncodeToString(hash[:]), len(serialized),
		result.Tx.Fee, result.TotalFee, result.ChangeAmount)
	for i, out := range result.Tx.Outputs {
		t.Logf("%s output[%d] commitment=%x one_time_pub=%x tx_pub=%x",
			label, i, out.AmountCommit, out.OneTimePub, out.TxPubKey)
	}
	// Full canonical JSON bytes are retained in test output for direct cross-tree
	// comparison; the concise fields above make important differences visible.
	t.Logf("%s serialized_tx_json=%s", label, serialized)
}

// productionCompatReader is a deterministic SHA-256 counter stream. It is test
// entropy only and is never used by production paths.
type productionCompatReader struct {
	seed    []byte
	counter uint64
	block   []byte
}

func newProductionCompatReader(seed string) io.Reader {
	return &productionCompatReader{seed: []byte(seed)}
}

func (r *productionCompatReader) Read(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		if len(r.block) == 0 {
			var count [8]byte
			binary.LittleEndian.PutUint64(count[:], r.counter)
			digest := sha256.Sum256(append(append([]byte(nil), r.seed...), count[:]...))
			r.counter++
			r.block = digest[:]
		}
		n := copy(p[written:], r.block)
		written += n
		r.block = r.block[n:]
	}
	return written, nil
}

var _ io.Reader = (*productionCompatReader)(nil)
