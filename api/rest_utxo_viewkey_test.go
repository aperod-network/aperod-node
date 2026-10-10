package api_test

// Tests for the view-key extension to GET /api/v1/address/{addr}/utxos:
//   - Stealth UTXO discovered + amount_napr populated via view_key_hex query param
//   - Stealth UTXO discovered via node-configured view key (SetNodeViewKey)
//   - Transparent/mint UTXO: matched without view key, amount_napr null
//   - Wrong view key: stealth UTXO not discovered

import (
	"encoding/hex"
	"net/http"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// makeStealthUTXO creates a UTXO in the UTXO set that is the result of a stealth
// payment to (spendPub, viewPub).  It returns the UTXO, the plaintext amount, and
// the HsScalar used for amount encryption (needed by the caller if it wants to
// verify decryption independently).
func makeStealthUTXO(t *testing.T, utxos *core.UTXOSet, spendPub, viewPub crypto.Point32, amount uint64, txHashByte byte) (crypto.Hash32, *crypto.Scalar32) {
	t.Helper()

	so, err := crypto.CreateStealthOutput(spendPub, viewPub)
	if err != nil {
		t.Fatalf("CreateStealthOutput: %v", err)
	}

	encAmount := core.EncryptAmount(amount, &so.HsScalar)

	var txHash crypto.Hash32
	txHash[0] = txHashByte
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  so.OneTimePub,
		TxPubKey:    so.TxPubKey,
		EncAmount:   encAmount,
		BlockHeight: 1,
	})
	return txHash, &so.HsScalar
}

// TestREST_AddressUTXOs_StealthDiscoveredByViewKeyHeader verifies that a stealth
// output sent to an address is discovered and its amount decoded when the view
// key is supplied via the X-View-Key request header (query param removed F-046).
func TestREST_AddressUTXOs_StealthDiscoveredByViewKeyHeader(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	const wantAmount = uint64(500_000_000) // 5 APRO in nAPRO
	txHash, _ := makeStealthUTXO(t, utxos, wk.Spend.Public, wk.View.Public, wantAmount, 0x01)

	viewKeyHex := hex.EncodeToString(wk.View.Private[:])
	code, resp := restGetHeader(t, srv, "/api/v1/address/"+string(addr)+"/utxos", "X-View-Key", viewKeyHex)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1 (stealth UTXO must be discovered via ECDH)", len(list))
	}
	entry := list[0].(map[string]interface{})

	if entry["tx_hash"] != hex.EncodeToString(txHash[:]) {
		t.Errorf("tx_hash = %v, want %s", entry["tx_hash"], hex.EncodeToString(txHash[:]))
	}

	// amount_napr must be populated and correct.
	gotAmt, ok := entry["amount_napr"].(float64)
	if !ok || entry["amount_napr"] == nil {
		t.Fatalf("amount_napr = %v (type %T), want %d (must be populated when view key provided)", entry["amount_napr"], entry["amount_napr"], wantAmount)
	}
	if uint64(gotAmt) != wantAmount {
		t.Errorf("amount_napr = %d, want %d", uint64(gotAmt), wantAmount)
	}
}

// TestREST_AddressUTXOs_StealthDiscoveredByNodeViewKey verifies that a stealth
// output is discovered and decoded when the view key is configured on the node
// via SetNodeViewKey (as it would be from node.yaml view_key), with no query param.
func TestREST_AddressUTXOs_StealthDiscoveredByNodeViewKey(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	// Configure view key on the server (simulates node.yaml view_key entry).
	srv.SetNodeViewKey(hex.EncodeToString(wk.View.Private[:]))

	const wantAmount = uint64(1_000_000_000) // 10 APRO
	makeStealthUTXO(t, utxos, wk.Spend.Public, wk.View.Public, wantAmount, 0x02)

	// No view_key_hex query param — node config is used automatically.
	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1 (node-configured view key must discover stealth UTXO)", len(list))
	}
	entry := list[0].(map[string]interface{})

	gotAmt, ok := entry["amount_napr"].(float64)
	if !ok || entry["amount_napr"] == nil {
		t.Fatalf("amount_napr = %v, want %d (node-configured view key must decode amount)", entry["amount_napr"], wantAmount)
	}
	if uint64(gotAmt) != wantAmount {
		t.Errorf("amount_napr = %d, want %d", uint64(gotAmt), wantAmount)
	}
}

// TestREST_AddressUTXOs_NoViewKeyAmountNull verifies that transparent/mint UTXOs
// are returned but amount_napr is null when no view key is provided.
func TestREST_AddressUTXOs_NoViewKeyAmountNull(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	// Add a transparent UTXO (OneTimePub == spendPub, no EncAmount).
	var txHash crypto.Hash32
	txHash[0] = 0x03
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  wk.Spend.Public,
		BlockHeight: 0,
	})

	code, resp := restGet(t, srv, "/api/v1/address/"+string(addr)+"/utxos")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1", len(list))
	}
	entry := list[0].(map[string]interface{})

	// amount_napr must be null for transparent outputs without a view key.
	if entry["amount_napr"] != nil {
		t.Errorf("amount_napr = %v, want null for transparent output with no view key", entry["amount_napr"])
	}
}

// TestREST_AddressUTXOs_WrongViewKeyNoStealth verifies that a stealth UTXO
// addressed to wallet A is NOT discovered when wallet B's view key is used.
func TestREST_AddressUTXOs_WrongViewKeyNoStealth(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wkA, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys A: %v", err)
	}
	wkB, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys B: %v", err)
	}

	// Address for wallet B (attacker) — but UTXO is sent to wallet A.
	addrB := crypto.EncodeAddress(crypto.MainnetByte, wkB.Spend.Public, wkB.View.Public)
	makeStealthUTXO(t, utxos, wkA.Spend.Public, wkA.View.Public, 999_999, 0x04)

	// Query with wallet B's view key against wallet B's address (via header).
	viewKeyBHex := hex.EncodeToString(wkB.View.Private[:])
	code, resp := restGetHeader(t, srv, "/api/v1/address/"+string(addrB)+"/utxos", "X-View-Key", viewKeyBHex)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	list, _ := resp["utxos"].([]interface{})
	if len(list) != 0 {
		t.Errorf("utxos count = %d, want 0 (wrong view key must not discover other wallet's stealth UTXO)", len(list))
	}
}

// TestREST_AddressUTXOs_MintAmountNullWithViewKey verifies that transparent/mint
// outputs (no EncAmount in chain) still have null amount_napr even when a view
// key is provided — the view key alone cannot decrypt mint outputs.
func TestREST_AddressUTXOs_MintAmountNullWithViewKey(t *testing.T) {
	srv, utxos := buildUTXOServer(t)

	wk, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatalf("GenerateWalletKeys: %v", err)
	}
	addr := crypto.EncodeAddress(crypto.MainnetByte, wk.Spend.Public, wk.View.Public)

	// Mint output: OneTimePub = spendPub + height*G, no TxPubKey, no EncAmount.
	const mintHeight = uint64(7)
	heightPub, _ := crypto.ScalarMulBase(crypto.ScalarFromUint64(mintHeight))
	mintPub, _ := crypto.AddPoints(wk.Spend.Public, heightPub)

	var txHash crypto.Hash32
	txHash[0] = 0x05
	utxos.Add(&core.UTXO{
		TxHash:      txHash,
		OutputIndex: 0,
		OneTimePub:  mintPub,
		BlockHeight: mintHeight,
		// TxPubKey and EncAmount intentionally left zero (as BuildMintTx does).
	})

	viewKeyHex := hex.EncodeToString(wk.View.Private[:])
	code, resp := restGetHeader(t, srv, "/api/v1/address/"+string(addr)+"/utxos", "X-View-Key", viewKeyHex)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	list, _ := resp["utxos"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("utxos count = %d, want 1 (mint UTXO matched via height-offset pub)", len(list))
	}
	entry := list[0].(map[string]interface{})

	// amount_napr must be null: mint outputs have no EncAmount to decrypt.
	if entry["amount_napr"] != nil {
		t.Errorf("amount_napr = %v, want null for mint output (view key cannot decrypt zero EncAmount)", entry["amount_napr"])
	}
}
