//go:build js && wasm

// wallet-wasm exposes only public browser-wallet operations.  It intentionally
// never serializes seeds or private keys across the JavaScript boundary.
package main

import (
	"fmt"
	"math"
	"syscall/js"

	"github.com/aperod/aperod/wallet"
)

var callbacks []js.Func

func promiseResult(value any, err error) js.Value {
	promise := js.Global().Get("Promise")
	if err != nil {
		return promise.Call("reject", err.Error())
	}
	return promise.Call("resolve", value)
}

func generateMnemonic(_ js.Value, args []js.Value) any {
	words := 12
	if len(args) > 0 && args[0].Type() != js.TypeUndefined {
		if args[0].Type() != js.TypeNumber {
			return promiseResult(nil, fmt.Errorf("word count must be 12 or 24"))
		}
		words = args[0].Int()
	}
	strength := wallet.Strength128
	if words == 24 {
		strength = wallet.Strength256
	} else if words != 12 {
		return promiseResult(nil, fmt.Errorf("word count must be 12 or 24"))
	}
	mnemonic, err := wallet.GenerateMnemonic(strength)
	if err != nil {
		return promiseResult(nil, err)
	}
	return promiseResult(map[string]any{"mnemonic": mnemonic}, nil)
}

func argumentIndex(args []js.Value, position int) (uint32, error) {
	if len(args) <= position || args[position].Type() == js.TypeUndefined {
		return 0, nil
	}
	if args[position].Type() != js.TypeNumber {
		return 0, fmt.Errorf("account and address index must be non-negative integers")
	}
	value := args[position].Float()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > math.MaxUint32 || math.Trunc(value) != value {
		return 0, fmt.Errorf("account and address index must be non-negative integers")
	}
	return uint32(value), nil
}

func deriveAddress(_ js.Value, args []js.Value) any {
	if len(args) < 1 || args[0].Type() != js.TypeString {
		return promiseResult(nil, fmt.Errorf("mnemonic is required"))
	}
	account, err := argumentIndex(args, 1)
	if err != nil {
		return promiseResult(nil, err)
	}
	index, err := argumentIndex(args, 2)
	if err != nil {
		return promiseResult(nil, err)
	}
	address, err := deriveMainnetAddress(args[0].String(), account, index)
	if err != nil {
		return promiseResult(nil, err)
	}
	return promiseResult(map[string]any{"address": address}, nil)
}

func main() {
	api := js.Global().Get("Object").New()
	callbacks = append(callbacks, js.FuncOf(generateMnemonic), js.FuncOf(deriveAddress), js.FuncOf(scanOutputs), js.FuncOf(estimateLocalFee), js.FuncOf(maxSpendable), js.FuncOf(buildSignedTransaction))
	api.Set("generateMnemonic", callbacks[0])
	api.Set("deriveAddress", callbacks[1])
	api.Set("scanOutputs", callbacks[2])
	api.Set("estimateFee", callbacks[3])
	api.Set("maxSpendable", callbacks[4])
	api.Set("buildSignedTransaction", callbacks[5])
	js.Global().Set("AperodWalletWasm", api)
	select {}
}
