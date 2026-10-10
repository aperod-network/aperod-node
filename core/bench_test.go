package core_test

// Benchmarks for 1.9.9: tx verification < 10ms, block verification < 50ms.

import (
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// buildBenchBlock creates a genesis block with a coinbase transaction.
func buildBenchBlock(b *testing.B) (*core.Block, crypto.ValidatorPrivKey) {
	b.Helper()
	validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()
	aliceKeys, _ := crypto.GenerateWalletKeys()
	cb := core.CoinbaseTx(aliceKeys.Spend.Public, 100_000_000)
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(validatorPriv)
	return &core.Block{Header: hdr, Txs: txs}, validatorPriv
}

// ─── Block verification benchmark ────────────────────────────────────────────

// BenchmarkBlockVerify measures how long VerifyBlock takes for a 1-tx block.
// Target: < 50 ms per block.
func BenchmarkBlockVerify(b *testing.B) {
	genesis, validatorPriv := buildBenchBlock(b)
	chain := core.NewChain()
	_ = chain.SetGenesis(genesis)

	// Build a block to verify repeatedly
	aliceKeys, _ := crypto.GenerateWalletKeys()
	cb := core.CoinbaseTx(aliceKeys.Spend.Public, 50_000_000)
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       1,
		PrevHash:     genesis.Hash(),
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: genesis.Header.ValidatorPub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(validatorPriv)
	block := &core.Block{Header: hdr, Txs: txs}

	utxos := core.NewUTXOSet()
	txV := core.NewTxVerifier(utxos)
	cfg := core.DefaultBlockVerifierConfig()
	verifier := core.NewBlockVerifier(cfg, chain, txV)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = verifier.VerifyBlock(block)
	}
}

// ─── Coinbase tx verification benchmark ──────────────────────────────────────

// BenchmarkTxVerify_Coinbase measures VerifyTx for a coinbase transaction.
// Target: < 10 ms per tx.
func BenchmarkTxVerify_Coinbase(b *testing.B) {
	aliceKeys, _ := crypto.GenerateWalletKeys()
	cb := core.CoinbaseTx(aliceKeys.Spend.Public, 100_000_000)
	utxos := core.NewUTXOSet()
	v := core.NewTxVerifier(utxos)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = v.VerifyTx(&cb)
	}
}

// ─── Merkle root benchmark ────────────────────────────────────────────────────

func BenchmarkMerkleRoot_100txs(b *testing.B) {
	aliceKeys, _ := crypto.GenerateWalletKeys()
	txs := make([]core.Transaction, 100)
	for i := range txs {
		txs[i] = core.CoinbaseTx(aliceKeys.Spend.Public, uint64(i+1)*1000)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = core.MerkleRoot(txs)
	}
}

// ─── UTXO set operations ──────────────────────────────────────────────────────

func BenchmarkUTXOSet_ApplyBlock(b *testing.B) {
	validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()
	aliceKeys, _ := crypto.GenerateWalletKeys()
	cb := core.CoinbaseTx(aliceKeys.Spend.Public, 100_000_000)
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(validatorPriv)
	block := &core.Block{Header: hdr, Txs: txs}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		utxos := core.NewUTXOSet()
		_ = utxos.ApplyBlock(block)
	}
}

// ─── Wallet scanner benchmark ─────────────────────────────────────────────────

func BenchmarkWalletScanner_ScanBlock(b *testing.B) {
	aliceKeys, _ := crypto.GenerateWalletKeys()
	scanner := core.NewWalletScanner(
		aliceKeys.View.Private,
		aliceKeys.Spend.Public,
		aliceKeys.View.Public,
		crypto.TestnetByte,
	)

	validatorPriv, validatorPub, _ := crypto.GenerateValidatorKey()
	cb := core.CoinbaseTx(aliceKeys.Spend.Public, 100_000_000)
	txs := []core.Transaction{cb}
	hdr := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(txs),
	}
	_ = hdr.Sign(validatorPriv)
	block := &core.Block{Header: hdr, Txs: txs}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scanner.ScanBlock(block)
	}
}
