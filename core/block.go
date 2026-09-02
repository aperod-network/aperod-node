// Package core contains the fundamental blockchain data structures.
package core

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/aperod/aperod/crypto"
)

// BlockHeader contains all metadata for a block, without the transactions.
// The hash of the header uniquely identifies a block in the chain.
type BlockHeader struct {
	// Height is the block number (genesis = 0).
	Height uint64
	// PrevHash is the SHA3-256 hash of the previous block header.
	PrevHash crypto.Hash32
	// MerkleRoot is the Merkle root of all transactions in this block.
	MerkleRoot crypto.Hash32
	// Timestamp is Unix nanoseconds when the block was produced.
	Timestamp int64
	// Round is the PoA consensus round number (monotonically increasing).
	Round uint32
	// ValidatorPub is the public key of the validator that proposed this block.
	ValidatorPub crypto.ValidatorPubKey
	// OraclePrice is the APRO/USD price embedded by the proposing validator,
	// expressed as USD-per-APRO × 10^9 (9 decimal fixed-point, uint64).
	// Example: price $0.001 → OraclePrice = 1_000_000.
	// Zero means the validator did not embed a price (pre-oracle blocks).
	OraclePrice uint64
	// BaseFee is the protocol-level base fee per byte in nAPRO for this block,
	// computed from the previous block's fill ratio (EIP-1559 style).
	// Every transaction must satisfy tx.Fee >= tx.Size() × BaseFee.
// Every transaction fee is burned; validators are paid by the staking reward pool
// or, after it is exhausted, by tail emission.
	// Encoded as nAPRO per byte (e.g. 200 = 200 nAPRO/byte).
	// Zero is treated as InitialBaseFeePerByte (genesis / pre-dynamic-fee blocks).
	BaseFee uint64
	// Signature is the ED25519 signature of the block header hash by ValidatorPub.
	Signature []byte
}

// Block is a full block including header and all transactions.
type Block struct {
	Header BlockHeader
	Txs    []Transaction
}

// Hash computes the SHA3-256 hash of the block header (the canonical block ID).
// OraclePrice is included so any tampering with the embedded price invalidates
// the block hash and thus the validator's signature.
func (h *BlockHeader) Hash() crypto.Hash32 {
	return crypto.HashBytes(
		encodeUint64(h.Height),
		h.PrevHash[:],
		h.MerkleRoot[:],
		encodeInt64(h.Timestamp),
		encodeUint32(h.Round),
		h.ValidatorPub,
		encodeUint64(h.OraclePrice),
		encodeUint64(h.BaseFee),
	)
}

// Sign signs the block header with the given validator private key.
// Call this after setting all header fields except Signature.
func (h *BlockHeader) Sign(priv crypto.ValidatorPrivKey) error {
	hash := h.Hash()
	sig, err := priv.Sign(hash)
	if err != nil {
		return fmt.Errorf("sign block header: %w", err)
	}
	h.Signature = sig
	return nil
}

// VerifySignature checks that the header's Signature is valid for ValidatorPub.
func (h *BlockHeader) VerifySignature() bool {
	// Temporarily clear signature to recompute hash
	sig := h.Signature
	h.Signature = nil
	hash := h.Hash()
	h.Signature = sig
	return h.ValidatorPub.Verify(hash, sig)
}

// Hash returns the hash of the full block (= header hash, since txs are in MerkleRoot).
func (b *Block) Hash() crypto.Hash32 {
	return b.Header.Hash()
}

// Time returns the block timestamp as time.Time.
func (b *Block) Time() time.Time {
	return time.Unix(0, b.Header.Timestamp)
}

// IsGenesis returns true if this is the genesis block (height 0, zero PrevHash).
func (b *Block) IsGenesis() bool {
	return b.Header.Height == 0 && b.Header.PrevHash == (crypto.Hash32{})
}

// Validate performs structural validation of the block.
// It does NOT check consensus rules (done in consensus.Engine).
func (b *Block) Validate() error {
	if b.Header.Timestamp <= 0 {
		return fmt.Errorf("block %d: invalid timestamp", b.Header.Height)
	}
	if len(b.Header.ValidatorPub) == 0 {
		return fmt.Errorf("block %d: missing validator public key", b.Header.Height)
	}
	if !b.Header.VerifySignature() {
		return fmt.Errorf("block %d: invalid validator signature", b.Header.Height)
	}
	expectedRoot := MerkleRoot(b.Txs)
	if expectedRoot != b.Header.MerkleRoot {
		return fmt.Errorf("block %d: merkle root mismatch", b.Header.Height)
	}
	for i, tx := range b.Txs {
		if err := tx.Validate(); err != nil {
			return fmt.Errorf("block %d tx[%d]: %w", b.Header.Height, i, err)
		}
	}
	return nil
}

// Size returns the approximate serialized size in bytes.
func (b *Block) Size() int {
	size := 32 + 32 + 32 + 8 + 4 + 32 + 64 // header fields
	for _, tx := range b.Txs {
		size += tx.Size()
	}
	return size
}

// ─── Little-endian encoding helpers ──────────────────────────────────────────

func encodeUint64(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

func encodeInt64(v int64) []byte {
	return encodeUint64(uint64(v))
}

func encodeUint32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}
