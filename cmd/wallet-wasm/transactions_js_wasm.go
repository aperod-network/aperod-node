//go:build js && wasm

package main

// Browser-only APRO transaction preparation and signing.  The mnemonic is
// accepted only by these WASM entry points and is never included in a result.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"syscall/js"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

type wasmOutput struct {
	TxHash       string `json:"tx_hash"`
	OutIdx       uint32 `json:"out_idx"`
	OneTimePub   string `json:"one_time_pub"`
	TxPubKey     string `json:"tx_pub_key"`
	AmountCommit string `json:"amount_commit"`
	EncAmount    string `json:"enc_amount"`
	BlockHeight  uint64 `json:"block_height"`
	Amount       uint64 `json:"amount_napr,omitempty"`
	BlindHex     string `json:"blind_hex,omitempty"`
}
type wasmDecoy struct {
	OneTimePub   string `json:"one_time_pub"`
	AmountCommit string `json:"amount_commit"`
}
type wasmRequest struct {
	Mnemonic   string       `json:"mnemonic"`
	Account    uint32       `json:"account"`
	Index      uint32       `json:"index"`
	Amount     uint64       `json:"amount_napr"`
	ToAddress  string       `json:"to_address"`
	FeePerByte uint64       `json:"fee_per_byte"`
	Outputs    []wasmOutput `json:"outputs"`
	Decoys     []wasmDecoy  `json:"decoys"`
}

func wasmJSON(v js.Value, out any) error {
	if v.Type() != js.TypeObject {
		return fmt.Errorf("request must be an object")
	}
	return json.Unmarshal([]byte(js.Global().Get("JSON").Call("stringify", v).String()), out)
}
func hex32(s, field string) ([32]byte, error) {
	var v [32]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return v, fmt.Errorf("%s must be 32-byte hex", field)
	}
	copy(v[:], b)
	return v, nil
}
func derivedKeys(r wasmRequest) (*wallet.DerivedKeys, error) {
	if r.Mnemonic == "" {
		return nil, fmt.Errorf("mnemonic is required")
	}
	return wallet.DeriveFromMnemonic(r.Mnemonic, "", r.Account, r.Index)
}
func ownedOutputs(r wasmRequest, keys *wallet.DerivedKeys) ([]core.OwnedUTXO, error) {
	owned := make([]core.OwnedUTXO, 0, len(r.Outputs))
	for i, x := range r.Outputs {
		h, err := hex32(x.TxHash, "outputs.tx_hash")
		if err != nil {
			return nil, err
		}
		op, err := hex32(x.OneTimePub, "outputs.one_time_pub")
		if err != nil {
			return nil, err
		}
		tp, err := hex32(x.TxPubKey, "outputs.tx_pub_key")
		if err != nil {
			return nil, err
		}
		ac, err := hex32(x.AmountCommit, "outputs.amount_commit")
		if err != nil {
			return nil, err
		}
		encBytes, err := hex.DecodeString(x.EncAmount)
		if err != nil || len(encBytes) != 8 {
			return nil, fmt.Errorf("outputs[%d].enc_amount must be 8-byte hex", i)
		}
		var enc [8]byte
		copy(enc[:], encBytes)
		var hs crypto.Scalar32
		var amount uint64
		var blind crypto.BlindFactor
		var zero crypto.Point32
		if crypto.Point32(tp) == zero {
			heightPub, hErr := crypto.ScalarMulBase(crypto.ScalarFromUint64(x.BlockHeight))
			expected, pErr := crypto.AddPoints(keys.Keys.Spend.Public, heightPub)
			if hErr != nil || pErr != nil || crypto.Point32(op) != expected || x.Amount == 0 {
				continue
			}
			amount, hs = x.Amount, crypto.ScalarFromUint64(x.BlockHeight)
			if x.BlockHeight > 0 {
				blind, err = crypto.DeterministicMintBlindV2(keys.Keys.Spend.Public, amount, x.BlockHeight)
			} else {
				blind, err = crypto.DeterministicMintBlind(keys.Keys.Spend.Public, amount)
			}
		} else {
			found, scanErr := crypto.ScanForOutput(keys.Keys.View.Private, keys.Keys.Spend.Public, crypto.Point32(tp), crypto.Point32(op))
			if scanErr != nil || found == nil {
				continue
			}
			hs, amount = *found, core.DecryptAmount(enc, found)
			blind, err = crypto.DeterministicPaymentBlind(hs, amount)
		}
		if x.BlindHex != "" {
			storedBlind, blindErr := hex32(x.BlindHex, "outputs.blind_hex")
			if blindErr != nil {
				return nil, blindErr
			}
			blind, err = crypto.BlindFactor(storedBlind), nil
		}
		if err != nil {
			return nil, fmt.Errorf("outputs[%d] blind: %w", i, err)
		}
		commit, err := crypto.Commit(amount, blind)
		if err != nil || commit != crypto.Commitment(ac) {
			return nil, fmt.Errorf("outputs[%d] commitment cannot be opened locally", i)
		}
		owned = append(owned, core.OwnedUTXO{UTXO: core.UTXO{
			TxHash: crypto.Hash32(h), OutputIndex: x.OutIdx, OneTimePub: crypto.Point32(op),
			TxPubKey: crypto.Point32(tp), AmountCommit: crypto.Commitment(ac), EncAmount: enc, BlockHeight: x.BlockHeight,
		}, HsScalar: hs, Amount: amount, Blind: blind})
	}
	return owned, nil
}
func publicDecoys(values []wasmDecoy) ([]core.DecoyUTXO, error) {
	out := make([]core.DecoyUTXO, 0, len(values))
	for _, d := range values {
		p, err := hex32(d.OneTimePub, "decoys.one_time_pub")
		if err != nil {
			return nil, err
		}
		c, err := hex32(d.AmountCommit, "decoys.amount_commit")
		if err != nil {
			return nil, err
		}
		out = append(out, core.DecoyUTXO{OneTimePub: crypto.Point32(p), AmountCommit: crypto.Commitment(c)})
	}
	return out, nil
}
func wasmValue(v any) js.Value {
	raw, _ := json.Marshal(v)
	return js.Global().Get("JSON").Call("parse", string(raw))
}
func parseRequest(args []js.Value) (wasmRequest, error) {
	if len(args) != 1 {
		return wasmRequest{}, fmt.Errorf("exactly one request object is required")
	}
	var r wasmRequest
	if err := wasmJSON(args[0], &r); err != nil {
		return r, err
	}
	return r, nil
}
func localBuilder(r wasmRequest) (*core.TxBuilder, *wallet.DerivedKeys, error) {
	keys, err := derivedKeys(r)
	if err != nil {
		return nil, nil, err
	}
	owned, err := ownedOutputs(r, keys)
	if err != nil {
		return nil, nil, err
	}
	decoys, err := publicDecoys(r.Decoys)
	if err != nil {
		return nil, nil, err
	}
	return core.NewTxBuilder(keys.Keys.Spend.Private, keys.Keys.View.Private, keys.Keys.Spend.Public, owned, r.FeePerByte).
		WithVersion(core.TxVersionCLSAG).
		WithDecoys(decoys), keys, nil
}
func scanOutputs(_ js.Value, args []js.Value) any {
	r, err := parseRequest(args)
	if err != nil {
		return promiseResult(nil, err)
	}
	keys, err := derivedKeys(r)
	if err != nil {
		return promiseResult(nil, err)
	}
	outs, err := ownedOutputs(r, keys)
	if err != nil {
		return promiseResult(nil, err)
	}
	type scanned struct {
		TxHash string `json:"tx_hash"`
		OutIdx uint32 `json:"out_idx"`
		Amount uint64 `json:"amount_napr"`
		Blind  string `json:"blind_hex"`
	}
	result := make([]scanned, len(outs))
	for i, u := range outs {
		result[i] = scanned{hex.EncodeToString(u.TxHash[:]), u.OutputIndex, u.Amount, hex.EncodeToString(u.Blind[:])}
	}
	return promiseResult(wasmValue(map[string]any{"outputs": result}), nil)
}
func estimateLocalFee(_ js.Value, args []js.Value) any {
	r, err := parseRequest(args)
	if err != nil {
		return promiseResult(nil, err)
	}
	if r.Amount == 0 {
		return promiseResult(nil, fmt.Errorf("amount_napr must be > 0"))
	}
	b, _, err := localBuilder(r)
	if err != nil {
		return promiseResult(nil, err)
	}
	q := b.EstimateFeeForAmount(r.Amount)
	return promiseResult(wasmValue(map[string]any{
		"fee_napr": q.Fee, "input_count": q.InputCount, "tx_size_bytes": q.TxSizeBytes,
		"sufficient": q.Sufficient, "spendable_total_napr": q.SpendableTotal, "utxo_count": q.UTXOCount,
	}), nil)
}
func maxSpendable(_ js.Value, args []js.Value) any {
	r, err := parseRequest(args)
	if err != nil {
		return promiseResult(nil, err)
	}
	b, _, err := localBuilder(r)
	if err != nil {
		return promiseResult(nil, err)
	}
	q := b.MaxSpendable()
	return promiseResult(wasmValue(map[string]any{
		"max_amount_napr": q.MaxAmount, "fee_napr": q.Fee, "input_count": q.InputCount,
		"spendable_total_napr": q.SpendableTotal, "utxo_count": q.UTXOCount,
	}), nil)
}
func buildSignedTransaction(_ js.Value, args []js.Value) any {
	r, err := parseRequest(args)
	if err != nil {
		return promiseResult(nil, err)
	}
	if r.Amount == 0 || r.ToAddress == "" {
		return promiseResult(nil, fmt.Errorf("amount_napr and to_address are required"))
	}
	b, keys, err := localBuilder(r)
	if err != nil {
		return promiseResult(nil, err)
	}
	change := crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys)
	result, err := b.Build(r.Amount, crypto.Address(r.ToAddress), change)
	if err != nil {
		return promiseResult(nil, err)
	}
	hash := result.Tx.Hash()
	spent := make([]map[string]any, len(result.SelectedUTXOs))
	for i, u := range result.SelectedUTXOs {
		spent[i] = map[string]any{"tx_hash": hex.EncodeToString(u.TxHash[:]), "out_idx": u.OutputIndex, "key_image_hex": hex.EncodeToString(result.Tx.Inputs[i].KeyImage[:])}
	}
	return promiseResult(wasmValue(map[string]any{
		"tx": result.Tx, "tx_hash": hex.EncodeToString(hash[:]), "total_fee_napr": result.TotalFee,
		"change_amount_napr": strconv.FormatUint(result.ChangeAmount, 10), "change_out_idx": result.ChangeOutIdx,
		"change_blind_hex": hex.EncodeToString(result.ChangeBlind[:]), "payment_blind_hex": hex.EncodeToString(result.PayBlind[:]),
		"payment_out_idx": result.PayOutIdx, "spent_key_images": spent,
		"decoy_count": result.RealDecoyCount, "fallback_decoy_count": result.FallbackDecoyCount,
	}), nil)
}
