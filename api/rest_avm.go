package api

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

var (
	avmCodePrefix    = []byte{0xff, 'a', 'v', 'm', '/', 'c', '/'}
	avmNoncePrefix   = []byte{0xff, 'a', 'v', 'm', '/', 'n', '/'}
	avmReceiptPrefix = []byte{0xff, 'a', 'v', 'm', '/', 'r', '/'}
)

type avmAccessJSON struct {
	KeyHex string `json:"key_hex"`
	Write  bool   `json:"write"`
}

type avmExecutionJSON struct {
	Action      string          `json:"action,omitempty"`
	ContractID  string          `json:"contract_id"`
	CodeHex     string          `json:"code_hex,omitempty"`
	Entry       string          `json:"entry"`
	CalldataHex string          `json:"calldata_hex,omitempty"`
	GasLimit    uint64          `json:"gas_limit"`
	AccessList  []avmAccessJSON `json:"access_list,omitempty"`
}

func (s *Server) avmStatus() map[string]interface{} {
	height := uint64(0)
	if s.chain != nil {
		height = s.chain.Height()
	}
	next := height
	if next < ^uint64(0) {
		next++
	}
	ready := atomic.LoadInt32(&s.syncing) == 0
	active := ready && s.avmActivationHeight > 0 && next >= s.avmActivationHeight
	return map[string]interface{}{
		"available":           s.avmStore != nil,
		"ready":               ready,
		"active":              active,
		"activation_height":   s.avmActivationHeight,
		"chain_height":        height,
		"next_block_height":   next,
		"transaction_version": uint8(core.TxVersionAVM),
		"gas_price_napr":      core.AVMGasPriceNAPR,
		"min_gas_limit":       core.AVMMinGasLimit,
		"max_gas_limit":       core.AVMMaxGasLimit,
	}
}

func (s *Server) restAVMStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	writeJSON(w, http.StatusOK, s.avmStatus())
}

func decodeHash32(value, name string) (crypto.Hash32, error) {
	var out crypto.Hash32
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != len(out) {
		return out, fmt.Errorf("%s must be 64 hex characters", name)
	}
	copy(out[:], raw)
	return out, nil
}

func metadataKey(prefix []byte, hash crypto.Hash32) []byte {
	key := make([]byte, 0, len(prefix)+len(hash))
	key = append(key, prefix...)
	return append(key, hash[:]...)
}

func (s *Server) requireAVMStore(w http.ResponseWriter) avm.Store {
	if atomic.LoadInt32(&s.syncing) != 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "node startup synchronization is still in progress")
		return nil
	}
	if s.avmStore == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "AVM state backend is not configured")
	}
	return s.avmStore
}

func (s *Server) restAVMContract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	store := s.requireAVMStore(w)
	if store == nil {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/avm/contracts/")
	parts := strings.Split(tail, "/")
	if len(parts) < 2 {
		writeJSONError(w, http.StatusBadRequest, "expected /contracts/{contract_id}/code or /state/{key_hex}")
		return
	}
	id, err := decodeHash32(parts[0], "contract_id")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var key []byte
	switch parts[1] {
	case "code":
		if len(parts) != 2 {
			writeJSONError(w, http.StatusBadRequest, "invalid code lookup path")
			return
		}
		key = metadataKey(avmCodePrefix, id)
	case "state":
		if len(parts) != 3 {
			writeJSONError(w, http.StatusBadRequest, "state key hex is required")
			return
		}
		stateKey, decodeErr := hex.DecodeString(parts[2])
		if decodeErr != nil || len(stateKey) == 0 || len(stateKey) > core.AVMMaxStateKeySize {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("state key must be hex encoding of 1..%d bytes", core.AVMMaxStateKeySize))
			return
		}
		key = append(append(append([]byte{}, id[:]...), '/'), stateKey...)
	default:
		writeJSONError(w, http.StatusNotFound, "unknown AVM contract resource")
		return
	}
	value, found, getErr := store.Get(key)
	if getErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "AVM state read failed: "+getErr.Error())
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "AVM value not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"contract_id": parts[0],
		"value_hex":   hex.EncodeToString(value),
		"size":        len(value),
	})
}

func (s *Server) restAVMSigner(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	store := s.requireAVMStore(w)
	if store == nil {
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/v1/avm/signers/")
	parts := strings.Split(tail, "/")
	if len(parts) != 2 || parts[1] != "nonce" {
		writeJSONError(w, http.StatusBadRequest, "expected /signers/{public_key}/nonce")
		return
	}
	signer, err := decodeHash32(parts[0], "signer")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, found, getErr := store.Get(metadataKey(avmNoncePrefix, signer))
	if getErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "AVM nonce read failed: "+getErr.Error())
		return
	}
	nonce := uint64(0)
	if found {
		if len(value) != 8 {
			writeJSONError(w, http.StatusInternalServerError, "stored AVM nonce is corrupt")
			return
		}
		nonce = binary.LittleEndian.Uint64(value)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"signer": parts[0], "nonce": nonce})
}

func (s *Server) restAVMReceipt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "GET only")
		return
	}
	store := s.requireAVMStore(w)
	if store == nil {
		return
	}
	hashHex := strings.TrimPrefix(r.URL.Path, "/api/v1/avm/receipts/")
	hash, err := decodeHash32(hashHex, "transaction hash")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	value, found, getErr := store.Get(metadataKey(avmReceiptPrefix, hash))
	if getErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "AVM receipt read failed: "+getErr.Error())
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "AVM receipt not found")
		return
	}
	receipt, decodeErr := decodeAVMReceipt(value)
	if decodeErr != nil {
		writeJSONError(w, http.StatusInternalServerError, "stored AVM receipt is corrupt: "+decodeErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func decodeAVMReceipt(value []byte) (map[string]interface{}, error) {
	const fixed = 32 + 32 + 8 + 8 + 4
	if len(value) < fixed {
		return nil, io.ErrUnexpectedEOF
	}
	returnLen := binary.LittleEndian.Uint32(value[80:84])
	if uint64(fixed)+uint64(returnLen) != uint64(len(value)) {
		return nil, fmt.Errorf("invalid return-data length")
	}
	return map[string]interface{}{
		"tx_hash":         hex.EncodeToString(value[:32]),
		"contract_id":     hex.EncodeToString(value[32:64]),
		"gas_used":        binary.LittleEndian.Uint64(value[64:72]),
		"state_writes":    binary.LittleEndian.Uint64(value[72:80]),
		"return_data_hex": hex.EncodeToString(value[84:]),
	}, nil
}

func parseAVMExecution(req avmExecutionJSON) (crypto.Hash32, []byte, []byte, []avm.Access, error) {
	id, err := decodeHash32(req.ContractID, "contract_id")
	if err != nil {
		return id, nil, nil, nil, err
	}
	code, err := hex.DecodeString(req.CodeHex)
	if err != nil {
		return id, nil, nil, nil, fmt.Errorf("code_hex is not valid hex")
	}
	input, err := hex.DecodeString(req.CalldataHex)
	if err != nil {
		return id, nil, nil, nil, fmt.Errorf("calldata_hex is not valid hex")
	}
	access := make([]avm.Access, len(req.AccessList))
	var previous []byte
	for i, item := range req.AccessList {
		key, keyErr := hex.DecodeString(item.KeyHex)
		if keyErr != nil || len(key) == 0 || len(key) > core.AVMMaxStateKeySize {
			return id, nil, nil, nil, fmt.Errorf("access_list[%d].key_hex is invalid", i)
		}
		if i > 0 && bytes.Compare(previous, key) >= 0 {
			return id, nil, nil, nil, fmt.Errorf("access_list must be strictly sorted and unique")
		}
		previous = key
		access[i] = avm.Access{Key: key, Write: item.Write}
	}
	return id, code, input, access, nil
}

func (s *Server) executeAVMReadOnly(w http.ResponseWriter, r *http.Request, simulation bool) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	store := s.requireAVMStore(w)
	if store == nil {
		return
	}
	var req avmExecutionJSON
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, core.AVMMaxCodeSize+core.AVMMaxCalldataSize+64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	id, code, input, access, err := parseAVMExecution(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	action := req.Action
	if action == "" {
		action = "call"
	}
	if !simulation || action == "call" {
		if len(code) != 0 {
			writeJSONError(w, http.StatusBadRequest, "code_hex is only allowed for deploy simulation")
			return
		}
		code, _, err = store.Get(metadataKey(avmCodePrefix, id))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "AVM code read failed: "+err.Error())
			return
		}
		if len(code) == 0 {
			writeJSONError(w, http.StatusNotFound, "AVM contract not found")
			return
		}
	} else if action != "deploy" {
		writeJSONError(w, http.StatusBadRequest, "action must be call or deploy")
		return
	} else if len(code) == 0 {
		writeJSONError(w, http.StatusBadRequest, "deploy simulation requires code_hex")
		return
	}
	executionStore := store
	if simulation {
		// Simulations may exercise state-writing paths, but all effects land in
		// a throw-away overlay and can never mutate canonical state.
		executionStore = avm.NewOverlayStore(store)
	}
	engine := avm.NewEngine(executionStore)
	defer engine.Close(context.Background())
	result, executeErr := engine.Execute(r.Context(), avm.ExecutionRequest{
		ContractID: [32]byte(id), Code: code, Entry: req.Entry, Input: input,
		GasLimit: req.GasLimit, ReadOnly: !simulation, AccessList: access,
	})
	if executeErr != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(executeErr, context.Canceled) {
			status = http.StatusRequestTimeout
		}
		writeJSONError(w, status, executeErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"contract_id": req.ContractID, "gas_used": result.GasUsed,
		"return_data_hex": hex.EncodeToString(result.ReturnData),
		"state_writes":    result.StateWrites,
		"read_only":       !simulation,
		"simulation":      simulation,
		"committed":       false,
	})
}

func (s *Server) restAVMQuery(w http.ResponseWriter, r *http.Request) {
	s.executeAVMReadOnly(w, r, false)
}

func (s *Server) restAVMSimulate(w http.ResponseWriter, r *http.Request) {
	s.executeAVMReadOnly(w, r, true)
}

func (s *Server) restAVMTransaction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	if atomic.LoadInt32(&s.syncing) != 0 {
		writeJSONError(w, http.StatusServiceUnavailable, "node startup synchronization is still in progress")
		return
	}
	var req struct {
		Tx core.Transaction `json:"tx"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid signed transaction JSON: "+err.Error())
		return
	}
	if req.Tx.Version != core.TxVersionAVM || req.Tx.AVM == nil {
		writeJSONError(w, http.StatusBadRequest, "only an already locally signed v6 AVM transaction is accepted")
		return
	}
	s.broadcastTxMu.RLock()
	broadcast := s.broadcastTxFn
	s.broadcastTxMu.RUnlock()
	if broadcast == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "P2P transaction broadcaster is not ready")
		return
	}
	if err := s.mempool.Add(req.Tx); err != nil {
		writeJSONError(w, http.StatusUnprocessableEntity, "mempool rejected transaction: "+err.Error())
		return
	}
	tx := req.Tx
	s.hub.BroadcastTx(&tx)
	broadcast(&tx)
	hash := tx.Hash()
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"tx_hash": hex.EncodeToString(hash[:]), "status": "pending",
		"version": uint8(tx.Version),
	})
}
