package api_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
)

func TestAVMStatusAndCanonicalLookups(t *testing.T) {
	srv, _ := buildChainServer(t, 2)
	state := avm.NewMemoryStore()
	srv.SetAVMStore(3, state)
	srv.SetReady()

	status, body := restGet(t, srv, "/api/v1/avm/status")
	if status != http.StatusOK || body["available"] != true || body["ready"] != true || body["active"] != true {
		t.Fatalf("status=%d body=%v", status, body)
	}

	var contract [32]byte
	contract[0] = 0x42
	contractHex := hex.EncodeToString(contract[:])
	codeKey := append([]byte{0xff, 'a', 'v', 'm', '/', 'c', '/'}, contract[:]...)
	stateKey := append(append(append([]byte{}, contract[:]...), '/'), []byte("owner")...)
	if err := state.Apply([]avm.Write{
		{Key: codeKey, Value: []byte{0, 97, 115, 109}},
		{Key: stateKey, Value: []byte("alice")},
	}); err != nil {
		t.Fatal(err)
	}

	status, body = restGet(t, srv, "/api/v1/avm/contracts/"+contractHex+"/code")
	if status != http.StatusOK || body["value_hex"] != "0061736d" {
		t.Fatalf("code status=%d body=%v", status, body)
	}
	status, body = restGet(t, srv, "/api/v1/avm/contracts/"+contractHex+"/state/6f776e6572")
	if status != http.StatusOK || body["value_hex"] != "616c696365" {
		t.Fatalf("state status=%d body=%v", status, body)
	}
}

func TestAVMNonceAndReceiptLookup(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	state := avm.NewMemoryStore()
	srv.SetAVMStore(1, state)
	srv.SetReady()
	signer := bytes.Repeat([]byte{0x11}, 32)
	txHash := bytes.Repeat([]byte{0x22}, 32)
	contract := bytes.Repeat([]byte{0x33}, 32)
	nonce := make([]byte, 8)
	binary.LittleEndian.PutUint64(nonce, 7)
	receipt := append(append([]byte{}, txHash...), contract...)
	number := make([]byte, 8)
	binary.LittleEndian.PutUint64(number, 123)
	receipt = append(receipt, number...)
	binary.LittleEndian.PutUint64(number, 2)
	receipt = append(receipt, number...)
	length := make([]byte, 4)
	binary.LittleEndian.PutUint32(length, 2)
	receipt = append(receipt, length...)
	receipt = append(receipt, 0xca, 0xfe)
	if err := state.Apply([]avm.Write{
		{Key: append([]byte{0xff, 'a', 'v', 'm', '/', 'n', '/'}, signer...), Value: nonce},
		{Key: append([]byte{0xff, 'a', 'v', 'm', '/', 'r', '/'}, txHash...), Value: receipt},
	}); err != nil {
		t.Fatal(err)
	}
	status, body := restGet(t, srv, "/api/v1/avm/signers/"+hex.EncodeToString(signer)+"/nonce")
	if status != http.StatusOK || body["nonce"] != float64(7) {
		t.Fatalf("nonce status=%d body=%v", status, body)
	}
	status, body = restGet(t, srv, "/api/v1/avm/receipts/"+hex.EncodeToString(txHash))
	if status != http.StatusOK || body["gas_used"] != float64(123) || body["return_data_hex"] != "cafe" {
		t.Fatalf("receipt status=%d body=%v", status, body)
	}
}

func TestAVMSubmitRejectsUnsignedOrNonV6Transactions(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	srv.SetReady()
	raw, _ := json.Marshal(map[string]interface{}{"tx": map[string]interface{}{"Version": 5}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/avm/transactions", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("locally signed v6")) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestAVMSubmitFailsClosedUntilP2PBroadcasterIsReady(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	srv.SetReady()
	raw, err := json.Marshal(map[string]interface{}{
		"tx": core.Transaction{
			Version: core.TxVersionAVM,
			AVM:     &core.AVMPayload{Action: core.AVMExecuteContract},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/avm/transactions", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("broadcaster is not ready")) {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}

func TestAVMReadOnlyQueryExecutesCanonicalCode(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	state := avm.NewMemoryStore()
	srv.SetAVMStore(1, state)
	srv.SetReady()
	var contract [32]byte
	contract[0] = 9
	// Minimal deterministic module exporting run: () -> ().
	code, err := hex.DecodeString("0061736d01000000010401600000030201000707010372756e00000a040102000b")
	if err != nil {
		t.Fatal(err)
	}
	codeKey := append([]byte{0xff, 'a', 'v', 'm', '/', 'c', '/'}, contract[:]...)
	if err := state.Apply([]avm.Write{{Key: codeKey, Value: code}}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]interface{}{
		"contract_id": hex.EncodeToString(contract[:]),
		"entry":       "run",
		"gas_limit":   1000,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/avm/query", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["read_only"] != true {
		t.Fatalf("response=%v", response)
	}
	if response["simulation"] != false || response["committed"] != false {
		t.Fatalf("response=%v", response)
	}
}

func TestAVMEndpointsStayUnavailableDuringStartupRecovery(t *testing.T) {
	srv, _ := buildChainServer(t, 0)
	srv.SetAVMStore(1, avm.NewMemoryStore())

	status, body := restGet(t, srv, "/api/v1/avm/status")
	if status != http.StatusOK || body["available"] != true || body["ready"] != false || body["active"] != false {
		t.Fatalf("status=%d body=%v", status, body)
	}

	status, body = restGet(t, srv, "/api/v1/avm/contracts/"+strings.Repeat("00", 32)+"/code")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%v", status, body)
	}
}
