//go:build js && wasm

// wallet-wasm exposes only public browser-wallet operations.  It intentionally
// never serializes seeds or private keys across the JavaScript boundary.
package main

import (
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"math"
	"syscall/js"

	"filippo.io/edwards25519"
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

// signWebAuthChallenge creates a Schnorr proof over the wallet spend key.  The
// mnemonic only crosses into WASM; JavaScript receives only the signature.
func signWebAuthChallenge(_ js.Value, args []js.Value) any {
	if len(args) < 2 || args[0].Type() != js.TypeString || args[1].Type() != js.TypeString {
		return promiseResult(nil, fmt.Errorf("mnemonic and challenge are required"))
	}
	account, err := argumentIndex(args, 2)
	if err != nil {
		return promiseResult(nil, err)
	}
	index, err := argumentIndex(args, 3)
	if err != nil {
		return promiseResult(nil, err)
	}
	dk, err := wallet.DeriveFromMnemonic(args[0].String(), "", account, index)
	if err != nil {
		return promiseResult(nil, err)
	}
	x, err := edwards25519.NewScalar().SetCanonicalBytes(dk.Keys.Spend.Private[:])
	if err != nil {
		return promiseResult(nil, err)
	}
	h := sha512.New()
	h.Write([]byte("APRO_WEB_AUTH_NONCE_V1\x00"))
	h.Write(dk.Keys.Spend.Private[:])
	h.Write([]byte(args[1].String()))
	r, _ := edwards25519.NewScalar().SetUniformBytes(h.Sum(nil))
	R := new(edwards25519.Point).ScalarBaseMult(r)
	h.Reset()
	h.Write([]byte("APRO_WEB_AUTH_CHALLENGE_V1\x00"))
	h.Write([]byte(args[1].String()))
	h.Write(dk.Keys.Spend.Public[:])
	h.Write(R.Bytes())
	c, _ := edwards25519.NewScalar().SetUniformBytes(h.Sum(nil))
	s := new(edwards25519.Scalar).MultiplyAdd(c, x, r)
	sig := append(append([]byte{}, R.Bytes()...), s.Bytes()...)
	return promiseResult(map[string]any{"signature": hex.EncodeToString(sig)}, nil)
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
	callbacks = append(callbacks, js.FuncOf(generateMnemonic), js.FuncOf(deriveAddress), js.FuncOf(signWebAuthChallenge), js.FuncOf(scanOutputs), js.FuncOf(estimateLocalFee), js.FuncOf(maxSpendable), js.FuncOf(buildSignedTransaction))
	api.Set("generateMnemonic", callbacks[0])
	api.Set("deriveAddress", callbacks[1])
	api.Set("signWebAuthChallenge", callbacks[2])
	api.Set("scanOutputs", callbacks[3])
	api.Set("estimateFee", callbacks[4])
	api.Set("maxSpendable", callbacks[5])
	api.Set("buildSignedTransaction", callbacks[6])
	callbacks = append(callbacks, js.FuncOf(buildLPoDTransaction))
	api.Set("buildLPoDTransaction", callbacks[7])
	js.Global().Set("AperodWalletWasm", api)
	select {}
}
