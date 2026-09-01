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
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// newMintTestEngine spins up a single-validator engine producing blocks every
// 20 ms, returning the engine, its chain, and a stop func.
func newMintTestEngine(t *testing.T, rewardAddress string) (*consensus.Engine, *core.Chain, func()) {
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
		BlockTime:     20 * time.Millisecond,
		BFTThreshold:  0.667,
		Validators:    []crypto.ValidatorPubKey{validatorPub},
		Registry:      reg,
		MyKey:         lk,
		RewardAddress: rewardAddress,
		// These tests cover the legacy admin-mint scheduler.  Keep them below
		// the hard-fork boundary; post-activation mints require an on-chain
		// authorization format that these legacy fixtures do not carry.
		RingCTV4ActivationHeight: ^uint64(0),
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	go eng.Run(stop)
	var once sync.Once
	return eng, chain, func() {
		once.Do(func() {
			close(stop)
			lk.Destroy()
		})
	}
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
	txHash, height, err := eng.ScheduleAdminMint(string(addr), amount, 2*time.Second)
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
			h, ht, err := eng.ScheduleAdminMint(string(addr), amt, 3*time.Second)
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

	_, _, err = eng.ScheduleAdminMint(string(rewardAddr), 100_000_000, 500*time.Millisecond)
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
			_, ht, err := eng.ScheduleAdminMint(string(addr), uint64(100_000_000+i), 5*time.Second)
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
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{validatorPub},
		RingCTV4ActivationHeight: ^uint64(0),
	}, chain, mp, newNopLogger()) // Run() never called — engine idle

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	addr := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	start := time.Now()
	_, _, err = eng.ScheduleAdminMint(string(addr), 100_000_000, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error from idle engine")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
}
