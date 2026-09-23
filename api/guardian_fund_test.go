package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func TestRESTGuardianFundStates(t *testing.T) {
	srv, chain := buildChainServer(t, 0)
	if code, body := restGet(t, srv, "/api/v1/guardian-fund"); code != http.StatusOK ||
		body["state"] != "disabled" || body["lock_policy"] != "consensus_locked_v1" {
		t.Fatalf("disabled response: %d %#v", code, body)
	}
	anchor := chain.Tip().Hash()
	srv.SetGuardianFundConfig(2, anchor)
	if _, body := restGet(t, srv, "/api/v1/guardian-fund"); body["state"] != "pending" {
		t.Fatalf("pending response: %#v", body)
	}
	srv.SetGuardianFundConfig(2, crypto.HashBytes([]byte("wrong-genesis")))
	if _, body := restGet(t, srv, "/api/v1/guardian-fund"); body["state"] != "invalid_configuration" {
		t.Fatalf("wrong-genesis response: %#v", body)
	}
	srv.SetGuardianFundConfig(2, anchor)

	// Add an activation-height block without the expected transaction; the
	// endpoint must fail closed as missing.
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	txs := []core.Transaction{core.CoinbaseTx(crypto.Point32(pub), 1)}
	h := core.BlockHeader{Height: 1, PrevHash: anchor, Timestamp: time.Now().UnixNano(), ValidatorPub: pub, MerkleRoot: core.MerkleRoot(txs)}
	if err := h.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := chain.AddBlock(&core.Block{Header: h, Txs: txs}); err != nil {
		t.Fatal(err)
	}
	srv.SetGuardianFundConfig(1, anchor)
	if _, body := restGet(t, srv, "/api/v1/guardian-fund"); body["state"] != "missing" {
		t.Fatalf("missing response: %#v", body)
	}
}

func TestRESTGuardianFundCanonicalDetectedIsNotFinalized(t *testing.T) {
	srv, chain := buildChainServer(t, 0)
	anchor := chain.Tip().Hash()
	guardian, err := core.BuildGuardianFundTx(anchor, 1)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	// No reward exists in this fixture, therefore the canonical Guardian index
	// is zero. The block is added directly because this is an API derivation
	// test, not a consensus acceptance test.
	txs := []core.Transaction{guardian}
	h := core.BlockHeader{Height: 1, PrevHash: anchor, Timestamp: time.Now().UnixNano(), ValidatorPub: pub, MerkleRoot: core.MerkleRoot(txs)}
	if err := h.Sign(priv); err != nil {
		t.Fatal(err)
	}
	if err := chain.AddBlock(&core.Block{Header: h, Txs: txs}); err != nil {
		t.Fatal(err)
	}
	srv.SetGuardianFundConfig(1, anchor)
	_, body := restGet(t, srv, "/api/v1/guardian-fund")
	if body["state"] != "canonical_detected" || body["amount_napro"] != "100000000000000000" ||
		body["protocol_locked"] != true || body["lock_policy"] != "consensus_locked_v1" ||
		body["output_index"].(float64) != 0 {
		t.Fatalf("canonical-detected response: %#v", body)
	}
}
