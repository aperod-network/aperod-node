package core

import (
        "encoding/binary"
        "fmt"

        "github.com/aperod/aperod/crypto"
)

// TxVersion identifies the transaction format version.
type TxVersion uint8

const (
        TxVersionBase    TxVersion = 1 // Standard RingCT transaction
        TxVersionGameAsset TxVersion = 2 // Transaction with game asset data in Extra
)

// Transaction is the core unit of value transfer in Aperod.
// All amounts are hidden via Pedersen commitments; senders are hidden via MLSAG ring signatures.
type Transaction struct {
        // Version identifies the tx format.
        Version TxVersion
        // Inputs are the ring-signature-covered UTXOs being spent.
        Inputs []RingInput
        // Outputs are the new UTXOs created by this transaction.
        Outputs []Output
        // Fee is the miner fee in base units (APR × 10^8). NOT hidden (required for block validity).
        Fee uint64
        // FeeCommit is a Pedersen commitment to Fee for the balance equation.
        FeeCommit crypto.Commitment
        // RangeProofs proves each output amount is in [0, 2^64).
        RangeProofs []*crypto.RangeProof
        // Signature is the MLSAG ring signature covering all inputs.
        // One per input (multi-input MLSAG is concatenated here for simplicity).
        Signatures []*crypto.MLSAGSignature
        // Extra holds optional data: game asset metadata, OP_RETURN-style notes.
        // Max 255 bytes.
        Extra []byte
}

// RingInput is one spent UTXO, described by a ring of public keys that hide
// the real spender among RingSize-1 decoys.
type RingInput struct {
        // KeyImage is the unique fingerprint of this UTXO spend (prevents double-spend).
        KeyImage crypto.KeyImage
        // Ring contains RingSize public keys (one-time spend keys of UTXOs).
        Ring []crypto.RingMember // len = crypto.RingSize
        // AmountCommit is the Pedersen commitment of the UTXO being spent (from the original Output).
        AmountCommit crypto.Commitment
}

// Output is a newly created UTXO.
type Output struct {
        // OneTimePub is the stealth address public key for this output.
        OneTimePub crypto.Point32
        // TxPubKey is the ephemeral public key used to derive OneTimePub.
        TxPubKey crypto.Point32
        // AmountCommit is the Pedersen commitment C = r·G + v·H hiding the amount.
        AmountCommit crypto.Commitment
        // EncAmount is the amount encrypted with the recipient's view key (for scanning).
        // 8 bytes XOR-encrypted with the ECDH shared secret.
        EncAmount [8]byte
}

// Hash returns the SHA3-256 hash of the transaction (used as transaction ID).
func (tx *Transaction) Hash() crypto.Hash32 {
        parts := [][]byte{
                {byte(tx.Version)},
                encodeUint64(tx.Fee),
                tx.Extra,
        }
        for _, inp := range tx.Inputs {
                parts = append(parts, inp.KeyImage[:])
                for _, rm := range inp.Ring {
                        parts = append(parts, rm[:])
                }
        }
        for _, out := range tx.Outputs {
                parts = append(parts, out.OneTimePub[:])
                parts = append(parts, out.AmountCommit[:])
        }
        return crypto.HashBytes(parts...)
}

// IsCoinbase returns true if this transaction has no inputs (miner reward).
// Coinbase transactions skip ring signature and range proof requirements.
func (tx *Transaction) IsCoinbase() bool {
        return len(tx.Inputs) == 0
}

// Validate checks structural validity of the transaction.
// Cryptographic validity (ring sigs, range proofs) is checked separately in tx_verifier.go.
func (tx *Transaction) Validate() error {
        if tx.Version == 0 {
                return fmt.Errorf("tx version 0 is invalid")
        }
        if len(tx.Outputs) == 0 {
                return fmt.Errorf("tx has no outputs")
        }
        if len(tx.Outputs) > 16 {
                return fmt.Errorf("tx has too many outputs: %d (max 16)", len(tx.Outputs))
        }
        if len(tx.Extra) > 255 {
                return fmt.Errorf("tx extra too large: %d bytes (max 255)", len(tx.Extra))
        }

        // Coinbase: no inputs required, no signatures or ring proofs
        if tx.IsCoinbase() {
                return nil
        }

        if len(tx.Inputs) == 0 {
                return fmt.Errorf("tx has no inputs")
        }
        if len(tx.Signatures) != len(tx.Inputs) {
                return fmt.Errorf("tx: %d signatures for %d inputs", len(tx.Signatures), len(tx.Inputs))
        }
        if len(tx.RangeProofs) != len(tx.Outputs) {
                return fmt.Errorf("tx: %d range proofs for %d outputs", len(tx.RangeProofs), len(tx.Outputs))
        }
        for i, inp := range tx.Inputs {
                if len(inp.Ring) != crypto.RingSize {
                        return fmt.Errorf("input %d: ring size %d != required %d", i, len(inp.Ring), crypto.RingSize)
                }
                if inp.KeyImage == (crypto.KeyImage{}) {
                        return fmt.Errorf("input %d: zero key image", i)
                }
        }
        return nil
}

// Size returns an approximate byte size for fee estimation.
func (tx *Transaction) Size() int {
        // Header: version(1) + fee(8) + feeCommit(32)
        size := 1 + 8 + 32
        // Each input: keyImage(32) + ring(11×32) + amountCommit(32)
        size += len(tx.Inputs) * (32 + crypto.RingSize*32 + 32)
        // Each output: oneTimePub(32) + txPubKey(32) + amountCommit(32) + encAmount(8)
        size += len(tx.Outputs) * (32 + 32 + 32 + 8)
        // MLSAG: c0(32) + ss(11×32) + keyImage(32) per input
        size += len(tx.Signatures) * (32 + crypto.RingSize*32 + 32)
        // Range proofs: simplified (real bulletproofs ~675 bytes each)
        size += len(tx.RangeProofs) * 675
        // Extra
        size += len(tx.Extra)
        return size
}

// MinFee returns the minimum fee for a transaction of this size (1 nAPR per byte).
func (tx *Transaction) MinFee() uint64 {
        return uint64(tx.Size())
}

// KeyImages returns all key images from inputs (for double-spend checking).
func (tx *Transaction) KeyImages() []crypto.KeyImage {
        kis := make([]crypto.KeyImage, len(tx.Inputs))
        for i, inp := range tx.Inputs {
                kis[i] = inp.KeyImage
        }
        return kis
}

// CoinbaseTx creates a special coinbase transaction that mints block rewards.
// In Aperod, block rewards go to the validator; privacy is not applied to coinbase.
func CoinbaseTx(validatorPub crypto.Point32, reward uint64) Transaction {
        blind, _ := crypto.NewBlindFactor()
        commit, _ := crypto.Commit(reward, blind)
        return Transaction{
                Version: TxVersionBase,
                Inputs:  nil, // coinbase has no inputs
                Outputs: []Output{{
                        OneTimePub:   validatorPub,
                        AmountCommit: commit,
                }},
                Fee: 0,
        }
}

// encodeUint64Tx is a local helper (block.go also has one; shared in a util pkg ideally).
func init() {
        // Ensure binary package is used
        _ = binary.LittleEndian
}
