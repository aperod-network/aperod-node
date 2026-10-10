//go:build js && wasm

package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/aperod/aperod/crypto"
)

type identityRequestJSON struct {
	Mnemonic string            `json:"mnemonic"`
	Outputs  []json.RawMessage `json:"outputs"`
}

type identityOutputJSON struct {
	TxHash      *string `json:"tx_hash"`
	OutIdx      *uint32 `json:"out_idx"`
	OneTimePub  *string `json:"one_time_pub"`
	TxPubKey    *string `json:"tx_pub_key"`
	BlockHeight *uint64 `json:"block_height"`
}

func parseIdentityOutput(raw json.RawMessage, index int) (identityOutput, error) {
	var fields identityOutputJSON
	if err := json.Unmarshal(raw, &fields); err != nil {
		return identityOutput{}, fmt.Errorf("outputs[%d] must be an object with valid public identity fields: %w", index, err)
	}
	if fields.TxHash == nil || fields.OutIdx == nil || fields.OneTimePub == nil ||
		fields.TxPubKey == nil || fields.BlockHeight == nil {
		return identityOutput{}, fmt.Errorf("outputs[%d] requires tx_hash, out_idx, one_time_pub, tx_pub_key, and block_height", index)
	}
	txHash, err := hex32(*fields.TxHash, fmt.Sprintf("outputs[%d].tx_hash", index))
	if err != nil {
		return identityOutput{}, err
	}
	oneTimePub, err := hex32(*fields.OneTimePub, fmt.Sprintf("outputs[%d].one_time_pub", index))
	if err != nil {
		return identityOutput{}, err
	}
	txPubKey, err := hex32(*fields.TxPubKey, fmt.Sprintf("outputs[%d].tx_pub_key", index))
	if err != nil {
		return identityOutput{}, err
	}
	if _, err := crypto.PointFromBytes(oneTimePub[:]); err != nil {
		return identityOutput{}, fmt.Errorf("outputs[%d].one_time_pub: %w", index, err)
	}
	var zero crypto.Point32
	if crypto.Point32(txPubKey) != zero {
		if _, err := crypto.PointFromBytes(txPubKey[:]); err != nil {
			return identityOutput{}, fmt.Errorf("outputs[%d].tx_pub_key: %w", index, err)
		}
	}
	return identityOutput{
		TxHash:      txHash,
		OutIdx:      *fields.OutIdx,
		OneTimePub:  crypto.Point32(oneTimePub),
		TxPubKey:    crypto.Point32(txPubKey),
		BlockHeight: *fields.BlockHeight,
	}, nil
}

func identifyOutputs(_ js.Value, args []js.Value) any {
	if len(args) != 1 || args[0].Type() != js.TypeObject || args[0].IsNull() {
		return promiseResult(nil, fmt.Errorf("exactly one identity request object is required"))
	}
	rawRequest := js.Global().Get("JSON").Call("stringify", args[0]).String()
	if len(rawRequest) > 1<<20 {
		return promiseResult(nil, fmt.Errorf("identity request exceeds the 1 MiB limit"))
	}
	var request identityRequestJSON
	if err := json.Unmarshal([]byte(rawRequest), &request); err != nil {
		return promiseResult(nil, fmt.Errorf("invalid identity request: %w", err))
	}
	if request.Mnemonic == "" {
		return promiseResult(nil, fmt.Errorf("mnemonic is required"))
	}
	if request.Outputs == nil {
		return promiseResult(nil, fmt.Errorf("outputs must be an array"))
	}
	if len(request.Outputs) > maxIdentityOutputs {
		return promiseResult(nil, fmt.Errorf("outputs exceeds the %d-output identity chunk limit", maxIdentityOutputs))
	}
	records := make([]identityOutput, len(request.Outputs))
	for i, raw := range request.Outputs {
		record, err := parseIdentityOutput(raw, i)
		if err != nil {
			return promiseResult(nil, err)
		}
		records[i] = record
	}
	keys, err := derivedKeys(wasmRequest{Mnemonic: request.Mnemonic})
	if err != nil {
		return promiseResult(nil, err)
	}
	identities, err := identifyOutputRecords(keys, records)
	if err != nil {
		return promiseResult(nil, err)
	}
	type identityResult struct {
		TxHash      string `json:"tx_hash"`
		OutIdx      uint32 `json:"out_idx"`
		KeyImageHex string `json:"key_image_hex"`
	}
	outputs := make([]identityResult, len(identities))
	for i, identity := range identities {
		outputs[i] = identityResult{
			TxHash:      fmt.Sprintf("%x", identity.TxHash),
			OutIdx:      identity.OutIdx,
			KeyImageHex: fmt.Sprintf("%x", identity.KeyImage),
		}
	}
	return promiseResult(wasmValue(map[string]any{"outputs": outputs}), nil)
}
