package consensus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/sale"
	"github.com/aperod/aperod/store"
	"github.com/aperod/aperod/wallet"
)

// Independent engines use real quorum signatures and separately committed
// LPoD funding. This is an offline forward-anchor rehearsal, not a claim of
// historical body coverage or a live network rollout.
func TestSaleTwoValidatorCanonicalDelivery(t *testing.T) {
	testSaleTwoValidatorCanonicalDelivery(t, "forward")
}

func TestSaleTwoValidatorNetAllocationCanonicalDelivery(t *testing.T) {
	testSaleTwoValidatorCanonicalDelivery(t, "net")
}

func TestSaleTwoValidatorOperatorEpochCanonicalDelivery(t *testing.T) {
	testSaleTwoValidatorCanonicalDelivery(t, "epoch")
}

func testSaleTwoValidatorCanonicalDelivery(t *testing.T, basis string) {
	t.Helper()
	netBasis := basis == "net"
	epochBasis := basis == "epoch"
	keys := []crypto.ValidatorPrivKey{validatorKey(t, 91), validatorKey(t, 92)}
	pubs := []crypto.ValidatorPubKey{keys[0].Public(), keys[1].Public()}
	buyer, err := wallet.DeriveFromMnemonic("abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	buyer.Address = crypto.AddressFromKeys(crypto.MainnetByte, buyer.Keys)
	g := engineBlock(t, 0, 0, crypto.Hash32{}, keys[0], nil)
	parent := engineBlock(t, store.LPoDV2ActivationHeight-1, 0, g.Hash(), keys[0], nil)
	parent.Header.Timestamp = time.Now().UnixNano() - 6_000_000_000
	parent.Header.Sign(keys[0])
	lm := &store.LPoDMigration{Version: 3, PositionLifecycleVersion: 1,
		Height: store.LPoDV2ActivationHeight, Genesis: g.Hash(), ParentHash: parent.Hash(),
		TrustAssumption: store.LPoDV3TrustAssumption, NominalEligible: 6_000_000_000 * lpod.Unit,
		ValidatorRemaining: 2_000_000_000 * lpod.Unit, TrustedValidators: pubs}
	lm.NominalSnapshot = &store.LPoDNominalSnapshot{Basis: store.LPoDV3NominalBasis,
		NominalCirculatingBefore: 6_000_000_000 * lpod.Unit, EligibleNominal: lm.NominalEligible,
		ValidatorRemaining: lm.ValidatorRemaining, EligibilityRule: store.LPoDV3EligibilityRule}
	lm.SnapshotRoot = lm.NominalSnapshot.Root()
	lm.ReconciliationRoot = lm.Root()
	for i, k := range keys {
		sig, err := k.Sign(lm.AttestationMessage())
		if err != nil {
			t.Fatal(err)
		}
		lm.Attestations = append(lm.Attestations, store.LPoDAttestation{Validator: pubs[i], Signature: sig})
	}
	dbs := make([]*store.DB, 2)
	directories := make([]string, 2)
	registries := make([]*core.ValidatorRegistry, 2)
	var funding *core.Block
	for i := range dbs {
		directories[i] = t.TempDir()
		d, err := store.Open(directories[i])
		if err != nil {
			t.Fatal(err)
		}
		dbs[i] = d
		t.Cleanup(func() { d.Close() })
		raw, _ := json.Marshal(g)
		if err = d.CommitRawBlockWithAVM(g.Hash(), 0, raw, nil, crypto.Hash32{}); err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(parent)
		if err = d.PutRawBlock(parent.Hash(), parent.Header.Height, raw); err != nil {
			t.Fatal(err)
		}
		if err = d.PutTip(parent.Hash(), parent.Header.Height); err != nil {
			t.Fatal(err)
		}
		if err = d.StoreStakingPoolRemaining(lm.ValidatorRemaining); err != nil {
			t.Fatal(err)
		}
		rg := core.NewValidatorRegistry()
		rg.InitFromGenesis(pubs, 100_000*lpod.Unit)
		registries[i] = rg
		snapshot := rg.TakeSnapshot()
		stakes := map[string]store.LPoDValidatorStake{}
		for _, p := range pubs {
			stakes[p.Hex()] = store.LPoDValidatorStake{Amount: 100_000 * lpod.Unit, Active: true}
		}
		settlement := &store.LPoDSettlement{Parent: parent.Hash(), Migration: lm, PositionProtocol: true,
			Timestamp: parent.Header.Timestamp + 3_000_000_000, Proposer: pubs[0].Hex(), Leader: buyer.Address,
			Stake: stakes, PreviousStake: stakes, AuditPreviousStake: stakes, AuditStake: stakes,
			RegistryBefore: &snapshot, RegistryAfter: &snapshot}
		c, pay, err := d.PreviewLPoD(lm.Height, settlement)
		if err != nil {
			t.Fatal(err)
		}
		reward, err := d.PayoutLPoD(lm.Height, settlement, c, pay)
		if err != nil {
			t.Fatal(err)
		}
		b := engineBlock(t, lm.Height, 1, parent.Hash(), keys[0], []core.Transaction{reward, core.LPoDCheckpointTx(c.Digest())})
		b.Header.Timestamp = settlement.Timestamp
		b.Header.Sign(keys[0])
		if funding != nil && funding.Hash() != b.Hash() {
			t.Fatal("independent funding differs")
		}
		funding = b
		raw, _ = json.Marshal(b)
		if err = d.CommitRawBlockWithAVM(b.Hash(), b.Header.Height, raw, nil, crypto.Hash32{}, settlement); err != nil {
			t.Fatal(err)
		}
	}
	lp, err := dbs[0].LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	knownIssued, err := store.SaleKnownIssuedFloor(lp)
	if err != nil {
		t.Fatal(err)
	}
	reconciliation := sale.Reconciliation{Genesis: g.Hash(), Parent: funding.Hash(), LPoDCommitment: lp.Digest(),
		OtherClaimsRoot:   crypto.HashBytes([]byte("offline disjoint Guardian reservation")),
		HistoricIssued:    knownIssued,
		ValidatorReserved: lp.Allocation.ValidatorRemaining, GuardianReserved: 1_000_000_000 * lpod.Unit}
	if netBasis {
		// This fresh fixture has no initial wallet carry or burns. Pool reward
		// payments are actual canonical outputs, not a nominal circulation field.
		reconciliation.NetAllocation = &sale.NetAllocationBasis{
			WalletNet:            lp.State.LeaderPaid + lp.State.AngelPaid,
			WalletAccountingRoot: crypto.HashBytes([]byte("offline funded net opening declaration")),
		}
		reconciliation.HistoricIssued = reconciliation.NetAllocation.WalletNet
		reconciliation.GuardianReserved = lp.State.Balance
		reconciliation.LPoDReserved = lp.Allocation.Remaining + lp.PrincipalLocked
	}
	if epochBasis {
		epoch := &sale.AllocationEpochBasis{
			FundingHeight: lp.Allocation.FundingHeight, FundingHash: lp.Allocation.FundingBlock,
			Burned: 13745052310704501, ProtectedLPoDPrincipal: lp.PrincipalLocked,
			LegacyHoldingsUnverified: true,
		}
		reconciliation.AllocationEpoch = epoch
		reconciliation.HistoricIssued = epoch.Burned
		reconciliation.GuardianReserved = lp.State.Balance
		reconciliation.LPoDReserved = lp.Allocation.Remaining
		epoch.Available = sale.CapNAPRO - epoch.Burned - reconciliation.ValidatorReserved -
			reconciliation.GuardianReserved - reconciliation.LPoDReserved
	}
	cp := sale.Checkpoint{Genesis: g.Hash(), Parent: funding.Hash(), Height: funding.Header.Height + 1,
		Network: crypto.MainnetByte, SnapshotRoot: reconciliation.Digest(),
		HistoricIssued: reconciliation.HistoricIssued,
		OtherReserved:  reconciliation.ValidatorReserved + reconciliation.GuardianReserved + reconciliation.LPoDReserved,
		SaleRemaining:  100_000 * lpod.Unit, SaleAuthority: pubs[0]}
	if epochBasis {
		cp.SaleRemaining = reconciliation.AllocationEpoch.Available
	}
	for i, k := range keys {
		sig, err := k.Sign(cp.Message())
		if err != nil {
			t.Fatal(err)
		}
		cp.Attestations = append(cp.Attestations, sale.Attestation{Validator: pubs[i], Signature: sig})
	}
	migration := &store.SaleMigration{Checkpoint: cp, Reconciliation: reconciliation, Trusted: pubs}
	// A fully re-signed declaration must not recycle rewards already paid after
	// the nominal funding snapshot, even though its remaining validator reserve
	// and all other signatures are otherwise valid.
	if knownIssued <= lm.NominalSnapshot.NominalCirculatingBefore {
		t.Fatal("funded control must include post-declaration reward issuance")
	}
	stale := *migration
	if epochBasis {
		// A nominal epoch must never masquerade as historically verified issuance,
		// even if both validators genuinely re-sign the changed declaration.
		altered := *reconciliation.AllocationEpoch
		altered.LegacyHoldingsUnverified = false
		stale.Reconciliation.AllocationEpoch = &altered
	} else if netBasis {
		if reconciliation.NetAllocation.WalletNet == 0 {
			t.Fatal("funded net control has no known reward payment")
		}
		omitted := *reconciliation.NetAllocation
		omitted.WalletNet = 0
		stale.Reconciliation.NetAllocation = &omitted
		stale.Reconciliation.HistoricIssued = 0
	} else {
		stale.Reconciliation.HistoricIssued = lm.NominalSnapshot.NominalCirculatingBefore
	}
	stale.Checkpoint.HistoricIssued = stale.Reconciliation.HistoricIssued
	stale.Checkpoint.SnapshotRoot = stale.Reconciliation.Digest()
	stale.Checkpoint.Attestations = nil
	for i, k := range keys {
		sig, err := k.Sign(stale.Checkpoint.Message())
		if err != nil {
			t.Fatal(err)
		}
		stale.Checkpoint.Attestations = append(stale.Checkpoint.Attestations, sale.Attestation{Validator: pubs[i], Signature: sig})
	}
	stalePath := filepath.Join(t.TempDir(), "stale-issued-checkpoint.json")
	staleBytes, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, staleBytes, 0600); err != nil {
		t.Fatal(err)
	}
	for _, d := range dbs {
		if _, err := d.LoadSaleMigrationFile(stalePath); err == nil {
			t.Fatal("fully signed checkpoint omitted known post-declaration issuance")
		}
	}
	configPath := filepath.Join(t.TempDir(), "sale-checkpoint.json")
	configBytes, err := json.Marshal(migration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	for _, d := range dbs {
		if loaded, err := d.LoadSaleMigrationFile(configPath); err != nil || loaded.Checkpoint.Message() != cp.Message() {
			t.Fatal("signed exact-parent startup configuration rejected", err)
		}
	}
	engines := make([]*Engine, 2)
	for i, d := range dbs {
		chain := core.NewChain()
		if err := chain.SetGenesis(g); err != nil {
			t.Fatal(err)
		}
		chain.FastForward([]*core.Block{parent, funding})
		locked, err := crypto.NewLockedValidatorKey(keys[i].Bytes(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(locked.Destroy)
		cfg := Config{MyKey: locked, Validators: pubs, Registry: registries[i], Store: d,
			LPoDMigration: lm, SaleMigration: migration, StakingPoolNAPR: lm.ValidatorRemaining,
			RewardAddress: string(buyer.Address), RewardAuthorizationActivationHeight: lm.Height,
			BFTThreshold: .667, RingCTV4ActivationHeight: lm.Height}
		cfg.OnCanonicalBlock = func(b *core.Block, p *avm.PreparedBlock) error {
			raw, err := json.Marshal(b)
			if err != nil {
				return err
			}
			return avm.CommitCanonicalBlock(d, b, raw, p)
		}
		e := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()), testNopLogger())
		u := core.NewUTXOSet()
		if err := u.ApplyBlock(funding); err != nil {
			t.Fatal(err)
		}
		registries[i].SetUTXOSet(u)
		e.SetTxVerifier(core.NewTxVerifier(u), u)
		engines[i] = e
	}
	// Both restored engines authenticate the exact parent with two real votes.
	for _, e := range engines {
		for i, k := range keys {
			sig, err := k.Sign(crypto.HashBytes([]byte("aperod/finalize/v1"), funding.Hash().Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err = e.HandleVote(FinalizeMsg{Height: funding.Header.Height, BlockHash: funding.Hash(), ValidatorPub: pubs[i], Signature: sig}); err != nil {
				t.Fatal(err)
			}
		}
		if !e.IsFinalizedHash(funding.Header.Height, funding.Hash()) {
			t.Fatal("parent lacks real quorum")
		}
	}
	var producer *Engine
	for _, e := range engines {
		if e.proposerAt(2).Equals(e.cfg.MyKey.Public()) {
			producer = e
		}
	}
	if producer == nil {
		t.Fatal("scheduled producer missing")
	}
	var httpRehearsal *saleHTTPRehearsal
	slot := uint64(2)
	if epochBasis {
		httpRehearsal = newSaleHTTPRehearsal(t, producer, migration, buyer.Address)
		// Do not accept even a fixture invoice until the checkpoint itself has
		// become a canonical, quorum-finalized block.
		httpRehearsal.request("reserve", httpRehearsal.reserveBody(), 503)
		activation, err := producer.produceBlock(cp.Height, slot, funding)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range engines {
			if err := e.handleIncomingBlock(activation); err != nil {
				t.Fatal(err)
			}
		}
		finalizeSaleRehearsal(t, engines, keys, activation)
		funding = activation
		slot = 4
		httpRehearsal.enqueue()
		// The authority only produces in its own scheduled rounds. Preserve the paid
		// obligation through the other validator's normal intervening block.
		var intervening *Engine
		for _, e := range engines {
			if e.proposerAt(3).Equals(e.cfg.MyKey.Public()) {
				intervening = e
			}
		}
		if intervening == nil {
			t.Fatal("intervening proposer missing")
		}
		waiting, err := intervening.produceBlock(funding.Header.Height+1, 3, funding)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range engines {
			if err := e.handleIncomingBlock(waiting); err != nil {
				t.Fatal(err)
			}
		}
		finalizeSaleRehearsal(t, engines, keys, waiting)
		funding = waiting
		httpRehearsal.receipt("payment_confirmed")
		// Keep the real durable pending-order builder installed by NewEngine.
	} else {
		producer.cfg.SaleTransactions = func(h uint64, parent crypto.Hash32) ([]core.Transaction, error) {
			tx, err := core.BuildSaleTransaction(core.SaleAuthorization{Genesis: cp.Genesis, Height: h, Parent: parent,
				Order: crypto.HashBytes([]byte("paid offline order")), Payment: crypto.HashBytes([]byte("verified offline payment")),
				Amount: 100 * lpod.Unit}, buyer.Address, cp.Network, keys[0])
			if err != nil {
				return nil, err
			}
			return []core.Transaction{*tx}, nil
		}
	}
	block, err := producer.produceBlock(funding.Header.Height+1, slot, funding)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range engines {
		if err = e.handleIncomingBlock(block); err != nil {
			t.Fatal(err)
		}
	}
	a, err := dbs[0].LoadSaleState()
	if err != nil {
		t.Fatal(err)
	}
	b, err := dbs[1].LoadSaleState()
	if err != nil {
		t.Fatal(err)
	}
	if a == nil || b == nil || *a != *b || a.Sold != 100*lpod.Unit {
		t.Fatal("validator sale ledgers disagree")
	}
	for _, e := range engines {
		for i, k := range keys {
			sig, err := k.Sign(crypto.HashBytes([]byte("aperod/finalize/v1"), block.Hash().Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err = e.HandleVote(FinalizeMsg{Height: block.Header.Height, BlockHash: block.Hash(),
				ValidatorPub: pubs[i], Signature: sig}); err != nil {
				t.Fatal(err)
			}
		}
		if !e.IsFinalizedHash(block.Header.Height, block.Hash()) {
			t.Fatal("buyer delivery lacks actual quorum finality")
		}
		restoredFinality := &Engine{cfg: e.cfg, lpodFinalizedHashes: map[uint64]crypto.Hash32{
			block.Header.Height: block.Hash(),
		}}
		if !restoredFinality.IsSaleFinalizedHash(parent.Header.Height, parent.Hash()) {
			t.Fatal("canonical sale ancestor lost finality after certificate-only restoration")
		}
		if restoredFinality.IsSaleFinalizedHash(block.Header.Height+1, block.Hash()) {
			t.Fatal("unfinalized future sale block accepted")
		}
		wrongHash := parent.Hash()
		wrongHash[0] ^= 1
		if restoredFinality.IsSaleFinalizedHash(parent.Header.Height, wrongHash) {
			t.Fatal("noncanonical sale ancestor accepted")
		}
		restoredFinality.lpodFinalizedHashes[block.Header.Height] = wrongHash
		if restoredFinality.IsSaleFinalizedHash(parent.Header.Height, parent.Hash()) {
			t.Fatal("unbound certificate anchor accepted")
		}
	}
	for _, d := range dbs {
		if _, err := d.LoadSaleMigrationFile(""); err == nil {
			t.Fatal("activated ledger silently accepted disabled sale configuration")
		}
		if loaded, err := d.LoadSaleMigrationFile(configPath); err != nil || loaded.Checkpoint.Message() != cp.Message() {
			t.Fatal("durable restart configuration rejected", err)
		}
	}
	if !engines[0].CanSettleSale() || engines[1].CanSettleSale() {
		t.Fatal("invoice admission does not match the eligible sale authority")
	}
	rotated := &Engine{cfg: engines[0].cfg}
	rotated.cfg.Registry = core.NewValidatorRegistry()
	rotated.cfg.Registry.InitFromGenesis([]crypto.ValidatorPubKey{pubs[1]}, 100_000*lpod.Unit)
	rotated.cfg.ValidatorAuthorityFork = &core.ValidatorAuthorityFork{
		Height: block.Header.Height + 1, Identity: pubs[1], Replacement: pubs[0],
	}
	if !rotated.CanSettleSale() {
		t.Fatal("authorized successor is not admitted under its active collateral identity")
	}
	rotated.cfg.ValidatorAuthorityFork.Height++
	if rotated.CanSettleSale() {
		t.Fatal("future authority rule admitted an unfunded signing identity early")
	}
	engines[0].halted.Store(true)
	if engines[0].CanSettleSale() {
		t.Fatal("halted producer still admits invoices")
	}
	engines[0].halted.Store(false)
	engines[0].cfg.StopProducingAtHeight = block.Header.Height + 1
	if engines[0].CanSettleSale() {
		t.Fatal("paused producer still admits invoices")
	}
	engines[0].cfg.StopProducingAtHeight = 0
	found := wallet.NewScannerFromDerived(buyer).ScanBlock(block)
	var delivered uint64
	for _, o := range found {
		for _, tx := range block.Txs {
			if tx.IsSale() && tx.Hash() == o.TxHash {
				delivered += o.Amount
			}
		}
	}
	if delivered != 100*lpod.Unit {
		t.Fatal("owner did not discover exact canonical sale output")
	}
	if a.Remaining != cp.SaleRemaining-delivered {
		t.Fatal("purchase did not debit the available allocation by its credited amount")
	}
	if httpRehearsal != nil {
		// Sign only after the actual owner scanner found the exact credited output.
		httpRehearsal.acknowledge(buyer.Keys)
	}
	// Purchases must exist as actual active outputs in the node snapshot, not
	// merely as a payment callback or an incremented display total.
	verifyPurchaseSnapshot := func(snapshot core.UTXOSnapshot) {
		t.Helper()
		var total uint64
		seen := make(map[crypto.Hash32]bool)
		for _, output := range snapshot.ActiveUTXOs {
			for _, tx := range block.Txs {
				if !tx.IsSale() || output.TxHash != tx.Hash() {
					continue
				}
				authorization, err := tx.SaleAuthorization()
				if err != nil || seen[output.TxHash] || output.OutputIndex != 0 ||
					output.BlockHeight != block.Header.Height || output.ProtocolLocked ||
					output.AmountCommit != tx.Outputs[0].AmountCommit {
					t.Fatal("snapshot contains invalid or duplicate purchase output", err)
				}
				seen[output.TxHash] = true
				total += authorization.Amount
			}
		}
		if total != delivered {
			t.Fatal("UTXO snapshot does not contain the exact credited purchase amount")
		}
	}
	for _, e := range engines {
		snapshot := e.utxos.TakeSnapshot()
		verifyPurchaseSnapshot(snapshot)
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var decoded core.UTXOSnapshot
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		restored := core.NewUTXOSet()
		restored.RestoreFromSnapshot(decoded)
		verifyPurchaseSnapshot(restored.TakeSnapshot())
	}
	for _, e := range engines {
		tip := e.chain.Tip().Hash()
		if e.handleIncomingBlock(block) == nil || e.chain.Tip().Hash() != tip {
			t.Fatal("replayed block changed canonical state")
		}
		verifyPurchaseSnapshot(e.utxos.TakeSnapshot())
	}
	for i, d := range dbs {
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := store.Open(directories[i])
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		persisted, err := reopened.LoadSaleState()
		if err != nil || persisted == nil || *persisted != *a {
			t.Fatalf("validator %d lost sale debit on reopening: %v", i, err)
		}
		if _, err := reopened.LoadSaleMigrationFile(configPath); err != nil {
			t.Fatal("reopened ledger rejected its signed configuration", err)
		}
		engines[i].cfg.Store = reopened
		if httpRehearsal != nil && producer == engines[i] {
			httpRehearsal.server.SetStore(reopened)
			httpRehearsal.receipt("completed")
			pending, err := reopened.PendingPaidSales(cp.Genesis, block.Header.Height+1, block.Hash(), cp.Network, keys[0])
			if err != nil || len(pending) != 0 {
				t.Fatal("restart requeued an already delivered payment", err)
			}
		}
	}
}
