package api

// apr_walletBatchSend — multi-output RingCT transaction RPC.
// Builds, signs, verifies, and submits a single transaction that pays N
// recipients atomically.  Used by the game batch-send API so one server
// call distributes rewards to many wallets with a single on-chain fee.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// ─── Types ────────────────────────────────────────────────────────────────────

type batchRecipientParam struct {
	Address    string `json:"address"`
	AmountNAPR uint64 `json:"amount_napr"`
}

type walletBatchSendParams struct {
	SpendKeyHex   string               `json:"spend_key_hex"`
	ViewKeyHex    string               `json:"view_key_hex"`
	ChangeAddress string               `json:"change_address"` // empty → derived from keys
	Recipients    []batchRecipientParam `json:"recipients"`     // max 15
	UTXOs         []walletUTXOInput    `json:"utxos"`
}

type batchPayOutput struct {
	OutIdx     int    `json:"out_idx"`
	AmountNAPR uint64 `json:"amount_napr"`
	BlindHex   string `json:"blind_hex"`
	Address    string `json:"address"`
}

type walletBatchSendResult struct {
	TxHash             string           `json:"tx_hash"`
	ChangeAmtNAPR      uint64           `json:"change_amount_napr"`
	ChangeOutIdx       int              `json:"change_out_idx"`
	ChangeBlindHex     string           `json:"change_blind_hex"`
	Outputs            []batchPayOutput `json:"outputs"`
	TotalFeeNAPR       uint64           `json:"total_fee_napr"`
	DecoyCount         int              `json:"decoy_count"`
	FallbackDecoyCount int              `json:"fallback_decoy_count"`
}

// ─── Handler ──────────────────────────────────────────────────────────────────

func (s *Server) aprWalletBatchSend(rawParams json.RawMessage) (interface{}, error) {
	var p walletBatchSendParams
	if err := json.Unmarshal(rawParams, &p); err != nil {
		return nil, fmt.Errorf("params: %w", err)
	}
	if len(p.Recipients) == 0 {
		return nil, fmt.Errorf("recipients list is empty")
	}
	if len(p.Recipients) > 15 {
		return nil, fmt.Errorf("recipients exceeds maximum of 15 per batch")
	}
	if len(p.UTXOs) == 0 {
		return nil, fmt.Errorf("utxos list is empty")
	}

	// ── 1. Decode private scalars ─────────────────────────────────────────────
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
	viewPub, err := crypto.PublicKeyFromPrivate(viewPriv)
	if err != nil {
		return nil, fmt.Errorf("derive view pub: %w", err)
	}

	// ── 2. Decode UTXOs (same path as aprWalletSend) ──────────────────────────
	ownedUTXOs := make([]core.OwnedUTXO, 0, len(p.UTXOs))
	for _, u := range p.UTXOs {
		txHash, err := hash32FromHex(u.TxHash)
		if err != nil {
			return nil, fmt.Errorf("utxo tx_hash %q: %w", u.TxHash, err)
		}

		tx, loc, txOk := s.chain.GetTransaction(txHash)
		if !txOk {
			mempoolTx, inMempool := s.mempool.Get(txHash)
			if inMempool {
				tx = mempoolTx
				loc.Block = &core.Block{}
				txOk = true
			}
		}
		if !txOk {
			// Fallback 2: tx-hash index disk lookup (newer blocks only).
			diskTx, diskLoc, diskFound, diskErr := s.getTransactionFromDisk(txHash)
			if diskErr != nil {
				s.log.Warn("disk tx-index fallback error",
					"tx", u.TxHash[:min(16, len(u.TxHash))], "err", diskErr)
			}
			if diskFound {
				tx, loc, txOk = diskTx, diskLoc, true
			}
		}

		var out core.Output
		if txOk {
			if int(u.OutIdx) >= len(tx.Outputs) {
				return nil, fmt.Errorf("out_idx %d out of range for tx %s (%d outputs)",
					u.OutIdx, u.TxHash[:min(16, len(u.TxHash))], len(tx.Outputs))
			}
			out = tx.Outputs[u.OutIdx]
		} else if s.blockStore != nil {
			// Fallback 3: UTXO store — written for every output in every block.
			su, suErr := s.blockStore.GetUTXO(txHash, uint32(u.OutIdx))
			if suErr != nil {
				return nil, fmt.Errorf("utxo store fallback for tx %s[%d]: %w",
					u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, suErr)
			}
			if su == nil {
				return nil, fmt.Errorf("tx %s not found on chain or mempool",
					u.TxHash[:min(16, len(u.TxHash))])
			}
			out = core.Output{
				OneTimePub:   su.OneTimePub,
				TxPubKey:     su.TxPubKey,
				AmountCommit: su.AmountCommit,
			}
			loc = core.TxLocation{
				Block:   &core.Block{Header: core.BlockHeader{Height: su.BlockHeight}},
				TxIndex: 0,
			}
		} else {
			return nil, fmt.Errorf("tx %s not found on chain or mempool",
				u.TxHash[:min(16, len(u.TxHash))])
		}

		// Detect transparent mint output.
		var zeroPub crypto.Point32
		var mintHeightScalar crypto.Scalar32
		var mintBlockHeight uint64
		isMintOut := false
		if out.TxPubKey == zeroPub {
			if out.OneTimePub == spendPub {
				isMintOut = true
				mintHeightScalar = crypto.ScalarFromUint64(0)
				mintBlockHeight = 0
			} else {
				h := loc.Block.Header.Height
				heightPub, hErr := crypto.ScalarMulBase(crypto.ScalarFromUint64(h))
				if hErr == nil {
					expectedMintPub, aErr := crypto.AddPoints(spendPub, heightPub)
					if aErr == nil && out.OneTimePub == expectedMintPub {
						isMintOut = true
						mintHeightScalar = crypto.ScalarFromUint64(h)
						mintBlockHeight = h
					}
				}
			}
		}

		var blind crypto.BlindFactor
		var hsScalar crypto.Scalar32
		if isMintOut {
			hsScalar = mintHeightScalar
			if u.BlindHex == "" {
				if mintBlockHeight > 0 {
					// Block-reward mint: try V2 blind (includes height), fall back to V1 for
					// UTXOs created before the F-049 migration.
					blindV2, errV2 := crypto.DeterministicMintBlindV2(spendPub, u.AmountNAPR, mintBlockHeight)
					if errV2 != nil {
						return nil, fmt.Errorf("deterministic mint blind v2 for %s[%d]: %w",
							u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, errV2)
					}
					cV2, cErrV2 := crypto.Commit(u.AmountNAPR, blindV2)
					if cErrV2 == nil && cV2 == out.AmountCommit {
						blind = blindV2
					} else {
						// Fall back to V1 for pre-migration UTXOs.
						blindV1, errV1 := crypto.DeterministicMintBlind(spendPub, u.AmountNAPR)
						if errV1 != nil {
							return nil, fmt.Errorf("deterministic mint blind v1 for %s[%d]: %w",
								u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, errV1)
						}
						blind = blindV1
					}
				} else {
					// Legacy/admin mint (height == 0): always use V1.
					blind, err = crypto.DeterministicMintBlind(spendPub, u.AmountNAPR)
					if err != nil {
						return nil, fmt.Errorf("deterministic blind for %s[%d]: %w",
							u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
					}
				}
			} else {
				blind, err = blindFactorFromHex(u.BlindHex)
				if err != nil {
					return nil, fmt.Errorf("blind_hex for %s[%d]: %w",
						u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
				}
			}
		} else {
			hs, scanErr := crypto.ScanForOutput(viewPriv, spendPub, out.TxPubKey, out.OneTimePub)
			if scanErr != nil || hs == nil {
				return nil, fmt.Errorf("output %s[%d] does not belong to wallet",
					u.TxHash[:min(16, len(u.TxHash))], u.OutIdx)
			}
			hsScalar = *hs
			if u.BlindHex == "" {
				blind, err = crypto.DeterministicPaymentBlind(hsScalar, u.AmountNAPR)
				if err != nil {
					return nil, fmt.Errorf("deterministic blind for %s[%d]: %w",
						u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
				}
				recomputed, cErr := crypto.Commit(u.AmountNAPR, blind)
				if cErr != nil || recomputed != out.AmountCommit {
					return nil, fmt.Errorf(
						"stealth output %s[%d]: commitment mismatch — UTXO predates deterministic blind migration",
						u.TxHash[:min(16, len(u.TxHash))], u.OutIdx)
				}
			} else {
				blind, err = blindFactorFromHex(u.BlindHex)
				if err != nil {
					return nil, fmt.Errorf("blind_hex for %s[%d]: %w",
						u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
				}
			}
		}

		// Pre-flight commitment check (mirrors rpc_wallet.go).
		if preCommit, preErr := crypto.Commit(u.AmountNAPR, blind); preErr != nil {
			return nil, fmt.Errorf("commit recompute for %s[%d]: %w",
				u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, preErr)
		} else if preCommit != out.AmountCommit {
			return nil, fmt.Errorf("commitment mismatch for tx %s[%d]: stored blind/amount does not reproduce on-chain commitment — re-mint required",
				u.TxHash[:min(16, len(u.TxHash))], u.OutIdx)
		}

		ownedUTXOs = append(ownedUTXOs, core.OwnedUTXO{
			UTXO: core.UTXO{
				TxHash:       txHash,
				OutputIndex:  u.OutIdx,
				OneTimePub:   out.OneTimePub,
				TxPubKey:     out.TxPubKey,
				AmountCommit: out.AmountCommit,
				EncAmount:    out.EncAmount,
				BlockHeight:  loc.Block.Header.Height,
			},
			HsScalar: hsScalar,
			Amount:   u.AmountNAPR,
			Blind:    blind,
		})
	}

	// ── 3. Determine change address ───────────────────────────────────────────
	changeAddr := crypto.Address(p.ChangeAddress)
	if p.ChangeAddress == "" {
		net := crypto.MainnetByte
		if len(p.Recipients) > 0 && len(p.Recipients[0].Address) >= 4 &&
			p.Recipients[0].Address[:4] == "tapr" {
			net = crypto.TestnetByte
		}
		changeAddr = crypto.EncodeAddress(net, spendPub, viewPub)
	}

	// ── 4. Build and sign multi-output transaction ────────────────────────────
	batchRecipients := make([]core.BatchRecipient, len(p.Recipients))
	for i, r := range p.Recipients {
		batchRecipients[i] = core.BatchRecipient{
			Address:    crypto.Address(r.Address),
			AmountNAPR: r.AmountNAPR,
		}
	}

	builder := core.NewTxBuilder(spendPriv, viewPriv, spendPub, ownedUTXOs, 0).
		WithDecoySet(s.utxos)
	result, err := builder.BuildMulti(batchRecipients, changeAddr)
	if err != nil {
		return nil, fmt.Errorf("build: %w", err)
	}

	// ── 5. Cryptographic verification ────────────────────────────────────────
	verifier := core.NewTxVerifier(s.utxos)
	if err := verifier.VerifyTx(&result.Tx); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}

	// ── 6. Submit to mempool ──────────────────────────────────────────────────
	if err := s.mempool.Add(result.Tx); err != nil {
		return nil, fmt.Errorf("mempool: %w", err)
	}

	// ── 7. Build response ─────────────────────────────────────────────────────
	txHash := result.Tx.Hash()
	changeBlindHex := ""
	if result.ChangeAmount > 0 {
		changeBlindHex = hex.EncodeToString(result.ChangeBlind[:])
	}

	outputs := make([]batchPayOutput, len(p.Recipients))
	for i, r := range p.Recipients {
		outputs[i] = batchPayOutput{
			OutIdx:     result.PayOutIdxs[i],
			AmountNAPR: result.PayAmounts[i],
			BlindHex:   hex.EncodeToString(result.PayBlinds[i][:]),
			Address:    r.Address,
		}
	}

	return walletBatchSendResult{
		TxHash:             fmt.Sprintf("%x", txHash[:]),
		ChangeAmtNAPR:      result.ChangeAmount,
		ChangeOutIdx:       result.ChangeOutIdx,
		ChangeBlindHex:     changeBlindHex,
		Outputs:            outputs,
		TotalFeeNAPR:       result.TotalFee,
		DecoyCount:         result.RealDecoyCount,
		FallbackDecoyCount: result.FallbackDecoyCount,
	}, nil
}
