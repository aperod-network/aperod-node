package core

import (
        "fmt"

        "github.com/aperod/aperod/crypto"
)

// TxVerifier validates the cryptographic integrity of transactions.
// Separate from Validate() (structural) — this checks ring sigs and range proofs.
type TxVerifier struct {
        utxos *UTXOSet
}

// NewTxVerifier creates a verifier backed by the given UTXO set.
func NewTxVerifier(utxos *UTXOSet) *TxVerifier {
        return &TxVerifier{utxos: utxos}
}

// VerifyTx performs full cryptographic verification of a non-coinbase transaction.
//
// Checks performed:
//  1. Structural validity (Validate)
//  2. No duplicate key images within the transaction
//  3. No already-spent key images (double-spend)
//  4. MLSAG ring signatures are valid for each input
//  5. Range proofs are valid for each output
//  6. Pedersen commitment balance: ΣC_in = ΣC_out + C_fee
func (v *TxVerifier) VerifyTx(tx *Transaction) error {
        // 1. Structural check
        if err := tx.Validate(); err != nil {
                return err
        }

        // Coinbase transactions have no cryptographic proofs to check
        if len(tx.Inputs) == 0 {
                return nil
        }

        txHash := tx.Hash()
        txHashPrefix := txHash // copy for slicing

        // 2. No duplicate key images within the transaction
        seen := make(map[crypto.KeyImage]bool, len(tx.Inputs))
        for i, inp := range tx.Inputs {
                if seen[inp.KeyImage] {
                        return fmt.Errorf("tx %x: duplicate key image at input %d", txHashPrefix[:8], i)
                }
                seen[inp.KeyImage] = true
        }

        // 3. No already-spent key images
        if v.utxos != nil {
                for i, inp := range tx.Inputs {
                        if v.utxos.IsSpent(inp.KeyImage) {
                                kiPrefix := inp.KeyImage
                                return fmt.Errorf("tx %x: double-spend at input %d (key image %x already spent)",
                                        txHashPrefix[:8], i, kiPrefix[:8])
                        }
                }
        }

        // 4. MLSAG ring signatures
        for i, inp := range tx.Inputs {
                sig := tx.Signatures[i]
                if sig == nil {
                        return fmt.Errorf("tx %x: nil signature at input %d", txHashPrefix[:8], i)
                }
                // The signed message is H(txHash || inputIndex)
                msg := ringSignMessage(txHash, uint32(i))
                ok, err := crypto.MLSAGVerify(msg, inp.Ring, sig)
                if err != nil {
                        return fmt.Errorf("tx %x: ring sig error at input %d: %w", txHashPrefix[:8], i, err)
                }
                if !ok {
                        return fmt.Errorf("tx %x: invalid ring signature at input %d", txHashPrefix[:8], i)
                }
                // Verify that the key image in the signature matches the input's key image
                if sig.KeyImage != inp.KeyImage {
                        return fmt.Errorf("tx %x: key image mismatch at input %d", txHashPrefix[:8], i)
                }
        }

        // 5. Range proofs for all outputs
        for i, proof := range tx.RangeProofs {
                ok, err := crypto.VerifyRange(proof)
                if err != nil {
                        return fmt.Errorf("tx %x: range proof error at output %d: %w", txHashPrefix[:8], i, err)
                }
                if !ok {
                        return fmt.Errorf("tx %x: invalid range proof at output %d", txHashPrefix[:8], i)
                }
                // Verify that the proof covers the correct commitment
                if proof.ValueCommit != tx.Outputs[i].AmountCommit {
                        return fmt.Errorf("tx %x: range proof commitment mismatch at output %d", txHashPrefix[:8], i)
                }
        }

        // 6. Commitment balance: ΣC_in = ΣC_out + C_fee
        inCommits := make([]crypto.Commitment, len(tx.Inputs))
        for i, inp := range tx.Inputs {
                inCommits[i] = inp.AmountCommit
        }
        outCommits := make([]crypto.Commitment, len(tx.Outputs))
        for i, out := range tx.Outputs {
                outCommits[i] = out.AmountCommit
        }
        balanced, err := crypto.CommitSum(inCommits, outCommits, tx.FeeCommit)
        if err != nil {
                return fmt.Errorf("tx %x: commitment sum error: %w", txHashPrefix[:8], err)
        }
        if !balanced {
                return fmt.Errorf("tx %x: commitment balance check failed (inputs ≠ outputs + fee)", txHashPrefix[:8])
        }

        return nil
}

// VerifyBlock verifies all transactions in a block.
// Applies inputs sequentially (no parallel to maintain UTXO consistency).
func (v *TxVerifier) VerifyBlock(block *Block) error {
        for i, tx := range block.Txs {
                if err := v.VerifyTx(&tx); err != nil {
                        h := tx.Hash()
                        return fmt.Errorf("block %d tx[%d] %x: %w",
                                block.Header.Height, i, h[:8], err)
                }
        }
        return nil
}

// ringSignMessage computes the message hash for an MLSAG ring signature.
// msg = SHA3(txHash || inputIndex) — binds the signature to a specific input.
func ringSignMessage(txHash crypto.Hash32, inputIdx uint32) crypto.Hash32 {
        idx := []byte{byte(inputIdx >> 24), byte(inputIdx >> 16), byte(inputIdx >> 8), byte(inputIdx)}
        return crypto.HashBytes([]byte("aperod/ring-sign/v1"), txHash[:], idx)
}

// RingSignMessage is exported for use in tx building.
func RingSignMessage(txHash crypto.Hash32, inputIdx uint32) crypto.Hash32 {
        return ringSignMessage(txHash, inputIdx)
}
