// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

package consensus

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

// Opt-in because this drives a real installed Chromium and an isolated PostgreSQL.
// No production connections, keys, validator process or funds are used.
func TestNativeChromeCanonicalTransfer(t *testing.T) {
	if os.Getenv("NATIVE_CHROME_TRANSFER_E2E") != "1" {
		t.Skip("opt-in native Chromium: NATIVE_CHROME_TRANSFER_E2E=1")
	}
	senderPhrase, err := wallet.GenerateMnemonic(wallet.Strength256)
	if err != nil {
		t.Fatal(err)
	}
	recipientPhrase, err := wallet.GenerateMnemonic(wallet.Strength256)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := wallet.DeriveFromMnemonic(senderPhrase, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := wallet.DeriveFromMnemonic(recipientPhrase, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, db, deposit, action, keys, priv, _ := positionEngineWithOwner(t, owner.Keys)
	e.cfg.RingCTCLSAGActivationHeight = 2
	e.utxos.SetCLSAGActivationHeight(2)
	e.pool.SetVerifier(e.txVerifier)
	acceptPositionBlock(t, e, priv, 3_000_000_000, *deposit)
	// These are consensus-authorized payout outputs, not UTXO/decoy mocks.
	for i := 0; i < 20; i++ {
		acceptPositionBlock(t, e, priv, 3_000_000_000)
	}
	exit := action
	// Keep each JSON-wire monetary value below the frozen signer's safe integer
	// boundary. This is a real partial canonical withdrawal, not a rounded value.
	exit.Action, exit.Nonce, exit.WithdrawAmount = core.LPoDWithdraw, 1, 1_000_000*100_000_000
	withdrawal, err := core.BuildLPoDPositionTx(exit, keys.Spend.Private, crypto.Scalar32{})
	if err != nil {
		t.Fatal(err)
	}
	acceptPositionBlock(t, e, priv, 1, *withdrawal)
	tip := e.chain.Tip()
	if !e.IsFinalizedHash(tip.Header.Height, tip.Hash()) {
		t.Fatal("fixture lacks real finality")
	}
	outputs, _, err := db.LPoDWalletOutputs(action.Beneficiary, "", 128)
	if err != nil {
		t.Fatal(err)
	}
	var starting uint64
	for _, u := range outputs {
		hs, err := crypto.ScanForOutput(keys.View.Private, keys.Spend.Public, u.TxPubKey, u.OneTimePub)
		if err != nil || hs == nil {
			t.Fatal("fixture output is not owned")
		}
		starting += core.DecryptAmount(u.EncAmount, hs)
	}
	if len(outputs) < 16 || starting == 0 {
		t.Fatal("fixture funding/decoy prerequisites not met")
	}
	server := api.NewServer(":0", e.chain, e.pool, e.utxos, slog.New(slog.NewTextHandler(io.Discard, nil)))
	server.SetStore(db)
	server.SetLPoDConfig(e.cfg.LPoDMigration, e.IsFinalizedHash)
	server.SetWalletSnapshotCapture(db.NewWalletReadSnapshot)
	// Deliberately isolated test credential; never a workspace/production key.
	const rpcKey = "native-chrome-isolated-fixture-only"
	server.SetAPIKey(rpcKey)
	server.SetReady()
	server.SetUTXOReady()
	var mu sync.Mutex
	issuedToOwner := func() uint64 {
		rows, _, err := db.LPoDWalletOutputs(action.Beneficiary, "", 128)
		if err != nil {
			t.Error(err)
			return 0
		}
		var total uint64
		for _, u := range rows {
			hs, err := crypto.ScanForOutput(keys.View.Private, keys.Spend.Public, u.TxPubKey, u.OneTimePub)
			if err != nil || hs == nil {
				t.Error("unowned canonical funding output")
				return 0
			}
			total += core.DecryptAmount(u.EncAmount, hs)
		}
		return total
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture/mine", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", 405)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		transactions := e.pool.SelectTxs(100)
		if len(transactions) != 1 {
			http.Error(w, "expected exactly one genuine mempool transaction", 409)
			return
		}
		beforeFunding := issuedToOwner()
		block := acceptPositionBlock(t, e, priv, 1, transactions...)
		if !e.IsFinalizedHash(block.Header.Height, block.Hash()) {
			t.Error("transfer block not finalized")
			http.Error(w, "not finalized", 500)
			return
		}
		accepted := transactions[0]
		canonical := e.chain.GetByHeight(block.Header.Height)
		found := false
		for _, tx := range canonical.Txs {
			if tx.Hash() == accepted.Hash() {
				found = true
			}
		}
		if !found {
			t.Error("accepted hash not in canonical block")
			http.Error(w, "missing transaction", 500)
			return
		}
		durable, err := db.GetBlock(block.Hash())
		if err != nil || durable == nil {
			t.Error("canonical block not durable")
			http.Error(w, "not durable", 500)
			return
		}
		// Exercise the real API's 10-confirmation bookkeeping, without faking status.
		for i := 0; i < 10; i++ {
			acceptPositionBlock(t, e, priv, 1)
		}
		afterFunding := issuedToOwner()
		if afterFunding < beforeFunding {
			t.Error("funding index unexpectedly shrank")
			http.Error(w, "funding index changed", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"tx_hash": fmt.Sprintf("%x", accepted.Hash()), "block_hash": fmt.Sprintf("%x", block.Hash()),
			"height": block.Header.Height, "fee_napr": accepted.Fee, "finalized": true,
			"canonical_funding_credit_napr": fmt.Sprint(afterFunding - beforeFunding),
		})
	})
	mux.Handle("/", server)
	httpServer := httptest.NewServer(mux)
	defer httpServer.Close()
	fixtureDir := t.TempDir()
	input := filepath.Join(fixtureDir, "private-input.json")
	raw, _ := json.Marshal(map[string]any{
		"senderPhrase": senderPhrase, "recipientPhrase": recipientPhrase,
		"sender": string(action.Beneficiary), "recipient": string(crypto.AddressFromKeys(crypto.MainnetByte, recipient.Keys)),
		"starting": fmt.Sprint(starting), "nodeURL": httpServer.URL, "rpcKey": rpcKey,
	})
	if err := os.WriteFile(input, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("pnpm", "--filter", "@workspace/api-server", "exec", "vitest", "run",
		"src/routes/wallet-native-chrome.integration.test.ts", "--maxWorkers=1")
	cmd.Dir = "../.."
	// No workspace credentials may be inherited by the fixture or browser.
for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "LD_LIBRARY_PATH", "NATIVE_CHROME_ARCHIVE", "NATIVE_CHROME_EXECUTABLE", "NATIVE_WALLET_SCREENSHOT"} {
		if value, ok := os.LookupEnv(name); ok {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "NATIVE_TRANSFER_INPUT="+input)
	cmd.WaitDelay = 5 * time.Second
	result, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native transfer fixture failed: %v\n%s", err, result)
	}
	t.Log(string(result))
}
