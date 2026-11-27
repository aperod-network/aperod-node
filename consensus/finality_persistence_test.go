package consensus

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func finalityTestChain(t *testing.T, marker string) (*core.Chain, *core.Block) {
	t.Helper()
	chain := core.NewChain()
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, Timestamp: 1}}
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}
	block := &core.Block{Header: core.BlockHeader{Height: 1, PrevHash: genesis.Hash(), Timestamp: 2,
		Round: uint32(len(marker))}}
	if err := chain.AddBlock(block); err != nil {
		t.Fatal(err)
	}
	return chain, block
}

func TestExplicitZeroFinalityScheduleKeepsV1AfterHardening(t *testing.T) {
	e := &Engine{cfg: Config{
		SecurityHardeningActivationHeight: 3_800_000,
		ExplicitFinalityVoteSchedule:      true,
	}}
	if got := e.expectedFinalityVoteVersion(4_024_017); got != crypto.FinalityVoteVersionLegacy {
		t.Fatalf("explicit zero expected v1, got v%d", got)
	}
	e.cfg.ExplicitFinalityVoteSchedule = false
	if got := e.expectedFinalityVoteVersion(4_024_017); got != crypto.FinalityVoteVersionHeightBound {
		t.Fatalf("legacy engine default expected v2, got v%d", got)
	}
}

func TestFinalityActivationPreservesLegacyVotesAcrossDiskRestarts(t *testing.T) {
	t.Run("same epoch", func(t *testing.T) { testFinalityTransitionRestarts(t, 2) })
	t.Run("epoch boundary", func(t *testing.T) { testFinalityTransitionRestarts(t, core.EpochLength) })
}

func testFinalityTransitionRestarts(t *testing.T, activation uint64) {
	t.Helper()
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain, before := finalityTestChain(t, "vote-version-boundary")
	for before.Header.Height+1 < activation {
		b := &core.Block{Header: core.BlockHeader{
			Height: before.Header.Height + 1, PrevHash: before.Hash(), Timestamp: before.Header.Timestamp + 1,
		}}
		if err := chain.AddBlock(b); err != nil {
			t.Fatal(err)
		}
		before = b
	}
	at := &core.Block{Header: core.BlockHeader{
		Height: activation, PrevHash: before.Hash(), Timestamp: before.Header.Timestamp + 1,
	}}
	dir := t.TempDir()
	open := func() *store.DB {
		t.Helper()
		db, err := store.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	t.Cleanup(func() {
		if db != nil {
			_ = db.Close()
		}
	})
	reopen := func() {
		t.Helper()
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		db = nil
		db = open()
	}
	persist := func(b *core.Block) {
		t.Helper()
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.PutRawBlock(b.Hash(), b.Header.Height, raw); err != nil {
			t.Fatal(err)
		}
		if err := db.PutTip(b.Hash(), b.Header.Height); err != nil {
			t.Fatal(err)
		}
	}
	engine := func() *Engine {
		t.Helper()
		e := NewEngine(Config{Validators: []crypto.ValidatorPubKey{pub}, Store: db,
			BFTThreshold: 0.667, SecurityHardeningActivationHeight: 1,
			FinalityVoteV2ActivationHeight: activation},
			chain, core.NewMempool(core.DefaultMempoolConfig()),
			slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := e.StartupError(); err != nil {
			t.Fatal(err)
		}
		return e
	}
	vote := func(b *core.Block, version uint8) FinalizeMsg {
		t.Helper()
		height, hash := b.Header.Height, b.Hash()
		sig, err := priv.Sign(crypto.FinalityVoteMessage(version, height, hash))
		if err != nil {
			t.Fatal(err)
		}
		return FinalizeMsg{Height: height, BlockHash: hash, ValidatorPub: pub, Version: version, Signature: sig}
	}
	saveVote := func(e *Engine, v FinalizeMsg) {
		t.Helper()
		if err := e.HandleVote(v); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveLocalFinalityVote(store.LocalFinalityVote{
			Version: 1, Height: v.Height, BlockHash: v.BlockHash, Validator: pub,
			Signature: v.Signature, VoteVersion: v.Version,
		}); err != nil {
			t.Fatal(err)
		}
	}
	persist(before)
	legacy := vote(before, crypto.FinalityVoteVersionLegacy)
	saveVote(engine(), legacy)
	reopen()
	e := engine()
	if !e.IsFinalizedHash(before.Header.Height, before.Hash()) {
		t.Fatal("valid pre-activation legacy certificate did not survive disk restart")
	}
	if err := chain.AddBlock(at); err != nil {
		t.Fatal(err)
	}
	persist(at)
	// Crash after saving the activation block but before signing its first
	// vote. The previous certificate and last local vote are still v1.
	reopen()
	e = engine()
	if !e.IsFinalizedHash(before.Header.Height, before.Hash()) || e.IsFinalizedHash(at.Header.Height, at.Hash()) {
		t.Fatal("restart at activation must restore only the historical finality")
	}
	local, err := db.LoadLocalFinalityVote()
	if err != nil || local == nil || local.Height != before.Header.Height ||
		local.VoteVersion != legacy.Version || !bytes.Equal(local.Signature, legacy.Signature) {
		t.Fatalf("restart at activation did not preserve the last v1 local vote: %v", err)
	}
	if err := e.HandleVote(vote(at, crypto.FinalityVoteVersionLegacy)); err == nil {
		t.Fatal("legacy vote accepted at activation")
	}
	heightBound := vote(at, crypto.FinalityVoteVersionHeightBound)
	saveVote(e, heightBound)
	reopen()
	if !engine().IsFinalizedHash(activation, at.Hash()) {
		t.Fatal("valid activation certificate did not survive disk restart")
	}
	historical, err := db.LoadFinalityCertificateAt(before.Header.Height)
	if err != nil || historical == nil || len(historical.Votes) != 1 ||
		!bytes.Equal(historical.Votes[0].Signature, legacy.Signature) {
		t.Fatalf("historical v1 certificate changed or disappeared: %v", err)
	}
	for _, want := range []FinalizeMsg{legacy, heightBound} {
		persisted, err := db.LoadFinalityVote(want.Height, pub)
		if err != nil || persisted == nil || persisted.VoteVersion != want.Version ||
			persisted.BlockHash != want.BlockHash || !bytes.Equal(persisted.Signature, want.Signature) {
			t.Fatalf("journal lost or changed vote v%d after transition: %v", want.Version, err)
		}
	}
}

func TestRehearsalFinalityScheduleRefusesSigningKey(t *testing.T) {
	priv, _, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Destroy()
	e := NewEngine(Config{MyKey: key, RehearsalFinalityV2Height: 2},
		core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if e.StartupError() == nil {
		t.Fatal("a signer started with a rehearsal finality schedule")
	}
}

func TestFinalityCertificateSurvivesRestartAndRejectsWrongFork(t *testing.T) {
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
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close finality persistence database: %v", err)
		}
	}()
	chain, block := finalityTestChain(t, "canonical")
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis(pubs, core.MinStakeNAPR*10)
	cfg := Config{Validators: pubs, Registry: registry, Store: db, BFTThreshold: 0.667}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), block.Hash().Bytes())
	for i := range privs {
		signature, err := privs[i].Sign(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.HandleVote(FinalizeMsg{BlockHash: block.Hash(), Height: 1,
			ValidatorPub: pubs[i], Signature: signature}); err != nil {
			t.Fatal(err)
		}
	}
	if !engine.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("live quorum did not finalize block")
	}
	restarted := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if err := restarted.StartupError(); err != nil {
		t.Fatalf("compatible legacy history failed startup: %v", err)
	}
	if !restarted.IsFinalizedHash(1, block.Hash()) || !restarted.IsFinalized(1) {
		t.Fatal("authentic tip certificate was not restored")
	}
	if err := restarted.requireLPoDParentFinalized(1, block.Hash()); err != nil {
		t.Fatalf("genuine restored quorum certificate did not authorize parent finality: %v", err)
	}
	if err := restarted.requireLPoDParentFinalized(1, crypto.HashBytes([]byte("same height, different hash"))); err == nil {
		t.Fatal("finality was inferred from parent height without its certified hash")
	}
	if err := restarted.requireLPoDParentFinalized(2, block.Hash()); err == nil {
		t.Fatal("finality was inferred from parent hash under a different height")
	}

	incompatible := cfg
	incompatible.SecurityHardeningActivationHeight = 1
	wrongVersion := NewEngine(incompatible, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if err := wrongVersion.StartupError(); err == nil || !strings.Contains(err.Error(), "certificate vote uses version 1 at height 1") {
		t.Fatalf("expected startup to report the durable vote version mismatch, got %v", err)
	}
	if !wrongVersion.halted.Load() || wrongVersion.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("startup accepted a legacy certificate at the v2 activation height")
	}
	cfg.Registry.SetSecurityHardeningActivationHeight(cfg.SecurityHardeningActivationHeight)

	otherChain, otherBlock := finalityTestChain(t, "other-fork")
	wrongFork := NewEngine(cfg, otherChain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if wrongFork.IsFinalizedHash(1, otherBlock.Hash()) || wrongFork.IsFinalized(1) {
		t.Fatal("certificate from another fork was accepted")
	}
	if err := wrongFork.requireLPoDParentFinalized(1, otherBlock.Hash()); err == nil {
		t.Fatal("unfinalized alternate-fork parent satisfied activation gate")
	}
	if !wrongFork.halted.Load() {
		t.Fatal("node did not halt on a durable certificate bound to another fork")
	}
}

func TestForgedFinalityCertificateFailsClosed(t *testing.T) {
	priv, pub, _ := crypto.GenerateValidatorKey()
	_, attacker, _ := crypto.GenerateValidatorKey()
	chain, block := finalityTestChain(t, "forged")
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close finality migration database: %v", err)
		}
	}()
	badSignature, _ := priv.Sign(crypto.HashBytes([]byte("not-finality")))
	if err := db.SaveFinalityCertificate(store.FinalityCertificate{Version: 1, Height: 1, BlockHash: block.Hash(),
		Votes: []store.FinalityVote{{Validator: attacker, Signature: badSignature}}}); err != nil {
		t.Fatal(err)
	}
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
	engine := NewEngine(Config{Validators: []crypto.ValidatorPubKey{pub}, Registry: registry, Store: db, BFTThreshold: 0.667},
		chain, core.NewMempool(core.DefaultMempoolConfig()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if engine.IsFinalized(1) || engine.IsFinalizedHash(1, block.Hash()) {
		t.Fatal("forged certificate restored finality")
	}
	if err := engine.requireLPoDParentFinalized(1, block.Hash()); err == nil {
		t.Fatal("forged certificate satisfied activation-parent gate")
	}
	if !engine.halted.Load() {
		t.Fatal("node did not halt on forged durable finality evidence")
	}
}

func TestFinalityCommitteeMigrationRestoresCertificateOnlyWithPinnedQuorum(t *testing.T) {
	historicalPriv, historicalPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	var authorityPrivs []crypto.ValidatorPrivKey
	var authorities []crypto.ValidatorPubKey
	for range 2 {
		priv, pub, err := crypto.GenerateValidatorKey()
		if err != nil {
			t.Fatal(err)
		}
		authorityPrivs = append(authorityPrivs, priv)
		authorities = append(authorities, pub)
	}

	chain, block := finalityTestChain(t, "migration-anchor")
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rawBlock, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(block.Hash(), block.Header.Height, rawBlock); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(block.Hash(), block.Header.Height); err != nil {
		t.Fatal(err)
	}
	certificateMessage := crypto.HashBytes([]byte("aperod/finalize/v1"), block.Hash().Bytes())
	historicalVote, err := historicalPriv.Sign(certificateMessage)
	if err != nil {
		t.Fatal(err)
	}
	certificate := store.FinalityCertificate{
		Version: 1, Height: block.Header.Height, BlockHash: block.Hash(),
		Votes: []store.FinalityVote{{Validator: historicalPub, Signature: historicalVote}},
	}
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}

	type signer struct {
		approval store.FinalityMigrationApproval
		priv     crypto.ValidatorPrivKey
	}
	signers := make([]signer, len(authorities))
	for i, authority := range authorities {
		signers[i] = signer{
			approval: store.FinalityMigrationApproval{
				Validator: authority,
				Signature: make([]byte, 64),
			},
			priv: authorityPrivs[i],
		}
	}
	sort.Slice(signers, func(i, j int) bool {
		return bytes.Compare(signers[i].approval.Validator, signers[j].approval.Validator) < 0
	})
	migration := &store.FinalityCommitteeMigration{
		Version:              store.FinalityCommitteeMigrationVersion,
		Genesis:              chain.Genesis().Hash(),
		CertificateHeight:    certificate.Height,
		CertificateBlockHash: certificate.BlockHash,
		ApprovalHeight:       block.Header.Height,
		ApprovalBlockHash:    block.Hash(),
		BFTThresholdPPM:      667_000,
		HistoricalCommittee:  []crypto.ValidatorPubKey{historicalPub},
	}
	for _, signer := range signers {
		migration.Approvals = append(migration.Approvals, signer.approval)
	}
	approvalMessage, err := migration.SigningMessage()
	if err != nil {
		t.Fatal(err)
	}
	for i := range signers {
		signature, err := signers[i].priv.Sign(approvalMessage)
		if err != nil {
			t.Fatal(err)
		}
		migration.Approvals[i].Signature = signature
	}

	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis(authorities, core.MinStakeNAPR*10)
	cfg := Config{
		Validators:                        authorities,
		Registry:                          registry,
		Store:                             db,
		BFTThreshold:                      0.667,
		SecurityHardeningActivationHeight: 1,
		RehearsalFinalityV2Height:         2,
		FinalityMigration:                 migration,
		FinalityMigrationTrustAuthorities: authorities,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if engine.halted.Load() || !engine.IsFinalizedHash(certificate.Height, certificate.BlockHash) {
		t.Fatal("valid signed migration did not restore the exact historical certificate")
	}

	insufficient := *migration
	insufficient.Approvals = append([]store.FinalityMigrationApproval(nil), migration.Approvals[:1]...)
	insufficientCfg := cfg
	insufficientCfg.FinalityMigration = &insufficient
	if !NewEngine(insufficientCfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log).halted.Load() {
		t.Fatal("migration with fewer than the pinned authority quorum was accepted")
	}

	tampered := *migration
	tampered.Approvals = append([]store.FinalityMigrationApproval(nil), migration.Approvals...)
	tampered.Approvals[0].Signature = append([]byte(nil), tampered.Approvals[0].Signature...)
	tampered.Approvals[0].Signature[0] ^= 0x01
	tamperedCfg := cfg
	tamperedCfg.FinalityMigration = &tampered
	if !NewEngine(tamperedCfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log).halted.Load() {
		t.Fatal("tampered migration signature was accepted")
	}

	// Advance from the restored historical v1 certificate to v2. The two
	// committees deliberately differ: the old approval cannot authorize
	// either the new votes or a fallback to the live registry on restart.
	next := &core.Block{Header: core.BlockHeader{
		Height: 2, PrevHash: block.Hash(), Timestamp: 3,
	}}
	if err := chain.AddBlock(next); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(next.Hash(), next.Header.Height, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(next.Hash(), next.Header.Height); err != nil {
		t.Fatal(err)
	}
	makeVote := func(i int, version uint8) FinalizeMsg {
		t.Helper()
		sig, err := authorityPrivs[i].Sign(crypto.FinalityVoteMessage(version, next.Header.Height, next.Hash()))
		if err != nil {
			t.Fatal(err)
		}
		return FinalizeMsg{Height: next.Header.Height, BlockHash: next.Hash(),
			ValidatorPub: authorities[i], Version: version, Signature: sig}
	}
	if err := engine.HandleVote(makeVote(0, crypto.FinalityVoteVersionLegacy)); err == nil {
		t.Fatal("accepted a legacy vote at the rehearsal transition")
	}
	nextCert := store.FinalityCertificate{Version: 1, Height: next.Header.Height, BlockHash: next.Hash()}
	for i := range authorities {
		v := makeVote(i, crypto.FinalityVoteVersionHeightBound)
		nextCert.Votes = append(nextCert.Votes, store.FinalityVote{
			Validator: v.ValidatorPub, Signature: v.Signature, VoteVersion: v.Version,
		})
	}
	if err := db.SaveFinalityCertificate(nextCert); err != nil {
		t.Fatal(err)
	}
	start := func(c Config) *Engine {
		return NewEngine(c, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	}
	if err := start(cfg).StartupError(); err == nil || !strings.Contains(err.Error(), "no persisted committee") {
		t.Fatalf("valid successor signatures without a durable committee should fail: %v", err)
	}
	for i := range authorities {
		if err := engine.HandleVote(makeVote(i, crypto.FinalityVoteVersionHeightBound)); err != nil {
			t.Fatal(err)
		}
	}
	if !engine.IsFinalizedHash(next.Header.Height, next.Hash()) {
		t.Fatal("v2 quorum did not finalize the transition block")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	db = reopened
	cfg.Store = db
	if e := start(cfg); e.StartupError() != nil || !e.IsFinalizedHash(next.Header.Height, next.Hash()) {
		t.Fatalf("successor certificate did not restore with the historical migration still configured: %v", e.StartupError())
	}
	tamperedCfg.Store = db
	if err := start(tamperedCfg).StartupError(); err == nil || !strings.Contains(err.Error(), "untrusted approval") {
		t.Fatalf("advancing the certificate must not bypass validation of the configured migration: %v", err)
	}
}

func TestFinalityCertificateForCanonicalAncestorDoesNotFinalizeTip(t *testing.T) {
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	chain, ancestor := finalityTestChain(t, "ancestor")
	tip := &core.Block{Header: core.BlockHeader{
		Height: 2, PrevHash: ancestor.Hash(), Timestamp: 3, Round: 2,
	}}
	if err := chain.AddBlock(tip); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	raw, err := json.Marshal(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRawBlock(ancestor.Hash(), 1, raw); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(tip.Hash(), tip.Header.Height); err != nil {
		t.Fatal(err)
	}
	message := crypto.HashBytes([]byte("aperod/finalize/v1"), ancestor.Hash().Bytes())
	signature, err := priv.Sign(message)
	if err != nil {
		t.Fatal(err)
	}
	certificate := store.FinalityCertificate{
		Version: 1, Height: 1, BlockHash: ancestor.Hash(),
		Votes: []store.FinalityVote{{Validator: pub, Signature: signature}},
	}
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}
	registry := core.NewValidatorRegistry()
	registry.InitFromGenesis([]crypto.ValidatorPubKey{pub}, core.MinStakeNAPR*10)
	cfg := Config{Validators: []crypto.ValidatorPubKey{pub}, Registry: registry, Store: db, BFTThreshold: 0.667}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	restart := func() *Engine {
		return NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	}

	recovered := restart()
	if recovered.halted.Load() || !recovered.IsFinalizedHash(1, ancestor.Hash()) {
		t.Fatal("authentic canonical ancestor certificate did not restore")
	}
	if recovered.IsFinalized(2) || recovered.IsFinalizedHash(2, tip.Hash()) {
		t.Fatal("ancestor certificate incorrectly finalized the current tip")
	}
	if err := recovered.requireLPoDParentFinalized(2, tip.Hash()); err == nil {
		t.Fatal("ancestor certificate authorized an uncertified LPoD parent")
	}

	// The non-validator's genesis fallback may be a placeholder that was never
	// active in the restored registry. It must not be added after the snapshot.
	_, placeholderPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	nonValidatorCfg := cfg
	nonValidatorCfg.Validators = []crypto.ValidatorPubKey{placeholderPub}
	nonValidatorRestart := NewEngine(nonValidatorCfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log)
	if nonValidatorRestart.halted.Load() || !nonValidatorRestart.IsFinalizedHash(1, ancestor.Hash()) {
		t.Fatal("static fallback changed the restored historical committee")
	}
	if got := len(registry.GetActiveValidators()); got != 1 {
		t.Fatalf("static fallback created a phantom active validator: got %d", got)
	}

	exitedRegistry := core.NewValidatorRegistry()
	exitedRegistry.RestoreFromSnapshot(core.RegistrySnapshot{Validators: map[string]*core.ValidatorEntry{
		pub.Hex(): {PubKey: pub, Status: core.ValidatorExited},
	}})
	exitedCfg := cfg
	exitedCfg.Registry = exitedRegistry
	if !NewEngine(exitedCfg, chain, core.NewMempool(core.DefaultMempoolConfig()), log).halted.Load() {
		t.Fatal("static fallback re-authorized a validator that exited in the restored registry")
	}

	// A signature that was sufficient for one validator is not a quorum
	// certificate once the active committee contains two validators.
	_, secondPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	twoValidatorRegistry := core.NewValidatorRegistry()
	twoValidatorRegistry.InitFromGenesis(
		[]crypto.ValidatorPubKey{pub, secondPub}, core.MinStakeNAPR*10)
	twoValidatorConfig := Config{
		Validators:   []crypto.ValidatorPubKey{pub, secondPub},
		Registry:     twoValidatorRegistry,
		Store:        db,
		BFTThreshold: 0.667,
	}
	if !NewEngine(twoValidatorConfig, chain, core.NewMempool(core.DefaultMempoolConfig()), log).halted.Load() {
		t.Fatal("insufficient historical votes did not halt consensus")
	}

	if err := db.RepairHeightIndex(1, crypto.HashBytes([]byte("different fork"))); err != nil {
		t.Fatal(err)
	}
	if !restart().halted.Load() {
		t.Fatal("mismatched ancestor height index did not halt consensus")
	}
	if err := db.RepairHeightIndex(1, ancestor.Hash()); err != nil {
		t.Fatal(err)
	}
	badSignature, err := priv.Sign(crypto.HashBytes([]byte("wrong finality message")))
	if err != nil {
		t.Fatal(err)
	}
	certificate.Votes[0].Signature = badSignature
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}
	if !restart().halted.Load() {
		t.Fatal("forged ancestor certificate did not halt consensus")
	}
	certificate.Height = 3
	if err := db.SaveFinalityCertificate(certificate); err != nil {
		t.Fatal(err)
	}
	if !restart().halted.Load() {
		t.Fatal("certificate above the tip did not halt consensus")
	}
}
