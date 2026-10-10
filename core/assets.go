package core

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ─── Asset protocol encoding (Task 4.2) ──────────────────────────────────────
//
// Game-asset metadata is stored in Transaction.Extra (max 255 bytes) using a
// compact binary format.  Every asset transaction carries the magic tag so that
// any node can identify the payload without inspecting the version field first.
//
// Wire format (big-endian):
//   [4]  magic   = 'A','P','R','A'
//   [1]  action  = 1 (Mint) | 2 (Transfer) | 3 (Burn)
//   [32] assetID = SHA3-256 of (creator || name || symbol || timestamp nonce)
//   [1]  nameLen  + [nameLen]  name    (max 64 bytes)
//   [1]  symLen   + [symLen]   symbol  (max 12 bytes)
//   [1]  typeLen  + [typeLen]  assetType
//   [1]  addrLen  + [addrLen]  ownerAddress
//   [8]  amount   (uint64, little-endian)
//   [rest]        metadata bytes (arbitrary, may be empty)

// AssetTag is the 4-byte magic prefix for game-asset Extra payloads.
var AssetTag = [4]byte{'A', 'P', 'R', 'A'}

// AssetAction identifies the operation encoded in the Extra payload.
type AssetAction uint8

const (
	ActionMint     AssetAction = 1 // Create a new asset
	ActionTransfer AssetAction = 2 // Transfer ownership
	ActionBurn     AssetAction = 3 // Destroy the asset
)

// maxNameBytes is the maximum byte length of an asset name.
const maxNameBytes = 64

// maxSymbolBytes is the maximum byte length of an asset symbol.
const maxSymbolBytes = 12

// maxAddrBytes is the maximum byte length of an encoded APRO address.
const maxAddrBytes = 120

// AssetPayload is the structured representation of a game-asset operation.
type AssetPayload struct {
	// Action is the operation type: Mint, Transfer, or Burn.
	Action AssetAction
	// AssetID is the 32-byte unique identifier of this asset.
	AssetID [32]byte
	// Name is the human-readable asset name (max 64 bytes).
	Name string
	// Symbol is the short ticker symbol (max 12 bytes).
	Symbol string
	// AssetType is the category string (e.g. "weapon", "armor").
	AssetType string
	// OwnerAddress is the recipient address for Mint/Transfer, ignored for Burn.
	OwnerAddress string
	// Amount is the token quantity (1 for NFTs; >1 for fungible assets).
	Amount uint64
	// Metadata is optional arbitrary JSON or binary data.
	Metadata []byte
}

// ─── Validation ───────────────────────────────────────────────────────────────

func (p AssetPayload) validate() error {
	if p.Action < ActionMint || p.Action > ActionBurn {
		return fmt.Errorf("unknown asset action: %d", p.Action)
	}
	if len(p.Name) > maxNameBytes {
		return fmt.Errorf("asset name too long: %d bytes (max %d)", len(p.Name), maxNameBytes)
	}
	if len(p.Symbol) > maxSymbolBytes {
		return fmt.Errorf("asset symbol too long: %d bytes (max %d)", len(p.Symbol), maxSymbolBytes)
	}
	if len(p.OwnerAddress) > maxAddrBytes {
		return fmt.Errorf("owner address too long: %d bytes (max %d)", len(p.OwnerAddress), maxAddrBytes)
	}
	return nil
}

// ─── Encoding ─────────────────────────────────────────────────────────────────

// EncodeAssetPayload serialises p into the Transaction.Extra binary format.
// The result is at most 255 bytes (enforced by Transaction.Validate).
func EncodeAssetPayload(p AssetPayload) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}

	// Pre-compute size:
	//   4 (tag) + 1 (action) + 32 (assetID)
	//   + 1+len(name) + 1+len(symbol) + 1+len(type) + 1+len(addr)
	//   + 8 (amount) + len(metadata)
	metaLen := len(p.Metadata)
	totalSize := 4 + 1 + 32 +
		1 + len(p.Name) +
		1 + len(p.Symbol) +
		1 + len(p.AssetType) +
		1 + len(p.OwnerAddress) +
		8 + metaLen

	if totalSize > 255 {
		return nil, fmt.Errorf("encoded asset payload too large: %d bytes (max 255)", totalSize)
	}

	buf := make([]byte, 0, totalSize)

	// Magic tag
	buf = append(buf, AssetTag[:]...)
	// Action
	buf = append(buf, byte(p.Action))
	// AssetID (32 bytes)
	buf = append(buf, p.AssetID[:]...)
	// Length-prefixed fields
	buf = appendLenPrefixed(buf, []byte(p.Name))
	buf = appendLenPrefixed(buf, []byte(p.Symbol))
	buf = appendLenPrefixed(buf, []byte(p.AssetType))
	buf = appendLenPrefixed(buf, []byte(p.OwnerAddress))
	// Amount (8 bytes, little-endian)
	var amtBuf [8]byte
	binary.LittleEndian.PutUint64(amtBuf[:], p.Amount)
	buf = append(buf, amtBuf[:]...)
	// Metadata (remainder)
	buf = append(buf, p.Metadata...)

	return buf, nil
}

func appendLenPrefixed(buf, data []byte) []byte {
	buf = append(buf, byte(len(data)))
	buf = append(buf, data...)
	return buf
}

// ─── Decoding ─────────────────────────────────────────────────────────────────

// ErrNotAssetPayload is returned when the Extra bytes do not start with AssetTag.
var ErrNotAssetPayload = errors.New("not an asset payload: missing 'APRA' magic tag")

// DecodeAssetPayload parses the Transaction.Extra binary format back into an
// AssetPayload.  Returns ErrNotAssetPayload if the data is not an asset payload.
func DecodeAssetPayload(data []byte) (AssetPayload, error) {
	if len(data) < 4 || data[0] != 'A' || data[1] != 'P' || data[2] != 'R' || data[3] != 'A' {
		return AssetPayload{}, ErrNotAssetPayload
	}

	r := data[4:] // skip tag

	// Action
	if len(r) < 1 {
		return AssetPayload{}, errors.New("asset payload truncated at action byte")
	}
	action := AssetAction(r[0])
	r = r[1:]

	// AssetID (32 bytes)
	if len(r) < 32 {
		return AssetPayload{}, errors.New("asset payload truncated at assetID")
	}
	var assetID [32]byte
	copy(assetID[:], r[:32])
	r = r[32:]

	// Length-prefixed fields
	name, r, err := readLenPrefixed(r, "name")
	if err != nil {
		return AssetPayload{}, err
	}
	sym, r, err := readLenPrefixed(r, "symbol")
	if err != nil {
		return AssetPayload{}, err
	}
	assetType, r, err := readLenPrefixed(r, "assetType")
	if err != nil {
		return AssetPayload{}, err
	}
	ownerAddr, r, err := readLenPrefixed(r, "ownerAddress")
	if err != nil {
		return AssetPayload{}, err
	}

	// Amount (8 bytes)
	if len(r) < 8 {
		return AssetPayload{}, errors.New("asset payload truncated at amount")
	}
	amount := binary.LittleEndian.Uint64(r[:8])
	r = r[8:]

	// Metadata (remainder)
	var metadata []byte
	if len(r) > 0 {
		metadata = make([]byte, len(r))
		copy(metadata, r)
	}

	p := AssetPayload{
		Action:       action,
		AssetID:      assetID,
		Name:         string(name),
		Symbol:       string(sym),
		AssetType:    string(assetType),
		OwnerAddress: string(ownerAddr),
		Amount:       amount,
		Metadata:     metadata,
	}

	if err := p.validate(); err != nil {
		return AssetPayload{}, fmt.Errorf("decoded payload invalid: %w", err)
	}

	return p, nil
}

func readLenPrefixed(data []byte, fieldName string) (value []byte, rest []byte, err error) {
	if len(data) < 1 {
		return nil, data, fmt.Errorf("asset payload truncated at %s length", fieldName)
	}
	n := int(data[0])
	data = data[1:]
	if len(data) < n {
		return nil, data, fmt.Errorf("asset payload truncated in %s (%d bytes needed, %d available)", fieldName, n, len(data))
	}
	value = data[:n]
	rest = data[n:]
	return value, rest, nil
}

// ─── Helper: does a Transaction carry an asset payload? ──────────────────────

// IsAssetTx reports whether the transaction carries a game-asset Extra payload.
func IsAssetTx(tx *Transaction) bool {
	return tx != nil &&
		tx.Version == TxVersionGameAsset &&
		len(tx.Extra) >= 4 &&
		tx.Extra[0] == 'A' && tx.Extra[1] == 'P' && tx.Extra[2] == 'R' && tx.Extra[3] == 'A'
}

// GetAssetPayload decodes the asset payload from a transaction.
// Returns ErrNotAssetPayload if the transaction is not an asset transaction.
func GetAssetPayload(tx *Transaction) (AssetPayload, error) {
	if tx == nil {
		return AssetPayload{}, errors.New("nil transaction")
	}
	return DecodeAssetPayload(tx.Extra)
}
