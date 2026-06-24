package core

import (
	"bytes"
	"strings"
	"testing"
)

// ─── EncodeAssetPayload / DecodeAssetPayload roundtrip ───────────────────────

func makeTestPayload(action AssetAction) AssetPayload {
	var id [32]byte
	for i := range id {
		id[i] = byte(i)
	}
	return AssetPayload{
		Action:       action,
		AssetID:      id,
		Name:         "Flame Sword",
		Symbol:       "FSWD",
		AssetType:    "weapon",
		OwnerAddress: "aprTestAddress12345",
		Amount:       1,
		Metadata:     []byte(`{"damage":150,"element":"fire"}`),
	}
}

func TestAsset_RoundTrip_Mint(t *testing.T) {
	p := makeTestPayload(ActionMint)
	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) > 255 {
		t.Fatalf("encoded size %d exceeds 255", len(encoded))
	}
	// Verify magic tag
	if !bytes.Equal(encoded[:4], AssetTag[:]) {
		t.Fatalf("missing magic tag: got %q", encoded[:4])
	}

	got, err := DecodeAssetPayload(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPayloadEqual(t, p, got)
}

func TestAsset_RoundTrip_Transfer(t *testing.T) {
	p := makeTestPayload(ActionTransfer)
	p.OwnerAddress = "aprNewOwnerAddress99"
	p.Amount = 5

	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := DecodeAssetPayload(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertPayloadEqual(t, p, got)
}

func TestAsset_RoundTrip_Burn(t *testing.T) {
	p := makeTestPayload(ActionBurn)
	p.OwnerAddress = ""
	p.Metadata = nil

	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := DecodeAssetPayload(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Action != ActionBurn {
		t.Errorf("action: want Burn, got %d", got.Action)
	}
	if len(got.Metadata) != 0 {
		t.Errorf("expected empty metadata, got %d bytes", len(got.Metadata))
	}
}

func TestAsset_RoundTrip_EmptyMetadata(t *testing.T) {
	p := makeTestPayload(ActionMint)
	p.Metadata = nil

	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeAssetPayload(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Metadata) != 0 {
		t.Errorf("expected nil metadata, got %d bytes", len(got.Metadata))
	}
}

func TestAsset_RoundTrip_MaxName(t *testing.T) {
	p := makeTestPayload(ActionMint)
	p.Name = strings.Repeat("x", maxNameBytes)
	p.Metadata = nil

	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := DecodeAssetPayload(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != p.Name {
		t.Errorf("name mismatch")
	}
}

// ─── Validation errors ───────────────────────────────────────────────────────

func TestAsset_Encode_NameTooLong(t *testing.T) {
	p := makeTestPayload(ActionMint)
	p.Name = strings.Repeat("x", maxNameBytes+1)
	_, err := EncodeAssetPayload(p)
	if err == nil {
		t.Fatal("expected error for name > maxNameBytes")
	}
}

func TestAsset_Encode_SymbolTooLong(t *testing.T) {
	p := makeTestPayload(ActionMint)
	p.Symbol = strings.Repeat("X", maxSymbolBytes+1)
	_, err := EncodeAssetPayload(p)
	if err == nil {
		t.Fatal("expected error for symbol > maxSymbolBytes")
	}
}

func TestAsset_Encode_InvalidAction(t *testing.T) {
	p := makeTestPayload(AssetAction(99))
	_, err := EncodeAssetPayload(p)
	if err == nil {
		t.Fatal("expected error for invalid action")
	}
}

// ─── Decode error cases ───────────────────────────────────────────────────────

func TestAsset_Decode_WrongMagic(t *testing.T) {
	_, err := DecodeAssetPayload([]byte("BADX" + strings.Repeat("\x00", 40)))
	if err != ErrNotAssetPayload {
		t.Fatalf("expected ErrNotAssetPayload, got %v", err)
	}
}

func TestAsset_Decode_TooShort(t *testing.T) {
	_, err := DecodeAssetPayload([]byte("APR"))
	if err != ErrNotAssetPayload {
		t.Fatalf("expected ErrNotAssetPayload for 3-byte input, got %v", err)
	}
}

func TestAsset_Decode_Truncated(t *testing.T) {
	p := makeTestPayload(ActionMint)
	encoded, _ := EncodeAssetPayload(p)
	// Truncate after tag + action + partial assetID
	_, err := DecodeAssetPayload(encoded[:10])
	if err == nil {
		t.Fatal("expected error for truncated payload")
	}
}

// ─── IsAssetTx / GetAssetPayload ─────────────────────────────────────────────

func TestIsAssetTx_True(t *testing.T) {
	p := makeTestPayload(ActionMint)
	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	tx := &Transaction{Version: TxVersionGameAsset, Extra: encoded}
	if !IsAssetTx(tx) {
		t.Error("IsAssetTx should return true")
	}
}

func TestIsAssetTx_False_WrongVersion(t *testing.T) {
	p := makeTestPayload(ActionMint)
	encoded, _ := EncodeAssetPayload(p)
	tx := &Transaction{Version: TxVersionBase, Extra: encoded}
	if IsAssetTx(tx) {
		t.Error("IsAssetTx should return false for TxVersionBase")
	}
}

func TestIsAssetTx_False_NoExtra(t *testing.T) {
	tx := &Transaction{Version: TxVersionGameAsset}
	if IsAssetTx(tx) {
		t.Error("IsAssetTx should return false for empty Extra")
	}
}

func TestGetAssetPayload_OK(t *testing.T) {
	p := makeTestPayload(ActionTransfer)
	encoded, _ := EncodeAssetPayload(p)
	tx := &Transaction{Version: TxVersionGameAsset, Extra: encoded}

	got, err := GetAssetPayload(tx)
	if err != nil {
		t.Fatalf("GetAssetPayload: %v", err)
	}
	assertPayloadEqual(t, p, got)
}

func TestGetAssetPayload_NilTx(t *testing.T) {
	_, err := GetAssetPayload(nil)
	if err == nil {
		t.Fatal("expected error for nil transaction")
	}
}

// ─── Integration: embed in Transaction.Validate ──────────────────────────────

func TestTransaction_Validate_AssetTx(t *testing.T) {
	p := makeTestPayload(ActionMint)
	encoded, err := EncodeAssetPayload(p)
	if err != nil {
		t.Fatal(err)
	}
	tx := &Transaction{
		Version: TxVersionGameAsset,
		Extra:   encoded,
		Fee:     0,
	}
	// Transaction.Validate checks Extra length; asset payloads fit in 255 bytes
	if err := tx.Validate(); err != nil {
		// Allow missing-input/output errors — we only care that Extra passes
		if strings.Contains(err.Error(), "extra too large") {
			t.Fatalf("Transaction.Validate rejected asset extra: %v", err)
		}
	}
}

// ─── Helper ──────────────────────────────────────────────────────────────────

func assertPayloadEqual(t *testing.T, want, got AssetPayload) {
	t.Helper()
	if got.Action != want.Action {
		t.Errorf("action: want %d, got %d", want.Action, got.Action)
	}
	if got.AssetID != want.AssetID {
		t.Errorf("assetID mismatch")
	}
	if got.Name != want.Name {
		t.Errorf("name: want %q, got %q", want.Name, got.Name)
	}
	if got.Symbol != want.Symbol {
		t.Errorf("symbol: want %q, got %q", want.Symbol, got.Symbol)
	}
	if got.AssetType != want.AssetType {
		t.Errorf("assetType: want %q, got %q", want.AssetType, got.AssetType)
	}
	if got.OwnerAddress != want.OwnerAddress {
		t.Errorf("ownerAddress: want %q, got %q", want.OwnerAddress, got.OwnerAddress)
	}
	if got.Amount != want.Amount {
		t.Errorf("amount: want %d, got %d", want.Amount, got.Amount)
	}
	if !bytes.Equal(got.Metadata, want.Metadata) {
		t.Errorf("metadata: want %q, got %q", want.Metadata, got.Metadata)
	}
}
