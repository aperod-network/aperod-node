package consensus

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func authorityEngineFixture(t *testing.T) (*Engine, crypto.ValidatorPrivKey, crypto.ValidatorPrivKey) {
	t.Helper()
	e, _, parent, old := lpodConsensusFixture(t)
	next, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(next.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(key.Destroy)
	// Preserve a realistic sole-validator nominal stake, not an empty registry.
	snap := e.cfg.Registry.TakeSnapshot()
	snap.Validators[old.Public().Hex()].StakeNAPR = 1_000_000 * core.BaseUnitsPerAPR
	e.cfg.Registry.RestoreFromSnapshot(snap)
	raw, _ := json.Marshal(snap)
	f := &core.ValidatorAuthorityFork{Version: 1, Height: 2, Parent: parent.Hash(),
		Genesis: e.chain.Genesis().Hash(), Identity: old.Public(), Replacement: pub,
		RegistrySHA256: crypto.Hash32(sha256.Sum256(raw))}
	f.Possession, err = next.Sign(f.Message())
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	cfg.MyKey, cfg.ValidatorAuthorityFork = key, f
	e = NewEngine(cfg, e.chain, e.pool, e.log)
	utxos := core.NewUTXOSet()
	e.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	if e.halted.Load() {
		t.Fatal("fixture authority fork rejected")
	}
	return e, old, next
}

func TestAuthorityForkLPoDBlockFinalityAndRestart(t *testing.T) {
	e, old, next := authorityEngineFixture(t)
	before, _ := json.Marshal(e.cfg.Registry.TakeSnapshot())
	if err := e.tick(); err != nil {
		t.Fatal(err)
	}
	b := e.chain.Tip()
	if b.Header.Height != 2 || !b.Header.ValidatorPub.Equals(old.Public()) {
		t.Fatal("collateral identity changed or block not produced")
	}
	if b.Header.VerifySignature() || !b.Header.VerifySignature(next.Public()) {
		t.Fatal("future block did not exclusively use replacement authority")
	}
	if len(b.Txs) != 2 || !b.Txs[1].IsLPoD() {
		t.Fatal("real LPoD settlement missing")
	}
	if !e.IsFinalized(2) {
		t.Fatal("replacement finality vote did not finalize block")
	}
	cert, err := e.cfg.Store.LoadFinalityCertificate()
	if err != nil || cert == nil || len(cert.Votes) != 1 {
		t.Fatal("replacement certificate not persisted")
	}
	if !cert.Votes[0].Validator.Equals(old.Public()) {
		t.Fatal("certificate accounting identity changed")
	}
	after, _ := json.Marshal(e.cfg.Registry.TakeSnapshot())
	if string(before) != string(after) {
		t.Fatal("stake/nonce/generation changed")
	}
	rebuilt := NewEngine(e.cfg, e.chain, e.pool, e.log)
	if rebuilt.halted.Load() || !rebuilt.IsFinalized(2) {
		t.Fatal("cannot restore replacement-signed finality")
	}
	entry, ok := rebuilt.cfg.Registry.GetEntry(old.Public())
	if !ok || entry.StakeNAPR != 1_000_000*core.BaseUnitsPerAPR {
		t.Fatal("stake not preserved on restart")
	}
}

func TestAuthorityForkSecondRotationKeepsOldBlocksValid(t *testing.T) {
	e, old, firstReplacement := authorityEngineFixture(t)
	if err := e.tick(); err != nil {
		t.Fatalf("produce first replacement block: %v", err)
	}
	historical := e.chain.Tip()
	if historical.Header.Height != 2 ||
		!historical.Header.VerifySignature(firstReplacement.Public()) {
		t.Fatal("first replacement block was not produced as expected")
	}

	secondReplacement, secondPublic, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(secondReplacement.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(locked.Destroy)
	registrySnapshot, err := json.Marshal(e.cfg.Registry.TakeSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	second := &core.ValidatorAuthorityFork{
		Version:             2,
		Genesis:             e.cfg.ValidatorAuthorityFork.Genesis,
		Height:              3,
		Parent:              historical.Hash(),
		Identity:            append(crypto.ValidatorPubKey(nil), old.Public()...),
		Replacement:         secondPublic,
		RegistrySHA256:      crypto.Hash32(sha256.Sum256(registrySnapshot)),
		LegacyWitnessSHA256: e.cfg.ValidatorAuthorityFork.LegacyWitnessSHA256,
		Previous:            e.cfg.ValidatorAuthorityFork.Clone(),
	}
	second.Possession, err = secondReplacement.Sign(second.Message())
	if err != nil {
		t.Fatal(err)
	}
	cfg := e.cfg
	cfg.MyKey = locked
	cfg.ValidatorAuthorityFork = second
	registry := core.NewValidatorRegistry()
	registry.RestoreFromSnapshot(e.cfg.Registry.TakeSnapshot())
	utxos := core.NewUTXOSet()
	registry.SetUTXOSet(utxos)
	cfg.Registry = registry
	rotated := NewEngine(cfg, e.chain, e.pool, e.log)
	rotated.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
	if rotated.halted.Load() {
		t.Fatal("valid second rotation halted engine")
	}
	if err := rotated.tick(); err != nil {
		t.Fatalf("produce second replacement block: %v", err)
	}
	current := rotated.chain.Tip()
	if current.Header.Height != 3 ||
		!current.Header.ValidatorPub.Equals(old.Public()) ||
		!current.Header.VerifySignature(secondPublic) {
		t.Fatal("second replacement did not sign under the existing collateral identity")
	}
	if !historical.Header.VerifySignature(rotated.authority(old.Public(), historical.Header.Height)) {
		t.Fatal("second rotation invalidated a historical block")
	}
	restarted := NewEngine(rotated.cfg, rotated.chain, rotated.pool, rotated.log)
	if restarted.halted.Load() {
		t.Fatal("restart rejected the historical and current authority schedule")
	}
}

func TestAuthorityForkRejectsOldBlockAndVote(t *testing.T) {
	e, old, _ := authorityEngineFixture(t)
	b := &core.Block{Header: core.BlockHeader{Height: 2, PrevHash: e.chain.Tip().Hash(),
		Timestamp: 2, ValidatorPub: old.Public(), MerkleRoot: core.MerkleRoot(nil)}}
	if err := b.Header.Sign(old); err != nil {
		t.Fatal(err)
	}
	if err := e.handleIncomingBlock(b); err == nil {
		t.Fatal("old key produced activation block")
	}
	hash := b.Hash()
	msg := crypto.HashBytes([]byte("aperod/finalize/v1"), hash[:])
	signature, err := old.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.handleVote(FinalizeMsg{Height: 2, BlockHash: b.Hash(),
		ValidatorPub: old.Public(), Signature: signature}); err == nil {
		t.Fatal("old key authorized a future finality vote")
	}
}

func TestAuthorityForkPinnedParentAndRegistryFailClosed(t *testing.T) {
	e, _, next := authorityEngineFixture(t)
	f := e.cfg.ValidatorAuthorityFork.Clone()
	f.Parent[0] ^= 1
	f.Possession, _ = next.Sign(f.Message())
	cfg := e.cfg
	cfg.ValidatorAuthorityFork = f
	if !NewEngine(cfg, e.chain, e.pool, e.log).halted.Load() {
		t.Fatal("accepted a different independently pinned parent")
	}
	snap := e.cfg.Registry.TakeSnapshot()
	snap.Validators[f.Identity.Hex()].StakeNAPR--
	e.cfg.Registry.RestoreFromSnapshot(snap)
	if err := e.tick(); err == nil {
		t.Fatal("accepted replica registry divergence")
	}
}
