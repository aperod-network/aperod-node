package consensus

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func finalityVoteEngineForTest(t *testing.T, chain *core.Chain, pubs []crypto.ValidatorPubKey, db *store.DB) *Engine {
	t.Helper()
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis(pubs, core.MinStakeNAPR*10)
	return NewEngine(Config{
		Validators: pubs, Registry: registry, Store: db, BFTThreshold: 0.667,
		SecurityHardeningActivationHeight: 1,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func signedFinalityVoteForTest(t *testing.T, priv crypto.ValidatorPrivKey, pub crypto.ValidatorPubKey, height uint64, hash crypto.Hash32) FinalizeMsg {
	t.Helper()
	version := crypto.FinalityVoteVersionHeightBound
	message := crypto.FinalityVoteMessage(version, height, hash)
	signature, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	return FinalizeMsg{
		Height: height, BlockHash: hash, Version: version,
		ValidatorPub: pub, Signature: signature,
	}
}

func TestPendingFinalityVotesRestoreAndCompleteQuorumAfterRestart(t *testing.T) {
	var privs []crypto.ValidatorPrivKey
	var pubs []crypto.ValidatorPubKey
	for range 3 {
		priv, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		privs, pubs = append(privs, priv), append(pubs, pub)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chain, block := finalityTestChain(t, "pending-votes")
	engine := finalityVoteEngineForTest(t, chain, pubs, db)
	for i := 0; i < 2; i++ {
		if err := engine.HandleVote(signedFinalityVoteForTest(t, privs[i], pubs[i], 1, block.Hash())); err != nil {
			t.Fatal(err)
		}
	}
	if engine.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("two of three votes unexpectedly finalized the block")
	}

	_, replacementPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	advancedRegistry := core.NewValidatorRegistry()
	advancedRegistry.InitFromGenesis([]crypto.ValidatorPubKey{replacementPub}, core.MinStakeNAPR*10)
	restarted := NewEngine(Config{
		Validators: pubs, Registry: advancedRegistry, Store: db,
		BFTThreshold: 0.667, SecurityHardeningActivationHeight: 1,
	}, chain, core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := len(restarted.votes[block.Hash()]); got != 2 {
		t.Fatalf("restored pending votes = %d, want 2", got)
	}
	if err := restarted.HandleVote(signedFinalityVoteForTest(t, privs[2], pubs[2], 1, block.Hash())); err != nil {
		t.Fatal(err)
	}
	if !restarted.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("restored partial quorum did not complete after restart")
	}
	certificate, err := db.LoadFinalityCertificateAt(1)
	if err != nil {
		t.Fatal(err)
	}
	if certificate == nil || len(certificate.Votes) != 3 {
		t.Fatalf("persisted completion certificate = %#v", certificate)
	}
	for _, vote := range certificate.Votes {
		if vote.VoteVersion != crypto.FinalityVoteVersionHeightBound {
			t.Fatalf("certificate vote signing version = %d, want height-bound v2", vote.VoteVersion)
		}
	}
}

func TestHeightBoundVoteEquivocationPersistsAcrossRestart(t *testing.T) {
	var privs []crypto.ValidatorPrivKey
	var pubs []crypto.ValidatorPubKey
	for range 3 {
		priv, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		privs, pubs = append(privs, priv), append(pubs, pub)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chain, block := finalityTestChain(t, "equivocation")
	engine := finalityVoteEngineForTest(t, chain, pubs, db)
	first := signedFinalityVoteForTest(t, privs[0], pubs[0], 1, block.Hash())
	if err := engine.HandleVote(first); err != nil {
		t.Fatal(err)
	}

	restarted := finalityVoteEngineForTest(t, chain, pubs, db)
	conflictingHash := crypto.HashStr("alternate-height-one-block")
	conflicting := signedFinalityVoteForTest(t, privs[0], pubs[0], 1, conflictingHash)
	if err := restarted.HandleVote(conflicting); err == nil {
		t.Fatal("conflicting height-bound vote was accepted")
	}
	if got := len(restarted.votes[block.Hash()]); got != 1 {
		t.Fatalf("canonical vote count after equivocation = %d, want 1", got)
	}
	evidence, err := db.LoadVoteEquivocationEvidence(1, pubs[0])
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil || evidence.BlockHash1 != block.Hash() || evidence.BlockHash2 != conflictingHash {
		t.Fatalf("durable equivocation evidence = %#v", evidence)
	}
	for _, item := range []struct {
		hash      crypto.Hash32
		signature []byte
	}{{evidence.BlockHash1, evidence.Signature1}, {evidence.BlockHash2, evidence.Signature2}} {
		message := crypto.FinalityVoteMessage(crypto.FinalityVoteVersionHeightBound, 1, item.hash)
		if !pubs[0].Verify(message, item.signature) {
			t.Fatal("persisted equivocation evidence contains an invalid signature")
		}
	}
}

func TestInvalidCandidateVoteDoesNotPoisonCanonicalVote(t *testing.T) {
	var privs []crypto.ValidatorPrivKey
	var pubs []crypto.ValidatorPubKey
	for range 3 {
		priv, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		privs, pubs = append(privs, priv), append(pubs, pub)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chain, block := finalityTestChain(t, "invalid-candidate")
	engine := finalityVoteEngineForTest(t, chain, pubs, db)
	invalidHash := crypto.HashStr("signed-but-noncanonical-block")
	invalidVote := signedFinalityVoteForTest(t, privs[0], pubs[0], 1, invalidHash)
	if err := engine.HandleVote(invalidVote); err == nil {
		t.Fatal("vote for a noncanonical block was accepted")
	}
	if prior, err := db.LoadFinalityVote(1, pubs[0]); err != nil || prior != nil {
		t.Fatalf("invalid candidate poisoned durable vote index: vote=%#v err=%v", prior, err)
	}
	canonicalVote := signedFinalityVoteForTest(t, privs[0], pubs[0], 1, block.Hash())
	if err := engine.HandleVote(canonicalVote); err != nil {
		t.Fatalf("valid canonical vote was blocked by the invalid candidate: %v", err)
	}
	if got := len(engine.votes[block.Hash()]); got != 1 {
		t.Fatalf("canonical vote count = %d, want 1", got)
	}
	evidence, err := db.LoadVoteEquivocationEvidence(1, pubs[0])
	if err != nil {
		t.Fatal(err)
	}
	if evidence == nil || evidence.BlockHash1 != invalidHash || evidence.BlockHash2 != block.Hash() {
		t.Fatalf("valid conflicting signatures were not retained as evidence: %#v", evidence)
	}
	if !pubs[0].Verify(crypto.FinalityVoteMessage(
		crypto.FinalityVoteVersionHeightBound, 1, invalidHash), evidence.Signature1) ||
		!pubs[0].Verify(crypto.FinalityVoteMessage(
			crypto.FinalityVoteVersionHeightBound, 1, block.Hash()), evidence.Signature2) {
		t.Fatal("candidate equivocation evidence contains invalid signatures")
	}
}

func TestRestoredLocalVoteRebroadcastsExactPersistedSignature(t *testing.T) {
	chain, block := finalityTestChain(t, "local-vote-rebroadcast")
	signature := bytes.Repeat([]byte{0x5a}, 64)
	restored := FinalizeMsg{
		Height: 1, BlockHash: block.Hash(), Version: crypto.FinalityVoteVersionHeightBound,
		Signature: signature,
	}
	var got *FinalizeMsg
	engine := &Engine{
		cfg: Config{OnVoteCast: func(vote FinalizeMsg) {
			copy := cloneFinalizeMsg(vote)
			got = &copy
		}},
		chain: chain, latestLocalVote: &restored, finalized: make(map[uint64]bool),
	}
	engine.rebroadcastRestoredLocalVote()
	if got == nil {
		t.Fatal("unfinalized restored local vote was not rebroadcast")
	}
	if got.Height != restored.Height || got.BlockHash != restored.BlockHash ||
		got.Version != restored.Version || !bytes.Equal(got.Signature, signature) {
		t.Fatal("restored local vote was changed instead of rebroadcast exactly")
	}
}
