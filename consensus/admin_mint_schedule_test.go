package consensus_test

// Tests for the engine-scheduled admin mint path (ScheduleAdminMint).
//
// Background
// ----------
// The legacy admin-mint path built the mint transaction with height=0 and
// pushed it through the mempool, giving every mint to one address the same
// one-time pub (mint_pub == spend_pub) and therefore the SAME key image.  One
// spent (or phantom) key-image index entry then permanently blocked every
// future mint to that address — void + re-mint could never recover the funds.
//
// The fix schedules the mint into the next produced block: produceBlock builds
// the tx with the block's own height, so mint_pub = spend_pub + height*G is
// unique per mint, exactly like the per-block coinbase reward.

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// newMintTestEngine spins up a single-validator engine producing blocks every
// 20 ms, returning the engine, its chain, and a stop func.
func newMintTestEngine(t *testing.T, rewardAddress string) (*consensus.Engine, *core.Chain, func()) {
	return newMintTestEngineWithStore(t, rewardAddress, nil)
}

func newMintTestEngineWithStore(t *testing.T, rewardAddress string, db *store.DB) (*consensus.Engine, *core.Chain, func()) {
	return newMintTestEngineWithStoreAndProduced(t, rewardAddress, db, nil)
}

func newMintTestEngineWithStoreAndProduced(t *testing.T, rewardAddress string, db *store.DB, onProduced func(*core.Block) error) (*consensus.Engine, *core.Chain, func()) {
	return newMintTestEngineWithAdminMintStore(t, rewardAddress, db, nil, onProduced)
}

func newMintTestEngineWithAdminMintStore(t *testing.T, rewardAddress string, db *store.DB, adminMintStore consensus.AdminMintRecordStore, onProduced func(*core.Block) error) (*consensus.Engine, *core.Chain, func()) {
	t.Helper()

	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey:", err)
	}

	genesisHdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHdr.Sign(validatorPriv); err != nil {
		t.Fatal("sign genesis:", err)
	}
	genesis := &core.Block{Header: genesisHdr}

	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal("SetGenesis:", err)
	}
	lk, err := crypto.NewLockedValidatorKey(validatorPriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}

	utxos := core.NewUTXOSet()
	reg := core.NewValidatorRegistry()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime:       20 * time.Millisecond,
		BFTThreshold:    0.667,
		Validators:      []crypto.ValidatorPubKey{validatorPub},
		Registry:        reg,
		MyKey:           lk,
		RewardAddress:   rewardAddress,
		Store:           db,
		AdminMintStore:  adminMintStore,
		OnBlockProduced: onProduced,
		// These tests cover the legacy admin-mint scheduler.  Keep them below
		// the hard-fork boundary; post-activation mints require an on-chain
		// authorization format that these legacy fixtures do not carry.
		RingCTV4ActivationHeight: ^uint64(0),
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		eng.Run(stop)
	}()
	var once sync.Once
	return eng, chain, func() {
		once.Do(func() {
			close(stop)
			<-engineDone
			lk.Destroy()
		})
	}
}

type failFirstCompletedAdminMintStore struct {
	db *store.DB

	mu              sync.Mutex
	failedCompleted bool
	loads           int
}

func (s *failFirstCompletedAdminMintStore) StoreAdminMintRecord(key string, record store.AdminMintRecord) error {
	s.mu.Lock()
	if record.State == "completed" && !s.failedCompleted {
		s.failedCompleted = true
		s.mu.Unlock()
		return errors.New("injected completed admin mint write failure")
	}
	s.mu.Unlock()
	return s.db.StoreAdminMintRecord(key, record)
}

func (s *failFirstCompletedAdminMintStore) LoadAdminMintRecord(key string) (store.AdminMintRecord, bool, error) {
	s.mu.Lock()
	s.loads++
	s.mu.Unlock()
	return s.db.LoadAdminMintRecord(key)
}

func (s *failFirstCompletedAdminMintStore) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

// expectedMintPub recomputes spend_pub + height*G for an address.
func expectedMintPub(t *testing.T, addr crypto.Address, height uint64) crypto.Point32 {
	t.Helper()
	_, spendPub, _, err := crypto.DecodeAddress(addr)
	if err != nil {
		t.Fatal("DecodeAddress:", err)
	}
	heightPub, err := crypto.ScalarMulBase(crypto.ScalarFromUint64(height))
	if err != nil {
		t.Fatal("ScalarMulBase:", err)
	}
	pub, err := crypto.AddPoints(spendPub, heightPub)
	if err != nil {
		t.Fatal("AddPoints:", err)
	}
	return pub
}

// TestScheduleAdminMint_CommittedWithHeightUniquePub verifies the happy path:
// the mint is committed in a produced block, and its output one-time pub is
// spend_pub + height*G for the REAL inclusion height (never the legacy
// height=0 spend_pub).
func TestScheduleAdminMint_CommittedWithHeightUniquePub(t *testing.T) {
	eng, chain, stopEng := newMintTestEngine(t, "")
	defer stopEng()

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	const amount = uint64(500_000_000) // 5 APRO
	txHash, height, err := eng.ScheduleAdminMint("happy-path", string(addr), amount, 2*time.Second)
	if err != nil {
		t.Fatalf("ScheduleAdminMint: %v", err)
	}
	if height == 0 {
		t.Fatal("mint reported height 0 — must be a real block height")
	}

	// Wait until the chain tip reaches the reported height, then locate the tx.
	deadline := time.Now().Add(2 * time.Second)
	var block *core.Block
	for time.Now().Before(deadline) {
		if b := chain.GetByHeight(height); b != nil {
			block = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if block == nil {
		t.Fatalf("block at height %d not found on chain", height)
	}

	var mintTx *core.Transaction
	for i := range block.Txs {
		if block.Txs[i].Hash() == txHash {
			mintTx = &block.Txs[i]
			break
		}
	}
	if mintTx == nil {
		t.Fatalf("mint tx %x not found in block %d", txHash[:8], height)
	}
	if !mintTx.IsCoinbase() {
		t.Fatal("mint tx must be coinbase (zero inputs)")
	}

	want := expectedMintPub(t, addr, height)
	if mintTx.Outputs[0].OneTimePub != want {
		t.Fatalf("mint OneTimePub != spend_pub + height*G for height %d", height)
	}

	// Regression guard: the output must NOT be the legacy bare spend_pub.
	_, spendPub, _, err := crypto.DecodeAddress(addr)
	if err != nil {
		t.Fatal(err)
	}
	if mintTx.Outputs[0].OneTimePub == spendPub {
		t.Fatal("mint OneTimePub equals bare spend_pub — legacy height=0 path regressed")
	}
}

// TestScheduleAdminMint_SameAddressDistinctKeyImages verifies that two mints
// to the SAME address land at different heights and get distinct one-time pubs
// (⇒ distinct key images) — the core property the fix exists to guarantee.
func TestScheduleAdminMint_SameAddressDistinctKeyImages(t *testing.T) {
	eng, chain, stopEng := newMintTestEngine(t, "")
	defer stopEng()

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	type res struct {
		txHash crypto.Hash32
		height uint64
		err    error
	}
	results := make(chan res, 2)
	for i := 0; i < 2; i++ {
		amount := uint64(100_000_000 * (i + 1))
		go func(amt uint64) {
			h, ht, err := eng.ScheduleAdminMint(fmt.Sprintf("same-%d", amt), string(addr), amt, 3*time.Second)
			results <- res{h, ht, err}
		}(amount)
	}

	var a, b res
	a = <-results
	b = <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("mint errors: %v / %v", a.err, b.err)
	}
	if a.height == b.height {
		t.Fatalf("two mints to one address committed at the SAME height %d — shared key image", a.height)
	}

	// Distinct heights ⇒ distinct one-time pubs.  Verify directly on-chain.
	pubs := make(map[crypto.Point32]bool)
	for _, r := range []res{a, b} {
		blk := chain.GetByHeight(r.height)
		if blk == nil {
			t.Fatalf("block %d missing", r.height)
		}
		found := false
		for i := range blk.Txs {
			if blk.Txs[i].Hash() == r.txHash {
				pubs[blk.Txs[i].Outputs[0].OneTimePub] = true
				found = true
			}
		}
		if !found {
			t.Fatalf("mint tx %x not in block %d", r.txHash[:8], r.height)
		}
	}
	if len(pubs) != 2 {
		t.Fatal("mints to one address share a one-time pub — key image collision")
	}
}

// TestScheduleAdminMint_RewardAddressRejected verifies that minting to the
// validator reward address is refused: the per-block coinbase reward would
// produce a colliding one-time pub in the same block.
func TestScheduleAdminMint_RewardAddressRejected(t *testing.T) {
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	rewardAddr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	eng, _, stopEng := newMintTestEngine(t, string(rewardAddr))
	defer stopEng()

	_, _, err = eng.ScheduleAdminMint("reward", string(rewardAddr), 100_000_000, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected rejection when minting to the reward address")
	}
}

// TestScheduleAdminMint_ManyMintsRespectCoinbaseCap verifies that queueing
// more mints than fit under maxCoinbasesPerBlock does NOT stall block
// production: the engine spreads them across several blocks and every mint
// eventually commits (regression guard for the over-capacity stall, where an
// over-limit block failed the engine's own validateCoinbasePolicy forever).
func TestScheduleAdminMint_ManyMintsRespectCoinbaseCap(t *testing.T) {
	eng, chain, stopEng := newMintTestEngine(t, "")
	defer stopEng()

	const n = 14 // > maxCoinbasesPerBlock (11)
	type res struct {
		height uint64
		err    error
	}
	results := make(chan res, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			keys, err := crypto.GenerateWalletKeys()
			if err != nil {
				results <- res{err: err}
				return
			}
			addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)
			_, ht, err := eng.ScheduleAdminMint(fmt.Sprintf("many-%d", i), string(addr), uint64(100_000_000+i), 5*time.Second)
			results <- res{height: ht, err: err}
		}(i)
	}

	perHeight := make(map[uint64]int)
	for i := 0; i < n; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("mint %d failed: %v", i, r.err)
		}
		perHeight[r.height]++
	}
	for h, count := range perHeight {
		blk := chain.GetByHeight(h)
		if blk == nil {
			t.Fatalf("block %d missing", h)
		}
		coinbases := 0
		for i := range blk.Txs {
			if blk.Txs[i].IsCoinbase() {
				coinbases++
			}
		}
		if coinbases > 11 {
			t.Fatalf("block %d has %d coinbases (> maxCoinbasesPerBlock)", h, coinbases)
		}
		if count > coinbases {
			t.Fatalf("block %d: %d mints reported but only %d coinbases present", h, count, coinbases)
		}
	}
}

// TestScheduleAdminMint_TimeoutWhenNotProducing verifies that a mint queued on
// an idle engine (never started) times out with an explicit error instead of
// hanging, and does NOT report a tx hash.
func TestScheduleAdminMint_TimeoutWhenNotProducing(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	genesisHdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHdr.Sign(validatorPriv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: genesisHdr}); err != nil {
		t.Fatal(err)
	}
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock:         noopCanonicalPersistence,
		BlockTime:                20 * time.Millisecond,
		BFTThreshold:             0.667,
		Validators:               []crypto.ValidatorPubKey{validatorPub},
		RingCTV4ActivationHeight: ^uint64(0),
	}, chain, mp, newNopLogger()) // Run() never called — engine idle

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	start := time.Now()
	_, _, err = eng.ScheduleAdminMint("idle", string(addr), 100_000_000, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error from idle engine")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}

func TestScheduleAdminMint_TimeoutThenRetryExactlyOnce(t *testing.T) {
	eng, chain, stopEng := newMintTestEngine(t, "")
	defer stopEng()
	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	if _, _, err := eng.ScheduleAdminMint("timeout-retry", string(addr), 321_000_000, time.Nanosecond); err == nil {
		t.Fatal("expected timeout")
	}
	hash, _, err := eng.ScheduleAdminMint("timeout-retry", string(addr), 321_000_000, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for h := uint64(1); h <= chain.Tip().Header.Height; h++ {
		if block := chain.GetByHeight(h); block != nil {
			for i := range block.Txs {
				if block.Txs[i].Hash() == hash {
					count++
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("mint appears %d times, want 1", count)
	}
}

func TestScheduleAdminMint_ConcurrentSameKeyJoins(t *testing.T) {
	eng, _, stopEng := newMintTestEngine(t, "")
	defer stopEng()
	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	type result struct {
		hash   crypto.Hash32
		height uint64
		err    error
	}
	results := make(chan result, 8)
	for i := 0; i < cap(results); i++ {
		go func() {
			hash, height, err := eng.ScheduleAdminMint("concurrent", string(addr), 111_000_000, 2*time.Second)
			results <- result{hash, height, err}
		}()
	}
	first := <-results
	if first.err != nil {
		t.Fatal(first.err)
	}
	for i := 1; i < cap(results); i++ {
		got := <-results
		if got.err != nil || got.hash != first.hash || got.height != first.height {
			t.Fatalf("joined outcome mismatch: %+v vs %+v", got, first)
		}
	}
}

func TestScheduleAdminMint_RestartPersistenceAndConflict(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	eng1, _, stop1 := newMintTestEngineWithStore(t, "", db)
	hash1, height1, err := eng1.ScheduleAdminMint("restart", string(addr), 654_000_000, 2*time.Second)
	if err != nil {
		stop1()
		t.Fatal(err)
	}
	stop1()

	eng2, _, stop2 := newMintTestEngineWithStore(t, "", db)
	defer stop2()
	hash2, height2, err := eng2.ScheduleAdminMint("restart", string(addr), 654_000_000, time.Nanosecond)
	if err != nil {
		t.Fatalf("persisted retry: %v", err)
	}
	if hash1 != hash2 || height1 != height2 {
		t.Fatal("persisted outcome changed across NewEngine")
	}
	if _, _, err := eng2.ScheduleAdminMint("restart", string(addr), 654_000_001, time.Second); err == nil {
		t.Fatal("expected conflicting key reuse error")
	}
}

func reconstructedMintChain(t *testing.T, source *core.Chain, replaceTxAt uint64) *core.Chain {
	t.Helper()
	rebuilt := core.NewChain()
	genesis := source.GetByHeight(0)
	if genesis == nil {
		t.Fatal("source chain has no genesis")
	}
	if err := rebuilt.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	for height := uint64(1); height <= source.Tip().Header.Height; height++ {
		block := source.GetByHeight(height)
		if block == nil {
			t.Fatalf("source chain missing block %d", height)
		}
		if height == replaceTxAt {
			replacement := *block
			replacement.Txs = nil
			block = &replacement
		}
		if err := rebuilt.AddBlock(block); err != nil {
			t.Fatalf("reconstruct block %d: %v", height, err)
		}
	}
	return rebuilt
}

func countMintTransaction(chain *core.Chain, hash crypto.Hash32) int {
	count := 0
	for height := uint64(1); height <= chain.Tip().Header.Height; height++ {
		block := chain.GetByHeight(height)
		if block == nil {
			continue
		}
		for _, tx := range block.Txs {
			if tx.Hash() == hash {
				count++
			}
		}
	}
	return count
}

func newMintTestEngineOnChainWithStore(t *testing.T, chain *core.Chain, db *store.DB) (*consensus.Engine, func()) {
	t.Helper()
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey:", err)
	}
	lk, err := crypto.NewLockedValidatorKey(validatorPriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	utxos := core.NewUTXOSet()
	eng := consensus.NewEngine(consensus.Config{
		OnCanonicalBlock: noopCanonicalPersistence,
		BlockTime: 20 * time.Millisecond, BFTThreshold: 0.667,
		Validators: []crypto.ValidatorPubKey{validatorPub}, Registry: core.NewValidatorRegistry(),
		MyKey: lk, Store: db, RingCTV4ActivationHeight: ^uint64(0),
	}, chain, core.NewMempool(core.DefaultMempoolConfig()), newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	stop := make(chan struct{})
	go eng.Run(stop)
	var once sync.Once
	return eng, func() {
		once.Do(func() {
			close(stop)
			lk.Destroy()
		})
	}
}

func TestScheduleAdminMint_RestartReconcilesPreparedCanonicalTransaction(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	eng1, chain1, stop1 := newMintTestEngineWithStore(t, "", db)
	hash, height, err := eng1.ScheduleAdminMint("prepared-canonical", string(addr), 987_000_000, 2*time.Second)
	if err != nil {
		stop1()
		t.Fatal(err)
	}
	stop1()
	if err := db.StoreAdminMintRecord("prepared-canonical", store.AdminMintRecord{
		State: "prepared", Address: string(addr), AmountNAPR: 987_000_000, TxHash: hash, Height: height,
	}); err != nil {
		t.Fatal(err)
	}

	rebuilt := reconstructedMintChain(t, chain1, 0)
	eng2, stop2 := newMintTestEngineOnChainWithStore(t, rebuilt, db)
	defer stop2()
	hash2, height2, err := eng2.ScheduleAdminMint("prepared-canonical", string(addr), 987_000_000, time.Second)
	if err != nil {
		t.Fatalf("reconcile prepared canonical transaction: %v", err)
	}
	if hash2 != hash || height2 != height {
		t.Fatal("reconciled outcome changed")
	}
	record, found, err := db.LoadAdminMintRecord("prepared-canonical")
	if err != nil || !found || record.State != "completed" {
		t.Fatalf("prepared record was not reconciled to completed: found=%v record=%+v err=%v", found, record, err)
	}
	if count := countMintTransaction(rebuilt, hash); count != 1 {
		t.Fatalf("canonical mint appears %d times, want 1", count)
	}
}

func TestScheduleAdminMint_RestartPreparedNoncanonicalFailsClosed(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	eng1, chain1, stop1 := newMintTestEngineWithStore(t, "", db)
	hash, height, err := eng1.ScheduleAdminMint("prepared-noncanonical", string(addr), 765_000_000, 2*time.Second)
	if err != nil {
		stop1()
		t.Fatal(err)
	}
	stop1()
	if err := db.StoreAdminMintRecord("prepared-noncanonical", store.AdminMintRecord{
		State: "prepared", Address: string(addr), AmountNAPR: 765_000_000, TxHash: hash, Height: height,
	}); err != nil {
		t.Fatal(err)
	}

	rebuilt := reconstructedMintChain(t, chain1, height)
	eng2, stop2 := newMintTestEngineOnChainWithStore(t, rebuilt, db)
	defer stop2()
	if _, _, err := eng2.ScheduleAdminMint("prepared-noncanonical", string(addr), 765_000_000, time.Second); err == nil {
		t.Fatal("expected unresolved prepared operation to fail closed")
	}
	if count := countMintTransaction(rebuilt, hash); count != 0 {
		t.Fatalf("noncanonical prepared mint appears %d times, want 0", count)
	}
}

func TestScheduleAdminMint_CompletedWriteFailureSameProcessRetryReconciles(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	mintStore := &failFirstCompletedAdminMintStore{db: db}
	eng, chain, stopEng := newMintTestEngineWithAdminMintStore(t, "", db, mintStore, nil)
	defer stopEng()

	const key = "completed-write-retry"
	const amount = uint64(876_000_000)
	if _, _, err := eng.ScheduleAdminMint(key, string(addr), amount, 2*time.Second); err == nil ||
		!strings.Contains(err.Error(), "persist committed admin mint outcome") {
		t.Fatalf("first ScheduleAdminMint error = %v, want completed-write error", err)
	}

	prepared, found, err := db.LoadAdminMintRecord(key)
	if err != nil || !found || prepared.State != "prepared" {
		t.Fatalf("record after completed-write failure: found=%v record=%+v err=%v", found, prepared, err)
	}
	hash, height, err := eng.ScheduleAdminMint(key, string(addr), amount, time.Second)
	if err != nil {
		t.Fatalf("same-process retry: %v", err)
	}
	if hash != prepared.TxHash || height != prepared.Height {
		t.Fatalf("reconciled outcome = (%s, %d), want (%s, %d)", hash, height, prepared.TxHash, prepared.Height)
	}
	if count := countMintTransaction(chain, hash); count != 1 {
		t.Fatalf("canonical mint appears %d times, want 1", count)
	}
	record, found, err := db.LoadAdminMintRecord(key)
	if err != nil || !found || record.State != "completed" {
		t.Fatalf("reconciled durable record: found=%v record=%+v err=%v", found, record, err)
	}

	// A completed outcome is not retained in mintByKey either: another retry
	// reloads the durable completed record rather than returning a cached req.
	loads := mintStore.loadCount()
	hash2, height2, err := eng.ScheduleAdminMint(key, string(addr), amount, time.Nanosecond)
	if err != nil || hash2 != hash || height2 != height {
		t.Fatalf("completed retry = (%s, %d, %v), want (%s, %d, nil)", hash2, height2, err, hash, height)
	}
	if got := mintStore.loadCount(); got != loads+1 {
		t.Fatalf("durable load count advanced by %d, want 1 (terminal map entry leaked)", got-loads)
	}
}

func TestScheduleAdminMint_ProducedBlockPersistenceFailureFailsClosed(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	persistErr := errors.New("injected durable block write failure")
	var produced int
	var producedMu sync.Mutex
	eng1, chain1, stop1 := newMintTestEngineWithStoreAndProduced(t, "", db, func(*core.Block) error {
		producedMu.Lock()
		produced++
		producedMu.Unlock()
		return persistErr
	})

	_, _, err = eng1.ScheduleAdminMint("persistence-failure", string(addr), 765_000_000, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "persistence failed") {
		stop1()
		t.Fatalf("ScheduleAdminMint error = %v, want clear persistence error", err)
	}
	record, found, err := db.LoadAdminMintRecord("persistence-failure")
	if err != nil || !found || record.State != "prepared" {
		stop1()
		t.Fatalf("prepared record changed after persistence failure: found=%v record=%+v err=%v", found, record, err)
	}
	if chain1.Tip().Header.Height != record.Height {
		stop1()
		t.Fatalf("in-memory block height = %d, prepared height = %d", chain1.Tip().Header.Height, record.Height)
	}
	if _, _, err := eng1.ScheduleAdminMint("persistence-failure", string(addr), 765_000_000, time.Second); err == nil || !strings.Contains(err.Error(), "persistence failed") {
		stop1()
		t.Fatalf("same-process retry error = %v, want retained persistence error", err)
	}
	time.Sleep(3 * 20 * time.Millisecond)
	producedMu.Lock()
	gotProduced := produced
	producedMu.Unlock()
	if gotProduced != 1 {
		stop1()
		t.Fatalf("persistence-failed engine produced %d blocks, want 1", gotProduced)
	}
	stop1()

	rebuilt := core.NewChain()
	if err := rebuilt.SetGenesis(chain1.GetByHeight(0)); err != nil {
		t.Fatal(err)
	}
	eng2, stop2 := newMintTestEngineOnChainWithStore(t, rebuilt, db)
	defer stop2()
	if _, _, err := eng2.ScheduleAdminMint("persistence-failure", string(addr), 765_000_000, time.Second); err == nil || !strings.Contains(err.Error(), "unresolved prepared") {
		t.Fatalf("reconstructed-chain retry error = %v, want unresolved prepared failure", err)
	}
}

func TestScheduleAdminMint_RestartIntentFailsClosed(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys, _ := crypto.GenerateWalletKeys()
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	if err := db.StoreAdminMintRecord("persisted-intent", store.AdminMintRecord{
		State: "intent", Address: string(addr), AmountNAPR: 432_000_000,
	}); err != nil {
		t.Fatal(err)
	}

	eng, chain, stopEng := newMintTestEngineWithStore(t, "", db)
	defer stopEng()
	if _, _, err := eng.ScheduleAdminMint("persisted-intent", string(addr), 432_000_000, time.Second); err == nil {
		t.Fatal("expected unresolved persisted intent to fail closed")
	}
	if count := countMintTransaction(chain, crypto.Hash32{}); count != 0 {
		t.Fatalf("unexpected mint transaction count %d", count)
	}
}
