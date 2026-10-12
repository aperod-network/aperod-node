// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package api_test

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestGenesisVestingCanonicalPaymentRestartAndPruning(t *testing.T) {
	path := t.TempDir()
	db, migration, leader, signer := syntheticLPoDV3FundingAt(t, path, true)
	activation := migration.Height+1
treasury, err := crypto.GenerateWalletKeys()
if err != nil { t.Fatal(err) }
treasuryAddress := crypto.AddressFromKeys(crypto.MainnetByte, treasury)
proof := &store.GenesisVestingFunding{Version: 2, Genesis: migration.Genesis, Snapshot: migration.SnapshotRoot,
Height: activation, Recipient: store.GenesisVestingRecipient, Reserve: store.GenesisVestingReserve,
OtherAllocations: store.GenesisNominalTotal-store.GenesisVestingReserve-store.GenesisLiquidityAmount, Trust: store.GenesisVestingTrust,
LiquidityRecipient: treasuryAddress, LiquidityAmount: store.GenesisLiquidityAmount, LiquidityTrust: store.GenesisLiquidityTrust}
	signature, err := signer.Sign(proof.Message())
if err != nil { t.Fatal(err) }
proof.Attestations = []store.LPoDAttestation{{Validator: signer.Public(), Signature: signature}}
	commit := func(timestamp int64, tamper bool) *core.Block {
		t.Helper()
		parent, height, err := db.GetTip()
		if err != nil { t.Fatal(err) }
		r := &store.LPoDSettlement{Parent: parent, PositionProtocol: true, Timestamp: timestamp,
			Proposer: signer.Public().Hex(), Leader: leader, GenesisVestingActivationHeight: activation,
GenesisVestingFunding: proof,
Stake: map[string]store.LPoDValidatorStake{signer.Public().Hex(): {Amount: 100_000*100_000_000, Active: true}}}
		next, payments, err := db.PreviewLPoD(height+1, r)
		if err != nil { t.Fatal(err) }
		payout, err := db.PayoutLPoD(height+1, r, next, payments)
		if err != nil { t.Fatal(err) }
		if tamper { payout.Outputs[0].EncAmount[0] ^= 1 }
		b := &core.Block{Header: core.BlockHeader{Height: height+1, PrevHash: parent,
			Timestamp: timestamp, ValidatorPub: signer.Public()},
			Txs: []core.Transaction{payout, core.LPoDCheckpointTx(next.Digest())}}
		b.Header.MerkleRoot = core.MerkleRoot(b.Txs)
		if err := b.Header.Sign(signer); err != nil { t.Fatal(err) }
		raw, _ := json.Marshal(b)
		err = db.CommitRawBlockWithAVM(b.Hash(), height+1, raw, nil, crypto.Hash32{}, r)
		if tamper {
			if err == nil { t.Fatal("re-signed mutated payment accepted") }
			tip, _, _ := db.GetTip()
			if tip != parent { t.Fatal("failed payment changed canonical tip") }
			return b
		}
		if err != nil { t.Fatal(err) }
		return b
	}
	deadline, amount, _ := store.GenesisVestingTranche(0)
	funding := commit(deadline-1, false)
matchedLiquidity := 0
for _, output := range funding.Txs[0].Outputs {
hs, err := crypto.ScanForOutput(treasury.View.Private, treasury.Spend.Public, output.TxPubKey, output.OneTimePub)
if err == nil && hs != nil {
if core.DecryptAmount(output.EncAmount, hs) != store.GenesisLiquidityAmount { t.Fatal("treasury amount is not exactly 300M") }
matchedLiquidity++
}
}
if matchedLiquidity != 1 { t.Fatal("treasury did not receive one canonical spendable output") }
	c, err := db.LoadLPoDCheckpoint()
	if err != nil || c.GenesisVesting == nil || c.GenesisVesting.Paid != 0 ||
		c.Allocation.Remaining != migration.NominalEligible-c.State.FundingDebit {
		t.Fatalf("canonical earmark failed: %v", err)
	}
	commit(deadline, true)
	payment := commit(deadline, false)
	c, err = db.LoadLPoDCheckpoint()
	if err != nil || c.GenesisVesting.Paid != amount { t.Fatalf("canonical payment failed: %v", err) }
	// Reconstruct the standard wallet-scannable output from public protocol
	// evidence; neither a fake tx hash nor a changed amount is acceptable.
	expected, err := core.BuildLPoDPayoutOutput(store.GenesisVestingRecipient, amount,
		payment.Header.Height, payment.Header.PrevHash, c.Digest())
matches := 0
for _, output := range payment.Txs[0].Outputs { if output == expected { matches++ } }
if err != nil || matches != 1 { t.Fatal("recipient or exact amount mismatch") }
	if err := db.Close(); err != nil { t.Fatal(err) }
	db, err = store.Open(path)
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { db.Close() })
	commit(deadline+1, false)
	c, err = db.LoadLPoDCheckpoint()
	if err != nil || c.GenesisVesting.Paid != amount { t.Fatalf("restart paid twice: %v", err) }
if c.GenesisVesting.LiquidityPaid != store.GenesisLiquidityAmount { t.Fatal("restart duplicated liquidity release") }

	server := api.NewServer(":0", core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
		core.NewUTXOSet(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStore(db)
	server.SetLPoDConfig(migration, func(uint64, crypto.Hash32) bool { return true })
	server.SetGenesisVestingConfig(activation)
	server.SetReady()
	server.SetUTXOReady()
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/genesis-vesting", nil))
	var response struct { State string; Liquidity struct { TxHash string `json:"tx_hash"`; Address string `json:"address"`; Funded string `json:"funded_napro"` }; Schedule []struct {
		TxHash string `json:"tx_hash"`; Finalized bool `json:"finalized"`; Amount string `json:"amount_napro"`
	} }
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != 200 ||
		response.State != "active" || response.Schedule[0].TxHash == "" || !response.Schedule[0].Finalized ||
		response.Schedule[1].TxHash != "" { t.Fatalf("canonical schedule evidence %d %s %v", rec.Code, rec.Body, err) }
if response.Liquidity.TxHash == "" || response.Liquidity.Address != string(treasuryAddress) || response.Liquidity.Funded != "30000000000000000" {
t.Fatal("public liquidity receipt missing or incorrect")
}
	var cursor [8]byte
	binary.LittleEndian.PutUint64(cursor[:], migration.Height)
	if err := db.PutMeta("prune_cursor", cursor[:]); err != nil { t.Fatal(err) }
	if _, err := db.PruneBlocksOlderThan(payment.Header.Height+1); err != nil { t.Fatal(err) }
	for _, b := range []*core.Block{funding, payment} {
		raw, err := db.GetRawBlockByHeight(b.Header.Height)
		var saved core.Block
		if err != nil || json.Unmarshal(raw, &saved) != nil || len(saved.Txs) != 2 {
			t.Fatal("annual funding/payment evidence was pruned")
		}
	}
	// Canonical tip selection after a reorg selects its reserve checkpoint,
	// not the highest/orphaned payment flag.
	if err := db.PutTip(funding.Hash(), funding.Header.Height); err != nil { t.Fatal(err) }
	c, err = db.LoadLPoDCheckpoint()
	if err != nil || c.GenesisVesting.Paid != 0 { t.Fatalf("orphan payment flag survived: %v", err) }
}