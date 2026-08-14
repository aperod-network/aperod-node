package api

import (
        "context"
        "encoding/hex"
        "encoding/json"
        "fmt"
        "strings"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
        "github.com/aperod/aperod/store"
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

// spentInputKI identifies the UTXO that was consumed as the real ring input
// and carries its key image so the API layer can persist it in utxo_blinds.
// This enables the Monero-style spent check on the UTXO supply page:
// if the key image appears in the node's ki/ index the UTXO is definitively spent.
type spentInputKI struct {
        TxHash      string `json:"tx_hash"`
        OutputIndex uint32 `json:"output_index"`
        KeyImageHex string `json:"key_image_hex"`
}

type walletSendResult struct {
        TxHash             string `json:"tx_hash"`
        // TotalFeeNAPR is the exact fee charged by the builder (size × base fee),
        // so wallets can display the real fee instead of a pre-send estimate.
        TotalFeeNAPR       uint64 `json:"total_fee_napr"`
        ChangeAmtNAPR      uint64 `json:"change_amount_napr"`
        ChangeOutIdx       int    `json:"change_out_idx"`
        ChangeBlindHex     string `json:"change_blind_hex"`
        PayBlindHex        string `json:"payment_blind_hex"`
        PayOutIdx          int    `json:"payment_out_idx"`
        PayAmtNAPR         uint64 `json:"payment_amount_napr"`
        // DecoyCount is the number of ring slots filled with real on-chain decoy
        // UTXOs.  A value below (RingSize-1)×InputCount means some slots used
        // randomly-generated Phase 1 fallback keys, which degrades privacy.
        DecoyCount         int    `json:"decoy_count"`
        // FallbackDecoyCount is the number of ring slots that could not be filled
        // with real decoys and fell back to randomly-generated Phase 1 keys.
        // Zero means full Phase 2 privacy.  Non-zero means the ring contains
        // provably fake members that break anonymity.
        FallbackDecoyCount int           `json:"fallback_decoy_count"`
        // SpentKeyImages carries the key image of each real ring input so the
        // caller can record it in utxo_blinds.key_image_hex.  Checking that value
        // against GET /api/v1/keyimage/{ki}/is-spent gives an authoritative
        // Monero-style spent check without needing the wallet's private key.
        SpentKeyImages     []spentInputKI `json:"spent_key_images,omitempty"`
}

// aprWalletSend builds, signs, verifies, and submits a real RingCT transaction.
// The Node.js layer handles key derivation and UTXO collection; this method
// does all cryptographic work and submits the tx to the mempool.
//
// ctx is the HTTP request context.  If the client disconnects before the
// mempool submission the function returns an error without submitting the tx,
// preventing the change-blind from being orphaned.
func (s *Server) aprWalletSend(ctx context.Context, rawParams json.RawMessage) (interface{}, error) {
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

                tx, loc, txOk := s.chain.GetTransaction(txHash)
                if !txOk {
                        // Fallback 1: check mempool (tx submitted but not yet in a block)
                        mempoolTx, inMempool := s.mempool.Get(txHash)
                        if inMempool {
                                tx = mempoolTx
                                loc.Block = &core.Block{} // block not yet assigned; height 0
                                txOk = true
                        }
                }
                if !txOk {
                        // Fallback 2: tx-hash index disk lookup (PutTxIdx entries).
                        // Only covers blocks accepted after PutTxIdx was introduced —
                        // older blocks fall through to Fallback 3 below.
                        diskTx, diskLoc, diskFound, diskErr := s.getTransactionFromDisk(txHash)
                        if diskErr != nil {
                                s.log.Warn("disk tx-index fallback error",
                                        "tx", u.TxHash[:min(16, len(u.TxHash))], "err", diskErr)
                        }
                        if diskFound {
                                tx, loc, txOk = diskTx, diskLoc, true
                        }
                }

                // Resolve the output — either from the full tx or from the UTXO store.
                var out core.Output
                if txOk {
                        if int(u.OutIdx) >= len(tx.Outputs) {
                                return nil, fmt.Errorf("out_idx %d out of range for tx %s (%d outputs)",
                                        u.OutIdx, u.TxHash[:min(16, len(u.TxHash))], len(tx.Outputs))
                        }
                        out = tx.Outputs[u.OutIdx]
                } else if s.blockStore != nil {
                        // Fallback 3: UTXO store — written at block-acceptance time for
                        // every output in every block since the node was first started.
                        // This is the only reliable fallback for admin-minted UTXOs in
                        // blocks predating PutTxIdx (the tx-hash index introduced later).
                        s.log.Info("WALLET_SEND_TRACE: trying blockStore.GetUTXO",
                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx)
                        su, suErr := s.blockStore.GetUTXO(txHash, uint32(u.OutIdx))
                        if suErr != nil {
                                s.log.Warn("WALLET_SEND_TRACE: blockStore.GetUTXO error",
                                        "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx, "err", suErr)
                                return nil, fmt.Errorf("utxo store fallback for tx %s[%d]: %w",
                                        u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, suErr)
                        }
                        if su != nil {
                                s.log.Info("WALLET_SEND_TRACE: blockStore.GetUTXO found UTXO",
                                        "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx,
                                        "height", su.BlockHeight,
                                        "commit", fmt.Sprintf("%x", su.AmountCommit[:8]))
                                // Synthesise core.Output from the stored UTXO fields.
                                // Only OneTimePub, TxPubKey, and AmountCommit are needed for
                                // RingCT input construction; EncAmount is not used downstream.
                                out = core.Output{
                                        OneTimePub:   su.OneTimePub,
                                        TxPubKey:     su.TxPubKey,
                                        AmountCommit: su.AmountCommit,
                                }
                                loc = core.TxLocation{
                                        Block:   &core.Block{Header: core.BlockHeader{Height: su.BlockHeight}},
                                        TxIndex: 0,
                                }
                        } else if func() bool {
                                s.log.Warn("WALLET_SEND_TRACE: blockStore.GetUTXO returned nil",
                                        "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx)
                                return false
                        }() {
                                // unreachable — the closure always returns false
                        } else if memUTXO := s.utxos.Get(txHash, uint32(u.OutIdx)); memUTXO != nil {
                                // Fallback 4: in-memory UTXOSet told us the block height; now
                                // read the block from disk to get the authoritative AmountCommit.
                                // The snapshot AmountCommit may be stale/corrupt after an OOM
                                // kill, but the raw block bytes on disk are the ground truth.
                                // The t/ tx-hash index is also missing (same OOM), so we scan
                                // the block's transactions directly.
                                s.log.Info("in-memory UTXO fallback: LevelDB u/+t/ absent; reading block from disk",
                                        "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx,
                                        "height", memUTXO.BlockHeight)
                                diskResolved := false
                                if s.blockStore != nil {
                                        raw, rawErr := s.blockStore.GetRawBlockByHeight(memUTXO.BlockHeight)
                                        if rawErr == nil && raw != nil {
                                                var blk core.Block
                                                if jsonErr := json.Unmarshal(raw, &blk); jsonErr == nil {
                                                        for ti, bTx := range blk.Txs {
                                                                if bTx.Hash() == txHash {
                                                                        if int(u.OutIdx) < len(bTx.Outputs) {
                                                                                out = bTx.Outputs[u.OutIdx]
                                                                                loc = core.TxLocation{
                                                                                        Block:   &blk,
                                                                                        TxIndex: ti,
                                                                                }
                                                                                diskResolved = true
                                                                                // Heal the stale byPubKey entry so VerifyTx
                                                                                // C-0 check finds the correct AmountCommit.
                                                                                s.utxos.PatchAmountCommit(
                                                                                        out.OneTimePub,
                                                                                        out.AmountCommit,
                                                                                )
                                                                                // Heal the missing u/ LevelDB entry so that
                                                                                // a subsequent node restart finds this UTXO
                                                                                // in LevelDB without needing Fallback 4 again.
                                                                                healEntry := &store.StoredUTXO{
                                                                                        TxHash:       txHash,
                                                                                        OutputIndex:  u.OutIdx,
                                                                                        OneTimePub:   out.OneTimePub,
                                                                                        TxPubKey:     out.TxPubKey,
                                                                                        AmountCommit: out.AmountCommit,
                                                                                        EncAmount:    out.EncAmount,
                                                                                        BlockHeight:  blk.Header.Height,
                                                                                }
                                                                                if putErr := s.blockStore.PutUTXO(txHash, u.OutIdx, healEntry); putErr != nil {
                                                                                        s.log.Warn("fallback4: failed to heal u/ entry in LevelDB",
                                                                                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx, "err", putErr)
                                                                                } else {
                                                                                        s.log.Info("fallback4: healed missing u/ entry in LevelDB",
                                                                                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx, "height", blk.Header.Height)
                                                                                }
                                                                        }
                                                                        break
                                                                }
                                                        }
                                                }
                                        }
                                        if rawErr != nil {
                                                s.log.Warn("fallback4: block read error",
                                                        "height", memUTXO.BlockHeight, "err", rawErr)
                                        }
                                }
                                if !diskResolved {
                                        // Block scan failed; use snapshot fields as last resort.
                                        // AmountCommit may be stale but ring-verify will catch it.
                                        s.log.Warn("fallback4: block scan failed; using snapshot AmountCommit",
                                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx)
                                        out = core.Output{
                                                OneTimePub:   memUTXO.OneTimePub,
                                                TxPubKey:     memUTXO.TxPubKey,
                                                AmountCommit: memUTXO.AmountCommit,
                                        }
                                        loc = core.TxLocation{
                                                Block:   &core.Block{Header: core.BlockHeader{Height: memUTXO.BlockHeight}},
                                                TxIndex: 0,
                                        }
                                        // Best-effort heal: write snapshot fields back to LevelDB so
                                        // subsequent restarts find this UTXO in u/ without Fallback 4.
                                        // AmountCommit may be stale (OOM snapshot); ring-verify will
                                        // catch it — the operator can re-mint to get a clean entry.
                                        if s.blockStore != nil {
                                                snapHeal := &store.StoredUTXO{
                                                        TxHash:       txHash,
                                                        OutputIndex:  u.OutIdx,
                                                        OneTimePub:   memUTXO.OneTimePub,
                                                        TxPubKey:     memUTXO.TxPubKey,
                                                        AmountCommit: memUTXO.AmountCommit,
                                                        EncAmount:    memUTXO.EncAmount,
                                                        BlockHeight:  memUTXO.BlockHeight,
                                                }
                                                if putErr := s.blockStore.PutUTXO(txHash, u.OutIdx, snapHeal); putErr != nil {
                                                        s.log.Warn("fallback4: failed to heal u/ entry (snapshot)",
                                                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx, "err", putErr)
                                                } else {
                                                        s.log.Info("fallback4: healed missing u/ entry from snapshot",
                                                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx, "height", memUTXO.BlockHeight)
                                                }
                                        }
                                }
                        } else {
                                // Both LevelDB u/ entry and in-memory UTXOSet have no record of
                                // this tx.  This happens when the node was restarted after an OOM
                                // kill and the UTXO store (u/ prefix) was not yet rebuilt.
                                // Run `aperod-node --repair-db` to restore missing u/ entries.
                                s.log.Warn("WALLET_SEND: tx not found in u/ store or in-memory UTXO set — run --repair-db",
                                        "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx)
                                return nil, fmt.Errorf("tx %s not found on chain or mempool — re-mint required after node restart",
                                        u.TxHash[:min(16, len(u.TxHash))])
                        }
                } else {
                        s.log.Warn("WALLET_SEND: blockStore is nil — cannot look up tx",
                                "tx", u.TxHash[:min(16, len(u.TxHash))], "out_idx", u.OutIdx)
                        return nil, fmt.Errorf("tx %s not found on chain or mempool — re-mint required after node restart",
                                u.TxHash[:min(16, len(u.TxHash))])
                }

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
                                        // Block-reward mint (height > 0): try V2 blind first (DeterministicMintBlindV2
                                        // includes height in the derivation — required for UTXOs created after the
                                        // F-049 fix).  Fall back to V1 for UTXOs minted before the migration.
                                        blindV2, errV2 := crypto.DeterministicMintBlindV2(spendPub, u.AmountNAPR, mintBlockHeight)
                                        if errV2 != nil {
                                                return nil, fmt.Errorf("deterministic mint blind v2 for %s[%d]: %w",
                                                        u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, errV2)
                                        }
                                        cV2, cErrV2 := crypto.Commit(u.AmountNAPR, blindV2)
                                        if cErrV2 == nil && cV2 == out.AmountCommit {
                                                blind = blindV2
                                                s.log.Info("DIAG: mint blind V2 OK",
                                                        "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                        "amount_napr", u.AmountNAPR,
                                                        "height", mintBlockHeight,
                                                )
                                        } else {
                                                // V2 mismatch: this UTXO was created before the F-049 blind migration.
                                                // Fall back to V1 for backward compatibility.
                                                blindV1, errV1 := crypto.DeterministicMintBlind(spendPub, u.AmountNAPR)
                                                if errV1 != nil {
                                                        return nil, fmt.Errorf("deterministic mint blind v1 for %s[%d]: %w",
                                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, errV1)
                                                }
                                                blind = blindV1
                                                cV1, _ := crypto.Commit(u.AmountNAPR, blindV1)
                                                if cV1 != out.AmountCommit {
                                                        s.log.Error("DIAG: MINT BLIND MISMATCH — neither V1 nor V2 blind matches on-chain commit",
                                                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                                "out_idx", u.OutIdx,
                                                                "amount_napr", u.AmountNAPR,
                                                                "height", mintBlockHeight,
                                                                "on_chain_commit", fmt.Sprintf("%x", out.AmountCommit[:]),
                                                        )
                                                } else {
                                                        s.log.Info("DIAG: mint blind V1 fallback OK (pre-migration UTXO)",
                                                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                                "amount_napr", u.AmountNAPR,
                                                                "height", mintBlockHeight,
                                                        )
                                                }
                                        }
                                } else {
                                        // Legacy/admin mint (height == 0): always use V1.
                                        blind, err = crypto.DeterministicMintBlind(spendPub, u.AmountNAPR)
                                        if err != nil {
                                                return nil, fmt.Errorf("deterministic blind for %s[%d]: %w",
                                                        u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
                                        }
                                        // Diagnostic: verify recomputed commitment matches on-chain commitment.
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
                                if cErr != nil {
                                        return nil, fmt.Errorf(
                                                "commitment mismatch for tx %s[%d]: amount_napr=%d — deterministic blind computation failed: %w",
                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, u.AmountNAPR, cErr)
                                } else if recomputed != out.AmountCommit {
                                        s.log.Error("DIAG: pre-flight stealth commitment mismatch",
                                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                                "out_idx", u.OutIdx,
                                                "amount_napr", u.AmountNAPR,
                                                "recomputed_commit", fmt.Sprintf("%x", recomputed[:8]),
                                                "on_chain_commit", fmt.Sprintf("%x", out.AmountCommit[:8]),
                                        )
                                        return nil, fmt.Errorf(
                                                "commitment mismatch for tx %s[%d]: amount_napr=%d recomputed=%x on_chain=%x — "+
                                                        "UTXO predates deterministic blind migration, admin re-mint required",
                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx,
                                                u.AmountNAPR,
                                                recomputed[:8],
                                                out.AmountCommit[:8])
                                }
                        } else {
                                blind, err = blindFactorFromHex(u.BlindHex)
                                if err != nil {
                                        return nil, fmt.Errorf("blind_hex for %s[%d]: %w",
                                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, err)
                                }
                        }
                }
                // ── Heal stale byPubKey entry ─────────────────────────────────────────
                // byPubKey is loaded from the snapshot at startup.  After an OOM kill
                // the snapshot may hold a corrupt AmountCommit for this UTXO while the
                // LevelDB u/ store or the raw block on disk has the correct value.
                // We patch byPubKey here — after out.AmountCommit is resolved from the
                // best available source (Fallback 1-4) — so that VerifyTx C-0 check
                // sees the same AmountCommit that the transaction was built with.
                // This is a no-op when byPubKey already holds the correct value.
                s.utxos.PatchAmountCommit(out.OneTimePub, out.AmountCommit)

                // ── Pre-flight commitment check ───────────────────────────────────────
                // Verify that (amount_napr, blind) reproduces the on-chain AmountCommit
                // before passing the UTXO to TxBuilder.  If the stored blind_hex or
                // amount_napr in the DB is wrong the ring builder will silently produce a
                // bad ring and the validator rejects the tx with "no ring member found
                // matching claimed commitment (C-0)".  Catching it here gives a clear
                // error that the TypeScript layer can map to "re-mint required" or UTXO
                // skip logic, rather than a cryptic ring-verification failure.
                if preCommit, preErr := crypto.Commit(u.AmountNAPR, blind); preErr != nil {
                        return nil, fmt.Errorf("commit recompute for %s[%d]: %w",
                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx, preErr)
                } else if preCommit != out.AmountCommit {
                        s.log.Error("DIAG: pre-flight commitment mismatch",
                                "tx", u.TxHash[:min(16, len(u.TxHash))],
                                "out_idx", u.OutIdx,
                                "amount_napr", u.AmountNAPR,
                                "recomputed_commit", fmt.Sprintf("%x", preCommit[:8]),
                                "on_chain_commit", fmt.Sprintf("%x", out.AmountCommit[:8]),
                        )
                        return nil, fmt.Errorf("commitment mismatch for tx %s[%d]: amount_napr=%d recomputed=%x on_chain=%x — stored blind/amount does not reproduce on-chain commitment — re-mint required",
                                u.TxHash[:min(16, len(u.TxHash))], u.OutIdx,
                                u.AmountNAPR,
                                preCommit[:8],
                                out.AmountCommit[:8])
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
        // Phase 2 decoys: WithDecoySet supplies the live UTXOSet so that
        // TxBuilder.SampleDecoys pulls real spent UTXOs from spentPubKeys as ring
        // members.  Spent UTXOs are absent from byPubKey, so the C-0 check in
        // tx_verifier.go skips them (identical to Phase 1 random keys).  The
        // redesigned C-0 check also tolerates active decoys with different
        // AmountCommit values — it requires only that AT LEAST ONE present ring
        // member matches inp.AmountCommit (the real spender, always in byPubKey).
        builder := core.NewTxBuilder(spendPriv, viewPriv, spendPub, ownedUTXOs, 0).
                WithDecoySet(s.utxos)
        result, err := builder.Build(p.AmountNAPR, crypto.Address(p.ToAddress), changeAddr)
        if err != nil {
                return nil, fmt.Errorf("build: %w", err)
        }
        if result.FallbackDecoyCount > 0 {
                s.log.Warn("privacy degraded: ring contains Phase 1 fallback decoys",
                        "fallback_decoy_count", result.FallbackDecoyCount,
                        "real_decoy_count", result.RealDecoyCount,
                        "input_count", result.InputCount,
                        "ring_size", "16",
                        "reason", "spent-UTXO pool has fewer real decoys than required",
                )
        }

        // ── 6. Cryptographic verification ────────────────────────────────────────
        // Phase 2: all ring members are real on-chain UTXOs — enable strict C-0 check.
        verifier := core.NewTxVerifier(s.utxos)
        if err := verifier.VerifyTx(&result.Tx); err != nil {
                if isKeyImageConflictErr(err) {
                        if dsErr := describeSpentInput(s.utxos, result); dsErr != nil {
                                return nil, fmt.Errorf("verify: %w", dsErr)
                        }
                }
                return nil, fmt.Errorf("verify: %w", err)
        }

        // ── 7. Submit to mempool ──────────────────────────────────────────────────
        // Guard: if the HTTP client disconnected while the ring-sig was being
        // computed (step 5-6), abort now so we never submit a tx whose change
        // blind will never reach the caller.  The UTXO remains unspent.
        if err := ctx.Err(); err != nil {
                s.log.Warn("WALLET_SEND: client disconnected before mempool submit — aborting",
                        "tx_hash", fmt.Sprintf("%x", result.Tx.Hash()),
                        "reason", err,
                )
                return nil, fmt.Errorf("client disconnected before transaction was submitted: %w", err)
        }
        if err := s.mempool.Add(result.Tx); err != nil {
                if isKeyImageConflictErr(err) {
                        if dsErr := describeSpentInput(s.utxos, result); dsErr != nil {
                                return nil, fmt.Errorf("mempool: %w", dsErr)
                        }
                }
                return nil, fmt.Errorf("mempool: %w", err)
        }

        // ── 8. Return tx hash + change + payment metadata ────────────────────────
        txHash := result.Tx.Hash()
        changeBlindHex := ""
        if result.ChangeAmount > 0 {
                changeBlindHex = hex.EncodeToString(result.ChangeBlind[:])
        }

        // ── 8a. Build spent-input key-image list (Monero-style) ──────────────
        // Each real ring input carries its key image in result.Tx.Inputs[i].KeyImage.
        // We pair it with the source UTXO from result.SelectedUTXOs so the caller
        // can record KI in utxo_blinds.key_image_hex.  Checking that value against
        // GET /api/v1/keyimage/{ki}/is-spent gives an authoritative spent check:
        // a KI present in the ki/ LevelDB index is definitively spent.
        spentKIs := make([]spentInputKI, 0, len(result.SelectedUTXOs))
        for i, su := range result.SelectedUTXOs {
                if i >= len(result.Tx.Inputs) {
                        break
                }
                ki := result.Tx.Inputs[i].KeyImage
                spentKIs = append(spentKIs, spentInputKI{
                        TxHash:      fmt.Sprintf("%x", su.TxHash[:]),
                        OutputIndex: su.OutputIndex,
                        KeyImageHex: fmt.Sprintf("%x", ki[:]),
                })
        }

        return walletSendResult{
                TxHash:             fmt.Sprintf("%x", txHash[:]),
                TotalFeeNAPR:       result.TotalFee,
                ChangeAmtNAPR:      result.ChangeAmount,
                ChangeOutIdx:       result.ChangeOutIdx,
                ChangeBlindHex:     changeBlindHex,
                PayBlindHex:        hex.EncodeToString(result.PayBlind[:]),
                PayOutIdx:          result.PayOutIdx,
                PayAmtNAPR:         p.AmountNAPR,
                DecoyCount:         result.RealDecoyCount,
                FallbackDecoyCount: result.FallbackDecoyCount,
                SpentKeyImages:     spentKIs,
        }, nil
}

// describeSpentInput checks each real input of a freshly built transaction
// against the chain key-image set and, when one is flagged as spent, returns
// an error that names the exact source UTXO (tx hash + output index).
//
// Wallets use this reference to skip only the failing candidate instead of
// discarding all of them (Task #1928).  The message also carries a phantom
// hint: after an OOM kill the persistent key-image index can hold entries for
// key images that never appeared in any confirmed transaction (Task #1929);
// operators repair that with `aperod-node --rebuild-key-images`.
//
// isKeyImageConflictErr reports whether an error from VerifyTx or Mempool.Add
// is a key-image double-spend conflict (verifier: "already spent";
// mempool: "double-spend attempt, key image conflicts with tx …").
func isKeyImageConflictErr(err error) bool {
        if err == nil {
                return false
        }
        msg := err.Error()
        return strings.Contains(msg, "already spent") || strings.Contains(msg, "double-spend")
}

// Returns nil when no input is flagged (the original error was unrelated).
//
// Callers must gate on isKeyImageConflictErr first: attributing an unrelated
// verification error to a "spent" UTXO (e.g. after a block landed between
// VerifyTx and the re-check) would make wallets skip a healthy candidate.
func describeSpentInput(utxos *core.UTXOSet, result *core.BuildResult) error {
        if utxos == nil || result == nil {
                return nil
        }
        for i, inp := range result.Tx.Inputs {
                if !utxos.IsSpent(inp.KeyImage) {
                        continue
                }
                if i < len(result.SelectedUTXOs) {
                        su := result.SelectedUTXOs[i]
                        return fmt.Errorf(
                                "key image already spent for utxo %x[%d] — if this UTXO has no confirmed spend on-chain, the key-image index holds a phantom entry; run aperod-node --rebuild-key-images",
                                su.TxHash[:], su.OutputIndex)
                }
                return fmt.Errorf("key image already spent at input %d", i)
        }
        return nil
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
