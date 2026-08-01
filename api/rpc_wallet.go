package api

import (
        "encoding/hex"
        "encoding/json"
        "fmt"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── apr_walletSend params / result ──────────────────────────────────────────

type walletUTXOInput struct {
        TxHash     string `json:"tx_hash"`
        OutIdx     uint32 `json:"out_idx"`
        AmountNAPR uint64 `json:"amount_napr"`
        // BlindHex is the 32-byte Pedersen blind factor as hex.
        // Leave empty for transparent mint outputs — Go recomputes DeterministicMintBlind.
        BlindHex string `json:"blind_hex"`
}

type walletSendParams struct {
        SpendKeyHex   string            `json:"spend_key_hex"`   // 32-byte spend scalar (hex)
        ViewKeyHex    string            `json:"view_key_hex"`    // 32-byte view scalar (hex)
        ToAddress     string            `json:"to_address"`      // recipient APRO address
        ChangeAddress string            `json:"change_address"`  // change address (empty = derive from keys)
        AmountNAPR    uint64            `json:"amount_napr"`     // payment amount in nAPRO
        UTXOs         []walletUTXOInput `json:"utxos"`           // caller-provided spendable UTXOs
}

type walletSendResult struct {
        TxHash          string `json:"tx_hash"`
        ChangeAmtNAPR   uint64 `json:"change_amount_napr"`
        ChangeOutIdx    int    `json:"change_out_idx"`
        ChangeBlindHex  string `json:"change_blind_hex"`
        PayBlindHex     string `json:"payment_blind_hex"`
        PayOutIdx       int    `json:"payment_out_idx"`
        PayAmtNAPR      uint64 `json:"payment_amount_napr"`
}

// aprWalletSend builds, signs, verifies, and submits a real RingCT transaction.
// The Node.js layer handles key derivation and UTXO collection; this method
// does all cryptographic work and submits the tx to the mempool.
func (s *Server) aprWalletSend(rawParams json.RawMessage) (interface{}, error) {
        var p walletSendParams
        if err := json.Unmarshal(rawParams, &p); err != nil {
                return nil, fmt.Errorf("params: %w", err)
        }
        if p.AmountNAPR == 0 {
                return nil, fmt.Errorf("amount_napr must be > 0")
        }
        if p.ToAddress == "" {
                return nil, fmt.Errorf("to_address is required")
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

        // ── 2. Derive public keys ─────────────────────────────────────────────────
        spendPub, err := crypto.PublicKeyFromPrivate(spendPriv)
        if err != nil {
                return nil, fmt.Errorf("derive spend pub: %w", err)
        }
        viewPub, err := crypto.PublicKeyFromPrivate(viewPriv)
        if err != nil {
                return nil, fmt.Errorf("derive view pub: %w", err)
        }

        // ── 3. Build OwnedUTXO slice from caller-provided list ────────────────────
        ownedUTXOs := make([]core.OwnedUTXO, 0, len(p.UTXOs))
        for _, u := range p.UTXOs {
                txHash, err := hash32FromHex(u.TxHash)
                if err != nil {
                        return nil, fmt.Errorf("utxo tx_hash %q: %w", u.TxHash, err)
                }

                tx, loc, ok := s.chain.GetTransaction(txHash)
                if !ok {
                        // Fallback: check mempool (tx submitted but not yet in a block)
                        mempoolTx, inMempool := s.mempool.Get(txHash)
                        if !inMempool {
                                return nil, fmt.Errorf("tx %s not found on chain or mempool — re-mint required after node restart", u.TxHash[:min(16, len(u.TxHash))])
                        }
                        tx = mempoolTx
                        loc.Block = &core.Block{} // block not yet assigned; height 0
                }
                if int(u.OutIdx) >= len(tx.Outputs) {
                        return nil, fmt.Errorf("out_idx %d out of range for tx %s (%d outputs)",
                                u.OutIdx, u.TxHash[:min(16, len(u.TxHash))], len(tx.Outputs))
                }
                out := tx.Outputs[u.OutIdx]

                // Detect transparent mint output: TxPubKey == zero AND OneTimePub matches
                // either the legacy literal spend_pub (all mints minted before the
                // per-height uniqueness fix — regardless of their actual block height,
                // since the old BuildMintTx never used height) or the new
                // spend_pub + height*G for the height this output was actually mined at
                // (see core.BuildMintTx). The legacy check MUST be tried first and
                // independently of height — old on-chain mints must remain spendable
                // forever, or wallet balance scanning breaks for every legacy reward.
                var zeroPub crypto.Point32
                var mintHeightScalar crypto.Scalar32
                isMintOut := false
                if out.TxPubKey == zeroPub {
                        if out.OneTimePub == spendPub {
                                isMintOut = true
                                mintHeightScalar = crypto.ScalarFromUint64(0)
                        } else {
                                h := loc.Block.Header.Height
                                heightPub, hErr := crypto.ScalarMulBase(crypto.ScalarFromUint64(h))
                                if hErr == nil {
                                        expectedMintPub, aErr := crypto.AddPoints(spendPub, heightPub)
                                        if aErr == nil && out.OneTimePub == expectedMintPub {
                                                isMintOut = true
                                                mintHeightScalar = crypto.ScalarFromUint64(h)
                                        }
                                }
                        }
                }

                var blind crypto.BlindFactor
                var hsScalar crypto.Scalar32
                if isMintOut {
                        hsScalar = mintHeightScalar
                        if u.BlindHex == "" {
                                blind, err = crypto.DeterministicMintBlind(spendPub, u.AmountNAPR)
                                if err != nil {
                                        return nil, fmt.Errorf("deterministic blind for %s[%d]: %w",
                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
                                }
                                // Diagnostic: verify recomputed commitment matches on-chain commitment.
                                // If these differ, spendPub or amountNAPR mismatches the original mint.
                                recomputedCommit, commitErr := crypto.Commit(u.AmountNAPR, blind)
                                if commitErr != nil {
                                        s.log.Error("DIAG: Commit recompute failed",
                                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                "err", commitErr)
                                } else if recomputedCommit != out.AmountCommit {
                                        s.log.Error("DIAG: MINT BLIND MISMATCH — recomputed commit != on-chain commit",
                                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                "out_idx", u.OutIdx,
                                                "amount_napr", u.AmountNAPR,
                                                "spend_pub_hex", fmt.Sprintf("%x", spendPub[:]),
                                                "on_chain_commit", fmt.Sprintf("%x", out.AmountCommit[:]),
                                                "recomputed_commit", fmt.Sprintf("%x", recomputedCommit[:]),
                                        )
                                } else {
                                        s.log.Info("DIAG: mint blind OK",
                                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                "amount_napr", u.AmountNAPR,
                                                "commit_prefix", fmt.Sprintf("%x", out.AmountCommit[:8]),
                                        )
                                }
                        } else {
                                blind, err = blindFactorFromHex(u.BlindHex)
                                if err != nil {
                                        return nil, fmt.Errorf("blind_hex for %s[%d]: %w",
                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
                                }
                        }
                } else {
                        // Stealth output: compute hs FIRST (needed for both signing and blind derivation).
                        hs, scanErr := crypto.ScanForOutput(viewPriv, spendPub, out.TxPubKey, out.OneTimePub)
                        if scanErr != nil || hs == nil {
                                return nil, fmt.Errorf("output %s[%d] does not belong to wallet (view scan failed)",
                                        u.TxHash[:min(16, len(u.TxHash))], u.OutIdx)
                        }
                        hsScalar = *hs

                        if u.BlindHex == "" {
                                // Derive deterministic blind from ECDH shared secret.
                                // Requires UTXO built after the deterministic blind migration (v2+).
                                blind, err = crypto.DeterministicPaymentBlind(hsScalar, u.AmountNAPR)
                                if err != nil {
                                        return nil, fmt.Errorf("deterministic blind for %s[%d]: %w",
                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
                                }
                                // Verify the commitment to catch pre-migration UTXOs (random blind).
                                recomputed, cErr := crypto.Commit(u.AmountNAPR, blind)
                                if cErr != nil || recomputed != out.AmountCommit {
                                        return nil, fmt.Errorf(
                                                "stealth output %s[%d]: commitment mismatch — "+
                                                        "UTXO predates deterministic blind migration, admin re-mint required",
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
                // hsScalar is set above for stealth outputs (ECDH shared secret) and for
                // mint outputs (the block height scalar); zero only for legacy/admin mints
                // where mint_pub == spend_pub directly (height=0).

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

        // ── 4. Determine change address ───────────────────────────────────────────
        changeAddr := crypto.Address(p.ChangeAddress)
        if p.ChangeAddress == "" {
                // Derive from private keys; match network of recipient
                net := crypto.MainnetByte
                if len(p.ToAddress) >= 4 && p.ToAddress[:4] == "tapr" {
                        net = crypto.TestnetByte
                }
                changeAddr = crypto.EncodeAddress(net, spendPub, viewPub)
        }

        // ── 5. Build and sign the transaction ────────────────────────────────────
        builder := core.NewTxBuilder(spendPriv, viewPriv, spendPub, ownedUTXOs, 0).
                WithDecoySet(s.utxos) // Phase 2: sample real chain UTXOs as ring decoys
        result, err := builder.Build(p.AmountNAPR, crypto.Address(p.ToAddress), changeAddr)
        if err != nil {
                return nil, fmt.Errorf("build: %w", err)
        }

        // ── 6. Cryptographic verification ────────────────────────────────────────
        // Phase 2: all ring members are real on-chain UTXOs — enable strict C-0 check.
        verifier := core.NewTxVerifier(s.utxos)
        if err := verifier.VerifyTx(&result.Tx); err != nil {
                return nil, fmt.Errorf("verify: %w", err)
        }

        // ── 7. Submit to mempool ──────────────────────────────────────────────────
        if err := s.mempool.Add(result.Tx); err != nil {
                return nil, fmt.Errorf("mempool: %w", err)
        }

        // ── 8. Return tx hash + change + payment metadata ────────────────────────
        txHash := result.Tx.Hash()
        changeBlindHex := ""
        if result.ChangeAmount > 0 {
                changeBlindHex = hex.EncodeToString(result.ChangeBlind[:])
        }

        return walletSendResult{
                TxHash:         fmt.Sprintf("%x", txHash[:]),
                ChangeAmtNAPR:  result.ChangeAmount,
                ChangeOutIdx:   result.ChangeOutIdx,
                ChangeBlindHex: changeBlindHex,
                PayBlindHex:    hex.EncodeToString(result.PayBlind[:]),
                PayOutIdx:      result.PayOutIdx,
                PayAmtNAPR:     p.AmountNAPR,
        }, nil
}

// ─── hex decode helpers ───────────────────────────────────────────────────────

func scalar32FromHex(s, name string) (crypto.Scalar32, error) {
        b, err := hex.DecodeString(s)
        if err != nil {
                return crypto.Scalar32{}, fmt.Errorf("%s: invalid hex: %w", name, err)
        }
        if len(b) != 32 {
                return crypto.Scalar32{}, fmt.Errorf("%s: expected 32 bytes, got %d", name, len(b))
        }
        var out crypto.Scalar32
        copy(out[:], b)
        return out, nil
}

func blindFactorFromHex(s string) (crypto.BlindFactor, error) {
        b, err := hex.DecodeString(s)
        if err != nil {
                return crypto.BlindFactor{}, fmt.Errorf("invalid hex: %w", err)
        }
        if len(b) != 32 {
                return crypto.BlindFactor{}, fmt.Errorf("expected 32 bytes, got %d", len(b))
        }
        var out crypto.BlindFactor
        copy(out[:], b)
        return out, nil
}

func hash32FromHex(s string) (crypto.Hash32, error) {
        b, err := hex.DecodeString(s)
        if err != nil {
                return crypto.Hash32{}, fmt.Errorf("invalid hex: %w", err)
        }
        if len(b) != 32 {
                return crypto.Hash32{}, fmt.Errorf("expected 32 bytes, got %d", len(b))
        }
        var out crypto.Hash32
        copy(out[:], b)
        return out, nil
}

func min(a, b int) int {
        if a < b {
                return a
        }
        return b
}
