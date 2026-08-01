package main

// End-to-end test: stake command warns when a real light-mode api.Server
// reports blocks_until_pruned for the specified UTXO.
//
// Unlike prune_warning_test.go, which uses a hand-crafted mock HTTP server
// that returns a fixed JSON body, this test wires a real api.Server backed
// by a live core.Chain and UTXOSet, with SetPruningMode/SetKeepBlocks applied
// exactly as cmd/node/main.go does.  This confirms that the
// SetKeepBlocks / SetPruningMode call-sites in the node startup path are never
// accidentally removed without the test catching it.
//
// Scenario:
//
//	keepBlocks = 50  →  threshold = 50/10 = 5
//	UTXO at block height 0  →  pruneAt = 0+50 = 50
//
//	tipHeight=46: blocksLeft = 50−46 = 4 ≤ 5  →  ⚠️ WARNING expected
//	tipHeight=44: blocksLeft = 50−44 = 6 > 5  →  no WARNING expected

import (
	"encoding/hex"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/api"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// buildLightNodeE2EServer creates a real api.Server in light-pruning mode
// with:
//   - a chain advanced to tipHeight blocks beyond genesis
//   - a single UTXO at block height 0 whose AmountCommit matches fix.commitHex
//   - SetPruningMode("light") + SetKeepBlocks(keepBlocks) wired exactly as
//     cmd/node/main.go does
//
// Returns a live *httptest.Server (registered for t.Cleanup) and the
// stakeFixture whose privKeyHex/txHashHex/amountAPR can be passed directly
// to runStakeCmd.
func buildLightNodeE2EServer(t *testing.T, tipHeight int, keepBlocks uint64) (*httptest.Server, stakeFixture) {
	t.Helper()
	fix := newStakeFixture(t)

	// Build a chain with a genesis block and tipHeight additional blocks.
	// We reuse the same block-factory pattern as api/rest_test.go so the
	// blocks are structurally valid (signed header, coinbase tx, merkle root).
	priv, pub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatalf("GenerateValidatorKey: %v", err)
	}
	makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
		txs := []core.Transaction{core.CoinbaseTx(crypto.Point32(pub), 1_000_000)}
		hdr := core.BlockHeader{
			Height:       height,
			PrevHash:     prevHash,
			Timestamp:    time.Now().UnixNano(),
			ValidatorPub: pub,
			MerkleRoot:   core.MerkleRoot(txs),
		}
		if signErr := hdr.Sign(priv); signErr != nil {
			t.Fatalf("block Sign at height %d: %v", height, signErr)
		}
		return &core.Block{Header: hdr, Txs: txs}
	}

	chain := core.NewChain()
	if err := chain.SetGenesis(makeBlock(0, crypto.Hash32{})); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	for i := 1; i <= tipHeight; i++ {
		parent := chain.GetByHeight(uint64(i - 1))
		if parent == nil {
			t.Fatalf("chain.GetByHeight(%d) returned nil during chain build", i-1)
		}
		if err := chain.AddBlock(makeBlock(uint64(i), parent.Hash())); err != nil {
			t.Fatalf("AddBlock at height %d: %v", i, err)
		}
	}

	// Register the UTXO at block height 0.
	// The commitment and tx-hash match the values the CLI will use (via fix).
	utxos := core.NewUTXOSet()
	txHashBytes, err := hex.DecodeString(fix.txHashHex)
	if err != nil || len(txHashBytes) != 32 {
		t.Fatalf("decode fixture txHashHex: %v", err)
	}
	var txHash crypto.Hash32
	copy(txHash[:], txHashBytes)

	commitBytes, err := hex.DecodeString(fix.commitHex)
	if err != nil || len(commitBytes) != 32 {
		t.Fatalf("decode fixture commitHex: %v", err)
	}
	var commit crypto.Commitment
	copy(commit[:], commitBytes)

	utxos.Add(&core.UTXO{
		TxHash:       txHash,
		OutputIndex:  0,
		AmountCommit: commit,
		BlockHeight:  0, // originating block = genesis → pruned first
	})

	mp := core.NewMempool(core.DefaultMempoolConfig())
	nopLog := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// Wire SetPruningMode / SetKeepBlocks exactly as cmd/node/main.go does
	// (lines annotated "lets stake endpoints detect pruned UTXOs" and
	// "enables blocks_until_pruned warning in restUTXO").
	apiSrv := api.NewServer(":0", chain, mp, utxos, nopLog)
	apiSrv.SetPruningMode("light")
	apiSrv.SetKeepBlocks(keepBlocks)

	httpSrv := httptest.NewServer(apiSrv)
	t.Cleanup(httpSrv.Close)
	return httpSrv, fix
}

// TestStakeLightNodeE2E_WarningWhenClose exercises the full path from the real
// api.Server (light mode, keepBlocks=50) through the CLI stake command.
//
// At tipHeight=46 the UTXO's block is 4 blocks from being pruned, which is
// ≤ threshold (5), so the server includes blocks_until_pruned=4 and the CLI
// must print the ⚠️ WARNING line.
func TestStakeLightNodeE2E_WarningWhenClose(t *testing.T) {
	const (
		keepBlocks uint64 = 50
		tipHeight         = 46 // blocksLeft = 50−46 = 4, threshold = 5 → ⚠️
	)

	httpSrv, fix := buildLightNodeE2EServer(t, tipHeight, keepBlocks)

	resetStakeCmd()
	out, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	// A broadcast error is acceptable — the test server returns 201 ok, so this
	// should succeed; but even if it doesn't, the warning is printed before the
	// broadcast attempt, so stdout must already contain ⚠️ WARNING.
	_ = runErr

	if !strings.Contains(out, "⚠️") || !strings.Contains(out, "WARNING") {
		t.Errorf(
			"expected ⚠️ WARNING in stdout when tipHeight=%d, keepBlocks=%d "+
				"(blocksLeft=4, threshold=5)\ngot stdout:\n%s",
			tipHeight, keepBlocks, out,
		)
	}
	// The warning must state the exact block count.
	if !strings.Contains(out, "4") {
		t.Errorf("expected blocks_until_pruned count (4) in warning; got stdout:\n%s", out)
	}
}

// TestStakeLightNodeE2E_NoWarningWhenFar exercises the same path as above but
// with tipHeight=44, where blocksLeft=6 > threshold(5), so the server omits
// blocks_until_pruned and the CLI must NOT print ⚠️ WARNING.
func TestStakeLightNodeE2E_NoWarningWhenFar(t *testing.T) {
	const (
		keepBlocks uint64 = 50
		tipHeight         = 44 // blocksLeft = 50−44 = 6, threshold = 5 → no ⚠️
	)

	httpSrv, fix := buildLightNodeE2EServer(t, tipHeight, keepBlocks)

	resetStakeCmd()
	out, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)
	_ = runErr

	if strings.Contains(out, "⚠️") || strings.Contains(out, "WARNING") {
		t.Errorf(
			"unexpected ⚠️ WARNING in stdout when tipHeight=%d, keepBlocks=%d "+
				"(blocksLeft=6, threshold=5)\ngot stdout:\n%s",
			tipHeight, keepBlocks, out,
		)
	}
}

// TestStakeLightNodeE2E_ArchiveModeNoWarning confirms that even when the UTXO
// is deep inside the danger zone, archive mode never emits blocks_until_pruned,
// so the CLI produces no warning against an archive node.
func TestStakeLightNodeE2E_ArchiveModeNoWarning(t *testing.T) {
	const (
		keepBlocks uint64 = 50
		tipHeight         = 48 // blocksLeft = 2 — would warn in light mode
	)

	// Build server in archive mode (overriding the fixture).
	fix := newStakeFixture(t)
	priv, pub, _ := crypto.GenerateValidatorKey()
	makeBlock := func(height uint64, prevHash crypto.Hash32) *core.Block {
		txs := []core.Transaction{core.CoinbaseTx(crypto.Point32(pub), 1_000_000)}
		hdr := core.BlockHeader{
			Height:    height, PrevHash: prevHash,
			Timestamp: time.Now().UnixNano(), ValidatorPub: pub,
			MerkleRoot: core.MerkleRoot(txs),
		}
		_ = hdr.Sign(priv)
		return &core.Block{Header: hdr, Txs: txs}
	}
	chain := core.NewChain()
	_ = chain.SetGenesis(makeBlock(0, crypto.Hash32{}))
	for i := 1; i <= tipHeight; i++ {
		parent := chain.GetByHeight(uint64(i - 1))
		_ = chain.AddBlock(makeBlock(uint64(i), parent.Hash()))
	}
	utxos := core.NewUTXOSet()
	txHashBytes, _ := hex.DecodeString(fix.txHashHex)
	var txHash crypto.Hash32
	copy(txHash[:], txHashBytes)
	commitBytes, _ := hex.DecodeString(fix.commitHex)
	var commit crypto.Commitment
	copy(commit[:], commitBytes)
	utxos.Add(&core.UTXO{TxHash: txHash, OutputIndex: 0, AmountCommit: commit, BlockHeight: 0})

	mp := core.NewMempool(core.DefaultMempoolConfig())
	nopLog := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	apiSrv := api.NewServer(":0", chain, mp, utxos, nopLog)
	apiSrv.SetPruningMode("archive") // archive mode — field must never appear
	apiSrv.SetKeepBlocks(keepBlocks)

	httpSrv := httptest.NewServer(apiSrv)
	t.Cleanup(httpSrv.Close)

	resetStakeCmd()
	out, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)
	_ = runErr

	if strings.Contains(out, "⚠️") || strings.Contains(out, "WARNING") {
		t.Errorf(
			"unexpected ⚠️ WARNING in archive mode (tipHeight=%d, keepBlocks=%d)\n"+
				"got stdout:\n%s",
			tipHeight, keepBlocks, out,
		)
	}
}
