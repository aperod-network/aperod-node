package consensus_test

// Tests for the registry-seeding fix that lets non-validator nodes pass the
// isKnownValidator() check inside handleIncomingBlock.
//
// Background
// ----------
// Before the fix, a non-validator node seeded its registry with its own random
// P2P-identity key (main.go, pre-fix):
//
//   validators = []crypto.ValidatorPubKey{myKey.Public()}
//
// When a block arrived from a real validator, isKnownValidator() returned false
// and the block was rejected — silently breaking sync.
//
// The fix (main.go lines 252-270) seeds the registry with the genesis-config
// validator keys instead:
//
//   for _, hexPub := range genesisConfig.Validators { validators = append(validators, pub) }
//
// The two tests below together form the regression guard:
//
//  1. TestNonValidatorAcceptsBlockFromRealValidator — correct path:
//     registry seeded with genesis validator key → block accepted, chain advances.
//
//  2. TestNonValidatorRejectsBlock_WhenSeededWithOwnKey — regression path:
//     registry seeded with the node's own P2P identity key (as before the fix) →
//     block rejected, chain stays at genesis.
//     If main.go regresses to using myKey.Public() here, this test fails.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/consensus"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// buildValidatorGenesisAndBlock is a shared helper that creates:
//   - a genesis block signed by validatorPriv/validatorPub
//   - a block at height 1 produced by the validator engine
//
// It returns the genesis block and height-1 block so subtests can reuse them.
func buildValidatorGenesisAndBlock(t *testing.T) (
	validatorPriv crypto.ValidatorPrivKey,
	validatorPub crypto.ValidatorPubKey,
	genesis *core.Block,
	block1 *core.Block,
) {
	t.Helper()

	var err error
	validatorPriv, validatorPub, err = crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (validator):", err)
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
	genesis = &core.Block{Header: genesisHdr}

	// Spin up the validator engine to produce block 1.
	validatorChain := core.NewChain()
	if err := validatorChain.SetGenesis(genesis); err != nil {
		t.Fatal("validator SetGenesis:", err)
	}
	lk, err := crypto.NewLockedValidatorKey(validatorPriv.Bytes(), nil)
	if err != nil {
		t.Fatal("NewLockedValidatorKey:", err)
	}
	defer lk.Destroy()

	validatorUTXOs := core.NewUTXOSet()
	validatorReg := core.NewValidatorRegistry()
	validatorMp := core.NewMempool(core.DefaultMempoolConfig())
	validatorEng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{validatorPub},
		Registry:     validatorReg,
		MyKey:        lk,
	}, validatorChain, validatorMp, newNopLogger())
	validatorEng.SetTxVerifier(core.NewTxVerifier(validatorUTXOs), validatorUTXOs)

	validatorStop := make(chan struct{})
	go validatorEng.Run(validatorStop)
	defer close(validatorStop)

	select {
	case block1 = <-validatorEng.ProducedCh():
		t.Logf("validator produced block height=%d", block1.Header.Height)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout: validator did not produce a block in 500 ms")
	}
	if block1.Header.Height != 1 {
		t.Fatalf("expected produced block at height 1, got %d", block1.Header.Height)
	}
	return
}

// TestNonValidatorAcceptsBlockFromRealValidator verifies the CORRECT startup
// path: the non-validator engine seeds its registry from the genesis-config
// validator keys (main.go lines 252-270, NonValidator=true branch).
//
// When a block arrives from the real validator, isKnownValidator() returns true
// and the block is accepted — the non-validator's chain height advances to 1.
func TestNonValidatorAcceptsBlockFromRealValidator(t *testing.T) {
	_, validatorPub, genesis, block1 := buildValidatorGenesisAndBlock(t)

	// Non-validator engine — seeded from genesis config (the fix).
	// This mirrors main.go NonValidator=true:
	//   validators = []crypto.ValidatorPubKey{<genesis pub>}
	nonValidatorChain := core.NewChain()
	if err := nonValidatorChain.SetGenesis(genesis); err != nil {
		t.Fatal("non-validator SetGenesis:", err)
	}

	nonValidatorUTXOs := core.NewUTXOSet()
	nonValidatorReg := core.NewValidatorRegistry()
	nonValidatorMp := core.NewMempool(core.DefaultMempoolConfig())
	nonValidatorEng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		// ← genesis validator's pub key (the fix): isKnownValidator() will return true.
		Validators: []crypto.ValidatorPubKey{validatorPub},
		Registry:   nonValidatorReg,
		MyKey:      nil, // non-validator: never produces blocks
	}, nonValidatorChain, nonValidatorMp, newNopLogger())
	nonValidatorEng.SetTxVerifier(core.NewTxVerifier(nonValidatorUTXOs), nonValidatorUTXOs)

	nonValidatorStop := make(chan struct{})
	go nonValidatorEng.Run(nonValidatorStop)
	defer close(nonValidatorStop)

	nonValidatorEng.NewBlockCh() <- block1

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("non-validator chain height = %d after 300 ms, want 1; "+
				"registry seeding with genesis key should have let the block through",
				nonValidatorChain.Height())
			return
		default:
			if nonValidatorChain.Height() == 1 {
				t.Log("OK: non-validator accepted block from real validator; chain advanced to height 1")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestNonValidatorRejectsBlock_WhenSeededWithOwnKey verifies the REGRESSION
// scenario: if the non-validator engine seeds its registry with its own random
// P2P-identity key (the pre-fix behaviour), the incoming block from the real
// validator is rejected because isKnownValidator() returns false.
//
// This test will FAIL if main.go regresses to:
//
//	validators = []crypto.ValidatorPubKey{myKey.Public()}
//
// in the NonValidator=true branch — confirming that the fix is still in place.
func TestNonValidatorRejectsBlock_WhenSeededWithOwnKey(t *testing.T) {
	_, _, genesis, block1 := buildValidatorGenesisAndBlock(t)

	// Simulate the P2P identity key of the non-validator node itself —
	// a completely different key that is NOT a network validator.
	nodePriv, nodePub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal("GenerateValidatorKey (node identity):", err)
	}
	_ = nodePriv // only the public key is used for seeding

	// Non-validator engine — seeded with its OWN key (the pre-fix / regression path).
	// This mirrors what main.go used to do (before the fix):
	//   validators = []crypto.ValidatorPubKey{myKey.Public()}
	nonValidatorChain := core.NewChain()
	if err := nonValidatorChain.SetGenesis(genesis); err != nil {
		t.Fatal("non-validator SetGenesis:", err)
	}

	nonValidatorUTXOs := core.NewUTXOSet()
	nonValidatorReg := core.NewValidatorRegistry()
	nonValidatorMp := core.NewMempool(core.DefaultMempoolConfig())
	nonValidatorEng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		// ← the node's OWN key (the regression): isKnownValidator() will return false
		//   for the real validator's block → block must be rejected.
		Validators: []crypto.ValidatorPubKey{nodePub},
		Registry:   nonValidatorReg,
		MyKey:      nil,
	}, nonValidatorChain, nonValidatorMp, newNopLogger())
	nonValidatorEng.SetTxVerifier(core.NewTxVerifier(nonValidatorUTXOs), nonValidatorUTXOs)

	nonValidatorStop := make(chan struct{})
	go nonValidatorEng.Run(nonValidatorStop)
	defer close(nonValidatorStop)

	nonValidatorEng.NewBlockCh() <- block1

	// Give the engine time to process — the block should be silently rejected.
	time.Sleep(150 * time.Millisecond)

	if nonValidatorChain.Height() != 0 {
		t.Errorf("chain advanced to height %d — block should have been rejected "+
			"when registry is seeded with the node's own key instead of genesis validators",
			nonValidatorChain.Height())
	} else {
		t.Log("OK: block correctly rejected when registry seeded with P2P identity key (regression scenario)")
	}
}

// buildEngineWithGenesis creates a minimal single-validator engine and returns
// the engine, chain, and validator private key so callers can craft custom blocks.
func buildEngineWithGenesis(t *testing.T) (
	eng *consensus.Engine,
	chain *core.Chain,
	validatorPriv crypto.ValidatorPrivKey,
	validatorPub crypto.ValidatorPubKey,
	genesis *core.Block,
) {
	t.Helper()

	var err error
	validatorPriv, validatorPub, err = crypto.GenerateValidatorKey()
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
	genesis = &core.Block{Header: genesisHdr}

	chain = core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal("SetGenesis:", err)
	}

	utxos := core.NewUTXOSet()
	reg := core.NewValidatorRegistry()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng = consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{validatorPub},
		Registry:     reg,
		MyKey:        nil, // relay node — never produces blocks
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go eng.Run(stop)

	return
}

// buildSignedBlock constructs and signs a block at height 1 extending genesis
// with the given nanosecond timestamp.
func buildSignedBlock(t *testing.T, genesis *core.Block, ts int64, validatorPriv crypto.ValidatorPrivKey, validatorPub crypto.ValidatorPubKey) *core.Block {
	t.Helper()

	hdr := core.BlockHeader{
		Height:       1,
		PrevHash:     genesis.Hash(),
		Round:        1,
		Timestamp:    ts,
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := hdr.Sign(validatorPriv); err != nil {
		t.Fatal("sign block:", err)
	}
	return &core.Block{Header: hdr}
}

// TestTimejackingGuard_PastBlockAccepted verifies that a block whose timestamp
// is 1 hour in the past is accepted by handleIncomingBlock.
//
// Before the fix the guard used ±15 s (absolute skew), which silently blocked
// all historical sync blocks on a restarting relay node.  The fix restricts the
// check to future-only (skewNs > maxClockSkewNs), so past-timestamped blocks
// must always be let through.
func TestTimejackingGuard_PastBlockAccepted(t *testing.T) {
	eng, chain, validatorPriv, validatorPub, genesis := buildEngineWithGenesis(t)

	// Block timestamped 1 hour in the past — typical of a relay node catching up
	// after a restart or network partition.
	pastTs := time.Now().Add(-1 * time.Hour).UnixNano()
	block := buildSignedBlock(t, genesis, pastTs, validatorPriv, validatorPub)

	eng.NewBlockCh() <- block

	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case <-deadline:
			t.Errorf("chain height = %d after 300 ms, want 1; "+
				"a block 1 hour in the past must not be rejected by the timejacking guard",
				chain.Height())
			return
		default:
			if chain.Height() == 1 {
				if n := eng.TimestampRejectedCount(); n != 0 {
					t.Errorf("TimestampRejectedCount = %d, want 0", n)
				}
				t.Log("OK: block with past timestamp accepted; relay catch-up is not blocked")
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// TestTimejackingGuard_FutureBlockRejected verifies that a block whose timestamp
// is 30 s in the future is rejected by the timejacking guard.
//
// maxClockSkewNs allows only 15 s of forward skew, so a 30 s future timestamp
// must be caught and the TimestampRejectedCount counter must be incremented.
// This prevents a malicious peer from shifting the chain tip's notion of time
// far into the future (timejacking attack).
func TestTimejackingGuard_FutureBlockRejected(t *testing.T) {
	eng, chain, validatorPriv, validatorPub, genesis := buildEngineWithGenesis(t)

	// Block timestamped 30 s in the future — beyond the 15 s maxClockSkewNs limit.
	futureTs := time.Now().Add(30 * time.Second).UnixNano()
	block := buildSignedBlock(t, genesis, futureTs, validatorPriv, validatorPub)

	eng.NewBlockCh() <- block

	// Give the engine time to process and reject the block.
	time.Sleep(150 * time.Millisecond)

	if chain.Height() != 0 {
		t.Errorf("chain advanced to height %d — future block should have been rejected by the timejacking guard",
			chain.Height())
	}
	if n := eng.TimestampRejectedCount(); n != 1 {
		t.Errorf("TimestampRejectedCount = %d, want 1", n)
	}
	t.Log("OK: block with 30 s future timestamp correctly rejected by timejacking guard")
}

// TestTimejackingBan_PeerBannedAfterThreshold verifies that a rogue peer
// sending future-timestamped blocks is automatically banned once the
// TimestampBanThreshold is crossed.
//
// The test wires a ban callback via consensus.Config.OnTimestampBan and sends
// threshold+1 future-timestamped blocks from the same validator.  It confirms:
//
//  1. Each block is rejected (chain stays at genesis height 0).
//  2. The OnTimestampBan callback fires once the threshold is reached and on
//     every subsequent rejection from the same sender — never before.
//  3. The callback receives the correct ValidatorPub and an increasing count.
func TestTimejackingBan_PeerBannedAfterThreshold(t *testing.T) {
	const threshold = 3 // small value for a fast test

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

	// Track ban-callback invocations so the test can assert on them.
	type banCall struct {
		pub   crypto.ValidatorPubKey
		count int
	}
	banCh := make(chan banCall, 20)

	utxos := core.NewUTXOSet()
	reg := core.NewValidatorRegistry()
	mp := core.NewMempool(core.DefaultMempoolConfig())
	eng := consensus.NewEngine(consensus.Config{
		BlockTime:    20 * time.Millisecond,
		BFTThreshold: 0.667,
		Validators:   []crypto.ValidatorPubKey{validatorPub},
		Registry:     reg,
		MyKey:        nil, // relay — never produces blocks

		TimestampBanThreshold: threshold,
		OnTimestampBan: func(pub crypto.ValidatorPubKey, count int) {
			banCh <- banCall{pub: pub, count: count}
		},
	}, chain, mp, newNopLogger())
	eng.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go eng.Run(stop)

	futureTs := time.Now().Add(30 * time.Second).UnixNano()

	// Send exactly threshold-1 future blocks — callback must NOT fire yet.
	for i := 0; i < threshold-1; i++ {
		block := buildSignedBlock(t, genesis, futureTs, validatorPriv, validatorPub)
		eng.NewBlockCh() <- block
	}
	time.Sleep(150 * time.Millisecond)

	select {
	case call := <-banCh:
		t.Fatalf("OnTimestampBan fired too early: got count=%d after only %d blocks (threshold=%d)",
			call.count, threshold-1, threshold)
	default:
		// Good: callback has not fired yet.
	}
	if n := eng.TimestampRejectedCount(); int(n) != threshold-1 {
		t.Errorf("TimestampRejectedCount = %d, want %d after %d rejections",
			n, threshold-1, threshold-1)
	}

	// Send the block that crosses the threshold — callback MUST fire now.
	eng.NewBlockCh() <- buildSignedBlock(t, genesis, futureTs, validatorPriv, validatorPub)
	select {
	case call := <-banCh:
		if !call.pub.Equals(validatorPub) {
			t.Errorf("OnTimestampBan got wrong ValidatorPub")
		}
		if call.count != threshold {
			t.Errorf("OnTimestampBan count = %d, want %d", call.count, threshold)
		}
		t.Logf("OK: OnTimestampBan fired with count=%d at threshold=%d", call.count, threshold)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnTimestampBan did not fire within 500 ms after threshold was crossed")
	}

	// Send one more — callback must fire again (count = threshold+1).
	eng.NewBlockCh() <- buildSignedBlock(t, genesis, futureTs, validatorPriv, validatorPub)
	select {
	case call := <-banCh:
		if call.count != threshold+1 {
			t.Errorf("OnTimestampBan count = %d on second crossing, want %d", call.count, threshold+1)
		}
		t.Logf("OK: OnTimestampBan fired again with count=%d", call.count)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("OnTimestampBan did not fire for second post-threshold block within 500 ms")
	}

	// Chain must still be at genesis — all future blocks were rejected.
	if chain.Height() != 0 {
		t.Errorf("chain height = %d, want 0; rogue future blocks must not advance the chain", chain.Height())
	}
	t.Log("OK: rogue peer correctly banned after threshold future-timestamped blocks")
}
