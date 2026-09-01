package api

// apr_walletMaxSpendable — computes the exact maximum sendable amount for a
// wallet, using the same UTXO resolution as apr_walletSend and the same coin
// selection as core.TxBuilder.Build (dedup by OneTimePub, largest-first,
// ≤8 inputs, fee = size × feePerByte).
//
// This exists so "send max" in wallets never fails with insufficient funds:
// the DB-tracked balance can diverge from the truly spendable amount when a
// DB record holds a wrong amount (commitment mismatch) or when several UTXOs
// share one key image (height-0 mints to the same address) — only the largest
// of those can ever be spent, but a naive balance sums them all.

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

type walletMaxSpendableParams struct {
	SpendKeyHex string            `json:"spend_key_hex"` // 32-byte spend scalar (hex)
	ViewKeyHex  string            `json:"view_key_hex"`  // 32-byte view scalar (hex)
	UTXOs       []walletUTXOInput `json:"utxos"`         // caller-provided candidate UTXOs
}

// skippedUTXOInfo describes a candidate UTXO that could not be resolved or
// failed the pre-flight commitment check and was therefore excluded from the
// spendable set (instead of failing the whole quote, as apr_walletSend does).
type skippedUTXOInfo struct {
	TxHash string `json:"tx_hash"`
	OutIdx uint32 `json:"out_idx"`
	Reason string `json:"reason"`
}

type walletMaxSpendableResult struct {
	// MaxAmountNAPR is the largest amount (nAPRO) a single apr_walletSend
	// call can deliver to a recipient right now.  Zero when nothing is
	// spendable.
	MaxAmountNAPR uint64 `json:"max_amount_napr"`
	// FeeNAPR is the exact fee TxBuilder will charge for that amount.
	FeeNAPR uint64 `json:"fee_napr"`
	// InputCount is the number of inputs the builder will select.
	InputCount int `json:"input_count"`
	// SpendableTotalNAPR is the sum of deduplicated resolvable UTXO amounts
	// (the ceiling before fee and the 8-input cap).
	SpendableTotalNAPR uint64 `json:"spendable_total_napr"`
	// SpendableUTXOCount is the number of UTXOs after OneTimePub dedup.
	SpendableUTXOCount int `json:"spendable_utxo_count"`
	// SkippedUTXOs lists candidates excluded from the calculation.
	SkippedUTXOs []skippedUTXOInfo `json:"skipped_utxos,omitempty"`
}

// aprWalletMaxSpendable resolves the caller's candidate UTXOs exactly like
// apr_walletSend, but skips unresolvable/mismatching entries instead of
// failing, then runs TxBuilder.MaxSpendable over the healthy set.
func (s *Server) aprWalletMaxSpendable(rawParams json.RawMessage) (interface{}, error) {
	var p walletMaxSpendableParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if len(p.UTXOs) == 0 {
		return walletMaxSpendableResult{}, nil
	}

	spendPriv, err := scalar32FromHex(p.SpendKeyHex, "spend_key_hex")
	if err != nil {
		return nil, err
	}
	viewPriv, err := scalar32FromHex(p.ViewKeyHex, "view_key_hex")
	if err != nil {
		return nil, err
	}
	spendPub, err := crypto.PublicKeyFromPrivate(spendPriv)
	if err != nil {
		return nil, fmt.Errorf("derive spend pub: %w", err)
	}

	owned := make([]core.OwnedUTXO, 0, len(p.UTXOs))
	var skipped []skippedUTXOInfo
	for _, u := range p.UTXOs {
		o, resErr := s.resolveOwnedUTXO(u, viewPriv, spendPub)
		if resErr != nil {
			// A bad DB record (wrong amount / stale blind / deleted UTXO)
			// must not block the quote — the send path skips such UTXOs
			// too (stale-retry loop), so the spendable amount excludes them.
			skipped = append(skipped, skippedUTXOInfo{
				TxHash: u.TxHash,
				OutIdx: u.OutIdx,
				Reason: resErr.Error(),
			})
			continue
		}
		owned = append(owned, o)
	}

	builder := core.NewTxBuilder(spendPriv, viewPriv, spendPub, owned, 0).
		WithVersion(s.mempool.NextSpendVersion())
	ms := builder.MaxSpendable()

	return walletMaxSpendableResult{
		MaxAmountNAPR:      ms.MaxAmount,
		FeeNAPR:            ms.Fee,
		InputCount:         ms.InputCount,
		SpendableTotalNAPR: ms.SpendableTotal,
		SpendableUTXOCount: ms.UTXOCount,
		SkippedUTXOs:       skipped,
	}, nil
}
