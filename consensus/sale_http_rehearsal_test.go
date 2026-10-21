package consensus

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/edwards25519"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// Real private node HTTP endpoints, disposable LevelDBs, signatures and wallet
// keys. The invoice is synthetic: no merchant API, production secret or money.
type saleHTTPRehearsal struct {
	t       *testing.T
	server  *api.Server
	http    *httptest.Server
	order   crypto.Hash32
	payment crypto.Hash32
	cp      crypto.Hash32
	address crypto.Address
}

func newSaleHTTPRehearsal(t *testing.T, e *Engine, m *store.SaleMigration, address crypto.Address) *saleHTTPRehearsal {
	t.Helper()
	s := api.NewServer("127.0.0.1:0", e.chain, e.pool, e.utxos, testNopLogger())
	s.SetStore(e.cfg.Store)
	s.SetValidatorKey(e.cfg.MyKey)
	s.SetAPIKey("offline-sale-fixture-only")
	s.SetSaleConfig(m, e.CanSettleSale, e.IsSaleFinalizedHash)
	h := httptest.NewServer(s)
	h.Client().Timeout = 5 * time.Second
	t.Cleanup(h.Close)
	// Same globally separated order/payment identities as the API adapter.
	order := sha256.Sum256([]byte("aperod/protocol-sale/order/v1\x00" +
		hex.EncodeToString(m.Checkpoint.Genesis[:]) + "\x00offline-epoch-purchase"))
	return &saleHTTPRehearsal{t: t, server: s, http: h, order: crypto.Hash32(order),
		payment: store.SaleProviderPayment("offline-provider-invoice"), cp: m.Checkpoint.Message(), address: address}
}

func (f *saleHTTPRehearsal) request(path string, body any, expected int) map[string]any {
	f.t.Helper()
	method := http.MethodGet
	var raw []byte
	if body != nil {
		method = http.MethodPost
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r, err := http.NewRequest(method, f.http.URL+"/api/v1/admin/protocol-sales/"+path, bytes.NewReader(raw))
	if err != nil {
		f.t.Fatal(err)
	}
	r.Header.Set("X-API-Key", "offline-sale-fixture-only")
	r.Header.Set("Content-Type", "application/json")
	response, err := f.http.Client().Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		f.t.Fatal(err)
	}
	if response.StatusCode != expected {
		f.t.Fatalf("%s: HTTP %d, expected %d: %v", path, response.StatusCode, expected, result)
	}
	return result
}

func (f *saleHTTPRehearsal) reserveBody() map[string]any {
	return map[string]any{"order": hex.EncodeToString(f.order[:]), "checkpoint": hex.EncodeToString(f.cp[:]),
		"address": string(f.address), "amount_napro": "10000000000", "quote_usd": "8"}
}

func (f *saleHTTPRehearsal) receipt(status string) map[string]any {
	f.t.Helper()
	r := f.request("receipt?order="+hex.EncodeToString(f.order[:]), nil, 200)
	if r["status"] != status || r["amount_napro"] != "10000000000" {
		f.t.Fatal("wrong canonical purchase receipt", r)
	}
	return r
}

func (f *saleHTTPRehearsal) enqueue() {
	f.t.Helper()
	capability := f.request("status", nil, 200)
	if capability["ready"] != true || capability["allocation_basis"] != "operator_attested_lpod_epoch" ||
		capability["historical_supply_verified"] != false {
		f.t.Fatal("epoch admission misrepresented history", capability)
	}
	for i := 0; i < 2; i++ {
		f.request("reserve", f.reserveBody(), 200)
	}
	changed := f.reserveBody()
	changed["amount_napro"] = "10000000001"
	f.request("reserve", changed, 409)
	invoice := map[string]any{"order": hex.EncodeToString(f.order[:]), "invoice": store.SaleInvoice{
		ProviderID: "offline-provider-invoice", Currency: "btc", Address: "offline-only-deposit",
		Amount: "0.00100000001", PriceUSD: "8",
	}}
	for i := 0; i < 2; i++ {
		f.request("invoice", invoice, 200)
	}
	paid := map[string]any{"order": hex.EncodeToString(f.order[:]), "payment": hex.EncodeToString(f.payment[:])}
	incorrect := crypto.HashBytes([]byte("another-payment"))
	f.request("paid", map[string]any{"order": paid["order"], "payment": hex.EncodeToString(incorrect[:])}, 409)
	for i := 0; i < 2; i++ {
		f.request("paid", paid, 200)
	}
	f.receipt("payment_confirmed") // A fully paid invoice is still NOT completion.
}

func (f *saleHTTPRehearsal) acknowledge(keys *crypto.WalletKeyPair) {
	f.t.Helper()
	r := f.receipt("finalized")
	challenge, ok := r["owner_challenge"].(string)
	if !ok || challenge == "" {
		f.t.Fatal("missing canonical owner challenge")
	}
	body := map[string]any{"order": hex.EncodeToString(f.order[:]), "block_hash": r["block_hash"],
		"tx_hash": r["tx_hash"], "signature": strings.Repeat("00", 64)}
	f.request("recover", body, 409)
	x, err := new(edwards25519.Scalar).SetCanonicalBytes(keys.Spend.Private[:])
	if err != nil {
		f.t.Fatal(err)
	}
	nonce := sha512.New()
	nonce.Write([]byte("APRO_WEB_AUTH_NONCE_V1\x00"))
	nonce.Write(keys.Spend.Private[:])
	nonce.Write([]byte(challenge))
	secret, err := new(edwards25519.Scalar).SetUniformBytes(nonce.Sum(nil))
	if err != nil {
		f.t.Fatal(err)
	}
	R := new(edwards25519.Point).ScalarBaseMult(secret).Bytes()
	h := sha512.New()
	h.Write([]byte("APRO_WEB_AUTH_CHALLENGE_V1\x00"))
	h.Write([]byte(challenge))
	h.Write(keys.Spend.Public[:])
	h.Write(R)
	c, err := new(edwards25519.Scalar).SetUniformBytes(h.Sum(nil))
	if err != nil {
		f.t.Fatal(err)
	}
	body["signature"] = hex.EncodeToString(append(append([]byte{}, R...), new(edwards25519.Scalar).MultiplyAdd(c, x, secret).Bytes()...))
	for i := 0; i < 2; i++ {
		f.request("recover", body, 200)
	}
	f.receipt("completed")
}

func finalizeSaleRehearsal(t *testing.T, engines []*Engine, keys []crypto.ValidatorPrivKey, b *core.Block) {
	t.Helper()
	for _, e := range engines {
		for _, key := range keys {
			sig, err := key.Sign(crypto.HashBytes([]byte("aperod/finalize/v1"), b.Hash().Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if err := e.HandleVote(FinalizeMsg{Height: b.Header.Height, BlockHash: b.Hash(), ValidatorPub: key.Public(), Signature: sig}); err != nil {
				t.Fatal(err)
			}
		}
		if !e.IsSaleFinalizedHash(b.Header.Height, b.Hash()) {
			t.Fatal("actual two-validator finality missing")
		}
	}
}
