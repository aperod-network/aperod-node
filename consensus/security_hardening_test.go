package consensus

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aperod/aperod/core"
)

func TestSecurityHardeningUsesParentOraclePriceWithoutHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, `{"price_usd":900}`)
	}))
	defer server.Close()

	const parentPrice uint64 = 1_000_000_000
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, OraclePrice: parentPrice}}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	engine := &Engine{
		cfg: Config{
			OracleURL:                         server.URL,
			OracleMaxDeviation:                0.90,
			SecurityHardeningActivationHeight: 10,
		},
		chain: chain,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	block := &core.Block{Header: core.BlockHeader{
		Height:      10,
		PrevHash:    genesis.Hash(),
		OraclePrice: parentPrice,
	}}
	if err := engine.checkOraclePriceDeviation(block); err != nil {
		t.Fatalf("parent-matching price rejected: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("hardening validation made %d HTTP oracle requests, want 0", got)
	}

	block.Header.OraclePrice = parentPrice * 105 / 100
	if err := engine.checkOraclePriceDeviation(block); err != nil {
		t.Fatalf("exact 5%% parent deviation rejected: %v", err)
	}
	block.Header.OraclePrice = parentPrice * 106 / 100
	if err := engine.checkOraclePriceDeviation(block); err == nil ||
		!strings.Contains(err.Error(), "exceeds fixed 5% limit") {
		t.Fatalf("6%% parent deviation error = %v, want rejection", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("deviation check made %d HTTP oracle requests, want 0", got)
	}
}

func TestSecurityHardeningActivatesRewardAuthorizationWhenUnset(t *testing.T) {
	genesis := &core.Block{Header: core.BlockHeader{Height: 0}}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	engine := &Engine{
		cfg:   Config{BlockRewardNAPR: 300_000_000, SecurityHardeningActivationHeight: 3_800_000},
		chain: chain,
	}
	if got := engine.rewardAuthorizationActivationHeight(); got != 3_800_000 {
		t.Fatalf("effective reward authorization height = %d, want 3800000", got)
	}
	if got := engine.CurrentBlockRewardNAPR(); got != 300_000_000 {
		t.Fatalf("pre-fork reward = %d, want legacy 300000000", got)
	}
	engine.cfg.SecurityHardeningActivationHeight = 1
	if got := engine.CurrentBlockRewardNAPR(); got != AuthorizedBlockRewardNAPR {
		t.Fatalf("fork-height reward = %d, want authorized reward %d", got, AuthorizedBlockRewardNAPR)
	}

	engine.cfg.RewardAuthorizationActivationHeight = 1_750_000
	if got := engine.rewardAuthorizationActivationHeight(); got != 1_750_000 {
		t.Fatalf("explicit historical authorization height = %d, want 1750000", got)
	}
}

func TestSecurityHardeningRejectsLegacyCoinbasePathAtFork(t *testing.T) {
	engine := &Engine{cfg: Config{SecurityHardeningActivationHeight: 100}}
	block := &core.Block{Header: core.BlockHeader{Height: 100}}
	if err := engine.validateCoinbasePolicy(block); err == nil ||
		!strings.Contains(err.Error(), "exactly one coinbase") {
		t.Fatalf("fork-height block without an authorized reward returned %v", err)
	}
}
