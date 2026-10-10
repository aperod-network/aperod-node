package consensus

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestLocalProductionReplaysAndPrunesBoundedFutureVotes(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Destroy()
	genesisHeader := core.BlockHeader{
		Height: 0, Timestamp: time.Now().UnixNano(),
		ValidatorPub: pub, MerkleRoot: core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(priv); err != nil {
		t.Fatal(err)
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&core.Block{Header: genesisHeader}); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(Config{
		OnCanonicalBlock: func(*core.Block, *avm.PreparedBlock) error { return nil },
		BlockTime:        20 * time.Millisecond, BFTThreshold: 0.667,
		Validators: []crypto.ValidatorPubKey{pub}, MyKey: key,
		RingCTV4ActivationHeight: ^uint64(0),
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	utxos := core.NewUTXOSet()
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	// These authenticated peer votes arrive before the local proposal at h=1.
	// None identifies a canonical block yet, so they are staged but cannot
	// create finality. Different hashes exercise the fixed queue bound.
	for i := 0; i < futureVoteLimit; i++ {
		hash := crypto.HashBytes([]byte{byte(i), byte(i >> 8), 0xa5})
		message := crypto.HashBytes([]byte("aperod/finalize/v1"), hash[:])
		signature, err := priv.Sign(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.HandleVote(FinalizeMsg{
			Height: 1, BlockHash: hash, ValidatorPub: pub, Signature: signature,
		}); err != nil {
			t.Fatalf("staged authenticated vote %d: %v", i, err)
		}
	}
	if len(engine.futureVotes) != futureVoteLimit {
		t.Fatalf("staged queue size = %d, want %d", len(engine.futureVotes), futureVoteLimit)
	}
	extraHash := crypto.HashBytes([]byte("queue overflow"))
	extraMessage := crypto.HashBytes([]byte("aperod/finalize/v1"), extraHash[:])
	extraSignature, err := priv.Sign(extraMessage)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.HandleVote(FinalizeMsg{
		Height: 1, BlockHash: extraHash, ValidatorPub: pub, Signature: extraSignature,
	}); err == nil {
		t.Fatal("vote queue accepted an entry beyond its bound")
	}
	if len(engine.finalized) != 0 {
		t.Fatal("staged votes created phantom finality before local proposal")
	}

	if err := engine.tick(); err != nil {
		t.Fatalf("local block production failed: %v", err)
	}
	produced := <-engine.ProducedCh()
	if produced.Header.Height != 1 || produced.Hash() != chain.Tip().Hash() {
		t.Fatal("local production did not accept the expected canonical block")
	}
	if len(engine.futureVotes) != 0 {
		t.Fatalf("staged votes for the losing candidate were not pruned: remaining=%d", len(engine.futureVotes))
	}
	if !engine.IsFinalizedHash(1, produced.Hash()) || len(engine.finalized) != 1 {
		t.Fatal("local block did not finalize solely from its actual local vote")
	}
}
