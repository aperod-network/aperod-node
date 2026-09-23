// SPDX-License-Identifier: LicenseRef-Aperod-LPoD
// Copyright (c) web3 Aperod APRO team

package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

type lpodSignRequest struct {
	Mnemonic    string `json:"mnemonic"`
	Genesis     string `json:"genesis"`
	Vault       string `json:"vault"`
	Action      string `json:"action"`
	DepositJSON string `json:"deposit_action_json"`
	Nonce       string `json:"nonce"`
	Withdraw    string `json:"withdraw_napro"`
	PositionID  string `json:"position_id"`
}

func lpodHash(s string) (crypto.Hash32, error) {
	var h crypto.Hash32
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != 32 {
		return h, fmt.Errorf("invalid 32-byte hash")
	}
	copy(h[:], b)
	return h, nil
}

// Native action signing stays local. Deposits must consume exactly one selected
// source output; this builder never silently rounds or sends wallet change away.
func signLPoD(r lpodSignRequest, keys *wallet.DerivedKeys, source *core.OwnedUTXO) (*core.Transaction, error) {
	genesis, err := lpodHash(r.Genesis)
	if err != nil {
		return nil, err
	}
	if genesis == (crypto.Hash32{}) {
		return nil, fmt.Errorf("missing chain anchor")
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, keys.Keys)
	var a core.LPoDPositionAction
	var sourceKey crypto.Scalar32
	if r.Action == "deposit" {
		if source == nil || source.Amount == 0 {
			return nil, fmt.Errorf("select an exact owned source output")
		}
		v, err := lpodHash(r.Vault)
		if err != nil {
			return nil, err
		}
		a = core.LPoDPositionAction{Action: core.LPoDDeposit, Genesis: genesis,
			PositionID: core.LPoDPositionID(genesis, source.TxHash, source.OutputIndex),
			Vault:      crypto.Point32(v), Beneficiary: address, Owner: keys.Keys.Spend.Public,
			SourceTx: source.TxHash, SourceIndex: source.OutputIndex, SourcePub: source.OneTimePub,
			Amount: source.Amount, Blind: source.Blind}
		sourceKey, err = crypto.AddScalars(source.HsScalar, keys.Keys.Spend.Private)
		if err != nil {
			return nil, err
		}
		pub, err := crypto.ScalarMulBase(sourceKey)
		if err != nil || pub != source.OneTimePub {
			return nil, fmt.Errorf("source ownership mismatch")
		}
	} else if r.Action == "withdraw" {
		if len(r.DepositJSON) > 4096 || json.Unmarshal([]byte(r.DepositJSON), &a) != nil {
			return nil, fmt.Errorf("invalid canonical deposit")
		}
		id, err := lpodHash(r.PositionID)
		if err != nil {
			return nil, err
		}
		if a.Action != core.LPoDDeposit || a.Nonce != 0 || a.Genesis != genesis || a.Beneficiary != address ||
			a.Owner != keys.Keys.Spend.Public || a.PositionID != id ||
			a.PositionID != core.LPoDPositionID(genesis, a.SourceTx, a.SourceIndex) {
			return nil, fmt.Errorf("position ownership or chain mismatch")
		}
		nonce, err := strconv.ParseUint(r.Nonce, 10, 64)
		if err != nil || nonce == ^uint64(0) {
			return nil, fmt.Errorf("invalid current nonce")
		}
		amount, err := strconv.ParseUint(r.Withdraw, 10, 64)
		if err != nil || amount > a.Amount {
			return nil, fmt.Errorf("invalid withdrawal amount")
		}
		a.Action = core.LPoDWithdraw
		a.Nonce = nonce + 1
		a.WithdrawAmount = amount
	} else {
		return nil, fmt.Errorf("invalid LPoD action")
	}
	return core.BuildLPoDPositionTx(a, keys.Keys.Spend.Private, sourceKey)
}
