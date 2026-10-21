package api_test

import (
	"bytes"
	stded25519 "crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

func minimalAVMContractCode(t *testing.T) []byte {
	t.Helper()
	code, err := hex.DecodeString("0061736d01000000010401600000030201000707010372756e00000a040102000b")
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func requestAVM(t *testing.T, server *api.Server, path string, body any) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, req)
	return response.Code, response.Body.Bytes()
}

func TestAVMGasEstimateReturnsExecutionAndReservationCost(t *testing.T) {
	server, _ := buildChainServer(t, 0)
	state := avm.NewMemoryStore()
	server.SetAVMStore(1, state)
	server.SetReady()
	code := minimalAVMContractCode(t)
	contractID := strings.Repeat("09", 32)
	id, _ := hex.DecodeString(contractID)
	codeKey := append([]byte{0xff, 'a', 'v', 'm', '/', 'c', '/'}, id...)
	if err := state.Apply([]avm.Write{{Key: codeKey, Value: code}}); err != nil {
		t.Fatal(err)
	}

	status, body := requestAVM(t, server, "/api/v1/avm/estimate-gas", map[string]any{
		"contract_id": contractID,
		"entry":       "run",
		"gas_limit":   uint64(1_000),
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var estimate struct {
		ContractID string `json:"contract_id"`
		GasUsed    uint64 `json:"gas_used"`
		GasLimit   uint64 `json:"gas_limit"`
		GasPrice   uint64 `json:"gas_price_napr"`
		GasFee     uint64 `json:"gas_fee_napr"`
		Committed  bool   `json:"committed"`
	}
	if err := json.Unmarshal(body, &estimate); err != nil {
		t.Fatal(err)
	}
	if estimate.ContractID != contractID || estimate.GasLimit != 1_000 ||
		estimate.GasPrice != core.AVMGasPriceNAPR ||
		estimate.GasFee != 1_000*core.AVMGasPriceNAPR ||
		estimate.GasUsed == 0 || estimate.Committed {
		t.Fatalf("unexpected gas estimate: %+v", estimate)
	}
}

func TestAVMQueryRejectsMalformedPayloadAndGasEstimateReportsOutOfGas(t *testing.T) {
	server, _ := buildChainServer(t, 0)
	state := avm.NewMemoryStore()
	server.SetAVMStore(1, state)
	server.SetReady()
	code := minimalAVMContractCode(t)
	contractID := strings.Repeat("0a", 32)
	id, _ := hex.DecodeString(contractID)
	codeKey := append([]byte{0xff, 'a', 'v', 'm', '/', 'c', '/'}, id...)
	if err := state.Apply([]avm.Write{{Key: codeKey, Value: code}}); err != nil {
		t.Fatal(err)
	}

	status, body := requestAVM(t, server, "/api/v1/avm/query", map[string]any{
		"contract_id": contractID,
		"entry":       "run",
		"gas_limit":   core.AVMMinGasLimit,
		"unexpected":  true,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("malformed query status=%d body=%s", status, body)
	}

	status, body = requestAVM(t, server, "/api/v1/avm/estimate-gas", map[string]any{
		"contract_id":  contractID,
		"entry":        "run",
		"gas_limit":    core.AVMMinGasLimit,
		"calldata_hex": strings.Repeat("00", int(core.AVMMinGasLimit)+1),
	})
	if status != http.StatusUnprocessableEntity || !bytes.Contains(body, []byte("out of gas")) {
		t.Fatalf("out-of-gas estimate status=%d body=%s", status, body)
	}
}

func signedAVMSubmissionForTest(t *testing.T, nonce uint64) core.Transaction {
	t.Helper()
	publicKey, privateKey, err := stded25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := &core.AVMPayload{
		Action:   core.AVMDeployContract,
		Code:     []byte{0, 'a', 's', 'm'},
		Entry:    "run",
		Nonce:    nonce,
		GasLimit: core.AVMMinGasLimit,
	}
	copy(payload.Signer[:], publicKey)
	payload.ContractID = core.DeriveAVMContractID(payload.Signer, nonce, payload.Code)
	signingHash := payload.SigningHash()
	copy(payload.Signature[:], stded25519.Sign(privateKey, signingHash[:]))

	var keyImage crypto.KeyImage
	keyImage[0] = byte(nonce + 1)
	responses := make([][32]byte, crypto.RingSize)
	return core.Transaction{
		Version: core.TxVersionAVM,
		Inputs: []core.RingInput{{
			KeyImage:        keyImage,
			Ring:            make([]crypto.RingMember, crypto.RingSize),
			RingCommitments: make([]crypto.Commitment, crypto.RingSize),
		}},
		Outputs:         []core.Output{{OneTimePub: crypto.Point32{1}}},
		Fee:             1_000_000_000,
		RangeProofs:     []*crypto.RangeProof{{}},
		CLSAGSignatures: []*crypto.CLSAGSignature{{S: responses}},
		AVM:             payload,
	}
}

func newAVMSubmissionServer(t *testing.T, expectedNonce uint64, broadcasts *int) *api.Server {
	t.Helper()
	config := core.DefaultMempoolConfig()
	config.AVMActivationHeight = 1
	config.RingCTCLSAGActivationHeight = 2
	config.CurrentHeight = func() uint64 { return 1 }
	config.AVMNonceLookup = func([32]byte) (uint64, error) { return expectedNonce, nil }
	server := api.NewServer(":0", core.NewChain(), core.NewMempool(config), core.NewUTXOSet(), testLogger())
	server.SetAVMStore(1, avm.NewMemoryStore())
	server.SetReady()
	server.SetTransactionBroadcaster(func(*core.Transaction) { *broadcasts++ })
	return server
}

func TestAVMSubmitRejectsNonceConflictAndBroadcastsValidTransaction(t *testing.T) {
	var broadcasts int
	for _, test := range []struct {
		name          string
		expectedNonce uint64
		txNonce       uint64
		wantStatus    int
		wantBroadcast int
	}{
		{name: "stale nonce", expectedNonce: 7, txNonce: 6, wantStatus: http.StatusUnprocessableEntity},
		{name: "accepted", expectedNonce: 0, txNonce: 0, wantStatus: http.StatusAccepted, wantBroadcast: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			broadcasts = 0
			server := newAVMSubmissionServer(t, test.expectedNonce, &broadcasts)
			status, body := requestAVM(t, server, "/api/v1/avm/transactions", map[string]any{
				"tx": signedAVMSubmissionForTest(t, test.txNonce),
			})
			if status != test.wantStatus {
				t.Fatalf("status=%d body=%s", status, body)
			}
			if broadcasts != test.wantBroadcast {
				t.Fatalf("broadcasts=%d, want %d", broadcasts, test.wantBroadcast)
			}
			if test.wantStatus == http.StatusUnprocessableEntity &&
				!bytes.Contains(body, []byte("AVM nonce")) {
				t.Fatalf("nonce conflict not reported: %s", body)
			}
			if test.wantStatus == http.StatusAccepted {
				var response map[string]any
				if err := json.Unmarshal(body, &response); err != nil {
					t.Fatal(err)
				}
				if response["status"] != "pending" || response["version"] != float64(core.TxVersionAVM) {
					t.Fatalf("unexpected submission response: %v", response)
				}
			}
		})
	}
}
