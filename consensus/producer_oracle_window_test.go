package consensus

import (
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
)

// Exercise the real block builder, not only the quote-selection helper.
func TestProducedBlockUsesPeerOracleWindow(t *testing.T) {
	const reference uint64 = 53_328_000
	for _, test := range []struct {
		name        string
		quote, want uint64
	}{
		{"observed invalid jump", 58_997_000, reference + reference/20},
		{"lower jump", 47_000_000, reference - reference/20},
		{"missing feed", 0, reference},
		{"exact boundary", reference + reference/20, reference + reference/20},
	} {
		t.Run(test.name, func(t *testing.T) {
			priv, pub, err := crypto.GenerateValidatorKey()
			if err != nil {
				t.Fatal(err)
			}
			key, err := crypto.NewLockedValidatorKey(priv.Bytes(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer key.Destroy()
			parent := &core.Block{Header: core.BlockHeader{
				Height: 0, Timestamp: time.Now().Add(-time.Second).UnixNano(),
				ValidatorPub: pub, MerkleRoot: core.MerkleRoot(nil), OraclePrice: reference,
			}}
			if err := parent.Header.Sign(priv); err != nil {
				t.Fatal(err)
			}
			chain := core.NewChain()
			if err := chain.SetGenesis(parent); err != nil {
				t.Fatal(err)
			}
			wallet, err := crypto.GenerateWalletKeys()
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{
				MyKey: key, Validators: []crypto.ValidatorPubKey{pub},
				RewardAddress:                       string(crypto.AddressFromKeys(crypto.MainnetByte, wallet)),
				BlockRewardNAPR:                     DefaultBlockRewardNAPR,
				SecurityHardeningActivationHeight:   1,
				RewardAuthorizationActivationHeight: ^uint64(0),
				RingCTV4ActivationHeight:            ^uint64(0),
			}
			engine := NewEngine(cfg, chain, core.NewMempool(core.DefaultMempoolConfig()),
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			utxos := core.NewUTXOSet()
			engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)
			atomic.StoreUint64(&engine.cachedOraclePrice, test.quote)
			block, err := engine.produceBlock(1, 1, parent)
			if err != nil {
				t.Fatal(err)
			}
			if block.Header.OraclePrice != test.want {
				t.Fatalf("price=%d want=%d", block.Header.OraclePrice, test.want)
			}
			if !block.Header.VerifySignature(pub) {
				t.Fatal("invalid produced signature")
			}
			if err := engine.checkOraclePriceDeviation(block); err != nil {
				t.Fatalf("peer oracle check rejected produced block: %v", err)
			}
			if err := engine.validateSelfProducedBlock(block); err != nil {
				t.Fatalf("valid produced control rejected: %v", err)
			}
			// Re-sign the otherwise valid control so rejection cannot be
			// attributed to a stale signature.
			block.Header.OraclePrice = 58_997_000
			if err := block.Header.Sign(priv); err != nil {
				t.Fatal(err)
			}
			err = engine.validateSelfProducedBlock(block)
			if err == nil || !strings.Contains(err.Error(), "exceeds fixed 5% limit") {
				t.Fatalf("mutated self-produced block accepted or wrong guard: %v", err)
			}
			if chain.Height() != 0 {
				t.Fatal("oracle validation mutated chain")
			}
		})
	}
}
