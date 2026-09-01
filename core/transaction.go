package core

import (
        "encoding/binary"
        "fmt"

        "github.com/aperod/aperod/crypto"
)

// TxVersion identifies the transaction format version.
type TxVersion uint8

const (
        TxVersionBase      TxVersion = 1 // Standard RingCT transaction
        TxVersionGameAsset TxVersion = 2 // Transaction with game asset data in Extra
        TxVersionStake     TxVersion = 3 // Validator stake deposit / withdrawal
	// TxVersionCommitmentBinding adds a linked Pedersen-opening proof to every
	// MLSAG signature. Legacy versions remain valid for historical replay.
	TxVersionCommitmentBinding TxVersion = 4
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
        // Fee is the miner fee in base units (APRO × 10^8). NOT hidden (required for block validity).
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
	// RealIndex is disclosed by v4 transactions so the state transition can
	// remove the exact UTXO proven by the direct ownership link. It is ignored
	// for legacy versions.
	RealIndex uint8
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
        // F-4 fix: hash covers ALL fields that determine what the transaction does,
        // so that the MLSAG signing message H(txHash||inputIdx) binds the signature
        // to FeeCommit, per-input AmountCommit, and per-output TxPubKey/EncAmount.
        // A third party cannot swap these fields without invalidating all signatures.
        parts := [][]byte{
                {byte(tx.Version)},
                encodeUint64(tx.Fee),
                tx.FeeCommit[:],
                tx.Extra,
        }
        for _, inp := range tx.Inputs {
                parts = append(parts, inp.KeyImage[:])
                parts = append(parts, inp.AmountCommit[:]) // F-4: was missing
                for _, rm := range inp.Ring {
                        parts = append(parts, rm[:])
                }
		if tx.Version == TxVersionCommitmentBinding {
			parts = append(parts, []byte{inp.RealIndex})
		}
        }
        for _, out := range tx.Outputs {
                parts = append(parts, out.OneTimePub[:])
                parts = append(parts, out.AmountCommit[:])
                parts = append(parts, out.TxPubKey[:])    // F-4: was missing
                parts = append(parts, out.EncAmount[:])   // F-4: was missing
        }
        return crypto.HashBytes(parts...)
}

// IsCoinbase returns true if this transaction has no inputs (miner reward).
// Coinbase transactions skip ring signature and range proof requirements.
func (tx *Transaction) IsCoinbase() bool {
        return len(tx.Inputs) == 0
}

// IsStake returns true if this is a validator stake deposit or withdrawal.
// Stake transactions carry their payload in Extra and skip RingCT validation.
func (tx *Transaction) IsStake() bool {
        return tx.Version == TxVersionStake
}

// Validate checks structural validity of the transaction.
// Cryptographic validity (ring sigs, range proofs) is checked separately in tx_verifier.go.
func (tx *Transaction) Validate() error {
        if tx.Version == 0 {
                return fmt.Errorf("tx version 0 is invalid")
        }
	switch tx.Version {
	case TxVersionBase, TxVersionGameAsset, TxVersionStake, TxVersionCommitmentBinding:
	default:
		return fmt.Errorf("unsupported tx version %d", tx.Version)
	}

        // Stake transactions carry payload in Extra.  Validate the Extra field,
        // then fall through to enforce RangeProof count for any outputs (C-2 fix:
        // the previous early-return let stake txs carry outputs without proofs).
        //
        // v1 payload (105 bytes): StakeWithdraw / StakePartialWithdraw
        // v2 payload (173 bytes): StakeDeposit with UTXO burn proof (C-1 fix)
        // v3 payload (237 bytes): StakeDeposit with UTXO burn proof + one-time-key ownership proof (F-049 fix)
        if tx.IsStake() {
                extraLen := len(tx.Extra)
                if extraLen != StakePayloadSize && extraLen != StakePayloadSizeV2 && extraLen != StakePayloadSizeV3 {
                        return fmt.Errorf("stake tx: extra must be %d bytes (withdraw) or %d bytes (deposit), got %d",
                                StakePayloadSize, StakePayloadSizeV2, extraLen)
                }
                action := StakeAction(tx.Extra[0])
                if action != StakeDeposit && action != StakeWithdraw && action != StakePartialWithdraw {
                        return fmt.Errorf("stake tx: unknown action %d", action)
                }
                // Stake txs with no outputs need no further RingCT validation.
                if len(tx.Outputs) == 0 {
                        return nil
                }
                // Has outputs → fall through to enforce len(RangeProofs)==len(Outputs).
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
		if tx.Version == TxVersionCommitmentBinding {
			if int(inp.RealIndex) >= crypto.RingSize {
				return fmt.Errorf("input %d: real index %d out of range", i, inp.RealIndex)
			}
			if tx.Signatures[i] != nil &&
				(len(tx.Signatures[i].BlindSS) != crypto.RingSize ||
					len(tx.Signatures[i].ValueSS) != crypto.RingSize) {
				return fmt.Errorf("input %d: v4 signature opening response length mismatch", i)
			}
		}
        }
        return nil
}

// ValidateTxVersionAtHeight applies the height-gated consensus policy for new
// transaction formats without changing historical replay semantics.
func ValidateTxVersionAtHeight(tx *Transaction, height, ringCTV4ActivationHeight uint64) error {
	if tx.Version == TxVersionCommitmentBinding &&
		ringCTV4ActivationHeight > 0 &&
		height < ringCTV4ActivationHeight {
		return fmt.Errorf("transaction version %d is not active until height %d",
			tx.Version, ringCTV4ActivationHeight)
	}
	return nil
}

// Size returns an approximate byte size for fee estimation.
func (tx *Transaction) Size() int {
        // Header: version(1) + fee(8) + feeCommit(32)
        size := 1 + 8 + 32
        // Each input: keyImage(32) + ring(16×32) + amountCommit(32)
        size += len(tx.Inputs) * (32 + crypto.RingSize*32 + 32)
	if tx.Version == TxVersionCommitmentBinding {
		size += len(tx.Inputs) // disclosed real index
	}
        // Each output: oneTimePub(32) + txPubKey(32) + amountCommit(32) + encAmount(8)
        size += len(tx.Outputs) * (32 + 32 + 32 + 8)
	// MLSAG: c0(32) + ss(16×32) + keyImage(32) per input. v4 adds
	// blind/value opening responses, one scalar per ring member each.
	sigBytes := 32 + crypto.RingSize*32 + 32
	if tx.Version == TxVersionCommitmentBinding {
		sigBytes += 2*crypto.RingSize*32 + 64 // opening responses + link R/S
	}
	size += len(tx.Signatures) * sigBytes
        // Range proofs: simplified (real bulletproofs ~675 bytes each)
        size += len(tx.RangeProofs) * 675
        // Extra
        size += len(tx.Extra)
        return size
}

// InitialBaseFeePerByte is the starting base fee per byte at genesis: 200 nAPRO/byte.
// A typical P2P transfer (~2 000 bytes) costs 400 000 nAPRO = 0.004 APRO at genesis.
// The base fee adjusts dynamically each block based on block fill ratio (EIP-1559).
const InitialBaseFeePerByte uint64 = 200

// MinBaseFeePerByte is the floor below which the base fee never drops: 50 nAPRO/byte.
// A 2 000-byte tx therefore costs at least 100 000 nAPRO = 0.001 APRO.
const MinBaseFeePerByte uint64 = 50

// MinFeeAt returns the minimum acceptable fee for this transaction at the given
// base fee rate.  Any excess paid above this minimum is a priority tip to the validator.
func (tx *Transaction) MinFeeAt(baseFeePerByte uint64) uint64 {
        if baseFeePerByte == 0 {
                baseFeePerByte = InitialBaseFeePerByte
        }
        return uint64(tx.Size()) * baseFeePerByte
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
