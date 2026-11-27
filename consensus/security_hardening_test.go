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

func TestSelfProducedBlockRejectsMissingOraclePriceAfterHardening(t *testing.T) {
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, OraclePrice: 1_000_000_000}}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	engine := &Engine{
		cfg:   Config{SecurityHardeningActivationHeight: 1},
		chain: chain,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	block := &core.Block{Header: core.BlockHeader{
		Height:      1,
		PrevHash:    genesis.Hash(),
		OraclePrice: 0,
	}}

	err := engine.validateSelfProducedBlock(block)
	if err == nil || !strings.Contains(err.Error(), "oracle price is missing after hardening activation") {
		t.Fatalf("self-produced zero-price block error = %v, want hardening rejection", err)
	}
}

func TestSelfProducedOraclePriceStaysInsideHardeningWindow(t *testing.T) {
	const parentPrice uint64 = 53_328_000
	tests := []struct {
		name     string
		quote    uint64
		want     uint64
		adjusted bool
	}{
		{
			name:     "live quote above limit is capped",
			quote:    58_997_000,
			want:     parentPrice + parentPrice/20,
			adjusted: true,
		},
		{
			name:     "quote below limit is floored",
			quote:    47_000_000,
			want:     parentPrice - parentPrice/20,
			adjusted: true,
		},
		{
			name:     "unavailable quote reuses committed price",
			quote:    0,
			want:     parentPrice,
			adjusted: true,
		},
		{
			name:  "exact upper boundary is retained",
			quote: parentPrice + parentPrice/20,
			want:  parentPrice + parentPrice/20,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			genesis := &core.Block{Header: core.BlockHeader{
				Height:      0,
				OraclePrice: parentPrice,
			}}
			chain := core.NewChain()
			if err := chain.SetGenesis(genesis); err != nil {
				t.Fatalf("SetGenesis: %v", err)
			}
			engine := &Engine{
				cfg:   Config{SecurityHardeningActivationHeight: 1},
				chain: chain,
				log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			atomic.StoreUint64(&engine.cachedOraclePrice, tt.quote)

			selected, err := engine.selectProducerOraclePrice(1, genesis)
			if err != nil {
				t.Fatalf("selectProducerOraclePrice: %v", err)
			}
			if selected.price != tt.want || selected.adjusted != tt.adjusted {
				t.Fatalf("selection = {price:%d adjusted:%t}, want {price:%d adjusted:%t}",
					selected.price, selected.adjusted, tt.want, tt.adjusted)
			}

			block := &core.Block{Header: core.BlockHeader{
				Height:      1,
				PrevHash:    genesis.Hash(),
				OraclePrice: selected.price,
			}}
			if err := engine.checkOraclePriceDeviation(block); err != nil {
				t.Fatalf("selected producer price is rejected by peer validation: %v", err)
			}
		})
	}
}

func TestProducerOraclePricePreservesPreHardeningBehavior(t *testing.T) {
	genesis := &core.Block{Header: core.BlockHeader{Height: 0, OraclePrice: 1_000_000_000}}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatalf("SetGenesis: %v", err)
	}
	engine := &Engine{
		cfg:   Config{},
		chain: chain,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	selected, err := engine.selectProducerOraclePrice(1, genesis)
	if err != nil {
		t.Fatalf("selectProducerOraclePrice: %v", err)
	}
	if selected.price != 0 || selected.adjusted {
		t.Fatalf("pre-hardening selection = %+v, want raw zero quote", selected)
	}
}

func TestPinnedMainnetOraclePriceGapRequiresExactCanonicalBlock(t *testing.T) {
	tests := []struct {
		name        string
		genesisHash string
		height      uint64
		blockHash   string
		parentHash  string
		price       uint64
		want        bool
	}{
		{
			name:        "exact canonical gap",
			genesisHash: canonicalMainnetGenesisHeaderHashHex,
			height:      mainnetOraclePriceGapHeight,
			blockHash:   mainnetOraclePriceGapBlockHashHex,
			parentHash:  mainnetOraclePriceGapParentHashHex,
			price:       0,
			want:        true,
		},
		{
			name:        "wrong genesis",
			genesisHash: "different-genesis",
			height:      mainnetOraclePriceGapHeight,
			blockHash:   mainnetOraclePriceGapBlockHashHex,
			parentHash:  mainnetOraclePriceGapParentHashHex,
			price:       0,
		},
		{
			name:        "wrong height",
			genesisHash: canonicalMainnetGenesisHeaderHashHex,
			height:      mainnetOraclePriceGapHeight + 1,
			blockHash:   mainnetOraclePriceGapBlockHashHex,
			parentHash:  mainnetOraclePriceGapParentHashHex,
			price:       0,
		},
		{
			name:        "wrong block",
			genesisHash: canonicalMainnetGenesisHeaderHashHex,
			height:      mainnetOraclePriceGapHeight,
			blockHash:   "different-block",
			parentHash:  mainnetOraclePriceGapParentHashHex,
			price:       0,
		},
		{
			name:        "wrong parent",
			genesisHash: canonicalMainnetGenesisHeaderHashHex,
			height:      mainnetOraclePriceGapHeight,
			blockHash:   mainnetOraclePriceGapBlockHashHex,
			parentHash:  "different-parent",
			price:       0,
		},
		{
			name:        "nonzero price",
			genesisHash: canonicalMainnetGenesisHeaderHashHex,
			height:      mainnetOraclePriceGapHeight,
			blockHash:   mainnetOraclePriceGapBlockHashHex,
			parentHash:  mainnetOraclePriceGapParentHashHex,
			price:       1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPinnedMainnetOraclePriceGap(tt.genesisHash, tt.height, tt.blockHash, tt.parentHash, tt.price); got != tt.want {
				t.Fatalf("exception match = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestHardenedOracleReferencePriceBridgesOnlyPinnedGap(t *testing.T) {
	previous := &core.Block{Header: core.BlockHeader{Height: mainnetOraclePriceGapHeight - 1, OraclePrice: 59_737_000}}
	gap := &core.Block{Header: core.BlockHeader{Height: mainnetOraclePriceGapHeight, OraclePrice: 0}}

	if got, err := hardenedOracleReferencePrice(previous, nil, false); err != nil || got != previous.Header.OraclePrice {
		t.Fatalf("normal parent reference = %d, %v; want %d, nil", got, err, previous.Header.OraclePrice)
	}
	if _, err := hardenedOracleReferencePrice(gap, previous, false); err == nil ||
		!strings.Contains(err.Error(), "parent oracle price is missing") {
		t.Fatalf("unapproved zero-price parent returned %v, want rejection", err)
	}
	if got, err := hardenedOracleReferencePrice(gap, previous, true); err != nil || got != previous.Header.OraclePrice {
		t.Fatalf("approved gap reference = %d, %v; want %d, nil", got, err, previous.Header.OraclePrice)
	}
	wrongPrevious := &core.Block{Header: core.BlockHeader{Height: previous.Header.Height - 1, OraclePrice: previous.Header.OraclePrice}}
	if _, err := hardenedOracleReferencePrice(gap, wrongPrevious, true); err == nil {
		t.Fatal("non-parent reference block was accepted")
	}
	zeroPrevious := &core.Block{Header: core.BlockHeader{Height: previous.Header.Height, OraclePrice: 0}}
	if _, err := hardenedOracleReferencePrice(gap, zeroPrevious, true); err == nil {
		t.Fatal("zero reference price was accepted")
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
