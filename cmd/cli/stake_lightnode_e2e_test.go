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
// At tipHeight=46 the UTXO's block is 4 blocks from being pruned.  4 is far
// below PartialUnbondingBlocks (~43 200), so the CLI must now return a hard
// rejection error rather than a warning — the commitment cannot be verified
// before unbonding completes.
func TestStakeLightNodeE2E_WarningWhenClose(t *testing.T) {
	const (
		keepBlocks uint64 = 50
		tipHeight         = 46 // blocksLeft = 50−46 = 4 < PartialUnbondingBlocks → rejected
	)

	httpSrv, fix := buildLightNodeE2EServer(t, tipHeight, keepBlocks)

	resetStakeCmd()
	_, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	// blocks_until_pruned=4 < PartialUnbondingBlocks: the command must error.
	if runErr == nil {
		t.Fatalf(
			"expected rejection error when tipHeight=%d, keepBlocks=%d "+
				"(blocksLeft=4 < PartialUnbondingBlocks=%d), got nil",
			tipHeight, keepBlocks, core.PartialUnbondingBlocks,
		)
	}
	// The error message must state the exact block count so the operator knows
	// how close to pruning the UTXO is.
	if !strings.Contains(runErr.Error(), "4") {
		t.Errorf("expected block count (4) in rejection error; got: %v", runErr)
	}
}

// TestStakeLightNodeE2E_NoWarningWhenFar exercises the same path as above but
// with a large keepBlocks so the UTXO is safely far from pruning:
// blocksLeft=199995 >> PartialUnbondingBlocks(43200), so the server omits
// blocks_until_pruned and the CLI must NOT error or print ⚠️ WARNING.
func TestStakeLightNodeE2E_NoWarningWhenFar(t *testing.T) {
	const (
		keepBlocks uint64 = 200_000
		tipHeight         = 5 // blocksLeft = 200 000−5 = 199 995 >> PartialUnbondingBlocks
	)

	httpSrv, fix := buildLightNodeE2EServer(t, tipHeight, keepBlocks)

	resetStakeCmd()
	out, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if runErr != nil {
		t.Fatalf(
			"expected success when tipHeight=%d, keepBlocks=%d "+
				"(blocksLeft=199995 >> PartialUnbondingBlocks=%d), got error: %v",
			tipHeight, keepBlocks, core.PartialUnbondingBlocks, runErr,
		)
	}
	if strings.Contains(out, "⚠️") || strings.Contains(out, "WARNING") {
		t.Errorf(
			"unexpected ⚠️ WARNING in stdout when far from prune window\ngot stdout:\n%s",
			out,
		)
	}
}

// TestStakeLightNodeE2E_RejectsAtPruneBoundary verifies that the CLI returns
// a hard rejection when the UTXO's originating block is exactly at the pruning
// boundary (tipHeight == pruneAt), i.e. blocks_until_pruned=0.
func TestStakeLightNodeE2E_RejectsAtPruneBoundary(t *testing.T) {
	const (
		keepBlocks uint64 = 50
		tipHeight         = 50 // == pruneAt → blocks_until_pruned=0 → rejected
	)

	httpSrv, fix := buildLightNodeE2EServer(t, tipHeight, keepBlocks)

	resetStakeCmd()
	_, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if runErr == nil {
		t.Fatalf(
			"expected rejection when tipHeight=%d == pruneAt (keepBlocks=%d, blocks_until_pruned=0), got nil",
			tipHeight, keepBlocks,
		)
	}
}

// TestStakeLightNodeE2E_RejectsPastPruneBoundary verifies that the CLI returns
// a hard rejection when the UTXO's originating block is past the pruning
// boundary (tipHeight > pruneAt), i.e. blocks_until_pruned=0.
func TestStakeLightNodeE2E_RejectsPastPruneBoundary(t *testing.T) {
	const (
		keepBlocks uint64 = 50
		tipHeight         = 52 // > pruneAt=50 → blocks_until_pruned=0 → rejected
	)

	httpSrv, fix := buildLightNodeE2EServer(t, tipHeight, keepBlocks)

	resetStakeCmd()
	_, runErr := runStakeCmd(t, httpSrv.URL, fix.privKeyHex, fix.txHashHex, fix.amountAPR)

	if runErr == nil {
		t.Fatalf(
			"expected rejection when tipHeight=%d > pruneAt=%d (keepBlocks=%d, blocks_until_pruned=0), got nil",
			tipHeight, keepBlocks, keepBlocks,
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
