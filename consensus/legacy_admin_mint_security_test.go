package consensus

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aperod/aperod/avm"
	"github.com/aperod/aperod/core"
	"github.com/aperod/aperod/crypto"
	"github.com/aperod/aperod/store"
)

// TestScheduleAdminMintFailsClosedBeforeFutureRingCTActivation is the exact
// regression for the unauthenticated mint reported against the old mainnet
// configuration. A future, positive RingCT activation height must not enable a
// newly scheduled legacy coinbase when no consensus-authenticated envelope
// exists.
func TestScheduleAdminMintFailsClosedBeforeFutureRingCTActivation(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	chain := core.NewChain()
	pool := core.NewMempool(core.DefaultMempoolConfig())
	engine := NewEngine(Config{
		Store:                               db,
		RingCTV4ActivationHeight:            1_750_000,
		RewardAuthorizationActivationHeight: 0,
	}, chain, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, keys)

	hash, height, err := engine.ScheduleAdminMint(
"legacy-unauthenticated-mint",
		string(address),
		1_000_000*100_000_000,
		time.Second,
	)
	if err == nil || !strings.Contains(err.Error(), "no consensus-authenticated mint authorization envelope") {
		t.Fatalf("ScheduleAdminMint error = %v, want fail-closed authorization error", err)
	}
	if hash != (crypto.Hash32{}) || height != 0 {
		t.Fatalf("rejected mint returned hash %x at height %d", hash, height)
	}
	if pool.Count() != 0 {
		t.Fatalf("rejected mint changed mempool: got %d transaction(s)", pool.Count())
	}
if _, found, err := db.LoadAdminMintRecord("legacy-unauthenticated-mint"); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("rejected unauthenticated mint persisted an intent")
	}
}

// TestProducerDropsPrivilegedLegacyMint closes non-RPC local creation paths.
// Even an in-memory coinbase already marked privileged must not reach a newly
// produced block without a consensus-authenticated envelope.
func TestProducerDropsPrivilegedLegacyMint(t *testing.T) {
	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := crypto.NewLockedValidatorKey(validatorPriv.Bytes(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Destroy()

	genesisHeader := core.BlockHeader{
		Height:       0,
		Timestamp:    time.Now().UnixNano(),
		ValidatorPub: validatorPub,
		MerkleRoot:   core.MerkleRoot(nil),
	}
	if err := genesisHeader.Sign(validatorPriv); err != nil {
		t.Fatal(err)
	}
	genesis := &core.Block{Header: genesisHeader}
	chain := core.NewChain()
	if err := chain.SetGenesis(genesis); err != nil {
		t.Fatal(err)
	}

	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	mint, err := core.BuildMintTx(address, 1_000_000*100_000_000, 1)
	if err != nil {
		t.Fatal(err)
	}
	pool := core.NewMempool(core.DefaultMempoolConfig())
	if err := pool.AddPrivileged(*mint); err != nil {
		t.Fatal(err)
	}

	utxos := core.NewUTXOSet()
	engine := NewEngine(Config{
		OnCanonicalBlock: func(*core.Block, *avm.PreparedBlock) error { return nil },
		Validators:       []crypto.ValidatorPubKey{validatorPub},
		MyKey:            locked,
		// Reproduce the vulnerable pre-activation production era.
		RingCTV4ActivationHeight: 1_750_000,
	}, chain, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.SetTxVerifier(core.NewTxVerifier(utxos), utxos)

	if err := engine.tick(); err != nil {
		t.Fatalf("produce block: %v", err)
	}
	block := chain.GetByHeight(1)
	if block == nil {
		t.Fatal("produced block missing")
	}
	if len(block.Txs) != 0 {
		t.Fatalf("produced block retained %d unauthorized transaction(s)", len(block.Txs))
	}
	if pool.Count() != 0 {
		t.Fatalf("unauthorized privileged mint remained in mempool: %d transaction(s)", pool.Count())
	}
}

// TestLegacyReplayBoundaryRemainsStructural documents the deliberate boundary:
// below coordinated reward-authorization activation, canonical replay still
// accepts the old structural coinbase format. Consequently a malicious
// proposer can still create arbitrary supply in a new legacy-era block; local
// source shutdown alone cannot remove that consensus vulnerability.
func TestLegacyReplayBoundaryRemainsStructural(t *testing.T) {
	const activation = uint64(100)
	keys, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, keys)
	arbitraryMint, err := core.BuildMintTx(address, 9_000_000_000*100_000_000, activation-1)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{
		Header: core.BlockHeader{Height: activation - 1},
		Txs:    []core.Transaction{*arbitraryMint},
	}
	engine := NewEngine(Config{
		RewardAuthorizationActivationHeight: activation,
	}, core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := engine.validateCoinbasePolicy(block); err != nil {
		t.Fatalf("legacy replay policy unexpectedly changed: %v", err)
	}
}

// TestSignedConsensusRewardFlowRemainsValid proves that disabling the legacy
// scheduler does not disable the existing capped authorization flow. The
// authorization is signed by the block proposer and binds the exact height,
// parent, recipient, amount, and replay-protection ID.
func TestSignedConsensusRewardFlowRemainsValid(t *testing.T) {
	const activation = uint64(100)

	validatorPriv, validatorPub, err := crypto.GenerateValidatorKey()
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := crypto.GenerateWalletKeys()
	if err != nil {
		t.Fatal(err)
	}
	address := crypto.AddressFromKeys(crypto.MainnetByte, recipient)
	parentHash := crypto.HashBytes([]byte("authorized-flow-parent"))

	reward, err := core.BuildAuthorizedRewardTx(
		address,
		AuthorizedBlockRewardNAPR,
		activation,
		parentHash,
		validatorPriv,
	)
	if err != nil {
		t.Fatal(err)
	}
	block := &core.Block{
		Header: core.BlockHeader{
			Height:       activation,
			PrevHash:     parentHash,
			ValidatorPub: validatorPub,
			BaseFee:      core.InitialBaseFeePerByte,
		},
		Txs: []core.Transaction{*reward},
	}
	engine := NewEngine(Config{
		Validators:                          []crypto.ValidatorPubKey{validatorPub},
		StakingPoolNAPR:                     2 * DefaultPoolBlockRewardNAPR,
		TailRewardNAPR:                      defaultTailRewardNAPR,
		RingCTV4ActivationHeight:            50,
		RewardAuthorizationActivationHeight: activation,
	}, core.NewChain(), core.NewMempool(core.DefaultMempoolConfig()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := engine.validateCoinbasePolicy(block); err != nil {
		t.Fatalf("valid signed consensus reward rejected: %v", err)
	}
}
