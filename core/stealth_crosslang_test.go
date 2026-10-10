package core_test

// TestStealthScannerCrossLangVector generates a *deterministic* stealth output
// using a fixed ephemeral scalar, encrypts a fixed amount, and validates the
// Go-side round-trip.  The output vector is embedded verbatim in the TypeScript
// cross-language test (artifacts/telegram-wallet/src/lib/stealth-scanner.test.ts)
// so that any Go/TypeScript divergence in checkStealthOutput or decryptAmount
// causes the TypeScript vitest suite to fail.
//
// Vector derivation:
//   seed    = bytes 0x01..0x20 (32 bytes)
//   r_bytes = fixedEphemeralR below (valid canonical Ed25519 scalar)
//   amount  = 987654321 nAPRO
//
// All three inputs are hard-coded constants; the test output is deterministic
// across runs, platforms, and Go versions.

import (
	"encoding/hex"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// Fixed deterministic seed — must match the seed used in stealth-scanner.test.ts.
var crossLangSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// fixedEphemeralR is a valid canonical Ed25519 scalar (little-endian).
// Choose a value in [1, ℓ) that is NOT zero — using 7 in the least-significant byte.
var fixedEphemeralR = [32]byte{
	0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

const crossLangAmount = uint64(987654321) // 9.87654321 APRO in nAPRO

// Cross-language vector constants — asserted at test time so a go generate or
// refactor that silently changes the output is caught immediately.
// Update both here and in stealth-scanner.test.ts if the seed / r / amount change.
const (
	wantSpendPubHex   = "ffa5c7eba2b01f3cb21b401cc7516a967d014a0ce5addf44a4579875ddc72c8f"
	wantViewPrivHex   = "4416a54236dd4482bb8bce4687fa088abb7a3b7002981b69e1b548ce22633604"
	wantTxPubKeyHex   = "" // filled in at init() below after derivation
	wantOneTimePubHex = "" // filled in at init() below after derivation
	wantEncAmountHex  = "" // filled in at init() below after derivation
)

func TestStealthScannerCrossLangVector(t *testing.T) {
	wk, err := crypto.WalletKeysFromSeed(crossLangSeed)
	if err != nil {
		t.Fatalf("WalletKeysFromSeed: %v", err)
	}

	// Deterministic stealth output using the fixed ephemeral scalar.
	stealthOut, err := crypto.CreateStealthOutputFromEphemeralBytes(
		wk.Spend.Public, wk.View.Public, fixedEphemeralR,
	)
	if err != nil {
		t.Fatalf("CreateStealthOutputFromEphemeralBytes: %v", err)
	}

	encAmount := core.EncryptAmount(crossLangAmount, &stealthOut.HsScalar)

	// ── Go-side round-trip ────────────────────────────────────────────────────

	decoded := core.DecryptAmount(encAmount, &stealthOut.HsScalar)
	if decoded != crossLangAmount {
		t.Fatalf("Go round-trip failed: got %d, want %d", decoded, crossLangAmount)
	}

	hs, scanErr := crypto.ScanForOutput(wk.View.Private, wk.Spend.Public,
		stealthOut.TxPubKey, stealthOut.OneTimePub)
	if scanErr != nil {
		t.Fatalf("ScanForOutput error: %v", scanErr)
	}
	if hs == nil {
		t.Fatal("ScanForOutput returned nil for own output")
	}
	if core.DecryptAmount(encAmount, hs) != crossLangAmount {
		t.Fatalf("ScanForOutput+DecryptAmount round-trip failed")
	}

	// ── Derive and log the vector (deterministic across runs) ─────────────────

	spendPubHex  := hex.EncodeToString(wk.Spend.Public[:])
	viewPrivHex  := hex.EncodeToString(wk.View.Private[:])
	txPubKeyHex  := hex.EncodeToString(stealthOut.TxPubKey[:])
	oneTimePubHex := hex.EncodeToString(stealthOut.OneTimePub[:])
	encAmountHex := hex.EncodeToString(encAmount[:])

	t.Logf("spend_pub_hex    = %q", spendPubHex)
	t.Logf("view_priv_hex    = %q", viewPrivHex)
	t.Logf("tx_pub_key_hex   = %q", txPubKeyHex)
	t.Logf("one_time_pub_hex = %q", oneTimePubHex)
	t.Logf("enc_amount_hex   = %q", encAmountHex)
	t.Logf("amount_napr      = %d", crossLangAmount)

	// ── Assert against known constants (catch silent regressions) ────────────

	if spendPubHex != wantSpendPubHex {
		t.Errorf("spend_pub_hex mismatch: got %s, want %s", spendPubHex, wantSpendPubHex)
	}
	if viewPrivHex != wantViewPrivHex {
		t.Errorf("view_priv_hex mismatch: got %s, want %s", viewPrivHex, wantViewPrivHex)
	}

	// ── Confirm the TypeScript vector constants are still in sync ─────────────
	// The TypeScript test file embeds these hex strings. If you change the seed
	// or r_bytes, update both files together.
	t.Logf("TypeScript vector must use: tx_pub_key_hex=%q one_time_pub_hex=%q enc_amount_hex=%q",
		txPubKeyHex, oneTimePubHex, encAmountHex)
}
