package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/wallet"
)

const identityTestMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

func identityTestStealth(t *testing.T, keys *wallet.DerivedKeys, ephemeral byte) *crypto.StealthOutput {
	t.Helper()
	var r [32]byte
	r[0] = ephemeral
	return identityTestStealthBytes(t, keys, r)
}

func identityTestStealthBytes(t *testing.T, keys *wallet.DerivedKeys, r [32]byte) *crypto.StealthOutput {
	t.Helper()
	output, err := crypto.CreateStealthOutputFromEphemeralBytes(keys.Keys.Spend.Public, keys.Keys.View.Public, r)
	if err != nil {
		t.Fatalf("create test stealth output: %v", err)
	}
	return output
}

func TestIdentifyOutputRecordsRecognizesOwnedStealthAndMintOnly(t *testing.T) {
	owner, err := wallet.DeriveFromMnemonic(identityTestMnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := wallet.DeriveFromMnemonic(identityTestMnemonic, "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	stealth := identityTestStealth(t, owner, 7)
	foreignStealth := identityTestStealth(t, foreign, 13)
	mintTx, err := core.BuildMintTx(
		crypto.AddressFromKeys(crypto.MainnetByte, owner.Keys),
		5000,
		37,
	)
	if err != nil {
		t.Fatal(err)
	}
	var txHash [32]byte
	stealthHash := crypto.HashBytes([]byte("identity-owned-stealth"))
	copy(txHash[:], stealthHash[:])
	owned := identityOutput{
		TxHash: txHash, OutIdx: 3, OneTimePub: stealth.OneTimePub,
		TxPubKey: stealth.TxPubKey, BlockHeight: 41,
	}
	mintHash := mintTx.Hash()
	mint := identityOutput{
		TxHash: [32]byte(mintHash), OutIdx: 0, OneTimePub: mintTx.Outputs[0].OneTimePub,
		TxPubKey: mintTx.Outputs[0].TxPubKey, BlockHeight: 37,
	}
	foreignOutput := identityOutput{
		TxHash: [32]byte(crypto.HashBytes([]byte("identity-foreign"))), OutIdx: 9,
		OneTimePub: foreignStealth.OneTimePub, TxPubKey: foreignStealth.TxPubKey, BlockHeight: 44,
	}

	identities, err := identifyOutputRecords(owner, []identityOutput{owned, foreignOutput, mint})
	if err != nil {
		t.Fatalf("identify public outputs: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("identified %d outputs, want owned stealth and mint only", len(identities))
	}
	if identities[0].TxHash != owned.TxHash || identities[0].OutIdx != owned.OutIdx {
		t.Fatalf("stealth output reference changed: %+v", identities[0])
	}
	if identities[1].TxHash != mint.TxHash || identities[1].OutIdx != mint.OutIdx {
		t.Fatalf("mint output reference changed: %+v", identities[1])
	}

	for i, pair := range []struct {
		output identityOutput
		hs     crypto.Scalar32
	}{{owned, stealth.HsScalar}, {mint, crypto.ScalarFromUint64(mint.BlockHeight)}} {
		priv, err := crypto.AddScalars(pair.hs, owner.Keys.Spend.Private)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := crypto.ComputeKeyImage(priv, pair.output.OneTimePub)
		if err != nil {
			t.Fatal(err)
		}
		expected, err = crypto.CanonicalKeyImage(expected)
		if err != nil {
			t.Fatal(err)
		}
		if identities[i].KeyImage != expected {
			t.Fatalf("output %d key image mismatch", i)
		}
	}
}

func TestIdentifyOutputRecordsEnforcesChunkBound(t *testing.T) {
	keys, err := wallet.DeriveFromMnemonic(identityTestMnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := identifyOutputRecords(keys, make([]identityOutput, maxIdentityOutputs+1)); err == nil {
		t.Fatal("oversized identity chunk accepted")
	}
}

func makeIdentityWASMProbeFixture(t *testing.T) map[string]any {
	t.Helper()
	owner, err := wallet.DeriveFromMnemonic(identityTestMnemonic, "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := wallet.DeriveFromMnemonic(identityTestMnemonic, "", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	owned := make([]map[string]any, 4096)
	for i := range owned {
		var ephemeral [32]byte
		binary.LittleEndian.PutUint64(ephemeral[:8], uint64(i+1))
		stealth := identityTestStealthBytes(t, owner, ephemeral)
		amount := uint64(1_000_000 + i)
		blind, err := crypto.DeterministicPaymentBlind(stealth.HsScalar, amount)
		if err != nil {
			t.Fatalf("derive fixture payment blind %d: %v", i, err)
		}
		commitment, err := crypto.Commit(amount, blind)
		if err != nil {
			t.Fatalf("commit fixture output %d: %v", i, err)
		}
		encryptedAmount := core.EncryptAmount(amount, &stealth.HsScalar)
		txHash := crypto.HashBytes([]byte(fmt.Sprintf("identity-probe-owned-%04d", i)))
		owned[i] = map[string]any{
			"tx_hash": hex.EncodeToString(txHash[:]), "out_idx": uint32(i % 5), "block_height": uint64(100 + i),
			"one_time_pub":  hex.EncodeToString(stealth.OneTimePub[:]),
			"tx_pub_key":    hex.EncodeToString(stealth.TxPubKey[:]),
			"amount_commit": hex.EncodeToString(commitment[:]),
			"enc_amount":    hex.EncodeToString(encryptedAmount[:]), "amount_napr": amount,
		}
	}

	const legacyAmount = uint64(1234567)
	legacyStealth := identityTestStealth(t, owner, 17)
	legacyBlind, err := crypto.NewBlindFactor()
	if err != nil {
		t.Fatal(err)
	}
	legacyCommit, err := crypto.Commit(legacyAmount, legacyBlind)
	if err != nil {
		t.Fatal(err)
	}
	legacyHash := crypto.HashBytes([]byte("identity-probe-legacy-random-blind"))
	legacyEncryptedAmount := core.EncryptAmount(legacyAmount, &legacyStealth.HsScalar)
	legacy := map[string]any{
		"tx_hash": hex.EncodeToString(legacyHash[:]), "out_idx": uint32(4), "block_height": uint64(71),
		"one_time_pub":  hex.EncodeToString(legacyStealth.OneTimePub[:]),
		"tx_pub_key":    hex.EncodeToString(legacyStealth.TxPubKey[:]),
		"amount_commit": hex.EncodeToString(legacyCommit[:]),
		"enc_amount":    hex.EncodeToString(legacyEncryptedAmount[:]), "amount_napr": legacyAmount,
	}

	const mintAmount = uint64(500000000000000000)
	mintTx, err := core.BuildMintTx(crypto.AddressFromKeys(crypto.MainnetByte, owner.Keys), mintAmount, 83)
	if err != nil {
		t.Fatal(err)
	}
	mint := mintTx.Outputs[0]
	mintHash := mintTx.Hash()
	mintRecord := map[string]any{
		"tx_hash": hex.EncodeToString(mintHash[:]), "out_idx": uint32(0), "block_height": uint64(83),
		"one_time_pub": hex.EncodeToString(mint.OneTimePub[:]), "tx_pub_key": hex.EncodeToString(mint.TxPubKey[:]),
		"amount_commit": hex.EncodeToString(mint.AmountCommit[:]), "enc_amount": "0000000000000000",
		"amount_napr": fmt.Sprintf("%d", mintAmount),
	}

	foreignStealth := identityTestStealth(t, foreign, 19)
	foreignHash := crypto.HashBytes([]byte("identity-probe-foreign"))
	foreignAmount := uint64(9001)
	foreignBlind, err := crypto.DeterministicPaymentBlind(foreignStealth.HsScalar, foreignAmount)
	if err != nil {
		t.Fatal(err)
	}
	foreignCommit, err := crypto.Commit(foreignAmount, foreignBlind)
	if err != nil {
		t.Fatal(err)
	}
	foreignEncrypted := core.EncryptAmount(foreignAmount, &foreignStealth.HsScalar)
	foreignRecord := map[string]any{
		"tx_hash": hex.EncodeToString(foreignHash[:]), "out_idx": uint32(8), "block_height": uint64(91),
		"one_time_pub":  hex.EncodeToString(foreignStealth.OneTimePub[:]),
		"tx_pub_key":    hex.EncodeToString(foreignStealth.TxPubKey[:]),
		"amount_commit": hex.EncodeToString(foreignCommit[:]),
		"enc_amount":    hex.EncodeToString(foreignEncrypted[:]), "amount_napr": foreignAmount,
	}
	return map[string]any{
		"mnemonic": identityTestMnemonic, "owned": owned,
		"legacy": legacy, "mint": mintRecord, "foreign": foreignRecord,
	}
}

func TestIdentityWASMFixtureHas4096UniqueOwnedReferencesAndKeys(t *testing.T) {
	fixture := makeIdentityWASMProbeFixture(t)
	outputs := fixture["owned"].([]map[string]any)
	if len(outputs) != 4096 {
		t.Fatalf("fixture has %d owned records, want 4096", len(outputs))
	}
	references, publicKeys := make(map[string]struct{}, len(outputs)), make(map[string]struct{}, len(outputs))
	for _, output := range outputs {
		reference := fmt.Sprintf("%s/%d", output["tx_hash"], output["out_idx"])
		publicKey := output["one_time_pub"].(string)
		if _, exists := references[reference]; exists {
			t.Fatalf("duplicate owned output reference %s", reference)
		}
		if _, exists := publicKeys[publicKey]; exists {
			t.Fatal("duplicate owned output public key")
		}
		references[reference], publicKeys[publicKey] = struct{}{}, struct{}{}
	}
}

// Optional public BIP-39 fixture consumed by the Node/WASM API probe.
func TestIdentityWASMFixture(t *testing.T) {
	path := os.Getenv("IDENTITY_WASM_TEST_FIXTURE")
	if path == "" {
		t.Skip("cross-runtime identity fixture not requested")
	}
	fixture := makeIdentityWASMProbeFixture(t)
	data, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
