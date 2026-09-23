// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/lpod"
	"github.com/aperod/aperod/wallet"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestLPoDCanonicalWalletPayoutDiscovery(t *testing.T) {
	const mnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	keys, err := wallet.DeriveFromMnemonic(mnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, db, deposit, a, owner, priv, _ := positionEngineWithOwner(t, keys.Keys)
	server := api.NewServer(":0", e.chain, e.pool, e.utxos, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStore(db)
	server.SetLPoDConfig(e.cfg.LPoDMigration, func(uint64, crypto.Hash32) bool { return true })
	server.SetReady()
	server.SetUTXOReady()
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	get := func() []struct {
		Hash  string `json:"tx_hash"`
		Index uint32 `json:"out_idx"`
	} {
		t.Helper()
		response, err := http.Get(httpServer.URL + "/api/v1/lpod/wallet-outputs?address=" + string(a.Beneficiary))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body struct {
			State   string `json:"state"`
			Outputs []struct {
				Hash  string `json:"tx_hash"`
				Index uint32 `json:"out_idx"`
			} `json:"outputs"`
		}
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil || response.StatusCode != 200 || body.State != "active" {
			t.Fatalf("output API %d %v", response.StatusCode, err)
		}
		return body.Outputs
	}
	deposited := acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	if len(get()) != 0 {
		t.Fatal("sole source was deposited; no payout yet")
	}
	exit := a
	exit.Action = core.LPoDWithdraw
	exit.Nonce = 1
	exit.WithdrawAmount = 10_000_000 * lpod.Unit
	partial, err := core.BuildLPoDPositionTx(exit, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	block := acceptPositionBlock(t, e, priv, 1, *partial)
	if len(get()) != 1 {
		t.Fatal("partial return missing from wallet index")
	}
	if err := e.utxos.RollbackBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := e.chain.RollbackLastBlock(block); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(deposited.Hash(), deposited.Header.Height); err != nil {
		t.Fatal(err)
	}
	if len(get()) != 0 {
		t.Fatal("orphan payout survived rollback")
	}
	if err := e.handleIncomingBlock(block); err != nil {
		t.Fatal(err)
	}
	// Authentic payouts provide the ordinary v5 ring's on-chain decoys too.
	for i := 0; i < 20; i++ {
		acceptPositionBlock(t, e, priv, 3_000_000_000)
	}
	exit.Nonce = 2
	exit.WithdrawAmount = 0
	full, err := core.BuildLPoDPositionTx(exit, owner.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	acceptPositionBlock(t, e, priv, 1, *full)
	rows := get()
	if len(rows) < 3 {
		t.Fatal("partial, reward, or full return not indexed")
	}
	outputs, _, err := db.LPoDWalletOutputs(a.Beneficiary, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	var total uint64
	for _, u := range outputs {
		hs, err := crypto.ScanForOutput(owner.View.Private, owner.Spend.Public, u.TxPubKey, u.OneTimePub)
		if err != nil || hs == nil {
			t.Fatal("indexed output not owned")
		}
		total += core.DecryptAmount(u.EncAmount, hs)
	}
	c, err := db.LoadLPoDCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if total != a.Amount+c.State.AngelPaid {
		t.Fatalf("returned principal/rewards %d, expected %d", total, a.Amount+c.State.AngelPaid)
	}
	if os.Getenv("LPOD_WASM_E2E") != "1" {
		return
	}
	dir := t.TempDir()
	result := filepath.Join(dir, "signed.json")
	cmd := exec.Command("pnpm", "--filter", "@workspace/api-server", "exec", "vitest", "run", "src/routes/wallet-lpod-discovery.integration.test.ts")
	cmd.Dir = "../.."
	cmd.Env = append(os.Environ(), "LPOD_LIVE_TEST_URL="+httpServer.URL, "LPOD_TEST_ADDRESS="+string(a.Beneficiary),
		"LPOD_TEST_GENESIS="+fmt.Sprintf("%x", a.Genesis[:]), "LPOD_TEST_VAULT="+fmt.Sprintf("%x", a.Vault[:]), "LPOD_TEST_RESULT="+result)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real HTTP/SQL-empty/WASM integration: %v\n%s", err, raw)
	}
	var signed struct {
		Ordinary  core.Transaction `json:"ordinary"`
		Redeposit core.Transaction `json:"redeposit"`
		Source    struct {
			Hash  string `json:"tx_hash"`
			Index uint32 `json:"out_idx"`
		} `json:"source"`
		KeyImage string `json:"key_image"`
	}
	raw, err = os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &signed); err != nil {
		t.Fatal(err)
	}
	if _, err := signed.Redeposit.LPoDPositionAction(); err != nil {
		t.Fatal(err)
	}
	if err := e.txVerifier.VerifyTx(&signed.Ordinary); err != nil {
		t.Fatalf("ordinary payout spend: %v", err)
	}
	status := func() bool {
		payload, _ := json.Marshal(map[string]interface{}{"key_images": []string{signed.KeyImage}, "refs": []interface{}{signed.Source}})
		response, err := http.Post(httpServer.URL+"/api/v1/wallet/key-images", "application/json", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body struct {
			Spent map[string]bool `json:"spent"`
		}
		if json.NewDecoder(response.Body).Decode(&body) != nil || response.StatusCode != 200 {
			t.Fatal("key image status unavailable")
		}
		return body.Spent[signed.KeyImage]
	}
	if status() {
		t.Fatal("unspent canonical payout hidden")
	}
	beforeRedeposit := e.chain.Tip()
	redeposited := acceptPositionBlock(t, e, priv, 1, signed.Redeposit)
	if !status() {
		t.Fatal("native re-deposit source still selectable")
	}
	for _, row := range get() {
		if row.Hash == signed.Source.Hash && row.Index == signed.Source.Index {
			t.Fatal("indexed re-deposit source was not excluded")
		}
	}
	if err := e.utxos.RollbackBlock(redeposited); err != nil {
		t.Fatal(err)
	}
	if err := e.chain.RollbackLastBlock(redeposited); err != nil {
		t.Fatal(err)
	}
	if err := db.PutTip(beforeRedeposit.Hash(), beforeRedeposit.Header.Height); err != nil {
		t.Fatal(err)
	}
	if status() {
		t.Fatal("rolled-back native re-deposit source not restored")
	}
	if err := e.pool.Add(signed.Ordinary); err != nil {
		t.Fatal(err)
	}
	if !status() {
		t.Fatal("pending ordinary source still selectable")
	}
	e.pool.Remove(signed.Ordinary.Hash())
	if status() {
		t.Fatal("dropped mempool source remains locked")
	}
	e.cfg.RingCTCLSAGActivationHeight = 2
	// The competing spend branch is an independent fixture execution, not a
	// permission to equivocate. Reset only the test detector's signer history.
	e.slashing = newSlashingDetector()
	acceptPositionBlock(t, e, priv, 1, signed.Ordinary)
	if !status() {
		t.Fatal("canonically spent ordinary source still selectable")
	}
	t.Log(string(raw[:0]), "SQL-empty signing material → real WASM ordinary spend and redeposit verified")
}
