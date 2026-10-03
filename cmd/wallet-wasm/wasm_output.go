package main

import "encoding/json"

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

// UnmarshalJSON keeps uint64 mint values exact when JavaScript supplies them
// as decimal strings, while preserving safe integer number fixtures.
func (o *wasmOutput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	amountRaw, hasAmount := fields["amount_napr"]
	delete(fields, "amount_napr")
	withoutAmount, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	type wasmOutputWithoutAmount wasmOutput
	var parsed wasmOutputWithoutAmount
	if err := json.Unmarshal(withoutAmount, &parsed); err != nil {
		return err
	}
	*o = wasmOutput(parsed)
	if hasAmount {
		amount, err := parsePublicAmount(amountRaw)
		if err != nil {
			return err
		}
		o.Amount = amount
	}
	return nil
}
