package consensus

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

func TestHistoricalMainnetRewardGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		genesis    crypto.Hash32
		activation uint64
		height     uint64
		want       bool
	}{
		{"before authorization", mainnetRewardGenesisHash, mainnetRewardAuthorizationHeight, mainnetRewardAuthorizationHeight - 1, false},
		{"authorization boundary", mainnetRewardGenesisHash, mainnetRewardAuthorizationHeight, mainnetRewardAuthorizationHeight, true},
		{"AVM era", mainnetRewardGenesisHash, mainnetRewardAuthorizationHeight, 1_851_420, true},
		{"last legacy block", mainnetRewardGenesisHash, mainnetRewardAuthorizationHeight, mainnetRewardCutoverHeight - 1, true},
		{"cutover boundary", mainnetRewardGenesisHash, mainnetRewardAuthorizationHeight, mainnetRewardCutoverHeight, false},
		{"another chain", crypto.HashBytes([]byte("another-chain")), mainnetRewardAuthorizationHeight, 1_851_420, false},
		{"different activation", mainnetRewardGenesisHash, mainnetRewardAuthorizationHeight + 1, 1_851_420, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := historicalMainnetReward(tc.genesis, tc.activation, tc.height); got != tc.want {
				t.Fatalf("historicalMainnetReward(%d) = %v, want %v", tc.height, got, tc.want)
			}
		})
	}
}

// Set APEROD_AUDIT_DB to an isolated, read-only copy of the canonical chain
// for a production-data regression check. The test must never open live data.
func TestCanonicalMainnetRewardHistoryOnCopy(t *testing.T) {
	path := os.Getenv("APEROD_AUDIT_DB")
	if path == "" {
		t.Skip("APEROD_AUDIT_DB is not set")
	}
	tmpRoot, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatalf("resolve temporary root: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve copied database: %v", err)
	}
	relative, err := filepath.Rel(tmpRoot, resolved)
	if err != nil || relative == "." || relative == ".." ||
		filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		t.Fatal("APEROD_AUDIT_DB must resolve below /tmp; refusing to open a live database")
	}
	db, err := store.OpenReadOnly(resolved)
	if err != nil {
		t.Fatalf("open copied chain read-only: %v", err)
	}
	defer db.Close()
	tipHash, tipHeight, err := db.GetTip()
	if err != nil || tipHeight != 2_478_718 ||
		fmt.Sprintf("%x", tipHash[:]) != "5d056ebbcf94cd3ebe97b0cddd62cd4803c960b5821dc4f1883711e1a38d1ab3" {
		t.Fatalf("copied tip does not match the independently observed live-chain anchor: height %d, err %v", tipHeight, err)
	}

	genesisRaw, err := db.GetRawBlockByHeight(0)
	if err != nil || len(genesisRaw) == 0 {
		t.Fatalf("read copied genesis: %v", err)
	}
	var genesis core.Block
	if err := json.Unmarshal(genesisRaw, &genesis); err != nil {
		t.Fatalf("decode copied genesis: %v", err)
	}
	if genesis.Hash() != mainnetRewardGenesisHash {
		t.Fatalf("copied genesis does not match the mainnet reward-history anchor")
	}
	chain := core.NewChain()
	if err := chain.SetGenesis(&genesis); err != nil {
		t.Fatalf("install copied genesis: %v", err)
	}
	engine := NewEngine(
		Config{RewardAuthorizationActivationHeight: mainnetRewardAuthorizationHeight},
		chain,
		core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	checkBlock := func(height uint64) {
		t.Helper()
		raw, err := db.GetRawBlockByHeight(height)
		if err != nil || len(raw) == 0 {
			t.Fatalf("read copied block %d: %v", height, err)
		}
		var block core.Block
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatalf("decode copied block %d: %v", height, err)
		}
		indexed, ok, err := db.GetCanonicalHash(height)
		if err != nil || !ok || block.Hash() != indexed || block.Header.Height != height {
			t.Fatalf("copied block %d does not match the canonical index: %v", height, err)
		}
		if err := engine.validateCoinbasePolicy(&block); err != nil {
			t.Fatalf("canonical reward rejected at height %d: %v", height, err)
		}
	}
	if os.Getenv("APEROD_AUDIT_FULL") == "1" {
		for height := mainnetRewardAuthorizationHeight; height <= tipHeight; height++ {
			checkBlock(height)
		}
		return
	}
	for _, height := range []uint64{1_851_420, mainnetRewardCutoverHeight - 1, mainnetRewardCutoverHeight, tipHeight} {
		checkBlock(height)
	}
}
