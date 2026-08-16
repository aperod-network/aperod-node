package api

// apr_walletEstimateFee — amount-aware fee dry-run.  Resolves the caller's
// candidate UTXOs exactly like apr_walletSend (same decoding, on-chain
// resolution, blind validation, and pre-flight commitment check via
// resolveOwnedUTXO), then replays core.TxBuilder.Build's coin selection
// (dedup by OneTimePub, largest-first, ≤8 inputs, fee = size × feePerByte)
// for the requested amount — without building or broadcasting anything.
//
// This exists so wallets can show the SAME fee the node will actually charge
// before the user confirms: the fee grows with the number of inputs, and a
// fixed 1-input estimate under-shows it whenever the balance is fragmented.

import (
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

type walletEstimateFeeParams struct {
	SpendKeyHex string            `json:"spend_key_hex"` // 32-byte spend scalar (hex)
	ViewKeyHex  string            `json:"view_key_hex"`  // 32-byte view scalar (hex)
	AmountNAPR  uint64            `json:"amount_napr"`   // payment amount in nAPRO
	UTXOs       []walletUTXOInput `json:"utxos"`         // caller-provided candidate UTXOs
}

type walletEstimateFeeResult struct {
	// FeeNAPR is the exact fee TxBuilder.Build will charge for this amount.
	FeeNAPR uint64 `json:"fee_napr"`
	// InputCount is the number of inputs Build will select.
	InputCount int `json:"input_count"`
	// TxSizeBytes is the estimated serialized transaction size.
	TxSizeBytes int `json:"tx_size_bytes"`
	// Sufficient reports whether the send would succeed on this UTXO set.
	Sufficient bool `json:"sufficient"`
	// SpendableTotalNAPR is the sum of deduplicated resolvable UTXO amounts.
	SpendableTotalNAPR uint64 `json:"spendable_total_napr"`
	// SpendableUTXOCount is the number of UTXOs after OneTimePub dedup.
	SpendableUTXOCount int `json:"spendable_utxo_count"`
	// BaseFeePerByte is the fee rate used for the estimate.
	BaseFeePerByte uint64 `json:"base_fee_per_byte"`
	// SkippedUTXOs lists candidates excluded from the calculation
	// (unresolvable / failed the pre-flight commitment check).
	SkippedUTXOs []skippedUTXOInfo `json:"skipped_utxos,omitempty"`
}

func (s *Server) aprWalletEstimateFee(rawParams json.RawMessage) (interface{}, error) {
	var p walletEstimateFeeParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if p.AmountNAPR == 0 {
		return nil, fmt.Errorf("amount_napr must be > 0")
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

	// Resolve candidates exactly like apr_walletMaxSpendable: skip bad DB
	// records instead of failing (the send path's stale-retry loop skips
	// them too), so the quote reflects the healthy set the send will use.
	owned := make([]core.OwnedUTXO, 0, len(p.UTXOs))
	var skipped []skippedUTXOInfo
	for _, u := range p.UTXOs {
		o, resErr := s.resolveOwnedUTXO(u, viewPriv, spendPub)
		if resErr != nil {
			skipped = append(skipped, skippedUTXOInfo{
				TxHash: u.TxHash,
				OutIdx: u.OutIdx,
				Reason: resErr.Error(),
			})
			continue
		}
		owned = append(owned, o)
	}

	// Rate 0 → NewTxBuilder falls back to core.InitialBaseFeePerByte —
	// exactly what apr_walletSend does, so the quote uses the same rate.
	builder := core.NewTxBuilder(spendPriv, viewPriv, spendPub, owned, 0)
	est := builder.EstimateFeeForAmount(p.AmountNAPR)

	return walletEstimateFeeResult{
		FeeNAPR:            est.Fee,
		InputCount:         est.InputCount,
		TxSizeBytes:        est.TxSizeBytes,
		Sufficient:         est.Sufficient,
		SpendableTotalNAPR: est.SpendableTotal,
		SpendableUTXOCount: est.UTXOCount,
		BaseFeePerByte:     core.InitialBaseFeePerByte,
		SkippedUTXOs:       skipped,
	}, nil
}
