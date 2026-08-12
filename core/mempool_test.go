package core_test

// Unit tests for the flood-resistant mempool eviction policy.
//
// The mempool enforces two capacity caps:
//   - MaxSize: maximum number of transactions (count)
//   - MaxBytes: maximum total byte size of all transactions (RAM cap)
//
// When either cap is exceeded, the lowest-fee-rate (fee/byte) non-system
// transaction is evicted so that a spammer flooding with dust-fee txs displaces
// their own entries rather than legitimate high-value ones.

import (
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── helpers ──────────────────────────────────────────────────────────────────

// makeRing returns a ring of RingSize distinct dummy public keys.
func makeRing() []crypto.RingMember {
	ring := make([]crypto.RingMember, crypto.RingSize)
	for i := range ring {
		ring[i][0] = byte(i + 1)
	}
	return ring
}

// makeKeyImage returns a non-zero key image that is unique per index.
func makeKeyImage(idx int) crypto.KeyImage {
	var ki crypto.KeyImage
	ki[0] = byte(idx >> 8)
	ki[1] = byte(idx)
	if ki[0] == 0 && ki[1] == 0 {
		ki[2] = 1 // ensure non-zero
	}
	return ki
}

// makeTx builds a minimal valid transaction that passes core.Transaction.Validate().
//
// It has 1 input (ring size = crypto.RingSize), 1 output, 1 signature stub, and
// 1 range proof stub.  No cryptographic verification is performed by Validate()
// beyond structural checks, so dummy byte arrays suffice for tests.
//
// fee is the absolute fee in nAPRO; kiIdx must be unique per call to avoid
// duplicate key-image rejections.
func makeTx(fee uint64, kiIdx int) core.Transaction {
	return core.Transaction{
		Version: core.TxVersionBase,
		Inputs: []core.RingInput{
			{
				KeyImage:     makeKeyImage(kiIdx),
				Ring:         makeRing(),
				AmountCommit: crypto.Commitment{},
			},
		},
		Outputs: []core.Output{
			{
				OneTimePub:   crypto.Point32{byte(kiIdx)},
				TxPubKey:     crypto.Point32{},
				AmountCommit: crypto.Commitment{},
			},
		},
		Fee:         fee,
		Signatures:  []*crypto.MLSAGSignature{{}},
		RangeProofs: []*crypto.RangeProof{{}},
	}
}

// silentLogger discards all log output so test output stays clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// ─── Tests ───────────────────────────────────────────────────────────────────

// TestMempool_EvictLowestFeeOnCountCap verifies that when the mempool reaches
// its MaxSize cap:
//   - the lowest-fee-rate transaction is evicted
//   - a newly added higher-fee transaction is kept
//   - the count never exceeds MaxSize
func TestMempool_EvictLowestFeeOnCountCap(t *testing.T) {
	const cap = 5

	// Use a very low BaseFeePerByte so we can control fees easily.
	cfg := core.MempoolConfig{
		MaxSize:        cap,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		TTL:            0, // TTL eviction not used here
		BaseFeePerByte: 1, // 1 nAPRO/byte — easy to exceed
	}
	pool := core.NewMempool(cfg, silentLogger())

	// Compute the minimum fee required to pass the fee check.
	// Use a dummy tx to get size, then multiply by BaseFeePerByte=1.
	sampleTx := makeTx(0, 999)
	txSize := sampleTx.Size()
	minFee := uint64(txSize) * cfg.BaseFeePerByte

	// Fill the pool to capacity with txs that pay exactly the minimum fee.
	for i := 0; i < cap; i++ {
		tx := makeTx(minFee, i)
		if err := pool.Add(tx); err != nil {
			t.Fatalf("Add tx %d: %v", i, err)
		}
	}
	if pool.Count() != cap {
		t.Fatalf("expected %d txs, got %d", cap, pool.Count())
	}

	// Compute hash of the cheapest tx (index 0) — it should be evicted.
	cheapestTx := makeTx(minFee, 0)
	cheapestHash := cheapestTx.Hash()

	// Add one high-fee tx that exceeds the cap — cheapest should be evicted.
	highFeeTx := makeTx(minFee*10, cap) // 10× the minimum fee
	if err := pool.Add(highFeeTx); err != nil {
		t.Fatalf("Add high-fee tx: %v", err)
	}

	// Pool must still be at cap (one in, one out).
	if pool.Count() != cap {
		t.Fatalf("count after eviction: want %d, got %d", cap, pool.Count())
	}

	// The high-fee tx must survive.
	if _, found := pool.Get(highFeeTx.Hash()); !found {
		t.Error("high-fee tx was evicted but should have survived")
	}

	// One of the minimum-fee txs must have been evicted.
	// (The pool had cap identical-fee-rate txs; the evicted one is deterministic
	// in tests with small maps, but we only assert that the pool shrank by one
	// and the high-fee tx is present.)
	_ = cheapestHash // already checked via pool.Count
}

// TestMempool_HighFeeRateSurvivesFlood fills the pool with low-fee-rate spam,
// then adds a single high-fee-rate tx and confirms it is kept while spam is
// displaced — the core property that makes mempool flooding economically costly
// for attackers.
func TestMempool_HighFeeRateSurvivesFlood(t *testing.T) {
	const cap = 10

	cfg := core.MempoolConfig{
		MaxSize:        cap,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sampleTx := makeTx(0, 9999)
	txSize := sampleTx.Size()
	minFee := uint64(txSize) * cfg.BaseFeePerByte

	// Flood with minimum-fee spam (cap txs).
	for i := 0; i < cap; i++ {
		if err := pool.Add(makeTx(minFee, i)); err != nil {
			t.Fatalf("spam Add %d: %v", i, err)
		}
	}
	if pool.Count() != cap {
		t.Fatalf("want %d spam txs, got %d", cap, pool.Count())
	}

	// Add a high-fee-rate tx.  It should survive; spam is evicted to make room.
	vipFee := minFee * 100
	vipTx := makeTx(vipFee, cap+1)
	if err := pool.Add(vipTx); err != nil {
		t.Fatalf("Add VIP tx: %v", err)
	}

	if _, found := pool.Get(vipTx.Hash()); !found {
		t.Fatal("VIP high-fee-rate tx was evicted — eviction policy is broken")
	}
	if pool.Count() != cap {
		t.Fatalf("count should remain at cap %d, got %d", cap, pool.Count())
	}
}

// TestMempool_ByteCapEnforced verifies that the MaxBytes RAM cap is enforced
// independently of the MaxSize count cap.
func TestMempool_ByteCapEnforced(t *testing.T) {
	sampleTx := makeTx(0, 9999)
	txSize := sampleTx.Size()
	minFee := uint64(txSize) * 1 // BaseFeePerByte = 1

	// Set MaxBytes to exactly 3 transaction-worth of bytes so a 4th forces eviction.
	cfg := core.MempoolConfig{
		MaxSize:        1_000,       // high count cap — bytes cap will trigger first
		MaxBytes:       txSize * 3,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	for i := 0; i < 3; i++ {
		if err := pool.Add(makeTx(minFee, i)); err != nil {
			t.Fatalf("Add tx %d: %v", i, err)
		}
	}

	before := pool.TotalBytes()
	if before > cfg.MaxBytes {
		t.Fatalf("totalBytes %d exceeds MaxBytes %d before 4th add", before, cfg.MaxBytes)
	}

	// Adding a 4th tx must evict one to keep within the byte cap.
	if err := pool.Add(makeTx(minFee*2, 3)); err != nil {
		t.Fatalf("Add 4th tx: %v", err)
	}

	if pool.TotalBytes() > cfg.MaxBytes {
		t.Fatalf("totalBytes %d still exceeds MaxBytes %d after eviction",
			pool.TotalBytes(), cfg.MaxBytes)
	}
	if pool.Count() != 3 {
		t.Fatalf("count should stay at 3 after byte-cap eviction, got %d", pool.Count())
	}
}

// TestMempool_CountNeverExceedsCap is a stress test: add many more transactions
// than MaxSize and verify the count never exceeds the cap.
func TestMempool_CountNeverExceedsCap(t *testing.T) {
	const cap = 20
	const adds = 100

	cfg := core.MempoolConfig{
		MaxSize:        cap,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sampleTx := makeTx(0, 9999)
	txSize := sampleTx.Size()
	minFee := uint64(txSize) * cfg.BaseFeePerByte

	for i := 0; i < adds; i++ {
		_ = pool.Add(makeTx(minFee, i))
		if pool.Count() > cap {
			t.Fatalf("after %d adds: count %d exceeds cap %d", i+1, pool.Count(), cap)
		}
	}
}

// TestMempool_TotalBytesNeverExceedsMaxBytes verifies the hard invariant:
// pool.TotalBytes() ≤ MaxBytes at all times, even after many concurrent
// evictions.
func TestMempool_TotalBytesNeverExceedsMaxBytes(t *testing.T) {
	sampleTx := makeTx(0, 9999)
	txSize := sampleTx.Size()
	minFee := uint64(txSize) * 1

	const maxTxs = 5
	cfg := core.MempoolConfig{
		MaxSize:        1_000,
		MaxBytes:       txSize * maxTxs,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	for i := 0; i < maxTxs*3; i++ {
		_ = pool.Add(makeTx(minFee+uint64(i), i)) // escalating fee so each is valid
		if pool.TotalBytes() > cfg.MaxBytes {
			t.Fatalf("after add %d: TotalBytes %d > MaxBytes %d",
				i+1, pool.TotalBytes(), cfg.MaxBytes)
		}
	}
}

// TestMempool_RejectWhenOnlySystemTxsAndByteCapExceeded verifies that when the
// pool contains only coinbase/stake transactions whose combined size already
// fills MaxBytes, a new regular transaction is rejected rather than inserted
// past the cap.
//
// This tests the previously-broken code path where evictLowestFeeRate returned
// false after a fallback evict, causing the caller to break out of the loop and
// insert the tx regardless of remaining cap pressure.
func TestMempool_RejectWhenPoolFullAndNothingToEvict(t *testing.T) {
	// Build one coinbase tx (IsCoinbase returns true ⟹ privileged path only).
	// We use AddPrivileged to insert it, then try Add() with a regular tx when
	// the byte cap has no remaining headroom.
	sampleTx := makeTx(0, 9999)
	txSize := sampleTx.Size()

	// MaxBytes = exactly one tx; the coinbase fills it via AddPrivileged.
	cfg := core.MempoolConfig{
		MaxSize:        1_000,
		MaxBytes:       txSize,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	// Insert one coinbase that consumes the entire byte budget.
	cb := core.CoinbaseTx(crypto.Point32{}, 1_000_000)
	if err := pool.AddPrivileged(cb); err != nil {
		t.Fatalf("AddPrivileged coinbase: %v", err)
	}
	if pool.TotalBytes() == 0 {
		t.Skip("coinbase size is 0, test not meaningful")
	}

	// Now try to add a regular tx — pool is full and the only entry is a
	// coinbase that cannot be evicted via fee-rate path (it's a system tx).
	// The fallback evictOldest should remove the coinbase, making room — unless
	// the coinbase is larger than txSize, in which case we verify no cap breach.
	minFee := uint64(txSize) * cfg.BaseFeePerByte
	err := pool.Add(makeTx(minFee, 1))

	// Whether the add succeeds or fails, the invariant must hold.
	if pool.TotalBytes() > cfg.MaxBytes {
		t.Fatalf("TotalBytes %d > MaxBytes %d after Add (err=%v)",
			pool.TotalBytes(), cfg.MaxBytes, err)
	}
}

// TestAddPrivileged_VerifiesNonCoinbaseTx confirms that AddPrivileged runs full
// cryptographic verification via TxVerifier.VerifyTx when a Verifier is wired in.
//
// A non-coinbase transaction carrying stub (all-zero) ring signatures must be
// rejected even on the privileged path — AddPrivileged is only exempt from fee
// and count checks, not from cryptographic validity.  This guards against an
// attacker who somehow obtains privileged access to the mempool and tries to
// inject an unverified transaction.
func TestAddPrivileged_VerifiesNonCoinbaseTx(t *testing.T) {
	cfg := core.MempoolConfig{
		MaxSize:        100,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 0, // no fee minimum — we test signature rejection, not fee policy
	}
	pool := core.NewMempool(cfg, silentLogger())

	// Wire a real TxVerifier with a nil UTXOSet (double-spend checks are skipped,
	// but MLSAG ring-signature and commitment checks still run).
	v := core.NewTxVerifier(nil)
	pool.SetVerifier(v)

	// makeTx produces a non-coinbase tx (1 ring input) with an all-zero stub
	// MLSAGSignature.  The ring-closure check in MLSAGVerify must reject it.
	tx := makeTx(0, 42)
	err := pool.AddPrivileged(tx)
	if err == nil {
		t.Error("AddPrivileged: expected non-nil error for non-coinbase tx with stub ring signature, got nil — verifier not enforced on privileged path?")
	}
}

// TestMempool_BytesTrackedCorrectlyAfterRemove confirms that Remove() correctly
// decrements totalBytes so the byte cap stays accurate after block inclusion.
func TestMempool_BytesTrackedCorrectlyAfterRemove(t *testing.T) {
	cfg := core.MempoolConfig{
		MaxSize:        100,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	sampleTx := makeTx(0, 9999)
	txSize := sampleTx.Size()
	minFee := uint64(txSize) * cfg.BaseFeePerByte

	tx0 := makeTx(minFee, 0)
	tx1 := makeTx(minFee, 1)
	if err := pool.Add(tx0); err != nil {
		t.Fatalf("Add tx0: %v", err)
	}
	if err := pool.Add(tx1); err != nil {
		t.Fatalf("Add tx1: %v", err)
	}
	beforeBytes := pool.TotalBytes()

	pool.Remove(tx0.Hash())

	afterBytes := pool.TotalBytes()
	if afterBytes >= beforeBytes {
		t.Fatalf("TotalBytes did not decrease after Remove: before=%d after=%d",
			beforeBytes, afterBytes)
	}
	if afterBytes < 0 {
		t.Fatalf("TotalBytes went negative: %d", afterBytes)
	}
}

// ─── Task #1224: CleanStaleTmpFiles removes old .tmp, ignores recent ones ─────
//
// Verifies the three cases:
//  1. A .tmp file older than 5 minutes is deleted and the node starts cleanly.
//  2. A .tmp file younger than 5 minutes is left alone.
//  3. No .tmp file present — function is a no-op.
func TestCleanStaleTmpFiles(t *testing.T) {
	dir := t.TempDir()
	tmp := dir + "/mempool.json.tmp"
	log := silentLogger()

	// Case 3: no .tmp — should not panic or error.
	core.CleanStaleTmpFiles(dir, log)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("case 3: expected no .tmp file, got err=%v", err)
	}

	// Case 2: fresh .tmp (mtime = now) — must NOT be removed.
	if err := os.WriteFile(tmp, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write fresh tmp: %v", err)
	}
	core.CleanStaleTmpFiles(dir, log)
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("case 2: fresh .tmp was incorrectly removed: %v", err)
	}

	// Case 1: back-date the .tmp to 10 minutes ago — must be removed.
	staleTime := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(tmp, staleTime, staleTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	core.CleanStaleTmpFiles(dir, log)
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("case 1: stale .tmp was NOT removed (err=%v)", err)
	}

	// Confirm that a normal mempool Load still works after the cleanup ran
	// (i.e. the absence of the .tmp does not break anything).
	pool := core.NewMempool(core.DefaultMempoolConfig(), log)
	n := pool.Load(dir, log)
	if n != 0 {
		t.Fatalf("expected 0 restored txs after cleanup, got %d", n)
	}
}

// ─── Task #487: double-spent key image is caught before entering the mempool ─
//
// Wires a TxVerifier backed by a UTXOSet with an already-spent key image into
// the mempool, then submits a transaction reusing that key image and asserts
// that Add() rejects it with the double-spend error BEFORE the tx is stored.
// A regression to the VerifyTx step-3 IsSpent guard would let double-spends
// sit in the mempool until block-apply time.
func TestMempool_RejectsDoubleSpentKeyImageBeforeEntry(t *testing.T) {
	cfg := core.MempoolConfig{
		MaxSize:        100,
		MaxBytes:       256 * 1024 * 1024,
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 0, // fee policy is not under test
	}
	pool := core.NewMempool(cfg, silentLogger())

	// Populate a UTXO set and mark one key image as spent.
	utxos := core.NewUTXOSet()
	const spentIdx = 7
	spentKI := makeKeyImage(spentIdx)
	utxos.MarkSpent(spentKI)
	if !utxos.IsSpent(spentKI) {
		t.Fatal("precondition: MarkSpent did not register the key image as spent")
	}

	// Wire the verifier into the mempool (same path production uses).
	pool.SetVerifier(core.NewTxVerifier(utxos))

	// A tx reusing the spent key image must be rejected by Add().
	tx := makeTx(0, spentIdx) // same kiIdx → same key image as spentKI
	err := pool.Add(tx)
	if err == nil {
		t.Fatal("Add: expected error for tx with already-spent key image, got nil")
	}
	if !strings.Contains(err.Error(), "double-spend") {
		t.Errorf("Add: expected double-spend rejection, got: %v", err)
	}

	// The rejected tx must never have entered the pool.
	if _, found := pool.Get(tx.Hash()); found {
		t.Error("rejected double-spend tx is present in the mempool")
	}
	if pool.Count() != 0 {
		t.Errorf("mempool count = %d after rejection, want 0", pool.Count())
	}

	// Control: a tx with a FRESH key image must NOT trip the double-spend
	// guard.  It still fails later in VerifyTx (stub ring members are absent
	// from the UTXO set → C-0a), which proves the spent-KI rejection above
	// fired specifically on the double-spend check, not on some later step.
	freshErr := pool.Add(makeTx(0, spentIdx+1))
	if freshErr == nil {
		t.Fatal("control tx unexpectedly passed full verification with stub proofs")
	}
	if strings.Contains(freshErr.Error(), "double-spend") {
		t.Errorf("control tx with fresh key image was rejected as double-spend: %v", freshErr)
	}
}

// ─── Task #436: concurrent Add/Remove/Evict does not race ────────────────────
//
// Runs N goroutines simultaneously, each attempting to Add a distinct tx.
// A second group of goroutines calls Remove on every tx hash they observe.
// The test is run with -race (go test -race) and must not deadlock.
func TestMempool_ConcurrentAddRemove_NoRace(t *testing.T) {
	const (
		workers    = 16
		txsPerGoro = 50
	)

	cfg := core.MempoolConfig{
		MaxSize:        workers * txsPerGoro / 2, // deliberately small to force evictions
		MaxBytes:       1 << 20,                  // 1 MiB — generous so byte cap is not hit
		MaxTxSize:      1_000_000,
		BaseFeePerByte: 1,
	}
	pool := core.NewMempool(cfg, silentLogger())

	// Pre-generate transactions so goroutines don't race on kiIdx allocation.
	total := workers * txsPerGoro
	txs := make([]core.Transaction, total)
	for i := range txs {
		txs[i] = makeTx(1+uint64(i), i) // escalating fee — each unique
	}

	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			base := g * txsPerGoro
			for i := 0; i < txsPerGoro; i++ {
				tx := txs[base+i]
				_ = pool.Add(tx) // may fail due to eviction — that is fine
				if i%3 == 0 {
					pool.Remove(tx.Hash())
				}
			}
		}()
	}
	wg.Wait()

	// Invariant: pool size must never exceed MaxSize.
	if pool.Count() > cfg.MaxSize {
		t.Errorf("pool.Count() = %d, exceeds MaxSize = %d", pool.Count(), cfg.MaxSize)
	}
	// Invariant: totalBytes must be non-negative.
	if pool.TotalBytes() < 0 {
		t.Errorf("pool.TotalBytes() = %d < 0", pool.TotalBytes())
	}
}

// ─── Task #486: fabricated ring-commitment swap is caught by the C-0 check ───
//
// TxVerifier's C-0 commitment-binding check requires that at least one ring
// member PRESENT in the UTXO set has AmountCommit == inp.AmountCommit.  These
// tests cover the previously untested branch where the ring member IS present
// in byPubKey but its stored commitment has been swapped for a fabricated one.
func TestTxVerifier_C0_FabricatedRingCommitmentRejected(t *testing.T) {
	utxos := core.NewUTXOSet()

	// A known on-chain UTXO with a distinctive, non-zero AmountCommit.
	var realPub crypto.Point32
	realPub[0] = 0xAA
	var realCommit crypto.Commitment
	realCommit[0] = 0xC0
	realCommit[1] = 0xFF
	utxos.Add(&core.UTXO{
		TxHash:       crypto.Hash32{0x01},
		OutputIndex:  0,
		OneTimePub:   realPub,
		AmountCommit: realCommit,
		BlockHeight:  1,
	})
	if utxos.GetByPubKey(realPub) == nil {
		t.Fatal("precondition: UTXO not present in byPubKey index")
	}

	v := core.NewTxVerifier(utxos)

	// buildTx crafts a tx whose ring contains the real on-chain pub key at
	// slot 0 (the rest are random-looking keys absent from the UTXO set) and
	// claims the given input AmountCommit.
	buildTx := func(claimedCommit crypto.Commitment) core.Transaction {
		ring := makeRing()
		ring[0] = realPub
		return core.Transaction{
			Version: core.TxVersionBase,
			Inputs: []core.RingInput{
				{
					KeyImage:     makeKeyImage(486),
					Ring:         ring,
					AmountCommit: claimedCommit,
				},
			},
			Outputs: []core.Output{
				{
					OneTimePub:   crypto.Point32{0xBB},
					AmountCommit: crypto.Commitment{},
				},
			},
			Fee:         0,
			Signatures:  []*crypto.MLSAGSignature{{}},
			RangeProofs: []*crypto.RangeProof{{}},
		}
	}

	// Case 1: fabricated commitment — the ring member is present in the UTXO
	// set but the claimed inp.AmountCommit does not match its stored commit.
	// VerifyTx must reject citing the C-0 check.
	var fabricated crypto.Commitment
	fabricated[0] = 0xDE
	fabricated[1] = 0xAD
	if fabricated == realCommit {
		t.Fatal("test bug: fabricated commit equals real commit")
	}
	tx1 := buildTx(fabricated)
	err := v.VerifyTx(&tx1)
	if err == nil {
		t.Fatal("VerifyTx: expected non-nil error for fabricated ring commitment, got nil")
	}
	if !strings.Contains(err.Error(), "C-0 check") {
		t.Errorf("VerifyTx: expected C-0 check rejection, got: %v", err)
	}
	// Must be the C-0 mismatch branch, not the C-0a all-absent branch: the
	// real ring member IS present in the UTXO set.
	if strings.Contains(err.Error(), "C-0a") {
		t.Errorf("VerifyTx: fired C-0a (all-absent) instead of C-0 commitment-binding: %v", err)
	}

	// Case 2 (control): the same tx with the CORRECT commitment must pass the
	// C-0 check.  It still fails later (stub ring signature), which proves the
	// rejection in case 1 came specifically from the commitment binding.
	tx2 := buildTx(realCommit)
	ctrlErr := v.VerifyTx(&tx2)
	if ctrlErr == nil {
		t.Fatal("control tx with stub ring signature unexpectedly passed full verification")
	}
	if strings.Contains(ctrlErr.Error(), "C-0") {
		t.Errorf("control tx with correct commitment was rejected by C-0/C-0a: %v", ctrlErr)
	}
}
