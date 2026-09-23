// SPDX-License-Identifier: Apache-2.0
// Copyright (c) web3 Aperod APRO team

//go:build js && wasm

package main

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"syscall/js"

	"github.com/aperod/aperod/core"
)

func buildLPoDTransaction(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return promiseResult(nil, fmt.Errorf("request required"))
	}
	var r lpodSignRequest
	var wr wasmRequest
	if err := wasmJSON(args[0], &r); err != nil {
		return promiseResult(nil, err)
	}
	if err := wasmJSON(args[0], &wr); err != nil {
		return promiseResult(nil, err)
	}
	keys, err := derivedKeys(wr)
	if err != nil {
		return promiseResult(nil, err)
	}
	var source *core.OwnedUTXO
	if r.Action == "deposit" {
		if len(wr.Outputs) != 1 {
			return promiseResult(nil, fmt.Errorf("select exactly one source output"))
		}
		outs, err := ownedOutputs(wr, keys)
		if err != nil {
			return promiseResult(nil, err)
		}
		if len(outs) != 1 {
			return promiseResult(nil, fmt.Errorf("source not owned by this wallet"))
		}
		source = &outs[0]
	}
	tx, err := signLPoD(r, keys, source)
	if err != nil {
		return promiseResult(nil, err)
	}
	h := tx.Hash()
	a, err := tx.LPoDPositionAction()
	if err != nil {
		return promiseResult(nil, err)
	}
	return promiseResult(wasmValue(map[string]any{"tx": tx, "tx_hash": hex.EncodeToString(h[:]),
		"position_id": hex.EncodeToString(a.PositionID[:]), "amount_napro": strconv.FormatUint(a.Amount, 10)}), nil)
}
