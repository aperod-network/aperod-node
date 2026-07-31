package api

import (
        "encoding/hex"
        "encoding/json"
        "fmt"

        "github.com/aperod/aperod/core"
        "github.com/aperod/aperod/crypto"
)

// ─── apr_scanUTXOs ───────────────────────────────────────────────────────────
//
// Scans the entire chain for stealth outputs belonging to a wallet identified
// by (spendPub, viewPriv). Used by the Node.js layer as a fallback when
// utxo_blinds DB records are missing — the returned UTXOs can be spent with
// blind_hex="" because the Go spending path derives the blind deterministically
// from the ECDH shared secret (HsScalar + amount).

type scanUTXOsParams struct {
        SpendPubHex string `json:"spend_pub_hex"` // 32-byte spend public key hex
        ViewKeyHex  string `json:"view_key_hex"`  // 32-byte view private scalar hex
}

type scannedUTXO struct {
        TxHash      string `json:"tx_hash"`
        OutIdx      uint32 `json:"out_idx"`
        AmountNAPR  uint64 `json:"amount_napr"`
        BlindHex    string `json:"blind_hex"`     // always "" — Go recomputes deterministically
        HsScalarHex string `json:"hs_scalar_hex"` // ECDH shared secret; pass to stake-deposit to derive blind
}

func (s *Server) aprScanUTXOs(params json.RawMessage) (interface{}, error) {
        var p scanUTXOsParams
        if err := json.Unmarshal(params, &p); err != nil {
                return nil, fmt.Errorf("params: %w", err)
        }

        // Decode spend public key
        spendPubBytes, err := hex.DecodeString(p.SpendPubHex)
        if err != nil || len(spendPubBytes) != 32 {
                return nil, fmt.Errorf("spend_pub_hex: expected 32-byte hex")
        }
        var spendPub crypto.Point32
        copy(spendPub[:], spendPubBytes)

        // Decode view private scalar
        viewPriv, err := scalar32FromHex(p.ViewKeyHex, "view_key_hex")
        if err != nil {
                return nil, err
        }

        // Derive view public key for scanner
        viewPub, err := crypto.PublicKeyFromPrivate(viewPriv)
        if err != nil {
                return nil, fmt.Errorf("derive view pub: %w", err)
        }

        // Build a scanner and scan the whole chain
        scanner := core.NewWalletScanner(viewPriv, spendPub, viewPub, crypto.MainnetByte)
        tip := s.chain.Height()
        owned := scanner.ScanChain(s.chain, 0, tip)

        // Check which UTXOs are still unspent (UTXO set membership)
        result := make([]scannedUTXO, 0, len(owned))
        for _, u := range owned {
                // Skip if already spent (not in UTXO set)
                if s.utxos.Get(u.TxHash, u.OutputIndex) == nil {
                        continue
                }

                // Verify the deterministic blind matches the on-chain commitment.
                // UTXOs built before the deterministic-blind migration won't match;
                // we skip them (they require the stored blind_hex from utxo_blinds).
                deterministicBlind, bErr := crypto.DeterministicPaymentBlind(u.HsScalar, u.Amount)
                if bErr != nil {
                        continue
                }
                commit, cErr := crypto.Commit(u.Amount, deterministicBlind)
                if cErr != nil || commit != u.AmountCommit {
                        // Pre-migration UTXO with random blind — not recoverable via scan.
                        // Caller must use utxo_blinds or request admin re-mint.
                        continue
                }

                result = append(result, scannedUTXO{
                        TxHash:      fmt.Sprintf("%x", u.TxHash[:]),
                        OutIdx:      u.OutputIndex,
                        AmountNAPR:  u.Amount,
                        BlindHex:    "", // Go spending path derives this deterministically
                        HsScalarHex: fmt.Sprintf("%x", u.HsScalar[:]),
                })
        }

        return map[string]interface{}{
                "utxos": result,
                "count": len(result),
        }, nil
}
